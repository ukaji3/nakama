package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// kirocrewBackend is the slice of service.Service the kirocrew commands
// use; tests substitute a fake.
type kirocrewBackend interface {
	GetKiroCrewWeb(ctx context.Context, name string) (*service.KiroCrewWeb, error)
	GetInstanceInfo(ctx context.Context, name string, showPassword bool) (*service.InstanceInfo, error)
	MintKiroCrewToken(ctx context.Context, name string) (string, error)
	EnableKiroCrewWeb(ctx context.Context, name string) (*service.EnableKiroCrewWebOutput, error)
	DisableKiroCrewWeb(ctx context.Context, name string) (*service.DisableKiroCrewWebOutput, error)
	EnsureBrowserForward(ctx context.Context, in service.BrowserForwardInput) (*service.BrowserForward, error)
}

type kirocrewDeps struct {
	connect func(ctx context.Context) (kirocrewBackend, error)
	open    openDeps // launcher, listener and page token, shared with `open`
}

func defaultKirocrewDeps() kirocrewDeps {
	return kirocrewDeps{
		connect: func(ctx context.Context) (kirocrewBackend, error) {
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return nil, err
			}
			return service.NewService(clients), nil
		},
		open: defaultOpenDeps(),
	}
}

func newAWSKirocrewCommand() *cobra.Command {
	return newAWSKirocrewCommandWithDeps(defaultKirocrewDeps())
}

// kirocrewOpenResult is the machine-readable output of `kirocrew`. It
// never carries the code-server password or the Kiro Crew token.
type kirocrewOpenResult struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
	// Via is "ssm" (port forward, the default) or "internet"
	// (CloudFront, --internet).
	Via       string `json:"via" yaml:"via"`
	LocalPort int    `json:"local_port,omitempty" yaml:"local_port,omitempty"`
	Method    string `json:"method" yaml:"method"` // auto-login | plain
	Opened    bool   `json:"opened" yaml:"opened"`
	AutoLogin string `json:"auto_login" yaml:"auto_login"`
	Note      string `json:"note,omitempty" yaml:"note,omitempty"`
}

