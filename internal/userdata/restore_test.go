package userdata

import (
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func restoreParams() RestoreParams {
	return RestoreParams{
		InstanceName:         "dev01",
		Region:               "ap-northeast-1",
		SSMPasswordParameter: "/nakama/dev01/password",
		SSMVersionParameter:  "/nakama/dev01/meta/code-server-version",
	}
}

// parseRestore checks the cloud-config framing and returns the parsed body.
func parseRestore(t *testing.T, ud string) restoreCloudConfig {
	t.Helper()
	// cloud-init only treats the user data as cloud-config when the very
	// first line is exactly this marker.
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatalf("user data must start with the #cloud-config line, got %q", ud[:min(len(ud), 40)])
	}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(ud), &raw); err != nil {
		t.Fatalf("user data is not valid YAML: %v\n%s", err, ud)
	}
	// The host keys of the backup must survive the first boot of the
	// new instance ID; cloud-init deletes them unless told otherwise.
	if v, ok := raw["ssh_deletekeys"]; !ok || v != false {
		t.Fatalf("ssh_deletekeys = %v (present=%v), want false", v, ok)
	}
	var cfg restoreCloudConfig
	if err := yaml.Unmarshal([]byte(ud), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cfg
}

func TestRenderRestore_CodeServer(t *testing.T) {
	t.Parallel()
	ud, err := RenderRestore(restoreParams())
	if err != nil {
		t.Fatalf("RenderRestore: %v", err)
	}
	cfg := parseRestore(t, ud)
	if len(cfg.WriteFiles) != 1 {
		t.Fatalf("write_files = %+v, want one script", cfg.WriteFiles)
	}
	f := cfg.WriteFiles[0]
	if f.Path != RestoreScriptPath || f.Permissions != "0700" || f.Owner != "root:root" {
		t.Errorf("script file = %+v, want %s root:root 0700", f, RestoreScriptPath)
	}
	if len(cfg.RunCmd) != 1 || strings.Join(cfg.RunCmd[0], " ") != "bash "+RestoreScriptPath {
		t.Errorf("runcmd = %v, want [[bash %s]]", cfg.RunCmd, RestoreScriptPath)
	}
	for _, want := range []string{
		`PASSWORD_PARAM="/nakama/dev01/password"`,
		`VERSION_PARAM="/nakama/dev01/meta/code-server-version"`,
		`REGION="ap-northeast-1"`,
		// Nothing to do on the fresh launch volume, which has no code-server.
		`if [ ! -f "${CONFIG}" ]; then`,
		"--with-decryption",
		`grep -v '^password:'`,
		`systemctl restart "code-server@${TARGET_USER}"`,
		// The official AWS CLI as ubuntu, or the snap on an older disk
		// (Issue #101).
		"nakama_aws() {",
		"nakama_aws ssm get-parameter",
		"nakama_aws ssm put-parameter",
	} {
		if !strings.Contains(f.Content, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// The script handles the password: tracing would write it to the log
	// and to every later snapshot (CWE-532).
	for _, banned := range []string{"set -x", "set -o xtrace", "set -euxo"} {
		if strings.Contains(f.Content, banned) {
			t.Errorf("script must not enable tracing, found %q", banned)
		}
	}
	// Restore reinstalls nothing: the restored volume already has it all.
	for _, banned := range []string{"install.sh", "apt-get", "snap install"} {
		if strings.Contains(f.Content, banned) {
			t.Errorf("script must not install software, found %q", banned)
		}
	}
}

func TestRenderRestore_BareOnlyKeepsHostKeys(t *testing.T) {
	t.Parallel()
	ud, err := RenderRestore(RestoreParams{InstanceName: "box1", Region: "ap-northeast-1", Bare: true})
	if err != nil {
		t.Fatalf("RenderRestore: %v", err)
	}
	cfg := parseRestore(t, ud)
	if len(cfg.WriteFiles) != 0 || len(cfg.RunCmd) != 0 {
		t.Errorf("a bare restore has no code-server password to set; got write_files=%+v runcmd=%v", cfg.WriteFiles, cfg.RunCmd)
	}
	if strings.Contains(ud, "password") {
		t.Errorf("bare restore user data must not mention a password:\n%s", ud)
	}
}

func TestRenderRestore_RequiresTheParameters(t *testing.T) {
	t.Parallel()
	for _, p := range []RestoreParams{
		{InstanceName: "dev01", Region: "ap-northeast-1", SSMVersionParameter: "/v"},
		{InstanceName: "dev01", Region: "ap-northeast-1", SSMPasswordParameter: "/p"},
	} {
		if _, err := RenderRestore(p); err == nil {
			t.Errorf("RenderRestore(%+v) = nil error, want one", p)
		}
	}
}

func TestRenderedRestoreScriptIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	ud, err := RenderRestore(restoreParams())
	if err != nil {
		t.Fatalf("RenderRestore: %v", err)
	}
	cfg := parseRestore(t, ud)
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(cfg.WriteFiles[0].Content)
	if out, cerr := cmd.CombinedOutput(); cerr != nil {
		t.Errorf("bash -n rejected the restore script: %v\n%s", cerr, out)
	}
}
