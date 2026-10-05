package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
)

// CloudFrontAPI is the subset of the CloudFront SDK nakama uses.
//
// Both VPC origins (CreateVpcOrigin et al.) and standard distributions
// are managed through this interface.
type CloudFrontAPI interface {
	// VPC origins
	CreateVpcOrigin(ctx context.Context, in *cloudfront.CreateVpcOriginInput, opts ...func(*cloudfront.Options)) (*cloudfront.CreateVpcOriginOutput, error)
	GetVpcOrigin(ctx context.Context, in *cloudfront.GetVpcOriginInput, opts ...func(*cloudfront.Options)) (*cloudfront.GetVpcOriginOutput, error)
	UpdateVpcOrigin(ctx context.Context, in *cloudfront.UpdateVpcOriginInput, opts ...func(*cloudfront.Options)) (*cloudfront.UpdateVpcOriginOutput, error)
	DeleteVpcOrigin(ctx context.Context, in *cloudfront.DeleteVpcOriginInput, opts ...func(*cloudfront.Options)) (*cloudfront.DeleteVpcOriginOutput, error)
	ListVpcOrigins(ctx context.Context, in *cloudfront.ListVpcOriginsInput, opts ...func(*cloudfront.Options)) (*cloudfront.ListVpcOriginsOutput, error)

	// Distributions
	CreateDistribution(ctx context.Context, in *cloudfront.CreateDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error)
	GetDistribution(ctx context.Context, in *cloudfront.GetDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error)
	GetDistributionConfig(ctx context.Context, in *cloudfront.GetDistributionConfigInput, opts ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error)
	UpdateDistribution(ctx context.Context, in *cloudfront.UpdateDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error)
	DeleteDistribution(ctx context.Context, in *cloudfront.DeleteDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error)
	ListDistributions(ctx context.Context, in *cloudfront.ListDistributionsInput, opts ...func(*cloudfront.Options)) (*cloudfront.ListDistributionsOutput, error)

	// CloudFront Functions (the Kiro Crew redirect, ADR 0020)
	CreateFunction(ctx context.Context, in *cloudfront.CreateFunctionInput, opts ...func(*cloudfront.Options)) (*cloudfront.CreateFunctionOutput, error)
	DescribeFunction(ctx context.Context, in *cloudfront.DescribeFunctionInput, opts ...func(*cloudfront.Options)) (*cloudfront.DescribeFunctionOutput, error)
	UpdateFunction(ctx context.Context, in *cloudfront.UpdateFunctionInput, opts ...func(*cloudfront.Options)) (*cloudfront.UpdateFunctionOutput, error)
	PublishFunction(ctx context.Context, in *cloudfront.PublishFunctionInput, opts ...func(*cloudfront.Options)) (*cloudfront.PublishFunctionOutput, error)
	DeleteFunction(ctx context.Context, in *cloudfront.DeleteFunctionInput, opts ...func(*cloudfront.Options)) (*cloudfront.DeleteFunctionOutput, error)

	// Tagging
	TagResource(ctx context.Context, in *cloudfront.TagResourceInput, opts ...func(*cloudfront.Options)) (*cloudfront.TagResourceOutput, error)
	ListTagsForResource(ctx context.Context, in *cloudfront.ListTagsForResourceInput, opts ...func(*cloudfront.Options)) (*cloudfront.ListTagsForResourceOutput, error)
}

func newCloudFrontClient(cfg aws.Config) CloudFrontAPI {
	// CloudFront is a global service; the regional config is irrelevant
	// for routing but the SDK requires a region.
	return cloudfront.NewFromConfig(cfg)
}
