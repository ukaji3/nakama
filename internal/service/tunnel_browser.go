package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/config"
)

// This file is the service side of `nakama aws open` and
// `nakama aws kirocrew` over SSM (Issue #118, ADR 0027): it makes sure
// the instance's tunnel daemon forwards code-server or Kiro Crew to a
// local port, and waits until a browser can use it. Without
// --local-port the forward listens on a free port of the system's
// choosing and the browser reaches it through the nakama router at
// http://<purpose>.<name>.nakama.localhost:<port>/ (Issue #124, ADR
// 0029).

// remotePortFor is the instance-side port a browser forward targets:
// code-server's port, or the Kiro Crew gateway on the instance's
// loopback (reached directly, without code-server, ADR 0027).
func remotePortFor(p ForwardPurpose) int {
	switch p {
	case ForwardPurposeKiroCrew:
		return kirocrewGatewayPort
	default:
		return config.CodeServerPort
	}
}

// browserForwardReadyTimeout bounds how long EnsureBrowserForward waits
// for the forward to answer (a minute covers the SSM agent reconnecting
// right after a start). A variable so tests can shorten it.
var browserForwardReadyTimeout = 60 * time.Second

// browserForwardProbeTimeout bounds one readiness request.
var browserForwardProbeTimeout = 5 * time.Second

// randomPortAttempts bounds how often a free local port is picked for a
// router-only forward when another program takes the chosen one first.
const randomPortAttempts = 5

// BrowserForwardInput selects a browser forward.
type BrowserForwardInput struct {
	Name    string
	Purpose ForwardPurpose
	// LocalPort is --local-port: the forward also listens on this port
	// and the browser opens http://localhost:<port>. 0 uses the router
	// and a free port.
	LocalPort int
}

// BrowserForward is a usable browser forward.
type BrowserForward struct {
	Name      string         `json:"name"`
	Purpose   ForwardPurpose `json:"purpose"`
	LocalPort int            `json:"local_port"`
	// Origin is what the browser opens: the router address
	// http://<purpose>.<name>.nakama.localhost:<router port>, or
	// http://localhost:<port> with --local-port.
	Origin string `json:"origin"`
	// RouterPort is the router's port when Origin is the router's.
	RouterPort int `json:"router_port,omitempty"`
	// Reused is true when the daemon already had this forward.
	Reused bool `json:"reused"`
}

// ErrLocalPortMismatch reports --local-port asking for a different port
// than the one the instance's code-server / Kiro Crew is already
// forwarded on.
type ErrLocalPortMismatch struct {
	Name     string
	Purpose  ForwardPurpose
	Existing string
	Wanted   int
}

func (e *ErrLocalPortMismatch) Error() string {
	return fmt.Sprintf("the %s of %s is already forwarded (local port %s); open it without --local-port, "+
		"or run `nakama aws tunnel stop %s` first to use localhost:%d", e.Purpose, e.Name, e.Existing, e.Name, e.Wanted)
}

// EnsureBrowserForward makes the instance's tunnel daemon forward
// code-server or Kiro Crew to a local port and waits until the far end
// answers through it:
//   - without --local-port the router is started first (EnsureRouter),
//     and the browser address is the router's;
//   - a forward to the same instance port that is not dead is reused;
//   - otherwise the forward is added to the running daemon, or a new
//     daemon is started with it.
//
// The caller has checked that the instance exists and is running.
func (s *Service) EnsureBrowserForward(ctx context.Context, in BrowserForwardInput) (*BrowserForward, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}
	if in.Purpose != ForwardPurposeCodeServer && in.Purpose != ForwardPurposeKiroCrew {
		return nil, fmt.Errorf("unknown browser forward %q", in.Purpose)
	}
	if in.LocalPort < 0 || in.LocalPort > 65535 {
		return nil, fmt.Errorf("invalid --local-port %d: must be 1-65535", in.LocalPort)
	}
	explicit := in.LocalPort != 0
	hooks := s.tunnelHooksOrDefault()
	var router *RouterStatus
	if !explicit {
		r, err := hooks.ensureRouter(ctx)
		if err != nil {
			return nil, err
		}
		router = r
	}
	want := ForwardSpec{RemotePort: strconv.Itoa(remotePortFor(in.Purpose)), Purpose: in.Purpose}
	if explicit {
		want.LocalPort = strconv.Itoa(in.LocalPort)
	}

	port, reused, err := s.ensureForward(ctx, hooks, in, want, explicit)
	if err != nil {
		return nil, err
	}
	bf := &BrowserForward{
		Name:      in.Name,
		Purpose:   in.Purpose,
		LocalPort: port,
		Origin:    "http://" + net.JoinHostPort("localhost", strconv.Itoa(port)),
		Reused:    reused,
	}
	if router != nil {
		bf.Origin = RouterOrigin(in.Purpose, in.Name, router.Port)
		bf.RouterPort = router.Port
	}
	if err := s.waitBrowserForward(ctx, hooks, bf); err != nil {
		return nil, err
	}
	return bf, nil
}

