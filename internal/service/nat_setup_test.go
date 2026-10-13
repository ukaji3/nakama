package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The NAT setup script is executed with bash against stub commands
// rather than only grepped: Issue #87 was a script that looked fine and
// died on the instance (dnf OOM-killed, the failure hidden by
// `|| true`, then `iptables: command not found`). The stubs log every
// call; dnf and iptables can be told to fail.
//
// The script writes to absolute system paths (/etc/sysctl.d, /usr/local/
// sbin, /etc/systemd/system, /var/tmp). The test rewrites those prefixes
// into a temporary root before running it, which keeps the script under
// test byte-for-byte what the instance runs apart from that prefix.

// natStubs are the commands the script calls. NAKAMA_STUB_ROOT holds the
// fake state (installed packages, active units, the swap list, rules).
var natStubs = map[string]string{
	"dnf": `#!/bin/bash
echo "dnf $*" >> "$NAKAMA_STUB_LOG"
case " $* " in
  *" --setopt=install_weak_deps=False "*) ;;
  *) echo "stub dnf: weak dependencies must be off" >&2; exit 64 ;;
esac
if [ -n "${NAKAMA_STUB_DNF_FAILS:-}" ]; then
  n=$(cat "$NAKAMA_STUB_ROOT/dnf-fails" 2>/dev/null || echo 0)
  if [ "$n" -lt "$NAKAMA_STUB_DNF_FAILS" ]; then
    echo $((n + 1)) > "$NAKAMA_STUB_ROOT/dnf-fails"
    echo "stub dnf: transient failure" >&2
    exit 1
  fi
fi
case " $* " in
  *" install "*)
    for p in "$@"; do
      case "$p" in -*|install) ;; *) echo "$p" >> "$NAKAMA_STUB_ROOT/installed" ;; esac
    done
    if grep -qx iptables-nft "$NAKAMA_STUB_ROOT/installed"; then
      cp "$NAKAMA_STUB_BIN/.iptables" "$NAKAMA_STUB_BIN/iptables"
      chmod +x "$NAKAMA_STUB_BIN/iptables"
    fi ;;
esac
exit 0
`,
	".iptables": `#!/bin/bash
echo "iptables $*" >> "$NAKAMA_STUB_LOG"
rules="$NAKAMA_STUB_ROOT/rules"
touch "$rules"
case "$*" in
  "-t nat -S POSTROUTING") grep -- "-t nat -A POSTROUTING" "$rules" | sed 's/^-t nat //' ; exit 0 ;;
esac
args="$*"
case "$args" in
  *" -C "*|"-C "*) grep -qxF -- "${args/-C/-A}" "$rules"; exit $? ;;
  *" -A "*|"-A "*)
    # NAKAMA_STUB_DROP_NAT: accept nat-table rules without keeping them,
    # so the script's final verification has something to catch.
    case "$args" in "-t nat "*) [ -n "${NAKAMA_STUB_DROP_NAT:-}" ] && exit 0 ;; esac
    echo "$args" >> "$rules"; exit 0 ;;
esac
exit 0
`,
	"systemctl": `#!/bin/bash
echo "systemctl $*" >> "$NAKAMA_STUB_LOG"
case "$1" in
  restart|start)
    if [ "$2" = nakama-nat.service ]; then
      if PATH="$NAKAMA_STUB_BIN:$PATH" bash "$NAKAMA_STUB_SETUP_BIN"; then
        echo active > "$NAKAMA_STUB_ROOT/unit-nakama-nat"
      else
        echo failed > "$NAKAMA_STUB_ROOT/unit-nakama-nat"; exit 1
      fi
    fi ;;
  is-active)
    unit="${@: -1}"; unit="${unit%.service}"
    [ "$(cat "$NAKAMA_STUB_ROOT/unit-$unit" 2>/dev/null)" = active ]; exit $? ;;
esac
exit 0
`,
	"sysctl": `#!/bin/bash
echo "sysctl $*" >> "$NAKAMA_STUB_LOG"
case "$*" in
  "-n net.ipv4.ip_forward") cat "$NAKAMA_STUB_ROOT/ip_forward" 2>/dev/null || echo 0 ;;
  "-q -p "*) grep -q "net.ipv4.ip_forward = 1" "${@: -1}" && echo 1 > "$NAKAMA_STUB_ROOT/ip_forward" ;;
  *) echo "stub sysctl: unexpected $*" >&2; exit 1 ;;
esac
`,
	// swapon, mkswap and sysctl accept only the options of AL2023's
	// util-linux 2.37 and procps-ng 3.3 (checked with --help in the
	// amazonlinux:2023 image): the first instance run failed on
	// "mkswap: invalid option -- 'q'", which a permissive stub let pass.
	"swapon": `#!/bin/bash
echo "swapon $*" >> "$NAKAMA_STUB_LOG"
if [ "$1" = --show=NAME ]; then
  [ "$2" = --noheadings ] || { echo "stub swapon: unexpected $*" >&2; exit 1; }
  cat "$NAKAMA_STUB_ROOT/swaps" 2>/dev/null; exit 0
fi
[ "$1" = -p ] && [ $# -eq 3 ] || { echo "stub swapon: unexpected $*" >&2; exit 1; }
echo "$3" >> "$NAKAMA_STUB_ROOT/swaps"
`,
	"swapoff": `#!/bin/bash
echo "swapoff $*" >> "$NAKAMA_STUB_LOG"
grep -vxF "$1" "$NAKAMA_STUB_ROOT/swaps" > "$NAKAMA_STUB_ROOT/swaps.new" || true
mv "$NAKAMA_STUB_ROOT/swaps.new" "$NAKAMA_STUB_ROOT/swaps"
`,
	"mkswap": `#!/bin/bash
echo "mkswap $*" >> "$NAKAMA_STUB_LOG"
for a in "$@"; do
  case "$a" in
    -c|-f|--check|--force|--verbose) ;;
    -*) echo "mkswap: invalid option -- '${a#-}'" >&2; exit 1 ;;
  esac
done
[ -f "${@: -1}" ] || { echo "mkswap: cannot open ${@: -1}" >&2; exit 1; }
`,
	"dd": `#!/bin/bash
echo "dd $*" >> "$NAKAMA_STUB_LOG"
for a in "$@"; do case "$a" in of=*) : > "${a#of=}" ;; esac; done
`,
	"ip": `#!/bin/bash
case "$*" in
  "route show default 0.0.0.0/0") echo "default via 10.20.0.1 dev ens5 proto dhcp src 10.20.0.228 metric 512" ;;
esac
`,
	"sleep": `#!/bin/bash
echo "sleep $*" >> "$NAKAMA_STUB_LOG"
`,
}

