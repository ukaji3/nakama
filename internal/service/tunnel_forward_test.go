package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"
)

// These tests drive forwardRunner against a fake plugin that really
// listens on the private port the runner picked, so the relay, the
// establishment check and the HTTP health check run over real loopback
// sockets. Only StartSession, the instance-ID read and the child
// process are faked.

// fastForwardTimers shortens the supervisor timings for one test. Tests
// using it must not run in parallel.
func fastForwardTimers(t *testing.T) {
	t.Helper()
	saved := []struct {
		p *time.Duration
		v time.Duration
	}{
		{&forwardRelayWait, forwardRelayWait},
		{&forwardEstablishTimeout, forwardEstablishTimeout},
		{&forwardEstablishPoll, forwardEstablishPoll},
		{&forwardHealthInterval, forwardHealthInterval},
		{&forwardHealthTimeout, forwardHealthTimeout},
		{&forwardDemandWindow, forwardDemandWindow},
		{&forwardAuthRetry, forwardAuthRetry},
		{&forwardTerminateTimeout, forwardTerminateTimeout},
	}
	savedFailures := forwardHealthFailures
	forwardRelayWait = 3 * time.Second
	forwardEstablishTimeout = 2 * time.Second
	forwardEstablishPoll = 10 * time.Millisecond
	forwardHealthInterval = 40 * time.Millisecond
	forwardHealthTimeout = 100 * time.Millisecond
	forwardHealthFailures = 2
	forwardAuthRetry = 50 * time.Millisecond
	forwardTerminateTimeout = time.Second
	t.Cleanup(func() {
		for _, s := range saved {
			*s.p = s.v
		}
		forwardHealthFailures = savedFailures
	})
}

// childPlan scripts one fake session-runner child.
type childPlan struct {
	// listen serves HTTP on the private port; without it the child never
	// becomes ready.
	listen bool
	// hang accepts connections but never answers (a session that stopped
	// answering without an error).
	hang bool
	// exitAfter ends the child with code after the delay (0: run until
	// killed or until exit is closed).
	exitAfter time.Duration
	code      int
	// exit, when non-nil, ends the child with code once closed.
	exit chan struct{}
}

// fakeForwardEnv is a forwardRunner with fake AWS and children.
type fakeForwardEnv struct {
	t      *testing.T
	runner *forwardRunner
	front  string // the user's local address
	report *recordingReport
	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	instanceID  string
	resolveErrs []error
	startErrs   []error // per StartSession call; nil entries succeed
	starts      int
	ports       map[string]string // session ID -> private port
	plans       []childPlan       // per started child, last one repeats
	children    int
	payloads    []sessionRunnerPayload
	disconnect  []int
	terminated  []string
	reloads     int
	sleeps      []time.Duration
	healthCalls int
}

func newFakeForwardEnv(t *testing.T, purpose ForwardPurpose, plans ...childPlan) *fakeForwardEnv {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	env := &fakeForwardEnv{
		t:          t,
		front:      l.Addr().String(),
		report:     &recordingReport{},
		instanceID: "i-0123456789abcdef0",
		ports:      map[string]string{},
		plans:      plans,
		done:       make(chan struct{}),
	}
	deps := &forwardDeps{
		resolveInstanceID: env.resolve,
		startSession:      env.startSession,
		terminateSession: func(id string) {
			env.mu.Lock()
			env.terminated = append(env.terminated, id)
			env.mu.Unlock()
		},
		runChild:    env.runChild,
		pickPort:    pickLoopbackPort,
		dial:        dialForward,
		healthCheck: env.healthCheck,
		reloadProxy: func() { env.mu.Lock(); env.reloads++; env.mu.Unlock() },
		now:         time.Now,
		// Backoff and credential waits (>= 1s, or the shortened auth
		// retry) are recorded and cut short; everything else really
		// sleeps.
		sleep: func(ctx context.Context, d time.Duration) error {
			if d >= time.Second || d == forwardAuthRetry {
				env.mu.Lock()
				env.sleeps = append(env.sleeps, d)
				env.mu.Unlock()
				d = 5 * time.Millisecond
			}
			return sleepCtx(ctx, d)
		},
		endpoint: "https://ssm.ap-northeast-1.amazonaws.com",
		region:   "ap-northeast-1",
		logf:     func(format string, args ...any) { t.Logf("runner: "+format, args...) },
	}
	spec := ForwardSpec{LocalPort: port, RemotePort: "8080", Purpose: purpose}
	env.runner = newForwardRunner(spec, []net.Listener{l}, deps, env.report)
	return env
}

