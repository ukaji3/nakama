// Package panel is the browser side of the nakama control panel (Issue
// #139, ADR 0031): the pages served at
// http://control.nakama.localhost:<router port>/, their session
// management and request checks. The process side (listening, the
// state file, the control channel) is service.RunPanel; `nakama ui`
// wires the two together.
package panel

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/ukaji3/nakama/internal/peeruid"
	"github.com/ukaji3/nakama/internal/service"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// SignInPath is where a browser redeems its one-time sign-in code.
const SignInPath = "/session"

// ReopenCommand is the command the pages name for opening the panel
// again.
const ReopenCommand = "nakama ui"

// SignInURL is the address `nakama ui` opens in the browser.
func SignInURL(routerPort int, code string) string {
	return service.PanelOrigin(routerPort) + SignInPath + "?" + url.Values{"c": {code}}.Encode()
}

// Config parameterizes the panel.
type Config struct {
	// RouterPort is the router port the browser uses; the panel answers
	// only for control.nakama.localhost:<RouterPort>.
	RouterPort int
	// Version is shown in the footer.
	Version string
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// PeerCheck refuses connections from other OS users; nil means
	// peeruid.Check. The direct peer is normally the router.
	PeerCheck func(*http.Request) error
	// Logf reports refused requests and sign-ins (the window of
	// `nakama ui`); nil discards them. Secrets are never passed to it.
	Logf func(format string, args ...any)
}

// Server is the control panel's HTTP application.
type Server struct {
	host, origin string
	version      string
	store        *store
	peerCheck    func(*http.Request) error
	logf         func(format string, args ...any)
	cop          *http.CrossOriginProtection
	pages        map[string]*template.Template
	assets       map[string]asset
	mux          *http.ServeMux
}

// New returns the panel application.
func New(cfg Config) *Server {
	s := &Server{
		host:      service.PanelHost(cfg.RouterPort),
		origin:    service.PanelOrigin(cfg.RouterPort),
		version:   cfg.Version,
		store:     newStore(cfg.Now),
		peerCheck: cfg.PeerCheck,
		logf:      cfg.Logf,
		cop:       http.NewCrossOriginProtection(),
		pages:     map[string]*template.Template{},
		assets:    loadAssets(),
	}
	if s.peerCheck == nil {
		s.peerCheck = peeruid.Check
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	for _, page := range []string{"home", "message"} {
		s.pages[page] = template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET "+SignInPath, s.handleSignIn)
	mux.HandleFunc("POST /logout", s.handleSignOut)
	mux.HandleFunc("GET /signed-out", s.handleSignedOut)
	mux.HandleFunc("GET /api/session", s.handleAPISession)
	mux.HandleFunc("GET /assets/{file}", s.handleAsset)
	mux.HandleFunc("/", s.handleNotFound)
	s.mux = mux
	return s
}

// Handler returns the HTTP handler of the panel.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		// Every response, even an error, first passes the Host and the
		// OS user checks (DNS rebinding, other users of the machine).
		if !s.checkHostAndPeer(w, r) {
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// NewSignInCode mints a one-time sign-in code for SignInURL.
func (s *Server) NewSignInCode() (string, error) { return s.store.newSignInCode() }

// asset is an embedded stylesheet or script.
type asset struct {
	contentType string
	body        []byte
}

// loadAssets reads the embedded assets once; only these names are
// served, so a request can never name another file or a directory.
func loadAssets() map[string]asset {
	out := map[string]asset{}
	entries, err := fs.ReadDir(assetFS, "assets")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(assetFS, path.Join("assets", e.Name()))
		if err != nil {
			panic(err)
		}
		ctype := mime.TypeByExtension(path.Ext(e.Name()))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		out[e.Name()] = asset{contentType: ctype, body: b}
	}
	return out
}

// handleAsset serves an embedded asset. Assets hold nothing secret, so
// they need no session (the Host and the OS user were checked in
// Handler).
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := s.assets[r.PathValue("file")]
	if !ok {
		s.handleNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.contentType)
	_, _ = w.Write(a.body)
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "home", pageData{CSRF: sess.csrf})
}

