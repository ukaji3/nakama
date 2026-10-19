package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The tests in this file execute the rendered update scripts with bash
// against stub commands, so that what reaches the Run Command stdout,
// stderr and the log on the instance is checked, not only the text of
// the script.

// updateStubs are the commands the update scripts call. Each records its
// arguments; NAKAMA_STUB_FAIL names one that must fail (exit 100), and
// NAKAMA_STUB_NOISE makes apt-get print that many bytes.
var updateStubs = map[string]string{
	"apt-get": `#!/bin/bash
echo "apt-get $*" >> "$NAKAMA_STUB_LOG"
if [ "$1" = dist-upgrade ] && [ -n "${NAKAMA_STUB_NOISE:-}" ]; then
  head -c "$NAKAMA_STUB_NOISE" /dev/zero | tr '\0' 'x'; echo
fi
if [ "${NAKAMA_STUB_FAIL:-}" = "apt-get $1" ]; then echo "E: stub apt-get $1 failed" >&2; exit 100; fi
echo "apt-get $1 done"
`,
	"fuser": `#!/bin/bash
exit 1
`,
	// curl serves the code-server installer as a no-op script, and AWS's
	// install script (requested with -o) as one that records its run and
	// reinstalls the official CLI stub.
	"curl": `#!/bin/bash
echo "curl $*" >> "$NAKAMA_STUB_LOG"
out=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac; done
if [ -z "$out" ]; then echo 'echo "code-server installed"'; exit 0; fi
[ "${NAKAMA_STUB_FAIL:-}" = "awscli install" ] && { echo "curl: (22) 503" >&2; exit 22; }
cat > "$out" <<'INSTALLER'
echo "awscli installer $* XDG_DATA_HOME=$XDG_DATA_HOME" >> "$NAKAMA_STUB_LOG"
echo "aws-cli/2.99.1 Python/3.13 Linux/6 exe/x86_64" > "$XDG_DATA_HOME/aws-cli/version"
INSTALLER
`,
	"systemctl": `#!/bin/bash
echo "systemctl $*" >> "$NAKAMA_STUB_LOG"
`,
	// sudo -u ubuntu [-H] <cmd...> runs as the current user with the fake
	// ubuntu home; anything else is refused.
	"sudo": `#!/bin/bash
[ "$1" = -u ] && [ "$2" = ubuntu ] || { echo "stub sudo: unexpected $*" >&2; exit 64; }
shift 2
[ "${1:-}" = -H ] && shift
export HOME="$NAKAMA_STUB_HOME"
exec "$@"
`,
	"code-server": `#!/bin/bash
echo "4.200.0 abc with Code 1.200.0"
`,
	// gpg is present on instances created after Issue #101.
	"gpg": `#!/bin/bash
exit 0
`,
	"chown": `#!/bin/bash
echo "chown $*" >> "$NAKAMA_STUB_LOG"
`,
	"aws": `#!/bin/bash
echo "aws $*" >> "$NAKAMA_STUB_LOG"
`,
}

type updateRun struct {
	stdout, stderr string
	exitErr        error
	log            string   // the update log on the "instance"
	calls          []string // what the stubs saw, in order
	home           string   // the fake ubuntu home
}

