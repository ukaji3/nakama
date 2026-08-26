package awsx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/ukaji3/nakama/internal/profilestore"
)

// fakeProvider is a stub aws.CredentialsProvider that records the
// login session the newLoginProvider hook was invoked with.
type fakeProvider struct {
	loginSession string
}

func (f *fakeProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "k", SecretAccessKey: "s"}, nil
}

// resolveRecorder builds resolverHooks whose collaborators are all
// fakes, recording every interaction for assertions.
type resolveRecorder struct {
	activeProfile *profilestore.Profile
	loadStoreErr  error

	envProfile string

	// resolveLoginSession script.
	defaultSession    string
	defaultSessionErr error
	sessionProfiles   []string

	// probe script: consumed one call at a time.
	probeResults []probeResult

	loadConfigCalls     []loadConfigCall
	providerCalls       []fakeProvider
	probeCalls          int
	defaultSessionCalls int
}

type probeResult struct {
	accountID string
	callerARN string
	err       error
}

type loadConfigCall struct {
	profile     string
	hasProvider bool
	provider    aws.CredentialsProvider
}

func (r *resolveRecorder) hooks() resolverHooks {
	return resolverHooks{
		loadActiveProfile: func() (*profilestore.Profile, error) {
			return r.activeProfile, r.loadStoreErr
		},
		profileFromEnv: func() string { return r.envProfile },
		loadConfig: func(_ context.Context, profile string, provider aws.CredentialsProvider) (aws.Config, error) {
			r.loadConfigCalls = append(r.loadConfigCalls, loadConfigCall{
				profile:     profile,
				hasProvider: provider != nil,
				provider:    provider,
			})
			return aws.Config{}, nil
		},
		probe: func(context.Context, aws.Config) (string, string, error) {
			if r.probeCalls >= len(r.probeResults) {
				panic("probe called more times than scripted")
			}
			res := r.probeResults[r.probeCalls]
			r.probeCalls++
			return res.accountID, res.callerARN, res.err
		},
		newLoginProvider: func(loginSession string) aws.CredentialsProvider {
			p := &fakeProvider{loginSession: loginSession}
			r.providerCalls = append(r.providerCalls, *p)
			return p
		},
		resolveLoginSession: func(profile string) (string, error) {
			r.defaultSessionCalls++
			r.sessionProfiles = append(r.sessionProfiles, profile)
			return r.defaultSession, r.defaultSessionErr
		},
	}
}

func activeFixture() *profilestore.Profile {
	return &profilestore.Profile{
		AccountID:    "111111111111",
		Alias:        "dev",
		LoginSession: "arn:aws:signin:::user/example",
		IdentityARN:  "arn:aws:sts::111111111111:assumed-role/Admin/u",
		SavedAt:      "2026-07-30T00:00:00+09:00",
	}
}

func TestResolveAWSConfig_ActiveProfileUsed(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		activeProfile: activeFixture(),
		envProfile:    "should-be-ignored", // active profile must win over AWS_PROFILE
		probeResults:  []probeResult{{accountID: "111111111111", callerARN: "arn:x"}},
	}
	_, accountID, callerARN, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err != nil {
		t.Fatalf("resolveAWSConfig: %v", err)
	}
	if accountID != "111111111111" || callerARN != "arn:x" {
		t.Errorf("identity=(%q,%q)", accountID, callerARN)
	}

	if len(rec.providerCalls) != 1 {
		t.Fatalf("newLoginProvider calls=%d, want 1", len(rec.providerCalls))
	}
	if got := rec.providerCalls[0]; got.loginSession != "arn:aws:signin:::user/example" {
		t.Errorf("provider constructed with session %q", got.loginSession)
	}
	if len(rec.loadConfigCalls) != 1 {
		t.Fatalf("loadConfig calls=%d, want 1", len(rec.loadConfigCalls))
	}
	lc := rec.loadConfigCalls[0]
	if lc.profile != "" {
		t.Errorf("loadConfig profile=%q; active profile must take precedence over AWS_PROFILE", lc.profile)
	}
	if !lc.hasProvider {
		t.Error("loadConfig must receive the native login provider")
	}
	if rec.defaultSessionCalls != 0 {
		t.Errorf("resolveLoginSession must not be consulted for the active profile path, got %d calls", rec.defaultSessionCalls)
	}
}