// handleSignIn redeems the sign-in code and starts a session. A session
// this browser already had ends: one browser, one session.
func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	sess, ok, err := s.store.redeem(r.URL.Query().Get("c"))
	if err != nil {
		s.logf("control panel: sign-in failed: %v", err)
		s.message(w, r, http.StatusInternalServerError, "internalErrorTitle", "internalErrorBody", false)
		return
	}
	if !ok {
		s.logf("control panel: refused a sign-in link (expired or already used)")
		s.message(w, r, http.StatusForbidden, "linkUsedTitle", "linkUsedBody", true)
		return
	}
	for _, c := range r.Cookies() {
		if c.Name == cookieName {
			s.store.end(c.Value)
		}
	}
	http.SetCookie(w, sessionCookie(sess.id, 0))
	s.logf("control panel: a browser signed in")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessWrite)
	if !ok {
		return
	}
	s.store.end(sess.id)
	http.SetCookie(w, sessionCookie("", -1))
	http.Redirect(w, r, "/signed-out", http.StatusSeeOther)
}

func (s *Server) handleSignedOut(w http.ResponseWriter, r *http.Request) {
	s.message(w, r, http.StatusOK, "signedOutTitle", "signedOutBody", true)
}

// apiSession is the answer of GET /api/session.
type apiSession struct {
	Version            string    `json:"version"`
	IdleTimeoutSeconds int       `json:"idleTimeoutSeconds"`
	ExpiresAt          time.Time `json:"expiresAt"`
}

func (s *Server) handleAPISession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, apiSession{
		Version:            s.version,
		IdleTimeoutSeconds: int(sessionIdleTimeout / time.Second),
		ExpiresAt:          sess.expiresAt().UTC(),
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if isAPI(r) {
		writeJSON(w, http.StatusNotFound, apiError{Error: "not found"})
		return
	}
	s.message(w, r, http.StatusNotFound, "notFoundTitle", "notFoundBody", false)
}

// sessionCookie is the session cookie (maxAge -1 deletes it).
func sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		// Lax, not Strict: the browser comes back from the AWS sign-in
		// page through a redirect chain that starts on another site, and
		// Strict cookies are not sent then (PoC of Issue #139). GET
		// requests change nothing, and every other request is checked
		// for its origin and CSRF token.
		SameSite: http.SameSiteLaxMode,
	}
}

// pageData is what the page templates get.
type pageData struct {
	Lang    lang
	T       map[string]string
	CSRF    string
	Version string
	Title   string
	Body    string
	Reopen  template.HTML
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, d pageData) {
	l := pickLang(r.Header.Get("Accept-Language"))
	d.Lang, d.T, d.Version = l, messages[l], s.version
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", string(l))
	w.Header().Add("Vary", "Accept-Language")
	w.WriteHeader(status)
	if err := s.pages[page].ExecuteTemplate(w, "layout", d); err != nil {
		s.logf("control panel: render %s: %v", page, err)
	}
}

// message renders a page with a title and a text, and the command that
// opens the panel again when reopen is set.
func (s *Server) message(w http.ResponseWriter, r *http.Request, status int, titleKey, bodyKey string, reopen bool) {
	l := pickLang(r.Header.Get("Accept-Language"))
	d := pageData{Title: messages[l][titleKey], Body: messages[l][bodyKey]}
	if reopen {
		// The format is a constant of this package and the command is
		// escaped, so the result is safe HTML.
		d.Reopen = template.HTML(fmt.Sprintf(messages[l]["reopen"], "<code>"+template.HTMLEscapeString(ReopenCommand)+"</code>")) // #nosec G203 -- see above
	}
	s.render(w, r, status, "message", d)
}
