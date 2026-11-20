package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/naming"
)

// --- fakes ----------------------------------------------------------------

// The stages of the cloudfront commands at which a test makes one call
// fail (cfAccount.inj).
const (
	cfStageAuthorize    = "cf-authorize"     // AuthorizeSecurityGroupIngress
	cfStageCreateOrigin = "cf-create-origin" // CreateVpcOrigin
	cfStageOriginWait   = "cf-origin-wait"   // GetVpcOrigin while waiting for Deployed
	cfStageCreateDist   = "cf-create-dist"   // CreateDistribution
	cfStageDistWait     = "cf-dist-wait"     // GetDistribution while waiting for Deployed
	cfStageRevoke       = "cf-revoke"        // RevokeSecurityGroupIngress
	cfStageDeleteDist   = "cf-delete-dist"   // DeleteDistribution
	cfStageDeleteOrigin = "cf-delete-origin" // DeleteVpcOrigin
)

const (
	cfTestInstance = "i-0cf0000000000000a"
	cfTestSG       = "sg-cf"
	cfTestPrivDNS  = "ip-10-0-1-10.ap-northeast-1.compute.internal"
)

// cfAccount is a small model of the AWS side of one instance's
// CloudFront entry path: the instance, its security group rule, VPC
// origins, distributions and the Kiro Crew redirect function. Every
// mutating call is appended to log, so tests can check the order.
type cfAccount struct {
	mu  sync.Mutex
	log []string
	inj *injector

	state   ec2types.InstanceStateName
	ingress bool // the CloudFront rule is on cfTestSG
	// revokeNotFound makes an absent rule fail with
	// InvalidPermission.NotFound instead of being listed in
	// UnknownIpPermissions (the two answers AWS gives).
	revokeNotFound bool

	origins  map[string]string // id -> status
	dists    map[string]*cfFakeDist
	function bool
	nOrigin  int
	nDist    int

	// what the last create calls asked for
	originARN     string
	distDomain    string // origin domain of the last distribution
	distWebACL    string
	distVPCOrigin string // VpcOriginId of the live distributions
}

type cfFakeDist struct {
	domain  string
	enabled bool
	// disabling is a disable that has not deployed yet: CloudFront
	// refuses to delete the distribution until a status read shows it
	// Deployed (deploys finish at the first read here).
	disabling bool
}

func newCFAccount(state ec2types.InstanceStateName) *cfAccount {
	return &cfAccount{
		inj: &injector{}, state: state,
		origins: map[string]string{}, dists: map[string]*cfFakeDist{},
	}
}

func (a *cfAccount) record(format string, args ...any) {
	a.log = append(a.log, fmt.Sprintf(format, args...))
}

func (a *cfAccount) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.log...)
}

// cfEC2 is the EC2 side of cfAccount.
type cfEC2 struct {
	awsx.EC2API
	a *cfAccount
}

func (f cfEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	if len(in.InstanceIds) == 0 {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId: aws.String(in.InstanceIds[0]), State: &ec2types.InstanceState{Name: f.a.state},
		InstanceType: ec2types.InstanceTypeT4gMedium, PrivateDnsName: aws.String(cfTestPrivDNS),
	}}}}}, nil
}

func (f cfEC2) DescribeManagedPrefixLists(context.Context, *ec2.DescribeManagedPrefixListsInput, ...func(*ec2.Options)) (*ec2.DescribeManagedPrefixListsOutput, error) {
	return &ec2.DescribeManagedPrefixListsOutput{PrefixLists: []ec2types.ManagedPrefixList{{PrefixListId: aws.String("pl-cf")}}}, nil
}

// cfRuleOK checks that the rule is exactly nakama's: tcp 8080 from the
// CloudFront prefix list, on the instance's group.
func cfRuleOK(sg *string, perms []ec2types.IpPermission) error {
	if aws.ToString(sg) != cfTestSG || len(perms) != 1 || aws.ToString(perms[0].IpProtocol) != "tcp" ||
		aws.ToInt32(perms[0].FromPort) != 8080 || aws.ToInt32(perms[0].ToPort) != 8080 ||
		len(perms[0].PrefixListIds) != 1 || aws.ToString(perms[0].PrefixListIds[0].PrefixListId) != "pl-cf" ||
		len(perms[0].IpRanges) != 0 {
		return fmt.Errorf("unexpected rule on %s: %+v", aws.ToString(sg), perms)
	}
	return nil
}

