package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
)

// The stages of CreateInstance at which a test makes one call fail.
const (
	stageENI          = "create-eni"          // CreateNetworkInterface
	stageRun          = "run-instance"        // RunInstances
	stageRunning      = "wait-running"        // waitInstanceRunning
	stageDNS          = "private-dns"         // privateDNSNameForInstance
	stageVPCOrigin    = "wait-vpc-origin"     // waitVPCOriginDeployed
	stageDistribution = "create-distribution" // CreateDistribution
	stageDistDeployed = "wait-distribution"   // waitDistributionDeployed
)

// injector fails the first call made at one stage. With cancel set, it
// cancels the create's context instead, as an interrupted command would.
type injector struct {
	at     string
	cancel context.CancelFunc
	fired  bool
}

func (j *injector) hit(ctx context.Context, stage string) error {
	if j.fired || j.at != stage {
		return nil
	}
	j.fired = true
	if j.cancel != nil {
		j.cancel()
		return ctx.Err()
	}
	return fmt.Errorf("injected failure at %s", stage)
}

// createEC2 extends the restore EC2 model with the failure stages and
// the calls DeleteInstance makes.
type createEC2 struct {
	*fakeRestoreEC2
	inj        *injector
	newLookups int
}

func (f *createEC2) CreateNetworkInterface(ctx context.Context, in *ec2.CreateNetworkInterfaceInput, opts ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error) {
	if err := f.inj.hit(ctx, stageENI); err != nil {
		return nil, err
	}
	return f.fakeRestoreEC2.CreateNetworkInterface(ctx, in, opts...)
}

func (f *createEC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	if err := f.inj.hit(ctx, stageRun); err != nil {
		return nil, err
	}
	return f.fakeRestoreEC2.RunInstances(ctx, in, opts...)
}

// DescribeInstances on the new instance is asked first by the running
// wait and next by the private DNS lookup.
func (f *createEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(in.InstanceIds) == 1 && in.InstanceIds[0] == "i-new" {
		f.newLookups++
		stage := stageRunning
		if f.newLookups > 1 {
			stage = stageDNS
		}
		if err := f.inj.hit(ctx, stage); err != nil {
			return nil, err
		}
	}
	return f.fakeRestoreEC2.DescribeInstances(ctx, in, opts...)
}

