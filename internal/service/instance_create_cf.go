package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// AWS-managed CloudFront policy IDs.
const (
	managedCachePolicyCachingDisabled   = "4135ea2d-6df8-44a3-9df3-4b5a84be39ad"
	managedOriginRequestPolicyAllViewer = "216adef6-5c7f-47e4-b989-5492eafa07d3"
)

// findWAFWebACLARN returns the ARN of the shared nakama-waf web ACL.
// The CLOUDFRONT scope means the WAFv2 client is in us-east-1, which
// is hidden inside awsx.WAFAPI.
func (s *Service) findWAFWebACLARN(ctx context.Context) (string, error) {
	acls, err := s.listAllWebACLs(ctx)
	if err != nil {
		return "", err
	}
	want := naming.WAFWebACL()
	for _, a := range acls {
		if a.Name != nil && *a.Name == want && a.ARN != nil {
			return *a.ARN, nil
		}
	}
	return "", fmt.Errorf("WAFv2 web ACL %q not found; run \"nakama aws init\" first", want)
}

// privateDNSNameForInstance returns the EC2 instance's private DNS
// hostname, retrying briefly because the field can lag the running
// state by a couple of seconds.
func (s *Service) privateDNSNameForInstance(ctx context.Context, instanceID string) (string, error) {
	var dns string
	err := awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    3 * time.Second,
		Timeout:     30 * time.Second,
		Description: fmt.Sprintf("private DNS name for %s", instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			return false, err
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				if i.PrivateDnsName != nil && *i.PrivateDnsName != "" {
					dns = *i.PrivateDnsName
					return true, nil
				}
			}
		}
		return false, nil
	})
	return dns, err
}

func (s *Service) createInstanceVPCOrigin(ctx context.Context, instanceName, instanceID string) (string, error) {
	out, err := s.clients.CloudFront.CreateVpcOrigin(ctx, &cloudfront.CreateVpcOriginInput{
		VpcOriginEndpointConfig: &cftypes.VpcOriginEndpointConfig{
			Name:                 aws.String(naming.EC2(instanceName) + "-origin"),
			Arn:                  aws.String(s.ec2InstanceARN(instanceID)),
			HTTPPort:             aws.Int32(int32(config.CodeServerPort)),
			HTTPSPort:            aws.Int32(443),
			OriginProtocolPolicy: cftypes.OriginProtocolPolicyHttpOnly,
			OriginSslProtocols: &cftypes.OriginSslProtocols{
				Quantity: aws.Int32(1),
				Items:    []cftypes.SslProtocol{cftypes.SslProtocolTLSv12},
			},
		},
		Tags: awshelper.CloudFrontTags(tagging.ForInstance(instanceName, tagging.ComponentVPCOrigin, naming.EC2(instanceName)+"-origin")),
	})
	if err != nil {
		return "", fmt.Errorf("CreateVpcOrigin: %w", err)
	}
	if out.VpcOrigin == nil || out.VpcOrigin.Id == nil {
		return "", errors.New("CreateVpcOrigin returned nil")
	}
	id := *out.VpcOrigin.Id
	slog.Info("vpc origin: created, waiting for Deployed", "id", id)

	if err := s.waitVPCOriginDeployed(ctx, id); err != nil {
		return "", fmt.Errorf("wait vpc origin: %w", err)
	}
	return id, nil
}

func (s *Service) waitVPCOriginDeployed(ctx context.Context, id string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    20 * time.Second,
		Timeout:     20 * time.Minute,
		Description: fmt.Sprintf("vpc origin %s Deployed", id),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.CloudFront.GetVpcOrigin(ctx, &cloudfront.GetVpcOriginInput{
			Id: aws.String(id),
		})
		if err != nil {
			return false, err
		}
		if out.VpcOrigin == nil || out.VpcOrigin.Status == nil {
			return false, errors.New("GetVpcOrigin returned nil")
		}
		st := *out.VpcOrigin.Status
		if st == "Deployed" {
			return true, nil
		}
		slog.Info("vpc origin: still deploying", "id", id, "status", st)
		return false, nil
	})
}

