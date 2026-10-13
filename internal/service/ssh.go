package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
)

// sshDocumentName is the AWS-managed SSM document that connects the
// session's data channel to a TCP port on the instance itself and
// relays it over the client's stdin/stdout (the vendored plugin picks
// StandardStreamForwarding because no local port is requested). This is
// the same document the official `aws ssm start-session` ProxyCommand
// recipe uses. See ADR 0019.
const sshDocumentName = "AWS-StartSSHSession"

// defaultSSHPort is the sshd port on the instance. Session Manager
// reaches it from inside the instance, so no security-group ingress and
// no public IP are involved.
const defaultSSHPort = 22

// sshLoginUser is the account the ssh config snippet selects: the same
// user `nakama aws connect` lands on (ADR 0009).
const sshLoginUser = "ubuntu"

// SSHProxyInput selects the instance to relay to.
type SSHProxyInput struct {
	Name string
	Port int // 0 => defaultSSHPort
}

// SSHProxy relays this process's stdin/stdout to the instance's sshd
// over a Session Manager data channel, which is what an OpenSSH
// `ProxyCommand` needs (ADR 0019). It deliberately does NOT use the
// connect supervisor (ADR 0014): the ssh client owns the lifetime of a
// ProxyCommand process and performs its own reconnection, so a single
// session per process is correct. The session runs in-process rather
// than in a session-runner child because the relay must own this
// process's real stdin/stdout.
//
// The vendored plugin prints "Starting session with SessionId: ..." on
// stdout before the relay begins. SSH clients skip such lines while
// looking for the server's identification string (RFC 4253 §4.2), which
// is what makes the equivalent aws CLI recipe work as well.
func (s *Service) SSHProxy(ctx context.Context, in SSHProxyInput) error {
	if err := ValidateInstanceName(in.Name); err != nil {
		return err
	}
	port := in.Port
	if port == 0 {
		port = defaultSSHPort
	}
	instanceID, err := s.resolveInstanceIDForSSH(ctx, in.Name)
	if err != nil {
		return err
	}

	out, err := s.clients.SSM.StartSession(ctx, &ssm.StartSessionInput{
		Target:       aws.String(instanceID),
		DocumentName: aws.String(sshDocumentName),
		Parameters:   map[string][]string{"portNumber": {fmt.Sprint(port)}},
	})
	if err != nil {
		if classifyStartSessionError(err) == startSessionErrAuth {
			return fmt.Errorf("StartSession failed with an authentication error: %w\n"+
				"Your AWS credentials have likely expired; run `nakama aws login` (or `aws login`) and retry", err)
		}
		return withTargetNotConnectedHint(fmt.Errorf("StartSession: %w", err))
	}

	payload, err := json.Marshal(sessionRunnerPayload{
		SessionID:  aws.ToString(out.SessionId),
		StreamURL:  aws.ToString(out.StreamUrl),
		TokenValue: aws.ToString(out.TokenValue),
		Endpoint:   fmt.Sprintf("https://ssm.%s.amazonaws.com", s.clients.Region),
		Region:     s.clients.Region,
		TargetID:   instanceID,
	})
	if err != nil {
		return fmt.Errorf("encode session payload: %w", err)
	}
	// Without the disconnect gate a permanent disconnect (server-side
	// session gone, websocket retries exhausted) leaves the vendored
	// plugin hanging, and a hanging ProxyCommand stalls the ssh client
	// until its own keepalives fire. With the gate the process exits
	// promptly and ssh sees the connection drop (Issue #61). The env
	// mutation is confined to this disposable proxy process, the same
	// pattern as RunSessionRunner's dialer/config hooks.
	_ = os.Setenv(disconnectExitCodeEnv, fmt.Sprint(DisconnectExitCode))
	defer func() { _ = os.Unsetenv(disconnectExitCodeEnv) }()
	return s.runSSHSession(ctx, payload)
}

// runSSHSession executes the relay session in this process. Tests
// override it through Service.ssh.
func (s *Service) runSSHSession(ctx context.Context, payload []byte) error {
	if s.ssh != nil && s.ssh.runSession != nil {
		return s.ssh.runSession(ctx, payload)
	}
	return RunSessionRunner(ctx, s.clients, payload, nil)
}

// sshHooks are the injection points used by the ssh tests.
type sshHooks struct {
	runSession func(ctx context.Context, payload []byte) error
}

// SSHSetupInput parameterizes `nakama aws ssh setup`.
type SSHSetupInput struct {
	Name string

	// PublicKeyPath is the public key to authorize on the instance.
	// Empty selects the first of the conventional locations that
	// exists (see resolvePublicKey).
	PublicKeyPath string

	// SkipKey authorizes nothing and only renders the ssh config
	// snippet (for users who manage authorized_keys themselves).
	SkipKey bool

	// Port is the sshd port to proxy to (0 => defaultSSHPort).
	Port int

	// NakamaPath overrides the nakama executable path embedded in the
	// generated ProxyCommand. Empty resolves os.Executable().
	NakamaPath string
}

