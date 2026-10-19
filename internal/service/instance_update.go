package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/awscli"
	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
)

// UpdateInput controls `nakama aws update`.
type UpdateInput struct {
	Name string
	Keep int32 // forwarded to the pre-update backup (<=0 -> keep all)
}

// UpdateResult reports the outcome of an update.
type UpdateResult struct {
	InstanceID        string `json:"instance_id"`
	BackupLabel       string `json:"backup_label"`
	CodeServerVersion string `json:"code_server_version,omitempty"`
	Rebooted          bool   `json:"rebooted"`       // a reboot was performed (was-running case)
	RebootPending     bool   `json:"reboot_pending"` // kernel update applies on next start (was-stopped case)
	RestoredToStopped bool   `json:"restored_to_stopped"`

	// AWSCLIVersion is the AWS CLI version after the update, when the
	// instance has the official install (Issue #101). Instances created
	// before that keep their snap, which nakama does not touch.
	AWSCLIVersion string `json:"aws_cli_version,omitempty"`
	// KiroCrew is "updated" or "failed" when the instance has Kiro Crew,
	// and empty when it has none (--no-kiro). A failure does not fail
	// the update.
	KiroCrew string `json:"kiro_crew,omitempty"`
}

// Kiro Crew update outcomes in UpdateResult.KiroCrew.
const (
	KiroCrewUpdated = "updated"
	KiroCrewFailed  = "failed"
)

// Update brings an existing instance up to date: it takes a pre-update
// backup, runs `apt-get dist-upgrade` and a code-server latest install
// via SSM Run Command, reboots only when needed, and restores the
// instance's original power state. A stopped instance is started for
// the update and stopped again afterwards (even if the update fails).
func (s *Service) Update(ctx context.Context, in UpdateInput) (*UpdateResult, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}

	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if err != nil {
		return nil, err
	}
	if instanceID == "" {
		return nil, fmt.Errorf("nakama instance %q not found", in.Name)
	}
	// Read everything the update needs before changing anything: an
	// error after the instance was started would skip the power-state
	// restore below and leave a stopped instance running.
	kind, err := s.ssmGetOptional(ctx, ssmKindPath(in.Name))
	if err != nil {
		return nil, err
	}

	state, _, err := s.fetchInstanceStateAndType(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	switch state {
	case "":
		return nil, fmt.Errorf("instance %s not found (terminated?)", instanceID)
	case string(ec2types.InstanceStateNameRunning), string(ec2types.InstanceStateNameStopped):
		// ok
	default:
		return nil, fmt.Errorf("instance must be running or stopped to update, got %q", state)
	}
	wasStopped := state == string(ec2types.InstanceStateNameStopped)

	res := &UpdateResult{InstanceID: instanceID}

	// 1. Pre-update backup (point-in-time; not waited to completion).
	label := "preupdate-" + time.Now().UTC().Format("2006-01-02-1504")
	b, err := s.CreateBackup(ctx, CreateBackupInput{
		Name:  in.Name,
		Label: label,
		Note:  "auto backup before nakama aws update",
		Keep:  in.Keep,
	})
	if err != nil {
		return nil, fmt.Errorf("pre-update backup: %w", err)
	}
	res.BackupLabel = b.Label

	// The update installs packages and the latest code-server from the
	// internet, so the shared NAT instance must provide egress. Ensure it
	// is running before the code-server instance comes up.
	if err := s.ensureNATInstanceRunning(ctx); err != nil {
		return res, fmt.Errorf("start NAT instance: %w", err)
	}

	// 2. Start the instance if it was stopped.
	if wasStopped {
		slog.Info("update: starting stopped instance", "instance_id", instanceID)
		if _, err := s.clients.EC2.StartInstances(ctx, &ec2.StartInstancesInput{
			InstanceIds: []string{instanceID},
		}); err != nil {
			return res, fmt.Errorf("StartInstances: %w", err)
		}
		if err := s.waitInstanceRunning(ctx, instanceID); err != nil {
			return res, fmt.Errorf("wait running: %w", err)
		}
	}

	// 3. Run the update via SSM Run Command.
	// A bare instance has no code-server to update, so only the OS part
	// of the script runs (Issue #52).
	stdout, runErr := s.runUpdateCommand(ctx, instanceID, ssmVersionPath(in.Name), kind == instanceKindBare)

	// 4. Reboot handling (only meaningful on success).
	if runErr == nil && rebootRequired(stdout) {
		if wasStopped {
			res.RebootPending = true // applied on the next start
		} else {
			slog.Info("update: kernel update requires reboot, rebooting", "instance_id", instanceID)
			if _, err := s.clients.EC2.RebootInstances(ctx, &ec2.RebootInstancesInput{
				InstanceIds: []string{instanceID},
			}); err != nil {
				return res, fmt.Errorf("RebootInstances: %w", err)
			}
			res.Rebooted = true
		}
	}

	// 5. Restore the original power state, even if the update failed.
	if wasStopped {
		slog.Info("update: stopping instance to restore prior state", "instance_id", instanceID)
		if _, err := s.clients.EC2.StopInstances(ctx, &ec2.StopInstancesInput{
			InstanceIds: []string{instanceID},
		}); err != nil {
			slog.Warn("update: StopInstances failed during power-state restore", "error", err)
		} else if err := s.waitInstanceStopped(ctx, instanceID); err != nil {
			slog.Warn("update: wait stopped failed during power-state restore", "error", err)
		} else {
			res.RestoredToStopped = true
		}
	}

	// Reconcile the NAT instance against the restored power state, whether
	// or not the update command succeeded: if the instance was stopped
	// again and nothing else is active, stop the NAT instance too.
	// Best-effort so it does not mask the update result.
	if err := s.reconcileNATInstancePowerState(ctx); err != nil {
		slog.Warn("update: NAT instance reconcile failed", "error", err)
	}

	if runErr != nil {
		return res, fmt.Errorf("update command: %w", runErr)
	}
	applyToolMarkers(res, stdout)
	if res.KiroCrew == KiroCrewFailed {
		slog.Warn("update: kirocrew update or restart failed", "instance", in.Name, "log", updateLogPath)
	}

	if kind == instanceKindBare {
		return res, nil
	}

	// Display-only read-back after a successful update: keep it
	// best-effort instead of failing the whole result.
	if v, err := s.ssmGetOptional(ctx, ssmVersionPath(in.Name)); err == nil {
		res.CodeServerVersion = v
	} else {
		slog.Warn("update: reading back code-server version failed", "error", err)
	}
	return res, nil
}

