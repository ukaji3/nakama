package cli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/profilestore"
)

// newTestProfileDeps returns deps rooted in a temp store with benign
// fakes. Individual tests override the fields they exercise. Nothing
// here touches the real ~/.aws or ~/.config/nakama, and no AWS API or
// aws CLI is ever invoked.
func newTestProfileDeps(t *testing.T) (profileDeps, *profilestore.Store) {
	t.Helper()
	store := &profilestore.Store{BaseDir: filepath.Join(t.TempDir(), "nakama")}
	deps := profileDeps{
		newStore: func() *profilestore.Store { return store },
		callerIdentity: func(context.Context) (string, string, error) {
			return "111111111111", "arn:aws:sts::111111111111:assumed-role/Admin/user", nil
		},
		loginSession: func(context.Context) (string, error) {
			return "arn:aws:signin:::user/example", nil
		},
		checkProfile: func(context.Context, string, string) error { return nil },
		now: func() time.Time {
			return time.Date(2026, 7, 30, 0, 0, 0, 0, time.FixedZone("JST", 9*3600))
		},
	}
	return deps, store
}

// runProfileCommand executes `nakama aws profile <args...>` against the
// given deps and returns stdout, stderr, and the execution error.
func runProfileCommand(t *testing.T, deps profileDeps, args ...string) (string, string, error) {
	t.Helper()
	cmd := newAWSProfileCommandWithDeps(deps)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func seedProfiles(t *testing.T, store *profilestore.Store, active string, profiles ...profilestore.Profile) {
	t.Helper()
	data := &profilestore.Data{Active: active, Profiles: profiles}
	if err := store.Save(data); err != nil {
		t.Fatalf("seed store: %v", err)
	}
}

func prof(account, alias string) profilestore.Profile {
	return profilestore.Profile{
		AccountID:    account,
		Alias:        alias,
		LoginSession: "arn:aws:signin:::user/" + alias,
		IdentityARN:  "arn:aws:sts::" + account + ":assumed-role/Admin/user",
		SavedAt:      "2026-07-30T00:00:00+09:00",
	}
}

func TestProfileSave_DefaultAliasIsAccountID(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	out, _, err := runProfileCommand(t, deps, "save")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(data.Profiles) != 1 {
		t.Fatalf("len(Profiles)=%d, want 1", len(data.Profiles))
	}
	p := data.Profiles[0]
	if p.AccountID != "111111111111" {
		t.Errorf("AccountID=%q", p.AccountID)
	}
	if p.Alias != "111111111111" {
		t.Errorf("Alias=%q, want default = account ID", p.Alias)
	}
	if p.LoginSession != "arn:aws:signin:::user/example" {
		t.Errorf("LoginSession=%q", p.LoginSession)
	}
	if p.IdentityARN != "arn:aws:sts::111111111111:assumed-role/Admin/user" {
		t.Errorf("IdentityARN=%q", p.IdentityARN)
	}
	if p.SavedAt == "" {
		t.Error("SavedAt empty")
	}
	if data.Active != "" {
		t.Errorf("save must not change active, got %q", data.Active)
	}
	if !strings.Contains(out, "nakama aws profile load") {
		t.Errorf("save output missing load hint:\n%s", out)
	}
}

func TestProfileSave_NameFlagSetsAlias(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	if _, _, err := runProfileCommand(t, deps, "save", "--name", "dev"); err != nil {
		t.Fatalf("save --name: %v", err)
	}
	data, _ := store.Load()
	if len(data.Profiles) != 1 || data.Profiles[0].Alias != "dev" {
		t.Errorf("alias not applied: %+v", data.Profiles)
	}
}

func TestProfileSave_UpsertOverwritesSameAccount(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("111111111111", "old"))
	if _, _, err := runProfileCommand(t, deps, "save", "--name", "new"); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, _ := store.Load()
	if len(data.Profiles) != 1 {
		t.Fatalf("len(Profiles)=%d, want 1 (upsert)", len(data.Profiles))
	}
	if data.Profiles[0].Alias != "new" {
		t.Errorf("Alias=%q, want new", data.Profiles[0].Alias)
	}
}