func TestResolveAWSConfig_ActiveProfileProbeFailure_FailClosed(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		activeProfile: activeFixture(),
		probeResults:  []probeResult{{err: errors.New("expired token")}},
	}
	_, _, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err == nil {
		t.Fatal("expected fail-closed error")
	}

	// Fail-closed: exactly one probe, no second loadConfig fallback.
	if rec.probeCalls != 1 {
		t.Errorf("probe calls=%d, want 1 (no fallback allowed)", rec.probeCalls)
	}
	if len(rec.loadConfigCalls) != 1 {
		t.Errorf("loadConfig calls=%d, want 1 (no fallback allowed)", len(rec.loadConfigCalls))
	}

	msg := err.Error()
	for _, want := range []string{
		"dev",                       // active alias
		"111111111111",              // active account
		"nakama aws login",          // recovery step 1 (native login)
		"aws login",                 // recovery alternative
		"nakama aws profile unload", // recovery step 2
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

func TestResolveAWSConfig_ActiveProfileAccountMismatch(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		activeProfile: activeFixture(),
		probeResults:  []probeResult{{accountID: "999999999999", callerARN: "arn:y"}},
	}
	_, _, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err == nil {
		t.Fatal("expected account mismatch error")
	}
	msg := err.Error()
	for _, want := range []string{"111111111111", "999999999999", "nakama aws profile unload", "nakama aws login"} {
		if !strings.Contains(msg, want) {
			t.Errorf("mismatch error missing %q:\n%s", want, msg)
		}
	}
}

func TestResolveAWSConfig_NoActive_DefaultChain(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		envProfile:   "my-profile",
		probeResults: []probeResult{{accountID: "222222222222", callerARN: "arn:z"}},
	}
	_, accountID, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err != nil {
		t.Fatalf("resolveAWSConfig: %v", err)
	}
	if accountID != "222222222222" {
		t.Errorf("accountID=%q", accountID)
	}
	if len(rec.loadConfigCalls) != 1 {
		t.Fatalf("loadConfig calls=%d, want 1", len(rec.loadConfigCalls))
	}
	lc := rec.loadConfigCalls[0]
	if lc.profile != "my-profile" {
		t.Errorf("loadConfig profile=%q, want AWS_PROFILE value", lc.profile)
	}
	if lc.hasProvider {
		t.Error("first phase must use the default chain (nil provider)")
	}
	if len(rec.providerCalls) != 0 {
		t.Errorf("newLoginProvider must not be called when the default chain works, got %d calls", len(rec.providerCalls))
	}
}

func TestResolveAWSConfig_NoActive_FallbackToLoginSession(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		envProfile:     "my-profile",
		defaultSession: "arn:aws:signin:::user/default-example",
		probeResults: []probeResult{
			{err: errors.New("no credential providers")},          // default chain fails
			{accountID: "333333333333", callerARN: "arn:aws:..."}, // login_session fallback succeeds
		},
	}
	_, accountID, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err != nil {
		t.Fatalf("resolveAWSConfig: %v", err)
	}
	if accountID != "333333333333" {
		t.Errorf("accountID=%q", accountID)
	}
	if rec.defaultSessionCalls != 1 {
		t.Errorf("resolveLoginSession calls=%d, want 1", rec.defaultSessionCalls)
	}
	if len(rec.providerCalls) != 1 {
		t.Fatalf("newLoginProvider calls=%d, want 1", len(rec.providerCalls))
	}
	if got := rec.providerCalls[0]; got.loginSession != "arn:aws:signin:::user/default-example" {
		t.Errorf("fallback provider constructed with session %q", got.loginSession)
	}
	if len(rec.loadConfigCalls) != 2 {
		t.Fatalf("loadConfig calls=%d, want 2", len(rec.loadConfigCalls))
	}
	// AWS_PROFILE is still honoured for non-credential settings.
	if lc := rec.loadConfigCalls[1]; lc.profile != "my-profile" || !lc.hasProvider {
		t.Errorf("fallback loadConfig=(%q,provider=%v), want (my-profile,true)", lc.profile, lc.hasProvider)
	}
}

func TestResolveAWSConfig_NoActive_NoLoginSessionFound(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		defaultSession: "", // no login_session in ~/.aws/config
		probeResults: []probeResult{
			{err: errors.New("no credential providers")},
		},
	}
	_, _, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err == nil {
		t.Fatal("expected error when the default chain fails and no login_session exists")
	}
	if len(rec.providerCalls) != 0 {
		t.Errorf("no provider must be built without a login_session, got %d calls", len(rec.providerCalls))
	}
	for _, want := range []string{"login_session", "nakama aws login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%s", want, err)
		}
	}
}

func TestResolveAWSConfig_NoActive_BothPhasesFail(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		defaultSession: "arn:aws:signin:::user/default-example",
		probeResults: []probeResult{
			{err: errors.New("no credential providers")},
			{err: errors.New("login session also failed")},
		},
	}
	_, _, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err == nil {
		t.Fatal("expected error when both phases fail")
	}
	for _, want := range []string{"login_session", "nakama aws login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%s", want, err)
		}
	}
}

func TestResolveAWSConfig_StoreLoadErrorPropagates(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{loadStoreErr: errors.New("corrupt profiles.json")}
	_, _, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err == nil {
		t.Fatal("expected store load error to propagate")
	}
	if !strings.Contains(err.Error(), "corrupt profiles.json") {
		t.Errorf("unexpected error: %v", err)
	}
}
