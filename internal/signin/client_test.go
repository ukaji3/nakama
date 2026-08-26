package signin

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capturedRequest records what the fake token endpoint received.
type capturedRequest struct {
	method      string
	path        string
	contentType string
	dpop        string
	body        map[string]any
}

// newTokenServer returns an httptest server that captures the request
// and replies with the given status/body.
func newTokenServer(t *testing.T, status int, respBody string, captured *capturedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.contentType = r.Header.Get("Content-Type")
		captured.dpop = r.Header.Get("DPoP")
		if err := json.NewDecoder(r.Body).Decode(&captured.body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
}

const successTokenResponse = `{
  "accessToken": {
    "accessKeyId": "AKIANEW",
    "secretAccessKey": "new-secret",
    "sessionToken": "new-session-token"
  },
  "tokenType": "aws_sigv4",
  "expiresIn": 900,
  "refreshToken": "new-refresh-token",
  "idToken": "id.token.value"
}`

func TestExchangeAuthCode_RequestShapeAndDPoP(t *testing.T) {
	t.Parallel()
	var captured capturedRequest
	ts := newTokenServer(t, http.StatusOK, successTokenResponse, &captured)
	defer ts.Close()

	key, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("GenerateDPoPKey: %v", err)
	}
	c := &Client{BaseURL: ts.URL, HTTPClient: ts.Client(), Now: fixedNow}
	out, err := c.ExchangeAuthCode(context.Background(), key,
		ClientIDSameDevice, "auth-code-1", "verifier-1", "http://127.0.0.1:1234/oauth/callback")
	if err != nil {
		t.Fatalf("ExchangeAuthCode: %v", err)
	}

	if captured.method != http.MethodPost || captured.path != "/v1/token" {
		t.Errorf("request = %s %s, want POST /v1/token", captured.method, captured.path)
	}
	if captured.contentType != "application/json" {
		t.Errorf("Content-Type=%q", captured.contentType)
	}
	wantBody := map[string]any{
		"clientId":     ClientIDSameDevice,
		"grantType":    "authorization_code",
		"code":         "auth-code-1",
		"codeVerifier": "verifier-1",
		"redirectUri":  "http://127.0.0.1:1234/oauth/callback",
	}
	if len(captured.body) != len(wantBody) {
		t.Errorf("body has %d fields, want %d: %v", len(captured.body), len(wantBody), captured.body)
	}
	for k, v := range wantBody {
		if captured.body[k] != v {
			t.Errorf("body[%s]=%v, want %v", k, captured.body[k], v)
		}
	}

	verifyDPoPProof(t, captured.dpop, &key.PublicKey, ts.URL+"/v1/token")

	if out.AccessToken.AccessKeyID != "AKIANEW" || out.RefreshToken != "new-refresh-token" ||
		out.IDToken != "id.token.value" || out.ExpiresIn != 900 || out.TokenType != "aws_sigv4" {
		t.Errorf("unexpected token output: %+v", out)
	}
}

