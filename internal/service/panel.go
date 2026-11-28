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

// This file is the process side of the nakama control panel (Issue #139,
// ADR 0031): the browser-facing server of `nakama ui` runs in that
// command's own process (in the foreground: closing the window stops
// it), on a free 127.0.0.1 port. The router sends
// http://control.nakama.localhost:<router port>/ there, and it keeps a
// control channel like the daemons' for a second `nakama ui` (a new
// sign-in code) and for the router (is it alive). Its state file sits
// next to router.json, never in the tunnel state directory.

// panelHostName is the host name of the control panel behind the router.
const panelHostName = "control." + routerDomain

// PanelHost returns the Host of the control panel behind the router on
// port: control.nakama.localhost:<port>.
func PanelHost(port int) string {
	return net.JoinHostPort(panelHostName, strconv.Itoa(port))
}

// PanelOrigin returns the browser origin of the control panel (no
// trailing slash).
func PanelOrigin(port int) string { return "http://" + PanelHost(port) }

// PanelState is the control panel's state file,
// ~/.config/nakama/panel.json (0600: it holds the control token).
type PanelState struct {
	PID int `json:"pid"`
	// Port is the loopback port of the browser-facing server.
	Port        int `json:"port"`
	ControlPort int `json:"controlPort"`
	// RouterPort is the router port the panel answers for.
	RouterPort int       `json:"routerPort"`
	Token      string    `json:"token"`
	Version    string    `json:"version"`
	StartedAt  time.Time `json:"startedAt"`
}

// PanelStatus is what the panel's control channel reports.
type PanelStatus struct {
	PID        int       `json:"pid"`
	RouterPort int       `json:"routerPort"`
	Version    string    `json:"version"`
	StartedAt  time.Time `json:"startedAt"`
}

// panelSignInCode is the answer of POST /sign-in-code.
type panelSignInCode struct {
	Code string `json:"code"`
}

// NoPanelError reports that no control panel state is recorded.
type NoPanelError struct{}

func (*NoPanelError) Error() string { return "the nakama control panel is not running" }

func isNoPanel(err error) bool {
	var np *NoPanelError
	return errors.As(err, &np)
}

// panelStatePath is panel.json in the nakama config directory.
func panelStatePath() (string, error) {
	dir, err := tunnelStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "panel.json"), nil
}

// writePanelState writes the state exclusively, like writeRouterState.
func writePanelState(st *PanelState) error {
	path, err := panelStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the nakama config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- fixed name under the user config dir
	if err != nil {
		return fmt.Errorf("create the control panel state file: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write the control panel state file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close the control panel state file: %w", err)
	}
	return nil
}

// readPanelState loads the state; a missing file is *NoPanelError.
func readPanelState() (*PanelState, error) {
	path, err := panelStatePath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- fixed name under the user config dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &NoPanelError{}
		}
		return nil, fmt.Errorf("read the control panel state file: %w", err)
	}
	var st PanelState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse the control panel state file %s: %w", path, err)
	}
	return &st, nil
}

// removePanelStateOwned deletes the state file while it still carries
// token (the panel generation the caller observed).
func removePanelStateOwned(token string) error {
	st, err := readPanelState()
	if err != nil {
		if isNoPanel(err) {
			return nil
		}
		return err
	}
	if st.Token != token {
		return nil
	}
	path, err := panelStatePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove the control panel state file: %w", err)
	}
	return nil
}

