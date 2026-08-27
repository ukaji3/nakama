package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
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
	var distID, vpcOriginID, instanceID, eniID, sgID string
	for _, f := range []struct {
		dst  *string
		path string
	}{
		{&distID, ssmDistributionIDPath(in.Name)},
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

	slog.Info("delete: starting", "name", in.Name,
		"distribution", distID, "vpc_origin", vpcOriginID,
		"instance", instanceID, "eni", eniID, "sg", sgID)

	if distID != "" {
		if err := s.deleteDistribution(ctx, distID); err != nil {
			if !in.Force {
				return fmt.Errorf("distribution: %w", err)
			}
			slog.Warn("delete: distribution failed (continuing due to --force)", "error", err)
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
	if instanceID != "" {
		if err := s.terminateInstanceAndWait(ctx, instanceID); err != nil {
			if !in.Force {
				return fmt.Errorf("instance: %w", err)
			}
			slog.Warn("delete: instance failed (continuing due to --force)", "error", err)
		}
	}
	if eniID != "" {
		if err := s.deleteENI(ctx, eniID); err != nil {
			if !in.Force {
				return fmt.Errorf("eni: %w", err)
			}
			slog.Warn("delete: eni failed (continuing due to --force)", "error", err)
		}
	}
	if sgID != "" {
		if err := s.deleteSG(ctx, sgID); err != nil {
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
