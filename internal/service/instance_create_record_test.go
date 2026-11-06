package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
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

	// Cancelled right after the create call returned (injector.after).
	stageSGMade  = "sg-created"        // CreateSecurityGroup
	stageENIMade = "eni-created"       // CreateNetworkInterface
	stageRunMade = "instance-launched" // RunInstances

	// The PutParameter that records the resource fails (createSSM).
	stageRecordSG  = "record-sg"
	stageRecordENI = "record-eni"
	stageRecordEC2 = "record-instance"
)

// injector fails the first call made at one stage. With cancel set, it
// cancels the create's context instead, as an interrupted command would.
type injector struct {
	at     string
	cancel context.CancelFunc
	fired  bool
}

func (j *injector) hit(ctx context.Context, stage string) error {
	if j.fired || stage == "" || j.at != stage {
		return nil
	}
	j.fired = true
	if j.cancel != nil {
		j.cancel()
		return ctx.Err()
	}
	return fmt.Errorf("injected failure at %s", stage)
}

// after cancels the create's context once the call at stage has
// returned, as a Ctrl-C between a create call and its record would.
func (j *injector) after(stage string) {
	if j.fired || j.cancel == nil || j.at != stage {
		return
	}
	j.fired = true
	j.cancel()
}

// createEC2 extends the restore EC2 model with the failure stages, the
// calls DeleteInstance makes, and the tags of the account's instances,
// security groups and ENIs, which the tag lookups filter on.
type createEC2 struct {
	*fakeRestoreEC2
	inj        *injector
	newLookups int

	// Tags by resource ID. Created resources are added; tests add
	// others. An instance's state is in fakeRestoreEC2.state.
	instTags, sgTags, eniTags map[string]map[string]string
	// sloppy makes the tag lookups ignore their filters and return every
	// resource, so that delete's own check of the tags is what is tested.
	sloppy bool
	// lookups counts the tag lookups: security groups, ENIs, and
	// instances by nakama:name.
	lookups int
	// fail maps a lookup ("sg", "eni", "instance") to the error code it
	// answers with.
	fail map[string]string
}

func (f *createEC2) failed(lookup string) error {
	if code := f.fail[lookup]; code != "" {
		return apiErr(code)
	}
	return nil
}

