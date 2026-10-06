package kiro

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The tests in this file execute the install fragment with bash against
// stub commands, rather than only checking that certain strings appear
// in it. Issue #79 was a string that looked right to such checks
// (`kiro-cli crew -y install`) and failed on every real instance:
// `kiro-cli crew` forwards its trailing arguments to the Kiro Crew CLI,
// and `kirocrew install` does not exist. The stubs reproduce that
// argument handling, so a wrong command line fails here instead.

// stubKiroCLI behaves like `kiro-cli crew` as documented by
// `kiro-cli crew --help` (kiro-cli 2.28.0): options are accepted only
// before the first trailing argument, the Kiro Crew CLI is installed if
// missing (with -y, without prompting), and every trailing argument,
// after an optional `--`, is forwarded to it.
const stubKiroCLI = `#!/bin/bash
echo "kiro-cli $*" >> "$NAKAMA_STUB_LOG"
[ "$1" = crew ] || { echo "stub kiro-cli: only 'crew' is modelled" >&2; exit 64; }
shift
yes=0
while [ $# -gt 0 ]; do
  case "$1" in
    -y|--yes) yes=1; shift ;;
    -v|--verbose) shift ;;
    --) shift; break ;;
    -*) echo "kiro-cli crew: unexpected option $1" >&2; exit 2 ;;
    *) break ;;
  esac
done
if [ ! -x "$HOME/.local/bin/kirocrew" ]; then
  [ "$yes" = 1 ] || { echo "kiro-cli crew: install Kiro Crew? (no tty)" >&2; exit 1; }
  echo "Installing Kiro Crew..."
  mkdir -p "$HOME/.kiro/crew-venv/bin"
  chmod 0775 "$HOME/.kiro" "$HOME/.kiro/crew-venv" "$HOME/.kiro/crew-venv/bin"
  cp "$NAKAMA_STUB_KIROCREW" "$HOME/.kiro/crew-venv/bin/kirocrew"
  ln -sf "$HOME/.kiro/crew-venv/bin/kirocrew" "$HOME/.local/bin/kirocrew"
fi
exec "$HOME/.local/bin/kirocrew" "$@"
`

// stubKirocrew accepts the subcommands Kiro Crew 0.7.2 has and rejects
// the rest with argparse's exit status 2, as the real CLI does.
// `service install` enforces the AppArmor precondition (no group- or
// world-writable ~/.kiro), which the install script must satisfy first.
const stubKirocrew = `#!/bin/bash
echo "kirocrew $*" >> "$NAKAMA_STUB_LOG"
case "$1" in
  --version) echo "kirocrew 0.7.2"; exit 0 ;;
  service)
    [ "$2" = install ] || { echo "kirocrew service: invalid choice: '$2'" >&2; exit 2; }
    if [ -n "$(find "$HOME/.kiro" -perm /022 -print -quit)" ]; then
      echo "kirocrew: refusing to install the AppArmor profile: $HOME/.kiro is group/world-writable" >&2
      exit 1
    fi
    echo "service installed" >> "$NAKAMA_STUB_LOG"
    exit 0 ;;
  gateway|doctor|status|restart|stop|logs|token|update|setup|config) exit 0 ;;
  *) echo "kirocrew: error: argument <command>: invalid choice: '$1'" >&2; exit 2 ;;
esac
`

// stubCurl stands in for `curl -fsSL https://cli.kiro.dev/install`: it
// prints an installer that places the kiro-cli stub in ~/.local/bin.
const stubCurl = `#!/bin/bash
echo "curl $*" >> "$NAKAMA_STUB_LOG"
cat <<'INSTALLER'
mkdir -p "$HOME/.local/bin"
cp "$NAKAMA_STUB_KIROCLI" "$HOME/.local/bin/kiro-cli"
chmod 0755 "$HOME/.local/bin/kiro-cli"
echo "Installation complete!"
INSTALLER
`

// stubSudo runs `sudo -u ubuntu -H <cmd...>` as the current user, with
// HOME pointing at the fake ubuntu home. Anything else is refused, so
// the script cannot drop to a user other than ubuntu unnoticed.
const stubSudo = `#!/bin/bash
[ "$1" = -u ] && [ "$2" = ubuntu ] && [ "$3" = -H ] || { echo "stub sudo: unexpected $*" >&2; exit 64; }
shift 3
export HOME="$NAKAMA_STUB_HOME"
exec "$@"
`

// stubChown accepts the one chown the script does (of the fake
// ~/.bashrc to ubuntu:ubuntu) without needing root.
const stubChown = `#!/bin/bash
echo "chown $*" >> "$NAKAMA_STUB_LOG"
`

type installRun struct {
	out  string   // combined stdout/stderr of the fragment
	log  []string // commands the stubs saw, in order
	home string   // the fake ubuntu home
}

