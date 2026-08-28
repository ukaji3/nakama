package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// This file is the CLI-process side of `nakama aws tunnel` (ADR 0017):
// preflight checks, spawning the detached daemon, and talking to its
// control channel for status and stop.

// tunnelDaemonArgs is the argv (after the binary path) of the hidden
// daemon command. It must match the command tree in internal/cli
// (aws_tunnel_daemon.go).
var tunnelDaemonArgs = []string{"aws", "tunnel-daemon"}

// tunnelHooks abstracts the process/network side effects of the tunnel
// orchestration so it can be unit tested without spawning daemons.
type tunnelHooks struct {
	// spawn starts the detached daemon process and returns its PID.
	spawn func(name string, forwards []ForwardSpec) (int, error)
	// query fetches a daemon's status over its control channel.
	query func(ctx context.Context, st *TunnelState, timeout time.Duration) (*TunnelStatus, error)
	// requestStop asks a daemon to shut down gracefully.
	requestStop func(ctx context.Context, st *TunnelState, timeout time.Duration) error
	// listenLocal probes that a local port is free (preflight).
	listenLocal func(port string) error
	sleep       func(ctx context.Context, d time.Duration) error
	now         func() time.Time
}

func defaultTunnelHooks() *tunnelHooks {
	return &tunnelHooks{
		spawn:       spawnTunnelDaemon,
		query:       queryTunnelStatus,
		requestStop: requestTunnelStop,
		listenLocal: probeLocalPort,
		sleep:       sleepCtx,
		now:         time.Now,
	}
}

// tunnelHooksOrDefault returns the injected hooks or the production
// defaults.
func (s *Service) tunnelHooksOrDefault() *tunnelHooks {
	if s.tunnel != nil {
		return s.tunnel
	}
	return defaultTunnelHooks()
}

// probeLocalPort checks that the forward's local listener port is
// still free. The plugin binds "localhost:<port>"; probing the same
// name catches both v4 and v6 conflicts without connecting anywhere.
func probeLocalPort(port string) error {
	l, err := net.Listen("tcp", "localhost:"+port)
	if err != nil {
		return fmt.Errorf("local port %s is not available: %w", port, err)
	}
	return l.Close()
}

// tunnelControlTimeout is the per-request budget for control-channel
// calls (liveness, status, stop). Local loopback traffic: generous.
const tunnelControlTimeout = 3 * time.Second

// tunnelReadyTimeout is how long StartTunnel waits for the spawned
// daemon to write its state file and answer /status.
const tunnelReadyTimeout = 15 * time.Second

