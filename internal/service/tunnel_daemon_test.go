package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// testTunnelLogger sends the daemon log to the test log.
type testTunnelLogger struct{ t *testing.T }

func (l testTunnelLogger) Infof(format string, args ...any)  { l.t.Logf("info: "+format, args...) }
func (l testTunnelLogger) Errorf(format string, args ...any) { l.t.Logf("error: "+format, args...) }
func (l testTunnelLogger) Close() error                      { return nil }

// newTestDaemon returns a daemon whose forwards never reach AWS: a
// Kiro Crew forward finds its instance gone (and dies), any other
// forward waits in its first attempt until the daemon stops.
func newTestDaemon(t *testing.T) (*tunnelDaemon, context.CancelFunc) {
	t.Helper()
	withTunnelStateDir(t)
	st := testTunnelState("dev01")
	st.Forwards = nil
	if err := writeTunnelState(st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &tunnelDaemon{
		name:    "dev01",
		logger:  testTunnelLogger{t},
		ctx:     ctx,
		tracker: newForwardTracker("dev01", time.Now),
		state:   st,
		newDeps: func(f ForwardSpec, _ *lineWriter, logf func(string, ...any)) *forwardDeps {
			return &forwardDeps{
				resolveInstanceID: func(ctx context.Context) (string, error) {
					if f.Purpose == ForwardPurposeKiroCrew {
						return "", nil
					}
					<-ctx.Done()
					return "", ctx.Err()
				},
				startSession: func(context.Context, *ssm.StartSessionInput) (*ssm.StartSessionOutput, error) {
					return nil, errors.New("unexpected StartSession")
				},
				terminateSession: func(string) {},
				pickPort:         pickLoopbackPort,
				dial:             dialForward,
				healthCheck:      httpHealthCheck,
				now:              time.Now,
				sleep:            sleepCtx,
				logf:             logf,
			}
		},
		listen: func(string) ([]net.Listener, error) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			return []net.Listener{l}, nil
		},
	}
	stop := func() {
		cancel()
		d.wg.Wait()
	}
	t.Cleanup(stop)
	return d, stop
}

func TestTunnelDaemonAddsForwards(t *testing.T) {
	d, _ := newTestDaemon(t)

	st, err := d.startForward(ForwardSpec{LocalPort: "8080", RemotePort: "8080"}, true)
	if err != nil {
		t.Fatalf("startForward: %v", err)
	}
	if st.State != ForwardStateConnecting || st.Spec != "8080:8080" {
		t.Errorf("status = %+v", st)
	}
	// A second forward on a live forward's local port is refused.
	if _, err := d.startForward(ForwardSpec{LocalPort: "8080", RemotePort: "8080", Purpose: ForwardPurposeCodeServer}, true); !errors.Is(err, errForwardConflict) {
		t.Errorf("duplicate local port: err = %v", err)
	}
	// A browser forward is added and recorded with its purpose, so
	// `tunnel restart` brings it back.
	if _, err := d.startForward(ForwardSpec{LocalPort: "5476", RemotePort: "5476", Purpose: ForwardPurposeKiroCrew}, true); err != nil {
		t.Fatalf("startForward kirocrew: %v", err)
	}
	recorded, err := readTunnelState("dev01")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recorded.Forwards, ","); got != "8080:8080,kirocrew=5476:5476" {
		t.Errorf("recorded forwards = %q", got)
	}
	if recorded.Token != d.state.Token {
		t.Error("the rewrite must keep the control token")
	}
	if _, err := ParseStoredForwardSpecs(recorded.Forwards); err != nil {
		t.Errorf("the recorded forwards must parse for restart: %v", err)
	}
}

