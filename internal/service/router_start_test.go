package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Tests of EnsureRouter and GetRouterStatus (Issue #124): starting the
// router, reusing it, replacing one of another version or port
// (decision 11), and the busy router port (decision 10). Not parallel:
// they use the state directory and NAKAMA_ROUTER_PORT.

// routerEnv fakes the router processes. Every router "process" is a
// state file plus an entry in live keyed by its token.
type routerEnv struct {
	svc     *Service
	clock   *fakeClock
	live    map[string]*RouterStatus
	spawned []int
	stopped []int
	busy    map[string]bool
	// spawnVersion is the version a spawned router reports; spawnsNothing
	// makes spawn start a router that never comes up.
	spawnVersion  string
	spawnsNothing bool
}

func newRouterEnv(t *testing.T) *routerEnv {
	t.Helper()
	withTunnelStateDir(t)
	t.Setenv(RouterPortEnv, "")
	env := &routerEnv{
		svc:          NewService(nil),
		clock:        &fakeClock{t: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)},
		live:         map[string]*RouterStatus{},
		busy:         map[string]bool{},
		spawnVersion: "v2",
	}
	env.svc.router = &routerHooks{
		spawn: func(port int) (int, error) {
			env.spawned = append(env.spawned, port)
			if env.spawnsNothing {
				return 99, nil
			}
			env.run(t, port, env.spawnVersion, "tok-"+env.spawnVersion+"-"+time.Now().String())
			return 99, nil
		},
		query: func(_ context.Context, st *RouterState, _ time.Duration) (*RouterStatus, error) {
			if s, ok := env.live[st.Token]; ok {
				return s, nil
			}
			return nil, errors.New("connection refused")
		},
		requestStop: func(_ context.Context, st *RouterState, _ time.Duration) error {
			s, ok := env.live[st.Token]
			if !ok {
				return errors.New("connection refused")
			}
			env.stopped = append(env.stopped, s.Port)
			delete(env.live, st.Token)
			return removeRouterStateOwned(st.Token)
		},
		listenLocal: func(port string) error {
			if env.busy[port] {
				return errors.New("address already in use")
			}
			return nil
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			env.clock.advance(d)
			return ctx.Err()
		},
		now:     env.clock.now,
		version: "v2",
	}
	return env
}

// run records a running router of version on port.
func (e *routerEnv) run(t *testing.T, port int, version, token string) {
	t.Helper()
	if err := writeRouterState(&RouterState{PID: 4242, Port: port, ControlPort: 1, Token: token, Version: version}); err != nil {
		t.Fatal(err)
	}
	e.live[token] = &RouterStatus{PID: 4242, Port: port, Version: version}
}

func TestEnsureRouterStartsOne(t *testing.T) {
	env := newRouterEnv(t)
	st, err := env.svc.EnsureRouter(context.Background())
	if err != nil {
		t.Fatalf("EnsureRouter: %v", err)
	}
	if len(env.spawned) != 1 || env.spawned[0] != DefaultRouterPort || st.Port != DefaultRouterPort || st.Version != "v2" {
		t.Errorf("spawned %v, status %+v", env.spawned, st)
	}
}

func TestEnsureRouterReusesTheRunningOne(t *testing.T) {
	env := newRouterEnv(t)
	env.run(t, DefaultRouterPort, "v2", "tok")
	if _, err := env.svc.EnsureRouter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(env.spawned) != 0 || len(env.stopped) != 0 {
		t.Errorf("a router of this version must be reused: spawned %v, stopped %v", env.spawned, env.stopped)
	}
}

// TestEnsureRouterReplacesAnotherVersion is decision 11 of Issue #124.
func TestEnsureRouterReplacesAnotherVersion(t *testing.T) {
	env := newRouterEnv(t)
	env.run(t, DefaultRouterPort, "v1", "tok-v1")
	st, err := env.svc.EnsureRouter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(env.stopped) != 1 || len(env.spawned) != 1 || st.Version != "v2" {
		t.Errorf("stopped %v, spawned %v, status %+v", env.stopped, env.spawned, st)
	}
}

func TestEnsureRouterMovesToANewPort(t *testing.T) {
	env := newRouterEnv(t)
	env.run(t, DefaultRouterPort, "v2", "tok")
	t.Setenv(RouterPortEnv, "19000")
	st, err := env.svc.EnsureRouter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(env.stopped) != 1 || env.stopped[0] != DefaultRouterPort || len(env.spawned) != 1 || env.spawned[0] != 19000 || st.Port != 19000 {
		t.Errorf("stopped %v, spawned %v, status %+v", env.stopped, env.spawned, st)
	}
}

