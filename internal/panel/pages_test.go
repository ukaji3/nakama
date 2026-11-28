package panel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/service"
)

// Tests of the panel's pages and operations (Issue #139) against the
// fake backend: every page renders in both languages, the forms build
// the CLI command lines the commands take, confirmations hold, opening
// hands the browser the right form, and the sign-in comes back.

func get(t *testing.T, p *testPanel, id, path, lang string) (int, string, http.Header) {
	t.Helper()
	w := p.do(t, browserRequest{method: http.MethodGet, path: path, cookie: id, lang: lang})
	return w.Code, w.Body.String(), w.Header()
}

func post(t *testing.T, p *testPanel, id, csrf, path string, form url.Values) (int, string, http.Header) {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", csrf)
	w := p.do(t, browserRequest{method: http.MethodPost, path: path, cookie: id, form: form})
	return w.Code, w.Body.String(), w.Header()
}

func TestEveryPageRendersInBothLanguages(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	p.fake.ops = []service.Operation{{ID: "20261128T120000Z-00000001", Title: "create dev02", Instance: "dev02",
		Args: []string{"aws", "create", "dev02", "--instance-type=t3.medium", "-o", "json"}, State: service.OperationSucceeded,
		StartedAt: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC), EndedAt: time.Date(2026, 11, 28, 12, 1, 0, 0, time.UTC)}}
	p.fake.output["20261128T120000Z-00000001"] = [2]string{`{"name":"dev02"}`, "time=… level=INFO msg=created"}
	p.fake.tunnels = []service.TunnelOverview{{Name: "dev01", Status: &service.TunnelStatus{Forwards: []service.ForwardStatus{{Spec: "8080:8080", State: "active", URL: "http://code-server.dev01.nakama.localhost:18888/"}}}}}
	p.fake.profiles = []Profile{{Active: true, AccountID: "111122223333", Alias: "work", Status: "valid"}}
	for _, lang := range []string{"en", "ja"} {
		for _, path := range []string{"/", "/?nat=1", "/instances/dev01", "/instances/box01", "/create", "/backups", "/acl", "/profiles",
			"/system", "/doctor", "/version", "/tunnels/dev01/suggest", "/ops", "/ops/20261128T120000Z-00000001"} {
			code, body, _ := get(t, p, id, path, lang)
			if code != http.StatusOK || !strings.Contains(body, "</html>") || strings.Contains(body, "internal error") {
				t.Errorf("%s %s = %d:\n%s", lang, path, code, body)
			}
			if strings.Contains(body, "<no value>") || strings.Contains(body, "%!") {
				t.Errorf("%s %s has a missing text or a broken format:\n%s", lang, path, body)
			}
		}
	}
	if _, body, _ := get(t, p, id, "/ops/20261128T120000Z-00000001", "en"); !strings.Contains(body, "&#34;name&#34;: &#34;dev02&#34;") ||
		!strings.Contains(body, "nakama aws create dev02 --instance-type=t3.medium -o json") {
		t.Errorf("the operation page must show the command and the indented result:\n%s", body)
	}
	if code, _, _ := get(t, p, id, "/instances/Bad_Name", "en"); code != http.StatusNotFound {
		t.Errorf("an invalid instance name = %d", code)
	}
	if code, _, _ := get(t, p, id, "/ops/../../etc", "en"); code == http.StatusOK {
		t.Error("an invalid operation id must not render")
	}
}

func TestHomeOffersTheAWSSignInWithoutCredentials(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	p.fake.accountErr = errors.New("no credentials")
	p.fake.profiles = []Profile{{AccountID: "444455556666", Alias: "old", Status: "expired"}}
	code, body, h := get(t, p, id, "/", "en")
	if code != http.StatusOK || !strings.Contains(body, `action="/login"`) || !strings.Contains(body, "no credentials") ||
		!strings.Contains(body, "444455556666") || strings.Contains(body, "dev01") {
		t.Errorf("home without AWS credentials = %d:\n%s", code, body)
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://*.signin.aws.amazon.com") {
		t.Errorf("the sign-in form redirects to AWS; form-action must allow it (Chrome checks redirects): %s", csp)
	}
}