type natScriptRun struct {
	root, bin string
	log       string
	out       string
	err       error
}

func (r natScriptRun) calls() []string {
	b, _ := os.ReadFile(r.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (r natScriptRun) state(name string) string {
	b, _ := os.ReadFile(filepath.Join(r.root, name))
	return strings.TrimSpace(string(b))
}

// runNATSetup runs nat_setup.sh with the stubs. preinstalled makes
// iptables present from the start (an instance that is already set up).
func runNATSetup(t *testing.T, preinstalled bool, env ...string) natScriptRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs the setup script with bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	tmp := t.TempDir()
	r := natScriptRun{root: filepath.Join(tmp, "state"), bin: filepath.Join(tmp, "bin"), log: filepath.Join(tmp, "calls.log")}
	sys := filepath.Join(tmp, "sys")
	for _, d := range []string{r.root, r.bin, sys + "/etc/sysctl.d", sys + "/usr/local/sbin", sys + "/etc/systemd/system", sys + "/var/tmp"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range natStubs {
		if err := os.WriteFile(filepath.Join(r.bin, name), []byte(body), 0o755); err != nil { // #nosec G306 -- test stubs must be executable
			t.Fatal(err)
		}
	}
	if preinstalled {
		if err := os.WriteFile(filepath.Join(r.bin, "iptables"), []byte(natStubs[".iptables"]), 0o755); err != nil { // #nosec G306 -- test stub
			t.Fatal(err)
		}
	}

	script := natSetupScript
	for _, p := range []string{"/etc/sysctl.d/", "/usr/local/sbin/", "/etc/systemd/system/", "/var/tmp/"} {
		script = strings.ReplaceAll(script, p, sys+p)
	}
	path := filepath.Join(tmp, "nat_setup.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bash, path)
	cmd.Env = append([]string{
		"PATH=" + r.bin + ":/usr/bin:/bin",
		"NAKAMA_STUB_LOG=" + r.log,
		"NAKAMA_STUB_ROOT=" + r.root,
		"NAKAMA_STUB_BIN=" + r.bin,
		"NAKAMA_STUB_SETUP_BIN=" + sys + "/usr/local/sbin/nakama-nat-setup",
	}, env...)
	out, err := cmd.CombinedOutput()
	r.out, r.err = string(out), err
	return r
}

func indexOfCall(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestNATSetupFreshInstance(t *testing.T) {
	r := runNATSetup(t, false)
	if r.err != nil {
		t.Fatalf("setup failed: %v\n%s", r.err, r.out)
	}
	calls := r.calls()

	upgrade := indexOfCall(calls, "dnf -y --setopt=install_weak_deps=False --releasever=latest upgrade")
	install := indexOfCall(calls, "dnf -y --setopt=install_weak_deps=False install iptables-nft")
	swapOn := indexOfCall(calls, "swapon -p 10 ")
	swapOff := indexOfCall(calls, "swapoff ")
	switch {
	case upgrade < 0:
		t.Errorf("the security updates (dnf --releasever=latest upgrade) must run; calls:\n%s", strings.Join(calls, "\n"))
	case install < 0:
		t.Errorf("iptables-nft must be installed; calls:\n%s", strings.Join(calls, "\n"))
	case swapOn < 0 || swapOn > upgrade:
		t.Errorf("the disk swap must be on before dnf runs (swapon at %d, upgrade at %d)", swapOn, upgrade)
	case swapOff < install:
		t.Errorf("the disk swap must be removed after dnf (swapoff at %d, install at %d)", swapOff, install)
	}
	if got := r.state("installed"); got != "iptables-nft" {
		t.Errorf("installed packages = %q, want only iptables-nft (no iptables-services)", got)
	}
	if strings.Contains(strings.Join(calls, "\n"), "iptables-services") {
		t.Error("iptables-services must not be installed: the rules are applied by nakama-nat.service on boot")
	}
	if r.state("swaps") != "" {
		t.Errorf("the temporary swap is still on: %q", r.state("swaps"))
	}
	if r.state("ip_forward") != "1" {
		t.Error("ip_forward was not enabled")
	}
	if r.state("unit-nakama-nat") != "active" {
		t.Error("nakama-nat.service was not started")
	}
	rules := r.state("rules")
	for _, want := range []string{
		"-t nat -A POSTROUTING -o ens5 -j MASQUERADE",
		"-A FORWARD -i ens5 -j ACCEPT",
		"-A FORWARD -o ens5 -m state --state RELATED,ESTABLISHED -j ACCEPT",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rule %q missing; rules:\n%s", want, rules)
		}
	}
	if !strings.Contains(r.out, "nakama-nat: ready") {
		t.Errorf("output lacks the ready line:\n%s", r.out)
	}
}

func TestNATSetupIsIdempotent(t *testing.T) {
	r := runNATSetup(t, true)
	if r.err != nil {
		t.Fatalf("setup failed: %v\n%s", r.err, r.out)
	}
	calls := r.calls()
	if indexOfCall(calls, "dnf -y --setopt=install_weak_deps=False install") >= 0 {
		t.Error("iptables is present: nothing must be installed")
	}
	if indexOfCall(calls, "dnf -y --setopt=install_weak_deps=False --releasever=latest upgrade") < 0 {
		t.Error("the security updates must run on every setup, including a repair")
	}
	if n := strings.Count(r.state("rules"), "MASQUERADE"); n != 1 {
		t.Errorf("MASQUERADE rules = %d, want 1", n)
	}
}

func TestNATSetupRetriesDNF(t *testing.T) {
	r := runNATSetup(t, false, "NAKAMA_STUB_DNF_FAILS=2")
	if r.err != nil {
		t.Fatalf("two transient dnf failures must be retried: %v\n%s", r.err, r.out)
	}
	if !strings.Contains(r.out, "(attempt 2/3)") {
		t.Errorf("output lacks the retry notice:\n%s", r.out)
	}
}

// TestNATSetupFailsLoudly pins the second half of Issue #87: a dnf that
// keeps failing (OOM-killed, offline) must fail the setup, not leave an
// instance without forwarding behind a successful run.
func TestNATSetupFailsLoudly(t *testing.T) {
	r := runNATSetup(t, false, "NAKAMA_STUB_DNF_FAILS=99")
	if r.err == nil {
		t.Fatalf("the setup must fail when dnf keeps failing; output:\n%s", r.out)
	}
	if strings.Contains(r.out, "nakama-nat: ready") {
		t.Error("a failed setup must not report ready")
	}
	if r.state("swaps") != "" {
		t.Errorf("the temporary swap must be removed on failure too: %q", r.state("swaps"))
	}
}

// TestNATSetupFailsWhenUpdatesFail: the security updates are required.
// An instance that already has iptables must not report ready when
// `dnf --releasever=latest upgrade` keeps failing.
func TestNATSetupFailsWhenUpdatesFail(t *testing.T) {
	r := runNATSetup(t, true, "NAKAMA_STUB_DNF_FAILS=99")
	if r.err == nil || strings.Contains(r.out, "nakama-nat: ready") {
		t.Fatalf("the setup must fail when the security updates fail (err=%v):\n%s", r.err, r.out)
	}
}

// TestNATSetupVerifiesTheRules: the setup ends by checking the result
// rather than trusting the steps before it.
func TestNATSetupVerifiesTheRules(t *testing.T) {
	r := runNATSetup(t, false, "NAKAMA_STUB_DROP_NAT=1")
	if r.err == nil || !strings.Contains(r.out, "MASQUERADE rule is missing") {
		t.Fatalf("a missing MASQUERADE rule must fail the setup (err=%v):\n%s", r.err, r.out)
	}
}

func TestNATSetupScriptIsValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("bash not available")
	}
	for name, script := range map[string]string{
		"nat_setup.sh":   natSetupScript,
		"check script":   natCheckScript,
		"repair wrapper": natRepairScript(),
	} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: bash -n: %v\n%s", name, err, out)
		}
	}
}