// rebootRequired reports whether the update script signalled that a
// reboot is pending (presence of /var/run/reboot-required).
func rebootRequired(commandStdout string) bool {
	return strings.Contains(commandStdout, "NAKAMA_REBOOT_REQUIRED=1")
}

// applyToolMarkers copies the AWS CLI and Kiro Crew results the update
// script reported into res. An unexpected Kiro Crew value is ignored.
func applyToolMarkers(res *UpdateResult, commandStdout string) {
	res.AWSCLIVersion = updateMarker(commandStdout, "NAKAMA_AWSCLI")
	switch k := updateMarker(commandStdout, "NAKAMA_KIROCREW"); k {
	case KiroCrewUpdated, KiroCrewFailed:
		res.KiroCrew = k
	}
}

// updateMarker returns the value of a KEY=value line the update script
// wrote to stdout, or "" when there is none.
func updateMarker(commandStdout, key string) string {
	for _, line := range strings.Split(commandStdout, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v
		}
	}
	return ""
}

// updateLogPath is where the update script writes its full output on
// the instance.
const updateLogPath = "/var/log/nakama-update.log"

// updateScriptHeader starts every update script. GetCommandInvocation
// returns only the first 24,000 characters of a command's stdout, and an
// apt dist-upgrade under xtrace easily writes more, which pushed the
// result markers at the end out of reach: a required reboot then went
// undetected. So the full output goes to updateLogPath, and only the
// markers are written to the Run Command stdout, kept as fd 3. When the
// script fails, the end of the log goes to stderr (fd 4), where the
// caller's error message shows it.
func updateScriptHeader() string {
	return `#!/bin/bash
set -euo pipefail
exec 3>&1 4>&2
exec >>` + updateLogPath + ` 2>&1
echo "=== nakama update $(date -u +%FT%TZ)"
trap 'rc=$?; set +x; if [ "$rc" -ne 0 ]; then { echo "nakama: the update failed (exit $rc); the end of ` + updateLogPath + ` follows"; tail -c 3500 ` + updateLogPath + `; } >&4; fi' EXIT
set -x
`
}

// updateToolsScript updates the tools nakama installs for the ubuntu
// user (Issue #101). Both steps run only where the tool is installed: an
// instance created before Issue #101 keeps its snap AWS CLI (nothing is
// installed here), and one created with --no-kiro has no Kiro Crew.
//
//   - The AWS CLI: AWS's install script, run again, updates in place.
//     A failure fails the update, as a failed code-server update does.
//   - Kiro Crew: `kirocrew update`, then `kirocrew restart` so the
//     gateway runs the new version. The output is discarded, not logged:
//     restart prints a dashboard URL with an access token. A failure only
//     warns (the OS and code-server are updated by then), and the result
//     goes to the caller as NAKAMA_KIROCREW=updated|failed.
//
// The NAKAMA_AWSCLI marker carries the AWS CLI version afterwards.
var updateToolsScript = awscli.InstallScript() + `
if [ -x ` + awscli.InstalledBin + ` ]; then
  nakama_install_awscli
  echo "NAKAMA_AWSCLI=$(sudo -u ubuntu env -i PATH=/usr/bin:/bin HOME=/nonexistent ` + awscli.InstalledBin + ` --version 2>&1 | awk '{print $1}' | cut -d/ -f2)" >&3
fi

if [ -x /home/ubuntu/.local/bin/kirocrew ]; then
  if sudo -u ubuntu -H bash -lc 'set -e; export PATH="$HOME/.local/bin:$PATH"; kirocrew update; kirocrew restart' >/dev/null 2>&1; then
    echo "nakama: Kiro Crew updated and restarted"
    echo "NAKAMA_KIROCREW=updated" >&3
  else
    echo "nakama: WARNING kirocrew update or kirocrew restart failed; run them as ubuntu to see why"
    echo "NAKAMA_KIROCREW=failed" >&3
  fi
fi
`