func (f *createEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("TerminateInstances %s", in.InstanceIds[0])
	f.state[in.InstanceIds[0]] = ec2types.InstanceStateNameTerminated
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *createEC2) DeleteNetworkInterface(_ context.Context, in *ec2.DeleteNetworkInterfaceInput, _ ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.record("DeleteNetworkInterface %s", aws.ToString(in.NetworkInterfaceId))
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (f *createEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.record("DeleteSecurityGroup %s", aws.ToString(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

// DescribeVolumes by tag is delete's orphan sweep: there are none.
func (f *createEC2) DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, opts ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	if len(in.VolumeIds) == 0 {
		return &ec2.DescribeVolumesOutput{}, nil
	}
	return f.fakeRestoreEC2.DescribeVolumes(ctx, in, opts...)
}

// createCF extends the restore CloudFront model with the failure stages
// and the calls DeleteInstance makes.
type createCF struct {
	*fakeRestoreCF
	inj         *injector
	originGone  bool
	distExists  bool
	distEnabled bool
}

func (f *createCF) GetVpcOrigin(ctx context.Context, _ *cloudfront.GetVpcOriginInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetVpcOriginOutput, error) {
	if f.originGone {
		return nil, apiErr("EntityNotFound")
	}
	if err := f.inj.hit(ctx, stageVPCOrigin); err != nil {
		return nil, err
	}
	return &cloudfront.GetVpcOriginOutput{ETag: aws.String("vo-etag"), VpcOrigin: &cftypes.VpcOrigin{Status: aws.String("Deployed")}}, nil
}

func (f *createCF) DeleteVpcOrigin(_ context.Context, in *cloudfront.DeleteVpcOriginInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteVpcOriginOutput, error) {
	f.ec2.record("DeleteVpcOrigin %s", aws.ToString(in.Id))
	f.originGone = true
	return &cloudfront.DeleteVpcOriginOutput{}, nil
}

func (f *createCF) CreateDistribution(ctx context.Context, in *cloudfront.CreateDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error) {
	if err := f.inj.hit(ctx, stageDistribution); err != nil {
		return nil, err
	}
	f.distExists, f.distEnabled = true, true
	return f.fakeRestoreCF.CreateDistribution(ctx, in, opts...)
}

func (f *createCF) GetDistribution(ctx context.Context, in *cloudfront.GetDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	if err := f.inj.hit(ctx, stageDistDeployed); err != nil {
		return nil, err
	}
	return f.fakeRestoreCF.GetDistribution(ctx, in, opts...)
}

func (f *createCF) GetDistributionConfig(context.Context, *cloudfront.GetDistributionConfigInput, ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	if !f.distExists {
		return nil, apiErr("NoSuchDistribution")
	}
	return &cloudfront.GetDistributionConfigOutput{
		ETag: aws.String("d-etag"), DistributionConfig: &cftypes.DistributionConfig{Enabled: aws.Bool(f.distEnabled)},
	}, nil
}

func (f *createCF) UpdateDistribution(_ context.Context, in *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	f.distEnabled = aws.ToBool(in.DistributionConfig.Enabled)
	return &cloudfront.UpdateDistributionOutput{}, nil
}

func (f *createCF) DeleteDistribution(_ context.Context, in *cloudfront.DeleteDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error) {
	f.ec2.record("DeleteDistribution %s", aws.ToString(in.Id))
	f.distExists = false
	return &cloudfront.DeleteDistributionOutput{}, nil
}

// createSSM refuses writes on a cancelled context, as the SDK does, so
// an interrupted create records nothing after the interruption.
type createSSM struct{ *fakeKirocrewSSM }

func (f createSSM) PutParameter(ctx context.Context, in *ssm.PutParameterInput, opts ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.fakeKirocrewSSM.PutParameter(ctx, in, opts...)
}

// newCreateAccount wires an initialized account with a running NAT
// instance, in which CreateInstance and DeleteInstance run end to end.
func newCreateAccount(inj *injector) (*Service, *createEC2, createSSM) {
	ec2f := &createEC2{fakeRestoreEC2: newFakeRestoreEC2(), inj: inj}
	ec2f.state["i-nat"] = ec2types.InstanceStateNameRunning
	ssmf := createSSM{newFakeKirocrewSSM(map[string]string{naming.SSMMetaInitialized(): "true"})}
	return &Service{clients: &awsx.Clients{
		EC2: ec2f, SSM: ssmf, WAF: fakeRestoreWAF{}, AMIResolver: &fakeAMI{},
		CloudFront: &createCF{fakeRestoreCF: &fakeRestoreCF{ec2: ec2f.fakeRestoreEC2}, inj: inj},
	}}, ec2f, ssmf
}

// recordedMeta returns the parameters under /nakama/dev01/meta/.
func recordedMeta(f createSSM) map[string]string {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	m := map[string]string{}
	for k, v := range f.params {
		if strings.HasPrefix(k, "/nakama/dev01/meta/") {
			m[k] = v
		}
	}
	return m
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// deletions returns the deleting calls in the log, from index from on.
func deletions(log []string, from int) []string {
	var out []string
	for _, l := range log[from:] {
		if strings.HasPrefix(l, "Delete") || strings.HasPrefix(l, "Terminate") {
			out = append(out, l)
		}
	}
	return out
}

// TestCreateInstance_RecordsEachResourceBeforeWaiting fails or
// interrupts create at each stage, then checks that every resource
// made by then is recorded and that delete removes exactly those.
func TestCreateInstance_RecordsEachResourceBeforeWaiting(t *testing.T) {
	t.Parallel()
	sg := map[string]string{ssmSGIDPath("dev01"): "sg-new"}
	network := map[string]string{ssmSGIDPath("dev01"): "sg-new", ssmENIIDPath("dev01"): "eni-new"}
	instance := instanceMetaEntries("dev01", instanceMetaValues{InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new"})
	bareInstance := instanceMetaEntries("dev01", instanceMetaValues{InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new", Bare: true})
	origin := instanceMetaEntries("dev01", instanceMetaValues{InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new", VPCOriginID: "vo-1"})
	dist := instanceMetaEntries("dev01", instanceMetaValues{
		InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new", VPCOriginID: "vo-1",
		DistributionID: "E1", DistributionDomain: "dnew.cloudfront.net",
	})
	ec2Gone := []string{"TerminateInstances i-new", "DeleteNetworkInterface eni-new", "DeleteSecurityGroup sg-new"}

	cases := []struct {
		stage   string
		bare    bool
		want    map[string]string
		deleted []string
	}{
		{stage: stageENI, want: sg, deleted: []string{"DeleteSecurityGroup sg-new"}},
		{stage: stageRun, want: network, deleted: []string{"DeleteNetworkInterface eni-new", "DeleteSecurityGroup sg-new"}},
		{stage: stageRunning, want: instance, deleted: ec2Gone},
		{stage: stageRunning, bare: true, want: bareInstance, deleted: ec2Gone},
		{stage: stageDNS, want: instance, deleted: ec2Gone},
		{stage: stageVPCOrigin, want: origin, deleted: append([]string{"DeleteVpcOrigin vo-1"}, ec2Gone...)},
		{stage: stageDistribution, want: origin, deleted: append([]string{"DeleteVpcOrigin vo-1"}, ec2Gone...)},
		{stage: stageDistDeployed, want: dist, deleted: append([]string{"DeleteDistribution E1", "DeleteVpcOrigin vo-1"}, ec2Gone...)},
	}
	for _, c := range cases {
		for _, cancel := range []bool{false, true} {
			name := c.stage
			if c.bare {
				name += "/bare"
			}
			if cancel {
				name += "/cancel"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, stop := context.WithCancel(context.Background())
				defer stop()
				inj := &injector{at: c.stage}
				if cancel {
					inj.cancel = stop
				}
				svc, ec2f, ssmf := newCreateAccount(inj)

				_, err := svc.CreateInstance(ctx, CreateInstanceInput{Name: "dev01", InstanceType: "t4g.medium", Bare: c.bare})
				if cancel && !errors.Is(err, context.Canceled) {
					t.Fatalf("CreateInstance error = %v, want the cancellation at %s", err, c.stage)
				}
				if !cancel && (err == nil || !strings.Contains(err.Error(), "injected failure")) {
					t.Fatalf("CreateInstance error = %v, want the failure injected at %s", err, c.stage)
				}
				if got := recordedMeta(ssmf); !sameMap(got, c.want) {
					t.Fatalf("recorded after failing at %s:\n got  %v\n want %v", c.stage, got, c.want)
				}

				n := len(ec2f.log)
				if err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01"}); err != nil {
					t.Fatalf("DeleteInstance: %v\nlog: %v", err, ec2f.log)
				}
				if got := deletions(ec2f.log, n); strings.Join(got, ", ") != strings.Join(c.deleted, ", ") {
					t.Errorf("delete removed %v, want %v", got, c.deleted)
				}
				ssmf.pmu.Lock()
				defer ssmf.pmu.Unlock()
				for k := range ssmf.params {
					if strings.HasPrefix(k, "/nakama/dev01/") {
						t.Errorf("delete left %s behind", k)
					}
				}
			})
		}
	}
}

// TestCreateInstance_FinalMetadata pins what a successful create leaves
// in SSM, for a code-server and a bare instance.
func TestCreateInstance_FinalMetadata(t *testing.T) {
	t.Parallel()
	for _, bare := range []bool{false, true} {
		t.Run(fmt.Sprintf("bare=%v", bare), func(t *testing.T) {
			t.Parallel()
			svc, ec2f, ssmf := newCreateAccount(&injector{})
			out, err := svc.CreateInstance(context.Background(), CreateInstanceInput{Name: "dev01", InstanceType: "t4g.medium", Bare: bare})
			if err != nil {
				t.Fatalf("CreateInstance: %v\nlog: %v", err, ec2f.log)
			}
			want := instanceMetaEntries("dev01", instanceMetaValues{
				InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new", VPCOriginID: "vo-1",
				DistributionID: "E1", DistributionDomain: "dnew.cloudfront.net",
			})
			wantURL := "https://dnew.cloudfront.net"
			if bare {
				want = instanceMetaEntries("dev01", instanceMetaValues{InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new", Bare: true})
				wantURL = ""
				if indexOf(ec2f.log, "CreateVpcOrigin") >= 0 || indexOf(ec2f.log, "CreateDistribution") >= 0 {
					t.Errorf("a bare create must not touch CloudFront: %v", ec2f.log)
				}
			}
			if got := recordedMeta(ssmf); !sameMap(got, want) {
				t.Errorf("metadata:\n got  %v\n want %v", got, want)
			}
			pw := ssmf.param(ssmPasswordPath("dev01"))
			if bare && pw != "" {
				t.Error("a bare instance has no password")
			}
			if !bare && len(pw) != config.PasswordLength {
				t.Errorf("password parameter = %d chars, want %d", len(pw), config.PasswordLength)
			}
			if out.InstanceID != "i-new" || out.URL != wantURL || out.Bare != bare ||
				out.DistributionID != want[ssmDistributionIDPath("dev01")] || out.VPCOriginID != want[ssmVPCOriginIDPath("dev01")] {
				t.Errorf("output = %+v", out)
			}
		})
	}
}

// TestRestore_NewInstanceRecordsTheVPCOriginBeforeWaiting: restore
// shares the split VPC origin creation, so a failed Deployed wait there
// also leaves the VPC origin recorded.
func TestRestore_NewInstanceRecordsTheVPCOriginBeforeWaiting(t *testing.T) {
	t.Parallel()
	svc, ec2f, ssmf := newCreateAccount(&injector{at: stageVPCOrigin})
	ec2f.snaps = []ec2types.Snapshot{taggedSnap("snap-b", "l1", "2026-10-01T00:00:00Z", "t4g.medium", "arm64", "vscode", "")}
	p, err := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01"})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if _, err := svc.Restore(context.Background(), p); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("Restore error = %v, want the injected failure", err)
	}
	if got := recordedMeta(ssmf)[ssmVPCOriginIDPath("dev01")]; got != "vo-1" {
		t.Errorf("vpc-origin-id = %q, want vo-1 recorded before the wait", got)
	}
}
