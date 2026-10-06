package service

// CloudFront half of the Kiro Crew web access (Issue #71, ADR 0020).
// Pure builders only; kirocrew.go drives the API calls.

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

// kirocrewPathPatterns are the code-server proxy paths a terminal link
// (http://localhost:5476/...) is rewritten to by code-server. The
// existing distribution redirects them to the crew distribution, so
// clicking the link printed by `kirocrew token` just works.
var kirocrewPathPatterns = []string{
	fmt.Sprintf("/proxy/%d*", kirocrewGatewayPort),
	fmt.Sprintf("/absproxy/%d*", kirocrewGatewayPort),
}

// kirocrewFunctionName names the redirect function for an instance.
// CloudFront function names allow [a-zA-Z0-9-_] up to 64 chars; nakama
// instance names are [a-z0-9-] up to 32, so this always fits.
func kirocrewFunctionName(instanceName string) string {
	return "nakama-" + instanceName + "-kirocrew"
}

// kirocrewDistributionComment identifies the crew distribution.
func kirocrewDistributionComment(instanceName string) string {
	return "nakama-" + instanceName + "-kirocrew"
}

// kirocrewDistributionConfig is the crew distribution: the same shape
// as the code-server one (same VPC origin, WAF, policies), plus the
// X-Forwarded-Host header that makes code-server route every request to
// the Kiro Crew proxy. CloudFront overwrites a viewer-supplied header
// of the same name, so the browser cannot steer it to another port.
func kirocrewDistributionConfig(callerRef, instanceName, vpcOriginID, originDomain, wafWebACLARN string) *cftypes.DistributionConfig {
	return distributionConfig(distributionParams{
		callerReference: callerRef,
		comment:         kirocrewDistributionComment(instanceName),
		vpcOriginID:     vpcOriginID,
		originDomain:    originDomain,
		wafWebACLARN:    wafWebACLARN,
		originHeaders:   map[string]string{"X-Forwarded-Host": kirocrewForwardedHost()},
	})
}

// kirocrewRedirectFunctionCode is the viewer-request function attached
// to the existing distribution's /proxy/5476* behaviors. It answers with
// a 302 to the same path and query on the crew distribution. The path
// test is exact so /proxy/54760 (another port) passes through untouched.
func kirocrewRedirectFunctionCode(crewDomain string) (string, error) {
	if err := validateCloudFrontDomain(crewDomain); err != nil {
		return "", err
	}
	return fmt.Sprintf(`// Managed by nakama (ADR 0020). Redirects code-server's proxy path for
// the Kiro Crew gateway to the dedicated Kiro Crew distribution.
var CREW = 'https://%s';
var PATTERN = /^\/(?:abs)?proxy\/%d(\/.*)?$/;

function encodeQuery(qs) {
  var parts = [];
  for (var key in qs) {
    var entry = qs[key];
    var values = entry.multiValue ? entry.multiValue : [entry];
    for (var i = 0; i < values.length; i++) {
      var v = values[i].value;
      parts.push(encodeURIComponent(key) + (v === undefined || v === '' ? '' : '=' + encodeURIComponent(v)));
    }
  }
  return parts.length ? '?' + parts.join('&') : '';
}

function handler(event) {
  var request = event.request;
  var match = PATTERN.exec(request.uri);
  if (!match) {
    return request;
  }
  var rest = match[1] ? match[1] : '/';
  return {
    statusCode: 302,
    statusDescription: 'Found',
    headers: {
      'location': { value: CREW + rest + encodeQuery(request.querystring) },
      'cache-control': { value: 'no-store' }
    }
  };
}
`, crewDomain, kirocrewGatewayPort), nil
}

// isKirocrewPathPattern reports whether a behavior is one nakama owns.
func isKirocrewPathPattern(p string) bool {
	for _, own := range kirocrewPathPatterns {
		if p == own {
			return true
		}
	}
	return false
}

// withoutKirocrewBehaviors returns a copy of cfg with nakama's Kiro
// Crew behaviors removed. Other behaviors are kept in order.
func withoutKirocrewBehaviors(cfg *cftypes.DistributionConfig) *cftypes.DistributionConfig {
	out := *cfg
	var kept []cftypes.CacheBehavior
	if cfg.CacheBehaviors != nil {
		for _, b := range cfg.CacheBehaviors.Items {
			if !isKirocrewPathPattern(aws.ToString(b.PathPattern)) {
				kept = append(kept, b)
			}
		}
	}
	out.CacheBehaviors = &cftypes.CacheBehaviors{Quantity: quantity(len(kept))}
	if len(kept) > 0 {
		out.CacheBehaviors.Items = kept
	}
	return &out
}