func queryPanelStatus(ctx context.Context, st *PanelState, timeout time.Duration) (*PanelStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var status PanelStatus
	if err := controlRequest(ctx, st.ControlPort, st.Token, http.MethodGet, "/status", nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func requestPanelSignInCode(ctx context.Context, st *PanelState, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out panelSignInCode
	if err := controlRequest(ctx, st.ControlPort, st.Token, http.MethodPost, "/sign-in-code", nil, &out); err != nil {
		return "", err
	}
	if out.Code == "" {
		return "", errors.New("the control panel returned no sign-in code")
	}
	return out.Code, nil
}

func requestPanelStop(ctx context.Context, st *PanelState, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return controlRequest(ctx, st.ControlPort, st.Token, http.MethodPost, "/stop", nil, nil)
}

// livePanel returns the state of the control panel when one answers.
// It only reads: a leftover state file is removed by `nakama ui`.
func livePanel(ctx context.Context) (*PanelState, *PanelStatus, error) {
	st, err := readPanelState()
	if err != nil {
		return nil, nil, err
	}
	status, err := queryPanelStatus(ctx, st, tunnelControlTimeout)
	if err != nil {
		return nil, nil, err
	}
	return st, status, nil
}

// panelAlive reports whether a control panel answers; the router keeps
// running while one does.
func panelAlive(ctx context.Context) bool {
	_, _, err := livePanel(ctx)
	return err == nil
}

// FindPanel returns the running control panel, or nil when none answers
// (a leftover state file is removed). It needs no AWS credentials.
func FindPanel(ctx context.Context) (*PanelStatus, error) {
	st, err := readPanelState()
	if err != nil {
		if isNoPanel(err) {
			return nil, nil
		}
		return nil, err
	}
	status, err := queryPanelStatus(ctx, st, tunnelControlTimeout)
	if err != nil {
		if rmErr := removePanelStateOwned(st.Token); rmErr != nil {
			return nil, rmErr
		}
		return nil, nil
	}
	return status, nil
}

// PanelSignInCode asks the running control panel for a new one-time
// sign-in code (a second `nakama ui`).
func PanelSignInCode(ctx context.Context) (string, error) {
	st, err := readPanelState()
	if err != nil {
		return "", err
	}
	return requestPanelSignInCode(ctx, st, tunnelControlTimeout)
}

// panelStopTimeout bounds the wait for a stopped panel to exit.
var panelStopTimeout = 15 * time.Second

// StopPanel asks the running control panel to stop and waits until it
// removed its state file.
func StopPanel(ctx context.Context) error {
	st, err := readPanelState()
	if err != nil {
		if isNoPanel(err) {
			return nil
		}
		return err
	}
	if err := requestPanelStop(ctx, st, tunnelControlTimeout); err != nil {
		return fmt.Errorf("the running nakama control panel (pid %d, %s) did not take the stop request: %w\n"+
			"close its window (or end that process) and try again", st.PID, st.Version, err)
	}
	deadline := time.Now().Add(panelStopTimeout)
	for {
		cur, err := readPanelState()
		if (err != nil && isNoPanel(err)) || (err == nil && cur.Token != st.Token) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the nakama control panel (pid %d) acknowledged stop but did not exit within %s", st.PID, panelStopTimeout)
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// PanelRunInput parameterizes RunPanel.
type PanelRunInput struct {
	// RouterPort is the router port the panel answers for.
	RouterPort int
	// Handler serves the browser (internal/panel).
	Handler http.Handler
	// SignInCode mints a one-time sign-in code for a browser.
	SignInCode func() (string, error)
	// Ready is called once the panel serves and its state file is
	// written; an error stops the panel and is returned by RunPanel.
	Ready func(st *PanelState) error
	// Logf reports what the panel does (the CLI's stderr).
	Logf func(format string, args ...any)
}

// RunPanel serves the control panel until ctx ends or the control
// channel asks it to stop. Another live panel is an error (the CLI hands
// the browser to it instead); a leftover state file is replaced.
func RunPanel(ctx context.Context, in PanelRunInput) error {
	if in.Handler == nil || in.SignInCode == nil {
		return errors.New("RunPanel: Handler and SignInCode are required")
	}
	logf := in.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for the control panel: %w", err)
	}
	defer func() { _ = front.Close() }()
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on the control panel's control channel: %w", err)
	}
	defer func() { _ = control.Close() }()
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("generate the control panel token: %w", err)
	}
	state := &PanelState{
		PID:         os.Getpid(),
		Port:        front.Addr().(*net.TCPAddr).Port,
		ControlPort: control.Addr().(*net.TCPAddr).Port,
		RouterPort:  in.RouterPort,
		Token:       hex.EncodeToString(tokenBytes),
		Version:     version.String(),
		StartedAt:   time.Now().UTC(),
	}
	if err := writePanelState(state); err != nil {
		if old, rerr := readPanelState(); rerr == nil {
			if _, qerr := queryPanelStatus(ctx, old, tunnelControlTimeout); qerr == nil {
				return fmt.Errorf("another nakama control panel (pid %d) is running", old.PID)
			}
			_ = removePanelStateOwned(old.Token)
		}
		if err := writePanelState(state); err != nil {
			return err
		}
	}
	defer func() { _ = removePanelStateOwned(state.Token) }()

	stopCh := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() { stopOnce.Do(func() { close(stopCh) }) }

	server := &http.Server{
		Handler:           in.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	serveErr := make(chan error, 2)
	go func() {
		if err := server.Serve(front); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("serve the control panel: %w", err)
		}
	}()
	controlServer := newControlServer(newPanelControlHandler(state, in.SignInCode, requestStop))
	go func() {
		if err := controlServer.Serve(control); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("serve the control panel's control channel: %w", err)
		}
	}()
	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		_ = controlServer.Shutdown(shutdownCtx)
	}

	if in.Ready != nil {
		if err := in.Ready(state); err != nil {
			shutdown()
			return err
		}
	}
	var runErr error
	select {
	case <-stopCh:
		logf("control panel: stop requested")
	case <-ctx.Done():
	case runErr = <-serveErr:
	}
	shutdown()
	return runErr
}

// newPanelControlHandler serves GET /status, POST /sign-in-code and POST
// /stop, guarded by the bearer token like the daemons' control channels.
func newPanelControlHandler(st *PanelState, signInCode func() (string, error), stop func()) http.Handler {
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request, method string) bool {
		if !tunnelControlAuthorized(r, st.Token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return false
		}
		return true
	}
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r, http.MethodGet) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&PanelStatus{PID: st.PID, RouterPort: st.RouterPort, Version: st.Version, StartedAt: st.StartedAt})
	})
	mux.HandleFunc("/sign-in-code", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r, http.MethodPost) {
			return
		}
		code, err := signInCode()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(panelSignInCode{Code: code})
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r, http.MethodPost) {
			return
		}
		_, _ = io.WriteString(w, "stopping\n")
		go stop()
	})
	return mux
}
