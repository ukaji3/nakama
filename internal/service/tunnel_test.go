package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

func TestParseForwardSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    ForwardSpec
		wantErr bool
	}{
		{name: "instance L:R", spec: "8080:8080", want: ForwardSpec{LocalPort: "8080", RemotePort: "8080"}},
		{name: "remote host L:host:R", spec: "5432:db.internal:5433", want: ForwardSpec{LocalPort: "5432", RemoteHost: "db.internal", RemotePort: "5433"}},
		{name: "too few parts", spec: "8080", wantErr: true},
		{name: "too many parts", spec: "1:2:3:4", wantErr: true},
		{name: "bad local port", spec: "0:8080", wantErr: true},
		{name: "bad remote port", spec: "8080:70000", wantErr: true},
		{name: "non-numeric port", spec: "abc:8080", wantErr: true},
		{name: "empty host", spec: "8080::8080", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseForwardSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseForwardSpecs(t *testing.T) {
	if _, err := ParseForwardSpecs(nil); err == nil {
		t.Error("empty specs must be rejected")
	}
	if _, err := ParseForwardSpecs([]string{"8080:8080", "8080:9090"}); err == nil ||
		!strings.Contains(err.Error(), "duplicate local port") {
		t.Errorf("duplicate local ports must be rejected, got %v", err)
	}
	got, err := ParseForwardSpecs([]string{"8080:8080", "5432:db.internal:5432"})
	if err != nil {
		t.Fatalf("ParseForwardSpecs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestForwardSpecStringAndDescribe(t *testing.T) {
	f := ForwardSpec{LocalPort: "8080", RemotePort: "8080"}
	if f.String() != "8080:8080" {
		t.Errorf("String = %q", f.String())
	}
	if got := f.Describe("dev01"); got != "localhost:8080 -> dev01:8080" {
		t.Errorf("Describe = %q", got)
	}
	g := ForwardSpec{LocalPort: "5432", RemoteHost: "db.internal", RemotePort: "5433"}
	if g.String() != "5432:db.internal:5433" {
		t.Errorf("String = %q", g.String())
	}
	if got := g.Describe("dev01"); got != "localhost:5432 -> db.internal (via dev01):5433" {
		t.Errorf("Describe = %q", got)
	}
}

func TestPortForwardDocument(t *testing.T) {
	doc, params := portForwardDocument(ForwardSpec{LocalPort: "8080", RemotePort: "9090"})
	if aws.ToString(doc) != "AWS-StartPortForwardingSession" {
		t.Errorf("doc = %q", aws.ToString(doc))
	}
	if params["portNumber"][0] != "9090" || params["localPortNumber"][0] != "8080" {
		t.Errorf("params = %v", params)
	}
	if _, ok := params["host"]; ok {
		t.Error("host must be absent without RemoteHost")
	}

	doc, params = portForwardDocument(ForwardSpec{LocalPort: "5432", RemoteHost: "db.internal", RemotePort: "5433"})
	if aws.ToString(doc) != "AWS-StartPortForwardingSessionToRemoteHost" {
		t.Errorf("doc = %q", aws.ToString(doc))
	}
	if params["host"][0] != "db.internal" {
		t.Errorf("params = %v", params)
	}
}

// withTunnelStateDir points the state directory at a temp dir for the
// duration of the test. Tests using it must not run in parallel.
func withTunnelStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := tunnelStateDirOverride
	tunnelStateDirOverride = dir
	t.Cleanup(func() { tunnelStateDirOverride = prev })
	return dir
}

func testTunnelState(name string) *TunnelState {
	return &TunnelState{
		Name:        name,
		PID:         4242,
		ControlPort: 45678,
		Token:       "tok-abc",
		Forwards:    []string{"8080:8080"},
		StartedAt:   time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC),
	}
}

func TestTunnelStateFileLifecycle(t *testing.T) {
	dir := withTunnelStateDir(t)

	if _, err := readTunnelState("dev01"); err == nil {
		t.Fatal("read of missing state must fail")
	} else {
		var nt *NoTunnelError
		if !errors.As(err, &nt) {
			t.Fatalf("want *NoTunnelError, got %T: %v", err, err)
		}
	}

	st := testTunnelState("dev01")
	if err := writeTunnelState(st); err != nil {
		t.Fatalf("writeTunnelState: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "dev01.json"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("state file mode = %o, want 600", perm)
		}
	}

	// O_EXCL: a second create for the same name must fail.
	if err := writeTunnelState(st); err == nil {
		t.Error("second writeTunnelState must fail (O_EXCL)")
	}

	got, err := readTunnelState("dev01")
	if err != nil {
		t.Fatalf("readTunnelState: %v", err)
	}
	if got.PID != st.PID || got.ControlPort != st.ControlPort || got.Token != st.Token ||
		len(got.Forwards) != 1 || got.Forwards[0] != "8080:8080" {
		t.Errorf("round trip mismatch: %+v", got)
	}

	if err := removeTunnelState("dev01"); err != nil {
		t.Fatalf("removeTunnelState: %v", err)
	}
	// Removing again is not an error (benign race with the daemon).
	if err := removeTunnelState("dev01"); err != nil {
		t.Fatalf("second removeTunnelState: %v", err)
	}
}

func TestTunnelControlHandlerAuthAndRoutes(t *testing.T) {
	stopped := make(chan struct{})
	var stopOnce sync.Once
	h := newTunnelControlHandler("secret-token", func() *TunnelStatus {
		return &TunnelStatus{Name: "dev01", PID: 1, Forwards: []ForwardStatus{{Spec: "8080:8080", State: ForwardStateActive}}}
	}, func() { stopOnce.Do(func() { close(stopped) }) })
	srv := httptest.NewServer(h)
	defer srv.Close()

	do := func(method, path, token string) int {
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if code := do(http.MethodGet, "/status", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", code)
	}
	if code := do(http.MethodGet, "/status", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", code)
	}
	if code := do(http.MethodPost, "/status", "secret-token"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /status: status = %d, want 405", code)
	}
	if code := do(http.MethodGet, "/stop", "secret-token"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET /stop: status = %d, want 405", code)
	}
	select {
	case <-stopped:
		t.Fatal("stop must not have been requested yet")
	default:
	}

	// Authorized status via the real client helper.
	st := &TunnelState{ControlPort: srv.Listener.Addr().(*net.TCPAddr).Port, Token: "secret-token"}
	status, err := queryTunnelStatus(context.Background(), st, 2*time.Second)
	if err != nil {
		t.Fatalf("queryTunnelStatus: %v", err)
	}
	if status.Name != "dev01" || len(status.Forwards) != 1 || status.Forwards[0].State != ForwardStateActive {
		t.Errorf("status = %+v", status)
	}

	// Authorized stop triggers the callback.
	if err := requestTunnelStop(context.Background(), st, 2*time.Second); err != nil {
		t.Fatalf("requestTunnelStop: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop was not requested within 2s")
	}
}

func TestForwardTrackerTransitions(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	forwards := []ForwardSpec{
		{LocalPort: "8080", RemotePort: "8080"},
		{LocalPort: "5432", RemoteHost: "db.internal", RemotePort: "5432"},
	}
	tr := newForwardTracker("dev01", forwards, clock.now)

	snap := tr.snapshot()
	if snap[0].State != ForwardStateConnecting || snap[1].State != ForwardStateConnecting {
		t.Fatalf("initial states = %+v", snap)
	}
	if len(tr.activeSessionIDs()) != 0 {
		t.Error("no active sessions initially")
	}

	tr.observer(0).SessionStarted("sess-a")
	tr.observer(1).SessionStarted("sess-b")
	snap = tr.snapshot()
	if snap[0].State != ForwardStateActive || snap[0].SessionID != "sess-a" {
		t.Errorf("forward 0 = %+v", snap[0])
	}
	if ids := tr.activeSessionIDs(); len(ids) != 2 {
		t.Errorf("activeSessionIDs = %v", ids)
	}

	tr.observer(0).SessionRetrying(2, 5, errConnectionLost)
	snap = tr.snapshot()
	if snap[0].State != ForwardStateReconnecting || snap[0].Attempts != 2 {
		t.Errorf("forward 0 after retry = %+v", snap[0])
	}
	if !strings.Contains(snap[0].Detail, "attempt 2/5") {
		t.Errorf("detail = %q", snap[0].Detail)
	}
	if ids := tr.activeSessionIDs(); len(ids) != 1 {
		t.Errorf("activeSessionIDs after retry = %v", ids)
	}

	// Auth-classified death gets the login + restart advisory with the
	// instance name filled in (copy-pastable).
	authErr := &smithy.GenericAPIError{Code: "ExpiredTokenException", Message: "expired"}
	tr.setDead(0, authErr)
	snap = tr.snapshot()
	if snap[0].State != ForwardStateDead ||
		!strings.Contains(snap[0].Detail, "nakama aws login") ||
		!strings.Contains(snap[0].Detail, "nakama aws tunnel restart dev01") {
		t.Errorf("forward 0 after auth death = %+v", snap[0])
	}

	// nil reason (normal session end).
	tr.setDead(1, nil)
	snap = tr.snapshot()
	if snap[1].State != ForwardStateDead || snap[1].Detail != "session ended" {
		t.Errorf("forward 1 after nil death = %+v", snap[1])
	}
	if ids := tr.activeSessionIDs(); len(ids) != 0 {
		t.Errorf("activeSessionIDs after deaths = %v", ids)
	}
}

// tunnelTestEnv wires a Service with fake SSM and scripted tunnel
// hooks for the orchestration tests.
type tunnelTestEnv struct {
	svc   *Service
	ssm   *fakeConnectSSM
	clock *fakeClock

	spawned      []string // names passed to spawn
	queryResults []queryResult
	queries      int
	stopErr      error
	stopCalled   int
	portBusy     map[string]bool
}

type queryResult struct {
	status *TunnelStatus
	err    error
}

func newTunnelTestEnv(t *testing.T) *tunnelTestEnv {
	t.Helper()
	env := &tunnelTestEnv{
		ssm:      &fakeConnectSSM{instanceID: "i-0123456789abcdef0"},
		clock:    &fakeClock{t: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)},
		portBusy: map[string]bool{},
	}
	env.svc = NewService(&awsx.Clients{SSM: env.ssm, Region: "ap-northeast-1"})
	env.svc.tunnel = &tunnelHooks{
		spawn: func(name string, forwards []ForwardSpec) (int, error) {
			env.spawned = append(env.spawned, name)
			return 4242, nil
		},
		query: func(_ context.Context, st *TunnelState, _ time.Duration) (*TunnelStatus, error) {
			i := env.queries
			env.queries++
			if i >= len(env.queryResults) {
				return nil, errors.New("no scripted query result")
			}
			return env.queryResults[i].status, env.queryResults[i].err
		},
		requestStop: func(_ context.Context, st *TunnelState, _ time.Duration) error {
			env.stopCalled++
			if env.stopErr == nil {
				// Simulate the daemon's final shutdown step.
				_ = removeTunnelState(st.Name)
			}
			return env.stopErr
		},
		listenLocal: func(port string) error {
			if env.portBusy[port] {
				return errors.New("port busy: " + port)
			}
			return nil
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			env.clock.advance(d)
			return ctx.Err()
		},
		now: env.clock.now,
	}
	return env
}

func testForwards() []ForwardSpec {
	return []ForwardSpec{{LocalPort: "8080", RemotePort: "8080"}}
}

func TestStartTunnelAlreadyRunning(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	live := &TunnelStatus{Name: "dev01", PID: 4242}
	env.queryResults = []queryResult{{status: live}}

	_, err := env.svc.StartTunnel(context.Background(), "dev01", testForwards())
	var already *TunnelAlreadyRunningError
	if !errors.As(err, &already) {
		t.Fatalf("want TunnelAlreadyRunningError, got %v", err)
	}
	if already.Status != live {
		t.Error("error must carry the live status")
	}
	if len(env.spawned) != 0 {
		t.Error("no daemon must be spawned when one is already running")
	}
}

func TestStartTunnelCleansStaleStateAndStarts(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed stale state: %v", err)
	}
	ready := &TunnelStatus{Name: "dev01", PID: 4242, Forwards: []ForwardStatus{{Spec: "8080:8080", State: ForwardStateActive}}}
	env.queryResults = []queryResult{
		{err: errors.New("connection refused")}, // liveness: stale
		{status: ready},                         // readiness after spawn
	}
	// The spawn fake must recreate the state file like a real daemon.
	baseSpawn := env.svc.tunnel.spawn
	env.svc.tunnel.spawn = func(name string, forwards []ForwardSpec) (int, error) {
		if _, err := baseSpawn(name, forwards); err != nil {
			return -1, err
		}
		return 4242, writeTunnelState(testTunnelState(name))
	}

	st, err := env.svc.StartTunnel(context.Background(), "dev01", testForwards())
	if err != nil {
		t.Fatalf("StartTunnel: %v", err)
	}
	if st != ready {
		t.Errorf("status = %+v", st)
	}
	if len(env.spawned) != 1 {
		t.Errorf("spawned = %v", env.spawned)
	}
}

func TestStartTunnelInstanceNotFound(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	env.ssm.instanceID = ""
	_, err := env.svc.StartTunnel(context.Background(), "ghost", testForwards())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
	if len(env.spawned) != 0 {
		t.Error("no daemon must be spawned for a missing instance")
	}
}

func TestStartTunnelLocalPortBusy(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	env.portBusy["8080"] = true
	_, err := env.svc.StartTunnel(context.Background(), "dev01", testForwards())
	if err == nil || !strings.Contains(err.Error(), "8080") {
		t.Fatalf("want port-busy error, got %v", err)
	}
	if len(env.spawned) != 0 {
		t.Error("no daemon must be spawned when a port is busy")
	}
}

func TestStartTunnelReadinessTimeout(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	// Spawn succeeds but never writes a state file; every readiness
	// probe fails until the deadline passes (fake sleep advances the
	// fake clock).
	_, err := env.svc.StartTunnel(context.Background(), "dev01", testForwards())
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("want readiness timeout, got %v", err)
	}
}