func (f cfEC2) AuthorizeSecurityGroupIngress(ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	if err := f.a.inj.hit(ctx, cfStageAuthorize); err != nil {
		return nil, err
	}
	if err := cfRuleOK(in.GroupId, in.IpPermissions); err != nil {
		return nil, err
	}
	if f.a.ingress {
		return nil, apiErr("InvalidPermission.Duplicate")
	}
	f.a.record("AuthorizeIngress")
	f.a.ingress = true
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (f cfEC2) RevokeSecurityGroupIngress(ctx context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	if err := f.a.inj.hit(ctx, cfStageRevoke); err != nil {
		return nil, err
	}
	if err := cfRuleOK(in.GroupId, in.IpPermissions); err != nil {
		return nil, err
	}
	if !f.a.ingress {
		if f.a.revokeNotFound {
			return nil, apiErr("InvalidPermission.NotFound")
		}
		return &ec2.RevokeSecurityGroupIngressOutput{Return: aws.Bool(true), UnknownIpPermissions: in.IpPermissions}, nil
	}
	f.a.record("RevokeIngress")
	f.a.ingress = false
	return &ec2.RevokeSecurityGroupIngressOutput{Return: aws.Bool(true)}, nil
}

// cfCloudFront is the CloudFront side of cfAccount. Deploys finish at
// once.
type cfCloudFront struct {
	awsx.CloudFrontAPI
	a *cfAccount
}

func (f cfCloudFront) CreateVpcOrigin(ctx context.Context, in *cloudfront.CreateVpcOriginInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateVpcOriginOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	if err := f.a.inj.hit(ctx, cfStageCreateOrigin); err != nil {
		return nil, err
	}
	f.a.nOrigin++
	id := fmt.Sprintf("vo-%d", f.a.nOrigin)
	f.a.origins[id] = "Deployed"
	f.a.originARN = aws.ToString(in.VpcOriginEndpointConfig.Arn)
	f.a.record("CreateVpcOrigin %s", id)
	return &cloudfront.CreateVpcOriginOutput{VpcOrigin: &cftypes.VpcOrigin{Id: aws.String(id)}}, nil
}

func (f cfCloudFront) GetVpcOrigin(ctx context.Context, in *cloudfront.GetVpcOriginInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetVpcOriginOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	st, ok := f.a.origins[aws.ToString(in.Id)]
	if !ok {
		return nil, apiErr("EntityNotFound")
	}
	if err := f.a.inj.hit(ctx, cfStageOriginWait); err != nil {
		return nil, err
	}
	return &cloudfront.GetVpcOriginOutput{ETag: aws.String("vo-etag"), VpcOrigin: &cftypes.VpcOrigin{Id: in.Id, Status: aws.String(st)}}, nil
}

func (f cfCloudFront) DeleteVpcOrigin(ctx context.Context, in *cloudfront.DeleteVpcOriginInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteVpcOriginOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	if err := f.a.inj.hit(ctx, cfStageDeleteOrigin); err != nil {
		return nil, err
	}
	id := aws.ToString(in.Id)
	for did, d := range f.a.dists {
		if d != nil && f.a.distVPCOrigin == id {
			return nil, fmt.Errorf("VPC origin %s is still used by distribution %s", id, did)
		}
	}
	f.a.record("DeleteVpcOrigin %s", id)
	delete(f.a.origins, id)
	return &cloudfront.DeleteVpcOriginOutput{}, nil
}

func (f cfCloudFront) CreateDistribution(ctx context.Context, in *cloudfront.CreateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	if err := f.a.inj.hit(ctx, cfStageCreateDist); err != nil {
		return nil, err
	}
	o := in.DistributionConfig.Origins.Items[0]
	if _, ok := f.a.origins[aws.ToString(o.VpcOriginConfig.VpcOriginId)]; !ok {
		return nil, fmt.Errorf("no VPC origin %s", aws.ToString(o.VpcOriginConfig.VpcOriginId))
	}
	f.a.nDist++
	id := fmt.Sprintf("E%d", f.a.nDist)
	domain := fmt.Sprintf("d%dexample.cloudfront.net", f.a.nDist)
	f.a.dists[id] = &cfFakeDist{domain: domain, enabled: true}
	f.a.distVPCOrigin = aws.ToString(o.VpcOriginConfig.VpcOriginId)
	f.a.distDomain = aws.ToString(o.DomainName)
	f.a.distWebACL = aws.ToString(in.DistributionConfig.WebACLId)
	f.a.record("CreateDistribution %s", id)
	return &cloudfront.CreateDistributionOutput{Distribution: &cftypes.Distribution{
		Id: aws.String(id), DomainName: aws.String(domain), ARN: aws.String("arn:aws:cloudfront::111122223333:distribution/" + id),
	}}, nil
}

func (f cfCloudFront) TagResource(context.Context, *cloudfront.TagResourceInput, ...func(*cloudfront.Options)) (*cloudfront.TagResourceOutput, error) {
	return &cloudfront.TagResourceOutput{}, nil
}

func (f cfCloudFront) GetDistribution(ctx context.Context, in *cloudfront.GetDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	d, ok := f.a.dists[aws.ToString(in.Id)]
	if !ok {
		return nil, apiErr("NoSuchDistribution")
	}
	if err := f.a.inj.hit(ctx, cfStageDistWait); err != nil {
		return nil, err
	}
	d.disabling = false // deployed
	return &cloudfront.GetDistributionOutput{Distribution: &cftypes.Distribution{
		Id: in.Id, DomainName: aws.String(d.domain), Status: aws.String("Deployed"),
		DistributionConfig: &cftypes.DistributionConfig{Enabled: aws.Bool(d.enabled)},
	}}, nil
}

func (f cfCloudFront) GetDistributionConfig(_ context.Context, in *cloudfront.GetDistributionConfigInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	d, ok := f.a.dists[aws.ToString(in.Id)]
	if !ok {
		return nil, apiErr("NoSuchDistribution")
	}
	return &cloudfront.GetDistributionConfigOutput{
		ETag: aws.String("etag-" + aws.ToString(in.Id)), DistributionConfig: &cftypes.DistributionConfig{Enabled: aws.Bool(d.enabled)},
	}, nil
}

func (f cfCloudFront) UpdateDistribution(_ context.Context, in *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	id := aws.ToString(in.Id)
	d, ok := f.a.dists[id]
	if !ok {
		return nil, apiErr("NoSuchDistribution")
	}
	d.enabled = aws.ToBool(in.DistributionConfig.Enabled)
	if !d.enabled {
		d.disabling = true
		f.a.record("DisableDistribution %s", id)
	}
	return &cloudfront.UpdateDistributionOutput{}, nil
}

func (f cfCloudFront) DeleteDistribution(ctx context.Context, in *cloudfront.DeleteDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	if err := f.a.inj.hit(ctx, cfStageDeleteDist); err != nil {
		return nil, err
	}
	id := aws.ToString(in.Id)
	d, ok := f.a.dists[id]
	if !ok {
		return nil, apiErr("NoSuchDistribution")
	}
	if d.enabled || d.disabling {
		return nil, apiErr("DistributionNotDisabled")
	}
	f.a.record("DeleteDistribution %s", id)
	delete(f.a.dists, id)
	if len(f.a.dists) == 0 {
		f.a.distVPCOrigin = ""
	}
	return &cloudfront.DeleteDistributionOutput{}, nil
}

func (f cfCloudFront) DescribeFunction(_ context.Context, _ *cloudfront.DescribeFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DescribeFunctionOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	f.a.record("DescribeFunction")
	if !f.a.function {
		return nil, apiErr("NoSuchFunctionExists")
	}
	return &cloudfront.DescribeFunctionOutput{ETag: aws.String("fn-etag")}, nil
}

func (f cfCloudFront) DeleteFunction(context.Context, *cloudfront.DeleteFunctionInput, ...func(*cloudfront.Options)) (*cloudfront.DeleteFunctionOutput, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	f.a.record("DeleteFunction")
	f.a.function = false
	return &cloudfront.DeleteFunctionOutput{}, nil
}

// cfService wires a cfAccount into a Service. params are the SSM
// parameters on top of the init marker and the instance's own records.
func cfService(a *cfAccount, params map[string]string) (*Service, *fakeKirocrewSSM) {
	p := map[string]string{
		naming.SSMMetaInitialized(): "true",
		ssmInstanceIDPath("dev01"):  cfTestInstance,
		ssmSGIDPath("dev01"):        cfTestSG,
		ssmENIIDPath("dev01"):       "eni-cf",
	}
	for k, v := range params {
		if v == "" {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	sm := newFakeKirocrewSSM(p)
	return NewService(&awsx.Clients{
		EC2: cfEC2{a: a}, CloudFront: cfCloudFront{a: a}, SSM: sm, WAF: fakeRestoreWAF{},
		AccountID: "111122223333", Region: "ap-northeast-1",
	}), sm
}

// withCloudFront gives the account an instance whose entry path is
// complete, as `cloudfront add` leaves it, plus Kiro Crew web access
// when crew is set. It returns the SSM parameters recording it.
func (a *cfAccount) withCloudFront(crew bool) map[string]string {
	a.ingress = true
	a.origins["vo-old"] = "Deployed"
	a.dists["EOLD"] = &cfFakeDist{domain: "dold.cloudfront.net", enabled: true}
	a.distVPCOrigin = "vo-old"
	params := map[string]string{
		ssmVPCOriginIDPath("dev01"):        "vo-old",
		ssmDistributionIDPath("dev01"):     "EOLD",
		ssmDistributionDomainPath("dev01"): "dold.cloudfront.net",
	}
	if crew {
		a.dists["ECREW"] = &cfFakeDist{domain: testCrewDomain, enabled: true}
		a.function = true
		params[ssmKirocrewDistributionIDPath("dev01")] = "ECREW"
		params[ssmKirocrewDistributionDomainPath("dev01")] = testCrewDomain
	}
	return params
}

// mutating returns the calls in log that change something.
func mutating(log []string) []string {
	var out []string
	for _, l := range log {
		if l != "DescribeFunction" {
			out = append(out, l)
		}
	}
	return out
}

// --- create without CloudFront ---------------------------------------------

// spyEC2 counts the CloudFront-related EC2 calls and passes every call
// on to the wrapped fake.
type spyEC2 struct {
	awsx.EC2API
	mu                        sync.Mutex
	authorized, prefixLookups int
}

func (f *spyEC2) AuthorizeSecurityGroupIngress(ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, o ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.mu.Lock()
	f.authorized++
	f.mu.Unlock()
	return f.EC2API.AuthorizeSecurityGroupIngress(ctx, in, o...)
}

func (f *spyEC2) DescribeManagedPrefixLists(ctx context.Context, in *ec2.DescribeManagedPrefixListsInput, o ...func(*ec2.Options)) (*ec2.DescribeManagedPrefixListsOutput, error) {
	f.mu.Lock()
	f.prefixLookups++
	f.mu.Unlock()
	return f.EC2API.DescribeManagedPrefixLists(ctx, in, o...)
}

// spyCF counts the CloudFront calls create and restore can make.
type spyCF struct {
	awsx.CloudFrontAPI
	mu    sync.Mutex
	calls int
}

func (f *spyCF) count() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
}

func (f *spyCF) CreateVpcOrigin(ctx context.Context, in *cloudfront.CreateVpcOriginInput, o ...func(*cloudfront.Options)) (*cloudfront.CreateVpcOriginOutput, error) {
	f.count()
	return f.CloudFrontAPI.CreateVpcOrigin(ctx, in, o...)
}

func (f *spyCF) GetVpcOrigin(ctx context.Context, in *cloudfront.GetVpcOriginInput, o ...func(*cloudfront.Options)) (*cloudfront.GetVpcOriginOutput, error) {
	f.count()
	return f.CloudFrontAPI.GetVpcOrigin(ctx, in, o...)
}

func (f *spyCF) CreateDistribution(ctx context.Context, in *cloudfront.CreateDistributionInput, o ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error) {
	f.count()
	return f.CloudFrontAPI.CreateDistribution(ctx, in, o...)
}

func (f *spyCF) GetDistribution(ctx context.Context, in *cloudfront.GetDistributionInput, o ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	f.count()
	return f.CloudFrontAPI.GetDistribution(ctx, in, o...)
}

func (f *spyCF) TagResource(ctx context.Context, in *cloudfront.TagResourceInput, o ...func(*cloudfront.Options)) (*cloudfront.TagResourceOutput, error) {
	f.count()
	return f.CloudFrontAPI.TagResource(ctx, in, o...)
}

// spyWAF counts the web ACL lookups.
type spyWAF struct {
	awsx.WAFAPI
	mu    sync.Mutex
	calls int
}

func (f *spyWAF) ListWebACLs(ctx context.Context, in *wafv2.ListWebACLsInput, o ...func(*wafv2.Options)) (*wafv2.ListWebACLsOutput, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.WAFAPI.ListWebACLs(ctx, in, o...)
}

// TestCreateInstance_CloudFrontIsOptIn pins ADR 0028: without
// --cloudfront create makes no CloudFront call, looks up neither the WAF
// web ACL nor the prefix list, and leaves the security group without an
// inbound rule. With it, it does all of that, and --bare excludes it.
func TestCreateInstance_CloudFrontIsOptIn(t *testing.T) {
	t.Parallel()
	for _, withCF := range []bool{false, true} {
		t.Run(fmt.Sprintf("cloudfront=%v", withCF), func(t *testing.T) {
			t.Parallel()
			svc, ec2f, _ := newCreateAccount(&injector{})
			spyE := &spyEC2{EC2API: ec2f}
			spyC := &spyCF{CloudFrontAPI: svc.clients.CloudFront}
			spyW := &spyWAF{WAFAPI: svc.clients.WAF}
			svc.clients.EC2, svc.clients.CloudFront, svc.clients.WAF = spyE, spyC, spyW

			out, err := svc.CreateInstance(context.Background(), CreateInstanceInput{Name: "dev01", InstanceType: "t4g.medium", CloudFront: withCF})
			if err != nil {
				t.Fatalf("CreateInstance: %v", err)
			}
			if !withCF {
				if spyE.authorized != 0 || spyE.prefixLookups != 0 || spyC.calls != 0 || spyW.calls != 0 {
					t.Errorf("SG rules %d, prefix list lookups %d, CloudFront calls %d, WAF calls %d; want none",
						spyE.authorized, spyE.prefixLookups, spyC.calls, spyW.calls)
				}
				if out.URL != "" || out.CloudFront {
					t.Errorf("output = %+v, want no URL", out)
				}
				return
			}
			if spyE.authorized != 1 || spyE.prefixLookups != 1 || spyC.calls == 0 || spyW.calls != 1 {
				t.Errorf("SG rules %d, prefix list lookups %d, CloudFront calls %d, WAF calls %d; want the entry path",
					spyE.authorized, spyE.prefixLookups, spyC.calls, spyW.calls)
			}
			if !out.CloudFront || out.URL == "" {
				t.Errorf("output = %+v, want the CloudFront URL", out)
			}
		})
	}
	t.Run("bare+cloudfront", func(t *testing.T) {
		t.Parallel()
		svc, ec2f, _ := newCreateAccount(&injector{})
		_, err := svc.CreateInstance(context.Background(), CreateInstanceInput{Name: "dev01", InstanceType: "t4g.medium", Bare: true, CloudFront: true})
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("err = %v, want the --bare/--cloudfront rejection", err)
		}
		if len(ec2f.log) != 0 {
			t.Errorf("a rejected create must not change anything: %v", ec2f.log)
		}
	})
	t.Run("cloudfront without the WAF", func(t *testing.T) {
		t.Parallel()
		svc, ec2f, _ := newCreateAccount(&injector{})
		svc.clients.WAF = noWebACLWAF{}
		_, err := svc.CreateInstance(context.Background(), CreateInstanceInput{Name: "dev01", InstanceType: "t4g.medium", CloudFront: true})
		if err == nil || !strings.Contains(err.Error(), "nakama aws init") {
			t.Fatalf("err = %v, want the missing web ACL", err)
		}
		if len(ec2f.log) != 0 {
			t.Errorf("a missing web ACL must stop create before anything is made: %v", ec2f.log)
		}
	})
}

// noWebACLWAF is an account where `nakama aws init` never created the
// web ACL.
type noWebACLWAF struct{ awsx.WAFAPI }

func (noWebACLWAF) ListWebACLs(context.Context, *wafv2.ListWebACLsInput, ...func(*wafv2.Options)) (*wafv2.ListWebACLsOutput, error) {
	return &wafv2.ListWebACLsOutput{}, nil
}

// --- add ------------------------------------------------------------------

func TestAddCloudFront_BuildsTheEntryPathInOrder(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	svc, sm := cfService(a, nil)

	out, err := svc.AddCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("AddCloudFront: %v", err)
	}
	want := []string{"AuthorizeIngress", "CreateVpcOrigin vo-1", "CreateDistribution E1"}
	if got := mutating(a.calls()); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if out.DistributionID != "E1" || out.VPCOriginID != "vo-1" || out.URL != "https://d1example.cloudfront.net" || out.AlreadyPresent {
		t.Errorf("output = %+v", out)
	}
	for path, want := range map[string]string{
		ssmVPCOriginIDPath("dev01"):        "vo-1",
		ssmDistributionIDPath("dev01"):     "E1",
		ssmDistributionDomainPath("dev01"): "d1example.cloudfront.net",
	} {
		if got := sm.param(path); got != want {
			t.Errorf("SSM %s = %q, want %q", path, got, want)
		}
	}
	// The same shape create builds: the VPC origin on this instance, the
	// distribution on that VPC origin with the private DNS name as its
	// origin domain and the shared WAF web ACL.
	if !strings.HasSuffix(a.originARN, ":instance/"+cfTestInstance) || a.distVPCOrigin != "vo-1" ||
		a.distDomain != cfTestPrivDNS || !strings.Contains(a.distWebACL, "nakama-waf") {
		t.Errorf("origin ARN %q, distribution origin %q / %q, web ACL %q", a.originARN, a.distVPCOrigin, a.distDomain, a.distWebACL)
	}
	if len(sm.scripts) != 0 {
		t.Errorf("add must not run anything on the instance: %v", sm.scripts)
	}
}

