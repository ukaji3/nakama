package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	smithy "github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

const (
	testCSDistID   = "ECODESERVER"
	testCrewDistID = "ECREW"
	testInstanceID = "i-0123456789abcdef0"
)

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

// fakeKirocrewCF is an in-memory CloudFront holding distributions and
// one function, recording every mutating call in order.
type fakeKirocrewCF struct {
	awsx.CloudFrontAPI

	mu       sync.Mutex
	calls    []string
	dists    map[string]*cftypes.DistributionConfig
	domains  map[string]string
	function *cftypes.FunctionSummary
	fnCode   string
	// fnInUse makes the next N DeleteFunction calls fail with
	// FunctionInUse (CloudFront releases associations asynchronously).
	fnInUse int
}

func newFakeKirocrewCF() *fakeKirocrewCF {
	cs := distributionConfig(distributionParams{
		callerReference: "ref", comment: "nakama-dev01",
		vpcOriginID: testVPCOriginID, originDomain: testOriginDNS, wafWebACLARN: testWAFARN,
	})
	return &fakeKirocrewCF{
		dists:   map[string]*cftypes.DistributionConfig{testCSDistID: cs},
		domains: map[string]string{testCSDistID: "dcodeserver123.cloudfront.net"},
	}
}

func (f *fakeKirocrewCF) record(c string) { f.calls = append(f.calls, c) }

func (f *fakeKirocrewCF) GetDistributionConfig(_ context.Context, in *cloudfront.GetDistributionConfigInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg, ok := f.dists[aws.ToString(in.Id)]
	if !ok {
		return nil, apiErr("NoSuchDistribution")
	}
	cp := *cfg
	return &cloudfront.GetDistributionConfigOutput{DistributionConfig: &cp, ETag: aws.String("etag-" + aws.ToString(in.Id))}, nil
}

func (f *fakeKirocrewCF) GetDistribution(_ context.Context, in *cloudfront.GetDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.Id)
	if _, ok := f.dists[id]; !ok {
		return nil, apiErr("NoSuchDistribution")
	}
	return &cloudfront.GetDistributionOutput{Distribution: &cftypes.Distribution{
		Id: aws.String(id), DomainName: aws.String(f.domains[id]), Status: aws.String("Deployed"),
	}}, nil
}

func (f *fakeKirocrewCF) CreateDistribution(_ context.Context, in *cloudfront.CreateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateDistribution")
	f.dists[testCrewDistID] = in.DistributionConfig
	f.domains[testCrewDistID] = testCrewDomain
	return &cloudfront.CreateDistributionOutput{Distribution: &cftypes.Distribution{
		Id: aws.String(testCrewDistID), DomainName: aws.String(testCrewDomain),
		ARN: aws.String("arn:aws:cloudfront::111122223333:distribution/" + testCrewDistID),
	}}, nil
}

func (f *fakeKirocrewCF) UpdateDistribution(_ context.Context, in *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.Id)
	if aws.ToString(in.IfMatch) != "etag-"+id {
		return nil, apiErr("PreconditionFailed")
	}
	f.record("UpdateDistribution:" + id)
	f.dists[id] = in.DistributionConfig
	return &cloudfront.UpdateDistributionOutput{}, nil
}

func (f *fakeKirocrewCF) DeleteDistribution(_ context.Context, in *cloudfront.DeleteDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.Id)
	f.record("DeleteDistribution:" + id)
	delete(f.dists, id)
	return &cloudfront.DeleteDistributionOutput{}, nil
}

func (f *fakeKirocrewCF) TagResource(context.Context, *cloudfront.TagResourceInput, ...func(*cloudfront.Options)) (*cloudfront.TagResourceOutput, error) {
	return &cloudfront.TagResourceOutput{}, nil
}

func (f *fakeKirocrewCF) DescribeFunction(_ context.Context, _ *cloudfront.DescribeFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DescribeFunctionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.function == nil {
		return nil, apiErr("NoSuchFunctionExists")
	}
	return &cloudfront.DescribeFunctionOutput{FunctionSummary: f.function, ETag: aws.String("fn-etag")}, nil
}

