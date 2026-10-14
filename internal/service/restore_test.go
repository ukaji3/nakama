package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"
	wafv2types "github.com/aws/aws-sdk-go-v2/service/wafv2/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// --- fakes ----------------------------------------------------------------

// fakeRestoreEC2 is a small EC2 model for restore: one nakama instance
// ("i-new" once launched, or a pre-existing "i-old"), the NAT instance,
// volumes, and the backups. Every mutating call is appended to log, so
// tests can check the order of operations.
type fakeRestoreEC2 struct {
	fakeEC2
	mu sync.Mutex

	snaps      []ec2types.Snapshot
	archByType map[string]string
	// namedOrphan makes assertNameAvailable see an EC2 instance carrying
	// the name although SSM has no record of it.
	namedOrphan bool

	// instances by ID (i-old for in-place tests, i-new once launched).
	state    map[string]ec2types.InstanceStateName
	rootVol  map[string]string // instance -> root volume ID
	volState map[string]ec2types.VolumeState
	userData string
	launched *ec2.RunInstancesInput

	log []string
}

func newFakeRestoreEC2(snaps ...ec2types.Snapshot) *fakeRestoreEC2 {
	return &fakeRestoreEC2{
		snaps:      snaps,
		archByType: map[string]string{"t3.medium": config.ArchX8664, "t4g.medium": config.ArchARM64, "m7g.xlarge": config.ArchARM64},
		state:      map[string]ec2types.InstanceStateName{},
		rootVol:    map[string]string{},
		volState:   map[string]ec2types.VolumeState{},
	}
}

func (f *fakeRestoreEC2) record(format string, a ...any) {
	f.log = append(f.log, fmt.Sprintf(format, a...))
}

func restoreFilterValue(fs []ec2types.Filter, name string) (string, bool) {
	for _, fl := range fs {
		if aws.ToString(fl.Name) == name && len(fl.Values) > 0 {
			return fl.Values[0], true
		}
	}
	return "", false
}

func (f *fakeRestoreEC2) instance(id string) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:     aws.String(id),
		InstanceType:   "t4g.medium",
		State:          &ec2types.InstanceState{Name: f.state[id]},
		RootDeviceName: aws.String("/dev/sda1"),
		Placement:      &ec2types.Placement{AvailabilityZone: aws.String("ap-northeast-1a")},
		PrivateDnsName: aws.String("ip-10-0-1-10.ap-northeast-1.compute.internal"),
		BlockDeviceMappings: []ec2types.InstanceBlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs:        &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String(f.rootVol[id])},
		}},
	}
}

func (f *fakeRestoreEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	one := func(i ec2types.Instance) *ec2.DescribeInstancesOutput {
		return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{i}}}}
	}
	if len(in.InstanceIds) > 0 {
		id := in.InstanceIds[0]
		if id == "i-nat" {
			return one(ec2types.Instance{InstanceId: aws.String("i-nat"), State: &ec2types.InstanceState{Name: f.state["i-nat"]}}), nil
		}
		if _, ok := f.state[id]; !ok {
			return nil, &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound", Message: id}
		}
		return one(f.instance(id)), nil
	}
	if c, _ := restoreFilterValue(in.Filters, "tag:"+tagging.KeyComponent); c == tagging.ComponentNATInstance {
		return one(ec2types.Instance{InstanceId: aws.String("i-nat"), State: &ec2types.InstanceState{Name: f.state["i-nat"]}}), nil
	}
	if _, ok := restoreFilterValue(in.Filters, "tag:"+tagging.KeyName); ok {
		if f.namedOrphan {
			return one(ec2types.Instance{InstanceId: aws.String("i-orphan")}), nil
		}
		return &ec2.DescribeInstancesOutput{}, nil
	}
	// hasActiveCodeServerInstances: the pending/running code-server ones.
	out := &ec2.DescribeInstancesOutput{}
	for id, st := range f.state {
		if id != "i-nat" && (st == ec2types.InstanceStateNameRunning || st == ec2types.InstanceStateNamePending) {
			out.Reservations = append(out.Reservations, ec2types.Reservation{Instances: []ec2types.Instance{f.instance(id)}})
		}
	}
	return out, nil
}

