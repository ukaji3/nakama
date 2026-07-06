package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// excludedAZID is the AWS-internal AZ ID nakama refuses to use
// because CloudFront VPC Origins is not supported there.
const excludedAZID = "apne1-az3"

// initAZSanityCheck verifies that none of the configured AZ names
// (config.AvailabilityZones) maps to the excluded AZ ID for this
// account. It does not modify any resources.
func (s *Service) initAZSanityCheck(ctx context.Context) error {
	out, err := s.clients.EC2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("zone-name"),
			Values: config.AvailabilityZones,
		}},
	})
	if err != nil {
		return err
	}

	got := make(map[string]string, len(out.AvailabilityZones)) // name -> id
	for _, az := range out.AvailabilityZones {
		if az.ZoneName != nil && az.ZoneId != nil {
			got[*az.ZoneName] = *az.ZoneId
		}
	}
	for _, name := range config.AvailabilityZones {
		zid, ok := got[name]
		if !ok {
			return fmt.Errorf("availability zone %q not found in region %s",
				name, config.Region)
		}
		if zid == excludedAZID {
			return fmt.Errorf("availability zone %q maps to %q which is "+
				"unsupported by CloudFront VPC Origins; pick a different AZ",
				name, excludedAZID)
		}
	}
	return nil
}

// initVPC creates or re-uses the nakama VPC and ensures its DNS
// attributes are enabled.
func (s *Service) initVPC(ctx context.Context, res *InitResult) error {
	id, err := s.findVPCByTags(ctx)
	if err != nil {
		return err
	}
	if id != "" {
		slog.Info("vpc: reusing existing", "vpc_id", id)
		res.VPCID = id
		return s.enableVPCDNS(ctx, id)
	}

	tags := tagging.ForShared(tagging.ComponentVPC, naming.VPC())
	out, err := s.clients.EC2.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String(config.VPCCIDR),
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeVpc, tags),
		},
	})
	if err != nil {
		return fmt.Errorf("CreateVpc: %w", err)
	}
	if out.Vpc == nil || out.Vpc.VpcId == nil {
		return errors.New("CreateVpc returned nil VPC")
	}
	res.VPCID = *out.Vpc.VpcId
	slog.Info("vpc: created", "vpc_id", res.VPCID, "cidr", config.VPCCIDR)
	return s.enableVPCDNS(ctx, res.VPCID)
}

func (s *Service) findVPCByTags(ctx context.Context) (string, error) {
	out, err := s.clients.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: nakamaTagFilters(map[string]string{
			tagging.NameTag: naming.VPC(),
		}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeVpcs: %w", err)
	}
	if len(out.Vpcs) == 0 {
		return "", nil
	}
	if out.Vpcs[0].VpcId == nil {
		return "", errors.New("DescribeVpcs returned nil VpcId")
	}
	return *out.Vpcs[0].VpcId, nil
}

// enableVPCDNS turns on EnableDnsSupport and EnableDnsHostnames so
// that EC2 instances receive resolvable internal DNS names (CloudFront
// VPC Origins requires the EC2 private DNS hostname).
func (s *Service) enableVPCDNS(ctx context.Context, vpcID string) error {
	mods := []*ec2.ModifyVpcAttributeInput{
		{VpcId: aws.String(vpcID), EnableDnsSupport: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}},
		{VpcId: aws.String(vpcID), EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}},
	}
	for _, m := range mods {
		if _, err := s.clients.EC2.ModifyVpcAttribute(ctx, m); err != nil {
			return fmt.Errorf("ModifyVpcAttribute: %w", err)
		}
	}
	return nil
}

// initInternetGateway creates the IGW (if missing) and ensures it is
// attached to the VPC.
func (s *Service) initInternetGateway(ctx context.Context, res *InitResult) error {
	out, err := s.clients.EC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: nakamaTagFilters(map[string]string{
			tagging.NameTag: naming.IGW(),
		}),
	})
	if err != nil {
		return fmt.Errorf("DescribeInternetGateways: %w", err)
	}

	var igwID string
	if len(out.InternetGateways) > 0 && out.InternetGateways[0].InternetGatewayId != nil {
		igwID = *out.InternetGateways[0].InternetGatewayId
		slog.Info("internet gateway: reusing existing", "igw_id", igwID)
	} else {
		tags := tagging.ForShared(tagging.ComponentIGW, naming.IGW())
		c, err := s.clients.EC2.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
			TagSpecifications: []ec2types.TagSpecification{
				awshelper.EC2TagSpec(ec2types.ResourceTypeInternetGateway, tags),
			},
		})
		if err != nil {
			return fmt.Errorf("CreateInternetGateway: %w", err)
		}
		if c.InternetGateway == nil || c.InternetGateway.InternetGatewayId == nil {
			return errors.New("CreateInternetGateway returned nil")
		}
		igwID = *c.InternetGateway.InternetGatewayId
		slog.Info("internet gateway: created", "igw_id", igwID)
	}
	res.InternetGatewayID = igwID

	if err := s.attachIGWIfNeeded(ctx, igwID, res.VPCID); err != nil {
		return err
	}
	return nil
}

