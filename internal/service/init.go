package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ukaji3/nakama/internal/version"
)

// InitInput captures the options of `nakama aws init`.
type InitInput struct {
	// NATMode is the egress mode to set up: NATModeInstance or
	// NATModeGateway (ADR 0026). Empty decides as PlanInit does: the
	// recorded mode, the NAT instance for an environment set up before
	// ADR 0026, the NAT gateway for a new one.
	NATMode string
}

// InitResult is the aggregate of every resource created or
// re-discovered during a single Init() call.
type InitResult struct {
	VPCID             string
	InternetGatewayID string
	PublicSubnetID    string            // single public subnet (1a)
	PrivateSubnetIDs  map[string]string // az name -> subnet id

	// NATMode is the egress mode init set up (ADR 0026):
	// NATModeInstance or NATModeGateway.
	NATMode string

	// NAT instance (ADR 0007, instance mode). The private subnets egress
	// through a small NAT instance whose power state follows the
	// code-server lifecycle.
	NATInstanceID    string
	NATInstanceENIID string
	NATInstanceSGID  string
	// NATForwarding is the result of the NAT instance's forwarding check
	// (Issue #87): NATForwardingOK or NATForwardingRepaired.
	NATForwarding string

	// NAT gateway (ADR 0026, gateway mode): a zonal NAT gateway with an
	// Elastic IP in the public subnet, always on.
	NATGatewayID              string
	NATGatewayEIPAllocationID string
	// NATGatewayPublicIP is the address the instances reach the internet
	// from in gateway mode.
	NATGatewayPublicIP string

	// Clean-up of the NAT the mode does not use. The instance mode
	// deletes the NAT gateways and releases their Elastic IPs (this also
	// migrates an environment set up before ADR 0007); the gateway mode
	// terminates the NAT instance and deletes its security group. Each
	// holds the IDs removed in this run, comma-separated.
	DeletedNATGatewayID     string
	ReleasedNATGatewayEIPID string
	TerminatedNATInstanceID string
	DeletedNATInstanceSGID  string

	PublicRouteTableID  string
	PrivateRouteTableID string
	WAFIPSetID          string
	WAFIPSetARN         string
	WAFWebACLID         string
	WAFWebACLARN        string
	InstanceRoleARN     string
	InstanceProfileARN  string
	// NAT instance IAM identity (separate from the code-server role:
	// SSM core only, no Parameter Store access). Set up in instance
	// mode only; the gateway mode keeps an existing one for a switch
	// back and creates none.
	NATInstanceRoleARN    string
	NATInstanceProfileARN string
}

