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

// `nakama aws open` (ADR 0012, ADR 0027) opens code-server in the
// user's browser and, by default, completes the password login
// automatically.
//
// By default the browser reaches code-server over an SSM port forward
// that the instance's tunnel daemon keeps up in the background
// (http://localhost:<port>); --internet uses the CloudFront
// distribution instead, as before Issue #118.
//
// code-server authenticates with a session cookie only (there is no
// URL-token login), and a CLI cannot write cookies into a browser's
// cookie store. The auto-login therefore works by serving a one-shot
// page on 127.0.0.1 whose HTML form POSTs the password to
// code-server's /login as a top-level navigation (METHOD A): the
// browser receives the Set-Cookie response first-party and lands
// authenticated. When METHOD A cannot be carried out, the command
// degrades to plainly opening the URL (METHOD C) with a stderr
// explanation. Over SSM the browser must open by itself: when no
// browser can be launched the command fails instead of printing a URL
// for the user to open (Issue #118); --print-url asks for the URLs
// explicitly.

// openBackend is the slice of the service layer that `open` needs.
// Tests substitute a fake; production wraps service.Service plus the
// IP discoverer from awsx.Clients.
type openBackend interface {
	GetInstanceInfo(ctx context.Context, name string, showPassword bool) (*service.InstanceInfo, error)
	ListACL(ctx context.Context) ([]service.ACLEntry, error)
	PublicIPv4(ctx context.Context) (string, error)
	EnsureBrowserForward(ctx context.Context, in service.BrowserForwardInput) (*service.BrowserForward, error)
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

// EnsureBrowserForward delegates to service.Service.EnsureBrowserForward.
func (b *awsOpenBackend) EnsureBrowserForward(ctx context.Context, in service.BrowserForwardInput) (*service.BrowserForward, error) {
	return b.svc.EnsureBrowserForward(ctx, in)
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
	// internet uses the CloudFront distribution instead of SSM.
	internet bool
	// localPort is --local-port (0: the default for the target).
	localPort int
}

// Via values of the machine-readable results.
const (
	viaSSM      = "ssm"
	viaInternet = "internet"
)

// openResult is the machine-readable output of `open`. It NEVER
// carries the password (JSON/YAML output must be safe to pipe/log).
type openResult struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
	// Via is "ssm" (port forward, the default) or "internet"
	// (CloudFront, --internet).
	Via string `json:"via" yaml:"via"`
	// LocalPort is the local port of the SSM forward.
	LocalPort int `json:"local_port,omitempty" yaml:"local_port,omitempty"`
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
		Short: "Open code-server in your browser over SSM, signed in",
		Long: `Opens the instance's code-server in your browser and logs you in
automatically.

By default the browser reaches code-server through an SSM Session
Manager port forward (ADR 0027): http://localhost:8080, or the port
given with --local-port. The forward runs in the instance's tunnel
daemon in the background and reconnects by itself after network
errors, network changes and sleep. Nothing on the instance is reachable
from the internet for this. See it with ` + "`nakama aws tunnel status <name>`" + `
and stop it with ` + "`nakama aws tunnel stop <name>`" + `.

--internet opens the CloudFront URL instead (ADR 0012); it needs your
public IP in the WAF allowlist (` + "`nakama aws acl add`" + `).

The password is fetched from SSM Parameter Store, kept in memory only,
and handed to the browser through a one-shot page served on 127.0.0.1
whose form POSTs it directly to code-server's /login endpoint. The page
path is a single-use random token; the local server accepts exactly one
request and shuts down. If the password is unavailable, code-server is
opened without it; retrieve it with ` + "`nakama aws info <name> --show-password`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAWSOpen(cmd, args[0], f, deps)
		},
	}
	addOpenFlags(cmd, f, openFlagHelp{
		noAuth:   "Skip auto-login; just open code-server",
		printURL: "Do not launch a browser; print the URL(s) instead",
	})
	return cmd
}

// openFlagHelp is the per-command wording of the shared flags.
type openFlagHelp struct {
	noAuth, printURL string
}

