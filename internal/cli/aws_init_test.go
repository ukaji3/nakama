package cli

import (
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

func TestInitCommand_Flags(t *testing.T) {
	t.Parallel()
	cmd := newAWSInitCommand()
	for _, name := range []string{"nat", "yes"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
	// An unknown --nat fails before any AWS call (no credentials here).
	cmd.SetArgs([]string{"--nat", "regional"})
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--nat") || !strings.Contains(err.Error(), "unknown NAT mode") {
		t.Fatalf("expected the --nat validation error, got %v", err)
	}
}

func TestInitSwitchPrompt(t *testing.T) {
	t.Parallel()
	toGateway := initSwitchPrompt(&service.InitPlan{
		NATMode: service.NATModeGateway, From: service.NATModeInstance, Switch: true,
		ActiveInstances: []string{"dev01", "dev02"},
	})
	for _, want := range []string{
		"from the NAT instance to a NAT gateway", "Elastic IP", "terminate the NAT instance",
		"IAM role and instance profile are kept", "charged hourly", "also when every instance is stopped",
		"Running instances: dev01, dev02", "connect / ssh / tunnel sessions", "drop", "[y/N]: ",
	} {
		if !strings.Contains(toGateway, want) {
			t.Errorf("to-gateway prompt lacks %q:\n%s", want, toGateway)
		}
	}

	toInstance := initSwitchPrompt(&service.InitPlan{
		NATMode: service.NATModeInstance, From: service.NATModeGateway, Switch: true,
	})
	for _, want := range []string{
		"from the NAT gateway to the NAT instance", "check that it forwards",
		"delete the NAT gateway and release its Elastic IP", "egress IP address changes", "[y/N]: ",
	} {
		if !strings.Contains(toInstance, want) {
			t.Errorf("to-instance prompt lacks %q:\n%s", want, toInstance)
		}
	}
	if strings.Contains(toInstance, "Running instances") {
		t.Errorf("no running instance, no session warning:\n%s", toInstance)
	}
}

func TestInitSummary(t *testing.T) {
	t.Parallel()
	gw := initSummary(&service.InitResult{
		NATMode: service.NATModeGateway, NATGatewayID: "nat-1", NATGatewayEIPAllocationID: "eipalloc-1",
		NATGatewayPublicIP: "203.0.113.10", TerminatedNATInstanceID: "i-nat", DeletedNATInstanceSGID: "sg-nat",
	})
	for _, want := range []string{"nat mode", "gateway", "nat-1", "eipalloc-1", "egress ip", "203.0.113.10", "terminated nat", "i-nat", "sg-nat"} {
		if !strings.Contains(gw, want) {
			t.Errorf("gateway summary lacks %q:\n%s", want, gw)
		}
	}
	for _, notWant := range []string{"nat forwarding", "nat iam role", "deleted natgw"} {
		if strings.Contains(gw, notWant) {
			t.Errorf("gateway summary shows %q:\n%s", notWant, gw)
		}
	}

	inst := initSummary(&service.InitResult{
		NATMode: service.NATModeInstance, NATInstanceID: "i-nat", NATForwarding: service.NATForwardingOK,
		NATInstanceRoleARN: "arn:role", DeletedNATGatewayID: "nat-1", ReleasedNATGatewayEIPID: "eipalloc-1",
	})
	for _, want := range []string{"instance", "i-nat", "nat forwarding", "nat iam role", "deleted natgw", "nat-1", "released eip", "eipalloc-1"} {
		if !strings.Contains(inst, want) {
			t.Errorf("instance summary lacks %q:\n%s", want, inst)
		}
	}
	if strings.Contains(inst, "egress ip") {
		t.Errorf("instance summary shows the NAT gateway fields:\n%s", inst)
	}
}
