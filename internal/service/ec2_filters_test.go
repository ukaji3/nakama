package service

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// matchEC2Filters reports whether a resource with the given tags and
// attributes passes the filters, the way EC2 evaluates them: every
// filter must match, and any one of a filter's values is enough. A tag
// filter ("tag:<key>") needs the tag with one of the values. Any other
// filter name must be one of attrs (e.g. "vpc-id",
// "instance-state-name"); a name the fake does not know is an error, so
// a test notices when the code starts filtering on something the fake
// does not model.
//
// The fakes use it so that the tests check the filters the code sends:
// a fake that ignored them would let code that finds (and deletes)
// resources that are not nakama's pass.
func matchEC2Filters(filters []ec2types.Filter, tags []ec2types.Tag, attrs map[string]string) (bool, error) {
	for _, fl := range filters {
		name := aws.ToString(fl.Name)
		var (
			have string
			ok   bool
		)
		if key, isTag := strings.CutPrefix(name, "tag:"); isTag {
			for _, t := range tags {
				if aws.ToString(t.Key) == key {
					have, ok = aws.ToString(t.Value), true
				}
			}
		} else {
			if _, known := attrs[name]; !known {
				return false, fmt.Errorf("the fake does not model the filter %q", name)
			}
			have, ok = attrs[name], true
		}
		if !ok || !slices.Contains(fl.Values, have) {
			return false, nil
		}
	}
	return true, nil
}

// ec2Tags turns a tag map (internal/tagging) into EC2 tags, sorted by
// key so the fakes stay deterministic.
func ec2Tags(m map[string]string) []ec2types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]ec2types.Tag, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return tags
}
