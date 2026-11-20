package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/cliproxy"
	"github.com/ukaji3/nakama/internal/tagging"
	"github.com/ukaji3/nakama/internal/version"
)

// fakeDoctorIAM answers SimulatePrincipalPolicy with a scripted result.
// pageSize > 0 truncates each response and hands out a Marker, so the
// pagination loop in diagnoseCapabilities is exercised.
type fakeDoctorIAM struct {
	awsx.IAMAPI

	denied   []string
	err      error
	pageSize int
	calls    int
}

func (f *fakeDoctorIAM) SimulatePrincipalPolicy(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	start := 0
	if in.Marker != nil {
		var err error
		if start, err = strconv.Atoi(*in.Marker); err != nil {
			return nil, fmt.Errorf("bad marker %q", *in.Marker)
		}
	}
	end := len(in.ActionNames)
	out := &iam.SimulatePrincipalPolicyOutput{}
	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
		out.IsTruncated = true
		out.Marker = aws.String(strconv.Itoa(end))
	}
	for _, a := range in.ActionNames[start:end] {
		decision := iamtypes.PolicyEvaluationDecisionTypeAllowed
		for _, d := range f.denied {
			if d == a {
				decision = iamtypes.PolicyEvaluationDecisionTypeExplicitDeny
			}
		}
		out.EvaluationResults = append(out.EvaluationResults, iamtypes.EvaluationResult{
			EvalActionName: aws.String(a),
			EvalDecision:   decision,
		})
	}
	return out, nil
}

// checkByName finds a check in the report.
func checkByName(t *testing.T, r *DoctorReport, name string) DoctorCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no %q check: %+v", name, r.Checks)
	return DoctorCheck{}
}

// doctorOptsNoAWS builds options with the AWS-dependent parts stubbed
// out so only the local checks are exercised.
func doctorOptsNoAWS(det *cliproxy.Detection, detErr error) DoctorOptions {
	return DoctorOptions{
		ClientErr: errors.New("no credentials found"),
		detect: func(context.Context) (*cliproxy.Detection, error) {
			return det, detErr
		},
		tunnelNames:   func() ([]string, error) { return nil, nil },
		latestRelease: upToDate,
	}
}

// upToDate stands in for the GitHub check, so that no test reaches
// api.github.com.
func upToDate(context.Context) version.CheckResult {
	return version.CheckResult{Current: "v0.24.0", Latest: "v0.24.0", Status: version.StatusUpToDate}
}

func TestDiagnoseCredentialFailureStillRunsLocalChecks(t *testing.T) {
	rep := Diagnose(context.Background(), doctorOptsNoAWS(&cliproxy.Detection{}, nil))

	if got := checkByName(t, rep, "proxy").Status; got != CheckOK {
		t.Errorf("proxy status = %q, want ok (local checks must run without credentials)", got)
	}
	cred := checkByName(t, rep, "credentials")
	if cred.Status != CheckFail {
		t.Errorf("credentials status = %q, want fail", cred.Status)
	}
	if !strings.Contains(cred.Advice, "nakama aws login") {
		t.Errorf("credentials advice = %q, want it to point at login", cred.Advice)
	}
	for _, name := range []string{"waf-allowlist", "egress", "capabilities"} {
		if got := checkByName(t, rep, name).Status; got != CheckSkip {
			t.Errorf("%s status = %q, want skip", name, got)
		}
	}
	if rep.Fails != 1 {
		t.Errorf("Fails = %d, want 1 (skips must not count as failures)", rep.Fails)
	}
}

func TestDiagnoseProxyVariants(t *testing.T) {
	tests := []struct {
		name       string
		det        *cliproxy.Detection
		wantDetail string
	}{
		{"direct", &cliproxy.Detection{}, "direct connection"},
		{"env", &cliproxy.Detection{ProxyURL: "http://proxy:8080", ProxyFromEnv: true}, "from environment variables"},
		{"os", &cliproxy.Detection{ProxyURL: "http://proxy:8080"}, "auto-detected from the OS settings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := Diagnose(context.Background(), doctorOptsNoAWS(tt.det, nil))
			c := checkByName(t, rep, "proxy")
			if c.Status != CheckOK || !strings.Contains(c.Detail, tt.wantDetail) {
				t.Errorf("proxy = %+v, want ok containing %q", c, tt.wantDetail)
			}
		})
	}
}

