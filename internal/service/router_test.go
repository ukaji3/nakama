package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the router's request side (Issue #124, ADR 0029): which
// addresses it answers, what reaches code-server and Kiro Crew, and the
// error pages.

func TestParseRouterHost(t *testing.T) {
	for _, tc := range []struct {
		host    string
		purpose ForwardPurpose
		name    string
		ok      bool
	}{
		{"code-server.dev01.nakama.localhost:18888", ForwardPurposeCodeServer, "dev01", true},
		{"kirocrew.dev01.nakama.localhost:18888", ForwardPurposeKiroCrew, "dev01", true},
		// Browsers send lower case, but host names are case-insensitive.
		{"Code-Server.DEV01.Nakama.LocalHost:18888", ForwardPurposeCodeServer, "dev01", true},
		{"code-server.dev01.nakama.localhost.:18888", ForwardPurposeCodeServer, "dev01", true},
		{"kirocrew.a-very-long-name-of-32-characters.nakama.localhost:18888", ForwardPurposeKiroCrew, "a-very-long-name-of-32-characters", false},
		{"kirocrew.a-very-long-name-of-32-character.nakama.localhost:18888", ForwardPurposeKiroCrew, "a-very-long-name-of-32-character", true},
		{"code-server.dev01.nakama.localhost:9999", "", "", false},
		{"code-server.dev01.nakama.localhost", "", "", false},
		{"jupyter.dev01.nakama.localhost:18888", "", "", false},
		{"x.code-server.dev01.nakama.localhost:18888", "", "", false},
		{"code-server.dev_01.nakama.localhost:18888", "", "", false},
		{"code-server.-dev.nakama.localhost:18888", "", "", false},
		{"code-server.dev01.localhost:18888", "", "", false},
		{"nakama.localhost:18888", "", "", false},
		{"localhost:18888", "", "", false},
		{"evil.example:18888", "", "", false},
		{"code-server.dev01.nakama.localhost.evil.example:18888", "", "", false},
	} {
		p, name, ok := parseRouterHost(tc.host, 18888)
		if ok != tc.ok || (ok && (p != tc.purpose || name != tc.name)) {
			t.Errorf("parseRouterHost(%q) = %q, %q, %v; want %q, %q, %v", tc.host, p, name, ok, tc.purpose, tc.name, tc.ok)
		}
	}
}

func TestRouterPortFromTheEnvironment(t *testing.T) {
	t.Setenv(RouterPortEnv, "")
	if p, err := RouterPort(); err != nil || p != DefaultRouterPort {
		t.Errorf("unset: %d, %v", p, err)
	}
	t.Setenv(RouterPortEnv, " 19000 ")
	if p, err := RouterPort(); err != nil || p != 19000 {
		t.Errorf("19000: %d, %v", p, err)
	}
	for _, bad := range []string{"abc", "0", "70000", "-1"} {
		t.Setenv(RouterPortEnv, bad)
		if _, err := RouterPort(); err == nil || !strings.Contains(err.Error(), RouterPortEnv) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	if got := RouterOrigin(ForwardPurposeKiroCrew, "dev01", 18888); got != "http://kirocrew.dev01.nakama.localhost:18888" {
		t.Errorf("RouterOrigin = %q", got)
	}
}

// fakeRoutes is a scripted routeTable.
type fakeRoutes struct {
	mu        sync.Mutex
	ports     map[string]string
	errs      map[string]error
	statuses  map[string]ForwardStatus
	resolved  int
	forgotten []string
}

func newFakeRoutes() *fakeRoutes {
	return &fakeRoutes{ports: map[string]string{}, errs: map[string]error{}, statuses: map[string]ForwardStatus{}}
}

func (f *fakeRoutes) resolve(_ context.Context, name string, p ForwardPurpose) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved++
	if err := f.errs[routeKey(name, p)]; err != nil {
		return "", err
	}
	if port, ok := f.ports[routeKey(name, p)]; ok {
		return port, nil
	}
	return "", &routeError{kind: routeNoTunnel, name: name, purpose: p}
}

