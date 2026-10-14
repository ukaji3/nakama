package userdata

import (
	"strings"
	"testing"
)

// TestEmbeddedTemplatesUseLF fails on a checkout that turned the
// templates into CRLF: bash rejects them on the instance ("/bin/bash^M:
// bad interpreter"), and Git for Windows does that by default unless
// .gitattributes pins eol=lf (Issue #89). It runs on the Windows CI job,
// which checks the repository out on Windows.
func TestEmbeddedTemplatesUseLF(t *testing.T) {
	for name, s := range map[string]string{
		"code-server.tmpl.sh": codeServerTemplate,
		"bare.tmpl.sh":        bareTemplate,
	} {
		if i := strings.IndexByte(s, '\r'); i >= 0 {
			t.Errorf("%s contains a carriage return at byte %d; check it out with LF (see .gitattributes)", name, i)
		}
	}
}
