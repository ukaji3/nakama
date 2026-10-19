package userdata

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/kiro"
)

func TestRenderBare(t *testing.T) {
	got, err := RenderBare(BareParams{InstanceName: "dev01", Region: "ap-northeast-1"})
	if err != nil {
		t.Fatalf("RenderBare: %v", err)
	}
	for _, want := range []string{
		`INSTANCE_NAME="dev01"`,
		`REGION="ap-northeast-1"`,
		"amazon-ssm-agent", // connect / tunnel / ssh depend on the agent
		"systemctl enable --now ssh",
		"git curl unzip unar jq build-essential gnupg",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered bare user data lacks %q", want)
		}
	}
	// A bare instance has no code-server and no password parameter.
	for _, absent := range []string{"code-server", "PASSWORD_PARAM", "get-parameter"} {
		if strings.Contains(got, absent) {
			t.Errorf("rendered bare user data must not mention %q", absent)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("template placeholders were left unrendered:\n%s", got)
	}
}

// TestRenderBareInstallsKiroByDefault pins that a bare box also gets
// Kiro by default (Issue #68): --bare drops the browser IDE, not the
// development tooling.
func TestRenderBareInstallsKiroByDefault(t *testing.T) {
	got, err := RenderBare(BareParams{InstanceName: "box01", Region: "ap-northeast-1", InstallKiro: true})
	if err != nil {
		t.Fatalf("RenderBare: %v", err)
	}
	if !strings.Contains(got, kiro.InstallScript()) {
		t.Error("the bare user-data must embed internal/kiro's install script verbatim")
	}
	if !strings.Contains(got, "unar") {
		t.Errorf("the bare base packages must include unar:\n%s", got)
	}
	// The trailing chown must not run after the Kiro steps: it would
	// not reset the modes, but keeping the order explicit documents
	// that ~/.kiro is tightened last.
	chownAt := strings.LastIndex(got, "chown -R")
	kiroAt := strings.Index(got, "cli.kiro.dev/install")
	if chownAt < 0 || kiroAt < 0 || chownAt > kiroAt {
		t.Error("the home-directory chown must precede the Kiro steps")
	}
}

func TestRenderBareNoKiro(t *testing.T) {
	got, err := RenderBare(BareParams{InstanceName: "box01", Region: "ap-northeast-1", InstallKiro: false})
	if err != nil {
		t.Fatalf("RenderBare: %v", err)
	}
	if strings.Contains(strings.ToLower(got), "kiro") {
		t.Errorf("--no-kiro must render no Kiro content at all:\n%s", got)
	}
}

func TestRenderBareIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	for _, installKiro := range []bool{true, false} {
		got, err := RenderBare(BareParams{InstanceName: "box01", Region: "ap-northeast-1", InstallKiro: installKiro})
		if err != nil {
			t.Fatalf("RenderBare(InstallKiro=%v): %v", installKiro, err)
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(got)
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Errorf("InstallKiro=%v: bash -n rejected the bare user-data: %v\n%s", installKiro, cerr, out)
		}
	}
}

// TestRenderBareAWSCLIWithoutTheSnap: the bare box gets the same AWS CLI
// as the regular image (Issue #101).
func TestRenderBareAWSCLIWithoutTheSnap(t *testing.T) {
	for _, kiro := range []bool{true, false} {
		got, err := RenderBare(BareParams{InstanceName: "dev01", Region: "ap-northeast-1", InstallKiro: kiro})
		if err != nil {
			t.Fatalf("RenderBare: %v", err)
		}
		assertAWSCLIFromTheInstaller(t, got, "apt-get install -y -qq git curl unzip unar jq build-essential gnupg")
	}
}
