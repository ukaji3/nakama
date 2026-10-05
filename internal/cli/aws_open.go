package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/browser"
	"github.com/ukaji3/nakama/internal/service"
)

// `nakama aws open` (ADR 0012) opens the code-server URL of an
// instance in the user's browser and, by default, completes the
// password login automatically.
//
// code-server authenticates with a session cookie only (there is no
// URL-token login), and a CLI cannot write cookies into a browser's
// cookie store. The auto-login therefore works by serving a one-shot
// page on 127.0.0.1 whose HTML form POSTs the password to
// https://<distribution-domain>/login as a top-level navigation
// (METHOD A): the browser receives the Set-Cookie response first-party
// and lands authenticated. When any step of METHOD A cannot be carried
// out, the command degrades to plainly opening/printing the URL
// (METHOD C) with a stderr explanation, still exiting 0.

// openBackend is the slice of the service layer that `open` needs.
// Tests substitute a fake; production wraps service.Service plus the
// IP discoverer from awsx.Clients.
type openBackend interface {
	GetInstanceInfo(ctx context.Context, name string, showPassword bool) (*service.InstanceInfo, error)
	ListACL(ctx context.Context) ([]service.ACLEntry, error)
	PublicIPv4(ctx context.Context) (string, error)
}

// awsOpenBackend is the production openBackend.
type awsOpenBackend struct {
	svc *service.Service
	ip  awsx.IPDiscoverer
}

// GetInstanceInfo delegates to service.Service.GetInstanceInfo.
func (b *awsOpenBackend) GetInstanceInfo(ctx context.Context, name string, showPassword bool) (*service.InstanceInfo, error) {
	return b.svc.GetInstanceInfo(ctx, name, showPassword)
}

// ListACL delegates to service.Service.ListACL.
func (b *awsOpenBackend) ListACL(ctx context.Context) ([]service.ACLEntry, error) {
	return b.svc.ListACL(ctx)
}

// PublicIPv4 delegates to the awsx IP discoverer.
func (b *awsOpenBackend) PublicIPv4(ctx context.Context) (string, error) {
	return b.ip.PublicIPv4(ctx)
}

// openDeps bundles the collaborators of `nakama aws open` so tests can
// exercise the full command without AWS, a real browser, or fixed
// ports (same dependency-injection style as profileDeps).
type openDeps struct {
	// connect builds the backend (production: awsx.NewClients +
	// service.NewService, called once per invocation).
	connect func(ctx context.Context) (openBackend, error)
	// newLauncher returns the browser launcher honouring the
	// --browser override.
	newLauncher func(overrideCommand string) browser.Launcher
	// listen binds the local one-shot login server (127.0.0.1, port 0).
	listen func() (net.Listener, error)
	// randToken mints the single-use URL token (>= 128 bits).
	randToken func() (string, error)
}

func defaultOpenDeps() openDeps {
	return openDeps{
		connect: func(ctx context.Context) (openBackend, error) {
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return nil, err
			}
			return &awsOpenBackend{svc: service.NewService(clients), ip: clients.IPDiscoverer}, nil
		},
		newLauncher: func(overrideCommand string) browser.Launcher {
			return browser.New(overrideCommand)
		},
		listen: func() (net.Listener, error) {
			// 127.0.0.1 only, kernel-assigned port: the login page
			// (which embeds the password) is never reachable from
			// other hosts.
			return net.Listen("tcp", "127.0.0.1:0")
		},
		randToken: newLoginPageToken,
	}
}

// newLoginPageToken returns a cryptographically random, URL-safe token
// (128 bits) that becomes the single-use path of the local login page.
func newLoginPageToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate login page token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

type openFlags struct {
	noAuth   bool
	printURL bool
	browser  string
	timeout  time.Duration
}

// openResult is the machine-readable output of `open`. It NEVER
// carries the password (JSON/YAML output must be safe to pipe/log).
type openResult struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
	// Method is "auto-login" (METHOD A) or "plain" (METHOD C).
	Method string `json:"method" yaml:"method"`
	// Opened reports whether a browser launch was attempted
	// successfully (false with --print-url or launcher fallback).
	Opened bool `json:"opened" yaml:"opened"`
	// AutoLogin is "completed", "pending", "skipped" or "timeout".
	AutoLogin string `json:"auto_login" yaml:"auto_login"`
	Note      string `json:"note,omitempty" yaml:"note,omitempty"`
}

