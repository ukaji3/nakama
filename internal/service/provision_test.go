package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/kiro"
)

func TestValidateProvisionSpec(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "setup.sh")
	if err := os.WriteFile(script, []byte("echo hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ValidateProvisionSpec(ProvisionSpec{}); err != nil {
		t.Errorf("the zero spec must be valid: %v", err)
	}
	if err := ValidateProvisionSpec(ProvisionSpec{Preset: "kiro"}); err != nil {
		t.Errorf("the bundled preset must be valid: %v", err)
	}
	err := ValidateProvisionSpec(ProvisionSpec{Preset: "nope"})
	if err == nil || !strings.Contains(err.Error(), "kiro") {
		t.Errorf("err = %v, want an unknown-preset error listing the available ones", err)
	}
	if err := ValidateProvisionSpec(ProvisionSpec{SetupScript: script}); err != nil {
		t.Errorf("an existing local script must be valid: %v", err)
	}
	if err := ValidateProvisionSpec(ProvisionSpec{SetupScript: filepath.Join(dir, "missing.sh")}); err == nil {
		t.Error("a missing local script must be rejected before create")
	}
	if err := ValidateProvisionSpec(ProvisionSpec{SetupScript: "https://example.com/s.sh"}); err != nil {
		t.Errorf("an https script URL must be valid: %v", err)
	}
	// Plain http would let the network rewrite the script that then runs
	// with the instance's privileges.
	if err := ValidateProvisionSpec(ProvisionSpec{SetupScript: "http://example.com/s.sh"}); err == nil {
		t.Error("a plain-http setup URL must be rejected")
	}
	if err := ValidateProvisionSpec(ProvisionSpec{Dotfiles: "http://example.com/df.git"}); err == nil {
		t.Error("a plain-http dotfiles URL must be rejected")
	}
	if err := ValidateProvisionSpec(ProvisionSpec{Dotfiles: "git@github.com:u/dotfiles.git"}); err != nil {
		t.Errorf("an ssh-style git URL must be accepted: %v", err)
	}
}

func TestProvisionStepOrder(t *testing.T) {
	got := provisionStepNames(ProvisionSpec{SetupScript: "/tmp/s.sh", Dotfiles: "https://x/df.git", Preset: "kiro"})
	want := []string{"preset:kiro", "dotfiles", "setup"}
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("steps = %v, want %v (preset first so later steps can build on it)", got, want)
		}
	}
	if len(provisionStepNames(ProvisionSpec{})) != 0 {
		t.Error("the zero spec must request no steps")
	}
}

func TestBuildProvisionScriptInlinesLocalScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "setup.sh")
	body := "echo \"hello $USER\"\napt-get install -y jq\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := buildProvisionScript(ProvisionSpec{SetupScript: script})
	if err != nil {
		t.Fatalf("buildProvisionScript: %v", err)
	}
	if !strings.Contains(got, body) {
		t.Errorf("the local script body must be inlined verbatim:\n%s", got)
	}
	// A quoted heredoc keeps the outer shell from expanding $USER.
	if !strings.Contains(got, "<<'NAKAMA_SETUP_EOF'") {
		t.Errorf("the body must be delivered through a quoted heredoc:\n%s", got)
	}
	if !strings.Contains(got, "sudo -u ubuntu -H") {
		t.Errorf("the script must run as ubuntu:\n%s", got)
	}
	if !strings.Contains(got, ProvisionLogPath) {
		t.Errorf("output must be teed to the provisioning log:\n%s", got)
	}
}

func TestBuildProvisionScriptRejectsMarkerCollision(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "evil.sh")
	if err := os.WriteFile(script, []byte("NAKAMA_SETUP_EOF\nrm -rf /\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildProvisionScript(ProvisionSpec{SetupScript: script}); err == nil {
		t.Error("a script containing the heredoc marker must be rejected (it would break out of the heredoc)")
	}
}

func TestBuildProvisionScriptFetchesRemoteScript(t *testing.T) {
	got, err := buildProvisionScript(ProvisionSpec{SetupScript: "https://example.com/s.sh"})
	if err != nil {
		t.Fatalf("buildProvisionScript: %v", err)
	}
	if !strings.Contains(got, `curl -fsSL "https://example.com/s.sh"`) {
		t.Errorf("an https script must be fetched with curl:\n%s", got)
	}
}

func TestBuildProvisionScriptDotfilesConventions(t *testing.T) {
	got, err := buildProvisionScript(ProvisionSpec{Dotfiles: "https://github.com/u/dotfiles.git"})
	if err != nil {
		t.Fatalf("buildProvisionScript: %v", err)
	}
	for _, want := range []string{
		"git clone --depth 1",
		"git -C \"$dir\" pull --ff-only", // idempotent re-run
		"install.sh setup.sh bootstrap.sh",
		"ln -sfn",
		".git|.github",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dotfiles step lacks %q:\n%s", want, got)
		}
	}
}

