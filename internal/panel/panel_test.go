package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the control panel's pages, sessions and request checks
// (Issue #139, ADR 0031). Requests are built the way Chrome 158 and
// Firefox 160 sent them in the PoC of Issue #139.

const (
	testHost   = "control.nakama.localhost:18888"
	testOrigin = "http://control.nakama.localhost:18888"
)

// testClock is a settable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type testPanel struct {
	*Server
	clock *testClock
	h     http.Handler
	logs  *[]string
}

func newTestPanel(t *testing.T) *testPanel {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	logs := &[]string{}
	s := New(Config{
		RouterPort: 18888,
		Version:    "v9.9.9-test",
		Now:        clock.Now,
		PeerCheck:  func(*http.Request) error { return nil },
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			*logs = append(*logs, fmt.Sprintf(format, args...))
		},
	})
	return &testPanel{Server: s, clock: clock, h: s.Handler(), logs: logs}
}

// browserRequest is a request as a browser sends it from the panel's
// own pages.
type browserRequest struct {
	method, path string
	host         string // default testHost
	cookie       string // session id
	site         string // Sec-Fetch-Site; default same-origin
	dest         string // Sec-Fetch-Dest; default empty / document
	origin       string // default testOrigin for POST
	noOrigin     bool
	csrfHeader   string
	form         url.Values
	lang         string
}

func (p *testPanel) do(t *testing.T, br browserRequest) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if br.form != nil {
		body = strings.NewReader(br.form.Encode())
	}
	r := httptest.NewRequest(br.method, br.path, body)
	r.Host = testHost
	if br.host != "" {
		r.Host = br.host
	}
	if br.form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if br.cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: br.cookie})
	}
	site := br.site
	if site == "" {
		site = "same-origin"
	}
	if site != "-" {
		r.Header.Set("Sec-Fetch-Site", site)
	}
	if br.dest != "" {
		r.Header.Set("Sec-Fetch-Dest", br.dest)
	}
	if br.method == http.MethodPost && !br.noOrigin {
		o := br.origin
		if o == "" {
			o = testOrigin
		}
		r.Header.Set("Origin", o)
	}
	if br.csrfHeader != "" {
		r.Header.Set(csrfHeader, br.csrfHeader)
	}
	if br.lang != "" {
		r.Header.Set("Accept-Language", br.lang)
	}
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, r)
	return w
}

// signIn redeems a fresh code and returns the session id and its CSRF
// token.
func (p *testPanel) signIn(t *testing.T) (string, string) {
	t.Helper()
	code, err := p.NewSignInCode()
	if err != nil {
		t.Fatal(err)
	}
	w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + code, site: "none"})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("sign-in: %d %s", w.Code, w.Body)
	}
	var id string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			id = c.Value
		}
	}
	sess, ok := p.store.lookup(id)
	if !ok {
		t.Fatal("the sign-in did not create a session")
	}
	return id, sess.csrf
}

func TestSignInSetsAHostOnlySecureCookieAndRedirectsHome(t *testing.T) {
	p := newTestPanel(t)
	code, err := p.NewSignInCode()
	if err != nil || len(code) != 64 {
		t.Fatalf("code %q, %v: want 256 random bits in hex", code, err)
	}
	w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + code, site: "none"})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("sign-in = %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v", cookies)
	}
	c := cookies[0]
	if c.Name != "__Host-nakama-panel" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" ||
		c.SameSite != http.SameSiteLaxMode || c.MaxAge != 0 || len(c.Value) < 40 {
		t.Errorf("session cookie = %+v; want __Host-, Secure, HttpOnly, Path=/, no Domain, Lax, a browser-session cookie", c)
	}
	// The home page then works with that cookie.
	if w := p.do(t, browserRequest{method: http.MethodGet, path: "/", cookie: c.Value, site: "same-origin"}); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `<meta name="nakama-csrf" content="`) {
		t.Errorf("home = %d %s", w.Code, w.Body)
	}
}

