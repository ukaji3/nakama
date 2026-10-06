package service

// Kiro Crew web access through code-server (Issue #71, ADR 0020).
//
//	browser ─ https://<crew>.cloudfront.net/…
//	  → CloudFront (crew)   X-Forwarded-Host: 15476.kirocrew.nakama.internal
//	  → code-server:8080    password cookie, then proxy-domain → port 15476
//	  → nginx 127.0.0.1:15476   Origin check, Host/Origin → localhost:5476
//	  → Kiro Crew 127.0.0.1:5476
//
// Enable and disable are idempotent and resumable: every step checks
// the current state first, and the crew distribution's ID is written to
// SSM the moment it exists so a retry never creates a second one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// ErrKiroCrewWebNotEnabled is returned when an instance has no crew
// distribution yet.
var ErrKiroCrewWebNotEnabled = errors.New("the Kiro Crew web access is not enabled for this instance")

func ssmKirocrewDistributionIDPath(name string) string {
	return naming.SSMPath(name, "meta", "kirocrew-distribution-id")
}

func ssmKirocrewDistributionDomainPath(name string) string {
	return naming.SSMPath(name, "meta", "kirocrew-distribution-domain")
}

// kirocrewRemoteTimeout bounds the wait for the SSM agent before the
// instance-side scripts run (the scripts themselves may install nginx).
const kirocrewRemoteTimeout = 10 * time.Minute

// kirocrewFunctionRetryInterval paces DeleteFunction retries while
// CloudFront still reports the function in use. A variable so tests do
// not wait for real.
var kirocrewFunctionRetryInterval = 20 * time.Second

// kirocrewInstance is the metadata the Kiro Crew operations need.
type kirocrewInstance struct {
	name           string
	instanceID     string
	kind           string
	distributionID string
	crewID         string
	crewDomain     string
}

func (s *Service) loadKirocrewInstance(ctx context.Context, name string) (*kirocrewInstance, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	m := &kirocrewInstance{name: name}
	for _, f := range []struct {
		dst  *string
		path string
	}{
		{&m.instanceID, ssmInstanceIDPath(name)},
		{&m.kind, ssmKindPath(name)},
		{&m.distributionID, ssmDistributionIDPath(name)},
		{&m.crewID, ssmKirocrewDistributionIDPath(name)},
		{&m.crewDomain, ssmKirocrewDistributionDomainPath(name)},
	} {
		v, err := s.ssmGetOptional(ctx, f.path)
		if err != nil {
			return nil, err
		}
		*f.dst = v
	}
	if m.instanceID == "" {
		return nil, fmt.Errorf("nakama instance %q not found (no /nakama/%s/meta/instance-id)", name, name)
	}
	if m.kind == instanceKindBare {
		return nil, fmt.Errorf("%q is a bare instance: it has no code-server to route Kiro Crew through; "+
			"reach the gateway with `nakama aws tunnel %s -L 5476:5476` instead", name, name)
	}
	return m, nil
}

func (s *Service) requireRunning(ctx context.Context, m *kirocrewInstance, verb string) error {
	state, _, err := s.fetchInstanceStateAndType(ctx, m.instanceID)
	if err != nil {
		return err
	}
	if state != "running" {
		return fmt.Errorf("instance %q is %s; start it first with `nakama aws start %s` and retry %s",
			m.name, orUnknown(state), m.name, verb)
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "not found"
	}
	return s
}

// EnableKiroCrewWebOutput is the result of EnableKiroCrewWeb.
type EnableKiroCrewWebOutput struct {
	Name           string `json:"name"`
	DistributionID string `json:"distribution_id"`
	URL            string `json:"url"`
}

