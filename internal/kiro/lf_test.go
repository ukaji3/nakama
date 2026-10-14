package kiro

import (
	"strings"
	"testing"
)

// TestInstallScriptUsesLF: see internal/userdata/lf_test.go (Issue #89).
func TestInstallScriptUsesLF(t *testing.T) {
	if i := strings.IndexByte(InstallScript(), '\r'); i >= 0 {
		t.Errorf("install.sh contains a carriage return at byte %d; check it out with LF (see .gitattributes)", i)
	}
}