func (f *fakeRestoreEC2) DescribeSnapshots(context.Context, *ec2.DescribeSnapshotsInput, ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	return &ec2.DescribeSnapshotsOutput{Snapshots: f.snaps}, nil
}

func (f *fakeRestoreEC2) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	t := string(in.InstanceTypes[0])
	arch, ok := f.archByType[t]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "InvalidInstanceType", Message: t}
	}
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []ec2types.InstanceTypeInfo{{
		ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{ec2types.ArchitectureType(arch)}},
	}}}, nil
}

func (f *fakeRestoreEC2) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{Vpcs: []ec2types.Vpc{{VpcId: aws.String("vpc-1")}}}, nil
}

func (f *fakeRestoreEC2) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{Subnets: []ec2types.Subnet{{SubnetId: aws.String("subnet-1a")}}}, nil
}

func (f *fakeRestoreEC2) DescribeManagedPrefixLists(context.Context, *ec2.DescribeManagedPrefixListsInput, ...func(*ec2.Options)) (*ec2.DescribeManagedPrefixListsOutput, error) {
	return &ec2.DescribeManagedPrefixListsOutput{PrefixLists: []ec2types.ManagedPrefixList{{PrefixListId: aws.String("pl-cf")}}}, nil
}

func (f *fakeRestoreEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{}, nil
}

func (f *fakeRestoreEC2) CreateSecurityGroup(context.Context, *ec2.CreateSecurityGroupInput, ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	f.record("CreateSecurityGroup")
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-new")}, nil
}

func (f *fakeRestoreEC2) AuthorizeSecurityGroupIngress(context.Context, *ec2.AuthorizeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (f *fakeRestoreEC2) DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return &ec2.DescribeNetworkInterfacesOutput{}, nil
}

func (f *fakeRestoreEC2) CreateNetworkInterface(context.Context, *ec2.CreateNetworkInterfaceInput, ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error) {
	f.record("CreateNetworkInterface")
	return &ec2.CreateNetworkInterfaceOutput{NetworkInterface: &ec2types.NetworkInterface{NetworkInterfaceId: aws.String("eni-new")}}, nil
}

func (f *fakeRestoreEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RunInstances %s %s", in.InstanceType, aws.ToString(in.ImageId))
	f.launched = in
	ud, _ := base64.StdEncoding.DecodeString(aws.ToString(in.UserData))
	f.userData = string(ud)
	f.state["i-new"] = ec2types.InstanceStateNameRunning
	f.rootVol["i-new"] = "vol-launch"
	f.volState["vol-launch"] = ec2types.VolumeStateInUse
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-new")}}}, nil
}

func (f *fakeRestoreEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("StartInstances %s", in.InstanceIds[0])
	f.state[in.InstanceIds[0]] = ec2types.InstanceStateNameRunning
	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeRestoreEC2) StopInstances(_ context.Context, in *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("StopInstances %s", in.InstanceIds[0])
	f.state[in.InstanceIds[0]] = ec2types.InstanceStateNameStopped
	return &ec2.StopInstancesOutput{}, nil
}

func (f *fakeRestoreEC2) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := in.VolumeIds[0]
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{{
		VolumeId: aws.String(id), State: f.volState[id], VolumeType: ec2types.VolumeTypeGp3,
	}}}, nil
}

func (f *fakeRestoreEC2) CreateVolume(_ context.Context, in *ec2.CreateVolumeInput, _ ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateVolume %s %s %s", aws.ToString(in.SnapshotId), aws.ToString(in.AvailabilityZone), in.VolumeType)
	f.volState["vol-restored"] = ec2types.VolumeStateAvailable
	return &ec2.CreateVolumeOutput{VolumeId: aws.String("vol-restored")}, nil
}

