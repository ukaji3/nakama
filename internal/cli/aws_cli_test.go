package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultShell(t *testing.T) {
	tests := []struct {
		name      string
		parentExe string
		comspec   string
		want      string
	}{
		{"powershell parent", `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, `C:\Windows\system32\cmd.exe`, `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`},
		{"pwsh parent", `C:\Program Files\PowerShell\7\pwsh.exe`, `C:\Windows\system32\cmd.exe`, `C:\Program Files\PowerShell\7\pwsh.exe`},
		{"cmd parent", `C:\Windows\system32\cmd.exe`, `C:\Windows\system32\cmd.exe`, `C:\Windows\system32\cmd.exe`},
		{"case-insensitive", `C:\WINDOWS\SYSTEM32\CMD.EXE`, ``, `C:\WINDOWS\SYSTEM32\CMD.EXE`},
		{"unknown parent falls back to ComSpec", `C:\Program Files\WindowsTerminal\wt.exe`, `C:\Windows\system32\cmd.exe`, `C:\Windows\system32\cmd.exe`},
		{"unknown parent, no ComSpec", `C:\some\launcher.exe`, ``, `cmd.exe`},
		{"empty parent", ``, `C:\Windows\system32\cmd.exe`, `C:\Windows\system32\cmd.exe`},
		{"posix separators tolerated", `/usr/bin/pwsh.exe`, ``, `/usr/bin/pwsh.exe`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultShell(tt.parentExe, tt.comspec); got != tt.want {
				t.Errorf("defaultShell(%q, %q) = %q, want %q", tt.parentExe, tt.comspec, got, tt.want)
			}
		})
	}
}

func TestCLIShellRejectsNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("gate applies to non-Windows systems only")
	}
	cmd := newAWSCLICommand()
	cmd.SetArgs([]string{"shell"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("shell must be rejected on non-Windows systems")
	}
	if !strings.Contains(err.Error(), "Windows-only") {
		t.Errorf("error must state the Windows-only nature, got %q", err.Error())
	}
}

func TestCLIShellRejectsNesting(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the nesting check sits behind the Windows gate")
	}
	t.Setenv("NAKAMA_AWS_CLI_SHELL", "1")
	cmd := newAWSCLICommand()
	cmd.SetArgs([]string{"shell"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already inside") {
		t.Errorf("nested invocation must be rejected, got %v", err)
	}
}
