package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"
)

// WAFAPI wraps the subset of WAFv2 used by nakama for managing the
// shared `nakama-allowed-ips` IP set and the `nakama-waf` web ACL
// associated with every nakama CloudFront distribution.
type WAFAPI interface {
	// IP set
	CreateIPSet(ctx context.Context, in *wafv2.CreateIPSetInput, opts ...func(*wafv2.Options)) (*wafv2.CreateIPSetOutput, error)
	GetIPSet(ctx context.Context, in *wafv2.GetIPSetInput, opts ...func(*wafv2.Options)) (*wafv2.GetIPSetOutput, error)
	UpdateIPSet(ctx context.Context, in *wafv2.UpdateIPSetInput, opts ...func(*wafv2.Options)) (*wafv2.UpdateIPSetOutput, error)
	ListIPSets(ctx context.Context, in *wafv2.ListIPSetsInput, opts ...func(*wafv2.Options)) (*wafv2.ListIPSetsOutput, error)
	DeleteIPSet(ctx context.Context, in *wafv2.DeleteIPSetInput, opts ...func(*wafv2.Options)) (*wafv2.DeleteIPSetOutput, error)

	// Web ACL
	CreateWebACL(ctx context.Context, in *wafv2.CreateWebACLInput, opts ...func(*wafv2.Options)) (*wafv2.CreateWebACLOutput, error)
	GetWebACL(ctx context.Context, in *wafv2.GetWebACLInput, opts ...func(*wafv2.Options)) (*wafv2.GetWebACLOutput, error)
	ListWebACLs(ctx context.Context, in *wafv2.ListWebACLsInput, opts ...func(*wafv2.Options)) (*wafv2.ListWebACLsOutput, error)
	UpdateWebACL(ctx context.Context, in *wafv2.UpdateWebACLInput, opts ...func(*wafv2.Options)) (*wafv2.UpdateWebACLOutput, error)
}

func newWAFClient(cfg aws.Config) WAFAPI {
	// CLOUDFRONT-scope WAFv2 must use us-east-1 regardless of the
	// caller's "logical" region.
	cp := cfg.Copy()
	cp.Region = "us-east-1"
	return wafv2.NewFromConfig(cp)
}
