package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
	"github.com/ukaji3/nakama/internal/userdata"
)

// Restore modes. In place swaps the root volume of the existing
// instance; a new instance is built when the original was deleted
// (Issue #96, ADR 0023).
const (
	RestoreModeInPlace     = "in-place"
	RestoreModeNewInstance = "new-instance"
)

// RestoreInput selects the backup to restore. An empty Label restores
// the most recent backup.
type RestoreInput struct {
	Name  string
	Label string

	// The options below apply only when the instance no longer exists
	// and restore builds a new one (Issue #96). InstanceType overrides
	// the type recorded on the backup and is required for backups taken
	// before nakama recorded it; Bare marks such an old backup as one of
	// a bare instance. WithAdmin / WithRole attach an IAM role as create
	// does; restore never re-attaches a role on its own.
	InstanceType string
	Bare         bool
	WithAdmin    bool
	WithRole     string
}

func (in RestoreInput) hasNewInstanceOptions() bool {
	return in.InstanceType != "" || in.Bare || in.WithAdmin || in.WithRole != ""
}

// RestorePlan is what Restore will do, resolved by PlanRestore with
// read-only calls so that the CLI can show it before asking.
type RestorePlan struct {
	Name   string
	Mode   string
	Backup Backup

	// New-instance mode only.
	InstanceType string
	Architecture string
	Bare         bool
	WithAdmin    bool
	WithRole     string

	// Warnings are things the user should know before confirming; the
	// CLI prints them even with --yes.
	Warnings []string

	instanceID string // in-place mode: the instance to restore
}

// RestoreResult is what the CLI renders after a restore.
type RestoreResult struct {
	Name            string `json:"name"`
	Action          string `json:"action"` // always "restored"
	Mode            string `json:"mode"`
	Label           string `json:"label"`
	InstanceID      string `json:"instance_id"`
	InstanceType    string `json:"instance_type,omitempty"`
	URL             string `json:"url,omitempty"`
	Bare            bool   `json:"bare,omitempty"`
	IAMRole         string `json:"iam_role,omitempty"`
	InstanceProfile string `json:"instance_profile,omitempty"`
}

// PlanRestore decides how to restore the named instance. When the
// instance exists, the backup goes back in place. When nakama has no
// record of it (it was deleted), the backup is restored onto a new
// instance of the same name; the settings come from the tags of the
// backup, overridden by the input. Everything that can be checked
// before creating resources is checked here.
func (s *Service) PlanRestore(ctx context.Context, in RestoreInput) (*RestorePlan, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}
	if in.WithAdmin && in.WithRole != "" {
		return nil, errors.New("--with-admin and --with-role are mutually exclusive")
	}
	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if err != nil {
		return nil, err
	}
	if instanceID != "" {
		return s.planInPlace(ctx, in, instanceID)
	}
	return s.planNewInstance(ctx, in)
}

