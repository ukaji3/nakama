package signin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCacheKey_SHA256Hex(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte(testLoginSession))
	want := hex.EncodeToString(sum[:])
	if got := CacheKey(testLoginSession); got != want {
		t.Errorf("CacheKey=%q, want %q", got, want)
	}
}

func TestTokenCache_SaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "login", "cache")
	cache := &TokenCache{Dir: dir}
	tok := testToken(t, fixedNow().Add(15*time.Minute))

	if err := cache.Save(testLoginSession, tok); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File name must be hex(sha256(session)).json.
	path := filepath.Join(dir, CacheKey(testLoginSession)+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected cache file at %s: %v", path, err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file perm=%o, want 600", perm)
		}
		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("dir perm=%o, want 700", perm)
		}
	}

	got, err := cache.Load(testLoginSession)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *got != *tok {
		t.Errorf("round trip mismatch:\ngot  %+v\nwant %+v", got, tok)
	}
}

// TestTokenCache_AWSCLIFormatCompatibility asserts the exact JSON
// document shape the AWS CLI reads/writes: field names, expiresAt
// format, and a dpopKey that x509.ParseECPrivateKey can read back.
func TestTokenCache_AWSCLIFormatCompatibility(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cache := &TokenCache{Dir: dir}
	tok := testToken(t, time.Date(2026, 8, 26, 12, 15, 0, 0, time.UTC))
	if err := cache.Save(testLoginSession, tok); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, CacheKey(testLoginSession)+".json"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("cache file is not JSON: %v", err)
	}
	for _, k := range []string{"accessToken", "tokenType", "clientId", "refreshToken", "idToken", "dpopKey"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("cache document missing top-level key %q", k)
		}
	}
	at, ok := doc["accessToken"].(map[string]any)
	if !ok {
		t.Fatalf("accessToken is %T", doc["accessToken"])
	}
	for _, k := range []string{"accessKeyId", "secretAccessKey", "sessionToken", "accountId", "expiresAt"} {
		if _, ok := at[k]; !ok {
			t.Errorf("accessToken missing key %q", k)
		}
	}
	if at["expiresAt"] != "2026-08-26T12:15:00Z" {
		t.Errorf("expiresAt=%v, want AWS CLI format 2026-08-26T12:15:00Z", at["expiresAt"])
	}
	// dpopKey must be a SEC1 EC private key PEM readable by
	// ParseECPrivateKey (via our parser that wraps it).
	keyPEM, ok := doc["dpopKey"].(string)
	if !ok {
		t.Fatalf("dpopKey is %T", doc["dpopKey"])
	}
	if _, err := ParseDPoPKeyPEM(keyPEM); err != nil {
		t.Errorf("dpopKey does not parse back: %v", err)
	}
}

// TestTokenCache_LoadsAWSCLIWrittenDocument feeds a document laid out
// exactly like the AWS CLI writes it (Python json.dumps spacing) and
// checks nakama reads it.
func TestTokenCache_LoadsAWSCLIWrittenDocument(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tok := testToken(t, fixedNow())
	doc := `{"accessToken": {"accessKeyId": "AKIACLI", "secretAccessKey": "cli-secret", ` +
		`"sessionToken": "cli-session", "accountId": "111111111111", ` +
		`"expiresAt": "2026-08-26T12:00:00Z"}, "tokenType": "aws_sigv4", ` +
		`"clientId": "arn:aws:signin:::devtools/same-device", ` +
		`"refreshToken": "cli-refresh", "idToken": "a.b.c", ` +
		`"dpopKey": ` + string(mustJSON(t, tok.DPoPKey)) + `}`
	path := filepath.Join(dir, CacheKey(testLoginSession)+".json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cache := &TokenCache{Dir: dir}
	got, err := cache.Load(testLoginSession)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken.AccessKeyID != "AKIACLI" || got.AccessToken.AccountID != "111111111111" ||
		got.RefreshToken != "cli-refresh" || got.ClientID != "arn:aws:signin:::devtools/same-device" {
		t.Errorf("unexpected token: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestTokenCache_LoadMissingIsErrTokenNotFound(t *testing.T) {
	t.Parallel()
	cache := &TokenCache{Dir: t.TempDir()}
	_, err := cache.Load(testLoginSession)
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err=%v, want ErrTokenNotFound", err)
	}
}

func TestDefaultCacheDir_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_LOGIN_CACHE_DIRECTORY", dir)
	got, err := DefaultCacheDir()
	if err != nil {
		t.Fatalf("DefaultCacheDir: %v", err)
	}
	if got != dir {
		t.Errorf("dir=%q, want %q", got, dir)
	}
}

func TestDefaultCacheDir_Default(t *testing.T) {
	// Empty is treated as unset by the resolver.
	t.Setenv("AWS_LOGIN_CACHE_DIRECTORY", "")
	got, err := DefaultCacheDir()
	if err != nil {
		t.Fatalf("DefaultCacheDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if got != filepath.Join(home, ".aws", "login", "cache") {
		t.Errorf("dir=%q", got)
	}
}

func TestDefaultCacheDir_TildeExpansion(t *testing.T) {
	t.Setenv("AWS_LOGIN_CACHE_DIRECTORY", "~/custom/cache")
	got, err := DefaultCacheDir()
	if err != nil {
		t.Fatalf("DefaultCacheDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if got != filepath.Join(home, "custom", "cache") {
		t.Errorf("dir=%q", got)
	}
}
