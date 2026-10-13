package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
)

// The NAT instance's forwarding is checked, and repaired when missing,
// at the end of `nakama aws init` (Issue #87). Before, the setup
// tolerated a failed package install (`dnf install … || true`), so a
// NAT instance whose dnf was OOM-killed came up without iptables and
// forwarded nothing: every instance behind it lost its outbound
// connectivity, never registered with SSM, and `connect` failed with
// TargetNotConnected, while init reported success.

// NAT forwarding states reported in InitResult.NATForwarding.
const (
	NATForwardingOK       = "ok"
	NATForwardingRepaired = "repaired"
)

// Timing of the check and the repair; variables so tests run fast.
var (
	// natAgentWaitTimeout bounds the wait for the NAT instance's SSM
	// agent to register (right after RunInstances or StartInstances).
	natAgentWaitTimeout = 5 * time.Minute
	// natRepairPollInterval is how often the repair's progress is read.
	natRepairPollInterval = 15 * time.Second
	// natRepairTimeout bounds the repair: the security updates on a
	// t4g.nano take a few minutes.
	natRepairTimeout = 25 * time.Minute
)

// natCheckScript reports, without changing anything, whether the NAT
// instance forwards: ip_forward, the iptables command, the MASQUERADE
// rule and nakama-nat.service. It first waits for the first-boot setup
// (cloud-final.service, which runs the user-data) to stop running, so a
// check right after creation does not race it. It also reports the
// state of a repair started by natRepairScript.
//
// The wait asks systemd, not `cloud-init status --wait`: when the OOM
// killer ends the user-data, systemd kills all of cloud-final.service,
// cloud-init with it, and cloud-init never records that its final stage
// finished. `cloud-init status` then says "running" forever, and --wait
// blocked the check on exactly the instances it has to repair (seen on
// a t4g.nano that reproduced Issue #87).
const natCheckScript = `set -u
for _ in $(seq 1 150); do
  # A oneshot unit is "activating" while it runs; afterwards it is
  # "active" (RemainAfterExit=yes) or "failed".
  [ "$(systemctl show -p ActiveState --value cloud-final.service 2>/dev/null)" = activating ] || break
  sleep 10
done
reasons=""
[ "$(sysctl -n net.ipv4.ip_forward 2>/dev/null)" = 1 ] || reasons="$reasons ip_forward_off"
if command -v iptables >/dev/null 2>&1; then
  iptables -t nat -S POSTROUTING 2>/dev/null | grep -q -- '-j MASQUERADE' || reasons="$reasons masquerade_missing"
else
  reasons="$reasons iptables_missing"
fi
systemctl is-active --quiet nakama-nat.service || reasons="$reasons service_inactive"
echo "NAKAMA_NAT_REPAIR=$(systemctl show -p ActiveState --value nakama-nat-repair.service 2>/dev/null)"
if [ -z "$reasons" ]; then
  echo "NAKAMA_NAT_OK=1"
else
  echo "NAKAMA_NAT_OK=0"
  echo "NAKAMA_NAT_REASONS=${reasons# }"
fi
if [ "$(systemctl show -p ActiveState --value nakama-nat-repair.service 2>/dev/null)" = failed ]; then
  journalctl -u nakama-nat-repair.service --no-pager -n 30 -o cat 2>/dev/null | sed 's/^/NAKAMA_NAT_LOG=/'
fi
`

// natRepairScriptDelimiter ends the setup script's here-document in
// natRepairScript; the setup script must not contain it on a line.
const natRepairScriptDelimiter = "NAKAMA_NAT_SETUP_EOF"

// natRepairScript starts the setup script as a transient systemd unit
// and returns at once; the caller follows it with natCheckScript.
//
// The setup runs `dnf upgrade`, which can upgrade amazon-ssm-agent and
// restart it. The agent's unit has KillMode=process, so a script it
// started would survive, but the agent would lose track of the command
// and report it failed. A unit of its own keeps the repair independent
// of the agent, and its state stays readable afterwards.
func natRepairScript() string {
	return `set -eu
if systemctl is-active --quiet nakama-nat-repair.service; then
  echo "NAKAMA_REPAIR=already-running"
  exit 0
fi
systemctl reset-failed nakama-nat-repair.service 2>/dev/null || true
umask 077
cat >/var/tmp/nakama-nat-setup.sh <<'` + natRepairScriptDelimiter + `'
` + natSetupScript + natRepairScriptDelimiter + `
systemd-run --unit=nakama-nat-repair --description="nakama NAT instance repair" /bin/bash /var/tmp/nakama-nat-setup.sh
echo "NAKAMA_REPAIR=started"
`
}

// natHealth is natCheckScript's report.
type natHealth struct {
	OK      bool
	Reasons string
	// Repair is the repair unit's ActiveState ("active" while it runs,
	// "failed" after a failure, "inactive" otherwise).
	Repair string
	// Log is the repair unit's journal tail after a failure.
	Log []string
}

func parseNATHealth(stdout string) (natHealth, error) {
	var h natHealth
	seen := false
	sc := bufio.NewScanner(strings.NewReader(stdout))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch key {
		case "NAKAMA_NAT_OK":
			h.OK, seen = value == "1", true
		case "NAKAMA_NAT_REASONS":
			h.Reasons = value
		case "NAKAMA_NAT_REPAIR":
			h.Repair = value
		case "NAKAMA_NAT_LOG":
			h.Log = append(h.Log, value)
		}
	}
	if !seen {
		return natHealth{}, fmt.Errorf("unexpected output from the NAT instance check (%d bytes)", len(stdout))
	}
	return h, nil
}

