package cli

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

// Tests of `code` and `kirocrew` over SSM, the default since Issue #118
// (ADR 0027). The CloudFront path (--internet) is covered by
// aws_open_test.go and aws_kirocrew_test.go.

func TestCodeSSM_AutoLoginThroughTheForward(t *testing.T) {
	backend := runningBackend()
	// Not allowlisted: over SSM the WAF does not matter and must not be
	// mentioned.
	backend.ip = "198.51.100.7"
	var pageBody string
	var fetchErr error
	launcher := &fakeLauncher{}
	launcher.onOpen = func(u string) { _, pageBody, fetchErr = httpGet(t, u) }
	deps := newTestOpenDeps(backend, launcher)

	out, errOut, err := runCodeCommand(t, deps, "dev01", "--timeout", "5s")
	if err != nil {
		t.Fatalf("open: %v (stderr: %s)", err, errOut)
	}
	if len(backend.forwardArgs) != 1 || backend.forwardArgs[0] != (service.BrowserForwardInput{Name: "dev01", Purpose: service.ForwardPurposeCodeServer}) {
		t.Fatalf("EnsureBrowserForward calls = %+v", backend.forwardArgs)
	}
	if fetchErr != nil {
		t.Fatalf("fetch one-shot page: %v", fetchErr)
	}
	// The page signs in at the router address, so the session cookie
	// belongs to this instance's own host name.
	for _, want := range []string{
		`action="http://code-server.dev01.nakama.localhost:18888/login?to=/"`,
		`name="password" value="s3cret-pw"`,
		`name="href" value="http://code-server.dev01.nakama.localhost:18888/login"`,
		`method="post"`,
	} {
		if !strings.Contains(pageBody, want) {
			t.Errorf("one-shot page missing %q:\n%s", want, pageBody)
		}
	}
	if urls := launcher.opened(); len(urls) != 1 || !strings.HasPrefix(urls[0], "http://127.0.0.1:") {
		t.Errorf("the browser must open the local one-shot page only, got %v", urls)
	}
	if want := "url        : http://code-server.dev01.nakama.localhost:18888\nvia        : ssm\nmethod     : auto-login\nauto_login : completed\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if !strings.Contains(errOut, "through the nakama router") || !strings.Contains(errOut, "nakama aws tunnel stop dev01") {
		t.Errorf("stderr must say how to stop the forward:\n%s", errOut)
	}
	if strings.Contains(errOut, "WAF") || strings.Contains(out, "cloudfront") {
		t.Errorf("the SSM path must not involve CloudFront or the WAF:\nstdout=%s\nstderr=%s", out, errOut)
	}
	if strings.Contains(out, "s3cret-pw") || strings.Contains(errOut, "s3cret-pw") {
		t.Error("the password must never appear in command output")
	}
}

func TestCodeSSM_LocalPort(t *testing.T) {
	backend := runningBackend()
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)
	out, errOut, err := runCodeCommand(t, deps, "dev01", "--local-port", "18080", "--no-auth")
	if err != nil {
		t.Fatalf("open: %v (stderr: %s)", err, errOut)
	}
	if backend.forwardArgs[0].LocalPort != 18080 {
		t.Errorf("EnsureBrowserForward local port = %d", backend.forwardArgs[0].LocalPort)
	}
	if urls := launcher.opened(); len(urls) != 1 || urls[0] != "http://localhost:18080" {
		t.Errorf("opened %v, want [http://localhost:18080]", urls)
	}
	if backend.showPwdArgs[0] {
		t.Error("--no-auth must not read the password")
	}
	if !strings.Contains(errOut, "forwarding localhost:18080 to code-server on dev01 over SSM") || !strings.Contains(out, "http://localhost:18080") {
		t.Errorf("stdout=%s\nstderr=%s", out, errOut)
	}
}

func TestCodeSSM_FlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--internet", "--local-port", "18080", "dev01"},
		{"--local-port", "0", "dev01"},
		{"--local-port", "70000", "dev01"},
	} {
		backend := runningBackend()
		_, _, err := runCodeCommand(t, newTestOpenDeps(backend, &fakeLauncher{}), args...)
		if err == nil {
			t.Errorf("open %v must fail", args)
		}
		if len(backend.forwardArgs) != 0 {
			t.Errorf("open %v must not start a forward", args)
		}
	}
}

