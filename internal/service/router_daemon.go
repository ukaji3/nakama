package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ukaji3/nakama/internal/version"
)

// This file is the `nakama aws router-daemon` process (Issue #124, ADR
// 0029): it serves the router (router.go) on 127.0.0.1 and ::1, answers
// its control channel, and ends by itself once no tunnel daemon has been
// running for a while. Its state file sits next to the tunnel state
// directory, never inside it: "router" is a valid instance name.

// RouterState is the router's state file, ~/.config/nakama/router.json
// (0600, it holds the control token).
type RouterState struct {
	PID         int       `json:"pid"`
	Port        int       `json:"port"`
	ControlPort int       `json:"controlPort"`
	Token       string    `json:"token"`
	Version     string    `json:"version"`
	StartedAt   time.Time `json:"startedAt"`
}

// RouterStatus is what the router's control channel reports.
type RouterStatus struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
}

// NoRouterError reports that no router state is recorded.
type NoRouterError struct{}

func (*NoRouterError) Error() string { return "the nakama router is not running" }

func isNoRouter(err error) bool {
	var nr *NoRouterError
	return errors.As(err, &nr)
}

// routerStatePath is router.json in the nakama config directory, the
// parent of the tunnel state directory.
func routerStatePath() (string, error) {
	dir, err := tunnelStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "router.json"), nil
}

// writeRouterState writes the state exclusively: an existing file means
// another router owns (or owned) it, and the caller decides first.
func writeRouterState(st *RouterState) error {
	path, err := routerStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the nakama config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- fixed name under the user config dir
	if err != nil {
		return fmt.Errorf("create the router state file: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write the router state file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close the router state file: %w", err)
	}
	return nil
}

// readRouterState loads the state; a missing file is *NoRouterError.
func readRouterState() (*RouterState, error) {
	path, err := routerStatePath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- fixed name under the user config dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &NoRouterError{}
		}
		return nil, fmt.Errorf("read the router state file: %w", err)
	}
	var st RouterState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse the router state file %s: %w", path, err)
	}
	return &st, nil
}

// removeRouterStateOwned deletes the state file while it still carries
// token, i.e. belongs to the router generation the caller observed (the
// same guard as removeTunnelStateOwned).
func removeRouterStateOwned(token string) error {
	st, err := readRouterState()
	if err != nil {
		if isNoRouter(err) {
			return nil
		}
		return err
	}
	if st.Token != token {
		return nil
	}
	path, err := routerStatePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove the router state file: %w", err)
	}
	return nil
}

// Timings of the router daemon; variables so tests can shorten them.
var (
	// routerIdleCheckInterval is how often the router looks for tunnel
	// daemons.
	routerIdleCheckInterval = 15 * time.Second
	// routerIdleExit is how long the router keeps running without any
	// tunnel daemon or control panel. `code` starts the router before
	// the tunnel daemon, so this also covers the start.
	routerIdleExit = time.Minute
)

// RouterDaemonInput parameterizes one router run.
type RouterDaemonInput struct {
	Port int
}

// routerDaemonDeps are the side effects of a router run (tests
// substitute fakes).
type routerDaemonDeps struct {
	logger tunnelLogger
	routes routeTable
	// liveTunnels counts the tunnel daemons that answer, and says why the
	// others did not.
	liveTunnels func(ctx context.Context) (int, error)
	// panelAlive reports whether a control panel answers (nil: none is
	// looked for); the router keeps running while one does.
	panelAlive func(ctx context.Context) bool
	listen     func(port string) ([]net.Listener, error)
	version    string
	// ready is called once the router serves (tests).
	ready func(st *RouterState)
}

// RunRouterDaemon is the entry point of the hidden
// `nakama aws router-daemon` command.
func RunRouterDaemon(ctx context.Context, in RouterDaemonInput) error {
	logger := openDaemonLogger("nakama-router")
	defer func() { _ = logger.Close() }()
	return runRouterDaemon(ctx, in, routerDaemonDeps{
		logger:      logger,
		routes:      newTunnelRoutes(),
		liveTunnels: countLiveTunnels,
		panelAlive:  panelAlive,
		listen:      listenLoopback,
		version:     version.String(),
	})
}

