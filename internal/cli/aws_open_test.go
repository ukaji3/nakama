package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/browser"
	"github.com/ukaji3/nakama/internal/service"
)

// fakeOpenBackend implements openBackend without AWS. It records the
// showPassword argument of every GetInstanceInfo call so tests can
// assert whether the password was requested.
type fakeOpenBackend struct {
	mu          sync.Mutex
	info        service.InstanceInfo
	password    string
	infoErr     error // returned from every GetInstanceInfo call
	passwordErr error // returned only when showPassword=true
	acl         []service.ACLEntry
	aclErr      error
	ip          string
	ipErr       error
	showPwdArgs []bool
}

func (b *fakeOpenBackend) GetInstanceInfo(_ context.Context, _ string, showPassword bool) (*service.InstanceInfo, error) {
	b.mu.Lock()
	b.showPwdArgs = append(b.showPwdArgs, showPassword)
	b.mu.Unlock()
	if b.infoErr != nil {
		return nil, b.infoErr
	}
	if showPassword && b.passwordErr != nil {
		return nil, b.passwordErr
	}
	info := b.info // copy
	if showPassword {
		info.Password = b.password
	}
	return &info, nil
}

func (b *fakeOpenBackend) ListACL(context.Context) ([]service.ACLEntry, error) {
	return b.acl, b.aclErr
}

func (b *fakeOpenBackend) PublicIPv4(context.Context) (string, error) {
	return b.ip, b.ipErr
}

// fakeLauncher implements browser.Launcher. onOpen (when set) runs
// synchronously with the URL, e.g. to fetch the one-shot login page
// like a real browser would.
type fakeLauncher struct {
	mu     sync.Mutex
	urls   []string
	err    error
	onOpen func(url string)
}

func (l *fakeLauncher) Open(_ context.Context, url string) error {
	l.mu.Lock()
	l.urls = append(l.urls, url)
	l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if l.onOpen != nil {
		l.onOpen(url)
	}
	return nil
}

func (l *fakeLauncher) opened() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.urls...)
}

// runningBackend returns a backend for a healthy running instance
// whose caller IP is allowlisted (no WAF warning noise in tests that
// exercise other paths).
func runningBackend() *fakeOpenBackend {
	return &fakeOpenBackend{
		info: service.InstanceInfo{
			Name:               "dev01",
			InstanceID:         "i-0123456789abcdef0",
			State:              "running",
			DistributionDomain: "d111abcdef.cloudfront.net",
			URL:                "https://d111abcdef.cloudfront.net",
		},
		password: "s3cret-pw",
		acl:      []service.ACLEntry{{CIDR: "203.0.113.10/32"}},
		ip:       "203.0.113.10",
	}
}

func newTestOpenDeps(backend openBackend, launcher browser.Launcher) openDeps {
	return openDeps{
		connect:     func(context.Context) (openBackend, error) { return backend, nil },
		newLauncher: func(string) browser.Launcher { return launcher },
		listen:      func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		randToken:   newLoginPageToken,
	}
}

// runOpenCommand executes `nakama aws open <args...>` against the
// given deps and returns stdout, stderr, and the execution error.
func runOpenCommand(t *testing.T, deps openDeps, args ...string) (string, string, error) {
	t.Helper()
	cmd := newAWSOpenCommandWithDeps(deps)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// httpGet fetches url with a short-timeout client and returns status
// code + body.
func httpGet(t *testing.T, url string) (int, string, error) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(body), nil
}

// TestOpen_EmptyPasswordFallsBack covers a password parameter that
// exists but is empty: METHOD A would POST a missing password and
// code-server would answer with its login page, which nakama cannot
// observe, so the command must degrade to METHOD C instead of claiming
// a completed auto-login.
func TestOpen_EmptyPasswordFallsBack(t *testing.T) {
	backend := runningBackend()
	backend.password = ""
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)

	out, errOut, err := runOpenCommand(t, deps, "dev01", "--timeout", "5s")
	if err != nil {
		t.Fatalf("open: %v (stderr: %s)", err, errOut)
	}
	if !strings.Contains(errOut, "empty") {
		t.Errorf("stderr does not explain the empty password:\n%s", errOut)
	}
	if !strings.Contains(errOut, "--show-password") {
		t.Errorf("stderr does not point at the manual password path:\n%s", errOut)
	}
	urls := launcher.opened()
	if len(urls) != 1 || urls[0] != backend.info.URL {
		t.Errorf("opened %v, want exactly the plain URL %q", urls, backend.info.URL)
	}
	if strings.Contains(out, "auto-login") {
		t.Errorf("stdout claims auto-login despite the empty password:\n%s", out)
	}
}