func TestStartTunnelRejectsReservedAndInvalidInput(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	if _, err := env.svc.StartTunnel(context.Background(), "Bad_Name", testForwards()); err == nil {
		t.Error("invalid name must be rejected")
	}
	if _, err := env.svc.StartTunnel(context.Background(), "dev01", nil); err == nil {
		t.Error("empty forwards must be rejected")
	}
}

func TestStopTunnelGraceful(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	outcome, err := env.svc.StopTunnel(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("StopTunnel: %v", err)
	}
	if !outcome.WasRunning {
		t.Error("WasRunning = false, want true")
	}
	if env.stopCalled != 1 {
		t.Errorf("stopCalled = %d", env.stopCalled)
	}
	if _, err := readTunnelState("dev01"); err == nil {
		t.Error("state file must be gone after stop")
	}
}

func TestStopTunnelStaleDaemon(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	env.stopErr = errors.New("connection refused")
	outcome, err := env.svc.StopTunnel(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("StopTunnel: %v", err)
	}
	if outcome.WasRunning {
		t.Error("WasRunning = true, want false (daemon was gone)")
	}
	if _, err := readTunnelState("dev01"); err == nil {
		t.Error("stale state file must be cleaned up")
	}
}

func TestStopTunnelNotRunning(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	_, err := env.svc.StopTunnel(context.Background(), "dev01")
	var nt *NoTunnelError
	if !errors.As(err, &nt) {
		t.Fatalf("want *NoTunnelError, got %v", err)
	}
}