// verifyDPoPProof parses and cryptographically verifies the DPoP
// header captured by the fake endpoint.
func verifyDPoPProof(t *testing.T, jwt string, pub *ecdsa.PublicKey, wantHTU string) {
	t.Helper()
	if jwt == "" {
		t.Fatal("DPoP header missing from the token request")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("DPoP JWT has %d parts", len(parts))
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode DPoP payload: %v", err)
	}
	var payload struct {
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		IAT int64  `json:"iat"`
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("parse DPoP payload: %v", err)
	}
	if payload.HTM != "POST" || payload.HTU != wantHTU {
		t.Errorf("DPoP htm/htu = %q/%q, want POST/%q", payload.HTM, payload.HTU, wantHTU)
	}
	if payload.IAT != fixedNow().Unix() {
		t.Errorf("DPoP iat=%d, want the injected clock %d", payload.IAT, fixedNow().Unix())
	}
	if payload.JTI == "" {
		t.Error("DPoP jti missing")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("bad DPoP signature encoding: err=%v len=%d", err, len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("DPoP signature does not verify")
	}
}

func TestRefresh_RequestShape(t *testing.T) {
	t.Parallel()
	var captured capturedRequest
	ts := newTokenServer(t, http.StatusOK, successTokenResponse, &captured)
	defer ts.Close()

	key, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("GenerateDPoPKey: %v", err)
	}
	c := &Client{BaseURL: ts.URL, HTTPClient: ts.Client(), Now: fixedNow}
	if _, err := c.Refresh(context.Background(), key, ClientIDSameDevice, "old-refresh-token"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	wantBody := map[string]any{
		"clientId":     ClientIDSameDevice,
		"grantType":    "refresh_token",
		"refreshToken": "old-refresh-token",
	}
	if len(captured.body) != len(wantBody) {
		t.Errorf("body has %d fields, want %d: %v", len(captured.body), len(wantBody), captured.body)
	}
	for k, v := range wantBody {
		if captured.body[k] != v {
			t.Errorf("body[%s]=%v, want %v", k, captured.body[k], v)
		}
	}
	verifyDPoPProof(t, captured.dpop, &key.PublicKey, ts.URL+"/v1/token")
}

func TestCreateToken_ErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status   int
		code     string
		wantHint string
	}{
		{401, "TOKEN_EXPIRED", "nakama aws login"},
		{401, "AUTHCODE_EXPIRED", "nakama aws login"},
		{403, "USER_CREDENTIALS_CHANGED", "nakama aws login"},
		{403, "INSUFFICIENT_PERMISSIONS", "SignInLocalDevelopmentAccess"},
		{500, "SOMETHING_ELSE", "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			var captured capturedRequest
			body := `{"error":"` + tc.code + `","message":"secret-server-message"}`
			ts := newTokenServer(t, tc.status, body, &captured)
			defer ts.Close()

			key, err := GenerateDPoPKey()
			if err != nil {
				t.Fatalf("GenerateDPoPKey: %v", err)
			}
			c := &Client{BaseURL: ts.URL, HTTPClient: ts.Client()}
			_, err = c.Refresh(context.Background(), key, ClientIDSameDevice, "rt")
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is %T, want *APIError", err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Code != tc.code {
				t.Errorf("APIError=(%d,%q), want (%d,%q)", apiErr.StatusCode, apiErr.Code, tc.status, tc.code)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("error %q missing remediation hint %q", err.Error(), tc.wantHint)
			}
			// The server-controlled message must never surface.
			if strings.Contains(err.Error(), "secret-server-message") {
				t.Errorf("error leaks the server message: %q", err.Error())
			}
		})
	}
}

func TestCreateToken_MalformedSuccessBody(t *testing.T) {
	t.Parallel()
	var captured capturedRequest
	ts := newTokenServer(t, http.StatusOK, `{not json`, &captured)
	defer ts.Close()

	key, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("GenerateDPoPKey: %v", err)
	}
	c := &Client{BaseURL: ts.URL, HTTPClient: ts.Client()}
	if _, err := c.Refresh(context.Background(), key, ClientIDSameDevice, "rt"); err == nil {
		t.Fatal("expected error for malformed body")
	}
}

func TestCreateToken_EmptyAccessToken(t *testing.T) {
	t.Parallel()
	var captured capturedRequest
	ts := newTokenServer(t, http.StatusOK, `{"accessToken":{},"tokenType":"aws_sigv4","expiresIn":900,"refreshToken":"r"}`, &captured)
	defer ts.Close()

	key, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("GenerateDPoPKey: %v", err)
	}
	c := &Client{BaseURL: ts.URL, HTTPClient: ts.Client()}
	if _, err := c.Refresh(context.Background(), key, ClientIDSameDevice, "rt"); err == nil {
		t.Fatal("expected error for empty access token")
	}
}