func TestOpen_MethodA_HappyPath(t *testing.T) {
	backend := runningBackend()
	var pageStatus int
	var pageBody string
	var fetchErr error
	launcher := &fakeLauncher{}
	launcher.onOpen = func(url string) {
		// Simulate the browser: fetch the one-shot page synchronously.
		pageStatus, pageBody, fetchErr = httpGet(t, url)
	}
	deps := newTestOpenDeps(backend, launcher)

	out, errOut, err := runOpenCommand(t, deps, "dev01", "--timeout", "5s")
	if err != nil {
		t.Fatalf("open: %v (stderr: %s)", err, errOut)
	}

	urls := launcher.opened()
	if len(urls) != 1 {
		t.Fatalf("launcher invocations=%v, want exactly 1 (the local one-shot URL)", urls)
	}
	loginURL := urls[0]
	if !strings.HasPrefix(loginURL, "http://127.0.0.1:") {
		t.Errorf("launched URL must be the local one-shot server, got %q", loginURL)
	}
	// Path = "/" + 22-char base64url token (128 bits).
	slash := strings.LastIndex(loginURL, "/")
	if token := loginURL[slash+1:]; len(token) < 22 {
		t.Errorf("one-shot path token too short (%d chars): %q", len(token), token)
	}

	if fetchErr != nil {
		t.Fatalf("fetch one-shot page: %v", fetchErr)
	}
	if pageStatus != http.StatusOK {
		t.Fatalf("one-shot page status=%d, want 200", pageStatus)
	}
	for _, want := range []string{
		`action="https://d111abcdef.cloudfront.net/login?to=/"`,
		`name="password" value="s3cret-pw"`,
		`name="base" value="."`,
		`name="href" value="https://d111abcdef.cloudfront.net/login"`,
		`method="post"`,
	} {
		if !strings.Contains(pageBody, want) {
			t.Errorf("one-shot page missing %q:\n%s", want, pageBody)
		}
	}

	// The server must be gone after the command completes: a second
	// fetch must NOT return the page.
	if status, body, err := httpGet(t, loginURL); err == nil {
		if status == http.StatusOK && strings.Contains(body, "s3cret-pw") {
			t.Errorf("second fetch returned the login page again (status %d)", status)
		}
	}

	if !strings.Contains(out, "auto-login") {
		t.Errorf("stdout should report auto-login:\n%s", out)
	}
	if strings.Contains(out, "s3cret-pw") || strings.Contains(errOut, "s3cret-pw") {
		t.Error("the password must never appear in command output")
	}
}

func TestOneShotServer_EscapesHTMLAndServesOnce(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	token, err := newLoginPageToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	srv, err := newOneShotLoginServer(lis, token, "d111abcdef.cloudfront.net", `a"<&b`)
	if err != nil {
		t.Fatalf("newOneShotLoginServer: %v", err)
	}
	defer srv.Close()

	// Wrong path -> 404, and it must NOT consume the one-shot page.
	if status, _, err := httpGet(t, "http://"+lis.Addr().String()+"/wrong"); err != nil || status != http.StatusNotFound {
		t.Errorf("wrong path: status=%d err=%v, want 404", status, err)
	}

	status, body, err := httpGet(t, srv.URL())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	// html/template attribute escaping: " -> &#34;, < -> &lt;, & -> &amp;
	if !strings.Contains(body, `value="a&#34;&lt;&amp;b"`) {
		t.Errorf("password not escaped in attribute:\n%s", body)
	}
	if strings.Contains(body, `value="a"<&b"`) {
		t.Errorf("raw password broke out of the attribute:\n%s", body)
	}
	// Security headers.
	resp, err := http.Get(srv.URL()) // second fetch; also checks 410
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("second fetch status=%d, want 410 Gone (single use)", resp.StatusCode)
	}

	select {
	case <-srv.Served():
	default:
		t.Error("Served channel must be closed after first delivery")
	}
}

