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
	VPCID               string
	InternetGatewayID   string
	PublicSubnetID      string            // single public subnet (1a)
	PrivateSubnetIDs    map[string]string // az name -> subnet id
	NATGatewayID        string
	NATGatewayEIPAlloc  string
	PublicRouteTableID  string
	PrivateRouteTableID string
	WAFIPSetID          string
	WAFIPSetARN         string
	WAFWebACLID         string
	WAFWebACLARN        string
	InstanceRoleARN     string
	InstanceProfileARN  string
}

// Init creates or re-uses the shared nakama base infrastructure.
//
// Order of operations:
//
//  1. VPC + DNS attributes
//  2. Internet Gateway (created and attached to the VPC)
//  3. Public subnet (1a) + Private subnets (1a, 1c)
//  4. EIP + NAT Gateway in the public subnet
//  5. Public/private route tables and routes
//  6. WAFv2 IP set + Web ACL (CLOUDFRONT scope, us-east-1)
//  7. IAM instance role + instance profile
//  8. SSM markers under /nakama/_meta/*
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
	if err := s.initNATGateway(ctx, res); err != nil {
		return nil, fmt.Errorf("nat gateway: %w", err)
	}
	if err := s.initRouteTables(ctx, res); err != nil {
		return nil, fmt.Errorf("route tables: %w", err)
	}
	if err := s.initWAF(ctx, res); err != nil {
		return nil, fmt.Errorf("waf: %w", err)
	}
	if err := s.initIAM(ctx, res); err != nil {
		return nil, fmt.Errorf("iam: %w", err)
	}
	if err := s.initSSMMarkers(ctx); err != nil {
		return nil, fmt.Errorf("ssm markers: %w", err)
	}

	slog.Info("nakama init: completed",
		"vpc", res.VPCID,
		"public_subnet", res.PublicSubnetID,
		"nat_gateway", res.NATGatewayID,
		"waf_web_acl", res.WAFWebACLID,
		"instance_profile", res.InstanceProfileARN,
	)
	return res, nil
}