// TestNATRepairScriptCarriesTheSetupVerbatim checks that the repair
// writes exactly natSetupScript: the here-document is quoted, so bash
// expands nothing in it, and its delimiter must not occur in the script.
func TestNATRepairScriptCarriesTheSetupVerbatim(t *testing.T) {
	if strings.Contains(natSetupScript, "\n"+natRepairScriptDelimiter+"\n") {
		t.Fatal("the setup script contains the here-document delimiter")
	}
	if runtime.GOOS == "windows" {
		t.Skip("runs bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	tmp := t.TempDir()
	stub := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	// systemctl: the repair unit is not running; systemd-run records
	// what it was asked to run.
	for name, body := range map[string]string{
		"systemctl":   "#!/bin/bash\n[ \"$1\" = is-active ] && exit 3\nexit 0\n",
		"systemd-run": "#!/bin/bash\necho \"$*\" > \"$NAKAMA_STUB_RUN\"\n",
	} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); err != nil { // #nosec G306 -- test stub
			t.Fatal(err)
		}
	}
	target := filepath.Join(tmp, "nakama-nat-setup.sh")
	script := strings.ReplaceAll(natRepairScript(), "/var/tmp/nakama-nat-setup.sh", target)
	cmd := exec.Command(bash, "-c", script)
	cmd.Env = []string{"PATH=" + stub + ":/usr/bin:/bin", "NAKAMA_STUB_RUN=" + filepath.Join(tmp, "run")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("repair wrapper: %v\n%s", err, out)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != natSetupScript {
		t.Error("the repair wrote a different script than natSetupScript")
	}
	run, _ := os.ReadFile(filepath.Join(tmp, "run"))
	if !strings.Contains(string(run), "--unit=nakama-nat-repair") || !strings.Contains(string(run), target) {
		t.Errorf("systemd-run %s", run)
	}
	if !strings.Contains(string(out), "NAKAMA_REPAIR=started") {
		t.Errorf("output: %s", out)
	}
}
