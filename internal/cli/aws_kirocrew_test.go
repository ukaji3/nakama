package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/browser"
	"github.com/ukaji3/nakama/internal/service"
)

const (
	testKCDomain = "d1234567abcdef.cloudfront.net"
	testKCToken  = "eyJzdWIiOiJsb2NhbC1hcHAifQ.7n8abmoz4xQ0RMKQBQJeUeQ9NgJN52tqnwC7EDwZLOI"
)

type fakeKirocrewBackend struct {
	web      *service.KiroCrewWeb
	webErr   error
	password string
	pwdErr   error
	tokenErr error
	minted   int
	enabled  []string
	disabled []string
	skipped  bool
	pwdAsked int
}

func (b *fakeKirocrewBackend) GetKiroCrewWeb(context.Context, string) (*service.KiroCrewWeb, error) {
	if b.webErr != nil {
		return nil, b.webErr
	}
	w := *b.web
	return &w, nil
}

func (b *fakeKirocrewBackend) GetInstanceInfo(_ context.Context, name string, showPassword bool) (*service.InstanceInfo, error) {
	if showPassword {
		b.pwdAsked++
	}
	if b.pwdErr != nil {
		return nil, b.pwdErr
	}
	return &service.InstanceInfo{Name: name, State: "running", Password: b.password}, nil
}

func (b *fakeKirocrewBackend) MintKiroCrewToken(context.Context, string) (string, error) {
	b.minted++
	if b.tokenErr != nil {
		return "", b.tokenErr
	}
	return testKCToken, nil
}

func (b *fakeKirocrewBackend) EnableKiroCrewWeb(_ context.Context, name string) (*service.EnableKiroCrewWebOutput, error) {
	b.enabled = append(b.enabled, name)
	return &service.EnableKiroCrewWebOutput{Name: name, DistributionID: "ECREW", URL: "https://" + testKCDomain}, nil
}

func (b *fakeKirocrewBackend) DisableKiroCrewWeb(_ context.Context, name string) (*service.DisableKiroCrewWebOutput, error) {
	b.disabled = append(b.disabled, name)
	return &service.DisableKiroCrewWebOutput{Name: name, InstanceSkipped: b.skipped}, nil
}

func enabledKirocrewBackend() *fakeKirocrewBackend {
	return &fakeKirocrewBackend{
		web: &service.KiroCrewWeb{
			Name: "dev01", InstanceID: "i-0123456789abcdef0", State: "running",
			DistributionID: "ECREW", Domain: testKCDomain, URL: "https://" + testKCDomain, Deployed: true,
		},
		password: "s3cret-pw",
	}
}

func runKirocrewCommand(t *testing.T, backend kirocrewBackend, launcher browser.Launcher, args ...string) (string, string, error) {
	t.Helper()
	deps := kirocrewDeps{
		connect: func(context.Context) (kirocrewBackend, error) { return backend, nil },
		open:    newTestOpenDeps(runningBackend(), launcher),
	}
	cmd := newAWSKirocrewCommandWithDeps(deps)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestKirocrew_AutoLogin pins what reaches the browser: a one-shot page
// on 127.0.0.1 whose form signs in to code-server on the crew
// distribution and carries the Kiro Crew token through the post-login
// redirect (?to=/&token=…).
func TestKirocrew_AutoLogin(t *testing.T) {
	backend := enabledKirocrewBackend()
	var pageBody string
	var fetchErr error
	launcher := &fakeLauncher{}
	launcher.onOpen = func(u string) { _, pageBody, fetchErr = httpGet(t, u) }

	out, errOut, err := runKirocrewCommand(t, backend, launcher, "dev01", "--timeout", "5s")
	if err != nil {
		t.Fatalf("kirocrew: %v (stderr %s)", err, errOut)
	}
	if fetchErr != nil {
		t.Fatalf("fetch one-shot page: %v", fetchErr)
	}
	if urls := launcher.opened(); len(urls) != 1 || !strings.HasPrefix(urls[0], "http://127.0.0.1:") {
		t.Fatalf("the browser must open the local one-shot page only, got %v", urls)
	}
	for _, want := range []string{
		// html/template escapes & inside the attribute.
		`action="https://` + testKCDomain + `/login?to=/&amp;token=` + url.QueryEscape(testKCToken) + `"`,
		`name="password" value="s3cret-pw"`,
		`name="href" value="https://` + testKCDomain + `/login"`,
		`name="base" value="."`,
		"Signing in to Kiro Crew",
	} {
		if !strings.Contains(pageBody, want) {
			t.Errorf("one-shot page lacks %q:\n%s", want, pageBody)
		}
	}
	for _, secret := range []string{"s3cret-pw", testKCToken} {
		if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
			t.Errorf("%q must never appear in the command output", secret)
		}
	}
	if !strings.Contains(out, "auto-login") || !strings.Contains(out, "completed") {
		t.Errorf("stdout should report a completed auto-login:\n%s", out)
	}
	if backend.minted != 1 {
		t.Errorf("exactly one token must be minted, got %d", backend.minted)
	}
}

