package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Tests of EnsureBrowserForward with the router (Issue #124): the free
// local port of a router-only forward, and the router as a precondition.

func TestEnsureBrowserForwardSkipsAPortTakenOnEitherLoopback(t *testing.T) {
	env := newBrowserEnv(t)
	env.queryResults = []queryResult{{status: &TunnelStatus{Name: "dev01", PID: 4242}}}
	// The system's free port is taken on ::1 (listenLocal probes both).
	env.portBusy["41001"] = true
	bf, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	if err != nil {
		t.Fatal(err)
	}
	if bf.LocalPort != 41002 || len(env.spawnedFwd) != 1 || env.spawnedFwd[0][0].Stored() != "code-server=41002:8080" {
		t.Errorf("forward %+v, spawned %v", bf, env.spawnedFwd)
	}
}

func TestEnsureBrowserForwardRetriesWhenTheDaemonLosesThePort(t *testing.T) {
	env := newBrowserEnv(t)
	env.liveDaemon(t, ForwardStatus{Spec: "5432:5432", State: ForwardStateActive})
	calls := 0
	env.svc.tunnel.addForward = func(_ context.Context, _ *TunnelState, f ForwardSpec, _ time.Duration) (*ForwardStatus, error) {
		calls++
		if calls == 1 {
			// Another program took the port before the daemon bound it.
			return nil, &LocalPortInUseError{Port: f.LocalPort, Holder: "dev01", Err: errors.New("bind: address already in use")}
		}
		env.added = append(env.added, f)
		return &ForwardStatus{Spec: f.String(), Purpose: f.Purpose}, nil
	}
	bf, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeKiroCrew})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || bf.LocalPort != 41002 || len(env.added) != 1 || env.added[0].Stored() != "kirocrew=41002:5476" {
		t.Errorf("calls %d, forward %+v, added %v", calls, bf, env.added)
	}
}

func TestEnsureBrowserForwardGivesUpOnPorts(t *testing.T) {
	env := newBrowserEnv(t)
	env.liveDaemon(t, ForwardStatus{Spec: "5432:5432", State: ForwardStateActive})
	env.addErr = &LocalPortInUseError{Port: "x", Err: errors.New("bind: address already in use")}
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	if err == nil || !strings.Contains(err.Error(), "could not find a free local port") {
		t.Fatalf("err = %v", err)
	}
}

// TestEnsureBrowserForwardNeedsTheRouter: a router that cannot start
// stops open before any forward is started.
func TestEnsureBrowserForwardNeedsTheRouter(t *testing.T) {
	env := newBrowserEnv(t)
	env.routerErr = &RouterPortInUseError{Port: 18888, Err: errors.New("address already in use")}
	_, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeCodeServer})
	var inUse *RouterPortInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("err = %v", err)
	}
	if len(env.spawned) != 0 || len(env.added) != 0 || len(env.probes) != 0 {
		t.Errorf("nothing may be started: spawned %v, added %v, probes %v", env.spawned, env.added, env.probes)
	}
}

// TestEnsureBrowserForwardWithALocalPortNeedsNoRouter: --local-port is
// the way around the router (for example when its port is taken).
func TestEnsureBrowserForwardWithALocalPortNeedsNoRouter(t *testing.T) {
	env := newBrowserEnv(t)
	env.routerErr = errors.New("the router must not be asked")
	env.queryResults = []queryResult{{status: &TunnelStatus{Name: "dev01", PID: 4242}}}
	bf, err := env.svc.EnsureBrowserForward(context.Background(), BrowserForwardInput{Name: "dev01", Purpose: ForwardPurposeKiroCrew, LocalPort: 15476})
	if err != nil {
		t.Fatal(err)
	}
	if env.routerCalls != 0 || bf.RouterPort != 0 || bf.Origin != "http://localhost:15476" {
		t.Errorf("router calls %d, forward %+v", env.routerCalls, bf)
	}
}
