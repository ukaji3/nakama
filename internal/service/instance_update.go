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
}

// Update brings an existing instance up to date: it takes a pre-update
// backup, runs `apt-get dist-upgrade` and a code-server latest install
// via SSM Run Command, reboots only when needed, and restores the
// instance's original power state. A stopped instance is started for
// the update and stopped again afterwards (even if the update fails).
func (s *Service) Update(ctx context.Context, in UpdateInput) (*UpdateResult, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}

	instanceID := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if instanceID == "" {
		return nil, fmt.Errorf("nakama instance %q not found", in.Name)
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
	stdout, runErr := s.runUpdateCommand(ctx, instanceID, ssmVersionPath(in.Name))

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

	if runErr != nil {
		return res, fmt.Errorf("update command: %w", runErr)
	}

	res.CodeServerVersion = s.ssmGetOptional(ctx, ssmVersionPath(in.Name))
	return res, nil
}

// rebootRequired reports whether the update script signalled that a
// reboot is pending (presence of /var/run/reboot-required).
func rebootRequired(commandStdout string) bool {
	return strings.Contains(commandStdout, "NAKAMA_REBOOT_REQUIRED=1")
}

// buildUpdateScript renders the in-instance update shell script.
// region/versionParam are injected so the instance can write the
// post-update code-server version back to Parameter Store.
func buildUpdateScript(region, versionParam string) string {
	return fmt.Sprintf(`#!/bin/bash
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive

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
    aws ssm put-parameter --region %q --name %q --type String --overwrite --value "${RV}" || true
  fi
fi

if [ -f /var/run/reboot-required ]; then
  echo "NAKAMA_REBOOT_REQUIRED=1"
else
  echo "NAKAMA_REBOOT_REQUIRED=0"
fi
`, region, versionParam)
}

// runUpdateCommand sends the update script via SSM Run Command and
// returns the command's stdout. It retries SendCommand while the SSM
// agent is not yet registered (InvalidInstanceId), which happens for a
// short window after a stopped instance is started.
func (s *Service) runUpdateCommand(ctx context.Context, instanceID, versionParam string) (string, error) {
	script := buildUpdateScript(config.Region, versionParam)

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
			Comment:        aws.String("nakama: apt dist-upgrade + code-server latest"),
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
			slog.Info("update: command success", "command_id", commandID, "stdout", trimOutput(out.StandardOutputContent))
			return true, nil
		case ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusTimedOut:
			return false, fmt.Errorf("ssm command terminated as %s; stderr: %s",
				out.Status, trimOutput(out.StandardErrorContent))
		}
		slog.Info("update: command pending", "command_id", commandID, "status", out.Status)
		return false, nil
	})
	return stdout, err
}
