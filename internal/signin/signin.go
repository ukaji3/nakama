// Package signin is a native Go client for the AWS Sign-In service
// that backs `aws login` (OAuth 2.0 authorization code + PKCE +
// DPoP, RFC 9449). It replaces nakama's former shell-outs to the
// Python AWS CLI so that credential acquisition AND refresh work in
// corporate audit-proxy environments where the CLI's certifi CA
// bundle fails TLS verification (ADR 0016).
//
// The wire protocol and the on-disk token cache are kept compatible
// with AWS CLI v2 (awscli/customizations/login, botocore
// LoginCredentialFetcher / build_dpop_header):
//
//   - authorization: GET {base}/v1/authorize?...&code_challenge=...
//   - token exchange / refresh: POST {base}/v1/token with a DPoP
//     header (ES256 JWT over a P-256 key), unsigned (no SigV4)
//   - cache: ~/.aws/login/cache/{hex(sha256(login_session))}.json
//     (AWS_LOGIN_CACHE_DIRECTORY overrides the directory)
//
// Compatibility means `aws login` and `nakama aws login` share the
// same cached session: whichever tool refreshes last wins, and the
// user can fall back to the AWS CLI at any time.
//
// SECURITY: access tokens, refresh tokens, id tokens and the DPoP
// private key never appear in argv, environment variables, log
// output, or error messages produced by this package. Errors carry
// only the sign-in error code and remediation guidance.
package signin

import nakamaconfig "github.com/ukaji3/nakama/internal/config"

// OAuth client identifiers registered by AWS for `aws login`-style
// developer sign-in (fixed ARNs, not secrets).
const (
	// ClientIDSameDevice is used for the local-callback browser flow.
	ClientIDSameDevice = "arn:aws:signin:::devtools/same-device"
	// ClientIDCrossDevice is used for the `--remote` paste-code flow.
	ClientIDCrossDevice = "arn:aws:signin:::devtools/cross-device"
)

// DefaultBaseURL returns the AWS Sign-In endpoint for nakama's fixed
// region (internal/config.Region).
func DefaultBaseURL() string {
	return "https://" + nakamaconfig.Region + ".signin.aws.amazon.com"
}