// addOpenFlags registers the flags `open` and `kirocrew` share.
func addOpenFlags(cmd *cobra.Command, f *openFlags, help openFlagHelp) {
	cmd.Flags().BoolVar(&f.noAuth, "no-auth", false, help.noAuth)
	cmd.Flags().BoolVar(&f.printURL, "print-url", false, help.printURL)
	cmd.Flags().StringVar(&f.browser, "browser", "",
		"Browser launcher command (the URL is appended as the last argument)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Second,
		"How long to wait for the browser to fetch the one-shot login page")
	cmd.Flags().BoolVar(&f.internet, "internet", false,
		"Use the CloudFront distribution (needs your IP in the WAF allowlist) instead of an SSM port forward")
	cmd.Flags().IntVar(&f.localPort, "local-port", 0,
		"Local port of the SSM port forward (default: the instance's port, i.e. 8080 for code-server and 5476 for Kiro Crew)")
}

// validateOpenFlags checks the flags `open` and `kirocrew` share.
func validateOpenFlags(cmd *cobra.Command, f *openFlags) error {
	if !cmd.Flags().Changed("local-port") {
		return nil
	}
	if f.internet {
		return errors.New("--local-port applies to the SSM port forward; it cannot be combined with --internet")
	}
	if f.localPort < 1 || f.localPort > 65535 {
		return fmt.Errorf("invalid --local-port %d: must be 1-65535", f.localPort)
	}
	return nil
}

func runAWSOpen(cmd *cobra.Command, name string, f *openFlags, deps openDeps) error {
	if err := validateOpenFlags(cmd, f); err != nil {
		return err
	}
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	if f.internet {
		return runAWSOpenInternet(ctx, cmd, name, f, deps, backend)
	}
	return runAWSOpenSSM(ctx, cmd, name, f, deps, backend)
}

// fetchOpenInfo reads the instance, with the password when auto-login
// is wanted. The consolidated fetch fails when ANY part fails,
// including the SecureString password read: it is retried without the
// password, and if that works only auto-login is skipped (the reason
// is returned).
func fetchOpenInfo(ctx context.Context, backend openBackend, name string, wantAuth bool) (*service.InstanceInfo, bool, error, error) {
	info, err := backend.GetInstanceInfo(ctx, name, wantAuth)
	if err != nil && wantAuth {
		infoNoPwd, retryErr := backend.GetInstanceInfo(ctx, name, false)
		if retryErr != nil {
			return nil, false, nil, err // genuinely broken (e.g. instance not found)
		}
		return infoNoPwd, false, fmt.Errorf("could not fetch the code-server password: %w", err), nil
	}
	if err != nil {
		return nil, false, nil, err
	}
	// An empty password would be POSTed as a missing password and
	// code-server would answer with its login page instead of a
	// session cookie, which METHOD A cannot detect (the browser owns
	// that navigation). Treat it as "password unavailable".
	if wantAuth && info.Password == "" {
		return info, false, errors.New("the code-server password in SSM Parameter Store is empty"), nil
	}
	return info, wantAuth, nil, nil
}

// requireCodeServer is the preflight both paths share: a bare instance
// has no code-server, and a stopped one cannot be reached.
func requireCodeServer(info *service.InstanceInfo, name string) error {
	if info.Kind == "bare" {
		return fmt.Errorf("instance %q was created with --bare: it has no code-server to open in a browser.\n"+
			"Use `nakama aws ssh setup %s` (desktop VS Code / JetBrains / ssh) or `nakama aws connect %s` instead", name, name, name)
	}
	if info.State != "running" {
		return fmt.Errorf("instance %q is %s, not running; start it with `nakama aws start %s`",
			name, info.State, name)
	}
	return nil
}

// runAWSOpenSSM opens code-server through the instance's SSM forward
// (the default, ADR 0027).
func runAWSOpenSSM(ctx context.Context, cmd *cobra.Command, name string, f *openFlags, deps openDeps, backend openBackend) error {
	stderr := cmd.ErrOrStderr()
	info, wantAuth, authSkipReason, err := fetchOpenInfo(ctx, backend, name, !f.noAuth)
	if err != nil {
		return err
	}
	if err := requireCodeServer(info, name); err != nil {
		return err
	}

	bf, err := ensureBrowserForward(ctx, cmd, backend.EnsureBrowserForward, name, service.ForwardPurposeCodeServer, f.localPort, "code-server")
	if err != nil {
		return err
	}
	view := openView{name: name, url: bf.Origin, via: viaSSM, localPort: bf.LocalPort}

	if wantAuth {
		target := loginTarget{origin: bf.Origin, label: "code-server"}
		done, skipReason := runAutoLogin(ctx, cmd, f, deps, target, info.Password, bf.Origin)
		if done {
			return renderOpenResult(cmd, view, "auto-login", !f.printURL, "completed", "")
		}
		var launchErr *browserLaunchError
		if errors.As(skipReason, &launchErr) {
			return noBrowserError(name, launchErr)
		}
		authSkipReason = skipReason
	}
	return runPlainOpen(ctx, cmd, f, deps, view, bf.Origin, authSkipReason, stderr)
}

