package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ukaji3/nakama/internal/version"
)

// InitResult is the aggregate of every resource created or
// re-discovered during a single Init() call.
type InitResult struct {
	VPCID             string
	InternetGatewayID string
	PublicSubnetID    string            // single public subnet (1a)
	PrivateSubnetIDs  map[string]string // az name -> subnet id

	// NAT instance (ADR 0007). The private subnets egress through a
	// small NAT instance whose power state follows the code-server
	// lifecycle, replacing the always-on NAT Gateway.
	NATInstanceID    string
	NATInstanceENIID string
	NATInstanceSGID  string
	// NATForwarding is the result of the forwarding check at the end
	// of init (Issue #87): NATForwardingOK or NATForwardingRepaired.
	NATForwarding string

	// Legacy NAT Gateway migration bookkeeping. Populated only when a
	// pre-ADR-0007 NAT Gateway / EIP was found and removed during init.
	DeletedNATGatewayID     string
	ReleasedNATGatewayEIPID string

	PublicRouteTableID  string
	PrivateRouteTableID string
	WAFIPSetID          string
	WAFIPSetARN         string
	WAFWebACLID         string
	WAFWebACLARN        string
	InstanceRoleARN     string
	InstanceProfileARN  string
	// NAT instance IAM identity (separate from the code-server role:
	// SSM core only, no Parameter Store access).
	NATInstanceRoleARN    string
	NATInstanceProfileARN string
}

// Init creates or re-uses the shared nakama base infrastructure.
//
// Order of operations:
//
//  1. VPC + DNS attributes
//  2. Internet Gateway (created and attached to the VPC)
//  3. Public subnet (1a) + Private subnets (1a, 1c)
//  4. IAM instance role + instance profile (needed by the NAT instance)
//  5. NAT instance in the public subnet (created or reused, running)
//  6. Public/private route tables and routes
//     (private default route -> NAT instance ENI)
//  7. Legacy NAT Gateway migration: delete the old NAT Gateway and
//     release its EIP if a pre-ADR-0007 one is still present
//  8. WAFv2 IP set + Web ACL (CLOUDFRONT scope, us-east-1)
//  9. SSM markers under /nakama/_meta/*
//
// All steps are idempotent: existing nakama-managed resources
// (identified by tags) are reused. The function logs progress
// at slog.Info.
func (s *Service) Init(ctx context.Context) (*InitResult, error) {
	slog.Info("nakama init: starting", "version", version.String())

	res := &InitResult{
		PrivateSubnetIDs: make(map[string]string),
	}

	if err := s.initAZSanityCheck(ctx); err != nil {
		return nil, fmt.Errorf("availability-zone check: %w", err)
	}
	if err := s.initVPC(ctx, res); err != nil {
		return nil, fmt.Errorf("vpc: %w", err)
	}
	if err := s.initInternetGateway(ctx, res); err != nil {
		return nil, fmt.Errorf("internet gateway: %w", err)
	}
	if err := s.initSubnets(ctx, res); err != nil {
		return nil, fmt.Errorf("subnets: %w", err)
	}
	// IAM must precede the NAT instance: RunInstances attaches the
	// nakama instance profile created here.
	if err := s.initIAM(ctx, res); err != nil {
		return nil, fmt.Errorf("iam: %w", err)
	}
	if err := s.initNATInstance(ctx, res); err != nil {
		return nil, fmt.Errorf("nat instance: %w", err)
	}
	if err := s.initRouteTables(ctx, res); err != nil {
		return nil, fmt.Errorf("route tables: %w", err)
	}
	// Repoint egress to the NAT instance first (initRouteTables above),
	// then remove the legacy NAT Gateway so egress is never interrupted.
	if err := s.migrateLegacyNATGateway(ctx, res); err != nil {
		return nil, fmt.Errorf("legacy nat gateway migration: %w", err)
	}
	if err := s.initWAF(ctx, res); err != nil {
		return nil, fmt.Errorf("waf: %w", err)
	}
	// Last, so everything else exists when the check fails: a NAT
	// instance that does not forward makes every instance behind it
	// unreachable, and init must not report success then (Issue #87).
	if err := s.initNATForwarding(ctx, res); err != nil {
		return nil, fmt.Errorf("nat instance forwarding: %w", err)
	}
	if err := s.initSSMMarkers(ctx); err != nil {
		return nil, fmt.Errorf("ssm markers: %w", err)
	}

	slog.Info("nakama init: completed",
		"vpc", res.VPCID,
		"public_subnet", res.PublicSubnetID,
		"nat_instance", res.NATInstanceID,
		"nat_instance_eni", res.NATInstanceENIID,
		"nat_forwarding", res.NATForwarding,
		"deleted_legacy_nat_gateway", res.DeletedNATGatewayID,
		"waf_web_acl", res.WAFWebACLID,
		"instance_profile", res.InstanceProfileARN,
	)
	return res, nil
}