// initNATForwarding checks the NAT instance's forwarding and repairs it
// when missing. A stopped NAT instance is started for the check and its
// power state reconciled afterwards (stopped again unless a code-server
// instance is running). An instance whose forwarding cannot be verified
// or repaired fails init: everything behind it would be unreachable.
func (s *Service) initNATForwarding(ctx context.Context, res *InitResult) (err error) {
	info, err := s.findNATInstance(ctx, res.VPCID)
	if err != nil {
		return err
	}
	if info.InstanceID == "" {
		return errors.New("nakama NAT instance not found")
	}
	if info.State != ec2types.InstanceStateNameRunning && info.State != ec2types.InstanceStateNamePending {
		slog.Info("nat instance: starting it to check its forwarding; its power state is restored afterwards",
			"instance_id", info.InstanceID, "state", info.State)
		if err := s.ensureNATInstanceRunning(ctx); err != nil {
			return err
		}
		defer func() {
			if rerr := s.reconcileNATInstancePowerState(ctx); rerr != nil {
				err = errors.Join(err, fmt.Errorf("restore the nat instance power state: %w", rerr))
			}
		}()
	} else if info.State == ec2types.InstanceStateNamePending {
		if err := s.waitInstanceRunning(ctx, info.InstanceID); err != nil {
			return fmt.Errorf("wait nat instance running: %w", err)
		}
	}

	slog.Info("nat instance: checking its forwarding", "instance_id", info.InstanceID)
	h, err := s.checkNATHealth(ctx, info.InstanceID)
	if err != nil {
		return err
	}
	if h.OK {
		slog.Info("nat instance: forwarding ok", "instance_id", info.InstanceID)
		res.NATForwarding = NATForwardingOK
		return nil
	}

	slog.Warn("nat instance: forwarding is not set up; repairing it (security updates included, this takes a few minutes)",
		"instance_id", info.InstanceID, "reasons", h.Reasons)
	if h.Repair != "active" {
		if _, err := s.runNATCommand(ctx, info.InstanceID, natRepairScript(), "nakama: repair NAT instance forwarding"); err != nil {
			return fmt.Errorf("start the nat instance repair: %w", err)
		}
	}
	err = awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    natRepairPollInterval,
		Timeout:     natRepairTimeout,
		Description: fmt.Sprintf("nat instance %s repair", info.InstanceID),
	}, func(ctx context.Context) (bool, error) {
		h, err = s.checkNATHealth(ctx, info.InstanceID)
		if err != nil {
			return false, err
		}
		switch {
		case h.OK && h.Repair != "active":
			return true, nil
		case h.Repair == "failed":
			return false, fmt.Errorf("the nat instance repair failed (%s):\n  %s",
				h.Reasons, strings.Join(h.Log, "\n  "))
		}
		slog.Info("nat instance: repair in progress", "instance_id", info.InstanceID)
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("%w\nThe NAT instance %s does not forward traffic, so instances behind it cannot reach "+
			"AWS or the internet. Re-run `nakama aws init` to retry; if it keeps failing, terminate the NAT instance "+
			"and run init again to create a new one", err, info.InstanceID)
	}
	slog.Info("nat instance: forwarding repaired", "instance_id", info.InstanceID)
	res.NATForwarding = NATForwardingRepaired
	return nil
}

func (s *Service) checkNATHealth(ctx context.Context, instanceID string) (natHealth, error) {
	out, err := s.runNATCommand(ctx, instanceID, natCheckScript, "nakama: check NAT instance forwarding")
	if err != nil {
		return natHealth{}, fmt.Errorf("check the nat instance forwarding: %w", err)
	}
	return parseNATHealth(out)
}

// runNATCommand runs a script on the NAT instance through Run Command
// and returns its stdout. SendCommand is retried while the instance's
// SSM agent has not registered yet (InvalidInstanceId), which is the
// case for a minute or so after the instance starts.
func (s *Service) runNATCommand(ctx context.Context, instanceID, script, comment string) (string, error) {
	var commandID string
	if err := awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    natRepairPollInterval,
		Timeout:     natAgentWaitTimeout,
		Description: fmt.Sprintf("ssm agent online on the nat instance %s", instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.SSM.SendCommand(ctx, &ssm.SendCommandInput{
			DocumentName: aws.String("AWS-RunShellScript"),
			InstanceIds:  []string{instanceID},
			Parameters: map[string][]string{
				"commands":         {script},
				"executionTimeout": {"1800"},
			},
			TimeoutSeconds: aws.Int32(600),
			Comment:        aws.String(comment),
		})
		if err != nil {
			if awshelper.IsAPIError(err, "InvalidInstanceId") {
				slog.Info("nat instance: waiting for its SSM agent to come online", "instance_id", instanceID)
				return false, nil
			}
			return false, fmt.Errorf("SendCommand: %w", err)
		}
		if out.Command == nil || out.Command.CommandId == nil {
			return false, errors.New("SendCommand returned nil command id")
		}
		commandID = *out.Command.CommandId
		return true, nil
	}); err != nil {
		return "", err
	}
	return s.waitSSMCommandOutput(ctx, commandID, instanceID)
}