func (e *fakeForwardEnv) start() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() {
		e.runner.run(ctx)
		close(e.done)
	}()
	e.t.Cleanup(e.stop)
}

func (e *fakeForwardEnv) stop() {
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		e.t.Error("the runner did not stop within 5s")
	}
}

func (e *fakeForwardEnv) resolve(context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.resolveErrs) > 0 {
		err := e.resolveErrs[0]
		e.resolveErrs = e.resolveErrs[1:]
		return "", err
	}
	return e.instanceID, nil
}

func (e *fakeForwardEnv) startSession(_ context.Context, in *ssm.StartSessionInput) (*ssm.StartSessionOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	i := e.starts
	e.starts++
	if i < len(e.startErrs) && e.startErrs[i] != nil {
		return nil, e.startErrs[i]
	}
	if aws.ToString(in.DocumentName) != "AWS-StartPortForwardingSession" || in.Parameters["portNumber"][0] != "8080" {
		e.t.Errorf("StartSession input = %s %v", aws.ToString(in.DocumentName), in.Parameters)
	}
	id := fmt.Sprintf("sess-%d", i)
	e.ports[id] = in.Parameters["localPortNumber"][0]
	return &ssm.StartSessionOutput{SessionId: aws.String(id), StreamUrl: aws.String("wss://example"), TokenValue: aws.String("token")}, nil
}

func (e *fakeForwardEnv) runChild(ctx context.Context, req sessionRunRequest) (sessionRunResult, error) {
	var p sessionRunnerPayload
	if err := json.Unmarshal(req.payload, &p); err != nil {
		e.t.Errorf("payload: %v", err)
	}
	e.mu.Lock()
	e.payloads = append(e.payloads, p)
	e.disconnect = append(e.disconnect, req.disconnectCode)
	port := e.ports[p.SessionID]
	plan := e.plans[len(e.plans)-1]
	if e.children < len(e.plans) {
		plan = e.plans[e.children]
	}
	n := e.children
	e.children++
	e.mu.Unlock()

	var closeAll func()
	if plan.listen {
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			return sessionRunResult{code: 1}, nil
		}
		if plan.hang {
			var mu sync.Mutex
			var held []net.Conn
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					mu.Lock()
					held = append(held, c)
					mu.Unlock()
				}
			}()
			closeAll = func() {
				_ = l.Close()
				mu.Lock()
				for _, c := range held {
					_ = c.Close()
				}
				mu.Unlock()
			}
		} else {
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, "child-%d %s %s", n, r.Host, r.URL.Path)
			}), ReadHeaderTimeout: time.Second}
			go func() { _ = srv.Serve(l) }()
			closeAll = func() { _ = srv.Close() }
		}
	} else {
		closeAll = func() {}
	}
	defer closeAll()

	var timer <-chan time.Time
	if plan.exitAfter > 0 {
		timer = time.After(plan.exitAfter)
	}
	select {
	case <-ctx.Done():
		return sessionRunResult{code: -1}, ctx.Err()
	case <-timer:
		return sessionRunResult{code: plan.code}, nil
	case <-plan.exit:
		return sessionRunResult{code: plan.code}, nil
	}
}

func (e *fakeForwardEnv) healthCheck(ctx context.Context, addr, host, path string) error {
	e.mu.Lock()
	e.healthCalls++
	e.mu.Unlock()
	return httpHealthCheck(ctx, addr, host, path)
}