func (f *fakeKirocrewCF) CreateFunction(_ context.Context, in *cloudfront.CreateFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateFunctionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateFunction")
	f.fnCode = string(in.FunctionCode)
	f.function = &cftypes.FunctionSummary{Name: in.Name, FunctionMetadata: &cftypes.FunctionMetadata{
		FunctionARN: aws.String("arn:aws:cloudfront::111122223333:function/" + aws.ToString(in.Name)),
	}}
	return &cloudfront.CreateFunctionOutput{ETag: aws.String("fn-etag"), FunctionSummary: f.function}, nil
}

func (f *fakeKirocrewCF) UpdateFunction(_ context.Context, in *cloudfront.UpdateFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateFunctionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdateFunction")
	f.fnCode = string(in.FunctionCode)
	return &cloudfront.UpdateFunctionOutput{ETag: aws.String("fn-etag"), FunctionSummary: f.function}, nil
}

func (f *fakeKirocrewCF) PublishFunction(_ context.Context, in *cloudfront.PublishFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.PublishFunctionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if aws.ToString(in.IfMatch) != "fn-etag" {
		return nil, apiErr("PreconditionFailed")
	}
	f.record("PublishFunction")
	return &cloudfront.PublishFunctionOutput{FunctionSummary: f.function}, nil
}

func (f *fakeKirocrewCF) DeleteFunction(context.Context, *cloudfront.DeleteFunctionInput, ...func(*cloudfront.Options)) (*cloudfront.DeleteFunctionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fnInUse > 0 {
		f.fnInUse--
		f.record("DeleteFunction:InUse")
		return nil, apiErr("FunctionInUse")
	}
	f.record("DeleteFunction")
	f.function = nil
	return &cloudfront.DeleteFunctionOutput{}, nil
}

func (f *fakeKirocrewCF) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeKirocrewSSM is a parameter store plus Run Command.
type fakeKirocrewSSM struct {
	*fakeRunCommandSSM

	pmu     sync.Mutex
	params  map[string]string
	deleted []string
}

func newFakeKirocrewSSM(params map[string]string) *fakeKirocrewSSM {
	return &fakeKirocrewSSM{
		fakeRunCommandSSM: &fakeRunCommandSSM{stdout: kirocrewMarkerOK + "\n"},
		params:            params,
	}
}

func (f *fakeKirocrewSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	v, ok := f.params[aws.ToString(in.Name)]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

func (f *fakeKirocrewSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.params[aws.ToString(in.Name)] = aws.ToString(in.Value)
	return &ssm.PutParameterOutput{}, nil
}

func (f *fakeKirocrewSSM) DeleteParameters(_ context.Context, in *ssm.DeleteParametersInput, _ ...func(*ssm.Options)) (*ssm.DeleteParametersOutput, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	for _, n := range in.Names {
		delete(f.params, n)
		f.deleted = append(f.deleted, n)
	}
	return &ssm.DeleteParametersOutput{}, nil
}

func (f *fakeKirocrewSSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	var out []ssmtypes.Parameter
	for k := range f.params {
		if strings.HasPrefix(k, aws.ToString(in.Path)) {
			out = append(out, ssmtypes.Parameter{Name: aws.String(k)})
		}
	}
	return &ssm.GetParametersByPathOutput{Parameters: out}, nil
}

func (f *fakeKirocrewSSM) param(name string) string {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return f.params[name]
}

// fakeKirocrewEC2 reports one instance in a fixed state and no volumes.
type fakeKirocrewEC2 struct {
	awsx.EC2API
	state ec2types.InstanceStateName
}

func (f *fakeKirocrewEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(in.InstanceIds) == 0 {
		return &ec2.DescribeInstancesOutput{}, nil // filter queries (NAT reconcile): nothing
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId: aws.String(in.InstanceIds[0]), State: &ec2types.InstanceState{Name: f.state},
		InstanceType: ec2types.InstanceTypeT3Medium,
	}}}}}, nil
}

func (f *fakeKirocrewEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{}, nil
}

func kirocrewTestParams() map[string]string {
	return map[string]string{
		ssmInstanceIDPath("dev01"):         testInstanceID,
		ssmDistributionIDPath("dev01"):     testCSDistID,
		ssmDistributionDomainPath("dev01"): "dcodeserver123.cloudfront.net",
		ssmVPCOriginIDPath("dev01"):        testVPCOriginID,
	}
}

