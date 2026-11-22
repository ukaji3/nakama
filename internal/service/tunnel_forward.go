package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/proxy"
)

// This file is the forward supervisor of the tunnel daemon (Issue #118,
// ADR 0027). It replaces the per-forward use of the connect supervisor
// (ADR 0014 / 0017), whose policy let a forward die for good on the
// disconnects Session Manager produces all the time:
//
//   - the daemon, not the plugin, owns the user's local port, so the
//     port stays open while a session is re-established and a
//     connection that arrives then waits for the new session instead of
//     being refused;
//   - a session is started when something needs it: right away while a
//     local connection was seen within forwardDemandWindow, otherwise
//     when the next one arrives. A session the server closed (Session
//     Manager's idle timeout) is therefore not reopened for nobody;
//   - every end of a session-runner child is a reason to reconnect,
//     and so are network errors, throttling and 5xx answers of
//     StartSession; credential errors are retried slowly, so a
//     `nakama aws login` brings the forward back by itself; there is no
//     attempt limit;
//   - forwards nakama opened for a browser are checked end to end with
//     an HTTP request through the session, which notices a session that
//     stopped answering without an error (Wi-Fi switch, sleep) within
//     about a minute.

// Timings of the forward supervisor. They are variables so that tests
// can shorten them; tests that do must not run in parallel.
var (
	// forwardRelayWait is how long a local connection waits for a
	// session when none is up.
	forwardRelayWait = 30 * time.Second
	// forwardEstablishTimeout bounds the time from StartSession until
	// the plugin listens on its private port.
	forwardEstablishTimeout = 30 * time.Second
	// forwardEstablishPoll is how often the private port is tried while
	// the session is being established.
	forwardEstablishPoll = 200 * time.Millisecond
	// forwardHealthInterval, forwardHealthTimeout and
	// forwardHealthFailures shape the HTTP health check: one request
	// every interval while local connections are open, each failing
	// after the timeout, and the session is replaced after that many
	// failures in a row.
	forwardHealthInterval = 15 * time.Second
	forwardHealthTimeout  = 10 * time.Second
	forwardHealthFailures = 3
	// forwardDemandWindow is how recently a local connection must have
	// been seen for a lost session to be re-established right away.
	forwardDemandWindow = 5 * time.Minute
	// forwardStableAfter is the session lifetime after which the
	// backoff starts over (autossh style, as in ADR 0014).
	forwardStableAfter = 60 * time.Second
	// forwardAuthRetry is the interval between attempts while the
	// credentials are rejected.
	forwardAuthRetry = 60 * time.Second
	// forwardDialTimeout bounds a connection to the plugin's private
	// port.
	forwardDialTimeout = 5 * time.Second
	// forwardTerminateTimeout bounds the best-effort TerminateSession of
	// a session the supervisor gave up on.
	forwardTerminateTimeout = 10 * time.Second
)

// errInstanceGone ends a forward whose instance no longer exists.
var errInstanceGone = errors.New("the instance no longer exists (no instance ID recorded for the name)")

// forwardDeps are the side effects of a forward supervisor. Production
// values come from Service.newForwardDeps; tests substitute fakes.
type forwardDeps struct {
	// resolveInstanceID reads the instance ID recorded for the name;
	// "" means the instance no longer exists.
	resolveInstanceID func(ctx context.Context) (string, error)
	// startSession calls ssm:StartSession.
	startSession func(ctx context.Context, in *ssm.StartSessionInput) (*ssm.StartSessionOutput, error)
	// terminateSession ends a session server-side, best effort.
	terminateSession func(sessionID string)
	// runChild runs one session-runner child until it exits or ctx is
	// cancelled (which kills it).
	runChild func(ctx context.Context, req sessionRunRequest) (sessionRunResult, error)
	// pickPort returns a free loopback port for the plugin to listen on.
	pickPort func() (string, error)
	// dial connects to the plugin's private port.
	dial func(ctx context.Context, addr string) (net.Conn, error)
	// healthCheck sends one HTTP request for path to addr with the given
	// Host header. Any HTTP answer is healthy.
	healthCheck func(ctx context.Context, addr, host, path string) error
	// reloadProxy re-reads the OS proxy settings before a reconnect.
	reloadProxy func()
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	// endpoint and region go into the session payload.
	endpoint string
	region   string
	logf     func(format string, args ...any)
}

