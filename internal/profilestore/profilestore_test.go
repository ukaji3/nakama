package profilestore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestStore returns a Store rooted in a temp directory so tests
// never touch the real ~/.config/nakama.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return &Store{BaseDir: filepath.Join(t.TempDir(), "nakama")}
}

func sampleProfile(accountID, alias string) Profile {
	return Profile{
		AccountID:    accountID,
		Alias:        alias,
		LoginSession: "arn:aws:signin:::user/example-" + accountID,
		IdentityARN:  "arn:aws:sts::" + accountID + ":assumed-role/Admin/user",
		SavedAt:      "2026-07-30T00:00:00+09:00",
	}
}

func TestLoad_MissingFileReturnsEmptyRegistry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	d, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Version != CurrentVersion {
		t.Errorf("Version=%d, want %d", d.Version, CurrentVersion)
	}
	if d.Active != "" || len(d.Profiles) != 0 {
		t.Errorf("expected empty registry, got %+v", d)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	in := &Data{
		Active:   "111111111111",
		Profiles: []Profile{sampleProfile("111111111111", "dev"), sampleProfile("222222222222", "prod")},
	}
	if err := s.Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Active != "111111111111" {
		t.Errorf("Active=%q", out.Active)
	}
	if len(out.Profiles) != 2 {
		t.Fatalf("len(Profiles)=%d, want 2", len(out.Profiles))
	}
	if out.Profiles[0].Alias != "dev" || out.Profiles[1].Alias != "prod" {
		t.Errorf("profiles round-trip mismatch: %+v", out.Profiles)
	}
	if out.Profiles[0].LoginSession == "" || out.Profiles[0].IdentityARN == "" || out.Profiles[0].SavedAt == "" {
		t.Errorf("profile fields lost in round-trip: %+v", out.Profiles[0])
	}
}

func TestUpsert_ReplacesSameAccount(t *testing.T) {
	t.Parallel()
	d := &Data{Version: CurrentVersion}
	d.Upsert(sampleProfile("111111111111", "old-alias"))
	updated := sampleProfile("111111111111", "new-alias")
	updated.LoginSession = "arn:aws:signin:::user/rotated"
	d.Upsert(updated)
	if len(d.Profiles) != 1 {
		t.Fatalf("len(Profiles)=%d, want 1 (upsert must replace)", len(d.Profiles))
	}
	if d.Profiles[0].Alias != "new-alias" || d.Profiles[0].LoginSession != "arn:aws:signin:::user/rotated" {
		t.Errorf("upsert did not overwrite: %+v", d.Profiles[0])
	}

	d.Upsert(sampleProfile("222222222222", "another"))
	if len(d.Profiles) != 2 {
		t.Fatalf("len(Profiles)=%d, want 2 after inserting a new account", len(d.Profiles))
	}
}

func TestFind_ByAccountIDAndAlias(t *testing.T) {
	t.Parallel()
	d := &Data{Version: CurrentVersion}
	d.Upsert(sampleProfile("111111111111", "dev"))
	d.Upsert(sampleProfile("222222222222", "prod"))

	if p, ok := d.Find("111111111111"); !ok || p.Alias != "dev" {
		t.Errorf("Find by accountID failed: %+v ok=%v", p, ok)
	}
	if p, ok := d.Find("prod"); !ok || p.AccountID != "222222222222" {
		t.Errorf("Find by alias failed: %+v ok=%v", p, ok)
	}
	if _, ok := d.Find("nope"); ok {
		t.Error("Find should miss for unknown key")
	}

	// Account ID takes precedence over a colliding alias.
	collide := sampleProfile("333333333333", "111111111111")
	d.Upsert(collide)
	if p, _ := d.Find("111111111111"); p.AccountID != "111111111111" {
		t.Errorf("account ID must win over alias collision, got %+v", p)
	}
}

func TestRemove_AndActiveProfile(t *testing.T) {
	t.Parallel()
	d := &Data{Version: CurrentVersion, Active: "111111111111"}
	d.Upsert(sampleProfile("111111111111", "dev"))

	if p, ok := d.ActiveProfile(); !ok || p.AccountID != "111111111111" {
		t.Errorf("ActiveProfile: %+v ok=%v", p, ok)
	}
	if !d.Remove("111111111111") {
		t.Fatal("Remove returned false for existing account")
	}
	if d.Remove("111111111111") {
		t.Error("Remove returned true for missing account")
	}
	if len(d.Profiles) != 0 {
		t.Errorf("Profiles not empty after Remove: %+v", d.Profiles)
	}

	// Dangling Active (entry removed) must not resolve.
	if _, ok := d.ActiveProfile(); ok {
		t.Error("ActiveProfile should miss when the entry is gone")
	}
	d.Active = ""
	if _, ok := d.ActiveProfile(); ok {
		t.Error("ActiveProfile should miss when Active is empty")
	}
}

