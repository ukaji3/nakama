package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"golang.org/x/term"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/browser"
	"github.com/ukaji3/nakama/internal/panel"
	"github.com/ukaji3/nakama/internal/service"
	"github.com/ukaji3/nakama/internal/version"
)

// `nakama ui` (Issue #139, ADR 0031) opens the nakama control panel in
// the browser; nakama without arguments does the same when it runs in a
// terminal, and a double-click on nakama.exe opens it next to the
// PowerShell window (ADR 0022). The panel's server runs in this
// command's process, in the foreground: the window it runs in keeps it,
// and closing the window (or Ctrl-C) stops it. The router sends
// http://control.nakama.localhost:<router port>/ to it. A second
// `nakama ui` while one runs only opens a new browser session on it.

// uiDeps are the side effects of `nakama ui` (tests substitute fakes).
type uiDeps struct {
	findPanel    func(ctx context.Context) (*service.PanelStatus, error)
	signInCode   func(ctx context.Context) (string, error)
	stopPanel    func(ctx context.Context) error
	ensureRouter func(ctx context.Context) (*service.RouterStatus, error)
	runPanel     func(ctx context.Context, in service.PanelRunInput) error
	newLauncher  func(overrideCommand string) browser.Launcher
	version      string
	now          func() time.Time
	// peerCheck is the panel's OS-user check (nil: peeruid.Check).
	peerCheck func(*http.Request) error
	// newBackend returns what the panel does with AWS.
	newBackend func() panel.Backend
	// quiet discards the panel's log (the double-click window, where
	// the shell owns the console).
	quiet bool
	// ready is called with the panel's address once the browser was
	// opened on it.
	ready func(origin string)
	// stopSignals returns a context that ends on the signals that stop
	// the panel (Ctrl-C, SIGTERM, a closed terminal).
	stopSignals func(parent context.Context) (context.Context, context.CancelFunc)
}

func defaultUIDeps() uiDeps {
	return uiDeps{
		findPanel:  service.FindPanel,
		signInCode: service.PanelSignInCode,
		stopPanel:  service.StopPanel,
		ensureRouter: func(ctx context.Context) (*service.RouterStatus, error) {
			return service.NewService(nil).EnsureRouter(ctx)
		},
		runPanel:    service.RunPanel,
		newBackend:  func() panel.Backend { return newUIBackend() },
		newLauncher: func(overrideCommand string) browser.Launcher { return browser.New(overrideCommand) },
		version:     version.String(),
		now:         time.Now,
		stopSignals: func(parent context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(parent, panelStopSignals()...)
		},
	}
}

func newUICommand() *cobra.Command { return newUICommandWithDeps(defaultUIDeps()) }

func newUICommandWithDeps(deps uiDeps) *cobra.Command {
	var browserCmd string
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the nakama control panel in your browser",
		Long: `Opens the nakama control panel in your browser, signed in, at
http://control.nakama.localhost:18888/ (NAKAMA_ROUTER_PORT moves it with
the router).

The panel runs in this command, in this window: keep the window open
while you use the panel; closing it or pressing Ctrl-C stops the panel.
Running the command again while the panel runs opens it in the browser
once more. Running nakama without arguments in a terminal does the
same; double-clicking nakama.exe on Windows opens the panel next to the
PowerShell window.

Operations started from the panel (create, delete, backup, restore and
the like) run the same nakama commands in a process of their own: they
keep running when the window is closed, and the panel shows them under
Operations for 7 days.

Only this browser session can use the panel: the browser signs in with
a one-time code in the address nakama opens, and on Linux the panel
answers only the OS user who runs it. Programs that run as your own
user can read your files and are not kept out.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUI(withCancel(cmd), cmd.ErrOrStderr(), browserCmd, deps)
		},
	}
	cmd.Flags().StringVar(&browserCmd, "browser", "",
		"Browser command to start (the address is appended as its last argument)")
	return cmd
}

// errNoBrowserForPanel wraps a failed browser launch: the panel is only
// usable from the browser nakama signs in, so it does not print the
// address instead (the rule of Issue #118).
func errNoBrowserForPanel(err error) error {
	return fmt.Errorf("could not launch a browser: %w\n"+
		"name the browser to start with --browser <command> (the address is appended as its last argument)", err)
}

// isInteractive reports whether nakama runs in a terminal (standard
// input and output): only then does nakama without arguments open the
// panel; scripts and pipes keep getting the help.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) // #nosec G115 -- file descriptors fit in int
}

func runUI(ctx context.Context, stderr io.Writer, browserCmd string, deps uiDeps) error {
	logf := func(format string, args ...any) { slog.Info(fmt.Sprintf(format, args...)) }
	if deps.quiet {
		logf = func(string, ...any) {}
	}
	ready := func(origin string) {
		if deps.ready != nil {
			deps.ready(origin)
		}
	}
	open := func(routerPort int, code string) error {
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := deps.newLauncher(browserCmd).Open(lctx, panel.SignInURL(routerPort, code)); err != nil {
			return errNoBrowserForPanel(err)
		}
		return nil
	}

	running, err := deps.findPanel(ctx)
	if err != nil {
		return err
	}
	if running != nil {
		if running.Version == deps.version {
			code, err := deps.signInCode(ctx)
			if err != nil {
				return fmt.Errorf("ask the running control panel (pid %d) for a sign-in: %w", running.PID, err)
			}
			if err := open(running.RouterPort, code); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stderr, "opened the running nakama control panel (pid %d) at %s/\n",
				running.PID, service.PanelOrigin(running.RouterPort))
			ready(service.PanelOrigin(running.RouterPort))
			return nil
		}
		_, _ = fmt.Fprintf(stderr, "stopping the nakama control panel of %s (pid %d)...\n", running.Version, running.PID)
		if err := deps.stopPanel(ctx); err != nil {
			return err
		}
	}

	router, err := deps.ensureRouter(ctx)
	if err != nil {
		return err
	}
	var backend panel.Backend
	if deps.newBackend != nil {
		backend = deps.newBackend()
	}
	// Operations older than the retention go now; StartOperation prunes
	// as well.
	_ = service.PruneOperations(ctx, service.OperationRetention)
	app := panel.New(panel.Config{
		RouterPort: router.Port,
		Version:    deps.version,
		Backend:    backend,
		Now:        deps.now,
		PeerCheck:  deps.peerCheck,
		Logf:       logf,
	})
	runCtx, stop := deps.stopSignals(ctx)
	defer stop()
	err = deps.runPanel(runCtx, service.PanelRunInput{
		RouterPort: router.Port,
		Handler:    app.Handler(),
		SignInCode: app.NewSignInCode,
		Logf:       logf,
		Ready: func(st *service.PanelState) error {
			code, err := app.NewSignInCode()
			if err != nil {
				return err
			}
			if err := open(router.Port, code); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stderr, "nakama control panel: %s/ (pid %d)\n"+
				"Keep this window open while you use it; close it or press Ctrl-C to stop the panel.\n"+
				"Operations started from the panel keep running when the window is closed.\n",
				service.PanelOrigin(router.Port), st.PID)
			ready(service.PanelOrigin(router.Port))
			return nil
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	_, _ = fmt.Fprintln(stderr, "nakama control panel stopped")
	return nil
}
