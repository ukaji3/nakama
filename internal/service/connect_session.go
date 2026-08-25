package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"

	"github.com/twinj/uuid"

	"github.com/aws/session-manager-plugin/src/datachannel"
	smplog "github.com/aws/session-manager-plugin/src/log"
	smpsession "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"

	// Blank imports register the Standard_Stream (shell) and Port
	// session handlers into the plugin's SessionRegistry via their
	// package init() functions.
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/portsession"
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/shellsession"

	"github.com/ukaji3/nakama/internal/awsx"
)

// DisconnectExitCode is the process exit code the session-runner child
// uses to signal a permanent disconnect (server-side session gone or
// websocket retries exhausted) to the Connect supervisor. The value is
// passed to the vendored plugin via SSM_PLUGIN_DISCONNECT_EXIT_CODE
// (see third_party/session-manager-plugin PINNED.md, "Local patches").
//
// 74 is chosen after BSD sysexits' EX_IOERR (input/output error): it
// cannot collide with the plugin's normal exit (0) or generic error
// exits (1/2).
const DisconnectExitCode = 74

// disconnectExitCodeEnv gates the vendored plugin patch; unset means
// upstream behavior (os.Exit(0) on permanent disconnect).
const disconnectExitCodeEnv = "SSM_PLUGIN_DISCONNECT_EXIT_CODE"

// sessionRunnerArgs is the argv (after the binary path) of the hidden
// child command that executes a single Session Manager session. It must
// match the command tree in internal/cli (aws_session_runner.go).
var sessionRunnerArgs = []string{"aws", "session-runner"}

// sessionRunnerPayload carries the StartSession result from the Connect
// supervisor (parent) to the session-runner child. It contains the
// session TokenValue, a secret, and is therefore passed over an
// inherited pipe on fd 3 — never via argv, the environment, or disk.
type sessionRunnerPayload struct {
	SessionID  string `json:"sessionId"`
	StreamURL  string `json:"streamUrl"`
	TokenValue string `json:"tokenValue"`
	Endpoint   string `json:"endpoint"`
	Region     string `json:"region"`
	TargetID   string `json:"targetId"`
}

// connectHooks abstracts the process-supervision side effects of
// Connect so the retry policy can be unit tested without spawning real
// processes or sleeping. defaultConnectHooks returns the production
// implementation.
type connectHooks struct {
	// runSession runs one session to completion and returns the child's
	// exit code. disconnectCode > 0 requests permanent-disconnect
	// detection with that exit code; 0 leaves the plugin's upstream
	// behavior untouched.
	runSession func(ctx context.Context, payload []byte, disconnectCode int) (int, error)
	// sleep waits for d or until ctx is cancelled.
	sleep func(ctx context.Context, d time.Duration) error
	// now is the clock used to measure session lifetimes.
	now func() time.Time
	// notify receives user-facing reconnection messages (stderr).
	notify io.Writer
	// stableAfter is the session lifetime after which the reconnect
	// budget is considered replenished (attempt counter reset).
	stableAfter time.Duration
}

func defaultConnectHooks() *connectHooks {
	return &connectHooks{
		runSession:  spawnSessionRunner,
		sleep:       sleepCtx,
		now:         time.Now,
		notify:      os.Stderr,
		stableAfter: 60 * time.Second,
	}
}

// spawnSessionRunner re-executes the current nakama binary as the
// hidden `nakama aws session-runner` command and waits for it. The
// session payload (which contains the secret TokenValue) is handed over
// on fd 3 via ExtraFiles; stdin/stdout/stderr are inherited so the
// child owns the terminal for the session's lifetime.
func spawnSessionRunner(ctx context.Context, payload []byte, disconnectCode int) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate nakama binary for session runner: %w", err)
	}

	// The payload (< 64 KiB, the pipe buffer size) is written before the
	// child starts, so the write cannot block or race a child crash.
	r, w, err := os.Pipe()
	if err != nil {
		return -1, fmt.Errorf("create session payload pipe: %w", err)
	}
	defer func() { _ = r.Close() }()
	if _, err := w.Write(payload); err != nil {
		_ = w.Close()
		return -1, fmt.Errorf("write session payload: %w", err)
	}
	if err := w.Close(); err != nil {
		return -1, fmt.Errorf("close session payload pipe: %w", err)
	}

	cmd := exec.Command(exe, sessionRunnerArgs...) // #nosec G204 -- argv is the re-exec'd nakama binary plus fixed args
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{r} // becomes fd 3 in the child
	env := os.Environ()
	if disconnectCode > 0 {
		env = append(env, fmt.Sprintf("%s=%d", disconnectExitCodeEnv, disconnectCode))
	}
	cmd.Env = env

	// While the child runs, the parent must survive terminal-generated
	// signals: in shell mode ^C (SIGINT) is forwarded to the remote by
	// the plugin in the child, and the supervisor dying first would
	// orphan the session. Dispositions are restored after the child
	// exits.
	signal.Ignore(os.Interrupt, syscall.SIGTSTP, syscall.SIGQUIT)
	defer signal.Reset(os.Interrupt, syscall.SIGTSTP, syscall.SIGQUIT)

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start session runner: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
		return -1, ctx.Err()
	case err := <-done:
		if err == nil {
			return 0, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, fmt.Errorf("wait for session runner: %w", err)
	}
}