func TestHomeListsInstancesWithTheirButtons(t *testing.T) {
	p := newTestPanel(t)
	id, _ := p.signIn(t)
	_, body, _ := get(t, p, id, "/", "en")
	for _, want := range []string{`action="/open/code/dev01" target="_blank"`, `action="/open/kirocrew/dev01" target="_blank"`,
		`name="action" value="stop"`, `name="action" value="start"`, `href="/instances/box01"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %s", want)
		}
	}
	if strings.Contains(body, `action="/open/code/box01"`) {
		t.Error("a bare or stopped instance has no code button")
	}
	// A running operation on dev01 disables its buttons.
	p.fake.ops = []service.Operation{{ID: "20261128T120000Z-00000009", Title: "update dev01", Instance: "dev01", State: service.OperationRunning}}
	_, body, _ = get(t, p, id, "/", "en")
	if !strings.Contains(body, `value="stop" disabled`) || !strings.Contains(body, "running an operation") {
		t.Errorf("a busy instance must show it and disable its buttons:\n%s", body)
	}
}

func TestPasswordOnlyOnRequest(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	if _, body, _ := get(t, p, id, "/instances/dev01", "en"); strings.Contains(body, "s3cret-pass") || len(p.fake.passwords) != 0 {
		t.Error("the detail page must not fetch or show the password by itself")
	}
	if code, _, _ := get(t, p, id, "/instances/dev01/password", "en"); code == http.StatusOK {
		t.Error("a GET must not show the password")
	}
	code, body, h := post(t, p, id, csrf, "/instances/dev01/password", nil)
	if code != http.StatusOK || !strings.Contains(body, "s3cret-pass") || h.Get("Cache-Control") != "no-store" {
		t.Errorf("show password = %d", code)
	}
	if strings.Contains(strings.Join(*p.logs, "\n"), "s3cret-pass") {
		t.Error("the password reached the log")
	}
}

func TestOperationsBuildTheCLICommandLines(t *testing.T) {
	for _, tc := range []struct {
		form    url.Values
		args    []string
		inst    string
		confirm confirmKind
		typed   string
		excl    bool
	}{
		{url.Values{"action": {"start"}, "name": {"dev01"}}, []string{"aws", "start", "dev01"}, "dev01", confirmNone, "", false},
		{url.Values{"action": {"stop"}, "name": {"dev01"}}, []string{"aws", "stop", "dev01"}, "dev01", confirmDialog, "", false},
		{url.Values{"action": {"create"}, "name": {"dev02"}, "instance_type": {"t3.medium"}, "cloudfront": {"1"}, "no_kiro": {"1"},
			"code_server_version": {"4.106.3"}, "setup": {"~/my setup.sh"}, "dotfiles": {"https://github.com/me/dots"}, "preset": {"kiro"},
			"with_role": {"my-role"}},
			[]string{"aws", "create", "dev02", "--instance-type=t3.medium", "--cloudfront", "--no-kiro", "--code-server-version=4.106.3",
				"--setup=~/my setup.sh", "--dotfiles=https://github.com/me/dots", "--preset=kiro", "--with-role=my-role"}, "dev02", confirmNone, "", false},
		{url.Values{"action": {"delete"}, "name": {"dev01"}, "force": {"1"}}, []string{"aws", "delete", "dev01", "--yes", "--force"}, "dev01", confirmTyped, "dev01", false},
		{url.Values{"action": {"edit_type"}, "name": {"dev01"}, "instance_type": {"t3.large"}}, []string{"aws", "edit", "dev01", "--instance-type=t3.large", "--yes"}, "dev01", confirmDialog, "", false},
		{url.Values{"action": {"edit_ebs"}, "name": {"dev01"}, "ebs": {"64"}}, []string{"aws", "edit", "dev01", "--ebs=64", "--yes"}, "dev01", confirmDialog, "", false},
		{url.Values{"action": {"update"}, "name": {"dev01"}, "keep": {"3"}}, []string{"aws", "update", "dev01", "--yes", "--keep=3"}, "dev01", confirmDialog, "", false},
		{url.Values{"action": {"provision"}, "name": {"dev01"}, "preset": {"kiro"}}, []string{"aws", "provision", "dev01", "--preset=kiro"}, "dev01", confirmNone, "", false},
		{url.Values{"action": {"backup_create"}, "name": {"dev01"}, "label": {"pre"}, "note": {"--yes; rm -rf"}},
			[]string{"aws", "backup", "create", "dev01", "--label=pre", "--note=--yes; rm -rf"}, "dev01", confirmNone, "", false},
		{url.Values{"action": {"backup_note"}, "name": {"dev01"}, "label": {"pre"}, "note": {""}}, []string{"aws", "backup", "note", "dev01", "--label=pre", "--note="}, "dev01", confirmNone, "", false},
		{url.Values{"action": {"backup_delete"}, "name": {"dev01"}, "label": {"pre"}}, []string{"aws", "backup", "delete", "dev01", "--label=pre", "--yes"}, "dev01", confirmTyped, "pre", false},
		{url.Values{"action": {"restore"}, "name": {"dev01"}, "label": {"pre"}, "instance_type": {"t3.small"}, "with_admin": {"1"}},
			[]string{"aws", "restore", "dev01", "--label=pre", "--instance-type=t3.small", "--with-admin", "--yes"}, "dev01", confirmTyped, "dev01", false},
		{url.Values{"action": {"cloudfront_add"}, "name": {"dev01"}, "kirocrew": {"1"}}, []string{"aws", "cloudfront", "add", "dev01", "--kirocrew"}, "dev01", confirmDialog, "", false},
		{url.Values{"action": {"cloudfront_delete"}, "name": {"dev01"}}, []string{"aws", "cloudfront", "delete", "dev01", "--yes"}, "dev01", confirmDialog, "", false},
		{url.Values{"action": {"acl_add"}}, []string{"aws", "acl", "add"}, "", confirmNone, "", false},
		{url.Values{"action": {"acl_add"}, "cidr": {"198.51.100.9/24"}}, []string{"aws", "acl", "add", "--", "198.51.100.0/24"}, "", confirmNone, "", false},
		{url.Values{"action": {"acl_delete"}, "cidr": {"203.0.113.7"}}, []string{"aws", "acl", "delete", "--yes", "--", "203.0.113.7"}, "", confirmDialog, "", false},
		{url.Values{"action": {"acl_delete_all"}, "keep_mine": {"1"}}, []string{"aws", "acl", "delete", "all", "--yes", "--keep-mine"}, "", confirmTyped, "all", false},
		{url.Values{"action": {"acl_prune"}, "older_than": {"30d"}}, []string{"aws", "acl", "prune", "--older-than=30d", "--yes"}, "", confirmTyped, "prune", false},
		{url.Values{"action": {"init"}, "nat": {"gateway"}}, []string{"aws", "init", "--yes", "--nat=gateway"}, "", confirmDialog, "", true},
		{url.Values{"action": {"tunnel_restart"}, "name": {"dev01"}}, []string{"aws", "tunnel", "restart", "dev01"}, "dev01", confirmNone, "", false},
	} {
		op, err := planOperation(tc.form)
		if err != nil {
			t.Errorf("%v: %v", tc.form, err)
			continue
		}
		want := append(tc.args, "-o", "json")
		if !reflect.DeepEqual(op.args, want) || op.instance != tc.inst || op.confirm != tc.confirm || op.typed != tc.typed || op.exclusive != tc.excl {
			t.Errorf("%v:\n got %q inst=%q confirm=%d typed=%q excl=%v\nwant %q inst=%q confirm=%d typed=%q excl=%v",
				tc.form, op.args, op.instance, op.confirm, op.typed, op.exclusive, want, tc.inst, tc.confirm, tc.typed, tc.excl)
		}
	}
}

func TestOperationsRefuseMalformedValues(t *testing.T) {
	for _, form := range []url.Values{
		{"action": {"start"}, "name": {"-rf"}},
		{"action": {"start"}, "name": {"Dev01"}},
		{"action": {"start"}},
		{"action": {"create"}, "name": {"dev02"}},
		{"action": {"create"}, "name": {"dev02"}, "instance_type": {"--bare"}},
		{"action": {"create"}, "name": {"dev02"}, "instance_type": {"t3.medium"}, "setup": {"a\nb"}},
		{"action": {"create"}, "name": {"dev02"}, "instance_type": {"t3.medium"}, "preset": {"evil"}},
		{"action": {"edit_ebs"}, "name": {"dev01"}, "ebs": {"-5"}},
		{"action": {"acl_add"}, "cidr": {"--yes"}},
		{"action": {"acl_add"}, "cidr": {"2001:db8::/32"}},
		{"action": {"acl_delete"}},
		{"action": {"acl_prune"}, "older_than": {"--yes"}},
		{"action": {"init"}, "nat": {"both"}},
		{"action": {"backup_delete"}, "name": {"dev01"}},
		{"action": {"shell"}, "name": {"dev01"}},
		{"action": {""}},
	} {
		if op, err := planOperation(form); err == nil {
			t.Errorf("%v must be refused, got %q", form, op.args)
		}
	}
}

func TestRunStartsAnOperationAfterItsConfirmation(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	// No confirmation needed: started at once, then its page.
	code, _, h := post(t, p, id, csrf, "/run", url.Values{"action": {"start"}, "name": {"box01"}, "back": {"/"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/ops/") || len(p.fake.started) != 1 ||
		p.fake.started[0].Instance != "box01" || p.fake.started[0].Title != "start box01" {
		t.Fatalf("start = %d %s, started %+v", code, h.Get("Location"), p.fake.started)
	}
	// A dialog: the first post shows the command, the second runs it.
	code, body, _ := post(t, p, id, csrf, "/run", url.Values{"action": {"stop"}, "name": {"dev01"}, "back": {"/"}})
	if code != http.StatusOK || !strings.Contains(body, "nakama aws stop dev01 -o json") || len(p.fake.started) != 1 {
		t.Fatalf("stop without confirmation = %d, started %d:\n%s", code, len(p.fake.started), body)
	}
	if !strings.Contains(body, `name="confirmed" value="1"`) || !strings.Contains(body, `name="action" value="stop"`) {
		t.Errorf("the confirmation must repost the form:\n%s", body)
	}
	if code, _, _ := post(t, p, id, csrf, "/run", url.Values{"action": {"stop"}, "name": {"dev01"}, "confirmed": {"1"}}); code != http.StatusSeeOther || len(p.fake.started) != 2 {
		t.Errorf("confirmed stop = %d, started %d", code, len(p.fake.started))
	}
	// Typed: the name must be typed.
	del := url.Values{"action": {"delete"}, "name": {"dev01"}, "confirmed": {"1"}, "typed": {"dev0"}}
	code, body, _ = post(t, p, id, csrf, "/run", del)
	if code != http.StatusOK || len(p.fake.started) != 2 || !strings.Contains(body, "does not match") {
		t.Fatalf("delete with a wrong name = %d, started %d", code, len(p.fake.started))
	}
	del.Set("typed", "dev01")
	if code, _, _ := post(t, p, id, csrf, "/run", del); code != http.StatusSeeOther || len(p.fake.started) != 3 ||
		!reflect.DeepEqual(p.fake.started[2].Args, []string{"aws", "delete", "dev01", "--yes", "-o", "json"}) {
		t.Errorf("confirmed delete = %d, %+v", code, p.fake.started)
	}
	// A conflict comes back as a message on the page it came from.
	p.fake.startErr = &service.OperationConflictError{Running: &service.Operation{Title: "update dev01"}}
	code, _, h = post(t, p, id, csrf, "/run", url.Values{"action": {"start"}, "name": {"dev01"}, "back": {"/instances/dev01"}})
	if code != http.StatusSeeOther || h.Get("Location") != "/instances/dev01" {
		t.Fatalf("conflict = %d %s", code, h.Get("Location"))
	}
	if _, body, _ := get(t, p, id, "/instances/dev01", "en"); !strings.Contains(body, "update dev01") || !strings.Contains(body, "still running") {
		t.Errorf("the conflict message must show once:\n%s", body)
	}
	// back must stay inside the panel.
	code, _, h = post(t, p, id, csrf, "/run", url.Values{"action": {"start"}, "name": {"Bad"}, "back": {"//evil.example/"}})
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Errorf("an outside back = %s", h.Get("Location"))
	}
}

func TestRunNeedsTheSessionChecks(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	w := p.do(t, browserRequest{method: http.MethodPost, path: "/run", cookie: id, site: "same-site",
		origin: "http://code-server.dev01.nakama.localhost:18888", form: url.Values{"csrf": {csrf}, "action": {"start"}, "name": {"dev01"}}})
	if w.Code != http.StatusForbidden || len(p.fake.started) != 0 {
		t.Errorf("a page inside code-server must not start operations: %d", w.Code)
	}
}

func TestOperationPageReloadsWhileRunningAndCancels(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	opID := "20261128T120000Z-0000000a"
	p.fake.ops = []service.Operation{{ID: opID, Title: "create dev02", State: service.OperationRunning}}
	if _, body, _ := get(t, p, id, "/ops/"+opID, "en"); !strings.Contains(body, `http-equiv="refresh" content="2"`) ||
		!strings.Contains(body, `action="/ops/`+opID+`/cancel"`) {
		t.Errorf("a running operation reloads and offers a stop:\n%s", body)
	}
	if code, _, _ := post(t, p, id, csrf, "/ops/"+opID+"/cancel", nil); code != http.StatusSeeOther || len(p.fake.cancelled) != 1 {
		t.Errorf("cancel = %d %v", code, p.fake.cancelled)
	}
	p.fake.ops[0].State = service.OperationFailed
	if _, body, _ := get(t, p, id, "/ops/"+opID, "en"); strings.Contains(body, "http-equiv") || strings.Contains(body, "/cancel") {
		t.Error("an ended operation neither reloads nor offers a stop")
	}
}

func TestOpenStreamsProgressThenTheSignInForm(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	code, body, h := post(t, p, id, csrf, "/open/code/dev01", nil)
	if code != http.StatusOK || len(p.fake.opened) != 1 {
		t.Fatalf("open = %d", code)
	}
	for _, want := range []string{"Checking the instance", "Preparing the connection",
		`<form id="nakama-auto" method="POST" action="http://code-server.dev01.nakama.localhost:18888/login?to=/">`,
		`name="password" value="s3cret-pass"`, `src="/assets/autosubmit.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("open page lacks %s:\n%s", want, body)
		}
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' http://*.nakama.localhost:18888 https://*.cloudfront.net") || h.Get("Cache-Control") != "no-store" {
		t.Errorf("open page headers: %v", h)
	}
	if strings.Contains(strings.Join(*p.logs, "\n"), "s3cret-pass") {
		t.Error("the password reached the log")
	}
	// Kiro Crew: a GET with the token.
	_, body, _ = post(t, p, id, csrf, "/open/kirocrew/dev01", nil)
	if !strings.Contains(body, `method="GET" action="http://kirocrew.dev01.nakama.localhost:18888/"`) || !strings.Contains(body, `name="token" value="tok-123"`) {
		t.Errorf("kirocrew page:\n%s", body)
	}
	// A form to anywhere else is refused.
	p.fake.nav = &Navigation{Method: "POST", Action: "https://evil.example/login", Fields: []Field{{"password", "x"}}}
	_, body, _ = post(t, p, id, csrf, "/open/code/dev01", nil)
	if strings.Contains(body, "nakama-auto") || !strings.Contains(body, "refusing to open") {
		t.Errorf("a foreign target must be refused:\n%s", body)
	}
	p.fake.nav, p.fake.openErr = nil, errors.New(`instance "dev01" is stopped, not running`)
	if _, body, _ = post(t, p, id, csrf, "/open/code/dev01", nil); !strings.Contains(body, "is stopped") || strings.Contains(body, "nakama-auto") {
		t.Errorf("an error page:\n%s", body)
	}
	if code, _, _ := post(t, p, id, csrf, "/open/shell/dev01", nil); code != http.StatusNotFound {
		t.Errorf("unknown kind = %d", code)
	}
	if code, _, _ := get(t, p, id, "/open/code/dev01", "en"); code == http.StatusOK {
		t.Error("opening needs a POST (it reveals the password)")
	}
}

