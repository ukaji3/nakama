package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/panel"
	"github.com/ukaji3/nakama/internal/profilestore"
	"github.com/ukaji3/nakama/internal/service"
	"github.com/ukaji3/nakama/internal/signin"
	"github.com/ukaji3/nakama/internal/version"
)

// uiBackend is the control panel's Backend (Issue #139, ADR 0031): the
// same service calls and helpers as the commands, so that a button does
// what the command does. Opening code-server and Kiro Crew follows
// `nakama aws code` / `kirocrew` step by step, but hands the browser a
// form instead of starting one.
type uiBackend struct {
	newClients func(ctx context.Context) (*awsx.Clients, error)
	login      loginDeps
	profiles   profileDeps
	// runQuick runs a short CLI command (tests substitute it).
	runQuick func(ctx context.Context, args []string) (stdout, stderr []byte, err error)
	now      func() time.Time

	mu        sync.Mutex
	clients   *awsx.Clients
	clientsAt time.Time
	// clientsFor is the active profile the cached clients belong to.
	clientsFor string
}

// clientsTTL is how long the panel reuses resolved credentials; a page
// load would otherwise ask STS every time.
const clientsTTL = 2 * time.Minute

func newUIBackend() *uiBackend {
	return &uiBackend{
		newClients: awsx.NewClients,
		login:      defaultLoginDeps(),
		profiles:   defaultProfileDeps(),
		runQuick:   runSelf,
		now:        time.Now,
	}
}

// runSelf runs this nakama binary with args (no standard input).
func runSelf(ctx context.Context, args []string) ([]byte, []byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...) // #nosec G204 -- this binary; args built by internal/panel
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (b *uiBackend) activeProfile() string {
	data, err := b.profiles.newStore().Load()
	if err != nil {
		return ""
	}
	return data.Active
}

func (b *uiBackend) awsClients(ctx context.Context) (*awsx.Clients, error) {
	key := b.activeProfile()
	b.mu.Lock()
	if b.clients != nil && b.clientsFor == key && b.now().Sub(b.clientsAt) < clientsTTL {
		c := b.clients
		b.mu.Unlock()
		return c, nil
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := b.newClients(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.clients, b.clientsAt, b.clientsFor = c, b.now(), key
	b.mu.Unlock()
	return c, nil
}

func (b *uiBackend) svc(ctx context.Context) (*service.Service, *awsx.Clients, error) {
	c, err := b.awsClients(ctx)
	if err != nil {
		return nil, nil, err
	}
	return service.NewService(c), c, nil
}

// InvalidateCredentials implements panel.Backend.
func (b *uiBackend) InvalidateCredentials() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients = nil
}

// Account implements panel.Backend.
func (b *uiBackend) Account(ctx context.Context) (*panel.Account, error) {
	c, err := b.awsClients(ctx)
	if err != nil {
		return nil, err
	}
	a := &panel.Account{AccountID: c.AccountID, Identity: c.CallerARN}
	if data, err := b.profiles.newStore().Load(); err == nil {
		if p, ok := data.Find(data.Active); ok && data.Active != "" {
			a.Alias = p.Alias
		}
	}
	return a, nil
}

// Login implements panel.Backend.
func (b *uiBackend) Login(ctx context.Context, req panel.LoginRequest, onAuthURL func(string)) (*panel.LoginResult, error) {
	res, err := b.login.login(ctx, signin.LoginOptions{OnAuthURL: onAuthURL, RedirectURL: req.RedirectURL})
	if err != nil {
		return nil, err
	}
	out, err := completeLogin(ctx, b.login, res, req.Alias)
	if err != nil {
		return nil, err
	}
	b.InvalidateCredentials()
	return &panel.LoginResult{AccountID: out.Account, Alias: out.Alias, Identity: out.Identity}, nil
}

// Profiles implements panel.Backend.
func (b *uiBackend) Profiles(ctx context.Context) ([]panel.Profile, error) {
	data, err := b.profiles.newStore().Load()
	if err != nil {
		return nil, err
	}
	rows := make([]panel.Profile, len(data.Profiles))
	var wg sync.WaitGroup
	for i, p := range data.Profiles {
		wg.Add(1)
		go func(i int, p profilestore.Profile) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			rows[i] = panel.Profile{
				Active:    p.AccountID == data.Active,
				AccountID: p.AccountID, Alias: p.Alias, Identity: p.IdentityARN, Saved: p.SavedAt,
				Status: classifyProfileStatus(b.profiles.checkSession(cctx, p.LoginSession)),
			}
		}(i, p)
	}
	wg.Wait()
	return rows, nil
}

