package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// awsCLICredentialsResponse mirrors the JSON document emitted by
// `aws configure export-credentials --format process`.
//
// The format is documented in the AWS CLI reference:
//
//	{
//	  "Version": 1,
//	  "AccessKeyId": "...",
//	  "SecretAccessKey": "...",
//	  "SessionToken": "...",
//	  "Expiration": "2026-05-28T05:00:00+00:00"
//	}
type awsCLICredentialsResponse struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

// parseAWSCLICredentials converts the raw JSON output of
// `aws configure export-credentials --format process` into an
// aws.Credentials value.
func parseAWSCLICredentials(raw []byte) (aws.Credentials, error) {
	var r awsCLICredentialsResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return aws.Credentials{}, fmt.Errorf("parse aws CLI export-credentials output: %w", err)
	}
	if r.AccessKeyID == "" || r.SecretAccessKey == "" {
		return aws.Credentials{}, errors.New("aws CLI returned empty credentials")
	}

	creds := aws.Credentials{
		AccessKeyID:     r.AccessKeyID,
		SecretAccessKey: r.SecretAccessKey,
		SessionToken:    r.SessionToken,
		Source:          "aws-cli:configure-export-credentials",
	}
	if r.Expiration != "" {
		exp, err := time.Parse(time.RFC3339, r.Expiration)
		if err == nil {
			creds.CanExpire = true
			creds.Expires = exp
		}
	}
	return creds, nil
}

// AWSCLICredentialsProvider is an aws.CredentialsProvider that
// retrieves short-term credentials by shelling out to
// `aws configure export-credentials --format process`.
//
// This works around the gap in AWS SDK for Go v2 (1.x) which, as of
// 2026-05, does not understand the `login_session` profile setting
// that `aws login` writes. The AWS CLI itself can resolve every
// modern credential source (login_session, sso_session, regular
// access keys, IMDS, ...) and exposes the resulting short-term
// credentials via the export-credentials command, so we delegate
// resolution to it as a last resort.
//
// Reference: https://docs.aws.amazon.com/sdkref/latest/guide/feature-login-credentials.html
//
// Usage:
//
//	provider := &AWSCLICredentialsProvider{}
//	creds, err := provider.Retrieve(ctx)
type AWSCLICredentialsProvider struct {
	// Profile is the AWS CLI profile name to pass via --profile.
	// Empty means use the CLI default selection rules.
	Profile string
	// Timeout bounds a single aws CLI invocation. Defaults to 10s.
	Timeout time.Duration
	// commandRunner is overridable for tests; nil means real exec.
	commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Retrieve implements aws.CredentialsProvider.
func (p *AWSCLICredentialsProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"configure", "export-credentials", "--format", "process"}
	if p.Profile != "" {
		args = append(args, "--profile", p.Profile)
	}

	runner := p.commandRunner
	if runner == nil {
		runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			// #nosec G204 -- name is hard-coded to "aws"; args contain
			// only static flags + the operator-supplied profile name
			// already validated upstream by AWS CLI itself.
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.Output()
		}
	}

	out, err := runner(cctx, "aws", args...)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("invoke aws CLI: %w", err)
	}
	return parseAWSCLICredentials(out)
}
