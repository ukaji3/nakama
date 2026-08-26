package signin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSigninServer builds a fake sign-in endpoint that validates the
// token exchange request and answers with a token whose idToken maps
// to testSessionWithAccount. The captured request body is stored into
// got.
func newSigninServer(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	idToken := makeIDToken(t, testSessionWithAccount)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/token" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("DPoP") == "" {
			t.Error("token request missing DPoP header")
		}
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		resp := map[string]any{
			"accessToken": map[string]string{
				"accessKeyId":     "AKIAFLOW",
				"secretAccessKey": "flow-secret",
				"sessionToken":    "flow-session",
			},
			"tokenType":    "aws_sigv4",
			"expiresIn":    900,
			"refreshToken": "flow-refresh",
			"idToken":      idToken,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestLogin_SameDeviceFlow(t *testing.T) {
	t.Parallel()
	var exchanged map[string]any
	ts := newSigninServer(t, &exchanged)
	defer ts.Close()

	cacheDir := t.TempDir()
	var seenAuthURL string

	res, err := Login(context.Background(), LoginOptions{
		Client: &Client{BaseURL: ts.URL, HTTPClient: ts.Client(), Now: fixedNow},
		Cache:  &TokenCache{Dir: cacheDir},
		now:    fixedNow,
		OnAuthURL: func(authURL string) {
			seenAuthURL = authURL
			// Simulate the browser: follow the redirect_uri from the
			// authorization URL with the code and the same state.
			u, err := url.Parse(authURL)
			if err != nil {
				t.Errorf("parse auth URL: %v", err)
				return
			}
			q := u.Query()
			redirect := q.Get("redirect_uri")
			state := q.Get("state")
			resp, err := http.Get(redirect + "?code=browser-code&state=" + url.QueryEscape(state)) // #nosec G107 -- 127.0.0.1 test callback
			if err != nil {
				t.Errorf("GET callback: %v", err)
				return
			}
			_ = resp.Body.Close()
		},
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Authorization URL checks.
	u, err := url.Parse(seenAuthURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	q := u.Query()
	if u.Path != "/v1/authorize" || q.Get("client_id") != ClientIDSameDevice ||
		q.Get("response_type") != "code" || q.Get("scope") != "openid" ||
		q.Get("code_challenge_method") != "SHA-256" {
		t.Errorf("unexpected authorization URL: %s", seenAuthURL)
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Errorf("redirect_uri=%q must bind 127.0.0.1", q.Get("redirect_uri"))
	}

	// Exchange request checks: the code from the browser and a
	// verifier that hashes to the advertised challenge.
	if exchanged["grantType"] != "authorization_code" || exchanged["code"] != "browser-code" {
		t.Errorf("unexpected exchange body: %v", exchanged)
	}
	if exchanged["redirectUri"] != q.Get("redirect_uri") {
		t.Errorf("redirectUri=%v, want %q", exchanged["redirectUri"], q.Get("redirect_uri"))
	}
	verifier, _ := exchanged["codeVerifier"].(string)
	if CodeChallenge(verifier) != q.Get("code_challenge") {
		t.Error("code_challenge does not match SHA-256(codeVerifier)")
	}

	// Result and cache checks.
	if res.LoginSession != testSessionWithAccount || res.AccountID != testAccountID {
		t.Errorf("result=(%q,%q)", res.LoginSession, res.AccountID)
	}
	cachePath := filepath.Join(cacheDir, CacheKey(testSessionWithAccount)+".json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	saved, err := (&TokenCache{Dir: cacheDir}).Load(testSessionWithAccount)
	if err != nil {
		t.Fatalf("load cache: %v", err)
	}
	if saved.AccessToken.AccessKeyID != "AKIAFLOW" || saved.AccessToken.AccountID != testAccountID ||
		saved.RefreshToken != "flow-refresh" || saved.ClientID != ClientIDSameDevice ||
		saved.TokenType != "aws_sigv4" {
		t.Errorf("unexpected cached token: %+v", saved)
	}
	if saved.AccessToken.ExpiresAt != FormatExpiresAt(fixedNow().Add(900*time.Second)) {
		t.Errorf("expiresAt=%q", saved.AccessToken.ExpiresAt)
	}
	if _, err := ParseDPoPKeyPEM(saved.DPoPKey); err != nil {
		t.Errorf("cached dpopKey unreadable: %v", err)
	}
}

func TestLogin_RemoteFlow(t *testing.T) {
	t.Parallel()
	var exchanged map[string]any
	ts := newSigninServer(t, &exchanged)
	defer ts.Close()

	cacheDir := t.TempDir()
	var seenAuthURL string

	res, err := Login(context.Background(), LoginOptions{
		Remote: true,
		Client: &Client{BaseURL: ts.URL, HTTPClient: ts.Client(), Now: fixedNow},
		Cache:  &TokenCache{Dir: cacheDir},
		now:    fixedNow,
		OnAuthURL: func(authURL string) {
			seenAuthURL = authURL
		},
		Prompt: func(context.Context) (string, error) {
			// Build the paste string exactly like the confirmation
			// page does: base64("state=...&code=...").
			u, err := url.Parse(seenAuthURL)
			if err != nil {
				return "", err
			}
			state := u.Query().Get("state")
			return base64.StdEncoding.EncodeToString([]byte("state=" + state + "&code=pasted-code")), nil
		},
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	u, err := url.Parse(seenAuthURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != ClientIDCrossDevice {
		t.Errorf("client_id=%q, want cross-device", q.Get("client_id"))
	}
	wantRedirect := ts.URL + "/v1/sessions/confirmation"
	if q.Get("redirect_uri") != wantRedirect {
		t.Errorf("redirect_uri=%q, want %q", q.Get("redirect_uri"), wantRedirect)
	}
	if exchanged["code"] != "pasted-code" || exchanged["clientId"] != ClientIDCrossDevice ||
		exchanged["redirectUri"] != wantRedirect {
		t.Errorf("unexpected exchange body: %v", exchanged)
	}
	if res.AccountID != testAccountID {
		t.Errorf("AccountID=%q", res.AccountID)
	}
}

func TestLogin_RemoteStateMismatch(t *testing.T) {
	t.Parallel()
	var exchanged map[string]any
	ts := newSigninServer(t, &exchanged)
	defer ts.Close()

	_, err := Login(context.Background(), LoginOptions{
		Remote:    true,
		Client:    &Client{BaseURL: ts.URL, HTTPClient: ts.Client()},
		Cache:     &TokenCache{Dir: t.TempDir()},
		OnAuthURL: func(string) {},
		Prompt: func(context.Context) (string, error) {
			return base64.StdEncoding.EncodeToString([]byte("state=wrong&code=c")), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Errorf("expected state mismatch error, got %v", err)
	}
	if exchanged != nil {
		t.Error("no token exchange may happen on state mismatch")
	}
}

func TestLogin_RemotePromptError(t *testing.T) {
	t.Parallel()
	_, err := Login(context.Background(), LoginOptions{
		Remote:    true,
		Client:    &Client{BaseURL: "http://127.0.0.1:0"},
		Cache:     &TokenCache{Dir: t.TempDir()},
		OnAuthURL: func(string) {},
		Prompt: func(context.Context) (string, error) {
			return "", errors.New("stdin closed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "verification code") {
		t.Errorf("expected prompt error, got %v", err)
	}
}

func TestLogin_RequiredCallbacks(t *testing.T) {
	t.Parallel()
	if _, err := Login(context.Background(), LoginOptions{}); err == nil {
		t.Error("expected error when OnAuthURL is missing")
	}
	if _, err := Login(context.Background(), LoginOptions{Remote: true, OnAuthURL: func(string) {}}); err == nil {
		t.Error("expected error when Prompt is missing in remote mode")
	}
}
