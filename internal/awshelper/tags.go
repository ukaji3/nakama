// Package awshelper provides cross-service utilities used by the
// nakama service layer:
//
//   - Tag conversion between map[string]string and the per-service
//     tag types defined by the AWS SDK v2.
//   - Polling helpers used to wait for resources to reach a target
//     state (VPC available, NAT GW available, distribution Deployed,
//     etc.).
//
// The package is intentionally service-agnostic: it depends on the
// SDK type packages but never on any concrete *Client.
package awshelper

import (
	"sort"

	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	wafv2types "github.com/aws/aws-sdk-go-v2/service/wafv2/types"
)

// sortedKeys returns the map keys sorted lexicographically so that
// the produced tag slices are deterministic (important for tests).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EC2Tags converts a map into []ec2types.Tag (deterministic order).
func EC2Tags(m map[string]string) []ec2types.Tag {
	keys := sortedKeys(m)
	out := make([]ec2types.Tag, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		out = append(out, ec2types.Tag{
			Key:   strPtr(k),
			Value: strPtr(v),
		})
	}
	return out
}

// EC2TagSpec returns a single ec2types.TagSpecification configured
// for the given resource type.
func EC2TagSpec(resourceType ec2types.ResourceType, m map[string]string) ec2types.TagSpecification {
	return ec2types.TagSpecification{
		ResourceType: resourceType,
		Tags:         EC2Tags(m),
	}
}

// EC2MapTags converts the AWS SDK ec2types.Tag slice back to a map
// for easy lookup. Tags with nil keys are skipped.
func EC2MapTags(tags []ec2types.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.Key == nil {
			continue
		}
		v := ""
		if t.Value != nil {
			v = *t.Value
		}
		out[*t.Key] = v
	}
	return out
}

// WAFv2Tags converts a map into []wafv2types.Tag.
func WAFv2Tags(m map[string]string) []wafv2types.Tag {
	keys := sortedKeys(m)
	out := make([]wafv2types.Tag, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		out = append(out, wafv2types.Tag{
			Key:   strPtr(k),
			Value: strPtr(v),
		})
	}
	return out
}

// IAMTags converts a map into []iamtypes.Tag.
func IAMTags(m map[string]string) []iamtypes.Tag {
	keys := sortedKeys(m)
	out := make([]iamtypes.Tag, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		out = append(out, iamtypes.Tag{
			Key:   strPtr(k),
			Value: strPtr(v),
		})
	}
	return out
}

// SSMTags converts a map into []ssmtypes.Tag.
func SSMTags(m map[string]string) []ssmtypes.Tag {
	keys := sortedKeys(m)
	out := make([]ssmtypes.Tag, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		out = append(out, ssmtypes.Tag{
			Key:   strPtr(k),
			Value: strPtr(v),
		})
	}
	return out
}

// CloudFrontTags converts a map into a CloudFront *cftypes.Tags
// (which is a wrapper over a slice).
func CloudFrontTags(m map[string]string) *cftypes.Tags {
	keys := sortedKeys(m)
	items := make([]cftypes.Tag, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		items = append(items, cftypes.Tag{
			Key:   strPtr(k),
			Value: strPtr(v),
		})
	}
	return &cftypes.Tags{Items: items}
}

// strPtr is a tiny helper used internally to avoid pulling in
// aws.String in this package's hot-path (and to make tests easier).
func strPtr(s string) *string { return &s }