// newForwardDeps returns the production dependencies for the forwards
// of one instance. childOut receives the session-runner children's
// stdout and stderr (the daemon log).
func (s *Service) newForwardDeps(name string, childOut io.Writer, logf func(string, ...any)) *forwardDeps {
	return &forwardDeps{
		resolveInstanceID: func(ctx context.Context) (string, error) {
			return s.ssmGetOptional(ctx, ssmInstanceIDPath(name))
		},
		startSession: func(ctx context.Context, in *ssm.StartSessionInput) (*ssm.StartSessionOutput, error) {
			return s.clients.SSM.StartSession(ctx, in)
		},
		terminateSession: func(sessionID string) {
			ctx, cancel := context.WithTimeout(context.Background(), forwardTerminateTimeout)
			defer cancel()
			if _, err := s.clients.SSM.TerminateSession(ctx, &ssm.TerminateSessionInput{
				SessionId: aws.String(sessionID),
			}); err != nil {
				logf("terminate session %s: %v", sessionID, err)
			}
		},
		runChild: func(ctx context.Context, req sessionRunRequest) (sessionRunResult, error) {
			return spawnSessionRunnerIO(ctx, req, daemonSessionRunnerIO(childOut))
		},
		pickPort:    pickLoopbackPort,
		dial:        dialForward,
		healthCheck: httpHealthCheck,
		reloadProxy: proxy.Reload,
		now:         time.Now,
		sleep:       sleepCtx,
		endpoint:    fmt.Sprintf("https://ssm.%s.amazonaws.com", s.clients.Region),
		region:      s.clients.Region,
		logf:        logf,
	}
}

// daemonSessionRunnerIO is how the tunnel daemon runs its
// session-runner children: no stdin, stdout and stderr into the daemon
// log, and no terminal signals to guard (the daemon has no terminal).
func daemonSessionRunnerIO(out io.Writer) sessionRunnerIO {
	return sessionRunnerIO{
		stdin:                nil,
		stdout:               out,
		stderr:               out,
		guardTerminalSignals: false,
	}
}

// pickLoopbackPort asks the kernel for a free port on 127.0.0.1. The
// plugin listens on "localhost:<port>", which Go binds to the first
// IPv4 address of localhost, i.e. 127.0.0.1. The port is free when this
// returns; the plugin binds it a few seconds later, after StartSession
// and the data channel handshake. Should another process take it in
// between, the plugin fails, the child exits and the next attempt picks
// another port.
func pickLoopbackPort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("pick a local port for the session: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return "", fmt.Errorf("pick a local port for the session: %w", err)
	}
	return fmt.Sprint(port), nil
}

// dialForward connects to the plugin's private port.
func dialForward(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: forwardDialTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// httpHealthCheck sends GET path to addr with the Host header a browser
// would send to the user's port, so the far end treats it like the
// browser's own requests (Kiro Crew checks Host). Any HTTP answer, even
// an error status, proves that the whole path answers.
func httpHealthCheck(ctx context.Context, addr, host, path string) error {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil, // loopback only; never through HTTP_PROXY
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialForward(ctx, addr)
			},
			DisableKeepAlives: true,
		},
		// The health endpoints answer directly; a redirect is an answer
		// too and must not be followed to some other host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if err := resp.Body.Close(); err != nil {
		return err
	}
	// The router's own error pages (no route yet, the forward did not
	// answer) are not answers of the far end.
	if reason := resp.Header.Get(routerErrorHeader); reason != "" {
		return fmt.Errorf("the nakama router answered %d: %s", resp.StatusCode, reason)
	}
	return nil
}

// LocalPortInUseError reports that the local port of a forward cannot
// be bound.
type LocalPortInUseError struct {
	Port string
	// Holder is the nakama instance whose tunnel forwards the port, when
	// that is what holds it.
	Holder string
	Err    error
}

func (e *LocalPortInUseError) Error() string {
	if e.Holder != "" {
		return fmt.Sprintf("local port %s is in use by the nakama tunnel of %s (see `nakama aws tunnel status %s`)",
			e.Port, e.Holder, e.Holder)
	}
	return fmt.Sprintf("local port %s is not available: %v", e.Port, e.Err)
}