// TestKirocrew_NoAuthOpensTokenURL: without auto-login the dashboard
// URL (with its token) is opened directly and code-server shows its own
// login page first. The password is never fetched.
func TestKirocrew_NoAuthOpensTokenURL(t *testing.T) {
	backend := enabledKirocrewBackend()
	launcher := &fakeLauncher{}
	_, _, err := runKirocrewCommand(t, backend, launcher, "dev01", "--no-auth")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://" + testKCDomain + "/?token=" + url.QueryEscape(testKCToken)
	if urls := launcher.opened(); len(urls) != 1 || urls[0] != want {
		t.Errorf("opened %v, want [%s]", urls, want)
	}
	if backend.pwdAsked != 0 {
		t.Error("--no-auth must not read the code-server password")
	}
}

func TestKirocrew_PrintURLWarnsAboutTheToken(t *testing.T) {
	backend := enabledKirocrewBackend()
	launcher := &fakeLauncher{}
	out, errOut, err := runKirocrewCommand(t, backend, launcher, "dev01", "--no-auth", "--print-url")
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.opened()) != 0 {
		t.Error("--print-url must not launch a browser")
	}
	if !strings.Contains(out, "?token=") || !strings.Contains(errOut, "5 minutes") {
		t.Errorf("--print-url must print the token URL and warn about its lifetime:\nstdout=%s\nstderr=%s", out, errOut)
	}
}

func TestKirocrew_PasswordFailureFallsBackToPlain(t *testing.T) {
	backend := enabledKirocrewBackend()
	backend.pwdErr = fmt.Errorf("AccessDenied")
	launcher := &fakeLauncher{}
	_, errOut, err := runKirocrewCommand(t, backend, launcher, "dev01")
	if err != nil {
		t.Fatalf("a password failure must degrade, not fail: %v", err)
	}
	if !strings.Contains(errOut, "auto-login skipped") {
		t.Errorf("the fallback must be explained:\n%s", errOut)
	}
	if urls := launcher.opened(); len(urls) != 1 || !strings.Contains(urls[0], testKCDomain+"/?token=") {
		t.Errorf("the plain dashboard URL must open, got %v", urls)
	}
}

func TestKirocrew_Preconditions(t *testing.T) {
	notEnabled := &fakeKirocrewBackend{webErr: fmt.Errorf("%w: run `nakama aws kirocrew enable dev01` first", service.ErrKiroCrewWebNotEnabled)}
	if _, _, err := runKirocrewCommand(t, notEnabled, &fakeLauncher{}, "dev01"); err == nil || !strings.Contains(err.Error(), "kirocrew enable dev01") {
		t.Errorf("not enabled: err = %v", err)
	}
	if notEnabled.minted != 0 {
		t.Error("no token may be minted before the checks pass")
	}

	stopped := enabledKirocrewBackend()
	stopped.web.State = "stopped"
	if _, _, err := runKirocrewCommand(t, stopped, &fakeLauncher{}, "dev01"); err == nil || !strings.Contains(err.Error(), "nakama aws start dev01") {
		t.Errorf("stopped: err = %v", err)
	}
	if stopped.minted != 0 {
		t.Error("no token may be minted for a stopped instance")
	}

	deploying := enabledKirocrewBackend()
	deploying.web.Deployed = false
	_, errOut, err := runKirocrewCommand(t, deploying, &fakeLauncher{}, "dev01", "--no-auth")
	if err != nil || !strings.Contains(errOut, "still deploying") {
		t.Errorf("a deploying distribution must warn but proceed: err=%v stderr=%s", err, errOut)
	}
}

func TestKirocrew_EnableAndDisableSubcommands(t *testing.T) {
	backend := enabledKirocrewBackend()
	out, _, err := runKirocrewCommand(t, backend, &fakeLauncher{}, "enable", "dev01")
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.enabled) != 1 || backend.enabled[0] != "dev01" || !strings.Contains(out, "nakama aws kirocrew dev01") {
		t.Errorf("enable: calls=%v out=%s", backend.enabled, out)
	}

	backend.skipped = true
	out, _, err = runKirocrewCommand(t, backend, &fakeLauncher{}, "disable", "dev01")
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.disabled) != 1 || !strings.Contains(out, "not running") {
		t.Errorf("disable: calls=%v out=%s", backend.disabled, out)
	}
	if backend.minted != 0 {
		t.Error("enable/disable must not mint tokens")
	}
}

func TestKirocrew_ReservedSubcommandNames(t *testing.T) {
	for _, name := range []string{"enable", "disable"} {
		if err := service.ValidateNewInstanceName(name); err == nil {
			t.Errorf("%q must be reserved: it is a kirocrew subcommand", name)
		}
	}
}

func TestFormatInfoShowsKiroCrewURLWhenEnabled(t *testing.T) {
	info := &service.InstanceInfo{Name: "dev01", URL: "https://dcs.cloudfront.net"}
	if strings.Contains(formatInfo(info, false), "kirocrew_url") {
		t.Error("kirocrew_url must be omitted when Kiro Crew web access is not enabled")
	}
	info.KiroCrewURL = "https://" + testKCDomain
	if !strings.Contains(formatInfo(info, false), "kirocrew_url      : https://"+testKCDomain) {
		t.Errorf("kirocrew_url missing:\n%s", formatInfo(info, false))
	}
}