func (f *fakeRoutes) forget(name string, p ForwardPurpose) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, routeKey(name, p))
}

func (f *fakeRoutes) status(_ context.Context, name string, p ForwardPurpose) (ForwardStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, ok := f.statuses[routeKey(name, p)]
	return fs, ok
}

// recordingBackend is a far end that records the last request and
// answers with the handler's response.
type recordingBackend struct {
	*httptest.Server
	mu   sync.Mutex
	last *http.Request
}

func newRecordingBackend(t *testing.T, h http.HandlerFunc) *recordingBackend {
	t.Helper()
	b := &recordingBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.last = r.Clone(context.Background())
		b.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		_, _ = io.WriteString(w, "far end")
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *recordingBackend) port() string {
	u, _ := url.Parse(b.URL)
	return u.Port()
}

func (b *recordingBackend) lastRequest() *http.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// startRouter serves a router handler for port 18888 on a test server.
func startRouter(t *testing.T, routes routeTable) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newRouterHandler(18888, routes, newRouterTransport(), t.Logf))
	t.Cleanup(srv.Close)
	return srv
}

// routerResponse is a fully read answer of the router.
type routerResponse struct {
	StatusCode int
	Header     http.Header
	Body       string
}

// routerDo sends a request to the router with the given Host and
// returns the response with its body still open (the caller closes it).
func routerDo(t *testing.T, router *httptest.Server, method, host, path string, h http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, router.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range h {
		req.Header[k] = v
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s %s: %v", method, host, path, err)
	}
	return resp
}

// routerGet sends a request to the router with the given Host and reads
// the whole answer.
func routerGet(t *testing.T, router *httptest.Server, method, host, path string, h http.Header) routerResponse {
	t.Helper()
	resp := routerDo(t, router, method, host, path, h)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return routerResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

// forwardingHeaders are what a client may send to pretend a proxy.
var forwardingHeaders = http.Header{
	"X-Forwarded-For":   {"203.0.113.7"},
	"X-Forwarded-Host":  {"evil.example"},
	"X-Forwarded-Proto": {"https"},
	"Forwarded":         {"for=203.0.113.7"},
	"X-Real-Ip":         {"203.0.113.7"},
}

func TestRouterPassesHostThroughToCodeServer(t *testing.T) {
	backend := newRecordingBackend(t, nil)
	routes := newFakeRoutes()
	routes.ports["dev01/code-server"] = backend.port()
	router := startRouter(t, routes)

	h := http.Header{"Origin": {"http://code-server.dev01.nakama.localhost:18888"}}
	for k, v := range forwardingHeaders {
		h[k] = v
	}
	resp := routerGet(t, router, http.MethodGet, "code-server.dev01.nakama.localhost:18888", "/healthz?x=1", h)
	if resp.StatusCode != 200 || resp.Body != "far end" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := backend.lastRequest()
	if got.Host != "code-server.dev01.nakama.localhost:18888" {
		t.Errorf("Host = %q: code-server compares Origin with Host, so it must pass unchanged", got.Host)
	}
	if got.Header.Get("Origin") != "http://code-server.dev01.nakama.localhost:18888" || got.URL.RequestURI() != "/healthz?x=1" {
		t.Errorf("Origin %q, URI %q", got.Header.Get("Origin"), got.URL.RequestURI())
	}
	for k := range forwardingHeaders {
		if v := got.Header.Get(k); v != "" {
			t.Errorf("%s = %q reached the far end", k, v)
		}
	}
}

func TestRouterRewritesForKiroCrew(t *testing.T) {
	var location string
	backend := newRecordingBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		if location != "" {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
		}
	})
	routes := newFakeRoutes()
	routes.ports["dev01/kirocrew"] = backend.port()
	router := startRouter(t, routes)
	const host = "kirocrew.dev01.nakama.localhost:18888"
	const public = "http://" + host

	h := http.Header{"Origin": {public}, "Referer": {public + "/settings?tab=1"}}
	for k, v := range forwardingHeaders {
		h[k] = v
	}
	routerGet(t, router, http.MethodPost, host, "/api/config", h)
	got := backend.lastRequest()
	if got.Host != "localhost:5476" {
		t.Errorf("Host = %q, want localhost:5476 (Kiro Crew only accepts loopback names)", got.Host)
	}
	if got.Header.Get("Origin") != "http://localhost:5476" || got.Header.Get("Referer") != "http://localhost:5476/settings?tab=1" {
		t.Errorf("Origin %q, Referer %q", got.Header.Get("Origin"), got.Header.Get("Referer"))
	}
	for k := range forwardingHeaders {
		if v := got.Header.Get(k); v != "" {
			t.Errorf("%s = %q reached Kiro Crew (it would stop treating the request as a local one)", k, v)
		}
	}

	// A foreign Origin / Referer stays foreign, so Kiro Crew refuses it.
	routerGet(t, router, http.MethodPost, host, "/api/config", http.Header{
		"Origin": {"http://evil.example"}, "Referer": {"http://kirocrew.dev01.nakama.localhost:188889/"}})
	got = backend.lastRequest()
	if got.Header.Get("Origin") != "http://evil.example" || got.Header.Get("Referer") != "http://kirocrew.dev01.nakama.localhost:188889/" {
		t.Errorf("foreign Origin %q, Referer %q must pass unchanged", got.Header.Get("Origin"), got.Header.Get("Referer"))
	}

	for _, tc := range []struct{ location, want string }{
		{"http://localhost:5476/?next=1", public + "/?next=1"},
		{"http://localhost:5476", public},
		{"HTTP://LOCALHOST:5476/x", public + "/x"},
		{"/relative", "/relative"},
		{"http://localhost:54760/", "http://localhost:54760/"},
		{"https://example.com/", "https://example.com/"},
	} {
		location = tc.location
		resp := routerGet(t, router, http.MethodGet, host, "/", nil)
		if got := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || got != tc.want {
			t.Errorf("Location %q -> %q (status %d), want %q", tc.location, got, resp.StatusCode, tc.want)
		}
	}
}

