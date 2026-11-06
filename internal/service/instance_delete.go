package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// DeleteInstanceInput controls how forgiving DeleteInstance should be.
type DeleteInstanceInput struct {
	Name  string
	Force bool // continue past "already gone" errors
}

// DeleteInstance tears down every resource owned by the named
// nakama instance. The order is the reverse of CreateInstance:
//
//  1. CloudFront distribution (disable, wait, delete)
//  2. CloudFront VPC origin
//  3. EC2 instance (terminate, wait)
//  4. ENI
//  5. Security group
//  6. SSM Parameter Store entries under /nakama/<name>/
func (s *Service) DeleteInstance(ctx context.Context, in DeleteInstanceInput) error {
	if err := ValidateInstanceName(in.Name); err != nil {
		return err
	}

	// Resource IDs come from SSM. A read failure here must abort the
	// delete: treating it as "no such resource" would skip cleanup and
	// falsely report success (leaving CloudFront/EC2/ENI/SG behind).
	var distID, vpcOriginID, instanceID, eniID, sgID, crewID string
	for _, f := range []struct {
		dst  *string
		path string
	}{
		{&distID, ssmDistributionIDPath(in.Name)},
		{&crewID, ssmKirocrewDistributionIDPath(in.Name)},
		{&vpcOriginID, ssmVPCOriginIDPath(in.Name)},
		{&instanceID, ssmInstanceIDPath(in.Name)},
		{&eniID, ssmENIIDPath(in.Name)},
		{&sgID, ssmSGIDPath(in.Name)},
	} {
		v, err := s.ssmGetOptional(ctx, f.path)
		if err != nil {
			return err
		}
		*f.dst = v
	}

	// A create or restore stopped between creating the EC2 instance, ENI
	// or security group and recording it leaves no ID in SSM. Those are
	// found by the instance's nakama tags instead (docs/design.md §8.27).
	var instanceIDs, eniIDs, sgIDs []string
	for _, r := range []struct {
		dst      *[]string
		recorded string
		what     string
		find     func(context.Context, string) ([]string, error)
	}{
		{&instanceIDs, instanceID, "ec2 instance", s.findTaggedInstances},
		{&eniIDs, eniID, "eni", s.findTaggedENIs},
		{&sgIDs, sgID, "security group", s.findTaggedSecurityGroups},
	} {
		ids, err := recordedOrTagged(ctx, in.Name, r.recorded, r.what, r.find)
		if err != nil {
			return err
		}
		*r.dst = ids
	}

	slog.Info("delete: starting", "name", in.Name,
		"distribution", distID, "kirocrew_distribution", crewID, "vpc_origin", vpcOriginID,
		"instance", instanceIDs, "eni", eniIDs, "sg", sgIDs)

	// The Kiro Crew distribution (ADR 0020) shares the VPC origin, so it
	// must be gone before the VPC origin below can be deleted.
	if crewID != "" {
		if err := s.deleteKirocrewDistributionForInstance(ctx, crewID); err != nil {
			if !in.Force {
				return fmt.Errorf("kirocrew distribution: %w", err)
			}
			slog.Warn("delete: kirocrew distribution failed (continuing due to --force)", "error", err)
		}
	}
	if distID != "" {
		if err := s.deleteDistribution(ctx, distID); err != nil {
			if !in.Force {
				return fmt.Errorf("distribution: %w", err)
			}
			slog.Warn("delete: distribution failed (continuing due to --force)", "error", err)
		}
	}
	// The redirect function is referenced by the code-server
	// distribution's behaviors; it can only go once that is deleted.
	if crewID != "" {
		if err := s.deleteKirocrewFunction(ctx, in.Name); err != nil {
			if !in.Force {
				return fmt.Errorf("kirocrew function: %w", err)
			}
			slog.Warn("delete: kirocrew function failed (continuing due to --force)", "error", err)
		}
	}
	if vpcOriginID != "" {
		if err := s.deleteVPCOrigin(ctx, vpcOriginID); err != nil {
			if !in.Force {
				return fmt.Errorf("vpc origin: %w", err)
			}
			slog.Warn("delete: vpc origin failed (continuing due to --force)", "error", err)
		}
	}
	for _, id := range instanceIDs {
		if err := s.terminateInstanceAndWait(ctx, id); err != nil {
			if !in.Force {
				return fmt.Errorf("instance: %w", err)
			}
			slog.Warn("delete: instance failed (continuing due to --force)", "error", err)
		}
	}
	// Root volumes swapped in by a pre-#38 restore carry
	// DeleteOnTermination=false and survive the termination above in
	// state "available". Sweep them by their nakama tags so delete
	// keeps its "no resources left behind" contract. Runs regardless
	// of instanceID: the orphan outlives its instance by definition.
	if err := s.deleteOrphanedVolumes(ctx, in.Name); err != nil {
		if !in.Force {
			return fmt.Errorf("volumes: %w", err)
		}
		slog.Warn("delete: orphaned volume sweep failed (continuing due to --force)", "error", err)
	}

	for _, id := range eniIDs {
		if err := s.deleteENI(ctx, id); err != nil {
			if !in.Force {
				return fmt.Errorf("eni: %w", err)
			}
			slog.Warn("delete: eni failed (continuing due to --force)", "error", err)
		}
	}
	for _, id := range sgIDs {
		if err := s.deleteSG(ctx, id); err != nil {
			if !in.Force {
				return fmt.Errorf("sg: %w", err)
			}
			slog.Warn("delete: sg failed (continuing due to --force)", "error", err)
		}
	}
	if err := s.deleteSSMUnderInstance(ctx, in.Name); err != nil {
		if !in.Force {
			return fmt.Errorf("ssm cleanup: %w", err)
		}
		slog.Warn("delete: ssm cleanup failed (continuing due to --force)", "error", err)
	}
	// The instance is gone; if no other code-server instance remains
	// pending/running, stop the shared NAT instance. Best-effort: the
	// delete itself already succeeded, so a reconcile error only warns.
	if err := s.reconcileNATInstancePowerStateExcluding(ctx, instanceID); err != nil {
		slog.Warn("delete: NAT instance reconcile failed", "error", err)
	}
	slog.Info("delete: completed", "name", in.Name)
	return nil
}

