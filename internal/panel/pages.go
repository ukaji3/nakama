package panel

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ukaji3/nakama/internal/service"
	"github.com/ukaji3/nakama/internal/version"
)

// The pages of the panel. Every page needs a session (accessPage);
// every form posts with the CSRF token (accessWrite). Reads call the
// Backend in this process; state changes start operations (actions.go).

// busyState says which instances have a running operation.
type busyState struct {
	Instances map[string]bool
	// All is set while an exclusive operation (init) runs, or while
	// anything runs for pages that start an exclusive one.
	All     bool
	Running []service.Operation
}

func (s *Server) busy(r *http.Request) busyState {
	b := busyState{Instances: map[string]bool{}}
	ops, err := s.backend.Operations(r.Context())
	if err != nil {
		return b
	}
	for _, o := range ops {
		if o.Done() {
			continue
		}
		b.Running = append(b.Running, o)
		if o.Exclusive {
			b.All = true
		}
		if o.Instance != "" {
			b.Instances[o.Instance] = true
		}
	}
	return b
}

type homeData struct {
	Instances []service.InstanceSummary
	ShowNAT   bool
	Err       string
	Busy      busyState
	Recent    []service.Operation
	Profiles  []Profile
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	d := homeData{ShowNAT: r.URL.Query().Get("nat") == "1", Busy: s.busy(r)}
	if _, err := s.backend.Account(r.Context()); err != nil {
		// Not signed in to AWS: the page offers the sign-in and the
		// saved profiles.
		d.Profiles, _ = s.backend.Profiles(r.Context())
	} else if list, err := s.backend.Instances(r.Context(), d.ShowNAT); err != nil {
		d.Err = err.Error()
	} else {
		d.Instances = list
	}
	if ops, err := s.backend.Operations(r.Context()); err == nil {
		if len(ops) > 5 {
			ops = ops[:5]
		}
		d.Recent = ops
	}
	// The sign-in form leaves the panel through a redirect to AWS.
	setFormAction(w, "https://*.signin.aws.amazon.com")
	s.page(w, r, sess, "home", "home", d)
}

type instanceData struct {
	Name     string
	Info     *service.InstanceInfo
	Err      string
	Password string
	Backups  []service.Backup
	Busy     bool
}

func (s *Server) instancePage(w http.ResponseWriter, r *http.Request, sess *session, withPassword bool) {
	name := r.PathValue("name")
	if err := service.ValidateInstanceName(name); err != nil {
		s.handleNotFound(w, r)
		return
	}
	busy := s.busy(r)
	d := instanceData{Name: name, Busy: busy.Instances[name] || busy.All}
	info, err := s.backend.Instance(r.Context(), name, withPassword)
	if err != nil {
		d.Err = err.Error()
	} else {
		d.Info = info
		if withPassword {
			d.Password = info.Password
		}
		d.Backups, _ = s.backend.Backups(r.Context(), name)
	}
	refresh := 0
	if d.Info != nil && transitional(d.Info.State) || d.Busy || anyPending(d.Backups) {
		// Starting, stopping or an operation running: show the change.
		refresh = 5
	}
	s.pageRefresh(w, r, sess, "home", "instance", d, refresh)
}

// anyPending reports a backup whose snapshot is not done yet.
func anyPending(list []service.Backup) bool {
	for _, b := range list {
		if b.State == "pending" {
			return true
		}
	}
	return false
}

// transitional reports an EC2 state on its way to another one.
func transitional(state string) bool {
	switch state {
	case "pending", "stopping", "shutting-down":
		return true
	}
	return false
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	s.instancePage(w, r, sess, false)
}

// handleInstancePassword shows the page with the code-server password
// (ADR 0031 decision 7: only on request). It is a POST so that the
// password is never fetched by a link, a prefetch or a reload.
func (s *Server) handleInstancePassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessWrite)
	if !ok {
		return
	}
	s.instancePage(w, r, sess, true)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	s.page(w, r, sess, "create", "create", nil)
}