// TestTunnelDaemonReplacesADeadForward: `code` may add a forward on the
// local port of a dead one; the daemon swaps it in place.
func TestTunnelDaemonReplacesADeadForward(t *testing.T) {
	d, _ := newTestDaemon(t)
	kc := ForwardSpec{LocalPort: "5476", RemotePort: "5476", Purpose: ForwardPurposeKiroCrew}
	if _, err := d.startForward(kc, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for d.tracker.state(0) != ForwardStateDead {
		if time.Now().After(deadline) {
			t.Fatalf("the forward did not die: %+v", d.tracker.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := d.startForward(ForwardSpec{LocalPort: "5476", RemotePort: "8080"}, true); err != nil {
		t.Fatalf("replacing the dead forward: %v", err)
	}
	snap := d.tracker.snapshot()
	if len(snap) != 1 || snap[0].Spec != "5476:8080" || snap[0].State == ForwardStateDead {
		t.Errorf("forwards after the replacement = %+v", snap)
	}
	recorded, err := readTunnelState("dev01")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recorded.Forwards, ","); got != "5476:8080" {
		t.Errorf("recorded forwards = %q", got)
	}
}

func TestTunnelDaemonAddFailures(t *testing.T) {
	d, stop := newTestDaemon(t)
	d.listen = func(port string) ([]net.Listener, error) {
		return nil, &LocalPortInUseError{Port: port, Err: errors.New("address already in use")}
	}
	_, err := d.startForward(ForwardSpec{LocalPort: "8080", RemotePort: "8080"}, true)
	var inUse *LocalPortInUseError
	if !errors.As(err, &inUse) {
		t.Errorf("err = %v, want LocalPortInUseError", err)
	}
	if len(d.tracker.snapshot()) != 0 {
		t.Error("a forward whose port cannot be bound must not be added")
	}
	stop()
	d.closing = true
	if _, err := d.startForward(ForwardSpec{LocalPort: "8081", RemotePort: "8080"}, true); err == nil {
		t.Error("a stopping daemon must refuse new forwards")
	}
}

// TestRewriteTunnelStateRefusesAForeignFile: a daemon never overwrites
// the state of another daemon generation.
func TestRewriteTunnelStateRefusesAForeignFile(t *testing.T) {
	withTunnelStateDir(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatal(err)
	}
	other := testTunnelState("dev01")
	other.Token = "another-generation"
	other.Forwards = []string{"9999:9999"}
	if err := rewriteTunnelState(other); err == nil {
		t.Fatal("rewriteTunnelState must refuse a file with another token")
	}
	st, err := readTunnelState("dev01")
	if err != nil || st.Forwards[0] != "8080:8080" {
		t.Errorf("state = %+v, %v; the file must be untouched", st, err)
	}
}

// TestStopLocalTunnel: delete stops this machine's tunnel of the name,
// and a missing tunnel is not an error.
func TestStopLocalTunnel(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	env.svc.stopLocalTunnel(context.Background(), "dev01")
	if env.stopCalled != 0 {
		t.Errorf("stop called %d times without a tunnel", env.stopCalled)
	}
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatal(err)
	}
	env.svc.stopLocalTunnel(context.Background(), "dev01")
	if env.stopCalled != 1 {
		t.Errorf("stop called %d times, want 1", env.stopCalled)
	}
}

// TestCheckLocalPortFreeNamesTheHolder: a busy port held by a recorded
// tunnel names that tunnel's instance.
func TestCheckLocalPortFreeNamesTheHolder(t *testing.T) {
	withTunnelStateDir(t)
	env := newTunnelTestEnv(t)
	st := testTunnelState("dev02")
	st.Forwards = []string{"kirocrew=5476:5476"}
	if err := writeTunnelState(st); err != nil {
		t.Fatal(err)
	}
	env.portBusy["5476"] = true
	env.portBusy["9000"] = true
	err := checkLocalPortFree(env.svc.tunnel, "5476")
	var inUse *LocalPortInUseError
	if !errors.As(err, &inUse) || inUse.Holder != "dev02" {
		t.Errorf("err = %v, want the port held by dev02", err)
	}
	err = checkLocalPortFree(env.svc.tunnel, "9000")
	if !errors.As(err, &inUse) || inUse.Holder != "" || !strings.Contains(err.Error(), fmt.Sprintf("local port %s is not available", "9000")) {
		t.Errorf("err = %v", err)
	}
	if err := checkLocalPortFree(env.svc.tunnel, "9001"); err != nil {
		t.Errorf("a free port: %v", err)
	}
}