// sleepCtx sleeps for d, returning early with ctx.Err() when the
// context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunSessionRunner is the child-process half of Connect: it decodes the
// session payload received from the supervisor (fd 3) and drives the
// vendored session-manager-plugin. On a normal session end the plugin
// terminates the process with os.Exit(0); with
// SSM_PLUGIN_DISCONNECT_EXIT_CODE set (by the supervisor) a permanent
// disconnect terminates it with that code instead. Credentials resolved
// by nakama (including the `aws login` fallback) are injected into the
// session so the plugin does not run its own credential chain.
func RunSessionRunner(ctx context.Context, clients *awsx.Clients, payloadJSON []byte) error {
	var p sessionRunnerPayload
	if err := json.Unmarshal(payloadJSON, &p); err != nil {
		return fmt.Errorf("decode session payload: %w", err)
	}
	creds, err := clients.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("resolve credentials: %w", err)
	}
	// Tell the vendored plugin to skip its own credential lookup in
	// OpenDataChannel and use the Signer/Credentials we set below.
	// Scope the variable to this call so it is not inherited by any
	// later child process (CWE-526).
	defer func() { _ = os.Unsetenv("SSM_PLUGIN_SKIP_CLIENT_CONFIGURE") }()
	_ = os.Setenv("SSM_PLUGIN_SKIP_CLIENT_CONFIGURE", "true")

	uuid.SwitchFormat(uuid.CleanHyphen)
	logger := smplog.Logger(false, "nakama-connect")
	sess := &smpsession.Session{
		SessionId:   p.SessionID,
		StreamUrl:   p.StreamURL,
		TokenValue:  p.TokenValue,
		Endpoint:    p.Endpoint,
		ClientId:    uuid.NewV4().String(),
		TargetId:    p.TargetID,
		Region:      p.Region,
		DataChannel: &datachannel.DataChannel{},
		Signer:      v4.NewSigner(),
		Credentials: creds,
	}
	return sess.Execute(logger)
}

// ExitCodeError propagates a child process exit code through the error
// chain so cmd/nakama can terminate with the same code.
type ExitCodeError struct {
	Code int
}

func (e *ExitCodeError) Error() string {
	return fmt.Sprintf("session runner exited with code %d", e.Code)
}

// startSessionErrorKind classifies ssm:StartSession failures for the
// Connect retry policy (ADR 0014).
type startSessionErrorKind int

const (
	// startSessionErrFatal aborts immediately with the original error.
	startSessionErrFatal startSessionErrorKind = iota
	// startSessionErrAuth aborts with an `aws login` advisory: retrying
	// cannot succeed until the user refreshes credentials.
	startSessionErrAuth
	// startSessionErrRetryable is worth retrying within the reconnect
	// budget (e.g. the SSM agent has not checked in yet after a
	// hibernate resume).
	startSessionErrRetryable
)

// classifyStartSessionError maps a StartSession error to the retry
// policy above using the smithy API error code.
func classifyStartSessionError(err error) startSessionErrorKind {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return startSessionErrFatal
	}
	switch apiErr.ErrorCode() {
	case "TargetNotConnected":
		// The instance is reachable in principle but its SSM agent is not
		// connected (yet) — typical right after a hibernate/stop resume.
		return startSessionErrRetryable
	case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId",
		"UnrecognizedClientException", "AccessDeniedException":
		// Expired or invalid credentials (the STS/SSM spellings differ),
		// or the caller lost ssm:StartSession permission mid-run.
		return startSessionErrAuth
	default:
		return startSessionErrFatal
	}
}