func newKirocrewTestService(state ec2types.InstanceStateName, params map[string]string) (*Service, *fakeKirocrewCF, *fakeKirocrewSSM) {
	cf := newFakeKirocrewCF()
	sm := newFakeKirocrewSSM(params)
	svc := NewService(&awsx.Clients{CloudFront: cf, SSM: sm, EC2: &fakeKirocrewEC2{state: state}, Region: "ap-northeast-1"})
	return svc, cf, sm
}

func TestEnableKiroCrewWebBuildsEverythingInOrder(t *testing.T) {
	svc, cf, sm := newKirocrewTestService(ec2types.InstanceStateNameRunning, kirocrewTestParams())
	out, err := svc.EnableKiroCrewWeb(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("EnableKiroCrewWeb: %v", err)
	}
	if out.URL != "https://"+testCrewDomain || out.DistributionID != testCrewDistID {
		t.Errorf("output = %+v", out)
	}
	want := []string{"CreateDistribution", "CreateFunction", "PublishFunction", "UpdateDistribution:" + testCSDistID}
	if got := cf.callLog(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("CloudFront calls = %v, want %v", got, want)
	}
	// The ID is persisted, so a later failure can resume.
	if got := sm.param(ssmKirocrewDistributionIDPath("dev01")); got != testCrewDistID {
		t.Errorf("crew distribution id not recorded: %q", got)
	}
	if got := sm.param(ssmKirocrewDistributionDomainPath("dev01")); got != testCrewDomain {
		t.Errorf("crew distribution domain not recorded: %q", got)
	}
	// The instance-side script targets the new distribution.
	// The domain reaches both the nginx Origin allowlist and the
	// code-server trusted-origins editor (passed as its argument).
	if script := sm.lastScript(); !strings.Contains(script, `"https://`+testCrewDomain+`"`) || !strings.Contains(script, " set "+testCrewDomain+" ") {
		t.Errorf("the enable script must carry the crew domain:\n%.300s", script)
	}
	// The crew distribution reuses the code-server origin and WAF.
	crew := cf.dists[testCrewDistID]
	if aws.ToString(crew.Origins.Items[0].VpcOriginConfig.VpcOriginId) != testVPCOriginID || aws.ToString(crew.WebACLId) != testWAFARN {
		t.Errorf("the crew distribution must reuse the VPC origin and WAF: %+v", crew)
	}
	if !hasKirocrewBehaviors(cf.dists[testCSDistID]) {
		t.Error("the code-server distribution must redirect /proxy/5476*")
	}
	if !strings.Contains(cf.fnCode, testCrewDomain) {
		t.Error("the redirect function must point at the crew distribution")
	}
}

func TestEnableKiroCrewWebIsIdempotent(t *testing.T) {
	svc, cf, _ := newKirocrewTestService(ec2types.InstanceStateNameRunning, kirocrewTestParams())
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err != nil {
		t.Fatal(err)
	}
	n := len(cf.callLog())
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err != nil {
		t.Fatalf("second enable: %v", err)
	}
	second := cf.callLog()[n:]
	for _, c := range second {
		if c == "CreateDistribution" || c == "CreateFunction" || strings.HasPrefix(c, "UpdateDistribution") {
			t.Errorf("a second enable must not create or redeploy anything, called %s (all: %v)", c, second)
		}
	}
	// The function is refreshed and republished (cheap, keeps code current).
	if strings.Join(second, ",") != "UpdateFunction,PublishFunction" {
		t.Errorf("second enable calls = %v", second)
	}
}