// createInstanceDistribution creates the CloudFront distribution
// pointing at the prepared VPC origin. The Origin DomainName is set
// to the EC2 instance's Private DNS hostname as required by
// CloudFront VPC Origins:
//
//	"If your VPC origin is an EC2 instance, copy and paste the
//	 Private IP DNS name of the instance into the Origin domain field."
//	-- https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html
//
// This function does NOT wait for the distribution to reach the
// Deployed state. The caller is expected to invoke
// waitDistributionDeployed after persisting metadata, so that a
// subsequent timeout cannot leave the Distribution unmanageable
// from nakama.
func (s *Service) createInstanceDistribution(ctx context.Context, instanceName, vpcOriginID, originDomain, wafWebACLARN string) (string, string, error) {
	caller, err := callerReference()
	if err != nil {
		return "", "", err
	}
	cfg := distributionConfig(distributionParams{
		callerReference: caller,
		comment:         "nakama-" + instanceName,
		vpcOriginID:     vpcOriginID,
		originDomain:    originDomain,
		wafWebACLARN:    wafWebACLARN,
	})

	out, err := s.clients.CloudFront.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: cfg,
	})
	if err != nil {
		return "", "", fmt.Errorf("CreateDistribution: %w", err)
	}
	if out.Distribution == nil || out.Distribution.Id == nil || out.Distribution.DomainName == nil {
		return "", "", errors.New("CreateDistribution returned nil")
	}
	id := *out.Distribution.Id
	domain := *out.Distribution.DomainName
	slog.Info("distribution: created, waiting for Deployed", "id", id, "domain", domain)

	if _, err := s.clients.CloudFront.TagResource(ctx, &cloudfront.TagResourceInput{
		Resource: out.Distribution.ARN,
		Tags:     awshelper.CloudFrontTags(tagging.ForInstance(instanceName, tagging.ComponentCloudFront, "nakama-"+instanceName)),
	}); err != nil {
		// non-fatal; tags can be re-applied later
		slog.Warn("distribution: TagResource failed", "id", id, "error", err)
	}

	return id, domain, nil
}

// distributionParams parameterises distributionConfig. Every nakama
// distribution shares one shape: a single VPC origin (code-server:8080
// on the instance), caching off, every viewer header forwarded (needed
// for WebSockets) and the shared WAF web ACL.
type distributionParams struct {
	callerReference string
	comment         string
	vpcOriginID     string
	originDomain    string
	wafWebACLARN    string
	// originHeaders are added to every origin request. The code-server
	// distribution has none; the Kiro Crew one injects X-Forwarded-Host.
	originHeaders map[string]string
}

func (p distributionParams) originID() string { return "vpc-origin-" + p.vpcOriginID }

// quantity is the int32 element count CloudFront list types carry.
// The lists nakama builds are tiny; a count that does not fit int32
// cannot occur and is a programming error.
func quantity(n int) *int32 {
	if n < 0 || n > math.MaxInt32 {
		panic(fmt.Sprintf("cloudfront list length %d out of range", n))
	}
	return aws.Int32(int32(n)) // #nosec G115 -- bounds checked above
}

func (p distributionParams) customHeaders() *cftypes.CustomHeaders {
	if len(p.originHeaders) == 0 {
		return &cftypes.CustomHeaders{Quantity: aws.Int32(0)}
	}
	names := make([]string, 0, len(p.originHeaders))
	for k := range p.originHeaders {
		names = append(names, k)
	}
	sort.Strings(names)
	items := make([]cftypes.OriginCustomHeader, 0, len(names))
	for _, k := range names {
		items = append(items, cftypes.OriginCustomHeader{
			HeaderName:  aws.String(k),
			HeaderValue: aws.String(p.originHeaders[k]),
		})
	}
	return &cftypes.CustomHeaders{Quantity: quantity(len(items)), Items: items}
}

