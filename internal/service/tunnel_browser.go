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
// local port, and waits until a browser can use it.

// DefaultLocalPort is the local port of a browser forward when
// --local-port is not given: the instance-side port itself, so
// `localhost:5476` links printed on the instance (`kirocrew token`)
// work in the local browser too.
func DefaultLocalPort(p ForwardPurpose) int {
	switch p {
	case ForwardPurposeKiroCrew:
		return kirocrewGatewayPort
	default:
		return config.CodeServerPort
	}
}

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

// BrowserForwardInput selects a browser forward.
type BrowserForwardInput struct {
	Name    string
	Purpose ForwardPurpose
	// LocalPort is --local-port; 0 picks DefaultLocalPort.
	LocalPort int
}

// BrowserForward is a usable browser forward.
type BrowserForward struct {
	Name      string         `json:"name"`
	Purpose   ForwardPurpose `json:"purpose"`
	LocalPort int            `json:"local_port"`
	// Origin is http://localhost:<port>, what the browser opens.
	Origin string `json:"origin"`
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
	return fmt.Sprintf("the %s of %s is already forwarded on localhost:%s; open that one (without --local-port), "+
		"or run `nakama aws tunnel stop %s` first to use localhost:%d", e.Purpose, e.Name, e.Existing, e.Name, e.Wanted)
}

// EnsureBrowserForward makes the instance's tunnel daemon forward
// code-server or Kiro Crew to a local port and waits until the far end
// answers through it:
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
	local := in.LocalPort
	if !explicit {
		local = DefaultLocalPort(in.Purpose)
	}
	remote := strconv.Itoa(remotePortFor(in.Purpose))
	want := ForwardSpec{LocalPort: strconv.Itoa(local), RemotePort: remote, Purpose: in.Purpose}
	hooks := s.tunnelHooksOrDefault()

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
	if err := s.waitBrowserForward(ctx, hooks, bf); err != nil {
		return nil, err
	}
	return bf, nil
}

// ensureForward returns the local port that forwards want's remote
// port, adding the forward or starting the daemon as needed.
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
			for _, fs := range status.Forwards {
				f, perr := ParseForwardSpec(fs.Spec)
				if perr != nil || f.RemoteHost != "" || f.RemotePort != want.RemotePort || fs.State == ForwardStateDead {
					continue
				}
				if explicit && f.LocalPort != want.LocalPort {
					return 0, false, &ErrLocalPortMismatch{Name: in.Name, Purpose: in.Purpose, Existing: f.LocalPort, Wanted: in.LocalPort}
				}
				port, _ := strconv.Atoi(f.LocalPort)
				return port, true, nil
			}
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
			if _, err := hooks.addForward(ctx, st, want, tunnelControlTimeout); err != nil {
				if errors.Is(err, ErrTunnelDaemonTooOld) {
					return 0, false, fmt.Errorf("%w; run `nakama aws tunnel restart %s` (it keeps the current forwards) and try again",
						err, in.Name)
				}
				return 0, false, err
			}
			port, _ := strconv.Atoi(want.LocalPort)
			return port, false, nil
		}
	}
	if _, err := s.StartTunnel(ctx, in.Name, []ForwardSpec{want}); err != nil {
		return 0, false, err
	}
	port, _ := strconv.Atoi(want.LocalPort)
	return port, false, nil
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
	path := bf.Purpose.healthPath()
	deadline := hooks.now().Add(browserForwardReadyTimeout)
	var last ForwardStatus
	for {
		pctx, cancel := context.WithTimeout(ctx, browserForwardProbeTimeout)
		err := hooks.probeHTTP(pctx, port, path)
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
