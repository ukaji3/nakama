package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// This file implements the `nakama aws tunnel-daemon` process (ADR
// 0017): the middle layer of the three-tier model (CLI -> daemon ->
// session-runner children). One daemon owns all forwards of one
// instance name; each forward is a forwardRunner (tunnel_forward.go,
// ADR 0027) that owns the local port and supervises the SSM
// port-forwarding sessions behind it. Forwards can be added while the
// daemon runs (`nakama aws open` / `kirocrew`, Issue #118).

// forwardTracker holds the observable state of every forward. It is
// written by the forward runners (through forwardReport) and read by
// the control channel's /status handler.
type forwardTracker struct {
	mu sync.Mutex
	// name is the instance name, used to render copy-pastable recovery
	// commands in details.
	name     string
	now      func() time.Time
	statuses []ForwardStatus
	// sessionIDs remembers the most recent session of each forward for
	// the best-effort TerminateSession sweep on shutdown.
	sessionIDs []string
}

func newForwardTracker(name string, now func() time.Time) *forwardTracker {
	return &forwardTracker{name: name, now: now}
}

// add appends a forward in the connecting state and returns its index.
func (t *forwardTracker) add(f ForwardSpec) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.statuses = append(t.statuses, ForwardStatus{
		Spec:    f.String(),
		Purpose: f.Purpose,
		State:   ForwardStateConnecting,
		Since:   t.now(),
	})
	t.sessionIDs = append(t.sessionIDs, "")
	return len(t.statuses) - 1
}

// reset puts forward i back to the connecting state for a new runner
// (a dead forward revived by `open`).
func (t *forwardTracker) reset(i int, f ForwardSpec) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.statuses[i] = ForwardStatus{Spec: f.String(), Purpose: f.Purpose, State: ForwardStateConnecting, Since: t.now()}
	t.sessionIDs[i] = ""
}

// state returns the current state of forward i.
func (t *forwardTracker) state(i int) ForwardState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.statuses[i].State
}

// get returns a copy of forward i's status.
func (t *forwardTracker) get(i int) ForwardStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.statuses[i]
}

// snapshot returns a copy of all forward statuses.
func (t *forwardTracker) snapshot() []ForwardStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ForwardStatus, len(t.statuses))
	copy(out, t.statuses)
	return out
}

// activeSessionIDs returns the sessions worth terminating server-side
// on shutdown (best-effort; dead and idle forwards have no live
// session).
func (t *forwardTracker) activeSessionIDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for i, st := range t.statuses {
		if st.State == ForwardStateActive && t.sessionIDs[i] != "" {
			ids = append(ids, t.sessionIDs[i])
		}
	}
	return ids
}

// update applies fn to forward i under the lock.
func (t *forwardTracker) update(i int, fn func(st *ForwardStatus)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.statuses[i])
}

// forwardReport adapts the tracker to one forward's forwardReporter.
type forwardReport struct {
	t *forwardTracker
	i int
}

// connecting marks a session attempt. A reconnecting or auth-failing
// forward keeps its state and detail, so `tunnel status` still says why
// it is retrying while the attempt runs.
func (r forwardReport) connecting() {
	r.t.update(r.i, func(st *ForwardStatus) {
		if st.State != ForwardStateIdle && st.State != ForwardStateDead {
			return
		}
		st.State = ForwardStateConnecting
		st.Since = r.t.now()
		st.Detail = ""
	})
}

func (r forwardReport) active(sessionID string) {
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	st := &r.t.statuses[r.i]
	st.State = ForwardStateActive
	st.SessionID = sessionID
	st.Since = r.t.now()
	st.Detail = ""
	st.LastErrorCode = ""
	r.t.sessionIDs[r.i] = sessionID
}

func (r forwardReport) retrying(attempt int, delay time.Duration, reason error, code string) {
	r.t.update(r.i, func(st *ForwardStatus) {
		st.State = ForwardStateReconnecting
		st.SessionID = ""
		st.Since = r.t.now()
		st.Attempts = attempt
		st.LastErrorCode = code
		st.Detail = fmt.Sprintf("attempt %d (next in %s): %v", attempt, delay, reason)
	})
}

