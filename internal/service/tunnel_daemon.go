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
// instance name; each forward is a supervised SSM port-forwarding
// session reusing the ADR 0014 loop (superviseSession).

// forwardTracker holds the observable state of every forward. It is
// written by the supervision goroutines (through forwardObserver) and
// read by the control channel's /status handler.
type forwardTracker struct {
	mu       sync.Mutex
	now      func() time.Time
	statuses []ForwardStatus
	// sessionIDs remembers the most recent session of each forward for
	// the best-effort TerminateSession sweep on shutdown.
	sessionIDs []string
}

func newForwardTracker(forwards []ForwardSpec, now func() time.Time) *forwardTracker {
	t := &forwardTracker{now: now, sessionIDs: make([]string, len(forwards))}
	t.statuses = make([]ForwardStatus, len(forwards))
	for i, f := range forwards {
		t.statuses[i] = ForwardStatus{
			Spec:  f.String(),
			State: ForwardStateConnecting,
			Since: now(),
		}
	}
	return t
}

// observer returns the sessionObserver for forward i.
func (t *forwardTracker) observer(i int) sessionObserver {
	return &forwardObserver{t: t, i: i}
}

// setDead marks forward i terminally dead. reason may be nil (normal
// session end, e.g. server-side timeout without retry budget).
func (t *forwardTracker) setDead(i int, reason error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := &t.statuses[i]
	st.State = ForwardStateDead
	st.Since = t.now()
	st.SessionID = ""
	switch {
	case reason == nil:
		st.Detail = "session ended"
	case classifyStartSessionError(reason) == startSessionErrAuth:
		st.Detail = "credentials expired or invalid; run `nakama aws login` and start the tunnel again"
	default:
		st.Detail = reason.Error()
	}
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
// on shutdown (best-effort; dead forwards have no live session).
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

// forwardObserver adapts tracker updates to the sessionObserver
// callbacks of one forward's supervision loop.
type forwardObserver struct {
	t *forwardTracker
	i int
}

// SessionStarted marks the forward active with its new session.
func (o *forwardObserver) SessionStarted(sessionID string) {
	o.t.mu.Lock()
	defer o.t.mu.Unlock()
	st := &o.t.statuses[o.i]
	st.State = ForwardStateActive
	st.SessionID = sessionID
	st.Since = o.t.now()
	st.Detail = ""
	o.t.sessionIDs[o.i] = sessionID
}

// SessionRetrying marks the forward reconnecting with the attempt info.
func (o *forwardObserver) SessionRetrying(attempt, maxRetries int, reason error) {
	o.t.mu.Lock()
	defer o.t.mu.Unlock()
	st := &o.t.statuses[o.i]
	st.State = ForwardStateReconnecting
	st.SessionID = ""
	st.Since = o.t.now()
	st.Attempts = attempt
	st.Detail = fmt.Sprintf("attempt %d/%d: %v", attempt, maxRetries, reason)
}

// TunnelDaemonInput parameterizes one daemon run.
type TunnelDaemonInput struct {
	Name     string
	Forwards []ForwardSpec
}

// RunTunnelDaemon is the entry point of the hidden
// `nakama aws tunnel-daemon` command. It supervises one SSM
// port-forwarding session per forward until a graceful stop arrives
// via the control channel (or SIGTERM/SIGINT on unix). It exits with
// an error before writing any state when preconditions fail
// (unresolvable instance, state file conflict); after that, individual
// forward failures never terminate the daemon — they surface as dead
// forwards in `tunnel status`.
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

	specs := make([]string, len(in.Forwards))
	for i, f := range in.Forwards {
		specs[i] = f.String()
	}
	state := &TunnelState{
		Name:        in.Name,
		PID:         os.Getpid(),
		ControlPort: listener.Addr().(*net.TCPAddr).Port,
		Token:       token,
		Forwards:    specs,
		StartedAt:   time.Now().UTC(),
	}
	if err := writeTunnelState(state); err != nil {
		logger.Errorf("%s: %v", in.Name, err)
		return err
	}
	defer func() { _ = removeTunnelState(in.Name) }()

	tracker := newForwardTracker(in.Forwards, time.Now)

	// Graceful stop plumbing: control channel POST /stop, and (unix)
	// SIGTERM/SIGINT, both funnel into stopOnce.
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() { stopOnce.Do(func() { close(stopCh) }) }
	cleanupSignals := notifyTunnelStopSignals(requestStop)
	defer cleanupSignals()

	server := &http.Server{
		Handler: newTunnelControlHandler(token, func() *TunnelStatus {
			return &TunnelStatus{
				Name:      state.Name,
				PID:       state.PID,
				StartedAt: state.StartedAt,
				Forwards:  tracker.snapshot(),
			}
		}, requestStop),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("%s: control channel: %v", in.Name, err)
			requestStop()
		}
	}()

	// One supervision goroutine per forward. The daemon deliberately
	// outlives dead forwards so `tunnel status` can show why they died
	// (e.g. expired credentials).
	fwdCtx, cancelForwards := context.WithCancel(ctx)
	defer cancelForwards()
	childOut := newLineWriter(func(line string) { logger.Infof("%s: session-runner: %s", in.Name, line) })
	hooks := s.tunnelDaemonConnectHooks(childOut)
	var wg sync.WaitGroup
	for i, f := range in.Forwards {
		wg.Add(1)
		go func(i int, f ForwardSpec) {
			defer wg.Done()
			docName, params := portForwardDocument(f)
			logger.Infof("%s: forward %s: starting", in.Name, f)
			err := s.superviseSessionWithHooks(fwdCtx, superviseSessionInput{
				InstanceID:   instanceID,
				DocumentName: docName,
				Parameters:   params,
				MaxRetries:   defaultTunnelRetries,
				Observer:     tracker.observer(i),
			}, hooks)
			// Context cancellation is the shutdown path, not a forward
			// failure.
			if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return
			}
			tracker.setDead(i, err)
			if err != nil {
				logger.Errorf("%s: forward %s: %v", in.Name, f, err)
			} else {
				logger.Infof("%s: forward %s: session ended", in.Name, f)
			}
		}(i, f)
	}
	logger.Infof("%s: tunnel daemon started (pid %d, %d forward(s))", in.Name, state.PID, len(in.Forwards))

	<-stopCh
	logger.Infof("%s: stop requested; shutting down", in.Name)

	// Shutdown order: remember live sessions, terminate the children
	// (which tears the local listeners down), then best-effort close
	// the server-side sessions, then stop answering control requests.
	liveSessions := tracker.activeSessionIDs()
	cancelForwards()
	wg.Wait()
	childOut.Flush()
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
	if err := removeTunnelState(in.Name); err != nil {
		logger.Errorf("%s: %v", in.Name, err)
	}
	logger.Infof("%s: tunnel daemon stopped", in.Name)
	return nil
}

// defaultTunnelRetries is the per-forward reconnect budget. Same
// policy as connect's interactive shell default (ADR 0014): the budget
// replenishes after 60s of stability, so long-lived tunnels survive
// repeated disconnects across days.
const defaultTunnelRetries = 5

// tunnelDaemonConnectHooks returns supervision hooks for daemon use:
// child stdio goes to the OS log instead of a terminal, and terminal
// signal guarding is skipped (the daemon has no terminal).
func (s *Service) tunnelDaemonConnectHooks(childOut *lineWriter) *connectHooks {
	base := defaultConnectHooks()
	return &connectHooks{
		runSession: func(ctx context.Context, payload []byte, disconnectCode int) (int, error) {
			return spawnSessionRunnerIO(ctx, payload, disconnectCode, sessionRunnerIO{
				stdin:                nil,
				stdout:               childOut,
				stderr:               childOut,
				guardTerminalSignals: false,
			})
		},
		sleep:       base.sleep,
		now:         base.now,
		notify:      newLineWriter(func(string) {}), // transitions reach the log via the tracker
		stableAfter: base.stableAfter,
	}
}
