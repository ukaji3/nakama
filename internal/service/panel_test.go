package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the control panel's process side (Issue #139, ADR 0031): its
// state file and control channel, and how the router finds it and sends
// control.nakama.localhost to it. Not parallel: they use the state
// directory and package timers.

// testPanelRun is RunPanel run in the background.
type testPanelRun struct {
	done   chan struct{}
	err    error
	state  *PanelState
	cancel context.CancelFunc
	codes  atomic.Int32
}

func startTestPanel(t *testing.T, routerPort int, handler http.Handler) *testPanelRun {
	t.Helper()
	tp := &testPanelRun{done: make(chan struct{})}
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "panel") })
	}
	ready := make(chan *PanelState, 1)
	ctx, cancel := context.WithCancel(context.Background())
	tp.cancel = cancel
	go func() {
		defer close(tp.done)
		tp.err = RunPanel(ctx, PanelRunInput{
			RouterPort: routerPort,
			Handler:    handler,
			SignInCode: func() (string, error) { return "code-" + strconv.Itoa(int(tp.codes.Add(1))), nil },
			Ready:      func(st *PanelState) error { ready <- st; return nil },
		})
	}()
	select {
	case tp.state = <-ready:
	case <-tp.done:
		t.Fatalf("the panel ended before serving: %v", tp.err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the panel did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-tp.done:
		case <-time.After(10 * time.Second):
		}
	})
	return tp
}

func TestRunPanelWritesItsStateAndAnswersItsControlChannel(t *testing.T) {
	withTunnelStateDir(t)
	tp := startTestPanel(t, 18888, nil)

	st, err := readPanelState()
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != os.Getpid() || st.RouterPort != 18888 || st.Port == 0 || st.ControlPort == 0 || st.Token == "" || st.Token != tp.state.Token {
		t.Errorf("state = %+v", st)
	}
	path, _ := panelStatePath()
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("state file mode: %v, %v (it holds the control token)", info, err)
	}
	if !strings.HasSuffix(path, "panel.json") || strings.Contains(path, "tunnels") {
		t.Errorf("the state file %s must not be in the tunnel state directory", path)
	}
	// The browser-facing server answers on the recorded port.
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(st.Port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "panel" {
		t.Errorf("the panel's own port answered %q", b)
	}

	status, err := FindPanel(context.Background())
	if err != nil || status == nil || status.RouterPort != 18888 || status.PID != os.Getpid() {
		t.Fatalf("FindPanel = %+v, %v", status, err)
	}
	wrong := *st
	wrong.Token = "not-the-token"
	if _, err := queryPanelStatus(context.Background(), &wrong, time.Second); err == nil {
		t.Error("the control channel must refuse a wrong token")
	}
	if _, err := requestPanelSignInCode(context.Background(), &wrong, time.Second); err == nil {
		t.Error("the sign-in code must need the token")
	}
	c1, err := PanelSignInCode(context.Background())
	c2, err2 := PanelSignInCode(context.Background())
	if err != nil || err2 != nil || c1 != "code-1" || c2 != "code-2" {
		t.Errorf("sign-in codes = %q %q, %v %v", c1, c2, err, err2)
	}
	if !panelAlive(context.Background()) {
		t.Error("panelAlive must see the running panel")
	}

	if err := StopPanel(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tp.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the panel did not stop")
	}
	if tp.err != nil {
		t.Errorf("RunPanel after a stop request = %v", tp.err)
	}
	if _, err := readPanelState(); !isNoPanel(err) {
		t.Errorf("the state file must be gone after the stop: %v", err)
	}
	if panelAlive(context.Background()) {
		t.Error("panelAlive after the stop")
	}
	if s, err := FindPanel(context.Background()); s != nil || err != nil {
		t.Errorf("FindPanel after the stop = %+v, %v", s, err)
	}
}

func TestRunPanelEndsWithItsContext(t *testing.T) {
	withTunnelStateDir(t)
	tp := startTestPanel(t, 18888, nil)
	tp.cancel()
	select {
	case <-tp.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the panel did not end with its context")
	}
	if tp.err != nil {
		t.Errorf("RunPanel = %v", tp.err)
	}
	if _, err := readPanelState(); !isNoPanel(err) {
		t.Errorf("state after the end: %v", err)
	}
}

