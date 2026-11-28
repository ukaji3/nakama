package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/browser"
	"github.com/ukaji3/nakama/internal/service"
)

// Tests of `nakama ui` (Issue #139, ADR 0031) with fakes for the router,
// the panel process and the browser. Not parallel: some build the root
// command tree (global flags).

type recordingLauncher struct {
	mu      sync.Mutex
	urls    []string
	command string
	err     error
}

func (l *recordingLauncher) Open(_ context.Context, u string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.urls = append(l.urls, u)
	return l.err
}

type uiFakes struct {
	running     *service.PanelStatus
	launcher    *recordingLauncher
	routerCalls int
	stopCalls   int
	runInput    *service.PanelRunInput
	// panelHandler is the Handler the command gave the panel process.
	panelHandler http.Handler
}

func newUIFakes() (*uiFakes, uiDeps) {
	f := &uiFakes{launcher: &recordingLauncher{}}
	deps := uiDeps{
		findPanel:  func(context.Context) (*service.PanelStatus, error) { return f.running, nil },
		signInCode: func(context.Context) (string, error) { return "code-from-running-panel", nil },
		stopPanel:  func(context.Context) error { f.stopCalls++; return nil },
		ensureRouter: func(context.Context) (*service.RouterStatus, error) {
			f.routerCalls++
			return &service.RouterStatus{Port: 18888, Version: "v9.9.9-test"}, nil
		},
		runPanel: func(ctx context.Context, in service.PanelRunInput) error {
			f.runInput = &in
			f.panelHandler = in.Handler
			if err := in.Ready(&service.PanelState{PID: 4242, RouterPort: in.RouterPort}); err != nil {
				return err
			}
			return context.Canceled // the window was closed
		},
		newLauncher: func(cmd string) browser.Launcher { f.launcher.command = cmd; return f.launcher },
		version:     "v9.9.9-test",
		now:         time.Now,
		peerCheck:   func(*http.Request) error { return nil },
		stopSignals: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(parent)
		},
	}
	return f, deps
}

