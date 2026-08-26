package signin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ukaji3/nakama/internal/proxy"
)

// Sign-in API error codes (the `error` field of a non-2xx response).
const (
	// ErrorCodeTokenExpired (HTTP 401): the refresh token has expired.
	ErrorCodeTokenExpired = "TOKEN_EXPIRED"
	// ErrorCodeAuthCodeExpired (HTTP 401): the authorization code
	// expired before it could be exchanged.
	ErrorCodeAuthCodeExpired = "AUTHCODE_EXPIRED"
	// ErrorCodeUserCredentialsChanged (HTTP 403): the console
	// credentials behind the session changed (e.g. password change).
	ErrorCodeUserCredentialsChanged = "USER_CREDENTIALS_CHANGED"
	// ErrorCodeInsufficientPermissions (HTTP 403): the IAM principal
	// lacks the SignInLocalDevelopmentAccess managed policy.
	ErrorCodeInsufficientPermissions = "INSUFFICIENT_PERMISSIONS"
)

// APIError is a non-2xx response from the sign-in token endpoint. It
// deliberately carries ONLY the HTTP status and the error code plus a
// canned remediation — never the raw response body — so that no token
// material or server-controlled text leaks into logs or messages.
type APIError struct {
	// StatusCode is the HTTP status of the response.
	StatusCode int
	// Code is the sign-in error code (e.g. TOKEN_EXPIRED); empty when
	// the response body had no parsable error field.
	Code string
}

// Error implements error with an actionable, secret-free message.
func (e *APIError) Error() string {
	switch e.Code {
	case ErrorCodeAuthCodeExpired:
		return "sign-in failed: the authorization code expired before it could be exchanged (AUTHCODE_EXPIRED); " +
			"run `nakama aws login` again and complete the browser flow promptly"
	case ErrorCodeTokenExpired:
		return "the aws login session has expired (TOKEN_EXPIRED; refresh tokens last at most 12h); " +
			"run `nakama aws login` (or `aws login`) again to sign in"
	case ErrorCodeUserCredentialsChanged:
		return "the AWS Management Console credentials behind this login session have changed (USER_CREDENTIALS_CHANGED); " +
			"run `nakama aws login` (or `aws login`) again to establish a new session"
	case ErrorCodeInsufficientPermissions:
		return "the IAM principal is not allowed to use console-session sign-in (INSUFFICIENT_PERMISSIONS); " +
			"attach the AWS managed policy SignInLocalDevelopmentAccess to your IAM user/role and retry " +
			"(the account root user does not need it)"
	default:
		return fmt.Sprintf("the AWS sign-in endpoint returned HTTP %d (error code %q)", e.StatusCode, e.Code)
	}
}

// tokenRequest is the CreateOAuth2Token request body (payload trait:
// the object is the entire body, no wrapper). Optional fields are
// omitted per grant type.
type tokenRequest struct {
	ClientID     string `json:"clientId"`
	GrantType    string `json:"grantType"`
	Code         string `json:"code,omitempty"`
	CodeVerifier string `json:"codeVerifier,omitempty"`
	RedirectURI  string `json:"redirectUri,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

// TokenOutput is the CreateOAuth2Token success response body.
type TokenOutput struct {
	AccessToken struct {
		AccessKeyID     string `json:"accessKeyId"`
		SecretAccessKey string `json:"secretAccessKey"`
		SessionToken    string `json:"sessionToken"`
	} `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
	RefreshToken string `json:"refreshToken"`
	// IDToken is present only for the authorization_code grant.
	IDToken string `json:"idToken"`
}

// Client talks to the AWS Sign-In token endpoint. The zero value is
// usable: BaseURL defaults to DefaultBaseURL(), HTTPClient to a
// proxy-aware client with a 30s timeout, Now to time.Now.
type Client struct {
	// BaseURL is the sign-in endpoint origin, without a trailing slash
	// (e.g. https://ap-northeast-1.signin.aws.amazon.com). Tests point
	// it at an httptest server.
	BaseURL string
	// HTTPClient issues the requests. It must route through
	// internal/proxy for corporate-proxy support (the default does).
	HTTPClient *http.Client
	// Now is the clock used for the DPoP iat claim (tests freeze it).
	Now func() time.Time
	// newJTI generates the DPoP jti claim; tests may fix it.
	newJTI func() (string, error)
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{
		Transport: proxy.NewTransport(),
		Timeout:   30 * time.Second,
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) jti() (string, error) {
	if c.newJTI != nil {
		return c.newJTI()
	}
	return newUUID4()
}

// ExchangeAuthCode exchanges an authorization code (+ PKCE verifier)
// for an access token. redirectURI is always sent, for both the
// same-device and the cross-device flow.
func (c *Client) ExchangeAuthCode(ctx context.Context, key *ecdsa.PrivateKey, clientID, code, codeVerifier, redirectURI string) (*TokenOutput, error) {
	return c.createToken(ctx, key, tokenRequest{
		ClientID:     clientID,
		GrantType:    "authorization_code",
		Code:         code,
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
	})
}

// Refresh exchanges a refresh token for a new access token (the
// refresh token itself is rotated by the server).
func (c *Client) Refresh(ctx context.Context, key *ecdsa.PrivateKey, clientID, refreshToken string) (*TokenOutput, error) {
	return c.createToken(ctx, key, tokenRequest{
		ClientID:     clientID,
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
	})
}

// createToken POSTs to {base}/v1/token with a DPoP proof header and no
// SigV4 signature, and maps non-2xx responses to *APIError.
func (c *Client) createToken(ctx context.Context, key *ecdsa.PrivateKey, reqBody tokenRequest) (*TokenOutput, error) {
	tokenURL := c.baseURL() + "/v1/token"

	body, err := json.Marshal(reqBody) // #nosec G117 -- the refresh token is the wire payload of the refresh_token grant (TLS-protected request body); it never reaches logs or output
	if err != nil {
		return nil, fmt.Errorf("encode token request: %w", err)
	}
	jti, err := c.jti()
	if err != nil {
		return nil, err
	}
	// htu is the full token endpoint URL (matching the AWS CLI, which
	// signs the resolved request URL).
	dpop, err := buildDPoPHeader(key, tokenURL, c.now(), jti)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("DPoP", dpop)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("call the AWS sign-in endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bound the response read; token documents are small.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read sign-in response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil {
			apiErr.Code = e.Error
		}
		return nil, apiErr
	}

	var out TokenOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("the AWS sign-in endpoint returned a malformed token response")
	}
	if out.AccessToken.AccessKeyID == "" || out.AccessToken.SecretAccessKey == "" {
		return nil, errors.New("the AWS sign-in endpoint returned an empty access token")
	}
	return &out, nil
}
