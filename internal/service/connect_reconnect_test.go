package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakeConnectSSM answers the instance-id Parameter Store read and
// counts / scripts StartSession calls.
type fakeConnectSSM struct {
	awsx.SSMAPI

	instanceID string

	startCalls int
	// startErrs[i] is returned by the i-th StartSession call (nil or
	// beyond the slice => success).
	startErrs []error
}

func (f *fakeConnectSSM) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if f.instanceID == "" {
		// The real SDK returns the modeled exception; ssmGetOptional
		// distinguishes it from other failures.
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{
		Parameter: &ssmtypes.Parameter{Value: aws.String(f.instanceID)},
	}, nil
}

func (f *fakeConnectSSM) StartSession(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	i := f.startCalls
	f.startCalls++
	if i < len(f.startErrs) && f.startErrs[i] != nil {
		return nil, f.startErrs[i]
	}
	return &ssm.StartSessionOutput{
		SessionId:  aws.String("sess-1"),
		StreamUrl:  aws.String("wss://example"),
		TokenValue: aws.String("token-secret"),
	}, nil
}

// fakeClock is a manually advanced clock for lifetime measurement.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// connectTestEnv wires a Service with fake SSM and scripted session
// runs. Each sessionRun entry describes one child execution: its exit
// code and how long the session stayed alive.
type sessionRun struct {
	code  int
	alive time.Duration
}

type connectTestEnv struct {
	svc    *Service
	ssm    *fakeConnectSSM
	clock  *fakeClock
	stderr *bytes.Buffer
	sleeps []time.Duration
	runs   int
	// disconnectCodes records the disconnectCode passed to runSession.
	disconnectCodes []int
}

func newConnectTestEnv(t *testing.T, script []sessionRun, startErrs []error) *connectTestEnv {
	t.Helper()
	env := &connectTestEnv{
		ssm:    &fakeConnectSSM{instanceID: "i-0123456789abcdef0", startErrs: startErrs},
		clock:  &fakeClock{t: time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)},
		stderr: &bytes.Buffer{},
	}
	env.svc = NewService(&awsx.Clients{SSM: env.ssm, Region: "ap-northeast-1"})
	env.svc.connect = &connectHooks{
		runSession: func(_ context.Context, payload []byte, disconnectCode int) (int, error) {
			var p sessionRunnerPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				t.Fatalf("runSession payload is not valid JSON: %v", err)
			}
			if p.TokenValue != "token-secret" {
				t.Errorf("payload TokenValue = %q, want %q", p.TokenValue, "token-secret")
			}
			if env.runs >= len(script) {
				t.Fatalf("runSession called %d times; script has only %d entries", env.runs+1, len(script))
			}
			run := script[env.runs]
			env.runs++
			env.disconnectCodes = append(env.disconnectCodes, disconnectCode)
			env.clock.advance(run.alive)
			return run.code, nil
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			env.sleeps = append(env.sleeps, d)
			return nil
		},
		now:         env.clock.now,
		notify:      env.stderr,
		stableAfter: 60 * time.Second,
	}
	return env
}

func TestConnectExitZeroEndsAfterSingleRun(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{{code: 0, alive: time.Second}}, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 5})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if env.ssm.startCalls != 1 {
		t.Errorf("StartSession calls = %d, want 1", env.ssm.startCalls)
	}
	if env.runs != 1 {
		t.Errorf("runSession calls = %d, want 1", env.runs)
	}
	if len(env.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", env.sleeps)
	}
}

func TestConnectReconnectsOnDisconnectCode(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{
		{code: DisconnectExitCode, alive: time.Second},
		{code: DisconnectExitCode, alive: time.Second},
		{code: 0, alive: time.Second},
	}, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 5})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if env.ssm.startCalls != 3 {
		t.Errorf("StartSession calls = %d, want 3", env.ssm.startCalls)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second}
	if len(env.sleeps) != len(want) || env.sleeps[0] != want[0] || env.sleeps[1] != want[1] {
		t.Errorf("sleeps = %v, want %v", env.sleeps, want)
	}
	if !strings.Contains(env.stderr.String(), "connection lost; reconnecting (attempt 1/5)") {
		t.Errorf("stderr missing reconnect notice: %q", env.stderr.String())
	}
	// Shell mode: the "new session" note must be printed.
	if !strings.Contains(env.stderr.String(), "previous shell state is lost") {
		t.Errorf("stderr missing shell-state notice: %q", env.stderr.String())
	}
	// Reconnection enabled => disconnect exit code requested from child.
	for i, c := range env.disconnectCodes {
		if c != DisconnectExitCode {
			t.Errorf("disconnectCodes[%d] = %d, want %d", i, c, DisconnectExitCode)
		}
	}
}

