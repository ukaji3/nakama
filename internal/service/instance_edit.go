package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/awshelper"
)

// EditEBSInput specifies a single edit operation: extend the root
// EBS volume. v1 only supports growth.
type EditEBSInput struct {
	Name      string
	NewSizeGB int32
}

// EditEBS modifies the EC2 root volume size and runs an in-instance
// growpart + resize2fs/xfs_growfs via SSM Run Command. The volume
// can only grow.
func (s *Service) EditEBS(ctx context.Context, in EditEBSInput) error {
	if err := ValidateInstanceName(in.Name); err != nil {
		return err
	}
	if in.NewSizeGB <= 0 {
		return errors.New("--ebs must be a positive integer (GiB)")
	}

	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if err != nil {
		return err
	}
	if instanceID == "" {
		return fmt.Errorf("nakama instance %q not found", in.Name)
	}

	volumeID, currentGiB, err := s.rootVolumeOf(ctx, instanceID)
	if err != nil {
		return err
	}
	if in.NewSizeGB <= currentGiB {
		return fmt.Errorf("new size %dGiB must be greater than current %dGiB (EBS cannot shrink)",
			in.NewSizeGB, currentGiB)
	}

	slog.Info("ebs: modifying volume", "volume_id", volumeID, "from_gib", currentGiB, "to_gib", in.NewSizeGB)
	if _, err := s.clients.EC2.ModifyVolume(ctx, &ec2.ModifyVolumeInput{
		VolumeId: aws.String(volumeID),
		Size:     aws.Int32(in.NewSizeGB),
	}); err != nil {
		return fmt.Errorf("ModifyVolume: %w", err)
	}

	if err := s.waitVolumeNewSizeVisible(ctx, volumeID, in.NewSizeGB); err != nil {
		return fmt.Errorf("wait modification: %w", err)
	}

	slog.Info("ebs: invoking in-instance growpart + resize", "instance_id", instanceID)
	cmdID, err := s.runGrowpartCommand(ctx, instanceID)
	if err != nil {
		return err
	}
	if err := s.waitSSMCommand(ctx, cmdID, instanceID); err != nil {
		return fmt.Errorf("growpart command: %w", err)
	}
	slog.Info("ebs: filesystem grown successfully")
	return nil
}

func (s *Service) rootVolumeOf(ctx context.Context, instanceID string) (string, int32, error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", 0, fmt.Errorf("DescribeInstances: %w", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			rootDev := ""
			if i.RootDeviceName != nil {
				rootDev = *i.RootDeviceName
			}
			for _, m := range i.BlockDeviceMappings {
				if m.DeviceName != nil && *m.DeviceName == rootDev && m.Ebs != nil && m.Ebs.VolumeId != nil {
					volID := *m.Ebs.VolumeId
					gib, err := s.volumeSizeGiB(ctx, volID)
					if err != nil {
						return "", 0, err
					}
					return volID, gib, nil
				}
			}
		}
	}
	return "", 0, fmt.Errorf("root volume not found for instance %s", instanceID)
}

func (s *Service) volumeSizeGiB(ctx context.Context, volumeID string) (int32, error) {
	out, err := s.clients.EC2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	})
	if err != nil {
		return 0, fmt.Errorf("DescribeVolumes: %w", err)
	}
	if len(out.Volumes) == 0 || out.Volumes[0].Size == nil {
		return 0, fmt.Errorf("volume %s not found", volumeID)
	}
	return *out.Volumes[0].Size, nil
}

// waitVolumeNewSizeVisible polls DescribeVolumes until the volume's
// reported Size matches the requested size. Modern Nitro instances
// see the new size almost immediately, but the modification can be
// in 'optimizing' state for a short window.
func (s *Service) waitVolumeNewSizeVisible(ctx context.Context, volumeID string, want int32) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("volume %s reports new size %dGiB", volumeID, want),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []string{volumeID},
		})
		if err != nil {
			return false, err
		}
		for _, v := range out.Volumes {
			if v.Size != nil && *v.Size == want && v.State == ec2types.VolumeStateInUse {
				return true, nil
			}
		}
		return false, nil
	})
}

const growpartScript = `#!/bin/bash
set -euxo pipefail
ROOT_PARTITION=$(findmnt -no SOURCE /)
ROOT_DEVICE=$(lsblk -no PKNAME "${ROOT_PARTITION}")
PARTITION_NUMBER=$(echo "${ROOT_PARTITION}" | grep -oE '[0-9]+$')
sudo growpart "/dev/${ROOT_DEVICE}" "${PARTITION_NUMBER}" || true
if sudo resize2fs "${ROOT_PARTITION}"; then
  echo "resize2fs ok"
elif sudo xfs_growfs /; then
  echo "xfs_growfs ok"
else
  echo "no compatible resizer for ${ROOT_PARTITION}" >&2
  exit 1
fi
df -h /
`

func (s *Service) runGrowpartCommand(ctx context.Context, instanceID string) (string, error) {
	out, err := s.clients.SSM.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceID},
		Parameters: map[string][]string{
			"commands": {growpartScript},
		},
		TimeoutSeconds: aws.Int32(300),
		Comment:        aws.String("nakama: growpart + resize after EBS expansion"),
	})
	if err != nil {
		return "", fmt.Errorf("SendCommand: %w", err)
	}
	if out.Command == nil || out.Command.CommandId == nil {
		return "", errors.New("SendCommand returned nil")
	}
	return *out.Command.CommandId, nil
}

// waitSSMCommand polls GetCommandInvocation for terminal status.
func (s *Service) waitSSMCommand(ctx context.Context, commandID, instanceID string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("ssm command %s on %s", commandID, instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.SSM.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			// "InvocationDoesNotExist" until SSM has accepted the command.
			if awshelper.IsAPIError(err, "InvocationDoesNotExist") {
				return false, nil
			}
			return false, err
		}
		switch out.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			// See waitSSMCommandOutput: the output itself is never logged.
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
}

// outputLen reports the size of a command output without exposing it.
func outputLen(p *string) int {
	if p == nil {
		return 0
	}
	return len(*p)
}

func trimOutput(p *string) string {
	if p == nil {
		return ""
	}
	const max = 4000
	s := *p
	if len(s) > max {
		s = s[:max] + "...(truncated)"
	}
	return s
}