// runAWSOpenInternet opens the CloudFront URL (--internet, ADR 0012).
func runAWSOpenInternet(ctx context.Context, cmd *cobra.Command, name string, f *openFlags, deps openDeps, backend openBackend) error {
	stderr := cmd.ErrOrStderr()
	info, wantAuth, authSkipReason, err := fetchOpenInfo(ctx, backend, name, !f.noAuth)
	if err != nil {
		return err
	}
	if info.Kind != "bare" && (info.DistributionDomain == "" || info.URL == "") {
		return fmt.Errorf("instance %q has no CloudFront distribution domain yet; "+
			"the distribution may still be provisioning — check with `nakama aws info %s`", name, name)
	}
	if err := requireCodeServer(info, name); err != nil {
		return err
	}

	// Soft preflight: warn when the caller's IP is not allowlisted
	// (the URL would return 403). Never fatal, never blocking.
	warnIfNotAllowlisted(ctx, backend, stderr)

	view := openView{name: info.Name, url: info.URL, via: viaInternet}
	if wantAuth {
		target := loginTarget{origin: "https://" + info.DistributionDomain, label: "code-server"}
		done, skipReason := runAutoLogin(ctx, cmd, f, deps, target, info.Password, info.URL)
		if done {
			return renderOpenResult(cmd, view, "auto-login", !f.printURL, "completed", "")
		}
		authSkipReason = skipReason
	}
	// METHOD C: plain URL, with the skip reason (if any) on stderr.
	return runPlainOpen(ctx, cmd, f, deps, view, info.URL, authSkipReason, stderr)
}

// ensureBrowserForward makes the SSM forward ready and explains what is
// going on, since it can take a while (StartSession, and right after a
// start the SSM agent needs a minute). A busy local port gets the
// --local-port advice.
func ensureBrowserForward(ctx context.Context, cmd *cobra.Command,
	ensure func(context.Context, service.BrowserForwardInput) (*service.BrowserForward, error),
	name string, purpose service.ForwardPurpose, localPort int, label string) (*service.BrowserForward, error) {
	stderr := cmd.ErrOrStderr()
	port := localPort
	if port == 0 {
		port = service.DefaultLocalPort(purpose)
	}
	_, _ = fmt.Fprintf(stderr, "forwarding localhost:%d to %s on %s over SSM...\n", port, label, name)
	bf, err := ensure(ctx, service.BrowserForwardInput{Name: name, Purpose: purpose, LocalPort: localPort})
	if err != nil {
		var inUse *service.LocalPortInUseError
		if errors.As(err, &inUse) {
			return nil, fmt.Errorf("%w\nchoose another local port with --local-port <port>", err)
		}
		return nil, err
	}
	if bf.Reused {
		_, _ = fmt.Fprintf(stderr, "using the running forward on localhost:%d\n", bf.LocalPort)
	}
	_, _ = fmt.Fprintf(stderr, "the forward keeps running in the background and reconnects by itself; "+
		"stop it with `nakama aws tunnel stop %s`\n", name)
	return bf, nil
}

// browserLaunchError marks a failure to start a browser.
type browserLaunchError struct{ err error }

func (e *browserLaunchError) Error() string { return "could not launch a browser: " + e.err.Error() }
func (e *browserLaunchError) Unwrap() error { return e.err }

// noBrowserError is the SSM paths' answer to a missing browser: they
// open the browser themselves and never fall back to printing a URL
// (Issue #118).
func noBrowserError(name string, err error) error {
	return fmt.Errorf("%w\nname the browser to start with --browser <command> (the URL is appended as its last argument); "+
		"the forward keeps running until `nakama aws tunnel stop %s`", err, name)
}

// errLoginPageNotFetched signals that METHOD A timed out because the
// browser never requested the one-shot login page.
var errLoginPageNotFetched = errors.New("the browser did not load the one-shot login page in time")