func TestAddCloudFront_IsIdempotent(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	svc, _ := cfService(a, nil)
	if _, err := svc.AddCloudFront(context.Background(), "dev01"); err != nil {
		t.Fatal(err)
	}
	n := len(a.calls())
	out, err := svc.AddCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if got := mutating(a.calls()[n:]); len(got) != 0 {
		t.Errorf("a second add must create nothing, called %v", got)
	}
	if !out.AlreadyPresent || out.URL != "https://d1example.cloudfront.net" {
		t.Errorf("second add output = %+v", out)
	}
}

// TestAddCloudFront_ResumesAfterEachStage stops add at every stage, then
// runs it again: the second run completes the entry path, and no VPC
// origin or distribution is ever created twice.
func TestAddCloudFront_ResumesAfterEachStage(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{cfStageAuthorize, cfStageCreateOrigin, cfStageOriginWait, cfStageCreateDist, cfStageDistWait} {
		for _, cancel := range []bool{false, true} {
			name := stage
			if cancel {
				name += "/cancel"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				a := newCFAccount(ec2types.InstanceStateNameRunning)
				svc, sm := cfService(a, nil)
				ctx, stop := context.WithCancel(context.Background())
				defer stop()
				a.inj = &injector{at: stage}
				if cancel {
					a.inj.cancel = stop
				}
				if _, err := svc.AddCloudFront(ctx, "dev01"); err == nil {
					t.Fatalf("AddCloudFront succeeded although %s failed", stage)
				}
				if !a.inj.fired {
					t.Fatalf("add never reached %s", stage)
				}
				// What exists by now is recorded.
				if a.nOrigin > 0 && sm.param(ssmVPCOriginIDPath("dev01")) != "vo-1" {
					t.Errorf("the VPC origin must be recorded the moment it exists")
				}
				if a.nDist > 0 && sm.param(ssmDistributionIDPath("dev01")) != "E1" {
					t.Errorf("the distribution must be recorded the moment it exists")
				}

				out, err := svc.AddCloudFront(context.Background(), "dev01")
				if err != nil {
					t.Fatalf("resumed add: %v", err)
				}
				if a.nOrigin != 1 || a.nDist != 1 || out.DistributionID != "E1" || out.VPCOriginID != "vo-1" || !a.ingress {
					t.Errorf("after the resume: %d VPC origins, %d distributions, rule %v, output %+v; want one of each",
						a.nOrigin, a.nDist, a.ingress, out)
				}
			})
		}
	}
}

