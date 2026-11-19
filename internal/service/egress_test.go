package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/naming"
)

func TestInspectEgress(t *testing.T) {
	t.Parallel()
	env := newInfraEnv("", nil).
		withNATGateway("nat-1", ec2types.NatGatewayStateAvailable).
		withNATInstance("i-nat", ec2types.InstanceStateNameStopped).
		withRouteTables(ec2types.Route{NatGatewayId: aws.String("nat-1")})
	env.ssm.params[naming.SSMMetaNATMode()] = NATModeGateway

	st, err := env.svc.InspectEgress(context.Background())
	if err != nil {
		t.Fatalf("InspectEgress: %v", err)
	}
	if st.RecordedMode != NATModeGateway || !st.Initialized || st.RouteTarget != EgressRouteNATGateway || st.RouteTargetID != "nat-1" {
		t.Errorf("status = %+v", st)
	}
	if len(st.NATGateways) != 1 || st.NATGateways[0].PublicIP != "198.51.100.7" ||
		len(st.NATInstances) != 1 || st.NATInstances[0].ENIID != "eni-i-nat" {
		t.Errorf("NATs = %+v / %+v", st.NATGateways, st.NATInstances)
	}
	if len(env.ec2.calls) != 0 {
		t.Errorf("InspectEgress must not change anything: %v", env.ec2.calls)
	}

	// Before init: no route table.
	st, err = newInfraEnv("", nil).svc.InspectEgress(context.Background())
	if err != nil || st.Initialized || st.RouteTarget != "" {
		t.Errorf("before init: %+v, %v", st, err)
	}
}

func TestEvaluateEgress(t *testing.T) {
	t.Parallel()
	gw := func(state string) EgressNATGateway {
		return EgressNATGateway{ID: "nat-1", State: state, PublicIP: "203.0.113.10"}
	}
	nat := EgressNATInstance{ID: "i-nat", State: "stopped", ENIID: "eni-nat"}
	cases := []struct {
		name       string
		st         EgressStatus
		want       CheckStatus
		wantDetail []string
		wantAdvice string
	}{
		{name: "not initialized", st: EgressStatus{},
			want: CheckWarn, wantDetail: []string{"does not exist yet"}, wantAdvice: "nakama aws init"},
		{name: "gateway healthy",
			st: EgressStatus{RecordedMode: NATModeGateway, Initialized: true, RouteTarget: EgressRouteNATGateway,
				RouteTargetID: "nat-1", NATGateways: []EgressNATGateway{gw("available")}},
			want: CheckOK, wantDetail: []string{"gateway mode (recorded)", "nat-1", "available", "203.0.113.10"}},
		{name: "instance healthy without a record",
			st: EgressStatus{Initialized: true, RouteTarget: EgressRouteNATInstance, RouteTargetID: "eni-nat",
				NATInstances: []EgressNATInstance{nat}},
			want: CheckOK, wantDetail: []string{"instance mode (none recorded; treated as instance)", "i-nat", "stopped"}},
		{name: "gateway failed",
			st: EgressStatus{RecordedMode: NATModeGateway, Initialized: true, RouteTarget: EgressRouteNATGateway,
				RouteTargetID: "nat-1", NATGateways: []EgressNATGateway{{ID: "nat-1", State: "failed", FailureMessage: "no capacity"}}},
			want: CheckFail, wantDetail: []string{"nat-1 is failed (no capacity)"}, wantAdvice: "replace it"},
		{name: "route to a gone NAT gateway",
			st:   EgressStatus{RecordedMode: NATModeGateway, Initialized: true, RouteTarget: EgressRouteNATGateway, RouteTargetID: "nat-gone"},
			want: CheckFail, wantDetail: []string{"nat-gone", "gone"}, wantAdvice: "nakama aws init"},
		{name: "route to a gone NAT instance",
			st:   EgressStatus{Initialized: true, RouteTarget: EgressRouteNATInstance, RouteTargetID: "eni-gone"},
			want: CheckFail, wantDetail: []string{"eni-gone"}, wantAdvice: "nakama aws init"},
		{name: "no default route",
			st:   EgressStatus{Initialized: true, RouteTarget: EgressRouteNone},
			want: CheckFail, wantDetail: []string{"no default route"}},
		{name: "default route elsewhere",
			st:   EgressStatus{Initialized: true, RouteTarget: EgressRouteOther, RouteTargetID: "igw-1"},
			want: CheckFail, wantDetail: []string{"igw-1"}},
		{name: "leftover NAT instance in gateway mode",
			st: EgressStatus{RecordedMode: NATModeGateway, Initialized: true, RouteTarget: EgressRouteNATGateway,
				RouteTargetID: "nat-1", NATGateways: []EgressNATGateway{gw("available")}, NATInstances: []EgressNATInstance{nat}},
			want: CheckWarn, wantDetail: []string{"i-nat (stopped) still exists"}, wantAdvice: "terminate"},
		{name: "leftover NAT gateway in instance mode",
			st: EgressStatus{RecordedMode: NATModeInstance, Initialized: true, RouteTarget: EgressRouteNATInstance,
				RouteTargetID: "eni-nat", NATInstances: []EgressNATInstance{nat}, NATGateways: []EgressNATGateway{gw("available")}},
			want: CheckWarn, wantDetail: []string{"nat-1 (available) still exists", "charged"}, wantAdvice: "delete it"},
		{name: "switch not finished",
			st: EgressStatus{RecordedMode: NATModeGateway, Initialized: true, RouteTarget: EgressRouteNATInstance,
				RouteTargetID: "eni-nat", NATInstances: []EgressNATInstance{nat}},
			want: CheckWarn, wantDetail: []string{"set to the gateway mode", "goes through the NAT instance"},
			wantAdvice: "finish the switch"},
		{name: "a fail outranks a warn",
			st: EgressStatus{RecordedMode: NATModeInstance, Initialized: true, RouteTarget: EgressRouteNATGateway,
				RouteTargetID: "nat-1", NATGateways: []EgressNATGateway{gw("failed")}},
			want: CheckFail, wantAdvice: "replace it"},
	}
	for _, c := range cases {
		got := evaluateEgress(&c.st)
		if got.Name != "egress" || got.Status != c.want {
			t.Errorf("%s: status %q, want %q (%+v)", c.name, got.Status, c.want, got)
		}
		for _, w := range c.wantDetail {
			if !strings.Contains(got.Detail, w) {
				t.Errorf("%s: detail lacks %q: %s", c.name, w, got.Detail)
			}
		}
		if !strings.Contains(got.Advice, c.wantAdvice) {
			t.Errorf("%s: advice %q lacks %q", c.name, got.Advice, c.wantAdvice)
		}
	}
}

func TestDiagnoseEgress(t *testing.T) {
	t.Parallel()
	opts := doctorAWSOpts(t, nil, "203.0.113.77", nil, &fakeDoctorIAM{})
	if c := checkByName(t, Diagnose(context.Background(), opts), "egress"); c.Status != CheckOK || !strings.Contains(c.Detail, "nat-1") {
		t.Errorf("healthy egress: %+v", c)
	}
	opts.egress = func(context.Context) (*EgressStatus, error) { return nil, errors.New("AccessDenied") }
	c := checkByName(t, Diagnose(context.Background(), opts), "egress")
	if c.Status != CheckWarn || !strings.Contains(c.Detail, "AccessDenied") || !strings.Contains(c.Advice, "ec2:DescribeNatGateways") {
		t.Errorf("unreadable egress: %+v", c)
	}
}