func TestCheckNavigation(t *testing.T) {
	for action, ok := range map[string]bool{
		"http://code-server.dev01.nakama.localhost:18888/login?to=/": true,
		"http://kirocrew.dev01.nakama.localhost:18888/":              true,
		"https://d111111abcdef8.cloudfront.net/login?to=/":           true,
		"http://code-server.dev01.nakama.localhost:9999/":            false,
		"http://nakama.localhost.evil.example:18888/":                false,
		"https://evil.example/":                                      false,
		"http://d111111abcdef8.cloudfront.net/":                      false,
		"javascript:alert(1)":                                        false,
	} {
		err := checkNavigation(&Navigation{Method: "POST", Action: action}, 18888)
		if (err == nil) != ok {
			t.Errorf("%s: %v, want ok=%v", action, err, ok)
		}
	}
}

func TestLoginGoesToAWSAndComesBack(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	release := make(chan struct{})
	var got LoginRequest
	p.fake.login = func(ctx context.Context, req LoginRequest, onAuthURL func(string)) (*LoginResult, error) {
		got = req
		onAuthURL("https://ap-northeast-1.signin.aws.amazon.com/v1/authorize?x=1")
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &LoginResult{AccountID: "111122223333", Alias: "work"}, nil
	}
	code, _, h := post(t, p, id, csrf, "/login", url.Values{"alias": {"work"}})
	if code != http.StatusSeeOther || h.Get("Location") != "https://ap-northeast-1.signin.aws.amazon.com/v1/authorize?x=1" {
		t.Fatalf("login = %d %s", code, h.Get("Location"))
	}
	if got.Alias != "work" || got.RedirectURL != testOrigin+"/login/done" {
		t.Errorf("login request = %+v", got)
	}
	// Back from AWS (a cross-site redirect: Lax cookies only) while the
	// exchange still runs: the page waits.
	w := p.do(t, browserRequest{method: http.MethodGet, path: "/login/done?login=ok", cookie: id, site: "cross-site"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `http-equiv="refresh" content="1"`) {
		t.Fatalf("login/done while waiting = %d:\n%s", w.Code, w.Body)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		w = p.do(t, browserRequest{method: http.MethodGet, path: "/login/done?login=ok", cookie: id, site: "same-origin"})
		if w.Code == http.StatusSeeOther || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login/done after the sign-in = %d %s", w.Code, w.Header().Get("Location"))
	}
	if _, body, _ := get(t, p, id, "/", "en"); !strings.Contains(body, "Signed in to AWS as work (account 111122223333)") {
		t.Errorf("home after the sign-in:\n%s", body)
	}
	if p.fake.invalidated == 0 {
		t.Error("a sign-in must drop cached credentials")
	}
}

func TestLoginFailureIsShown(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	p.fake.login = func(context.Context, LoginRequest, func(string)) (*LoginResult, error) {
		return nil, errors.New("the account lacks SignInLocalDevelopmentAccess")
	}
	code, _, h := post(t, p, id, csrf, "/login", nil)
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("failed login = %d %s", code, h.Get("Location"))
	}
	if _, body, _ := get(t, p, id, "/", "en"); !strings.Contains(body, "SignInLocalDevelopmentAccess") {
		t.Errorf("the failure must be shown:\n%s", body)
	}
}