// TestAddCloudFront_RecreatesWhatIsGone: a recorded VPC origin or
// distribution that CloudFront no longer has is built again.
func TestAddCloudFront_RecreatesWhatIsGone(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	svc, sm := cfService(a, map[string]string{
		ssmVPCOriginIDPath("dev01"):        "vo-gone",
		ssmDistributionIDPath("dev01"):     "EGONE",
		ssmDistributionDomainPath("dev01"): "dgone.cloudfront.net",
	})
	out, err := svc.AddCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("AddCloudFront: %v", err)
	}
	if out.VPCOriginID != "vo-1" || out.DistributionID != "E1" || out.AlreadyPresent {
		t.Errorf("output = %+v, want both rebuilt", out)
	}
	if sm.param(ssmDistributionDomainPath("dev01")) != "d1example.cloudfront.net" {
		t.Errorf("the domain of the new distribution must replace the old record")
	}
}

// TestAddCloudFront_RecordsTheDomainFromCloudFront: when the entry path
// exists but the distribution's domain was not recorded, the domain is
// taken from CloudFront and nothing is created.
func TestAddCloudFront_RecordsTheDomainFromCloudFront(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	params := a.withCloudFront(false)
	params[ssmDistributionDomainPath("dev01")] = ""
	svc, sm := cfService(a, params)
	out, err := svc.AddCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("AddCloudFront: %v", err)
	}
	if !out.AlreadyPresent || out.URL != "https://dold.cloudfront.net" || sm.param(ssmDistributionDomainPath("dev01")) != "dold.cloudfront.net" {
		t.Errorf("output = %+v, recorded domain %q", out, sm.param(ssmDistributionDomainPath("dev01")))
	}
}

