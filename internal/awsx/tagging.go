package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

// TaggingAPI wraps the Resource Groups Tagging API which nakama uses
// to discover its own resources by the tag schema defined in
// internal/tagging.
type TaggingAPI interface {
	GetResources(ctx context.Context, in *resourcegroupstaggingapi.GetResourcesInput, opts ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

func newTaggingClient(cfg aws.Config) TaggingAPI {
	return resourcegroupstaggingapi.NewFromConfig(cfg)
}
