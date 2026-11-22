package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// Tests of the router process (Issue #124, ADR 0029): its state file,
// control channel, port handling and the exit without tunnels. They use
// real loopback sockets and must not run in parallel (state directory,
// package timers).

// memLogger records daemon log lines.
type memLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *memLogger) Infof(format string, args ...any)  { l.add("info: "+format, args...) }
func (l *memLogger) Errorf(format string, args ...any) { l.add("error: "+format, args...) }
func (l *memLogger) Close() error                      { return nil }
func (l *memLogger) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")))
}

func (l *memLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// freePort returns a port that is free on 127.0.0.1 and ::1.
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		p, err := pickLoopbackPort()
		if err != nil {
			t.Fatal(err)
		}
		if probeLocalPort(p) == nil {
			n, _ := strconv.Atoi(p)
			return n
		}
	}
	t.Fatal("no free port")
	return 0
}

// testRouter is a router daemon run in the background.
type testRouter struct {
	// done is closed when the daemon returned; err is what it returned.
	done   chan struct{}
	err    error
	state  *RouterState
	logger *memLogger
	cancel context.CancelFunc
}

// startTestRouter runs the router daemon in the background and waits
// until it serves.
func startTestRouter(t *testing.T, port int, live func(context.Context) int) *testRouter {
	t.Helper()
	tr := &testRouter{done: make(chan struct{}), logger: &memLogger{}}
	ready := make(chan *RouterState, 1)
	ctx, cancel := context.WithCancel(context.Background())
	tr.cancel = cancel
	go func() {
		defer close(tr.done)
		tr.err = runRouterDaemon(ctx, RouterDaemonInput{Port: port}, routerDaemonDeps{
			logger:      tr.logger,
			routes:      newFakeRoutes(),
			liveTunnels: live,
			listen:      listenLoopback,
			version:     "v9.9.9-test",
			ready:       func(st *RouterState) { ready <- st },
		})
	}()
	select {
	case tr.state = <-ready:
	case <-tr.done:
		cancel()
		t.Fatalf("the router ended before serving: %v\n%s", tr.err, tr.logger.text())
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the router did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-tr.done:
		case <-time.After(10 * time.Second):
		}
	})
	return tr
}

