package signin

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// testLoginSession is an ARN-shaped session identifier used across
// the signin tests.
const testLoginSession = "arn:aws:signin:::session/user-example-123"

// testAccountID must match the 5th ARN field of testSessionWithAccount.
const testAccountID = "111111111111"

// testSessionWithAccount is a session ARN carrying an account ID.
const testSessionWithAccount = "arn:aws:sts::111111111111:assumed-role/Admin/user"

// makeIDToken builds a structurally valid (unsigned) JWT whose payload
// carries the given sub claim.
func makeIDToken(t *testing.T, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadJSON, err := json.Marshal(map[string]string{"sub": sub})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signature := base64.RawURLEncoding.EncodeToString([]byte("test-signature"))
	return header + "." + payload + "." + signature
}

// fixedNow is a frozen clock for deterministic expiresAt values.
func fixedNow() time.Time {
	return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
}

// testToken returns a valid cached token document whose access token
// expires at the given time.
func testToken(t *testing.T, expiresAt time.Time) *Token {
	t.Helper()
	key, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("GenerateDPoPKey: %v", err)
	}
	pemText, err := MarshalDPoPKeyPEM(key)
	if err != nil {
		t.Fatalf("MarshalDPoPKeyPEM: %v", err)
	}
	return &Token{
		AccessToken: AccessToken{
			AccessKeyID:     "AKIAEXAMPLE",
			SecretAccessKey: "secret-example",
			SessionToken:    "session-token-example",
			AccountID:       testAccountID,
			ExpiresAt:       FormatExpiresAt(expiresAt),
		},
		TokenType:    "aws_sigv4",
		ClientID:     ClientIDSameDevice,
		RefreshToken: "refresh-token-example",
		IDToken:      makeIDToken(t, testSessionWithAccount),
		DPoPKey:      pemText,
	}
}
