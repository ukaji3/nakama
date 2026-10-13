package service

import (
	"context"
	_ "embed" // natSetupScript
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

type natInstanceInfo struct {
	InstanceID   string
	ENIID        string
	State        ec2types.InstanceStateName
	InstanceType ec2types.InstanceType
}

// initNATInstance creates or reuses the shared NAT instance. The instance
// lives in the public subnet and the private route table targets its primary
// network interface. Its power state is reconciled by code-server lifecycle
// operations rather than kept permanently running like a NAT Gateway.
func (s *Service) initNATInstance(ctx context.Context, res *InitResult) error {
	sgID, err := s.upsertNATInstanceSecurityGroup(ctx, res.VPCID)
	if err != nil {
		return err
	}
	res.NATInstanceSGID = sgID

	info, err := s.findNATInstance(ctx, res.VPCID)
	if err != nil {
		return err
	}
	if info.InstanceID == "" {
		info, err = s.runNATInstance(ctx, res.PublicSubnetID, sgID)
		if err != nil {
			return err
		}
		slog.Info("nat instance: created", "instance_id", info.InstanceID, "eni_id", info.ENIID)
	} else {
		slog.Info("nat instance: reusing existing", "instance_id", info.InstanceID, "eni_id", info.ENIID, "state", info.State)
	}
	if info.ENIID == "" {
		info.ENIID, err = s.primaryNetworkInterfaceID(ctx, info.InstanceID)
		if err != nil {
			return err
		}
	}
	if err := s.disableSourceDestCheck(ctx, info.InstanceID); err != nil {
		return err
	}

	res.NATInstanceID = info.InstanceID
	res.NATInstanceENIID = info.ENIID
	return nil
}

func (s *Service) upsertNATInstanceSecurityGroup(ctx context.Context, vpcID string) (string, error) {
	name := naming.NATInstanceSG()
	out, err := s.clients.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: append(nakamaTagFilters(map[string]string{
			tagging.NameTag:      name,
			tagging.KeyComponent: tagging.ComponentNATInstance,
		}), ec2types.Filter{Name: aws.String("vpc-id"), Values: []string{vpcID}}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeSecurityGroups(nat): %w", err)
	}

	var sgID string
	if len(out.SecurityGroups) > 0 && out.SecurityGroups[0].GroupId != nil {
		sgID = *out.SecurityGroups[0].GroupId
		slog.Info("nat sg: reusing existing", "sg_id", sgID)
	} else {
		tags := tagging.ForShared(tagging.ComponentNATInstance, name)
		created, err := s.clients.EC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName:   aws.String(name),
			Description: aws.String("nakama NAT instance SG: inbound forwarding from nakama VPC CIDR"),
			VpcId:       aws.String(vpcID),
			TagSpecifications: []ec2types.TagSpecification{
				awshelper.EC2TagSpec(ec2types.ResourceTypeSecurityGroup, tags),
			},
		})
		if err != nil {
			return "", fmt.Errorf("CreateSecurityGroup(nat): %w", err)
		}
		if created.GroupId == nil {
			return "", errors.New("CreateSecurityGroup(nat) returned nil GroupId")
		}
		sgID = *created.GroupId
		slog.Info("nat sg: created", "sg_id", sgID)
	}

	if err := s.ensureNATSecurityGroupIngress(ctx, sgID); err != nil {
		return "", err
	}
	return sgID, nil
}

func (s *Service) ensureNATSecurityGroupIngress(ctx context.Context, sgID string) error {
	_, err := s.clients.EC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("-1"),
			IpRanges: []ec2types.IpRange{{
				CidrIp:      aws.String(config.VPCCIDR),
				Description: aws.String("nakama VPC CIDR to NAT instance"),
			}},
		}},
	})
	if err != nil {
		if awshelper.IsAPIError(err, "InvalidPermission.Duplicate") {
			return nil
		}
		return fmt.Errorf("AuthorizeSecurityGroupIngress(nat): %w", err)
	}
	slog.Info("nat sg: ingress added", "sg_id", sgID, "cidr", config.VPCCIDR)
	return nil
}