type backupsData struct {
	Backups []service.Backup
	Err     string
	Busy    busyState
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	d := backupsData{Busy: s.busy(r)}
	list, err := s.backend.Backups(r.Context(), "")
	if err != nil {
		d.Err = err.Error()
	}
	d.Backups = list
	refresh := 0
	if anyPending(list) {
		refresh = 10
	}
	s.pageRefresh(w, r, sess, "backups", "backups", d, refresh)
}

type aclData struct {
	Entries []service.ACLEntry
	Err     string
}

func (s *Server) handleACL(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	var d aclData
	list, err := s.backend.ACL(r.Context())
	if err != nil {
		d.Err = err.Error()
	}
	d.Entries = list
	s.page(w, r, sess, "acl", "acl", d)
}

type profilesData struct {
	Profiles []Profile
	Err      string
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	var d profilesData
	list, err := s.backend.Profiles(r.Context())
	if err != nil {
		d.Err = err.Error()
	}
	d.Profiles = list
	setFormAction(w, "https://*.signin.aws.amazon.com")
	s.page(w, r, sess, "profiles", "profiles", d)
}

// handleProfileAction runs `nakama aws profile load|unload|delete|save`
// (local and quick: no operation is needed).
func (s *Server) handleProfileAction(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessWrite)
	if !ok {
		return
	}
	key, err := text(r.PostForm, "key", 128)
	if err == nil && strings.HasPrefix(key, "-") {
		err = errors.New("invalid profile")
	}
	alias, aerr := text(r.PostForm, "alias", 128)
	if err == nil {
		err = aerr
	}
	var args []string
	switch r.PostFormValue("action") {
	case "load", "delete":
		if key == "" {
			err = errors.New("no profile given")
		}
		args = []string{"aws", "profile", r.PostFormValue("action"), "--", key}
	case "unload":
		args = []string{"aws", "profile", "unload"}
	case "save":
		args = flagValue([]string{"aws", "profile", "save"}, "alias", alias)
	default:
		err = errors.New("unknown profile action")
	}
	if err != nil {
		s.flashAndRedirect(w, r, sess, err.Error(), true, "/profiles")
		return
	}
	out, err := s.backend.RunQuick(r.Context(), append(args, "-o", "json"))
	s.backend.InvalidateCredentials()
	if err != nil {
		s.flashAndRedirect(w, r, sess, err.Error(), true, "/profiles")
		return
	}
	s.flashAndRedirect(w, r, sess, prettyJSON(out), false, "/profiles")
}

type systemData struct {
	Tunnels []service.TunnelOverview
	Err     string
	Busy    busyState
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	d := systemData{Busy: s.busy(r)}
	list, err := s.backend.Tunnels(r.Context())
	if err != nil {
		d.Err = err.Error()
	}
	d.Tunnels = list
	s.page(w, r, sess, "system", "system", d)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	s.page(w, r, sess, "system", "doctor", s.backend.Doctor(r.Context()))
}

type versionData struct {
	Result version.CheckResult
	Err    string
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	res := s.backend.CheckVersion(r.Context())
	d := versionData{Result: res}
	if res.Err != nil {
		d.Err = res.Err.Error()
	}
	s.page(w, r, sess, "system", "version", d)
}

type suggestData struct {
	Name       string
	Suggestion *service.TunnelSuggestion
	Err        string
}

func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := service.ValidateInstanceName(name); err != nil {
		s.handleNotFound(w, r)
		return
	}
	d := suggestData{Name: name}
	sug, err := s.backend.SuggestTunnel(r.Context(), name)
	if err != nil {
		d.Err = err.Error()
	}
	d.Suggestion = sug
	s.page(w, r, sess, "system", "suggest", d)
}

type opsData struct {
	Ops []service.Operation
	Err string
}

func (s *Server) handleOps(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	var d opsData
	ops, err := s.backend.Operations(r.Context())
	if err != nil {
		d.Err = err.Error()
	}
	d.Ops = ops
	s.page(w, r, sess, "ops", "ops", d)
}

