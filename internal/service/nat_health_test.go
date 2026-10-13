package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakeNATSSM plays the NAT instance behind Run Command: the check
// script reports health from the fields, and the repair script flips
// them after repairPolls checks (the repair runs in the background).
type fakeNATSSM struct {
	awsx.SSMAPI

	mu sync.Mutex
	// notRegistered makes the first SendCommand calls fail with
	// InvalidInstanceId, as right after the instance starts.
	notRegistered int
	healthy       bool
	reasons       string
	// repairFixes says whether the repair succeeds; repairPolls is how
	// many checks report it still running.
	repairFixes bool
	repairPolls int
	repairing   bool
	repairState string
	checks      int
	repairs     int
	lastScript  string
}

func (f *fakeNATSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notRegistered > 0 {
		f.notRegistered--
		return nil, &smithy.GenericAPIError{Code: "InvalidInstanceId", Message: "Instances not in a valid state"}
	}
	f.lastScript = strings.Join(in.Parameters["commands"], "\n")
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd")}}, nil
}

func (f *fakeNATSSM) GetCommandInvocation(_ context.Context, _ *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out string
	switch {
	case strings.Contains(f.lastScript, "systemd-run --unit=nakama-nat-repair"):
		f.repairs++
		f.repairing, f.repairState = true, "active"
		out = "NAKAMA_REPAIR=started\n"
	case strings.Contains(f.lastScript, "NAKAMA_NAT_OK"):
		f.checks++
		if f.repairing {
			if f.repairPolls > 0 {
				f.repairPolls--
			} else {
				f.repairing = false
				if f.repairFixes {
					f.healthy, f.reasons, f.repairState = true, "", "inactive"
				} else {
					f.repairState = "failed"
				}
			}
		}
		out = "NAKAMA_NAT_REPAIR=" + f.repairState + "\n"
		if f.healthy {
			out += "NAKAMA_NAT_OK=1\n"
		} else {
			out += "NAKAMA_NAT_OK=0\nNAKAMA_NAT_REASONS=" + f.reasons + "\n"
		}
		if f.repairState == "failed" {
			out += "NAKAMA_NAT_LOG=nakama-nat: dnf --releasever=latest upgrade failed (attempt 3/3)\n"
		}
	default:
		return nil, errors.New("unexpected script")
	}
	return &ssm.GetCommandInvocationOutput{
		Status:                ssmtypes.CommandInvocationStatusSuccess,
		StandardOutputContent: aws.String(out),
	}, nil
}

// fastNATTimers shortens the check's waits for the test.
func fastNATTimers(t *testing.T) {
	t.Helper()
	pi, rt, at := natRepairPollInterval, natRepairTimeout, natAgentWaitTimeout
	natRepairPollInterval, natRepairTimeout, natAgentWaitTimeout = time.Millisecond, 2*time.Second, 2*time.Second
	t.Cleanup(func() { natRepairPollInterval, natRepairTimeout, natAgentWaitTimeout = pi, rt, at })
}

func newNATForwardingService(ec2 *fakeNATEC2, ssmFake *fakeNATSSM) *Service {
	return &Service{clients: &awsx.Clients{EC2: ec2, SSM: ssmFake}}
}

func TestInitNATForwardingHealthyChangesNothing(t *testing.T) {
	fastNATTimers(t)
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning, "i-dev")
	fake := &fakeNATSSM{healthy: true}
	res := &InitResult{}
	if err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	if res.NATForwarding != NATForwardingOK {
		t.Errorf("NATForwarding = %q", res.NATForwarding)
	}
	if fake.repairs != 0 || len(ec2.started) != 0 || len(ec2.stopped) != 0 {
		t.Errorf("a healthy running NAT must not be touched: repairs=%d started=%v stopped=%v", fake.repairs, ec2.started, ec2.stopped)
	}
}

func TestInitNATForwardingRepairs(t *testing.T) {
	fastNATTimers(t)
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning, "i-dev")
	fake := &fakeNATSSM{reasons: "iptables_missing service_inactive", repairFixes: true, repairPolls: 3}
	res := &InitResult{}
	if err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), res); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if res.NATForwarding != NATForwardingRepaired {
		t.Errorf("NATForwarding = %q, want repaired", res.NATForwarding)
	}
	if fake.repairs != 1 {
		t.Errorf("repairs = %d, want 1", fake.repairs)
	}
	if fake.checks < 5 {
		t.Errorf("checks = %d: init must keep checking until the repair is done", fake.checks)
	}
}