func newAWSKirocrewCommandWithDeps(deps kirocrewDeps) *cobra.Command {
	f := &openFlags{}
	cmd := &cobra.Command{
		Use:   "kirocrew [enable|disable] <name>",
		Short: "Open the Kiro Crew dashboard in your browser over SSM, signed in",
		Long: `Opens the Kiro Crew web dashboard of an instance in your browser, signed
in.

By default the browser reaches Kiro Crew through an SSM Session Manager
port forward to the Kiro Crew gateway on the instance (ADR 0027):
http://localhost:5476, or the port given with --local-port. No
` + "`kirocrew enable`" + ` is needed for this, and it works on --bare instances
too. The forward runs in the instance's tunnel daemon in the background
and reconnects by itself; stop it with ` + "`nakama aws tunnel stop <name>`" + `.
Kiro Crew sees these requests as local ones, so the dashboard also
allows changing settings and showing secrets.

--internet opens the dashboard through its own CloudFront distribution
instead (ADR 0020). Kiro Crew is never exposed directly there: the
distribution reaches code-server, which checks its password session and
forwards the request to a loopback-only proxy in front of the gateway.
The same WAF allowlist as code-server applies (` + "`nakama aws acl add`" + `).

  nakama aws kirocrew <name>                   open the dashboard over SSM
  nakama aws cloudfront add <name>             give the instance CloudFront (needed first)
  nakama aws kirocrew enable <name>            build the CloudFront access (once per instance)
  nakama aws kirocrew --internet <name>        open the dashboard through CloudFront
  nakama aws kirocrew disable <name>           remove the CloudFront access

Opening mints a dashboard token on the instance (returned encrypted to
this process) and hands it to the browser through a one-shot page on
127.0.0.1; with --internet the code-server password goes along, exactly
like ` + "`nakama aws open --internet`" + `. Neither value is written to disk or
logged.

"enable" and "disable" are subcommands, so they cannot be used as
instance names.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKirocrewOpen(cmd, args[0], f, deps)
		},
	}
	addOpenFlags(cmd, f, openFlagHelp{
		noAuth: "Skip the automatic sign-in (over SSM: open the dashboard without a token; " +
			"with --internet: skip the code-server auto-login and open the dashboard URL with the token only)",
		printURL: "Do not launch a browser; print the URL(s) instead (the URLs carry a token valid for 5 minutes)",
	})

	cmd.AddCommand(
		&cobra.Command{
			Use:   "enable <name>",
			Short: "Build the Kiro Crew access through CloudFront for --internet (idempotent)",
			Long: `Creates a CloudFront distribution dedicated to Kiro Crew, installs a
loopback-only nginx proxy on the instance, enables code-server's
host-based proxy for that distribution, and makes code-server's
/proxy/5476/ links (e.g. the URL printed by ` + "`kirocrew token`" + `)
redirect to it. Only ` + "`nakama aws kirocrew --internet`" + ` needs this; the
default SSM path does not.

It builds on the instance's code-server CloudFront distribution, which
instances get only on request: run ` + "`nakama aws cloudfront add <name>`" + `
first (ADR 0028). ` + "`nakama aws cloudfront delete`" + ` removes both.

The instance must be running. code-server is restarted once, so an open
browser tab reconnects. CloudFront takes several minutes (up to ~15) to
deploy; the command waits for it. Safe to re-run: it resumes where a
previous run stopped and never creates a second distribution.

The distribution adds CloudFront and WAF charges; see the AWS pricing
calculator for an estimate.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runKirocrewEnable(cmd, args[0], deps)
			},
		},
		&cobra.Command{
			Use:   "disable <name>",
			Short: "Remove the Kiro Crew web access of an instance (idempotent)",
			Long: `Removes the redirect, the CloudFront function and the Kiro Crew
distribution, and (when the instance is running) the nginx site and the
code-server proxy settings. Parts that are already gone are skipped.
` + "`nakama aws delete`" + ` also removes all of it.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runKirocrewDisable(cmd, args[0], deps)
			},
		},
	)
	return cmd
}

func runKirocrewEnable(cmd *cobra.Command, name string, deps kirocrewDeps) error {
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "enabling Kiro Crew web access for %s; CloudFront deploys take several minutes "+
		"(code-server restarts once, open tabs reconnect)\n", name)
	out, err := backend.EnableKiroCrewWeb(ctx, name)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Kiro Crew web access enabled\n  name : %s\n  url  : %s\n\nOpen it with: nakama aws kirocrew --internet %s",
		out.Name, out.URL, out.Name)
	return renderOutput(cmd.OutOrStdout(), global.Output, out, text)
}

func runKirocrewDisable(cmd *cobra.Command, name string, deps kirocrewDeps) error {
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	out, err := backend.DisableKiroCrewWeb(ctx, name)
	if err != nil {
		return err
	}
	text := "Kiro Crew web access disabled for " + name
	if out.InstanceSkipped {
		text += fmt.Sprintf("\nnote: %s is not running, so its nginx site and code-server proxy settings were left in place "+
			"(harmless); run `nakama aws start %s` and disable again to remove them", name, name)
	}
	return renderOutput(cmd.OutOrStdout(), global.Output, out, text)
}

func runKirocrewOpen(cmd *cobra.Command, name string, f *openFlags, deps kirocrewDeps) error {
	if err := validateOpenFlags(cmd, f); err != nil {
		return err
	}
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	if f.internet {
		return runKirocrewOpenInternet(ctx, cmd, name, f, deps, backend)
	}
	return runKirocrewOpenSSM(ctx, cmd, name, f, deps, backend)
}

// runKirocrewOpenSSM opens the dashboard through an SSM forward to the
// Kiro Crew gateway on the instance (the default, ADR 0027). The token
// alone signs the browser in; code-server is not on this path, so a
// bare instance works too.
func runKirocrewOpenSSM(ctx context.Context, cmd *cobra.Command, name string, f *openFlags, deps kirocrewDeps, backend kirocrewBackend) error {
	info, err := backend.GetInstanceInfo(ctx, name, false)
	if err != nil {
		return err
	}
	if info.State != "running" {
		return fmt.Errorf("instance %q is %s, not running; start it with `nakama aws start %s`", name, orNotFound(info.State), name)
	}
	bf, err := ensureBrowserForward(ctx, cmd, backend.EnsureBrowserForward, name, service.ForwardPurposeKiroCrew, f.localPort, "Kiro Crew")
	if err != nil {
		return err
	}
	view := kirocrewView{name: name, url: bf.Origin, via: viaSSM, localPort: bf.LocalPort}

	if f.noAuth {
		return openKirocrewURL(ctx, cmd, f, deps.open, view, bf.Origin+"/", nil)
	}
	// The dashboard token is minted right before opening: its link
	// window is 5 minutes.
	token, err := backend.MintKiroCrewToken(ctx, name)
	if err != nil {
		return err
	}
	done, reason := runKirocrewTokenPage(ctx, cmd, f, deps.open, bf.Origin, token)
	if done {
		return renderKirocrewResult(cmd, view, "auto-login", !f.printURL, "completed", "")
	}
	var launchErr *browserLaunchError
	if errors.As(reason, &launchErr) {
		return noBrowserError(name, launchErr)
	}
	// The page was not fetched in time: open the dashboard with the
	// token directly (Kiro Crew strips it once it set its cookie).
	return openKirocrewURL(ctx, cmd, f, deps.open, view, bf.Origin+"/?"+url.Values{"token": {token}}.Encode(), reason)
}

// runKirocrewTokenPage hands the token to the browser through a
// one-shot page that opens origin with it, so the token stays off the
// browser's command line.
func runKirocrewTokenPage(ctx context.Context, cmd *cobra.Command, f *openFlags, deps openDeps, origin, token string) (bool, error) {
	pageToken, err := deps.randToken()
	if err != nil {
		return false, err
	}
	lis, err := deps.listen()
	if err != nil {
		return false, fmt.Errorf("bind local login server: %w", err)
	}
	srv, err := newOneShotTokenServer(lis, pageToken, origin, "Kiro Crew", token)
	if err != nil {
		_ = lis.Close()
		return false, err
	}
	defer srv.Close()
	return serveOneShot(ctx, cmd, f, deps, srv, origin,
		fmt.Sprintf("open on this machine within %s; the token inside expires in 5 minutes", f.timeout),
		"sign-in page delivered to the browser; opening Kiro Crew at "+origin)
}

// openKirocrewURL launches the browser at target (over SSM, where a
// browser that cannot be launched is an error) or prints it with
// --print-url.
func openKirocrewURL(ctx context.Context, cmd *cobra.Command, f *openFlags, deps openDeps, view kirocrewView, target string, skipReason error) error {
	stderr := cmd.ErrOrStderr()
	autoLogin, note := "skipped", ""
	if skipReason != nil {
		if errors.Is(skipReason, errLoginPageNotFetched) {
			autoLogin = "timeout"
		}
		note = "sign-in page skipped: " + skipReason.Error()
		_, _ = fmt.Fprintf(stderr, "warning: sign-in page skipped: %v; opening the dashboard with the token directly\n", skipReason)
	}
	if f.printURL {
		if strings.Contains(target, "token=") {
			_, _ = fmt.Fprintf(stderr, "note: the URL carries a Kiro Crew token valid for 5 minutes\n")
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), target); err != nil {
			return err
		}
		return renderKirocrewResultTo(cmd, view, "plain", false, autoLogin, note, true)
	}
	lctx, cancel := context.WithTimeout(ctx, f.timeout)
	err := deps.newLauncher(f.browser).Open(lctx, target)
	cancel()
	if err != nil {
		return noBrowserError(view.name, &browserLaunchError{err: err})
	}
	return renderKirocrewResult(cmd, view, "plain", true, autoLogin, note)
}

// runKirocrewOpenInternet opens the dashboard through its CloudFront
// distribution (--internet, ADR 0020).
func runKirocrewOpenInternet(ctx context.Context, cmd *cobra.Command, name string, f *openFlags, deps kirocrewDeps, backend kirocrewBackend) error {
	stderr := cmd.ErrOrStderr()
	web, err := backend.GetKiroCrewWeb(ctx, name)
	if err != nil {
		return err
	}
	if web.State != "running" {
		return fmt.Errorf("instance %q is %s, not running; start it with `nakama aws start %s`", name, orNotFound(web.State), name)
	}
	if !web.Deployed {
		_, _ = fmt.Fprintf(stderr, "note: the Kiro Crew distribution is still deploying; the page may not load until it is done\n")
	}

	// The dashboard token is minted right before opening: its link
	// window is 5 minutes.
	token, err := backend.MintKiroCrewToken(ctx, name)
	if err != nil {
		return err
	}
	extra := url.Values{"token": {token}}
	dashboardURL := web.URL + "/?" + extra.Encode()
	view := kirocrewView{name: name, url: web.URL, via: viaInternet}

	var skipReason error
	if !f.noAuth {
		info, err := backend.GetInstanceInfo(ctx, name, true)
		switch {
		case err != nil:
			skipReason = fmt.Errorf("could not fetch the code-server password: %w", err)
		case info.Password == "":
			skipReason = errors.New("the code-server password in SSM Parameter Store is empty")
		default:
			done, reason := runKirocrewAutoLogin(ctx, cmd, f, deps.open, web, info.Password, extra)
			if done {
				return renderKirocrewResult(cmd, view, "auto-login", !f.printURL, "completed", "")
			}
			skipReason = reason
		}
	}
	return runKirocrewPlainOpen(ctx, cmd, f, deps.open, view, dashboardURL, skipReason)
}

// runKirocrewAutoLogin hands the password and the token to the browser
// through the one-shot page, which signs in to code-server on the crew
// distribution; code-server then redirects to /?token=… (it keeps query
// parameters across its post-login redirect), where Kiro Crew swaps the
// token for its own session cookie.
func runKirocrewAutoLogin(ctx context.Context, cmd *cobra.Command, f *openFlags, deps openDeps, web *service.KiroCrewWeb, password string, extra url.Values) (bool, error) {
	pageToken, err := deps.randToken()
	if err != nil {
		return false, err
	}
	lis, err := deps.listen()
	if err != nil {
		return false, fmt.Errorf("bind local login server: %w", err)
	}
	srv, err := newOneShotLoginServerFor(lis, pageToken, loginTarget{origin: "https://" + web.Domain, label: "Kiro Crew", extra: extra}, password)
	if err != nil {
		_ = lis.Close()
		return false, err
	}
	defer srv.Close()
	return serveOneShot(ctx, cmd, f, deps, srv, web.URL,
		fmt.Sprintf("open on this machine within %s; the token inside expires in 5 minutes", f.timeout),
		"auto-login page delivered to the browser; signing in to Kiro Crew at "+web.URL)
}

// runKirocrewPlainOpen opens the dashboard URL (with its token) without
// signing in to code-server; code-server shows its login page first and
// returns to the dashboard afterwards (--internet).
func runKirocrewPlainOpen(ctx context.Context, cmd *cobra.Command, f *openFlags, deps openDeps, view kirocrewView, dashboardURL string, skipReason error) error {
	stderr := cmd.ErrOrStderr()
	autoLogin, note := "skipped", ""
	if skipReason != nil {
		if errors.Is(skipReason, errLoginPageNotFetched) {
			autoLogin = "timeout"
		}
		note = "auto-login skipped: " + skipReason.Error()
		_, _ = fmt.Fprintf(stderr, "warning: auto-login skipped: %v\n", skipReason)
		_, _ = fmt.Fprintf(stderr, "Sign in to code-server with the password from `nakama aws info %s --show-password`.\n", view.name)
	}
	if f.printURL {
		_, _ = fmt.Fprintf(stderr, "note: the URL carries a Kiro Crew token valid for 5 minutes\n")
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), dashboardURL); err != nil {
			return err
		}
		return renderKirocrewResultTo(cmd, view, "plain", false, autoLogin, note, true)
	}
	lctx, cancel := context.WithTimeout(ctx, f.timeout)
	err := deps.newLauncher(f.browser).Open(lctx, dashboardURL)
	cancel()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: could not launch a browser (%v); open this URL within 5 minutes:\n", err)
		if note != "" {
			note += "; "
		}
		note += "browser launch failed: " + err.Error()
		if _, perr := fmt.Fprintln(cmd.OutOrStdout(), dashboardURL); perr != nil {
			return perr
		}
		return renderKirocrewResultTo(cmd, view, "plain", false, autoLogin, note, true)
	}
	return renderKirocrewResult(cmd, view, "plain", true, autoLogin, note)
}

// kirocrewView is what the result of `kirocrew` describes.
type kirocrewView struct {
	name      string
	url       string
	via       string
	localPort int
}

func renderKirocrewResult(cmd *cobra.Command, view kirocrewView, method string, opened bool, autoLogin, note string) error {
	return renderKirocrewResultTo(cmd, view, method, opened, autoLogin, note, false)
}

// renderKirocrewResultTo renders the result. The URL in the result is
// the bare dashboard URL, never the token-carrying one.
func renderKirocrewResultTo(cmd *cobra.Command, view kirocrewView, method string, opened bool, autoLogin, note string, urlAlreadyPrinted bool) error {
	res := kirocrewOpenResult{Name: view.name, URL: view.url, Via: view.via, LocalPort: view.localPort,
		Method: method, Opened: opened, AutoLogin: autoLogin, Note: note}
	var b strings.Builder
	if !urlAlreadyPrinted {
		fmt.Fprintf(&b, "url        : %s\n", view.url)
	}
	fmt.Fprintf(&b, "via        : %s\nmethod     : %s\nauto_login : %s", view.via, method, autoLogin)
	if note != "" {
		fmt.Fprintf(&b, "\nnote       : %s", note)
	}
	return renderOutput(cmd.OutOrStdout(), global.Output, res, b.String())
}

func orNotFound(state string) string {
	if state == "" {
		return "not found"
	}
	return state
}