// RunQuick implements panel.Backend.
func (b *uiBackend) RunQuick(ctx context.Context, args []string) (string, error) {
	stdout, stderr, err := b.runQuick(ctx, args)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return string(stdout), nil
}

// Instances implements panel.Backend.
func (b *uiBackend) Instances(ctx context.Context, showNAT bool) ([]service.InstanceSummary, error) {
	s, _, err := b.svc(ctx)
	if err != nil {
		return nil, err
	}
	return s.ListInstances(ctx, showNAT)
}

// Instance implements panel.Backend.
func (b *uiBackend) Instance(ctx context.Context, name string, withPassword bool) (*service.InstanceInfo, error) {
	s, _, err := b.svc(ctx)
	if err != nil {
		return nil, err
	}
	return s.GetInstanceInfo(ctx, name, withPassword)
}

// Backups implements panel.Backend.
func (b *uiBackend) Backups(ctx context.Context, name string) ([]service.Backup, error) {
	s, _, err := b.svc(ctx)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return s.ListAllBackups(ctx)
	}
	return s.ListBackups(ctx, name)
}

// ACL implements panel.Backend.
func (b *uiBackend) ACL(ctx context.Context) ([]service.ACLEntry, error) {
	s, _, err := b.svc(ctx)
	if err != nil {
		return nil, err
	}
	return s.ListACL(ctx)
}

// Tunnels implements panel.Backend.
func (b *uiBackend) Tunnels(ctx context.Context) ([]service.TunnelOverview, error) {
	// Like `tunnel status`: local state only, no credentials.
	return service.NewService(nil).ListTunnelStatus(ctx)
}

// SuggestTunnel implements panel.Backend.
func (b *uiBackend) SuggestTunnel(ctx context.Context, name string) (*service.TunnelSuggestion, error) {
	s, _, err := b.svc(ctx)
	if err != nil {
		return nil, err
	}
	return s.SuggestTunnel(ctx, name)
}

// Doctor implements panel.Backend.
func (b *uiBackend) Doctor(ctx context.Context) *service.DoctorReport {
	c, err := b.awsClients(ctx)
	return service.Diagnose(ctx, service.DoctorOptions{Clients: c, ClientErr: err})
}

// CheckVersion implements panel.Backend.
func (b *uiBackend) CheckVersion(ctx context.Context) version.CheckResult {
	return version.Checker{}.Check(ctx, version.Version)
}

// loginNavigation is the form of the one-shot login page of `code`
// (newOneShotLoginServerFor): code-server's own login form, with ?to=/
// and the extra query parameters it keeps across its redirect.
func loginNavigation(origin, password string, extra url.Values) *panel.Navigation {
	query := "to=/"
	if len(extra) > 0 {
		query += "&" + extra.Encode()
	}
	return &panel.Navigation{
		Method: "POST",
		Action: origin + "/login?" + query,
		Fields: []panel.Field{{Name: "password", Value: password}, {Name: "base", Value: "."}, {Name: "href", Value: origin + "/login"}},
	}
}

// plainNavigation opens base (an origin with a trailing slash) with the
// given query fields.
func plainNavigation(base string, fields ...panel.Field) *panel.Navigation {
	return &panel.Navigation{Method: "GET", Action: base, Fields: fields}
}

// OpenCode implements panel.Backend.
func (b *uiBackend) OpenCode(ctx context.Context, name string, o panel.OpenOptions, progress func(panel.OpenStage)) (*panel.Navigation, error) {
	progress(panel.StageInstance)
	s, c, err := b.svc(ctx)
	if err != nil {
		return nil, err
	}
	backend := &awsOpenBackend{svc: s, ip: c.IPDiscoverer}
	info, wantAuth, skipReason, err := fetchOpenInfo(ctx, backend, name, !o.NoAuth)
	if err != nil {
		return nil, err
	}
	if err := requireCodeServer(info, name); err != nil {
		return nil, err
	}
	var notes []string
	if skipReason != nil {
		notes = append(notes, "auto-login skipped: "+skipReason.Error())
	}
	var nav *panel.Navigation
	if o.Internet {
		if info.Kind != "bare" && !info.CloudFront {
			return nil, fmt.Errorf("instance %q has no CloudFront; add it on the instance page "+
				"(nakama aws cloudfront add %s, about 15-20 minutes), or open it without --internet", name, name)
		}
		if info.DistributionDomain == "" || info.URL == "" {
			return nil, fmt.Errorf("instance %q has a CloudFront distribution but no domain recorded yet; "+
				"run cloudfront add again to finish it", name)
		}
		var warn bytes.Buffer
		warnIfNotAllowlisted(ctx, backend, &warn)
		if w := strings.TrimSpace(warn.String()); w != "" {
			notes = append(notes, w)
		}
		if wantAuth {
			nav = loginNavigation("https://"+info.DistributionDomain, info.Password, nil)
		} else {
			nav = plainNavigation(strings.TrimSuffix(info.URL, "/") + "/")
		}
	} else {
		progress(panel.StageForward)
		bf, err := s.EnsureBrowserForward(ctx, service.BrowserForwardInput{Name: name, Purpose: service.ForwardPurposeCodeServer})
		if err != nil {
			return nil, err
		}
		if wantAuth {
			nav = loginNavigation(bf.Origin, info.Password, nil)
		} else {
			nav = plainNavigation(bf.Origin + "/")
		}
	}
	nav.Notes = notes
	return nav, nil
}