func TestOneShotServer_SecurityHeaders(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	token, _ := newLoginPageToken()
	srv, err := newOneShotLoginServer(lis, token, "d.example", "pw")
	if err != nil {
		t.Fatalf("newOneShotLoginServer: %v", err)
	}
	defer srv.Close()

	resp, err := http.Get(srv.URL())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control=%q, want no-store", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy=%q, want no-referrer", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<meta name="referrer" content="no-referrer">`) {
		t.Errorf("page missing referrer meta tag:\n%s", body)
	}
}

func TestOpen_NoAuth_NeverFetchesPassword(t *testing.T) {
	backend := runningBackend()
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)

	_, errOut, err := runOpenCommand(t, deps, "dev01", "--no-auth")
	if err != nil {
		t.Fatalf("open --no-auth: %v (stderr: %s)", err, errOut)
	}
	for _, sp := range backend.showPwdArgs {
		if sp {
			t.Error("--no-auth must never request the password (showPassword=true seen)")
		}
	}
	urls := launcher.opened()
	if len(urls) != 1 || urls[0] != "https://d111abcdef.cloudfront.net" {
		t.Errorf("opened URLs=%v, want exactly the plain distribution URL", urls)
	}
}

func TestOpen_PrintURL_NoLauncherInvocation(t *testing.T) {
	backend := runningBackend()
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)

	// Short timeout: nobody fetches the one-shot page in this test, so
	// the command must fall back and finish quickly instead of hanging.
	out, _, err := runOpenCommand(t, deps, "dev01", "--print-url", "--timeout", "100ms")
	if err != nil {
		t.Fatalf("open --print-url: %v", err)
	}
	if len(launcher.opened()) != 0 {
		t.Errorf("--print-url must not launch a browser, got %v", launcher.opened())
	}
	if !strings.Contains(out, "https://d111abcdef.cloudfront.net") {
		t.Errorf("stdout must contain the code-server URL:\n%s", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:") {
		t.Errorf("stdout must contain the local one-shot login URL:\n%s", out)
	}
	if strings.Contains(out, "s3cret-pw") {
		t.Error("the password must never be printed")
	}
}

func TestOpen_FallbackToPlain_WhenLauncherMissing(t *testing.T) {
	backend := runningBackend()
	launcher := &fakeLauncher{err: fmt.Errorf("browser launcher %q: %w", "xdg-open", exec.ErrNotFound)}
	deps := newTestOpenDeps(backend, launcher)

	out, errOut, err := runOpenCommand(t, deps, "dev01", "--timeout", "1s")
	if err != nil {
		t.Fatalf("open must succeed when only the launcher is missing: %v", err)
	}
	if !strings.Contains(out, "https://d111abcdef.cloudfront.net") {
		t.Errorf("stdout must contain the plain URL for manual opening:\n%s", out)
	}
	if !strings.Contains(errOut, "auto-login skipped") {
		t.Errorf("stderr must explain that auto-login was skipped:\n%s", errOut)
	}
	if !strings.Contains(errOut, "nakama aws info dev01 --show-password") {
		t.Errorf("stderr must tell how to get the password:\n%s", errOut)
	}
}

func TestOpen_FallbackToPlain_WhenPasswordFetchFails(t *testing.T) {
	backend := runningBackend()
	backend.passwordErr = errors.New("read password: AccessDeniedException")
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)

	_, errOut, err := runOpenCommand(t, deps, "dev01")
	if err != nil {
		t.Fatalf("open must fall back when the password fetch fails: %v", err)
	}
	urls := launcher.opened()
	if len(urls) != 1 || urls[0] != "https://d111abcdef.cloudfront.net" {
		t.Errorf("opened URLs=%v, want the plain distribution URL", urls)
	}
	if !strings.Contains(errOut, "auto-login skipped") ||
		!strings.Contains(errOut, "password") {
		t.Errorf("stderr must explain the password-fetch failure:\n%s", errOut)
	}
	if !strings.Contains(errOut, "nakama aws info dev01 --show-password") {
		t.Errorf("stderr must tell how to get the password:\n%s", errOut)
	}
}

func TestOpen_Preflight_NotRunning(t *testing.T) {
	backend := runningBackend()
	backend.info.State = "stopped"
	deps := newTestOpenDeps(backend, &fakeLauncher{})

	_, _, err := runOpenCommand(t, deps, "dev01")
	if err == nil {
		t.Fatal("expected error for a stopped instance")
	}
	if !strings.Contains(err.Error(), "nakama aws start dev01") {
		t.Errorf("error must suggest `nakama aws start`: %v", err)
	}
}

func TestOpen_Preflight_NoDistributionDomain(t *testing.T) {
	backend := runningBackend()
	backend.info.DistributionDomain = ""
	backend.info.URL = ""
	deps := newTestOpenDeps(backend, &fakeLauncher{})

	_, _, err := runOpenCommand(t, deps, "dev01")
	if err == nil {
		t.Fatal("expected error when the distribution domain is missing")
	}
	if !strings.Contains(err.Error(), "distribution") {
		t.Errorf("error should mention the distribution: %v", err)
	}
}

func TestOpen_Preflight_InstanceNotFound(t *testing.T) {
	backend := runningBackend()
	backend.infoErr = errors.New(`nakama instance "dev01" not found`)
	deps := newTestOpenDeps(backend, &fakeLauncher{})

	_, _, err := runOpenCommand(t, deps, "dev01")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected the not-found error to propagate, got: %v", err)
	}
}

func TestOpen_WAFWarning_IPNotAllowlisted(t *testing.T) {
	backend := runningBackend()
	backend.ip = "198.51.100.7" // not covered by 203.0.113.10/32
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)

	_, errOut, err := runOpenCommand(t, deps, "dev01", "--no-auth")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(errOut, "nakama aws acl add") {
		t.Errorf("stderr must warn about the missing allowlist entry:\n%s", errOut)
	}
}

func TestOpen_WAFWarning_CIDRContainmentNotStringCompare(t *testing.T) {
	backend := runningBackend()
	backend.ip = "203.0.113.77"
	backend.acl = []service.ACLEntry{{CIDR: "203.0.113.0/24"}} // covers the IP
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)

	_, errOut, err := runOpenCommand(t, deps, "dev01", "--no-auth")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if strings.Contains(errOut, "acl add") {
		t.Errorf("IP inside a /24 entry must not warn (CIDR containment):\n%s", errOut)
	}
}

func TestOpen_WAFWarning_ListACLErrorIsNonFatal(t *testing.T) {
	backend := runningBackend()
	backend.aclErr = errors.New("ListIPSets: throttled")
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)

	_, errOut, err := runOpenCommand(t, deps, "dev01", "--no-auth")
	if err != nil {
		t.Fatalf("ListACL failure must not break open: %v", err)
	}
	if strings.Contains(errOut, "acl add") {
		t.Errorf("no warning must be printed when the check itself failed:\n%s", errOut)
	}
}

func TestOpen_Timeout_FallsBackWithoutHanging(t *testing.T) {
	backend := runningBackend()
	launcher := &fakeLauncher{} // captures URLs but never fetches the page
	deps := newTestOpenDeps(backend, launcher)

	start := time.Now()
	_, errOut, err := runOpenCommand(t, deps, "dev01", "--timeout", "100ms")
	if err != nil {
		t.Fatalf("timeout must degrade to METHOD C, not fail: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("command took %s; the injected 100ms timeout was not honoured", elapsed)
	}
	if !strings.Contains(errOut, "auto-login skipped") ||
		!strings.Contains(errOut, "did not load") {
		t.Errorf("stderr must report that auto-login was not completed:\n%s", errOut)
	}
	urls := launcher.opened()
	if len(urls) != 2 || urls[1] != "https://d111abcdef.cloudfront.net" {
		t.Errorf("opened URLs=%v, want [one-shot URL, plain URL]", urls)
	}
}

func TestOpen_JSONOutput_NeverContainsPassword(t *testing.T) {
	prev := global.Output
	global.Output = "json"
	t.Cleanup(func() { global.Output = prev })

	backend := runningBackend()
	launcher := &fakeLauncher{}
	launcher.onOpen = func(url string) {
		_, _, _ = httpGet(t, url) // complete METHOD A
	}
	deps := newTestOpenDeps(backend, launcher)

	out, errOut, err := runOpenCommand(t, deps, "dev01", "--timeout", "5s")
	if err != nil {
		t.Fatalf("open: %v (stderr: %s)", err, errOut)
	}
	if strings.Contains(out, "s3cret-pw") || strings.Contains(errOut, "s3cret-pw") {
		t.Errorf("JSON output must never contain the password:\n%s", out)
	}
	for _, want := range []string{`"url": "https://d111abcdef.cloudfront.net"`, `"method": "auto-login"`, `"auto_login": "completed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"password"`) {
		t.Errorf("JSON output must not have a password field:\n%s", out)
	}
}

func TestOpen_ListenFailureFallsBack(t *testing.T) {
	backend := runningBackend()
	launcher := &fakeLauncher{}
	deps := newTestOpenDeps(backend, launcher)
	deps.listen = func() (net.Listener, error) {
		return nil, errors.New("bind: address already in use")
	}

	_, errOut, err := runOpenCommand(t, deps, "dev01")
	if err != nil {
		t.Fatalf("listener bind failure must fall back to METHOD C: %v", err)
	}
	urls := launcher.opened()
	if len(urls) != 1 || urls[0] != "https://d111abcdef.cloudfront.net" {
		t.Errorf("opened URLs=%v, want the plain distribution URL", urls)
	}
	if !strings.Contains(errOut, "auto-login skipped") {
		t.Errorf("stderr must explain the fallback:\n%s", errOut)
	}
}