func TestProfileSave_NoLoginSessionFails(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	deps.loginSession = func(context.Context) (string, error) {
		return "", errors.New("exit status 1")
	}
	_, _, err := runProfileCommand(t, deps, "save")
	if err == nil {
		t.Fatal("expected error when login_session is missing")
	}
	if !strings.Contains(err.Error(), "aws login") {
		t.Errorf("error should explain the aws login requirement: %v", err)
	}
	if data, _ := store.Load(); len(data.Profiles) != 0 {
		t.Errorf("nothing must be saved on failure, got %+v", data.Profiles)
	}
}

func TestProfileSave_AliasCollisionRejected(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("222222222222", "dev"))
	_, _, err := runProfileCommand(t, deps, "save", "--name", "dev")
	if err == nil {
		t.Fatal("expected alias collision error")
	}
	if !strings.Contains(err.Error(), "dev") || !strings.Contains(err.Error(), "222222222222") {
		t.Errorf("collision error should name the conflicting account: %v", err)
	}
}

func TestProfileList_ActiveMarkAndColumns(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "222222222222",
		prof("111111111111", "dev"),
		prof("222222222222", "prod"),
	)
	deps.checkProfile = func(_ context.Context, profileName, configFile string) error {
		if configFile == "" {
			t.Error("checkProfile must receive nakama's aws_config path")
		}
		if profileName == "nakama-111111111111" {
			return errors.New("expired token") // -> expired
		}
		return nil // -> valid
	}

	out, _, err := runProfileCommand(t, deps, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(lines), out)
	}
	for _, col := range []string{"ACTIVE", "ACCOUNT", "ALIAS", "IDENTITY", "SAVED", "STATUS"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header missing %q:\n%s", col, lines[0])
		}
	}

	var devLine, prodLine string
	for _, l := range lines[1:] {
		switch {
		case strings.Contains(l, "111111111111"):
			devLine = l
		case strings.Contains(l, "222222222222"):
			prodLine = l
		}
	}
	if devLine == "" || prodLine == "" {
		t.Fatalf("missing rows:\n%s", out)
	}
	if strings.HasPrefix(devLine, "*") {
		t.Errorf("non-active row must not carry the * mark:\n%s", devLine)
	}
	if !strings.HasPrefix(prodLine, "*") {
		t.Errorf("active row must start with *:\n%s", prodLine)
	}
	if !strings.Contains(devLine, "expired") {
		t.Errorf("dev row should be expired:\n%s", devLine)
	}
	if !strings.Contains(prodLine, "valid") {
		t.Errorf("prod row should be valid:\n%s", prodLine)
	}
}

func TestProfileList_Empty(t *testing.T) {
	deps, _ := newTestProfileDeps(t)
	out, _, err := runProfileCommand(t, deps, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "no saved profiles") {
		t.Errorf("empty list output unexpected:\n%s", out)
	}
}

func TestClassifyProfileStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{nil, "valid"},
		{errors.New("expired token"), "expired"},
		{exec.ErrNotFound, "unknown"},
		{context.DeadlineExceeded, "unknown"},
		{context.Canceled, "unknown"},
	}
	for _, c := range cases {
		if got := classifyProfileStatus(c.err); got != c.want {
			t.Errorf("classifyProfileStatus(%v)=%q, want %q", c.err, got, c.want)
		}
	}
}

func TestProfileLoad_ByAccountID(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("111111111111", "dev"))
	out, _, err := runProfileCommand(t, deps, "load", "111111111111")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	data, _ := store.Load()
	if data.Active != "111111111111" {
		t.Errorf("Active=%q", data.Active)
	}
	if !strings.Contains(out, "111111111111") {
		t.Errorf("load output missing account:\n%s", out)
	}
}

func TestProfileLoad_ByAlias(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("111111111111", "dev"), prof("222222222222", "prod"))
	if _, _, err := runProfileCommand(t, deps, "load", "prod"); err != nil {
		t.Fatalf("load by alias: %v", err)
	}
	data, _ := store.Load()
	if data.Active != "222222222222" {
		t.Errorf("Active=%q, want 222222222222", data.Active)
	}
}