func TestRunPanelRefusesASecondPanelAndReplacesALeftover(t *testing.T) {
	withTunnelStateDir(t)
	tp := startTestPanel(t, 18888, nil)
	err := RunPanel(context.Background(), PanelRunInput{
		RouterPort: 18888,
		Handler:    http.NotFoundHandler(),
		SignInCode: func() (string, error) { return "x", nil },
		Ready:      func(*PanelState) error { t.Error("a second panel must not serve"); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "another nakama control panel") {
		t.Errorf("second RunPanel = %v", err)
	}
	if st, _ := readPanelState(); st == nil || st.Token != tp.state.Token {
		t.Error("the running panel's state file must stay")
	}
	tp.cancel()
	<-tp.done

	// A panel that is gone left its file: the next one replaces it.
	leftover := &PanelState{PID: 1, Port: 1, ControlPort: freePort(t), Token: "gone", Version: "v0"}
	if err := writePanelState(leftover); err != nil {
		t.Fatal(err)
	}
	next := startTestPanel(t, 18888, nil)
	if st, _ := readPanelState(); st == nil || st.Token != next.state.Token {
		t.Errorf("the leftover state file was not replaced: %+v", st)
	}
}

func TestRunPanelStopsWhenReadyFails(t *testing.T) {
	withTunnelStateDir(t)
	boom := errors.New("no browser")
	err := RunPanel(context.Background(), PanelRunInput{
		RouterPort: 18888,
		Handler:    http.NotFoundHandler(),
		SignInCode: func() (string, error) { return "x", nil },
		Ready:      func(*PanelState) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Errorf("RunPanel = %v, want the Ready error", err)
	}
	if _, err := readPanelState(); !isNoPanel(err) {
		t.Errorf("state after a failed start: %v", err)
	}
}

func TestFindPanelRemovesALeftoverStateFile(t *testing.T) {
	withTunnelStateDir(t)
	if err := writePanelState(&PanelState{PID: 1, Port: 1, ControlPort: freePort(t), Token: "gone"}); err != nil {
		t.Fatal(err)
	}
	if s, err := FindPanel(context.Background()); s != nil || err != nil {
		t.Errorf("FindPanel = %+v, %v", s, err)
	}
	if _, err := readPanelState(); !isNoPanel(err) {
		t.Errorf("the leftover must be removed: %v", err)
	}
	if err := StopPanel(context.Background()); err != nil {
		t.Errorf("StopPanel without a panel = %v", err)
	}
}

func TestIsPanelHost(t *testing.T) {
	for host, want := range map[string]bool{
		"control.nakama.localhost:18888":              true,
		"Control.Nakama.LocalHost:18888":              true,
		"control.nakama.localhost.:18888":             true,
		"control.nakama.localhost:9999":               false,
		"control.nakama.localhost":                    false,
		"x.control.nakama.localhost:18888":            false,
		"control.dev01.nakama.localhost:18888":        false,
		"code-server.control.nakama.localhost:18888":  false,
		"control.nakama.localhost.evil.example:18888": false,
	} {
		if got := isPanelHost(host, 18888); got != want {
			t.Errorf("isPanelHost(%q) = %v, want %v", host, got, want)
		}
	}
	// An instance may be called "control": its addresses stay its own.
	if p, name, ok := parseRouterHost("code-server.control.nakama.localhost:18888", 18888); !ok || p != ForwardPurposeCodeServer || name != "control" {
		t.Errorf("an instance named control: %q %q %v", p, name, ok)
	}
}

// fakePanelRoute is a scripted panelRoute.
type fakePanelRoute struct {
	mu       sync.Mutex
	port     string
	forgot   int
	resolved int
}

func (f *fakePanelRoute) resolve(context.Context, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved++
	if f.port == "" {
		return "", errPanelNotRunning
	}
	return f.port, nil
}

func (f *fakePanelRoute) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot++
}

// startPanelRouter serves a router handler for port 18888 with the given
// panel route and peer check.
func startPanelRouter(t *testing.T, pr panelRoute, peer func(*http.Request) error) (*routerHandler, *httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	logs := &[]string{}
	h := newRouterHandler(18888, newFakeRoutes(), newRouterTransport(), func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		*logs = append(*logs, format)
	})
	h.panel, h.peerCheck = pr, peer
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv, logs
}

func TestRouterSendsTheControlHostToThePanel(t *testing.T) {
	backend := newRecordingBackend(t, nil)
	_, router, _ := startPanelRouter(t, &fakePanelRoute{port: backend.port()}, func(*http.Request) error { return nil })
	got := routerGet(t, router, http.MethodGet, "control.nakama.localhost:18888", "/api/session", forwardingHeaders)
	if got.StatusCode != http.StatusOK || got.Body != "far end" {
		t.Fatalf("control panel through the router = %d %q", got.StatusCode, got.Body)
	}
	last := backend.lastRequest()
	if last.Host != "control.nakama.localhost:18888" {
		t.Errorf("the panel must get the browser's Host, got %q", last.Host)
	}
	for _, k := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
		if v := last.Header.Get(k); v != "" {
			t.Errorf("%s reached the panel: %q", k, v)
		}
	}
}

func TestRouterSaysHowToStartThePanel(t *testing.T) {
	_, router, _ := startPanelRouter(t, &fakePanelRoute{}, func(*http.Request) error { return nil })
	got := routerGet(t, router, http.MethodGet, "control.nakama.localhost:18888", "/", nil)
	if got.StatusCode != http.StatusBadGateway || got.Header.Get(routerErrorHeader) == "" ||
		!strings.Contains(got.Body, "<code>nakama</code>") || got.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("no panel = %d %v %s", got.StatusCode, got.Header, got.Body)
	}
}