func TestRouterDaemonServesAndStops(t *testing.T) {
	withTunnelStateDir(t)
	port := freePort(t)
	tr := startTestRouter(t, port, func(context.Context) int { return 1 })

	st, err := readRouterState()
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if st.Port != port || st.Version != "v9.9.9-test" || st.PID != os.Getpid() || st.Token == "" || st.ControlPort == 0 {
		t.Errorf("state = %+v", st)
	}
	path, _ := routerStatePath()
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("state file mode: %v, %v (it holds the control token)", info.Mode(), err)
	}
	if !strings.HasSuffix(path, "router.json") || strings.Contains(path, "tunnels") {
		t.Errorf("the state file %s must not be in the tunnel state directory (router is a valid instance name)", path)
	}

	status, err := queryRouterStatus(context.Background(), st, time.Second)
	if err != nil || status.Version != "v9.9.9-test" || status.Port != port {
		t.Fatalf("status = %+v, %v", status, err)
	}
	wrong := *st
	wrong.Token = "not-the-token"
	if _, err := queryRouterStatus(context.Background(), &wrong, time.Second); err == nil {
		t.Error("the control channel must refuse a wrong token")
	}

	// It serves the router on both loopback families.
	for _, addr := range []string{"127.0.0.1", "::1"} {
		if addr == "::1" && !ipv6LoopbackAvailable() {
			continue
		}
		req, _ := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(addr, strconv.Itoa(port))+"/", nil)
		req.Host = "evil.example:" + strconv.Itoa(port)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("%s: status %d", addr, resp.StatusCode)
		}
	}

	if err := requestRouterStop(context.Background(), st, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.done:
		if tr.err != nil {
			t.Errorf("stop: %v", tr.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the router did not stop")
	}
	if _, err := readRouterState(); !isNoRouter(err) {
		t.Errorf("the state file must be gone: %v", err)
	}
	if probeLocalPort(strconv.Itoa(port)) != nil {
		t.Error("the router port must be released")
	}
}

// TestRouterDaemonEndsWithoutTunnels: without a tunnel daemon there is
// nothing to route; the router ends after routerIdleExit, and not while
// a tunnel runs.
func TestRouterDaemonEndsWithoutTunnels(t *testing.T) {
	withTunnelStateDir(t)
	prevCheck, prevExit := routerIdleCheckInterval, routerIdleExit
	routerIdleCheckInterval, routerIdleExit = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { routerIdleCheckInterval, routerIdleExit = prevCheck, prevExit })

	var live atomic.Int32
	live.Store(1)
	tr := startTestRouter(t, freePort(t), func(context.Context) int { return int(live.Load()) })
	select {
	case <-tr.done:
		t.Fatalf("the router ended while a tunnel runs: %v", tr.err)
	case <-time.After(400 * time.Millisecond):
	}
	live.Store(0)
	select {
	case <-tr.done:
		if tr.err != nil {
			t.Errorf("idle exit: %v", tr.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the router kept running without tunnels")
	}
	if !strings.Contains(tr.logger.text(), "no tunnel daemon") {
		t.Errorf("the log must say why it ended:\n%s", tr.logger.text())
	}
	if _, err := readRouterState(); !isNoRouter(err) {
		t.Errorf("the state file must be gone: %v", err)
	}
}

func TestRouterDaemonPortTaken(t *testing.T) {
	withTunnelStateDir(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	logger := &memLogger{}
	err = runRouterDaemon(context.Background(), RouterDaemonInput{Port: l.Addr().(*net.TCPAddr).Port}, routerDaemonDeps{
		logger: logger, routes: newFakeRoutes(), liveTunnels: func(context.Context) int { return 1 }, listen: listenLoopback, version: "v",
	})
	var inUse *LocalPortInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("err = %v", err)
	}
	if _, err := readRouterState(); !isNoRouter(err) {
		t.Errorf("no state may be written: %v", err)
	}
	if !strings.Contains(logger.text(), "listen on port") {
		t.Errorf("log:\n%s", logger.text())
	}
}

func TestRouterDaemonReplacesALeftoverStateFile(t *testing.T) {
	withTunnelStateDir(t)
	// A router that is gone: nothing answers on its control port.
	if err := writeRouterState(&RouterState{PID: 1, Port: 1, ControlPort: closedPort(t), Token: "old"}); err != nil {
		t.Fatal(err)
	}
	tr := startTestRouter(t, freePort(t), func(context.Context) int { return 1 })
	st, err := readRouterState()
	if err != nil || st.Token != tr.state.Token {
		t.Fatalf("the leftover must be replaced: %+v, %v", st, err)
	}
}

func TestRouterDaemonLeavesARunningRouterAlone(t *testing.T) {
	withTunnelStateDir(t)
	// Another router answers its control channel (on another port).
	control := httptest.NewServer(newRouterControlHandler("tok", func() *RouterStatus {
		return &RouterStatus{PID: 4242, Port: 19999, Version: "v"}
	}, func() {}))
	defer control.Close()
	cport, _ := strconv.Atoi(control.URL[strings.LastIndex(control.URL, ":")+1:])
	if err := writeRouterState(&RouterState{PID: 4242, Port: 19999, ControlPort: cport, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	logger := &memLogger{}
	err := runRouterDaemon(context.Background(), RouterDaemonInput{Port: freePort(t)}, routerDaemonDeps{
		logger: logger, routes: newFakeRoutes(), liveTunnels: func(context.Context) int { return 1 }, listen: listenLoopback, version: "v",
	})
	if err == nil || !strings.Contains(err.Error(), "another nakama router (pid 4242)") {
		t.Fatalf("err = %v", err)
	}
	st, rerr := readRouterState()
	if rerr != nil || st.Token != "tok" {
		t.Errorf("the running router's state must stay: %+v, %v", st, rerr)
	}
}

func TestRouterControlHandlerMethods(t *testing.T) {
	stopped := make(chan struct{}, 1)
	h := newRouterControlHandler("tok", func() *RouterStatus { return &RouterStatus{Port: 1} }, func() { stopped <- struct{}{} })
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{http.MethodGet, "/status", "tok", 200},
		{http.MethodPost, "/status", "tok", 405},
		{http.MethodGet, "/status", "", 401},
		{http.MethodGet, "/stop", "tok", 405},
		{http.MethodPost, "/stop", "bad", 401},
		{http.MethodPost, "/stop", "tok", 200},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s (%q) = %d, want %d", tc.method, tc.path, tc.token, rec.Code, tc.want)
		}
		if tc.path == "/status" && tc.want == 200 {
			var st RouterStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Port != 1 {
				t.Errorf("status body %q", rec.Body.String())
			}
		}
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Error("POST /stop must stop the router")
	}
}

// closedPort returns a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// TestRouterDaemonRoutesToATunnelForward runs the whole local path: the
// router process, the tunnel state file and the daemon's /status, and
// the forward's local port (a test server standing in for it).
func TestRouterDaemonRoutesToATunnelForward(t *testing.T) {
	withTunnelStateDir(t)
	forward := newRecordingBackend(t, nil)
	daemon := httptest.NewServer(newTunnelControlHandler("tok-abc", tunnelControl{
		status: func() *TunnelStatus {
			return &TunnelStatus{Name: "dev01", Forwards: []ForwardStatus{
				{Spec: forward.port() + ":8080", Purpose: ForwardPurposeCodeServer, State: ForwardStateActive},
			}}
		},
		stop: func() {},
		add:  func(ForwardSpec) (ForwardStatus, error) { return ForwardStatus{}, errors.New("unused") },
	}))
	defer daemon.Close()
	st := testTunnelState("dev01")
	st.ControlPort, _ = strconv.Atoi(daemon.URL[strings.LastIndex(daemon.URL, ":")+1:])
	if err := writeTunnelState(st); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	tr := &testRouter{done: make(chan struct{}), logger: &memLogger{}}
	ready := make(chan *RouterState, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(tr.done)
		tr.err = runRouterDaemon(ctx, RouterDaemonInput{Port: port}, routerDaemonDeps{
			logger: tr.logger, routes: newTunnelRoutes(), liveTunnels: countLiveTunnels,
			listen: listenLoopback, version: "v", ready: func(st *RouterState) { ready <- st },
		})
	}()
	defer func() { cancel(); <-tr.done }()
	select {
	case <-ready:
	case <-tr.done:
		t.Fatalf("router: %v\n%s", tr.err, tr.logger.text())
	}
	if n := countLiveTunnels(context.Background()); n != 1 {
		t.Errorf("live tunnels = %d", n)
	}

	host := RouterHost(ForwardPurposeCodeServer, "dev01", port)
	if err := httpHealthCheck(context.Background(), net.JoinHostPort("localhost", strconv.Itoa(port)), host, "/healthz"); err != nil {
		t.Fatalf("through the router: %v", err)
	}
	if got := forward.lastRequest(); got == nil || got.Host != host || got.URL.Path != "/healthz" {
		t.Errorf("the forward got %+v", got)
	}
	// Kiro Crew has no forward on this tunnel: the router says so.
	err := httpHealthCheck(context.Background(), net.JoinHostPort("localhost", strconv.Itoa(port)),
		RouterHost(ForwardPurposeKiroCrew, "dev01", port), "/api/health")
	if err == nil || !strings.Contains(err.Error(), "No kirocrew forward for dev01") {
		t.Errorf("missing forward: %v", err)
	}
}