func newAWSOpenCommand() *cobra.Command {
	return newAWSOpenCommandWithDeps(defaultOpenDeps())
}

func newAWSOpenCommandWithDeps(deps openDeps) *cobra.Command {
	f := &openFlags{}
	cmd := &cobra.Command{
		Use:   "open <name>",
		Short: "Open the code-server URL in your browser with automatic login",
		Long: `Opens the instance's code-server CloudFront URL in your browser and logs
you in automatically (ADR 0012). The password is fetched from SSM
Parameter Store, kept in memory only, and handed to the browser through
a one-shot page served on 127.0.0.1 whose form POSTs it directly to
code-server's /login endpoint. The page path is a single-use random
token; the local server accepts exactly one request and shuts down.

If auto-login cannot be performed (password unavailable, no browser
launcher, page not fetched in time), the plain URL is opened or printed
instead and the reason goes to stderr; retrieve the password manually
with ` + "`nakama aws info <name> --show-password`" + `.

Reaching the URL requires your public IP to be in the WAF allowlist
(` + "`nakama aws acl add`" + `).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAWSOpen(cmd, args[0], f, deps)
		},
	}
	cmd.Flags().BoolVar(&f.noAuth, "no-auth", false,
		"Skip auto-login; just open the code-server URL")
	cmd.Flags().BoolVar(&f.printURL, "print-url", false,
		"Do not launch a browser; print the URL(s) instead (for headless/SSH use)")
	cmd.Flags().StringVar(&f.browser, "browser", "",
		"Browser launcher command (the URL is appended as the last argument)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Second,
		"How long to wait for the browser to fetch the one-shot login page")
	return cmd
}

func runAWSOpen(cmd *cobra.Command, name string, f *openFlags, deps openDeps) error {
	ctx := withCancel(cmd)
	stderr := cmd.ErrOrStderr()

	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}

	// authSkipReason != nil means METHOD A was abandoned and we run
	// METHOD C; the reason is reported on stderr (never fatal).
	var authSkipReason error
	wantAuth := !f.noAuth

	info, err := backend.GetInstanceInfo(ctx, name, wantAuth)
	if err != nil && wantAuth {
		// The consolidated fetch fails when ANY part fails — including
		// the SecureString password read. Retry without the password:
		// if that succeeds the instance itself is fine and only
		// auto-login must be skipped (fallback to METHOD C).
		infoNoPwd, retryErr := backend.GetInstanceInfo(ctx, name, false)
		if retryErr != nil {
			return err // genuinely broken (e.g. instance not found)
		}
		info = infoNoPwd
		wantAuth = false
		authSkipReason = fmt.Errorf("could not fetch the code-server password: %w", err)
	} else if err != nil {
		return err
	}

	// Fatal preflights: without a URL or a running instance there is
	// nothing meaningful to open.
	if info.Kind == "bare" {
		return fmt.Errorf("instance %q was created with --bare: it has no code-server to open in a browser.\n"+
			"Use `nakama aws ssh setup %s` (desktop VS Code / JetBrains / ssh) or `nakama aws connect %s` instead", name, name, name)
	}
	if info.DistributionDomain == "" || info.URL == "" {
		return fmt.Errorf("instance %q has no CloudFront distribution domain yet; "+
			"the distribution may still be provisioning — check with `nakama aws info %s`", name, name)
	}
	if info.State != "running" {
		return fmt.Errorf("instance %q is %s, not running; start it with `nakama aws start %s`",
			name, info.State, name)
	}

	// An empty password would be POSTed as a missing password and
	// code-server would answer with its login page instead of a
	// session cookie, which METHOD A cannot detect (the browser owns
	// that navigation). Treat it as "password unavailable" and fall
	// back to METHOD C rather than reporting a login that did not
	// happen.
	if wantAuth && info.Password == "" {
		wantAuth = false
		authSkipReason = errors.New("the code-server password in SSM Parameter Store is empty")
	}

	// Soft preflight: warn when the caller's IP is not allowlisted
	// (the URL would return 403). Never fatal, never blocking.
	warnIfNotAllowlisted(ctx, backend, stderr)

	if wantAuth {
		done, skipReason := runAutoLogin(ctx, cmd, name, f, deps, info)
		if done {
			return renderOpenResult(cmd, info, "auto-login", !f.printURL, "completed", "")
		}
		authSkipReason = skipReason
	}

	// METHOD C: plain URL, with the skip reason (if any) on stderr.
	return runPlainOpen(ctx, cmd, name, f, deps, info, authSkipReason)
}

// errLoginPageNotFetched signals that METHOD A timed out because the
// browser never requested the one-shot login page.
var errLoginPageNotFetched = errors.New("the browser did not load the one-shot login page in time")

// runAutoLogin performs METHOD A. It returns done=true when the
// browser fetched the one-shot login page (the form auto-submits to
// code-server from there on). On any failure it returns done=false
// plus the reason, and the caller falls back to METHOD C.
func runAutoLogin(ctx context.Context, cmd *cobra.Command, name string, f *openFlags, deps openDeps, info *service.InstanceInfo) (bool, error) {
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	token, err := deps.randToken()
	if err != nil {
		return false, err
	}
	lis, err := deps.listen()
	if err != nil {
		return false, fmt.Errorf("bind local login server: %w", err)
	}

	srv, err := newOneShotLoginServer(lis, token, info.DistributionDomain, info.Password)
	if err != nil {
		_ = lis.Close() // best-effort cleanup; the bind error path dominates
		return false, err
	}
	defer srv.Close()

	loginURL := srv.URL()

	if f.printURL {
		// Headless mode: hand both URLs to the operator. The one-shot
		// URL must be printed BEFORE blocking so it can be pasted
		// while the local server is still alive.
		if _, err := fmt.Fprintf(stdout, "url       : %s\nlogin_url : %s  (open on this machine within %s to auto-login)\n",
			info.URL, loginURL, f.timeout); err != nil {
			return false, err
		}
	} else {
		launcher := deps.newLauncher(f.browser)
		lctx, cancel := context.WithTimeout(ctx, f.timeout)
		err := launcher.Open(lctx, loginURL)
		cancel()
		if err != nil {
			return false, fmt.Errorf("could not launch a browser: %w", err)
		}
	}

	select {
	case <-srv.Served():
		// Give the response a moment to flush, then stop accepting.
		srv.ShutdownGracefully(2 * time.Second)
		if !f.printURL {
			// Best-effort progress note; a stderr write failure must
			// not fail a successful open.
			_, _ = fmt.Fprintf(stderr, "auto-login page delivered to the browser; signing in to %s\n", info.URL)
		}
		return true, nil
	case <-time.After(f.timeout):
		return false, fmt.Errorf("%w (waited %s)", errLoginPageNotFetched, f.timeout)
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// runPlainOpen performs METHOD C: open or print the plain code-server
// URL. skipReason (when non-nil) explains on stderr why auto-login was
// skipped; the command still succeeds as long as the URL could be
// opened or printed.
func runPlainOpen(ctx context.Context, cmd *cobra.Command, name string, f *openFlags, deps openDeps, info *service.InstanceInfo, skipReason error) error {
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	autoLogin := "skipped"
	note := ""
	if skipReason != nil {
		if errors.Is(skipReason, errLoginPageNotFetched) {
			autoLogin = "timeout"
		}
		note = "auto-login skipped: " + skipReason.Error()
		// Best-effort diagnostics; stderr write failures must not turn
		// a successful fallback into an error.
		_, _ = fmt.Fprintf(stderr, "warning: auto-login skipped: %v\n", skipReason)
		_, _ = fmt.Fprintf(stderr, "Log in manually with the password from `nakama aws info %s --show-password`.\n", name)
	}

	if f.printURL {
		return renderOpenResult(cmd, info, "plain", false, autoLogin, note)
	}

	launcher := deps.newLauncher(f.browser)
	lctx, cancel := context.WithTimeout(ctx, f.timeout)
	err := launcher.Open(lctx, info.URL)
	cancel()
	if err != nil {
		// No launcher (or it failed): print the URL instead. Still a
		// success — the user got a usable URL (approved fallback).
		_, _ = fmt.Fprintf(stderr, "warning: could not launch a browser (%v); open the URL manually:\n", err)
		if note != "" {
			note += "; "
		}
		note += "browser launch failed: " + err.Error()
		if _, perr := fmt.Fprintln(stdout, info.URL); perr != nil {
			return perr
		}
		return renderOpenResultTo(cmd, info, "plain", false, autoLogin, note, true)
	}
	return renderOpenResult(cmd, info, "plain", true, autoLogin, note)
}

// renderOpenResult writes the final result via renderOutput (table
// text for humans, structured object for --output json|yaml). The
// password is never part of the result.
func renderOpenResult(cmd *cobra.Command, info *service.InstanceInfo, method string, opened bool, autoLogin, note string) error {
	return renderOpenResultTo(cmd, info, method, opened, autoLogin, note, false)
}

// renderOpenResultTo is renderOpenResult with urlAlreadyPrinted
// controlling whether the table text repeats the URL (it is skipped
// when the fallback path already printed the bare URL to stdout).
func renderOpenResultTo(cmd *cobra.Command, info *service.InstanceInfo, method string, opened bool, autoLogin, note string, urlAlreadyPrinted bool) error {
	res := openResult{
		Name:      info.Name,
		URL:       info.URL,
		Method:    method,
		Opened:    opened,
		AutoLogin: autoLogin,
		Note:      note,
	}
	var text string
	switch {
	case opened && method == "auto-login":
		text = fmt.Sprintf("opened %s in your browser (auto-login)", info.URL)
	case opened:
		text = fmt.Sprintf("opened %s in your browser", info.URL)
	case urlAlreadyPrinted:
		// The bare URL is already on stdout; keep table mode quiet.
		if global.Output == "table" {
			return nil
		}
	case method == "auto-login":
		text = fmt.Sprintf("auto-login page delivered; the browser is signing in to %s", info.URL)
	default:
		text = info.URL
	}
	return renderOutput(cmd.OutOrStdout(), global.Output, res, text)
}

// warnIfNotAllowlisted checks the caller's public IPv4 against the WAF
// allowlist and prints a stderr warning when it is not covered. The
// check is strictly best-effort (ADR 0012): any failure is logged at
// debug level and must never break `open`.
func warnIfNotAllowlisted(ctx context.Context, backend openBackend, stderr io.Writer) {
	ip, err := backend.PublicIPv4(ctx)
	if err != nil {
		slog.Debug("open: skip WAF allowlist check (public IP discovery failed)", "error", err)
		return
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		slog.Debug("open: skip WAF allowlist check (unparseable public IP)", "ip", ip, "error", err)
		return
	}
	entries, err := backend.ListACL(ctx)
	if err != nil {
		slog.Debug("open: skip WAF allowlist check (ListACL failed)", "error", err)
		return
	}
	for _, e := range entries {
		prefix, err := netip.ParsePrefix(e.CIDR)
		if err != nil {
			slog.Debug("open: unparseable allowlist entry", "cidr", e.CIDR, "error", err)
			continue
		}
		// Proper CIDR containment (e.g. a /24 entry covers the IP),
		// not string equality.
		if prefix.Contains(addr) {
			return
		}
	}
	// Best-effort warning; a stderr write failure must not break `open`.
	_, _ = fmt.Fprintf(stderr,
		"warning: your public IP %s is not in the WAF allowlist; the URL will return 403. Add it with `nakama aws acl add`.\n", addr)
}

// ---------------------------------------------------------------------------
// One-shot local login server (METHOD A)
// ---------------------------------------------------------------------------

// loginPageTemplate is the auto-submitting login form. Security
// properties (ADR 0012):
//   - html/template: every interpolated value is contextually escaped,
//     so a password containing quotes or `<` cannot break out of the
//     attribute;
//   - no external resources, <meta name="referrer" content="no-referrer">;
//   - the form mirrors code-server's own interactive login form
//     (password / base / href) so the cookie domain and path are
//     computed exactly as in a manual login;
//   - a visible submit button keeps the flow usable with JavaScript
//     disabled.
var loginPageTemplate = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<title>nakama: signing in to {{.Target}}</title>
</head>
<body onload="document.getElementById('nakama-login').submit()">
<p>Signing in to {{.Target}}&hellip; If nothing happens (JavaScript disabled), click the button below.</p>
<form id="nakama-login" method="post" action="{{.Action}}">
<input type="hidden" name="password" value="{{.Password}}">
<input type="hidden" name="base" value=".">
<input type="hidden" name="href" value="{{.Href}}">
<button type="submit">Continue to {{.Target}}</button>
</form>
</body>
</html>
`))

