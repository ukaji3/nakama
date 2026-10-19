package cli

import (
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

func TestUpdateSummary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   service.UpdateResult
		want []string
		not  []string
	}{
		{
			name: "everything updated",
			in: service.UpdateResult{BackupLabel: "preupdate-1", CodeServerVersion: "4.200.0",
				AWSCLIVersion: "2.38.1", KiroCrew: service.KiroCrewUpdated, Rebooted: true},
			want: []string{"updated: dev01 (backup preupdate-1, code-server 4.200.0, AWS CLI 2.38.1, Kiro Crew updated and restarted) [rebooted]"},
		},
		{
			name: "older instance without the official CLI or Kiro",
			in:   service.UpdateResult{BackupLabel: "preupdate-1", CodeServerVersion: "4.200.0"},
			want: []string{"updated: dev01 (backup preupdate-1, code-server 4.200.0)"},
			not:  []string{"AWS CLI", "Kiro"},
		},
		{
			name: "bare with a failed Kiro Crew update",
			in:   service.UpdateResult{BackupLabel: "preupdate-1", AWSCLIVersion: "2.38.1", KiroCrew: service.KiroCrewFailed, RebootPending: true},
			want: []string{"AWS CLI 2.38.1", "Kiro Crew update FAILED", "[kernel update applies on next start]"},
			not:  []string{"code-server"},
		},
	}
	for _, c := range cases {
		got := updateSummary("dev01", &c.in)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q must not mention %q", c.name, got, n)
			}
		}
	}
}
