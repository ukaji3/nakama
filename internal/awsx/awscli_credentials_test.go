package awsx

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseAWSCLICredentials_OK(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"Version": 1,
		"AccessKeyId": "AKIA1234567890",
		"SecretAccessKey": "secret-very-secret",
		"SessionToken": "session-token-abc",
		"Expiration": "2026-05-28T05:00:00Z"
	}`)
	got, err := parseAWSCLICredentials(raw)
	if err != nil {
		t.Fatalf("parseAWSCLICredentials: %v", err)
	}
	if got.AccessKeyID != "AKIA1234567890" {
		t.Errorf("AccessKeyID=%q", got.AccessKeyID)
	}
	if got.SecretAccessKey != "secret-very-secret" {
		t.Errorf("SecretAccessKey=%q", got.SecretAccessKey)
	}
	if got.SessionToken != "session-token-abc" {
		t.Errorf("SessionToken=%q", got.SessionToken)
	}
	if !got.CanExpire {
		t.Error("expected CanExpire=true when Expiration is present")
	}
	if got.Expires.IsZero() {
		t.Error("expected Expires to be set")
	}
	if got.Source == "" {
		t.Error("expected non-empty Source")
	}
}

func TestParseAWSCLICredentials_NoExpiration(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"Version": 1,
		"AccessKeyId": "AKIA",
		"SecretAccessKey": "s"
	}`)
	got, err := parseAWSCLICredentials(raw)
	if err != nil {
		t.Fatalf("parseAWSCLICredentials: %v", err)
	}
	if got.CanExpire {
		t.Error("expected CanExpire=false when Expiration is absent")
	}
}

func TestParseAWSCLICredentials_MalformedJSON(t *testing.T) {
	t.Parallel()
	_, err := parseAWSCLICredentials([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}

func TestParseAWSCLICredentials_EmptyKeys(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		[]byte(`{"Version":1}`),
		[]byte(`{"Version":1,"AccessKeyId":"k"}`),
		[]byte(`{"Version":1,"SecretAccessKey":"s"}`),
	}
	for i, c := range cases {
		if _, err := parseAWSCLICredentials(c); err == nil {
			t.Errorf("case %d: expected error on empty credentials, got nil", i)
		}
	}
}

func TestAWSCLICredentialsProvider_Retrieve_UsesCustomRunner(t *testing.T) {
	t.Parallel()
	want := []byte(`{"Version":1,"AccessKeyId":"AKIA-FAKE","SecretAccessKey":"secret"}`)
	called := false
	p := &AWSCLICredentialsProvider{
		commandRunner: func(_ context.Context, name string, args ...string) ([]byte, error) {
			called = true
			if name != "aws" {
				t.Errorf("name=%q, want aws", name)
			}
			if len(args) < 4 || args[0] != "configure" || args[1] != "export-credentials" || args[2] != "--format" || args[3] != "process" {
				t.Errorf("unexpected args: %v", args)
			}
			return want, nil
		},
	}
	got, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !called {
		t.Error("commandRunner was not called")
	}
	if got.AccessKeyID != "AKIA-FAKE" {
		t.Errorf("AccessKeyID=%q", got.AccessKeyID)
	}
}

func TestAWSCLICredentialsProvider_Retrieve_PassesProfile(t *testing.T) {
	t.Parallel()
	p := &AWSCLICredentialsProvider{
		Profile: "AdministratorAccess-886252662208",
		commandRunner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			found := false
			for i := 0; i < len(args)-1; i++ {
				if args[i] == "--profile" && args[i+1] == "AdministratorAccess-886252662208" {
					found = true
				}
			}
			if !found {
				t.Errorf("--profile <name> not present in args: %v", args)
			}
			return []byte(`{"Version":1,"AccessKeyId":"k","SecretAccessKey":"s"}`), nil
		},
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
}

func TestAWSCLICredentialsProvider_Retrieve_RunnerError(t *testing.T) {
	t.Parallel()
	want := errors.New("command not found")
	p := &AWSCLICredentialsProvider{
		commandRunner: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return nil, want
		},
	}
	_, err := p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, want) {
		t.Errorf("expected wrapped %v, got %v", want, err)
	}
}

func TestAWSCLICredentialsProvider_Retrieve_RespectsTimeout(t *testing.T) {
	t.Parallel()
	p := &AWSCLICredentialsProvider{
		Timeout: 10 * time.Millisecond,
		commandRunner: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	_, err := p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