// Unwrap returns the bind error.
func (e *LocalPortInUseError) Unwrap() error { return e.Err }

// listenLoopback binds the user's port on 127.0.0.1 and, where the host
// has IPv6 loopback, on ::1. Browsers resolve "localhost" to both and
// may try ::1 first, so another program on [::1]:<port> would receive
// the browser's traffic; a port taken on either address therefore
// counts as in use. A host without IPv6 loopback gets the IPv4 listener
// only.
func listenLoopback(port string) ([]net.Listener, error) {
	v4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, &LocalPortInUseError{Port: port, Err: err}
	}
	v6, err := net.Listen("tcp", net.JoinHostPort("::1", port))
	if err == nil {
		return []net.Listener{v4, v6}, nil
	}
	if ipv6LoopbackAvailable() {
		_ = v4.Close()
		return nil, &LocalPortInUseError{Port: port, Err: err}
	}
	return []net.Listener{v4}, nil
}

// ipv6LoopbackAvailable reports whether ::1 can be bound at all.
func ipv6LoopbackAvailable() bool {
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// forwardOutcomeKind classifies how one session attempt ended.
type forwardOutcomeKind int

const (
	// forwardRetry: reconnect with backoff (when something wants it).
	forwardRetry forwardOutcomeKind = iota
	// forwardAuth: the credentials were rejected; retry slowly.
	forwardAuth
	// forwardGone: the instance no longer exists; the forward ends.
	forwardGone
	// forwardFatal: an error that cannot go away by itself (e.g. an
	// invalid document); the forward ends.
	forwardFatal
)

// forwardOutcome is the result of one session attempt.
type forwardOutcome struct {
	kind forwardOutcomeKind
	err  error
	// code is the AWS error code, when there was one.
	code string
	// served is how long the session served the port (0 if it never
	// became ready).
	served time.Duration
}

// httpStatusCoder is implemented by the SDK's HTTP response errors.
type httpStatusCoder interface{ HTTPStatusCode() int }

// classifyForwardError maps an error of StartSession (or of the
// instance ID read) to the forward policy. Unlike connect's
// classification (ADR 0014), anything without an answer from AWS is
// retried: right after a laptop wakes up or the network changes, DNS
// and connections fail for a while, and that must not end a forward.
func classifyForwardError(err error) (forwardOutcomeKind, string) {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		// DNS, connect, TLS, timeouts, a proxy refusing: no answer from
		// AWS at all.
		return forwardRetry, ""
	}
	code := apiErr.ErrorCode()
	switch classifyStartSessionError(err) {
	case startSessionErrAuth:
		return forwardAuth, code
	case startSessionErrRetryable:
		return forwardRetry, code
	}
	switch code {
	case "ThrottlingException", "Throttling", "TooManyRequestsException", "RequestLimitExceeded",
		"InternalServerError", "InternalFailure", "ServiceUnavailable", "ServiceUnavailableException":
		return forwardRetry, code
	}
	var sc httpStatusCoder
	if errors.As(err, &sc) && (sc.HTTPStatusCode() >= 500 || sc.HTTPStatusCode() == http.StatusTooManyRequests) {
		return forwardRetry, code
	}
	return forwardFatal, code
}

// childExitReason explains why a session-runner child ended.
func childExitReason(code int) error {
	switch code {
	case 0:
		return errors.New("the session was closed on the server side (e.g. the Session Manager idle timeout)")
	case DisconnectExitCode:
		return errConnectionLost
	case -1:
		return errors.New("the session runner was killed")
	default:
		return fmt.Errorf("the session runner exited with code %d", code)
	}
}

// forwardReporter receives a forward's state changes (the daemon's
// tracker in production).
type forwardReporter interface {
	connecting()
	active(sessionID string)
	retrying(attempt int, delay time.Duration, reason error, code string)
	authFailing(reason error, code string)
	idle()
	dead(reason error)
}

