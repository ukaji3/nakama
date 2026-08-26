package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
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

// tunnelControlHandler serves GET /status and POST /stop.
func newTunnelControlHandler(token string, status func() *TunnelStatus, requestStop func()) http.Handler {
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
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "stopping\n")
		// Trigger asynchronously so this response can be written before
		// the daemon tears the listener down.
		go requestStop()
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

// tunnelControlClient issues authorized requests against a daemon's
// control channel. Injected into Service via tunnelHooks so
// orchestration tests need no real daemon.
func tunnelControlRequest(ctx context.Context, st *TunnelState, method, path string, out any) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", st.ControlPort, path)
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+st.Token)
	client := &http.Client{
		Transport: &http.Transport{
			// The daemon is local; never route control traffic through a
			// proxy that HTTP_PROXY etc. might configure.
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout: 2 * time.Second,
			}).DialContext,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("tunnel control %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
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
	if err := tunnelControlRequest(ctx, st, http.MethodGet, "/status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// requestTunnelStop asks the daemon to shut down gracefully.
func requestTunnelStop(ctx context.Context, st *TunnelState, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return tunnelControlRequest(ctx, st, http.MethodPost, "/stop", nil)
}