// ensureForward returns the local port of the forward to want's remote
// port, adding the forward or starting the daemon as needed. want has
// no local port unless explicit; then a free one is picked.
func (s *Service) ensureForward(ctx context.Context, hooks *tunnelHooks, in BrowserForwardInput, want ForwardSpec, explicit bool) (int, bool, error) {
	st, err := readTunnelState(in.Name)
	if err != nil && !isNoTunnel(err) {
		return 0, false, err
	}
	if err == nil {
		status, qErr := hooks.query(ctx, st, tunnelControlTimeout)
		if qErr != nil {
			// A daemon that does not answer is gone: clean up and start
			// a fresh one below.
			if rmErr := removeTunnelStateOwned(in.Name, st.Token); rmErr != nil {
				return 0, false, rmErr
			}
		} else {
			if live, _ := findBrowserForward(status, want.RemotePort); live >= 0 {
				f, _ := ParseForwardSpec(status.Forwards[live].Spec)
				if explicit && f.LocalPort != want.LocalPort {
					return 0, false, &ErrLocalPortMismatch{Name: in.Name, Purpose: in.Purpose, Existing: f.LocalPort, Wanted: in.LocalPort}
				}
				port, _ := strconv.Atoi(f.LocalPort)
				return port, true, nil
			}
			return s.addBrowserForward(ctx, hooks, in, st, status, want, explicit)
		}
	}
	if !explicit {
		p, err := pickFreeLocalPort(hooks)
		if err != nil {
			return 0, false, err
		}
		want.LocalPort = p
	}
	if _, err := s.StartTunnel(ctx, in.Name, []ForwardSpec{want}); err != nil {
		return 0, false, err
	}
	port, _ := strconv.Atoi(want.LocalPort)
	return port, false, nil
}

// addBrowserForward adds want to the running daemon. A free port picked
// for a router-only forward can be taken by another program before the
// daemon binds it; the daemon then answers 409 and another port is
// tried.
func (s *Service) addBrowserForward(ctx context.Context, hooks *tunnelHooks, in BrowserForwardInput, st *TunnelState, status *TunnelStatus, want ForwardSpec, explicit bool) (int, bool, error) {
	attempts := 1
	if !explicit {
		attempts = randomPortAttempts
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if explicit {
			if holdsLocalPort(status, want.LocalPort) {
				// A live forward to some other port uses ours.
				return 0, false, &LocalPortInUseError{Port: want.LocalPort, Holder: in.Name,
					Err: errors.New("forwarded to another port of the instance")}
			}
			// The port is free, or held by a dead forward of this daemon
			// (whose listeners are closed), which the daemon replaces.
			if err := checkLocalPortFree(hooks, want.LocalPort); err != nil {
				return 0, false, err
			}
		} else {
			p, err := pickFreeLocalPort(hooks)
			if err != nil {
				return 0, false, err
			}
			want.LocalPort = p
		}
		_, err := hooks.addForward(ctx, st, want, tunnelControlTimeout)
		if err == nil {
			port, _ := strconv.Atoi(want.LocalPort)
			return port, false, nil
		}
		if errors.Is(err, ErrTunnelDaemonTooOld) {
			return 0, false, fmt.Errorf("%w; run `nakama aws tunnel restart %s` (it keeps the current forwards) and try again",
				err, in.Name)
		}
		var inUse *LocalPortInUseError
		if explicit || !errors.As(err, &inUse) {
			return 0, false, err
		}
		lastErr = err
	}
	return 0, false, fmt.Errorf("could not find a free local port for the forward: %w", lastErr)
}

