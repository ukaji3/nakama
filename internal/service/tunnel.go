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
	// LocalPort is the local listener port. The tunnel daemon binds it
	// itself (127.0.0.1 and ::1) and relays each connection to the
	// session-runner child's plugin, which listens on a private port
	// of its own (Issue #118).
	LocalPort string
	// RemoteHost, when non-empty, forwards through the instance to
	// this host (AWS-StartPortForwardingSessionToRemoteHost).
	RemoteHost string
	// RemotePort is the target port.
	RemotePort string
	// Purpose marks the forwards nakama opens for a browser itself
	// (`nakama aws code` / `nakama aws kirocrew`). It selects the HTTP
	// health check the daemon runs through the forward and the label in
	// `tunnel status`. Forwards given with -L have none.
	Purpose ForwardPurpose
}

// ForwardPurpose names what a forward carries when nakama opened it for
// a browser (Issue #118).
type ForwardPurpose string

const (
	// ForwardPurposeNone is a plain -L forward: the daemon knows nothing
	// about its protocol and runs no health check through it.
	ForwardPurposeNone ForwardPurpose = ""
	// ForwardPurposeCodeServer is the code-server forward of
	// `nakama aws code`.
	ForwardPurposeCodeServer ForwardPurpose = "code-server"
	// ForwardPurposeKiroCrew is the Kiro Crew forward of
	// `nakama aws kirocrew`.
	ForwardPurposeKiroCrew ForwardPurpose = "kirocrew"
)

// healthPath is the HTTP path the daemon requests through a forward of
// this purpose to tell whether the far end still answers. Both are
// served without authentication: code-server's /healthz (registered
// before its auth middleware) and Kiro Crew's /api/health (one of its
// PROBE_PATHS, exempt from the token and Host checks). Empty means no
// health check.
func (p ForwardPurpose) healthPath() string {
	switch p {
	case ForwardPurposeCodeServer:
		return "/healthz"
	case ForwardPurposeKiroCrew:
		return "/api/health"
	default:
		return ""
	}
}