// specTags returns the tags a create call puts on a resource of type rt.
func specTags(specs []ec2types.TagSpecification, rt ec2types.ResourceType) map[string]string {
	m := map[string]string{}
	for _, s := range specs {
		if s.ResourceType == rt {
			for _, t := range s.Tags {
				m[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}
	}
	return m
}

// matching returns, in ID order, the resources whose tags pass every
// tag filter exactly, as EC2 does for values without wildcards, and
// whose state (when state is given) passes the instance-state-name one.
func (f *createEC2) matching(tags map[string]map[string]string, filters []ec2types.Filter, state func(string) string) []string {
	var ids []string
	for id, have := range tags {
		ok := true
		for _, fl := range filters {
			name := aws.ToString(fl.Name)
			var v string
			switch {
			case strings.HasPrefix(name, "tag:"):
				v = have[strings.TrimPrefix(name, "tag:")]
			case name == "instance-state-name" && state != nil:
				v = state(id)
			default:
				continue // vpc-id, subnet-id: there is one of each
			}
			if !slices.Contains(fl.Values, v) {
				ok = false
			}
		}
		if ok || f.sloppy {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// The create calls below refuse a cancelled context, as the SDK does:
// they are what an interrupted create calls next.

func (f *createEC2) CreateSecurityGroup(ctx context.Context, in *ec2.CreateSecurityGroupInput, opts ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	out, err := f.fakeRestoreEC2.CreateSecurityGroup(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	f.sgTags[aws.ToString(out.GroupId)] = specTags(in.TagSpecifications, ec2types.ResourceTypeSecurityGroup)
	f.inj.after(stageSGMade)
	return out, nil
}

func (f *createEC2) AuthorizeSecurityGroupIngress(ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, opts ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.fakeRestoreEC2.AuthorizeSecurityGroupIngress(ctx, in, opts...)
}

func (f *createEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.lookups++
	if err := f.failed("sg"); err != nil {
		return nil, err
	}
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, id := range f.matching(f.sgTags, in.Filters, nil) {
		out.SecurityGroups = append(out.SecurityGroups, ec2types.SecurityGroup{GroupId: aws.String(id), Tags: awshelper.EC2Tags(f.sgTags[id])})
	}
	return out, nil
}

func (f *createEC2) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	f.lookups++
	if err := f.failed("eni"); err != nil {
		return nil, err
	}
	out := &ec2.DescribeNetworkInterfacesOutput{}
	for _, id := range f.matching(f.eniTags, in.Filters, nil) {
		out.NetworkInterfaces = append(out.NetworkInterfaces, ec2types.NetworkInterface{NetworkInterfaceId: aws.String(id), TagSet: awshelper.EC2Tags(f.eniTags[id])})
	}
	return out, nil
}

func (f *createEC2) CreateNetworkInterface(ctx context.Context, in *ec2.CreateNetworkInterfaceInput, opts ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error) {
	if err := f.inj.hit(ctx, stageENI); err != nil {
		return nil, err
	}
	out, err := f.fakeRestoreEC2.CreateNetworkInterface(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	f.eniTags[aws.ToString(out.NetworkInterface.NetworkInterfaceId)] = specTags(in.TagSpecifications, ec2types.ResourceTypeNetworkInterface)
	f.inj.after(stageENIMade)
	return out, nil
}

func (f *createEC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	if err := f.inj.hit(ctx, stageRun); err != nil {
		return nil, err
	}
	out, err := f.fakeRestoreEC2.RunInstances(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.instTags["i-new"] = specTags(in.TagSpecifications, ec2types.ResourceTypeInstance)
	f.mu.Unlock()
	f.inj.after(stageRunMade)
	return out, nil
}

// DescribeInstances on the new instance is asked first by the running
// wait and next by the private DNS lookup. A query by nakama:name is
// answered from the tags.
func (f *createEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, byName := restoreFilterValue(in.Filters, "tag:"+tagging.KeyName); byName && len(in.InstanceIds) == 0 {
		f.lookups++
		if err := f.failed("instance"); err != nil {
			return nil, err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		state := func(id string) string { return string(f.state[id]) }
		out := &ec2.DescribeInstancesOutput{}
		for _, id := range f.matching(f.instTags, in.Filters, state) {
			out.Reservations = append(out.Reservations, ec2types.Reservation{Instances: []ec2types.Instance{{
				InstanceId: aws.String(id), State: &ec2types.InstanceState{Name: f.state[id]}, Tags: awshelper.EC2Tags(f.instTags[id]),
			}}})
		}
		return out, nil
	}
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
	delete(f.eniTags, aws.ToString(in.NetworkInterfaceId))
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (f *createEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.record("DeleteSecurityGroup %s", aws.ToString(in.GroupId))
	delete(f.sgTags, aws.ToString(in.GroupId))
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
// an interrupted create records nothing after the interruption unless
// it records on a context of its own. It fails the write of one
// resource ID at the record stages.
type createSSM struct {
	*fakeKirocrewSSM
	inj *injector
}

var recordStages = map[string]string{
	ssmSGIDPath("dev01"):       stageRecordSG,
	ssmENIIDPath("dev01"):      stageRecordENI,
	ssmInstanceIDPath("dev01"): stageRecordEC2,
}

func (f createSSM) PutParameter(ctx context.Context, in *ssm.PutParameterInput, opts ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.inj.hit(ctx, recordStages[aws.ToString(in.Name)]); err != nil {
		return nil, err
	}
	return f.fakeKirocrewSSM.PutParameter(ctx, in, opts...)
}

// newCreateAccount wires an initialized account with a running NAT
// instance, in which CreateInstance and DeleteInstance run end to end.
func newCreateAccount(inj *injector) (*Service, *createEC2, createSSM) {
	ec2f := &createEC2{
		fakeRestoreEC2: newFakeRestoreEC2(), inj: inj,
		instTags: map[string]map[string]string{}, sgTags: map[string]map[string]string{}, eniTags: map[string]map[string]string{},
	}
	ec2f.state["i-nat"] = ec2types.InstanceStateNameRunning
	ssmf := createSSM{newFakeKirocrewSSM(map[string]string{naming.SSMMetaInitialized(): "true"}), inj}
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
				deleteRemoves(t, svc, ec2f, ssmf, c.deleted)
			})
		}
	}
}

// deleteRemoves runs delete dev01 and checks that it removes exactly
// the resources in deleted, in that order, and every /nakama/dev01/
// parameter.
func deleteRemoves(t *testing.T, svc *Service, ec2f *createEC2, ssmf createSSM, deleted []string) {
	t.Helper()
	n := len(ec2f.log)
	if err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01"}); err != nil {
		t.Fatalf("DeleteInstance: %v\nlog: %v", err, ec2f.log)
	}
	if got := deletions(ec2f.log, n); strings.Join(got, ", ") != strings.Join(deleted, ", ") {
		t.Errorf("delete removed %v, want %v", got, deleted)
	}
	ssmf.pmu.Lock()
	defer ssmf.pmu.Unlock()
	for k := range ssmf.params {
		if strings.HasPrefix(k, "/nakama/dev01/") {
			t.Errorf("delete left %s behind", k)
		}
	}
}

// TestCreateInstance_EachCreateCallIsRecordedOrFoundByTags covers the
// gap between a create call and its record, for the security group,
// the ENI and the EC2 instance of create and of restore (Amazon Q on
// PR #112). A Ctrl-C right after the call returned must not stop the
// record; a failing record must leave the resource where delete still
// finds it, by its tags.
func TestCreateInstance_EachCreateCallIsRecordedOrFoundByTags(t *testing.T) {
	t.Parallel()
	sg := map[string]string{ssmSGIDPath("dev01"): "sg-new"}
	network := map[string]string{ssmSGIDPath("dev01"): "sg-new", ssmENIIDPath("dev01"): "eni-new"}
	instance := instanceMetaEntries("dev01", instanceMetaValues{InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new"})
	bareInstance := instanceMetaEntries("dev01", instanceMetaValues{InstanceID: "i-new", ENIID: "eni-new", SecurityGroupID: "sg-new", Bare: true})
	sgGone := []string{"DeleteSecurityGroup sg-new"}
	networkGone := []string{"DeleteNetworkInterface eni-new", "DeleteSecurityGroup sg-new"}
	ec2Gone := []string{"TerminateInstances i-new", "DeleteNetworkInterface eni-new", "DeleteSecurityGroup sg-new"}

	cases := []struct {
		name    string
		stage   string
		cancel  bool
		bare    bool
		restore bool
		want    map[string]string // recorded when create stops
		deleted []string
	}{
		{name: "sg/record-fails", stage: stageRecordSG, want: map[string]string{}, deleted: sgGone},
		{name: "sg/cancel", stage: stageSGMade, cancel: true, want: sg, deleted: sgGone},
		{name: "eni/record-fails", stage: stageRecordENI, want: sg, deleted: networkGone},
		{name: "eni/cancel", stage: stageENIMade, cancel: true, want: network, deleted: networkGone},
		{name: "instance/record-fails", stage: stageRecordEC2, want: network, deleted: ec2Gone},
		{name: "instance/cancel", stage: stageRunMade, cancel: true, want: instance, deleted: ec2Gone},
		{name: "instance/bare/cancel", stage: stageRunMade, cancel: true, bare: true, want: bareInstance, deleted: ec2Gone},
		{name: "restore/sg/record-fails", stage: stageRecordSG, restore: true, want: map[string]string{}, deleted: sgGone},
		{name: "restore/instance/record-fails", stage: stageRecordEC2, restore: true, want: network, deleted: ec2Gone},
		{name: "restore/instance/cancel", stage: stageRunMade, cancel: true, restore: true, want: instance, deleted: ec2Gone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			inj := &injector{at: c.stage}
			if c.cancel {
				inj.cancel = stop
			}
			svc, ec2f, ssmf := newCreateAccount(inj)

			var err error
			if c.restore {
				ec2f.snaps = []ec2types.Snapshot{taggedSnap("snap-b", "l1", "2026-10-01T00:00:00Z", "t4g.medium", "arm64", "vscode", "")}
				p, perr := svc.PlanRestore(context.Background(), RestoreInput{Name: "dev01"})
				if perr != nil {
					t.Fatalf("PlanRestore: %v", perr)
				}
				_, err = svc.Restore(ctx, p)
			} else {
				_, err = svc.CreateInstance(ctx, CreateInstanceInput{Name: "dev01", InstanceType: "t4g.medium", Bare: c.bare})
			}
			if !inj.fired {
				t.Fatalf("the create never reached %s", c.stage)
			}
			if c.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want the cancellation after %s", err, c.stage)
			}
			if !c.cancel && (err == nil || !strings.Contains(err.Error(), "injected failure")) {
				t.Fatalf("error = %v, want the failure injected at %s", err, c.stage)
			}
			if got := recordedMeta(ssmf); !sameMap(got, c.want) {
				t.Fatalf("recorded after %s:\n got  %v\n want %v", c.stage, got, c.want)
			}
			deleteRemoves(t, svc, ec2f, ssmf, c.deleted)
		})
	}
}

// TestDeleteInstance_TagLookupTakesOnlyTheNamedInstance seeds an account
// with resources of dev01 that SSM has no record of, next to those of
// dev010 and dev0 (names sharing a prefix), ones without nakama's
// managed tag and a terminated dev01 instance. delete dev01 must remove
// every dev01 resource, two of each included, and nothing else. With
// sloppy the API ignores the filters, so delete's own check of the tags
// is what keeps the others safe.
func TestDeleteInstance_TagLookupTakesOnlyTheNamedInstance(t *testing.T) {
	t.Parallel()
	for _, sloppy := range []bool{false, true} {
		t.Run(fmt.Sprintf("sloppy=%v", sloppy), func(t *testing.T) {
			t.Parallel()
			svc, ec2f, ssmf := newCreateAccount(&injector{})
			ec2f.sloppy = sloppy
			seedForeignResources(ec2f)
			deleteRemoves(t, svc, ec2f, ssmf, []string{
				"TerminateInstances i-dev01-a", "TerminateInstances i-dev01-b",
				"DeleteNetworkInterface eni-dev01-a", "DeleteNetworkInterface eni-dev01-b",
				"DeleteSecurityGroup sg-dev01-a", "DeleteSecurityGroup sg-dev01-b",
			})
		})
	}
}

// TestDeleteInstance_RecordedIDsAreNotLookedUp: what SSM records is
// deleted as before, and no tag lookup is made for it.
func TestDeleteInstance_RecordedIDsAreNotLookedUp(t *testing.T) {
	t.Parallel()
	svc, ec2f, ssmf := newCreateAccount(&injector{})
	seedForeignResources(ec2f)
	for k, v := range map[string]string{
		ssmInstanceIDPath("dev01"): "i-dev01-a", ssmENIIDPath("dev01"): "eni-dev01-a", ssmSGIDPath("dev01"): "sg-dev01-a",
	} {
		ssmf.params[k] = v
	}
	n := ec2f.lookups
	deleteRemoves(t, svc, ec2f, ssmf, []string{
		"TerminateInstances i-dev01-a", "DeleteNetworkInterface eni-dev01-a", "DeleteSecurityGroup sg-dev01-a",
	})
	if ec2f.lookups != n {
		t.Errorf("delete made %d tag lookups although SSM records every ID", ec2f.lookups-n)
	}
}

// seedForeignResources adds two unrecorded resources of each kind for
// dev01, and resources delete dev01 must never touch.
func seedForeignResources(f *createEC2) {
	tags := func(name, component, nameTag string) map[string]string {
		return tagging.ForInstance(name, component, nameTag)
	}
	unmanaged := func(m map[string]string) map[string]string {
		delete(m, tagging.KeyManaged)
		return m
	}
	for _, n := range []string{"dev01-a", "dev01-b"} {
		f.sgTags["sg-"+n] = tags("dev01", tagging.ComponentSG, naming.SecurityGroup("dev01"))
		f.eniTags["eni-"+n] = tags("dev01", tagging.ComponentENI, naming.ENI("dev01"))
		f.instTags["i-"+n] = tags("dev01", tagging.ComponentEC2, naming.EC2("dev01"))
	}
	f.state["i-dev01-a"] = ec2types.InstanceStateNameRunning
	f.state["i-dev01-b"] = ec2types.InstanceStateNameStopped
	for _, other := range []string{"dev010", "dev0"} {
		f.sgTags["sg-"+other] = tags(other, tagging.ComponentSG, naming.SecurityGroup(other))
		f.eniTags["eni-"+other] = tags(other, tagging.ComponentENI, naming.ENI(other))
		f.instTags["i-"+other] = tags(other, tagging.ComponentEC2, naming.EC2(other))
		f.state["i-"+other] = ec2types.InstanceStateNameRunning
	}
	f.sgTags["sg-unmanaged"] = unmanaged(tags("dev01", tagging.ComponentSG, naming.SecurityGroup("dev01")))
	f.eniTags["eni-unmanaged"] = unmanaged(tags("dev01", tagging.ComponentENI, naming.ENI("dev01")))
	f.instTags["i-unmanaged"] = unmanaged(tags("dev01", tagging.ComponentEC2, naming.EC2("dev01")))
	f.state["i-unmanaged"] = ec2types.InstanceStateNameRunning
	f.instTags["i-dev01-terminated"] = tags("dev01", tagging.ComponentEC2, naming.EC2("dev01"))
	f.state["i-dev01-terminated"] = ec2types.InstanceStateNameTerminated
	f.instTags["i-dev01-shutting-down"] = tags("dev01", tagging.ComponentEC2, naming.EC2("dev01"))
	f.state["i-dev01-shutting-down"] = ec2types.InstanceStateNameShuttingDown
}

// TestDeleteInstance_TagLookupErrors: a lookup the IAM policy denies
// only warns, and delete removes what SSM records; any other lookup
// error stops delete before it removes anything.
func TestDeleteInstance_TagLookupErrors(t *testing.T) {
	t.Parallel()
	ids := map[string]struct{ path, id, deleted string }{
		"instance": {ssmInstanceIDPath("dev01"), "i-dev01-a", "TerminateInstances i-dev01-a"},
		"eni":      {ssmENIIDPath("dev01"), "eni-dev01-a", "DeleteNetworkInterface eni-dev01-a"},
		"sg":       {ssmSGIDPath("dev01"), "sg-dev01-a", "DeleteSecurityGroup sg-dev01-a"},
	}
	// account has the seeded resources, with every ID but that of
	// unrecorded in SSM.
	account := func(unrecorded string) (*Service, *createEC2, createSSM, []string) {
		svc, ec2f, ssmf := newCreateAccount(&injector{})
		seedForeignResources(ec2f)
		var deleted []string
		for _, kind := range []string{"instance", "eni", "sg"} {
			if kind != unrecorded {
				ssmf.params[ids[kind].path] = ids[kind].id
				deleted = append(deleted, ids[kind].deleted)
			}
		}
		return svc, ec2f, ssmf, deleted
	}
	for _, lookup := range []string{"instance", "eni", "sg"} {
		t.Run(lookup+"/denied", func(t *testing.T) {
			t.Parallel()
			svc, ec2f, ssmf, deleted := account(lookup)
			ec2f.fail = map[string]string{lookup: "UnauthorizedOperation"}
			deleteRemoves(t, svc, ec2f, ssmf, deleted)
			if ec2f.lookups != 1 {
				t.Errorf("delete made %d tag lookups, want the denied one", ec2f.lookups)
			}
		})
	}
	t.Run("eni/throttled", func(t *testing.T) {
		t.Parallel()
		svc, ec2f, ssmf, _ := account("eni")
		ec2f.fail = map[string]string{"eni": "RequestLimitExceeded"}
		n := len(ec2f.log)
		err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01", Force: true})
		if err == nil || !strings.Contains(err.Error(), "RequestLimitExceeded") {
			t.Fatalf("DeleteInstance error = %v, want the failed ENI lookup", err)
		}
		if got := deletions(ec2f.log, n); len(got) > 0 {
			t.Errorf("delete removed %v before stopping on the lookup error", got)
		}
		if ssmf.param(ssmInstanceIDPath("dev01")) == "" {
			t.Error("delete dropped the records although it stopped")
		}
	})
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
