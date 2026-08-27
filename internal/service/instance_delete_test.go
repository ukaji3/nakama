package service

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

// TestDeleteInstanceAbortsWhenSSMFails pins the M4 fix: when the
// resource-ID reads fail for a reason other than ParameterNotFound
// (SSM outage, expired credentials), delete must abort with an error
// instead of treating every ID as absent and reporting success while
// CloudFront/EC2/ENI/SG survive.
func TestDeleteInstanceAbortsWhenSSMFails(t *testing.T) {
	t.Parallel()

	svc := &Service{clients: &awsx.Clients{
		// Only SSM is wired: DeleteInstance must fail before touching
		// any other client (a nil-pointer panic here would mean the
		// abort did not happen first).
		SSM: fakeSSMScripted{err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}},
	}}
	err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01"})
	if err == nil {
		t.Fatal("DeleteInstance succeeded despite SSM failure")
	}
	if !strings.Contains(err.Error(), "GetParameter") {
		t.Errorf("error does not surface the failing read: %v", err)
	}
}

// fakeVolumeSweepEC2 scripts DescribeVolumes/DeleteVolume for the
// orphaned-volume sweep. Each DescribeVolumes call consumes one page
// of states, simulating the asynchronous detach after termination.
type fakeVolumeSweepEC2 struct {
	awsx.EC2API
	pages   [][]ec2types.Volume
	calls   int
	deleted []string
}

func (f *fakeVolumeSweepEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	i := f.calls
	if i >= len(f.pages) {
		i = len(f.pages) - 1
	}
	f.calls++
	return &ec2.DescribeVolumesOutput{Volumes: f.pages[i]}, nil
}

func (f *fakeVolumeSweepEC2) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	f.deleted = append(f.deleted, *in.VolumeId)
	return &ec2.DeleteVolumeOutput{}, nil
}

// TestDeleteOrphanedVolumes pins the M3 sweep: available volumes are
// deleted, a volume still detaching (in-use) is re-observed and swept
// once it becomes available, and in-use volumes are never deleted.
func TestDeleteOrphanedVolumes(t *testing.T) {
	t.Parallel()

	vol := func(id string, st ec2types.VolumeState) ec2types.Volume {
		return ec2types.Volume{VolumeId: &id, State: st}
	}
	fake := &fakeVolumeSweepEC2{pages: [][]ec2types.Volume{
		{vol("vol-a", ec2types.VolumeStateAvailable), vol("vol-b", ec2types.VolumeStateInUse)},
		{vol("vol-b", ec2types.VolumeStateAvailable)},
	}}
	svc := &Service{clients: &awsx.Clients{EC2: fake}}
	if err := svc.deleteOrphanedVolumes(context.Background(), "dev01"); err != nil {
		t.Fatalf("deleteOrphanedVolumes: %v", err)
	}
	if len(fake.deleted) != 2 || fake.deleted[0] != "vol-a" || fake.deleted[1] != "vol-b" {
		t.Errorf("deleted = %v, want [vol-a vol-b]", fake.deleted)
	}
	if fake.calls < 2 {
		t.Errorf("DescribeVolumes calls = %d, want >= 2 (re-observation)", fake.calls)
	}
}

// fakeDOTEC2 captures the ModifyInstanceAttribute input.
type fakeDOTEC2 struct {
	awsx.EC2API
	got *ec2.ModifyInstanceAttributeInput
}

func (f *fakeDOTEC2) ModifyInstanceAttribute(_ context.Context, in *ec2.ModifyInstanceAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error) {
	f.got = in
	return &ec2.ModifyInstanceAttributeOutput{}, nil
}

// TestSetDeleteOnTermination pins the restore-side half of M3: the
// swapped-in root volume is re-marked DeleteOnTermination=true on the
// device it was attached at.
func TestSetDeleteOnTermination(t *testing.T) {
	t.Parallel()

	fake := &fakeDOTEC2{}
	svc := &Service{clients: &awsx.Clients{EC2: fake}}
	if err := svc.setDeleteOnTermination(context.Background(), "i-0123", "/dev/sda1", "vol-new"); err != nil {
		t.Fatalf("setDeleteOnTermination: %v", err)
	}
	if fake.got == nil || *fake.got.InstanceId != "i-0123" {
		t.Fatalf("input = %+v", fake.got)
	}
	m := fake.got.BlockDeviceMappings[0]
	if *m.DeviceName != "/dev/sda1" || !*m.Ebs.DeleteOnTermination || *m.Ebs.VolumeId != "vol-new" {
		t.Errorf("mapping = %+v", m)
	}
}
