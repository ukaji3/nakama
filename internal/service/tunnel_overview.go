package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TunnelOverview is one instance's entry in the cross-instance tunnel
// listing (`nakama aws tunnel status` without a name).
type TunnelOverview struct {
	Name string `json:"name"`
	// Status is the live status when the daemon answered.
	Status *TunnelStatus `json:"status,omitempty"`
	// Stale is true when a state file exists but its daemon does not
	// answer; the file has been cleaned up by then.
	Stale bool `json:"stale,omitempty"`
	// Err explains why the entry could not be read (other than stale).
	Err string `json:"error,omitempty"`
}

// ListTunnelStatus reports every locally recorded tunnel across
// instances (Issue #55). Instances whose daemon no longer answers are
// reported as stale — GetTunnelStatus removes their state file as part
// of the probe, the same cleanup the single-instance path performs.
func (s *Service) ListTunnelStatus(ctx context.Context) ([]TunnelOverview, error) {
	names, err := listTunnelStateNames()
	if err != nil {
		return nil, fmt.Errorf("read the tunnel state directory: %w", err)
	}
	out := make([]TunnelOverview, 0, len(names))
	for _, name := range names {
		item := TunnelOverview{Name: name}
		st, err := s.GetTunnelStatus(ctx, name)
		switch {
		case err == nil:
			item.Status = st
		case isNoTunnel(err):
			item.Stale = true
		default:
			item.Err = err.Error()
		}
		out = append(out, item)
	}
	return out, nil
}

// isNoTunnel reports whether err is the "no tunnel for this name"
// sentinel (state file absent, or removed because the daemon was gone).
func isNoTunnel(err error) bool {
	var nt *NoTunnelError
	return errors.As(err, &nt)
}

// ListeningPort is one listening TCP socket observed on the instance.
type ListeningPort struct {
	Port int `json:"port"`
	// Address is the bind address as reported by the instance.
	Address string `json:"address"`
	// Process is the owning program when the instance reported one.
	Process string `json:"process,omitempty"`
	// Label names well-known nakama ports (e.g. code-server).
	Label string `json:"label,omitempty"`
	// Spec is the -L argument that would forward this port.
	Spec string `json:"spec"`
}

// TunnelSuggestion is the result of `nakama aws tunnel suggest`.
type TunnelSuggestion struct {
	Name  string          `json:"name"`
	Ports []ListeningPort `json:"ports"`
	// Command is a ready-to-run tunnel command covering every port.
	Command string `json:"command,omitempty"`
}

// wellKnownInstancePorts labels ports nakama itself puts on instances.
var wellKnownInstancePorts = map[int]string{
	8080: "code-server",
	22:   "sshd (use `nakama aws ssh` instead)",
}

// SuggestTunnel lists the TCP ports listening on the instance and turns
// them into ready-made `-L` specs (Issue #55). Read-only: it runs `ss`
// (falling back to netstat) over SSM Run Command and changes nothing.
func (s *Service) SuggestTunnel(ctx context.Context, name string) (*TunnelSuggestion, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	instanceID, err := s.resolveInstanceIDForSSH(ctx, name)
	if err != nil {
		return nil, err
	}
	stdout, err := s.runRemoteScript(ctx, instanceID, listeningPortsScript(),
		"nakama: list listening TCP ports", 3*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("list listening ports on %s: %w", name, err)
	}
	ports := parseListeningPorts(stdout)
	sug := &TunnelSuggestion{Name: name, Ports: ports}
	if cmd := suggestedTunnelCommand(name, ports); cmd != "" {
		sug.Command = cmd
	}
	return sug, nil
}

// listeningPortsScript prints one "addr:port program" line per
// listening TCP socket. `ss` is present on Ubuntu; netstat is a
// fallback for older images.
func listeningPortsScript() string {
	return `set -u
if command -v ss >/dev/null 2>&1; then
  ss -ltnH 2>/dev/null | awk '{print "NAKAMA_PORT " $4 " " $NF}'
else
  netstat -ltn 2>/dev/null | awk '/^tcp/ {print "NAKAMA_PORT " $4 " -"}'
fi
`
}

// parseListeningPorts extracts the listening sockets from the script
// output. Loopback-only sockets are kept (forwarding is exactly how a
// developer reaches them), duplicates by port are collapsed, and the
// result is sorted by port.
func parseListeningPorts(stdout string) []ListeningPort {
	seen := map[int]ListeningPort{}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || fields[0] != "NAKAMA_PORT" {
			continue
		}
		addr := fields[1]
		port, ok := portFromSocketAddr(addr)
		if !ok {
			continue
		}
		p := ListeningPort{
			Port:    port,
			Address: addr,
			Label:   wellKnownInstancePorts[port],
			Spec:    fmt.Sprintf("%d:%d", port, port),
		}
		if len(fields) > 2 && fields[2] != "-" {
			p.Process = processName(fields[2])
		}
		// Prefer the entry that carries a process name.
		if prev, dup := seen[port]; dup && prev.Process != "" {
			continue
		}
		seen[port] = p
	}
	out := make([]ListeningPort, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// portFromSocketAddr extracts the port from an ss/netstat local address
// ("0.0.0.0:8080", "127.0.0.1:5432", "[::]:22", "*:8080").
func portFromSocketAddr(addr string) (int, bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 || i == len(addr)-1 {
		return 0, false
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// processName pulls the program name out of the ss users:(...) field,
// e.g. `users:(("node",pid=1234,fd=20))` => node.
func processName(field string) string {
	if i := strings.Index(field, `("`); i >= 0 {
		rest := field[i+2:]
		if j := strings.Index(rest, `"`); j > 0 {
			return rest[:j]
		}
	}
	return strings.Trim(field, `"`)
}

// suggestedTunnelCommand renders a copy-pasteable tunnel command for
// the forwardable ports. sshd is excluded: `nakama aws ssh` is the
// right tool for port 22.
func suggestedTunnelCommand(name string, ports []ListeningPort) string {
	var args []string
	for _, p := range ports {
		if p.Port == 22 {
			continue
		}
		args = append(args, "-L "+p.Spec)
	}
	if len(args) == 0 {
		return ""
	}
	return fmt.Sprintf("nakama aws tunnel %s %s", name, strings.Join(args, " "))
}