// SSHSetupResult reports what `ssh setup` did.
type SSHSetupResult struct {
	// Alias is the ssh Host alias in the snippet.
	Alias string
	// PublicKeyPath is the key that was authorized ("" when skipped).
	PublicKeyPath string
	// KeyAlreadyPresent is true when authorized_keys already had it.
	KeyAlreadyPresent bool
	// Snippet is the ssh config fragment to Include.
	Snippet string
	// SnippetPath is where the snippet was written (nakama's own
	// config dir; the user's ~/.ssh/config is never modified).
	SnippetPath string
	// IncludeLine is the line the user adds to ~/.ssh/config.
	IncludeLine string
}

// SSHSetup authorizes the caller's public key for the ubuntu user on
// the instance (idempotently, over SSM Run Command) and renders an ssh
// config snippet that routes `ssh <alias>` through
// `nakama aws ssh proxy`. Nothing outside nakama's own config directory
// is modified: the snippet is written to
// ~/.config/nakama/ssh/<name>.conf and the user includes it explicitly.
func (s *Service) SSHSetup(ctx context.Context, in SSHSetupInput) (*SSHSetupResult, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}
	port := in.Port
	if port == 0 {
		port = defaultSSHPort
	}
	instanceID, err := s.resolveInstanceIDForSSH(ctx, in.Name)
	if err != nil {
		return nil, err
	}

	res := &SSHSetupResult{Alias: sshHostAlias(in.Name)}
	if !in.SkipKey {
		keyPath, keyData, err := resolvePublicKey(in.PublicKeyPath)
		if err != nil {
			return nil, err
		}
		res.PublicKeyPath = keyPath
		stdout, err := s.runRemoteScript(ctx, instanceID, authorizeKeyScript(keyData),
			"nakama: authorize an ssh public key for ubuntu", 5*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("authorize public key on %s: %w", in.Name, err)
		}
		res.KeyAlreadyPresent = strings.Contains(stdout, "NAKAMA_KEY_ALREADY_PRESENT=1")
	}

	nakamaPath := in.NakamaPath
	if nakamaPath == "" {
		if nakamaPath, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("resolve the nakama executable path: %w", err)
		}
	}
	res.Snippet = sshConfigSnippet(in.Name, nakamaPath, port)
	if res.SnippetPath, err = writeSSHSnippet(in.Name, res.Snippet); err != nil {
		return nil, err
	}
	res.IncludeLine = "Include " + res.SnippetPath
	return res, nil
}

// resolveInstanceIDForSSH maps a nakama instance name to its EC2
// instance id, with the same "not found" wording as connect.
func (s *Service) resolveInstanceIDForSSH(ctx context.Context, name string) (string, error) {
	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(name))
	if err != nil {
		return "", err
	}
	if instanceID == "" {
		return "", fmt.Errorf("nakama instance %q not found", name)
	}
	return instanceID, nil
}

// sshHostAlias is the ssh Host alias for an instance.
func sshHostAlias(name string) string { return "nakama-" + name }

// sshConfigSnippet renders the ssh config fragment. Pure function:
// unit-tested on every OS.
func sshConfigSnippet(name, nakamaPath string, port int) string {
	proxy := quoteSSHCommandPath(nakamaPath)
	return fmt.Sprintf(`# Generated by `+"`nakama aws ssh setup %s`"+`. Include this file from
# ~/.ssh/config; nakama never edits ~/.ssh/config itself.
Host %s
    User %s
    HostName %s
    ProxyCommand %s aws ssh proxy %s --port %d
    # The proxy carries ssh over SSM Session Manager: no inbound port,
    # no public IP, and host keys are per-instance as usual.
    StrictHostKeyChecking accept-new
`, name, sshHostAlias(name), sshLoginUser, sshHostAlias(name), proxy, name, port)
}

// quoteSSHCommandPath quotes an executable path for use inside an ssh
// ProxyCommand (Windows paths routinely contain spaces).
func quoteSSHCommandPath(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

// conventionalPublicKeys are probed in order when no key is given.
var conventionalPublicKeys = []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"}

// resolvePublicKey returns the path and contents of the public key to
// authorize: the explicit path, or the first conventional key found.
func resolvePublicKey(explicit string) (string, string, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit) // #nosec G304 -- the path is the user's explicit --identity value
		if err != nil {
			return "", "", fmt.Errorf("read public key: %w", err)
		}
		key, err := validatePublicKey(string(data))
		return explicit, key, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve the home directory: %w", err)
	}
	for _, name := range conventionalPublicKeys {
		p := filepath.Join(home, ".ssh", name)
		data, err := os.ReadFile(p) // #nosec G304 -- conventional ssh key location under the user's home
		if err != nil {
			continue
		}
		key, err := validatePublicKey(string(data))
		if err != nil {
			return "", "", err
		}
		return p, key, nil
	}
	return "", "", fmt.Errorf("no ssh public key found in %s: generate one with `ssh-keygen -t ed25519` "+
		"or pass --identity <path to .pub>", filepath.Join(home, ".ssh"))
}