// TestOpenSSM_NoBrowserIsAnError: over SSM the browser must open by
// itself; when it cannot, the command fails and does not print a URL
// for the user to open (Issue #118).
func TestCodeSSM_NoBrowserIsAnError(t *testing.T) {
	for _, args := range [][]string{{"dev01"}, {"dev01", "--no-auth"}} {
		backend := runningBackend()
		launcher := &fakeLauncher{err: errors.New("no browser found")}
		out, _, err := runCodeCommand(t, newTestOpenDeps(backend, launcher), args...)
		if err == nil || !strings.Contains(err.Error(), "could not launch a browser") || !strings.Contains(err.Error(), "--browser") {
			t.Errorf("open %v: err = %v, want the launch failure with the --browser advice", args, err)
		}
		if strings.Contains(out, "http://") {
			t.Errorf("open %v must not print a URL to open by hand: %q", args, out)
		}
	}
}

func TestCodeSSM_PortInUseAdvice(t *testing.T) {
	backend := runningBackend()
	backend.forwardErr = &service.LocalPortInUseError{Port: "18080", Holder: "dev02"}
	_, _, err := runCodeCommand(t, newTestOpenDeps(backend, &fakeLauncher{}), "dev01", "--local-port", "18080")
	if err == nil || !strings.Contains(err.Error(), "dev02") || !strings.Contains(err.Error(), "--local-port") {
		t.Fatalf("err = %v, want the holder and the --local-port advice", err)
	}
}

// TestOpenSSM_RouterPortInUse: a busy router port stops open with the
// NAKAMA_ROUTER_PORT advice, and nothing else is offered in its place
// (Issue #124 decision 10).
func TestCodeSSM_RouterPortInUse(t *testing.T) {
	backend := runningBackend()
	backend.forwardErr = &service.RouterPortInUseError{Port: 18888, Err: errors.New("address already in use")}
	launcher := &fakeLauncher{}
	_, _, err := runCodeCommand(t, newTestOpenDeps(backend, launcher), "dev01")
	if err == nil || !strings.Contains(err.Error(), "NAKAMA_ROUTER_PORT") || !strings.Contains(err.Error(), "18888") {
		t.Fatalf("err = %v, want the router port and the NAKAMA_ROUTER_PORT advice", err)
	}
	if len(launcher.opened()) != 0 {
		t.Error("no browser may be opened")
	}
}

func TestCodeSSM_Preflight(t *testing.T) {
	stopped := runningBackend()
	stopped.info.State = "stopped"
	if _, _, err := runCodeCommand(t, newTestOpenDeps(stopped, &fakeLauncher{}), "dev01"); err == nil || !strings.Contains(err.Error(), "nakama aws start dev01") {
		t.Errorf("stopped: err = %v", err)
	}
	bare := runningBackend()
	bare.info.Kind = "bare"
	if _, _, err := runCodeCommand(t, newTestOpenDeps(bare, &fakeLauncher{}), "dev01"); err == nil || !strings.Contains(err.Error(), "--bare") {
		t.Errorf("bare: err = %v", err)
	}
	// Over SSM no CloudFront distribution is needed.
	noCF := runningBackend()
	noCF.info.DistributionID, noCF.info.DistributionDomain, noCF.info.URL, noCF.info.CloudFront = "", "", "", false
	if _, _, err := runCodeCommand(t, newTestOpenDeps(noCF, &fakeLauncher{}), "dev01", "--no-auth"); err != nil {
		t.Errorf("an instance without a distribution must open over SSM: %v", err)
	}
	for _, b := range []*fakeOpenBackend{stopped, bare} {
		if len(b.forwardArgs) != 0 {
			t.Error("no forward may be started when the preflight fails")
		}
	}
}

func TestCodeSSM_JSONOutput(t *testing.T) {
	prev := global.Output
	global.Output = "json"
	t.Cleanup(func() { global.Output = prev })
	backend := runningBackend()
	launcher := &fakeLauncher{}
	launcher.onOpen = func(u string) { _, _, _ = httpGet(t, u) }
	out, _, err := runCodeCommand(t, newTestOpenDeps(backend, launcher), "dev01", "--timeout", "5s")
	if err != nil {
		t.Fatal(err)
	}
	var res openResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if res.Via != "ssm" || res.LocalPort != testForwardPort || res.URL != "http://code-server.dev01.nakama.localhost:18888" || res.AutoLogin != "completed" {
		t.Errorf("result = %+v", res)
	}
	if strings.Contains(out, "s3cret-pw") {
		t.Error("the JSON output must never carry the password")
	}
}