func (e *fakeForwardEnv) startCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts
}

// get fetches path through the user's port and returns the body.
func (e *fakeForwardEnv) get(path string) (string, error) {
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + e.front + path)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// waitFor polls cond for up to 5s.
func (e *fakeForwardEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s; reports: %v", what, e.report.events())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recordingReport records the state changes of one forward.
type recordingReport struct {
	mu  sync.Mutex
	evs []string
}

func (r *recordingReport) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, s)
}
func (r *recordingReport) connecting()       { r.add("connecting") }
func (r *recordingReport) active(id string)  { r.add("active " + id) }
func (r *recordingReport) idle()             { r.add("idle") }
func (r *recordingReport) dead(reason error) { r.add("dead " + reason.Error()) }
func (r *recordingReport) authFailing(reason error, code string) {
	r.add("auth " + code)
}
func (r *recordingReport) retrying(attempt int, delay time.Duration, reason error, code string) {
	r.add(fmt.Sprintf("retrying %d %s %s: %v", attempt, delay, code, reason))
}

func (r *recordingReport) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.evs...)
}

func (r *recordingReport) has(prefix string) bool {
	for _, e := range r.events() {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func TestForwardRelaysThroughTheSession(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone, childPlan{listen: true})
	env.start()
	body, err := env.get("/x")
	if err != nil {
		t.Fatalf("GET through the forward: %v", err)
	}
	// The far end sees the Host the browser sent (the user's port).
	if want := "child-0 " + env.front + " /x"; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if !env.report.has("active sess-0") {
		t.Errorf("reports = %v, want active sess-0", env.report.events())
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	// The plugin runs untouched (raw mode) and with the disconnect exit
	// code, so a session it gives up on ends the child instead of
	// hanging.
	if env.payloads[0].Mode != sessionModeRaw || env.disconnect[0] != DisconnectExitCode {
		t.Errorf("payload mode = %q, disconnect code = %d", env.payloads[0].Mode, env.disconnect[0])
	}
	if env.payloads[0].TokenValue != "token" || env.payloads[0].TargetID != "i-0123456789abcdef0" {
		t.Errorf("payload = %+v", env.payloads[0])
	}
	if env.ports["sess-0"] == strings.Split(env.front, ":")[1] {
		t.Error("the plugin must listen on a private port, not on the user's port")
	}
}

// TestForwardReconnectsAfterServerClose: the plugin exits 0 when the
// server closes the session (idle timeout); while the forward is in
// use the runner starts a new session, and the user's port never
// closes.
func TestForwardReconnectsAfterServerClose(t *testing.T) {
	fastForwardTimers(t)
	first := make(chan struct{})
	env := newFakeForwardEnv(t, ForwardPurposeNone,
		childPlan{listen: true, code: 0, exit: first},
		childPlan{listen: true})
	env.start()
	if _, err := env.get("/"); err != nil {
		t.Fatalf("first GET: %v", err)
	}
	close(first)
	env.waitFor("a second session", func() bool { return env.report.has("active sess-1") })
	body, err := env.get("/after")
	if err != nil || !strings.HasPrefix(body, "child-1 ") {
		t.Fatalf("GET after the reconnect = %q, %v", body, err)
	}
	if !env.report.has("retrying 1 1s : the session was closed on the server side") {
		t.Errorf("reports = %v, want a retry naming the server-side close", env.report.events())
	}
}

// TestForwardConnectionWaitsForTheSession: a connection that arrives
// while no session is up waits for one instead of being refused.
func TestForwardConnectionWaitsForTheSession(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone, childPlan{listen: true})
	// StartSession fails twice with a network error before it works.
	env.startErrs = []error{errors.New("dial tcp: lookup ssm.ap-northeast-1.amazonaws.com: no such host"), errors.New("dial tcp: i/o timeout")}
	env.start()
	body, err := env.get("/waited")
	if err != nil || !strings.HasPrefix(body, "child-0 ") {
		t.Fatalf("GET while reconnecting = %q, %v", body, err)
	}
	if env.startCount() != 3 {
		t.Errorf("StartSession calls = %d, want 3", env.startCount())
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.sleeps) != 2 || env.sleeps[0] != time.Second || env.sleeps[1] != 2*time.Second {
		t.Errorf("backoff = %v, want [1s 2s]", env.sleeps)
	}
	if env.reloads != 2 {
		t.Errorf("proxy reloads = %d, want one before each reconnect", env.reloads)
	}
}

// TestForwardRetriesTransientAWSErrors: throttling, 5xx and
// TargetNotConnected are retried; none ends the forward.
func TestForwardRetriesTransientAWSErrors(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone, childPlan{listen: true})
	env.startErrs = []error{
		&smithy.GenericAPIError{Code: "ThrottlingException"},
		&smithy.GenericAPIError{Code: "TargetNotConnected"},
		&smithy.GenericAPIError{Code: "InternalServerError"},
	}
	env.start()
	env.waitFor("the session", func() bool { return env.report.has("active sess-3") })
	if !env.report.has("retrying 2 2s TargetNotConnected") {
		t.Errorf("reports = %v, want the TargetNotConnected code reported", env.report.events())
	}
}