func TestRouterRefusesOtherUsersAtTheControlHost(t *testing.T) {
	backend := newRecordingBackend(t, nil)
	pr := &fakePanelRoute{port: backend.port()}
	_, router, logs := startPanelRouter(t, pr, func(*http.Request) error { return errors.New("uid 1001") })
	got := routerGet(t, router, http.MethodGet, "control.nakama.localhost:18888", "/", nil)
	if got.StatusCode != http.StatusForbidden || backend.lastRequest() != nil || pr.resolved != 0 {
		t.Errorf("another user = %d, reached the panel: %v", got.StatusCode, backend.lastRequest() != nil)
	}
	if len(*logs) == 0 || !strings.Contains((*logs)[0], "refused a control panel request") {
		t.Errorf("logs = %v", *logs)
	}
	// The other addresses are not checked (unchanged behavior).
	if got := routerGet(t, router, http.MethodGet, "code-server.dev01.nakama.localhost:18888", "/", nil); got.StatusCode == http.StatusForbidden {
		t.Error("the peer check must apply to the control host only")
	}
}

func TestRouterForgetsAPanelThatDoesNotAnswer(t *testing.T) {
	pr := &fakePanelRoute{port: strconv.Itoa(freePort(t))}
	_, router, _ := startPanelRouter(t, pr, func(*http.Request) error { return nil })
	got := routerGet(t, router, http.MethodGet, "control.nakama.localhost:18888", "/", nil)
	if got.StatusCode != http.StatusBadGateway || !strings.Contains(got.Body, "did not answer") || pr.forgot != 1 {
		t.Errorf("a gone panel = %d forgot=%d %s", got.StatusCode, pr.forgot, got.Body)
	}
}

func TestLivePanelRouteChecksTheRouterPortAndCaches(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	var lookups int
	l := &livePanelRoute{
		now: func() time.Time { return now },
		lookup: func(context.Context) (*PanelState, *PanelStatus, error) {
			lookups++
			return &PanelState{Port: 4242}, &PanelStatus{RouterPort: 18888}, nil
		},
	}
	if _, err := l.resolve(context.Background(), 19000); !errors.Is(err, errPanelNotRunning) {
		t.Errorf("a panel for another router port must not be used: %v", err)
	}
	if p, err := l.resolve(context.Background(), 18888); p != "4242" || err != nil {
		t.Fatalf("resolve = %q, %v", p, err)
	}
	n := lookups
	if p, _ := l.resolve(context.Background(), 18888); p != "4242" || lookups != n {
		t.Errorf("a resolved port must be reused for %s", routerLookupTTL)
	}
	now = now.Add(routerLookupTTL)
	_, _ = l.resolve(context.Background(), 18888)
	if lookups != n+1 {
		t.Error("an expired answer must be looked up again")
	}
	l.forget()
	_, _ = l.resolve(context.Background(), 18888)
	if lookups != n+2 {
		t.Error("forget must drop the cached answer")
	}
	l.lookup = func(context.Context) (*PanelState, *PanelStatus, error) { return nil, nil, &NoPanelError{} }
	l.forget()
	if _, err := l.resolve(context.Background(), 18888); !errors.Is(err, errPanelNotRunning) {
		t.Errorf("no panel = %v", err)
	}
}

func TestWithPanelKeepsTheRouterAlive(t *testing.T) {
	none := func(context.Context) (int, error) { return 0, nil }
	if n, _ := withPanel(none, func(context.Context) bool { return true })(context.Background()); n != 1 {
		t.Errorf("a live panel must count: %d", n)
	}
	if n, _ := withPanel(none, func(context.Context) bool { return false })(context.Background()); n != 0 {
		t.Errorf("no panel: %d", n)
	}
	failing := errors.New("dev01 does not answer")
	if n, err := withPanel(func(context.Context) (int, error) { return 2, failing }, func(context.Context) bool { return true })(context.Background()); n != 3 || !errors.Is(err, failing) {
		t.Errorf("tunnels and panel: %d, %v", n, err)
	}
	if n, _ := withPanel(none, nil)(context.Background()); n != 0 {
		t.Errorf("no panel lookup: %d", n)
	}
}

func TestRouterStaysWhileAPanelRuns(t *testing.T) {
	withTunnelStateDir(t)
	prevCheck, prevExit := routerIdleCheckInterval, routerIdleExit
	routerIdleCheckInterval, routerIdleExit = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { routerIdleCheckInterval, routerIdleExit = prevCheck, prevExit })

	tp := startTestPanel(t, 18888, nil)
	var stopped atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	logger := &memLogger{}
	go func() {
		defer close(done)
		watchRouterIdle(ctx, withPanel(func(context.Context) (int, error) { return 0, nil }, panelAlive),
			func() { stopped.Store(true) }, logger)
	}()
	time.Sleep(300 * time.Millisecond)
	if stopped.Load() {
		t.Fatal("the router must not stop while a control panel runs")
	}
	tp.cancel()
	<-tp.done
	deadline := time.Now().Add(5 * time.Second)
	for !stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !stopped.Load() || !strings.Contains(logger.text(), "no tunnel daemon and no control panel") {
		t.Errorf("the router must stop once the panel is gone: stopped=%v\n%s", stopped.Load(), logger.text())
	}
}
