package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// This file holds the shared types of `nakama aws tunnel` (ADR 0017):
// the -L forward specification, the SSM port-forwarding document
// mapping, the on-disk daemon state, and the status model served over
// the daemon's control channel.

// ForwardSpec is one parsed -L port forward.
type ForwardSpec struct {
	// LocalPort is the local listener port (bound by the vendored
	// plugin inside the session-runner child).
	LocalPort string
	// RemoteHost, when non-empty, forwards through the instance to
	// this host (AWS-StartPortForwardingSessionToRemoteHost).
	RemoteHost string
	// RemotePort is the target port.
	RemotePort string
}

// ParseForwardSpec parses an -L spec: "<local>:<remote>" forwards to
// the instance itself; "<local>:<host>:<remote>" forwards through the
// instance to <host>.
func ParseForwardSpec(s string) (ForwardSpec, error) {
	parts := strings.Split(s, ":")
	var f ForwardSpec
	switch len(parts) {
	case 2:
		f = ForwardSpec{LocalPort: parts[0], RemotePort: parts[1]}
	case 3:
		f = ForwardSpec{LocalPort: parts[0], RemoteHost: parts[1], RemotePort: parts[2]}
	default:
		return ForwardSpec{}, fmt.Errorf("invalid -L %q: want <local>:<remote> or <local>:<host>:<remote>", s)
	}
	if err := validatePort(f.LocalPort); err != nil {
		return ForwardSpec{}, fmt.Errorf("invalid -L %q: local port: %w", s, err)
	}
	if err := validatePort(f.RemotePort); err != nil {
		return ForwardSpec{}, fmt.Errorf("invalid -L %q: remote port: %w", s, err)
	}
	if len(parts) == 3 && f.RemoteHost == "" {
		return ForwardSpec{}, fmt.Errorf("invalid -L %q: empty host", s)
	}
	return f, nil
}

// validatePort checks that p is a decimal TCP port number.
func validatePort(p string) error {
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q is not a valid port (1-65535)", p)
	}
	return nil
}

// String renders the spec back to its -L form.
func (f ForwardSpec) String() string {
	if f.RemoteHost != "" {
		return f.LocalPort + ":" + f.RemoteHost + ":" + f.RemotePort
	}
	return f.LocalPort + ":" + f.RemotePort
}

// Describe renders a human-readable "localhost:L -> target:R" line for
// start/status output.
func (f ForwardSpec) Describe(instanceName string) string {
	target := instanceName
	if f.RemoteHost != "" {
		target = f.RemoteHost + " (via " + instanceName + ")"
	}
	return fmt.Sprintf("localhost:%s -> %s:%s", f.LocalPort, target, f.RemotePort)
}

// portForwardDocument maps a ForwardSpec to the SSM document name and
// parameters. One SSM session forwards exactly one port, so each -L
// gets its own supervised session.
func portForwardDocument(f ForwardSpec) (*string, map[string][]string) {
	params := map[string][]string{
		"portNumber":      {f.RemotePort},
		"localPortNumber": {f.LocalPort},
	}
	if f.RemoteHost != "" {
		params["host"] = []string{f.RemoteHost}
		return aws.String("AWS-StartPortForwardingSessionToRemoteHost"), params
	}
	return aws.String("AWS-StartPortForwardingSession"), params
}

// ParseForwardSpecs parses all -L values, rejecting duplicate local
// ports (two sessions cannot bind the same listener).
func ParseForwardSpecs(specs []string) ([]ForwardSpec, error) {
	if len(specs) == 0 {
		return nil, errors.New("at least one -L <local>:<remote> or <local>:<host>:<remote> forward is required")
	}
	seen := map[string]bool{}
	out := make([]ForwardSpec, 0, len(specs))
	for _, s := range specs {
		f, err := ParseForwardSpec(s)
		if err != nil {
			return nil, err
		}
		if seen[f.LocalPort] {
			return nil, fmt.Errorf("duplicate local port %s in -L specs", f.LocalPort)
		}
		seen[f.LocalPort] = true
		out = append(out, f)
	}
	return out, nil
}

