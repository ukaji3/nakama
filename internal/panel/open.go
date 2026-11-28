package panel

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ukaji3/nakama/internal/service"
)

// Opening code-server and Kiro Crew (ADR 0031 decision 5). The button
// posts a form with target=_blank, so the browser opens the new tab
// itself, on the click (no pop-up blocker is involved, PoC of Issue
// #139). The answer is streamed: the page says what it is waiting for
// (the SSM forward can take a minute after a start), and ends with the
// form that signs the browser in to code-server (POST of the password)
// or Kiro Crew (GET with the token), submitted by autosubmit.js once the
// page is complete. The secrets live in that one page only (no-store).

var openPageHead = template.Must(template.New("openhead").Parse(`<!DOCTYPE html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/assets/panel.css">
<script src="/assets/autosubmit.js" defer></script>
</head>
<body>
<header class="bar"><span class="brand">nakama</span></header>
<main>
<h1>{{.Title}}</h1>
<ul class="progress">
`))

var openPageStep = template.Must(template.New("openstep").Parse(`<li>{{.}}</li>
`))

var openPageForm = template.Must(template.New("openform").Parse(`</ul>
{{range .Notes}}<p class="flash error">{{.}}</p>
{{end}}<form id="nakama-auto" method="{{.Method}}" action="{{.Action}}">
{{range .Fields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{end}}<button type="submit">{{.Continue}}</button>
</form>
</main>
</body>
</html>
`))

var openPageError = template.Must(template.New("openerror").Parse(`</ul>
<p class="flash error">{{.Err}}</p>
<p><a href="/">{{.Back}}</a></p>
</main>
</body>
</html>
`))

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.check(w, r, accessWrite); !ok {
		return
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	if (kind != "code" && kind != "kirocrew") || service.ValidateInstanceName(name) != nil {
		s.handleNotFound(w, r)
		return
	}
	opts := OpenOptions{Internet: r.PostFormValue("internet") == "1", NoAuth: r.PostFormValue("noauth") == "1"}
	l := pickLang(r.Header.Get("Accept-Language"))
	t := messages[l]
	// The form goes to the router's address of the instance, or to its
	// CloudFront distribution with --internet.
	setFormAction(w, "http://*.nakama.localhost:"+strconv.Itoa(s.routerPort), "https://*.cloudfront.net")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", string(l))
	title := fmt.Sprintf(t["openCodeTitle"], name)
	if kind == "kirocrew" {
		title = fmt.Sprintf(t["openKiroCrewTitle"], name)
	}
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	_ = openPageHead.Execute(w, map[string]any{"Lang": l, "Title": title})
	flush()
	progress := func(stage OpenStage) {
		_ = openPageStep.Execute(w, t["stage_"+string(stage)])
		flush()
	}
	var nav *Navigation
	var err error
	if kind == "code" {
		nav, err = s.backend.OpenCode(r.Context(), name, opts, progress)
	} else {
		nav, err = s.backend.OpenKiroCrew(r.Context(), name, opts, progress)
	}
	if err == nil {
		err = checkNavigation(nav, s.routerPort)
	}
	if err != nil {
		s.logf("control panel: open %s of %s: %v", kind, name, err)
		_ = openPageError.Execute(w, map[string]any{"Err": err.Error(), "Back": t["backHome"]})
		return
	}
	_ = openPageForm.Execute(w, map[string]any{
		"Method": nav.Method, "Action": nav.Action, "Fields": nav.Fields, "Notes": nav.Notes, "Continue": t["continue"],
	})
}

// checkNavigation makes sure the form only goes where code and kirocrew
// go: the router's instance addresses or a CloudFront distribution.
func checkNavigation(nav *Navigation, routerPort int) error {
	if nav == nil {
		return fmt.Errorf("nothing to open")
	}
	if nav.Method != http.MethodPost && nav.Method != http.MethodGet {
		return fmt.Errorf("unexpected form method %q", nav.Method)
	}
	u, err := url.Parse(nav.Action)
	if err != nil {
		return err
	}
	switch {
	case u.Scheme == "http" && u.Port() == strconv.Itoa(routerPort) && hasSuffixFold(u.Hostname(), ".nakama.localhost"):
	case u.Scheme == "https" && hasSuffixFold(u.Hostname(), ".cloudfront.net"):
	default:
		return fmt.Errorf("refusing to open %s", u.Redacted())
	}
	return nil
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) > len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}