func TestSignInCodesAreSingleUseAndShortLived(t *testing.T) {
	p := newTestPanel(t)
	code, _ := p.NewSignInCode()
	if w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + code, site: "none"}); w.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", w.Code)
	}
	w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + code, site: "none", lang: "en"})
	if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "<code>nakama ui</code>") {
		t.Errorf("second use = %d, cookies %v, body %s; want 403 naming nakama ui", w.Code, w.Result().Cookies(), w.Body)
	}
	late, _ := p.NewSignInCode()
	p.clock.advance(signInCodeTTL)
	if w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + late, site: "none"}); w.Code != http.StatusForbidden {
		t.Errorf("a code %s old = %d, want 403", signInCodeTTL, w.Code)
	}
	for _, bad := range []string{"", "?c=", "?c=" + strings.Repeat("0", 64)} {
		if w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + bad, site: "none"}); w.Code != http.StatusForbidden {
			t.Errorf("sign-in %q = %d, want 403", bad, w.Code)
		}
	}
}

func TestSignInReplacesTheBrowsersSession(t *testing.T) {
	p := newTestPanel(t)
	old, _ := p.signIn(t)
	code, _ := p.NewSignInCode()
	r := browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + code, site: "none", cookie: old}
	if w := p.do(t, r); w.Code != http.StatusSeeOther {
		t.Fatalf("sign-in: %d", w.Code)
	}
	if _, ok := p.store.lookup(old); ok {
		t.Error("the browser's previous session must end when it signs in again")
	}
}

func TestPagesNeedASession(t *testing.T) {
	p := newTestPanel(t)
	w := p.do(t, browserRequest{method: http.MethodGet, path: "/", site: "none", lang: "ja"})
	body := w.Body.String()
	if w.Code != http.StatusUnauthorized || !strings.Contains(body, "ログインしていません") || !strings.Contains(body, "<code>nakama ui</code>") ||
		!strings.Contains(body, `<html lang="ja">`) || strings.Contains(body, "nakama-csrf") {
		t.Errorf("home without a session = %d %s", w.Code, body)
	}
	w = p.do(t, browserRequest{method: http.MethodGet, path: "/", cookie: "guessed", site: "none"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a guessed cookie = %d", w.Code)
	}
	if w := p.do(t, browserRequest{method: http.MethodGet, path: "/api/session", site: "same-origin"}); w.Code != http.StatusUnauthorized ||
		w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("API without a session = %d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestOnlyTheControlPanelHostIsServed(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	for _, host := range []string{"evil.example:18888", "control.nakama.localhost:9999", "control.nakama.localhost",
		"code-server.dev01.nakama.localhost:18888", "127.0.0.1:18888", "localhost:18888"} {
		w := p.do(t, browserRequest{method: http.MethodGet, path: "/", host: host, cookie: id})
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %s = %d, want 421", host, w.Code)
		}
	}
	for _, host := range []string{"Control.Nakama.LocalHost:18888", "control.nakama.localhost.:18888"} {
		if w := p.do(t, browserRequest{method: http.MethodGet, path: "/", host: host, cookie: id}); w.Code != http.StatusOK {
			t.Errorf("Host %s = %d, want 200", host, w.Code)
		}
	}
}

func TestOtherOSUsersAreRefusedEverywhere(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	p.peerCheck = func(*http.Request) error { return errors.New("uid 1001") }
	code, _ := p.NewSignInCode()
	for _, path := range []string{"/", SignInPath + "?c=" + code, "/assets/panel.css", "/signed-out", "/api/session", "/nope"} {
		if w := p.do(t, browserRequest{method: http.MethodGet, path: path, cookie: id}); w.Code != http.StatusForbidden {
			t.Errorf("%s from another OS user = %d, want 403", path, w.Code)
		}
	}
	p.peerCheck = func(*http.Request) error { return nil }
	if w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + code, site: "none"}); w.Code != http.StatusSeeOther {
		t.Errorf("the code must still be unused after the refused request: %d", w.Code)
	}
}