func (s *Service) planInPlace(ctx context.Context, in RestoreInput, instanceID string) (*RestorePlan, error) {
	state, _, err := s.fetchInstanceStateAndType(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	switch ec2types.InstanceStateName(state) {
	case "", ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown:
		// The metadata outlived the instance (a delete that stopped
		// halfway, or a termination outside nakama). Building a new one
		// over the leftover records would mix two instances' resources.
		return nil, fmt.Errorf("nakama instance %q is recorded as %s, but that EC2 instance no longer exists; "+
			"run \"nakama aws delete %s --force\" to clear what is left, then restore again",
			in.Name, instanceID, in.Name)
	}
	if in.hasNewInstanceOptions() {
		return nil, fmt.Errorf("nakama instance %q exists, so restore replaces its disk in place; "+
			"--instance-type, --bare, --with-admin and --with-role apply only to a deleted instance "+
			"(use \"nakama aws edit\" to change the instance type)", in.Name)
	}
	b, err := s.findBackup(ctx, in.Name, in.Label)
	if err != nil {
		return nil, err
	}
	return &RestorePlan{Name: in.Name, Mode: RestoreModeInPlace, Backup: *b, instanceID: instanceID}, nil
}

func (s *Service) planNewInstance(ctx context.Context, in RestoreInput) (*RestorePlan, error) {
	if err := ValidateNewInstanceName(in.Name); err != nil {
		return nil, err
	}
	backups, err := s.ListBackups(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	if len(backups) == 0 {
		return nil, fmt.Errorf("nakama instance %q not found, and it has no backups (see \"nakama aws backup list\")", in.Name)
	}
	b, err := s.findBackup(ctx, in.Name, in.Label)
	if err != nil {
		return nil, err
	}
	if b.State != string(ec2types.SnapshotStateCompleted) {
		return nil, fmt.Errorf("backup %q of %q is %s; restore it once \"nakama aws backup list\" shows it completed",
			b.Label, in.Name, b.State)
	}
	// An EC2 instance carrying the name without nakama's record of it
	// would end up as a second instance of the same name.
	if err := s.assertNameAvailable(ctx, in.Name); err != nil {
		return nil, fmt.Errorf("cannot restore %q as a new instance: %w, but nakama has no record of it", in.Name, err)
	}
	if err := s.requireInitialized(ctx); err != nil {
		return nil, err
	}

	p := &RestorePlan{
		Name: in.Name, Mode: RestoreModeNewInstance, Backup: *b,
		WithAdmin: in.WithAdmin, WithRole: in.WithRole,
	}
	untagged := b.InstanceType == "" && b.Architecture == "" && b.Kind == ""

	switch b.Kind {
	case instanceKindBare:
		p.Bare = true
	case instanceKindVSCode:
		if in.Bare {
			return nil, fmt.Errorf("backup %q was taken from a code-server instance; --bare does not apply", b.Label)
		}
	default:
		p.Bare = in.Bare
	}

	p.InstanceType = in.InstanceType
	if p.InstanceType == "" {
		p.InstanceType = b.InstanceType
	}
	if p.InstanceType == "" {
		return nil, fmt.Errorf("backup %q was taken before nakama recorded the instance's settings; "+
			"pass --instance-type (and --bare if it was a bare instance)", b.Label)
	}
	if p.Architecture, err = s.resolveInstanceArchitecture(ctx, p.InstanceType); err != nil {
		return nil, err
	}
	// The disk holds an operating system built for one architecture; an
	// instance of the other would not boot it.
	if b.Architecture != "" && b.Architecture != p.Architecture {
		return nil, fmt.Errorf("backup %q holds a system for %s, but %s is %s; choose an instance type for %s",
			b.Label, b.Architecture, p.InstanceType, p.Architecture, b.Architecture)
	}
	if untagged {
		kind := "a code-server instance"
		if p.Bare {
			kind = "a bare instance"
		}
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"backup %q was taken before nakama recorded the instance's settings, so nakama cannot check that "+
				"its system is for %s (%s), or that it was %s. With the wrong architecture the instance does not boot; "+
				"delete it and restore again with a matching --instance-type", b.Label, p.Architecture, p.InstanceType, kind))
	}
	if b.IAMRole != "" && !in.WithAdmin && in.WithRole == "" {
		hint := "--with-role " + b.IAMRole
		if b.IAMRole == naming.AdminInstanceRole() {
			hint = "--with-admin"
		}
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"the instance had the IAM role %q; restore does not attach it again. Pass %s to attach it", b.IAMRole, hint))
	}
	return p, nil
}

// Restore carries out a plan from PlanRestore. Either way the instance
// is left stopped.
func (s *Service) Restore(ctx context.Context, p *RestorePlan) (*RestoreResult, error) {
	if p == nil {
		return nil, errors.New("restore: no plan")
	}
	switch p.Mode {
	case RestoreModeInPlace:
		return s.restoreInPlace(ctx, p)
	case RestoreModeNewInstance:
		return s.restoreNewInstance(ctx, p)
	default:
		return nil, fmt.Errorf("restore: unknown mode %q", p.Mode)
	}
}

// restoreInPlace swaps the root volume of the existing instance for one
// created from the backup and deletes the previous root volume.
func (s *Service) restoreInPlace(ctx context.Context, p *RestorePlan) (*RestoreResult, error) {
	if err := s.replaceRootVolume(ctx, p.Name, p.instanceID, p.Backup.SnapshotID); err != nil {
		return nil, err
	}
	// The instance is left stopped; reconcile the shared NAT instance so
	// it is stopped too when no code-server instance is active.
	if err := s.reconcileNATInstancePowerState(ctx); err != nil {
		slog.Warn("restore: NAT instance reconcile failed", "error", err)
	}
	slog.Info("restore: completed (instance left stopped)", "instance", p.Name)
	return &RestoreResult{
		Name: p.Name, Action: "restored", Mode: p.Mode, Label: p.Backup.Label, InstanceID: p.instanceID,
	}, nil
}

