package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// browserEnv extends the tunnel orchestration fakes with the add and
// HTTP-probe hooks EnsureBrowserForward uses.
type browserEnv struct {
	*tunnelTestEnv
	added      []ForwardSpec
	addErr     error
	spawnedFwd [][]ForwardSpec
	probeErrs  []error // per probe; beyond the slice => success
	probes     []string
}

func newBrowserEnv(t *testing.T) *browserEnv {
	t.Helper()
	withTunnelStateDir(t)
	env := &browserEnv{tunnelTestEnv: newTunnelTestEnv(t)}
	h := env.svc.tunnel
	h.spawn = func(name string, forwards []ForwardSpec) (int, error) {
		env.spawned = append(env.spawned, name)
		env.spawnedFwd = append(env.spawnedFwd, forwards)
		st := testTunnelState(name)
		st.Forwards = storedForwards(forwards)
		return 4242, writeTunnelState(st)
	}
	h.addForward = func(_ context.Context, _ *TunnelState, f ForwardSpec, _ time.Duration) (*ForwardStatus, error) {
		if env.addErr != nil {
			return nil, env.addErr
		}
		env.added = append(env.added, f)
		return &ForwardStatus{Spec: f.String(), Purpose: f.Purpose, State: ForwardStateConnecting}, nil
	}
	h.probeHTTP = func(_ context.Context, port, path string) error {
		env.probes = append(env.probes, port+path)
		i := len(env.probes) - 1
		if i < len(env.probeErrs) {
			return env.probeErrs[i]
		}
		return nil
	}
	return env
}

// liveDaemon seeds a running daemon for dev01 whose status lists
// forwards; every later query answers with it.
func (e *browserEnv) liveDaemon(t *testing.T, forwards ...ForwardStatus) {
	t.Helper()
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatal(err)
	}
	status := &TunnelStatus{Name: "dev01", PID: 4242, Forwards: forwards}
	e.svc.tunnel.query = func(context.Context, *TunnelState, time.Duration) (*TunnelStatus, error) {
		return status, nil
	}
}

func TestEnsureBrowserForwardStartsADaemon(t *testing.T) {
	for _, tc := range []struct {
		purpose  ForwardPurpose
		want     string
		probe    string
		origin   string
		explicit int
	}{
		{ForwardPurposeCodeServer, "code-server=8080:8080", "8080/healthz", "http://localhost:8080", 0},
		{ForwardPurposeKiroCrew, "kirocrew=5476:5476", "5476/api/health", "http://localhost:5476", 0},
		{ForwardPurposeCodeServer, "code-server=18080:8080", "18080/healthz", "http://localhost:18080", 18080},
	} {
		t.Run(tc.want, func(t *testing.T) {
			env := newBrowserEnv(t)
			ready := &TunnelStatus{Name: "dev01", PID: 4242}
			env.queryResults = []queryResult{{status: ready}}
			bf, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: tc.purpose, LocalPort: tc.explicit})
			if err != nil {
				t.Fatalf("EnsureBrowserForward: %v", err)
			}
			if len(env.spawnedFwd) != 1 || len(env.spawnedFwd[0]) != 1 || env.spawnedFwd[0][0].Stored() != tc.want {
				t.Errorf("spawned forwards = %v, want [%s]", env.spawnedFwd, tc.want)
			}
			if bf.Origin != tc.origin || bf.Reused {
				t.Errorf("forward = %+v", bf)
			}
			if len(env.probes) != 1 || env.probes[0] != tc.probe {
				t.Errorf("probes = %v, want [%s]", env.probes, tc.probe)
			}
		})
	}
}

func TestEnsureBrowserForwardReusesAMatchingForward(t *testing.T) {
	env := newBrowserEnv(t)
	// A plain -L forward to code-server's port counts.
	env.liveDaemon(t, ForwardStatus{Spec: "28080:8080", State: ForwardStateIdle})
	bf, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	if err != nil {
		t.Fatalf("EnsureBrowserForward: %v", err)
	}
	if !bf.Reused || bf.LocalPort != 28080 || bf.Origin != "http://localhost:28080" {
		t.Errorf("forward = %+v", bf)
	}
	if len(env.added) != 0 || len(env.spawned) != 0 {
		t.Errorf("nothing must be added or spawned: added=%v spawned=%v", env.added, env.spawned)
	}
}

func TestEnsureBrowserForwardRejectsAnotherExplicitPort(t *testing.T) {
	env := newBrowserEnv(t)
	env.liveDaemon(t, ForwardStatus{Spec: "8080:8080", Purpose: ForwardPurposeCodeServer, State: ForwardStateActive})
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer, LocalPort: 18080})
	var mismatch *ErrLocalPortMismatch
	if !errors.As(err, &mismatch) || mismatch.Existing != "8080" {
		t.Fatalf("err = %v, want ErrLocalPortMismatch", err)
	}
	if !strings.Contains(err.Error(), "nakama aws tunnel stop dev01") {
		t.Errorf("message = %q", err)
	}
}