// TestForwardRecoversAfterCredentialFailure: a rejected credential is
// retried at the slow interval, and the forward comes back once it
// works (the user ran `nakama aws login`).
func TestForwardRecoversAfterCredentialFailure(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone, childPlan{listen: true})
	env.startErrs = []error{&smithy.GenericAPIError{Code: "ExpiredTokenException"}, &smithy.GenericAPIError{Code: "ExpiredTokenException"}}
	env.start()
	env.waitFor("the session", func() bool { return env.report.has("active sess-2") })
	if !env.report.has("auth ExpiredTokenException") {
		t.Errorf("reports = %v", env.report.events())
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	for _, d := range env.sleeps {
		if d != forwardAuthRetry {
			t.Errorf("credential failures must wait the auth interval, slept %v", env.sleeps)
		}
	}
}

func TestForwardEndsWhenTheInstanceIsGone(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone, childPlan{listen: true})
	env.instanceID = ""
	env.start()
	select {
	case <-env.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner must end when the instance is gone")
	}
	if !env.report.has("dead the instance no longer exists") {
		t.Errorf("reports = %v", env.report.events())
	}
	// The user's port is released.
	l, err := net.Listen("tcp", env.front)
	if err != nil {
		t.Errorf("the local port is still held after the forward died: %v", err)
	} else {
		_ = l.Close()
	}
}

func TestForwardEndsOnAFatalError(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone, childPlan{listen: true})
	env.startErrs = []error{&smithy.GenericAPIError{Code: "InvalidDocument", Message: "no such document"}}
	env.start()
	select {
	case <-env.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner must end on a fatal error")
	}
	if !env.report.has("dead StartSession:") {
		t.Errorf("reports = %v", env.report.events())
	}
}

// TestForwardReadsTheInstanceIDForEachSession: a failed read of the
// instance ID (network) is retried, not fatal.
func TestForwardReadsTheInstanceIDForEachSession(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone, childPlan{listen: true})
	env.resolveErrs = []error{errors.New("GetParameter: dial tcp: connection refused")}
	env.start()
	env.waitFor("the session", func() bool { return env.report.has("active sess-0") })
}

