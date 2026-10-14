package service

import (
	"strings"
	"testing"
)

// TestEmbeddedScriptsUseLF: see internal/userdata/lf_test.go (Issue
// #89). The NAT setup is the NAT instance's user-data and, inside
// natRepairScript, what init runs to repair it.
func TestEmbeddedScriptsUseLF(t *testing.T) {
	for name, s := range map[string]string{
		"nat_setup.sh":    natSetupScript,
		"natRepairScript": natRepairScript(),
		"natCheckScript":  natCheckScript,
	} {
		if i := strings.IndexByte(s, '\r'); i >= 0 {
			t.Errorf("%s contains a carriage return at byte %d; check it out with LF (see .gitattributes)", name, i)
		}
	}
}