func TestConnectBudgetExhaustedIsError(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{
		{code: DisconnectExitCode, alive: time.Second},
		{code: DisconnectExitCode, alive: time.Second},
		{code: DisconnectExitCode, alive: time.Second},
	}, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 2})
	if err == nil {
		t.Fatal("Connect returned nil, want budget-exhausted error")
	}
	if !strings.Contains(err.Error(), "reconnect attempt(s) exhausted") {
		t.Errorf("error = %q, want mention of exhausted attempts", err)
	}
	if env.ssm.startCalls != 3 { // initial + 2 reconnects
		t.Errorf("StartSession calls = %d, want 3", env.ssm.startCalls)
	}
}

func TestConnectAttemptCounterResetsAfterStableSession(t *testing.T) {
	// Budget of 1, but every disconnect happens after a long-lived
	// (>= stableAfter) session, so the counter keeps resetting and the
	// budget never exhausts (autossh-style).
	env := newConnectTestEnv(t, []sessionRun{
		{code: DisconnectExitCode, alive: 2 * time.Minute},
		{code: DisconnectExitCode, alive: 2 * time.Minute},
		{code: DisconnectExitCode, alive: 2 * time.Minute},
		{code: 0, alive: time.Second},
	}, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 1})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if env.ssm.startCalls != 4 {
		t.Errorf("StartSession calls = %d, want 4", env.ssm.startCalls)
	}
	// Every reconnect is attempt 1/1 after a reset.
	if got := strings.Count(env.stderr.String(), "(attempt 1/1)"); got != 3 {
		t.Errorf("attempt 1/1 notices = %d, want 3 (stderr: %q)", got, env.stderr.String())
	}
}

func TestConnectShortLivedSessionsConsumeBudget(t *testing.T) {
	// Contrast to the reset test: short-lived sessions must not reset
	// the counter, so budget 1 exhausts on the second disconnect.
	env := newConnectTestEnv(t, []sessionRun{
		{code: DisconnectExitCode, alive: time.Second},
		{code: DisconnectExitCode, alive: time.Second},
	}, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 1})
	if err == nil {
		t.Fatal("Connect returned nil, want budget-exhausted error")
	}
	if env.ssm.startCalls != 2 {
		t.Errorf("StartSession calls = %d, want 2", env.ssm.startCalls)
	}
}

func TestConnectAuthErrorAbortsWithLoginAdvice(t *testing.T) {
	authErr := &smithy.GenericAPIError{Code: "ExpiredTokenException", Message: "token expired"}
	env := newConnectTestEnv(t, nil, []error{authErr})
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 5})
	if err == nil {
		t.Fatal("Connect returned nil, want auth error")
	}
	if !strings.Contains(err.Error(), "aws login") {
		t.Errorf("error = %q, want `aws login` advice", err)
	}
	if env.runs != 0 {
		t.Errorf("runSession calls = %d, want 0", env.runs)
	}
	if len(env.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none (auth errors must not retry)", env.sleeps)
	}
}

func TestConnectTargetNotConnectedRetriesWithinBudget(t *testing.T) {
	tnc := &smithy.GenericAPIError{Code: "TargetNotConnected", Message: "agent not online"}
	env := newConnectTestEnv(t, []sessionRun{{code: 0, alive: time.Second}},
		[]error{tnc, tnc, nil})
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 5})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if env.ssm.startCalls != 3 {
		t.Errorf("StartSession calls = %d, want 3", env.ssm.startCalls)
	}
	if len(env.sleeps) != 2 {
		t.Errorf("sleeps = %v, want 2 backoffs", env.sleeps)
	}
}