// --- resources without a record --------------------------------------------

// tagLookupDenied are the error codes of a tag lookup that the caller's
// IAM policy does not allow.
var tagLookupDenied = []string{"UnauthorizedOperation", "AccessDenied"}

// recordedOrTagged returns the ID SSM recorded or, when there is none,
// every ID that find returns for the instance's tags. A lookup the IAM
// policy denies only warns, so that a policy cut to an older permission
// list keeps deleting what is recorded.
func recordedOrTagged(ctx context.Context, name, recorded, what string,
	find func(context.Context, string) ([]string, error)) ([]string, error) {
	if recorded != "" {
		return []string{recorded}, nil
	}
	ids, err := find(ctx, name)
	if awshelper.IsAPIError(err, tagLookupDenied...) {
		slog.Warn("delete: not allowed to look up an unrecorded "+what+" by its tags; one left by an interrupted create would remain",
			"name", name, "error", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up an unrecorded %s: %w", what, err)
	}
	if len(ids) > 0 {
		slog.Info("delete: found by tags, not in SSM", "name", name, "resource", what, "ids", ids)
	}
	return ids, nil
}

// carriesNakamaTags reports whether tags hold nakama:managed=true and
// every tag of want with exactly that value. The lookups below check
// what their filters returned once more, so that delete never takes a
// resource of another instance or one nakama does not manage.
func carriesNakamaTags(tags []ec2types.Tag, want map[string]string) bool {
	have := awshelper.EC2MapTags(tags)
	if have[tagging.KeyManaged] != tagging.ValueManagedTrue {
		return false
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// findTaggedInstances returns the instances carrying the tags runInstance
// gives the instance, except terminated and shutting-down ones.
func (s *Service) findTaggedInstances(ctx context.Context, name string) ([]string, error) {
	want := map[string]string{
		tagging.NameTag:      naming.EC2(name),
		tagging.KeyName:      name,
		tagging.KeyComponent: tagging.ComponentEC2,
	}
	states := []string{"pending", "running", "stopping", "stopped"}
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: append(nakamaTagFilters(want),
			ec2types.Filter{Name: aws.String("instance-state-name"), Values: states}),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeInstances: %w", err)
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if i.InstanceId != nil && i.State != nil && slices.Contains(states, string(i.State.Name)) &&
				carriesNakamaTags(i.Tags, want) {
				ids = append(ids, *i.InstanceId)
			}
		}
	}
	return ids, nil
}

// findTaggedENIs returns the ENIs carrying the tags upsertInstanceENI
// finds one by.
func (s *Service) findTaggedENIs(ctx context.Context, name string) ([]string, error) {
	want := instanceResourceTags(name, naming.ENI(name))
	out, err := s.clients.EC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: nakamaTagFilters(want),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeNetworkInterfaces: %w", err)
	}
	var ids []string
	for _, n := range out.NetworkInterfaces {
		if n.NetworkInterfaceId != nil && carriesNakamaTags(n.TagSet, want) {
			ids = append(ids, *n.NetworkInterfaceId)
		}
	}
	return ids, nil
}