type opData struct {
	Op     *service.Operation
	Result string
	Log    string
}

func (s *Server) handleOp(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	op, err := s.backend.Operation(r.Context(), r.PathValue("id"))
	if err != nil {
		s.handleNotFound(w, r)
		return
	}
	stdout, log, _ := s.backend.OperationOutput(op.ID)
	d := pageData{CSRF: sess.csrf, Nav: "ops", Data: opData{Op: op, Result: stdout, Log: log}}
	d.Flash, d.FlashErr = s.store.takeFlash(sess.id)
	if acct, err := s.backend.Account(r.Context()); err == nil {
		d.Account = acct
	} else {
		d.AccountErr = err.Error()
	}
	if !op.Done() {
		d.Refresh = 2
	}
	s.render(w, r, http.StatusOK, "op", d)
}

func (s *Server) handleOpCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessWrite)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.backend.CancelOperation(r.Context(), id); err != nil {
		s.flashAndRedirect(w, r, sess, err.Error(), true, "/ops/"+url.PathEscape(id))
		return
	}
	s.flashAndRedirect(w, r, sess, tr(r, "opCancelled"), false, "/ops/"+url.PathEscape(id))
}

type confirmData struct {
	Line   string
	Fields []Field
	// Irreversible operations need Word typed in.
	Irreversible bool
	Word         string
	// Wrong is set when the typed confirmation did not match.
	Wrong bool
}

// handleRun starts an operation from a form, after its confirmation.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessWrite)
	if !ok {
		return
	}
	back := safeBack(r.PostFormValue("back"))
	op, err := planOperation(r.PostForm)
	if err != nil {
		s.flashAndRedirect(w, r, sess, fmt.Sprintf(tr(r, "invalidInput"), err.Error()), true, back)
		return
	}
	if op.confirm != confirmNone && r.PostFormValue("confirmed") != "1" ||
		op.confirm == confirmTyped && r.PostFormValue("typed") != op.typed {
		var fields []Field
		for k, vs := range r.PostForm {
			if k == "csrf" || k == "confirmed" || k == "typed" {
				continue
			}
			for _, v := range vs {
				fields = append(fields, Field{Name: k, Value: v})
			}
		}
		d := confirmData{Line: commandLine(op.args), Fields: fields,
			Irreversible: op.confirm == confirmTyped, Word: op.typed,
			Wrong: r.PostFormValue("confirmed") == "1"}
		s.page(w, r, sess, "ops", "confirm", d)
		return
	}
	started, err := s.backend.StartOperation(r.Context(), service.OperationInput{
		Title: op.title, Instance: op.instance, Exclusive: op.exclusive, Args: op.args,
	})
	if err != nil {
		s.flashAndRedirect(w, r, sess, err.Error(), true, back)
		return
	}
	s.logf("control panel: started %s (%s)", started.Title, started.ID)
	http.Redirect(w, r, "/ops/"+url.PathEscape(started.ID), http.StatusSeeOther)
}

// backPattern lists the pages a form may return to: a fixed set of the
// panel's own paths, so "back" can never send the browser elsewhere
// (no "//host", no "/\\host").
var backPattern = regexp.MustCompile(`^/(instances/[a-z0-9-]{2,32}|create|backups|acl|system|profiles|ops)?$`)

// safeBack returns back when it is one of the panel's pages, "/"
// otherwise.
func safeBack(back string) string {
	if backPattern.MatchString(back) {
		return back
	}
	return "/"
}

// setFormAction widens the page's form-action to also allow origins a
// form may leave the panel for (Chrome checks redirects too).
func setFormAction(w http.ResponseWriter, origins ...string) {
	csp := strings.Replace(contentSecurityPolicy, "form-action 'self'", "form-action 'self' "+strings.Join(origins, " "), 1)
	w.Header().Set("Content-Security-Policy", csp)
}
