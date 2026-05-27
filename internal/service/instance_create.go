package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
	"github.com/ukaji3/nakama/internal/userdata"
)

// CreateInstanceInput captures the user-supplied options for
// nakama aws create.
type CreateInstanceInput struct {
	Name              string
	InstanceType      string
	EBSGiB            int32  // defaults to config.EBSDefaultGiB when 0
	CodeServerVersion string // empty -> latest
}

// CreateInstanceOutput is what the CLI ultimately renders to the
// user. Password is the plain-text code-server password (also
// stored as SecureString in SSM).
type CreateInstanceOutput struct {
	Name               string `json:"name"`
	InstanceID         string `json:"instance_id"`
	ENIID              string `json:"eni_id"`
	SecurityGroupID    string `json:"security_group_id"`
	VPCOriginID        string `json:"vpc_origin_id"`
	DistributionID     string `json:"distribution_id"`
	DistributionDomain string `json:"distribution_domain"`
	URL                string `json:"url"`
	Password           string `json:"password"`
}

// CreateInstance provisions everything needed to expose a new
// nakama code-server instance through CloudFront. The flow is
// described in docs/design.md §8.2.
func (s *Service) CreateInstance(ctx context.Context, in CreateInstanceInput) (*CreateInstanceOutput, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.InstanceType) == "" {
		return nil, errors.New("instance-type is required")
	}
	if in.EBSGiB <= 0 {
		in.EBSGiB = config.EBSDefaultGiB
	}
	if err := s.requireInitialized(ctx); err != nil {
		return nil, err
	}
	if err := s.assertNameAvailable(ctx, in.Name); err != nil {
		return nil, err
	}

	slog.Info("create: starting", "name", in.Name, "instance_type", in.InstanceType, "ebs_gib", in.EBSGiB)

	amiID, err := s.clients.AMIResolver.LatestUbuntu2604AMD64(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve ubuntu 26.04 ami: %w", err)
	}
	slog.Info("create: ami resolved", "ami_id", amiID)

	password, err := generatePassword()
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	if err := s.putPasswordSecure(ctx, in.Name, password); err != nil {
		return nil, fmt.Errorf("store password: %w", err)
	}

	base, err := s.discoverBaseInfra(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover base infrastructure: %w", err)
	}

	prefixListID, err := s.cloudFrontPrefixListID(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve CloudFront prefix list: %w", err)
	}

	sgID, err := s.upsertInstanceSecurityGroup(ctx, in.Name, base.VPCID, prefixListID)
	if err != nil {
		return nil, fmt.Errorf("security group: %w", err)
	}

	eniID, err := s.upsertInstanceENI(ctx, in.Name, base.PrivateSubnet1aID, sgID)
	if err != nil {
		return nil, fmt.Errorf("eni: %w", err)
	}

	ud, err := userdata.RenderCodeServer(userdata.CodeServerParams{
		InstanceName:         in.Name,
		Region:               config.Region,
		Port:                 config.CodeServerPort,
		SSMPasswordParameter: ssmPasswordPath(in.Name),
		SSMVersionParameter:  ssmVersionPath(in.Name),
		PinnedVersion:        in.CodeServerVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("render user data: %w", err)
	}

	instanceID, err := s.runInstance(ctx, runInstanceArgs{
		Name:           in.Name,
		AMIID:          amiID,
		InstanceType:   in.InstanceType,
		EBSGiB:         in.EBSGiB,
		ENIID:          eniID,
		UserDataBase64: base64.StdEncoding.EncodeToString([]byte(ud)),
	})
	if err != nil {
		return nil, fmt.Errorf("run instance: %w", err)
	}

	if err := s.waitInstanceRunning(ctx, instanceID); err != nil {
		return nil, fmt.Errorf("wait instance running: %w", err)
	}

	privateDNS, err := s.privateDNSNameForInstance(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("resolve private DNS: %w", err)
	}
	slog.Info("create: private DNS resolved", "private_dns", privateDNS)

	vpcOriginID, err := s.createInstanceVPCOrigin(ctx, in.Name, instanceID)
	if err != nil {
		return nil, fmt.Errorf("create vpc origin: %w", err)
	}

	distID, distDomain, err := s.createInstanceDistribution(ctx, in.Name, vpcOriginID, privateDNS, base.WAFWebACLARN)
	if err != nil {
		return nil, fmt.Errorf("create distribution: %w", err)
	}

	// Persist instance metadata BEFORE waiting for the distribution
	// to be Deployed. This way, if the Deployed wait times out,
	// `nakama aws delete <name>` can still clean up everything.
	if err := s.writeInstanceMeta(ctx, in.Name, map[string]string{
		ssmInstanceIDPath(in.Name):         instanceID,
		ssmENIIDPath(in.Name):              eniID,
		ssmSGIDPath(in.Name):               sgID,
		ssmVPCOriginIDPath(in.Name):        vpcOriginID,
		ssmDistributionIDPath(in.Name):     distID,
		ssmDistributionDomainPath(in.Name): distDomain,
	}); err != nil {
		return nil, fmt.Errorf("write instance metadata: %w", err)
	}

	if err := s.waitDistributionDeployed(ctx, distID); err != nil {
		return nil, fmt.Errorf("wait distribution: %w", err)
	}

	out := &CreateInstanceOutput{
		Name:               in.Name,
		InstanceID:         instanceID,
		ENIID:              eniID,
		SecurityGroupID:    sgID,
		VPCOriginID:        vpcOriginID,
		DistributionID:     distID,
		DistributionDomain: distDomain,
		URL:                "https://" + distDomain,
		Password:           password,
	}
	slog.Info("create: completed",
		"name", in.Name,
		"instance_id", instanceID,
		"distribution_id", distID,
		"url", out.URL,
	)
	return out, nil
}

