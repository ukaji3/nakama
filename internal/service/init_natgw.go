package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// NAT gateway waits (ADR 0026). Variables so the tests can shorten them.
var (
	// natGatewayPollInterval is how often a NAT gateway's state is read.
	natGatewayPollInterval = 10 * time.Second
	// natGatewayAvailableTimeout bounds the wait for a new NAT gateway;
	// AWS usually needs a few minutes.
	natGatewayAvailableTimeout = 10 * time.Minute
	// natGatewayDeletedTimeout bounds the wait for a deletion.
	natGatewayDeletedTimeout = 10 * time.Minute
)

// natGatewayInfo describes a nakama NAT gateway.
type natGatewayInfo struct {
	ID             string
	State          ec2types.NatGatewayState
	VPCID          string
	SubnetID       string
	AllocationID   string
	PublicIP       string
	FailureMessage string
}

func natGatewayInfoFrom(nat ec2types.NatGateway) natGatewayInfo {
	info := natGatewayInfo{
		ID:             aws.ToString(nat.NatGatewayId),
		State:          nat.State,
		VPCID:          aws.ToString(nat.VpcId),
		SubnetID:       aws.ToString(nat.SubnetId),
		FailureMessage: aws.ToString(nat.FailureMessage),
	}
	for _, a := range nat.NatGatewayAddresses {
		if a.AllocationId == nil {
			continue
		}
		info.AllocationID = aws.ToString(a.AllocationId)
		info.PublicIP = aws.ToString(a.PublicIp)
		if a.IsPrimary != nil && *a.IsPrimary {
			break
		}
	}
	return info
}

// initNATGateway sets up the gateway mode's egress (ADR 0026): a zonal
// NAT gateway with an Elastic IP in the public subnet. An existing
// nakama NAT gateway is reused (a pending one is waited for), one that
// failed is deleted and replaced. It returns once the gateway is
// available; the caller repoints the private route afterwards, so the
// current egress keeps working until then.
func (s *Service) initNATGateway(ctx context.Context, res *InitResult) error {
	gws, err := s.listNATGateways(ctx, res.VPCID)
	if err != nil {
		return err
	}
	gw, others := preferredNATGateway(gws)
	for _, o := range others {
		slog.Warn("nat gateway: another nakama NAT gateway exists in the VPC; it is not used and is charged while it exists",
			"natgw_id", o.ID, "state", o.State, "used_natgw_id", gw.ID)
	}

	if gw.ID != "" && gw.State == ec2types.NatGatewayStateFailed {
		slog.Warn("nat gateway: failed; deleting it and creating a new one",
			"natgw_id", gw.ID, "reason", gw.FailureMessage)
		if err := s.deleteNATGatewayAndWait(ctx, gw.ID); err != nil {
			return err
		}
		res.DeletedNATGatewayID = gw.ID
		gw = natGatewayInfo{}
	}

	if gw.ID == "" {
		allocID, err := s.upsertNATGatewayEIP(ctx)
		if err != nil {
			return err
		}
		tags := tagging.ForShared(tagging.ComponentNATGW, naming.NATGW())
		out, err := s.clients.EC2.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
			SubnetId:         aws.String(res.PublicSubnetID),
			AllocationId:     aws.String(allocID),
			ConnectivityType: ec2types.ConnectivityTypePublic,
			TagSpecifications: []ec2types.TagSpecification{
				awshelper.EC2TagSpec(ec2types.ResourceTypeNatgateway, tags),
			},
		})
		if err != nil {
			return fmt.Errorf("CreateNatGateway: %w", err)
		}
		if out.NatGateway == nil || out.NatGateway.NatGatewayId == nil {
			return errors.New("CreateNatGateway returned no NAT gateway")
		}
		gw = natGatewayInfoFrom(*out.NatGateway)
		slog.Info("nat gateway: created; waiting until it is available (usually a few minutes)",
			"natgw_id", gw.ID, "allocation_id", allocID)
	} else {
		slog.Info("nat gateway: reusing existing", "natgw_id", gw.ID, "state", gw.State)
	}

	gw, err = s.waitNATGatewayAvailable(ctx, gw.ID)
	if err != nil {
		return err
	}
	res.NATGatewayID = gw.ID
	res.NATGatewayEIPAllocationID = gw.AllocationID
	res.NATGatewayPublicIP = gw.PublicIP
	slog.Info("nat gateway: available", "natgw_id", gw.ID, "public_ip", gw.PublicIP)

	// An Elastic IP left by a NAT gateway that failed or was deleted is
	// charged while it is allocated; release every tagged one but ours.
	released, err := s.releaseNATGatewayEIPs(ctx, gw.AllocationID)
	if err != nil {
		return err
	}
	res.ReleasedNATGatewayEIPID = strings.Join(released, ", ")
	return nil
}

