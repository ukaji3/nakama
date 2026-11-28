package panel

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// The request checks of the control panel (ADR 0031, verified in the
// PoC of Issue #139 with Chrome 158 and Firefox 160):
//
//   - Host must be control.nakama.localhost:<router port> exactly: a DNS
//     rebinding page reaches this port with its own name.
//   - The client must be this OS user (Linux, peeruid).
//   - Every page and API needs a session (the cookie) except the
//     sign-in link, the signed-out page and the stylesheet.
//   - All *.nakama.localhost hosts are one site, so a page inside
//     code-server (a dev server under /proxy/<port>/, say) sends every
//     cookie along, SameSite=Strict ones included. SameSite therefore
//     protects nothing here: API reads need Sec-Fetch-Site:
//     same-origin, and every state-changing request needs POST,
//     Sec-Fetch-Site: same-origin, the panel's own Origin,
//     http.CrossOriginProtection and the session's CSRF token.
//   - Embedded requests (iframes and the like) are refused, besides
//     frame-ancestors 'none'.

type access int

const (
	// accessPage is a GET of an HTML page: a session is enough.
	accessPage access = iota
	// accessRead is a GET of the API.
	accessRead
	// accessWrite changes state (form POST or API POST).
	accessWrite
)

// csrfHeader carries the CSRF token on API requests made by the pages'
// scripts; forms send the csrf field instead.
const csrfHeader = "X-Nakama-CSRF"

// contentSecurityPolicy allows this origin's own stylesheets, scripts
// and forms only. Pages whose forms leave the panel (the AWS sign-in,
// code-server) set their own policy with those origins in form-action:
// Chrome checks form-action on redirects too.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	// same-origin, not no-referrer: with no-referrer a same-origin form
	// POST carries "Origin: null" (PoC of Issue #139), and the Origin
	// check below needs the real one. Other origins still get no
	// Referer.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
}

// apiError is the body of a refused API request.
type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func isAPI(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/") }

// checkHostAndPeer runs before everything else.
func (s *Server) checkHostAndPeer(w http.ResponseWriter, r *http.Request) bool {
	if !strings.EqualFold(r.Host, s.host) && !hostWithRootDot(r.Host, s.host) {
		s.logf("control panel: refused a request for another host name")
		s.refuse(w, r, http.StatusMisdirectedRequest, "notPanelTitle", "notPanelBody", "misdirected request")
		return false
	}
	if err := s.peerCheck(r); err != nil {
		s.logf("control panel: refused a request: %v", err)
		s.refuse(w, r, http.StatusForbidden, "otherUserTitle", "otherUserBody", "forbidden")
		return false
	}
	return true
}

// hostWithRootDot accepts "control.nakama.localhost.:18888".
func hostWithRootDot(got, want string) bool {
	name, port, ok := strings.Cut(want, ":")
	return ok && strings.EqualFold(got, name+".:"+port)
}

// check applies the session and origin checks of a, and returns the
// session. On false the response is written.
func (s *Server) check(w http.ResponseWriter, r *http.Request, a access) (*session, bool) {
	var sess *session
	for _, c := range r.Cookies() {
		if c.Name != cookieName {
			continue
		}
		if x, ok := s.store.lookup(c.Value); ok {
			sess = x
			break
		}
	}
	if sess == nil {
		s.refuse(w, r, http.StatusUnauthorized, "sessionEndedTitle", "sessionEndedBody", "not signed in")
		return nil, false
	}
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "iframe", "frame", "embed", "object", "fencedframe":
		s.logf("control panel: refused an embedded request for %s", r.URL.Path)
		s.refuse(w, r, http.StatusForbidden, "refusedTitle", "refusedBody", "embedded requests are refused")
		return nil, false
	}
	if a == accessPage {
		return sess, true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "same-origin" {
		s.logf("control panel: refused %s %s (Sec-Fetch-Site %q)", r.Method, r.URL.Path, site)
		s.refuse(w, r, http.StatusForbidden, "refusedTitle", "refusedBody", "cross-origin request")
		return nil, false
	}
	if a == accessRead {
		return sess, true
	}
	if r.Method != http.MethodPost {
		s.refuse(w, r, http.StatusMethodNotAllowed, "methodTitle", "methodBody", "method not allowed")
		return nil, false
	}
	if r.Header.Get("Origin") != s.origin {
		s.logf("control panel: refused %s %s (Origin %q)", r.Method, r.URL.Path, r.Header.Get("Origin"))
		s.refuse(w, r, http.StatusForbidden, "refusedTitle", "refusedBody", "cross-origin request")
		return nil, false
	}
	if err := s.cop.Check(r); err != nil {
		s.logf("control panel: refused %s %s: %v", r.Method, r.URL.Path, err)
		s.refuse(w, r, http.StatusForbidden, "refusedTitle", "refusedBody", "cross-origin request")
		return nil, false
	}
	token := r.Header.Get(csrfHeader)
	if token == "" {
		token = r.PostFormValue("csrf")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
		s.logf("control panel: refused %s %s (CSRF token)", r.Method, r.URL.Path)
		s.refuse(w, r, http.StatusForbidden, "refusedTitle", "refusedBody", "invalid CSRF token")
		return nil, false
	}
	return sess, true
}

// refuse answers an API request with JSON and a page request with an
// HTML page; the page names the command that reopens the panel when
// the browser has no session.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, status int, titleKey, bodyKey, apiMessage string) {
	if isAPI(r) {
		writeJSON(w, status, apiError{Error: apiMessage})
		return
	}
	s.message(w, r, status, titleKey, bodyKey, status == http.StatusUnauthorized)
}