func TestDiagnoseReportsTLSInterception(t *testing.T) {
	rep := Diagnose(context.Background(), doctorOptsNoAWS(&cliproxy.Detection{
		ProxyURL:    "http://proxy:8080",
		Intercepted: true,
		RootSubject: "CN=Corp Root CA",
		BundlePath:  "/home/u/.config/nakama/aws-ca-bundle.pem",
	}, nil))
	c := checkByName(t, rep, "tls")
	if c.Status != CheckOK {
		t.Errorf("tls status = %q, want ok (an OS-trusted interception CA is not a failure)", c.Status)
	}
	if !strings.Contains(c.Detail, "CN=Corp Root CA") {
		t.Errorf("tls detail = %q, want the re-signing root", c.Detail)
	}
	if !strings.Contains(c.Advice, "aws cli shell") {
		t.Errorf("tls advice = %q, want the cli shell pointer", c.Advice)
	}
}

func TestDiagnoseTLSVerificationWarning(t *testing.T) {
	rep := Diagnose(context.Background(), doctorOptsNoAWS(&cliproxy.Detection{
		Intercepted: true,
		RootSubject: "CN=Corp Root CA",
		CheckErr:    errors.New("x509: certificate signed by unknown authority"),
	}, nil))
	if got := checkByName(t, rep, "tls").Status; got != CheckWarn {
		t.Errorf("tls status = %q, want warn when the verification probe failed", got)
	}
}

func TestDiagnoseProxyAuthFailureAdvises(t *testing.T) {
	basic := &cliproxy.ProxyAuthError{Proxy: "http://proxy:8080", Schemes: []string{"Basic"}}
	rep := Diagnose(context.Background(), doctorOptsNoAWS(nil, basic))
	c := checkByName(t, rep, "proxy")
	if c.Status != CheckFail {
		t.Errorf("proxy status = %q, want fail", c.Status)
	}
	if !strings.Contains(c.Advice, "HTTPS_PROXY=http://user:password@") {
		t.Errorf("advice = %q, want the Basic credential form", c.Advice)
	}
	if got := checkByName(t, rep, "tls").Status; got != CheckSkip {
		t.Errorf("tls status = %q, want skip after a failed probe", got)
	}

	ntlm := &cliproxy.ProxyAuthError{Proxy: "http://proxy:8080", Schemes: []string{"NTLM", "Negotiate"}}
	rep = Diagnose(context.Background(), doctorOptsNoAWS(nil, ntlm))
	if adv := checkByName(t, rep, "proxy").Advice; !strings.Contains(adv, "px") || !strings.Contains(adv, "Cntlm") {
		t.Errorf("advice = %q, want the local relay hint for NTLM", adv)
	}
}

func TestDiagnoseUnreachableEndpoint(t *testing.T) {
	rep := Diagnose(context.Background(), doctorOptsNoAWS(nil, errors.New("dial tcp: i/o timeout")))
	c := checkByName(t, rep, "proxy")
	if c.Status != CheckFail || !strings.Contains(c.Detail, "could not reach") {
		t.Errorf("proxy = %+v, want a fail about reachability", c)
	}
}

// doctorAWSOpts wires the credential-dependent checks with fakes.
func doctorAWSOpts(t *testing.T, acl []ACLEntry, ip string, ipErr error, iamFake *fakeDoctorIAM) DoctorOptions {
	t.Helper()
	return DoctorOptions{
		Clients: &awsx.Clients{
			IAM:       iamFake,
			AccountID: "209866815526",
			CallerARN: "arn:aws:iam::209866815526:user/dev",
			Region:    "ap-northeast-1",
		},
		detect: func(context.Context) (*cliproxy.Detection, error) {
			return &cliproxy.Detection{}, nil
		},
		activeProfile: func() (string, bool) { return "prod", true },
		aclEntries:    func(context.Context) ([]ACLEntry, error) { return acl, nil },
		// One instance has CloudFront, so the allowlist is in use.
		cloudFrontInstances: func(context.Context) ([]string, error) { return []string{"dev01"}, nil },
		publicIP: func(context.Context) (string, error) {
			if ipErr != nil {
				return "", ipErr
			}
			return ip, nil
		},
		tunnelNames:   func() ([]string, error) { return nil, nil },
		latestRelease: upToDate,
		egress: func(context.Context) (*EgressStatus, error) {
			return &EgressStatus{
				RecordedMode: NATModeGateway, Initialized: true,
				RouteTarget: EgressRouteNATGateway, RouteTargetID: "nat-1",
				NATGateways: []EgressNATGateway{{ID: "nat-1", State: "available", PublicIP: "203.0.113.10"}},
			}, nil
		},
	}
}

