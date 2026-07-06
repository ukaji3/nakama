package service

import (
	"context"
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

// migrateLegacyNATGateway removes a pre-ADR-0007 NAT Gateway and releases
// its Elastic IP if either is still present. nakama used to provision a
// NAT Gateway (always-on, fixed hourly cost); ADR 0007 replaces it with a
// NAT instance whose power state follows the code-server lifecycle.
//
// This runs AFTER initRouteTables has repointed the private default route
// to the NAT instance ENI, so egress is never interrupted by the deletion.
//
// The operation is idempotent: when no legacy NAT Gateway / EIP is found it
// is a no-op, so repeated `nakama aws init` runs are safe.
func (s *Service) migrateLegacyNATGateway(ctx context.Context, res *InitResult) error {
	natID, state, err := s.findNATGateway(ctx, res.VPCID)
	if err != nil {
		return err
	}
	if natID != "" {
		slog.Warn("legacy nat gateway: deleting to eliminate fixed hourly cost (ADR 0007 migration)",
			"natgw_id", natID, "state", state)
		if _, err := s.clients.EC2.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{
			NatGatewayId: aws.String(natID),
		}); err != nil {
			if !awshelper.IsAPIError(err, "NatGatewayNotFound") {
				return fmt.Errorf("DeleteNatGateway: %w", err)
			}
		} else {
			res.DeletedNATGatewayID = natID
			if err := s.waitNATGatewayDeleted(ctx, natID); err != nil {
				return err
			}
		}
	}

	// Release the EIP that was reserved for the legacy NAT Gateway so the
	// unattached-address charge does not persist.
	allocID, err := s.findLegacyNATGatewayEIP(ctx)
	if err != nil {
		return err
	}
	if allocID != "" {
		slog.Warn("legacy nat gateway eip: releasing", "allocation_id", allocID)
		if _, err := s.clients.EC2.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
			AllocationId: aws.String(allocID),
		}); err != nil {
			// A just-deleted NAT Gateway can retain the EIP briefly; treat
			// "in use" as non-fatal and let a later init retry the release.
			if awshelper.IsAPIError(err, "InvalidIPAddress.InUse") {
				slog.Warn("legacy nat gateway eip: still in use, will retry on next init",
					"allocation_id", allocID)
				return nil
			}
			if awshelper.IsAPIError(err, "InvalidAllocationID.NotFound") {
				return nil
			}
			return fmt.Errorf("ReleaseAddress: %w", err)
		}
		res.ReleasedNATGatewayEIPID = allocID
		slog.Info("legacy nat gateway eip: released", "allocation_id", allocID)
	}
	return nil
}

func (s *Service) waitNATGatewayDeleted(ctx context.Context, natID string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    10 * time.Second,
		Timeout:     5 * time.Minute,
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
		if len(out.NatGateways) == 0 {
			return true, nil
		}
		st := out.NatGateways[0].State
		if st == ec2types.NatGatewayStateDeleted {
			return true, nil
		}
		slog.Info("legacy nat gateway: still deleting", "natgw_id", natID, "state", st)
		return false, nil
	})
}

// findNATGateway returns (id, state) of the existing nakama-managed NAT
// Gateway in the given VPC, or ("", "") if none. Deleted/deleting NAT
// gateways are skipped.
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

// findLegacyNATGatewayEIP returns the AllocationId of the EIP that was
// reserved for the legacy NAT Gateway, or "" if none is tagged as ours.
func (s *Service) findLegacyNATGatewayEIP(ctx context.Context) (string, error) {
	out, err := s.clients.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: nakamaTagFilters(map[string]string{
			tagging.NameTag: naming.NATGWEIP(),
		}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeAddresses: %w", err)
	}
	if len(out.Addresses) > 0 && out.Addresses[0].AllocationId != nil {
		return *out.Addresses[0].AllocationId, nil
	}
	return "", nil
}
