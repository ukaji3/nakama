package signin

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

// ParseVerificationCode parses the verification string the user
// pastes from the browser in the cross-device (--remote) flow. It is
// expected to be base64 of a query string carrying `state` and `code`
// (mirroring the AWS CLI's CrossDeviceLoginTokenFetcher).
func ParseVerificationCode(verification string) (code, state string, err error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(verification))
	if err != nil {
		return "", "", errors.New("failed to decode the verification code (expected the base64 string shown in the browser)")
	}
	params, err := url.ParseQuery(string(decoded))
	if err != nil {
		return "", "", errors.New("failed to parse the verification code contents")
	}
	code = params.Get("code")
	state = params.Get("state")
	if code == "" || state == "" {
		return "", "", errors.New("the verification code does not contain an authorization code and state")
	}
	return code, state, nil
}