func (f *fakeRestoreEC2) DetachVolume(_ context.Context, in *ec2.DetachVolumeInput, _ ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.VolumeId)
	f.record("DetachVolume %s", id)
	f.volState[id] = ec2types.VolumeStateAvailable
	return &ec2.DetachVolumeOutput{}, nil
}

func (f *fakeRestoreEC2) AttachVolume(_ context.Context, in *ec2.AttachVolumeInput, _ ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.VolumeId)
	f.record("AttachVolume %s %s %s", id, aws.ToString(in.InstanceId), aws.ToString(in.Device))
	f.volState[id] = ec2types.VolumeStateInUse
	f.rootVol[aws.ToString(in.InstanceId)] = id
	return &ec2.AttachVolumeOutput{}, nil
}

func (f *fakeRestoreEC2) ModifyInstanceAttribute(_ context.Context, in *ec2.ModifyInstanceAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error) {
	for _, m := range in.BlockDeviceMappings {
		if m.Ebs != nil && aws.ToBool(m.Ebs.DeleteOnTermination) {
			f.record("DeleteOnTermination %s", aws.ToString(m.Ebs.VolumeId))
		}
	}
	return &ec2.ModifyInstanceAttributeOutput{}, nil
}

func (f *fakeRestoreEC2) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	f.record("DeleteVolume %s", aws.ToString(in.VolumeId))
	return &ec2.DeleteVolumeOutput{}, nil
}

// fakeRestoreCF deploys VPC origins and distributions instantly.
type fakeRestoreCF struct {
	awsx.CloudFrontAPI
	ec2 *fakeRestoreEC2 // shares the call log
}

func (f *fakeRestoreCF) CreateVpcOrigin(context.Context, *cloudfront.CreateVpcOriginInput, ...func(*cloudfront.Options)) (*cloudfront.CreateVpcOriginOutput, error) {
	f.ec2.record("CreateVpcOrigin")
	return &cloudfront.CreateVpcOriginOutput{VpcOrigin: &cftypes.VpcOrigin{Id: aws.String("vo-1")}}, nil
}

func (f *fakeRestoreCF) GetVpcOrigin(context.Context, *cloudfront.GetVpcOriginInput, ...func(*cloudfront.Options)) (*cloudfront.GetVpcOriginOutput, error) {
	return &cloudfront.GetVpcOriginOutput{VpcOrigin: &cftypes.VpcOrigin{Status: aws.String("Deployed")}}, nil
}

func (f *fakeRestoreCF) CreateDistribution(context.Context, *cloudfront.CreateDistributionInput, ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error) {
	f.ec2.record("CreateDistribution")
	return &cloudfront.CreateDistributionOutput{Distribution: &cftypes.Distribution{
		Id: aws.String("E1"), DomainName: aws.String("dnew.cloudfront.net"), ARN: aws.String("arn:aws:cloudfront::1:distribution/E1"),
	}}, nil
}

func (f *fakeRestoreCF) TagResource(context.Context, *cloudfront.TagResourceInput, ...func(*cloudfront.Options)) (*cloudfront.TagResourceOutput, error) {
	return &cloudfront.TagResourceOutput{}, nil
}

func (f *fakeRestoreCF) GetDistribution(context.Context, *cloudfront.GetDistributionInput, ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	f.ec2.record("DistributionDeployed")
	return &cloudfront.GetDistributionOutput{Distribution: &cftypes.Distribution{Status: aws.String("Deployed")}}, nil
}

type fakeRestoreWAF struct{ awsx.WAFAPI }

func (fakeRestoreWAF) ListWebACLs(context.Context, *wafv2.ListWebACLsInput, ...func(*wafv2.Options)) (*wafv2.ListWebACLsOutput, error) {
	return &wafv2.ListWebACLsOutput{WebACLs: []wafv2types.WebACLSummary{{
		Name: aws.String(naming.WAFWebACL()), ARN: aws.String("arn:aws:wafv2:us-east-1:1:global/webacl/nakama-waf/x"),
	}}}, nil
}

type fakeAMI struct{ arch string }