// TunnelState is the daemon state persisted at
// ~/.config/nakama/tunnels/<name>.json (0600). It is the only place
// the control endpoint and its bearer token live; liveness is decided
// by querying the control endpoint, not by probing the PID.
type TunnelState struct {
	Name        string    `json:"name"`
	PID         int       `json:"pid"`
	ControlPort int       `json:"controlPort"` // 127.0.0.1-bound control channel
	Token       string    `json:"token"`       // bearer token for /status and /stop
	Forwards    []string  `json:"forwards"`    // -L specs, for display without the daemon
	StartedAt   time.Time `json:"startedAt"`
}

// tunnelStateDirOverride redirects the state directory in tests so
// they never touch the real user configuration.
var tunnelStateDirOverride string

// tunnelStateDir resolves the tunnel state directory:
// $XDG_CONFIG_HOME/nakama/tunnels when XDG_CONFIG_HOME is set and
// non-empty, otherwise ~/.config/nakama/tunnels (same convention as
// internal/profilestore).
func tunnelStateDir() (string, error) {
	if tunnelStateDirOverride != "" {
		return tunnelStateDirOverride, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "nakama", "tunnels"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for nakama tunnels: %w", err)
	}
	return filepath.Join(home, ".config", "nakama", "tunnels"), nil
}

// tunnelStatePath returns the state file path for the instance name.
func tunnelStatePath(name string) (string, error) {
	dir, err := tunnelStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// writeTunnelState persists st exclusively: an existing file means
// another daemon owns (or owned) this tunnel name, and the caller must
// decide staleness first. 0600 because the file contains the control
// token.
func writeTunnelState(st *TunnelState) error {
	path, err := tunnelStatePath(st.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create tunnel state dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path is derived from the validated instance name under the user config dir
	if err != nil {
		return fmt.Errorf("create tunnel state file (another tunnel for %q may be starting): %w", st.Name, err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write tunnel state: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close tunnel state: %w", err)
	}
	return nil
}

// readTunnelState loads the state for name. A missing file returns
// (*NoTunnelError) so callers can distinguish "not running" from real
// failures.
func readTunnelState(name string) (*TunnelState, error) {
	path, err := tunnelStatePath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- path is derived from the validated instance name under the user config dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &NoTunnelError{Name: name}
		}
		return nil, fmt.Errorf("read tunnel state: %w", err)
	}
	var st TunnelState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse tunnel state %s: %w", path, err)
	}
	return &st, nil
}

// removeTunnelState deletes the state file; a missing file is not an
// error (stop and daemon shutdown may race benignly).
func removeTunnelState(name string) error {
	path, err := tunnelStatePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove tunnel state: %w", err)
	}
	return nil
}

// NoTunnelError reports that no tunnel daemon is running for the
// instance.
type NoTunnelError struct{ Name string }

func (e *NoTunnelError) Error() string {
	return fmt.Sprintf("no tunnel is running for %q", e.Name)
}

// TunnelAlreadyRunningError reports that a live daemon already
// forwards ports for the instance; Status carries its current
// forwards for display.
type TunnelAlreadyRunningError struct{ Status *TunnelStatus }

func (e *TunnelAlreadyRunningError) Error() string {
	return fmt.Sprintf("a tunnel is already running for %q", e.Status.Name)
}

// ForwardState is the lifecycle state of one supervised forward.
type ForwardState string

const (
	// ForwardStateConnecting means StartSession has not succeeded yet.
	ForwardStateConnecting ForwardState = "connecting"
	// ForwardStateActive means a session-runner child is serving the port.
	ForwardStateActive ForwardState = "active"
	// ForwardStateReconnecting means disconnected, retrying with backoff.
	ForwardStateReconnecting ForwardState = "reconnecting"
	// ForwardStateDead means supervision ended; Detail explains why.
	ForwardStateDead ForwardState = "dead"
)

// ForwardStatus is the observable state of one forward, served by the
// daemon's control channel and rendered by `tunnel status`.
type ForwardStatus struct {
	Spec      string       `json:"spec"`
	State     ForwardState `json:"state"`
	Detail    string       `json:"detail,omitempty"`
	SessionID string       `json:"sessionId,omitempty"`
	Since     time.Time    `json:"since"`
	// Attempts is the reconnect budget consumed so far (resets after a
	// stable session, see ADR 0014).
	Attempts int `json:"reconnectAttempts"`
}

// TunnelStatus is the full daemon status.
type TunnelStatus struct {
	Name      string          `json:"name"`
	PID       int             `json:"pid"`
	StartedAt time.Time       `json:"startedAt"`
	Forwards  []ForwardStatus `json:"forwards"`
}