// TestForwardHealthCheckReplacesASilentSession: with a browser
// connection open, a session that stops answering is replaced after
// forwardHealthFailures failed checks, and the next session serves.
func TestForwardHealthCheckReplacesASilentSession(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeCodeServer,
		childPlan{listen: true, hang: true},
		childPlan{listen: true})
	env.start()
	env.waitFor("the first session", func() bool { return env.report.has("active sess-0") })
	// A browser connection that stays open (the hung session never
	// answers it).
	c, err := net.Dial("tcp", env.front)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	env.waitFor("the replacement session", func() bool { return env.report.has("active sess-1") })
	if !env.report.has("retrying 1 1s : the forward stopped answering") {
		t.Errorf("reports = %v, want the health check as the reason", env.report.events())
	}
	env.mu.Lock()
	terminated := append([]string(nil), env.terminated...)
	env.mu.Unlock()
	if len(terminated) == 0 || terminated[0] != "sess-0" {
		t.Errorf("terminated = %v, want the silent session closed server-side", terminated)
	}
	// The connection that was relayed through the dead session is
	// closed, so the browser reconnects instead of waiting forever.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("the connection of the replaced session is still open")
	}
	body, err := env.get("/healthz")
	if err != nil || !strings.HasPrefix(body, "child-1 ") {
		t.Fatalf("GET after the replacement = %q, %v", body, err)
	}
}

// TestForwardHealthCheckOnlyWithConnections: without a local
// connection no health check runs (no traffic of nakama's own keeps an
// unused session from the idle timeout), and plain -L forwards have
// none at all.
func TestForwardHealthCheckOnlyWithConnections(t *testing.T) {
	fastForwardTimers(t)
	for _, purpose := range []ForwardPurpose{ForwardPurposeCodeServer, ForwardPurposeNone} {
		env := newFakeForwardEnv(t, purpose, childPlan{listen: true})
		env.start()
		env.waitFor("the session", func() bool { return env.report.has("active sess-0") })
		time.Sleep(10 * forwardHealthInterval)
		env.mu.Lock()
		calls := env.healthCalls
		env.mu.Unlock()
		if calls != 0 {
			t.Errorf("%q: %d health checks without a connection", purpose, calls)
		}
		env.stop()
	}
}

// TestForwardGoesIdleAndWakesOnDemand: a session that ends while
// nothing used the forward recently is not re-established until a
// local connection arrives, which then waits for the new session.
func TestForwardGoesIdleAndWakesOnDemand(t *testing.T) {
	fastForwardTimers(t)
	forwardDemandWindow = 150 * time.Millisecond
	env := newFakeForwardEnv(t, ForwardPurposeNone,
		childPlan{listen: true, exitAfter: 400 * time.Millisecond, code: 0},
		childPlan{listen: true})
	env.start()
	env.waitFor("idle", func() bool { return env.report.has("idle") })
	time.Sleep(200 * time.Millisecond)
	if n := env.startCount(); n != 1 {
		t.Fatalf("StartSession calls while idle = %d, want 1", n)
	}
	body, err := env.get("/wake")
	if err != nil || !strings.HasPrefix(body, "child-1 ") {
		t.Fatalf("GET that wakes the forward = %q, %v", body, err)
	}
	if n := env.startCount(); n != 2 {
		t.Errorf("StartSession calls = %d, want 2", n)
	}
}

// TestForwardEstablishTimeout: a child that never listens is killed
// after forwardEstablishTimeout, its session terminated, and the next
// attempt serves.
func TestForwardEstablishTimeout(t *testing.T) {
	fastForwardTimers(t)
	forwardEstablishTimeout = 200 * time.Millisecond
	env := newFakeForwardEnv(t, ForwardPurposeNone,
		childPlan{listen: false},
		childPlan{listen: true})
	env.start()
	env.waitFor("the second session", func() bool { return env.report.has("active sess-1") })
	if !env.report.has("retrying 1 1s : the session was not ready within") {
		t.Errorf("reports = %v", env.report.events())
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.terminated) == 0 || env.terminated[0] != "sess-0" {
		t.Errorf("terminated = %v", env.terminated)
	}
}

// TestForwardChildFailureBeforeReady: a child that exits before it
// listens (websocket dial failed right after wake-up) is retried.
func TestForwardChildFailureBeforeReady(t *testing.T) {
	fastForwardTimers(t)
	env := newFakeForwardEnv(t, ForwardPurposeNone,
		childPlan{listen: false, exitAfter: 20 * time.Millisecond, code: 1},
		childPlan{listen: true})
	env.start()
	env.waitFor("the second session", func() bool { return env.report.has("active sess-1") })
	if !env.report.has("retrying 1 1s : the session ended before the forward was ready: the session runner exited with code 1") {
		t.Errorf("reports = %v", env.report.events())
	}
}

