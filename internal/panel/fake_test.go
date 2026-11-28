package panel

import (
	"context"
	"errors"
	"sync"

	"github.com/ukaji3/nakama/internal/service"
	"github.com/ukaji3/nakama/internal/version"
)

// fakeBackend is a scripted Backend that records what the panel asked.
type fakeBackend struct {
	mu sync.Mutex

	account    *Account
	accountErr error
	instances  []service.InstanceSummary
	info       map[string]*service.InstanceInfo
	backups    []service.Backup
	acl        []service.ACLEntry
	profiles   []Profile
	tunnels    []service.TunnelOverview
	ops        []service.Operation
	output     map[string][2]string
	nav        *Navigation
	openErr    error
	startErr   error

	// login is called by Login (nil: succeed at once after the URL).
	login func(ctx context.Context, req LoginRequest, onAuthURL func(string)) (*LoginResult, error)

	started     []service.OperationInput
	quick       [][]string
	cancelled   []string
	opened      []string
	passwords   []string
	invalidated int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		account: &Account{AccountID: "111122223333", Identity: "arn:aws:sts::111122223333:assumed-role/dev/user", Alias: "work"},
		instances: []service.InstanceSummary{
			{Name: "dev01", Type: "vscode", State: "running", InstanceType: "t3.medium"},
			{Name: "box01", Type: "bare", State: "stopped", InstanceType: "t4g.small"},
		},
		info: map[string]*service.InstanceInfo{
			"dev01": {Name: "dev01", State: "running", Kind: "vscode", InstanceType: "t3.medium", Password: "s3cret-pass"},
			"box01": {Name: "box01", State: "stopped", Kind: "bare", InstanceType: "t4g.small"},
		},
		backups: []service.Backup{{Name: "dev01", Label: "before-update", CreatedAt: "2026-11-28T00:00:00Z", State: "completed", SizeGiB: 30}},
		acl:     []service.ACLEntry{{CIDR: "203.0.113.7/32", AddedAt: "2026-11-28T00:00:00Z"}},
		output:  map[string][2]string{},
	}
}

func (f *fakeBackend) Account(context.Context) (*Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	return f.account, nil
}

func (f *fakeBackend) InvalidateCredentials() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated++
}

func (f *fakeBackend) Login(ctx context.Context, req LoginRequest, onAuthURL func(string)) (*LoginResult, error) {
	if f.login != nil {
		return f.login(ctx, req, onAuthURL)
	}
	onAuthURL("https://ap-northeast-1.signin.aws.amazon.com/v1/authorize?x=1")
	return &LoginResult{AccountID: "111122223333", Alias: req.Alias}, nil
}

func (f *fakeBackend) Profiles(context.Context) ([]Profile, error) { return f.profiles, nil }

func (f *fakeBackend) RunQuick(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quick = append(f.quick, args)
	return `{"ok":true}`, nil
}

func (f *fakeBackend) Instances(context.Context, bool) ([]service.InstanceSummary, error) {
	return f.instances, nil
}

func (f *fakeBackend) Instance(_ context.Context, name string, withPassword bool) (*service.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.info[name]
	if !ok {
		return nil, errors.New(`instance "` + name + `" not found`)
	}
	c := *i
	if withPassword {
		f.passwords = append(f.passwords, name)
	} else {
		c.Password = ""
	}
	return &c, nil
}

func (f *fakeBackend) Backups(_ context.Context, name string) ([]service.Backup, error) {
	if name == "" {
		return f.backups, nil
	}
	var out []service.Backup
	for _, b := range f.backups {
		if b.Name == name {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeBackend) ACL(context.Context) ([]service.ACLEntry, error) { return f.acl, nil }

func (f *fakeBackend) Tunnels(context.Context) ([]service.TunnelOverview, error) {
	return f.tunnels, nil
}

func (f *fakeBackend) SuggestTunnel(_ context.Context, name string) (*service.TunnelSuggestion, error) {
	return &service.TunnelSuggestion{Name: name, Ports: []service.ListeningPort{{Port: 3000, Address: "127.0.0.1", Process: "node"}},
		Command: "nakama aws tunnel " + name + " -L 3000:3000"}, nil
}

func (f *fakeBackend) Doctor(context.Context) *service.DoctorReport {
	return &service.DoctorReport{Checks: []service.DoctorCheck{{Name: "credentials", Status: service.CheckOK, Detail: "account 111122223333"}}, OKs: 1}
}

func (f *fakeBackend) CheckVersion(context.Context) version.CheckResult {
	return version.CheckResult{Current: "v9.9.9-test", Latest: "v9.9.9", Status: version.StatusDevelopment}
}

func (f *fakeBackend) OpenCode(_ context.Context, name string, o OpenOptions, progress func(OpenStage)) (*Navigation, error) {
	f.mu.Lock()
	f.opened = append(f.opened, "code "+name)
	nav, err := f.nav, f.openErr
	f.mu.Unlock()
	progress(StageInstance)
	progress(StageForward)
	if err != nil {
		return nil, err
	}
	if nav != nil {
		return nav, nil
	}
	origin := "http://code-server." + name + ".nakama.localhost:18888"
	if o.NoAuth {
		return &Navigation{Method: "GET", Action: origin + "/"}, nil
	}
	return &Navigation{Method: "POST", Action: origin + "/login?to=/", Fields: []Field{{"password", "s3cret-pass"}, {"base", "."}, {"href", origin + "/login"}}}, nil
}

func (f *fakeBackend) OpenKiroCrew(_ context.Context, name string, _ OpenOptions, progress func(OpenStage)) (*Navigation, error) {
	f.mu.Lock()
	f.opened = append(f.opened, "kirocrew "+name)
	f.mu.Unlock()
	progress(StageSignIn)
	return &Navigation{Method: "GET", Action: "http://kirocrew." + name + ".nakama.localhost:18888/", Fields: []Field{{"token", "tok-123"}}}, nil
}

func (f *fakeBackend) StartOperation(_ context.Context, in service.OperationInput) (*service.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.started = append(f.started, in)
	op := service.Operation{ID: "20261128T120000Z-0000000" + string(rune('0'+len(f.started))), Title: in.Title, Instance: in.Instance,
		Exclusive: in.Exclusive, Args: in.Args, State: service.OperationRunning}
	f.ops = append([]service.Operation{op}, f.ops...)
	return &op, nil
}

func (f *fakeBackend) Operations(context.Context) ([]service.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]service.Operation(nil), f.ops...), nil
}

func (f *fakeBackend) Operation(_ context.Context, id string) (*service.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.ops {
		if f.ops[i].ID == id {
			o := f.ops[i]
			return &o, nil
		}
	}
	return nil, service.ErrUnknownOperation
}

func (f *fakeBackend) OperationOutput(id string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.output[id]
	return o[0], o[1], nil
}

func (f *fakeBackend) CancelOperation(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	return nil
}
