package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// ConnectInput selects the connect mode. The three modes are mutually
// exclusive: a non-empty Command runs a command; a non-empty LocalPort
// starts a port-forward; otherwise an interactive shell is opened.
type ConnectInput struct {
	Name       string
	Command    []string // empty => interactive shell
	TTY        bool     // run Command in an interactive PTY
	LocalPort  string   // port-forward: local listener port
	RemotePort string   // port-forward: target port
	RemoteHost string   // optional: forward through the instance to this host

	// MaxRetries is the maximum number of automatic reconnect attempts
	// after a permanent disconnect (server-side session gone, e.g. after
	// EC2 hibernate/stop, or websocket retries exhausted). 0 disables
	// reconnection (single run). See ADR 0014.
	MaxRetries int
}

// Connect opens an AWS Systems Manager Session Manager session to the
// nakama instance and supervises it (ADR 0014): the session itself is
// executed by a child re-exec of the nakama binary (hidden
// `nakama aws session-runner` command) because the vendored
// session-manager-plugin terminates the whole process with os.Exit on
// both normal and abnormal session end. The child is spawned with
// SSM_PLUGIN_DISCONNECT_EXIT_CODE so permanent disconnects surface as a
// dedicated exit code, which this supervisor turns into a fresh
// StartSession + child run with exponential backoff.
func (s *Service) Connect(ctx context.Context, in ConnectInput) error {
	if err := ValidateInstanceName(in.Name); err != nil {
		return err
	}
	if in.MaxRetries < 0 {
		return fmt.Errorf("invalid retry count %d: must be >= 0", in.MaxRetries)
	}
	instanceID := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if instanceID == "" {
		return fmt.Errorf("nakama instance %q not found", in.Name)
	}

	hooks := s.connectHooksOrDefault()
	docName, params := connectDocument(in)
	shellMode := len(in.Command) == 0 && in.LocalPort == ""

	// With reconnection disabled the child runs without the disconnect
	// exit-code gate, so its behavior (including exit 0 on a server-side
	// session timeout) is identical to the pre-ADR-0014 single run.
	disconnectCode := 0
	if in.MaxRetries > 0 {
		disconnectCode = DisconnectExitCode
	}

	// attempt counts reconnects used from the budget of in.MaxRetries.
	// It is reset after a session that stayed alive for at least
	// hooks.stableAfter, so long-lived tunnels never exhaust the budget
	// across days (autossh-style).
	attempt := 0
	for {
		out, err := s.clients.SSM.StartSession(ctx, &ssm.StartSessionInput{
			Target:       aws.String(instanceID),
			DocumentName: docName,
			Parameters:   params,
		})
		if err != nil {
			switch classifyStartSessionError(err) {
			case startSessionErrAuth:
				return fmt.Errorf("StartSession failed with an authentication error: %w\n"+
					"Your AWS credentials have likely expired; run `aws login` (or refresh "+
					"your credentials) and reconnect", err)
			case startSessionErrRetryable:
				if attempt >= in.MaxRetries {
					return fmt.Errorf("StartSession: %w", err)
				}
				attempt++
				_, _ = fmt.Fprintf(hooks.notify, "target not connected; retrying (attempt %d/%d)...\n",
					attempt, in.MaxRetries)
				if err := hooks.sleep(ctx, backoffDelay(attempt)); err != nil {
					return err
				}
				continue
			default:
				return fmt.Errorf("StartSession: %w", err)
			}
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

		startedAt := hooks.now()
		code, err := hooks.runSession(ctx, payload, disconnectCode)
		if err != nil {
			return err
		}
		alive := hooks.now().Sub(startedAt)

		switch {
		case code == 0:
			return nil
		case code == DisconnectExitCode && disconnectCode != 0:
			if alive >= hooks.stableAfter {
				attempt = 0
			}
			if attempt >= in.MaxRetries {
				return fmt.Errorf("connection lost and %d reconnect attempt(s) exhausted", in.MaxRetries)
			}
			attempt++
			_, _ = fmt.Fprintf(hooks.notify, "connection lost; reconnecting (attempt %d/%d)...\n",
				attempt, in.MaxRetries)
			if shellMode {
				_, _ = fmt.Fprintln(hooks.notify, "note: reconnecting opens a new session; previous shell state is lost")
			}
			if err := hooks.sleep(ctx, backoffDelay(attempt)); err != nil {
				return err
			}
			continue
		case code < 0:
			return errors.New("session runner terminated abnormally (killed by a signal)")
		default:
			// Propagate the child's exit code unchanged (see ExitCodeError
			// handling in cmd/nakama).
			return &ExitCodeError{Code: code}
		}
	}
}

// connectHooksOrDefault returns the injected supervision hooks or the
// production defaults (child re-exec, real clock, real sleep, stderr).
func (s *Service) connectHooksOrDefault() *connectHooks {
	if s.connect != nil {
		return s.connect
	}
	return defaultConnectHooks()
}

// backoffDelay returns the exponential backoff before reconnect attempt
// k (1-based): 1s, 2s, 4s, 8s, 16s, then capped at 30s. No jitter: a
// single client per session makes thundering herds a non-issue.
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 5 { // 2^(6-1) = 32 > 30s cap
		return 30 * time.Second
	}
	return time.Duration(1<<(attempt-1)) * time.Second
}

// connectUser is the OS user that connect sessions land in. Session
// Manager authenticates as ssm-user, which has passwordless sudo on
// managed nodes by default (the SSM agent writes
// "ssm-user ALL=(ALL) NOPASSWD:ALL" to /etc/sudoers.d/ssm-agent-users),
// so nakama switches to the Ubuntu default user that code-server runs
// as. See ADR 0009.
const connectUser = "ubuntu"

// connectDocument maps a ConnectInput to the SSM document name and
// parameters.
//
// Shell and command modes run as the ubuntu user rather than the
// ssm-user that Session Manager logs in as:
//   - The interactive shell uses AWS-StartInteractiveCommand running
//     "exec sudo su - ubuntu". exec replaces the ssm-user shell, so a
//     single "exit" ends the session (there is no parent shell to fall
//     back to).
//   - Commands run via "sudo su - ubuntu -c '<command>'"; the command
//     is single-quoted so the outer shell does not re-split or expand
//     it.
//
// Port-forwarding carries no user context and is unchanged.
func connectDocument(in ConnectInput) (*string, map[string][]string) {
	switch {
	case in.LocalPort != "":
		params := map[string][]string{
			"portNumber":      {in.RemotePort},
			"localPortNumber": {in.LocalPort},
		}
		if in.RemoteHost != "" {
			params["host"] = []string{in.RemoteHost}
			return aws.String("AWS-StartPortForwardingSessionToRemoteHost"), params
		}
		return aws.String("AWS-StartPortForwardingSession"), params
	case len(in.Command) > 0:
		doc := "AWS-StartNonInteractiveCommand"
		if in.TTY {
			doc = "AWS-StartInteractiveCommand"
		}
		command := fmt.Sprintf("sudo su - %s -c %s", connectUser, shellQuote(strings.Join(in.Command, " ")))
		return aws.String(doc), map[string][]string{"command": {command}}
	default:
		command := fmt.Sprintf("exec sudo su - %s", connectUser)
		return aws.String("AWS-StartInteractiveCommand"), map[string][]string{"command": {command}}
	}
}

// shellQuote returns s as a single POSIX shell word: wrapped in single
// quotes with embedded single quotes escaped as '\”. This makes s safe
// to splice into a command line without word-splitting or expansion.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