func (r forwardReport) authFailing(reason error, code string) {
	r.t.update(r.i, func(st *ForwardStatus) {
		st.State = ForwardStateReconnecting
		st.SessionID = ""
		st.Since = r.t.now()
		st.LastErrorCode = code
		st.Detail = fmt.Sprintf("credentials expired or invalid (%v); run `nakama aws login`: the forward retries every %s "+
			"and reconnects by itself once they work (or run `nakama aws tunnel restart %s`)", reason, forwardAuthRetry, r.t.name)
	})
}

func (r forwardReport) idle() {
	r.t.update(r.i, func(st *ForwardStatus) {
		st.State = ForwardStateIdle
		st.SessionID = ""
		st.Since = r.t.now()
		st.Attempts = 0
		st.Detail = "no session; the next local connection starts one"
	})
}

func (r forwardReport) dead(reason error) {
	r.t.update(r.i, func(st *ForwardStatus) {
		st.State = ForwardStateDead
		st.SessionID = ""
		st.Since = r.t.now()
		st.Detail = reason.Error()
	})
}

// TunnelDaemonInput parameterizes one daemon run.
type TunnelDaemonInput struct {
	Name     string
	Forwards []ForwardSpec
}

// errForwardConflict reports an add request for a local port the
// daemon already forwards.
var errForwardConflict = errors.New("this tunnel already forwards that local port")

// tunnelDaemon is the state of a running daemon.
type tunnelDaemon struct {
	name   string
	logger tunnelLogger
	// ctx is cancelled to stop every forward.
	ctx     context.Context
	tracker *forwardTracker
	wg      sync.WaitGroup

	mu       sync.Mutex
	closing  bool
	state    *TunnelState
	forwards []ForwardSpec // index-aligned with the tracker
	// newDeps builds a forward's dependencies (tests substitute fakes).
	newDeps func(f ForwardSpec, childOut *lineWriter, logf func(string, ...any)) *forwardDeps
	// listen binds a forward's local port.
	listen func(port string) ([]net.Listener, error)
}

// startForward binds f's local port and starts its runner. persist
// rewrites the state file (forwards added after startup).
func (d *tunnelDaemon) startForward(f ForwardSpec, persist bool) (ForwardStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return ForwardStatus{}, errors.New("the tunnel daemon is stopping")
	}
	slot := -1
	for i, existing := range d.forwards {
		if existing.LocalPort != f.LocalPort {
			continue
		}
		if d.tracker.state(i) != ForwardStateDead {
			return ForwardStatus{}, fmt.Errorf("%w (%s)", errForwardConflict, existing.Stored())
		}
		slot = i // a dead forward on the same port is replaced
	}
	listeners, err := d.listen(f.LocalPort)
	if err != nil {
		return ForwardStatus{}, err
	}
	logf := func(format string, args ...any) {
		d.logger.Infof("%s: forward %s: %s", d.name, f.Stored(), fmt.Sprintf(format, args...))
	}
	out := newLineWriter(func(line string) {
		d.logger.Infof("%s: forward %s: session-runner: %s", d.name, f.Stored(), line)
	})
	if slot < 0 {
		slot = d.tracker.add(f)
		d.forwards = append(d.forwards, f)
	} else {
		d.tracker.reset(slot, f)
		d.forwards[slot] = f
	}
	if persist {
		d.state.Forwards = storedForwards(d.forwards)
		if err := rewriteTunnelState(d.state); err != nil {
			d.logger.Errorf("%s: record forward %s: %v", d.name, f.Stored(), err)
		}
	}
	runner := newForwardRunner(f, listeners, d.newDeps(f, out, logf), forwardReport{t: d.tracker, i: slot})
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		logf("starting")
		runner.run(d.ctx)
		out.Flush()
		logf("stopped")
	}()
	return d.tracker.get(slot), nil
}

// markDead records a forward that could not even start (its local port
// was taken between the CLI's check and the daemon's bind).
func (d *tunnelDaemon) markDead(f ForwardSpec, reason error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.tracker.add(f)
	d.forwards = append(d.forwards, f)
	forwardReport{t: d.tracker, i: i}.dead(reason)
}

// status renders the daemon status.
func (d *tunnelDaemon) status() *TunnelStatus {
	return &TunnelStatus{
		Name:      d.state.Name,
		PID:       d.state.PID,
		StartedAt: d.state.StartedAt,
		Forwards:  d.tracker.snapshot(),
	}
}

// storedForwards renders forwards in their state-file form.
func storedForwards(forwards []ForwardSpec) []string {
	out := make([]string, len(forwards))
	for i, f := range forwards {
		out[i] = f.Stored()
	}
	return out
}

