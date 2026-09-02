package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakeSSHSSM answers the instance-id lookup and records StartSession.
type fakeSSHSSM struct {
	awsx.SSMAPI

	instanceID string
	startIn    *ssm.StartSessionInput
	startErr   error
}

func (f *fakeSSHSSM) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if f.instanceID == "" {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(f.instanceID)}}, nil
}

func (f *fakeSSHSSM) StartSession(_ context.Context, in *ssm.StartSessionInput, _ ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	f.startIn = in
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &ssm.StartSessionOutput{
		SessionId:  aws.String("sess-ssh"),
		StreamUrl:  aws.String("wss://example"),
		TokenValue: aws.String("token-secret"),
	}, nil
}

func TestSSHProxyStartsStandardStreamSession(t *testing.T) {
	fake := &fakeSSHSSM{instanceID: "i-0123456789abcdef0"}
	svc := NewService(&awsx.Clients{SSM: fake, Region: "ap-northeast-1"})
	var gotPayload []byte
	svc.ssh = &sshHooks{runSession: func(_ context.Context, payload []byte) error {
		gotPayload = payload
		return nil
	}}

	if err := svc.SSHProxy(context.Background(), SSHProxyInput{Name: "dev01"}); err != nil {
		t.Fatalf("SSHProxy: %v", err)
	}
	if got := aws.ToString(fake.startIn.DocumentName); got != "AWS-StartSSHSession" {
		t.Errorf("DocumentName = %q, want AWS-StartSSHSession", got)
	}
	if got := fake.startIn.Parameters["portNumber"]; len(got) != 1 || got[0] != "22" {
		t.Errorf("portNumber = %v, want [22]", got)
	}
	if got := aws.ToString(fake.startIn.Target); got != "i-0123456789abcdef0" {
		t.Errorf("Target = %q, want the resolved instance id", got)
	}
	if !strings.Contains(string(gotPayload), "token-secret") {
		t.Error("the session payload must carry the StartSession token to the relay")
	}
}

func TestSSHProxyPortOverride(t *testing.T) {
	fake := &fakeSSHSSM{instanceID: "i-0123456789abcdef0"}
	svc := NewService(&awsx.Clients{SSM: fake, Region: "ap-northeast-1"})
	svc.ssh = &sshHooks{runSession: func(context.Context, []byte) error { return nil }}
	if err := svc.SSHProxy(context.Background(), SSHProxyInput{Name: "dev01", Port: 2222}); err != nil {
		t.Fatalf("SSHProxy: %v", err)
	}
	if got := fake.startIn.Parameters["portNumber"]; len(got) != 1 || got[0] != "2222" {
		t.Errorf("portNumber = %v, want [2222]", got)
	}
}