func TestAddCloudFront_Preconditions(t *testing.T) {
	t.Parallel()
	type setup func(a *cfAccount, params map[string]string)
	cases := []struct {
		name    string
		state   ec2types.InstanceStateName
		setup   setup
		wantErr string
	}{
		{name: "bare", setup: func(_ *cfAccount, p map[string]string) { p[ssmKindPath("dev01")] = instanceKindBare }, wantErr: "bare"},
		{name: "stopped", state: ec2types.InstanceStateNameStopped, wantErr: "nakama aws start dev01"},
		{name: "unknown instance", setup: func(_ *cfAccount, p map[string]string) { p[ssmInstanceIDPath("dev01")] = "" }, wantErr: "not found"},
		{name: "not initialized", setup: func(_ *cfAccount, p map[string]string) { p[naming.SSMMetaInitialized()] = "false" }, wantErr: "nakama aws init"},
		{name: "no security group", setup: func(_ *cfAccount, p map[string]string) { p[ssmSGIDPath("dev01")] = "" }, wantErr: "security group"},
		{
			name: "distribution disabled by a stopped delete",
			setup: func(a *cfAccount, p map[string]string) {
				for k, v := range a.withCloudFront(false) {
					p[k] = v
				}
				a.dists["EOLD"].enabled = false
			},
			wantErr: "cloudfront delete dev01",
		},
		{
			name: "a stopped delete left its marker",
			setup: func(a *cfAccount, p map[string]string) {
				for k, v := range a.withCloudFront(false) {
					p[k] = v
				}
				p[ssmCloudFrontDeletingPath("dev01")] = "true"
			},
			wantErr: "cloudfront delete dev01",
		},
		{
			name: "distribution without its VPC origin",
			setup: func(a *cfAccount, p map[string]string) {
				for k, v := range a.withCloudFront(false) {
					p[k] = v
				}
				delete(a.origins, "vo-old")
			},
			wantErr: "its VPC origin does not",
		},
		{
			name: "Kiro Crew distribution without the code-server one",
			setup: func(_ *cfAccount, p map[string]string) {
				p[ssmKirocrewDistributionIDPath("dev01")] = "ECREW"
			},
			wantErr: "Kiro Crew distribution",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			state := c.state
			if state == "" {
				state = ec2types.InstanceStateNameRunning
			}
			a := newCFAccount(state)
			params := map[string]string{}
			if c.setup != nil {
				c.setup(a, params)
			}
			before := len(a.calls())
			svc, sm := cfService(a, params)
			_, err := svc.AddCloudFront(context.Background(), "dev01")
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
			if got := mutating(a.calls()[before:]); len(got) != 0 {
				t.Errorf("a refused add must change nothing, called %v", got)
			}
			if len(sm.scripts) != 0 {
				t.Errorf("a refused add must not run anything on the instance")
			}
		})
	}
}

