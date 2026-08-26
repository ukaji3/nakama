package signin

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildAuthorizeURL_AllQueryParameters(t *testing.T) {
	t.Parallel()
	base := "https://ap-northeast-1.signin.aws.amazon.com"
	state := "6f1c9a4e-1111-4222-8333-444455556666"
	redirect := "http://127.0.0.1:43210/oauth/callback"
	challenge := CodeChallenge("some-verifier")

	raw := BuildAuthorizeURL(base, ClientIDSameDevice, state, redirect, challenge)

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if u.Scheme != "https" || u.Host != "ap-northeast-1.signin.aws.amazon.com" || u.Path != "/v1/authorize" {
		t.Errorf("unexpected endpoint: %s://%s%s", u.Scheme, u.Host, u.Path)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             ClientIDSameDevice,
		"state":                 state,
		"code_challenge_method": "SHA-256",
		"scope":                 "openid",
		"redirect_uri":          redirect,
		"code_challenge":        challenge,
	}
	if len(q) != len(want) {
		t.Errorf("query has %d params, want %d: %v", len(q), len(want), q)
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("param %s=%q, want %q", k, got, v)
		}
	}
	// The redirect URI must be percent-encoded in the raw URL.
	if !strings.Contains(raw, "redirect_uri=http%3A%2F%2F127.0.0.1%3A43210%2Foauth%2Fcallback") {
		t.Errorf("redirect_uri not percent-encoded:\n%s", raw)
	}
	// The client_id ARN must be percent-encoded too.
	if !strings.Contains(raw, "client_id=arn%3Aaws%3Asignin%3A%3A%3Adevtools%2Fsame-device") {
		t.Errorf("client_id not percent-encoded:\n%s", raw)
	}
	// The code challenge (base64url alphabet) must appear verbatim.
	if !strings.HasSuffix(raw, "&code_challenge="+challenge) {
		t.Errorf("code_challenge must be the last parameter, verbatim:\n%s", raw)
	}
}

func TestLoginSessionFromIDToken(t *testing.T) {
	t.Parallel()
	token := makeIDToken(t, testSessionWithAccount)
	got, err := LoginSessionFromIDToken(token)
	if err != nil {
		t.Fatalf("LoginSessionFromIDToken: %v", err)
	}
	if got != testSessionWithAccount {
		t.Errorf("session=%q, want %q", got, testSessionWithAccount)
	}
}

func TestLoginSessionFromIDToken_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"not a jwt":       "only-one-part",
		"two parts":       "a.b",
		"bad base64":      "a.!!!.c",
		"bad json":        "a." + "bm90LWpzb24" + ".c", // "not-json"
		"missing sub":     makeIDToken(t, ""),
		"empty":           "",
		"four parts":      "a.b.c.d",
		"whitespace only": "   ",
	}
	for name, tok := range cases {
		if _, err := LoginSessionFromIDToken(tok); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAccountIDFromLoginSession(t *testing.T) {
	t.Parallel()
	got, err := AccountIDFromLoginSession(testSessionWithAccount)
	if err != nil {
		t.Fatalf("AccountIDFromLoginSession: %v", err)
	}
	if got != testAccountID {
		t.Errorf("accountID=%q, want %q", got, testAccountID)
	}

	for _, bad := range []string{"", "not-an-arn", "arn:aws:signin", "arn:aws:sts:::user/x", "x:aws:sts::123:y"} {
		if _, err := AccountIDFromLoginSession(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestTokenValidateAndExpiresAt(t *testing.T) {
	t.Parallel()
	tok := testToken(t, fixedNow().Add(10*time.Minute))
	if err := tok.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ts, err := tok.ExpiresAtTime()
	if err != nil {
		t.Fatalf("ExpiresAtTime: %v", err)
	}
	if !ts.Equal(fixedNow().Add(10 * time.Minute)) {
		t.Errorf("expiresAt=%v", ts)
	}

	broken := *tok
	broken.RefreshToken = ""
	if err := broken.Validate(); err == nil {
		t.Error("expected validation error for missing refreshToken")
	}
	broken = *tok
	broken.DPoPKey = ""
	if err := broken.Validate(); err == nil {
		t.Error("expected validation error for missing dpopKey")
	}
	broken = *tok
	broken.ClientID = ""
	if err := broken.Validate(); err == nil {
		t.Error("expected validation error for missing clientId")
	}
	broken = *tok
	broken.AccessToken.AccessKeyID = ""
	if err := broken.Validate(); err == nil {
		t.Error("expected validation error for missing access token")
	}
}

func TestFormatExpiresAt_UTCSecondPrecision(t *testing.T) {
	t.Parallel()
	jst := time.FixedZone("JST", 9*3600)
	ts := time.Date(2026, 8, 26, 21, 4, 5, 999999999, jst) // 12:04:05 UTC
	if got := FormatExpiresAt(ts); got != "2026-08-26T12:04:05Z" {
		t.Errorf("FormatExpiresAt=%q", got)
	}
}
