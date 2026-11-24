package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/service"
)

// TestFormatTunnelStatusLabelsBrowserForwards: the forwards of `code`
// and `kirocrew` are named in `tunnel status`, and the idle state shows
// as such (Issue #118).
func TestFormatTunnelStatusLabelsBrowserForwards(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	st := &service.TunnelStatus{Name: "dev01", Forwards: []service.ForwardStatus{
		{Spec: "8080:8080", Purpose: service.ForwardPurposeCodeServer, State: service.ForwardStateActive, Since: now.Add(-time.Minute)},
		{Spec: "5476:5476", Purpose: service.ForwardPurposeKiroCrew, State: service.ForwardStateIdle, Since: now.Add(-10 * time.Minute),
			Detail: "no session; the next local connection starts one"},
		{Spec: "5432:db.internal:5432", State: service.ForwardStateReconnecting, Detail: "attempt 2 (next in 2s): connection lost"},
	}}
	out := formatTunnelStatus(st, now)
	for _, want := range []string{
		"8080:8080 (code-server)",
		"5476:5476 (kirocrew)",
		"idle",
		"no session; the next local connection starts one",
		"5432:db.internal:5432 ",
		"attempt 2 (next in 2s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status table lacks %q:\n%s", want, out)
		}
	}
	over := formatTunnelOverview([]service.TunnelOverview{{Name: "dev01", Status: st}}, now)
	if !strings.Contains(over, "dev01") || !strings.Contains(over, "8080:8080 (code-server)") {
		t.Errorf("overview:\n%s", over)
	}
}

// TestFormatRouterSection: `tunnel status` shows the router and the
// addresses of the forwards it serves (Issue #124).
func TestFormatRouterSection(t *testing.T) {
	browser := &service.TunnelStatus{Name: "dev01", Forwards: []service.ForwardStatus{
		{Spec: "41001:8080", Purpose: service.ForwardPurposeCodeServer, URL: "http://code-server.dev01.nakama.localhost:18888/"},
		{Spec: "5432:5432"},
	}}
	plain := &service.TunnelStatus{Name: "dev02", Forwards: []service.ForwardStatus{{Spec: "5432:5432"}}}
	router := &service.RouterStatus{PID: 77, Port: 18888}

	if got := formatRouterSection(nil, []*service.TunnelStatus{plain}, false); got != "" {
		t.Errorf("plain -L forwards without a router need no section: %q", got)
	}
	notRunning := formatRouterSection(nil, []*service.TunnelStatus{browser}, false)
	if !strings.Contains(notRunning, "router: not running") || !strings.Contains(notRunning, "nakama aws code <name>") {
		t.Errorf("not running:\n%s", notRunning)
	}
	one := formatRouterSection(router, []*service.TunnelStatus{browser}, false)
	for _, want := range []string{"router: running on localhost:18888 (pid 77)", "open in a browser:", "  http://code-server.dev01.nakama.localhost:18888/"} {
		if !strings.Contains(one, want) {
			t.Errorf("one instance lacks %q:\n%s", want, one)
		}
	}
	all := formatRouterSection(router, []*service.TunnelStatus{browser, plain}, true)
	if !strings.Contains(all, "dev01") || !strings.Contains(all, "http://code-server.dev01.nakama.localhost:18888/") || strings.Contains(all, "dev02") {
		t.Errorf("overview:\n%s", all)
	}
}
