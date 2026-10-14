package service

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
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
	// (git show 0244a3d:internal/service/instance_create_cf.go). This is
	// json.Marshal of the SDK type, so an SDK upgrade that adds fields
	// to the CloudFront types changes it even though the request does
	// not change: unset (nil / "") members are not serialized. When
	// regenerating, check that only such fields were added.
	const want = `{"CallerReference":"nakama-ref","Comment":"nakama-dev01","DefaultCacheBehavior":{"TargetOriginId":"vpc-origin-vo_abc","ViewerProtocolPolicy":"redirect-to-https","AllowedMethods":{"Items":["GET","HEAD","OPTIONS","PUT","POST","PATCH","DELETE"],"Quantity":7,"CachedMethods":{"Items":["GET","HEAD"],"Quantity":2}},"CachePolicyId":"4135ea2d-6df8-44a3-9df3-4b5a84be39ad","Compress":false,"DefaultTTL":null,"FieldLevelEncryptionId":null,"ForwardedValues":null,"FunctionAssociations":null,"GrpcConfig":null,"LambdaFunctionAssociations":null,"MaxTTL":null,"MinTTL":null,"OriginRequestPolicyId":"216adef6-5c7f-47e4-b989-5492eafa07d3","RealtimeLogConfigArn":null,"ResponseHeadersPolicyId":null,"SmoothStreaming":null,"TrustedKeyGroups":null,"TrustedSigners":null},"Enabled":true,"Origins":{"Items":[{"DomainName":"ip-10-20-10-1.ap-northeast-1.compute.internal","Id":"vpc-origin-vo_abc","ConnectionAttempts":3,"ConnectionTimeout":10,"CustomHeaders":{"Quantity":0,"Items":null},"CustomOriginConfig":null,"OriginAccessControlId":null,"OriginPath":null,"OriginShield":{"Enabled":false,"OriginShieldRegion":null},"ResponseCompletionTimeout":null,"S3OriginConfig":null,"VpcOriginConfig":{"VpcOriginId":"vo_abc","OriginKeepaliveTimeout":null,"OriginReadTimeout":null,"OwnerAccountId":null}}],"Quantity":1},"Aliases":null,"AnycastIpListId":null,"CacheBehaviors":null,"CacheTagConfig":null,"ConnectionFunctionAssociation":null,"ConnectionMode":"","ContinuousDeploymentPolicyId":null,"CustomErrorResponses":null,"DefaultRootObject":null,"HttpVersion":"http1.1","IsIPV6Enabled":false,"Logging":null,"OriginGroups":null,"PriceClass":"PriceClass_All","Restrictions":{"GeoRestriction":{"Quantity":0,"RestrictionType":"none","Items":null}},"Staging":null,"TenantConfig":null,"ViewerCertificate":{"ACMCertificateArn":null,"Certificate":null,"CertificateSource":"","CloudFrontDefaultCertificate":true,"IAMCertificateId":null,"MinimumProtocolVersion":"TLSv1.2_2021","SSLSupportMethod":""},"ViewerMtlsConfig":null,"WebACLId":"arn:aws:wafv2:us-east-1:111122223333:global/webacl/nakama-waf/x"}`
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

// apiDefaultCacheBehaviorJSON is the default behavior exactly as
// GetDistributionConfig returned it for a nakama-created distribution in
// production (2026-10-06, only the VPC origin id replaced). The API
// fills fields nakama never set, such as SmoothStreaming; Issue #73.
const apiDefaultCacheBehaviorJSON = `{
    "TargetOriginId": "vpc-origin-vo_abc",
    "TrustedSigners": {"Enabled": false, "Quantity": 0},
    "TrustedKeyGroups": {"Enabled": false, "Quantity": 0},
    "ViewerProtocolPolicy": "redirect-to-https",
    "AllowedMethods": {
        "Quantity": 7,
        "Items": ["HEAD", "DELETE", "POST", "GET", "OPTIONS", "PUT", "PATCH"],
        "CachedMethods": {"Quantity": 2, "Items": ["HEAD", "GET"]}
    },
    "SmoothStreaming": false,
    "Compress": false,
    "LambdaFunctionAssociations": {"Quantity": 0},
    "FunctionAssociations": {"Quantity": 0},
    "FieldLevelEncryptionId": "",
    "CachePolicyId": "4135ea2d-6df8-44a3-9df3-4b5a84be39ad",
    "OriginRequestPolicyId": "216adef6-5c7f-47e4-b989-5492eafa07d3",
    "GrpcConfig": {"Enabled": false}
}`

