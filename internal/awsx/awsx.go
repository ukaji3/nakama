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
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	nakamaConfig "github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/profilestore"
	"github.com/ukaji3/nakama/internal/proxy"
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

// NewClients loads AWS configuration and constructs the concrete
// client wrappers. The region is forced to nakamaConfig.Region.
//
// Credential resolution flow (ADR 0011):
//
//  1. The nakama profile store (~/.config/nakama) is consulted first.
//     If an active profile is set (`nakama aws profile load`),
//     credentials are resolved EXCLUSIVELY through
//     `aws configure export-credentials` against nakama's own
//     generated AWS config (AWS_CONFIG_FILE), and verified with an
//     STS GetCallerIdentity probe. This path is fail-closed: any
//     failure aborts instead of falling back to another chain, so
//     nakama never silently operates on an unintended account. The
//     active profile also takes precedence over AWS_PROFILE.
//  2. Without an active profile, config.LoadDefaultConfig is called
//     and STS GetCallerIdentity is used as a connectivity probe.
//  3. If that probe fails, nakama assumes the gap is the missing
//     `aws login` (login_session) provider in SDK Go v2 (see
//     internal/awsx/awscli_credentials.go) and retries with
//     credentials sourced from `aws configure export-credentials`.
//
// AWS_PROFILE is honoured in phases 2 and 3.
func NewClients(ctx context.Context) (*Clients, error) {
	cfg, accountID, callerARN, err := resolveAWSConfig(ctx, defaultResolverHooks())
	if err != nil {
		return nil, err
	}
	return assembleClients(cfg, accountID, callerARN), nil
}

// ResolveCallerIdentity resolves credentials via the default chain
// with the `aws configure export-credentials` fallback — deliberately
// IGNORING any active nakama profile — and returns the STS caller
// identity. `nakama aws profile save` uses it to capture whichever
// account the user is currently logged into with `aws login`.
func ResolveCallerIdentity(ctx context.Context) (accountID, callerARN string, err error) {
	_, accountID, callerARN, err = resolveDefaultChain(ctx, defaultResolverHooks())
	return accountID, callerARN, err
}

// resolverHooks abstracts the individual steps of credential
// resolution so the decision logic in resolveAWSConfig can be unit
// tested without touching real AWS APIs, the aws CLI, or the user's
// configuration files.
type resolverHooks struct {
	// loadActiveProfile returns the active nakama profile (nil when
	// none is loaded) and the path of nakama's generated aws_config.
	loadActiveProfile func() (*profilestore.Profile, string, error)
	// profileFromEnv returns AWS_PROFILE / AWS_DEFAULT_PROFILE.
	profileFromEnv func() string
	// loadConfig wraps config.LoadDefaultConfig.
	loadConfig func(ctx context.Context, profile string, provider aws.CredentialsProvider) (aws.Config, error)
	// probe wraps the STS GetCallerIdentity connectivity check.
	probe func(ctx context.Context, cfg aws.Config) (accountID, callerARN string, err error)
	// newCLIProvider builds the export-credentials-based provider.
	newCLIProvider func(profileName, configFile string) aws.CredentialsProvider
}

func defaultResolverHooks() resolverHooks {
	return resolverHooks{
		loadActiveProfile: loadActiveProfile,
		profileFromEnv:    profileFromEnv,
		loadConfig:        loadConfig,
		probe:             probeIdentity,
		newCLIProvider: func(profileName, configFile string) aws.CredentialsProvider {
			return aws.NewCredentialsCache(&AWSCLICredentialsProvider{
				Profile:    profileName,
				ConfigFile: configFile,
			})
		},
	}
}

// loadActiveProfile reads the nakama profile store and returns the
// active profile, if any, plus the generated aws_config path.
func loadActiveProfile() (*profilestore.Profile, string, error) {
	store := profilestore.New()
	data, err := store.Load()
	if err != nil {
		return nil, "", fmt.Errorf("read nakama profile store: %w", err)
	}
	if data.Active == "" {
		return nil, "", nil
	}
	p, ok := data.ActiveProfile()
	if !ok {
		return nil, "", fmt.Errorf("nakama profile store is inconsistent: active account %s has no saved profile; "+
			"run `nakama aws profile unload` (or `nakama aws profile save` after `aws login`) to recover", data.Active)
	}
	configPath, err := store.AWSConfigPath()
	if err != nil {
		return nil, "", err
	}
	return &p, configPath, nil
}