// forwardRunner supervises one forward: it owns the user's local port,
// keeps (or lazily starts) an SSM port-forwarding session behind it and
// relays every local connection to the session's plugin.
type forwardRunner struct {
	spec   ForwardSpec
	deps   *forwardDeps
	report forwardReporter
	front  []net.Listener

	mu sync.Mutex
	// backend is the plugin's private address while a session serves
	// the port, "" otherwise; ready is closed when it becomes set.
	backend string
	ready   chan struct{}
	// gen counts sessions; a relayed connection belongs to the session
	// it was dialled in and is closed when that session ends.
	gen      uint64
	clients  int
	lastSeen time.Time
	conns    map[net.Conn]uint64 // relayed connections -> session generation (0: waiting)
	wake     chan struct{}
	// wg tracks the accept loops and the goroutines serving connections.
	wg sync.WaitGroup
}

func newForwardRunner(spec ForwardSpec, front []net.Listener, deps *forwardDeps, report forwardReporter) *forwardRunner {
	return &forwardRunner{
		spec:     spec,
		deps:     deps,
		report:   report,
		front:    front,
		ready:    make(chan struct{}),
		lastSeen: deps.now(), // the forward was just asked for: connect right away
		conns:    map[net.Conn]uint64{},
		wake:     make(chan struct{}, 1),
	}
}

// run supervises the forward until ctx is cancelled or the forward
// cannot continue (instance gone, fatal error). It closes the local
// listeners and every local connection, and waits for the goroutines
// serving them, before returning.
func (r *forwardRunner) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer func() {
		cancel()
		r.shutdown()
		r.wg.Wait()
	}()
	for _, l := range r.front {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.acceptLoop(ctx, l)
		}()
	}
	attempt := 0
	started := false
	for {
		if ctx.Err() != nil {
			return
		}
		if !r.wanted() {
			r.report.idle()
			r.deps.logf("idle: no local connection for %s; the next one starts a session", forwardDemandWindow)
			if !r.waitForDemand(ctx) {
				return
			}
			// A new demand starts a fresh round of attempts.
			attempt = 0
		}
		// The network may have changed since the last session (another
		// proxy, a VPN): read the OS proxy settings again.
		if started && r.deps.reloadProxy != nil {
			r.deps.reloadProxy()
		}
		started = true
		r.report.connecting()
		out := r.session(ctx)
		if ctx.Err() != nil {
			return
		}
		switch out.kind {
		case forwardGone, forwardFatal:
			r.deps.logf("giving up: %v", out.err)
			r.report.dead(out.err)
			return
		case forwardAuth:
			r.deps.logf("credentials rejected (%v); retrying in %s", out.err, forwardAuthRetry)
			r.report.authFailing(out.err, out.code)
			if err := r.deps.sleep(ctx, forwardAuthRetry); err != nil {
				return
			}
		default:
			if out.served >= forwardStableAfter {
				attempt = 0
			}
			attempt++
			delay := backoffDelay(attempt)
			r.deps.logf("session lost (%v); attempt %d in %s", out.err, attempt, delay)
			r.report.retrying(attempt, delay, out.err, out.code)
			if !r.wanted() {
				continue // nothing uses the forward: go idle instead of waiting
			}
			if err := r.deps.sleep(ctx, delay); err != nil {
				return
			}
		}
	}
}

