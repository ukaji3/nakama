package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// RestoreInput selects the backup to restore in place. An empty
// Label restores the most recent backup.
type RestoreInput struct {
	Name  string
	Label string
}

// Restore performs an in-place restore: it stops the instance,
// creates a volume from the selected snapshot, swaps it in as the
// root device, and deletes the previous root volume. The instance
// is left stopped.
func (s *Service) Restore(ctx context.Context, in RestoreInput) error {
	if err := ValidateInstanceName(in.Name); err != nil {
		return err
	}
	b, err := s.findBackup(ctx, in.Name, in.Label)
	if err != nil {
		return err
	}
	instanceID := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if instanceID == "" {
		return fmt.Errorf("nakama instance %q not found", in.Name)
	}
	device, oldVolumeID, az, volType, err := s.rootVolumeDetails(ctx, instanceID)
	if err != nil {
		return err
	}

	slog.Info("restore: stopping instance", "instance", in.Name, "snapshot_id", b.SnapshotID)
	if _, err := s.clients.EC2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
		return fmt.Errorf("StopInstances: %w", err)
	}
	if err := s.waitInstanceStopped(ctx, instanceID); err != nil {
		return err
	}

	tags := tagging.ForInstance(in.Name, tagging.ComponentEBS, naming.EC2(in.Name))
	volOut, err := s.clients.EC2.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		SnapshotId:       aws.String(b.SnapshotID),
		VolumeType:       volType,
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeVolume, tags),
		},
	})
	if err != nil {
		return fmt.Errorf("CreateVolume: %w", err)
	}
	newVolumeID := *volOut.VolumeId
	if err := s.waitVolumeState(ctx, newVolumeID, ec2types.VolumeStateAvailable); err != nil {
		return err
	}

	slog.Info("restore: swapping root volume", "old", oldVolumeID, "new", newVolumeID, "device", device)
	if _, err := s.clients.EC2.DetachVolume(ctx, &ec2.DetachVolumeInput{VolumeId: aws.String(oldVolumeID)}); err != nil {
		return fmt.Errorf("DetachVolume(%s): %w", oldVolumeID, err)
	}
	if err := s.waitVolumeState(ctx, oldVolumeID, ec2types.VolumeStateAvailable); err != nil {
		return err
	}
	if _, err := s.clients.EC2.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(newVolumeID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String(device),
	}); err != nil {
		return fmt.Errorf("AttachVolume(%s): %w", newVolumeID, err)
	}
	if err := s.waitVolumeState(ctx, newVolumeID, ec2types.VolumeStateInUse); err != nil {
		return err
	}

	if _, err := s.clients.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(oldVolumeID)}); err != nil {
		return fmt.Errorf("DeleteVolume(%s): %w", oldVolumeID, err)
	}
	// The instance is left stopped; reconcile the shared NAT instance so
	// it is stopped too when no code-server instance is active.
	if err := s.reconcileNATInstancePowerState(ctx); err != nil {
		slog.Warn("restore: NAT instance reconcile failed", "error", err)
	}
	slog.Info("restore: completed (instance left stopped)", "instance", in.Name)
	return nil
}

// rootVolumeDetails returns the root device name, current root volume
// ID, availability zone, and volume type of the instance.
func (s *Service) rootVolumeDetails(ctx context.Context, instanceID string) (device, volumeID, az string, volType ec2types.VolumeType, err error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return "", "", "", "", fmt.Errorf("DescribeInstances: %w", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			rootDev := aws.ToString(i.RootDeviceName)
			if i.Placement != nil {
				az = aws.ToString(i.Placement.AvailabilityZone)
			}
			for _, m := range i.BlockDeviceMappings {
				if m.DeviceName != nil && *m.DeviceName == rootDev && m.Ebs != nil && m.Ebs.VolumeId != nil {
					volumeID = *m.Ebs.VolumeId
				}
			}
			device = rootDev
		}
	}
	if device == "" || volumeID == "" || az == "" {
		return "", "", "", "", fmt.Errorf("root volume not found for instance %s", instanceID)
	}
	vt, err := s.volumeType(ctx, volumeID)
	if err != nil {
		return "", "", "", "", err
	}
	return device, volumeID, az, vt, nil
}

func (s *Service) volumeType(ctx context.Context, volumeID string) (ec2types.VolumeType, error) {
	out, err := s.clients.EC2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volumeID}})
	if err != nil {
		return "", fmt.Errorf("DescribeVolumes: %w", err)
	}
	if len(out.Volumes) == 0 {
		return "", fmt.Errorf("volume %s not found", volumeID)
	}
	return out.Volumes[0].VolumeType, nil
}

func (s *Service) waitInstanceStopped(ctx context.Context, instanceID string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("instance %s stopped", instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
		if err != nil {
			if awshelper.IsAPIError(err, "InvalidInstanceID.NotFound") {
				slog.Info("instance: not yet visible, waiting", "instance_id", instanceID)
				return false, nil
			}
			return false, err
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				if i.State != nil && i.State.Name == ec2types.InstanceStateNameStopped {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

func (s *Service) waitVolumeState(ctx context.Context, volumeID string, want ec2types.VolumeState) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("volume %s -> %s", volumeID, want),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volumeID}})
		if err != nil {
			return false, err
		}
		for _, v := range out.Volumes {
			if v.State == want {
				return true, nil
			}
		}
		return false, nil
	})
}