// resolveAWSConfig performs the full credential resolution described
// in the NewClients doc comment and returns the ready-to-use
// aws.Config plus the probed caller identity.
func resolveAWSConfig(ctx context.Context, h resolverHooks) (aws.Config, string, string, error) {
	active, awsConfigPath, err := h.loadActiveProfile()
	if err != nil {
		return aws.Config{}, "", "", err
	}
	if active != nil {
		return resolveActiveProfile(ctx, h, active, awsConfigPath)
	}
	return resolveDefaultChain(ctx, h)
}

// resolveActiveProfile resolves credentials through the active nakama
// profile ONLY (fail-closed: no fallback to any other chain, so a
// broken session can never silently redirect operations to whatever
// account the default chain happens to resolve).
func resolveActiveProfile(ctx context.Context, h resolverHooks, active *profilestore.Profile, awsConfigPath string) (aws.Config, string, string, error) {
	provider := h.newCLIProvider(profilestore.ProfileName(active.AccountID), awsConfigPath)
	// The active profile takes precedence over AWS_PROFILE, so the
	// env profile is intentionally not passed here.
	cfg, err := h.loadConfig(ctx, "", provider)
	if err != nil {
		return aws.Config{}, "", "", activeProfileError(active, err)
	}
	accountID, callerARN, err := h.probe(ctx, cfg)
	if err != nil {
		return aws.Config{}, "", "", activeProfileError(active, err)
	}
	if accountID != active.AccountID {
		return aws.Config{}, "", "", fmt.Errorf(
			"active nakama profile %q expects account %s but the credentials resolved to account %s "+
				"(the underlying aws login session was likely replaced); "+
				"run `aws login` for account %s and `nakama aws profile save` again, "+
				"or `nakama aws profile unload` to use the default credential chain",
			active.Alias, active.AccountID, accountID, active.AccountID)
	}
	slog.Debug("credentials resolved via active nakama profile",
		"account", active.AccountID, "alias", active.Alias)
	return cfg, accountID, callerARN, nil
}

// activeProfileError wraps a resolution failure of the active nakama
// profile with actionable recovery steps.
func activeProfileError(active *profilestore.Profile, cause error) error {
	return fmt.Errorf(
		"failed to obtain credentials for the active nakama profile %q (account %s): %w\n"+
			"The aws login session for this account has probably expired (refresh tokens last at most 12h).\n"+
			"Recovery: run `aws login` for account %s again (then `nakama aws profile save` if it still fails), "+
			"or run `nakama aws profile unload` to fall back to the default credential chain",
		active.Alias, active.AccountID, cause, active.AccountID)
}

// resolveDefaultChain is the pre-ADR-0011 resolution flow: default
// credential chain first, then the `aws configure export-credentials`
// fallback. AWS_PROFILE is honoured in both phases.
func resolveDefaultChain(ctx context.Context, h resolverHooks) (aws.Config, string, string, error) {
	profile := h.profileFromEnv()

	cfg, err := h.loadConfig(ctx, profile, nil /* default chain */)
	if err != nil {
		return aws.Config{}, "", "", err
	}

	if accountID, callerARN, err := h.probe(ctx, cfg); err == nil {
		return cfg, accountID, callerARN, nil
	} else {
		slog.Debug("default credential chain failed; trying aws CLI fallback",
			"error", err)
	}

	// Fallback: use `aws configure export-credentials` to obtain
	// short-term credentials and feed them to the SDK.
	cliProvider := h.newCLIProvider(profile, "")
	cfg2, err := h.loadConfig(ctx, profile, cliProvider)
	if err != nil {
		return aws.Config{}, "", "", err
	}
	accountID, callerARN, err := h.probe(ctx, cfg2)
	if err != nil {
		return aws.Config{}, "", "", errors.New("AWS credential resolution failed; both the default credential chain " +
			"and the `aws configure export-credentials` fallback returned an error. " +
			"Run `aws sts get-caller-identity` to confirm your CLI authentication, " +
			"or set AWS_PROFILE explicitly. Original error: " + err.Error())
	}
	slog.Info("credentials resolved via aws CLI fallback (login_session compatibility shim)")
	return cfg2, accountID, callerARN, nil
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
//
// The HTTP client is always replaced with one whose Proxy is
// proxy.Func(): the SDK's default BuildableClient uses
// http.ProxyFromEnvironment, which ignores OS-level proxy settings
// (Windows WinHTTP/IE, PAC). Both credential-resolution paths — the
// active nakama profile and the default chain — funnel through this
// function, so every AWS API call gets the same proxy behavior.
func loadConfig(ctx context.Context, profile string, provider aws.CredentialsProvider) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(nakamaConfig.Region),
		config.WithHTTPClient(awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
			tr.Proxy = proxy.Func()
		})),
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
