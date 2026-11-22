package service

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/ukaji3/nakama/internal/version"
)

// This file is the CLI-process side of the nakama router (Issue #124,
// ADR 0029): making sure a router of this nakama version runs on the
// configured port, and reading its status for `tunnel status`.

// routerDaemonArgs is the argv (after the binary path) of the hidden
// router command. It must match the command tree in internal/cli
// (aws_router_daemon.go).
var routerDaemonArgs = []string{"aws", "router-daemon"}

// routerHooks abstracts the process side effects of EnsureRouter so it
// can be tested without spawning processes.
type routerHooks struct {
	spawn       func(port int) (int, error)
	query       func(ctx context.Context, st *RouterState, timeout time.Duration) (*RouterStatus, error)
	requestStop func(ctx context.Context, st *RouterState, timeout time.Duration) error
	// listenLocal probes that the router port is free (127.0.0.1 and ::1).
	listenLocal func(port string) error
	sleep       func(ctx context.Context, d time.Duration) error
	now         func() time.Time
	// version is this nakama's version string; a router of another
	// version is replaced.
	version string
}

func defaultRouterHooks() *routerHooks {
	return &routerHooks{
		spawn:       spawnRouterDaemon,
		query:       queryRouterStatus,
		requestStop: requestRouterStop,
		listenLocal: probeLocalPort,
		sleep:       sleepCtx,
		now:         time.Now,
		version:     version.String(),
	}
}

func (s *Service) routerHooksOrDefault() *routerHooks {
	if s.router != nil {
		return s.router
	}
	return defaultRouterHooks()
}

// routerReadyTimeout bounds the wait for a spawned router, and for a
// replaced one to exit.
var routerReadyTimeout = 15 * time.Second

// RouterPortInUseError reports that the router port is taken by another
// program (decision 10 of Issue #124: no automatic other port).
type RouterPortInUseError struct {
	Port int
	// Holder is the nakama instance whose tunnel forwards the port, when
	// that is what holds it.
	Holder string
	Err    error
}

func (e *RouterPortInUseError) Error() string {
	holder := ""
	if e.Holder != "" {
		holder = fmt.Sprintf(" (the nakama tunnel of %s forwards it)", e.Holder)
	}
	return fmt.Sprintf("local port %d of the nakama router is not available%s: %v\n"+
		"set %s to a free port (for example `%s=%d`) and try again",
		e.Port, holder, e.Err, RouterPortEnv, RouterPortEnv, e.Port+1)
}

// Unwrap returns the bind error.
func (e *RouterPortInUseError) Unwrap() error { return e.Err }

// EnsureRouter makes sure the router of this nakama version runs on the
// configured port (RouterPort) and returns its status. A router of
// another version, or on another port, is stopped and replaced; open
// WebSocket connections through it are cut and the pages reconnect.
func (s *Service) EnsureRouter(ctx context.Context) (*RouterStatus, error) {
	port, err := RouterPort()
	if err != nil {
		return nil, err
	}
	h := s.routerHooksOrDefault()
	st, err := readRouterState()
	switch {
	case err == nil:
		status, qErr := h.query(ctx, st, tunnelControlTimeout)
		if qErr != nil {
			// A router that does not answer is gone.
			if err := removeRouterStateOwned(st.Token); err != nil {
				return nil, err
			}
			break
		}
		if status.Version == h.version && status.Port == port {
			return status, nil
		}
		// Moving to another port: make sure it is free before the
		// running router (which still serves its own port) is stopped.
		if status.Port != port {
			if err := h.listenLocal(strconv.Itoa(port)); err != nil {
				return nil, &RouterPortInUseError{Port: port, Holder: tunnelHoldingPort(strconv.Itoa(port)), Err: err}
			}
		}
		if err := stopRouter(ctx, h, st); err != nil {
			return nil, err
		}
	case !isNoRouter(err):
		return nil, err
	}

	if err := h.listenLocal(strconv.Itoa(port)); err != nil {
		return nil, &RouterPortInUseError{Port: port, Holder: tunnelHoldingPort(strconv.Itoa(port)), Err: err}
	}
	if _, err := h.spawn(port); err != nil {
		return nil, fmt.Errorf("start the nakama router: %w", err)
	}
	deadline := h.now().Add(routerReadyTimeout)
	for {
		if st, err := readRouterState(); err == nil {
			if status, qErr := h.query(ctx, st, tunnelControlTimeout); qErr == nil && status.Port == port {
				// Two CLIs may race to start it; whichever router won the
				// port serves both. One of another version is replaced on
				// the next call.
				return status, nil
			}
		}
		if h.now().After(deadline) {
			return nil, fmt.Errorf("the nakama router did not become ready within %s; "+
				"check the OS log (unix: syslog tag nakama-router, Windows: Application event log, source nakama)",
				routerReadyTimeout)
		}
		if err := h.sleep(ctx, 200*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// stopRouter asks a running router to stop and waits until it has
// removed its state file (and with it released its port).
func stopRouter(ctx context.Context, h *routerHooks, st *RouterState) error {
	if err := h.requestStop(ctx, st, tunnelControlTimeout); err != nil {
		return removeRouterStateOwned(st.Token)
	}
	deadline := h.now().Add(routerReadyTimeout)
	for {
		cur, err := readRouterState()
		if (err != nil && isNoRouter(err)) || (err == nil && cur.Token != st.Token) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.now().After(deadline) {
			return fmt.Errorf("the nakama router (pid %d) acknowledged stop but did not exit within %s", st.PID, routerReadyTimeout)
		}
		if err := h.sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// GetRouterStatus returns the running router's status, or nil when no
// router answers (a leftover state file is removed). It needs no AWS
// credentials.
func (s *Service) GetRouterStatus(ctx context.Context) (*RouterStatus, error) {
	h := s.routerHooksOrDefault()
	st, err := readRouterState()
	if err != nil {
		if isNoRouter(err) {
			return nil, nil
		}
		return nil, err
	}
	status, err := h.query(ctx, st, tunnelControlTimeout)
	if err != nil {
		if rmErr := removeRouterStateOwned(st.Token); rmErr != nil {
			return nil, rmErr
		}
		return nil, nil
	}
	return status, nil
}

// queryRouterStatus asks a (possibly dead) router for its status.
func queryRouterStatus(ctx context.Context, st *RouterState, timeout time.Duration) (*RouterStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var status RouterStatus
	if err := controlRequest(ctx, st.ControlPort, st.Token, http.MethodGet, "/status", nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// requestRouterStop asks the router to shut down.
func requestRouterStop(ctx context.Context, st *RouterState, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return controlRequest(ctx, st.ControlPort, st.Token, http.MethodPost, "/stop", nil, nil)
}

// spawnRouterDaemon re-executes this nakama binary as the hidden
// `nakama aws router-daemon` command, detached like the tunnel daemon.
func spawnRouterDaemon(port int) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate the nakama binary for the router: %w", err)
	}
	args := append(append([]string{}, routerDaemonArgs...), "--port", strconv.Itoa(port))
	cmd := exec.Command(exe, args...) // #nosec G204 -- argv is the re-exec'd nakama binary plus a validated port
	cmd.SysProcAttr = tunnelDaemonSysProcAttr()
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}
