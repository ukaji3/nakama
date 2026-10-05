package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

const (
	testVPCOriginID = "vo_abc"
	testOriginDNS   = "ip-10-20-10-1.ap-northeast-1.compute.internal"
	testWAFARN      = "arn:aws:wafv2:us-east-1:111122223333:global/webacl/nakama-waf/x"
)

// TestCodeServerDistributionConfigUnchanged pins the code-server
// distribution byte for byte. distributionConfig was extracted from
// createInstanceDistribution for the Kiro Crew distribution; this
// guards the existing one against accidental change (Issue #71).
func TestCodeServerDistributionConfigUnchanged(t *testing.T) {
	got, err := json.Marshal(distributionConfig(distributionParams{
		callerReference: "nakama-ref",
		comment:         "nakama-dev01",
		vpcOriginID:     testVPCOriginID,
		originDomain:    testOriginDNS,
		wafWebACLARN:    testWAFARN,
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Generated from the pre-refactor createInstanceDistribution literal
	// (git show 0244a3d:internal/service/instance_create_cf.go).
	const want = `{"CallerReference":"nakama-ref","Comment":"nakama-dev01","DefaultCacheBehavior":{"TargetOriginId":"vpc-origin-vo_abc","ViewerProtocolPolicy":"redirect-to-https","AllowedMethods":{"Items":["GET","HEAD","OPTIONS","PUT","POST","PATCH","DELETE"],"Quantity":7,"CachedMethods":{"Items":["GET","HEAD"],"Quantity":2}},"CachePolicyId":"4135ea2d-6df8-44a3-9df3-4b5a84be39ad","Compress":false,"DefaultTTL":null,"FieldLevelEncryptionId":null,"ForwardedValues":null,"FunctionAssociations":null,"GrpcConfig":null,"LambdaFunctionAssociations":null,"MaxTTL":null,"MinTTL":null,"OriginRequestPolicyId":"216adef6-5c7f-47e4-b989-5492eafa07d3","RealtimeLogConfigArn":null,"ResponseHeadersPolicyId":null,"SmoothStreaming":null,"TrustedKeyGroups":null,"TrustedSigners":null},"Enabled":true,"Origins":{"Items":[{"DomainName":"ip-10-20-10-1.ap-northeast-1.compute.internal","Id":"vpc-origin-vo_abc","ConnectionAttempts":3,"ConnectionTimeout":10,"CustomHeaders":{"Quantity":0,"Items":null},"CustomOriginConfig":null,"OriginAccessControlId":null,"OriginPath":null,"OriginShield":{"Enabled":false,"OriginShieldRegion":null},"S3OriginConfig":null,"VpcOriginConfig":{"VpcOriginId":"vo_abc"}}],"Quantity":1},"Aliases":null,"AnycastIpListId":null,"CacheBehaviors":null,"ContinuousDeploymentPolicyId":null,"CustomErrorResponses":null,"DefaultRootObject":null,"HttpVersion":"http1.1","IsIPV6Enabled":false,"Logging":null,"OriginGroups":null,"PriceClass":"PriceClass_All","Restrictions":{"GeoRestriction":{"Quantity":0,"RestrictionType":"none","Items":null}},"Staging":null,"ViewerCertificate":{"ACMCertificateArn":null,"Certificate":null,"CertificateSource":"","CloudFrontDefaultCertificate":true,"IAMCertificateId":null,"MinimumProtocolVersion":"TLSv1.2_2021","SSLSupportMethod":""},"WebACLId":"arn:aws:wafv2:us-east-1:111122223333:global/webacl/nakama-waf/x"}`
	if string(got) != want {
		t.Errorf("the code-server distribution config changed:\n got %s\nwant %s", got, want)
	}
}

func TestKirocrewDistributionConfig(t *testing.T) {
	cfg := kirocrewDistributionConfig("nakama-ref", "dev01", testVPCOriginID, testOriginDNS, testWAFARN)
	if got := aws.ToString(cfg.Comment); got != "nakama-dev01-kirocrew" {
		t.Errorf("comment = %q", got)
	}
	if got := aws.ToString(cfg.WebACLId); got != testWAFARN {
		t.Errorf("the crew distribution must reuse the shared WAF, got %q", got)
	}
	origin := cfg.Origins.Items[0]
	if got := aws.ToString(origin.VpcOriginConfig.VpcOriginId); got != testVPCOriginID {
		t.Errorf("the crew distribution must reach code-server through the existing VPC origin, got %q", got)
	}
	hdrs := origin.CustomHeaders
	if aws.ToInt32(hdrs.Quantity) != 1 || len(hdrs.Items) != 1 {
		t.Fatalf("want exactly one origin custom header, got %+v", hdrs)
	}
	if n, v := aws.ToString(hdrs.Items[0].HeaderName), aws.ToString(hdrs.Items[0].HeaderValue); n != "X-Forwarded-Host" || v != "15476.kirocrew.nakama.internal" {
		t.Errorf("custom header = %s: %s", n, v)
	}
	def := cfg.DefaultCacheBehavior
	if aws.ToString(def.CachePolicyId) != managedCachePolicyCachingDisabled ||
		aws.ToString(def.OriginRequestPolicyId) != managedOriginRequestPolicyAllViewer ||
		def.ViewerProtocolPolicy != cftypes.ViewerProtocolPolicyRedirectToHttps {
		t.Errorf("the crew distribution must use the same policies as code-server: %+v", def)
	}
}

// runRedirectFunction executes the CloudFront Function with node. The
// cloudfront-js-2.0 runtime is ES5.1-plus; the function sticks to that
// subset, so node evaluates it the same way.
func runRedirectFunction(t *testing.T, code string, event map[string]any) map[string]any {
	t.Helper()
	ev, _ := json.Marshal(event)
	script := code + "\nprocess.stdout.write(JSON.stringify(handler(" + string(ev) + ")));\n"
	path := filepath.Join(t.TempDir(), "fn.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return res
}

func TestKirocrewRedirectFunction(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	code, err := kirocrewRedirectFunctionCode(testCrewDomain)
	if err != nil {
		t.Fatal(err)
	}
	qs := func(kv ...string) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = map[string]any{"value": kv[i+1]}
		}
		return m
	}
	const token = "eyJzdWIiOiJsb2NhbC1hcHAifQ.7n8abmoz4xQ0RMKQBQJeUeQ9NgJN52tqnwC7EDwZLOI"
	for _, tc := range []struct {
		uri  string
		qs   map[string]any
		want string
	}{
		{"/proxy/5476/", qs("token", token), "https://" + testCrewDomain + "/?token=" + token},
		{"/proxy/5476", qs(), "https://" + testCrewDomain + "/"},
		{"/absproxy/5476/chat/abc", qs("a", "1", "b", "2"), "https://" + testCrewDomain + "/chat/abc?a=1&b=2"},
		{"/proxy/5476/x", qs("q", "a b&c"), "https://" + testCrewDomain + "/x?q=a%20b%26c"},
	} {
		res := runRedirectFunction(t, code, map[string]any{"request": map[string]any{"uri": tc.uri, "querystring": tc.qs}})
		if res["statusCode"] != float64(302) {
			t.Errorf("%s: statusCode = %v, want 302", tc.uri, res["statusCode"])
			continue
		}
		loc := res["headers"].(map[string]any)["location"].(map[string]any)["value"]
		if loc != tc.want {
			t.Errorf("%s: location = %v, want %s", tc.uri, loc, tc.want)
		}
	}

	// multiValue (repeated keys) survive.
	res := runRedirectFunction(t, code, map[string]any{"request": map[string]any{
		"uri": "/proxy/5476/", "querystring": map[string]any{
			"k": map[string]any{"value": "1", "multiValue": []any{map[string]any{"value": "1"}, map[string]any{"value": "2"}}},
		},
	}})
	if loc := res["headers"].(map[string]any)["location"].(map[string]any)["value"]; loc != "https://"+testCrewDomain+"/?k=1&k=2" {
		t.Errorf("multiValue location = %v", loc)
	}

	// Other ports and other paths pass through as the request itself.
	for _, uri := range []string{"/proxy/54760/", "/proxy/547/", "/proxy/5476x/", "/", "/login", "/absproxy/8080/"} {
		res := runRedirectFunction(t, code, map[string]any{"request": map[string]any{"uri": uri, "querystring": map[string]any{}}})
		if _, isResponse := res["statusCode"]; isResponse || res["uri"] != uri {
			t.Errorf("%s must pass through untouched, got %v", uri, res)
		}
	}
}

func TestKirocrewRedirectFunctionValidatesDomain(t *testing.T) {
	if _, err := kirocrewRedirectFunctionCode("evil.example'; x='"); err == nil {
		t.Error("the crew domain is interpolated into JavaScript and must be validated")
	}
}

func testExistingDistributionConfig() *cftypes.DistributionConfig {
	cfg := distributionConfig(distributionParams{
		callerReference: "nakama-ref", comment: "nakama-dev01",
		vpcOriginID: testVPCOriginID, originDomain: testOriginDNS, wafWebACLARN: testWAFARN,
	})
	// A user-added behavior that must survive nakama's edits.
	cfg.CacheBehaviors = &cftypes.CacheBehaviors{Quantity: aws.Int32(1), Items: []cftypes.CacheBehavior{{
		PathPattern: aws.String("/static/*"), TargetOriginId: aws.String("vpc-origin-" + testVPCOriginID),
	}}}
	return cfg
}

func TestWithKirocrewBehaviorsIsIdempotent(t *testing.T) {
	const fnARN = "arn:aws:cloudfront::111122223333:function/nakama-dev01-kirocrew"
	cfg := testExistingDistributionConfig()
	once, err := withKirocrewBehaviors(cfg, fnARN)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := withKirocrewBehaviors(once, fnARN)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(once)
	b, _ := json.Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("applying the behaviors twice must be a no-op:\n%s\n%s", a, b)
	}
	if got := aws.ToInt32(once.CacheBehaviors.Quantity); got != 3 || len(once.CacheBehaviors.Items) != 3 {
		t.Fatalf("want 2 nakama behaviors + 1 user behavior, got Quantity=%d items=%d", got, len(once.CacheBehaviors.Items))
	}
	for i, want := range []string{"/proxy/5476*", "/absproxy/5476*", "/static/*"} {
		if got := aws.ToString(once.CacheBehaviors.Items[i].PathPattern); got != want {
			t.Errorf("behavior %d = %q, want %q (nakama's go first, the user's are kept)", i, got, want)
		}
	}
	nb := once.CacheBehaviors.Items[0]
	if nb.FunctionAssociations == nil || aws.ToString(nb.FunctionAssociations.Items[0].FunctionARN) != fnARN ||
		nb.FunctionAssociations.Items[0].EventType != cftypes.EventTypeViewerRequest {
		t.Errorf("the redirect function must run on viewer-request: %+v", nb.FunctionAssociations)
	}
	if aws.ToString(nb.TargetOriginId) != aws.ToString(cfg.DefaultCacheBehavior.TargetOriginId) ||
		aws.ToString(nb.OriginRequestPolicyId) != managedOriginRequestPolicyAllViewer {
		t.Errorf("the behavior must mirror the default one: %+v", nb)
	}
	if !hasKirocrewBehaviors(once) || hasKirocrewBehaviors(cfg) {
		t.Error("hasKirocrewBehaviors disagrees with the config")
	}
	// The input is not mutated.
	if aws.ToInt32(cfg.CacheBehaviors.Quantity) != 1 {
		t.Error("withKirocrewBehaviors must not mutate its input")
	}
}

func TestWithoutKirocrewBehaviorsRestoresTheOriginal(t *testing.T) {
	cfg := testExistingDistributionConfig()
	with, err := withKirocrewBehaviors(cfg, "arn:aws:cloudfront::1:function/f")
	if err != nil {
		t.Fatal(err)
	}
	back := withoutKirocrewBehaviors(with)
	a, _ := json.Marshal(cfg)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Errorf("removing the behaviors must restore the original:\n%s\n%s", a, b)
	}
	// Removing from a config that never had them is a no-op too, and an
	// empty list is expressed as Quantity 0 with no Items.
	plain := distributionConfig(distributionParams{callerReference: "r", comment: "c", vpcOriginID: "v", originDomain: "d", wafWebACLARN: "w"})
	cleared := withoutKirocrewBehaviors(plain)
	if aws.ToInt32(cleared.CacheBehaviors.Quantity) != 0 || cleared.CacheBehaviors.Items != nil {
		t.Errorf("an empty behavior list must be Quantity=0 without Items: %+v", cleared.CacheBehaviors)
	}
}

func TestKirocrewNamesFitCloudFrontLimits(t *testing.T) {
	long := strings.Repeat("a", 32) // the longest instance name nakama accepts
	if n := len(kirocrewFunctionName(long)); n > 64 {
		t.Errorf("function name is %d chars, CloudFront allows 64", n)
	}
}
