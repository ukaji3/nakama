package service

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is the request side of the nakama router (Issue #124, ADR
// 0029): one small background process per user that listens on a fixed
// loopback port and sends
//
//	http://<purpose>.<instance>.nakama.localhost:<port>/
//
// to the browser forward of that instance's tunnel daemon, chosen by the
// Host header. Browsers resolve *.localhost to the loopback addresses
// themselves (Chrome, Edge, Firefox; Safari through the system resolver
// from macOS 26 on), so several instances open side by side without the
// user choosing or remembering local ports, and each instance gets an
// origin (and with it, cookies and storage) of its own.
//
// The router needs no AWS credentials: like `nakama aws tunnel status`
// it reads the tunnel state files and asks each daemon's control
// channel which local port serves the forward.

const (
	// DefaultRouterPort is the router's port when NAKAMA_ROUTER_PORT is
	// not set. It is fixed so that the addresses stay the same (history,
	// bookmarks); 8888 was avoided because Jupyter uses it.
	DefaultRouterPort = 18888
	// RouterPortEnv names the environment variable that moves the
	// router to another port.
	RouterPortEnv = "NAKAMA_ROUTER_PORT"
	// routerDomain is the name every router address ends in.
	routerDomain = "nakama.localhost"
)

// routerPurposes are the forwards the router serves, in the order of
// the host name pattern.
var routerPurposes = []ForwardPurpose{ForwardPurposeCodeServer, ForwardPurposeKiroCrew}

// routerHostPattern matches <purpose>.<instance>.nakama.localhost, with
// an optional trailing root dot. The instance part is the instance name
// pattern itself, so a name nakama accepts is always routable.
var routerHostPattern = func() *regexp.Regexp {
	names := make([]string, len(routerPurposes))
	for i, p := range routerPurposes {
		names[i] = regexp.QuoteMeta(string(p))
	}
	instance := strings.TrimSuffix(strings.TrimPrefix(instanceNamePattern.String(), "^"), "$")
	return regexp.MustCompile(`^(` + strings.Join(names, "|") + `)\.(` + instance + `)\.` +
		regexp.QuoteMeta(routerDomain) + `\.?$`)
}()

// RouterPort returns the router port this process uses: NAKAMA_ROUTER_PORT
// when set, DefaultRouterPort otherwise.
func RouterPort() (int, error) {
	v := strings.TrimSpace(os.Getenv(RouterPortEnv))
	if v == "" {
		return DefaultRouterPort, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid %s=%q: it must be a port number (1-65535)", RouterPortEnv, v)
	}
	return n, nil
}

// RouterHost returns the Host of a browser forward behind the router:
// <purpose>.<name>.nakama.localhost:<port>.
func RouterHost(p ForwardPurpose, name string, port int) string {
	return net.JoinHostPort(string(p)+"."+name+"."+routerDomain, strconv.Itoa(port))
}

// RouterOrigin returns the browser origin of a forward behind the
// router (no trailing slash).
func RouterOrigin(p ForwardPurpose, name string, port int) string {
	return "http://" + RouterHost(p, name, port)
}

// parseRouterHost splits a Host header into the purpose and instance it
// addresses. It fails for any other name, and for a nakama name on
// another port: the router only answers for its own addresses (a DNS
// rebinding barrier, see ADR 0029).
func parseRouterHost(host string, port int) (ForwardPurpose, string, bool) {
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != strconv.Itoa(port) {
		return ForwardPurposeNone, "", false
	}
	m := routerHostPattern.FindStringSubmatch(strings.ToLower(h))
	if m == nil {
		return ForwardPurposeNone, "", false
	}
	return ForwardPurpose(m[1]), m[2], true
}

// findBrowserForward returns the forward a browser uses for the
// instance-side port remotePort: the first forward to that port of the
// instance itself (no remote host) that is not dead. `open` / `kirocrew`
// reuse exactly this forward (EnsureBrowserForward), and the router
// routes to it, so the two never disagree. dead is the last dead
// forward to the port, for explaining why none is usable.
func findBrowserForward(status *TunnelStatus, remotePort string) (live int, dead int) {
	dead = -1
	for i, fs := range status.Forwards {
		f, err := ParseForwardSpec(fs.Spec)
		if err != nil || f.RemoteHost != "" || f.RemotePort != remotePort {
			continue
		}
		if fs.State == ForwardStateDead {
			dead = i
			continue
		}
		return i, dead
	}
	return -1, dead
}

// routeErrorKind says why the router cannot serve an address.
type routeErrorKind int

const (
	// routeNoTunnel: no tunnel daemon is recorded for the instance.
	routeNoTunnel routeErrorKind = iota
	// routeDaemonGone: the recorded daemon does not answer.
	routeDaemonGone
	// routeNoForward: the daemon has no forward for the purpose.
	routeNoForward
	// routeForwardDead: the forward for the purpose has ended.
	routeForwardDead
)

// routeError is a failed lookup; the router turns it into a page that
// says what to do.
type routeError struct {
	kind    routeErrorKind
	name    string
	purpose ForwardPurpose
	// detail is the daemon's last word on a dead forward.
	detail string
}

func (e *routeError) Error() string {
	switch e.kind {
	case routeNoTunnel:
		return fmt.Sprintf("no tunnel is running for %s", e.name)
	case routeDaemonGone:
		return fmt.Sprintf("the tunnel daemon of %s does not answer", e.name)
	case routeForwardDead:
		return fmt.Sprintf("the %s forward of %s has ended: %s", e.purpose, e.name, e.detail)
	default:
		return fmt.Sprintf("the tunnel of %s has no %s forward", e.name, e.purpose)
	}
}

// routeTable resolves the browser forwards for the router.
type routeTable interface {
	// resolve returns the local port of the instance's forward for the
	// purpose, or a *routeError.
	resolve(ctx context.Context, name string, p ForwardPurpose) (string, error)
	// forget drops a cached answer (after the forward failed).
	forget(name string, p ForwardPurpose)
	// status returns the forward's current status, for error pages.
	status(ctx context.Context, name string, p ForwardPurpose) (ForwardStatus, bool)
}

// routerLookupTTL bounds how long the router trusts a resolved port. A
// page load makes hundreds of requests; asking the daemon for each of
// them would only add latency.
var routerLookupTTL = 2 * time.Second

// tunnelRoutes is the production routeTable: the tunnel state files and
// the daemons' control channels.
type tunnelRoutes struct {
	query func(ctx context.Context, st *TunnelState, timeout time.Duration) (*TunnelStatus, error)
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]routeCacheEntry
}

