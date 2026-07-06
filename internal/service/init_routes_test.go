package service

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakeRouteEC2 is a minimal EC2API mock for the route-table tests.
type fakeRouteEC2 struct {
	awsx.EC2API

	routes []ec2types.Route // current routes on the single route table

	createInputs  []*ec2.CreateRouteInput
	replaceInputs []*ec2.ReplaceRouteInput
}

func (f *fakeRouteEC2) DescribeRouteTables(_ context.Context, _ *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{
		RouteTables: []ec2types.RouteTable{{
			RouteTableId: aws.String("rtb-1"),
			Routes:       f.routes,
		}},
	}, nil
}

func (f *fakeRouteEC2) CreateRoute(_ context.Context, in *ec2.CreateRouteInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error) {
	f.createInputs = append(f.createInputs, in)
	return &ec2.CreateRouteOutput{}, nil
}

func (f *fakeRouteEC2) ReplaceRoute(_ context.Context, in *ec2.ReplaceRouteInput, _ ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error) {
	f.replaceInputs = append(f.replaceInputs, in)
	return &ec2.ReplaceRouteOutput{}, nil
}

func TestEnsureDefaultRoute_CreatesNetworkInterfaceRoute(t *testing.T) {
	t.Parallel()
	f := &fakeRouteEC2{routes: nil} // no existing default route
	s := newServiceWithEC2(f)

	if err := s.ensureDefaultRoute(context.Background(), "rtb-1", routeTargetNetworkInterface, "eni-nat"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if len(f.createInputs) != 1 {
		t.Fatalf("CreateRoute called %d times, want 1", len(f.createInputs))
	}
	if len(f.replaceInputs) != 0 {
		t.Fatalf("ReplaceRoute called %d times, want 0", len(f.replaceInputs))
	}
	in := f.createInputs[0]
	if in.NetworkInterfaceId == nil || *in.NetworkInterfaceId != "eni-nat" {
		t.Errorf("CreateRoute NetworkInterfaceId = %v, want eni-nat", in.NetworkInterfaceId)
	}
	if in.NatGatewayId != nil {
		t.Errorf("CreateRoute NatGatewayId should be nil, got %v", in.NatGatewayId)
	}
}

func TestEnsureDefaultRoute_ReplacesLegacyNATGatewayRoute(t *testing.T) {
	t.Parallel()
	// Existing 0.0.0.0/0 route points at the legacy NAT Gateway.
	f := &fakeRouteEC2{routes: []ec2types.Route{{
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String("nat-legacy"),
	}}}
	s := newServiceWithEC2(f)

	if err := s.ensureDefaultRoute(context.Background(), "rtb-1", routeTargetNetworkInterface, "eni-nat"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if len(f.replaceInputs) != 1 {
		t.Fatalf("ReplaceRoute called %d times, want 1", len(f.replaceInputs))
	}
	if len(f.createInputs) != 0 {
		t.Fatalf("CreateRoute called %d times, want 0", len(f.createInputs))
	}
	in := f.replaceInputs[0]
	if in.NetworkInterfaceId == nil || *in.NetworkInterfaceId != "eni-nat" {
		t.Errorf("ReplaceRoute NetworkInterfaceId = %v, want eni-nat", in.NetworkInterfaceId)
	}
}

func TestEnsureDefaultRoute_NoopWhenAlreadyPointingToENI(t *testing.T) {
	t.Parallel()
	f := &fakeRouteEC2{routes: []ec2types.Route{{
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NetworkInterfaceId:   aws.String("eni-nat"),
	}}}
	s := newServiceWithEC2(f)

	if err := s.ensureDefaultRoute(context.Background(), "rtb-1", routeTargetNetworkInterface, "eni-nat"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if len(f.createInputs) != 0 || len(f.replaceInputs) != 0 {
		t.Errorf("expected no route mutation, got create=%d replace=%d",
			len(f.createInputs), len(f.replaceInputs))
	}
}

func TestEnsureDefaultRoute_GatewayKindSetsGatewayId(t *testing.T) {
	t.Parallel()
	f := &fakeRouteEC2{routes: nil}
	s := newServiceWithEC2(f)

	if err := s.ensureDefaultRoute(context.Background(), "rtb-1", routeTargetGateway, "igw-1"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if len(f.createInputs) != 1 {
		t.Fatalf("CreateRoute called %d times, want 1", len(f.createInputs))
	}
	if in := f.createInputs[0]; in.GatewayId == nil || *in.GatewayId != "igw-1" {
		t.Errorf("CreateRoute GatewayId = %v, want igw-1", in.GatewayId)
	}
}