func TestEnableKiroCrewWebResumesAfterRemoteFailure(t *testing.T) {
	svc, cf, sm := newKirocrewTestService(ec2types.InstanceStateNameRunning, kirocrewTestParams())
	sm.status = ssmtypes.CommandInvocationStatusFailed
	sm.stdout = kirocrewMarkerError + " the Kiro Crew gateway is not answering on 127.0.0.1:5476\n"
	_, err := svc.EnableKiroCrewWeb(context.Background(), "dev01")
	if err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Fatalf("the remote reason must surface, got %v", err)
	}
	if got := sm.param(ssmKirocrewDistributionIDPath("dev01")); got != testCrewDistID {
		t.Fatal("the created distribution must stay recorded after a later failure")
	}
	// The redirect must not be installed when the instance side failed.
	if hasKirocrewBehaviors(cf.dists[testCSDistID]) {
		t.Error("the code-server distribution must not redirect to a broken setup")
	}
	// Retry once the gateway is up: no second distribution.
	sm.status, sm.stdout = "", kirocrewMarkerOK+"\n"
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	creates := 0
	for _, c := range cf.callLog() {
		if c == "CreateDistribution" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("CreateDistribution called %d times, want 1", creates)
	}
}

func TestEnableKiroCrewWebPreconditions(t *testing.T) {
	bare := kirocrewTestParams()
	bare[ssmKindPath("dev01")] = instanceKindBare
	svc, cf, _ := newKirocrewTestService(ec2types.InstanceStateNameRunning, bare)
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err == nil || !strings.Contains(err.Error(), "bare") {
		t.Errorf("a bare instance must be refused with an explanation, got %v", err)
	}
	if len(cf.callLog()) != 0 {
		t.Error("nothing may be created for a bare instance")
	}

	svc, cf, _ = newKirocrewTestService(ec2types.InstanceStateNameStopped, kirocrewTestParams())
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err == nil || !strings.Contains(err.Error(), "nakama aws start dev01") {
		t.Errorf("a stopped instance must be refused with the start hint, got %v", err)
	}
	if len(cf.callLog()) != 0 {
		t.Error("nothing may be created for a stopped instance")
	}

	svc, _, _ = newKirocrewTestService(ec2types.InstanceStateNameRunning, map[string]string{})
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("an unknown instance must be refused, got %v", err)
	}
}

func TestDisableKiroCrewWebRemovesEverythingInOrder(t *testing.T) {
	svc, cf, sm := newKirocrewTestService(ec2types.InstanceStateNameRunning, kirocrewTestParams())
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err != nil {
		t.Fatal(err)
	}
	cf.fnInUse = 1 // CloudFront still holds the association once
	prev := kirocrewFunctionRetryInterval
	kirocrewFunctionRetryInterval = time.Millisecond
	t.Cleanup(func() { kirocrewFunctionRetryInterval = prev })
	n := len(cf.callLog())
	out, err := svc.DisableKiroCrewWeb(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("DisableKiroCrewWeb: %v", err)
	}
	if out.InstanceSkipped {
		t.Error("a running instance must be cleaned up")
	}
	got := strings.Join(cf.callLog()[n:], ",")
	want := strings.Join([]string{
		"UpdateDistribution:" + testCSDistID, // stop redirecting first
		"DeleteFunction:InUse", "DeleteFunction",
		"UpdateDistribution:" + testCrewDistID, // disable, then delete
		"DeleteDistribution:" + testCrewDistID,
	}, ",")
	if got != want {
		t.Errorf("disable calls = %s\nwant          %s", got, want)
	}
	if hasKirocrewBehaviors(cf.dists[testCSDistID]) {
		t.Error("the redirect behaviors must be gone")
	}
	if !strings.Contains(sm.lastScript(), "rm -f "+kirocrewNginxConfPath) {
		t.Error("the instance-side cleanup script must run")
	}
	if sm.param(ssmKirocrewDistributionIDPath("dev01")) != "" || sm.param(ssmKirocrewDistributionDomainPath("dev01")) != "" {
		t.Error("the kirocrew metadata must be removed last")
	}
	// Idempotent: a second disable finds nothing to do and succeeds.
	if _, err := svc.DisableKiroCrewWeb(context.Background(), "dev01"); err != nil {
		t.Errorf("second disable: %v", err)
	}
}