func TestRouterRefusesOtherHosts(t *testing.T) {
	routes := newFakeRoutes()
	router := startRouter(t, routes)
	for _, host := range []string{"evil.example:18888", "localhost:18888", "code-server.dev01.nakama.localhost:9999", "127.0.0.1:18888"} {
		resp := routerGet(t, router, http.MethodGet, host, "/", nil)
		body := resp.Body
		if resp.StatusCode != http.StatusMisdirectedRequest || resp.Header.Get(routerErrorHeader) == "" || !strings.Contains(body, "not a nakama address") {
			t.Errorf("%s: status %d, header %q, body %q", host, resp.StatusCode, resp.Header.Get(routerErrorHeader), body)
		}
	}
	if routes.resolved != 0 {
		t.Error("an unknown host must not even be looked up")
	}
}

func TestRouterErrorPagesSayWhatToDo(t *testing.T) {
	routes := newFakeRoutes()
	routes.errs["dev02/code-server"] = &routeError{kind: routeForwardDead, name: "dev02", purpose: ForwardPurposeCodeServer,
		detail: "the instance no longer exists <script>"}
	routes.errs["dev03/kirocrew"] = &routeError{kind: routeNoForward, name: "dev03", purpose: ForwardPurposeKiroCrew}
	router := startRouter(t, routes)
	for _, tc := range []struct {
		host string
		want []string
	}{
		{"code-server.dev01.nakama.localhost:18888", []string{"no tunnel is running for dev01", "nakama aws open dev01"}},
		{"code-server.dev02.nakama.localhost:18888", []string{"has ended: the instance no longer exists &lt;script&gt;", "nakama aws open dev02", "nakama aws tunnel status dev02"}},
		{"kirocrew.dev03.nakama.localhost:18888", []string{"has no kirocrew forward", "nakama aws kirocrew dev03"}},
	} {
		resp := routerGet(t, router, http.MethodGet, tc.host, "/", nil)
		body := resp.Body
		if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get(routerErrorHeader) == "" {
			t.Errorf("%s: status %d, headers %v", tc.host, resp.StatusCode, resp.Header)
		}
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: page lacks %q:\n%s", tc.host, w, body)
			}
		}
		if strings.Contains(body, "<script>") {
			t.Errorf("%s: the detail must be escaped", tc.host)
		}
	}
}

