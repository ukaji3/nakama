package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// initRouteTables creates the public and private route tables, the
// 0.0.0.0/0 routes, and the subnet associations.
//
// The public route table routes 0.0.0.0/0 to the Internet Gateway.
// The private route table routes 0.0.0.0/0 to the NAT instance's
// primary network interface (ADR 0007). If a legacy 0.0.0.0/0 route
// still points at the old NAT Gateway, it is replaced in place so
// egress is switched to the NAT instance before the gateway is removed.
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
	if err := s.ensureDefaultRoute(ctx, pubID, routeTargetGateway, res.InternetGatewayID); err != nil {
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
	if res.NATInstanceENIID == "" {
		return fmt.Errorf("cannot route private subnets: NAT instance ENI is unknown")
	}
	if err := s.ensureDefaultRoute(ctx, privID, routeTargetNetworkInterface, res.NATInstanceENIID); err != nil {
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

// Route target kinds for ensureDefaultRoute.
const (
	routeTargetGateway          = "gateway"
	routeTargetNetworkInterface = "network-interface"
)

// ensureDefaultRoute installs (or repoints) a 0.0.0.0/0 route on the
// route table toward the given target.
//
//   - routeTargetGateway sets GatewayId (Internet Gateway).
//   - routeTargetNetworkInterface sets NetworkInterfaceId (NAT instance ENI).
//
// If no default route exists, it is created. If one exists but points at
// a different target (e.g. a legacy NAT Gateway), it is replaced via
// ReplaceRoute. If it already points at the desired target, this is a no-op.
func (s *Service) ensureDefaultRoute(ctx context.Context, rtbID, kind, targetID string) error {
	if targetID == "" {
		return fmt.Errorf("ensureDefaultRoute: empty target for kind %q", kind)
	}
	out, err := s.clients.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		RouteTableIds: []string{rtbID},
	})
	if err != nil {
		return fmt.Errorf("DescribeRouteTables: %w", err)
	}

	var existing *ec2types.Route
	if len(out.RouteTables) > 0 {
		for i := range out.RouteTables[0].Routes {
			r := out.RouteTables[0].Routes[i]
			if r.DestinationCidrBlock != nil && *r.DestinationCidrBlock == "0.0.0.0/0" {
				route := r
				existing = &route
				break
			}
		}
	}

	if existing != nil {
		if routeMatchesTarget(*existing, kind, targetID) {
			return nil // already routed to the desired target
		}
		in := &ec2.ReplaceRouteInput{
			RouteTableId:         aws.String(rtbID),
			DestinationCidrBlock: aws.String("0.0.0.0/0"),
		}
		if err := setRouteTarget(kind, targetID, &in.GatewayId, &in.NetworkInterfaceId); err != nil {
			return err
		}
		if _, err := s.clients.EC2.ReplaceRoute(ctx, in); err != nil {
			return fmt.Errorf("ReplaceRoute: %w", err)
		}
		slog.Info("route: 0.0.0.0/0 replaced", "rtb_id", rtbID, "kind", kind, "target_id", targetID)
		return nil
	}

	in := &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}
	if err := setRouteTarget(kind, targetID, &in.GatewayId, &in.NetworkInterfaceId); err != nil {
		return err
	}
	if _, err := s.clients.EC2.CreateRoute(ctx, in); err != nil {
		return fmt.Errorf("CreateRoute: %w", err)
	}
	slog.Info("route: 0.0.0.0/0 added", "rtb_id", rtbID, "kind", kind, "target_id", targetID)
	return nil
}

// setRouteTarget writes targetID into the appropriate route field
// pointer according to kind.
func setRouteTarget(kind, targetID string, gatewayID, networkInterfaceID **string) error {
	switch kind {
	case routeTargetGateway:
		*gatewayID = aws.String(targetID)
	case routeTargetNetworkInterface:
		*networkInterfaceID = aws.String(targetID)
	default:
		return fmt.Errorf("unknown default-route kind %q", kind)
	}
	return nil
}

// routeMatchesTarget reports whether the existing route already points
// at the desired target of the given kind.
func routeMatchesTarget(r ec2types.Route, kind, targetID string) bool {
	switch kind {
	case routeTargetGateway:
		return r.GatewayId != nil && *r.GatewayId == targetID
	case routeTargetNetworkInterface:
		return r.NetworkInterfaceId != nil && *r.NetworkInterfaceId == targetID
	default:
		return false
	}
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