// preferredNATGateway picks the NAT gateway to use from the nakama ones
// in the VPC: available first, then pending, then failed. The rest are
// returned so the caller can report them.
func preferredNATGateway(gws []natGatewayInfo) (natGatewayInfo, []natGatewayInfo) {
	rank := func(st ec2types.NatGatewayState) int {
		switch st {
		case ec2types.NatGatewayStateAvailable:
			return 0
		case ec2types.NatGatewayStatePending:
			return 1
		default:
			return 2
		}
	}
	best := -1
	for i, gw := range gws {
		if best < 0 || rank(gw.State) < rank(gws[best].State) {
			best = i
		}
	}
	if best < 0 {
		return natGatewayInfo{}, nil
	}
	var others []natGatewayInfo
	for i, gw := range gws {
		if i != best {
			others = append(others, gw)
		}
	}
	return gws[best], others
}

// upsertNATGatewayEIP returns the allocation ID of an Elastic IP for
// the NAT gateway: a nakama-tagged one that nothing uses, or a new one.
func (s *Service) upsertNATGatewayEIP(ctx context.Context) (string, error) {
	addrs, err := s.listNATGatewayEIPs(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if a.AssociationId == nil && a.AllocationId != nil {
			slog.Info("nat gateway eip: reusing existing", "allocation_id", *a.AllocationId)
			return *a.AllocationId, nil
		}
	}
	tags := tagging.ForShared(tagging.ComponentEIP, naming.NATGWEIP())
	out, err := s.clients.EC2.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain: ec2types.DomainTypeVpc,
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeElasticIp, tags),
		},
	})
	if err != nil {
		return "", fmt.Errorf("AllocateAddress: %w", err)
	}
	if out.AllocationId == nil {
		return "", errors.New("AllocateAddress returned no AllocationId")
	}
	slog.Info("nat gateway eip: allocated", "allocation_id", *out.AllocationId, "public_ip", aws.ToString(out.PublicIp))
	return *out.AllocationId, nil
}

// listNATGatewayEIPs returns the Elastic IPs tagged as the NAT gateway's.
func (s *Service) listNATGatewayEIPs(ctx context.Context) ([]ec2types.Address, error) {
	out, err := s.clients.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: nakamaTagFilters(map[string]string{
			tagging.NameTag: naming.NATGWEIP(),
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeAddresses: %w", err)
	}
	return out.Addresses, nil
}

// releaseNATGatewayEIPs releases the NAT gateway's tagged Elastic IPs
// except keep ("" releases all of them) and returns the released
// allocation IDs. An address that is still associated (a NAT gateway
// that was just deleted can hold it for a while) is left for a later
// init, as is one that AWS reports in use.
func (s *Service) releaseNATGatewayEIPs(ctx context.Context, keep string) ([]string, error) {
	addrs, err := s.listNATGatewayEIPs(ctx)
	if err != nil {
		return nil, err
	}
	var released []string
	for _, a := range addrs {
		id := aws.ToString(a.AllocationId)
		if id == "" || id == keep {
			continue
		}
		if a.AssociationId != nil {
			slog.Warn("nat gateway eip: still associated; the next init releases it",
				"allocation_id", id, "association_id", *a.AssociationId)
			continue
		}
		if _, err := s.clients.EC2.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(id)}); err != nil {
			switch {
			case awshelper.IsAPIError(err, "InvalidIPAddress.InUse"):
				slog.Warn("nat gateway eip: still in use; the next init releases it", "allocation_id", id)
				continue
			case awshelper.IsAPIError(err, "InvalidAllocationID.NotFound"):
				continue
			default:
				return released, fmt.Errorf("ReleaseAddress(%s): %w", id, err)
			}
		}
		slog.Info("nat gateway eip: released", "allocation_id", id)
		released = append(released, id)
	}
	return released, nil
}

// removeNATGateway is the instance mode's clean-up (ADR 0007, ADR 0026):
// it deletes every nakama NAT gateway in the VPC and releases their
// Elastic IPs. It runs after the private route points at the NAT
// instance, so egress does not stop. It also migrates an environment
// set up before ADR 0007, whose egress went through a NAT gateway of
// the same name. Without a NAT gateway it does nothing.
func (s *Service) removeNATGateway(ctx context.Context, res *InitResult) error {
	gws, err := s.listNATGateways(ctx, res.VPCID)
	if err != nil {
		return err
	}
	var deleted []string
	for _, gw := range gws {
		slog.Warn("nat gateway: deleting it; egress goes through the NAT instance now",
			"natgw_id", gw.ID, "state", gw.State)
		if err := s.deleteNATGatewayAndWait(ctx, gw.ID); err != nil {
			return err
		}
		deleted = append(deleted, gw.ID)
	}
	res.DeletedNATGatewayID = strings.Join(deleted, ", ")

	released, err := s.releaseNATGatewayEIPs(ctx, "")
	if err != nil {
		return err
	}
	res.ReleasedNATGatewayEIPID = strings.Join(released, ", ")
	return nil
}

