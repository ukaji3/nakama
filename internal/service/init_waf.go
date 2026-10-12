package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"
	wafv2types "github.com/aws/aws-sdk-go-v2/service/wafv2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// scopeCloudFront is the WAFv2 scope value required for resources
// associated with CloudFront distributions.
const scopeCloudFront = wafv2types.ScopeCloudfront

// initWAF creates (or re-uses) a single shared IP set and a single
// shared web ACL, both in the CLOUDFRONT scope (us-east-1).
//
// The web ACL's default action is BLOCK; one rule allows traffic
// from any address in the IP set.
func (s *Service) initWAF(ctx context.Context, res *InitResult) error {
	ipSetID, ipSetARN, err := s.upsertIPSet(ctx)
	if err != nil {
		return err
	}
	res.WAFIPSetID = ipSetID
	res.WAFIPSetARN = ipSetARN

	aclID, aclARN, err := s.upsertWebACL(ctx, ipSetARN)
	if err != nil {
		return err
	}
	res.WAFWebACLID = aclID
	res.WAFWebACLARN = aclARN
	return nil
}

func (s *Service) upsertIPSet(ctx context.Context) (string, string, error) {
	name := naming.WAFIPSet()

	ipSets, err := s.listAllIPSets(ctx)
	if err != nil {
		return "", "", err
	}
	for _, ip := range ipSets {
		if ip.Name != nil && *ip.Name == name && ip.Id != nil && ip.ARN != nil {
			slog.Info("waf ip set: reusing existing", "id", *ip.Id, "arn", *ip.ARN)
			return *ip.Id, *ip.ARN, nil
		}
	}

	tags := tagging.ForShared(tagging.ComponentWAF, name)
	c, err := s.clients.WAF.CreateIPSet(ctx, &wafv2.CreateIPSetInput{
		Name:             aws.String(name),
		Scope:            scopeCloudFront,
		IPAddressVersion: wafv2types.IPAddressVersionIpv4,
		Addresses:        []string{}, // start empty
		Description:      aws.String("nakama-managed IP allowlist"),
		Tags:             awshelper.WAFv2Tags(tags),
	})
	if err != nil {
		return "", "", fmt.Errorf("CreateIPSet: %w", err)
	}
	if c.Summary == nil || c.Summary.Id == nil || c.Summary.ARN == nil {
		return "", "", errors.New("CreateIPSet returned nil summary")
	}
	slog.Info("waf ip set: created", "id", *c.Summary.Id, "arn", *c.Summary.ARN)
	return *c.Summary.Id, *c.Summary.ARN, nil
}

func (s *Service) upsertWebACL(ctx context.Context, ipSetARN string) (string, string, error) {
	name := naming.WAFWebACL()

	acls, err := s.listAllWebACLs(ctx)
	if err != nil {
		return "", "", err
	}
	for _, a := range acls {
		if a.Name != nil && *a.Name == name && a.Id != nil && a.ARN != nil {
			slog.Info("waf web acl: reusing existing", "id", *a.Id, "arn", *a.ARN)
			return *a.Id, *a.ARN, nil
		}
	}

	tags := tagging.ForShared(tagging.ComponentWAF, name)
	in := &wafv2.CreateWebACLInput{
		Name:        aws.String(name),
		Scope:       scopeCloudFront,
		Description: aws.String("nakama-managed CloudFront web ACL"),
		DefaultAction: &wafv2types.DefaultAction{
			Block: &wafv2types.BlockAction{},
		},
		VisibilityConfig: &wafv2types.VisibilityConfig{
			SampledRequestsEnabled:   true,
			CloudWatchMetricsEnabled: true,
			MetricName:               aws.String("nakamaWaf"),
		},
		Rules: []wafv2types.Rule{
			{
				Name:     aws.String("nakama-allow-from-ipset"),
				Priority: 0,
				Action: &wafv2types.RuleAction{
					Allow: &wafv2types.AllowAction{},
				},
				Statement: &wafv2types.Statement{
					IPSetReferenceStatement: &wafv2types.IPSetReferenceStatement{
						ARN: aws.String(ipSetARN),
					},
				},
				VisibilityConfig: &wafv2types.VisibilityConfig{
					SampledRequestsEnabled:   true,
					CloudWatchMetricsEnabled: true,
					MetricName:               aws.String("nakamaAllowFromIpSet"),
				},
			},
		},
		Tags: awshelper.WAFv2Tags(tags),
	}
	c, err := s.createWebACLAwaitingIPSet(ctx, in)
	if err != nil {
		return "", "", err
	}
	if c.Summary == nil || c.Summary.Id == nil || c.Summary.ARN == nil {
		return "", "", errors.New("CreateWebACL returned nil summary")
	}
	slog.Info("waf web acl: created", "id", *c.Summary.Id, "arn", *c.Summary.ARN)
	return *c.Summary.Id, *c.Summary.ARN, nil
}