func TestDiagnoseWAFAllowlistMembership(t *testing.T) {
	// This machine's IP is covered by a /24 entry.
	rep := Diagnose(context.Background(), doctorAWSOpts(t,
		[]ACLEntry{{CIDR: "203.0.113.0/24"}}, "203.0.113.77", nil, &fakeDoctorIAM{}))
	c := checkByName(t, rep, "waf-allowlist")
	if c.Status != CheckOK || !strings.Contains(c.Detail, "203.0.113.0/24") {
		t.Errorf("check = %+v, want ok naming the covering entry", c)
	}

	// Not in the allowlist: warn, not fail (connect/tunnel/ssh still work).
	rep = Diagnose(context.Background(), doctorAWSOpts(t,
		[]ACLEntry{{CIDR: "198.51.100.1/32"}}, "203.0.113.77", nil, &fakeDoctorIAM{}))
	c = checkByName(t, rep, "waf-allowlist")
	if c.Status != CheckWarn {
		t.Errorf("status = %q, want warn", c.Status)
	}
	if !strings.Contains(c.Advice, "nakama aws acl add") {
		t.Errorf("advice = %q, want the acl add hint", c.Advice)
	}
	if rep.Fails != 0 {
		t.Errorf("Fails = %d, want 0: a missing allowlist entry must not be fatal", rep.Fails)
	}
}

func TestDiagnoseWAFUnknownPublicIP(t *testing.T) {
	rep := Diagnose(context.Background(), doctorAWSOpts(t,
		[]ACLEntry{{CIDR: "203.0.113.0/24"}}, "", errors.New("lookup failed"), &fakeDoctorIAM{}))
	c := checkByName(t, rep, "waf-allowlist")
	if c.Status != CheckWarn || !strings.Contains(c.Detail, "could not determine") {
		t.Errorf("check = %+v, want a warn about the unknown IP", c)
	}
}

// TestDiagnoseWAFOnlyMattersWithCloudFront: CloudFront is opt-in (ADR
// 0028). With no instance that has it, the allowlist is not in use and
// the check is skipped, even when this machine is not in it; with some,
// the detail names them; when they cannot be listed, the allowlist is
// checked as before.
func TestDiagnoseWAFOnlyMattersWithCloudFront(t *testing.T) {
	notListed := []ACLEntry{{CIDR: "198.51.100.1/32"}}

	opts := doctorAWSOpts(t, notListed, "203.0.113.77", nil, &fakeDoctorIAM{})
	opts.cloudFrontInstances = func(context.Context) ([]string, error) { return nil, nil }
	listed := false
	opts.aclEntries = func(context.Context) ([]ACLEntry, error) { listed = true; return notListed, nil }
	rep := Diagnose(context.Background(), opts)
	c := checkByName(t, rep, "waf-allowlist")
	if c.Status != CheckSkip || !strings.Contains(c.Detail, "no instance has CloudFront") {
		t.Errorf("without CloudFront: check = %+v, want skip", c)
	}
	if listed {
		t.Error("without CloudFront the allowlist need not be read")
	}
	if rep.Warns != 0 {
		t.Errorf("Warns = %d, want 0 without CloudFront", rep.Warns)
	}

	opts = doctorAWSOpts(t, []ACLEntry{{CIDR: "203.0.113.0/24"}}, "203.0.113.77", nil, &fakeDoctorIAM{})
	opts.cloudFrontInstances = func(context.Context) ([]string, error) { return []string{"dev01", "web01"}, nil }
	c = checkByName(t, Diagnose(context.Background(), opts), "waf-allowlist")
	if c.Status != CheckOK || !strings.Contains(c.Detail, "CloudFront on dev01, web01") {
		t.Errorf("with CloudFront: check = %+v, want ok naming the instances", c)
	}

	opts = doctorAWSOpts(t, notListed, "203.0.113.77", nil, &fakeDoctorIAM{})
	opts.cloudFrontInstances = func(context.Context) ([]string, error) { return nil, errors.New("AccessDenied") }
	c = checkByName(t, Diagnose(context.Background(), opts), "waf-allowlist")
	if c.Status != CheckWarn || strings.Contains(c.Detail, "CloudFront on") {
		t.Errorf("lookup failed: check = %+v, want the plain allowlist verdict", c)
	}
}

