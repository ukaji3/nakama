package signin

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

const testTokenURL = "https://ap-northeast-1.signin.aws.amazon.com/v1/token"

func TestBuildDPoPHeader_StructureAndSignature(t *testing.T) {
	t.Parallel()
	key, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("GenerateDPoPKey: %v", err)
	}
	iat := time.Date(2026, 8, 26, 3, 4, 5, 0, time.UTC)
	jti := "6f1c9a4e-1111-4222-8333-444455556666"

	jwt, err := buildDPoPHeader(key, testTokenURL, iat, jti)
	if err != nil {
		t.Fatalf("buildDPoPHeader: %v", err)
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	for i, p := range parts {
		if strings.ContainsAny(p, "=+/") {
			t.Errorf("part %d is not base64url-no-pad: %q", i, p)
		}
	}

	// --- header ---
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		JWK struct {
			Kty string `json:"kty"`
			X   string `json:"x"`
			Y   string `json:"y"`
			Crv string `json:"crv"`
		} `json:"jwk"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if header.Typ != "dpop+jwt" || header.Alg != "ES256" {
		t.Errorf("typ/alg = %q/%q", header.Typ, header.Alg)
	}
	if header.JWK.Kty != "EC" || header.JWK.Crv != "P-256" {
		t.Errorf("jwk kty/crv = %q/%q", header.JWK.Kty, header.JWK.Crv)
	}
	// x/y must be the 32-byte big-endian coordinates.
	xBytes, err := base64.RawURLEncoding.DecodeString(header.JWK.X)
	if err != nil {
		t.Fatalf("decode jwk.x: %v", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(header.JWK.Y)
	if err != nil {
		t.Fatalf("decode jwk.y: %v", err)
	}
	if len(xBytes) != 32 || len(yBytes) != 32 {
		t.Fatalf("coordinate lengths = %d/%d, want 32/32", len(xBytes), len(yBytes))
	}
	if new(big.Int).SetBytes(xBytes).Cmp(key.X) != 0 ||
		new(big.Int).SetBytes(yBytes).Cmp(key.Y) != 0 {
		t.Error("jwk coordinates do not match the public key")
	}

	// --- payload ---
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload struct {
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		IAT int64  `json:"iat"`
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.HTM != "POST" {
		t.Errorf("htm=%q, want POST", payload.HTM)
	}
	if payload.HTU != testTokenURL {
		t.Errorf("htu=%q, want the full token URL", payload.HTU)
	}
	if payload.IAT != iat.Unix() {
		t.Errorf("iat=%d, want %d", payload.IAT, iat.Unix())
	}
	if payload.JTI != jti {
		t.Errorf("jti=%q, want %q", payload.JTI, jti)
	}

	// --- signature: r||s (32 bytes each) over SHA256(h.p) ---
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature length=%d, want 64 (r||s padded to 32 bytes each)", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Error("ES256 signature does not verify against the embedded JWK key")
	}
}

func TestDPoPKeyPEM_RoundTrip(t *testing.T) {
	t.Parallel()
	key, err := GenerateDPoPKey()
	if err != nil {
		t.Fatalf("GenerateDPoPKey: %v", err)
	}
	pemText, err := MarshalDPoPKeyPEM(key)
	if err != nil {
		t.Fatalf("MarshalDPoPKeyPEM: %v", err)
	}
	if !strings.HasPrefix(pemText, "-----BEGIN EC PRIVATE KEY-----\n") ||
		!strings.HasSuffix(pemText, "-----END EC PRIVATE KEY-----\n") {
		t.Errorf("unexpected PEM framing:\n%s", pemText)
	}
	// RFC 1421: body lines wrapped at 64 characters.
	for _, line := range strings.Split(strings.TrimSpace(pemText), "\n") {
		if strings.HasPrefix(line, "-----") {
			continue
		}
		if len(line) > 64 {
			t.Errorf("PEM body line exceeds 64 chars: %d", len(line))
		}
	}

	parsed, err := ParseDPoPKeyPEM(pemText)
	if err != nil {
		t.Fatalf("ParseDPoPKeyPEM: %v", err)
	}
	if parsed.D.Cmp(key.D) != 0 {
		t.Error("round-tripped key does not match the original")
	}
}

func TestParseDPoPKeyPEM_Invalid(t *testing.T) {
	t.Parallel()
	if _, err := ParseDPoPKeyPEM("not a pem"); err == nil {
		t.Error("expected error for non-PEM input")
	}
	if _, err := ParseDPoPKeyPEM("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"); err == nil {
		t.Error("expected error for garbage DER")
	}
}