func TestRouterExplainsAForwardThatDoesNotAnswer(t *testing.T) {
	// A port nothing listens on: the forward does not answer.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	routes := newFakeRoutes()
	routes.ports["dev01/code-server"] = dead
	routes.statuses["dev01/code-server"] = ForwardStatus{State: ForwardStateReconnecting,
		Detail: "attempt 3 (next in 4s): StartSession: TargetNotConnected", LastErrorCode: "TargetNotConnected"}
	router := startRouter(t, routes)
	resp := routerGet(t, router, http.MethodGet, "code-server.dev01.nakama.localhost:18888", "/", nil)
	body := resp.Body
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get(routerErrorHeader) == "" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{"did not answer", "attempt 3", "may be stopped", "nakama aws start dev01", "nakama aws tunnel status dev01"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if len(routes.forgotten) != 1 || routes.forgotten[0] != "dev01/code-server" {
		t.Errorf("the failed route must be forgotten: %v", routes.forgotten)
	}
}

// TestRouterRelaysWebSockets: code-server's workbench and Kiro Crew's
// dashboard live on WebSocket connections.
func TestRouterRelaysWebSockets(t *testing.T) {
	backend := newRecordingBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade expected", http.StatusBadRequest)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(brw, buf); err != nil {
			return
		}
		_, _ = conn.Write([]byte(strings.ToUpper(string(buf))))
	})
	routes := newFakeRoutes()
	routes.ports["dev01/kirocrew"] = backend.port()
	router := startRouter(t, routes)

	conn, err := net.Dial("tcp", strings.TrimPrefix(router.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(conn, "GET /api/ws HTTP/1.1\r\nHost: kirocrew.dev01.nakama.localhost:18888\r\n"+
		"Origin: http://kirocrew.dev01.nakama.localhost:18888\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A 101 has no body; the connection itself carries the frames.
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "PING" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	if got := backend.lastRequest(); got.Header.Get("Origin") != "http://localhost:5476" || got.Host != "localhost:5476" {
		t.Errorf("the upgrade must be rewritten too: Host %q, Origin %q", got.Host, got.Header.Get("Origin"))
	}
}

// TestRouterStreamsEventStreams: an event stream reaches the browser
// event by event, not when the far end finishes.
func TestRouterStreamsEventStreams(t *testing.T) {
	release := make(chan struct{})
	backend := newRecordingBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	routes := newFakeRoutes()
	routes.ports["dev01/kirocrew"] = backend.port()
	router := startRouter(t, routes)
	resp := routerDo(t, router, http.MethodGet, "kirocrew.dev01.nakama.localhost:18888", "/api/events", nil)
	defer func() { _ = resp.Body.Close() }()
	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: one\n" {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Error("the first event did not arrive before the stream ended")
	}
	close(release)
}

// TestRouterClosesIdleUpstreamConnections: the tunnel daemon counts open
// connections as use of the forward, so the router must not keep idle
// ones (ADR 0027 decision 6). Not parallel: it shortens a package timer.
func TestRouterClosesIdleUpstreamConnections(t *testing.T) {
	prev := routerUpstreamIdleTimeout
	routerUpstreamIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { routerUpstreamIdleTimeout = prev })

	closed := make(chan struct{}, 4)
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	backend.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed <- struct{}{}
		}
	}
	backend.Start()
	t.Cleanup(backend.Close)
	u, _ := url.Parse(backend.URL)
	routes := newFakeRoutes()
	routes.ports["dev01/code-server"] = u.Port()
	router := startRouter(t, routes)

	_ = routerGet(t, router, http.MethodGet, "code-server.dev01.nakama.localhost:18888", "/", nil)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the router kept its idle connection to the forward")
	}
}