// wafPropagationErrorCodes are the CreateWebACL errors AWS documents as
// "if you've just created a resource that you're using in this
// operation, you might just need to wait a few minutes" (CreateWebACL,
// Errors). The web ACL references the IP set, which the same init run
// may have created a second earlier (Issue #81).
var wafPropagationErrorCodes = []string{"WAFUnavailableEntityException", "WAFNonexistentItemException"}

// Retry pacing for wafPropagationErrorCodes. AWS gives "from a few
// seconds to a number of minutes"; variables so tests can shorten them.
var (
	wafPropagationInterval = 5 * time.Second
	wafPropagationMaxWait  = 3 * time.Minute
)

// createWebACLAwaitingIPSet calls CreateWebACL and, while WAF reports
// that the referenced IP set is not visible yet, retries every
// wafPropagationInterval until wafPropagationMaxWait has passed. Any
// other error is returned at once. A failed CreateWebACL creates
// nothing, so retrying cannot produce a duplicate.
func (s *Service) createWebACLAwaitingIPSet(ctx context.Context, in *wafv2.CreateWebACLInput) (*wafv2.CreateWebACLOutput, error) {
	start := time.Now()
	for attempt := 1; ; attempt++ {
		out, err := s.clients.WAF.CreateWebACL(ctx, in)
		if err == nil {
			return out, nil
		}
		if !awshelper.IsAPIError(err, wafPropagationErrorCodes...) {
			return nil, fmt.Errorf("CreateWebACL: %w", err)
		}
		waited := time.Since(start)
		if waited+wafPropagationInterval > wafPropagationMaxWait {
			return nil, fmt.Errorf("CreateWebACL: %w (the IP set was still not visible to WAF after %s and %d attempts; "+
				"run `nakama aws init` again, it reuses everything already created)", err, waited.Round(time.Second), attempt)
		}
		slog.Info("waf web acl: waiting for the IP set to propagate",
			"attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wafPropagationInterval):
		}
	}
}

// listAllIPSets pages through ListIPSets. WAFv2 caps each page at
// Limit and signals continuation via NextMarker; a single fixed-limit
// call silently misses resources in accounts with more than 100 IP
// sets in the CloudFront scope.
func (s *Service) listAllIPSets(ctx context.Context) ([]wafv2types.IPSetSummary, error) {
	var all []wafv2types.IPSetSummary
	var marker *string
	for {
		out, err := s.clients.WAF.ListIPSets(ctx, &wafv2.ListIPSetsInput{
			Scope:      scopeCloudFront,
			Limit:      aws.Int32(100),
			NextMarker: marker,
		})
		if err != nil {
			return nil, fmt.Errorf("ListIPSets: %w", err)
		}
		all = append(all, out.IPSets...)
		// Guards against both terminal shapes (nil/empty marker) and a
		// server echoing the same marker back, which would loop forever.
		if out.NextMarker == nil || *out.NextMarker == "" || len(out.IPSets) == 0 ||
			(marker != nil && *marker == *out.NextMarker) {
			return all, nil
		}
		marker = out.NextMarker
	}
}

// listAllWebACLs pages through ListWebACLs (see listAllIPSets).
func (s *Service) listAllWebACLs(ctx context.Context) ([]wafv2types.WebACLSummary, error) {
	var all []wafv2types.WebACLSummary
	var marker *string
	for {
		out, err := s.clients.WAF.ListWebACLs(ctx, &wafv2.ListWebACLsInput{
			Scope:      scopeCloudFront,
			Limit:      aws.Int32(100),
			NextMarker: marker,
		})
		if err != nil {
			return nil, fmt.Errorf("ListWebACLs: %w", err)
		}
		all = append(all, out.WebACLs...)
		if out.NextMarker == nil || *out.NextMarker == "" || len(out.WebACLs) == 0 ||
			(marker != nil && *marker == *out.NextMarker) {
			return all, nil
		}
		marker = out.NextMarker
	}
}