// runInstallScript executes script (the fragment, wrapped the way the
// user-data wraps it) with the stubs first on PATH. preinstalled
// creates a working Kiro CLI and Kiro Crew beforehand, as on an
// instance where the script runs a second time (`setup rerun`).
func runInstallScript(t *testing.T, script string, preinstalled bool) installRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the install script runs on the Linux instance")
	}
	for _, tool := range []string{"bash", "find", "grep", "mkdir", "cp", "ln", "chmod"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "stubs")
	home := filepath.Join(dir, "home", "ubuntu")
	stubs := filepath.Join(dir, "stub-src")
	for _, d := range []string{bin, home, stubs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "sudo"), stubSudo)
	write(filepath.Join(bin, "curl"), stubCurl)
	write(filepath.Join(bin, "chown"), stubChown)
	write(filepath.Join(stubs, "kiro-cli"), stubKiroCLI)
	write(filepath.Join(stubs, "kirocrew"), stubKirocrew)
	if preinstalled {
		local := filepath.Join(home, ".local", "bin")
		venv := filepath.Join(home, ".kiro", "crew-venv", "bin")
		for _, d := range []string{local, venv} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(local, "kiro-cli"), stubKiroCLI)
		write(filepath.Join(venv, "kirocrew"), stubKirocrew)
		if err := os.Symlink(filepath.Join(venv, "kirocrew"), filepath.Join(local, "kirocrew")); err != nil {
			t.Fatal(err)
		}
		// As on the instance: umask 0002 left the tree group-writable.
		if err := os.Chmod(filepath.Join(home, ".kiro"), 0o775); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(dir, "calls.log")

	// The fragment hard-codes /home/ubuntu for ~/.bashrc; point it at the
	// fake home so the test never touches the real file system.
	fragment := strings.ReplaceAll(script, "/home/ubuntu/", home+"/")
	wrapped := "set -euo pipefail\n" + fragment + "\necho nakama-fragment-returned\n"
	cmd := exec.Command("bash", "-c", wrapped)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NAKAMA_STUB_LOG="+logPath,
		"NAKAMA_STUB_HOME="+home,
		"NAKAMA_STUB_KIROCLI="+filepath.Join(stubs, "kiro-cli"),
		"NAKAMA_STUB_KIROCREW="+filepath.Join(stubs, "kirocrew"),
		"LC_ALL=C",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the fragment must never abort its caller (it is best-effort): %v\n%s", err, out)
	}
	var calls []string
	if b, err := os.ReadFile(logPath); err == nil {
		calls = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return installRun{out: string(out), log: calls, home: home}
}

func (r installRun) called(prefix string) bool {
	for _, c := range r.log {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestInstallScriptInstallsTheGatewayService(t *testing.T) {
	for _, tc := range []struct {
		name         string
		preinstalled bool
	}{
		{"fresh instance", false},
		{"re-run on an instance that already has Kiro Crew", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runInstallScript(t, InstallScript(), tc.preinstalled)
			if !r.called("service installed") {
				t.Fatalf("the gateway service was not installed\ncalls: %q\noutput:\n%s", r.log, r.out)
			}
			if strings.Contains(r.out, "WARNING") {
				t.Errorf("a successful install must not warn:\n%s", r.out)
			}
			if !strings.Contains(r.out, "nakama: Kiro CLI and Kiro Crew ready") {
				t.Errorf("the success message is missing:\n%s", r.out)
			}
			if !r.called("kirocrew --version") {
				t.Errorf("the Kiro Crew step must end in `kirocrew --version` (via `kiro-cli crew -y -- --version`); calls: %q", r.log)
			}
			// The AppArmor precondition: nothing under ~/.kiro is group- or
			// world-writable when the service is installed.
			err := filepath.Walk(filepath.Join(r.home, ".kiro"), func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi.Mode().Perm()&0o022 != 0 {
					t.Errorf("%s is %v: the service install would refuse", p, fi.Mode().Perm())
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// The PATH line is added once, even on a re-run.
			bashrc, _ := os.ReadFile(filepath.Join(r.home, ".bashrc"))
			if n := strings.Count(string(bashrc), `export PATH="$HOME/.local/bin:$PATH"`); n != 1 {
				t.Errorf("~/.bashrc carries the PATH line %d times, want 1", n)
			}
		})
	}
}

// TestInstallScriptRegressionIssue79 replays the shipped command line
// (`kiro-cli crew -y install`) through the same harness and expects it
// to fail the way it failed on the instances: kirocrew rejects
// `install`, the inner shell stops, the service is never installed,
// and the fragment only warns. This proves the harness models
// `kiro-cli crew`'s argument forwarding, so the passing test above is
// meaningful.
func TestInstallScriptRegressionIssue79(t *testing.T) {
	broken := strings.Replace(InstallScript(),
		`"$HOME/.local/bin/kiro-cli" crew -y -- --version`,
		`"$HOME/.local/bin/kiro-cli" crew -y install`, 1)
	if broken == InstallScript() {
		t.Fatal("the fixed command line is not in the script; update this test")
	}
	r := runInstallScript(t, broken, false)
	if r.called("service installed") {
		t.Fatal("the harness accepted `kiro-cli crew -y install`; it no longer models how kiro-cli forwards arguments")
	}
	if !strings.Contains(r.out, "invalid choice: 'install'") || !strings.Contains(r.out, "WARNING Kiro CLI / Kiro Crew setup failed") {
		t.Errorf("want kirocrew's rejection and the best-effort warning, got:\n%s", r.out)
	}
	if !strings.Contains(r.out, "nakama-fragment-returned") {
		t.Error("even a failed install must return control to the including script")
	}
}

// TestInstallScriptFailureIsBestEffortWhenTheDownloadFails pins the
// pipefail behaviour with an actual failing download: no kiro-cli is
// installed, nothing else runs, the fragment warns and returns.
func TestInstallScriptFailureIsBestEffortWhenTheDownloadFails(t *testing.T) {
	failing := strings.Replace(InstallScript(),
		"curl -fsSL https://cli.kiro.dev/install | bash",
		"curl -fsSL https://cli.kiro.dev/install && false | bash", 1)
	if failing == InstallScript() {
		t.Fatal("the download line is not in the script; update this test")
	}
	r := runInstallScript(t, failing, false)
	if r.called("kiro-cli") || r.called("service installed") {
		t.Errorf("nothing may run after a failed download; calls: %q", r.log)
	}
	if !strings.Contains(r.out, "WARNING") || !strings.Contains(r.out, "nakama-fragment-returned") {
		t.Errorf("want the warning and control back in the caller:\n%s", r.out)
	}
}
