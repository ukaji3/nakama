package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/profilestore"
	"github.com/ukaji3/nakama/internal/signin"
)

const testAuthURL = "https://ap-northeast-1.signin.aws.amazon.com/v1/authorize?x=y"

// newTestLoginDeps returns loginDeps with benign fakes rooted in a
// temp store. The fake login invokes OnAuthURL exactly like the real
// flow so the URL-display behavior is exercised.
func newTestLoginDeps(t *testing.T) (loginDeps, *profilestore.Store, *[]string) {
	t.Helper()
	store := &profilestore.Store{BaseDir: filepath.Join(t.TempDir(), "nakama")}
	browserCalls := &[]string{}
	deps := loginDeps{
		login: func(_ context.Context, opts signin.LoginOptions) (*signin.LoginResult, error) {
			opts.OnAuthURL(testAuthURL)
			return &signin.LoginResult{
				LoginSession: "arn:aws:sts::111111111111:assumed-role/Admin/user",
				AccountID:    "111111111111",
			}, nil
		},
		identity: func(context.Context, string) (string, string, error) {
			return "111111111111", "arn:aws:sts::111111111111:assumed-role/Admin/user", nil
		},
		openBrowser: func(_ context.Context, url string) error {
			*browserCalls = append(*browserCalls, url)
			return nil
		},
		newStore: func() *profilestore.Store { return store },
		now: func() time.Time {
			return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
		},
	}
	return deps, store, browserCalls
}