// deleteNATGatewayAndWait deletes a NAT gateway and waits until it is
// gone. A gateway that is already gone is not an error.
func (s *Service) deleteNATGatewayAndWait(ctx context.Context, natID string) error {
	if _, err := s.clients.EC2.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(natID),
	}); err != nil {
		if awshelper.IsAPIError(err, "NatGatewayNotFound") {
			return nil
		}
		return fmt.Errorf("DeleteNatGateway(%s): %w", natID, err)
	}
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    natGatewayPollInterval,
		Timeout:     natGatewayDeletedTimeout,
		Description: fmt.Sprintf("nat gateway %s deleted", natID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []string{natID},
		})
		if err != nil {
			if awshelper.IsAPIError(err, "NatGatewayNotFound") {
				return true, nil
			}
			return false, err
		}
		if len(out.NatGateways) == 0 || out.NatGateways[0].State == ec2types.NatGatewayStateDeleted {
			slog.Info("nat gateway: deleted", "natgw_id", natID)
			return true, nil
		}
		slog.Info("nat gateway: still deleting", "natgw_id", natID, "state", out.NatGateways[0].State)
		return false, nil
	})
}

// waitNATGatewayAvailable waits until the NAT gateway is available and
// returns its final description. A gateway that fails or goes away is
// an error carrying AWS's reason.
func (s *Service) waitNATGatewayAvailable(ctx context.Context, natID string) (natGatewayInfo, error) {
	var info natGatewayInfo
	err := awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    natGatewayPollInterval,
		Timeout:     natGatewayAvailableTimeout,
		Description: fmt.Sprintf("nat gateway %s available", natID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []string{natID},
		})
		if err != nil {
			if awshelper.IsAPIError(err, "NatGatewayNotFound") {
				return false, fmt.Errorf("nat gateway %s is gone", natID)
			}
			return false, fmt.Errorf("DescribeNatGateways(%s): %w", natID, err)
		}
		if len(out.NatGateways) == 0 {
			return false, fmt.Errorf("nat gateway %s is gone", natID)
		}
		info = natGatewayInfoFrom(out.NatGateways[0])
		switch info.State {
		case ec2types.NatGatewayStateAvailable:
			return true, nil
		case ec2types.NatGatewayStatePending:
			slog.Info("nat gateway: still pending", "natgw_id", natID)
			return false, nil
		default:
			return false, fmt.Errorf("nat gateway %s is %s: %s", natID, info.State, info.FailureMessage)
		}
	})
	return info, err
}

// listNATGateways returns the nakama NAT gateways that are not deleted
// or being deleted, in the VPC when vpcID is set and anywhere in the
// region otherwise.
func (s *Service) listNATGateways(ctx context.Context, vpcID string) ([]natGatewayInfo, error) {
	filters := natGatewayTagFilters(naming.NATGW())
	if vpcID != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("vpc-id"), Values: []string{vpcID}})
	}
	out, err := s.clients.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{Filter: filters})
	if err != nil {
		return nil, fmt.Errorf("DescribeNatGateways: %w", err)
	}
	var gws []natGatewayInfo
	for _, nat := range out.NatGateways {
		if nat.NatGatewayId == nil ||
			nat.State == ec2types.NatGatewayStateDeleted || nat.State == ec2types.NatGatewayStateDeleting {
			continue
		}
		gws = append(gws, natGatewayInfoFrom(nat))
	}
	return gws, nil
}

// natGatewayTagFilters builds the filter set for DescribeNatGateways.
// Note: DescribeNatGateways uses Filter (singular) rather than Filters.
func natGatewayTagFilters(name string) []ec2types.Filter {
	return []ec2types.Filter{
		{
			Name:   aws.String("tag:" + tagging.KeyManaged),
			Values: []string{tagging.ValueManagedTrue},
		},
		{
			Name:   aws.String("tag:" + tagging.NameTag),
			Values: []string{name},
		},
	}
}

// ensureNATGatewayAvailable is the gateway mode's egress check before an
// instance boots or runs an update: the NAT gateway must be available
// (a pending one, such as one init is creating, is waited for). nakama
// does not start or stop a NAT gateway; it is always on (ADR 0026).
func (s *Service) ensureNATGatewayAvailable(ctx context.Context) error {
	gws, err := s.listNATGateways(ctx, "")
	if err != nil {
		return err
	}
	gw, _ := preferredNATGateway(gws)
	switch gw.State {
	case "":
		return errors.New("no nakama NAT instance or NAT gateway found, so the instances cannot reach the internet; " +
			"run \"nakama aws init\" first")
	case ec2types.NatGatewayStateAvailable:
		return nil
	case ec2types.NatGatewayStatePending:
		slog.Info("nat gateway: waiting until it is available", "natgw_id", gw.ID)
		_, err := s.waitNATGatewayAvailable(ctx, gw.ID)
		return err
	default:
		return fmt.Errorf("nat gateway %s is %s (%s), so the instances cannot reach the internet; "+
			"run \"nakama aws init\" to replace it", gw.ID, gw.State, gw.FailureMessage)
	}
}