// Init creates or re-uses the shared nakama base infrastructure.
//
// Order of operations:
//
//  1. Egress mode: decided (InitInput.NATMode, PlanInit) and recorded
//     under /nakama/_meta/nat-mode before anything changes
//  2. VPC + DNS attributes
//  3. Internet Gateway (created and attached to the VPC)
//  4. Public subnet (1a) + Private subnets (1a, 1c)
//  5. IAM instance role + instance profile (and, in instance mode, the
//     NAT instance's)
//  6. Egress (initEgress): the NAT of the mode, the route tables with
//     the private default route pointing at it, then the removal of
//     the other NAT
//  7. WAFv2 IP set + Web ACL (CLOUDFRONT scope, us-east-1)
//  8. Instance mode: the NAT instance's forwarding check (unless it ran
//     in step 6 before a switch)
//  9. SSM markers under /nakama/_meta/*
//
// All steps are idempotent: existing nakama-managed resources
// (identified by tags) are reused, and a run that stopped halfway is
// finished by the next one. The function logs progress at slog.Info.
func (s *Service) Init(ctx context.Context, in InitInput) (*InitResult, error) {
	slog.Info("nakama init: starting", "version", version.String())

	res := &InitResult{
		PrivateSubnetIDs: make(map[string]string),
	}

	if err := s.initAZSanityCheck(ctx); err != nil {
		return nil, fmt.Errorf("availability-zone check: %w", err)
	}
	plan, err := s.planNATMode(ctx, in.NATMode)
	if err != nil {
		return nil, fmt.Errorf("nat mode: %w", err)
	}
	res.NATMode = plan.NATMode
	if plan.Switch {
		slog.Warn("nakama init: switching the egress", "from", plan.From, "to", plan.NATMode)
	}
	// Record first: a run that stops halfway (an error, Ctrl-C) is
	// finished in the same mode by the next init, and a new environment
	// is no longer "new" once its VPC exists.
	if err := s.writeNATMode(ctx, res.NATMode); err != nil {
		return nil, fmt.Errorf("nat mode: %w", err)
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
	forwardingChecked, err := s.initEgress(ctx, res)
	if err != nil {
		return nil, err
	}
	if err := s.initWAF(ctx, res); err != nil {
		return nil, fmt.Errorf("waf: %w", err)
	}
	// Last, so everything else exists when the check fails: a NAT
	// instance that does not forward makes every instance behind it
	// unreachable, and init must not report success then (Issue #87).
	if res.NATMode == NATModeInstance && !forwardingChecked {
		if err := s.initNATForwarding(ctx, res); err != nil {
			return nil, fmt.Errorf("nat instance forwarding: %w", err)
		}
	}
	if err := s.initSSMMarkers(ctx); err != nil {
		return nil, fmt.Errorf("ssm markers: %w", err)
	}

	slog.Info("nakama init: completed",
		"vpc", res.VPCID,
		"public_subnet", res.PublicSubnetID,
		"nat_mode", res.NATMode,
		"nat_instance", res.NATInstanceID,
		"nat_forwarding", res.NATForwarding,
		"nat_gateway", res.NATGatewayID,
		"nat_gateway_public_ip", res.NATGatewayPublicIP,
		"deleted_nat_gateway", res.DeletedNATGatewayID,
		"terminated_nat_instance", res.TerminatedNATInstanceID,
		"waf_web_acl", res.WAFWebACLID,
		"instance_profile", res.InstanceProfileARN,
	)
	return res, nil
}

// initEgress sets up the private subnets' way to the internet for
// res.NATMode (ADR 0026). The order keeps egress working throughout a
// switch: the new NAT is built and checked first, then the private
// default route is repointed with ReplaceRoute (connections through the
// old NAT drop at this moment), and only then is the other NAT removed.
// A failure before the route moves leaves the old NAT in charge; the
// next init carries on from where this one stopped.
//
// It reports whether the NAT instance's forwarding was already checked:
// when the route moves from a NAT gateway to the NAT instance, the check
// runs before the route moves, so a NAT instance that does not forward
// never takes over the egress.
func (s *Service) initEgress(ctx context.Context, res *InitResult) (forwardingChecked bool, err error) {
	switch res.NATMode {
	case NATModeInstance:
		if err := s.initNATInstance(ctx, res); err != nil {
			return false, fmt.Errorf("nat instance: %w", err)
		}
		_, route, err := s.privateDefaultRoute(ctx, res.VPCID)
		if err != nil {
			return false, fmt.Errorf("route tables: %w", err)
		}
		if route != nil && route.NatGatewayId != nil {
			slog.Info("nat instance: checking its forwarding before the private route moves to it")
			if err := s.initNATForwarding(ctx, res); err != nil {
				return false, fmt.Errorf("nat instance forwarding: %w", err)
			}
			forwardingChecked = true
		}
		if err := s.initRouteTables(ctx, res, routeTargetNetworkInterface, res.NATInstanceENIID); err != nil {
			return forwardingChecked, fmt.Errorf("route tables: %w", err)
		}
		if err := s.removeNATGateway(ctx, res); err != nil {
			return forwardingChecked, fmt.Errorf("nat gateway removal: %w", err)
		}
		return forwardingChecked, nil

	case NATModeGateway:
		if err := s.initNATGateway(ctx, res); err != nil {
			return false, fmt.Errorf("nat gateway: %w", err)
		}
		if err := s.initRouteTables(ctx, res, routeTargetNATGateway, res.NATGatewayID); err != nil {
			return false, fmt.Errorf("route tables: %w", err)
		}
		if err := s.removeNATInstance(ctx, res); err != nil {
			return false, fmt.Errorf("nat instance removal: %w", err)
		}
		return false, nil

	default:
		return false, ValidateNATMode(res.NATMode)
	}
}