// TestCloudFrontInstanceNames counts the instances whose distribution
// domain is recorded.
func TestCloudFrontInstanceNames(t *testing.T) {
	t.Parallel()
	svc, _ := cfService(newCFAccount(ec2types.InstanceStateNameRunning), nil)
	svc.clients.EC2 = listEC2{names: []string{"web01", "dev01", "box01"}}
	sm := svc.clients.SSM.(*fakeKirocrewSSM)
	sm.params[ssmDistributionDomainPath("web01")] = "dweb.cloudfront.net"
	sm.params[ssmDistributionDomainPath("dev01")] = "ddev.cloudfront.net"
	names, err := svc.cloudFrontInstanceNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "dev01,web01" {
		t.Errorf("names = %v, want dev01,web01 (sorted, box01 has none)", names)
	}
}

// listEC2 answers the instance listing with one instance per name.
type listEC2 struct {
	awsx.EC2API
	names []string
}

func (f listEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	out := &ec2.DescribeInstancesOutput{}
	for i, n := range f.names {
		out.Reservations = append(out.Reservations, ec2types.Reservation{Instances: []ec2types.Instance{{
			InstanceId: aws.String(fmt.Sprintf("i-%d", i)), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			Tags: []ec2types.Tag{{Key: aws.String(tagging.KeyName), Value: aws.String(n)}},
		}}})
	}
	return out, nil
}

func TestDiagnoseCredentialsReportsIdentityAndProfile(t *testing.T) {
	opts := doctorAWSOpts(t, nil, "203.0.113.10", nil, &fakeDoctorIAM{})
	rep := Diagnose(context.Background(), opts)
	c := checkByName(t, rep, "credentials")
	if c.Status != CheckOK {
		t.Fatalf("credentials = %+v, want ok", c)
	}
	for _, want := range []string{"209866815526", "user/dev", `active nakama profile "prod"`} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q lacks %q", c.Detail, want)
		}
	}
}

func TestDiagnoseCapabilityDenied(t *testing.T) {
	// Denying ssm:StartSession must break exactly the session-based
	// features and leave the others ok — and as warn, not fail: a
	// deliberately scoped principal is not a broken environment.
	opts := doctorAWSOpts(t, nil, "203.0.113.10", nil, &fakeDoctorIAM{denied: []string{"ssm:StartSession"}})
	rep := Diagnose(context.Background(), opts)
	for _, feature := range []string{"connect", "ssh", "tunnel"} {
		c := checkByName(t, rep, "capability: "+feature)
		if c.Status != CheckWarn {
			t.Errorf("capability %s = %q, want warn", feature, c.Status)
		}
		if !strings.Contains(c.Detail, "ssm:StartSession") {
			t.Errorf("capability %s detail = %q, want the missing action", feature, c.Detail)
		}
	}
	for _, feature := range []string{"list / info", "backup", "acl"} {
		if c := checkByName(t, rep, "capability: "+feature); c.Status != CheckOK {
			t.Errorf("capability %s = %q, want ok (unrelated to the denied action)", feature, c.Status)
		}
	}
	if rep.Fails != 0 {
		t.Errorf("Fails = %d, want 0: missing capabilities are warn, not fail", rep.Fails)
	}
}

func TestDiagnoseCapabilitiesAllAllowed(t *testing.T) {
	opts := doctorAWSOpts(t, nil, "203.0.113.10", nil, &fakeDoctorIAM{})
	rep := Diagnose(context.Background(), opts)
	var capabilityRows int
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "capability: ") {
			capabilityRows++
			if c.Status != CheckOK {
				t.Errorf("%s = %q (%s), want ok", c.Name, c.Status, c.Detail)
			}
		}
	}
	var wantRows int
	for _, row := range featurePermissionMatrix() {
		if row.Simulate {
			wantRows++
		}
	}
	if capabilityRows != wantRows {
		t.Errorf("report has %d capability rows, want %d (one per simulated feature)", capabilityRows, wantRows)
	}
}