// valid reports whether p is a purpose nakama knows.
func (p ForwardPurpose) valid() bool {
	switch p {
	case ForwardPurposeNone, ForwardPurposeCodeServer, ForwardPurposeKiroCrew:
		return true
	default:
		return false
	}
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

// String renders the spec back to its -L form. The purpose is not part
// of it (see Stored).
func (f ForwardSpec) String() string {
	if f.RemoteHost != "" {
		return f.LocalPort + ":" + f.RemoteHost + ":" + f.RemotePort
	}
	return f.LocalPort + ":" + f.RemotePort
}

// storedPurposeSeparator separates the purpose from the -L form in the
// stored spec ("code-server=8080:8080"). "=" can never appear in a
// valid -L spec, so a user-supplied -L value carrying it is rejected by
// ParseForwardSpec as before.
const storedPurposeSeparator = "="

// Stored renders the spec in the form the tunnel state file and the
// daemon's --forward flags carry: the -L form, prefixed with the
// purpose when there is one. Plain forwards stay in the -L form, so
// state files written by earlier versions read back unchanged.
func (f ForwardSpec) Stored() string {
	if f.Purpose == ForwardPurposeNone {
		return f.String()
	}
	return string(f.Purpose) + storedPurposeSeparator + f.String()
}

// ParseStoredForwardSpec parses the Stored form: a -L spec, optionally
// prefixed with "<purpose>=".
func ParseStoredForwardSpec(s string) (ForwardSpec, error) {
	purpose, spec, found := strings.Cut(s, storedPurposeSeparator)
	if !found {
		return ParseForwardSpec(s)
	}
	p := ForwardPurpose(purpose)
	if p == ForwardPurposeNone || !p.valid() {
		return ForwardSpec{}, fmt.Errorf("invalid forward %q: unknown purpose %q", s, purpose)
	}
	f, err := ParseForwardSpec(spec)
	if err != nil {
		return ForwardSpec{}, err
	}
	f.Purpose = p
	return f, nil
}

// ParseStoredForwardSpecs parses the forwards recorded in a tunnel state
// file (or handed to the daemon), rejecting duplicate local ports like
// ParseForwardSpecs.
func ParseStoredForwardSpecs(specs []string) ([]ForwardSpec, error) {
	if len(specs) == 0 {
		return nil, errors.New("no forwards given")
	}
	seen := map[string]bool{}
	out := make([]ForwardSpec, 0, len(specs))
	for _, s := range specs {
		f, err := ParseStoredForwardSpec(s)
		if err != nil {
			return nil, err
		}
		if seen[f.LocalPort] {
			return nil, fmt.Errorf("duplicate local port %s in forwards", f.LocalPort)
		}
		seen[f.LocalPort] = true
		out = append(out, f)
	}
	return out, nil
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
// gets its own supervised session. The tunnel daemon passes a copy of
// the spec whose LocalPort is the plugin's private port (the one the
// daemon relays to), not the user's port (Issue #118).
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
	Forwards    []string  `json:"forwards"`    // Stored specs, for restart and display without the daemon
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

// rewriteTunnelState replaces the state file of a running daemon (the
// forward list grows when `code` adds a forward, Issue #118). It only
// replaces a file that still carries st.Token, i.e. one this daemon
// generation owns, and swaps it in with a rename so a reader never sees
// a half-written file.
func rewriteTunnelState(st *TunnelState) error {
	current, err := readTunnelState(st.Name)
	if err != nil {
		return err
	}
	if current.Token != st.Token {
		return fmt.Errorf("tunnel state for %q belongs to another daemon", st.Name)
	}
	path, err := tunnelStatePath(st.Name)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+st.Name+".*.tmp")
	if err != nil {
		return fmt.Errorf("create tunnel state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op after a successful rename
	// os.CreateTemp creates the file with mode 0600, which the control
	// token needs.
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tunnel state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tunnel state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace tunnel state: %w", err)
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
// removeTunnelStateOwned deletes the state file only while it still
// carries the given control token, i.e. while it belongs to the
// daemon generation the caller actually observed. Without the check,
// cleaning up a daemon that was merely slow to answer could delete
// the state file a newer daemon just wrote (or the old daemon's exit
// cleanup could delete the new daemon's file). The read-check-delete
// window is not atomic, but it shrinks the race from "any time" to
// microseconds. A missing file is not an error.
func removeTunnelStateOwned(name, token string) error {
	st, err := readTunnelState(name)
	if err != nil {
		var missing *NoTunnelError
		if errors.As(err, &missing) {
			return nil
		}
		return err
	}
	if st.Token != token {
		return nil
	}
	return removeTunnelState(name)
}

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
	// ForwardStateIdle means the local port is open but no session is
	// up: nothing has used the forward for a while, so the daemon waits
	// for the next connection before it starts one (Issue #118).
	ForwardStateIdle ForwardState = "idle"
	// ForwardStateDead means supervision ended; Detail explains why.
	ForwardStateDead ForwardState = "dead"
)

// ForwardStatus is the observable state of one forward, served by the
// daemon's control channel and rendered by `tunnel status`.
type ForwardStatus struct {
	Spec string `json:"spec"`
	// Purpose is set for the forwards of `code` / `kirocrew`.
	Purpose   ForwardPurpose `json:"purpose,omitempty"`
	State     ForwardState   `json:"state"`
	Detail    string         `json:"detail,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
	Since     time.Time      `json:"since"`
	// Attempts is the reconnect budget consumed so far (resets after a
	// stable session, see ADR 0014).
	Attempts int `json:"reconnectAttempts"`
	// LastErrorCode is the AWS error code of the latest failed attempt
	// (e.g. TargetNotConnected), so a caller can add the matching
	// advice without parsing Detail.
	LastErrorCode string `json:"lastErrorCode,omitempty"`
	// URL is the router address of the forward, when it is the browser
	// forward of code-server or Kiro Crew and the router runs (Issue
	// #124). The CLI process fills it in; the daemon never sends it.
	URL string `json:"url,omitempty"`
}

// TunnelStatus is the full daemon status.
type TunnelStatus struct {
	Name      string          `json:"name"`
	PID       int             `json:"pid"`
	StartedAt time.Time       `json:"startedAt"`
	Forwards  []ForwardStatus `json:"forwards"`
}