func TestGetTunnelStatusLiveAndStale(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	live := &TunnelStatus{Name: "dev01", PID: 4242}
	env.queryResults = []queryResult{{status: live}, {err: errors.New("refused")}}

	st, err := env.svc.GetTunnelStatus(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("GetTunnelStatus: %v", err)
	}
	if st != live {
		t.Errorf("status = %+v", st)
	}

	// Second query fails => stale cleanup + NoTunnelError.
	_, err = env.svc.GetTunnelStatus(context.Background(), "dev01")
	var nt *NoTunnelError
	if !errors.As(err, &nt) {
		t.Fatalf("want *NoTunnelError, got %v", err)
	}
	if _, err := readTunnelState("dev01"); err == nil {
		t.Error("stale state file must be cleaned up")
	}
}

// recordingObserver captures superviseSession lifecycle callbacks.
type recordingObserver struct {
	started  []string
	retrying []string
}

func (r *recordingObserver) SessionStarted(id string) { r.started = append(r.started, id) }
func (r *recordingObserver) SessionRetrying(attempt, maxRetries int, reason error) {
	r.retrying = append(r.retrying, reason.Error())
}

func TestSuperviseSessionNotifiesObserver(t *testing.T) {
	// disconnect once (74), then clean exit: observer sees two session
	// starts and one retry.
	env := newConnectTestEnv(t, []sessionRun{
		{code: DisconnectExitCode, alive: time.Second},
		{code: 0, alive: time.Second},
	}, nil)
	obs := &recordingObserver{}
	docName, params := portForwardDocument(ForwardSpec{LocalPort: "8080", RemotePort: "8080"})
	err := env.svc.superviseSession(context.Background(), superviseSessionInput{
		InstanceID:   "i-0123456789abcdef0",
		DocumentName: docName,
		Parameters:   params,
		MaxRetries:   5,
		Observer:     obs,
	})
	if err != nil {
		t.Fatalf("superviseSession: %v", err)
	}
	if len(obs.started) != 2 {
		t.Errorf("started = %v, want 2 entries", obs.started)
	}
	if len(obs.retrying) != 1 || !strings.Contains(obs.retrying[0], "connection lost") {
		t.Errorf("retrying = %v", obs.retrying)
	}
}