func TestBuildProvisionScriptPresetKiro(t *testing.T) {
	got, err := buildProvisionScript(ProvisionSpec{Preset: "kiro"})
	if err != nil {
		t.Fatalf("buildProvisionScript: %v", err)
	}
	for _, want := range []string{
		"cli.kiro.dev/install",
		"crew -y -- --version",
		"kirocrew service install",
		"sudo -u ubuntu -H",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the kiro preset lacks %q:\n%s", want, got)
		}
	}
}

// TestPresetKiroSharesTheUserDataScript pins that the preset and the
// cloud-init user-data run the same steps: the preset is the re-run
// path for what create does by default, so the two must not drift
// (Issue #68).
func TestPresetKiroSharesTheUserDataScript(t *testing.T) {
	if got := provisionPresets()["kiro"]; got != kiro.InstallScript() {
		t.Error("the kiro preset must be exactly internal/kiro's install script, " +
			"so `setup rerun --preset kiro` reproduces what the user-data did")
	}
}

func TestProvisionScriptStepsAreIsolated(t *testing.T) {
	got, err := buildProvisionScript(ProvisionSpec{Preset: "kiro", Dotfiles: "https://x/df.git"})
	if err != nil {
		t.Fatalf("buildProvisionScript: %v", err)
	}
	// Each step runs in a subshell and reports its own result, so one
	// failing step neither aborts nor hides the others.
	if strings.Count(got, "nakama_step ") != 2 || strings.Count(got, "nakama_done ") != 2 {
		t.Errorf("each step needs its own begin/result markers:\n%s", got)
	}
	if !strings.Contains(got, "NAKAMA_STEP_FAILED") {
		t.Errorf("the script must emit a machine-readable failure marker:\n%s", got)
	}
}

func TestFailedProvisionStep(t *testing.T) {
	out := strings.Join([]string{
		`NAKAMA_STEP_BEGIN "preset:kiro"`,
		`NAKAMA_STEP_OK "preset:kiro"`,
		`NAKAMA_STEP_BEGIN "setup"`,
		`NAKAMA_STEP_FAILED "setup" (exit 127)`,
	}, "\n")
	step, ok := failedProvisionStep(out)
	if !ok || step != "setup" {
		t.Errorf("failedProvisionStep = (%q, %v), want (setup, true)", step, ok)
	}
	if _, ok := failedProvisionStep("NAKAMA_STEP_OK \"setup\""); ok {
		t.Error("a successful run must report no failed step")
	}
}

func TestProvisionPresetNames(t *testing.T) {
	names := ProvisionPresetNames()
	if len(names) == 0 || names[0] != "kiro" {
		t.Errorf("ProvisionPresetNames() = %v, want the kiro preset", names)
	}
}

func TestProvisionZeroSpecDoesNothing(t *testing.T) {
	// No AWS client is touched: the zero spec short-circuits.
	res, err := NewService(nil).Provision(context.Background(), "dev01", ProvisionSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Failed || len(res.Steps) != 0 {
		t.Errorf("result = %+v, want an empty, successful result", res)
	}
}

func TestProvisionRejectsBadNameAndPreset(t *testing.T) {
	svc := NewService(nil)
	if _, err := svc.Provision(context.Background(), "Bad Name", ProvisionSpec{Preset: "kiro"}); err == nil {
		t.Error("an invalid instance name must be rejected")
	}
	if _, err := svc.Provision(context.Background(), "dev01", ProvisionSpec{Preset: "nope"}); err == nil {
		t.Error("an unknown preset must be rejected")
	}
}

func TestBuildProvisionScriptStartsWithBashShebang(t *testing.T) {
	// aws:runShellScript executes the commands as a FILE via `sh`
	// (amazon-ssm-agent runshellscript.go), so without this shebang the
	// script runs under dash, where the process substitution used for
	// logging is a syntax error (Issue #61).
	got, err := buildProvisionScript(ProvisionSpec{Preset: "kiro"})
	if err != nil {
		t.Fatalf("buildProvisionScript: %v", err)
	}
	if !strings.HasPrefix(got, "#!/bin/bash\n") {
		t.Errorf("the provision script must start with a bash shebang:\n%.80s", got)
	}
}

func TestSetupBodyMakesTempReadableForUbuntu(t *testing.T) {
	// Both delivery paths run the script as ubuntu via sudo; a
	// root-owned 0600/0711 temp file would be unreadable (Issue #61).
	remote, err := setupBody("https://example.com/s.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	local := filepath.Join(dir, "s.sh")
	if err := os.WriteFile(local, []byte("echo hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localBody, err := setupBody(local)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"https": remote, "local": localBody} {
		if !strings.Contains(body, `chown ubuntu:ubuntu "$tmp"`) {
			t.Errorf("%s branch must chown the temp script to ubuntu:\n%s", name, body)
		}
		if strings.Contains(body, "chmod +x") {
			t.Errorf("%s branch must use an explicit mode, not chmod +x (0711 keeps the file unreadable):\n%s", name, body)
		}
	}
}