// StartTunnel starts (or reports) the background port-forwarding
// daemon for the instance. On success it returns the daemon's initial
// status. When a live daemon already serves this instance it returns
// *TunnelAlreadyRunningError carrying that daemon's status, and starts
// nothing (the requested forwards are NOT applied).
func (s *Service) StartTunnel(ctx context.Context, name string, forwards []ForwardSpec) (*TunnelStatus, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	if len(forwards) == 0 {
		return nil, fmt.Errorf("at least one -L forward is required")
	}
	hooks := s.tunnelHooksOrDefault()

	// Already running? Liveness is decided by the control channel, not
	// the PID: a dead daemon leaves a stale state file behind (crash,
	// power loss), which is cleaned up here.
	if st, err := readTunnelState(name); err == nil {
		if status, qErr := hooks.query(ctx, st, tunnelControlTimeout); qErr == nil {
			return nil, &TunnelAlreadyRunningError{Status: status}
		}
		if err := removeTunnelStateOwned(name, st.Token); err != nil {
			return nil, err
		}
	}

	// Fail fast in the CLI process, where errors are visible: instance
	// existence and local port availability.
	id, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(name))
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("nakama instance %q not found", name)
	}
	for _, f := range forwards {
		if err := hooks.listenLocal(f.LocalPort); err != nil {
			return nil, err
		}
	}

	if _, err := hooks.spawn(name, forwards); err != nil {
		return nil, fmt.Errorf("start tunnel daemon: %w", err)
	}

	// Readiness: the daemon writes its state file after binding the
	// control channel, so state + /status answering means ready.
	deadline := hooks.now().Add(tunnelReadyTimeout)
	for {
		if st, err := readTunnelState(name); err == nil {
			if status, qErr := hooks.query(ctx, st, tunnelControlTimeout); qErr == nil {
				return status, nil
			}
		}
		if hooks.now().After(deadline) {
			return nil, fmt.Errorf("tunnel daemon for %q did not become ready within %s; "+
				"check the OS log (unix: syslog tag nakama-tunnel, Windows: Application event log, source nakama)",
				name, tunnelReadyTimeout)
		}
		if err := hooks.sleep(ctx, 200*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// TunnelStopOutcome reports what StopTunnel actually did.
type TunnelStopOutcome struct {
	// WasRunning is false when only a stale state file was cleaned up.
	WasRunning bool
}

// StopTunnel gracefully stops the daemon for the instance: all
// session-runner children are terminated, server-side sessions are
// closed best-effort, and the state file is removed. A stale state
// file (daemon already gone) is cleaned up silently.
func (s *Service) StopTunnel(ctx context.Context, name string) (*TunnelStopOutcome, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	hooks := s.tunnelHooksOrDefault()
	st, err := readTunnelState(name)
	if err != nil {
		return nil, err // includes *NoTunnelError
	}
	if err := hooks.requestStop(ctx, st, tunnelControlTimeout); err != nil {
		// Daemon unreachable: nothing is forwarding; clean up.
		if rmErr := removeTunnelStateOwned(name, st.Token); rmErr != nil {
			return nil, rmErr
		}
		return &TunnelStopOutcome{WasRunning: false}, nil
	}
	// The daemon removes its state file as the last shutdown step;
	// wait for that so "stop then start" cannot race the O_EXCL create.
	deadline := hooks.now().Add(tunnelReadyTimeout)
	for {
		if _, err := readTunnelState(name); err != nil {
			var notRunning *NoTunnelError
			if errors.As(err, &notRunning) {
				return &TunnelStopOutcome{WasRunning: true}, nil
			}
			return nil, err
		}
		if hooks.now().After(deadline) {
			return nil, fmt.Errorf("tunnel daemon for %q acknowledged stop but did not exit within %s", name, tunnelReadyTimeout)
		}
		if err := hooks.sleep(ctx, 200*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// RestartTunnel restarts the instance's tunnel with the forward specs
// recorded in its state file: the running daemon (if any) is stopped
// gracefully — a stale state file is cleaned up silently — and a fresh
// daemon is spawned with the same forwards. The new daemon starts from
// this CLI process's current credential chain, so `nakama aws login`
// followed by `tunnel restart` is the recovery path for forwards that
// died with expired credentials (ADR 0017). Without a state file there
// is nothing to restart: *NoTunnelError tells the caller to use
// `tunnel start` with explicit -L specs instead.
func (s *Service) RestartTunnel(ctx context.Context, name string) (*TunnelStatus, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	st, err := readTunnelState(name)
	if err != nil {
		return nil, err // includes *NoTunnelError
	}
	// Re-validate the recorded specs before tearing anything down: the
	// state file is user-writable, and a spec that no longer parses
	// must fail while the old daemon (if alive) keeps forwarding.
	forwards, err := ParseForwardSpecs(st.Forwards)
	if err != nil {
		return nil, fmt.Errorf("tunnel state for %q contains invalid forwards: %w", name, err)
	}
	// StopTunnel waits until the state file is gone, so the subsequent
	// start cannot race the new daemon's O_EXCL state-file create, and
	// the local port probe runs after the old listeners are torn down.
	if _, err := s.StopTunnel(ctx, name); err != nil {
		return nil, err
	}
	return s.StartTunnel(ctx, name, forwards)
}

// GetTunnelStatus returns the live status of the instance's tunnel
// daemon. A stale state file is cleaned up and reported as
// *NoTunnelError.
func (s *Service) GetTunnelStatus(ctx context.Context, name string) (*TunnelStatus, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	hooks := s.tunnelHooksOrDefault()
	st, err := readTunnelState(name)
	if err != nil {
		return nil, err // includes *NoTunnelError
	}
	status, err := hooks.query(ctx, st, tunnelControlTimeout)
	if err != nil {
		if rmErr := removeTunnelStateOwned(name, st.Token); rmErr != nil {
			return nil, rmErr
		}
		return nil, &NoTunnelError{Name: name}
	}
	return status, nil
}

// spawnTunnelDaemon re-executes the current nakama binary as the
// hidden `nakama aws tunnel-daemon` command, detached from this
// process's session/console (OS-specific SysProcAttr, see
// tunnel_unix.go / tunnel_windows.go). Stdio is wired to the null
// device: the daemon logs to the OS log instead (ADR 0017). Forward
// specs carry no secrets, so argv is fine.
func spawnTunnelDaemon(name string, forwards []ForwardSpec) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate nakama binary for tunnel daemon: %w", err)
	}
	args := append(append([]string{}, tunnelDaemonArgs...), "--name", name)
	for _, f := range forwards {
		args = append(args, "--forward", f.String())
	}
	cmd := exec.Command(exe, args...) // #nosec G204 -- argv is the re-exec'd nakama binary plus validated specs
	cmd.SysProcAttr = tunnelDaemonSysProcAttr()
	// Stdin/Stdout/Stderr nil => os/exec wires the null device.
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start tunnel daemon: %w", err)
	}
	pid := cmd.Process.Pid
	// The daemon outlives this CLI process; drop the handle so no Wait
	// is owed. On unix the CLI exits promptly, so no zombie lingers.
	_ = cmd.Process.Release()
	return pid, nil
}
