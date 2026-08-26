package signin

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateCodeVerifier_LengthAndCharset(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		v, err := GenerateCodeVerifier()
		if err != nil {
			t.Fatalf("GenerateCodeVerifier: %v", err)
		}
		if len(v) != 64 {
			t.Fatalf("len=%d, want 64", len(v))
		}
		for _, c := range v {
			if !strings.ContainsRune(verifierCharset, c) {
				t.Fatalf("verifier %q contains %q outside [A-Za-z0-9-._~]", v, c)
			}
		}
		if seen[v] {
			t.Fatalf("duplicate verifier generated: %q", v)
		}
		seen[v] = true
	}
}

func TestCodeChallenge_S256(t *testing.T) {
	t.Parallel()
	verifier := "test-verifier-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ.~_-"
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	got := CodeChallenge(verifier)
	if got != want {
		t.Errorf("CodeChallenge=%q, want %q", got, want)
	}
	if strings.ContainsAny(got, "=+/") {
		t.Errorf("challenge %q must be base64url without padding", got)
	}
	// SHA-256 digest is 32 bytes -> 43 base64url chars without padding.
	if len(got) != 43 {
		t.Errorf("challenge length=%d, want 43", len(got))
	}
}

// TestCodeChallenge_RFC7636Vector checks the S256 example from
// RFC 7636 appendix B.
func TestCodeChallenge_RFC7636Vector(t *testing.T) {
	t.Parallel()
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := CodeChallenge(verifier); got != want {
		t.Errorf("CodeChallenge=%q, want %q", got, want)
	}
}