// validatePublicKey trims the key and rejects anything that is not a
// single-line OpenSSH public key (a private key here would be both a
// user error and a secret leak into authorized_keys).
func validatePublicKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("the public key file is empty")
	}
	if strings.Contains(key, "\n") {
		return "", fmt.Errorf("the key file has multiple lines: pass a single OpenSSH public key (.pub), not a private key")
	}
	if !strings.HasPrefix(key, "ssh-") && !strings.HasPrefix(key, "ecdsa-") && !strings.HasPrefix(key, "sk-") {
		return "", fmt.Errorf("%q does not look like an OpenSSH public key (expected a line starting with ssh-… or ecdsa-…)", firstField(key))
	}
	return key, nil
}

// firstField returns the first whitespace-separated field (the key type
// for a public key) so error messages never echo whole key material.
func firstField(s string) string {
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return s[:i]
	}
	return s
}

// authorizeKeyScript builds an idempotent shell script that appends the
// key to ubuntu's authorized_keys unless it is already there. The key
// is passed through a quoted heredoc so no shell expansion applies.
func authorizeKeyScript(publicKey string) string {
	return fmt.Sprintf(`set -eu
install -d -m 0700 -o ubuntu -g ubuntu /home/ubuntu/.ssh
touch /home/ubuntu/.ssh/authorized_keys
chown ubuntu:ubuntu /home/ubuntu/.ssh/authorized_keys
chmod 0600 /home/ubuntu/.ssh/authorized_keys
key=$(cat <<'NAKAMA_EOF'
%s
NAKAMA_EOF
)
if grep -qxF "$key" /home/ubuntu/.ssh/authorized_keys; then
  echo "NAKAMA_KEY_ALREADY_PRESENT=1"
else
  printf '%%s\n' "$key" >> /home/ubuntu/.ssh/authorized_keys
  echo "NAKAMA_KEY_ADDED=1"
fi
systemctl is-active --quiet ssh || systemctl start ssh || true
`, publicKey)
}

// runRemoteScript runs a shell script on the instance over SSM Run
// Command and returns its stdout. It retries while the SSM agent is not
// yet registered, like the update path does.
func (s *Service) runRemoteScript(ctx context.Context, instanceID, script, comment string, timeout time.Duration) (string, error) {
	var commandID string
	if err := awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     timeout,
		Description: fmt.Sprintf("ssm agent online on %s", instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.SSM.SendCommand(ctx, &ssm.SendCommandInput{
			DocumentName: aws.String("AWS-RunShellScript"),
			InstanceIds:  []string{instanceID},
			Parameters: map[string][]string{
				"commands":         {script},
				"executionTimeout": {"3600"},
			},
			TimeoutSeconds: aws.Int32(600),
			Comment:        aws.String(comment),
		})
		if err != nil {
			if awshelper.IsAPIError(err, "InvalidInstanceId") {
				return false, nil
			}
			return false, fmt.Errorf("SendCommand: %w", err)
		}
		if out.Command == nil || out.Command.CommandId == nil {
			return false, fmt.Errorf("SendCommand returned nil command id")
		}
		commandID = *out.Command.CommandId
		return true, nil
	}); err != nil {
		return "", err
	}
	return s.waitSSMCommandOutput(ctx, commandID, instanceID)
}

// writeSSHSnippet stores the snippet under nakama's own config dir
// (0700/0600) and returns its path. The user's ~/.ssh is untouched.
func writeSSHSnippet(name, snippet string) (string, error) {
	dir, err := nakamaConfigSubdir("ssh")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".conf")
	if err := os.WriteFile(path, []byte(snippet), 0o600); err != nil { // #nosec G306 -- 0600 is intended
		return "", fmt.Errorf("write the ssh config snippet: %w", err)
	}
	return path, nil
}

// nakamaConfigSubdirOverride redirects the config subdirectory in tests.
var nakamaConfigSubdirOverride string

// nakamaConfigSubdir creates and returns $XDG_CONFIG_HOME/nakama/<sub>
// (or ~/.config/nakama/<sub>), 0700 — the same convention as the tunnel
// state directory and internal/profilestore.
func nakamaConfigSubdir(sub string) (string, error) {
	base := nakamaConfigSubdirOverride
	if base == "" {
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			base = filepath.Join(x, "nakama")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home directory for the nakama config dir: %w", err)
			}
			base = filepath.Join(home, ".config", "nakama")
		}
	}
	dir := filepath.Join(base, sub)
	if err := os.MkdirAll(dir, 0o700); err != nil { // #nosec G703 -- derived from the user's own XDG_CONFIG_HOME/home
		return "", fmt.Errorf("create the nakama config dir: %w", err)
	}
	return dir, nil
}
