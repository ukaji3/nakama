package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// initNATGateway creates an EIP and a single NAT Gateway in the
// public subnet, waiting for it to reach the "available" state.
func (s *Service) initNATGateway(ctx context.Context, res *InitResult) error {
	allocID, err := s.upsertNATGatewayEIP(ctx)
	if err != nil {
		return err
	}
	res.NATGatewayEIPAlloc = allocID

	natID, state, err := s.findNATGateway(ctx, res.VPCID)
	if err != nil {
		return err
	}
	if natID != "" && state == ec2types.NatGatewayStateAvailable {
		slog.Info("nat gateway: reusing existing", "natgw_id", natID)
		res.NATGatewayID = natID
		return nil
	}
	if natID == "" {
		tags := tagging.ForShared(tagging.ComponentNATGW, naming.NATGW())
		c, err := s.clients.EC2.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
			SubnetId:     aws.String(res.PublicSubnetID),
			AllocationId: aws.String(allocID),
			TagSpecifications: []ec2types.TagSpecification{
				awshelper.EC2TagSpec(ec2types.ResourceTypeNatgateway, tags),
			},
		})
		if err != nil {
			return fmt.Errorf("CreateNatGateway: %w", err)
		}
		if c.NatGateway == nil || c.NatGateway.NatGatewayId == nil {
			return errors.New("CreateNatGateway returned nil")
		}
		natID = *c.NatGateway.NatGatewayId
		slog.Info("nat gateway: created, waiting for available", "natgw_id", natID)
	} else {
		slog.Info("nat gateway: waiting for available", "natgw_id", natID, "state", state)
	}
	res.NATGatewayID = natID

	return s.waitNATGatewayAvailable(ctx, natID)
}

// upsertNATGatewayEIP returns the AllocationId of the EIP reserved
// for the NAT gateway, allocating one if none is tagged as ours.
func (s *Service) upsertNATGatewayEIP(ctx context.Context) (string, error) {
	// We don't have an EC2API.DescribeAddresses method on the
	// interface; rely on the resource-groups tagging API for lookup.
	// However adding DescribeAddresses keeps this self-contained.
	out, err := s.clients.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: nakamaTagFilters(map[string]string{
			tagging.NameTag: naming.NATGWEIP(),
		}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeAddresses: %w", err)
	}
	if len(out.Addresses) > 0 && out.Addresses[0].AllocationId != nil {
		id := *out.Addresses[0].AllocationId
		slog.Info("eip: reusing existing", "allocation_id", id)
		return id, nil
	}

	tags := tagging.ForShared(tagging.ComponentEIP, naming.NATGWEIP())
	c, err := s.clients.EC2.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain: ec2types.DomainTypeVpc,
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeElasticIp, tags),
		},
	})
	if err != nil {
		return "", fmt.Errorf("AllocateAddress: %w", err)
	}
	if c.AllocationId == nil {
		return "", errors.New("AllocateAddress returned nil AllocationId")
	}
	slog.Info("eip: allocated", "allocation_id", *c.AllocationId, "public_ip", deref(c.PublicIp))
	return *c.AllocationId, nil
}

// findNATGateway returns (id, state) of the existing nakama-managed
// NAT GW in the given VPC, or ("", "") if none.
//
// Deleted NAT gateways are filtered out.
func (s *Service) findNATGateway(ctx context.Context, vpcID string) (string, ec2types.NatGatewayState, error) {
	out, err := s.clients.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
		Filter: append(natGatewayTagFilters(naming.NATGW()), ec2types.Filter{
			Name:   aws.String("vpc-id"),
			Values: []string{vpcID},
		}),
	})
	if err != nil {
		return "", "", fmt.Errorf("DescribeNatGateways: %w", err)
	}
	for _, nat := range out.NatGateways {
		if nat.State == ec2types.NatGatewayStateDeleted || nat.State == ec2types.NatGatewayStateDeleting {
			continue
		}
		if nat.NatGatewayId == nil {
			continue
		}
		return *nat.NatGatewayId, nat.State, nil
	}
	return "", "", nil
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

func (s *Service) waitNATGatewayAvailable(ctx context.Context, natID string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    10 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("nat gateway %s available", natID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []string{natID},
		})
		if err != nil {
			return false, err
		}
		if len(out.NatGateways) == 0 {
			return false, fmt.Errorf("nat gateway %s disappeared", natID)
		}
		st := out.NatGateways[0].State
		if st == ec2types.NatGatewayStateAvailable {
			return true, nil
		}
		if st == ec2types.NatGatewayStateFailed || st == ec2types.NatGatewayStateDeleted || st == ec2types.NatGatewayStateDeleting {
			return false, fmt.Errorf("nat gateway %s entered terminal state %s", natID, st)
		}
		slog.Info("nat gateway: still pending", "natgw_id", natID, "state", st)
		return false, nil
	})
}