// session runs one StartSession + session-runner child and returns how
// it ended.
func (r *forwardRunner) session(ctx context.Context) forwardOutcome {
	instanceID, err := r.deps.resolveInstanceID(ctx)
	if err != nil {
		kind, code := classifyForwardError(err)
		if kind == forwardFatal {
			kind = forwardRetry // a failed read of our own parameter is no reason to give up
		}
		return forwardOutcome{kind: kind, err: err, code: code}
	}
	if instanceID == "" {
		return forwardOutcome{kind: forwardGone, err: errInstanceGone}
	}
	port, err := r.deps.pickPort()
	if err != nil {
		return forwardOutcome{kind: forwardRetry, err: err}
	}
	private := r.spec
	private.LocalPort = port
	docName, params := portForwardDocument(private)
	out, err := r.deps.startSession(ctx, &ssm.StartSessionInput{
		Target:       aws.String(instanceID),
		DocumentName: docName,
		Parameters:   params,
	})
	if err != nil {
		kind, code := classifyForwardError(err)
		return forwardOutcome{kind: kind, err: fmt.Errorf("StartSession: %w", err), code: code}
	}
	sessionID := aws.ToString(out.SessionId)
	payload, err := json.Marshal(sessionRunnerPayload{
		SessionID:  sessionID,
		StreamURL:  aws.ToString(out.StreamUrl),
		TokenValue: aws.ToString(out.TokenValue),
		Endpoint:   r.deps.endpoint,
		Region:     r.deps.region,
		TargetID:   instanceID,
	})
	if err != nil {
		r.deps.terminateSession(sessionID)
		return forwardOutcome{kind: forwardFatal, err: fmt.Errorf("encode session payload: %w", err)}
	}

	childCtx, killChild := context.WithCancel(ctx)
	defer killChild()
	type childResult struct {
		res sessionRunResult
		err error
	}
	done := make(chan childResult, 1)
	go func() {
		// The disconnect exit code makes the plugin exit (instead of
		// hanging) when its own reconnect attempts are exhausted.
		res, err := r.deps.runChild(childCtx, sessionRunRequest{payload: payload, disconnectCode: DisconnectExitCode})
		done <- childResult{res, err}
	}()
	addr := net.JoinHostPort("localhost", port)

	// Establishment: the plugin listens on its private port once the
	// data channel handshake is done.
	deadline := r.deps.now().Add(forwardEstablishTimeout)
	for !r.backendAnswers(ctx, addr) {
		select {
		case cr := <-done:
			reason := childExitReason(cr.res.code)
			if cr.err != nil {
				reason = cr.err
			}
			r.deps.terminateSession(sessionID)
			return forwardOutcome{kind: forwardRetry, err: fmt.Errorf("the session ended before the forward was ready: %w", reason)}
		case <-ctx.Done():
			killChild()
			<-done
			return forwardOutcome{kind: forwardRetry, err: ctx.Err()}
		default:
		}
		if r.deps.now().After(deadline) {
			killChild()
			<-done
			r.deps.terminateSession(sessionID)
			return forwardOutcome{kind: forwardRetry,
				err: fmt.Errorf("the session was not ready within %s", forwardEstablishTimeout)}
		}
		if err := r.deps.sleep(ctx, forwardEstablishPoll); err != nil {
			killChild()
			<-done
			return forwardOutcome{kind: forwardRetry, err: err}
		}
	}

	readyAt := r.deps.now()
	r.setBackend(addr)
	r.report.active(sessionID)
	r.deps.logf("session %s serving localhost:%s", sessionID, r.spec.LocalPort)

	unhealthy := make(chan error, 1)
	watched := make(chan struct{})
	if path := r.spec.Purpose.healthPath(); path != "" {
		go func() {
			defer close(watched)
			r.watch(childCtx, addr, path, unhealthy)
		}()
	} else {
		close(watched)
	}
	var cr childResult
	var reason error
	select {
	case cr = <-done:
	case reason = <-unhealthy:
		killChild()
		cr = <-done
	}
	// The health check must not outlive the session it checks.
	killChild()
	<-watched
	served := r.deps.now().Sub(readyAt)
	r.clearBackend()
	if ctx.Err() != nil {
		return forwardOutcome{kind: forwardRetry, err: ctx.Err(), served: served}
	}
	switch {
	case reason != nil:
		r.deps.terminateSession(sessionID)
	case cr.err != nil:
		reason = cr.err
		r.deps.terminateSession(sessionID)
	default:
		reason = childExitReason(cr.res.code)
		if cr.res.code != 0 && cr.res.code != DisconnectExitCode {
			// The server side may still hold a session the child no
			// longer serves.
			r.deps.terminateSession(sessionID)
		}
	}
	return forwardOutcome{kind: forwardRetry, err: reason, served: served}
}