// withKirocrewBehaviors returns a copy of cfg with the redirect
// behaviors in front of any others, replacing earlier copies, so the
// call is idempotent. Each one is a copy of the default behavior with
// nakama's function as its only edge code (kirocrewCacheBehavior).
func withKirocrewBehaviors(cfg *cftypes.DistributionConfig, functionARN string) (*cftypes.DistributionConfig, error) {
	def := cfg.DefaultCacheBehavior
	if def == nil || def.TargetOriginId == nil {
		return nil, fmt.Errorf("distribution has no default cache behavior to mirror")
	}
	base := withoutKirocrewBehaviors(cfg)
	behaviors := make([]cftypes.CacheBehavior, 0, len(kirocrewPathPatterns)+int(aws.ToInt32(base.CacheBehaviors.Quantity)))
	for _, pattern := range kirocrewPathPatterns {
		behaviors = append(behaviors, kirocrewCacheBehavior(def, pattern, functionARN))
	}
	behaviors = append(behaviors, base.CacheBehaviors.Items...)
	base.CacheBehaviors = &cftypes.CacheBehaviors{
		Quantity: quantity(len(behaviors)),
		Items:    behaviors,
	}
	return base, nil
}

// kirocrewCacheBehavior builds one redirect behavior from the default
// behavior.
//
// Every field is copied. UpdateDistribution, unlike CreateDistribution,
// rejects a behavior that omits fields the API always returns ("The
// parameter SmoothStreaming flag is missing.", Issue #73); copying them
// also keeps the redirect paths under the same security settings as the
// rest of the distribution (signed URLs, response headers, logging).
// TestKirocrewCacheBehaviorCopiesEveryField fails when an SDK update
// adds a field that is not handled here.
//
// Only the edge code is nakama's: the redirect function on
// viewer-request and no Lambda@Edge (a viewer-request Lambda cannot
// share a behavior with a viewer-request function).
//
// def normally comes from GetDistributionConfig, which fills every
// field. When it does not (a config nakama built itself), the gaps get
// the values the API reports for an unset field, so the result is
// always accepted by UpdateDistribution.
func kirocrewCacheBehavior(def *cftypes.DefaultCacheBehavior, pattern, functionARN string) cftypes.CacheBehavior {
	b := cftypes.CacheBehavior{
		PathPattern: aws.String(pattern),

		AllowedMethods:          def.AllowedMethods,
		CachePolicyId:           def.CachePolicyId,
		Compress:                def.Compress,
		FieldLevelEncryptionId:  def.FieldLevelEncryptionId,
		GrpcConfig:              def.GrpcConfig,
		OriginRequestPolicyId:   def.OriginRequestPolicyId,
		RealtimeLogConfigArn:    def.RealtimeLogConfigArn,
		ResponseHeadersPolicyId: def.ResponseHeadersPolicyId,
		SmoothStreaming:         def.SmoothStreaming,
		TargetOriginId:          def.TargetOriginId,
		TrustedKeyGroups:        def.TrustedKeyGroups,
		TrustedSigners:          def.TrustedSigners,
		ViewerProtocolPolicy:    def.ViewerProtocolPolicy,

		// The legacy cache settings are deprecated in favour of cache
		// policies, but a distribution edited to use them still requires
		// them on every behavior; nakama's own distributions leave them nil.
		DefaultTTL:      def.DefaultTTL,      //nolint:staticcheck // SA1019: mirrored as-is, see above
		ForwardedValues: def.ForwardedValues, //nolint:staticcheck // SA1019: mirrored as-is, see above
		MaxTTL:          def.MaxTTL,          //nolint:staticcheck // SA1019: mirrored as-is, see above
		MinTTL:          def.MinTTL,          //nolint:staticcheck // SA1019: mirrored as-is, see above

		FunctionAssociations: &cftypes.FunctionAssociations{
			Quantity: aws.Int32(1),
			Items: []cftypes.FunctionAssociation{{
				EventType:   cftypes.EventTypeViewerRequest,
				FunctionARN: aws.String(functionARN),
			}},
		},
		LambdaFunctionAssociations: &cftypes.LambdaFunctionAssociations{Quantity: aws.Int32(0)},
	}
	if b.Compress == nil {
		b.Compress = aws.Bool(false)
	}
	if b.SmoothStreaming == nil {
		b.SmoothStreaming = aws.Bool(false)
	}
	if b.FieldLevelEncryptionId == nil {
		b.FieldLevelEncryptionId = aws.String("")
	}
	if b.TrustedSigners == nil {
		b.TrustedSigners = &cftypes.TrustedSigners{Enabled: aws.Bool(false), Quantity: aws.Int32(0)}
	}
	if b.TrustedKeyGroups == nil {
		b.TrustedKeyGroups = &cftypes.TrustedKeyGroups{Enabled: aws.Bool(false), Quantity: aws.Int32(0)}
	}
	if b.GrpcConfig == nil {
		b.GrpcConfig = &cftypes.GrpcConfig{Enabled: aws.Bool(false)}
	}
	return b
}

// hasKirocrewBehaviors reports whether the redirect is in place.
func hasKirocrewBehaviors(cfg *cftypes.DistributionConfig) bool {
	if cfg == nil || cfg.CacheBehaviors == nil {
		return false
	}
	for _, b := range cfg.CacheBehaviors.Items {
		if isKirocrewPathPattern(aws.ToString(b.PathPattern)) {
			return true
		}
	}
	return false
}

// kirocrewURL is the browser-facing URL of the crew distribution.
func kirocrewURL(crewDomain string) string {
	return "https://" + strings.TrimSuffix(crewDomain, "/")
}