func (s *Service) putPasswordSecure(ctx context.Context, name, password string) error {
	_, err := s.clients.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:        aws.String(ssmPasswordPath(name)),
		Value:       aws.String(password),
		Type:        ssmtypes.ParameterTypeSecureString,
		Overwrite:   aws.Bool(true),
		Description: aws.String("nakama-managed code-server password"),
	})
	return err
}

func (s *Service) writeInstanceMeta(ctx context.Context, _ string, kv map[string]string) error {
	for k, v := range kv {
		_, err := s.clients.SSM.PutParameter(ctx, &ssm.PutParameterInput{
			Name:      aws.String(k),
			Value:     aws.String(v),
			Type:      ssmtypes.ParameterTypeString,
			Overwrite: aws.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("PutParameter(%s): %w", k, err)
		}
	}
	return nil
}

// assertNameAvailable returns an error if a nakama-managed EC2
// instance with the given Name tag already exists.
func (s *Service) assertNameAvailable(ctx context.Context, name string) error {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagging.KeyManaged), Values: []string{tagging.ValueManagedTrue}},
			{Name: aws.String("tag:" + tagging.KeyName), Values: []string{name}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	if err != nil {
		return fmt.Errorf("DescribeInstances: %w", err)
	}
	for _, r := range out.Reservations {
		if len(r.Instances) > 0 {
			return fmt.Errorf("nakama instance %q already exists", name)
		}
	}
	return nil
}

func (s *Service) waitInstanceRunning(ctx context.Context, instanceID string) error {
	return awshelper.Poll(ctx, awshelper.PollOptions{
		Interval:    5 * time.Second,
		Timeout:     5 * time.Minute,
		Description: fmt.Sprintf("instance %s running", instanceID),
	}, func(ctx context.Context) (bool, error) {
		out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			return false, err
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				if i.State == nil || i.State.Name == "" {
					continue
				}
				switch i.State.Name {
				case ec2types.InstanceStateNameRunning:
					return true, nil
				case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown:
					return false, fmt.Errorf("instance %s entered terminal state %s", instanceID, i.State.Name)
				}
				slog.Info("instance: still pending", "instance_id", instanceID, "state", i.State.Name)
			}
		}
		return false, nil
	})
}

// baseInfra holds the IDs of base resources discovered via tag
// search before instance-level provisioning.
type baseInfra struct {
	VPCID             string
	PrivateSubnet1aID string
	WAFWebACLARN      string
}

func (s *Service) discoverBaseInfra(ctx context.Context) (baseInfra, error) {
	var b baseInfra

	vpcID, err := s.findVPCByTags(ctx)
	if err != nil {
		return b, err
	}
	if vpcID == "" {
		return b, errors.New("nakama VPC not found; run \"nakama aws init\"")
	}
	b.VPCID = vpcID

	subOut, err := s.clients.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: append(nakamaTagFilters(map[string]string{
			tagging.NameTag: naming.PrivateSubnet(azSuffix(config.AvailabilityZones[0])),
		}), ec2types.Filter{Name: aws.String("vpc-id"), Values: []string{vpcID}}),
	})
	if err != nil {
		return b, fmt.Errorf("DescribeSubnets: %w", err)
	}
	if len(subOut.Subnets) == 0 || subOut.Subnets[0].SubnetId == nil {
		return b, errors.New("private subnet 1a not found; run \"nakama aws init\"")
	}
	b.PrivateSubnet1aID = *subOut.Subnets[0].SubnetId

	// WAF web ACL ARN; constructed via ListWebACLs because the
	// WAFv2 client is in us-east-1 and not visible to EC2/SSM tag APIs.
	aclARN, err := s.findWAFWebACLARN(ctx)
	if err != nil {
		return b, err
	}
	b.WAFWebACLARN = aclARN
	return b, nil
}