type routeCacheEntry struct {
	port    string
	expires time.Time
}

func newTunnelRoutes() *tunnelRoutes {
	return &tunnelRoutes{query: queryTunnelStatus, now: time.Now, cache: map[string]routeCacheEntry{}}
}

func routeKey(name string, p ForwardPurpose) string { return name + "/" + string(p) }

// lookup reads the instance's daemon status. Only answers from the
// daemon count as routes; nothing here changes any state.
func (t *tunnelRoutes) lookup(ctx context.Context, name string) (*TunnelStatus, error) {
	st, err := readTunnelState(name)
	if err != nil {
		if isNoTunnel(err) {
			return nil, &routeError{kind: routeNoTunnel, name: name}
		}
		return nil, err
	}
	status, err := t.query(ctx, st, tunnelControlTimeout)
	if err != nil {
		return nil, &routeError{kind: routeDaemonGone, name: name}
	}
	return status, nil
}

func (t *tunnelRoutes) resolve(ctx context.Context, name string, p ForwardPurpose) (string, error) {
	key := routeKey(name, p)
	t.mu.Lock()
	if e, ok := t.cache[key]; ok && t.now().Before(e.expires) {
		t.mu.Unlock()
		return e.port, nil
	}
	t.mu.Unlock()

	status, err := t.lookup(ctx, name)
	if err != nil {
		return "", err
	}
	live, dead := findBrowserForward(status, strconv.Itoa(remotePortFor(p)))
	if live < 0 {
		if dead >= 0 {
			return "", &routeError{kind: routeForwardDead, name: name, purpose: p, detail: status.Forwards[dead].Detail}
		}
		return "", &routeError{kind: routeNoForward, name: name, purpose: p}
	}
	f, err := ParseForwardSpec(status.Forwards[live].Spec)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	t.cache[key] = routeCacheEntry{port: f.LocalPort, expires: t.now().Add(routerLookupTTL)}
	t.mu.Unlock()
	return f.LocalPort, nil
}

func (t *tunnelRoutes) forget(name string, p ForwardPurpose) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.cache, routeKey(name, p))
}

func (t *tunnelRoutes) status(ctx context.Context, name string, p ForwardPurpose) (ForwardStatus, bool) {
	status, err := t.lookup(ctx, name)
	if err != nil {
		return ForwardStatus{}, false
	}
	live, dead := findBrowserForward(status, strconv.Itoa(remotePortFor(p)))
	switch {
	case live >= 0:
		return status.Forwards[live], true
	case dead >= 0:
		return status.Forwards[dead], true
	default:
		return ForwardStatus{}, false
	}
}

