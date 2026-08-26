package signin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newRefreshServer returns an httptest server that counts requests and
// answers every POST with a successful refresh response.
func newRefreshServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("DPoP") == "" {
			t.Error("refresh request missing DPoP header")
		}
		_, _ = w.Write([]byte(successTokenResponse))
	}))
}

func newTestProvider(t *testing.T, ts *httptest.Server, tok *Token, now func() time.Time) (*RefreshingCredentialsProvider, *TokenCache) {
	t.Helper()
	cache := &TokenCache{Dir: t.TempDir()}
	if tok != nil {
		if err := cache.Save(testLoginSession, tok); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
	}
	client := &Client{Now: now}
	if ts != nil {
		client.BaseURL = ts.URL
		client.HTTPClient = ts.Client()
	}
	return &RefreshingCredentialsProvider{
		LoginSession: testLoginSession,
		Cache:        cache,
		Client:       client,
		Now:          now,
	}, cache
}

func TestProviderRetrieve_FreshTokenNoNetwork(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	ts := newRefreshServer(t, &calls)
	defer ts.Close()

	// Expires in 10 minutes: above the 5-minute threshold.
	tok := testToken(t, fixedNow().Add(10*time.Minute))
	p, _ := newTestProvider(t, ts, tok, fixedNow)

	creds, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("refresh calls=%d, want 0 (token is fresh)", calls.Load())
	}
	if creds.AccessKeyID != "AKIAEXAMPLE" || creds.SessionToken != "session-token-example" {
		t.Errorf("unexpected credentials: %+v", creds)
	}
	if !creds.CanExpire || !creds.Expires.Equal(fixedNow().Add(10*time.Minute)) {
		t.Errorf("expiry not propagated: CanExpire=%v Expires=%v", creds.CanExpire, creds.Expires)
	}
	if creds.AccountID != testAccountID {
		t.Errorf("AccountID=%q", creds.AccountID)
	}
}

func TestProviderRetrieve_RefreshesAndRotates(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	ts := newRefreshServer(t, &calls)
	defer ts.Close()

	// Expires in 2 minutes: below the 5-minute threshold.
	tok := testToken(t, fixedNow().Add(2*time.Minute))
	p, cache := newTestProvider(t, ts, tok, fixedNow)

	creds, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("refresh calls=%d, want 1", calls.Load())
	}
	if creds.AccessKeyID != "AKIANEW" || creds.SecretAccessKey != "new-secret" || creds.SessionToken != "new-session-token" {
		t.Errorf("credentials not refreshed: %+v", creds)
	}
	wantExpiry := fixedNow().Add(900 * time.Second)
	if !creds.Expires.Equal(wantExpiry) {
		t.Errorf("Expires=%v, want %v", creds.Expires, wantExpiry)
	}

	// The rotated token must be persisted, with invariants preserved.
	saved, err := cache.Load(testLoginSession)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	if saved.RefreshToken != "new-refresh-token" {
		t.Errorf("refreshToken=%q, want rotated value", saved.RefreshToken)
	}
	if saved.AccessToken.AccessKeyID != "AKIANEW" {
		t.Errorf("accessKeyId=%q not updated", saved.AccessToken.AccessKeyID)
	}
	if saved.AccessToken.AccountID != testAccountID {
		t.Errorf("accountId=%q, must be carried over from the old token", saved.AccessToken.AccountID)
	}
	if saved.AccessToken.ExpiresAt != FormatExpiresAt(wantExpiry) {
		t.Errorf("expiresAt=%q, want %q", saved.AccessToken.ExpiresAt, FormatExpiresAt(wantExpiry))
	}
	if saved.IDToken != tok.IDToken || saved.DPoPKey != tok.DPoPKey ||
		saved.ClientID != tok.ClientID || saved.TokenType != tok.TokenType {
		t.Error("idToken/dpopKey/clientId/tokenType must be unchanged by a refresh")
	}
}

func TestProviderRetrieve_TokenExpiredMapsToRelogin(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"TOKEN_EXPIRED","message":"m"}`))
	}))
	defer ts.Close()

	tok := testToken(t, fixedNow().Add(-1*time.Minute)) // already expired
	p, _ := newTestProvider(t, ts, tok, fixedNow)

	_, err := p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != ErrorCodeTokenExpired {
		t.Errorf("err=%v, want wrapped APIError TOKEN_EXPIRED", err)
	}
	if !strings.Contains(err.Error(), "nakama aws login") {
		t.Errorf("error must direct the user to re-login: %v", err)
	}
	// Secrets must not leak.
	for _, secret := range []string{tok.RefreshToken, tok.AccessToken.SecretAccessKey, tok.AccessToken.SessionToken} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks a secret: %v", err)
		}
	}
}

func TestProviderRetrieve_403Mappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code     string
		wantHint string
	}{
		{ErrorCodeUserCredentialsChanged, "nakama aws login"},
		{ErrorCodeInsufficientPermissions, "SignInLocalDevelopmentAccess"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"` + tc.code + `","message":"m"}`))
			}))
			defer ts.Close()

			p, _ := newTestProvider(t, ts, testToken(t, fixedNow()), fixedNow)
			_, err := p.Retrieve(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Errorf("err=%v, want wrapped APIError %s", err, tc.code)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("error %q missing hint %q", err.Error(), tc.wantHint)
			}
		})
	}
}

func TestProviderRetrieve_NoCachedToken(t *testing.T) {
	t.Parallel()
	p, _ := newTestProvider(t, nil, nil, fixedNow)
	_, err := p.Retrieve(context.Background())
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err=%v, want ErrTokenNotFound", err)
	}
	if !strings.Contains(err.Error(), "nakama aws login") {
		t.Errorf("error must direct the user to login: %v", err)
	}
}

func TestProviderRetrieve_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	ts := newRefreshServer(t, &calls)
	defer ts.Close()

	// Movable clock: after the first refresh the saved token is fresh
	// (expires in 900s), so later Retrieves take the cache path.
	tok := testToken(t, fixedNow().Add(1*time.Minute))
	p, _ := newTestProvider(t, ts, tok, fixedNow)

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = p.Retrieve(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	// The mutex serializes Retrieve: exactly one refresh happens (the
	// first caller updates the cache; everyone after sees > threshold).
	if got := calls.Load(); got != 1 {
		t.Errorf("refresh calls=%d, want exactly 1", got)
	}
}
