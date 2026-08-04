package awsx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/ukaji3/nakama/internal/profilestore"
)

// fakeProvider is a stub aws.CredentialsProvider that records how it
// was constructed by the newCLIProvider hook.
type fakeProvider struct {
	profileName string
	configFile  string
}

func (f *fakeProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "k", SecretAccessKey: "s"}, nil
}

// resolveRecorder builds resolverHooks whose collaborators are all
// fakes, recording every interaction for assertions.
type resolveRecorder struct {
	activeProfile *profilestore.Profile
	awsConfigPath string
	loadStoreErr  error

	envProfile string

	// probe script: consumed one call at a time.
	probeResults []probeResult

	loadConfigCalls []loadConfigCall
	providerCalls   []fakeProvider
	probeCalls      int
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
		loadActiveProfile: func() (*profilestore.Profile, string, error) {
			return r.activeProfile, r.awsConfigPath, r.loadStoreErr
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
		newCLIProvider: func(profileName, configFile string) aws.CredentialsProvider {
			p := &fakeProvider{profileName: profileName, configFile: configFile}
			r.providerCalls = append(r.providerCalls, *p)
			return p
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
		awsConfigPath: "/fake/nakama/aws_config",
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
		t.Fatalf("newCLIProvider calls=%d, want 1", len(rec.providerCalls))
	}
	if got := rec.providerCalls[0]; got.profileName != "nakama-111111111111" || got.configFile != "/fake/nakama/aws_config" {
		t.Errorf("provider constructed with (%q,%q)", got.profileName, got.configFile)
	}
	if len(rec.loadConfigCalls) != 1 {
		t.Fatalf("loadConfig calls=%d, want 1", len(rec.loadConfigCalls))
	}
	lc := rec.loadConfigCalls[0]
	if lc.profile != "" {
		t.Errorf("loadConfig profile=%q; active profile must take precedence over AWS_PROFILE", lc.profile)
	}
	if !lc.hasProvider {
		t.Error("loadConfig must receive the nakama CLI provider")
	}
}

func TestResolveAWSConfig_ActiveProfileProbeFailure_FailClosed(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		activeProfile: activeFixture(),
		awsConfigPath: "/fake/nakama/aws_config",
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
		"aws login",                 // recovery step 1
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
		awsConfigPath: "/fake/nakama/aws_config",
		probeResults:  []probeResult{{accountID: "999999999999", callerARN: "arn:y"}},
	}
	_, _, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err == nil {
		t.Fatal("expected account mismatch error")
	}
	msg := err.Error()
	for _, want := range []string{"111111111111", "999999999999", "nakama aws profile unload"} {
		if !strings.Contains(msg, want) {
			t.Errorf("mismatch error missing %q:\n%s", want, msg)
		}
	}
}

func TestResolveAWSConfig_NoActive_LegacyDefaultChain(t *testing.T) {
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
		t.Error("legacy first phase must use the default chain (nil provider)")
	}
	if len(rec.providerCalls) != 0 {
		t.Errorf("newCLIProvider must not be called when the default chain works, got %d calls", len(rec.providerCalls))
	}
}

func TestResolveAWSConfig_NoActive_LegacyFallbackToCLI(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		envProfile: "my-profile",
		probeResults: []probeResult{
			{err: errors.New("no credential providers")},          // default chain fails
			{accountID: "333333333333", callerARN: "arn:aws:..."}, // CLI fallback succeeds
		},
	}
	_, accountID, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err != nil {
		t.Fatalf("resolveAWSConfig: %v", err)
	}
	if accountID != "333333333333" {
		t.Errorf("accountID=%q", accountID)
	}
	if len(rec.providerCalls) != 1 {
		t.Fatalf("newCLIProvider calls=%d, want 1", len(rec.providerCalls))
	}
	// The legacy fallback must NOT point at nakama's config file.
	if got := rec.providerCalls[0]; got.profileName != "my-profile" || got.configFile != "" {
		t.Errorf("fallback provider constructed with (%q,%q), want (my-profile, \"\")", got.profileName, got.configFile)
	}
}

func TestResolveAWSConfig_NoActive_BothPhasesFail(t *testing.T) {
	t.Parallel()
	rec := &resolveRecorder{
		probeResults: []probeResult{
			{err: errors.New("no credential providers")},
			{err: errors.New("aws CLI also failed")},
		},
	}
	_, _, _, err := resolveAWSConfig(context.Background(), rec.hooks())
	if err == nil {
		t.Fatal("expected error when both phases fail")
	}
	if !strings.Contains(err.Error(), "export-credentials") {
		t.Errorf("error should mention the export-credentials fallback:\n%s", err)
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
