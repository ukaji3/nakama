package service

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// ProvisionSpec declares what to run on a freshly created instance
// (Issue #54). The zero value provisions nothing, which keeps
// `nakama aws create` behaving exactly as before.
type ProvisionSpec struct {
	// SetupScript is a local path or an https URL to a shell script run
	// as ubuntu after the instance is up.
	SetupScript string
	// Dotfiles is an https/git URL cloned into ~/.dotfiles and applied
	// by convention (install.sh / setup.sh / bootstrap.sh, else
	// symlinked dotfiles).
	Dotfiles string
	// Preset names a bundled provisioning recipe (see provisionPresets).
	Preset string
}

// IsZero reports whether nothing was requested.
func (p ProvisionSpec) IsZero() bool {
	return p.SetupScript == "" && p.Dotfiles == "" && p.Preset == ""
}

// ProvisionResult reports the outcome of one provisioning run.
type ProvisionResult struct {
	Name string `json:"name"`
	// Steps are the steps that were requested, in execution order.
	Steps []string `json:"steps"`
	// Failed is true when the remote script reported a failing step.
	Failed bool `json:"failed,omitempty"`
	// Err carries the failure reason (transport or step failure).
	Err string `json:"error,omitempty"`
	// LogPath is where the instance keeps the provisioning log.
	LogPath string `json:"log_path"`
}

// ProvisionLogPath is where the remote script tees its output, so a
// failed run can be inspected after the fact.
const ProvisionLogPath = "/var/log/nakama-provision.log"

// provisionPresets are the bundled recipes. `kiro` is Issue #20
// generalized into a preset: kiro-cli plus Kiro Crew with its gateway
// service, installed as the ubuntu user (the installer targets
// ~/.local/bin, and `kirocrew service install` needs passwordless sudo,
// which the Ubuntu AMI provides).
var provisionPresets = map[string]string{
	"kiro": `echo "nakama: installing kiro-cli"
sudo -u ubuntu -H bash -lc 'curl -fsSL https://cli.kiro.dev/install | bash'
echo "nakama: installing Kiro Crew"
sudo -u ubuntu -H bash -lc 'export PATH="$HOME/.local/bin:$PATH"; kiro-cli crew --yes'
echo "nakama: installing the Kiro Crew gateway service"
sudo -u ubuntu -H bash -lc 'export PATH="$HOME/.local/bin:$PATH"; kiro-cli crew --yes service install'
`,
}

// ProvisionPresetNames lists the bundled presets (for CLI help and
// validation messages).
func ProvisionPresetNames() []string {
	names := make([]string, 0, len(provisionPresets))
	for n := range provisionPresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ValidateProvisionSpec checks the spec before any resource is created,
// so a typo in --preset or an unreadable --setup file fails fast instead
// of after a multi-minute create.
func ValidateProvisionSpec(p ProvisionSpec) error {
	if p.Preset != "" {
		if _, ok := provisionPresets[p.Preset]; !ok {
			return fmt.Errorf("unknown --preset %q: available presets: %s",
				p.Preset, strings.Join(ProvisionPresetNames(), ", "))
		}
	}
	if p.SetupScript != "" && !isHTTPSURL(p.SetupScript) {
		if _, err := os.Stat(p.SetupScript); err != nil {
			return fmt.Errorf("--setup %q is neither an https URL nor a readable file: %w", p.SetupScript, err)
		}
	}
	if p.SetupScript != "" && isHTTPURL(p.SetupScript) && !isHTTPSURL(p.SetupScript) {
		return fmt.Errorf("--setup %q must use https (plain http would let the network alter the script)", p.SetupScript)
	}
	if p.Dotfiles != "" && isHTTPURL(p.Dotfiles) && !isHTTPSURL(p.Dotfiles) {
		return fmt.Errorf("--dotfiles %q must use https (or an ssh-style git URL)", p.Dotfiles)
	}
	return nil
}

// isHTTPURL reports whether s parses as an http(s) URL.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// isHTTPSURL reports whether s parses as an https URL.
func isHTTPSURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https"
}

// Provision runs the requested provisioning steps on an existing
// instance over SSM Run Command. It is used by `nakama aws create`
// (after the instance is up) and by `nakama aws setup rerun`.
//
// Idempotency is the script author's responsibility: nakama runs the
// steps as given, in order, and reports which one failed.
func (s *Service) Provision(ctx context.Context, name string, p ProvisionSpec) (*ProvisionResult, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	if err := ValidateProvisionSpec(p); err != nil {
		return nil, err
	}
	res := &ProvisionResult{Name: name, Steps: provisionStepNames(p), LogPath: ProvisionLogPath}
	if p.IsZero() {
		return res, nil
	}
	instanceID, err := s.resolveInstanceIDForSSH(ctx, name)
	if err != nil {
		return nil, err
	}
	script, err := buildProvisionScript(p)
	if err != nil {
		return nil, err
	}
	stdout, err := s.runRemoteScript(ctx, instanceID, script,
		"nakama: provision (setup/dotfiles/preset)", 45*time.Minute)
	if err != nil {
		res.Failed = true
		res.Err = err.Error()
		return res, fmt.Errorf("provision %s: %w", name, err)
	}
	if step, ok := failedProvisionStep(stdout); ok {
		res.Failed = true
		res.Err = fmt.Sprintf("step %q failed", step)
		return res, fmt.Errorf("provision %s: step %q failed (see %s on the instance)", name, step, ProvisionLogPath)
	}
	return res, nil
}