func (f *fakeAMI) LatestUbuntu2604(_ context.Context, arch string) (string, error) {
	f.arch = arch
	return "ami-" + arch, nil
}

// restoreService wires the fakes. SSM starts with the init marker and
// whatever params the test adds.
func restoreService(ec2f *fakeRestoreEC2, params map[string]string) (*Service, *fakeParamSSM, *fakeAMI) {
	p := map[string]string{naming.SSMMetaInitialized(): "true"}
	for k, v := range params {
		p[k] = v
	}
	ssmf := &fakeParamSSM{params: p, onPut: func(name string) { ec2f.record("PutParameter %s", name) }}
	ami := &fakeAMI{}
	return &Service{clients: &awsx.Clients{
		EC2: ec2f, SSM: ssmf, CloudFront: &fakeRestoreCF{ec2: ec2f}, WAF: fakeRestoreWAF{}, AMIResolver: ami,
	}}, ssmf, ami
}

// taggedSnap is a completed backup carrying the source tags of #96.
func taggedSnap(id, label, at, instanceType, arch, kind, role string) ec2types.Snapshot {
	sn := snapNamed(id, "dev01", label, "", at)
	for k, v := range map[string]string{
		tagging.KeyInstanceType: instanceType, tagging.KeyArchitecture: arch, tagging.KeyKind: kind, tagging.KeyIAMRole: role,
	} {
		if v != "" {
			sn.Tags = append(sn.Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
	}
	return sn
}

// --- PlanRestore ----------------------------------------------------------

func TestPlanRestore_ExistingInstanceIsRestoredInPlace(t *testing.T) {
	t.Parallel()
	f := newFakeRestoreEC2(snapNamed("snap-1", "dev01", "l1", "", "2026-10-01T00:00:00Z"))
	f.state["i-old"] = ec2types.InstanceStateNameRunning
	svc, _, _ := restoreService(f, map[string]string{ssmInstanceIDPath("dev01"): "i-old"})

	p, err := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01"})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if p.Mode != RestoreModeInPlace || p.instanceID != "i-old" || p.Backup.Label != "l1" {
		t.Errorf("plan = %+v, want in-place on i-old from l1", p)
	}
	// Options for a new instance make no sense here, and silently
	// ignoring them would surprise the user.
	for _, in := range []RestoreInput{
		{Name: "dev01", InstanceType: "t3.medium"}, {Name: "dev01", Bare: true},
		{Name: "dev01", WithAdmin: true}, {Name: "dev01", WithRole: "r"},
	} {
		if _, err := svc.PlanRestore(context.Background(), in); err == nil || !strings.Contains(err.Error(), "in place") {
			t.Errorf("PlanRestore(%+v) error = %v, want a rejection of new-instance options", in, err)
		}
	}
	if len(f.log) != 0 {
		t.Errorf("planning must not change anything, got %v", f.log)
	}
}

func TestPlanRestore_DeletedInstanceIsRebuiltFromTheBackupTags(t *testing.T) {
	t.Parallel()
	f := newFakeRestoreEC2(
		taggedSnap("snap-old", "old", "2026-09-01T00:00:00Z", "t3.medium", "x86_64", "vscode", ""),
		taggedSnap("snap-new", "new", "2026-10-01T00:00:00Z", "m7g.xlarge", "arm64", "vscode", "nakama-instance-role-admin"),
	)
	svc, _, _ := restoreService(f, nil)

	p, err := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01"})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if p.Mode != RestoreModeNewInstance || p.Backup.Label != "new" || p.InstanceType != "m7g.xlarge" ||
		p.Architecture != config.ArchARM64 || p.Bare || p.WithAdmin || p.WithRole != "" {
		t.Errorf("plan = %+v, want a new arm64 m7g.xlarge code-server instance from the latest backup, no role", p)
	}
	// The admin role is not re-attached on its own, but the user is told.
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "--with-admin") {
		t.Errorf("warnings = %q, want one pointing at --with-admin", p.Warnings)
	}

	// --instance-type overrides within the architecture; --label picks.
	p, err = svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01", Label: "old", InstanceType: "t3.medium", WithRole: "r"})
	if err != nil {
		t.Fatalf("PlanRestore(old): %v", err)
	}
	if p.Backup.Label != "old" || p.InstanceType != "t3.medium" || p.WithRole != "r" || len(p.Warnings) != 0 {
		t.Errorf("plan = %+v", p)
	}
	if len(f.log) != 0 {
		t.Errorf("planning must not change anything, got %v", f.log)
	}
}