// initRouteTables creates the public and private route tables, the
// 0.0.0.0/0 routes, and the subnet associations.
func (s *Service) initRouteTables(ctx context.Context, res *InitResult) error {
	pubID, err := s.upsertRouteTable(ctx, routeTableSpec{
		Name:      naming.PublicRouteTable(),
		VPCID:     res.VPCID,
		Component: tagging.ComponentRouteTable,
	})
	if err != nil {
		return err
	}
	res.PublicRouteTableID = pubID
	if err := s.ensureDefaultRoute(ctx, pubID, "gateway", res.InternetGatewayID); err != nil {
		return err
	}
	if err := s.ensureSubnetAssociation(ctx, pubID, res.PublicSubnetID); err != nil {
		return err
	}

	privID, err := s.upsertRouteTable(ctx, routeTableSpec{
		Name:      naming.PrivateRouteTable(),
		VPCID:     res.VPCID,
		Component: tagging.ComponentRouteTable,
	})
	if err != nil {
		return err
	}
	res.PrivateRouteTableID = privID
	if err := s.ensureDefaultRoute(ctx, privID, "nat", res.NATGatewayID); err != nil {
		return err
	}
	for _, subID := range res.PrivateSubnetIDs {
		if err := s.ensureSubnetAssociation(ctx, privID, subID); err != nil {
			return err
		}
	}
	return nil
}

type routeTableSpec struct {
	Name      string
	VPCID     string
	Component string
}

func (s *Service) upsertRouteTable(ctx context.Context, spec routeTableSpec) (string, error) {
	out, err := s.clients.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: append(nakamaTagFilters(map[string]string{
			tagging.NameTag: spec.Name,
		}), ec2types.Filter{
			Name:   aws.String("vpc-id"),
			Values: []string{spec.VPCID},
		}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeRouteTables(%s): %w", spec.Name, err)
	}
	if len(out.RouteTables) > 0 && out.RouteTables[0].RouteTableId != nil {
		id := *out.RouteTables[0].RouteTableId
		slog.Info("route table: reusing existing", "name", spec.Name, "rtb_id", id)
		return id, nil
	}

	tags := tagging.ForShared(spec.Component, spec.Name)
	c, err := s.clients.EC2.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId: aws.String(spec.VPCID),
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeRouteTable, tags),
		},
	})
	if err != nil {
		return "", fmt.Errorf("CreateRouteTable(%s): %w", spec.Name, err)
	}
	if c.RouteTable == nil || c.RouteTable.RouteTableId == nil {
		return "", fmt.Errorf("CreateRouteTable(%s) returned nil", spec.Name)
	}
	id := *c.RouteTable.RouteTableId
	slog.Info("route table: created", "name", spec.Name, "rtb_id", id)
	return id, nil
}

// ensureDefaultRoute installs a 0.0.0.0/0 route on the route table
// pointing to either an Internet Gateway or a NAT Gateway. Returns
// nil if a 0.0.0.0/0 route already exists (regardless of its target).
func (s *Service) ensureDefaultRoute(ctx context.Context, rtbID, kind, targetID string) error {
	out, err := s.clients.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		RouteTableIds: []string{rtbID},
	})
	if err != nil {
		return fmt.Errorf("DescribeRouteTables: %w", err)
	}
	if len(out.RouteTables) > 0 {
		for _, r := range out.RouteTables[0].Routes {
			if r.DestinationCidrBlock != nil && *r.DestinationCidrBlock == "0.0.0.0/0" {
				return nil // already routed
			}
		}
	}

	in := &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}
	switch kind {
	case "gateway":
		in.GatewayId = aws.String(targetID)
	case "nat":
		in.NatGatewayId = aws.String(targetID)
	default:
		return fmt.Errorf("unknown default-route kind %q", kind)
	}
	if _, err := s.clients.EC2.CreateRoute(ctx, in); err != nil {
		return fmt.Errorf("CreateRoute: %w", err)
	}
	slog.Info("route: 0.0.0.0/0 added", "rtb_id", rtbID, "kind", kind, "target_id", targetID)
	return nil
}

// ensureSubnetAssociation associates the given subnet with the route
// table if it is not already associated.
func (s *Service) ensureSubnetAssociation(ctx context.Context, rtbID, subnetID string) error {
	out, err := s.clients.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		RouteTableIds: []string{rtbID},
	})
	if err != nil {
		return fmt.Errorf("DescribeRouteTables: %w", err)
	}
	if len(out.RouteTables) > 0 {
		for _, a := range out.RouteTables[0].Associations {
			if a.SubnetId != nil && *a.SubnetId == subnetID {
				return nil
			}
		}
	}

	if _, err := s.clients.EC2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String(subnetID),
	}); err != nil {
		return fmt.Errorf("AssociateRouteTable: %w", err)
	}
	slog.Info("route table: associated", "rtb_id", rtbID, "subnet_id", subnetID)
	return nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