func TestProfileActionsRunTheProfileCommands(t *testing.T) {
	p := newTestPanel(t)
	id, csrf := p.signIn(t)
	for _, tc := range []struct {
		form url.Values
		want []string
	}{
		{url.Values{"action": {"load"}, "key": {"111122223333"}}, []string{"aws", "profile", "load", "--", "111122223333", "-o", "json"}},
		{url.Values{"action": {"delete"}, "key": {"work"}}, []string{"aws", "profile", "delete", "--", "work", "-o", "json"}},
		{url.Values{"action": {"unload"}}, []string{"aws", "profile", "unload", "-o", "json"}},
		{url.Values{"action": {"save"}, "alias": {"home"}}, []string{"aws", "profile", "save", "--alias=home", "-o", "json"}},
	} {
		p.fake.quick = nil
		code, _, h := post(t, p, id, csrf, "/profiles", tc.form)
		if code != http.StatusSeeOther || h.Get("Location") != "/profiles" || len(p.fake.quick) != 1 || !reflect.DeepEqual(p.fake.quick[0], tc.want) {
			t.Errorf("%v: %d, ran %q", tc.form, code, p.fake.quick)
		}
	}
	p.fake.quick = nil
	for _, form := range []url.Values{{"action": {"load"}}, {"action": {"load"}, "key": {"--all"}}, {"action": {"wipe"}}} {
		if _, _, _ = post(t, p, id, csrf, "/profiles", form); len(p.fake.quick) != 0 {
			t.Errorf("%v must not run anything: %q", form, p.fake.quick)
		}
	}
}