// OpenKiroCrew implements panel.Backend.
func (b *uiBackend) OpenKiroCrew(ctx context.Context, name string, o panel.OpenOptions, progress func(panel.OpenStage)) (*panel.Navigation, error) {
	progress(panel.StageInstance)
	s, _, err := b.svc(ctx)
	if err != nil {
		return nil, err
	}
	if o.Internet {
		web, err := s.GetKiroCrewWeb(ctx, name)
		if err != nil {
			return nil, err
		}
		if web.State != "running" {
			return nil, fmt.Errorf("instance %q is %s, not running; start it first", name, orNotFound(web.State))
		}
		var notes []string
		if !web.Deployed {
			notes = append(notes, "the Kiro Crew distribution is still deploying; the page may not load until it is done")
		}
		progress(panel.StageSignIn)
		token, err := s.MintKiroCrewToken(ctx, name)
		if err != nil {
			return nil, err
		}
		extra := url.Values{"token": {token}}
		if !o.NoAuth {
			info, err := s.GetInstanceInfo(ctx, name, true)
			switch {
			case err != nil:
				notes = append(notes, "auto-login skipped: could not fetch the code-server password: "+err.Error())
			case info.Password == "":
				notes = append(notes, "auto-login skipped: the code-server password in SSM Parameter Store is empty")
			default:
				nav := loginNavigation("https://"+web.Domain, info.Password, extra)
				nav.Notes = notes
				return nav, nil
			}
		}
		nav := plainNavigation(strings.TrimSuffix(web.URL, "/")+"/", panel.Field{Name: "token", Value: token})
		nav.Notes = notes
		return nav, nil
	}
	info, err := s.GetInstanceInfo(ctx, name, false)
	if err != nil {
		return nil, err
	}
	if info.State != "running" {
		return nil, fmt.Errorf("instance %q is %s, not running; start it first", name, orNotFound(info.State))
	}
	progress(panel.StageForward)
	bf, err := s.EnsureBrowserForward(ctx, service.BrowserForwardInput{Name: name, Purpose: service.ForwardPurposeKiroCrew})
	if err != nil {
		return nil, err
	}
	if o.NoAuth {
		return plainNavigation(bf.Origin + "/"), nil
	}
	progress(panel.StageSignIn)
	// The dashboard token is minted right before opening: its link
	// window is 5 minutes.
	token, err := s.MintKiroCrewToken(ctx, name)
	if err != nil {
		return nil, err
	}
	return plainNavigation(bf.Origin+"/", panel.Field{Name: "token", Value: token}), nil
}

// StartOperation implements panel.Backend.
func (b *uiBackend) StartOperation(ctx context.Context, in service.OperationInput) (*service.Operation, error) {
	_ = service.PruneOperations(ctx, service.OperationRetention)
	return service.StartOperation(ctx, in)
}

// Operations implements panel.Backend.
func (b *uiBackend) Operations(ctx context.Context) ([]service.Operation, error) {
	return service.ListOperations(ctx)
}

// Operation implements panel.Backend.
func (b *uiBackend) Operation(ctx context.Context, id string) (*service.Operation, error) {
	return service.GetOperation(ctx, id)
}

// OperationOutput implements panel.Backend.
func (b *uiBackend) OperationOutput(id string) (string, string, error) {
	return service.OperationOutput(id)
}

// CancelOperation implements panel.Backend.
func (b *uiBackend) CancelOperation(ctx context.Context, id string) error {
	return service.CancelOperation(ctx, id)
}