// provisionStepNames lists the requested steps in execution order:
// preset first (base tooling), then dotfiles, then the user script,
// so a user script can build on both.
func provisionStepNames(p ProvisionSpec) []string {
	var steps []string
	if p.Preset != "" {
		steps = append(steps, "preset:"+p.Preset)
	}
	if p.Dotfiles != "" {
		steps = append(steps, "dotfiles")
	}
	if p.SetupScript != "" {
		steps = append(steps, "setup")
	}
	return steps
}

// buildProvisionScript renders the remote script. Every step is
// announced and its failure reported without aborting the whole run, so
// one broken step does not hide the others; the caller turns a reported
// failure into a warning (the instance itself stays usable).
func buildProvisionScript(p ProvisionSpec) (string, error) {
	var b strings.Builder
	// The SSM agent's aws:runShellScript writes the commands to
	// _script.sh and executes the FILE with `sh -c` (see
	// agent/plugins/runscript/runshellscript.go in amazon-ssm-agent), so
	// the shebang selects the interpreter and a script without one runs
	// under /bin/sh (dash on Ubuntu). This script needs bash: the
	// process substitution below is not POSIX (Issue #61).
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -u\n")
	b.WriteString("exec > >(tee -a " + ProvisionLogPath + ") 2>&1\n")
	b.WriteString(`echo "nakama: provisioning started at $(date -Is)"` + "\n")
	b.WriteString("nakama_step() {\n  echo \"NAKAMA_STEP_BEGIN $1\"\n}\n")
	b.WriteString("nakama_done() {\n  if [ \"$2\" -eq 0 ]; then echo \"NAKAMA_STEP_OK $1\"; else echo \"NAKAMA_STEP_FAILED $1 (exit $2)\"; fi\n}\n")

	if p.Preset != "" {
		body, ok := provisionPresets[p.Preset]
		if !ok {
			return "", fmt.Errorf("unknown preset %q", p.Preset)
		}
		b.WriteString(stepBlock("preset:"+p.Preset, body))
	}
	if p.Dotfiles != "" {
		b.WriteString(stepBlock("dotfiles", dotfilesBody(p.Dotfiles)))
	}
	if p.SetupScript != "" {
		body, err := setupBody(p.SetupScript)
		if err != nil {
			return "", err
		}
		b.WriteString(stepBlock("setup", body))
	}
	b.WriteString(`echo "nakama: provisioning finished at $(date -Is)"` + "\n")
	return b.String(), nil
}

// stepBlock wraps a step body with begin/result markers. The body runs
// in a subshell so `exit` inside it cannot end the whole run.
func stepBlock(name, body string) string {
	return fmt.Sprintf("nakama_step %q\n(\n%s\n)\nnakama_done %q $?\n", name, body, name)
}

// dotfilesBody clones the repository as ubuntu and applies it by
// convention: an install script if present, otherwise symlink the
// dot-prefixed files into $HOME.
func dotfilesBody(repo string) string {
	return fmt.Sprintf(`set -e
command -v git >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq git; }
sudo -u ubuntu -H bash -lc '
set -e
repo=%q
dir="$HOME/.dotfiles"
if [ -d "$dir/.git" ]; then
  git -C "$dir" pull --ff-only
else
  git clone --depth 1 "$repo" "$dir"
fi
cd "$dir"
for s in install.sh setup.sh bootstrap.sh; do
  if [ -f "$s" ]; then
    echo "nakama: running dotfiles/$s"
    chmod +x "$s"
    "./$s"
    exit 0
  fi
done
echo "nakama: no install script found; linking dotfiles"
for f in .[!.]*; do
  case "$f" in
    .git|.github) continue ;;
  esac
  ln -sfn "$dir/$f" "$HOME/$f"
done
'
`, repo)
}

// setupBody renders the user's setup step: fetch-and-run for an https
// URL, or the inlined local file (through a quoted heredoc, so nothing
// in the script is expanded by the outer shell).
func setupBody(src string) (string, error) {
	if isHTTPSURL(src) {
		// mktemp creates the file 0600 root-owned; without the chown the
		// script would be unreadable for the ubuntu user it runs as
		// (Issue #61).
		return fmt.Sprintf(`set -e
tmp=$(mktemp)
curl -fsSL %q -o "$tmp"
chmod 0700 "$tmp"
chown ubuntu:ubuntu "$tmp"
sudo -u ubuntu -H bash -lc "$tmp"
rm -f "$tmp"
`, src), nil
	}
	data, err := os.ReadFile(src) // #nosec G304 -- the path is the user's explicit --setup value
	if err != nil {
		return "", fmt.Errorf("read --setup script: %w", err)
	}
	body := string(data)
	if strings.Contains(body, "NAKAMA_SETUP_EOF") {
		return "", fmt.Errorf("the --setup script must not contain the marker NAKAMA_SETUP_EOF")
	}
	return fmt.Sprintf(`set -e
tmp=$(mktemp)
cat > "$tmp" <<'NAKAMA_SETUP_EOF'
%s
NAKAMA_SETUP_EOF
chmod 0700 "$tmp"
chown ubuntu:ubuntu "$tmp"
sudo -u ubuntu -H bash -lc "$tmp"
rm -f "$tmp"
`, body), nil
}

// failedProvisionStep reports the first failing step found in the
// remote output.
func failedProvisionStep(stdout string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "NAKAMA_STEP_FAILED "); ok {
			name := rest
			if i := strings.Index(rest, " (exit"); i > 0 {
				name = rest[:i]
			}
			return strings.Trim(name, `"`), true
		}
	}
	return "", false
}