// Kiro Crew accepts only loopback names in Host and only its own
// loopback origins in Origin (a DNS rebinding barrier and its CSRF and
// WebSocket checks), so the router presents its requests as if the
// browser had opened the gateway's own address.
var (
	kiroCrewUpstreamHost   = net.JoinHostPort("localhost", strconv.Itoa(kirocrewGatewayPort))
	kiroCrewUpstreamOrigin = "http://" + kiroCrewUpstreamHost
)

// routerUpstreamIdleTimeout closes the router's idle connections to the
// forwards quickly. The tunnel daemon counts every open local connection
// as use of the forward (ADR 0027 decision 6); idle keep-alive
// connections held by the router would keep the SSM session, and its
// health checks, alive for nothing.
var routerUpstreamIdleTimeout = 10 * time.Second

// newRouterTransport returns the transport from the router to the
// forwards: loopback only, never through a proxy, HTTP/1.1.
func newRouterTransport() *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     routerUpstreamIdleTimeout,
		ForceAttemptHTTP2:   false,
	}
}

// routerHandler is the router's HTTP handler.
type routerHandler struct {
	port      int
	routes    routeTable
	transport http.RoundTripper
	logf      func(format string, args ...any)

	proxies sync.Map // routeKey + "/" + backend port -> *httputil.ReverseProxy
}

func newRouterHandler(port int, routes routeTable, transport http.RoundTripper, logf func(string, ...any)) *routerHandler {
	return &routerHandler{port: port, routes: routes, transport: transport, logf: logf}
}

// publicOriginKey carries the browser-facing origin of a request into
// ModifyResponse, after the outbound Host was rewritten.
type publicOriginKey struct{}

func (h *routerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, name, ok := parseRouterHost(r.Host, h.port)
	if !ok {
		writeRouterPage(w, http.StatusMisdirectedRequest, routerPage{
			Title:  "This is not a nakama address",
			Detail: fmt.Sprintf("The nakama router on this port only serves http://<code-server|kirocrew>.<instance>.%s:%d/.", routerDomain, h.port),
		})
		return
	}
	backend, err := h.routes.resolve(r.Context(), name, p)
	if err != nil {
		var re *routeError
		if errors.As(err, &re) {
			writeRouterPage(w, http.StatusBadGateway, routeErrorPage(re))
			return
		}
		h.logf("router: look up %s of %s: %v", p, name, err)
		writeRouterPage(w, http.StatusBadGateway, routerPage{Title: "The nakama router could not look up " + name, Detail: err.Error()})
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), publicOriginKey{}, "http://"+r.Host))
	h.proxy(name, p, backend).ServeHTTP(w, r)
}

// proxy returns the reverse proxy for one instance, purpose and forward
// port.
func (h *routerHandler) proxy(name string, p ForwardPurpose, backend string) *httputil.ReverseProxy {
	key := routeKey(name, p) + "/" + backend
	if v, ok := h.proxies.Load(key); ok {
		return v.(*httputil.ReverseProxy)
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", backend)}
	rp := &httputil.ReverseProxy{
		// Rewrite (not Director): it drops the client's Forwarded and
		// X-Forwarded-For / -Host / -Proto and adds none of its own.
		Rewrite:   func(pr *httputil.ProxyRequest) { rewriteRouterRequest(pr, p, target) },
		Transport: h.transport,
		ModifyResponse: func(resp *http.Response) error {
			if p == ForwardPurposeKiroCrew {
				rewriteKiroCrewLocation(resp)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.routes.forget(name, p)
			if errors.Is(err, context.Canceled) {
				return // the browser went away
			}
			h.logf("router: %s of %s: %v", p, name, err)
			fs, _ := h.routes.status(context.WithoutCancel(r.Context()), name, p)
			writeRouterPage(w, http.StatusBadGateway, upstreamErrorPage(name, p, fs))
		},
	}
	actual, _ := h.proxies.LoadOrStore(key, rp)
	return actual.(*httputil.ReverseProxy)
}

// rewriteRouterRequest prepares the request to a forward.
//   - code-server gets the browser's Host: its WebSocket origin check
//     compares Origin with Host.
//   - Kiro Crew gets Host localhost:5476, and Origin / Referer that name
//     the router address itself are rewritten to http://localhost:5476.
//     A foreign Origin is left as it is, so Kiro Crew still refuses it.
//   - No Forwarded / X-Forwarded-* / X-Real-IP header reaches the far
//     end: Kiro Crew treats requests with them as proxied ones, which
//     may not change settings or show secrets.
func rewriteRouterRequest(pr *httputil.ProxyRequest, p ForwardPurpose, target *url.URL) {
	public := "http://" + pr.In.Host
	pr.SetURL(target)
	// Rewrite has already dropped Forwarded and X-Forwarded-For / -Host
	// / -Proto; X-Real-IP is not one of them.
	pr.Out.Header.Del("X-Real-Ip")
	if p != ForwardPurposeKiroCrew {
		pr.Out.Host = pr.In.Host
		return
	}
	pr.Out.Host = kiroCrewUpstreamHost
	if o := pr.Out.Header.Get("Origin"); o != "" && strings.EqualFold(o, public) {
		pr.Out.Header.Set("Origin", kiroCrewUpstreamOrigin)
	}
	if ref := pr.Out.Header.Get("Referer"); ref != "" {
		if rest, ok := cutOriginPrefix(ref, public); ok {
			pr.Out.Header.Set("Referer", kiroCrewUpstreamOrigin+rest)
		}
	}
}

// rewriteKiroCrewLocation maps an absolute redirect to the gateway's own
// address back to the router address the browser uses.
func rewriteKiroCrewLocation(resp *http.Response) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return
	}
	rest, ok := cutOriginPrefix(loc, kiroCrewUpstreamOrigin)
	if !ok {
		return
	}
	public, _ := resp.Request.Context().Value(publicOriginKey{}).(string)
	if public == "" {
		return
	}
	resp.Header.Set("Location", public+rest)
}

