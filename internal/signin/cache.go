package signin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Cache file/directory permissions (identical to the AWS CLI's
// JSONFileCache and to nakama's profilestore).
const (
	cacheDirPerm  os.FileMode = 0o700
	cacheFilePerm os.FileMode = 0o600
)

// ErrTokenNotFound is returned by TokenCache.Load when no cached token
// exists for the requested login session.
var ErrTokenNotFound = errors.New("no cached login token for this session")

// DefaultCacheDir resolves the login token cache directory the same
// way the AWS CLI does: the AWS_LOGIN_CACHE_DIRECTORY environment
// variable when set (with a leading ~ expanded), otherwise
// ~/.aws/login/cache.
func DefaultCacheDir() (string, error) {
	if dir := os.Getenv("AWS_LOGIN_CACHE_DIRECTORY"); dir != "" {
		return expandHome(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for the login token cache: %w", err)
	}
	return filepath.Join(home, ".aws", "login", "cache"), nil
}

// expandHome expands a leading "~" or "~/..." to the user's home
// directory (subset of the AWS CLI's os.path.expanduser behavior).
func expandHome(path string) (string, error) {
	if path != "~" && !hasHomePrefix(path) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~ in AWS_LOGIN_CACHE_DIRECTORY: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func hasHomePrefix(path string) bool {
	return len(path) >= 2 && path[0] == '~' && (path[1] == '/' || path[1] == os.PathSeparator)
}

// CacheKey returns the cache file base name for a login session:
// lowercase hex of SHA-256(login_session), matching the AWS CLI's
// generate_login_cache_key.
func CacheKey(loginSession string) string {
	sum := sha256.Sum256([]byte(loginSession))
	return hex.EncodeToString(sum[:])
}

// TokenCache reads and writes AWS-CLI-compatible login token files.
type TokenCache struct {
	// Dir overrides the cache directory. Empty means the default
	// resolution (AWS_LOGIN_CACHE_DIRECTORY or ~/.aws/login/cache),
	// re-evaluated on every operation. Tests inject a temp directory.
	Dir string
}

// path returns the absolute cache file path for a login session.
func (c *TokenCache) path(loginSession string) (string, error) {
	dir := c.Dir
	if dir == "" {
		var err error
		dir, err = DefaultCacheDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, CacheKey(loginSession)+".json"), nil
}

// Load reads the cached token for the given login session. It returns
// ErrTokenNotFound (wrapped with context) when the file is absent.
func (c *TokenCache) Load(loginSession string) (*Token, error) {
	path, err := c.path(loginSession)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is derived from the fixed cache dir + a sha256 hex name
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w (session %s)", ErrTokenNotFound, loginSession)
	}
	if err != nil {
		return nil, fmt.Errorf("read login token cache: %w", err)
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse login token cache %s: %w", filepath.Base(path), err)
	}
	return &t, nil
}

// Save atomically writes the token for the given login session,
// creating the cache directory (0700) if needed. The file is written
// with 0600 via a same-directory temp file + rename so concurrent
// readers never observe a partial document.
func (c *TokenCache) Save(loginSession string, t *Token) error {
	path, err := c.path(loginSession)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, cacheDirPerm); err != nil {
		return fmt.Errorf("create login token cache dir %s: %w", dir, err)
	}
	blob, err := json.Marshal(t) // #nosec G117 -- serializing the refresh token IS this cache's purpose (AWS-CLI-compatible 0600 file); it never reaches logs or output
	if err != nil {
		return fmt.Errorf("encode login token: %w", err)
	}
	return atomicWrite(path, blob)
}

// atomicWrite writes data to path via a same-directory temp file and
// rename (same pattern as internal/profilestore).
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup; after a successful rename this is a no-op.
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(cacheFilePerm); err != nil {
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