// --- delete ---------------------------------------------------------------

// shortenFunctionRetry makes deleteKirocrewFunction's retry pacing
// instant. Tests using it must not call t.Parallel().
func shortenFunctionRetry(t *testing.T) {
	t.Helper()
	prev := kirocrewFunctionRetryInterval
	kirocrewFunctionRetryInterval = time.Millisecond
	t.Cleanup(func() { kirocrewFunctionRetryInterval = prev })
}

func TestDeleteCloudFront_RemovesEverythingInOrder(t *testing.T) {
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	svc, sm := cfService(a, a.withCloudFront(true))
	shortenFunctionRetry(t)

	p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("PlanDeleteCloudFront: %v", err)
	}
	if !p.HasCloudFront() || p.DistributionID != "EOLD" || p.DistributionDomain != "dold.cloudfront.net" ||
		p.KiroCrewDistributionID != "ECREW" || p.KiroCrewDomain != testCrewDomain || p.VPCOriginID != "vo-old" ||
		p.SecurityGroupID != cfTestSG || p.State != "running" {
		t.Errorf("plan = %+v", p)
	}
	if len(a.calls()) != 0 {
		t.Errorf("planning must not change anything: %v", a.calls())
	}

	out, err := svc.DeleteCloudFront(context.Background(), p)
	if err != nil {
		t.Fatalf("DeleteCloudFront: %v", err)
	}
	// The rule goes first, so nothing reaches the instance through
	// CloudFront while the slow teardown runs; the VPC origin goes last.
	want := []string{
		"RevokeIngress",
		"DisableDistribution ECREW", "DeleteDistribution ECREW",
		"DisableDistribution EOLD", "DeleteDistribution EOLD",
		"DeleteFunction",
		"DeleteVpcOrigin vo-old",
	}
	if got := mutating(a.calls()); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("calls = %v\nwant    %v", got, want)
	}
	if !out.IngressRuleRemoved || out.InstanceSkipped || out.DistributionID != "EOLD" || out.KiroCrewDistributionID != "ECREW" {
		t.Errorf("output = %+v", out)
	}
	if !strings.Contains(sm.lastScript(), "rm -f "+kirocrewNginxConfPath) {
		t.Error("the Kiro Crew instance side must be cleaned up on a running instance")
	}
	for _, k := range cloudFrontMetaKeys("dev01") {
		if sm.param(k) != "" {
			t.Errorf("%s must be removed", k)
		}
	}
	// Only the CloudFront records go; the instance stays.
	for _, k := range []string{ssmInstanceIDPath("dev01"), ssmSGIDPath("dev01"), ssmENIIDPath("dev01")} {
		if sm.param(k) == "" {
			t.Errorf("%s must be kept", k)
		}
	}
}

func TestDeleteCloudFront_WithoutKiroCrewTouchesNoFunctionAndNoInstance(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	svc, sm := cfService(a, a.withCloudFront(false))
	p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteCloudFront(context.Background(), p); err != nil {
		t.Fatalf("DeleteCloudFront: %v", err)
	}
	want := []string{"RevokeIngress", "DisableDistribution EOLD", "DeleteDistribution EOLD", "DeleteVpcOrigin vo-old"}
	if got := a.calls(); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("calls = %v, want %v (no CloudFront Functions call)", got, want)
	}
	if len(sm.scripts) != 0 {
		t.Errorf("without Kiro Crew web access nothing runs on the instance: %v", sm.scripts)
	}
}

func TestDeleteCloudFront_StoppedInstanceSkipsTheInstanceSide(t *testing.T) {
	a := newCFAccount(ec2types.InstanceStateNameStopped)
	svc, sm := cfService(a, a.withCloudFront(true))
	shortenFunctionRetry(t)
	p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.DeleteCloudFront(context.Background(), p)
	if err != nil {
		t.Fatalf("DeleteCloudFront: %v", err)
	}
	if !out.InstanceSkipped || len(sm.scripts) != 0 {
		t.Errorf("a stopped instance must be skipped, not woken: %+v, scripts %d", out, len(sm.scripts))
	}
	if len(a.dists) != 0 || len(a.origins) != 0 || sm.param(ssmKirocrewDistributionIDPath("dev01")) != "" {
		t.Errorf("the CloudFront side must be gone even so: dists %v, origins %v", a.dists, a.origins)
	}
}