func TestKirocrewSSM_OpensTheDashboardWithItsToken(t *testing.T) {
	backend := enabledKirocrewBackend()
	// The SSM path needs no crew distribution.
	backend.webErr = errors.New("GetKiroCrewWeb must not be called over SSM")
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
	if len(backend.forwardArgs) != 1 || backend.forwardArgs[0].Purpose != service.ForwardPurposeKiroCrew || backend.forwardArgs[0].LocalPort != 0 {
		t.Errorf("EnsureBrowserForward calls = %+v", backend.forwardArgs)
	}
	for _, want := range []string{
		`action="http://kirocrew.dev01.nakama.localhost:18888/"`,
		`method="get"`,
		`name="token" value="` + testKCToken + `"`,
		"Opening Kiro Crew",
	} {
		if !strings.Contains(pageBody, want) {
			t.Errorf("one-shot page lacks %q:\n%s", want, pageBody)
		}
	}
	if strings.Contains(pageBody, "password") {
		t.Error("the SSM path signs in with the Kiro Crew token only")
	}
	if backend.pwdAsked != 0 {
		t.Error("the code-server password must not be read over SSM")
	}
	if urls := launcher.opened(); len(urls) != 1 || !strings.HasPrefix(urls[0], "http://127.0.0.1:") {
		t.Errorf("the browser must open the local one-shot page only (the token stays off its command line), got %v", urls)
	}
	if strings.Contains(out, testKCToken) || strings.Contains(errOut, testKCToken) {
		t.Error("the token must never appear in the command output")
	}
	if !strings.Contains(out, "via        : ssm") || !strings.Contains(out, "http://kirocrew.dev01.nakama.localhost:18888") {
		t.Errorf("stdout = %q", out)
	}
}

func TestKirocrewSSM_LocalPortAndNoAuth(t *testing.T) {
	backend := enabledKirocrewBackend()
	launcher := &fakeLauncher{}
	if _, _, err := runKirocrewCommand(t, backend, launcher, "dev01", "--local-port", "15999", "--no-auth"); err != nil {
		t.Fatal(err)
	}
	if backend.forwardArgs[0].LocalPort != 15999 {
		t.Errorf("local port = %d", backend.forwardArgs[0].LocalPort)
	}
	if urls := launcher.opened(); len(urls) != 1 || urls[0] != "http://localhost:15999/" {
		t.Errorf("opened %v", urls)
	}
	if backend.minted != 0 {
		t.Error("--no-auth must not mint a token")
	}
}

// TestKirocrewSSM_PageNotFetchedOpensTheTokenURL: when the browser does
// not load the one-shot page in time, the dashboard is opened with the
// token directly, still by nakama.
func TestKirocrewSSM_PageNotFetchedOpensTheTokenURL(t *testing.T) {
	backend := enabledKirocrewBackend()
	launcher := &fakeLauncher{} // never fetches the page
	_, errOut, err := runKirocrewCommand(t, backend, launcher, "dev01", "--timeout", "50ms")
	if err != nil {
		t.Fatalf("kirocrew: %v (stderr %s)", err, errOut)
	}
	urls := launcher.opened()
	want := "http://kirocrew.dev01.nakama.localhost:18888/?token=" + url.QueryEscape(testKCToken)
	if len(urls) != 2 || urls[1] != want {
		t.Errorf("opened %v, want the one-shot page, then %s", urls, want)
	}
}

func TestKirocrewSSM_NoBrowserIsAnError(t *testing.T) {
	backend := enabledKirocrewBackend()
	launcher := &fakeLauncher{err: errors.New("no browser found")}
	out, _, err := runKirocrewCommand(t, backend, launcher, "dev01")
	if err == nil || !strings.Contains(err.Error(), "--browser") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out, "http://") || strings.Contains(out, testKCToken) {
		t.Errorf("no URL may be printed for the user to open: %q", out)
	}
}

func TestKirocrewSSM_Preconditions(t *testing.T) {
	stopped := enabledKirocrewBackend()
	stopped.state = "stopped"
	if _, _, err := runKirocrewCommand(t, stopped, &fakeLauncher{}, "dev01"); err == nil || !strings.Contains(err.Error(), "nakama aws start dev01") {
		t.Errorf("stopped: err = %v", err)
	}
	if len(stopped.forwardArgs) != 0 || stopped.minted != 0 {
		t.Error("nothing may be started or minted for a stopped instance")
	}
	if _, _, err := runKirocrewCommand(t, enabledKirocrewBackend(), &fakeLauncher{}, "--internet", "--local-port", "1", "dev01"); err == nil {
		t.Error("--local-port with --internet must fail")
	}
}