func TestDisableKiroCrewWebSkipsAStoppedInstance(t *testing.T) {
	params := kirocrewTestParams()
	svc, _, sm := newKirocrewTestService(ec2types.InstanceStateNameStopped, params)
	before := len(sm.scripts)
	out, err := svc.DisableKiroCrewWeb(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !out.InstanceSkipped || len(sm.scripts) != before {
		t.Errorf("a stopped instance must be skipped, not woken: %+v scripts=%d", out, len(sm.scripts)-before)
	}
}

func TestGetKiroCrewWeb(t *testing.T) {
	svc, _, _ := newKirocrewTestService(ec2types.InstanceStateNameRunning, kirocrewTestParams())
	if _, err := svc.GetKiroCrewWeb(context.Background(), "dev01"); !errors.Is(err, ErrKiroCrewWebNotEnabled) {
		t.Errorf("before enable: err = %v, want ErrKiroCrewWebNotEnabled", err)
	}
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetKiroCrewWeb(context.Background(), "dev01")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://"+testCrewDomain || !got.Deployed || got.State != "running" || got.InstanceID != testInstanceID {
		t.Errorf("GetKiroCrewWeb = %+v", got)
	}
}

// fakeDeleteVPCOriginCF adds the VPC origin calls DeleteInstance makes.
type fakeDeleteVPCOriginCF struct {
	*fakeKirocrewCF
	vpcGone bool
}

func (f *fakeDeleteVPCOriginCF) GetVpcOrigin(context.Context, *cloudfront.GetVpcOriginInput, ...func(*cloudfront.Options)) (*cloudfront.GetVpcOriginOutput, error) {
	if f.vpcGone {
		return nil, apiErr("EntityNotFound")
	}
	return &cloudfront.GetVpcOriginOutput{ETag: aws.String("vo-etag")}, nil
}

func (f *fakeDeleteVPCOriginCF) DeleteVpcOrigin(context.Context, *cloudfront.DeleteVpcOriginInput, ...func(*cloudfront.Options)) (*cloudfront.DeleteVpcOriginOutput, error) {
	f.mu.Lock()
	f.record("DeleteVpcOrigin")
	f.mu.Unlock()
	f.vpcGone = true
	return &cloudfront.DeleteVpcOriginOutput{}, nil
}

// TestDeleteInstanceCleansUpKiroCrewFirst pins the order delete needs
// when Kiro Crew web access was enabled: the crew distribution before
// the code-server one (both reference the VPC origin), the redirect
// function after the code-server distribution (which references it),
// and the VPC origin last.
func TestDeleteInstanceCleansUpKiroCrewFirst(t *testing.T) {
	svc, cf, sm := newKirocrewTestService(ec2types.InstanceStateNameRunning, kirocrewTestParams())
	if _, err := svc.EnableKiroCrewWeb(context.Background(), "dev01"); err != nil {
		t.Fatal(err)
	}
	// Only the CloudFront part of delete is under test: drop the EC2
	// resource IDs so terminate/ENI/SG are skipped.
	sm.pmu.Lock()
	delete(sm.params, ssmInstanceIDPath("dev01"))
	sm.pmu.Unlock()
	full := &fakeDeleteVPCOriginCF{fakeKirocrewCF: cf}
	svc.clients.CloudFront = full
	n := len(cf.callLog())

	if err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01"}); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	var order []string
	for _, c := range cf.callLog()[n:] {
		if strings.HasPrefix(c, "Delete") {
			order = append(order, c)
		}
	}
	want := []string{"DeleteDistribution:" + testCrewDistID, "DeleteDistribution:" + testCSDistID, "DeleteFunction", "DeleteVpcOrigin"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("delete order = %v, want %v", order, want)
	}
	if sm.param(ssmKirocrewDistributionIDPath("dev01")) != "" {
		t.Error("the kirocrew metadata must be swept with the rest of /nakama/<name>/")
	}
}

// TestDeleteInstanceWithoutKiroCrewTouchesNoFunction pins that a plain
// delete makes no CloudFront Functions calls: users who never enabled
// Kiro Crew web access must not need those permissions.
func TestDeleteInstanceWithoutKiroCrewTouchesNoFunction(t *testing.T) {
	params := kirocrewTestParams()
	delete(params, ssmInstanceIDPath("dev01"))
	svc, cf, _ := newKirocrewTestService(ec2types.InstanceStateNameRunning, params)
	svc.clients.CloudFront = &fakeDeleteVPCOriginCF{fakeKirocrewCF: cf}
	if err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01"}); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	for _, c := range cf.callLog() {
		if strings.Contains(c, "Function") {
			t.Errorf("a delete without Kiro Crew web access called %s", c)
		}
	}
}