func runRouterDaemon(ctx context.Context, in RouterDaemonInput, deps routerDaemonDeps) error {
	logger := deps.logger
	if in.Port < 1 || in.Port > 65535 {
		err := fmt.Errorf("invalid router port %d", in.Port)
		logger.Errorf("router: %v", err)
		return err
	}
	front, err := deps.listen(strconv.Itoa(in.Port))
	if err != nil {
		// Another router (or another program) has the port: the CLI that
		// spawned this one finds the running router, or reports the port.
		logger.Errorf("router: listen on port %d: %v", in.Port, err)
		return err
	}
	defer func() {
		for _, l := range front {
			_ = l.Close()
		}
	}()
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logger.Errorf("router: listen control channel: %v", err)
		return fmt.Errorf("listen control channel: %w", err)
	}
	defer func() { _ = control.Close() }()
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		logger.Errorf("router: generate control token: %v", err)
		return fmt.Errorf("generate control token: %w", err)
	}
	state := &RouterState{
		PID:         os.Getpid(),
		Port:        in.Port,
		ControlPort: control.Addr().(*net.TCPAddr).Port,
		Token:       hex.EncodeToString(tokenBytes),
		Version:     deps.version,
		StartedAt:   time.Now().UTC(),
	}
	if err := writeRouterState(state); err != nil {
		// Either another router runs (on another port: this process holds
		// this one) or a router that is gone left its file behind. Only
		// the leftover is replaced.
		if old, rerr := readRouterState(); rerr == nil {
			if _, qerr := queryRouterStatus(ctx, old, tunnelControlTimeout); qerr == nil {
				err := fmt.Errorf("another nakama router (pid %d) runs on port %d", old.PID, old.Port)
				logger.Errorf("router: %v", err)
				return err
			}
			_ = removeRouterStateOwned(old.Token)
		}
		if err := writeRouterState(state); err != nil {
			logger.Errorf("router: %v", err)
			return err
		}
	}
	defer func() { _ = removeRouterStateOwned(state.Token) }()

	stopCh := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() { stopOnce.Do(func() { close(stopCh) }) }
	cleanupSignals := notifyTunnelStopSignals(requestStop)
	defer cleanupSignals()

	logf := func(format string, args ...any) { logger.Infof(format, args...) }
	server := &http.Server{
		Handler:           newRouterHandler(in.Port, deps.routes, newRouterTransport(), logf),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	for _, l := range front {
		go func() {
			if err := server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Errorf("router: serve %s: %v", l.Addr(), err)
				requestStop()
			}
		}()
	}
	controlServer := newControlServer(newRouterControlHandler(state.Token, func() *RouterStatus {
		return &RouterStatus{PID: state.PID, Port: state.Port, Version: state.Version, StartedAt: state.StartedAt}
	}, requestStop))
	go func() {
		if err := controlServer.Serve(control); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("router: control channel: %v", err)
			requestStop()
		}
	}()
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	defer cancelMonitor()
	go watchRouterIdle(monitorCtx, withPanel(deps.liveTunnels, deps.panelAlive), requestStop, logger)

	logger.Infof("router: serving http://<code-server|kirocrew>.<instance>.%s:%d/ and http://%s/ (pid %d, %s)",
		routerDomain, in.Port, PanelHost(in.Port), state.PID, state.Version)
	if deps.ready != nil {
		deps.ready(state)
	}
	select {
	case <-stopCh:
	case <-ctx.Done():
	}
	logger.Infof("router: stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	_ = controlServer.Shutdown(shutdownCtx)
	if err := removeRouterStateOwned(state.Token); err != nil {
		logger.Errorf("router: %v", err)
	}
	logger.Infof("router: stopped")
	return nil
}

// withPanel counts a control panel that answers as one more client of
// the router, so the router stays while the panel runs (Issue #139).
func withPanel(liveTunnels func(context.Context) (int, error), panelAlive func(context.Context) bool) func(context.Context) (int, error) {
	if panelAlive == nil {
		return liveTunnels
	}
	return func(ctx context.Context) (int, error) {
		n, err := liveTunnels(ctx)
		if panelAlive(ctx) {
			n++
		}
		return n, err
	}
}

// watchRouterIdle stops the router once no tunnel daemon (and no control
// panel) has answered for routerIdleExit: without one there is nothing
// to route to. When a
// recorded daemon stops answering, it logs why, once, and once more when
// the daemons answer again (Issue #135): a router that exits although a
// daemon runs must say what failed.
func watchRouterIdle(ctx context.Context, liveTunnels func(context.Context) (int, error), stop func(), logger tunnelLogger) {
	var idleSince time.Time
	failing := false
	t := time.NewTicker(routerIdleCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n, err := liveTunnels(ctx)
			switch {
			case err != nil && !failing:
				failing = true
				logger.Errorf("router: a tunnel daemon does not answer: %v", err)
			case err == nil && failing:
				failing = false
				logger.Infof("router: the tunnel daemons answer again")
			}
			if n > 0 {
				idleSince = time.Time{}
				continue
			}
			if idleSince.IsZero() {
				idleSince = now
			}
			if now.Sub(idleSince) >= routerIdleExit {
				logger.Infof("router: no tunnel daemon and no control panel for %s; exiting", routerIdleExit)
				stop()
				return
			}
		}
	}
}

// countLiveTunnels counts the recorded tunnel daemons that answer, and
// returns why the others did not (nil when all did, or none is
// recorded). It only reads: leftover state files are cleaned up by the
// CLI commands.
func countLiveTunnels(ctx context.Context) (int, error) {
	names, err := listTunnelStateNames()
	if err != nil {
		return 0, fmt.Errorf("list the tunnel state files: %w", err)
	}
	n := 0
	var errs []error
	for _, name := range names {
		st, err := readTunnelState(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if _, err := queryTunnelStatus(ctx, st, tunnelControlTimeout); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// newRouterControlHandler serves GET /status and POST /stop, guarded by
// the bearer token like the tunnel daemon's control channel.
func newRouterControlHandler(token string, status func() *RouterStatus, stop func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if !tunnelControlAuthorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if !tunnelControlAuthorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, "stopping\n")
		go stop()
	})
	return mux
}
