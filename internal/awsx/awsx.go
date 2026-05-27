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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"

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
}

// NewClients loads the default AWS configuration and constructs the
// concrete client wrappers. The region is forced to nakamaConfig.Region.
func NewClients(ctx context.Context) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(nakamaConfig.Region))
	if err != nil {
		return nil, err
	}
	c := newClientsFromConfig(cfg)

	idOut, err := c.STS.GetCallerIdentity(ctx, nil)
	if err != nil {
		return nil, err
	}
	if idOut != nil && idOut.Account != nil {
		c.AccountID = *idOut.Account
	}
	return c, nil
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
	}
}
