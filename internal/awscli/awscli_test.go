package awscli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/kiro"
)

// The tests execute the fragments with bash against stub commands. The
// instance's fixed paths (/home/ubuntu, /snap/bin/aws) are redirected
// into a temporary directory, and PATH holds only the stubs and the few
// host tools the fragments need, so a missing gpg can be tested for real.

// hostTools are linked into the restricted PATH.
var hostTools = []string{"bash", "grep", "mktemp", "rm", "env", "mkdir", "ln", "chmod", "cat"}

type harness struct {
	dir, home, snapAWS, log string
	path                    string
}

func newHarness(t *testing.T, withGPG bool) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fragments run on the Linux instance")
	}
	dir := t.TempDir()
	h := &harness{
		dir:     dir,
		home:    filepath.Join(dir, "home", "ubuntu"),
		snapAWS: filepath.Join(dir, "snap", "bin", "aws"),
		log:     filepath.Join(dir, "calls.log"),
	}
	stubs := filepath.Join(dir, "stubs")
	tools := filepath.Join(dir, "tools")
	for _, d := range []string{stubs, tools, h.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range hostTools {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(p, filepath.Join(tools, tool)); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body = strings.ReplaceAll(body, "@LOG@", h.log)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// sudo -u ubuntu [-H] <cmd...>: runs as the current user, with HOME
	// set to the fake ubuntu home only for -H. Any other user is refused.
	write(filepath.Join(stubs, "sudo"), `#!/bin/bash
echo "sudo $*" >> "@LOG@"
[ "$1" = -u ] && [ "$2" = ubuntu ] || { echo "stub sudo: unexpected $*" >&2; exit 64; }
shift 2
if [ "${1:-}" = -H ]; then shift; export HOME="`+h.home+`"; fi
exec "$@"
`)
	write(filepath.Join(stubs, "chown"), `#!/bin/bash
echo "chown $*" >> "@LOG@"
`)
	// curl serves a stand-in for AWS's install script, unless told to fail.
	write(filepath.Join(stubs, "curl"), `#!/bin/bash
echo "curl $*" >> "@LOG@"
[ -n "${NAKAMA_STUB_CURL_FAIL:-}" ] && { echo "curl: (22) 503" >&2; exit 22; }
out=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac; done
cat > "$out" <<'INSTALLER'
echo "installer $* HOME=$HOME XDG_DATA_HOME=$XDG_DATA_HOME XDG_BIN_HOME=$XDG_BIN_HOME" >> "@LOG@"
bin="$XDG_DATA_HOME/aws-cli/v2/current/bin"
[ -e "$bin/aws" ] && echo "installer: updating the existing install" >> "@LOG@"
mkdir -p "$bin" "$XDG_BIN_HOME"
cat > "$bin/aws" <<'AWS'
#!/bin/bash
echo "aws $* | HOME=$HOME AWS_CONFIG_FILE=$AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE=$AWS_SHARED_CREDENTIALS_FILE AWS_PROFILE=${AWS_PROFILE:-} PATH=$PATH" >> "@LOG@"
[ "$1" = --version ] && echo "aws-cli/2.99.0 Python/3.13 Linux/6 exe/x86_64"
exit 0
AWS
chmod 0755 "$bin/aws"
ln -sf "$bin/aws" "$XDG_BIN_HOME/aws"
INSTALLER
`)
	if withGPG {
		write(filepath.Join(stubs, "gpg"), "#!/bin/bash\nexit 0\n")
	}
	h.path = stubs + string(os.PathListSeparator) + tools
	return h
}

// writeSnap puts a stand-in for the snap's aws at the redirected path.
func (h *harness) writeSnap(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.snapAWS), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/bash\necho \"snap-aws $*\" >> \"" + h.log + "\"\n"
	if err := os.WriteFile(h.snapAWS, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

type run struct {
	out   string
	err   error
	calls []string
}

func (r run) called(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (r run) call(prefix string) string {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return c
		}
	}
	return ""
}

// exec runs `set -euo pipefail`, the fragments, then body, the way the
// user-data and the update scripts include them.
func (h *harness) exec(t *testing.T, body string, env ...string) run {
	t.Helper()
	redirect := func(s string) string {
		s = strings.ReplaceAll(s, "/home/ubuntu/", h.home+"/")
		return strings.ReplaceAll(s, "/snap/bin/aws", h.snapAWS)
	}
	script := "set -euo pipefail\n" + redirect(InstallScript()) + "\n" + redirect(RunScript()) + "\n" + body + "\n"
	_ = os.Remove(h.log)
	cmd := exec.Command(filepath.Join(h.dir, "tools", "bash"), "-c", script)
	cmd.Env = append([]string{"PATH=" + h.path, "LC_ALL=C"}, env...)
	out, err := cmd.CombinedOutput()
	var calls []string
	if b, rerr := os.ReadFile(h.log); rerr == nil {
		calls = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return run{out: string(out), err: err, calls: calls}
}

func TestInstallAWSCLIForTheUbuntuUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	r := h.exec(t, "nakama_install_awscli\necho done")
	if r.err != nil || !strings.Contains(r.out, "done") {
		t.Fatalf("install failed: %v\n%s", r.err, r.out)
	}
	if !r.called("sudo -u ubuntu -H bash -c") {
		t.Errorf("the install must run as ubuntu; calls: %q", r.calls)
	}
	if !r.called("curl -fsSL https://awscli.amazonaws.com/v2/install.sh -o ") {
		t.Errorf("AWS's install script must be downloaded to a file; calls: %q", r.calls)
	}
	inst := r.call("installer ")
	for _, want := range []string{
		"installer --quiet ",
		"HOME=" + h.home + " ",
		"XDG_DATA_HOME=" + h.home + "/.local ",
		"XDG_BIN_HOME=" + h.home + "/.local/bin",
	} {
		if !strings.Contains(inst, want) {
			t.Errorf("installer call %q lacks %q", inst, want)
		}
	}
	// The layout the request asked for: ~/.local/aws-cli and ~/.local/bin/aws.
	official := filepath.Join(h.home, ".local", "aws-cli", "v2", "current", "bin", "aws")
	if fi, err := os.Stat(official); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("%s is missing or not executable: %v", official, err)
	}
	if got := strings.TrimPrefix(InstalledBin, "/home/ubuntu"); got != strings.TrimPrefix(official, h.home) {
		t.Errorf("InstalledBin = %s does not match the install location %s", InstalledBin, official)
	}
	if target, err := os.Readlink(filepath.Join(h.home, ".local", "bin", "aws")); err != nil || target != official {
		t.Errorf("~/.local/bin/aws -> %q (%v), want %s", target, err, official)
	}
	if !r.called("aws --version") || !strings.Contains(r.out, "aws-cli/2.99.0") {
		t.Errorf("the install must end by running aws --version; calls %q", r.calls)
	}
	bashrc, _ := os.ReadFile(filepath.Join(h.home, ".bashrc"))
	if n := strings.Count(string(bashrc), `export PATH="$HOME/.local/bin:$PATH"`); n != 1 {
		t.Errorf("~/.bashrc carries the PATH line %d times, want 1", n)
	}
	if !r.called("chown ubuntu:ubuntu " + h.home + "/.bashrc") {
		t.Errorf("~/.bashrc must stay owned by ubuntu; calls %q", r.calls)
	}
}