func (s *Service) runNATInstance(ctx context.Context, subnetID, sgID string) (natInstanceInfo, error) {
	amiID, err := s.resolveNATInstanceAMI(ctx)
	if err != nil {
		return natInstanceInfo{}, err
	}
	tags := tagging.ForShared(tagging.ComponentNATInstance, naming.NATInstance())
	volTags := tagging.ForShared(tagging.ComponentNATInstance, naming.NATInstance()+"-root")

	runIn := &ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: ec2types.InstanceType(config.NATInstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			SubnetId:                 aws.String(subnetID),
			Groups:                   []string{sgID},
			AssociatePublicIpAddress: aws.Bool(true),
			DeleteOnTermination:      aws.Bool(true),
		}},
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(naming.NATInstanceProfile()),
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(natInstanceUserData()))),
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeInstance, tags),
			awshelper.EC2TagSpec(ec2types.ResourceTypeVolume, volTags),
		},
	}
	// The NAT instance profile may have been created moments ago in
	// the same init run; IAM is eventually consistent and RunInstances
	// rejects a not-yet-propagated profile with InvalidParameterValue.
	// Retry that specific failure with a bounded backoff.
	var out *ec2.RunInstancesOutput
	for attempt := 1; ; attempt++ {
		out, err = s.clients.EC2.RunInstances(ctx, runIn)
		if err == nil {
			break
		}
		if attempt >= 20 || !awshelper.IsAPIError(err, "InvalidParameterValue") {
			return natInstanceInfo{}, fmt.Errorf("RunInstances(nat): %w", err)
		}
		slog.Info("nat instance: waiting for IAM instance profile propagation",
			"attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return natInstanceInfo{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
		return natInstanceInfo{}, errors.New("RunInstances(nat) returned no instance")
	}
	info := natInstanceInfo{
		InstanceID: *out.Instances[0].InstanceId,
		ENIID:      primaryNetworkInterfaceIDFromInstance(out.Instances[0]),
	}
	if out.Instances[0].State != nil {
		info.State = out.Instances[0].State.Name
	}
	if info.ENIID == "" {
		info.ENIID, err = s.primaryNetworkInterfaceID(ctx, info.InstanceID)
		if err != nil {
			return natInstanceInfo{}, err
		}
	}
	if err := s.waitInstanceRunning(ctx, info.InstanceID); err != nil {
		return natInstanceInfo{}, fmt.Errorf("wait nat instance running: %w", err)
	}
	info.State = ec2types.InstanceStateNameRunning
	return info, nil
}

func (s *Service) resolveNATInstanceAMI(ctx context.Context) (string, error) {
	out, err := s.clients.SSM.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(config.NATInstanceAMISSMParameter),
	})
	if err != nil {
		return "", fmt.Errorf("GetParameter(%s): %w", config.NATInstanceAMISSMParameter, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
		return "", fmt.Errorf("GetParameter(%s) returned empty AMI ID", config.NATInstanceAMISSMParameter)
	}
	return *out.Parameter.Value, nil
}

// natSetupScript configures the NAT instance: security updates,
// iptables (nf_tables) and the forwarding rules, with the memory limits
// of a t4g.nano in mind (Issue #87). It is the instance's user-data and
// also what `nakama aws init` runs to repair an instance whose
// forwarding is not set up.
//
//go:embed nat_setup.sh
var natSetupScript string

func natInstanceUserData() string {
	return natSetupScript
}

func (s *Service) findNATInstance(ctx context.Context, vpcID string) (natInstanceInfo, error) {
	filters := []ec2types.Filter{
		{Name: aws.String("tag:" + tagging.KeyManaged), Values: []string{tagging.ValueManagedTrue}},
		{Name: aws.String("tag:" + tagging.KeyComponent), Values: []string{tagging.ComponentNATInstance}},
		{Name: aws.String("tag:" + tagging.NameTag), Values: []string{naming.NATInstance()}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}
	if vpcID != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("vpc-id"), Values: []string{vpcID}})
	}
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters})
	if err != nil {
		return natInstanceInfo{}, fmt.Errorf("DescribeInstances(nat): %w", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			info := natInstanceInfo{
				ENIID:        primaryNetworkInterfaceIDFromInstance(i),
				InstanceType: i.InstanceType,
			}
			if i.InstanceId != nil {
				info.InstanceID = *i.InstanceId
			}
			if i.State != nil {
				info.State = i.State.Name
			}
			if info.InstanceID != "" {
				return info, nil
			}
		}
	}
	return natInstanceInfo{}, nil
}

func primaryNetworkInterfaceIDFromInstance(i ec2types.Instance) string {
	for _, ni := range i.NetworkInterfaces {
		if ni.NetworkInterfaceId == nil {
			continue
		}
		if ni.Attachment != nil && ni.Attachment.DeviceIndex != nil && *ni.Attachment.DeviceIndex == 0 {
			return *ni.NetworkInterfaceId
		}
	}
	if len(i.NetworkInterfaces) > 0 && i.NetworkInterfaces[0].NetworkInterfaceId != nil {
		return *i.NetworkInterfaces[0].NetworkInterfaceId
	}
	return ""
}

