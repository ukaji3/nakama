package awsx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// exitServer is a check site behind a network with redundant exits: it
// answers on each connection with the next address of answers, the way
// a proxy or NAT that picks its exit per connection would show up.
type exitServer struct {
	*httptest.Server
	used     atomic.Int32 // connections that carried a request
	requests atomic.Int32
	reused   atomic.Int32 // requests on a connection that carried one before
	h2       atomic.Int32
}

// exitConn is one connection to an exitServer.
type exitConn struct {
	once  sync.Once
	index int32
	reqs  atomic.Int32
}

type exitConnKey struct{}

func newExitServer(t *testing.T, tls bool, answers ...string) *exitServer {
	t.Helper()
	s := &exitServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if r.ProtoMajor == 2 {
			s.h2.Add(1)
		}
		c := r.Context().Value(exitConnKey{}).(*exitConn)
		c.once.Do(func() { c.index = s.used.Add(1) - 1 })
		if c.reqs.Add(1) > 1 {
			s.reused.Add(1)
		}
		_, _ = fmt.Fprintln(w, answers[int(c.index)%len(answers)])
	}))
	s.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, exitConnKey{}, &exitConn{})
	}
	if tls {
		s.EnableHTTP2 = true
		s.StartTLS()
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
	return s
}

// dialLog records what the check transport dials, and dials it.
type dialLog struct {
	mu    sync.Mutex
	dials []string // "network addr"
	// redirect maps a host that does not resolve (proxy.invalid) to
	// the address of the test server behind it.
	redirect map[string]string
}

func (l *dialLog) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	l.mu.Lock()
	l.dials = append(l.dials, network+" "+addr)
	l.mu.Unlock()
	if to, ok := l.redirect[addr]; ok {
		addr = to
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// testDiscoverer builds the production discoverer around base (which
// carries the test servers' TLS trust) and the given sites.
func testDiscoverer(base *http.Transport, sites []ipCheckSite, l *dialLog) *httpIPDiscoverer {
	return &httpIPDiscoverer{
		client:      &http.Client{Timeout: 5 * time.Second, Transport: base.Clone()},
		checkClient: &http.Client{Timeout: 5 * time.Second, Transport: newIPCheckTransport(base.Clone(), sites, l.dial)},
		sites:       sites,
	}
}

func TestDetectPublicIPv4s_FindsEveryExit(t *testing.T) {
	t.Parallel()
	// Each site sees the three exits in a different order; one exit is
	// seen by only one site.
	a := newExitServer(t, true, "198.51.100.1", "198.51.100.2", "198.51.100.1")
	b := newExitServer(t, true, "203.0.113.9", "198.51.100.2", "198.51.100.2")
	base := a.Client().Transport.(*http.Transport).Clone() // trusts the test certificate
	base.ForceAttemptHTTP2 = true
	l := &dialLog{}
	d := testDiscoverer(base, []ipCheckSite{{"a", a.URL}, {"b", b.URL}}, l)

	got, err := d.DetectPublicIPv4s(context.Background())
	if err != nil {
		t.Fatalf("DetectPublicIPv4s: %v", err)
	}
	if strings.Join(got.IPs, ",") != "198.51.100.1,198.51.100.2,203.0.113.9" || got.Checks != 6 || len(got.Failures) != 0 {
		t.Errorf("detected = %+v, want the three exits sorted, from 6 checks", got)
	}
	for name, s := range map[string]*exitServer{"a": a, "b": b} {
		// A reused connection (keep-alive, or an HTTP/2 connection
		// carrying several requests) would show one exit only.
		if s.requests.Load() != IPCheckRounds || s.reused.Load() != 0 {
			t.Errorf("site %s: %d requests, %d of them on a reused connection; want %d, each on a new one", name, s.requests.Load(), s.reused.Load(), IPCheckRounds)
		}
		if s.h2.Load() != IPCheckRounds {
			t.Errorf("site %s: %d of the requests were HTTP/2; the test means to cover HTTP/2 too", name, s.h2.Load())
		}
	}
	for _, dl := range l.dials {
		if !strings.HasPrefix(dl, "tcp4 ") {
			t.Errorf("dialed %q; a direct connection to a check site must be IPv4", dl)
		}
	}
}

func TestDetectPublicIPv4s_LeavesTheProxyConnectionAlone(t *testing.T) {
	t.Parallel()
	// A forward proxy for plain HTTP: it answers for the site itself.
	var asked atomic.Value
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.URL.String())
		_, _ = fmt.Fprintln(w, "203.0.113.77")
	}))
	t.Cleanup(proxySrv.Close)
	port := proxySrv.Listener.Addr().(*net.TCPAddr).Port
	l := &dialLog{redirect: map[string]string{fmt.Sprintf("proxy.invalid:%d", port): proxySrv.Listener.Addr().String()}}
	base := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("proxy.invalid:%d", port)})}
	d := testDiscoverer(base, []ipCheckSite{{"site", "http://site.invalid/ip"}}, l)

	got, err := d.DetectPublicIPv4s(context.Background())
	if err != nil || strings.Join(got.IPs, ",") != "203.0.113.77" {
		t.Fatalf("DetectPublicIPv4s = %+v, %v", got, err)
	}
	if v, _ := asked.Load().(string); v != "http://site.invalid/ip" {
		t.Errorf("the proxy was asked for %q", v)
	}
	if len(l.dials) != IPCheckRounds {
		t.Errorf("dials = %v, want one new connection to the proxy per check", l.dials)
	}
	for _, dl := range l.dials {
		if dl != fmt.Sprintf("tcp proxy.invalid:%d", port) {
			t.Errorf("dialed %q; the connection to a proxy must keep the network it asked for", dl)
		}
	}
}