// EnableKiroCrewWeb builds the crew distribution, the instance-side
// proxy and the redirect from code-server's /proxy/5476 path.
func (s *Service) EnableKiroCrewWeb(ctx context.Context, name string) (*EnableKiroCrewWebOutput, error) {
	m, err := s.loadKirocrewInstance(ctx, name)
	if err != nil {
		return nil, err
	}
	if m.distributionID == "" {
		return nil, fmt.Errorf("instance %q has no code-server distribution (/nakama/%s/meta/distribution-id)", name, name)
	}
	if err := s.requireRunning(ctx, m, "`nakama aws kirocrew enable`"); err != nil {
		return nil, err
	}

	// The crew distribution mirrors the code-server one: same origin
	// (the VPC origin in front of code-server:8080) and same WAF.
	existing, err := s.clients.CloudFront.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{
		Id: aws.String(m.distributionID),
	})
	if err != nil {
		return nil, fmt.Errorf("GetDistributionConfig(%s): %w", m.distributionID, err)
	}
	origin, wafARN, err := codeServerOrigin(existing.DistributionConfig)
	if err != nil {
		return nil, err
	}

	// 1. Crew distribution (created once; the ID is persisted at once).
	if err := s.ensureKirocrewDistribution(ctx, m, origin, wafARN); err != nil {
		return nil, err
	}

	// 2. Instance side: nginx + code-server proxy-domain.
	script, err := kirocrewEnableScript(m.crewDomain)
	if err != nil {
		return nil, err
	}
	slog.Info("kirocrew: configuring the instance (nginx, code-server proxy-domain)", "instance", m.instanceID)
	stdout, runErr := s.runRemoteScript(ctx, m.instanceID, script, "nakama: kirocrew enable "+name, kirocrewRemoteTimeout)
	if err := parseKirocrewScriptResult(stdout, runErr); err != nil {
		return nil, kirocrewSetupAdvice(name, err)
	}

	// 3. Redirect function, published to LIVE.
	fnARN, err := s.ensureKirocrewFunction(ctx, name, m.crewDomain)
	if err != nil {
		return nil, err
	}

	// 4. Redirect behaviors on the code-server distribution.
	if _, err := s.updateDistributionBehaviors(ctx, m.distributionID, func(cfg *cftypes.DistributionConfig) (*cftypes.DistributionConfig, error) {
		return withKirocrewBehaviors(cfg, fnARN)
	}); err != nil {
		return nil, err
	}

	// 5. Both distributions must be Deployed before the URL works.
	if err := s.waitDistributionsDeployed(ctx, m.crewID, m.distributionID); err != nil {
		return nil, err
	}
	return &EnableKiroCrewWebOutput{Name: name, DistributionID: m.crewID, URL: kirocrewURL(m.crewDomain)}, nil
}

// codeServerOrigin extracts the VPC origin and WAF from the
// code-server distribution, validating the shape nakama created.
func codeServerOrigin(cfg *cftypes.DistributionConfig) (cftypes.Origin, string, error) {
	if cfg == nil || cfg.Origins == nil || len(cfg.Origins.Items) != 1 {
		return cftypes.Origin{}, "", errors.New("the code-server distribution does not have the single origin nakama creates")
	}
	o := cfg.Origins.Items[0]
	if o.VpcOriginConfig == nil || aws.ToString(o.VpcOriginConfig.VpcOriginId) == "" || aws.ToString(o.DomainName) == "" {
		return cftypes.Origin{}, "", errors.New("the code-server distribution's origin is not a VPC origin")
	}
	return o, aws.ToString(cfg.WebACLId), nil
}

func (s *Service) ensureKirocrewDistribution(ctx context.Context, m *kirocrewInstance, origin cftypes.Origin, wafARN string) error {
	if m.crewID != "" {
		out, err := s.clients.CloudFront.GetDistribution(ctx, &cloudfront.GetDistributionInput{Id: aws.String(m.crewID)})
		switch {
		case err == nil && out.Distribution != nil && out.Distribution.DomainName != nil:
			m.crewDomain = *out.Distribution.DomainName
			slog.Info("kirocrew: distribution already exists", "id", m.crewID, "domain", m.crewDomain)
			return nil
		case awshelper.IsAPIError(err, "NoSuchDistribution"):
			slog.Warn("kirocrew: recorded distribution is gone; creating a new one", "id", m.crewID)
		case err != nil:
			return fmt.Errorf("GetDistribution(%s): %w", m.crewID, err)
		}
	}

	ref, err := callerReference()
	if err != nil {
		return err
	}
	cfg := kirocrewDistributionConfig(ref, m.name,
		aws.ToString(origin.VpcOriginConfig.VpcOriginId), aws.ToString(origin.DomainName), wafARN)
	out, err := s.clients.CloudFront.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{DistributionConfig: cfg})
	if err != nil {
		return fmt.Errorf("CreateDistribution(kirocrew): %w", err)
	}
	if out.Distribution == nil || out.Distribution.Id == nil || out.Distribution.DomainName == nil {
		return errors.New("CreateDistribution(kirocrew) returned nil")
	}
	m.crewID, m.crewDomain = *out.Distribution.Id, *out.Distribution.DomainName
	// Persist immediately: a retry after a later failure must find it.
	if err := s.writeInstanceMeta(ctx, m.name, map[string]string{
		ssmKirocrewDistributionIDPath(m.name):     m.crewID,
		ssmKirocrewDistributionDomainPath(m.name): m.crewDomain,
	}); err != nil {
		return fmt.Errorf("record the kirocrew distribution %s: %w", m.crewID, err)
	}
	slog.Info("kirocrew: distribution created", "id", m.crewID, "domain", m.crewDomain)
	if _, err := s.clients.CloudFront.TagResource(ctx, &cloudfront.TagResourceInput{
		Resource: out.Distribution.ARN,
		Tags:     awshelper.CloudFrontTags(tagging.ForInstance(m.name, tagging.ComponentCloudFront, kirocrewDistributionComment(m.name))),
	}); err != nil {
		slog.Warn("kirocrew: TagResource failed", "id", m.crewID, "error", err)
	}
	return nil
}

// ensureKirocrewFunction creates or updates the redirect function and
// publishes it, returning the LIVE function ARN.
func (s *Service) ensureKirocrewFunction(ctx context.Context, instanceName, crewDomain string) (string, error) {
	code, err := kirocrewRedirectFunctionCode(crewDomain)
	if err != nil {
		return "", err
	}
	fname := kirocrewFunctionName(instanceName)
	fcfg := &cftypes.FunctionConfig{
		Comment: aws.String("nakama: redirect /proxy/5476 to the Kiro Crew distribution of " + instanceName),
		Runtime: cftypes.FunctionRuntimeCloudfrontJs20,
	}
	var etag *string
	desc, err := s.clients.CloudFront.DescribeFunction(ctx, &cloudfront.DescribeFunctionInput{Name: aws.String(fname)})
	switch {
	case err == nil:
		upd, err := s.clients.CloudFront.UpdateFunction(ctx, &cloudfront.UpdateFunctionInput{
			Name: aws.String(fname), IfMatch: desc.ETag, FunctionCode: []byte(code), FunctionConfig: fcfg,
		})
		if err != nil {
			return "", fmt.Errorf("UpdateFunction(%s): %w", fname, err)
		}
		etag = upd.ETag
	case awshelper.IsAPIError(err, "NoSuchFunctionExists"):
		created, err := s.clients.CloudFront.CreateFunction(ctx, &cloudfront.CreateFunctionInput{
			Name: aws.String(fname), FunctionCode: []byte(code), FunctionConfig: fcfg,
		})
		if err != nil {
			return "", fmt.Errorf("CreateFunction(%s): %w", fname, err)
		}
		etag = created.ETag
	default:
		return "", fmt.Errorf("DescribeFunction(%s): %w", fname, err)
	}
	pub, err := s.clients.CloudFront.PublishFunction(ctx, &cloudfront.PublishFunctionInput{Name: aws.String(fname), IfMatch: etag})
	if err != nil {
		return "", fmt.Errorf("PublishFunction(%s): %w", fname, err)
	}
	if pub.FunctionSummary == nil || pub.FunctionSummary.FunctionMetadata == nil || pub.FunctionSummary.FunctionMetadata.FunctionARN == nil {
		return "", fmt.Errorf("PublishFunction(%s) returned no ARN", fname)
	}
	return *pub.FunctionSummary.FunctionMetadata.FunctionARN, nil
}

// updateDistributionBehaviors applies edit to a distribution's config
// and updates it only when something changed (idempotent re-runs do
// not trigger a redeploy). It reports whether an update was issued. A
// missing distribution is not an error.
func (s *Service) updateDistributionBehaviors(ctx context.Context, id string, edit func(*cftypes.DistributionConfig) (*cftypes.DistributionConfig, error)) (bool, error) {
	cur, err := s.clients.CloudFront.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{Id: aws.String(id)})
	if err != nil {
		if awshelper.IsAPIError(err, "NoSuchDistribution") {
			return false, nil
		}
		return false, fmt.Errorf("GetDistributionConfig(%s): %w", id, err)
	}
	next, err := edit(cur.DistributionConfig)
	if err != nil {
		return false, err
	}
	before, _ := json.Marshal(cur.DistributionConfig)
	after, _ := json.Marshal(next)
	if string(before) == string(after) {
		return false, nil
	}
	if _, err := s.clients.CloudFront.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
		Id: aws.String(id), DistributionConfig: next, IfMatch: cur.ETag,
	}); err != nil {
		return false, fmt.Errorf("UpdateDistribution(%s): %w", id, err)
	}
	slog.Info("kirocrew: distribution behaviors updated", "id", id)
	return true, nil
}