// TestHealthCheckIgnoresRouterPages: open's readiness check goes through
// the router, whose own error pages are not answers of the far end.
func TestHealthCheckIgnoresRouterPages(t *testing.T) {
	var fromRouter bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fromRouter {
			writeRouterPage(w, http.StatusBadGateway, routerPage{Title: "No code-server forward for dev01"})
			return
		}
		w.WriteHeader(http.StatusBadGateway) // the far end's own 502 is still an answer
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := httpHealthCheck(context.Background(), addr, "code-server.dev01.nakama.localhost:18888", "/healthz"); err != nil {
		t.Errorf("an answer of the far end must count: %v", err)
	}
	fromRouter = true
	err := httpHealthCheck(context.Background(), addr, "code-server.dev01.nakama.localhost:18888", "/healthz")
	if err == nil || !strings.Contains(err.Error(), "No code-server forward for dev01") {
		t.Errorf("a router page must not count: %v", err)
	}
}

func TestTunnelRoutesReadTheDaemons(t *testing.T) {
	withTunnelStateDir(t)
	if err := writeTunnelState(testTunnelState("dev01")); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}
	status := &TunnelStatus{Name: "dev01", Forwards: []ForwardStatus{
		{Spec: "5432:db.internal:5432", State: ForwardStateActive},
		{Spec: "41001:5476", Purpose: ForwardPurposeKiroCrew, State: ForwardStateDead, Detail: "gone"},
		{Spec: "28080:8080", State: ForwardStateIdle}, // a plain -L forward to code-server counts
	}}
	var queryErr error
	queries := 0
	routes := newTunnelRoutes()
	routes.now = clock.now
	routes.query = func(context.Context, *TunnelState, time.Duration) (*TunnelStatus, error) {
		queries++
		return status, queryErr
	}
	ctx := context.Background()

	port, err := routes.resolve(ctx, "dev01", ForwardPurposeCodeServer)
	if err != nil || port != "28080" {
		t.Fatalf("code-server: %q, %v", port, err)
	}
	if _, err := routes.resolve(ctx, "dev01", ForwardPurposeCodeServer); err != nil || queries != 1 {
		t.Errorf("a fresh answer must come from the cache: %d queries, %v", queries, err)
	}
	routes.forget("dev01", ForwardPurposeCodeServer)
	_, _ = routes.resolve(ctx, "dev01", ForwardPurposeCodeServer)
	clock.advance(routerLookupTTL + time.Millisecond)
	_, _ = routes.resolve(ctx, "dev01", ForwardPurposeCodeServer)
	if queries != 3 {
		t.Errorf("forget and the TTL must both cause a new query: %d", queries)
	}

	_, err = routes.resolve(ctx, "dev01", ForwardPurposeKiroCrew)
	var re *routeError
	if !errors.As(err, &re) || re.kind != routeForwardDead || re.detail != "gone" {
		t.Errorf("dead Kiro Crew forward: %v", err)
	}
	if fs, ok := routes.status(ctx, "dev01", ForwardPurposeKiroCrew); !ok || fs.Detail != "gone" {
		t.Errorf("status of the dead forward = %+v, %v", fs, ok)
	}

	status.Forwards = status.Forwards[:1]
	routes.forget("dev01", ForwardPurposeCodeServer)
	if _, err := routes.resolve(ctx, "dev01", ForwardPurposeCodeServer); !errors.As(err, &re) || re.kind != routeNoForward {
		t.Errorf("no code-server forward: %v", err)
	}
	queryErr = errors.New("connection refused")
	if _, err := routes.resolve(ctx, "dev01", ForwardPurposeCodeServer); !errors.As(err, &re) || re.kind != routeDaemonGone {
		t.Errorf("daemon gone: %v", err)
	}
	if _, err := routes.resolve(ctx, "dev02", ForwardPurposeCodeServer); !errors.As(err, &re) || re.kind != routeNoTunnel {
		t.Errorf("no tunnel: %v", err)
	}
	if fileExists(t, "dev01") != true {
		t.Error("the router must never remove a tunnel state file")
	}
}

func fileExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := readTunnelState(name)
	return err == nil
}