// oneShotLoginServer serves the auto-submitting login page exactly
// once on 127.0.0.1 and then refuses further requests. The password
// lives only in the rendered in-memory page; nothing is written to
// disk and nothing is logged.
type oneShotLoginServer struct {
	lis    net.Listener
	srv    *http.Server
	path   string // "/" + random token
	page   []byte
	once   sync.Once
	served chan struct{}
}

// newOneShotLoginServer renders the login page for the given
// CloudFront domain and password and starts serving it at
// /<token> on the provided listener.
func newOneShotLoginServer(lis net.Listener, token, domain, password string) (*oneShotLoginServer, error) {
	return newOneShotLoginServerFor(lis, token, loginTarget{domain: domain, label: "code-server"}, password)
}

// loginTarget says where the one-shot page signs in. Every target is a
// code-server login endpoint (the Kiro Crew distribution is fronted by
// code-server too, ADR 0020); extra carries query parameters that
// code-server preserves across its post-login redirect — the Kiro Crew
// dashboard token rides on that.
type loginTarget struct {
	domain string
	label  string
	extra  url.Values
}

// newOneShotLoginServerFor is newOneShotLoginServer for any target.
func newOneShotLoginServerFor(lis net.Listener, token string, target loginTarget, password string) (*oneShotLoginServer, error) {
	// ?to=/ makes code-server 302 to the root after setting the session
	// cookie. code-server's redirect keeps every other query parameter
	// (src/node/http.ts redirect: "Query variables on the current URI
	// will be preserved"), so extra parameters reach the root page.
	query := "to=/"
	if len(target.extra) > 0 {
		query += "&" + target.extra.Encode()
	}
	var page strings.Builder
	err := loginPageTemplate.Execute(&page, struct {
		Action   string
		Password string
		Href     string
		Target   string
	}{
		Action:   "https://" + target.domain + "/login?" + query,
		Password: password,
		// href mirrors what the interactive login form submits so the
		// cookie domain/path are derived identically (verified against
		// code-server's src/node/routes/login.ts).
		Href:   "https://" + target.domain + "/login",
		Target: target.label,
	})
	if err != nil {
		return nil, fmt.Errorf("render login page: %w", err)
	}

	s := &oneShotLoginServer{
		lis:    lis,
		path:   "/" + token,
		page:   []byte(page.String()),
		served: make(chan struct{}),
	}
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = s.srv.Serve(lis) }() // returns http.ErrServerClosed on shutdown
	return s, nil
}

