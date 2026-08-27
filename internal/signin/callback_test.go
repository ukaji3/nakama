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
	s, err := StartCallbackServer("the-state")
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

func TestCallbackServer_StateMismatchCannotPoison(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer("expected-state")
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	// A request with the wrong state (local attacker, port-scanning
	// web page) is rejected outright...
	status, body := getCallback(t, s.RedirectURI(), "code=evil-code&state=attacker-state")
	if status != http.StatusNotFound {
		t.Errorf("mismatched state: status=%d, want 404", status)
	}
	if strings.Contains(body, "ログイン") {
		t.Errorf("mismatched state must not receive a login page:\n%s", body)
	}

	// ...and must NOT occupy the one-shot channel: the real redirect
	// arriving afterwards still completes the login.
	getCallback(t, s.RedirectURI(), "code=the-code&state=expected-state")
	code, err := s.WaitForCode(context.Background(), "expected-state")
	if err != nil {
		t.Fatalf("WaitForCode after stray request: %v", err)
	}
	if code != "the-code" {
		t.Errorf("code=%q, want the-code", code)
	}
}

func TestCallbackServer_ErrorQuery(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer("the-state")
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	// RFC 6749 requires state on error redirects too; only a
	// state-matching error is trusted.
	status, body := getCallback(t, s.RedirectURI(), "error=access_denied&state=the-state")
	if status != http.StatusOK {
		t.Errorf("status=%d", status)
	}
	if !strings.Contains(body, "ログインに失敗しました") {
		t.Errorf("error page unexpected:\n%s", body)
	}

	_, err = s.WaitForCode(context.Background(), "the-state")
	if err == nil {
		t.Fatal("expected error result")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error should carry the OAuth error code: %v", err)
	}
}

func TestCallbackServer_Timeout(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer("any")
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
	s, err := StartCallbackServer("any")
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
	s, err := StartCallbackServer("s")
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

// TestCallbackServer_StrayErrorSurfacesInTimeout pins the diagnostic
// path: an ignored (state-less or mismatched) error redirect is
// reported inside the timeout message, without leaking states.
func TestCallbackServer_StrayErrorSurfacesInTimeout(t *testing.T) {
	t.Parallel()
	s, err := StartCallbackServer("expected-state")
	if err != nil {
		t.Fatalf("StartCallbackServer: %v", err)
	}
	defer func() { _ = s.Close() }()
	s.timeout = 50 * time.Millisecond

	if status, _ := getCallback(t, s.RedirectURI(), "error=access_denied"); status != http.StatusNotFound {
		t.Errorf("state-less error redirect: status=%d, want 404", status)
	}

	_, err = s.WaitForCode(context.Background(), "expected-state")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("timeout message should mention the ignored error: %v", err)
	}
	if strings.Contains(err.Error(), "expected-state") {
		t.Errorf("timeout message leaks the state: %v", err)
	}
}
