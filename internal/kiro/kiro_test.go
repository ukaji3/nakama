package kiro

import (
	"os/exec"
	"strings"
	"testing"
)

// commandLineIndex returns the position of the first *command* line
// containing substr, skipping comments. The script explains the
// AppArmor constraint in prose that quotes the very commands being
// ordered, so a plain strings.Index would match the comment instead.
func commandLineIndex(script, substr string) int {
	offset := 0
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") && strings.Contains(line, substr) {
			return offset
		}
		offset += len(line) + 1
	}
	return -1
}

// TestInstallScriptStepOrder pins the sequence the install has to
// follow. The chmod in particular must come between the Crew install
// (which creates ~/.kiro) and the service install (which refuses to
// place its AppArmor profile while that tree is group-writable).
func TestInstallScriptStepOrder(t *testing.T) {
	s := InstallScript()
	steps := []string{
		`curl -fsSL https://cli.kiro.dev/install | bash`,
		`"$HOME/.local/bin/kiro-cli" crew -y install`,
		`chmod -R go-w "$HOME/.kiro"`,
		`kirocrew service install`,
	}
	prev := -1
	for _, step := range steps {
		at := commandLineIndex(s, step)
		if at < 0 {
			t.Fatalf("the install script is missing the step %q:\n%s", step, s)
		}
		if at <= prev {
			t.Errorf("step %q appears out of order (at %d, previous step at %d)", step, at, prev)
		}
		prev = at
	}
}

// TestInstallScriptTightensKiroBeforeServiceInstall states the AppArmor
// constraint as its own test, so a future edit that drops the chmod
// fails with the reason attached.
func TestInstallScriptTightensKiroBeforeServiceInstall(t *testing.T) {
	s := InstallScript()
	chmodAt := commandLineIndex(s, `chmod -R go-w "$HOME/.kiro"`)
	serviceAt := commandLineIndex(s, "kirocrew service install")
	if chmodAt < 0 {
		t.Fatal("~/.kiro must be made non-group-writable: `kirocrew service install` " +
			"refuses to install its AppArmor profile otherwise")
	}
	if serviceAt < 0 {
		t.Fatal("the gateway service install step is missing")
	}
	if chmodAt > serviceAt {
		t.Error("the chmod must precede `kirocrew service install`, not follow it")
	}
}

// TestInstallScriptRunsAsUbuntu pins that the steps drop privileges:
// both call sites execute as root, and the installer targets
// ~/.local/bin of the login user.
func TestInstallScriptRunsAsUbuntu(t *testing.T) {
	s := InstallScript()
	if !strings.Contains(s, "sudo -u ubuntu -H bash -lc") {
		t.Error("the install steps must run as the ubuntu user via `sudo -u ubuntu -H bash -lc`")
	}
	if !strings.Contains(s, `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Error("the script must put ~/.local/bin on PATH: the official installer does not")
	}
}

// TestInstallScriptAddsPathIdempotently pins that re-running the script
// (e.g. `setup rerun --preset kiro`) does not append the PATH line
// twice.
func TestInstallScriptAddsPathIdempotently(t *testing.T) {
	s := InstallScript()
	if !strings.Contains(s, "grep -qF") || !strings.Contains(s, "/home/ubuntu/.bashrc") {
		t.Error("the PATH line must be added to ~/.bashrc only when absent")
	}
}

// TestInstallScriptIsBestEffort pins that a failure is reported instead
// of aborting the caller: the user-data and the provisioning script
// both continue so the instance stays usable.
func TestInstallScriptIsBestEffort(t *testing.T) {
	s := InstallScript()
	if !strings.Contains(s, "if sudo -u ubuntu") {
		t.Error("the install must run inside an `if` so its failure is captured, not propagated")
	}
	if !strings.Contains(s, "WARNING") {
		t.Error("a failed install must emit a warning")
	}
	if !strings.Contains(s, "setup rerun") {
		t.Error("the warning must point at the retry path (`nakama aws setup rerun`)")
	}
}

// TestInstallScriptFragmentContract pins the properties that let the
// same text be embedded in two different scripts: no shebang of its
// own, and no `set` that would change the including script's options.
func TestInstallScriptFragmentContract(t *testing.T) {
	s := InstallScript()
	if strings.HasPrefix(s, "#!") {
		t.Error("the fragment must not carry a shebang: it is embedded in other scripts")
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "set -") && !strings.Contains(line, "pipefail") {
			t.Errorf("the fragment must not change the including script's shell options: %q", line)
		}
	}
	// The one `set` allowed is inside the quoted sudo payload, which is
	// a separate shell.
	if !strings.Contains(s, "set -euo pipefail") {
		t.Error("the inner shell must stop at the first failing step (set -euo pipefail)")
	}
}

// TestInstallScriptUsesPipefailForTheDownload pins the reason pipefail
// is set: a failed download otherwise feeds an empty script to bash,
// which exits 0 and silently installs nothing.
func TestInstallScriptUsesPipefailForTheDownload(t *testing.T) {
	s := InstallScript()
	pipefailAt := strings.Index(s, "set -euo pipefail")
	curlAt := strings.Index(s, "curl -fsSL https://cli.kiro.dev/install | bash")
	if pipefailAt < 0 || curlAt < 0 || pipefailAt > curlAt {
		t.Error("pipefail must be in effect before the piped installer download")
	}
}

// TestInstallScriptIsValidBash runs the fragment through `bash -n`
// wrapped the way its callers wrap it, so a syntax error cannot reach
// a cloud-init boot (where it would be discovered only by a failing
// instance).
func TestInstallScriptIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	wrapped := "#!/bin/bash\nset -euxo pipefail\nTARGET_USER=ubuntu\n" + InstallScript()
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(wrapped)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bash -n rejected the fragment: %v\n%s", err, out)
	}
}