// restoreNewInstance rebuilds a deleted instance from its backup: it
// launches a fresh Ubuntu instance with the same name and the resources
// create makes, then swaps the backup in as its root volume. The URL,
// the code-server password and the private IP are new.
func (s *Service) restoreNewInstance(ctx context.Context, p *RestorePlan) (*RestoreResult, error) {
	// As in create, the IAM role is prepared (and the caller's
	// permissions preflighted) before any other resource exists.
	var roleSel *instanceProfileSelection
	if p.WithAdmin || p.WithRole != "" {
		var err error
		if roleSel, err = s.ensureInstanceProfileForCreate(ctx, CreateInstanceInput{
			Name: p.Name, WithAdmin: p.WithAdmin, WithRole: p.WithRole,
		}); err != nil {
			return nil, err
		}
	}
	slog.Info("restore: building a new instance", "name", p.Name, "label", p.Backup.Label,
		"instance_type", p.InstanceType, "bare", p.Bare)

	amiID, err := s.clients.AMIResolver.LatestUbuntu2604(ctx, p.Architecture)
	if err != nil {
		return nil, fmt.Errorf("resolve ubuntu 26.04 %s ami: %w", p.Architecture, err)
	}
	// The password of the deleted instance went with its SSM parameter;
	// the restored code-server is switched to this one on first boot.
	if !p.Bare {
		password, err := generatePassword()
		if err != nil {
			return nil, fmt.Errorf("generate password: %w", err)
		}
		if err := s.putPasswordSecure(ctx, p.Name, password); err != nil {
			return nil, fmt.Errorf("store password: %w", err)
		}
	}
	net, err := s.prepareInstanceNetwork(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	ud, err := userdata.RenderRestore(userdata.RestoreParams{
		InstanceName:         p.Name,
		Region:               config.Region,
		Bare:                 p.Bare,
		SSMPasswordParameter: ssmPasswordPath(p.Name),
		SSMVersionParameter:  ssmVersionPath(p.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("render user data: %w", err)
	}
	if err := s.ensureNATInstanceRunning(ctx); err != nil {
		return nil, fmt.Errorf("start NAT instance: %w", err)
	}
	instanceID, err := s.runInstance(ctx, runInstanceArgs{
		Name:                p.Name,
		AMIID:               amiID,
		InstanceType:        p.InstanceType,
		EBSGiB:              config.EBSDefaultGiB, // replaced by the backup below
		ENIID:               net.eniID,
		UserDataBase64:      base64.StdEncoding.EncodeToString([]byte(ud)),
		InstanceProfileName: selectedProfileName(roleSel),
	})
	if err != nil {
		return nil, fmt.Errorf("run instance: %w", err)
	}

	// From here on, every resource is recorded as soon as it exists, so
	// that `nakama aws delete` can clean up after a failure.
	meta := instanceMetaValues{
		InstanceID:      instanceID,
		ENIID:           net.eniID,
		SecurityGroupID: net.sgID,
		IAMRole:         selectedRoleName(roleSel),
		InstanceProfile: selectedProfileName(roleSel),
		Bare:            p.Bare,
	}
	if err := s.writeInstanceMeta(ctx, p.Name, instanceMetaEntries(p.Name, meta)); err != nil {
		return nil, fmt.Errorf("write instance metadata: %w", err)
	}
	if err := s.waitInstanceRunning(ctx, instanceID); err != nil {
		return nil, fmt.Errorf("wait instance running: %w", err)
	}

	if !p.Bare {
		privateDNS, err := s.privateDNSNameForInstance(ctx, instanceID)
		if err != nil {
			return nil, fmt.Errorf("resolve private DNS: %w", err)
		}
		if meta.VPCOriginID, err = s.createInstanceVPCOrigin(ctx, p.Name, instanceID); err != nil {
			return nil, fmt.Errorf("create vpc origin: %w", err)
		}
		if err := s.writeInstanceMeta(ctx, p.Name, instanceMetaEntries(p.Name, meta)); err != nil {
			return nil, fmt.Errorf("write instance metadata: %w", err)
		}
		if meta.DistributionID, meta.DistributionDomain, err = s.createInstanceDistribution(
			ctx, p.Name, meta.VPCOriginID, privateDNS, net.base.WAFWebACLARN); err != nil {
			return nil, fmt.Errorf("create distribution: %w", err)
		}
		if err := s.writeInstanceMeta(ctx, p.Name, instanceMetaEntries(p.Name, meta)); err != nil {
			return nil, fmt.Errorf("write instance metadata: %w", err)
		}
	}

	// The distribution deploys in the background while the disk is
	// swapped. replaceRootVolume stops the instance first.
	if err := s.replaceRootVolume(ctx, p.Name, instanceID, p.Backup.SnapshotID); err != nil {
		return nil, fmt.Errorf("%w (the new instance is recorded; \"nakama aws delete %s\" removes it)", err, p.Name)
	}
	if err := s.reconcileNATInstancePowerState(ctx); err != nil {
		slog.Warn("restore: NAT instance reconcile failed", "error", err)
	}
	res := &RestoreResult{
		Name: p.Name, Action: "restored", Mode: p.Mode, Label: p.Backup.Label,
		InstanceID: instanceID, InstanceType: p.InstanceType, Bare: p.Bare,
		IAMRole: meta.IAMRole, InstanceProfile: meta.InstanceProfile,
	}
	if !p.Bare {
		if err := s.waitDistributionDeployed(ctx, meta.DistributionID); err != nil {
			return nil, fmt.Errorf("wait distribution: %w", err)
		}
		res.URL = "https://" + meta.DistributionDomain
	}
	slog.Info("restore: completed as a new instance (left stopped)", "name", p.Name, "instance_id", instanceID)
	return res, nil
}

// replaceRootVolume stops the instance, creates a volume from the
// snapshot, swaps it in as the root device, and deletes the previous
// root volume. The instance is left stopped.
func (s *Service) replaceRootVolume(ctx context.Context, name, instanceID, snapshotID string) error {
	device, oldVolumeID, az, volType, err := s.rootVolumeDetails(ctx, instanceID)
	if err != nil {
		return err
	}

	slog.Info("restore: stopping instance", "instance", name, "snapshot_id", snapshotID)
	if _, err := s.clients.EC2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
		return fmt.Errorf("StopInstances: %w", err)
	}
	if err := s.waitInstanceStopped(ctx, instanceID); err != nil {
		return err
	}

	tags := tagging.ForInstance(name, tagging.ComponentEBS, naming.EC2(name))
	volOut, err := s.clients.EC2.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		SnapshotId:       aws.String(snapshotID),
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
	// Volumes attached after launch default to DeleteOnTermination=
	// false (unlike the launch-time root volume, which create sets to
	// true). Without re-asserting it here, `delete` would terminate
	// the instance but leave this root volume behind in state
	// "available", silently billing forever.
	if err := s.setDeleteOnTermination(ctx, instanceID, device, newVolumeID); err != nil {
		return err
	}

	if _, err := s.clients.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(oldVolumeID)}); err != nil {
		return fmt.Errorf("DeleteVolume(%s): %w", oldVolumeID, err)
	}
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

// setDeleteOnTermination marks the volume attached at device so it is
// deleted together with the instance, restoring the launch-time
// behavior for a root volume that was swapped in after launch.
func (s *Service) setDeleteOnTermination(ctx context.Context, instanceID, device, volumeID string) error {
	if _, err := s.clients.EC2.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		BlockDeviceMappings: []ec2types.InstanceBlockDeviceMappingSpecification{{
			DeviceName: aws.String(device),
			Ebs: &ec2types.EbsInstanceBlockDeviceSpecification{
				DeleteOnTermination: aws.Bool(true),
				VolumeId:            aws.String(volumeID),
			},
		}},
	}); err != nil {
		return fmt.Errorf("ModifyInstanceAttribute(DeleteOnTermination, %s): %w", volumeID, err)
	}
	slog.Info("restore: delete-on-termination re-asserted", "volume", volumeID, "device", device)
	return nil
}