func runLoginCommand(t *testing.T, deps loginDeps, args ...string) (string, string, error) {
	t.Helper()
	cmd := newAWSLoginCommandWithDeps(deps)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestLogin_SavesAndActivatesProfile(t *testing.T) {
	deps, store, browserCalls := newTestLoginDeps(t)
	out, errOut, err := runLoginCommand(t, deps)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	data, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(data.Profiles) != 1 {
		t.Fatalf("len(Profiles)=%d, want 1", len(data.Profiles))
	}
	p := data.Profiles[0]
	if p.AccountID != "111111111111" || p.Alias != "111111111111" {
		t.Errorf("profile=(%q,%q)", p.AccountID, p.Alias)
	}
	if p.LoginSession != "arn:aws:sts::111111111111:assumed-role/Admin/user" {
		t.Errorf("LoginSession=%q", p.LoginSession)
	}
	if data.Active != "111111111111" {
		t.Errorf("Active=%q, want the fresh account (auto-load)", data.Active)
	}

	if !strings.Contains(errOut, testAuthURL) {
		t.Errorf("the authorization URL must be displayed:\n%s", errOut)
	}
	if len(*browserCalls) != 1 || (*browserCalls)[0] != testAuthURL {
		t.Errorf("browser calls=%v", *browserCalls)
	}
	for _, want := range []string{"111111111111", "arn:aws:sts::111111111111:assumed-role/Admin/user"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestLogin_NameFlagSetsAlias(t *testing.T) {
	deps, store, _ := newTestLoginDeps(t)
	if _, _, err := runLoginCommand(t, deps, "--name", "dev"); err != nil {
		t.Fatalf("login --name: %v", err)
	}
	data, _ := store.Load()
	if len(data.Profiles) != 1 || data.Profiles[0].Alias != "dev" {
		t.Errorf("alias not applied: %+v", data.Profiles)
	}
}

func TestLogin_RemoteSkipsBrowser(t *testing.T) {
	deps, _, browserCalls := newTestLoginDeps(t)
	deps.login = func(_ context.Context, opts signin.LoginOptions) (*signin.LoginResult, error) {
		if !opts.Remote {
			t.Error("Remote flag not propagated to signin.LoginOptions")
		}
		opts.OnAuthURL(testAuthURL)
		return &signin.LoginResult{
			LoginSession: "arn:aws:sts::111111111111:assumed-role/Admin/user",
			AccountID:    "111111111111",
		}, nil
	}
	_, errOut, err := runLoginCommand(t, deps, "--remote")
	if err != nil {
		t.Fatalf("login --remote: %v", err)
	}
	if len(*browserCalls) != 0 {
		t.Errorf("no browser must be launched in remote mode, got %v", *browserCalls)
	}
	if !strings.Contains(errOut, testAuthURL) {
		t.Errorf("the authorization URL must still be displayed:\n%s", errOut)
	}
}

func TestLogin_BrowserFailureDegradesToURLOnly(t *testing.T) {
	deps, store, _ := newTestLoginDeps(t)
	deps.openBrowser = func(context.Context, string) error {
		return errors.New("no launcher available")
	}
	_, errOut, err := runLoginCommand(t, deps)
	if err != nil {
		t.Fatalf("login must succeed even when no browser can be launched: %v", err)
	}
	if !strings.Contains(errOut, testAuthURL) {
		t.Errorf("URL missing from output:\n%s", errOut)
	}
	if !strings.Contains(errOut, "open the URL manually") {
		t.Errorf("expected manual-open guidance:\n%s", errOut)
	}
	if data, _ := store.Load(); data.Active != "111111111111" {
		t.Errorf("Active=%q", data.Active)
	}
}

func TestLogin_UpsertsExistingAccountAndActivates(t *testing.T) {
	deps, store, _ := newTestLoginDeps(t)
	seedProfiles(t, store, "222222222222",
		prof("111111111111", "old-alias"),
		prof("222222222222", "other"),
	)
	if _, _, err := runLoginCommand(t, deps, "--name", "fresh"); err != nil {
		t.Fatalf("login: %v", err)
	}
	data, _ := store.Load()
	if len(data.Profiles) != 2 {
		t.Fatalf("len(Profiles)=%d, want 2 (upsert)", len(data.Profiles))
	}
	p, ok := data.FindByAccount("111111111111")
	if !ok || p.Alias != "fresh" {
		t.Errorf("profile not upserted: %+v", data.Profiles)
	}
	if data.Active != "111111111111" {
		t.Errorf("Active=%q, want the fresh login", data.Active)
	}
}

func TestLogin_AliasCollisionRejected(t *testing.T) {
	deps, store, _ := newTestLoginDeps(t)
	seedProfiles(t, store, "", prof("222222222222", "dev"))
	_, _, err := runLoginCommand(t, deps, "--name", "dev")
	if err == nil {
		t.Fatal("expected alias collision error")
	}
	if !strings.Contains(err.Error(), "222222222222") {
		t.Errorf("collision error should name the conflicting account: %v", err)
	}
	if data, _ := store.Load(); data.Active != "" {
		t.Errorf("active must stay unchanged on failure, got %q", data.Active)
	}
}

func TestLogin_LoginFailurePropagates(t *testing.T) {
	deps, store, _ := newTestLoginDeps(t)
	deps.login = func(context.Context, signin.LoginOptions) (*signin.LoginResult, error) {
		return nil, errors.New("sign-in timed out")
	}
	_, _, err := runLoginCommand(t, deps)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected login failure, got %v", err)
	}
	if data, _ := store.Load(); len(data.Profiles) != 0 {
		t.Errorf("nothing must be saved on failure: %+v", data.Profiles)
	}
}

func TestLogin_IdentityFailurePropagates(t *testing.T) {
	deps, store, _ := newTestLoginDeps(t)
	deps.identity = func(context.Context, string) (string, string, error) {
		return "", "", errors.New("sts unavailable")
	}
	_, _, err := runLoginCommand(t, deps)
	if err == nil || !strings.Contains(err.Error(), "STS") {
		t.Errorf("expected STS confirmation failure, got %v", err)
	}
	if data, _ := store.Load(); len(data.Profiles) != 0 {
		t.Errorf("nothing must be saved on failure: %+v", data.Profiles)
	}
}
