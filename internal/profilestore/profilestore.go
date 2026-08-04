// Package profilestore persists nakama's multi-account AWS profile
// registry under $XDG_CONFIG_HOME/nakama (default: ~/.config/nakama).
//
// The registry remembers, per AWS account, the `login_session` value
// that `aws login` wrote to the user's ~/.aws/config. When the user
// logs into a different account, the AWS CLI overwrites that value in
// the default profile; keeping a copy here lets nakama keep addressing
// the previous account's session (see ADR 0011).
//
// SECURITY NOTE: this package intentionally stores NO secret material.
// A login_session value is an identity-ARN-shaped identifier, not a
// credential. The actual short-term tokens live in ~/.aws/login/cache/
// and are managed exclusively by the AWS CLI; nakama never reads,
// copies, or persists them.
//
// Files (both written atomically via temp file + rename):
//
//	profiles.json  (0600)  the registry itself
//	aws_config     (0600)  an AWS-CLI-format config regenerated from
//	                       profiles.json on every save; handed to `aws`
//	                       child processes via AWS_CONFIG_FILE
package profilestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CurrentVersion is the schema version written to profiles.json.
const CurrentVersion = 1

// File and directory names / permissions.
const (
	// ProfilesFileName is the registry file inside the store directory.
	ProfilesFileName = "profiles.json"
	// AWSConfigFileName is the generated AWS CLI config file inside the
	// store directory.
	AWSConfigFileName = "aws_config"

	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// profileNamePrefix prefixes every profile section nakama writes into
// the generated aws_config, namespacing them away from user profiles.
const profileNamePrefix = "nakama-"

// ProfileName returns the AWS CLI profile name nakama uses for the
// given account in its generated aws_config (e.g. "nakama-123456789012").
func ProfileName(accountID string) string {
	return profileNamePrefix + accountID
}

// Profile is one saved account entry.
//
// LoginSession is the (non-secret) identifier `aws login` stores under
// the login_session key; IdentityARN is the STS caller ARN observed at
// save time (shown in `nakama aws profile list` for human recognition).
type Profile struct {
	AccountID    string `json:"account_id" yaml:"account_id"`
	Alias        string `json:"alias" yaml:"alias"`
	LoginSession string `json:"login_session" yaml:"login_session"`
	IdentityARN  string `json:"identity_arn" yaml:"identity_arn"`
	// SavedAt is an RFC3339 timestamp of the last save/upsert.
	SavedAt string `json:"saved_at" yaml:"saved_at"`
}

// Data is the full content of profiles.json.
type Data struct {
	Version int `json:"version" yaml:"version"`
	// Active is the account ID of the currently loaded profile, or ""
	// when nakama should use the default AWS credential chain.
	Active   string    `json:"active" yaml:"active"`
	Profiles []Profile `json:"profiles" yaml:"profiles"`
}

// Upsert inserts p, replacing any existing entry with the same
// AccountID (re-saving the same account overwrites the old entry).
func (d *Data) Upsert(p Profile) {
	for i := range d.Profiles {
		if d.Profiles[i].AccountID == p.AccountID {
			d.Profiles[i] = p
			return
		}
	}
	d.Profiles = append(d.Profiles, p)
}

// FindByAccount returns the profile whose AccountID equals key.
func (d *Data) FindByAccount(key string) (Profile, bool) {
	for _, p := range d.Profiles {
		if p.AccountID == key {
			return p, true
		}
	}
	return Profile{}, false
}

// Find looks a profile up by account ID first, then by alias.
func (d *Data) Find(key string) (Profile, bool) {
	if p, ok := d.FindByAccount(key); ok {
		return p, true
	}
	for _, p := range d.Profiles {
		if p.Alias == key {
			return p, true
		}
	}
	return Profile{}, false
}

// Remove deletes the profile with the given AccountID. It reports
// whether an entry was actually removed.
func (d *Data) Remove(accountID string) bool {
	for i := range d.Profiles {
		if d.Profiles[i].AccountID == accountID {
			d.Profiles = append(d.Profiles[:i], d.Profiles[i+1:]...)
			return true
		}
	}
	return false
}

// ActiveProfile returns the profile referenced by Active, if any.
func (d *Data) ActiveProfile() (Profile, bool) {
	if d.Active == "" {
		return Profile{}, false
	}
	return d.FindByAccount(d.Active)
}

// Store reads and writes the on-disk registry.
type Store struct {
	// BaseDir overrides the store directory; empty means the default
	// resolution ($XDG_CONFIG_HOME/nakama or ~/.config/nakama). Tests
	// inject a temporary directory here so they never touch the real
	// user configuration.
	BaseDir string
}

// New returns a Store using the default directory resolution.
func New() *Store { return &Store{} }

// DefaultBaseDir resolves the store directory:
// $XDG_CONFIG_HOME/nakama when XDG_CONFIG_HOME is set and non-empty,
// otherwise ~/.config/nakama.
func DefaultBaseDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "nakama"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for nakama config: %w", err)
	}
	return filepath.Join(home, ".config", "nakama"), nil
}

// Dir returns the effective store directory.
func (s *Store) Dir() (string, error) {
	if s.BaseDir != "" {
		return s.BaseDir, nil
	}
	return DefaultBaseDir()
}

// ProfilesPath returns the absolute path of profiles.json.
func (s *Store) ProfilesPath() (string, error) {
	dir, err := s.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ProfilesFileName), nil
}

// AWSConfigPath returns the absolute path of the generated aws_config.
func (s *Store) AWSConfigPath() (string, error) {
	dir, err := s.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, AWSConfigFileName), nil
}

// Load reads profiles.json. A missing file yields an empty registry
// (Version=CurrentVersion) so first use needs no explicit init.
func (s *Store) Load() (*Data, error) {
	path, err := s.ProfilesPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is nakama's own config file
	if errors.Is(err, os.ErrNotExist) {
		return &Data{Version: CurrentVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if d.Version > CurrentVersion {
		return nil, fmt.Errorf("%s has unsupported version %d (this nakama supports up to %d); upgrade nakama",
			path, d.Version, CurrentVersion)
	}
	d.Version = CurrentVersion
	return &d, nil
}

// Save atomically writes profiles.json and regenerates aws_config from
// it. The directory is created with 0700 and both files with 0600.
func (s *Store) Save(d *Data) error {
	dir, err := s.Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create nakama config dir %s: %w", dir, err)
	}
	// MkdirAll leaves pre-existing directories untouched; enforce the
	// expected permission either way.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("chmod nakama config dir %s: %w", dir, err)
	}

	d.Version = CurrentVersion
	blob, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profiles: %w", err)
	}
	blob = append(blob, '\n')
	if err := atomicWrite(filepath.Join(dir, ProfilesFileName), blob); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, AWSConfigFileName), []byte(renderAWSConfig(d)))
}

// atomicWrite writes data to path via a same-directory temp file and
// rename, so readers never observe a partially written file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup; after a successful rename the file no
		// longer exists and this is a no-op.
		_ = os.Remove(tmpName)
	}()
	// os.CreateTemp creates with 0600 already, but be explicit in case
	// the platform default changes.
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