// TestForwardBackoffResetsAfterAStableSession: a session that served
// for forwardStableAfter starts the backoff over.
func TestForwardBackoffResetsAfterAStableSession(t *testing.T) {
	fastForwardTimers(t)
	saved := forwardStableAfter
	forwardStableAfter = 100 * time.Millisecond
	t.Cleanup(func() { forwardStableAfter = saved })
	env := newFakeForwardEnv(t, ForwardPurposeNone,
		childPlan{listen: true, exitAfter: 20 * time.Millisecond, code: DisconnectExitCode},
		childPlan{listen: true, exitAfter: 20 * time.Millisecond, code: DisconnectExitCode},
		childPlan{listen: true, exitAfter: 300 * time.Millisecond, code: DisconnectExitCode},
		childPlan{listen: true})
	env.start()
	env.waitFor("the fourth session", func() bool { return env.report.has("active sess-3") })
	env.mu.Lock()
	defer env.mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, time.Second}
	if len(env.sleeps) != len(want) {
		t.Fatalf("backoff = %v, want %v", env.sleeps, want)
	}
	for i := range want {
		if env.sleeps[i] != want[i] {
			t.Errorf("backoff = %v, want %v", env.sleeps, want)
		}
	}
}

func TestClassifyForwardError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind forwardOutcomeKind
		code string
	}{
		{"network", errors.New("dial tcp: i/o timeout"), forwardRetry, ""},
		{"TargetNotConnected", &smithy.GenericAPIError{Code: "TargetNotConnected"}, forwardRetry, "TargetNotConnected"},
		{"throttling", &smithy.GenericAPIError{Code: "ThrottlingException"}, forwardRetry, "ThrottlingException"},
		{"5xx code", &smithy.GenericAPIError{Code: "InternalServerError"}, forwardRetry, "InternalServerError"},
		{"5xx status", statusAPIError{code: "Odd", status: 503}, forwardRetry, "Odd"},
		{"429 status", statusAPIError{code: "Odd", status: 429}, forwardRetry, "Odd"},
		{"expired", &smithy.GenericAPIError{Code: "ExpiredTokenException"}, forwardAuth, "ExpiredTokenException"},
		{"denied", fmt.Errorf("wrapped: %w", &smithy.GenericAPIError{Code: "AccessDeniedException"}), forwardAuth, "AccessDeniedException"},
		{"invalid document", &smithy.GenericAPIError{Code: "InvalidDocument"}, forwardFatal, "InvalidDocument"},
		{"4xx status", statusAPIError{code: "Odd", status: 400}, forwardFatal, "Odd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, code := classifyForwardError(tc.err)
			if kind != tc.kind || code != tc.code {
				t.Errorf("classifyForwardError(%v) = %v %q, want %v %q", tc.err, kind, code, tc.kind, tc.code)
			}
		})
	}
}

// statusAPIError is an API error with an HTTP status, like the SDK's.
type statusAPIError struct {
	code   string
	status int
}

func (e statusAPIError) Error() string                 { return e.code }
func (e statusAPIError) ErrorCode() string             { return e.code }
func (e statusAPIError) ErrorMessage() string          { return e.code }
func (e statusAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }
func (e statusAPIError) HTTPStatusCode() int           { return e.status }

func TestChildExitReason(t *testing.T) {
	for code, want := range map[int]string{
		0:                  "closed on the server side",
		DisconnectExitCode: "connection lost",
		-1:                 "killed",
		3:                  "exited with code 3",
	} {
		if got := childExitReason(code).Error(); !strings.Contains(got, want) {
			t.Errorf("childExitReason(%d) = %q, want it to contain %q", code, got, want)
		}
	}
}

