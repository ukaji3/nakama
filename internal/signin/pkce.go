package signin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
)

// verifierLength is the PKCE code-verifier length used by AWS CLI v2
// (64 characters drawn from the RFC 7636 unreserved set).
const verifierLength = 64

// verifierCharset is the RFC 7636 unreserved character set
// ([A-Za-z0-9-._~]), matching the AWS CLI's
// string.ascii_letters + string.digits + '-._~'.
const verifierCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// GenerateCodeVerifier returns a 64-character PKCE code verifier drawn
// uniformly from verifierCharset using crypto/rand.
func GenerateCodeVerifier() (string, error) {
	charsetLen := big.NewInt(int64(len(verifierCharset)))
	buf := make([]byte, verifierLength)
	for i := range buf {
		n, err := rand.Int(rand.Reader, charsetLen)
		if err != nil {
			return "", fmt.Errorf("generate PKCE code verifier: %w", err)
		}
		buf[i] = verifierCharset[n.Int64()]
	}
	return string(buf), nil
}

// CodeChallenge computes the S256 PKCE code challenge for the given
// verifier: base64url (no padding) of SHA-256(verifier).
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
