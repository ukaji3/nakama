package service

import (
	"context"
	"slices"
	"strings"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/naming"
)

func TestValidateNATMode(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{NATModeInstance, NATModeGateway} {
		if err := ValidateNATMode(ok); err != nil {
			t.Errorf("ValidateNATMode(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Gateway", "nat-gateway", "regional"} {
		if err := ValidateNATMode(bad); err == nil {
			t.Errorf("ValidateNATMode(%q) accepted", bad)
		}
	}
}

func TestPlanInit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		vpc       bool
		recorded  string
		requested string
		// running and stopped are code-server instances by name.
		running, stopped []string

		wantMode   string
		wantNew    bool
		wantSwitch bool
		wantFrom   string
		wantActive []string
	}{
		{name: "new environment defaults to the NAT gateway",
			wantMode: NATModeGateway, wantNew: true},
		{name: "new environment with --nat instance",
			requested: NATModeInstance, wantMode: NATModeInstance, wantNew: true},
		{name: "existing environment without a record keeps the NAT instance", vpc: true,
			running:  []string{"dev01"},
			wantMode: NATModeInstance},
		{name: "existing environment keeps the recorded gateway", vpc: true, recorded: NATModeGateway,
			wantMode: NATModeGateway},
		{name: "--nat equal to the record is no switch", vpc: true, recorded: NATModeGateway, requested: NATModeGateway,
			wantMode: NATModeGateway},
		{name: "switch an unrecorded environment to the gateway", vpc: true, requested: NATModeGateway,
			running: []string{"dev02", "dev01"}, stopped: []string{"dev03"},
			wantMode: NATModeGateway, wantSwitch: true, wantFrom: NATModeInstance, wantActive: []string{"dev01", "dev02"}},
		{name: "switch back to the NAT instance", vpc: true, recorded: NATModeGateway, requested: NATModeInstance,
			stopped:  []string{"dev01"},
			wantMode: NATModeInstance, wantSwitch: true, wantFrom: NATModeGateway},
		{name: "a record without the VPC is kept for the new environment", recorded: NATModeInstance,
			wantMode: NATModeInstance, wantNew: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := newInfraEnv("", nil)
			env.ec2.vpcExists = c.vpc
			for _, n := range c.running {
				env.withCodeServer(n, ec2types.InstanceStateNameRunning)
			}
			for _, n := range c.stopped {
				env.withCodeServer(n, ec2types.InstanceStateNameStopped)
			}
			// The NAT instance is no code-server instance.
			env.withNATInstance("i-nat", ec2types.InstanceStateNameRunning)
			if c.recorded != "" {
				env.ssm.params[naming.SSMMetaNATMode()] = c.recorded
			}
			p, err := env.svc.PlanInit(context.Background(), c.requested)
			if err != nil {
				t.Fatalf("PlanInit: %v", err)
			}
			if p.NATMode != c.wantMode || p.NewEnvironment != c.wantNew || p.Switch != c.wantSwitch ||
				p.From != c.wantFrom || !slices.Equal(p.ActiveInstances, c.wantActive) || p.RecordedNATMode != c.recorded {
				t.Errorf("plan = %+v", p)
			}
			if len(env.ssm.params) != map[bool]int{true: 1, false: 0}[c.recorded != ""] {
				t.Errorf("PlanInit must not write anything: %v", env.ssm.params)
			}
		})
	}
}

func TestPlanInitRejects(t *testing.T) {
	t.Parallel()
	env := newInfraEnv("", nil)
	if _, err := env.svc.PlanInit(context.Background(), "regional"); err == nil || !strings.Contains(err.Error(), "unknown NAT mode") {
		t.Errorf("an unknown --nat: %v", err)
	}

	// A mode written by a newer nakama is not guessed at.
	env.ssm.params[naming.SSMMetaNATMode()] = "regional"
	_, err := env.svc.PlanInit(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "newer version") || !strings.Contains(err.Error(), naming.SSMMetaNATMode()) {
		t.Errorf("an unknown recorded mode: %v", err)
	}
}

func TestWriteNATMode(t *testing.T) {
	t.Parallel()
	env := newInfraEnv("", nil)
	if err := env.svc.writeNATMode(context.Background(), NATModeGateway); err != nil {
		t.Fatal(err)
	}
	if got := env.ssm.params[naming.SSMMetaNATMode()]; got != NATModeGateway {
		t.Errorf("recorded %q", got)
	}
	got, err := env.svc.recordedNATMode(context.Background())
	if err != nil || got != NATModeGateway {
		t.Errorf("recordedNATMode = %q, %v", got, err)
	}
}