// TestEnsureRouterKeepsTheRunningOneWhenTheNewPortIsTaken: a busy new
// port is reported before anything is stopped.
func TestEnsureRouterKeepsTheRunningOneWhenTheNewPortIsTaken(t *testing.T) {
	env := newRouterEnv(t)
	env.run(t, DefaultRouterPort, "v2", "tok")
	t.Setenv(RouterPortEnv, "19000")
	env.busy["19000"] = true
	_, err := env.svc.EnsureRouter(context.Background())
	var inUse *RouterPortInUseError
	if !errors.As(err, &inUse) || inUse.Port != 19000 {
		t.Fatalf("err = %v", err)
	}
	if len(env.stopped) != 0 || len(env.spawned) != 0 {
		t.Errorf("the running router must stay: stopped %v, spawned %v", env.stopped, env.spawned)
	}
}

func TestEnsureRouterCleansUpALeftover(t *testing.T) {
	env := newRouterEnv(t)
	if err := writeRouterState(&RouterState{PID: 1, Port: DefaultRouterPort, Token: "gone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.EnsureRouter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(env.spawned) != 1 || len(env.stopped) != 0 {
		t.Errorf("spawned %v, stopped %v", env.spawned, env.stopped)
	}
}

// TestEnsureRouterPortInUse is decision 10 of Issue #124: stop with the
// NAKAMA_ROUTER_PORT advice, never move elsewhere by itself.
func TestEnsureRouterPortInUse(t *testing.T) {
	env := newRouterEnv(t)
	env.busy["18888"] = true
	_, err := env.svc.EnsureRouter(context.Background())
	var inUse *RouterPortInUseError
	if !errors.As(err, &inUse) || inUse.Port != DefaultRouterPort {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"local port 18888 of the nakama router", "NAKAMA_ROUTER_PORT=18889"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	if len(env.spawned) != 0 {
		t.Error("no router may be started")
	}

	// A nakama tunnel forwarding the port is named.
	other := testTunnelState("dev02")
	other.Forwards = []string{"18888:3000"}
	if err := writeTunnelState(other); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.EnsureRouter(context.Background()); !errors.As(err, &inUse) || inUse.Holder != "dev02" ||
		!strings.Contains(err.Error(), "the nakama tunnel of dev02") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureRouterTimesOut(t *testing.T) {
	env := newRouterEnv(t)
	env.spawnsNothing = true
	_, err := env.svc.EnsureRouter(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), "nakama-router") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureRouterRejectsAnInvalidPort(t *testing.T) {
	env := newRouterEnv(t)
	t.Setenv(RouterPortEnv, "http")
	if _, err := env.svc.EnsureRouter(context.Background()); err == nil || !strings.Contains(err.Error(), RouterPortEnv) {
		t.Fatalf("err = %v", err)
	}
	if len(env.spawned) != 0 {
		t.Error("nothing may be started")
	}
}

func TestGetRouterStatus(t *testing.T) {
	env := newRouterEnv(t)
	if st, err := env.svc.GetRouterStatus(context.Background()); st != nil || err != nil {
		t.Errorf("none: %+v, %v", st, err)
	}
	env.run(t, DefaultRouterPort, "v1", "tok")
	if st, err := env.svc.GetRouterStatus(context.Background()); err != nil || st == nil || st.Version != "v1" {
		t.Errorf("running: %+v, %v", st, err)
	}
	delete(env.live, "tok")
	if st, err := env.svc.GetRouterStatus(context.Background()); st != nil || err != nil {
		t.Errorf("gone: %+v, %v", st, err)
	}
	if _, err := readRouterState(); !isNoRouter(err) {
		t.Errorf("the leftover state must be removed: %v", err)
	}
}

// TestGetTunnelStatusAddsRouterURLs: `tunnel status` shows the router
// address of the forwards the router serves.
func TestGetTunnelStatusAddsRouterURLs(t *testing.T) {
	env := newRouterEnv(t)
	tenv := newTunnelTestEnv(t)
	tenv.svc.router = env.svc.router
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatal(err)
	}
	forwards := func() *TunnelStatus {
		return &TunnelStatus{Name: "dev01", Forwards: []ForwardStatus{
			{Spec: "28080:8080", State: ForwardStateActive}, // a plain -L forward to code-server is the one used
			{Spec: "41001:5476", Purpose: ForwardPurposeKiroCrew, State: ForwardStateIdle},
			{Spec: "5432:db.internal:5432", State: ForwardStateActive},
		}}
	}
	tenv.queryResults = []queryResult{{status: forwards()}, {status: forwards()}}

	// No router: no addresses.
	st, err := tenv.svc.GetTunnelStatus(context.Background(), "dev01")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Forwards {
		if f.URL != "" {
			t.Errorf("without a router no URL may be shown: %+v", f)
		}
	}
	env.run(t, DefaultRouterPort, "v2", "tok")
	st, err = tenv.svc.GetTunnelStatus(context.Background(), "dev01")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{
		"http://code-server.dev01.nakama.localhost:18888/",
		"http://kirocrew.dev01.nakama.localhost:18888/",
		"",
	} {
		if got := st.Forwards[i].URL; got != want {
			t.Errorf("forward %s: URL %q, want %q", st.Forwards[i].Spec, got, want)
		}
	}
}
