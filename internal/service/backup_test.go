package service

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/awsx"
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

// snapNamed is snap() plus a nakama:name tag identifying the owning instance.
func snapNamed(id, name, label, note, t string) ec2types.Snapshot {
	sn := snap(id, label, note, t)
	sn.Tags = append(sn.Tags, ec2types.Tag{Key: aws.String(tagging.KeyName), Value: aws.String(name)})
	return sn
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

func TestListAllBackups_IncludesNameAcrossInstances(t *testing.T) {
	t.Parallel()
	f := &fakeSnapEC2{snaps: []ec2types.Snapshot{
		snapNamed("snap-a", "dev01", "old", "", "2026-05-01T00:00:00Z"),
		snapNamed("snap-b", "web02", "new", "memo", "2026-05-29T00:00:00Z"),
	}}
	got, err := newServiceWithEC2(f).ListAllBackups(context.Background())
	if err != nil {
		t.Fatalf("ListAllBackups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 backups, got %d", len(got))
	}
	if got[0].Name != "web02" || got[0].Label != "new" {
		t.Errorf("expected newest-first web02/new, got %+v", got[0])
	}
	if got[1].Name != "dev01" || got[1].Label != "old" {
		t.Errorf("expected dev01/old second, got %+v", got[1])
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

// fakeParamSSM answers GetParameter from a map; a missing key is
// ParameterNotFound, as in SSM. PutParameter records what was written.
type fakeParamSSM struct {
	awsx.SSMAPI
	params map[string]string
	put    map[string]string
	onPut  func(name string) // optional: observe the order of writes
}

func (f *fakeParamSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	v, ok := f.params[aws.ToString(in.Name)]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

func (f *fakeParamSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	if f.put == nil {
		f.put = map[string]string{}
	}
	f.put[aws.ToString(in.Name)] = aws.ToString(in.Value)
	if f.onPut != nil {
		f.onPut(aws.ToString(in.Name))
	}
	return &ssm.PutParameterOutput{}, nil
}

// fakeBackupEC2 serves one instance with a root volume and captures
// the CreateSnapshot request.
type fakeBackupEC2 struct {
	fakeSnapEC2
	instanceType ec2types.InstanceType
	arch         ec2types.ArchitectureValues
	got          *ec2.CreateSnapshotInput
}

func (f *fakeBackupEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId:     aws.String("i-src"),
		InstanceType:   f.instanceType,
		Architecture:   f.arch,
		RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []ec2types.InstanceBlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs:        &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-root")},
		}},
	}}}}}, nil
}

func (f *fakeBackupEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{{VolumeId: aws.String("vol-root"), Size: aws.Int32(40)}}}, nil
}

func (f *fakeBackupEC2) CreateSnapshot(_ context.Context, in *ec2.CreateSnapshotInput, _ ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
	f.got = in
	return &ec2.CreateSnapshotOutput{SnapshotId: aws.String("snap-1"), State: ec2types.SnapshotStatePending, VolumeSize: aws.Int32(40)}, nil
}

// TestCreateBackup_RecordsTheSourceInstance pins the tags restore needs
// to rebuild a deleted instance (Issue #96): type and architecture from
// EC2, kind and IAM role from the instance metadata.
func TestCreateBackup_RecordsTheSourceInstance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		params map[string]string
		want   map[string]string
		absent []string
	}{
		{
			name:   "code-server instance with a role",
			params: map[string]string{ssmIAMRolePath("dev01"): "nakama-admin-role"},
			want: map[string]string{
				tagging.KeyInstanceType: "m7g.xlarge", tagging.KeyArchitecture: "arm64",
				tagging.KeyKind: instanceKindVSCode, tagging.KeyIAMRole: "nakama-admin-role",
			},
		},
		{
			name:   "bare instance without a role",
			params: map[string]string{ssmKindPath("dev01"): instanceKindBare},
			want: map[string]string{
				tagging.KeyInstanceType: "m7g.xlarge", tagging.KeyArchitecture: "arm64", tagging.KeyKind: instanceKindBare,
			},
			absent: []string{tagging.KeyIAMRole},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			params := map[string]string{ssmInstanceIDPath("dev01"): "i-src"}
			for k, v := range c.params {
				params[k] = v
			}
			f := &fakeBackupEC2{instanceType: "m7g.xlarge", arch: ec2types.ArchitectureValuesArm64}
			svc := &Service{clients: &awsx.Clients{EC2: f, SSM: &fakeParamSSM{params: params}}}
			b, err := svc.CreateBackup(context.Background(), CreateBackupInput{Name: "dev01", Label: "before-x"})
			if err != nil {
				t.Fatalf("CreateBackup: %v", err)
			}
			if f.got == nil || len(f.got.TagSpecifications) != 1 {
				t.Fatalf("CreateSnapshot not called with one tag specification: %+v", f.got)
			}
			tags := awshelper.EC2MapTags(f.got.TagSpecifications[0].Tags)
			for k, want := range c.want {
				if tags[k] != want {
					t.Errorf("tag %s = %q, want %q", k, tags[k], want)
				}
			}
			for _, k := range c.absent {
				if _, ok := tags[k]; ok {
					t.Errorf("tag %s must be absent, got %q", k, tags[k])
				}
			}
			if b.InstanceType != "m7g.xlarge" || b.Kind != c.want[tagging.KeyKind] {
				t.Errorf("returned backup does not carry the source: %+v", b)
			}
		})
	}
}

// TestSnapshotToBackup_MapsTheSourceTags checks the read side.
func TestSnapshotToBackup_MapsTheSourceTags(t *testing.T) {
	t.Parallel()
	sn := snap("snap-1", "l", "", "2026-10-01T00:00:00Z")
	sn.Tags = append(sn.Tags,
		ec2types.Tag{Key: aws.String(tagging.KeyInstanceType), Value: aws.String("t3.medium")},
		ec2types.Tag{Key: aws.String(tagging.KeyArchitecture), Value: aws.String("x86_64")},
		ec2types.Tag{Key: aws.String(tagging.KeyKind), Value: aws.String("bare")},
		ec2types.Tag{Key: aws.String(tagging.KeyIAMRole), Value: aws.String("r")},
	)
	b := snapshotToBackup(sn)
	if b.InstanceType != "t3.medium" || b.Architecture != "x86_64" || b.Kind != "bare" || b.IAMRole != "r" {
		t.Errorf("source tags not mapped: %+v", b)
	}
	if old := snapshotToBackup(snap("snap-2", "l", "", "t")); old.InstanceType != "" || old.Kind != "" {
		t.Errorf("a backup without source tags must map to empty fields: %+v", old)
	}
}
