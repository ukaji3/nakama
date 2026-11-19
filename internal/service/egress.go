package service

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Kinds of the private default route's target in EgressStatus.
const (
	// EgressRouteNATInstance: the route points at a network interface
	// (the NAT instance's, ADR 0007).
	EgressRouteNATInstance = "nat-instance"
	// EgressRouteNATGateway: the route points at a NAT gateway (ADR 0026).
	EgressRouteNATGateway = "nat-gateway"
	// EgressRouteNone: the private route table has no default route.
	EgressRouteNone = "none"
	// EgressRouteOther: the route points at something else.
	EgressRouteOther = "other"
)

// EgressStatus is a read-only description of the private subnets' way to
// the internet: what init recorded, where the private default route
// points, and which NATs exist. `nakama aws doctor` reports it.
type EgressStatus struct {
	// RecordedMode is the mode the last init recorded ("" when none).
	RecordedMode string `json:"recorded_mode,omitempty"`
	// Initialized reports that the private route table exists.
	Initialized bool `json:"initialized"`
	// RouteTarget is one of the EgressRoute* kinds; RouteTargetID is the
	// network interface or NAT gateway ID it points at.
	RouteTarget   string `json:"route_target,omitempty"`
	RouteTargetID string `json:"route_target_id,omitempty"`
	// NATInstances and NATGateways are the nakama NATs that exist.
	NATInstances []EgressNATInstance `json:"nat_instances,omitempty"`
	NATGateways  []EgressNATGateway  `json:"nat_gateways,omitempty"`
}

// EgressNATInstance is a nakama NAT instance in EgressStatus.
type EgressNATInstance struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	ENIID    string `json:"eni_id,omitempty"`
	PublicIP string `json:"public_ip,omitempty"`
}

// EgressNATGateway is a nakama NAT gateway in EgressStatus.
type EgressNATGateway struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	PublicIP       string `json:"public_ip,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
}

// InspectEgress reads the egress without changing anything. It needs
// ssm:GetParameter, ec2:DescribeRouteTables, ec2:DescribeInstances and
// ec2:DescribeNatGateways.
func (s *Service) InspectEgress(ctx context.Context) (*EgressStatus, error) {
	recorded, err := s.recordedNATMode(ctx)
	if err != nil {
		return nil, err
	}
	st := &EgressStatus{RecordedMode: recorded}

	rtbID, route, err := s.privateDefaultRoute(ctx, "")
	if err != nil {
		return nil, err
	}
	st.Initialized = rtbID != ""
	switch {
	case !st.Initialized:
	case route == nil:
		st.RouteTarget = EgressRouteNone
	case route.NatGatewayId != nil:
		st.RouteTarget, st.RouteTargetID = EgressRouteNATGateway, *route.NatGatewayId
	case route.NetworkInterfaceId != nil:
		st.RouteTarget, st.RouteTargetID = EgressRouteNATInstance, *route.NetworkInterfaceId
	default:
		st.RouteTarget = EgressRouteOther
		st.RouteTargetID = firstNonEmpty(aws.ToString(route.GatewayId), aws.ToString(route.InstanceId),
			aws.ToString(route.TransitGatewayId), aws.ToString(route.VpcPeeringConnectionId))
	}

	nats, err := s.listNATInstances(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, n := range nats {
		st.NATInstances = append(st.NATInstances, EgressNATInstance{
			ID: n.InstanceID, State: string(n.State), ENIID: n.ENIID, PublicIP: n.PublicIP,
		})
	}
	gws, err := s.listNATGateways(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, g := range gws {
		st.NATGateways = append(st.NATGateways, EgressNATGateway{
			ID: g.ID, State: string(g.State), PublicIP: g.PublicIP, FailureMessage: g.FailureMessage,
		})
	}
	return st, nil
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