func TestEnsureBrowserForwardAddsToTheRunningDaemon(t *testing.T) {
	env := newBrowserEnv(t)
	// The code-server forward died (e.g. the instance was replaced):
	// open adds a new one, which the daemon swaps in.
	env.liveDaemon(t,
		ForwardStatus{Spec: "5432:db.internal:5432", State: ForwardStateActive},
		ForwardStatus{Spec: "5476:5476", Purpose: ForwardPurposeKiroCrew, State: ForwardStateDead})
	bf, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeKiroCrew})
	if err != nil {
		t.Fatalf("EnsureBrowserForward: %v", err)
	}
	if len(env.added) != 1 || env.added[0].Stored() != "kirocrew=5476:5476" {
		t.Errorf("added = %v", env.added)
	}
	if bf.Reused || bf.LocalPort != 5476 {
		t.Errorf("forward = %+v", bf)
	}
	if len(env.spawned) != 0 {
		t.Errorf("no daemon must be spawned: %v", env.spawned)
	}
}

func TestEnsureBrowserForwardOldDaemon(t *testing.T) {
	env := newBrowserEnv(t)
	env.liveDaemon(t, ForwardStatus{Spec: "5432:5432", State: ForwardStateActive})
	env.addErr = ErrTunnelDaemonTooOld
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	if !errors.Is(err, ErrTunnelDaemonTooOld) || !strings.Contains(err.Error(), "nakama aws tunnel restart dev01") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureBrowserForwardPortHeldByAnotherTunnel(t *testing.T) {
	env := newBrowserEnv(t)
	other := testTunnelState("dev02")
	other.Forwards = []string{"code-server=8080:8080"}
	if err := writeTunnelState(other); err != nil {
		t.Fatal(err)
	}
	env.portBusy["8080"] = true
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	var inUse *LocalPortInUseError
	if !errors.As(err, &inUse) || inUse.Holder != "dev02" {
		t.Fatalf("err = %v, want the port named as dev02's", err)
	}
	if !strings.Contains(err.Error(), "in use by the nakama tunnel of dev02") {
		t.Errorf("message = %q", err)
	}
	if len(env.spawned) != 0 {
		t.Error("no daemon must be spawned")
	}
}

func TestEnsureBrowserForwardPortHeldBySomethingElse(t *testing.T) {
	env := newBrowserEnv(t)
	env.portBusy["8080"] = true
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	var inUse *LocalPortInUseError
	if !errors.As(err, &inUse) || inUse.Holder != "" || inUse.Port != "8080" {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureBrowserForwardWaitsUntilItAnswers(t *testing.T) {
	env := newBrowserEnv(t)
	env.liveDaemon(t, ForwardStatus{Spec: "8080:8080", Purpose: ForwardPurposeCodeServer, State: ForwardStateReconnecting})
	env.probeErrs = []error{errors.New("connection reset"), errors.New("EOF")}
	if _, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer}); err != nil {
		t.Fatalf("EnsureBrowserForward: %v", err)
	}
	if len(env.probes) != 3 {
		t.Errorf("probes = %v, want 3", env.probes)
	}
}

func TestEnsureBrowserForwardTimesOutWithTheDaemonsReason(t *testing.T) {
	env := newBrowserEnv(t)
	env.liveDaemon(t, ForwardStatus{Spec: "8080:8080", Purpose: ForwardPurposeCodeServer, State: ForwardStateReconnecting,
		Detail: "attempt 4 (next in 8s): StartSession: TargetNotConnected", LastErrorCode: "TargetNotConnected"})
	env.probeErrs = make([]error, 1000)
	for i := range env.probeErrs {
		env.probeErrs[i] = errors.New("connection refused")
	}
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	if err == nil {
		t.Fatal("want a timeout")
	}
	for _, want := range []string{"did not become usable within", "attempt 4", "SSM agent is not connected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestEnsureBrowserForwardStopsOnADeadForward(t *testing.T) {
	env := newBrowserEnv(t)
	env.liveDaemon(t, ForwardStatus{Spec: "5432:5432", State: ForwardStateActive})
	// The new forward is reported dead right away.
	added := false
	env.svc.tunnel.addForward = func(_ context.Context, _ *TunnelState, f ForwardSpec, _ time.Duration) (*ForwardStatus, error) {
		added = true
		return &ForwardStatus{Spec: f.String(), State: ForwardStateConnecting}, nil
	}
	env.svc.tunnel.query = func(context.Context, *TunnelState, time.Duration) (*TunnelStatus, error) {
		fs := []ForwardStatus{{Spec: "5432:5432", State: ForwardStateActive}}
		if added {
			fs = append(fs, ForwardStatus{Spec: "8080:8080", State: ForwardStateDead, Detail: errInstanceGone.Error()})
		}
		return &TunnelStatus{Name: "dev01", Forwards: fs}, nil
	}
	env.probeErrs = []error{errors.New("refused"), errors.New("refused")}
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("err = %v, want the dead forward's reason", err)
	}
}

func TestEnsureBrowserForwardValidatesInput(t *testing.T) {
	env := newBrowserEnv(t)
	for _, in := range []BrowserForwardInput{
		{Name: "Bad_Name", Purpose: ForwardPurposeCodeServer},
		{Name: "dev01", Purpose: ForwardPurposeNone},
		{Name: "dev01", Purpose: ForwardPurposeCodeServer, LocalPort: 70000},
	} {
		if _, err := env.svc.EnsureBrowserForward(context.Background(), in); err == nil {
			t.Errorf("EnsureBrowserForward(%+v) must fail", in)
		}
	}
	if DefaultLocalPort(ForwardPurposeCodeServer) != 8080 || DefaultLocalPort(ForwardPurposeKiroCrew) != 5476 {
		t.Error("default local ports must be 8080 (code-server) and 5476 (Kiro Crew)")
	}
}