func TestValidateNewInstanceNameReserved(t *testing.T) {
	for _, name := range []string{"start", "stop", "status", "restart"} {
		if err := ValidateNewInstanceName(name); err == nil {
			t.Errorf("reserved name %q must be rejected", name)
		}
		// Reserved names still pass the general (existing-instance)
		// validation: only creation is restricted.
		if err := ValidateInstanceName(name); err != nil {
			t.Errorf("ValidateInstanceName(%q) = %v, want nil", name, err)
		}
	}
	if err := ValidateNewInstanceName("dev01"); err != nil {
		t.Errorf("ValidateNewInstanceName(dev01) = %v", err)
	}
	if err := ValidateNewInstanceName("Bad_Name"); err == nil {
		t.Error("pattern violations must still be rejected")
	}
}

// TestRemoveTunnelStateOwned pins the compare-and-delete semantics:
// only the daemon generation that owns the file (token match) may
// remove it; a foreign (newer) file is left untouched.
func TestRemoveTunnelStateOwned(t *testing.T) {
	withTunnelStateDir(t)

	// Missing file: no error.
	if err := removeTunnelStateOwned("dev01", "tok-abc"); err != nil {
		t.Fatalf("missing file: %v", err)
	}

	// Token match: removed.
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := removeTunnelStateOwned("dev01", "tok-abc"); err != nil {
		t.Fatalf("owned removal: %v", err)
	}
	if _, err := readTunnelState("dev01"); err == nil {
		t.Error("state file must be gone after owned removal")
	}

	// Token mismatch (a newer daemon rewrote the file): preserved.
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := removeTunnelStateOwned("dev01", "tok-of-old-daemon"); err != nil {
		t.Fatalf("foreign removal attempt: %v", err)
	}
	if _, err := readTunnelState("dev01"); err != nil {
		t.Errorf("state file of the newer daemon was deleted: %v", err)
	}
}