// backendAnswers reports whether the plugin accepts connections on addr.
func (r *forwardRunner) backendAnswers(ctx context.Context, addr string) bool {
	dctx, cancel := context.WithTimeout(ctx, forwardEstablishPoll*5)
	defer cancel()
	c, err := r.deps.dial(dctx, addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// watch runs the HTTP health check while local connections are open
// and reports when forwardHealthFailures checks in a row failed.
func (r *forwardRunner) watch(ctx context.Context, addr, path string, unhealthy chan<- error) {
	host := net.JoinHostPort("localhost", r.spec.LocalPort)
	failures := 0
	for {
		if err := r.deps.sleep(ctx, forwardHealthInterval); err != nil {
			return
		}
		if r.activeClients() == 0 {
			// Nothing to protect, and no traffic of our own that would
			// keep an unused session from the idle timeout.
			failures = 0
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, forwardHealthTimeout)
		err := r.deps.healthCheck(hctx, addr, host, path)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
			continue
		}
		failures++
		r.deps.logf("health check %d/%d failed: %v", failures, forwardHealthFailures, err)
		if failures >= forwardHealthFailures {
			unhealthy <- fmt.Errorf("the forward stopped answering (%d health checks in a row failed, last: %w)", failures, err)
			return
		}
	}
}

// acceptLoop accepts local connections on one listener.
func (r *forwardRunner) acceptLoop(ctx context.Context, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// e.g. too many open files: do not spin.
			r.deps.logf("accept on %s: %v", l.Addr(), err)
			if sleepCtx(ctx, 100*time.Millisecond) != nil {
				return
			}
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.serve(ctx, c)
		}()
	}
}

// serve relays one local connection through the current session,
// waiting up to forwardRelayWait for one.
func (r *forwardRunner) serve(ctx context.Context, c net.Conn) {
	r.clientOpened(c)
	defer r.clientClosed(c)
	for {
		addr, gen, ok := r.waitBackend(ctx, forwardRelayWait)
		if !ok {
			_ = c.Close()
			return
		}
		b, err := r.deps.dial(ctx, addr)
		if err != nil {
			// The session may have ended between the lookup and the
			// dial; wait for the next one if this one is gone.
			if r.currentGen() != gen {
				continue
			}
			_ = c.Close()
			return
		}
		if !r.attach(c, gen) {
			// The session ended while dialling.
			_ = b.Close()
			continue
		}
		relay(c, b)
		return
	}
}

// relay copies both ways until either side is done, then closes both.
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err != nil {
			// A broken side: tear both down so the other copy ends too.
			_ = a.Close()
			_ = b.Close()
			return
		}
		// Clean EOF: pass the half-close on.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

func (r *forwardRunner) clientOpened(c net.Conn) {
	r.mu.Lock()
	r.clients++
	r.lastSeen = r.deps.now()
	r.conns[c] = 0
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *forwardRunner) clientClosed(c net.Conn) {
	r.mu.Lock()
	r.clients--
	r.lastSeen = r.deps.now()
	delete(r.conns, c)
	r.mu.Unlock()
}

// attach records that c relays through session gen; false when that
// session has already ended.
func (r *forwardRunner) attach(c net.Conn, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gen != gen || r.backend == "" {
		return false
	}
	r.conns[c] = gen
	return true
}

func (r *forwardRunner) activeClients() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients
}

func (r *forwardRunner) currentGen() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

// wanted reports whether a session should be up: a local connection is
// open or was seen within forwardDemandWindow.
func (r *forwardRunner) wanted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients > 0 || r.deps.now().Sub(r.lastSeen) <= forwardDemandWindow
}

// waitForDemand blocks until a local connection arrives; false when ctx
// ends first.
func (r *forwardRunner) waitForDemand(ctx context.Context) bool {
	for !r.wanted() {
		select {
		case <-r.wake:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// waitBackend returns the current session's address, waiting up to
// wait for one.
func (r *forwardRunner) waitBackend(ctx context.Context, wait time.Duration) (string, uint64, bool) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		r.mu.Lock()
		addr, gen, ready := r.backend, r.gen, r.ready
		r.mu.Unlock()
		if addr != "" {
			return addr, gen, true
		}
		select {
		case <-ready:
		case <-t.C:
			return "", 0, false
		case <-ctx.Done():
			return "", 0, false
		}
	}
}

// setBackend publishes a ready session.
func (r *forwardRunner) setBackend(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.backend = addr
	close(r.ready)
}

// clearBackend withdraws the session and closes the connections that
// were relayed through it (they cannot outlive it); connections still
// waiting for a session are kept.
func (r *forwardRunner) clearBackend() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backend = ""
	r.ready = make(chan struct{})
	for c, gen := range r.conns {
		if gen == r.gen {
			_ = c.Close()
		}
	}
}

// shutdown closes the local listeners and every local connection.
func (r *forwardRunner) shutdown() {
	for _, l := range r.front {
		_ = l.Close()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.conns {
		_ = c.Close()
	}
}