// runAutoLogin performs METHOD A against target. It returns done=true
// when the browser fetched the one-shot login page (the form
// auto-submits to code-server from there on). On any failure it
// returns done=false plus the reason (a *browserLaunchError when no
// browser could be started), and the caller falls back to METHOD C.
// displayURL is what --print-url shows as the site URL.
func runAutoLogin(ctx context.Context, cmd *cobra.Command, f *openFlags, deps openDeps, target loginTarget, password, displayURL string) (bool, error) {
	token, err := deps.randToken()
	if err != nil {
		return false, err
	}
	lis, err := deps.listen()
	if err != nil {
		return false, fmt.Errorf("bind local login server: %w", err)
	}
	srv, err := newOneShotLoginServerFor(lis, token, target, password)
	if err != nil {
		_ = lis.Close() // best-effort cleanup; the bind error path dominates
		return false, err
	}
	defer srv.Close()
	return serveOneShot(ctx, cmd, f, deps, srv, displayURL, fmt.Sprintf("open on this machine within %s to auto-login", f.timeout),
		"auto-login page delivered to the browser; signing in to "+displayURL)
}

// serveOneShot hands the one-shot page to the browser (or prints its
// URL with --print-url) and waits until it was fetched.
func serveOneShot(ctx context.Context, cmd *cobra.Command, f *openFlags, deps openDeps, srv *oneShotLoginServer, displayURL, printHint, deliveredNote string) (bool, error) {
	pageURL := srv.URL()
	if f.printURL {
		// Headless mode: hand both URLs to the operator. The one-shot
		// URL must be printed BEFORE blocking so it can be pasted
		// while the local server is still alive.
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "url       : %s\nlogin_url : %s  (%s)\n",
			displayURL, pageURL, printHint); err != nil {
			return false, err
		}
	} else {
		launcher := deps.newLauncher(f.browser)
		lctx, cancel := context.WithTimeout(ctx, f.timeout)
		err := launcher.Open(lctx, pageURL)
		cancel()
		if err != nil {
			return false, &browserLaunchError{err: err}
		}
	}

	select {
	case <-srv.Served():
		// Give the response a moment to flush, then stop accepting.
		srv.ShutdownGracefully(2 * time.Second)
		if !f.printURL {
			// Best-effort progress note; a stderr write failure must
			// not fail a successful open.
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), deliveredNote)
		}
		return true, nil
	case <-time.After(f.timeout):
		return false, fmt.Errorf("%w (waited %s)", errLoginPageNotFetched, f.timeout)
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// openView is what the result of `open` describes.
type openView struct {
	name      string
	url       string
	via       string
	localPort int
}

// runPlainOpen performs METHOD C: open or print the plain URL.
// skipReason (when non-nil) explains on stderr why auto-login was
// skipped. Over SSM a browser that cannot be launched is an error
// (Issue #118); over the internet the URL is printed instead, as
// before.
func runPlainOpen(ctx context.Context, cmd *cobra.Command, f *openFlags, deps openDeps, view openView, openURL string, skipReason error, stderr io.Writer) error {
	stdout := cmd.OutOrStdout()
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
		_, _ = fmt.Fprintf(stderr, "Log in manually with the password from `nakama aws info %s --show-password`.\n", view.name)
	}

	if f.printURL {
		return renderOpenResult(cmd, view, "plain", false, autoLogin, note)
	}

	launcher := deps.newLauncher(f.browser)
	lctx, cancel := context.WithTimeout(ctx, f.timeout)
	err := launcher.Open(lctx, openURL)
	cancel()
	if err != nil {
		if view.via == viaSSM {
			return noBrowserError(view.name, &browserLaunchError{err: err})
		}
		// No launcher (or it failed): print the URL instead. Still a
		// success — the user got a usable URL (approved fallback).
		_, _ = fmt.Fprintf(stderr, "warning: could not launch a browser (%v); open the URL manually:\n", err)
		if note != "" {
			note += "; "
		}
		note += "browser launch failed: " + err.Error()
		if _, perr := fmt.Fprintln(stdout, view.url); perr != nil {
			return perr
		}
		return renderOpenResultTo(cmd, view, "plain", false, autoLogin, note, true)
	}
	return renderOpenResult(cmd, view, "plain", true, autoLogin, note)
}

// renderOpenResult writes the final result via renderOutput (table
// text for humans, structured object for --output json|yaml). The
// password is never part of the result.
func renderOpenResult(cmd *cobra.Command, view openView, method string, opened bool, autoLogin, note string) error {
	return renderOpenResultTo(cmd, view, method, opened, autoLogin, note, false)
}

