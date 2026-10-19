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
	// curl serves the code-server installer as a no-op script.
	"curl": `#!/bin/bash
echo "curl $*" >> "$NAKAMA_STUB_LOG"
echo 'echo "code-server installed"'
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
