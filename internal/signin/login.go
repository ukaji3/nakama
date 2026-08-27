package signin

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"time"
)

// LoginOptions configures a Login flow run.
type LoginOptions struct {
	// Remote selects the cross-device flow (--remote): the
	// authorization URL is shown, and the user pastes the verification
	// code the browser displays after sign-in.
	Remote bool

	// Client is the sign-in API client; nil means a default Client.
	Client *Client
	// Cache is the token cache; nil means the default AWS-CLI cache.
	Cache *TokenCache

	// OnAuthURL is invoked once with the authorization URL. The caller
	// is responsible for displaying it and/or opening a browser.
	// Required.
	OnAuthURL func(url string)
	// Prompt reads the pasted verification code in the Remote flow
	// (the caller renders the prompt text). Required when Remote.
	Prompt func(ctx context.Context) (string, error)

	// Test hooks; zero values mean the real implementations.
	newState      func() (string, error)
	newVerifier   func() (string, error)
	newKey        func() (*ecdsa.PrivateKey, error)
	startCallback func(expectedState string) (*CallbackServer, error)
	now           func() time.Time
}

// LoginResult is the outcome of a successful login.
type LoginResult struct {
	// LoginSession is the session identifier (the idToken sub claim,
	// an ARN-shaped non-secret string).
	LoginSession string
	// AccountID is the AWS account extracted from LoginSession.
	AccountID string
	// Token is the cached token document. It contains secrets and
	// must not be logged.
	Token *Token
}

// Login runs the full sign-in flow: authorization (same-device local
// callback or cross-device paste), auth-code exchange with a fresh
// DPoP key, and an AWS-CLI-compatible cache write. It returns the
// login session identifier and the account ID.
func Login(ctx context.Context, opts LoginOptions) (*LoginResult, error) {
	if opts.OnAuthURL == nil {
		return nil, errors.New("signin.Login: OnAuthURL is required")
	}
	if opts.Remote && opts.Prompt == nil {
		return nil, errors.New("signin.Login: Prompt is required for the remote flow")
	}
	client := opts.Client
	if client == nil {
		client = &Client{}
	}
	cache := opts.Cache
	if cache == nil {
		cache = &TokenCache{}
	}
	newState := opts.newState
	if newState == nil {
		newState = newUUID4
	}
	newVerifier := opts.newVerifier
	if newVerifier == nil {
		newVerifier = GenerateCodeVerifier
	}
	newKey := opts.newKey
	if newKey == nil {
		newKey = GenerateDPoPKey
	}
	startCallback := opts.startCallback
	if startCallback == nil {
		startCallback = StartCallbackServer
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}

	key, err := newKey()
	if err != nil {
		return nil, err
	}
	verifier, err := newVerifier()
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, err
	}
	challenge := CodeChallenge(verifier)

	var (
		clientID    string
		redirectURI string
		code        string
	)
	if opts.Remote {
		clientID = ClientIDCrossDevice
		redirectURI = client.baseURL() + "/v1/sessions/confirmation"
		opts.OnAuthURL(BuildAuthorizeURL(client.baseURL(), clientID, state, redirectURI, challenge))

		verification, err := opts.Prompt(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the verification code: %w", err)
		}
		gotCode, gotState, err := ParseVerificationCode(verification)
		if err != nil {
			return nil, err
		}
		if gotState != state {
			return nil, errors.New("sign-in failed: the state parameter in the verification code does not match the expected value")
		}
		code = gotCode
	} else {
		clientID = ClientIDSameDevice
		server, err := startCallback(state)
		if err != nil {
			return nil, err
		}
		defer func() { _ = server.Close() }()
		redirectURI = server.RedirectURI()
		opts.OnAuthURL(BuildAuthorizeURL(client.baseURL(), clientID, state, redirectURI, challenge))

		code, err = server.WaitForCode(ctx, state)
		if err != nil {
			return nil, err
		}
	}

	out, err := client.ExchangeAuthCode(ctx, key, clientID, code, verifier, redirectURI)
	if err != nil {
		return nil, err
	}
	if out.IDToken == "" {
		return nil, errors.New("the AWS sign-in endpoint returned no id token")
	}

	session, err := LoginSessionFromIDToken(out.IDToken)
	if err != nil {
		return nil, err
	}
	accountID, err := AccountIDFromLoginSession(session)
	if err != nil {
		return nil, err
	}
	keyPEM, err := MarshalDPoPKeyPEM(key)
	if err != nil {
		return nil, err
	}

	token := &Token{
		AccessToken: AccessToken{
			AccessKeyID:     out.AccessToken.AccessKeyID,
			SecretAccessKey: out.AccessToken.SecretAccessKey,
			SessionToken:    out.AccessToken.SessionToken,
			AccountID:       accountID,
			ExpiresAt:       FormatExpiresAt(now().Add(time.Duration(out.ExpiresIn) * time.Second)),
		},
		TokenType:    out.TokenType,
		ClientID:     clientID,
		RefreshToken: out.RefreshToken,
		IDToken:      out.IDToken,
		DPoPKey:      keyPEM,
	}
	if err := cache.Save(session, token); err != nil {
		return nil, err
	}

	return &LoginResult{
		LoginSession: session,
		AccountID:    accountID,
		Token:        token,
	}, nil
}