func TestPlanRestore_RejectsWhatCannotWorkBeforeCreatingAnything(t *testing.T) {
	t.Parallel()
	tagged := taggedSnap("snap-1", "l1", "2026-10-01T00:00:00Z", "m7g.xlarge", "arm64", "vscode", "")
	pending := taggedSnap("snap-p", "p1", "2026-10-02T00:00:00Z", "m7g.xlarge", "arm64", "vscode", "")
	pending.State = ec2types.SnapshotStatePending
	untagged := snapNamed("snap-u", "dev01", "u1", "", "2026-10-01T00:00:00Z")

	cases := []struct {
		name    string
		snaps   []ec2types.Snapshot
		orphan  bool
		params  map[string]string
		in      RestoreInput
		wantErr string
	}{
		{name: "no backups", in: RestoreInput{Name: "dev01"}, wantErr: "no backups"},
		{name: "unknown label", snaps: []ec2types.Snapshot{tagged}, in: RestoreInput{Name: "dev01", Label: "nope"}, wantErr: "not found"},
		{name: "backup still pending", snaps: []ec2types.Snapshot{pending}, in: RestoreInput{Name: "dev01"}, wantErr: "pending"},
		{name: "other architecture", snaps: []ec2types.Snapshot{tagged}, in: RestoreInput{Name: "dev01", InstanceType: "t3.medium"}, wantErr: "for arm64"},
		{name: "unknown instance type", snaps: []ec2types.Snapshot{tagged}, in: RestoreInput{Name: "dev01", InstanceType: "x9.huge"}, wantErr: "does not exist"},
		{name: "--bare on a code-server backup", snaps: []ec2types.Snapshot{tagged}, in: RestoreInput{Name: "dev01", Bare: true}, wantErr: "--bare does not apply"},
		{name: "old backup without --instance-type", snaps: []ec2types.Snapshot{untagged}, in: RestoreInput{Name: "dev01"}, wantErr: "--instance-type"},
		{name: "EC2 with the name but no record", snaps: []ec2types.Snapshot{tagged}, orphan: true, in: RestoreInput{Name: "dev01"}, wantErr: "no record"},
		{name: "record outlived the instance", snaps: []ec2types.Snapshot{tagged}, params: map[string]string{ssmInstanceIDPath("dev01"): "i-gone"}, in: RestoreInput{Name: "dev01"}, wantErr: "delete dev01 --force"},
		{name: "not initialized", snaps: []ec2types.Snapshot{tagged}, params: map[string]string{naming.SSMMetaInitialized(): "false"}, in: RestoreInput{Name: "dev01"}, wantErr: "nakama aws init"},
		{name: "reserved name", in: RestoreInput{Name: "start"}, wantErr: "reserved"},
		{name: "admin and role", in: RestoreInput{Name: "dev01", WithAdmin: true, WithRole: "r"}, wantErr: "mutually exclusive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRestoreEC2(c.snaps...)
			f.namedOrphan = c.orphan
			svc, ssmf, _ := restoreService(f, c.params)
			_, err := svc.PlanRestore(context.Background(), c.in)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
			if len(f.log) != 0 || len(ssmf.put) != 0 {
				t.Errorf("a rejected plan must not change anything: ec2=%v ssm=%v", f.log, ssmf.put)
			}
		})
	}
}

