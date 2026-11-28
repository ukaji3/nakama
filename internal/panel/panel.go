// Package panel is the browser side of the nakama control panel (Issue
// #139, ADR 0031): the pages served at
// http://control.nakama.localhost:<router port>/, their session
// management and request checks. The process side (listening, the
// state file, the control channel) is service.RunPanel; what the pages
// do with AWS is the Backend, implemented by the CLI. `nakama ui` (and
// nakama without arguments) wires them together.
package panel

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
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
// again (nakama without arguments; `nakama ui` does the same).
const ReopenCommand = "nakama"

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
	// Backend does the work (required for every page but the sign-in).
	Backend Backend
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
	routerPort   int
	version      string
	backend      Backend
	store        *store
	now          func() time.Time
	peerCheck    func(*http.Request) error
	logf         func(format string, args ...any)
	cop          *http.CrossOriginProtection
	pages        map[string]*template.Template
	assets       map[string]asset
	mux          *http.ServeMux

	loginMu sync.Mutex
	login   *loginAttempt
}

// pageNames are the templates in templates/ besides the layout.
var pageNames = []string{
	"home", "message", "instance", "create", "backups", "acl", "profiles", "system",
	"doctor", "version", "suggest", "ops", "op", "confirm", "login_done",
}

var templateFuncs = template.FuncMap{
	"cmdline": commandLine,
	"pretty":  prettyJSON,
	"stateClass": func(state string) string {
		switch state {
		case "running", "available", "completed", "succeeded", "ok", "active", "valid", "up-to-date":
			return "good"
		case "stopped", "failed", "fail", "expired", "dead", "interrupted", "outdated":
			return "bad"
		case "skip", "unknown", "development", "cancelled":
			return "muted"
		default:
			return "pending"
		}
	},
	"ts": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05")
	},
	"has": func(m map[string]bool, k string) bool { return m[k] },
	// dict passes named values to a shared template.
	"dict": func(kv ...any) (map[string]any, error) {
		if len(kv)%2 != 0 {
			return nil, fmt.Errorf("dict: odd number of arguments")
		}
		m := make(map[string]any, len(kv)/2)
		for i := 0; i < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
			}
			m[k] = kv[i+1]
		}
		return m, nil
	},
}

// New returns the panel application.
func New(cfg Config) *Server {
	s := &Server{
		host:       service.PanelHost(cfg.RouterPort),
		origin:     service.PanelOrigin(cfg.RouterPort),
		routerPort: cfg.RouterPort,
		version:    cfg.Version,
		backend:    cfg.Backend,
		store:      newStore(cfg.Now),
		now:        cfg.Now,
		peerCheck:  cfg.PeerCheck,
		logf:       cfg.Logf,
		cop:        http.NewCrossOriginProtection(),
		pages:      map[string]*template.Template{},
		assets:     loadAssets(),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.peerCheck == nil {
		s.peerCheck = peeruid.Check
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	for _, page := range pageNames {
		s.pages[page] = template.Must(template.New(page).Funcs(templateFuncs).
			ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+page+".html"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET "+SignInPath, s.handleSignIn)
	mux.HandleFunc("POST /logout", s.handleSignOut)
	mux.HandleFunc("GET /signed-out", s.handleSignedOut)
	mux.HandleFunc("GET /api/session", s.handleAPISession)
	mux.HandleFunc("GET /assets/{file}", s.handleAsset)

	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /login/done", s.handleLoginDone)
	mux.HandleFunc("GET /instances/{name}", s.handleInstance)
	mux.HandleFunc("POST /instances/{name}/password", s.handleInstancePassword)
	mux.HandleFunc("GET /create", s.handleCreate)
	mux.HandleFunc("GET /backups", s.handleBackups)
	mux.HandleFunc("GET /acl", s.handleACL)
	mux.HandleFunc("GET /profiles", s.handleProfiles)
	mux.HandleFunc("POST /profiles", s.handleProfileAction)
	mux.HandleFunc("GET /system", s.handleSystem)
	mux.HandleFunc("GET /doctor", s.handleDoctor)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /tunnels/{name}/suggest", s.handleSuggest)
	mux.HandleFunc("GET /ops", s.handleOps)
	mux.HandleFunc("GET /ops/{id}", s.handleOp)
	mux.HandleFunc("POST /ops/{id}/cancel", s.handleOpCancel)
	mux.HandleFunc("POST /run", s.handleRun)
	mux.HandleFunc("POST /open/{kind}/{name}", s.handleOpen)
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
	// Nav marks the current section in the header.
	Nav string
	// Account is the AWS identity; AccountErr why there is none.
	Account    *Account
	AccountErr string
	Flash      string
	FlashErr   bool
	// Refresh reloads the page after this many seconds (running
	// operations, a sign-in in progress).
	Refresh int
	// Message pages.
	Title  string
	Body   string
	Reopen template.HTML
	// Data is the page's own content.
	Data any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, d pageData) {
	l := pickLang(r.Header.Get("Accept-Language"))
	d.Lang, d.T, d.Version = l, messages[l], s.version
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", d); err != nil {
		s.logf("control panel: render %s: %v", page, err)
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString("internal error")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", string(l))
	w.Header().Add("Vary", "Accept-Language")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// page renders a page of a signed-in session: the header shows the AWS
// account, and a message stored by the previous POST is shown once.
func (s *Server) page(w http.ResponseWriter, r *http.Request, sess *session, nav, page string, data any) {
	s.pageRefresh(w, r, sess, nav, page, data, 0)
}

// pageRefresh is page that reloads itself after refresh seconds (0:
// never), while something it shows is changing.
func (s *Server) pageRefresh(w http.ResponseWriter, r *http.Request, sess *session, nav, page string, data any, refresh int) {
	d := pageData{CSRF: sess.csrf, Nav: nav, Data: data, Refresh: refresh}
	d.Flash, d.FlashErr = s.store.takeFlash(sess.id)
	if s.backend != nil {
		acct, err := s.backend.Account(r.Context())
		if err != nil {
			d.AccountErr = err.Error()
		} else {
			d.Account = acct
		}
	}
	s.render(w, r, http.StatusOK, page, d)
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

// flashAndRedirect stores a message for the next page and sends the
// browser there (POST-redirect-GET).
func (s *Server) flashAndRedirect(w http.ResponseWriter, r *http.Request, sess *session, msg string, isErr bool, to string) {
	s.store.setFlash(sess.id, msg, isErr)
	// to is a constant, a safeBack() result (an allowlist of the panel's
	// own paths) or "/ops/" + url.PathEscape(id): never another host.
	http.Redirect(w, r, to, http.StatusSeeOther) // #nosec G710 -- see above
}

// tr returns the text of key in the request's language.
func tr(r *http.Request, key string) string {
	return messages[pickLang(r.Header.Get("Accept-Language"))][key]
}

// prettyJSON indents a JSON document; other text is returned as is.
func prettyJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return s
	}
	return buf.String()
}