// pickFreeLocalPort asks the system for a free loopback port that is
// also free on ::1 (the daemon binds both, listenLoopback).
func pickFreeLocalPort(hooks *tunnelHooks) (string, error) {
	var lastErr error
	for i := 0; i < randomPortAttempts; i++ {
		p, err := hooks.pickPort()
		if err != nil {
			return "", err
		}
		if err := hooks.listenLocal(p); err != nil {
			lastErr = err
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("could not find a free local port for the forward: %w", lastErr)
}

// holdsLocalPort reports whether a forward of the daemon that is not
// dead listens on the local port.
func holdsLocalPort(status *TunnelStatus, port string) bool {
	for _, fs := range status.Forwards {
		if f, err := ParseForwardSpec(fs.Spec); err == nil && f.LocalPort == port && fs.State != ForwardStateDead {
			return true
		}
	}
	return false
}

// waitBrowserForward waits until an HTTP request through the forward
// gets an answer, or the forward dies, or the timeout passes. On
// failure the error carries the daemon's latest detail, and the
// TargetNotConnected advice when that was the cause.
func (s *Service) waitBrowserForward(ctx context.Context, hooks *tunnelHooks, bf *BrowserForward) error {
	port := strconv.Itoa(bf.LocalPort)
	// Through the router when the browser uses it: that checks the
	// router's route to the forward as well.
	dialPort, host := port, net.JoinHostPort("localhost", port)
	if bf.RouterPort != 0 {
		dialPort, host = strconv.Itoa(bf.RouterPort), RouterHost(bf.Purpose, bf.Name, bf.RouterPort)
	}
	path := bf.Purpose.healthPath()
	deadline := hooks.now().Add(browserForwardReadyTimeout)
	var last ForwardStatus
	for {
		pctx, cancel := context.WithTimeout(ctx, browserForwardProbeTimeout)
		err := hooks.probeHTTP(pctx, dialPort, host, path)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fs, ok := s.forwardStatusOf(ctx, hooks, bf.Name, port); ok {
			last = fs
			if fs.State == ForwardStateDead {
				return fmt.Errorf("the forward to %s of %s failed: %s", bf.Purpose, bf.Name, fs.Detail)
			}
		}
		if hooks.now().After(deadline) {
			detail := last.Detail
			if detail == "" {
				detail = fmt.Sprintf("no answer on localhost:%s (last: %v)", port, err)
			}
			waitErr := fmt.Errorf("the forward to %s of %s did not become usable within %s: %s",
				bf.Purpose, bf.Name, browserForwardReadyTimeout, detail)
			if last.LastErrorCode == "TargetNotConnected" {
				return s.targetNotConnectedHint(ctx, &wrappedAPIError{err: waitErr, code: last.LastErrorCode})
			}
			return waitErr
		}
		if err := hooks.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// forwardStatusOf returns the daemon's status of the forward on port.
func (s *Service) forwardStatusOf(ctx context.Context, hooks *tunnelHooks, name, port string) (ForwardStatus, bool) {
	st, err := readTunnelState(name)
	if err != nil {
		return ForwardStatus{}, false
	}
	status, err := hooks.query(ctx, st, tunnelControlTimeout)
	if err != nil {
		return ForwardStatus{}, false
	}
	for _, fs := range status.Forwards {
		if f, err := ParseForwardSpec(fs.Spec); err == nil && f.LocalPort == port {
			return fs, true
		}
	}
	return ForwardStatus{}, false
}

// wrappedAPIError carries an AWS error code under a message of our own,
// so the TargetNotConnected advice (which looks at the code) applies to
// a failure reported by the daemon.
type wrappedAPIError struct {
	err  error
	code string
}

func (e *wrappedAPIError) Error() string { return e.err.Error() }
func (e *wrappedAPIError) Unwrap() error { return e.err }

// ErrorCode returns the AWS error code the daemon reported.
func (e *wrappedAPIError) ErrorCode() string { return e.code }

// ErrorMessage returns the message.
func (e *wrappedAPIError) ErrorMessage() string { return e.err.Error() }

// ErrorFault is unknown: the daemon does not report it.
func (e *wrappedAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }
