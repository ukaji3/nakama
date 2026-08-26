package signin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// RefreshThreshold is how long before expiry a cached access token is
// still considered usable without refreshing (the AWS CLI's
// _REFRESH_THRESHOLD of 5 minutes).
const RefreshThreshold = 5 * time.Minute

// credentialsSource is the aws.Credentials Source tag for this
// provider.
const credentialsSource = "nakama-signin"

// RefreshingCredentialsProvider is an aws.CredentialsProvider that
// resolves credentials from the AWS-CLI-compatible login token cache
// and refreshes them natively (DPoP-signed refresh_token grant) when
// less than RefreshThreshold remains before expiry. The rotated
// refresh token is written back to the cache.
//
// It is safe for concurrent Retrieve calls (an internal mutex
// serializes cache access and refreshes) and is designed to be
// wrapped in aws.NewCredentialsCache.
type RefreshingCredentialsProvider struct {
	// LoginSession identifies the cached token (ARN-shaped identifier,
	// not a secret).
	LoginSession string
	// Cache is the token cache; nil means the default AWS-CLI cache.
	Cache *TokenCache
	// Client is the sign-in API client used for refreshes; nil means a
	// default Client.
	Client *Client
	// Now is the clock (tests freeze it); nil means time.Now.
	Now func() time.Time

	mu sync.Mutex
}

// NewRefreshingCredentialsProvider returns a provider for the given
// login session with default cache, client, and clock.
func NewRefreshingCredentialsProvider(loginSession string) *RefreshingCredentialsProvider {
	return &RefreshingCredentialsProvider{LoginSession: loginSession}
}

func (p *RefreshingCredentialsProvider) cache() *TokenCache {
	if p.Cache != nil {
		return p.Cache
	}
	return &TokenCache{}
}

func (p *RefreshingCredentialsProvider) client() *Client {
	if p.Client != nil {
		return p.Client
	}
	return &Client{}
}

func (p *RefreshingCredentialsProvider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Retrieve implements aws.CredentialsProvider.
func (p *RefreshingCredentialsProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	token, err := p.cache().Load(p.LoginSession)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf(
			"load the aws login token cache: %w; run `nakama aws login` (or `aws login`) to sign in", err)
	}
	if err := token.Validate(); err != nil {
		return aws.Credentials{}, fmt.Errorf(
			"%w; run `nakama aws login` (or `aws login`) to re-create it", err)
	}
	expiresAt, err := token.ExpiresAtTime()
	if err != nil {
		return aws.Credentials{}, fmt.Errorf(
			"%w; run `nakama aws login` (or `aws login`) to re-create it", err)
	}

	// Fresh enough: reuse the cached access token without any network
	// round trip.
	if expiresAt.Sub(p.now()) > RefreshThreshold {
		return credentialsFromToken(token, expiresAt), nil
	}

	key, err := ParseDPoPKeyPEM(token.DPoPKey)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf(
			"%w; run `nakama aws login` (or `aws login`) to re-create the token", err)
	}
	out, err := p.client().Refresh(ctx, key, token.ClientID, token.RefreshToken)
	if err != nil {
		// *APIError values already carry the error code and the
		// remediation; nothing secret is wrapped in.
		return aws.Credentials{}, fmt.Errorf("refresh the aws login session: %w", err)
	}

	newExpiresAt := p.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	// Overwrite only the refreshed fields: the access token and the
	// rotated refresh token. accountId is carried over from the old
	// token; idToken, dpopKey, clientId, and tokenType are unchanged.
	token.AccessToken = AccessToken{
		AccessKeyID:     out.AccessToken.AccessKeyID,
		SecretAccessKey: out.AccessToken.SecretAccessKey,
		SessionToken:    out.AccessToken.SessionToken,
		AccountID:       token.AccessToken.AccountID,
		ExpiresAt:       FormatExpiresAt(newExpiresAt),
	}
	token.RefreshToken = out.RefreshToken

	if err := p.cache().Save(p.LoginSession, token); err != nil {
		return aws.Credentials{}, err
	}
	return credentialsFromToken(token, newExpiresAt.UTC()), nil
}

// credentialsFromToken converts a cached token to aws.Credentials.
func credentialsFromToken(t *Token, expiresAt time.Time) aws.Credentials {
	return aws.Credentials{
		AccessKeyID:     t.AccessToken.AccessKeyID,
		SecretAccessKey: t.AccessToken.SecretAccessKey,
		SessionToken:    t.AccessToken.SessionToken,
		Source:          credentialsSource,
		CanExpire:       true,
		Expires:         expiresAt,
		AccountID:       t.AccessToken.AccountID,
	}
}