// distributionConfig renders the distribution config shared by the
// code-server and Kiro Crew distributions.
func distributionConfig(p distributionParams) *cftypes.DistributionConfig {
	return &cftypes.DistributionConfig{
		CallerReference: aws.String(p.callerReference),
		Comment:         aws.String(p.comment),
		Enabled:         aws.Bool(true),
		HttpVersion:     cftypes.HttpVersionHttp11,
		IsIPV6Enabled:   aws.Bool(false),
		PriceClass:      cftypes.PriceClassPriceClassAll,
		WebACLId:        aws.String(p.wafWebACLARN),
		Origins: &cftypes.Origins{
			Quantity: aws.Int32(1),
			Items: []cftypes.Origin{{
				Id:         aws.String(p.originID()),
				DomainName: aws.String(p.originDomain),
				VpcOriginConfig: &cftypes.VpcOriginConfig{
					VpcOriginId: aws.String(p.vpcOriginID),
				},
				CustomHeaders:      p.customHeaders(),
				ConnectionAttempts: aws.Int32(3),
				ConnectionTimeout:  aws.Int32(10),
				OriginShield:       &cftypes.OriginShield{Enabled: aws.Bool(false)},
			}},
		},
		DefaultCacheBehavior: &cftypes.DefaultCacheBehavior{
			TargetOriginId:       aws.String(p.originID()),
			ViewerProtocolPolicy: cftypes.ViewerProtocolPolicyRedirectToHttps,
			AllowedMethods: &cftypes.AllowedMethods{
				Quantity: aws.Int32(7),
				Items: []cftypes.Method{
					cftypes.MethodGet, cftypes.MethodHead, cftypes.MethodOptions,
					cftypes.MethodPut, cftypes.MethodPost, cftypes.MethodPatch, cftypes.MethodDelete,
				},
				CachedMethods: &cftypes.CachedMethods{
					Quantity: aws.Int32(2),
					Items:    []cftypes.Method{cftypes.MethodGet, cftypes.MethodHead},
				},
			},
			Compress:              aws.Bool(false),
			CachePolicyId:         aws.String(managedCachePolicyCachingDisabled),
			OriginRequestPolicyId: aws.String(managedOriginRequestPolicyAllViewer),
		},
		ViewerCertificate: &cftypes.ViewerCertificate{
			CloudFrontDefaultCertificate: aws.Bool(true),
			MinimumProtocolVersion:       cftypes.MinimumProtocolVersionTLSv122021,
		},
		Restrictions: &cftypes.Restrictions{
			GeoRestriction: &cftypes.GeoRestriction{
				RestrictionType: cftypes.GeoRestrictionTypeNone,
				Quantity:        aws.Int32(0),
			},
		},
	}
}

func (s *Service) waitDistributionDeployed(ctx context.Context, id string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    30 * time.Second,
		Timeout:     20 * time.Minute,
		Description: fmt.Sprintf("distribution %s Deployed", id),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.CloudFront.GetDistribution(ctx, &cloudfront.GetDistributionInput{
			Id: aws.String(id),
		})
		if err != nil {
			return false, err
		}
		if out.Distribution == nil || out.Distribution.Status == nil {
			return false, errors.New("GetDistribution returned nil")
		}
		st := *out.Distribution.Status
		if st == "Deployed" {
			return true, nil
		}
		slog.Info("distribution: still deploying", "id", id, "status", st)
		return false, nil
	})
}

// callerReference returns a hex string suitable for the CloudFront
// CallerReference field (must be unique per account).
func callerReference() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "nakama-" + hex.EncodeToString(b), nil
}

// ec2InstanceARN builds an EC2 instance ARN using the caller's
// account ID, captured by awsx.NewClients via STS GetCallerIdentity.
func (s *Service) ec2InstanceARN(instanceID string) string {
	account := s.clients.AccountID
	if account == "" {
		account = "000000000000"
	}
	return "arn:aws:ec2:" + config.Region + ":" + account + ":instance/" + instanceID
}