// URL returns the single-use local login URL.
func (s *oneShotLoginServer) URL() string {
	return "http://" + s.lis.Addr().String() + s.path
}

// Served is closed after the page has been delivered once.
func (s *oneShotLoginServer) Served() <-chan struct{} {
	return s.served
}

func (s *oneShotLoginServer) handle(w http.ResponseWriter, r *http.Request) {
	// Wrong path (token mismatch): plain 404 without any hint.
	if r.URL.Path != s.path {
		http.NotFound(w, r)
		return
	}
	delivered := false
	s.once.Do(func() {
		delivered = true
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// Best-effort write: the browser owns the connection; there is
		// no meaningful recovery for a failed body write here.
		_, _ = w.Write(s.page)
		close(s.served)
	})
	if !delivered {
		// The page is strictly single-use: any replay gets 410 Gone.
		http.Error(w, "gone: this one-shot login page has already been used", http.StatusGone)
	}
}

// ShutdownGracefully stops accepting new connections and waits up to
// grace for the in-flight response to flush.
func (s *oneShotLoginServer) ShutdownGracefully(grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // best-effort; Close in the caller's defer is the backstop
}

// Close force-closes the server (defer backstop).
func (s *oneShotLoginServer) Close() {
	_ = s.srv.Close()
}