func TestDetectPublicIPv4s_PartialFailureKeepsWhatWasFound(t *testing.T) {
	t.Parallel()
	good := newExitServer(t, false, "198.51.100.1")
	// A TLS-intercepting proxy adding X-Forwarded-For makes checkip
	// answer with the internal address; an HTML page is no answer.
	bad := newExitServer(t, false, "10.1.2.3", "<!doctype html>", "10.1.2.3")
	d := testDiscoverer(&http.Transport{}, []ipCheckSite{{"good", good.URL}, {"bad", bad.URL}}, &dialLog{})

	got, err := d.DetectPublicIPv4s(context.Background())
	if err != nil {
		t.Fatalf("DetectPublicIPv4s: %v", err)
	}
	if strings.Join(got.IPs, ",") != "198.51.100.1" || len(got.Failures) != 3 {
		t.Fatalf("detected = %+v", got)
	}
	sum := got.FailureSummary()
	if len(sum) != 1 || !strings.HasPrefix(sum[0], "bad: 3 of 3 checks failed: answered ") {
		t.Errorf("FailureSummary = %q", sum)
	}
}

func TestDetectPublicIPv4s_FailsWhenNothingIsFound(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	t.Cleanup(down.Close)
	v6 := newExitServer(t, false, "2001:db8::1")
	d := testDiscoverer(&http.Transport{}, []ipCheckSite{{"down", down.URL}, {"v6", v6.URL}}, &dialLog{})

	got, err := d.DetectPublicIPv4s(context.Background())
	if err == nil || len(got.IPs) != 0 {
		t.Fatalf("DetectPublicIPv4s = %+v, %v; want an error", got, err)
	}
	for _, want := range []string{"no check site answered", "down: 3 of 3 checks failed: the check site returned status 429", "v6: 3 of 3 checks failed: answered the IPv6 address 2001:db8::1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestPublicIPv4_AsksTheFirstSiteOnce(t *testing.T) {
	t.Parallel()
	first := newExitServer(t, false, "198.51.100.1")
	second := newExitServer(t, false, "198.51.100.2")
	d := testDiscoverer(&http.Transport{}, []ipCheckSite{{"first", first.URL}, {"second", second.URL}}, &dialLog{})
	ip, err := d.PublicIPv4(context.Background())
	if err != nil || ip != "198.51.100.1" || first.requests.Load() != 1 || second.requests.Load() != 0 {
		t.Errorf("PublicIPv4 = %q, %v (requests %d, %d)", ip, err, first.requests.Load(), second.requests.Load())
	}
}

func TestPublicIPv4Answer(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"198.51.100.7":        "",
		"::ffff:198.51.100.7": "",
		"2001:db8::1":         "IPv6",
		"10.1.2.3":            "not a public address",
		"172.16.0.1":          "not a public address",
		"192.168.0.1":         "not a public address",
		"100.64.0.1":          "not a public address",
		"127.0.0.1":           "not a public address",
		"169.254.1.1":         "not a public address",
		"0.0.0.0":             "not a public address",
		"224.0.0.1":           "not a public address",
		"255.255.255.255":     "not a public address",
		"<!doctype html>":     "not an IP address",
		"198.51.100.7, 10.0":  "not an IP address",
	} {
		ip, err := publicIPv4(raw)
		switch {
		case want == "" && (err != nil || ip.String() != "198.51.100.7"):
			t.Errorf("publicIPv4(%q) = %v, %v; want 198.51.100.7", raw, ip, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("publicIPv4(%q) error = %v, want %q", raw, err, want)
		}
	}
}

func TestIPCheckNetwork(t *testing.T) {
	t.Parallel()
	hosts := map[string]bool{"checkip.amazonaws.com": true, "ifconfig.io": true}
	for _, c := range []struct{ network, addr, want string }{
		{"tcp", "ifconfig.io:443", "tcp4"},
		{"tcp", "checkip.amazonaws.com:443", "tcp4"},
		{"tcp", "proxy.example.com:8080", "tcp"},
		{"tcp6", "ifconfig.io:443", "tcp6"},
		{"tcp", "ifconfig.io", "tcp"}, // no port: not a dial target
	} {
		if got := ipCheckNetwork(c.network, c.addr, hosts); got != c.want {
			t.Errorf("ipCheckNetwork(%q, %q) = %q, want %q", c.network, c.addr, got, c.want)
		}
	}
}

func TestIPCheckSites(t *testing.T) {
	t.Parallel()
	var names []string
	for _, s := range ipCheckSites {
		u, err := url.Parse(s.url)
		if err != nil || u.Scheme != "https" || u.Hostname() != s.name {
			t.Errorf("site %+v: want an https URL on the host it is named after", s)
		}
		names = append(names, s.name)
	}
	// PublicIPv4 (open, doctor) asks the first one.
	if strings.Join(names, ",") != "checkip.amazonaws.com,ifconfig.io" {
		t.Errorf("sites = %v", names)
	}
}

func TestFailureSummary(t *testing.T) {
	t.Parallel()
	var none *DetectedIPs
	if none.FailureSummary() != nil || (&DetectedIPs{Checks: 6}).FailureSummary() != nil {
		t.Error("no failure, no summary")
	}
	d := &DetectedIPs{Checks: 6, Failures: []IPCheckFailure{
		{Site: "a", Err: errors.New("first")}, {Site: "b", Err: errors.New("timeout")}, {Site: "a", Err: errors.New("second")},
	}}
	if got := strings.Join(d.FailureSummary(), " | "); got != "a: 2 of 3 checks failed: first | b: 1 of 3 checks failed: timeout" {
		t.Errorf("FailureSummary = %q", got)
	}
}