func TestInstallAWSCLIAgainUpdatesWithoutDuplicatingThePath(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	if r := h.exec(t, "nakama_install_awscli"); r.err != nil {
		t.Fatalf("first install: %v\n%s", r.err, r.out)
	}
	r := h.exec(t, "nakama_install_awscli")
	if r.err != nil || !r.called("installer: updating the existing install") {
		t.Fatalf("the second run must update in place: %v, calls %q\n%s", r.err, r.calls, r.out)
	}
	bashrc, _ := os.ReadFile(filepath.Join(h.home, ".bashrc"))
	if n := strings.Count(string(bashrc), `export PATH="$HOME/.local/bin:$PATH"`); n != 1 {
		t.Errorf("~/.bashrc carries the PATH line %d times after two runs, want 1", n)
	}
}

// TestInstallAWSCLIStopsTheCaller: without gpg the download would be
// installed unverified, and a failed download must not run anything;
// either way the caller under set -e stops.
func TestInstallAWSCLIStopsTheCaller(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		withGPG bool
		env     []string
		want    string
	}{
		{name: "no gpg", withGPG: false, want: "gpg is required"},
		{name: "download fails", withGPG: true, env: []string{"NAKAMA_STUB_CURL_FAIL=1"}, want: "curl: (22)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, c.withGPG)
			r := h.exec(t, "nakama_install_awscli\necho after-install", c.env...)
			if r.err == nil || strings.Contains(r.out, "after-install") {
				t.Fatalf("the caller must stop: err %v\n%s", r.err, r.out)
			}
			if !strings.Contains(r.out, c.want) {
				t.Errorf("output lacks %q:\n%s", c.want, r.out)
			}
			if r.called("installer ") {
				t.Errorf("the installer must not run; calls %q", r.calls)
			}
			if _, err := os.Stat(filepath.Join(h.home, ".local", "bin", "aws")); err == nil {
				t.Errorf("nothing may be installed")
			}
		})
	}
}