func TestSave_ActiveSwitchPersists(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	d := &Data{Profiles: []Profile{sampleProfile("111111111111", "dev")}}
	if err := s.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	d2, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d2.Active != "" {
		t.Fatalf("Active=%q, want empty", d2.Active)
	}
	d2.Active = "111111111111"
	if err := s.Save(d2); err != nil {
		t.Fatalf("Save(active): %v", err)
	}

	d3, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d3.Active != "111111111111" {
		t.Errorf("Active=%q after switch, want 111111111111", d3.Active)
	}
}

func TestSave_Permissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions not applicable on windows")
	}
	s := newTestStore(t)
	if err := s.Save(&Data{Profiles: []Profile{sampleProfile("111111111111", "dev")}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dir, _ := s.Dir()
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("dir perm=%o, want 700", got)
	}
	for _, name := range []string{ProfilesFileName, AWSConfigFileName} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := st.Mode().Perm(); got != 0o600 {
			t.Errorf("%s perm=%o, want 600", name, got)
		}
	}
}

func TestSave_AtomicNoTempLeftoverAndValidJSON(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.Save(&Data{Profiles: []Profile{sampleProfile("111111111111", "dev")}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Overwrite (exercises the rename-over-existing path).
	if err := s.Save(&Data{Profiles: []Profile{sampleProfile("222222222222", "prod")}}); err != nil {
		t.Fatalf("Save (overwrite): %v", err)
	}

	dir, _ := s.Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
	if len(entries) != 2 {
		t.Errorf("expected exactly profiles.json + aws_config, got %d entries", len(entries))
	}

	raw, err := os.ReadFile(filepath.Join(dir, ProfilesFileName))
	if err != nil {
		t.Fatalf("read profiles.json: %v", err)
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if d.Version != CurrentVersion {
		t.Errorf("persisted Version=%d, want %d", d.Version, CurrentVersion)
	}
	if len(d.Profiles) != 1 || d.Profiles[0].AccountID != "222222222222" {
		t.Errorf("overwrite did not fully replace content: %+v", d.Profiles)
	}
}

func TestSave_AWSConfigContent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	d := &Data{Profiles: []Profile{
		// Intentionally unsorted; output must be sorted by account ID.
		sampleProfile("222222222222", "prod"),
		sampleProfile("111111111111", "dev"),
	}}
	if err := s.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, _ := s.AWSConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read aws_config: %v", err)
	}
	got := string(raw)

	for _, want := range []string{
		"[profile nakama-111111111111]",
		"[profile nakama-222222222222]",
		"login_session = arn:aws:signin:::user/example-111111111111",
		"login_session = arn:aws:signin:::user/example-222222222222",
		"region = ap-northeast-1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("aws_config missing %q:\n%s", want, got)
		}
	}
	// Deterministic ordering: 111... section before 222....
	if strings.Index(got, "nakama-111111111111") > strings.Index(got, "nakama-222222222222") {
		t.Errorf("aws_config sections not sorted by account ID:\n%s", got)
	}
	// No secret-shaped keys must ever appear.
	for _, forbidden := range []string{"aws_access_key_id", "aws_secret_access_key", "aws_session_token"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("aws_config contains forbidden key %q", forbidden)
		}
	}
}

func TestLoad_RejectsNewerVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	dir, _ := s.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	blob := []byte(`{"version": 99, "active": "", "profiles": []}`)
	if err := os.WriteFile(filepath.Join(dir, ProfilesFileName), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("expected error for unsupported future version")
	}
}

func TestLoad_RejectsCorruptJSON(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	dir, _ := s.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProfilesFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("expected error for corrupt profiles.json")
	}
}

func TestDefaultBaseDir_XDGAndHome(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	tmp := t.TempDir()

	t.Setenv("XDG_CONFIG_HOME", tmp)
	got, err := DefaultBaseDir()
	if err != nil {
		t.Fatalf("DefaultBaseDir: %v", err)
	}
	if want := filepath.Join(tmp, "nakama"); got != want {
		t.Errorf("with XDG_CONFIG_HOME: got %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	// Keep the fallback inside the temp dir. os.UserHomeDir reads $HOME
	// on unix but %USERPROFILE% on Windows, so set both.
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	got, err = DefaultBaseDir()
	if err != nil {
		t.Fatalf("DefaultBaseDir (home fallback): %v", err)
	}
	if want := filepath.Join(tmp, ".config", "nakama"); got != want {
		t.Errorf("home fallback: got %q, want %q", got, want)
	}
}

func TestProfileName(t *testing.T) {
	t.Parallel()
	if got := ProfileName("123456789012"); got != "nakama-123456789012" {
		t.Errorf("ProfileName=%q", got)
	}
}