func TestDiagnoseCapabilitiesPagination(t *testing.T) {
	// A tiny page size forces the Marker loop; every action must still
	// be evaluated (nothing may be reported missing just because it
	// arrived on a later page).
	fake := &fakeDoctorIAM{pageSize: 3}
	opts := doctorAWSOpts(t, nil, "203.0.113.10", nil, fake)
	rep := Diagnose(context.Background(), opts)
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "capability: ") && c.Status != CheckOK {
			t.Errorf("%s = %q (%s), want ok across pagination", c.Name, c.Status, c.Detail)
		}
	}
	if fake.calls < 2 {
		t.Errorf("fake was called %d times; pagination was not exercised", fake.calls)
	}
}

func TestDiagnoseCapabilitiesUnverifiable(t *testing.T) {
	opts := doctorAWSOpts(t, nil, "203.0.113.10", nil, &fakeDoctorIAM{err: errors.New("AccessDenied: iam:SimulatePrincipalPolicy")})
	rep := Diagnose(context.Background(), opts)
	c := checkByName(t, rep, "capabilities")
	if c.Status != CheckWarn {
		t.Errorf("status = %q, want warn (the check itself is optional)", c.Status)
	}
	if rep.Fails != 0 {
		t.Errorf("Fails = %d, want 0: an unverifiable optional check must not fail the run", rep.Fails)
	}
}

func TestAclEntryCoversIP(t *testing.T) {
	tests := []struct {
		cidr, ip string
		want     bool
	}{
		{"203.0.113.10/32", "203.0.113.10", true},
		{"203.0.113.10", "203.0.113.10", true},
		{"203.0.113.0/24", "203.0.113.77", true},
		{"203.0.113.0/24", "198.51.100.1", false},
		{"not-a-cidr", "203.0.113.10", false},
		{"2001:db8::/32", "2001:db8::1", true},
	}
	for _, tt := range tests {
		if got := aclEntryCoversIP(tt.cidr, tt.ip); got != tt.want {
			t.Errorf("aclEntryCoversIP(%q, %q) = %v, want %v", tt.cidr, tt.ip, got, tt.want)
		}
	}
}

func TestDiagnoseTunnelsReportsDeadForwards(t *testing.T) {
	opts := doctorOptsNoAWS(&cliproxy.Detection{}, nil)
	opts.tunnelNames = func() ([]string, error) { return []string{"dev01"}, nil }
	opts.tunnelStatus = func(context.Context, string) (*TunnelStatus, error) {
		return &TunnelStatus{
			Name: "dev01",
			Forwards: []ForwardStatus{
				{Spec: "8080:8080", State: ForwardStateActive, Since: time.Now()},
				{Spec: "5432:5432", State: ForwardStateDead, Detail: "credentials expired"},
			},
		}, nil
	}
	rep := Diagnose(context.Background(), opts)
	c := checkByName(t, rep, "tunnels")
	if c.Status != CheckWarn {
		t.Errorf("status = %q, want warn", c.Status)
	}
	if !strings.Contains(c.Detail, "5432:5432") || !strings.Contains(c.Advice, "tunnel restart") {
		t.Errorf("check = %+v, want the dead spec and the restart advice", c)
	}
}

func TestDiagnoseTunnelsHealthyAndEmpty(t *testing.T) {
	opts := doctorOptsNoAWS(&cliproxy.Detection{}, nil)
	if got := checkByName(t, Diagnose(context.Background(), opts), "tunnels").Detail; !strings.Contains(got, "no tunnels") {
		t.Errorf("detail = %q, want the empty case", got)
	}

	opts.tunnelNames = func() ([]string, error) { return []string{"dev01"}, nil }
	opts.tunnelStatus = func(context.Context, string) (*TunnelStatus, error) {
		return &TunnelStatus{Name: "dev01", Forwards: []ForwardStatus{{Spec: "8080:8080", State: ForwardStateActive}}}, nil
	}
	c := checkByName(t, Diagnose(context.Background(), opts), "tunnels")
	if c.Status != CheckOK || !strings.Contains(c.Detail, "dev01") {
		t.Errorf("check = %+v, want ok naming the tunnel", c)
	}
}

