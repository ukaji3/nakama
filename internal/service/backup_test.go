package service

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/tagging"
)

// fakeSnapEC2 overrides only DescribeSnapshots; other EC2API methods
// panic via the embedded nil interface.
type fakeSnapEC2 struct {
	fakeEC2
	snaps []ec2types.Snapshot
}

func (f *fakeSnapEC2) DescribeSnapshots(_ context.Context, _ *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	return &ec2.DescribeSnapshotsOutput{Snapshots: f.snaps}, nil
}

func snap(id, label, note, t string) ec2types.Snapshot {
	tags := []ec2types.Tag{
		{Key: aws.String(tagging.KeyBackupLabel), Value: aws.String(label)},
		{Key: aws.String(tagging.KeyBackupTime), Value: aws.String(t)},
	}
	if note != "" {
		tags = append(tags, ec2types.Tag{Key: aws.String(tagging.KeyNote), Value: aws.String(note)})
	}
	return ec2types.Snapshot{SnapshotId: aws.String(id), State: ec2types.SnapshotStateCompleted, VolumeSize: aws.Int32(20), Tags: tags}
}

func TestListBackups_SortsNewestFirstAndMapsTags(t *testing.T) {
	t.Parallel()
	f := &fakeSnapEC2{snaps: []ec2types.Snapshot{
		snap("snap-old", "old", "", "2026-05-01T00:00:00Z"),
		snap("snap-new", "new", "memo", "2026-05-29T00:00:00Z"),
	}}
	got, err := newServiceWithEC2(f).ListBackups(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(got) != 2 || got[0].Label != "new" || got[1].Label != "old" {
		t.Fatalf("expected newest-first [new, old], got %+v", got)
	}
	if got[0].Note != "memo" || got[0].SnapshotID != "snap-new" || got[0].SizeGiB != 20 {
		t.Errorf("tag mapping wrong: %+v", got[0])
	}
}

func TestCreateBackup_RejectsInvalidLabelAndLongNote(t *testing.T) {
	t.Parallel()
	svc := newServiceWithEC2(&fakeSnapEC2{})
	if _, err := svc.CreateBackup(context.Background(), CreateBackupInput{Name: "dev01", Label: "Bad_Label"}); err == nil {
		t.Error("expected error for invalid label")
	}
	if _, err := svc.CreateBackup(context.Background(), CreateBackupInput{Name: "dev01", Note: strings.Repeat("x", maxNoteLen+1)}); err == nil {
		t.Error("expected error for over-long note")
	}
}

func TestFindBackup_LatestAndNotFound(t *testing.T) {
	t.Parallel()
	f := &fakeSnapEC2{snaps: []ec2types.Snapshot{
		snap("snap-old", "old", "", "2026-05-01T00:00:00Z"),
		snap("snap-new", "new", "", "2026-05-29T00:00:00Z"),
	}}
	svc := newServiceWithEC2(f)
	b, err := svc.findBackup(context.Background(), "dev01", "")
	if err != nil || b.Label != "new" {
		t.Fatalf("latest selection failed: b=%+v err=%v", b, err)
	}
	if _, err := svc.findBackup(context.Background(), "dev01", "missing"); err == nil {
		t.Error("expected not-found error for unknown label")
	}
}