// waitDistributionsDeployed waits for every distribution in parallel
// and returns the first error, cancelling the remaining waits.
func (s *Service) waitDistributionsDeployed(ctx context.Context, ids ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(ids))
	for _, id := range ids {
		go func(id string) { errs <- s.waitDistributionDeployed(ctx, id) }(id)
	}
	var first error
	for range ids {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// DisableKiroCrewWebOutput is the result of DisableKiroCrewWeb.
type DisableKiroCrewWebOutput struct {
	Name string `json:"name"`
	// InstanceSkipped is true when the instance was not running, so the
	// nginx site and the code-server proxy-domain were left in place
	// (they are harmless without the distribution; re-run disable after
	// `nakama aws start` to remove them).
	InstanceSkipped bool `json:"instance_skipped,omitempty"`
}

// DisableKiroCrewWeb removes everything EnableKiroCrewWeb created. Each
// piece is skipped when already gone, so it completes partial states.
func (s *Service) DisableKiroCrewWeb(ctx context.Context, name string) (*DisableKiroCrewWebOutput, error) {
	m, err := s.loadKirocrewInstance(ctx, name)
	if err != nil {
		return nil, err
	}
	out := &DisableKiroCrewWebOutput{Name: name}

	// 1. Stop redirecting first, so no link points at a dying host. The
	// function can only be deleted once that change has deployed.
	if m.distributionID != "" {
		updated, err := s.updateDistributionBehaviors(ctx, m.distributionID, func(cfg *cftypes.DistributionConfig) (*cftypes.DistributionConfig, error) {
			return withoutKirocrewBehaviors(cfg), nil
		})
		if err != nil {
			return nil, err
		}
		if updated {
			if err := s.waitDistributionDeployed(ctx, m.distributionID); err != nil {
				return nil, err
			}
		}
	}
	// 2. The function, now that nothing references it.
	if err := s.deleteKirocrewFunction(ctx, name); err != nil {
		return nil, err
	}
	// 3. The crew distribution.
	if m.crewID != "" {
		if err := s.deleteDistribution(ctx, m.crewID); err != nil {
			return nil, fmt.Errorf("kirocrew distribution: %w", err)
		}
	}
	// 4. Instance side, when reachable.
	state, _, err := s.fetchInstanceStateAndType(ctx, m.instanceID)
	if err != nil {
		return nil, err
	}
	if state == "running" {
		stdout, runErr := s.runRemoteScript(ctx, m.instanceID, kirocrewDisableScript(), "nakama: kirocrew disable "+name, kirocrewRemoteTimeout)
		if err := parseKirocrewScriptResult(stdout, runErr); err != nil {
			return nil, err
		}
	} else {
		out.InstanceSkipped = true
		slog.Warn("kirocrew: instance is not running; left its nginx site and code-server proxy-domain in place",
			"instance", m.instanceID, "state", orUnknown(state))
	}
	// 5. Metadata last: until here a retry still finds the distribution.
	if _, err := s.clients.SSM.DeleteParameters(ctx, &ssm.DeleteParametersInput{
		Names: []string{ssmKirocrewDistributionIDPath(name), ssmKirocrewDistributionDomainPath(name)},
	}); err != nil {
		return nil, fmt.Errorf("DeleteParameters(kirocrew meta): %w", err)
	}
	return out, nil
}

// deleteKirocrewFunction deletes the redirect function, tolerating its
// absence and retrying while CloudFront still reports it in use (the
// association is released only after the distribution update deploys).
func (s *Service) deleteKirocrewFunction(ctx context.Context, instanceName string) error {
	fname := kirocrewFunctionName(instanceName)
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    kirocrewFunctionRetryInterval,
		Timeout:     10 * time.Minute,
		Description: fmt.Sprintf("delete function %s", fname),
	}, func(ctx context.Context) (bool, error) {
		desc, err := s.clients.CloudFront.DescribeFunction(ctx, &cloudfront.DescribeFunctionInput{Name: aws.String(fname)})
		if err != nil {
			if awshelper.IsAPIError(err, "NoSuchFunctionExists") {
				return true, nil
			}
			return false, fmt.Errorf("DescribeFunction(%s): %w", fname, err)
		}
		if _, err := s.clients.CloudFront.DeleteFunction(ctx, &cloudfront.DeleteFunctionInput{Name: aws.String(fname), IfMatch: desc.ETag}); err != nil {
			switch {
			case awshelper.IsAPIError(err, "NoSuchFunctionExists"):
				return true, nil
			case awshelper.IsAPIError(err, "FunctionInUse"):
				slog.Info("kirocrew: function still in use, waiting", "function", fname)
				return false, nil
			}
			return false, fmt.Errorf("DeleteFunction(%s): %w", fname, err)
		}
		slog.Info("kirocrew: function deleted", "function", fname)
		return true, nil
	})
}

