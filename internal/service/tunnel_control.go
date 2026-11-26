package service

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The tunnel daemon's control channel (ADR 0017): a 127.0.0.1-bound
// HTTP endpoint guarded by a random bearer token from the state file.
// It exists because Windows offers no cross-platform way to ask a
// detached process to shut down gracefully (killing the daemon would
// orphan its session-runner children and keep the forwards alive), and
// because `tunnel status` needs live per-forward state either way.

// tunnelControl is what the control channel serves.
type tunnelControl struct {
	status func() *TunnelStatus
	stop   func()
	// add starts a new forward (POST /forwards, Issue #118).
	add func(f ForwardSpec) (ForwardStatus, error)
}

// addForwardRequest is the body of POST /forwards.
type addForwardRequest struct {
	// Forward is the spec in its Stored form.
	Forward string `json:"forward"`
}

// newTunnelControlHandler serves GET /status, POST /stop and POST
// /forwards.
func newTunnelControlHandler(token string, c tunnelControl) http.Handler {
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
		_ = json.NewEncoder(w).Encode(c.status())
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
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "stopping\n")
		// Trigger asynchronously so this response can be written before
		// the daemon tears the listener down.
		go c.stop()
	})
	mux.HandleFunc("/forwards", func(w http.ResponseWriter, r *http.Request) {
		if !tunnelControlAuthorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req addForwardRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		f, err := ParseStoredForwardSpec(req.Forward)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		st, err := c.add(f)
		if err != nil {
			code := http.StatusInternalServerError
			var inUse *LocalPortInUseError
			if errors.As(err, &inUse) || errors.Is(err, errForwardConflict) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	return mux
}

// tunnelControlAuthorized checks the bearer token in constant time.
func tunnelControlAuthorized(r *http.Request, token string) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := strings.TrimPrefix(auth, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// tunnelControlError is a non-200 answer of the control channel.
type tunnelControlError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *tunnelControlError) Error() string {
	return fmt.Sprintf("tunnel control %s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Message)
}

// tunnelControlRequest issues an authorized request against a daemon's
// control channel, sending in (when non-nil) as JSON and decoding the
// answer into out (when non-nil). Injected into Service via tunnelHooks
// so orchestration tests need no real daemon.
func tunnelControlRequest(ctx context.Context, st *TunnelState, method, path string, in, out any) error {
	return controlRequest(ctx, st.ControlPort, st.Token, method, path, in, out)
}

// controlClient sends every control-channel request. It is shared and
// keeps no connection open: a connection per request is closed once its
// answer is read (Issue #135). Before, each request built its own
// Transport with keep-alive and no idle timeout, so each one left a
// loopback connection open in this process and in the daemon. The
// router asks every 15 seconds, and more often while it routes, so after
// a few days the connections to one daemon used up the local ephemeral
// ports (28,232 with Linux's default range); the requests then failed
// and the router took the running daemon for gone and exited. The
// daemon is local, so a new connection per request costs nothing worth
// pooling for.
var controlClient = &http.Client{
	Transport: &http.Transport{
		// The daemon is local; never route control traffic through a
		// proxy that HTTP_PROXY etc. might configure.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 2 * time.Second,
		}).DialContext,
		DisableKeepAlives: true,
	},
}

// controlIdleTimeout closes a control-channel connection that waits this
// long for its next request. controlClient does not keep connections, but
// clients of the versions before Issue #135 do, and a daemon or a router
// can outlive the CLI that talks to it (a router of an older version asks
// a newer tunnel daemon, and the other way round).
var controlIdleTimeout = 30 * time.Second

// newControlServer is the HTTP server of a daemon's control channel (the
// tunnel daemons and the router).
func newControlServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       controlIdleTimeout,
	}
}

// controlRequest is the request side of every nakama daemon's control
// channel (the tunnel daemons and the local router): a 127.0.0.1 HTTP
// endpoint guarded by a bearer token from the daemon's state file.
func controlRequest(ctx context.Context, controlPort int, token, method, path string, in, out any) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", controlPort, path)
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	// The host is always 127.0.0.1; only the port comes from the daemon's
	// own state file.
	req, err := http.NewRequestWithContext(ctx, method, url, body) // #nosec G704 -- loopback control channel, see above
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := controlClient.Do(req) // #nosec G704 -- loopback control channel (127.0.0.1)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &tunnelControlError{Method: method, Path: path, Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode tunnel control response: %w", err)
		}
	}
	return nil
}

// queryTunnelStatus asks a (possibly dead) daemon for its status.
func queryTunnelStatus(ctx context.Context, st *TunnelState, timeout time.Duration) (*TunnelStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var status TunnelStatus
	if err := tunnelControlRequest(ctx, st, http.MethodGet, "/status", nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// requestTunnelStop asks the daemon to shut down gracefully.
func requestTunnelStop(ctx context.Context, st *TunnelState, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return tunnelControlRequest(ctx, st, http.MethodPost, "/stop", nil, nil)
}

// ErrTunnelDaemonTooOld reports a running daemon from a nakama version
// without POST /forwards.
var ErrTunnelDaemonTooOld = errors.New("the running tunnel daemon was started by an older nakama and cannot take new forwards")

// requestTunnelAddForward asks the daemon to start one more forward.
func requestTunnelAddForward(ctx context.Context, st *TunnelState, f ForwardSpec, timeout time.Duration) (*ForwardStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out ForwardStatus
	err := tunnelControlRequest(ctx, st, http.MethodPost, "/forwards", addForwardRequest{Forward: f.Stored()}, &out)
	var ce *tunnelControlError
	if errors.As(err, &ce) {
		switch ce.Status {
		case http.StatusNotFound:
			return nil, ErrTunnelDaemonTooOld
		case http.StatusConflict:
			return nil, &LocalPortInUseError{Port: f.LocalPort, Holder: st.Name, Err: errors.New(ce.Message)}
		}
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}