func TestSSHProxyUnknownInstance(t *testing.T) {
	svc := NewService(&awsx.Clients{SSM: &fakeSSHSSM{}, Region: "ap-northeast-1"})
	svc.ssh = &sshHooks{runSession: func(context.Context, []byte) error { return nil }}
	err := svc.SSHProxy(context.Background(), SSHProxyInput{Name: "nope"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

func TestSSHConfigSnippet(t *testing.T) {
	got := sshConfigSnippet("dev01", "/usr/local/bin/nakama", 22)
	for _, want := range []string{
		"Host nakama-dev01",
		"User ubuntu",
		"ProxyCommand /usr/local/bin/nakama aws ssh proxy dev01 --port 22",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("snippet lacks %q:\n%s", want, got)
		}
	}
}

func TestSSHConfigSnippetQuotesWindowsPath(t *testing.T) {
	got := sshConfigSnippet("dev01", `C:\Program Files\nakama\nakama.exe`, 2222)
	if !strings.Contains(got, `ProxyCommand "C:\Program Files\nakama\nakama.exe" aws ssh proxy dev01 --port 2222`) {
		t.Errorf("a path with spaces must be quoted:\n%s", got)
	}
}

func TestValidatePublicKey(t *testing.T) {
	if _, err := validatePublicKey("ssh-ed25519 AAAAC3Nz user@host\n"); err != nil {
		t.Errorf("a valid ed25519 key was rejected: %v", err)
	}
	if _, err := validatePublicKey("ecdsa-sha2-nistp256 AAAAE2 user@host"); err != nil {
		t.Errorf("a valid ecdsa key was rejected: %v", err)
	}
	if _, err := validatePublicKey(""); err == nil {
		t.Error("an empty file must be rejected")
	}
	// A private key is multi-line: rejecting it prevents leaking secret
	// material into authorized_keys.
	if _, err := validatePublicKey("-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"); err == nil {
		t.Error("a multi-line (private) key must be rejected")
	}
	if _, err := validatePublicKey("not-a-key blob"); err == nil {
		t.Error("a non-key line must be rejected")
	}
}

func TestValidatePublicKeyErrorHidesKeyMaterial(t *testing.T) {
	_, err := validatePublicKey("bogus-type SECRETBLOBSHOULDNOTAPPEAR comment")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "SECRETBLOBSHOULDNOTAPPEAR") {
		t.Errorf("the error must not echo key material: %v", err)
	}
}

func TestAuthorizeKeyScriptIsIdempotentAndQuoted(t *testing.T) {
	script := authorizeKeyScript("ssh-ed25519 AAAAC3Nz user@host")
	for _, want := range []string{
		"grep -qxF",                    // only append when absent
		"NAKAMA_KEY_ALREADY_PRESENT=1", // detectable by the caller
		"<<'NAKAMA_EOF'",               // quoted heredoc: no shell expansion of key material
		"install -d -m 0700 -o ubuntu", // correct ownership/mode
		"chmod 0600",                   // authorized_keys mode
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
}

func TestResolvePublicKeyExplicitAndFallback(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "mykey.pub")
	if err := os.WriteFile(explicit, []byte("ssh-ed25519 AAAAC3Nz user@host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, key, err := resolvePublicKey(explicit)
	if err != nil {
		t.Fatalf("resolvePublicKey(explicit): %v", err)
	}
	if path != explicit || key != "ssh-ed25519 AAAAC3Nz user@host" {
		t.Errorf("got (%q, %q), want the explicit key trimmed", path, key)
	}
	if _, _, err := resolvePublicKey(filepath.Join(dir, "missing.pub")); err == nil {
		t.Error("a missing explicit key must error")
	}
}

func TestWriteSSHSnippetStaysInNakamaConfigDir(t *testing.T) {
	dir := t.TempDir()
	old := nakamaConfigSubdirOverride
	nakamaConfigSubdirOverride = dir
	t.Cleanup(func() { nakamaConfigSubdirOverride = old })

	path, err := writeSSHSnippet("dev01", "Host nakama-dev01\n")
	if err != nil {
		t.Fatalf("writeSSHSnippet: %v", err)
	}
	if want := filepath.Join(dir, "ssh", "dev01.conf"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("snippet mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSSHSetupRendersSnippetWithoutTouchingKeys(t *testing.T) {
	dir := t.TempDir()
	old := nakamaConfigSubdirOverride
	nakamaConfigSubdirOverride = dir
	t.Cleanup(func() { nakamaConfigSubdirOverride = old })

	svc := NewService(&awsx.Clients{SSM: &fakeSSHSSM{instanceID: "i-0123456789abcdef0"}, Region: "ap-northeast-1"})
	res, err := svc.SSHSetup(context.Background(), SSHSetupInput{
		Name: "dev01", SkipKey: true, NakamaPath: "/usr/local/bin/nakama",
	})
	if err != nil {
		t.Fatalf("SSHSetup: %v", err)
	}
	if res.PublicKeyPath != "" {
		t.Errorf("PublicKeyPath = %q, want empty with SkipKey", res.PublicKeyPath)
	}
	if res.Alias != "nakama-dev01" {
		t.Errorf("Alias = %q", res.Alias)
	}
	if !strings.HasPrefix(res.IncludeLine, "Include ") || !strings.Contains(res.IncludeLine, res.SnippetPath) {
		t.Errorf("IncludeLine = %q", res.IncludeLine)
	}
	onDisk, err := os.ReadFile(res.SnippetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != res.Snippet {
		t.Error("the written snippet must match the reported one")
	}
}

func TestSSHSetupUnknownInstance(t *testing.T) {
	svc := NewService(&awsx.Clients{SSM: &fakeSSHSSM{}, Region: "ap-northeast-1"})
	_, err := svc.SSHSetup(context.Background(), SSHSetupInput{Name: "nope", SkipKey: true})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

func TestSSHProxyAuthErrorAdvisesLogin(t *testing.T) {
	fake := &fakeSSHSSM{
		instanceID: "i-0123456789abcdef0",
		startErr:   errors.New("ExpiredTokenException: token expired"),
	}
	svc := NewService(&awsx.Clients{SSM: fake, Region: "ap-northeast-1"})
	err := svc.SSHProxy(context.Background(), SSHProxyInput{Name: "dev01"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "StartSession") {
		t.Errorf("err = %v, want it to mention StartSession", err)
	}
}

func TestSSHProxySetsDisconnectGate(t *testing.T) {
	// A hanging ProxyCommand stalls ssh; the disconnect gate makes a
	// permanent disconnect end the process promptly (Issue #61).
	fake := &fakeSSHSSM{instanceID: "i-0123456789abcdef0"}
	svc := NewService(&awsx.Clients{SSM: fake, Region: "ap-northeast-1"})
	var gateDuringRun string
	svc.ssh = &sshHooks{runSession: func(context.Context, []byte) error {
		gateDuringRun = os.Getenv(disconnectExitCodeEnv)
		return nil
	}}
	if err := svc.SSHProxy(context.Background(), SSHProxyInput{Name: "dev01"}); err != nil {
		t.Fatalf("SSHProxy: %v", err)
	}
	if want := fmt.Sprint(DisconnectExitCode); gateDuringRun != want {
		t.Errorf("disconnect gate during the session = %q, want %q", gateDuringRun, want)
	}
	if got := os.Getenv(disconnectExitCodeEnv); got != "" {
		t.Errorf("the gate must be unset after the session, got %q", got)
	}
}