// KiroCrewWeb describes an enabled crew distribution.
type KiroCrewWeb struct {
	Name           string `json:"name"`
	InstanceID     string `json:"instance_id"`
	State          string `json:"state"`
	DistributionID string `json:"distribution_id"`
	Domain         string `json:"domain"`
	URL            string `json:"url"`
	// Deployed is false while CloudFront is still propagating.
	Deployed bool `json:"deployed"`
}

// GetKiroCrewWeb returns the crew distribution of an instance, or an
// error wrapping ErrKiroCrewWebNotEnabled.
func (s *Service) GetKiroCrewWeb(ctx context.Context, name string) (*KiroCrewWeb, error) {
	m, err := s.loadKirocrewInstance(ctx, name)
	if err != nil {
		return nil, err
	}
	if m.crewID == "" || m.crewDomain == "" {
		return nil, fmt.Errorf("%w: run `nakama aws kirocrew enable %s` first", ErrKiroCrewWebNotEnabled, name)
	}
	if err := validateCloudFrontDomain(m.crewDomain); err != nil {
		return nil, err
	}
	state, _, err := s.fetchInstanceStateAndType(ctx, m.instanceID)
	if err != nil {
		return nil, err
	}
	out := &KiroCrewWeb{
		Name: name, InstanceID: m.instanceID, State: state,
		DistributionID: m.crewID, Domain: m.crewDomain, URL: kirocrewURL(m.crewDomain),
	}
	dist, err := s.clients.CloudFront.GetDistribution(ctx, &cloudfront.GetDistributionInput{Id: aws.String(m.crewID)})
	if err != nil {
		if awshelper.IsAPIError(err, "NoSuchDistribution") {
			return nil, fmt.Errorf("%w: the recorded distribution %s no longer exists; run `nakama aws kirocrew enable %s` again",
				ErrKiroCrewWebNotEnabled, m.crewID, name)
		}
		return nil, fmt.Errorf("GetDistribution(%s): %w", m.crewID, err)
	}
	out.Deployed = dist.Distribution != nil && aws.ToString(dist.Distribution.Status) == "Deployed"
	return out, nil
}

// kirocrewSetupAdvice adds the remedy to the precondition failures that
// mean Kiro Crew itself is missing or not running as a service. create
// installs it by default, but the install can fail without failing
// create (it is best-effort), and before v0.19.2 it always stopped short
// of the gateway service (Issue #79). Other errors are returned as is.
func kirocrewSetupAdvice(name string, err error) error {
	msg := err.Error()
	if !strings.Contains(msg, kirocrewErrNotInstalled) && !strings.Contains(msg, kirocrewErrNoGateway) {
		return err
	}
	return fmt.Errorf("%w\n"+
		"Kiro Crew and its gateway service must be running on the instance first. Install (or repair) them with:\n"+
		"  nakama aws setup rerun %s --preset kiro\n"+
		"then run `nakama aws kirocrew enable %s` again; it continues where it stopped.\n"+
		"If Kiro Crew is installed but only the service is missing: nakama aws connect %s 'chmod -R go-w ~/.kiro && kirocrew service install'",
		err, name, name, name)
}

// deleteKirocrewDistributionForInstance is DeleteInstance's first step when Kiro
// Crew web access was enabled: the crew distribution shares the VPC
// origin with the code-server distribution, so it has to go before the
// VPC origin can be deleted. The redirect function is deleted later by
// deleteKirocrewFunction, once the code-server distribution is gone.
func (s *Service) deleteKirocrewDistributionForInstance(ctx context.Context, crewID string) error {
	if crewID == "" {
		return nil
	}
	return s.deleteDistribution(ctx, crewID)
}