func TestEmbeddedRequestsAreRefused(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	for _, dest := range []string{"iframe", "frame", "embed", "object"} {
		w := p.do(t, browserRequest{method: http.MethodGet, path: "/", cookie: id, site: "same-site", dest: dest})
		if w.Code != http.StatusForbidden {
			t.Errorf("GET / with Sec-Fetch-Dest %s = %d, want 403", dest, w.Code)
		}
		w = p.do(t, browserRequest{method: http.MethodPost, path: "/logout", cookie: id, dest: dest, form: url.Values{"csrf": {csrf}}})
		if w.Code != http.StatusForbidden {
			t.Errorf("POST /logout with Sec-Fetch-Dest %s = %d, want 403", dest, w.Code)
		}
	}
	if _, ok := p.store.lookup(id); !ok {
		t.Error("an embedded sign-out must not end the session")
	}
}

func TestAPIReadsNeedSameOrigin(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	for _, site := range []string{"same-site", "cross-site", "none", "-"} {
		w := p.do(t, browserRequest{method: http.MethodGet, path: "/api/session", cookie: id, site: site})
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("Sec-Fetch-Site %s = %d %s, want 403 JSON", site, w.Code, w.Body)
		}
	}
	w := p.do(t, browserRequest{method: http.MethodGet, path: "/api/session", cookie: id, site: "same-origin"})
	var got apiSession
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Version != "v9.9.9-test" ||
		got.IdleTimeoutSeconds != 1800 || !got.ExpiresAt.Equal(p.clock.Now().Add(12*time.Hour)) {
		t.Errorf("GET /api/session = %d %s", w.Code, w.Body)
	}
}

func TestStateChangesNeedOriginAndCSRF(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	form := url.Values{"csrf": {csrf}}
	for name, br := range map[string]browserRequest{
		// What a page inside code-server (same site) can send: every
		// cookie, but Sec-Fetch-Site same-site and its own Origin.
		"same-site page":           {site: "same-site", origin: "http://code-server.dev01.nakama.localhost:18888", form: form},
		"cross-site page":          {site: "cross-site", origin: "https://evil.example", form: form},
		"no Sec-Fetch-Site":        {site: "-", form: form},
		"no Origin":                {noOrigin: true, form: form},
		"Origin null":              {origin: "null", form: form},
		"Origin of another port":   {origin: "http://control.nakama.localhost:9999", form: form},
		"wrong CSRF token":         {form: url.Values{"csrf": {"guess"}}},
		"no CSRF token":            {form: url.Values{}},
		"CSRF token of other user": {csrfHeader: "x" + csrf[1:]},
	} {
		br.method, br.path, br.cookie = http.MethodPost, "/logout", id
		if w := p.do(t, br); w.Code != http.StatusForbidden {
			t.Errorf("%s: POST /logout = %d, want 403", name, w.Code)
		}
	}
	if _, ok := p.store.lookup(id); !ok {
		t.Fatal("a refused sign-out must not end the session")
	}
	if w := p.do(t, browserRequest{method: http.MethodGet, path: "/logout", cookie: id}); w.Code == http.StatusSeeOther {
		t.Error("GET /logout must not sign out")
	}
	// The header form is accepted as well (scripts of later pages).
	w := p.do(t, browserRequest{method: http.MethodPost, path: "/logout", cookie: id, csrfHeader: csrf})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/signed-out" {
		t.Fatalf("sign-out = %d %q", w.Code, w.Header().Get("Location"))
	}
	if cs := w.Result().Cookies(); len(cs) != 1 || cs[0].Name != cookieName || cs[0].MaxAge >= 0 || !cs[0].Secure {
		t.Errorf("the sign-out must delete the cookie: %+v", cs)
	}
	if w := p.do(t, browserRequest{method: http.MethodGet, path: "/", cookie: id}); w.Code != http.StatusUnauthorized {
		t.Errorf("home after sign-out = %d", w.Code)
	}
	if w := p.do(t, browserRequest{method: http.MethodGet, path: "/signed-out", site: "same-origin"}); w.Code != http.StatusOK {
		t.Errorf("signed-out page = %d", w.Code)
	}
}