// TestDeleteCloudFront_ResumesAfterEachStage stops delete at each stage
// and runs it again from a fresh plan, as the user would: the second run
// completes, and the records stay until the end.
func TestDeleteCloudFront_ResumesAfterEachStage(t *testing.T) {
	shortenFunctionRetry(t)
	for _, stage := range []string{cfStageRevoke, cfStageDeleteDist, cfStageDeleteOrigin} {
		t.Run(stage, func(t *testing.T) {
			a := newCFAccount(ec2types.InstanceStateNameRunning)
			svc, sm := cfService(a, a.withCloudFront(true))
			a.inj = &injector{at: stage}
			p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.DeleteCloudFront(context.Background(), p); err == nil {
				t.Fatalf("DeleteCloudFront succeeded although %s failed", stage)
			}
			if stage == cfStageRevoke && len(mutating(a.calls())) != 0 {
				t.Errorf("a failed revoke must stop delete before anything else: %v", a.calls())
			}
			if sm.param(ssmVPCOriginIDPath("dev01")) == "" || sm.param(ssmDistributionIDPath("dev01")) == "" {
				t.Fatal("the records must stay until everything is gone, so a re-run finds it")
			}
			// Meanwhile add refuses to build on the half-removed path.
			if sm.param(ssmCloudFrontDeletingPath("dev01")) != "true" {
				t.Fatal("delete must mark the instance before its first step")
			}
			if _, err := svc.AddCloudFront(context.Background(), "dev01"); err == nil || !strings.Contains(err.Error(), "stopped part way") {
				t.Errorf("add after a stopped delete: err = %v, want the refusal", err)
			}
			p, err = svc.PlanDeleteCloudFront(context.Background(), "dev01")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.DeleteCloudFront(context.Background(), p); err != nil {
				t.Fatalf("resumed delete: %v", err)
			}
			if a.ingress || len(a.dists) != 0 || len(a.origins) != 0 || a.function {
				t.Errorf("left behind: rule %v, dists %v, origins %v, function %v", a.ingress, a.dists, a.origins, a.function)
			}
			for _, k := range cloudFrontMetaKeys("dev01") {
				if sm.param(k) != "" {
					t.Errorf("%s must be removed", k)
				}
			}
		})
	}
}

// TestDeleteCloudFront_OnAnInstanceWithoutCloudFront: a bare instance
// created before ADR 0028 still carries the inbound rule; delete removes
// just that. A second run, or one on an instance that never had the
// rule, changes nothing.
func TestDeleteCloudFront_OnAnInstanceWithoutCloudFront(t *testing.T) {
	t.Parallel()
	for _, notFound := range []bool{false, true} {
		t.Run(fmt.Sprintf("revokeNotFound=%v", notFound), func(t *testing.T) {
			t.Parallel()
			a := newCFAccount(ec2types.InstanceStateNameRunning)
			a.ingress = true
			a.revokeNotFound = notFound
			svc, sm := cfService(a, map[string]string{ssmKindPath("dev01"): instanceKindBare})
			p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
			if err != nil {
				t.Fatal(err)
			}
			if p.HasCloudFront() {
				t.Errorf("plan = %+v, want no CloudFront", p)
			}
			out, err := svc.DeleteCloudFront(context.Background(), p)
			if err != nil {
				t.Fatalf("DeleteCloudFront: %v", err)
			}
			if !out.IngressRuleRemoved || strings.Join(a.calls(), ",") != "RevokeIngress" || len(sm.scripts) != 0 {
				t.Errorf("output %+v, calls %v; want only the rule removed", out, a.calls())
			}
			if sm.param(ssmCloudFrontDeletingPath("dev01")) != "" {
				t.Error("with nothing to take apart, no marker is left")
			}
			out, err = svc.DeleteCloudFront(context.Background(), p)
			if err != nil {
				t.Fatalf("second DeleteCloudFront: %v", err)
			}
			if out.IngressRuleRemoved || len(a.calls()) != 1 {
				t.Errorf("a second run must change nothing: %+v, calls %v", out, a.calls())
			}
		})
	}
}

// TestDeleteCloudFront_WaitsForTheDisableAStoppedRunLeft: a delete that
// was stopped right after disabling the distribution leaves it
// Enabled=false but not yet deployed, and CloudFront refuses to delete
// it until then (DistributionNotDisabled, seen on AWS). The re-run must
// wait for the deploy instead of failing.
func TestDeleteCloudFront_WaitsForTheDisableAStoppedRunLeft(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	params := a.withCloudFront(false)
	a.ingress = false // the stopped run removed the rule already
	a.dists["EOLD"].enabled, a.dists["EOLD"].disabling = false, true
	params[ssmCloudFrontDeletingPath("dev01")] = "true"
	svc, sm := cfService(a, params)
	p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteCloudFront(context.Background(), p); err != nil {
		t.Fatalf("resumed delete: %v", err)
	}
	want := []string{"DeleteDistribution EOLD", "DeleteVpcOrigin vo-old"}
	if got := mutating(a.calls()); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if sm.param(ssmCloudFrontDeletingPath("dev01")) != "" || sm.param(ssmDistributionIDPath("dev01")) != "" {
		t.Error("the records and the marker must be gone")
	}
}

// TestDeleteCloudFront_ClearsAStrayMarker: a marker left without the IDs
// it guarded (the records were removed some other way) would make add
// refuse for good. delete has nothing to take apart then, but still
// clears the records, so add works afterwards (Amazon Q on PR #121).
func TestDeleteCloudFront_ClearsAStrayMarker(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	svc, sm := cfService(a, map[string]string{ssmCloudFrontDeletingPath("dev01"): "true"})
	if _, err := svc.AddCloudFront(context.Background(), "dev01"); err == nil || !strings.Contains(err.Error(), "stopped part way") {
		t.Fatalf("add with a stray marker: err = %v, want the refusal", err)
	}
	p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
	if err != nil {
		t.Fatal(err)
	}
	if p.HasCloudFront() {
		t.Fatalf("plan = %+v, want nothing to take apart", p)
	}
	if _, err := svc.DeleteCloudFront(context.Background(), p); err != nil {
		t.Fatalf("DeleteCloudFront: %v", err)
	}
	if sm.param(ssmCloudFrontDeletingPath("dev01")) != "" {
		t.Fatal("delete must clear the stray marker")
	}
	if _, err := svc.AddCloudFront(context.Background(), "dev01"); err != nil {
		t.Errorf("add after delete cleared the marker: %v", err)
	}
}

