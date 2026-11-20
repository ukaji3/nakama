package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/service"
)

// TestFormatTunnelStatusLabelsBrowserForwards: the forwards of `open`
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
