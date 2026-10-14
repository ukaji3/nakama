package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// backupLabelPattern validates a user-supplied backup label.
var backupLabelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

const maxNoteLen = 255

// CreateBackupInput captures the options for `nakama aws backup create`.
type CreateBackupInput struct {
	Name  string
	Label string // empty -> generated from timestamp
	Note  string
	Keep  int32 // <=0 -> no pruning
}

// Backup is one row of `nakama aws backup list`. Name is the owning
// nakama instance (surfaced when listing across all instances).
// SnapshotID is the AWS-native ID, surfaced only in JSON/YAML output.
type Backup struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	Note       string `json:"note,omitempty"`
	CreatedAt  string `json:"created_at"`
	State      string `json:"state"`
	SizeGiB    int32  `json:"size_gib"`
	SnapshotID string `json:"snapshot_id"`

	// The instance the backup was taken from, recorded so that restore
	// can rebuild it after a delete (Issue #96). Empty for backups
	// taken before nakama recorded them.
	InstanceType string `json:"instance_type,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Kind         string `json:"kind,omitempty"`
	IAMRole      string `json:"iam_role,omitempty"`
}

func validateNote(note string) error {
	if utf8.RuneCountInString(note) > maxNoteLen {
		return fmt.Errorf("--note must be at most %d characters", maxNoteLen)
	}
	return nil
}

// CreateBackup snapshots the root EBS volume of the named instance.
func (s *Service) CreateBackup(ctx context.Context, in CreateBackupInput) (*Backup, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}
	if err := validateNote(in.Note); err != nil {
		return nil, err
	}
	if in.Label == "" {
		in.Label = time.Now().UTC().Format("2006-01-02-1504")
	}
	if !backupLabelPattern.MatchString(in.Label) {
		return nil, fmt.Errorf("invalid --label %q: must match %s", in.Label, backupLabelPattern.String())
	}

	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if err != nil {
		return nil, err
	}
	if instanceID == "" {
		return nil, fmt.Errorf("nakama instance %q not found", in.Name)
	}
	volumeID, _, err := s.rootVolumeOf(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	src, err := s.backupSource(ctx, in.Name, instanceID)
	if err != nil {
		return nil, err
	}

	existing, err := s.ListBackups(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	for _, b := range existing {
		if b.Label == in.Label {
			return nil, fmt.Errorf("backup label %q already exists for instance %q", in.Label, in.Name)
		}
	}

	createdAt := time.Now().UTC().Format(time.RFC3339)
	tags := tagging.ForBackup(in.Name, in.Label, naming.Snapshot(in.Name, in.Label), createdAt, in.Note, src)
	csIn := &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeSnapshot, tags),
		},
	}
	if in.Note != "" {
		csIn.Description = aws.String(in.Note)
	}
	slog.Info("backup: creating snapshot", "instance", in.Name, "label", in.Label, "volume_id", volumeID)
	out, err := s.clients.EC2.CreateSnapshot(ctx, csIn)
	if err != nil {
		return nil, fmt.Errorf("CreateSnapshot: %w", err)
	}

	b := &Backup{
		Label: in.Label, Note: in.Note, CreatedAt: createdAt, State: string(out.State),
		InstanceType: src.InstanceType, Architecture: src.Architecture, Kind: src.Kind, IAMRole: src.IAMRole,
	}
	if out.SnapshotId != nil {
		b.SnapshotID = *out.SnapshotId
	}
	if out.VolumeSize != nil {
		b.SizeGiB = *out.VolumeSize
	}

	if in.Keep > 0 {
		if err := s.pruneBackups(ctx, in.Name, in.Keep); err != nil {
			return nil, fmt.Errorf("prune: %w", err)
		}
	}
	slog.Info("backup: created", "instance", in.Name, "label", in.Label, "snapshot_id", b.SnapshotID)
	return b, nil
}

// backupSource reads what restore needs to rebuild the instance once it
// is deleted (Issue #96): the instance type and architecture from EC2,
// and the kind and attached IAM role from the instance metadata.
func (s *Service) backupSource(ctx context.Context, name, instanceID string) (tagging.BackupSource, error) {
	var src tagging.BackupSource
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return src, fmt.Errorf("DescribeInstances: %w", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			src.InstanceType = string(i.InstanceType)
			src.Architecture = string(i.Architecture)
		}
	}
	kind, err := s.ssmGetOptional(ctx, ssmKindPath(name))
	if err != nil {
		return src, err
	}
	src.Kind = instanceKindVSCode
	if kind == instanceKindBare {
		src.Kind = instanceKindBare
	}
	if src.IAMRole, err = s.ssmGetOptional(ctx, ssmIAMRolePath(name)); err != nil {
		return src, err
	}
	return src, nil
}

// ListBackups returns the named instance's snapshots, newest first.
func (s *Service) ListBackups(ctx context.Context, name string) ([]Backup, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	return s.listBackups(ctx, name)
}

// ListAllBackups returns every nakama-managed backup across all
// instances, newest first. It exists so that `backup list` with no name
// can recover backups whose owning instance was already deleted (the
// instance name is otherwise hard to recall).
func (s *Service) ListAllBackups(ctx context.Context) ([]Backup, error) {
	return s.listBackups(ctx, "")
}

// listBackups queries backup snapshots. An empty name lists backups of
// every instance (each Backup carries its owning instance Name); a
// non-empty name is added as a tag:Name filter.
func (s *Service) listBackups(ctx context.Context, name string) ([]Backup, error) {
	filters := []ec2types.Filter{
		{Name: aws.String("tag:" + tagging.KeyManaged), Values: []string{tagging.ValueManagedTrue}},
		{Name: aws.String("tag:" + tagging.KeyComponent), Values: []string{tagging.ComponentBackup}},
	}
	if name != "" {
		filters = append(filters, ec2types.Filter{
			Name: aws.String("tag:" + tagging.KeyName), Values: []string{name},
		})
	}
	out, err := s.clients.EC2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
		Filters:  filters,
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeSnapshots: %w", err)
	}
	backups := make([]Backup, 0, len(out.Snapshots))
	for _, sn := range out.Snapshots {
		backups = append(backups, snapshotToBackup(sn))
	}
	// Newest first; break ties by instance name for a stable order when
	// listing across instances.
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].CreatedAt != backups[j].CreatedAt {
			return backups[i].CreatedAt > backups[j].CreatedAt
		}
		return backups[i].Name < backups[j].Name
	})
	return backups, nil
}

// snapshotToBackup maps an EC2 snapshot (with nakama tags) to a Backup.
func snapshotToBackup(sn ec2types.Snapshot) Backup {
	t := awshelper.EC2MapTags(sn.Tags)
	b := Backup{
		Name:      t[tagging.KeyName],
		Label:     t[tagging.KeyBackupLabel],
		Note:      t[tagging.KeyNote],
		CreatedAt: t[tagging.KeyBackupTime],
		State:     string(sn.State),

		InstanceType: t[tagging.KeyInstanceType],
		Architecture: t[tagging.KeyArchitecture],
		Kind:         t[tagging.KeyKind],
		IAMRole:      t[tagging.KeyIAMRole],
	}
	if b.CreatedAt == "" && sn.StartTime != nil {
		b.CreatedAt = sn.StartTime.UTC().Format(time.RFC3339)
	}
	if sn.SnapshotId != nil {
		b.SnapshotID = *sn.SnapshotId
	}
	if sn.VolumeSize != nil {
		b.SizeGiB = *sn.VolumeSize
	}
	return b
}

// findBackup resolves a backup by label (empty label -> newest).
func (s *Service) findBackup(ctx context.Context, name, label string) (*Backup, error) {
	backups, err := s.ListBackups(ctx, name)
	if err != nil {
		return nil, err
	}
	return selectBackup(backups, name, label)
}

// selectBackup picks a backup by label from a newest-first listing
// (empty label -> newest).
func selectBackup(backups []Backup, name, label string) (*Backup, error) {
	if len(backups) == 0 {
		return nil, fmt.Errorf("no backups found for instance %q", name)
	}
	if label == "" {
		return &backups[0], nil
	}
	for i := range backups {
		if backups[i].Label == label {
			return &backups[i], nil
		}
	}
	return nil, fmt.Errorf("backup label %q not found for instance %q", label, name)
}

// SetBackupNote sets or clears (empty note) the memo on a backup.
func (s *Service) SetBackupNote(ctx context.Context, name, label, note string) (*Backup, error) {
	if label == "" {
		return nil, errors.New("--label is required")
	}
	if err := validateNote(note); err != nil {
		return nil, err
	}
	b, err := s.findBackup(ctx, name, label)
	if err != nil {
		return nil, err
	}
	if note != "" {
		_, err = s.clients.EC2.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{b.SnapshotID},
			Tags:      []ec2types.Tag{{Key: aws.String(tagging.KeyNote), Value: aws.String(note)}},
		})
	} else {
		_, err = s.clients.EC2.DeleteTags(ctx, &ec2.DeleteTagsInput{
			Resources: []string{b.SnapshotID},
			Tags:      []ec2types.Tag{{Key: aws.String(tagging.KeyNote)}},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("update note: %w", err)
	}
	b.Note = note
	return b, nil
}

// DeleteBackup deletes the snapshot selected by label.
func (s *Service) DeleteBackup(ctx context.Context, name, label string) error {
	if label == "" {
		return errors.New("--label is required")
	}
	b, err := s.findBackup(ctx, name, label)
	if err != nil {
		return err
	}
	if _, err := s.clients.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(b.SnapshotID),
	}); err != nil {
		return fmt.Errorf("DeleteSnapshot: %w", err)
	}
	return nil
}

// pruneBackups keeps the newest `keep` snapshots and deletes the rest.
func (s *Service) pruneBackups(ctx context.Context, name string, keep int32) error {
	backups, err := s.ListBackups(ctx, name)
	if err != nil {
		return err
	}
	for i := int(keep); i < len(backups); i++ {
		slog.Info("backup: pruning old snapshot", "label", backups[i].Label, "snapshot_id", backups[i].SnapshotID)
		if _, err := s.clients.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
			SnapshotId: aws.String(backups[i].SnapshotID),
		}); err != nil {
			return fmt.Errorf("DeleteSnapshot(%s): %w", backups[i].SnapshotID, err)
		}
	}
	return nil
}