// apiShapedDistributionConfig is the code-server distribution as the API
// returns it on read.
func apiShapedDistributionConfig(t *testing.T) *cftypes.DistributionConfig {
	t.Helper()
	cfg := distributionConfig(distributionParams{
		callerReference: "nakama-ref", comment: "nakama-dev01",
		vpcOriginID: testVPCOriginID, originDomain: testOriginDNS, wafWebACLARN: testWAFARN,
	})
	var def cftypes.DefaultCacheBehavior
	if err := json.Unmarshal([]byte(apiDefaultCacheBehaviorJSON), &def); err != nil {
		t.Fatal(err)
	}
	cfg.DefaultCacheBehavior = &def
	return cfg
}

func exportedFieldNames(t reflect.Type, skip ...string) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && !slices.Contains(skip, f.Name) {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

func TestKirocrewCacheBehaviorCopiesEveryField(t *testing.T) {
	defType := reflect.TypeOf(cftypes.DefaultCacheBehavior{})
	// The SDK's two behavior types must keep the same fields (plus
	// PathPattern); a field CloudFront adds to both must be handled.
	if d, c := exportedFieldNames(defType), exportedFieldNames(reflect.TypeOf(cftypes.CacheBehavior{}), "PathPattern"); !slices.Equal(d, c) {
		t.Fatalf("DefaultCacheBehavior and CacheBehavior diverged:\n default: %v\nbehavior: %v", d, c)
	}

	// Give every field of the default a value that neither the zero value
	// nor kirocrewCacheBehavior's fallbacks could produce.
	var def cftypes.DefaultCacheBehavior
	dv := reflect.ValueOf(&def).Elem()
	for i := 0; i < dv.NumField(); i++ {
		f, sf := dv.Field(i), defType.Field(i)
		if !sf.IsExported() {
			continue
		}
		switch {
		case f.Kind() == reflect.String: // enums such as ViewerProtocolPolicy
			f.SetString("marker-" + sf.Name)
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.String:
			f.Set(reflect.ValueOf(aws.String("marker-" + sf.Name)))
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Bool:
			f.Set(reflect.ValueOf(aws.Bool(true)))
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Int64:
			f.Set(reflect.ValueOf(aws.Int64(4242)))
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct:
			f.Set(reflect.New(f.Type().Elem())) // an empty struct, unlike every fallback
		default:
			t.Fatalf("field %s has a kind (%s) this test does not know how to fill; extend it", sf.Name, f.Type())
		}
	}

	const fnARN = "arn:aws:cloudfront::111122223333:function/nakama-dev01-kirocrew"
	b := kirocrewCacheBehavior(&def, "/proxy/5476*", fnARN)
	bv := reflect.ValueOf(b)
	for _, name := range exportedFieldNames(defType, "FunctionAssociations", "LambdaFunctionAssociations") {
		if got, want := bv.FieldByName(name).Interface(), dv.FieldByName(name).Interface(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s is not copied from the default behavior: got %#v, want %#v", name, got, want)
		}
	}
	if aws.ToString(b.PathPattern) != "/proxy/5476*" {
		t.Errorf("PathPattern = %q", aws.ToString(b.PathPattern))
	}
	// The edge code is nakama's own, whatever the default carries.
	if fa := b.FunctionAssociations; fa == nil || aws.ToInt32(fa.Quantity) != 1 || len(fa.Items) != 1 ||
		aws.ToString(fa.Items[0].FunctionARN) != fnARN || fa.Items[0].EventType != cftypes.EventTypeViewerRequest {
		t.Errorf("FunctionAssociations must be exactly the redirect on viewer-request: %+v", b.FunctionAssociations)
	}
	if la := b.LambdaFunctionAssociations; la == nil || aws.ToInt32(la.Quantity) != 0 || len(la.Items) != 0 {
		t.Errorf("LambdaFunctionAssociations must be empty: %+v", b.LambdaFunctionAssociations)
	}
}

func TestKirocrewCacheBehaviorFillsWhatTheAPIRequires(t *testing.T) {
	// A default behavior nakama built itself leaves these unset; the API
	// accepts that on create but not on update.
	sparse := distributionConfig(distributionParams{callerReference: "r", comment: "c", vpcOriginID: "v", originDomain: "d", wafWebACLARN: "w"}).DefaultCacheBehavior
	b := kirocrewCacheBehavior(sparse, "/proxy/5476*", "arn:fn")
	if b.SmoothStreaming == nil || aws.ToBool(b.SmoothStreaming) {
		t.Errorf("SmoothStreaming = %v, want false", b.SmoothStreaming)
	}
	if b.Compress == nil || aws.ToBool(b.Compress) {
		t.Errorf("Compress = %v, want the default's false", b.Compress)
	}
	if b.FieldLevelEncryptionId == nil || *b.FieldLevelEncryptionId != "" {
		t.Errorf("FieldLevelEncryptionId = %v, want \"\"", b.FieldLevelEncryptionId)
	}
	if ts := b.TrustedSigners; ts == nil || aws.ToBool(ts.Enabled) || aws.ToInt32(ts.Quantity) != 0 || ts.Enabled == nil || ts.Quantity == nil {
		t.Errorf("TrustedSigners = %+v, want disabled with Quantity 0", ts)
	}
	if kg := b.TrustedKeyGroups; kg == nil || aws.ToBool(kg.Enabled) || kg.Enabled == nil || kg.Quantity == nil || aws.ToInt32(kg.Quantity) != 0 {
		t.Errorf("TrustedKeyGroups = %+v, want disabled with Quantity 0", kg)
	}
	if g := b.GrpcConfig; g == nil || g.Enabled == nil || aws.ToBool(g.Enabled) {
		t.Errorf("GrpcConfig = %+v, want disabled", g)
	}
	// Signed URLs on the default must carry over rather than be dropped:
	// the redirect paths keep the distribution's protection.
	signed := *sparse
	signed.TrustedKeyGroups = &cftypes.TrustedKeyGroups{Enabled: aws.Bool(true), Quantity: aws.Int32(1), Items: []string{"kg-1"}}
	if got := kirocrewCacheBehavior(&signed, "/p*", "arn:fn").TrustedKeyGroups; got == nil || !aws.ToBool(got.Enabled) || len(got.Items) != 1 || got.Items[0] != "kg-1" {
		t.Errorf("TrustedKeyGroups of the default must be kept: %+v", got)
	}
}

// xmlElement is a generic XML tree, enough to compare which elements the
// SDK serializes.
type xmlElement struct {
	XMLName  xml.Name
	Children []xmlElement `xml:",any"`
}

func (e xmlElement) child(name string) *xmlElement {
	for i := range e.Children {
		if e.Children[i].XMLName.Local == name {
			return &e.Children[i]
		}
	}
	return nil
}

func (e xmlElement) childNames() []string {
	var names []string
	for _, c := range e.Children {
		names = append(names, c.XMLName.Local)
	}
	sort.Strings(names)
	return names
}

// capturingTransport records a request body and fails the call, so the
// SDK serializes a real request without anything leaving the process.
type capturingTransport struct{ body []byte }

func (c *capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		c.body, _ = io.ReadAll(r.Body)
	}
	return &http.Response{StatusCode: http.StatusInternalServerError, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
}

// serializedUpdateDistribution returns the XML body the SDK sends for
// UpdateDistribution with cfg.
func serializedUpdateDistribution(t *testing.T, cfg *cftypes.DistributionConfig) xmlElement {
	t.Helper()
	rt := &capturingTransport{}
	client := cloudfront.New(cloudfront.Options{
		Region: "us-east-1", Credentials: aws.AnonymousCredentials{},
		HTTPClient: &http.Client{Transport: rt}, RetryMaxAttempts: 1,
	})
	_, _ = client.UpdateDistribution(context.Background(), &cloudfront.UpdateDistributionInput{
		Id: aws.String("E1"), IfMatch: aws.String("etag"), DistributionConfig: cfg,
	})
	if len(rt.body) == 0 {
		t.Fatal("the SDK sent no request body")
	}
	var root xmlElement
	if err := xml.Unmarshal(rt.body, &root); err != nil {
		t.Fatalf("parse the request body: %v\n%s", err, rt.body)
	}
	return root
}

func TestKirocrewBehaviorsSerializeLikeTheAPIDefault(t *testing.T) {
	// What the API returns for the default behavior is what it expects
	// back for every behavior on update.
	apiCfg := apiShapedDistributionConfig(t)
	want := append(serializedUpdateDistribution(t, apiCfg).child("DefaultCacheBehavior").childNames(), "PathPattern")
	sort.Strings(want)

	for name, cfg := range map[string]*cftypes.DistributionConfig{
		// enable reads the distribution back from the API...
		"read back from the API": apiCfg,
		// ...but the builder must not depend on that.
		"built by nakama": distributionConfig(distributionParams{
			callerReference: "r", comment: "c", vpcOriginID: testVPCOriginID, originDomain: testOriginDNS, wafWebACLARN: testWAFARN,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			next, err := withKirocrewBehaviors(cfg, "arn:aws:cloudfront::111122223333:function/nakama-dev01-kirocrew")
			if err != nil {
				t.Fatal(err)
			}
			items := serializedUpdateDistribution(t, next).child("CacheBehaviors").child("Items")
			if items == nil || len(items.Children) != len(kirocrewPathPatterns) {
				t.Fatalf("want %d serialized behaviors, got %+v", len(kirocrewPathPatterns), items)
			}
			for _, b := range items.Children {
				if got := b.childNames(); !slices.Equal(got, want) {
					t.Errorf("serialized behavior elements differ from the API's default behavior:\n got: %v\nwant: %v", got, want)
				}
			}
		})
	}
}