func (s *Service) primaryNetworkInterfaceID(ctx context.Context, instanceID string) (string, error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return "", fmt.Errorf("DescribeInstances(%s): %w", instanceID, err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if id := primaryNetworkInterfaceIDFromInstance(i); id != "" {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("primary network interface not found for instance %s", instanceID)
}

func (s *Service) disableSourceDestCheck(ctx context.Context, instanceID string) error {
	_, err := s.clients.EC2.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		SourceDestCheck: &ec2types.AttributeBooleanValue{
			Value: aws.Bool(false),
		},
	})
	if err != nil {
		return fmt.Errorf("ModifyInstanceAttribute(%s, SourceDestCheck=false): %w", instanceID, err)
	}
	return nil
}

func (s *Service) ensureNATInstanceRunning(ctx context.Context) error {
	info, err := s.findNATInstance(ctx, "")
	if err != nil {
		return err
	}
	if info.InstanceID == "" {
		return errors.New("nakama NAT instance not found; run \"nakama aws init\" first")
	}
	if info.State == ec2types.InstanceStateNameRunning {
		return nil
	}
	if info.State == ec2types.InstanceStateNamePending {
		return s.waitInstanceRunning(ctx, info.InstanceID)
	}
	if info.State == ec2types.InstanceStateNameStopping {
		if err := s.waitInstanceStopped(ctx, info.InstanceID); err != nil {
			return fmt.Errorf("wait nat instance stopped: %w", err)
		}
	}
	if _, err := s.clients.EC2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{info.InstanceID}}); err != nil {
		return fmt.Errorf("StartInstances(nat): %w", err)
	}
	slog.Info("nat instance: starting", "instance_id", info.InstanceID)
	return s.waitInstanceRunning(ctx, info.InstanceID)
}

func (s *Service) ensureNATInstanceStopped(ctx context.Context) error {
	info, err := s.findNATInstance(ctx, "")
	if err != nil {
		return err
	}
	if info.InstanceID == "" || info.State == ec2types.InstanceStateNameStopped {
		return nil
	}
	if info.State == ec2types.InstanceStateNameStopping {
		return s.waitInstanceStopped(ctx, info.InstanceID)
	}
	if info.State == ec2types.InstanceStateNamePending {
		if err := s.waitInstanceRunning(ctx, info.InstanceID); err != nil {
			return fmt.Errorf("wait nat instance running before stop: %w", err)
		}
	}
	if _, err := s.clients.EC2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{info.InstanceID}}); err != nil {
		return fmt.Errorf("StopInstances(nat): %w", err)
	}
	slog.Info("nat instance: stopping", "instance_id", info.InstanceID)
	return s.waitInstanceStopped(ctx, info.InstanceID)
}

// hasActiveCodeServerInstancesExcluding reports whether any nakama
// code-server instance is pending/running, ignoring excludeInstanceID.
//
// The exclusion lets stop/delete callers make an immediate decision even
// before the target instance's state has propagated out of "running":
// the just-stopped instance is treated as inactive for the purpose of
// deciding whether the NAT instance is still needed.
func (s *Service) hasActiveCodeServerInstancesExcluding(ctx context.Context, excludeInstanceID string) (bool, error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagging.KeyManaged), Values: []string{tagging.ValueManagedTrue}},
			{Name: aws.String("tag:" + tagging.KeyComponent), Values: []string{tagging.ComponentEC2}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	})
	if err != nil {
		return false, fmt.Errorf("DescribeInstances(active code-server): %w", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if excludeInstanceID != "" && i.InstanceId != nil && *i.InstanceId == excludeInstanceID {
				continue
			}
			return true, nil
		}
	}
	return false, nil
}

// reconcileNATInstancePowerState starts the NAT instance when at least one
// code-server instance is pending/running and stops it otherwise.
func (s *Service) reconcileNATInstancePowerState(ctx context.Context) error {
	return s.reconcileNATInstancePowerStateExcluding(ctx, "")
}

// reconcileNATInstancePowerStateExcluding is reconcile with the given
// code-server instance excluded from the "is anything still active" check.
func (s *Service) reconcileNATInstancePowerStateExcluding(ctx context.Context, excludeInstanceID string) error {
	active, err := s.hasActiveCodeServerInstancesExcluding(ctx, excludeInstanceID)
	if err != nil {
		return err
	}
	if active {
		return s.ensureNATInstanceRunning(ctx)
	}
	return s.ensureNATInstanceStopped(ctx)
}