func runUICommand(t *testing.T, deps uiDeps, args ...string) (string, string, error) {
	t.Helper()
	cmd := newUICommandWithDeps(deps)
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

var signInURLPattern = regexp.MustCompile(`^http://control\.nakama\.localhost:18888/session\?c=([0-9a-f]{64})$`)

func TestUIStartsThePanelAndOpensItSignedIn(t *testing.T) {
	f, deps := newUIFakes()
	_, stderr, err := runUICommand(t, deps)
	if err != nil {
		t.Fatal(err)
	}
	if f.routerCalls != 1 || f.runInput == nil || f.runInput.RouterPort != 18888 || f.runInput.SignInCode == nil {
		t.Fatalf("router calls %d, run input %+v", f.routerCalls, f.runInput)
	}
	if len(f.launcher.urls) != 1 {
		t.Fatalf("browser opened %v", f.launcher.urls)
	}
	m := signInURLPattern.FindStringSubmatch(f.launcher.urls[0])
	if m == nil {
		t.Fatalf("opened %q, want the sign-in address with a one-time code", f.launcher.urls[0])
	}
	// The code belongs to the panel the command runs: its handler takes
	// it once.
	signIn := func() int {
		r := httptest.NewRequest(http.MethodGet, "/session?"+url.Values{"c": {m[1]}}.Encode(), nil)
		r.Host = "control.nakama.localhost:18888"
		w := httptest.NewRecorder()
		f.panelHandler.ServeHTTP(w, r)
		return w.Code
	}
	if got := signIn(); got != http.StatusSeeOther {
		t.Fatalf("sign-in with the opened code = %d, want 303", got)
	}
	if got := signIn(); got != http.StatusForbidden {
		t.Errorf("the opened code must work once: second use = %d", got)
	}
	if !strings.Contains(stderr, "http://control.nakama.localhost:18888/") || !strings.Contains(stderr, "Keep this window open") ||
		strings.Contains(stderr, m[1]) {
		t.Errorf("stderr = %q; want the address and the note, never the code", stderr)
	}
	if !strings.Contains(stderr, "nakama control panel stopped") {
		t.Errorf("closing the window is a normal end: %q", stderr)
	}
}

func TestUIOpensTheRunningPanelAgain(t *testing.T) {
	f, deps := newUIFakes()
	f.running = &service.PanelStatus{PID: 777, RouterPort: 19000, Version: "v9.9.9-test"}
	_, stderr, err := runUICommand(t, deps)
	if err != nil {
		t.Fatal(err)
	}
	if f.routerCalls != 0 || f.runInput != nil || f.stopCalls != 0 {
		t.Errorf("a running panel of this version must be reused: router %d, run %v, stop %d", f.routerCalls, f.runInput != nil, f.stopCalls)
	}
	if len(f.launcher.urls) != 1 || f.launcher.urls[0] != "http://control.nakama.localhost:19000/session?c=code-from-running-panel" {
		t.Errorf("opened %v", f.launcher.urls)
	}
	if !strings.Contains(stderr, "pid 777") || strings.Contains(stderr, "code-from-running-panel") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestUIReplacesAPanelOfAnotherVersion(t *testing.T) {
	f, deps := newUIFakes()
	f.running = &service.PanelStatus{PID: 777, RouterPort: 18888, Version: "v0.30.2"}
	if _, _, err := runUICommand(t, deps); err != nil {
		t.Fatal(err)
	}
	if f.stopCalls != 1 || f.routerCalls != 1 || f.runInput == nil {
		t.Errorf("stop %d, router %d, run %v", f.stopCalls, f.routerCalls, f.runInput != nil)
	}
}

func TestUIFailsWhenNoBrowserStarts(t *testing.T) {
	for _, running := range []*service.PanelStatus{nil, {PID: 777, RouterPort: 18888, Version: "v9.9.9-test"}} {
		f, deps := newUIFakes()
		f.running = running
		f.launcher.err = errors.New(`exec: "xdg-open": executable file not found in $PATH`)
		stdout, stderr, err := runUICommand(t, deps)
		if err == nil || !strings.Contains(err.Error(), "--browser <command>") {
			t.Errorf("running=%v: err = %v, want the --browser hint", running != nil, err)
		}
		if strings.Contains(stdout+stderr, "/session?c=") {
			t.Errorf("running=%v: the sign-in address must not be printed instead (Issue #118): %q %q", running != nil, stdout, stderr)
		}
	}
}

func TestUIPassesTheBrowserFlag(t *testing.T) {
	f, deps := newUIFakes()
	if _, _, err := runUICommand(t, deps, "--browser", "firefox --new-window"); err != nil {
		t.Fatal(err)
	}
	if f.launcher.command != "firefox --new-window" {
		t.Errorf("launcher command = %q", f.launcher.command)
	}
}

func TestUIIsListedAndTakesNoArguments(t *testing.T) {
	_, deps := newUIFakes()
	if _, _, err := runUICommand(t, deps, "extra"); err == nil {
		t.Error("ui takes no arguments")
	}
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s+ui\s+Open the nakama control panel`).MatchString(out.String()) ||
		!strings.Contains(out.String(), "Run nakama without arguments to open the control panel") {
		t.Errorf("the help must list ui and say what nakama without arguments does:\n%s", out.String())
	}
}

// TestNakamaWithoutArgumentsOutsideATerminalPrintsTheHelp: scripts and
// pipes keep the old behavior (go test has no terminal).
func TestNakamaWithoutArgumentsOutsideATerminalPrintsTheHelp(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Available Commands:") {
		t.Errorf("want the help, got:\n%s", out.String())
	}
	root = NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"nosuchcommand"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("an unknown command must stay an error: %v", err)
	}
}

func TestPanelBesideTheDoubleClickShell(t *testing.T) {
	f, deps := newUIFakes()
	var out bytes.Buffer
	stop := startPanelBeside(&out, deps)
	if !strings.Contains(out.String(), "panel:  http://control.nakama.localhost:18888/ (opened in your browser") {
		t.Errorf("window line = %q", out.String())
	}
	if len(f.launcher.urls) != 1 || !signInURLPattern.MatchString(f.launcher.urls[0]) {
		t.Errorf("browser opened %v", f.launcher.urls)
	}
	stop()
	// A failure is one warning line, and the shell still opens.
	f, deps = newUIFakes()
	f.launcher.err = errors.New("no browser")
	out.Reset()
	stop = startPanelBeside(&out, deps)
	stop()
	if !strings.Contains(out.String(), "panel:  WARNING: the control panel did not start") {
		t.Errorf("window line = %q", out.String())
	}
}

func TestRecordedCommandsReturnTheBinarysExitCode(t *testing.T) {
	if code := runRecordedCommand([]string{"version"}); code != 0 {
		t.Errorf("version = %d", code)
	}
	if code := runRecordedCommand([]string{"aws", "info"}); code != 1 {
		t.Errorf("info without a name = %d", code)
	}
}

func TestLoginNavigationIsTheOneShotLoginForm(t *testing.T) {
	nav := loginNavigation("http://code-server.dev01.nakama.localhost:18888", "pw", url.Values{"token": {"t"}})
	if nav.Method != "POST" || nav.Action != "http://code-server.dev01.nakama.localhost:18888/login?to=/&token=t" {
		t.Errorf("nav = %+v", nav)
	}
	want := []string{"password=pw", "base=.", "href=http://code-server.dev01.nakama.localhost:18888/login"}
	for i, f := range nav.Fields {
		if f.Name+"="+f.Value != want[i] {
			t.Errorf("field %d = %s=%s, want %s", i, f.Name, f.Value, want[i])
		}
	}
	// The same form `nakama aws code` serves on its one-shot page.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newOneShotLoginServerFor(lis, "x", loginTarget{origin: "http://code-server.dev01.nakama.localhost:18888", label: "code-server", extra: url.Values{"token": {"t"}}}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	page := string(srv.page)
	if !strings.Contains(page, `action="http://code-server.dev01.nakama.localhost:18888/login?to=/&amp;token=t"`) ||
		!strings.Contains(page, `name="href" value="http://code-server.dev01.nakama.localhost:18888/login"`) {
		t.Errorf("the one-shot page differs:\n%s", page)
	}
}