// findTaggedSecurityGroups returns the security groups carrying the tags
// upsertInstanceSecurityGroup finds one by.
func (s *Service) findTaggedSecurityGroups(ctx context.Context, name string) ([]string, error) {
	want := instanceResourceTags(name, naming.SecurityGroup(name))
	out, err := s.clients.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: nakamaTagFilters(want),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeSecurityGroups: %w", err)
	}
	var ids []string
	for _, g := range out.SecurityGroups {
		if g.GroupId != nil && carriesNakamaTags(g.Tags, want) {
			ids = append(ids, *g.GroupId)
		}
	}
	return ids, nil
}

// --- distribution ---------------------------------------------------------

func (s *Service) deleteDistribution(ctx context.Context, id string) error {
	cfgOut, err := s.clients.CloudFront.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{
		Id: aws.String(id),
	})
	if err != nil {
		if awshelper.IsAPIError(err, "NoSuchDistribution") {
			return nil
		}
		return fmt.Errorf("GetDistributionConfig: %w", err)
	}
	if cfgOut.DistributionConfig == nil || cfgOut.ETag == nil {
		return errors.New("GetDistributionConfig returned nil")
	}

	if cfgOut.DistributionConfig.Enabled != nil && *cfgOut.DistributionConfig.Enabled {
		cfg := *cfgOut.DistributionConfig
		cfg.Enabled = aws.Bool(false)
		upd, err := s.clients.CloudFront.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
			Id:                 aws.String(id),
			DistributionConfig: &cfg,
			IfMatch:            cfgOut.ETag,
		})
		if err != nil {
			return fmt.Errorf("UpdateDistribution(disable): %w", err)
		}
		_ = upd
		slog.Info("distribution: disabled, waiting for Deployed", "id", id)
		if err := s.waitDistributionDeployed(ctx, id); err != nil {
			return err
		}
	}

	// Re-fetch ETag (any update changes it).
	cfgOut, err = s.clients.CloudFront.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{
		Id: aws.String(id),
	})
	if err != nil {
		if awshelper.IsAPIError(err, "NoSuchDistribution") {
			return nil
		}
		return fmt.Errorf("GetDistributionConfig (post-disable): %w", err)
	}

	_, err = s.clients.CloudFront.DeleteDistribution(ctx, &cloudfront.DeleteDistributionInput{
		Id:      aws.String(id),
		IfMatch: cfgOut.ETag,
	})
	if err != nil {
		if awshelper.IsAPIError(err, "NoSuchDistribution") {
			return nil
		}
		return fmt.Errorf("DeleteDistribution: %w", err)
	}
	slog.Info("distribution: deleted", "id", id)
	return nil
}

// --- vpc origin -----------------------------------------------------------

func (s *Service) deleteVPCOrigin(ctx context.Context, id string) error {
	getOut, err := s.clients.CloudFront.GetVpcOrigin(ctx, &cloudfront.GetVpcOriginInput{
		Id: aws.String(id),
	})
	if err != nil {
		if awshelper.IsAPIError(err, "EntityNotFound", "NoSuchResource") {
			return nil
		}
		return fmt.Errorf("GetVpcOrigin: %w", err)
	}
	if getOut.ETag == nil {
		return errors.New("GetVpcOrigin returned nil ETag")
	}
	_, err = s.clients.CloudFront.DeleteVpcOrigin(ctx, &cloudfront.DeleteVpcOriginInput{
		Id:      aws.String(id),
		IfMatch: getOut.ETag,
	})
	if err != nil {
		return fmt.Errorf("DeleteVpcOrigin: %w", err)
	}
	slog.Info("vpc origin: delete requested", "id", id)
	return s.waitVPCOriginGone(ctx, id)
}

func (s *Service) waitVPCOriginGone(ctx context.Context, id string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    20 * time.Second,
		Timeout:     20 * time.Minute,
		Description: fmt.Sprintf("vpc origin %s gone", id),
	}, func(ctx context.Context) (bool, error) {
		_, err := s.clients.CloudFront.GetVpcOrigin(ctx, &cloudfront.GetVpcOriginInput{
			Id: aws.String(id),
		})
		if err != nil {
			if awshelper.IsAPIError(err, "EntityNotFound", "NoSuchResource") {
				return true, nil
			}
			return false, err
		}
		slog.Info("vpc origin: still deleting", "id", id)
		return false, nil
	})
}

// --- ec2 ------------------------------------------------------------------

