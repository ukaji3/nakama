package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const testKirocrewToken = "eyJzdWIiOiJsb2NhbC1hcHAiLCJleHAiOjE3OTEyMjg4MTJ9.7n8abmoz4xQ0RMKQBQJeUeQ9NgJN52tqnwC7EDwZLOI"

// One key for the whole file: 4096-bit generation is slow.
var testTokenKey, testTokenPub = func() (*rsa.PrivateKey, string) {
	priv, pub, err := newKirocrewTokenKey()
	if err != nil {
		panic(err)
	}
	return priv, pub
}()

func encryptForTest(t *testing.T, plain string) string {
	t.Helper()
	c, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &testTokenKey.PublicKey, []byte(plain), nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(c)
}

func TestKirocrewTokenScript(t *testing.T) {
	s := kirocrewTokenScript(testTokenPub)
	for _, want := range []string{
		"#!/bin/bash\n",
		"set -euo pipefail",
		"<<'NAKAMA_KIROCREW_PUBKEY'", // quoted: the key is not expanded
		"-----BEGIN PUBLIC KEY-----",
		"rsa_padding_mode:oaep",
		"rsa_oaep_md:sha256",
		"rsa_mgf1_md:sha256",
		"trap 'rm -f \"$key\"' EXIT",
		"kirocrew token",
		kirocrewCipherLinePrefix,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("token script lacks %q", want)
		}
	}
	if strings.Contains(s, "set -x") {
		t.Error("xtrace would print the plaintext token into the command history")
	}
	// The only thing echoed is the ciphertext and error markers.
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "echo ") && strings.Contains(line, "$token") {
			t.Errorf("the plaintext token must never be echoed: %q", line)
		}
	}
	if _, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("bash -n: %v\n%s", err, out)
		}
	}
}

// TestKirocrewTokenScriptEndToEnd runs the real script with bash and
// openssl, with a stub `kirocrew` and `sudo` on PATH, and decrypts what
// it prints. This exercises the exact openssl invocation the instance
// uses (Ubuntu ships OpenSSL 3).
func TestKirocrewTokenScriptEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub sudo/kirocrew are shebang scripts, which Windows cannot execute; the script only runs on Ubuntu")
	}
	for _, tool := range []string{"bash", "openssl", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available", tool)
		}
	}
	bin := t.TempDir()
	// sudo -u ubuntu -H bash -lc '<cmd>' → run <cmd> locally.
	stub := map[string]string{
		"sudo": "#!/bin/bash\nwhile [ \"$1\" != \"bash\" ]; do shift; done\nshift; shift\nexec bash -c \"$1\"\n",
		"kirocrew": "#!/bin/bash\n[ \"$1\" = token ] || exit 2\n" +
			"echo 'http://localhost:5476?token=" + testKirocrewToken + "'\n" +
			"echo 'https://phone.example.ts.net/?token=other' \n",
	}
	for name, body := range stub {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(kirocrewTokenScript(testTokenPub))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), testKirocrewToken) {
		t.Fatalf("the plaintext token appeared in the script output:\n%s", out)
	}
	got, err := decryptKirocrewToken(testTokenKey, string(out))
	if err != nil {
		t.Fatalf("decrypt: %v\noutput:\n%s", err, out)
	}
	if got != testKirocrewToken {
		t.Errorf("token = %q, want %q", got, testKirocrewToken)
	}
}

func TestDecryptKirocrewToken(t *testing.T) {
	good := "noise\n" + kirocrewCipherLinePrefix + " " + encryptForTest(t, testKirocrewToken) + "\n"
	if got, err := decryptKirocrewToken(testTokenKey, good); err != nil || got != testKirocrewToken {
		t.Fatalf("round trip = %q, %v", got, err)
	}

	for name, stdout := range map[string]string{
		"remote error":   kirocrewMarkerError + " kirocrew token failed (is the gateway running?)\n",
		"no marker":      "something else\n",
		"not base64":     kirocrewCipherLinePrefix + " !!!\n",
		"wrong key":      kirocrewCipherLinePrefix + " " + base64.StdEncoding.EncodeToString(make([]byte, 512)) + "\n",
		"bad characters": kirocrewCipherLinePrefix + " " + encryptForTest(t, `abc"><script>alert(1)</script>xxxxxxxx`) + "\n",
		"too short":      kirocrewCipherLinePrefix + " " + encryptForTest(t, "short") + "\n",
	} {
		if _, err := decryptKirocrewToken(testTokenKey, stdout); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	// A malformed value must not be echoed in the error.
	_, err := decryptKirocrewToken(testTokenKey, kirocrewCipherLinePrefix+" "+encryptForTest(t, "secret-but-malformed value!")+"\n")
	if err == nil || strings.Contains(err.Error(), "secret-but-malformed") {
		t.Errorf("the error must not contain the decrypted value: %v", err)
	}
	// The remote reason surfaces.
	_, err = decryptKirocrewToken(testTokenKey, kirocrewMarkerError+" openssl is not installed on the instance\n")
	if err == nil || !strings.Contains(err.Error(), "openssl is not installed") {
		t.Errorf("remote reason lost: %v", err)
	}
}

func TestMintKiroCrewToken(t *testing.T) {
	logs := captureLogs(t)
	svc, _, sm := newKirocrewTestService(ec2types.InstanceStateNameRunning, kirocrewTestParams())
	// Play the instance: encrypt the token with the public key the
	// script carries, the way openssl does on the instance.
	sm.reply = func(script string) (ssmtypes.CommandInvocationStatus, string, string) {
		i := strings.Index(script, "-----BEGIN PUBLIC KEY-----")
		j := strings.Index(script, "-----END PUBLIC KEY-----")
		if i < 0 || j < 0 {
			return ssmtypes.CommandInvocationStatusFailed, kirocrewMarkerError + " no key\n", ""
		}
		pubPEM := script[i : j+len("-----END PUBLIC KEY-----")]
		return ssmtypes.CommandInvocationStatusSuccess, kirocrewCipherLinePrefix + " " + encryptWithPEM(t, pubPEM, testKirocrewToken) + "\n", ""
	}
	got, err := svc.MintKiroCrewToken(context.Background(), "dev01")
	if err != nil {
		t.Fatalf("MintKiroCrewToken: %v", err)
	}
	if got != testKirocrewToken {
		t.Errorf("token = %q", got)
	}
	if strings.Contains(logs.String(), testKirocrewToken) {
		t.Errorf("the token leaked into the logs:\n%s", logs.String())
	}
	if strings.Contains(sm.lastScript(), "PRIVATE KEY") {
		t.Error("only the public key may leave the process")
	}

	stopped, _, _ := newKirocrewTestService(ec2types.InstanceStateNameStopped, kirocrewTestParams())
	if _, err := stopped.MintKiroCrewToken(context.Background(), "dev01"); err == nil || !strings.Contains(err.Error(), "nakama aws start dev01") {
		t.Errorf("a stopped instance must be refused with the start hint, got %v", err)
	}
}

// encryptWithPEM encrypts like `openssl pkeyutl` with the PEM public key
// the script carries (RSA-OAEP, SHA-256 for both digest and MGF1).
func encryptWithPEM(t *testing.T, pubPEM, plain string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		t.Fatal("no PEM block in the script")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	c, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key.(*rsa.PublicKey), []byte(plain), nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(c)
}
