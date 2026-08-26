package signin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// getCallback performs the browser-side GET against the callback
// server and returns status + body.
func getCallback(t *testing.T, base, query string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + "?" + query) // #nosec G107 -- 127.0.0.1 test server
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestCallbackServer_CodeAndState(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer()
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	uri := s.RedirectURI()
	if !strings.HasPrefix(uri, "http://127.0.0.1:") || !strings.HasSuffix(uri, "/oauth/callback") {
		t.Fatalf("unexpected redirect URI: %s", uri)
	}

	status, body := getCallback(t, uri, "code=the-code&state=the-state")
	if status != http.StatusOK {
		t.Errorf("status=%d", status)
	}
	if !strings.Contains(body, "ログインが完了しました") {
		t.Errorf("success page unexpected:\n%s", body)
	}
	// The page must never echo the authorization code.
	if strings.Contains(body, "the-code") {
		t.Errorf("callback page leaks the authorization code:\n%s", body)
	}

	code, err := s.WaitForCode(context.Background(), "the-state")
	if err != nil {
		t.Fatalf("WaitForCode: %v", err)
	}
	if code != "the-code" {
		t.Errorf("code=%q", code)
	}
}

func TestCallbackServer_StateMismatch(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer()
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	getCallback(t, s.RedirectURI(), "code=the-code&state=attacker-state")

	_, err = s.WaitForCode(context.Background(), "expected-state")
	if err == nil {
		t.Fatal("expected state mismatch error")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("error should mention the state check: %v", err)
	}
	// Neither the code nor the states may leak into the error.
	for _, secret := range []string{"the-code", "attacker-state", "expected-state"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
}

func TestCallbackServer_ErrorQuery(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer()
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	status, body := getCallback(t, s.RedirectURI(), "error=access_denied")
	if status != http.StatusOK {
		t.Errorf("status=%d", status)
	}
	if !strings.Contains(body, "ログインに失敗しました") {
		t.Errorf("error page unexpected:\n%s", body)
	}

	_, err = s.WaitForCode(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error result")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error should carry the OAuth error code: %v", err)
	}
}

func TestCallbackServer_Timeout(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer()
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()
	s.timeout = 20 * time.Millisecond

	_, err = s.WaitForCode(context.Background(), "any")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

func TestCallbackServer_ContextCancel(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer()
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WaitForCode(ctx, "any"); err == nil {
		t.Error("expected context cancellation error")
	}
}

func TestCallbackServer_UnrelatedRequestIgnored(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer()
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	// No code/state/error: must not deliver a result.
	status, _ := getCallback(t, s.RedirectURI(), "unrelated=1")
	if status != http.StatusNotFound {
		t.Errorf("status=%d, want 404 for a query without code/state/error", status)
	}

	// A later valid redirect still wins.
	getCallback(t, s.RedirectURI(), "code=c&state=s")
	code, err := s.WaitForCode(context.Background(), "s")
	if err != nil || code != "c" {
		t.Errorf("WaitForCode=(%q,%v)", code, err)
	}
}