func TestSessionsEndWhenIdleAndAfterTwelveHours(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	home := browserRequest{method: http.MethodGet, path: "/", cookie: id}
	p.clock.advance(sessionIdleTimeout - time.Second)
	if w := p.do(t, home); w.Code != http.StatusOK {
		t.Fatalf("a session used within %s must live: %d", sessionIdleTimeout, w.Code)
	}
	p.clock.advance(sessionIdleTimeout)
	if w := p.do(t, home); w.Code != http.StatusUnauthorized {
		t.Errorf("a session idle for %s must end: %d", sessionIdleTimeout, w.Code)
	}

	id, _ = p.signIn(t)
	home.cookie = id
	for used := time.Duration(0); used+20*time.Minute < sessionMaxAge; used += 20 * time.Minute {
		p.clock.advance(20 * time.Minute)
		if w := p.do(t, home); w.Code != http.StatusOK {
			t.Fatalf("a session in use must live until %s, it ended after %s", sessionMaxAge, used+20*time.Minute)
		}
	}
	p.clock.advance(20 * time.Minute)
	if w := p.do(t, home); w.Code != http.StatusUnauthorized {
		t.Errorf("a session in use must still end %s after the sign-in: %d", sessionMaxAge, w.Code)
	}
}

func TestSessionsAndCodesAreBounded(t *testing.T) {
	p := newTestPanel(t)
	first, _ := p.signIn(t)
	for i := 0; i < maxSessions; i++ {
		p.clock.advance(time.Second)
		p.signIn(t)
	}
	if _, ok := p.store.lookup(first); ok {
		t.Errorf("more than %d sessions must evict the oldest", maxSessions)
	}
	if n := len(p.store.sessions); n != maxSessions {
		t.Errorf("sessions = %d, want %d", n, maxSessions)
	}
	oldest, _ := p.NewSignInCode()
	for i := 0; i < maxSignInCodes; i++ {
		p.clock.advance(time.Millisecond)
		if _, err := p.NewSignInCode(); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.store.codes) != maxSignInCodes {
		t.Errorf("codes = %d, want %d", len(p.store.codes), maxSignInCodes)
	}
	if w := p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + oldest, site: "none"}); w.Code != http.StatusForbidden {
		t.Errorf("the oldest code must be dropped: %d", w.Code)
	}
}

// TestLimitsOfZeroOrLessKeepTheNewest: the eviction loops end on an
// empty map, so a bound of zero or less neither hangs the panel nor
// dereferences a missing oldest entry (review of #140).
func TestLimitsOfZeroOrLessKeepTheNewest(t *testing.T) {
	for _, limit := range []int{0, -1} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			st := newStore(nil)
			st.maxCodes, st.maxSessions = limit, limit
			var codes []string
			for i := 0; i < 3; i++ {
				c, err := st.newSignInCode()
				if err != nil {
					t.Error(err)
					return
				}
				codes = append(codes, c)
			}
			if len(st.codes) != 1 {
				t.Errorf("limit %d: %d codes kept, want the newest only", limit, len(st.codes))
			}
			if _, ok, _ := st.redeem(codes[0]); ok {
				t.Errorf("limit %d: an older code must be gone", limit)
			}
			first, ok, err := st.redeem(codes[2])
			if !ok || err != nil {
				t.Errorf("limit %d: the newest code must work: %v %v", limit, ok, err)
				return
			}
			c, _ := st.newSignInCode()
			if _, ok, _ := st.redeem(c); !ok {
				t.Errorf("limit %d: second sign-in failed", limit)
			}
			if _, ok := st.lookup(first.id); ok || len(st.sessions) != 1 {
				t.Errorf("limit %d: %d sessions, the first kept=%v; want the newest only", limit, len(st.sessions), ok)
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("limit %d: minting or redeeming does not end", limit)
		}
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	for _, br := range []browserRequest{
		{method: http.MethodGet, path: "/", cookie: id},
		{method: http.MethodGet, path: "/"},
		{method: http.MethodGet, path: "/", host: "evil.example:18888"},
		{method: http.MethodGet, path: "/assets/panel.css"},
		{method: http.MethodGet, path: "/nope"},
		{method: http.MethodGet, path: "/api/session", cookie: id},
	} {
		w := p.do(t, br)
		h := w.Header()
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") ||
			h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "same-origin" ||
			h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Cross-Origin-Resource-Policy") != "same-origin" || h.Get("Cross-Origin-Opener-Policy") != "same-origin" {
			t.Errorf("%s %s (Host %q) = %d, headers %v", br.method, br.path, br.host, w.Code, h)
		}
	}
}