// RunTunnelDaemon is the entry point of the hidden
// `nakama aws tunnel-daemon` command. It supervises the forwards until
// a graceful stop arrives via the control channel (or SIGTERM/SIGINT on
// unix). It exits with an error before writing any state when
// preconditions fail (unresolvable instance, state file conflict);
// after that, individual forward failures never terminate the daemon:
// they surface as dead forwards in `tunnel status`.
func (s *Service) RunTunnelDaemon(ctx context.Context, in TunnelDaemonInput) error {
	logger := openTunnelLogger()
	defer func() { _ = logger.Close() }()

	if err := ValidateInstanceName(in.Name); err != nil {
		logger.Errorf("%s: %v", in.Name, err)
		return err
	}
	if len(in.Forwards) == 0 {
		err := errors.New("no forwards given")
		logger.Errorf("%s: %v", in.Name, err)
		return err
	}
	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if err != nil {
		logger.Errorf("%v", err)
		return err
	}
	if instanceID == "" {
		err := fmt.Errorf("nakama instance %q not found", in.Name)
		logger.Errorf("%v", err)
		return err
	}

	// Control channel first: its port goes into the state file.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logger.Errorf("%s: listen control channel: %v", in.Name, err)
		return fmt.Errorf("listen control channel: %w", err)
	}
	defer func() { _ = listener.Close() }()
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		logger.Errorf("%s: generate control token: %v", in.Name, err)
		return fmt.Errorf("generate control token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	state := &TunnelState{
		Name:        in.Name,
		PID:         os.Getpid(),
		ControlPort: listener.Addr().(*net.TCPAddr).Port,
		Token:       token,
		Forwards:    storedForwards(in.Forwards),
		StartedAt:   time.Now().UTC(),
	}
	if err := writeTunnelState(state); err != nil {
		logger.Errorf("%s: %v", in.Name, err)
		return err
	}
	defer func() { _ = removeTunnelStateOwned(in.Name, state.Token) }()

	fwdCtx, cancelForwards := context.WithCancel(ctx)
	defer cancelForwards()
	d := &tunnelDaemon{
		name:    in.Name,
		logger:  logger,
		ctx:     fwdCtx,
		tracker: newForwardTracker(in.Name, time.Now),
		state:   state,
		newDeps: func(_ ForwardSpec, childOut *lineWriter, logf func(string, ...any)) *forwardDeps {
			return s.newForwardDeps(in.Name, childOut, logf)
		},
		listen: listenLoopback,
	}
	for _, f := range in.Forwards {
		if _, err := d.startForward(f, false); err != nil {
			logger.Errorf("%s: forward %s: %v", in.Name, f.Stored(), err)
			d.markDead(f, err)
		}
	}

	// Graceful stop plumbing: control channel POST /stop, and (unix)
	// SIGTERM/SIGINT, both funnel into stopOnce.
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() { stopOnce.Do(func() { close(stopCh) }) }
	cleanupSignals := notifyTunnelStopSignals(requestStop)
	defer cleanupSignals()

	server := &http.Server{
		Handler: newTunnelControlHandler(token, tunnelControl{
			status: d.status,
			stop:   requestStop,
			add: func(f ForwardSpec) (ForwardStatus, error) {
				return d.startForward(f, true)
			},
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("%s: control channel: %v", in.Name, err)
			requestStop()
		}
	}()
	logger.Infof("%s: tunnel daemon started (pid %d, %d forward(s))", in.Name, state.PID, len(in.Forwards))

	<-stopCh
	logger.Infof("%s: stop requested; shutting down", in.Name)

	// Shutdown order: refuse new forwards, remember live sessions, stop
	// the runners (which kills the children and closes the local
	// ports), then best-effort close the server-side sessions, then stop
	// answering control requests.
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
	liveSessions := d.tracker.activeSessionIDs()
	cancelForwards()
	d.wg.Wait()
	termCtx, termCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer termCancel()
	for _, id := range liveSessions {
		if _, err := s.clients.SSM.TerminateSession(termCtx, &ssm.TerminateSessionInput{
			SessionId: aws.String(id),
		}); err != nil {
			logger.Errorf("%s: terminate session %s: %v", in.Name, id, err)
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	if err := removeTunnelStateOwned(in.Name, state.Token); err != nil {
		logger.Errorf("%s: %v", in.Name, err)
	}
	logger.Infof("%s: tunnel daemon stopped", in.Name)
	return nil
}
