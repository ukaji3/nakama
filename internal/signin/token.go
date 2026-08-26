package signin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// expiresAtLayout is the accessToken.expiresAt timestamp format used
// by the AWS CLI cache (UTC, second precision, literal Z suffix).
const expiresAtLayout = "2006-01-02T15:04:05Z"

// AccessToken is the accessToken object of the cached token document.
// Field declaration order matches the AWS CLI's insertion order so the
// emitted JSON is structurally identical.
type AccessToken struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken"`
	AccountID       string `json:"accountId"`
	ExpiresAt       string `json:"expiresAt"`
}

// Token is the full cached token document, byte-format compatible with
// the AWS CLI's ~/.aws/login/cache/*.json files. DPoPKey holds the
// SEC1 EC private key PEM; it MUST never be logged or echoed.
type Token struct {
	AccessToken  AccessToken `json:"accessToken"`
	TokenType    string      `json:"tokenType"`
	ClientID     string      `json:"clientId"`
	RefreshToken string      `json:"refreshToken"`
	IDToken      string      `json:"idToken"`
	DPoPKey      string      `json:"dpopKey"`
}

// Validate checks that every field required to use and refresh the
// token is present, mirroring the AWS CLI's LoginCredentialFetcher
// required-field check.
func (t *Token) Validate() error {
	switch {
	case t.AccessToken.AccessKeyID == "" || t.AccessToken.SecretAccessKey == "" || t.AccessToken.SessionToken == "":
		return errors.New("cached login token is missing access token fields")
	case t.AccessToken.ExpiresAt == "":
		return errors.New("cached login token is missing accessToken.expiresAt")
	case t.RefreshToken == "":
		return errors.New("cached login token is missing refreshToken")
	case t.DPoPKey == "":
		return errors.New("cached login token is missing dpopKey")
	case t.ClientID == "":
		return errors.New("cached login token is missing clientId")
	}
	return nil
}

// ExpiresAtTime parses accessToken.expiresAt as a UTC timestamp.
func (t *Token) ExpiresAtTime() (time.Time, error) {
	ts, err := time.Parse(expiresAtLayout, t.AccessToken.ExpiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("cached login token has malformed expiresAt: %w", err)
	}
	return ts, nil
}

// FormatExpiresAt renders a timestamp in the cache's expiresAt format.
func FormatExpiresAt(ts time.Time) string {
	return ts.UTC().Format(expiresAtLayout)
}

// LoginSessionFromIDToken extracts the login session identifier (the
// `sub` claim, an ARN-shaped string) from the idToken JWT. Only the
// payload segment is decoded; the signature is not verified because
// the token was received directly from the sign-in endpoint over TLS
// (same trust model as the AWS CLI).
func LoginSessionFromIDToken(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid JWT id token: cannot extract payload")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", errors.New("invalid JWT id token: malformed payload encoding")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("invalid JWT id token: malformed payload JSON")
	}
	if claims.Sub == "" {
		return "", errors.New("invalid JWT id token: missing sub claim")
	}
	return claims.Sub, nil
}

// AccountIDFromLoginSession extracts the AWS account ID from the
// ARN-shaped login session (the fifth colon-separated ARN field:
// arn:partition:service:region:ACCOUNT:resource).
func AccountIDFromLoginSession(session string) (string, error) {
	parts := strings.SplitN(session, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || parts[4] == "" {
		return "", fmt.Errorf("login session %q is not an ARN with an account ID", session)
	}
	return parts[4], nil
}