func TestConnectTargetNotConnectedExhaustsBudget(t *testing.T) {
	tnc := &smithy.GenericAPIError{Code: "TargetNotConnected", Message: "agent not online"}
	env := newConnectTestEnv(t, nil, []error{tnc, tnc})
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 1})
	if err == nil {
		t.Fatal("Connect returned nil, want error")
	}
	if !strings.Contains(err.Error(), "StartSession") {
		t.Errorf("error = %q, want StartSession error", err)
	}
	if env.ssm.startCalls != 2 { // initial + 1 retry
		t.Errorf("StartSession calls = %d, want 2", env.ssm.startCalls)
	}
}

func TestConnectFatalStartSessionErrorAbortsImmediately(t *testing.T) {
	fatal := &smithy.GenericAPIError{Code: "InternalServerError", Message: "boom"}
	env := newConnectTestEnv(t, nil, []error{fatal})
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 5})
	if err == nil {
		t.Fatal("Connect returned nil, want error")
	}
	if env.ssm.startCalls != 1 {
		t.Errorf("StartSession calls = %d, want 1", env.ssm.startCalls)
	}
	if len(env.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", env.sleeps)
	}
}

func TestConnectContextCancelStopsBackoff(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{
		{code: DisconnectExitCode, alive: time.Second},
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the backoff sleep must abort at once
	err := env.svc.Connect(ctx, ConnectInput{Name: "dev01", MaxRetries: 5})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error = %v, want context.Canceled", err)
	}
	if env.ssm.startCalls != 1 {
		t.Errorf("StartSession calls = %d, want 1", env.ssm.startCalls)
	}
}

func TestConnectRetryDisabledRunsChildWithoutDisconnectDetection(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{{code: 0, alive: time.Second}}, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 0})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if env.runs != 1 {
		t.Fatalf("runSession calls = %d, want 1", env.runs)
	}
	// MaxRetries == 0 must not arm the disconnect exit-code gate: the
	// child behaves exactly like the pre-ADR-0014 single run.
	if env.disconnectCodes[0] != 0 {
		t.Errorf("disconnectCode = %d, want 0 (detection disabled)", env.disconnectCodes[0])
	}
}

func TestConnectOtherExitCodePropagates(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{{code: 3, alive: time.Second}}, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: 5})
	var ec *ExitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("Connect error = %v, want *ExitCodeError", err)
	}
	if ec.Code != 3 {
		t.Errorf("ExitCodeError.Code = %d, want 3", ec.Code)
	}
}

func TestConnectNegativeRetriesRejected(t *testing.T) {
	env := newConnectTestEnv(t, nil, nil)
	err := env.svc.Connect(context.Background(), ConnectInput{Name: "dev01", MaxRetries: -1})
	if err == nil || !strings.Contains(err.Error(), "must be >= 0") {
		t.Fatalf("Connect error = %v, want invalid retry count error", err)
	}
}

func TestClassifyStartSessionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want startSessionErrorKind
	}{
		{"nil-ish generic error", errors.New("dial tcp: timeout"), startSessionErrFatal},
		{"TargetNotConnected", &smithy.GenericAPIError{Code: "TargetNotConnected"}, startSessionErrRetryable},
		{"ExpiredToken", &smithy.GenericAPIError{Code: "ExpiredToken"}, startSessionErrAuth},
		{"ExpiredTokenException", &smithy.GenericAPIError{Code: "ExpiredTokenException"}, startSessionErrAuth},
		{"InvalidClientTokenId", &smithy.GenericAPIError{Code: "InvalidClientTokenId"}, startSessionErrAuth},
		{"UnrecognizedClientException", &smithy.GenericAPIError{Code: "UnrecognizedClientException"}, startSessionErrAuth},
		{"AccessDeniedException", &smithy.GenericAPIError{Code: "AccessDeniedException"}, startSessionErrAuth},
		{"other API error", &smithy.GenericAPIError{Code: "InternalServerError"}, startSessionErrFatal},
		{"wrapped API error", wrapAPIErr(&smithy.GenericAPIError{Code: "TargetNotConnected"}), startSessionErrRetryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyStartSessionError(tt.err); got != tt.want {
				t.Errorf("classifyStartSessionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// wrapAPIErr wraps err one level like the AWS SDK's OperationError does.
func wrapAPIErr(err error) error {
	return &wrappedErr{err}
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "operation error SSM: StartSession: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestBackoffDelay(t *testing.T) {
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		if got := backoffDelay(i + 1); got != w {
			t.Errorf("backoffDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// --- RetryChildFailures (tunnel forwards, Issue #45) ---

func TestSuperviseRetriesRunnerFailureWhenEnabled(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{
		{code: 1, alive: time.Second},
		{code: 1, alive: time.Second},
		{code: 0, alive: time.Second},
	}, nil)
	err := env.svc.superviseSessionWithHooks(context.Background(), superviseSessionInput{
		InstanceID:         "i-0123456789abcdef0",
		MaxRetries:         5,
		RetryChildFailures: true,
	}, env.svc.connect)
	if err != nil {
		t.Fatalf("supervise: %v", err)
	}
	if env.runs != 3 {
		t.Errorf("runSession calls = %d, want 3", env.runs)
	}
	want := []time.Duration{time.Second, 2 * time.Second}
	if len(env.sleeps) != len(want) || env.sleeps[0] != want[0] || env.sleeps[1] != want[1] {
		t.Errorf("backoff sleeps = %v, want %v", env.sleeps, want)
	}
	if !strings.Contains(env.stderr.String(), "session runner failed (exit code 1); reconnecting (attempt 1/5)") {
		t.Errorf("notify output %q lacks the runner-failure reconnect notice", env.stderr.String())
	}
}

func TestSuperviseRunnerFailureExhaustsBudget(t *testing.T) {
	env := newConnectTestEnv(t, []sessionRun{
		{code: 1, alive: time.Second},
		{code: 1, alive: time.Second},
		{code: 1, alive: time.Second},
	}, nil)
	err := env.svc.superviseSessionWithHooks(context.Background(), superviseSessionInput{
		InstanceID:         "i-0123456789abcdef0",
		MaxRetries:         2,
		RetryChildFailures: true,
	}, env.svc.connect)
	if err == nil {
		t.Fatal("want error after exhausting the reconnect budget")
	}
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 1 {
		t.Errorf("error %v does not carry ExitCodeError with code 1", err)
	}
	if !strings.Contains(err.Error(), "exited with code 1") || !strings.Contains(err.Error(), "2 reconnect attempt(s) exhausted") {
		t.Errorf("error %q does not state the exit code and budget exhaustion", err)
	}
	if env.runs != 3 {
		t.Errorf("runSession calls = %d, want 3", env.runs)
	}
}

func TestSuperviseRunnerFailureFatalWithoutFlag(t *testing.T) {
	// connect semantics (ADR 0014): the child's exit code is the remote
	// command's exit code and must propagate unchanged without retries.
	env := newConnectTestEnv(t, []sessionRun{{code: 2, alive: time.Second}}, nil)
	err := env.svc.superviseSessionWithHooks(context.Background(), superviseSessionInput{
		InstanceID: "i-0123456789abcdef0",
		MaxRetries: 5,
	}, env.svc.connect)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 2 {
		t.Fatalf("err = %v, want ExitCodeError{Code: 2}", err)
	}
	if env.runs != 1 {
		t.Errorf("runSession calls = %d, want 1 (no retry)", env.runs)
	}
	if len(env.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", env.sleeps)
	}
}

func TestSuperviseRunnerFailureStabilityReplenishesBudget(t *testing.T) {
	// A session that stayed alive >= stableAfter replenishes the budget
	// before its failure consumes an attempt (same policy as the
	// DisconnectExitCode path). Without replenishment MaxRetries=2 would
	// be exhausted at the third failure; with it the run succeeds.
	env := newConnectTestEnv(t, []sessionRun{
		{code: 1, alive: time.Second},
		{code: 1, alive: 2 * time.Minute},
		{code: 1, alive: time.Second},
		{code: 0, alive: time.Second},
	}, nil)
	err := env.svc.superviseSessionWithHooks(context.Background(), superviseSessionInput{
		InstanceID:         "i-0123456789abcdef0",
		MaxRetries:         2,
		RetryChildFailures: true,
	}, env.svc.connect)
	if err != nil {
		t.Fatalf("supervise: %v", err)
	}
	if env.runs != 4 {
		t.Errorf("runSession calls = %d, want 4", env.runs)
	}
}
