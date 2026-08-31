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

// ConnectInput selects the connect mode. The two modes are mutually
// exclusive: a non-empty Command runs a command; otherwise an
// interactive shell is opened. Port forwarding lives in
// `nakama aws tunnel` (ADR 0017).
type ConnectInput struct {
	Name    string
	Command []string // empty => interactive shell
	TTY     bool     // run Command in an interactive PTY

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
// dedicated exit code, which the supervisor turns into a fresh
// StartSession + child run with exponential backoff.
func (s *Service) Connect(ctx context.Context, in ConnectInput) error {
	if err := ValidateInstanceName(in.Name); err != nil {
		return err
	}
	if in.MaxRetries < 0 {
		return fmt.Errorf("invalid retry count %d: must be >= 0", in.MaxRetries)
	}
	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if err != nil {
		return err
	}
	if instanceID == "" {
		return fmt.Errorf("nakama instance %q not found", in.Name)
	}

	docName, params := connectDocument(in)
	return s.superviseSession(ctx, superviseSessionInput{
		InstanceID:         instanceID,
		DocumentName:       docName,
		Parameters:         params,
		MaxRetries:         in.MaxRetries,
		ShellReconnectNote: len(in.Command) == 0,
	})
}

// sessionObserver receives lifecycle notifications from
// superviseSession. Implementations must be safe for use from the
// supervising goroutine only (no concurrency is introduced by the
// supervisor itself). The terminal outcome is superviseSession's
// return value, not an observer callback.
type sessionObserver interface {
	// SessionStarted fires after a successful StartSession, right
	// before the session-runner child is spawned.
	SessionStarted(sessionID string)
	// SessionRetrying fires when the supervisor decides to retry
	// (retryable StartSession error or permanent disconnect), before
	// the backoff sleep. attempt is 1-based and counts against
	// maxRetries.
	SessionRetrying(attempt, maxRetries int, reason error)
}

// superviseSessionInput parameterizes one supervised Session Manager
// session (ADR 0014). It is shared by `connect` (shell / command) and
// by each `tunnel` port forward (ADR 0017).
type superviseSessionInput struct {
	InstanceID   string
	DocumentName *string
	Parameters   map[string][]string

	// MaxRetries is the maximum number of automatic reconnect attempts
	// after a permanent disconnect. 0 disables reconnection.
	MaxRetries int

	// ShellReconnectNote prints the "new session, shell state lost"
	// advisory on reconnect (interactive shell mode only).
	ShellReconnectNote bool

	// RetryChildFailures also consumes reconnect attempts for generic
	// session-runner failures (any non-zero exit other than
	// DisconnectExitCode). Tunnel forwards enable this: a port-forward
	// child has no user shell whose exit code must be preserved, and a
	// failure to (re-)establish the data channel — e.g. the websocket
	// dial right after a laptop wake, before the network is back —
	// surfaces as a generic exit 1, not as the DisconnectExitCode gate
	// (which only fires after the channel was established). connect
	// keeps this off so the remote command's exit code propagates
	// unchanged (ADR 0014).
	RetryChildFailures bool

	// Observer, when non-nil, receives lifecycle notifications.
	Observer sessionObserver
}

// errConnectionLost is the reason passed to SessionRetrying when the
// session-runner child reports a permanent disconnect.
var errConnectionLost = errors.New("connection lost")

// superviseSession runs the StartSession → session-runner child loop
// with the ADR 0014 retry policy: a permanent disconnect (child exit
// DisconnectExitCode) or a retryable StartSession error consumes one
// attempt from the MaxRetries budget with exponential backoff; a
// session that stayed alive for at least hooks.stableAfter replenishes
// the budget (autossh-style); authentication errors abort immediately
// with an `aws login` advisory.
func (s *Service) superviseSession(ctx context.Context, in superviseSessionInput) error {
	return s.superviseSessionWithHooks(ctx, in, s.connectHooksOrDefault())
}

// superviseSessionWithHooks is superviseSession with explicit
// supervision hooks; the tunnel daemon injects hooks that wire child
// stdio into the OS log instead of a terminal (ADR 0017).
func (s *Service) superviseSessionWithHooks(ctx context.Context, in superviseSessionInput, hooks *connectHooks) error {
	if in.MaxRetries < 0 {
		return fmt.Errorf("invalid retry count %d: must be >= 0", in.MaxRetries)
	}

	// With reconnection disabled the child runs without the disconnect
	// exit-code gate, so its behavior (including exit 0 on a server-side
	// session timeout) is identical to the pre-ADR-0014 single run.
	disconnectCode := 0
	if in.MaxRetries > 0 {
		disconnectCode = DisconnectExitCode
	}

	// attempt counts reconnects used from the budget of in.MaxRetries.
	attempt := 0
	for {
		out, err := s.clients.SSM.StartSession(ctx, &ssm.StartSessionInput{
			Target:       aws.String(in.InstanceID),
			DocumentName: in.DocumentName,
			Parameters:   in.Parameters,
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
				if in.Observer != nil {
					in.Observer.SessionRetrying(attempt, in.MaxRetries, err)
				}
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
			TargetID:   in.InstanceID,
		})
		if err != nil {
			return fmt.Errorf("encode session payload: %w", err)
		}

		if in.Observer != nil {
			in.Observer.SessionStarted(aws.ToString(out.SessionId))
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
			if in.Observer != nil {
				in.Observer.SessionRetrying(attempt, in.MaxRetries, errConnectionLost)
			}
			_, _ = fmt.Fprintf(hooks.notify, "connection lost; reconnecting (attempt %d/%d)...\n",
				attempt, in.MaxRetries)
			if in.ShellReconnectNote {
				_, _ = fmt.Fprintln(hooks.notify, "note: reconnecting opens a new session; previous shell state is lost")
			}
			if err := hooks.sleep(ctx, backoffDelay(attempt)); err != nil {
				return err
			}
			continue
		case code < 0:
			return errors.New("session runner terminated abnormally (killed by a signal)")
		default:
			// Generic child failure. For connect, propagate the exit code
			// unchanged (see ExitCodeError handling in cmd/nakama): it is
			// the remote command's exit code. Tunnel forwards instead
			// consume a reconnect attempt (RetryChildFailures), because a
			// failed data-channel establishment exits with a generic code
			// and never reaches the DisconnectExitCode gate.
			exitErr := &ExitCodeError{Code: code}
			if !in.RetryChildFailures || disconnectCode == 0 {
				return exitErr
			}
			if alive >= hooks.stableAfter {
				attempt = 0
			}
			if attempt >= in.MaxRetries {
				return fmt.Errorf("%w and %d reconnect attempt(s) exhausted", exitErr, in.MaxRetries)
			}
			attempt++
			if in.Observer != nil {
				in.Observer.SessionRetrying(attempt, in.MaxRetries, exitErr)
			}
			_, _ = fmt.Fprintf(hooks.notify, "session runner failed (exit code %d); reconnecting (attempt %d/%d)...\n",
				code, attempt, in.MaxRetries)
			if err := hooks.sleep(ctx, backoffDelay(attempt)); err != nil {
				return err
			}
			continue
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
// Port forwarding is handled by `nakama aws tunnel`
// (portForwardDocument in tunnel.go, ADR 0017).
func connectDocument(in ConnectInput) (*string, map[string][]string) {
	if len(in.Command) > 0 {
		doc := "AWS-StartNonInteractiveCommand"
		if in.TTY {
			doc = "AWS-StartInteractiveCommand"
		}
		command := fmt.Sprintf("sudo su - %s -c %s", connectUser, shellQuote(strings.Join(in.Command, " ")))
		return aws.String(doc), map[string][]string{"command": {command}}
	}
	command := fmt.Sprintf("exec sudo su - %s", connectUser)
	return aws.String("AWS-StartInteractiveCommand"), map[string][]string{"command": {command}}
}

// shellQuote returns s as a single POSIX shell word: wrapped in single
// quotes with embedded single quotes escaped as '\”. This makes s safe
// to splice into a command line without word-splitting or expansion.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