func (r updateRun) called(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// runUpdateScript runs script with the stubs first on PATH. The fixed
// paths of the instance (the log, the reboot flag, /home/ubuntu) are
// redirected into a temporary directory; prepare may add files there.
func runUpdateScript(t *testing.T, script string, env []string, prepare func(home string)) updateRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the update script runs on the Linux instance")
	}
	for _, tool := range []string{"bash", "head", "tr", "tail", "awk", "date", "seq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "stubs")
	home := filepath.Join(dir, "home", "ubuntu")
	for _, d := range []string{bin, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range updateStubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if prepare != nil {
		prepare(home)
	}
	logPath := filepath.Join(dir, "update.log")
	rebootFlag := filepath.Join(dir, "reboot-required")
	if strings.Contains(strings.Join(env, " "), "NAKAMA_TEST_REBOOT=1") {
		if err := os.WriteFile(rebootFlag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	callLog := filepath.Join(dir, "calls.log")

	s := strings.ReplaceAll(script, updateLogPath, logPath)
	s = strings.ReplaceAll(s, "/var/run/reboot-required", rebootFlag)
	s = strings.ReplaceAll(s, "/home/ubuntu/", home+"/")
	cmd := exec.Command("bash", "-c", s)
	cmd.Env = append(append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NAKAMA_STUB_LOG="+callLog,
		"NAKAMA_STUB_HOME="+home,
		"LC_ALL=C",
	), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := updateRun{stdout: stdout.String(), stderr: stderr.String(), exitErr: err, home: home}
	if b, rerr := os.ReadFile(logPath); rerr == nil {
		r.log = string(b)
	}
	if b, rerr := os.ReadFile(callLog); rerr == nil {
		r.calls = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return r
}

// ssmStdoutLimit is how much of a command's stdout GetCommandInvocation
// returns (StandardOutputContent: "The first 24,000 characters").
const ssmStdoutLimit = 24000

// ssmStdout is what the caller gets back of stdout.
func ssmStdout(s string) string {
	if len(s) > ssmStdoutLimit {
		return s[:ssmStdoutLimit]
	}
	return s
}

func bothUpdateScripts() map[string]string {
	return map[string]string{
		"code-server": buildUpdateScript("ap-northeast-1", "/nakama/dev01/meta/code-server-version"),
		"bare":        buildBareUpdateScript(),
	}
}

// TestUpdateScriptKeepsTheMarkersWithinTheStdoutLimit: SSM returns only
// the first 24,000 characters of stdout, so the reboot marker must not
// depend on how much apt prints.
func TestUpdateScriptKeepsTheMarkersWithinTheStdoutLimit(t *testing.T) {
	for name, script := range bothUpdateScripts() {
		t.Run(name, func(t *testing.T) {
			r := runUpdateScript(t, script, []string{"NAKAMA_STUB_NOISE=60000", "NAKAMA_TEST_REBOOT=1"}, nil)
			if r.exitErr != nil {
				t.Fatalf("update failed: %v\nstderr: %s", r.exitErr, r.stderr)
			}
			if !rebootRequired(ssmStdout(r.stdout)) {
				t.Errorf("the reboot marker is not within the first %d characters of stdout (%d bytes): a required reboot would be missed", ssmStdoutLimit, len(r.stdout))
			}
			for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
				if !strings.HasPrefix(line, "NAKAMA_") {
					t.Errorf("stdout carries more than result markers: %q", line)
				}
			}
			if len(r.stdout) > 1000 {
				t.Errorf("stdout is %d bytes; only the markers belong there", len(r.stdout))
			}
			if !strings.Contains(r.log, "apt-get dist-upgrade done") || len(r.log) < 60000 || !strings.Contains(r.log, "=== nakama update ") {
				t.Errorf("the full output must be in the log (%d bytes)", len(r.log))
			}
			if !strings.Contains(r.log, "+ apt-get update") {
				t.Errorf("the log should carry the xtrace of the script")
			}
		})
	}
}

func TestUpdateScriptReportsNoReboot(t *testing.T) {
	for name, script := range bothUpdateScripts() {
		r := runUpdateScript(t, script, nil, nil)
		if r.exitErr != nil || rebootRequired(r.stdout) || !strings.Contains(r.stdout, "NAKAMA_REBOOT_REQUIRED=0") {
			t.Errorf("%s: exit %v, stdout %q; want success with NAKAMA_REBOOT_REQUIRED=0", name, r.exitErr, r.stdout)
		}
	}
}

// TestUpdateScriptFailureShowsTheEndOfTheLog: a failure stops the script
// and puts the end of the log on stderr, small enough for the caller's
// 4,000-character error excerpt.
func TestUpdateScriptFailureShowsTheEndOfTheLog(t *testing.T) {
	for name, script := range bothUpdateScripts() {
		t.Run(name, func(t *testing.T) {
			r := runUpdateScript(t, script, []string{"NAKAMA_STUB_FAIL=apt-get dist-upgrade", "NAKAMA_STUB_NOISE=60000"}, nil)
			if r.exitErr == nil {
				t.Fatal("a failed apt-get must fail the update")
			}
			if strings.Contains(r.stdout, "NAKAMA_REBOOT_REQUIRED") {
				t.Errorf("no result marker after a failure: %q", r.stdout)
			}
			if !strings.Contains(r.stderr, "the update failed (exit 100)") || !strings.Contains(r.stderr, "E: stub apt-get dist-upgrade failed") {
				t.Errorf("stderr should name the failure and end with the log:\n%s", r.stderr)
			}
			if len(r.stderr) > 3900 {
				t.Errorf("stderr is %d bytes; the caller shows only the first 4,000", len(r.stderr))
			}
			if r.called("systemctl restart code-server") {
				t.Errorf("nothing may run after the failure: %q", r.calls)
			}
		})
	}
}

// officialAWSCLI installs a stand-in for the official AWS CLI in the fake
// ubuntu home, as on an instance created after Issue #101. It prints the
// version the (stub) installer last wrote and records its calls.
func officialAWSCLI(t *testing.T) func(home string) {
	return func(home string) {
		t.Helper()
		bin := filepath.Join(home, ".local", "aws-cli", "v2", "current", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		// nakama_aws clears the environment (env -i), so the stub carries
		// the call log path itself (runUpdateScript puts it two levels up).
		callLog := filepath.Join(filepath.Dir(filepath.Dir(home)), "calls.log")
		body := "#!/bin/bash\necho \"official-aws $* HOME=$HOME\" >> \"" + callLog + "\"\n" +
			"if [ \"$1\" = --version ]; then cat \"" + filepath.Join(home, ".local", "aws-cli", "version") + "\" 2>/dev/null || echo \"aws-cli/2.98.0 Python/3.13\"; fi\n"
		if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(bin, "aws"), filepath.Join(home, ".local", "bin", "aws")); err != nil {
			t.Fatal(err)
		}
	}
}

// kiroCrew installs a stand-in kirocrew in the fake ubuntu home. Like
// the real one, restart prints a dashboard URL with an access token.
func kiroCrew(t *testing.T, failOn string) func(home string) {
	return func(home string) {
		t.Helper()
		bin := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `#!/bin/bash
echo "kirocrew $* HOME=$HOME" >> "$NAKAMA_STUB_LOG"
[ "$1" = "` + failOn + `" ] && { echo "kirocrew $1: failed" >&2; exit 1; }
[ "$1" = restart ] && printf '\xe2\x9c\x85 Restarted kirocrew service.\n\xf0\x9f\x94\x91 http://kirocrew.localhost:5476?token=SECRET-TOKEN-123\n'
exit 0
`
		if err := os.WriteFile(filepath.Join(bin, "kirocrew"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func both(fns ...func(string)) func(string) {
	return func(home string) {
		for _, f := range fns {
			f(home)
		}
	}
}

// TestUpdateScriptUpdatesTheAWSCLIAndKiroCrew covers an instance created
// after Issue #101, with Kiro Crew.
func TestUpdateScriptUpdatesTheAWSCLIAndKiroCrew(t *testing.T) {
	for name, script := range bothUpdateScripts() {
		t.Run(name, func(t *testing.T) {
			r := runUpdateScript(t, script, nil, both(officialAWSCLI(t), kiroCrew(t, "")))
			if r.exitErr != nil {
				t.Fatalf("update failed: %v\nstderr: %s\nlog: %s", r.exitErr, r.stderr, r.log)
			}
			if !r.called("awscli installer --quiet XDG_DATA_HOME=" + r.home + "/.local") {
				t.Errorf("the AWS CLI must be updated in ~/.local/aws-cli; calls %q", r.calls)
			}
			if got := updateMarker(r.stdout, "NAKAMA_AWSCLI"); got != "2.99.1" {
				t.Errorf("NAKAMA_AWSCLI = %q, want the version after the update (2.99.1); stdout %q", got, r.stdout)
			}
			// kirocrew update, then restart, both as ubuntu, in that order.
			var kc []string
			for _, c := range r.calls {
				if strings.HasPrefix(c, "kirocrew ") {
					kc = append(kc, c)
				}
			}
			if len(kc) != 2 || !strings.HasPrefix(kc[0], "kirocrew update HOME="+r.home) || !strings.HasPrefix(kc[1], "kirocrew restart HOME="+r.home) {
				t.Errorf("want kirocrew update then restart as ubuntu; got %q", kc)
			}
			if updateMarker(r.stdout, "NAKAMA_KIROCREW") != KiroCrewUpdated {
				t.Errorf("stdout %q lacks NAKAMA_KIROCREW=updated", r.stdout)
			}
			// restart prints a dashboard token; it must not reach the log
			// (or SSM's output, which a failure would show).
			if strings.Contains(r.log+r.stdout+r.stderr, "SECRET-TOKEN-123") {
				t.Errorf("the Kiro Crew access token leaked into the update output")
			}
			if !strings.Contains(r.log, "Kiro Crew updated and restarted") {
				t.Errorf("the log should say Kiro Crew was updated")
			}
		})
	}
}

// TestUpdateScriptLeavesOlderInstancesAlone: an instance from before
// Issue #101 keeps its snap AWS CLI (no official install is added), and
// one created with --no-kiro has no Kiro Crew to update.
func TestUpdateScriptLeavesOlderInstancesAlone(t *testing.T) {
	for name, script := range bothUpdateScripts() {
		t.Run(name, func(t *testing.T) {
			r := runUpdateScript(t, script, nil, nil)
			if r.exitErr != nil {
				t.Fatalf("update failed: %v\nstderr: %s", r.exitErr, r.stderr)
			}
			if r.called("awscli installer") || r.called("curl -fsSL https://awscli.amazonaws.com") {
				t.Errorf("no official AWS CLI may be installed on an older instance; calls %q", r.calls)
			}
			if _, err := os.Stat(filepath.Join(r.home, ".local", "bin", "aws")); err == nil {
				t.Errorf("~/.local/bin/aws appeared")
			}
			if r.called("kirocrew") || strings.Contains(r.stdout, "NAKAMA_KIROCREW") || strings.Contains(r.stdout, "NAKAMA_AWSCLI") {
				t.Errorf("nothing to report without the tools: calls %q, stdout %q", r.calls, r.stdout)
			}
			if !strings.Contains(r.stdout, "NAKAMA_REBOOT_REQUIRED=0") {
				t.Errorf("the update must still finish: %q", r.stdout)
			}
		})
	}
}

// TestUpdateScriptKiroCrewFailureOnlyWarns: the OS and code-server are
// updated by then, so a failing kirocrew does not fail the update.
func TestUpdateScriptKiroCrewFailureOnlyWarns(t *testing.T) {
	for _, failOn := range []string{"update", "restart"} {
		r := runUpdateScript(t, buildUpdateScript("ap-northeast-1", "/v"), nil, kiroCrew(t, failOn))
		if r.exitErr != nil {
			t.Fatalf("kirocrew %s failing must not fail the update: %v\n%s", failOn, r.exitErr, r.stderr)
		}
		if updateMarker(r.stdout, "NAKAMA_KIROCREW") != KiroCrewFailed || !strings.Contains(r.stdout, "NAKAMA_REBOOT_REQUIRED=0") {
			t.Errorf("kirocrew %s: stdout %q, want NAKAMA_KIROCREW=failed and the reboot marker", failOn, r.stdout)
		}
		if failOn == "update" && r.called("kirocrew restart") {
			t.Errorf("restart must not run after a failed update; calls %q", r.calls)
		}
		if !strings.Contains(r.log, "WARNING kirocrew update or kirocrew restart failed") {
			t.Errorf("the log should carry the warning")
		}
	}
}

// TestUpdateScriptAWSCLIFailureFailsTheUpdate: as with code-server, a
// failed AWS CLI update fails the command.
func TestUpdateScriptAWSCLIFailureFailsTheUpdate(t *testing.T) {
	r := runUpdateScript(t, buildUpdateScript("ap-northeast-1", "/v"), []string{"NAKAMA_STUB_FAIL=awscli install"},
		both(officialAWSCLI(t), kiroCrew(t, "")))
	if r.exitErr == nil {
		t.Fatal("a failed AWS CLI update must fail the update")
	}
	if !strings.Contains(r.stderr, "curl: (22)") || strings.Contains(r.stdout, "NAKAMA_REBOOT_REQUIRED") {
		t.Errorf("stderr %q / stdout %q", r.stderr, r.stdout)
	}
}

// TestUpdateScriptRecordsTheVersionThroughNakamaAWS: on an instance with
// the official CLI, the code-server version is written by running it as
// ubuntu with ubuntu's AWS settings ignored, not as root.
func TestUpdateScriptRecordsTheVersionThroughNakamaAWS(t *testing.T) {
	r := runUpdateScript(t, buildUpdateScript("ap-northeast-1", "/nakama/dev01/meta/code-server-version"), nil, officialAWSCLI(t))
	if r.exitErr != nil {
		t.Fatalf("update failed: %v\n%s", r.exitErr, r.stderr)
	}
	want := "official-aws ssm put-parameter --region ap-northeast-1 --name /nakama/dev01/meta/code-server-version --type String --overwrite --value 4.200.0 HOME=/nonexistent"
	if !r.called(want) {
		t.Errorf("want %q; calls %q", want, r.calls)
	}
	if r.called("aws ssm put-parameter") {
		t.Errorf("root's aws (the snap link) must not be used when the official CLI is there; calls %q", r.calls)
	}
}