func (s *Service) terminateInstanceAndWait(ctx context.Context, instanceID string) error {
	if _, err := s.clients.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		if awshelper.IsAPIError(err, "InvalidInstanceID.NotFound") {
			return nil
		}
		return fmt.Errorf("TerminateInstances: %w", err)
	}
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    10 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("instance %s terminated", instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			if awshelper.IsAPIError(err, "InvalidInstanceID.NotFound") {
				return true, nil
			}
			return false, err
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				if i.State != nil && i.State.Name == ec2types.InstanceStateNameTerminated {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

func (s *Service) deleteENI(ctx context.Context, id string) error {
	// Detach happens automatically when the EC2 is terminated; ENI
	// becomes available shortly thereafter.
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     2 * time.Minute,
		Description: fmt.Sprintf("eni %s deleted", id),
	}, func(ctx context.Context) (bool, error) {
		_, err := s.clients.EC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: aws.String(id),
		})
		if err == nil {
			slog.Info("eni: deleted", "eni_id", id)
			return true, nil
		}
		if awshelper.IsAPIError(err, "InvalidNetworkInterfaceID.NotFound") {
			return true, nil
		}
		// Still attached / in use; retry until timeout.
		if strings.Contains(err.Error(), "currently in use") || awshelper.IsAPIError(err, "InvalidParameterValue") {
			return false, nil
		}
		return false, err
	})
}

func (s *Service) deleteSG(ctx context.Context, id string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     2 * time.Minute,
		Description: fmt.Sprintf("sg %s deleted", id),
	}, func(ctx context.Context) (bool, error) {
		_, err := s.clients.EC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(id),
		})
		if err == nil {
			slog.Info("sg: deleted", "sg_id", id)
			return true, nil
		}
		if awshelper.IsAPIError(err, "InvalidGroup.NotFound") {
			return true, nil
		}
		// "DependencyViolation" means resources still reference the SG;
		// keep polling.
		if awshelper.IsAPIError(err, "DependencyViolation") {
			return false, nil
		}
		return false, err
	})
}

// --- ssm ------------------------------------------------------------------

func (s *Service) deleteSSMUnderInstance(ctx context.Context, name string) error {
	prefix := "/nakama/" + name + "/"
	var names []string
	var nextToken *string
	for {
		page, err := s.clients.SSM.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:      aws.String(prefix),
			Recursive: aws.Bool(true),
			NextToken: nextToken,
		})
		if err != nil {
			return fmt.Errorf("GetParametersByPath: %w", err)
		}
		for _, p := range page.Parameters {
			if p.Name != nil {
				names = append(names, *p.Name)
			}
		}
		if page.NextToken == nil || *page.NextToken == "" {
			break
		}
		nextToken = page.NextToken
	}

	// DeleteParameters supports up to 10 names per call.
	for i := 0; i < len(names); i += 10 {
		end := i + 10
		if end > len(names) {
			end = len(names)
		}
		_, err := s.clients.SSM.DeleteParameters(ctx, &ssm.DeleteParametersInput{
			Names: names[i:end],
		})
		if err != nil {
			return fmt.Errorf("DeleteParameters: %w", err)
		}
	}
	if len(names) > 0 {
		slog.Info("ssm: parameters deleted", "count", len(names), "prefix", prefix)
	}
	return nil
}

// deleteOrphanedVolumes removes EBS volumes that belong to the named
// instance (nakama:managed + nakama:name + nakama:component=ebs tags)
// but are no longer attached to anything. Detachment during instance
// termination is asynchronous, so volumes still in a transient state
// are polled briefly before being swept; volumes legitimately in use
// (state in-use) are never touched.
func (s *Service) deleteOrphanedVolumes(ctx context.Context, name string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := s.clients.EC2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("tag:" + tagging.KeyManaged), Values: []string{tagging.ValueManagedTrue}},
				{Name: aws.String("tag:" + tagging.KeyName), Values: []string{name}},
				{Name: aws.String("tag:" + tagging.KeyComponent), Values: []string{tagging.ComponentEBS}},
			},
		})
		if err != nil {
			return fmt.Errorf("DescribeVolumes: %w", err)
		}

		var transient bool
		for _, v := range out.Volumes {
			if v.VolumeId == nil {
				continue
			}
			switch v.State {
			case ec2types.VolumeStateAvailable:
				if _, err := s.clients.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: v.VolumeId}); err != nil {
					return fmt.Errorf("DeleteVolume(%s): %w", *v.VolumeId, err)
				}
				slog.Info("delete: orphaned volume removed", "volume", *v.VolumeId)
			case ec2types.VolumeStateCreating, ec2types.VolumeStateDeleting, ec2types.VolumeStateInUse:
				// in-use covers the asynchronous detach right after
				// the termination above; deleting resolves on its
				// own. Recheck until the deadline — an in-use volume
				// is never deleted, only re-observed.
				transient = true
			default:
				// error: not ours to sweep.
			}
		}
		if !transient || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