func TestStoredForwardSpec(t *testing.T) {
	f := ForwardSpec{LocalPort: "18080", RemotePort: "8080", Purpose: ForwardPurposeCodeServer}
	if f.String() != "18080:8080" || f.Stored() != "code-server=18080:8080" {
		t.Errorf("String = %q, Stored = %q", f.String(), f.Stored())
	}
	got, err := ParseStoredForwardSpec(f.Stored())
	if err != nil || got != f {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	// Plain forwards keep the -L form, so earlier state files read back.
	plain, err := ParseStoredForwardSpec("5432:db.internal:5432")
	if err != nil || plain.Purpose != ForwardPurposeNone || plain.RemoteHost != "db.internal" {
		t.Errorf("plain = %+v, %v", plain, err)
	}
	for _, bad := range []string{"bogus=8080:8080", "=8080:8080", "kirocrew=8080"} {
		if _, err := ParseStoredForwardSpec(bad); err == nil {
			t.Errorf("ParseStoredForwardSpec(%q) must fail", bad)
		}
	}
	// A user's -L cannot carry a purpose.
	if _, err := ParseForwardSpec("code-server=8080:8080"); err == nil {
		t.Error("ParseForwardSpec must reject a purpose prefix")
	}
	if _, err := ParseStoredForwardSpecs([]string{"kirocrew=5476:5476", "5476:5476"}); err == nil {
		t.Error("duplicate local ports must be rejected")
	}
	if p := ForwardPurposeKiroCrew.healthPath(); p != "/api/health" {
		t.Errorf("Kiro Crew health path = %q", p)
	}
	if p := ForwardPurposeCodeServer.healthPath(); p != "/healthz" {
		t.Errorf("code-server health path = %q", p)
	}
}

func TestListenLoopback(t *testing.T) {
	port, err := pickLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	ls, err := listenLoopback(port)
	if err != nil {
		t.Fatalf("listenLoopback(%s): %v", port, err)
	}
	defer func() {
		for _, l := range ls {
			_ = l.Close()
		}
	}()
	if ipv6LoopbackAvailable() && len(ls) != 2 {
		t.Errorf("listeners = %d, want 127.0.0.1 and ::1", len(ls))
	}
	_, err = listenLoopback(port)
	var inUse *LocalPortInUseError
	if !errors.As(err, &inUse) || inUse.Port != port {
		t.Fatalf("second bind = %v, want LocalPortInUseError", err)
	}
	if !strings.Contains(err.Error(), "local port "+port+" is not available") {
		t.Errorf("message = %q", err)
	}
	if probeLocalPort(port) == nil {
		t.Error("probeLocalPort must report the held port")
	}
}

// TestListenLoopbackRejectsAPortTakenOnIPv6: another program on
// [::1]:<port> would receive the browser's traffic for localhost, so
// the port counts as in use.
func TestListenLoopbackRejectsAPortTakenOnIPv6(t *testing.T) {
	if !ipv6LoopbackAvailable() {
		t.Skip("no IPv6 loopback")
	}
	l6, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l6.Close() }()
	port := fmt.Sprint(l6.Addr().(*net.TCPAddr).Port)
	if ls, err := listenLoopback(port); err == nil {
		for _, l := range ls {
			_ = l.Close()
		}
		t.Fatal("listenLoopback must fail when ::1 is taken")
	}
}

// TestDaemonSessionRunnerIO pins how the daemon runs its session
// runners: no stdin, output into the daemon log, no signal guard, and
// no console (Issue #126; the Windows effect is checked by
// TestDaemonSessionRunnerHasNoConsole).
func TestDaemonSessionRunnerIO(t *testing.T) {
	var out strings.Builder
	opts := daemonSessionRunnerIO(&out)
	if opts.stdin != nil || opts.stdout != &out || opts.stderr != &out || opts.guardTerminalSignals || !opts.noConsole {
		t.Errorf("daemon session runner options = %+v", opts)
	}
}