func TestPlanRestore_OldBackupNeedsTheTypeAndWarns(t *testing.T) {
	t.Parallel()
	f := newFakeRestoreEC2(snapNamed("snap-u", "dev01", "u1", "", "2026-07-01T00:00:00Z"))
	svc, _, _ := restoreService(f, nil)
	p, err := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01", InstanceType: "t4g.medium", Bare: true})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if !p.Bare || p.InstanceType != "t4g.medium" || p.Architecture != config.ArchARM64 {
		t.Errorf("plan = %+v, want a bare arm64 t4g.medium", p)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "cannot check") || !strings.Contains(p.Warnings[0], "bare") {
		t.Errorf("warnings = %q, want the unchecked-architecture warning for a bare instance", p.Warnings)
	}
}

// --- Restore: new instance --------------------------------------------------

func indexOf(log []string, prefix string) int {
	for i, l := range log {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func TestRestore_NewInstanceFlow(t *testing.T) {
	t.Parallel()
	f := newFakeRestoreEC2(taggedSnap("snap-b", "l1", "2026-10-01T00:00:00Z", "t4g.medium", "arm64", "vscode", ""))
	f.state["i-nat"] = ec2types.InstanceStateNameStopped
	svc, ssmf, ami := restoreService(f, nil)

	p, err := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01"})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	res, err := svc.Restore(context.Background(), p)
	if err != nil {
		t.Fatalf("Restore: %v\nlog: %v", err, f.log)
	}

	if ami.arch != config.ArchARM64 || f.launched == nil || f.launched.InstanceType != "t4g.medium" {
		t.Fatalf("launch = %+v (ami arch %q), want t4g.medium on the arm64 AMI", f.launched, ami.arch)
	}
	// The launch user data is the restore cloud-config, not the create
	// script: nothing is reinstalled and the host keys are kept.
	if !strings.HasPrefix(f.userData, "#cloud-config\n") || !strings.Contains(f.userData, "ssh_deletekeys: false") ||
		strings.Contains(f.userData, "install.sh") {
		t.Errorf("user data is not the restore cloud-config:\n%s", f.userData)
	}

	// A new password was stored, and every resource was recorded.
	if pw := ssmf.put[ssmPasswordPath("dev01")]; len(pw) != config.PasswordLength {
		t.Errorf("password parameter = %d chars, want a new %d-char password", len(pw), config.PasswordLength)
	}
	for path, want := range map[string]string{
		ssmInstanceIDPath("dev01"):         "i-new",
		ssmENIIDPath("dev01"):              "eni-new",
		ssmSGIDPath("dev01"):               "sg-new",
		ssmVPCOriginIDPath("dev01"):        "vo-1",
		ssmDistributionIDPath("dev01"):     "E1",
		ssmDistributionDomainPath("dev01"): "dnew.cloudfront.net",
	} {
		if ssmf.put[path] != want {
			t.Errorf("SSM %s = %q, want %q", path, ssmf.put[path], want)
		}
	}
	if _, ok := ssmf.put[ssmKindPath("dev01")]; ok {
		t.Errorf("a code-server instance must not be marked as bare")
	}

	// Order: launch, then the browser entry, then the disk swap (which
	// stops the instance first), deleting the fresh launch volume.
	want := []string{
		"RunInstances", "CreateVpcOrigin", "CreateDistribution",
		"StopInstances i-new", "CreateVolume snap-b ap-northeast-1a gp3", "DetachVolume vol-launch",
		"AttachVolume vol-restored i-new /dev/sda1", "DeleteOnTermination vol-restored", "DeleteVolume vol-launch",
		"DistributionDeployed",
	}
	last := -1
	for _, w := range want {
		i := indexOf(f.log, w)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in %v", w, f.log)
		}
		last = i
	}
	// Each resource is recorded before the disk swap, so that a failure
	// there still leaves something `delete` can clean up.
	stop := indexOf(f.log, "StopInstances i-new")
	for _, path := range []string{ssmInstanceIDPath("dev01"), ssmVPCOriginIDPath("dev01"), ssmDistributionIDPath("dev01")} {
		if i := indexOf(f.log, "PutParameter "+path); i < 0 || i > stop {
			t.Errorf("%s must be recorded before the swap; log %v", path, f.log)
		}
	}
	if i := indexOf(f.log, "PutParameter "+ssmInstanceIDPath("dev01")); i > indexOf(f.log, "CreateVpcOrigin") {
		t.Errorf("the instance must be recorded before CloudFront is set up; log %v", f.log)
	}
	if indexOf(f.log, "StartInstances i-new") >= 0 || f.state["i-new"] != ec2types.InstanceStateNameStopped {
		t.Errorf("the instance must be left stopped; state %s, log %v", f.state["i-new"], f.log)
	}
	if res.Mode != RestoreModeNewInstance || res.InstanceID != "i-new" || res.URL != "https://dnew.cloudfront.net" || res.Label != "l1" {
		t.Errorf("result = %+v", res)
	}
}

func TestRestore_NewBareInstanceHasNoBrowserPath(t *testing.T) {
	t.Parallel()
	f := newFakeRestoreEC2(taggedSnap("snap-b", "l1", "2026-10-01T00:00:00Z", "t4g.medium", "arm64", "bare", ""))
	f.state["i-nat"] = ec2types.InstanceStateNameRunning
	svc, ssmf, _ := restoreService(f, nil)
	p, err := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01"})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	res, err := svc.Restore(context.Background(), p)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if indexOf(f.log, "CreateVpcOrigin") >= 0 || indexOf(f.log, "CreateDistribution") >= 0 {
		t.Errorf("a bare restore must not touch CloudFront: %v", f.log)
	}
	if _, ok := ssmf.put[ssmPasswordPath("dev01")]; ok {
		t.Errorf("a bare instance has no password")
	}
	if ssmf.put[ssmKindPath("dev01")] != instanceKindBare {
		t.Errorf("kind = %q, want bare", ssmf.put[ssmKindPath("dev01")])
	}
	if strings.Contains(f.userData, "write_files") || !strings.Contains(f.userData, "ssh_deletekeys: false") {
		t.Errorf("bare user data should only keep the host keys:\n%s", f.userData)
	}
	if indexOf(f.log, "AttachVolume vol-restored i-new") < 0 || !res.Bare || res.URL != "" {
		t.Errorf("result = %+v, log %v", res, f.log)
	}
	// Nothing else runs, so the NAT instance is stopped again.
	if indexOf(f.log, "StopInstances i-nat") < 0 {
		t.Errorf("the NAT instance should be reconciled to stopped: %v", f.log)
	}
}

