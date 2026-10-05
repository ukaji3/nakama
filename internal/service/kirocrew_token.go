package service

// Fetching a Kiro Crew dashboard token without leaving it readable in
// AWS (Issue #71, ADR 0020).
//
// The token is minted on the instance by `kirocrew token`. It reaches
// nakama over SSM Run Command, whose invocation output (stdout) stays
// retrievable through GetCommandInvocation for up to 30 days. A link
// token is only valid for 5 minutes, but anyone with
// ssm:GetCommandInvocation could otherwise read live tokens off the
// history. So nakama generates an RSA key pair in memory per request,
// ships only the public key, and the instance returns the token
// encrypted with RSA-OAEP (SHA-256) via openssl; only the ciphertext is
// ever stored by SSM. Session Manager would avoid the history entirely,
// but its command mode is currently broken (Issue #70).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// kirocrewCipherLinePrefix starts the output line carrying the
	// RSA-encrypted token (the line format, not a credential).
	kirocrewCipherLinePrefix = "NAKAMA_KIROCREW_TOKEN_RSA"
	kirocrewTokenKeyBits     = 4096
	// kirocrewTokenTimeout bounds the wait for the SSM agent.
	kirocrewTokenTimeout = 2 * time.Minute
)

// kirocrewTokenPattern is the shape of a dashboard token (base64url
// segments joined by dots). The value goes into a URL and an HTML
// attribute, so anything else is refused rather than escaped. The
// length is checked separately: RE2 caps repeat counts at 1000.
var kirocrewTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

const (
	kirocrewTokenMinLen = 20
	kirocrewTokenMaxLen = 1024
)

func validKirocrewToken(token string) bool {
	return len(token) >= kirocrewTokenMinLen && len(token) <= kirocrewTokenMaxLen && kirocrewTokenPattern.MatchString(token)
}

// kirocrewTokenScript renders the remote script. pubPEM is the
// per-request public key.
func kirocrewTokenScript(pubPEM string) string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
fail() { echo "%[1]s $*"; exit 1; }
command -v openssl >/dev/null 2>&1 || fail "openssl is not installed on the instance"
key=$(mktemp)
trap 'rm -f "$key"' EXIT
cat > "$key" <<'NAKAMA_KIROCREW_PUBKEY'
%[2]sNAKAMA_KIROCREW_PUBKEY
out=$(sudo -u ubuntu -H bash -lc 'export PATH="$HOME/.local/bin:$PATH"; kirocrew token' 2>/dev/null) \
  || fail "kirocrew token failed (is the gateway running? check: systemctl status kirocrew)"
url=$(printf '%%s\n' "$out" | grep -m1 -E '^http://(localhost|127\.0\.0\.1|kirocrew\.localhost):%[3]d/?\?token=' || true)
[ -n "$url" ] || fail "kirocrew token printed no loopback URL"
token=${url#*\?token=}
token=${token%%%%&*}
cipher=$(printf '%%s' "$token" | openssl pkeyutl -encrypt -pubin -inkey "$key" \
  -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 | base64 -w0) \
  || fail "openssl could not encrypt the token"
unset out url token
echo "%[4]s $cipher"
`, kirocrewMarkerError, pubPEM, kirocrewGatewayPort, kirocrewCipherLinePrefix)
}

// newKirocrewTokenKey generates the per-request key pair and returns
// the private key and the PEM-encoded public key.
func newKirocrewTokenKey() (*rsa.PrivateKey, string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, kirocrewTokenKeyBits)
	if err != nil {
		return nil, "", fmt.Errorf("generate the token key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, "", fmt.Errorf("encode the token key: %w", err)
	}
	return priv, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// decryptKirocrewToken extracts and decrypts the token from the script
// output and validates its shape.
func decryptKirocrewToken(priv *rsa.PrivateKey, stdout string) (string, error) {
	var cipherB64 string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if reason, ok := strings.CutPrefix(line, kirocrewMarkerError+" "); ok {
			return "", fmt.Errorf("could not mint a Kiro Crew token on the instance: %s", reason)
		}
		if v, ok := strings.CutPrefix(line, kirocrewCipherLinePrefix+" "); ok {
			cipherB64 = strings.TrimSpace(v)
		}
	}
	if cipherB64 == "" {
		return "", fmt.Errorf("the instance returned no encrypted token")
	}
	cipher, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return "", fmt.Errorf("decode the encrypted token: %w", err)
	}
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, cipher, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt the token: %w", err)
	}
	token := string(plain)
	if !validKirocrewToken(token) {
		// Do not echo the value: it may be a real token in an
		// unexpected format.
		return "", fmt.Errorf("the token returned by kirocrew has an unexpected format (%d bytes)", len(token))
	}
	return token, nil
}

// MintKiroCrewToken runs `kirocrew token` on the instance and returns
// the dashboard token. The instance must be running.
func (s *Service) MintKiroCrewToken(ctx context.Context, name string) (string, error) {
	m, err := s.loadKirocrewInstance(ctx, name)
	if err != nil {
		return "", err
	}
	if err := s.requireRunning(ctx, m, "`nakama aws kirocrew`"); err != nil {
		return "", err
	}
	priv, pubPEM, err := newKirocrewTokenKey()
	if err != nil {
		return "", err
	}
	stdout, runErr := s.runRemoteScript(ctx, m.instanceID, kirocrewTokenScript(pubPEM),
		"nakama: kirocrew token "+name, kirocrewTokenTimeout)
	token, err := decryptKirocrewToken(priv, stdout)
	if err != nil {
		if runErr != nil && !strings.Contains(stdout, kirocrewMarkerError) {
			return "", fmt.Errorf("could not mint a Kiro Crew token on the instance: %w", runErr)
		}
		return "", err
	}
	return token, nil
}