// updateRebootMarker reports, on the result channel, whether a reboot
// is pending (presence of /var/run/reboot-required).
const updateRebootMarker = `if [ -f /var/run/reboot-required ]; then
  echo "NAKAMA_REBOOT_REQUIRED=1" >&3
else
  echo "NAKAMA_REBOOT_REQUIRED=0" >&3
fi
`

// buildBareUpdateScript renders the update script for a bare instance
// (Issue #52): the OS part only, with the same reboot marker so the
// caller's reboot handling is unchanged.
func buildBareUpdateScript() string {
	return updateScriptHeader() + `export DEBIAN_FRONTEND=noninteractive
export HOME="${HOME:-/root}"
apt-get update -qq
apt-get dist-upgrade -y -qq
apt-get autoremove -y -qq
` + updateToolsScript + updateRebootMarker
}

// buildUpdateScript renders the in-instance update shell script.
// region/versionParam are injected so the instance can write the
// post-update code-server version back to Parameter Store.
func buildUpdateScript(region, versionParam string) string {
	return updateScriptHeader() + awscli.RunScript() + fmt.Sprintf(`export DEBIAN_FRONTEND=noninteractive
# SSM Run Command runs as root in a non-login shell that does not export
# HOME; the code-server install.sh reads $HOME. Default to /root (matches
# the cloud-init userdata workaround).
export HOME="${HOME:-/root}"

# Wait out any boot-time apt/dpkg lock (e.g. unattended-upgrades).
for _ in $(seq 1 60); do
  if ! fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1; then break; fi
  echo "waiting for dpkg lock..."; sleep 10
done

apt-get update -y
apt-get dist-upgrade -y -o Dpkg::Options::="--force-confdef" -o Dpkg::Options::="--force-confold"

# code-server -> latest
curl -fsSL https://code-server.dev/install.sh | sh
systemctl restart code-server@ubuntu || true

# Best-effort: record the running code-server version back to SSM.
if VL="$(sudo -u ubuntu code-server --version 2>/dev/null | head -n1)"; then
  RV="$(echo "${VL}" | awk '{print $1}')"
  if [ -n "${RV}" ]; then
    nakama_aws ssm put-parameter --region %q --name %q --type String --overwrite --value "${RV}" >/dev/null || true
  fi
fi

`, region, versionParam) + updateToolsScript + updateRebootMarker
}

// runUpdateCommand sends the update script via SSM Run Command and
// returns the command's stdout. It retries SendCommand while the SSM
// agent is not yet registered (InvalidInstanceId), which happens for a
// short window after a stopped instance is started.
func (s *Service) runUpdateCommand(ctx context.Context, instanceID, versionParam string, bare bool) (string, error) {
	script := buildUpdateScript(config.Region, versionParam)
	if bare {
		script = buildBareUpdateScript()
	}

	var commandID string
	if err := awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    10 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("ssm agent online on %s", instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.SSM.SendCommand(ctx, &ssm.SendCommandInput{
			DocumentName: aws.String("AWS-RunShellScript"),
			InstanceIds:  []string{instanceID},
			Parameters: map[string][]string{
				"commands":         {script},
				"executionTimeout": {"3600"},
			},
			TimeoutSeconds: aws.Int32(600),
			Comment:        aws.String("nakama: apt dist-upgrade, code-server, AWS CLI and Kiro Crew"),
		})
		if err != nil {
			// The agent may not be registered yet right after start.
			if awshelper.IsAPIError(err, "InvalidInstanceId") {
				slog.Info("update: waiting for SSM agent to come online", "instance_id", instanceID)
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

// waitSSMCommandOutput polls GetCommandInvocation until terminal and
// returns the accumulated stdout (truncated for logging on failure).
func (s *Service) waitSSMCommandOutput(ctx context.Context, commandID, instanceID string) (string, error) {
	var stdout string
	err := awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    10 * time.Second,
		Timeout:     40 * time.Minute,
		Description: fmt.Sprintf("ssm command %s on %s", commandID, instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.SSM.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			if awshelper.IsAPIError(err, "InvocationDoesNotExist") {
				return false, nil
			}
			return false, err
		}
		if out.StandardOutputContent != nil {
			stdout = *out.StandardOutputContent
		}
		switch out.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			// Never log the remote output itself: callers run scripts
			// whose stdout can carry secrets (e.g. the encrypted Kiro
			// Crew token exchange), and the default log level would put
			// it on the user's terminal. The size is enough to debug.
			slog.Debug("ssm command success", "command_id", commandID, "stdout_bytes", outputLen(out.StandardOutputContent))
			return true, nil
		case ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusTimedOut:
			return false, fmt.Errorf("ssm command terminated as %s; stderr: %s",
				out.Status, trimOutput(out.StandardErrorContent))
		}
		slog.Info("ssm command pending", "command_id", commandID, "status", out.Status)
		return false, nil
	})
	return stdout, err
}
