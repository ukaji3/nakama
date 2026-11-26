package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the control channel's connections (Issue #135): every
// request used to leave a loopback connection open in the requesting
// process and in the daemon, until the ephemeral ports to the daemon ran
// out and the router took the running daemon for gone.

// connCounter counts a server's open connections through ConnState.
type connCounter struct{ open atomic.Int64 }

func (c *connCounter) track(_ net.Conn, s http.ConnState) {
	switch s {
	case http.StateNew:
		c.open.Add(1)
	case http.StateClosed, http.StateHijacked:
		c.open.Add(-1)
	}
}

// waitOpen waits until n connections are open, and returns how many are.
func (c *connCounter) waitOpen(n int64, within time.Duration) int64 {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c.open.Load() == n {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.open.Load()
}

// serveControl serves srv on a fresh 127.0.0.1 port and returns the port.
func serveControl(t *testing.T, srv *http.Server) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// statusHandler answers GET /status like a tunnel daemon.
var statusHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"name":"dev01"}`)
})

// TestControlRequestClosesItsConnections: against a server that never
// closes an idle connection itself (the daemons before this fix), no
// request leaves a connection open, also not one kept for reuse.
func TestControlRequestClosesItsConnections(t *testing.T) {
	t.Parallel()
	var conns connCounter
	port := serveControl(t, &http.Server{
		Handler:           statusHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         conns.track,
	})
	const requests = 50
	for i := 0; i < requests; i++ {
		var st TunnelStatus
		if err := controlRequest(context.Background(), port, "tok", http.MethodGet, "/status", nil, &st); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if st.Name != "dev01" {
			t.Fatalf("request %d: status %+v", i, st)
		}
	}
	// An error answer must not keep its connection either.
	if err := controlRequest(context.Background(), port, "wrong", http.MethodGet, "/status", nil, nil); err == nil {
		t.Fatal("a wrong token must fail")
	}
	if open := conns.waitOpen(0, 2*time.Second); open != 0 {
		t.Errorf("%d of %d connections are still open after the requests", open, requests+1)
	}
}

// TestControlServerClosesIdleConnections: the control server closes a
// connection that a keep-alive client (any version before this fix)
// leaves idle.
func TestControlServerClosesIdleConnections(t *testing.T) {
	// Not parallel: it shortens controlIdleTimeout.
	prev := controlIdleTimeout
	controlIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { controlIdleTimeout = prev })

	var conns connCounter
	srv := newControlServer(statusHandler)
	srv.ConnState = conns.track
	port := serveControl(t, srv)

	keepAlive := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer keepAlive.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := keepAlive.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if open := conns.open.Load(); open != 1 {
		t.Fatalf("the keep-alive client holds %d connections, want 1 (the test needs an idle connection)", open)
	}
	if open := conns.waitOpen(0, 3*time.Second); open != 0 {
		t.Errorf("the idle connection is still open after the idle timeout (%d open)", open)
	}
}

// TestRouterControlChannelClosesIdleConnections: the router's own
// control channel uses the same server: an idle keep-alive connection
// is closed by the router.
func TestRouterControlChannelClosesIdleConnections(t *testing.T) {
	// Not parallel: it shortens controlIdleTimeout and uses the state dir.
	withTunnelStateDir(t)
	prev := controlIdleTimeout
	controlIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { controlIdleTimeout = prev })

	tr := startTestRouter(t, freePort(t), func(context.Context) (int, error) { return 1, nil })
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tr.state.ControlPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET /status HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer %s\r\n\r\n", tr.state.Token); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("status %d, close %v: want a kept-alive 200", resp.StatusCode, resp.Close)
	}
	// The router must close the connection after the idle timeout.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("read after the idle timeout: %v, want EOF (the router closed the connection)", err)
	}
}

// TestCountLiveTunnelsSaysWhichDaemonDoesNotAnswer: the count is the
// daemons that answer; the error names the others and says why.
func TestCountLiveTunnelsSaysWhichDaemonDoesNotAnswer(t *testing.T) {
	withTunnelStateDir(t)
	port := serveControl(t, &http.Server{Handler: statusHandler, ReadHeaderTimeout: 5 * time.Second})
	live := testTunnelState("dev01")
	live.ControlPort, live.Token = port, "tok"
	if err := writeTunnelState(live); err != nil {
		t.Fatal(err)
	}
	if n, err := countLiveTunnels(context.Background()); n != 1 || err != nil {
		t.Fatalf("one answering daemon: %d, %v", n, err)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := testTunnelState("dev02")
	gone.ControlPort = closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	if err := writeTunnelState(gone); err != nil {
		t.Fatal(err)
	}
	n, err := countLiveTunnels(context.Background())
	if n != 1 || err == nil || !strings.Contains(err.Error(), "dev02: ") || strings.Contains(err.Error(), "dev01") {
		t.Errorf("one daemon gone: %d, %v; want 1 and an error about dev02 only", n, err)
	}
}

// TestWatchRouterIdleLogsWhyADaemonDoesNotAnswer: a daemon that stops
// answering is logged once with the reason, and once when it answers
// again; repeated failures do not repeat the line.
func TestWatchRouterIdleLogsWhyADaemonDoesNotAnswer(t *testing.T) {
	// Not parallel: it shortens the router's timers.
	prevCheck, prevExit := routerIdleCheckInterval, routerIdleExit
	routerIdleCheckInterval, routerIdleExit = 5*time.Millisecond, time.Hour
	t.Cleanup(func() { routerIdleCheckInterval, routerIdleExit = prevCheck, prevExit })

	refused := errors.New(`dev01: Get "http://127.0.0.1:1/status": dial tcp 127.0.0.1:1: connect: cannot assign requested address`)
	type answer struct {
		n   int
		err error
	}
	script := []answer{{1, nil}, {0, refused}, {0, refused}, {0, refused}, {1, nil}, {1, nil}, {0, refused}}
	var calls atomic.Int32
	done := make(chan struct{})
	logger := &memLogger{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		watchRouterIdle(ctx, func(context.Context) (int, error) {
			i := int(calls.Add(1)) - 1
			if i >= len(script) {
				cancel()
				return 1, nil
			}
			return script[i].n, script[i].err
		}, func() { t.Error("the router must not stop: routerIdleExit is an hour") }, logger)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher did not finish the script")
	}
	var notAnswering, again int
	for _, line := range strings.Split(logger.text(), "\n") {
		switch {
		case strings.HasPrefix(line, "error: router: a tunnel daemon does not answer: ") && strings.Contains(line, "cannot assign requested address"):
			notAnswering++
		case line == "info: router: the tunnel daemons answer again":
			again++
		}
	}
	// Failures at 2-4 and 7: two lines; answers again at 5 and after 7.
	if notAnswering != 2 || again != 2 {
		t.Errorf("logged %d failure and %d recovery lines, want 2 and 2:\n%s", notAnswering, again, logger.text())
	}
}