func TestRestore_InPlaceStillSwapsTheExistingRoot(t *testing.T) {
	t.Parallel()
	f := newFakeRestoreEC2(snapNamed("snap-1", "dev01", "l1", "", "2026-10-01T00:00:00Z"))
	f.state["i-old"] = ec2types.InstanceStateNameRunning
	f.state["i-nat"] = ec2types.InstanceStateNameRunning
	f.rootVol["i-old"] = "vol-cur"
	f.volState["vol-cur"] = ec2types.VolumeStateInUse
	svc, ssmf, _ := restoreService(f, map[string]string{ssmInstanceIDPath("dev01"): "i-old"})

	p, err := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01"})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	res, err := svc.Restore(context.Background(), p)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := []string{"StopInstances i-old", "CreateVolume snap-1", "DetachVolume vol-cur",
		"AttachVolume vol-restored i-old /dev/sda1", "DeleteOnTermination vol-restored", "DeleteVolume vol-cur"}
	last := -1
	for _, w := range want {
		i := indexOf(f.log, w)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in %v", w, f.log)
		}
		last = i
	}
	if indexOf(f.log, "RunInstances") >= 0 || len(ssmf.put) != 0 {
		t.Errorf("in-place restore must not build anything: log %v, ssm %v", f.log, ssmf.put)
	}
	if res.Mode != RestoreModeInPlace || res.InstanceID != "i-old" || res.Label != "l1" {
		t.Errorf("result = %+v", res)
	}
}