func TestAssetsNeedNoSessionAndAreNotListed(t *testing.T) {
	p := newTestPanel(t)
	w := p.do(t, browserRequest{method: http.MethodGet, path: "/assets/panel.css", site: "same-origin"})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") || !strings.Contains(w.Body.String(), "body") {
		t.Errorf("stylesheet = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/assets/", "/assets", "/assets/missing.css", "/assets/../panel.go", "/assets/%2e%2e/panel.go"} {
		if w := p.do(t, browserRequest{method: http.MethodGet, path: path}); w.Code == http.StatusOK {
			t.Errorf("%s = 200 %q", path, w.Body.String()[:min(80, w.Body.Len())])
		}
	}
}

func TestPickLang(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   lang
	}{
		{"", langEnglish},
		{"ja", langJapanese},
		{"ja-JP,ja;q=0.9,en-US;q=0.8,en;q=0.7", langJapanese},
		{"en-US,en;q=0.9,ja;q=0.8", langEnglish},
		{"fr-FR,fr;q=0.9,ja;q=0.5", langJapanese},
		{"de", langEnglish},
		{"en;q=0.5, ja;q=0.6", langJapanese},
		{"ja;q=0, en", langEnglish},
		{"*", langEnglish},
	} {
		if got := pickLang(tc.header); got != tc.want {
			t.Errorf("pickLang(%q) = %s, want %s", tc.header, got, tc.want)
		}
	}
}

func TestEveryTextExistsInBothLanguages(t *testing.T) {
	for l, m := range messages {
		for other, om := range messages {
			for k := range m {
				if om[k] == "" {
					t.Errorf("%q exists in %s but not in %s", k, l, other)
				}
			}
		}
		if !strings.Contains(m["reopen"], "%s") {
			t.Errorf("%s reopen text must name the command", l)
		}
	}
}

func TestSignInURL(t *testing.T) {
	got := SignInURL(18888, "abc")
	if got != "http://control.nakama.localhost:18888/session?c=abc" {
		t.Errorf("SignInURL = %s", got)
	}
}

func TestRefusalsAreLoggedWithoutSecrets(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	code, _ := p.NewSignInCode()
	_ = p.do(t, browserRequest{method: http.MethodPost, path: "/logout", cookie: id, origin: "http://evil.example", form: url.Values{"csrf": {csrf}}})
	_ = p.do(t, browserRequest{method: http.MethodGet, path: SignInPath + "?c=" + code + "x", site: "none"})
	joined := strings.Join(*p.logs, "\n")
	if !strings.Contains(joined, "refused") {
		t.Errorf("refusals must be logged: %q", joined)
	}
	for _, secret := range []string{id, csrf, code} {
		if strings.Contains(joined, secret) {
			t.Error("a log line contains a secret")
		}
	}
}
