package signin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// GenerateDPoPKey creates a new P-256 private key for DPoP proofs.
func GenerateDPoPKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate DPoP key: %w", err)
	}
	return key, nil
}

// dpopJWK is the embedded public JWK of the DPoP JWT header. Field
// order matches the AWS CLI's build_dpop_header (kty, x, y, crv).
type dpopJWK struct {
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Crv string `json:"crv"`
}

// dpopHeader is the DPoP JWT protected header (typ, alg, jwk).
type dpopHeader struct {
	Typ string  `json:"typ"`
	Alg string  `json:"alg"`
	JWK dpopJWK `json:"jwk"`
}

// dpopPayload is the DPoP JWT claim set (htm, htu, iat, jti).
type dpopPayload struct {
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	IAT int64  `json:"iat"`
	JTI string `json:"jti"`
}

// buildDPoPHeader builds the DPoP proof JWT (ES256, compact
// serialization) for a POST request to uri, faithfully porting the
// AWS CLI's build_dpop_header:
//
//   - jwk x/y are the 32-byte big-endian P-256 coordinates,
//     base64url-encoded without padding;
//   - the signing input is base64url(header) "." base64url(payload);
//   - the ECDSA signature is encoded as r||s with each integer
//     left-padded to 32 bytes, base64url-encoded without padding.
func buildDPoPHeader(key *ecdsa.PrivateKey, uri string, iat time.Time, jti string) (string, error) {
	// The JWK carries the 32-byte big-endian coordinates. Take them from
	// the SEC 1 uncompressed point (0x04 || X || Y, 65 bytes for P-256)
	// rather than the X/Y big.Int fields, which Go 1.26 deprecates.
	point, err := key.PublicKey.Bytes()
	if err != nil {
		return "", fmt.Errorf("encode the DPoP public key: %w", err)
	}
	if len(point) != 65 || point[0] != 4 {
		return "", errors.New("the DPoP key is not an uncompressed P-256 point")
	}
	header := dpopHeader{
		Typ: "dpop+jwt",
		Alg: "ES256",
		JWK: dpopJWK{
			Kty: "EC",
			X:   base64.RawURLEncoding.EncodeToString(point[1:33]),
			Y:   base64.RawURLEncoding.EncodeToString(point[33:]),
			Crv: "P-256",
		},
	}
	payload := dpopPayload{
		HTM: "POST",
		HTU: uri,
		IAT: iat.Unix(),
		JTI: jti,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("encode DPoP header: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode DPoP payload: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) +
		"." + base64.RawURLEncoding.EncodeToString(payloadJSON)

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign DPoP proof: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// MarshalDPoPKeyPEM serializes the private key as a SEC1 EC private
// key PEM block ("EC PRIVATE KEY", 64-column base64 body), the exact
// format the AWS CLI stores under the cache's dpopKey field.
func MarshalDPoPKeyPEM(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("serialize DPoP key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

// ParseDPoPKeyPEM parses a SEC1 EC private key PEM produced by
// MarshalDPoPKeyPEM or by the AWS CLI.
func ParseDPoPKeyPEM(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("cached token: dpopKey is not valid PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cached token: parse dpopKey: %w", err)
	}
	return key, nil
}