// TestNakamaAWSRunsTheOfficialCLIAsUbuntuWithoutItsSettings: root
// scripts must never execute the ubuntu-owned binary as root, and
// ubuntu's AWS settings must not steer nakama's calls.
func TestNakamaAWSRunsTheOfficialCLIAsUbuntuWithoutItsSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	h.writeSnap(t)
	r := h.exec(t, "nakama_install_awscli >/dev/null\nnakama_aws ssm get-parameter --name /nakama/dev01/password",
		"AWS_PROFILE=from-the-caller", "AWS_CONFIG_FILE=/home/ubuntu/.aws/config")
	if r.err != nil {
		t.Fatalf("nakama_aws failed: %v\n%s", r.err, r.out)
	}
	if !r.called("sudo -u ubuntu env -i ") {
		t.Errorf("the official CLI must run as ubuntu with an empty environment; calls %q", r.calls)
	}
	got := r.call("aws ssm get-parameter")
	for _, want := range []string{
		"HOME=/nonexistent ", "AWS_CONFIG_FILE=/dev/null ", "AWS_SHARED_CREDENTIALS_FILE=/dev/null ",
		"AWS_PROFILE= ", "PATH=/usr/bin:/bin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("aws call %q lacks %q", got, want)
		}
	}
	if r.called("snap-aws") {
		t.Errorf("the snap must not be used when the official CLI is installed")
	}
}

func TestNakamaAWSFallsBackToTheSnapOnAnOlderDisk(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	h.writeSnap(t)
	r := h.exec(t, "nakama_aws ssm put-parameter --name /x")
	if r.err != nil || !r.called("snap-aws ssm put-parameter --name /x") {
		t.Fatalf("want the snap: %v, calls %q\n%s", r.err, r.calls, r.out)
	}
	if r.called("sudo") {
		t.Errorf("the snap runs as root, as before; calls %q", r.calls)
	}

	none := newHarness(t, true)
	r = none.exec(t, "nakama_aws --version || echo rc=$?")
	if !strings.Contains(r.out, "rc=127") || !strings.Contains(r.out, "the AWS CLI is not installed") {
		t.Errorf("without any AWS CLI: %s", r.out)
	}
}

// TestPathLineMatchesTheKiroInstall: both fragments add the same line,
// so whichever runs first adds it and the other finds it.
func TestPathLineMatchesTheKiroInstall(t *testing.T) {
	t.Parallel()
	const line = `export PATH="$HOME/.local/bin:$PATH"`
	if !strings.Contains(InstallScript(), line) || !strings.Contains(kiro.InstallScript(), line) {
		t.Errorf("the AWS CLI and Kiro fragments must add the same PATH line %q", line)
	}
}

func TestFragmentsAreValidBashAndLF(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]string{"install.sh": InstallScript(), "run.sh": RunScript()} {
		if strings.Contains(s, "\r") {
			t.Errorf("%s contains a carriage return; check it out with LF (see .gitattributes)", name)
		}
		if strings.HasPrefix(s, "#!") || strings.Contains(s, "\nset -") {
			t.Errorf("%s must be a fragment: no shebang and no set of its own", name)
		}
		if _, err := exec.LookPath("bash"); err != nil {
			continue
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("bash -n rejected %s: %v\n%s", name, err, out)
		}
	}
}