func TestRestartTunnelRestartsWithRecordedForwards(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	// Seed a live daemon's state with two recorded forwards.
	st := testTunnelState("dev01")
	st.Forwards = []string{"8080:8080", "5432:db.internal:5432"}
	if err := writeTunnelState(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	// Capture the specs handed to spawn and recreate the state file
	// like a real daemon would.
	var spawnedForwards []string
	baseSpawn := env.svc.tunnel.spawn
	env.svc.tunnel.spawn = func(name string, forwards []ForwardSpec) (int, error) {
		for _, f := range forwards {
			spawnedForwards = append(spawnedForwards, f.String())
		}
		if _, err := baseSpawn(name, forwards); err != nil {
			return -1, err
		}
		return 4242, writeTunnelState(testTunnelState(name))
	}
	ready := &TunnelStatus{Name: "dev01", PID: 4242}
	env.queryResults = []queryResult{{status: ready}} // readiness after respawn

	got, err := env.svc.RestartTunnel(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("RestartTunnel: %v", err)
	}
	if got != ready {
		t.Errorf("status = %+v", got)
	}
	if env.stopCalled != 1 {
		t.Errorf("stopCalled = %d, want 1 (graceful stop before respawn)", env.stopCalled)
	}
	want := []string{"8080:8080", "5432:db.internal:5432"}
	if len(spawnedForwards) != len(want) {
		t.Fatalf("spawned forwards = %v, want %v", spawnedForwards, want)
	}
	for i := range want {
		if spawnedForwards[i] != want[i] {
			t.Errorf("spawned forward %d = %q, want %q", i, spawnedForwards[i], want[i])
		}
	}
}

func TestRestartTunnelStaleDaemonStillRestarts(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	// The daemon is gone: stop's control request fails, the stale state
	// is cleaned up, and the restart proceeds from the recorded specs.
	env.stopErr = errors.New("connection refused")
	baseSpawn := env.svc.tunnel.spawn
	env.svc.tunnel.spawn = func(name string, forwards []ForwardSpec) (int, error) {
		if _, err := baseSpawn(name, forwards); err != nil {
			return -1, err
		}
		return 4242, writeTunnelState(testTunnelState(name))
	}
	ready := &TunnelStatus{Name: "dev01", PID: 4242}
	env.queryResults = []queryResult{{status: ready}}

	got, err := env.svc.RestartTunnel(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("RestartTunnel: %v", err)
	}
	if got != ready {
		t.Errorf("status = %+v", got)
	}
	if len(env.spawned) != 1 {
		t.Errorf("spawned = %v", env.spawned)
	}
}

func TestRestartTunnelNoState(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	_, err := env.svc.RestartTunnel(context.Background(), "dev01")
	var nt *NoTunnelError
	if !errors.As(err, &nt) {
		t.Fatalf("want *NoTunnelError, got %T: %v", err, err)
	}
	if env.stopCalled != 0 || len(env.spawned) != 0 {
		t.Error("nothing must be stopped or spawned without a state file")
	}
}

func TestRestartTunnelInvalidRecordedSpecs(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	st := testTunnelState("dev01")
	st.Forwards = []string{"not-a-spec"}
	if err := writeTunnelState(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	_, err := env.svc.RestartTunnel(context.Background(), "dev01")
	if err == nil || !strings.Contains(err.Error(), "invalid forwards") {
		t.Fatalf("want invalid-forwards error, got %v", err)
	}
	if env.stopCalled != 0 {
		t.Error("the running daemon must not be stopped when the recorded specs are unusable")
	}
}