func TestProfileLoad_NotFoundListsCandidates(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("111111111111", "dev"))
	_, _, err := runProfileCommand(t, deps, "load", "nope")
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "111111111111") || !strings.Contains(err.Error(), "dev") {
		t.Errorf("not-found error should list candidates: %v", err)
	}
	if data, _ := store.Load(); data.Active != "" {
		t.Errorf("active must stay unchanged on lookup failure, got %q", data.Active)
	}
}

func TestProfileLoad_ExpiredSessionWarnsButLoads(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("111111111111", "dev"))
	deps.checkProfile = func(context.Context, string, string) error {
		return errors.New("expired token")
	}
	_, errOut, err := runProfileCommand(t, deps, "load", "dev")
	if err != nil {
		t.Fatalf("load must succeed despite validation failure: %v", err)
	}
	if !strings.Contains(errOut, "warning") || !strings.Contains(errOut, "aws login") {
		t.Errorf("expected expiry warning with aws login guidance on stderr:\n%s", errOut)
	}
	if data, _ := store.Load(); data.Active != "111111111111" {
		t.Errorf("Active=%q, want 111111111111 (set despite warning)", data.Active)
	}
}

func TestProfileDelete_ClearsActive(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "111111111111", prof("111111111111", "dev"), prof("222222222222", "prod"))
	out, _, err := runProfileCommand(t, deps, "delete", "dev")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	data, _ := store.Load()
	if data.Active != "" {
		t.Errorf("Active=%q, want cleared", data.Active)
	}
	if len(data.Profiles) != 1 || data.Profiles[0].AccountID != "222222222222" {
		t.Errorf("unexpected remaining profiles: %+v", data.Profiles)
	}
	if !strings.Contains(out, "active") {
		t.Errorf("delete output should mention the active selection was cleared:\n%s", out)
	}
}

func TestProfileDelete_NonActiveKeepsActive(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "222222222222", prof("111111111111", "dev"), prof("222222222222", "prod"))
	out, _, err := runProfileCommand(t, deps, "delete", "111111111111")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	data, _ := store.Load()
	if data.Active != "222222222222" {
		t.Errorf("Active=%q, must be preserved", data.Active)
	}
	if strings.Contains(out, "cleared") {
		t.Errorf("delete of a non-active profile must not claim the active selection was cleared:\n%s", out)
	}
}

func TestProfileDelete_NotFound(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("111111111111", "dev"))
	if _, _, err := runProfileCommand(t, deps, "delete", "nope"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestProfileUnload_ClearsActive(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "111111111111", prof("111111111111", "dev"))
	out, _, err := runProfileCommand(t, deps, "unload")
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if data, _ := store.Load(); data.Active != "" {
		t.Errorf("Active=%q, want cleared", data.Active)
	}
	if !strings.Contains(out, "default AWS credential chain") {
		t.Errorf("unload output should mention the fallback to the default chain:\n%s", out)
	}
}

func TestProfileUnload_NoActiveIsNoop(t *testing.T) {
	deps, store := newTestProfileDeps(t)
	seedProfiles(t, store, "", prof("111111111111", "dev"))
	out, _, err := runProfileCommand(t, deps, "unload")
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if !strings.Contains(out, "no active profile") {
		t.Errorf("unload without active should say so:\n%s", out)
	}
	if data, _ := store.Load(); len(data.Profiles) != 1 {
		t.Errorf("profiles must be preserved: %+v", data.Profiles)
	}
}

func TestFormatProfileTable_ActiveMark(t *testing.T) {
	t.Parallel()
	rows := []profileRow{
		{Active: false, Account: "111111111111", Alias: "dev", Identity: "arn:a", Saved: "2026-07-30T00:00:00+09:00", Status: "expired"},
		{Active: true, Account: "222222222222", Alias: "prod", Identity: "arn:b", Saved: "2026-07-30T00:00:00+09:00", Status: "valid"},
	}
	out := formatProfileTable(rows)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), out)
	}
	if strings.HasPrefix(lines[1], "*") {
		t.Errorf("inactive row must not start with *:\n%s", lines[1])
	}
	if !strings.HasPrefix(lines[2], "*") {
		t.Errorf("active row must start with *:\n%s", lines[2])
	}
}