// TestEveryTextKeyExists: html/template renders a missing map key as
// an empty string, so a key used by a template or a handler but absent
// from messages would vanish silently.
func TestEveryTextKeyExists(t *testing.T) {
	keyRef := regexp.MustCompile(`(?:\.T|\$t|\$\.T)\.([A-Za-z_]+)`)
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]string{}
	for _, e := range entries {
		b, _ := templateFS.ReadFile("templates/" + e.Name())
		for _, m := range keyRef.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = e.Name()
		}
	}
	goRef := regexp.MustCompile(`(?:tr\(r, |t\[|messages\[l\]\[)"([A-Za-z_]+)"[\])]`)
	for _, f := range []string{"panel.go", "pages.go", "open.go", "login.go", "guard.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range goRef.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = f
		}
	}
	for _, stage := range []OpenStage{StageInstance, StageForward, StageSignIn} {
		used["stage_"+string(stage)] = "open.go"
	}
	for k, where := range used {
		for l, m := range messages {
			if _, ok := m[k]; !ok {
				t.Errorf("%s uses %q, missing in %s", where, k, l)
			}
		}
	}
}

func TestSafeBack(t *testing.T) {
	for in, want := range map[string]string{
		"/": "/", "/instances/dev01": "/instances/dev01", "/acl": "/acl", "/backups": "/backups",
		"//evil.example/": "/", `/\evil.example`: "/", "https://evil.example/": "/", "/instances/../x": "/",
		"/instances/Dev01": "/", "": "/", "/ops/x": "/",
	} {
		if got := safeBack(in); got != want {
			t.Errorf("safeBack(%q) = %q, want %q", in, got, want)
		}
	}
}
