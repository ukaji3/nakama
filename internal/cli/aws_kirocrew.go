package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

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
	Name      string `json:"name" yaml:"name"`
	URL       string `json:"url" yaml:"url"`
	Method    string `json:"method" yaml:"method"` // auto-login | plain
	Opened    bool   `json:"opened" yaml:"opened"`
	AutoLogin string `json:"auto_login" yaml:"auto_login"`
	Note      string `json:"note,omitempty" yaml:"note,omitempty"`
}

func newAWSKirocrewCommandWithDeps(deps kirocrewDeps) *cobra.Command {
	f := &openFlags{}
	cmd := &cobra.Command{
		Use:   "kirocrew [enable|disable] <name>",
		Short: "Open the Kiro Crew dashboard through code-server (enable it first)",
		Long: `Opens the Kiro Crew web dashboard of an instance in your browser, signed
in to both code-server and Kiro Crew (ADR 0020).

Kiro Crew is never exposed directly: a dedicated CloudFront distribution
reaches code-server, which checks its password session and forwards the
request to a loopback-only proxy in front of the Kiro Crew gateway. The
same WAF allowlist as code-server applies (` + "`nakama aws acl add`" + `).

  nakama aws kirocrew enable <name>    build it (once per instance)
  nakama aws kirocrew <name>           open the dashboard
  nakama aws kirocrew disable <name>   remove it

Opening fetches the code-server password from SSM and mints a dashboard
token on the instance (returned encrypted to this process), then hands
both to the browser through a one-shot page on 127.0.0.1, exactly like
` + "`nakama aws open`" + `. Neither value is written to disk or logged.

"enable" and "disable" are subcommands, so they cannot be used as
instance names.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKirocrewOpen(cmd, args[0], f, deps)
		},
	}
	cmd.Flags().BoolVar(&f.noAuth, "no-auth", false,
		"Skip the code-server auto-login; open the dashboard URL with the token only")
	cmd.Flags().BoolVar(&f.printURL, "print-url", false,
		"Do not launch a browser; print the URL(s) instead (the URLs carry a token valid for 5 minutes)")
	cmd.Flags().StringVar(&f.browser, "browser", "",
		"Browser launcher command (the URL is appended as the last argument)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Second,
		"How long to wait for the browser to fetch the one-shot login page")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "enable <name>",
			Short: "Build the Kiro Crew web access for an instance (idempotent)",
			Long: `Creates a CloudFront distribution dedicated to Kiro Crew, installs a
loopback-only nginx proxy on the instance, enables code-server's
host-based proxy for that distribution, and makes code-server's
/proxy/5476/ links (e.g. the URL printed by ` + "`kirocrew token`" + `)
redirect to it.

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
	text := fmt.Sprintf("Kiro Crew web access enabled\n  name : %s\n  url  : %s\n\nOpen it with: nakama aws kirocrew %s",
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
	ctx := withCancel(cmd)
	stderr := cmd.ErrOrStderr()

	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
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
				return renderKirocrewResult(cmd, name, web.URL, "auto-login", !f.printURL, "completed", "")
			}
			skipReason = reason
		}
	}
	return runKirocrewPlainOpen(ctx, cmd, name, f, deps.open, web, dashboardURL, skipReason)
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
	srv, err := newOneShotLoginServerFor(lis, pageToken, loginTarget{domain: web.Domain, label: "Kiro Crew", extra: extra}, password)
	if err != nil {
		_ = lis.Close()
		return false, err
	}
	defer srv.Close()

	if f.printURL {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "url       : %s\nlogin_url : %s  (open on this machine within %s; the token inside expires in 5 minutes)\n",
			web.URL, srv.URL(), f.timeout); err != nil {
			return false, err
		}
	} else {
		lctx, cancel := context.WithTimeout(ctx, f.timeout)
		err := deps.newLauncher(f.browser).Open(lctx, srv.URL())
		cancel()
		if err != nil {
			return false, fmt.Errorf("could not launch a browser: %w", err)
		}
	}

	select {
	case <-srv.Served():
		srv.ShutdownGracefully(2 * time.Second)
		if !f.printURL {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "auto-login page delivered to the browser; signing in to Kiro Crew at %s\n", web.URL)
		}
		return true, nil
	case <-time.After(f.timeout):
		return false, fmt.Errorf("%w (waited %s)", errLoginPageNotFetched, f.timeout)
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// runKirocrewPlainOpen opens the dashboard URL (with its token) without
// signing in to code-server; code-server shows its login page first and
// returns to the dashboard afterwards.
func runKirocrewPlainOpen(ctx context.Context, cmd *cobra.Command, name string, f *openFlags, deps openDeps, web *service.KiroCrewWeb, dashboardURL string, skipReason error) error {
	stderr := cmd.ErrOrStderr()
	autoLogin, note := "skipped", ""
	if skipReason != nil {
		if errors.Is(skipReason, errLoginPageNotFetched) {
			autoLogin = "timeout"
		}
		note = "auto-login skipped: " + skipReason.Error()
		_, _ = fmt.Fprintf(stderr, "warning: auto-login skipped: %v\n", skipReason)
		_, _ = fmt.Fprintf(stderr, "Sign in to code-server with the password from `nakama aws info %s --show-password`.\n", name)
	}
	if f.printURL {
		_, _ = fmt.Fprintf(stderr, "note: the URL carries a Kiro Crew token valid for 5 minutes\n")
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), dashboardURL); err != nil {
			return err
		}
		return renderKirocrewResultTo(cmd, name, web.URL, "plain", false, autoLogin, note, true)
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
		return renderKirocrewResultTo(cmd, name, web.URL, "plain", false, autoLogin, note, true)
	}
	return renderKirocrewResult(cmd, name, web.URL, "plain", true, autoLogin, note)
}

func renderKirocrewResult(cmd *cobra.Command, name, u, method string, opened bool, autoLogin, note string) error {
	return renderKirocrewResultTo(cmd, name, u, method, opened, autoLogin, note, false)
}

// renderKirocrewResultTo renders the result. The URL in the result is
// the bare dashboard URL, never the token-carrying one.
func renderKirocrewResultTo(cmd *cobra.Command, name, u, method string, opened bool, autoLogin, note string, urlAlreadyPrinted bool) error {
	res := kirocrewOpenResult{Name: name, URL: u, Method: method, Opened: opened, AutoLogin: autoLogin, Note: note}
	var b strings.Builder
	if !urlAlreadyPrinted {
		fmt.Fprintf(&b, "url        : %s\n", u)
	}
	fmt.Fprintf(&b, "method     : %s\nauto_login : %s", method, autoLogin)
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
