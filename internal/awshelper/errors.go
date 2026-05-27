package awshelper

import (
	"errors"

	"github.com/aws/smithy-go"
)

// IsAPIError reports whether err is an AWS SDK API error whose
// ErrorCode equals one of the supplied codes (case-sensitive).
//
// Use this sparingly: nakama prefers the lookup-then-create idempotency
// pattern over relying on AlreadyExists-style error codes. This helper
// exists for the cases where lookup is unavailable (e.g. tagging
// before resource creation has propagated).
func IsAPIError(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	got := apiErr.ErrorCode()
	for _, c := range codes {
		if got == c {
			return true
		}
	}
	return false
}