// renderOpenResultTo is renderOpenResult with urlAlreadyPrinted
// controlling whether the table text repeats the URL (it is skipped
// when the fallback path already printed the bare URL to stdout).
func renderOpenResultTo(cmd *cobra.Command, view openView, method string, opened bool, autoLogin, note string, urlAlreadyPrinted bool) error {
	res := openResult{
		Name:      view.name,
		URL:       view.url,
		Via:       view.via,
		LocalPort: view.localPort,
		Method:    method,
		Opened:    opened,
		AutoLogin: autoLogin,
		Note:      note,
	}
	suffix := ""
	if view.via == viaSSM {
		suffix = " over SSM"
	}
	var text string
	switch {
	case opened && method == "auto-login":
		text = fmt.Sprintf("opened %s in your browser%s (auto-login)", view.url, suffix)
	case opened:
		text = fmt.Sprintf("opened %s in your browser%s", view.url, suffix)
	case urlAlreadyPrinted:
		// The bare URL is already on stdout; keep table mode quiet.
		if global.Output == "table" {
			return nil
		}
	case method == "auto-login":
		text = fmt.Sprintf("auto-login page delivered; the browser is signing in to %s", view.url)
	default:
		text = view.url
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
// One-shot local pages (METHOD A)
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

// tokenPageTemplate is the auto-submitting GET form that opens the Kiro
// Crew dashboard with its token over SSM (ADR 0027). It keeps the token
// off the browser's command line, which other local users can read;
// the same security properties as loginPageTemplate apply.
var tokenPageTemplate = template.Must(template.New("token").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<title>nakama: opening {{.Target}}</title>
</head>
<body onload="document.getElementById('nakama-open').submit()">
<p>Opening {{.Target}}&hellip; If nothing happens (JavaScript disabled), click the button below.</p>
<form id="nakama-open" method="get" action="{{.Action}}">
<input type="hidden" name="token" value="{{.Token}}">
<button type="submit">Continue to {{.Target}}</button>
</form>
</body>
</html>
`))

// oneShotLoginServer serves one page exactly once on 127.0.0.1 and then
// refuses further requests. The secret lives only in the rendered
// in-memory page; nothing is written to disk and nothing is logged.
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
	return newOneShotLoginServerFor(lis, token, loginTarget{origin: "https://" + domain, label: "code-server"}, password)
}

// loginTarget says where the one-shot page signs in. Every target is a
// code-server login endpoint (the Kiro Crew distribution is fronted by
// code-server too, ADR 0020): a CloudFront origin
// (https://<distribution>) or the SSM forward (http://localhost:<port>,
// ADR 0027). extra carries query parameters that code-server preserves
// across its post-login redirect; the Kiro Crew dashboard token rides on
// that.
type loginTarget struct {
	origin string
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
		Action:   target.origin + "/login?" + query,
		Password: password,
		// href mirrors what the interactive login form submits so the
		// cookie domain/path are derived identically (verified against
		// code-server's src/node/routes/login.ts).
		Href:   target.origin + "/login",
		Target: target.label,
	})
	if err != nil {
		return nil, fmt.Errorf("render login page: %w", err)
	}
	return newOneShotPageServer(lis, token, page.String()), nil
}

// newOneShotTokenServer serves the page that opens origin with the Kiro
// Crew token (ADR 0027).
func newOneShotTokenServer(lis net.Listener, pageToken, origin, label, crewToken string) (*oneShotLoginServer, error) {
	var page strings.Builder
	if err := tokenPageTemplate.Execute(&page, struct {
		Action, Token, Target string
	}{Action: origin + "/", Token: crewToken, Target: label}); err != nil {
		return nil, fmt.Errorf("render token page: %w", err)
	}
	return newOneShotPageServer(lis, pageToken, page.String()), nil
}

// newOneShotPageServer serves page at /<token> on lis, once.
func newOneShotPageServer(lis net.Listener, token, page string) *oneShotLoginServer {
	s := &oneShotLoginServer{
		lis:    lis,
		path:   "/" + token,
		page:   []byte(page),
		served: make(chan struct{}),
	}
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = s.srv.Serve(lis) }() // returns http.ErrServerClosed on shutdown
	return s
}

// URL returns the single-use local page URL.
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