// cutOriginPrefix returns what follows origin in u when u is the origin
// itself or a URL on it (case-insensitive, as host names are).
func cutOriginPrefix(u, origin string) (string, bool) {
	if len(u) < len(origin) || !strings.EqualFold(u[:len(origin)], origin) {
		return "", false
	}
	rest := u[len(origin):]
	if rest == "" || rest[0] == '/' || rest[0] == '?' || rest[0] == '#' {
		return rest, true
	}
	return "", false // e.g. http://localhost:54760
}

// routerPage is an error page of the router.
type routerPage struct {
	Title    string
	Detail   string
	Commands []string
}

var routerPageTemplate = template.Must(template.New("router").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<title>nakama: {{.Title}}</title>
</head>
<body>
<h1>{{.Title}}</h1>
<p>{{.Detail}}</p>
{{if .Commands}}<p>Run on this machine:</p>
<ul>{{range .Commands}}<li><code>{{.}}</code></li>{{end}}</ul>{{end}}
</body>
</html>
`))

// routerErrorHeader marks the router's own error pages, so the
// readiness check of `open` does not take them for an answer of
// code-server or Kiro Crew. Its value is the page title.
const routerErrorHeader = "X-Nakama-Router-Error"

// writeRouterPage writes an error page. It is never cached, so the page
// goes away as soon as the forward is back.
func writeRouterPage(w http.ResponseWriter, code int, page routerPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(routerErrorHeader, page.Title)
	w.WriteHeader(code)
	_ = routerPageTemplate.Execute(w, page)
}

// openCommand is the command that (re)creates the forward of p.
func openCommand(p ForwardPurpose, name string) string {
	if p == ForwardPurposeKiroCrew {
		return "nakama aws kirocrew " + name
	}
	return "nakama aws open " + name
}

func routeErrorPage(e *routeError) routerPage {
	page := routerPage{Title: fmt.Sprintf("No %s forward for %s", e.purpose, e.name), Detail: e.Error() + "."}
	page.Commands = []string{openCommand(e.purpose, e.name)}
	if e.kind == routeForwardDead || e.kind == routeDaemonGone {
		page.Commands = append(page.Commands, "nakama aws tunnel status "+e.name)
	}
	return page
}

// upstreamErrorPage explains a forward that did not answer, with the
// daemon's last word on it. TargetNotConnected means the SSM agent of
// the instance is not connected, typically because it is stopped.
func upstreamErrorPage(name string, p ForwardPurpose, fs ForwardStatus) routerPage {
	page := routerPage{
		Title:    fmt.Sprintf("The %s forward of %s did not answer", p, name),
		Detail:   "The SSM session behind it is not up.",
		Commands: []string{"nakama aws tunnel status " + name},
	}
	if fs.Detail != "" {
		page.Detail = fmt.Sprintf("The SSM session behind it is not up (%s: %s).", fs.State, fs.Detail)
	}
	if fs.LastErrorCode == "TargetNotConnected" {
		page.Detail += " The SSM agent of the instance is not connected; the instance may be stopped."
		page.Commands = append([]string{"nakama aws start " + name}, page.Commands...)
	}
	return page
}