// TestRevokeSGIngress_AnswersForAMissingRuleOrGroup pins which answers
// mean "nothing to remove" and which are failures.
func TestRevokeSGIngress_AnswersForAMissingRuleOrGroup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		err         error
		out         *ec2.RevokeSecurityGroupIngressOutput
		wantRemoved bool
		wantErr     bool
	}{
		{name: "removed", out: &ec2.RevokeSecurityGroupIngressOutput{Return: aws.Bool(true)}, wantRemoved: true},
		{name: "unknown rule", out: &ec2.RevokeSecurityGroupIngressOutput{UnknownIpPermissions: []ec2types.IpPermission{{}}}},
		{name: "rule not found", err: apiErr("InvalidPermission.NotFound")},
		{name: "group not found", err: apiErr("InvalidGroup.NotFound")},
		{name: "denied", err: apiErr("UnauthorizedOperation"), wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			svc := &Service{clients: &awsx.Clients{EC2: revokeEC2{out: c.out, err: c.err}}}
			removed, err := svc.revokeSGIngress(context.Background(), "sg-1", "pl-cf")
			if (err != nil) != c.wantErr || removed != c.wantRemoved {
				t.Errorf("revokeSGIngress = %v, %v; want removed %v, error %v", removed, err, c.wantRemoved, c.wantErr)
			}
		})
	}
}

type revokeEC2 struct {
	awsx.EC2API
	out *ec2.RevokeSecurityGroupIngressOutput
	err error
}

func (f revokeEC2) RevokeSecurityGroupIngress(context.Context, *ec2.RevokeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	return f.out, f.err
}

func TestDeleteCloudFront_RejectsAMissingPlan(t *testing.T) {
	t.Parallel()
	svc, _ := cfService(newCFAccount(ec2types.InstanceStateNameRunning), nil)
	if _, err := svc.DeleteCloudFront(context.Background(), nil); err == nil {
		t.Error("a nil plan must be rejected")
	}
	if _, err := svc.DeleteCloudFront(context.Background(), &DeleteCloudFrontPlan{Name: "dev01"}); err == nil {
		t.Error("a plan not made by PlanDeleteCloudFront must be rejected")
	}
	if _, err := svc.PlanDeleteCloudFront(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("an unknown instance: err = %v", err)
	}
}

// TestAddThenDeleteCloudFront_RoundTrip: add on an instance without
// CloudFront, then delete, leaves the account as it was, and add works
// again afterwards with new resources.
func TestAddThenDeleteCloudFront_RoundTrip(t *testing.T) {
	t.Parallel()
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	svc, sm := cfService(a, nil)
	for round := 1; round <= 2; round++ {
		out, err := svc.AddCloudFront(context.Background(), "dev01")
		if err != nil {
			t.Fatalf("round %d add: %v", round, err)
		}
		if out.DistributionID != fmt.Sprintf("E%d", round) {
			t.Errorf("round %d: distribution %s", round, out.DistributionID)
		}
		p, err := svc.PlanDeleteCloudFront(context.Background(), "dev01")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.DeleteCloudFront(context.Background(), p); err != nil {
			t.Fatalf("round %d delete: %v", round, err)
		}
		if a.ingress || len(a.dists) != 0 || len(a.origins) != 0 {
			t.Fatalf("round %d left: rule %v, dists %v, origins %v", round, a.ingress, a.dists, a.origins)
		}
		for _, k := range cloudFrontMetaKeys("dev01") {
			if sm.param(k) != "" {
				t.Errorf("round %d left %s", round, k)
			}
		}
	}
}

// TestDeleteInstance_SharesTheCloudFrontTeardown: delete still removes
// the entry path through the same steps (and with --force keeps going
// past a failing one).
func TestDeleteInstance_SharesTheCloudFrontTeardown(t *testing.T) {
	shortenFunctionRetry(t)
	a := newCFAccount(ec2types.InstanceStateNameRunning)
	params := a.withCloudFront(true)
	params[ssmInstanceIDPath("dev01")] = "" // only the CloudFront part is under test
	params[ssmSGIDPath("dev01")] = ""
	params[ssmENIIDPath("dev01")] = ""
	svc, _ := cfService(a, params)
	svc.clients.EC2 = deleteOnlyEC2{cfEC2: cfEC2{a: a}}
	a.inj = &injector{at: cfStageDeleteDist}
	err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01", Force: true})
	if err != nil {
		t.Fatalf("DeleteInstance --force: %v", err)
	}
	got := mutating(a.calls())
	// The first DeleteDistribution (Kiro Crew's) failed; --force went on.
	want := []string{
		"DisableDistribution ECREW",
		"DisableDistribution EOLD", "DeleteDistribution EOLD",
		"DeleteFunction",
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("calls = %v\nwant    %v", got, want)
	}
}

// deleteOnlyEC2 answers the tag lookups delete makes when SSM records
// no EC2 resources: there are none.
type deleteOnlyEC2 struct{ cfEC2 }

func (deleteOnlyEC2) DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return &ec2.DescribeNetworkInterfacesOutput{}, nil
}

func (deleteOnlyEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{}, nil
}

func (deleteOnlyEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{}, nil
}
