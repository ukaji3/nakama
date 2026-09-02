package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseListeningPorts(t *testing.T) {
	// Real-world shapes: ss -ltnH columns, IPv6, wildcard, loopback,
	// duplicate port on v4+v6, and a netstat fallback line ("-").
	stdout := strings.Join([]string{
		`NAKAMA_PORT 0.0.0.0:8080 users:(("node",pid=1234,fd=20))`,
		`NAKAMA_PORT 127.0.0.1:5432 users:(("postgres",pid=999,fd=6))`,
		`NAKAMA_PORT [::]:22 users:(("sshd",pid=800,fd=3))`,
		`NAKAMA_PORT *:3000 -`,
		`NAKAMA_PORT 0.0.0.0:8080 -`,
		`garbage line`,
		`NAKAMA_PORT not-an-addr users:(("x",pid=1,fd=1))`,
	}, "\n")

	ports := parseListeningPorts(stdout)
	if len(ports) != 4 {
		t.Fatalf("got %d ports, want 4 (deduplicated by port): %+v", len(ports), ports)
	}
	if ports[0].Port != 22 || ports[1].Port != 3000 || ports[2].Port != 5432 || ports[3].Port != 8080 {
		t.Errorf("ports are not sorted ascending: %+v", ports)
	}
	if ports[3].Process != "node" {
		t.Errorf("port 8080 process = %q, want node (the entry with a process must win over the duplicate)", ports[3].Process)
	}
	if ports[3].Spec != "8080:8080" {
		t.Errorf("spec = %q, want 8080:8080", ports[3].Spec)
	}
	if ports[3].Label != "code-server" {
		t.Errorf("port 8080 label = %q, want the code-server label", ports[3].Label)
	}
	if ports[0].Label == "" || !strings.Contains(ports[0].Label, "ssh") {
		t.Errorf("port 22 label = %q, want the ssh pointer", ports[0].Label)
	}
	// A loopback-only socket must still be listed: forwarding is exactly
	// how a developer reaches it.
	if ports[2].Address != "127.0.0.1:5432" || ports[2].Process != "postgres" {
		t.Errorf("loopback socket = %+v, want it preserved", ports[2])
	}
}

func TestParseListeningPortsEmpty(t *testing.T) {
	if got := parseListeningPorts(""); len(got) != 0 {
		t.Errorf("got %+v, want no ports", got)
	}
}

func TestPortFromSocketAddr(t *testing.T) {
	tests := []struct {
		addr string
		want int
		ok   bool
	}{
		{"0.0.0.0:8080", 8080, true},
		{"[::]:22", 22, true},
		{"*:3000", 3000, true},
		{"127.0.0.1:5432", 5432, true},
		{"noport", 0, false},
		{"1.2.3.4:", 0, false},
		{"1.2.3.4:99999", 0, false},
		{"1.2.3.4:abc", 0, false},
	}
	for _, tt := range tests {
		got, ok := portFromSocketAddr(tt.addr)
		if got != tt.want || ok != tt.ok {
			t.Errorf("portFromSocketAddr(%q) = (%d, %v), want (%d, %v)", tt.addr, got, ok, tt.want, tt.ok)
		}
	}
}

func TestSuggestedTunnelCommandExcludesSSH(t *testing.T) {
	ports := []ListeningPort{
		{Port: 22, Spec: "22:22"},
		{Port: 8080, Spec: "8080:8080"},
		{Port: 5432, Spec: "5432:5432"},
	}
	got := suggestedTunnelCommand("dev01", ports)
	want := "nakama aws tunnel dev01 -L 8080:8080 -L 5432:5432"
	if got != want {
		t.Errorf("command = %q, want %q (port 22 belongs to `nakama aws ssh`)", got, want)
	}
	if suggestedTunnelCommand("dev01", []ListeningPort{{Port: 22, Spec: "22:22"}}) != "" {
		t.Error("with only sshd listening there is nothing to suggest")
	}
}

func TestListeningPortsScriptHasNetstatFallback(t *testing.T) {
	script := listeningPortsScript()
	for _, want := range []string{"command -v ss", "ss -ltnH", "netstat -ltn", "NAKAMA_PORT"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
}

func TestSuggestTunnelRejectsBadName(t *testing.T) {
	svc := NewService(nil)
	if _, err := svc.SuggestTunnel(context.Background(), "Bad Name"); err == nil {
		t.Error("an invalid instance name must be rejected before any AWS call")
	}
}

func TestListTunnelStatusAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	old := tunnelStateDirOverride
	tunnelStateDirOverride = dir
	t.Cleanup(func() { tunnelStateDirOverride = old })

	// Two recorded tunnels: one live, one whose daemon is gone.
	for _, name := range []string{"dev01", "dev02"} {
		if err := writeTunnelState(&TunnelState{
			Name: name, PID: 1, ControlPort: 1, Token: "t-" + name,
			Forwards: []string{"8080:8080"}, StartedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	svc := NewService(nil)
	svc.tunnel = &tunnelHooks{
		query: func(_ context.Context, st *TunnelState, _ time.Duration) (*TunnelStatus, error) {
			if st.Name == "dev02" {
				return nil, errors.New("control channel unreachable")
			}
			return &TunnelStatus{
				Name:     st.Name,
				Forwards: []ForwardStatus{{Spec: "8080:8080", State: ForwardStateActive, Since: time.Now()}},
			}, nil
		},
	}

	items, err := svc.ListTunnelStatus(context.Background())
	if err != nil {
		t.Fatalf("ListTunnelStatus: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}
	if items[0].Name != "dev01" || items[0].Status == nil || items[0].Stale {
		t.Errorf("dev01 = %+v, want a live status", items[0])
	}
	if items[1].Name != "dev02" || items[1].Status != nil || !items[1].Stale {
		t.Errorf("dev02 = %+v, want stale", items[1])
	}
	// The stale probe must clean the leftover state file up.
	if names, err := listTunnelStateNames(); err != nil || len(names) != 1 || names[0] != "dev01" {
		t.Errorf("state files after the probe = %v (err %v), want only dev01", names, err)
	}
}

func TestListTunnelStatusEmpty(t *testing.T) {
	dir := t.TempDir()
	old := tunnelStateDirOverride
	tunnelStateDirOverride = dir
	t.Cleanup(func() { tunnelStateDirOverride = old })

	items, err := NewService(nil).ListTunnelStatus(context.Background())
	if err != nil {
		t.Fatalf("ListTunnelStatus: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("got %+v, want no items", items)
	}
}