func TestDiagnoseTunnelsStaleState(t *testing.T) {
	opts := doctorOptsNoAWS(&cliproxy.Detection{}, nil)
	opts.tunnelNames = func() ([]string, error) { return []string{"dev01"}, nil }
	opts.tunnelStatus = func(context.Context, string) (*TunnelStatus, error) {
		return nil, errors.New("control channel unreachable")
	}
	c := checkByName(t, Diagnose(context.Background(), opts), "tunnels")
	if c.Status != CheckWarn || !strings.Contains(c.Detail, "does not answer") {
		t.Errorf("check = %+v, want a warn about the stale daemon", c)
	}
}

func TestDiagnoseVersion(t *testing.T) {
	const page = "https://github.com/ukaji3/nakama/releases/tag/v0.25.0"
	tests := []struct {
		name       string
		res        version.CheckResult
		wantStatus CheckStatus
		wantDetail string
		wantAdvice string
	}{
		{"up to date", version.CheckResult{Current: "0.25.0", Latest: "v0.25.0", URL: page, Status: version.StatusUpToDate},
			CheckOK, "0.25.0 is the latest release", ""},
		{"ahead", version.CheckResult{Current: "0.26.0-rc.1", Latest: "v0.25.0", URL: page, Status: version.StatusAhead},
			CheckOK, "newer than the latest release (v0.25.0)", ""},
		{"outdated", version.CheckResult{Current: "0.24.0", Latest: "v0.25.0", URL: page, Status: version.StatusOutdated},
			CheckWarn, "0.24.0 is out of date; the latest release is v0.25.0", "download v0.25.0 from " + page},
		{"development", version.CheckResult{Current: "dev", Latest: "v0.25.0", URL: page, Status: version.StatusDevelopment},
			CheckSkip, "dev is a development build", ""},
		{"check failed", version.CheckResult{Current: "0.24.0", Status: version.StatusUnknown, Err: errors.New("GitHub answered with status 403")},
			CheckWarn, "could not check for a newer release: GitHub answered with status 403", "api.github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// With and without credentials: the check runs either way.
			for _, opts := range []DoctorOptions{
				doctorOptsNoAWS(&cliproxy.Detection{}, nil),
				doctorAWSOpts(t, nil, "203.0.113.10", nil, &fakeDoctorIAM{}),
			} {
				opts.latestRelease = func(context.Context) version.CheckResult { return tt.res }
				rep := Diagnose(context.Background(), opts)
				c := checkByName(t, rep, "version")
				if c.Status != tt.wantStatus || !strings.Contains(c.Detail, tt.wantDetail) || !strings.Contains(c.Advice, tt.wantAdvice) {
					t.Errorf("version check = %+v, want %s with detail containing %q and advice containing %q",
						c, tt.wantStatus, tt.wantDetail, tt.wantAdvice)
				}
				if tt.wantAdvice == "" && c.Advice != "" {
					t.Errorf("advice = %q, want none", c.Advice)
				}
				if last := rep.Checks[len(rep.Checks)-1].Name; last != "version" {
					t.Errorf("last check = %q, want version (appended after the existing ones)", last)
				}
			}
		})
	}
}

func TestDiagnoseVersionNeverFails(t *testing.T) {
	// An old version and a failed check are warnings: neither may turn a
	// clean diagnosis into a failure.
	for _, st := range []version.Status{version.StatusOutdated, version.StatusUnknown} {
		opts := doctorAWSOpts(t, []ACLEntry{{CIDR: "203.0.113.10/32"}}, "203.0.113.10", nil, &fakeDoctorIAM{})
		opts.latestRelease = func(context.Context) version.CheckResult {
			return version.CheckResult{Current: "0.24.0", Latest: "v0.25.0", Status: st, Err: errors.New("timeout")}
		}
		rep := Diagnose(context.Background(), opts)
		if rep.Fails != 0 {
			t.Errorf("%s: Fails = %d, want 0", st, rep.Fails)
		}
		if rep.Warns != 1 {
			t.Errorf("%s: Warns = %d, want 1 (the version check only): %+v", st, rep.Warns, rep.Checks)
		}
	}
}