func TestInitNATForwardingFailsWhenRepairFails(t *testing.T) {
	fastNATTimers(t)
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning, "i-dev")
	fake := &fakeNATSSM{reasons: "iptables_missing", repairFixes: false, repairPolls: 1}
	err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), &InitResult{})
	if err == nil {
		t.Fatal("init must fail when the NAT instance cannot be repaired")
	}
	for _, want := range []string{"repair failed", "iptables_missing", "attempt 3/3", "Re-run `nakama aws init`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestInitNATForwardingDoesNotRestartARunningRepair(t *testing.T) {
	fastNATTimers(t)
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning)
	// A previous init was interrupted while the repair ran.
	fake := &fakeNATSSM{reasons: "iptables_missing", repairing: true, repairState: "active", repairFixes: true, repairPolls: 2}
	if err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), &InitResult{}); err != nil {
		t.Fatal(err)
	}
	if fake.repairs != 0 {
		t.Errorf("a repair already running must be waited for, not started again (repairs=%d)", fake.repairs)
	}
}

func TestInitNATForwardingStartsAStoppedNATAndRestoresIt(t *testing.T) {
	fastNATTimers(t)
	// No code-server instance is running, so the NAT must be stopped
	// again after the check.
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameStopped)
	fake := &fakeNATSSM{healthy: true, notRegistered: 3}
	if err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), &InitResult{}); err != nil {
		t.Fatal(err)
	}
	if len(ec2.started) != 1 || len(ec2.stopped) != 1 {
		t.Errorf("started=%v stopped=%v, want the NAT started for the check and stopped afterwards", ec2.started, ec2.stopped)
	}
}

func TestInitNATForwardingKeepsTheNATRunningWhenNeeded(t *testing.T) {
	fastNATTimers(t)
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameStopped, "i-dev")
	fake := &fakeNATSSM{healthy: true}
	if err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), &InitResult{}); err != nil {
		t.Fatal(err)
	}
	if len(ec2.stopped) != 0 {
		t.Errorf("a code-server instance is running: the NAT must stay up (stopped=%v)", ec2.stopped)
	}
}

func TestInitNATForwardingRestoresPowerOnFailure(t *testing.T) {
	fastNATTimers(t)
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameStopped)
	fake := &fakeNATSSM{reasons: "iptables_missing", repairFixes: false}
	if err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), &InitResult{}); err == nil {
		t.Fatal("want an error")
	}
	if len(ec2.stopped) != 1 {
		t.Errorf("the NAT must be stopped again even when the repair fails (stopped=%v)", ec2.stopped)
	}
}

func TestInitNATForwardingGivesUpWhenTheAgentNeverRegisters(t *testing.T) {
	fastNATTimers(t)
	natAgentWaitTimeout = 50 * time.Millisecond
	ec2 := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning)
	fake := &fakeNATSSM{notRegistered: 1 << 30}
	err := newNATForwardingService(ec2, fake).initNATForwarding(context.Background(), &InitResult{})
	if err == nil || !strings.Contains(err.Error(), "ssm agent online") {
		t.Errorf("err = %v", err)
	}
}

func TestParseNATHealth(t *testing.T) {
	h, err := parseNATHealth("noise\nNAKAMA_NAT_REPAIR=failed\nNAKAMA_NAT_OK=0\nNAKAMA_NAT_REASONS=iptables_missing service_inactive\nNAKAMA_NAT_LOG=a=b\nNAKAMA_NAT_LOG=c\n")
	if err != nil {
		t.Fatal(err)
	}
	if h.OK || h.Reasons != "iptables_missing service_inactive" || h.Repair != "failed" ||
		len(h.Log) != 2 || h.Log[0] != "a=b" {
		t.Errorf("%+v", h)
	}
	if _, err := parseNATHealth("garbage"); err == nil {
		t.Error("output without the OK line must be an error")
	}
}

func TestWithTargetNotConnectedHint(t *testing.T) {
	notConnected := &smithy.GenericAPIError{Code: "TargetNotConnected", Message: "i-1 is not connected."}
	err := withTargetNotConnectedHint(errors.New("StartSession: " + notConnected.Error()))
	if strings.Contains(err.Error(), "NAT") {
		t.Error("a plain error must be returned unchanged")
	}
	wrapped := withTargetNotConnectedHint(errors.Join(errors.New("StartSession"), notConnected))
	for _, want := range []string{"is not connected", "NAT instance", "nakama aws list --show-nat", "nakama aws init"} {
		if !strings.Contains(wrapped.Error(), want) {
			t.Errorf("hint lacks %q:\n%v", want, wrapped)
		}
	}
	if !errors.Is(wrapped, notConnected) {
		t.Error("the API error must stay inspectable")
	}
	other := &smithy.GenericAPIError{Code: "AccessDeniedException"}
	if got := withTargetNotConnectedHint(other); got != error(other) {
		t.Errorf("other API errors must be unchanged, got %v", got)
	}
}
