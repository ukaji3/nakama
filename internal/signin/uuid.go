package signin

import (
	"crypto/rand"
	"fmt"
)

// newUUID4 returns a random RFC 4122 version-4 UUID string built from
// crypto/rand. It is used for the OAuth `state` parameter and the DPoP
// `jti` claim, mirroring the AWS CLI's uuid.uuid4().
func newUUID4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