// attachIGWIfNeeded attaches the IGW to the VPC unless it is
// already attached to it.
func (s *Service) attachIGWIfNeeded(ctx context.Context, igwID, vpcID string) error {
	out, err := s.clients.EC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []string{igwID},
	})
	if err != nil {
		return fmt.Errorf("DescribeInternetGateways: %w", err)
	}
	if len(out.InternetGateways) > 0 {
		for _, att := range out.InternetGateways[0].Attachments {
			if att.VpcId != nil && *att.VpcId == vpcID {
				return nil // already attached
			}
		}
	}

	if _, err := s.clients.EC2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}); err != nil {
		return fmt.Errorf("AttachInternetGateway: %w", err)
	}
	slog.Info("internet gateway: attached", "igw_id", igwID, "vpc_id", vpcID)
	return nil
}

// initSubnets creates one public subnet and N private subnets
// (one per configured AZ).
func (s *Service) initSubnets(ctx context.Context, res *InitResult) error {
	primaryAZ := config.AvailabilityZones[0] // public subnet lives in the first AZ
	publicID, err := s.upsertSubnet(ctx, subnetSpec{
		Name:      naming.PublicSubnet(azSuffix(primaryAZ)),
		Component: tagging.ComponentSubnetPublic,
		CIDR:      config.PublicSubnet1aCIDR,
		AZ:        primaryAZ,
		VPCID:     res.VPCID,
		MapPublic: true,
	})
	if err != nil {
		return err
	}
	res.PublicSubnetID = publicID

	for i, az := range config.AvailabilityZones {
		var cidr string
		switch i {
		case 0:
			cidr = config.PrivateSubnet1aCIDR
		case 1:
			cidr = config.PrivateSubnet1cCIDR
		default:
			return fmt.Errorf("unsupported AZ count %d (only 2 are wired up in v1)", len(config.AvailabilityZones))
		}
		id, err := s.upsertSubnet(ctx, subnetSpec{
			Name:      naming.PrivateSubnet(azSuffix(az)),
			Component: tagging.ComponentSubnetPrivate,
			CIDR:      cidr,
			AZ:        az,
			VPCID:     res.VPCID,
			MapPublic: false,
		})
		if err != nil {
			return err
		}
		res.PrivateSubnetIDs[az] = id
	}
	return nil
}

type subnetSpec struct {
	Name      string
	Component string
	CIDR      string
	AZ        string
	VPCID     string
	MapPublic bool
}

func (s *Service) upsertSubnet(ctx context.Context, spec subnetSpec) (string, error) {
	out, err := s.clients.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: append(nakamaTagFilters(map[string]string{
			tagging.NameTag: spec.Name,
		}), ec2types.Filter{
			Name:   aws.String("vpc-id"),
			Values: []string{spec.VPCID},
		}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeSubnets(%s): %w", spec.Name, err)
	}
	if len(out.Subnets) > 0 && out.Subnets[0].SubnetId != nil {
		id := *out.Subnets[0].SubnetId
		slog.Info("subnet: reusing existing", "name", spec.Name, "subnet_id", id)
		if err := s.ensureSubnetPublicIPv4(ctx, id, spec.MapPublic); err != nil {
			return "", err
		}
		return id, nil
	}

	tags := tagging.ForShared(spec.Component, spec.Name)
	c, err := s.clients.EC2.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:            aws.String(spec.VPCID),
		CidrBlock:        aws.String(spec.CIDR),
		AvailabilityZone: aws.String(spec.AZ),
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeSubnet, tags),
		},
	})
	if err != nil {
		return "", fmt.Errorf("CreateSubnet(%s): %w", spec.Name, err)
	}
	if c.Subnet == nil || c.Subnet.SubnetId == nil {
		return "", fmt.Errorf("CreateSubnet(%s) returned nil", spec.Name)
	}
	id := *c.Subnet.SubnetId
	if err := s.ensureSubnetPublicIPv4(ctx, id, spec.MapPublic); err != nil {
		return "", err
	}
	slog.Info("subnet: created", "name", spec.Name, "subnet_id", id, "az", spec.AZ, "cidr", spec.CIDR)
	return id, nil
}

func (s *Service) ensureSubnetPublicIPv4(ctx context.Context, subnetID string, enabled bool) error {
	if !enabled {
		return nil
	}
	if _, err := s.clients.EC2.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
		SubnetId: aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}); err != nil {
		return fmt.Errorf("ModifySubnetAttribute(%s, MapPublicIpOnLaunch=true): %w", subnetID, err)
	}
	slog.Info("subnet: public IPv4 auto-assign enabled", "subnet_id", subnetID)
	return nil
}

// nakamaTagFilters builds an EC2 filter set that selects resources
// tagged with `nakama:managed=true` and any extra tag pairs supplied.
func nakamaTagFilters(extra map[string]string) []ec2types.Filter {
	filters := []ec2types.Filter{{
		Name:   aws.String("tag:" + tagging.KeyManaged),
		Values: []string{tagging.ValueManagedTrue},
	}}
	for k, v := range extra {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("tag:" + k),
			Values: []string{v},
		})
	}
	return filters
}

// azSuffix returns the trailing AZ letter of an AZ name, e.g.
// "ap-northeast-1a" -> "1a".
func azSuffix(az string) string {
	const region = "ap-northeast-"
	if strings.HasPrefix(az, region) {
		return az[len(region):]
	}
	// fallback: take the last 2 characters
	if len(az) >= 2 {
		return az[len(az)-2:]
	}
	return az
}
