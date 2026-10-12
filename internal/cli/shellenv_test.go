package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/cliproxy"
)

func TestPrependWindowsPath(t *testing.T) {
	const dir = `C:\Users\you\Tools`
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "prepended, the variable's own spelling kept",
			env:  []string{"A=1", `Path=C:\Windows\system32;C:\Windows`, "B=2"},
			want: []string{"A=1", `Path=C:\Users\you\Tools;C:\Windows\system32;C:\Windows`, "B=2"},
		},
		{
			name: "upper-case name",
			env:  []string{`PATH=C:\bin`},
			want: []string{`PATH=C:\Users\you\Tools;C:\bin`},
		},
		{
			name: "existing occurrences dropped: case, trailing separator, quotes, spaces",
			env:  []string{`Path=c:\users\YOU\tools;C:\a;C:\Users\you\Tools\;"C:\Users\you\Tools";C:\b; C:\Users\you\Tools `},
			want: []string{`Path=C:\Users\you\Tools;C:\a;C:\b`},
		},
		{
			name: "other entries kept as is, including empty ones and similar prefixes",
			env:  []string{`Path=C:\Users\you\Tools2;;C:\Users\you`},
			want: []string{`Path=C:\Users\you\Tools;C:\Users\you\Tools2;;C:\Users\you`},
		},
		{
			name: "no PATH at all",
			env:  []string{"A=1"},
			want: []string{"A=1", `Path=C:\Users\you\Tools`},
		},
		{
			name: "empty PATH",
			env:  []string{"Path="},
			want: []string{`Path=C:\Users\you\Tools`},
		},
		{
			name: "per-drive directory entries are not PATH",
			env:  []string{`=C:=C:\work`, `Path=C:\bin`},
			want: []string{`=C:=C:\work`, `Path=C:\Users\you\Tools;C:\bin`},
		},
		{
			name: "only the first PATH is changed",
			env:  []string{`Path=C:\a`, `PATH=C:\b`},
			want: []string{`Path=C:\Users\you\Tools;C:\a`, `PATH=C:\b`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := slices.Clone(tt.env)
			got := prependWindowsPath(tt.env, dir)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
			if !slices.Equal(tt.env, in) {
				t.Errorf("the input was modified: %q", tt.env)
			}
		})
	}
}

func TestPrependWindowsPathQuotesSemicolons(t *testing.T) {
	got := prependWindowsPath([]string{`Path=C:\bin;"D:\a;b"`}, `D:\a;b`)
	want := []string{`Path="D:\a;b";C:\bin;"D:\a;b"`}
	// The quoted old entry is split at its ';' by the plain split, so
	// it is not recognized; keeping it is harmless (a duplicate later
	// in PATH), and what matters is that the new entry is quoted.
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if !strings.HasPrefix(got[0], `Path="D:\a;b";`) {
		t.Errorf("a folder with ';' must be quoted, got %q", got[0])
	}
}

func TestSameWindowsDir(t *testing.T) {
	tests := []struct {
		p, dir string
		want   bool
	}{
		{`C:\Tools`, `c:\tools`, true},
		{`C:\Tools\`, `C:\Tools`, true},
		{`C:\Tools/`, `C:\Tools`, true},
		{`"C:\Tools"`, `C:\Tools`, true},
		{`C:\`, `c:\`, true},
		{`C:\`, `C:`, false},
		{`C:\Tools2`, `C:\Tools`, false},
		{``, `C:\Tools`, false},
		{``, ``, false},
	}
	for _, tt := range tests {
		if got := sameWindowsDir(tt.p, tt.dir); got != tt.want {
			t.Errorf("sameWindowsDir(%q, %q) = %v, want %v", tt.p, tt.dir, got, tt.want)
		}
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func TestCLIShellEnv(t *testing.T) {
	det := &cliproxy.Detection{ProxyURL: "http://proxy.example:8080", BundlePath: `C:\Users\you\.config\nakama\aws-ca-bundle.pem`}
	base := []string{`Path=C:\Windows`, "HTTPS_PROXY=http://old:1"}

	env := cliShellEnv(base, det, `C:\Tools`)
	for key, want := range map[string]string{
		"Path":                 `C:\Tools;C:\Windows`,
		"HTTP_PROXY":           det.ProxyURL,
		"HTTPS_PROXY":          det.ProxyURL,
		"AWS_CA_BUNDLE":        det.BundlePath,
		"NAKAMA_AWS_CLI_SHELL": "1",
	} {
		if got, _ := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	env = cliShellEnv(base, det, "")
	if got, _ := envValue(env, "Path"); got != `C:\Windows` {
		t.Errorf("without nakama's folder PATH must be unchanged, got %q", got)
	}
}

func TestIsDoubleClick(t *testing.T) {
	const explorer = `C:\Windows\explorer.exe`
	tests := []struct {
		name   string
		args   []string
		parent string
		want   bool
	}{
		{"double-clicked", []string{`C:\Tools\nakama.exe`}, explorer, true},
		{"parent name case-insensitive", []string{"nakama.exe"}, `C:\WINDOWS\EXPLORER.EXE`, true},
		{"from Explorer with arguments (a shortcut)", []string{"nakama.exe", "aws", "list"}, explorer, false},
		{"from PowerShell", []string{"nakama.exe"}, `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, false},
		{"from cmd", []string{"nakama.exe"}, `C:\Windows\system32\cmd.exe`, false},
		{"parent unknown", []string{"nakama.exe"}, "", false},
		{"similar name", []string{"nakama.exe"}, `C:\tools\myexplorer.exe`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDoubleClick(tt.args, tt.parent); got != tt.want {
				t.Errorf("isDoubleClick(%q, %q) = %v, want %v", tt.args, tt.parent, got, tt.want)
			}
		})
	}
}

func TestLauncherShell(t *testing.T) {
	const ps = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	if path, args := launcherShell(ps, `C:\Windows\system32\cmd.exe`); path != ps || !slices.Equal(args, []string{"-NoLogo"}) {
		t.Errorf("with Windows PowerShell: %q %q", path, args)
	}
	if path, args := launcherShell("", `C:\Windows\system32\cmd.exe`); path != `C:\Windows\system32\cmd.exe` || args != nil {
		t.Errorf("without Windows PowerShell, ComSpec: %q %q", path, args)
	}
	if path, args := launcherShell("", ""); path != "cmd.exe" || args != nil {
		t.Errorf("without either: %q %q", path, args)
	}
}
