package signin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// callbackTimeout bounds the whole wait for the OAuth redirect (same
// 10-minute budget as the AWS CLI's AuthCodeFetcher).
const callbackTimeout = 10 * time.Minute

// callbackPath is the local redirect path registered with the
// authorization endpoint.
const callbackPath = "/oauth/callback"

// Minimal pages shown in the browser after the redirect. They carry
// no dynamic content on purpose: echoing query parameters (the auth
// code in particular) back into HTML is never necessary here.
const (
	callbackSuccessHTML = `<!DOCTYPE html><html lang="ja"><head><meta charset="utf-8"><title>nakama</title></head>` +
		`<body><p>ログインが完了しました。このタブを閉じて CLI に戻ってください。</p>` +
		`<p>Login complete. You can close this tab and return to the CLI.</p></body></html>`
	callbackErrorHTML = `<!DOCTYPE html><html lang="ja"><head><meta charset="utf-8"><title>nakama</title></head>` +
		`<body><p>ログインに失敗しました。CLI に戻ってエラーを確認してください。</p>` +
		`<p>Login failed. Return to the CLI and check the reported error.</p></body></html>`
)

// callbackResult is what one redirect request delivered.
type callbackResult struct {
	code  string
	state string
	err   error
}

// CallbackServer is the one-shot local HTTP server that receives the
// OAuth authorization-code redirect for the same-device flow. Unlike
// the AWS CLI (which binds all interfaces), it binds 127.0.0.1 only.
type CallbackServer struct {
	ln      net.Listener
	srv     *http.Server
	results chan callbackResult
	// timeout is the overall wait budget; tests may shorten it.
	timeout time.Duration
}

// StartCallbackServer binds 127.0.0.1 on an ephemeral port and starts
// serving the /oauth/callback endpoint in the background. The caller
// must Close the server when done.
func StartCallbackServer() (*CallbackServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start the local sign-in callback server: %w", err)
	}
	s := &CallbackServer{
		ln:      ln,
		results: make(chan callbackResult, 1),
		timeout: callbackTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, s.handleCallback)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = s.srv.Serve(ln) }() // returns on Close; error is irrelevant
	return s, nil
}

// RedirectURI returns the redirect_uri to register with the
// authorization endpoint (http://127.0.0.1:{port}/oauth/callback).
func (s *CallbackServer) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.ln.Addr().(*net.TCPAddr).Port, callbackPath)
}

// handleCallback processes the OAuth redirect. Only the first result
// is delivered; stray or repeated requests still get a page but are
// otherwise ignored. The authorization code is intentionally never
// logged or echoed.
func (s *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var res callbackResult
	switch {
	case q.Get("error") != "":
		// The error code is server-controlled but bounded; report only
		// its value, never the code/state parameters.
		res = callbackResult{err: fmt.Errorf("the authorization endpoint reported an error (%s)", q.Get("error"))}
	case q.Get("code") != "" && q.Get("state") != "":
		res = callbackResult{code: q.Get("code"), state: q.Get("state")}
	default:
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if res.err != nil {
		_, _ = fmt.Fprint(w, callbackErrorHTML)
	} else {
		_, _ = fmt.Fprint(w, callbackSuccessHTML)
	}

	select {
	case s.results <- res:
	default: // a result was already delivered; ignore duplicates
	}
}

// WaitForCode blocks until the redirect arrives, then validates the
// state parameter and returns the authorization code. It fails when
// the context is cancelled, the overall timeout (10 minutes) elapses,
// the endpoint reported an error, or the state does not match.
func (s *CallbackServer) WaitForCode(ctx context.Context, expectedState string) (string, error) {
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case res := <-s.results:
		if res.err != nil {
			return "", res.err
		}
		if res.state != expectedState {
			return "", errors.New("sign-in failed: the state parameter returned by the browser does not match the expected value")
		}
		return res.code, nil
	case <-timer.C:
		return "", errors.New("sign-in timed out: no browser redirect was received within 10 minutes; run `nakama aws login` again")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close shuts the listener down. Safe to call multiple times.
func (s *CallbackServer) Close() error {
	return s.srv.Close()
}
