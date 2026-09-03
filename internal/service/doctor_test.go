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
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/cliproxy"
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
		tunnelNames: func() ([]string, error) { return nil, nil },
	}
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
	for _, name := range []string{"waf-allowlist", "capabilities"} {
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
		publicIP: func(context.Context) (string, error) {
			if ipErr != nil {
				return "", ipErr
			}
			return ip, nil
		},
		tunnelNames: func() ([]string, error) { return nil, nil },
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
