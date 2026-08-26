package signin

import "net/url"

// BuildAuthorizeURL builds the {base}/v1/authorize URL with the exact
// query parameter order the AWS CLI emits: response_type, client_id,
// state, code_challenge_method, scope, redirect_uri, code_challenge.
// codeChallenge is already base64url (no padding) and is appended
// last, unre-encoded beyond standard query escaping (its alphabet is
// query-safe).
func BuildAuthorizeURL(baseURL, clientID, state, redirectURI, codeChallenge string) string {
	return baseURL + "/v1/authorize" +
		"?response_type=code" +
		"&client_id=" + url.QueryEscape(clientID) +
		"&state=" + url.QueryEscape(state) +
		"&code_challenge_method=SHA-256" +
		"&scope=openid" +
		"&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&code_challenge=" + url.QueryEscape(codeChallenge)
}
