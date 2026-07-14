// Package awsx defines narrow, mockable interfaces over the
// AWS SDK v2 client surface that nakama actually uses.
//
// All concrete service interactions go through these interfaces so
// that the service layer (internal/service/...) can be exercised
// with hand-rolled or generated mocks.
//
// Conventions:
//   - Each interface lives in its own file alongside a constructor
//     that wraps the SDK client.
//   - Method signatures use the SDK's input/output types directly
//     to minimize translation overhead.
//   - All methods accept a context.Context as the first parameter.
package awsx

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	nakamaConfig "github.com/ukaji3/nakama/internal/config"
)

// Clients aggregates every AWS client wrapper nakama uses.
// It is constructed once per CLI invocation by NewClients.
type Clients struct {
	EC2          EC2API
	CloudFront   CloudFrontAPI
	WAF          WAFAPI
	SSM          SSMAPI
	Tagging      TaggingAPI
	IAM          IAMAPI
	STS          STSAPI
	AMIResolver  AMIResolver
	IPDiscoverer IPDiscoverer

	// AccountID is the AWS account that holds the credentials used by
	// this Clients. Resolved once at construction time via STS.
	AccountID string

	// CallerARN is the ARN reported by STS GetCallerIdentity for the
	// credentials used by this Clients. It may be an IAM user ARN, an
	// STS assumed-role ARN, or the account root ARN. `nakama aws create
	// --with-admin/--with-role` uses it as the SimulatePrincipalPolicy
	// policy source (after assumed-role normalization, see
	// internal/service).
	CallerARN string

	// Region is the AWS region these clients target (nakamaConfig.Region).
	Region string

	// Credentials is the resolved credentials provider (after the
	// `aws login` fallback, if it was needed). `nakama aws connect`
	// injects these into the Session Manager data channel so that the
	// vendored plugin reuses nakama's credential resolution instead of
	// running its own chain.
	Credentials aws.CredentialsProvider
}

// NewClients loads the default AWS configuration and constructs the
// concrete client wrappers. The region is forced to nakamaConfig.Region.
//
// Credential resolution flow:
//
//  1. config.LoadDefaultConfig is called and STS GetCallerIdentity
//     is used as a connectivity probe.
//  2. If the probe succeeds, that configuration is used as-is.
//  3. If the probe fails, nakama assumes the gap is the missing
//     `aws login` (login_session) provider in SDK Go v2 (see
//     internal/awsx/awscli_credentials.go) and retries with
//     credentials sourced from `aws configure export-credentials`.
//
// AWS_PROFILE is honoured in both phases.
func NewClients(ctx context.Context) (*Clients, error) {
	profile := profileFromEnv()

	cfg, err := loadConfig(ctx, profile, nil /* default chain */)
	if err != nil {
		return nil, err
	}

	if accountID, callerARN, err := probeIdentity(ctx, cfg); err == nil {
		return assembleClients(cfg, accountID, callerARN), nil
	} else {
		slog.Debug("default credential chain failed; trying aws CLI fallback",
			"error", err)
	}

	// Fallback: use `aws configure export-credentials` to obtain
	// short-term credentials and feed them to the SDK.
	cliProvider := aws.NewCredentialsCache(&AWSCLICredentialsProvider{Profile: profile})
	cfg2, err := loadConfig(ctx, profile, cliProvider)
	if err != nil {
		return nil, err
	}
	accountID, callerARN, err := probeIdentity(ctx, cfg2)
	if err != nil {
		return nil, errors.New("AWS credential resolution failed; both the default credential chain " +
			"and the `aws configure export-credentials` fallback returned an error. " +
			"Run `aws sts get-caller-identity` to confirm your CLI authentication, " +
			"or set AWS_PROFILE explicitly. Original error: " + err.Error())
	}
	slog.Info("credentials resolved via aws CLI fallback (login_session compatibility shim)")
	return assembleClients(cfg2, accountID, callerARN), nil
}

func profileFromEnv() string {
	for _, k := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		if v := lookupEnvNonEmpty(k); v != "" {
			return v
		}
	}
	return ""
}

// loadConfig wraps config.LoadDefaultConfig with the optional
// profile/region overrides and (when non-nil) a custom credentials
// provider.
func loadConfig(ctx context.Context, profile string, provider aws.CredentialsProvider) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(nakamaConfig.Region),
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	if provider != nil {
		opts = append(opts, config.WithCredentialsProvider(provider))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// probeIdentity calls STS GetCallerIdentity with a short timeout to
// verify that cfg can actually obtain credentials. Returns the
// resolved AWS account ID and the caller ARN on success.
func probeIdentity(ctx context.Context, cfg aws.Config) (accountID, callerARN string, err error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, nil)
	if err != nil {
		return "", "", err
	}
	if out == nil || out.Account == nil {
		return "", "", errors.New("STS GetCallerIdentity returned nil account")
	}
	if out.Arn != nil {
		callerARN = *out.Arn
	}
	return *out.Account, callerARN, nil
}

// assembleClients builds the per-service wrappers around the given
// configuration. Credentials are baked into cfg already.
func assembleClients(cfg aws.Config, accountID, callerARN string) *Clients {
	c := newClientsFromConfig(cfg)
	c.AccountID = accountID
	c.CallerARN = callerARN
	return c
}

// awsConfig is exported only as a hook for tests that wish to
// substitute custom clients. Real callers should use NewClients.
func newClientsFromConfig(cfg aws.Config) *Clients {
	return &Clients{
		EC2:          newEC2Client(cfg),
		CloudFront:   newCloudFrontClient(cfg),
		WAF:          newWAFClient(cfg),
		SSM:          newSSMClient(cfg),
		Tagging:      newTaggingClient(cfg),
		IAM:          newIAMClient(cfg),
		STS:          newSTSClient(cfg),
		AMIResolver:  newAMIResolver(cfg),
		IPDiscoverer: NewHTTPIPDiscoverer(),
		Region:       cfg.Region,
		Credentials:  cfg.Credentials,
	}
}
