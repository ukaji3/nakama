package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// cloudFrontPrefixListID returns the AWS-managed prefix list ID
// for "com.amazonaws.global.cloudfront.origin-facing" in the
// current region.
func (s *Service) cloudFrontPrefixListID(ctx context.Context) (string, error) {
	out, err := s.clients.EC2.DescribeManagedPrefixLists(ctx, &ec2.DescribeManagedPrefixListsInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("prefix-list-name"),
			Values: []string{config.CloudFrontVPCOriginsPrefixListName},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeManagedPrefixLists: %w", err)
	}
	for _, pl := range out.PrefixLists {
		if pl.PrefixListId != nil {
			return *pl.PrefixListId, nil
		}
	}
	return "", fmt.Errorf("CloudFront managed prefix list %q not found",
		config.CloudFrontVPCOriginsPrefixListName)
}

// upsertInstanceSecurityGroup ensures a nakama-tagged SG exists for
// this instance and that it has a single ingress rule allowing TCP
// from the CloudFront managed prefix list to the code-server port.
func (s *Service) upsertInstanceSecurityGroup(ctx context.Context, instanceName, vpcID, prefixListID string) (string, error) {
	sgName := naming.SecurityGroup(instanceName)

	out, err := s.clients.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: append(nakamaTagFilters(map[string]string{
			tagging.NameTag: sgName,
			tagging.KeyName: instanceName,
		}), ec2types.Filter{Name: aws.String("vpc-id"), Values: []string{vpcID}}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeSecurityGroups: %w", err)
	}

	var sgID string
	if len(out.SecurityGroups) > 0 && out.SecurityGroups[0].GroupId != nil {
		sgID = *out.SecurityGroups[0].GroupId
		slog.Info("sg: reusing existing", "sg_id", sgID)
	} else {
		tags := tagging.ForInstance(instanceName, tagging.ComponentSG, sgName)
		c, err := s.clients.EC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName:   aws.String(sgName),
			Description: aws.String("nakama EC2 SG: ingress from CloudFront managed prefix list"),
			VpcId:       aws.String(vpcID),
			TagSpecifications: []ec2types.TagSpecification{
				awshelper.EC2TagSpec(ec2types.ResourceTypeSecurityGroup, tags),
			},
		})
		if err != nil {
			return "", fmt.Errorf("CreateSecurityGroup: %w", err)
		}
		if c.GroupId == nil {
			return "", errors.New("CreateSecurityGroup returned nil GroupId")
		}
		sgID = *c.GroupId
		slog.Info("sg: created", "sg_id", sgID)
	}

	if err := s.ensureSGIngress(ctx, sgID, prefixListID); err != nil {
		return "", err
	}
	return sgID, nil
}

// ensureSGIngress installs the CloudFront prefix-list ingress rule
// if it is not already present. AWS rejects duplicate rules with
// InvalidPermission.Duplicate; we treat that as success.
func (s *Service) ensureSGIngress(ctx context.Context, sgID, prefixListID string) error {
	_, err := s.clients.EC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int32(int32(config.CodeServerPort)),
			ToPort:     aws.Int32(int32(config.CodeServerPort)),
			PrefixListIds: []ec2types.PrefixListId{{
				PrefixListId: aws.String(prefixListID),
				Description:  aws.String("CloudFront VPC Origins managed prefix list"),
			}},
		}},
	})
	if err != nil {
		if awshelper.IsAPIError(err, "InvalidPermission.Duplicate") {
			return nil
		}
		return fmt.Errorf("AuthorizeSecurityGroupIngress: %w", err)
	}
	slog.Info("sg: ingress added", "sg_id", sgID, "prefix_list_id", prefixListID, "port", config.CodeServerPort)
	return nil
}

// upsertInstanceENI creates (or re-uses) the per-instance ENI in the
// private subnet. The ENI is the long-lived holder of the EC2's
// private IP and is unaffected by stop/start cycles.
func (s *Service) upsertInstanceENI(ctx context.Context, instanceName, subnetID, sgID string) (string, error) {
	name := naming.ENI(instanceName)

	out, err := s.clients.EC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: append(nakamaTagFilters(map[string]string{
			tagging.NameTag: name,
			tagging.KeyName: instanceName,
		}), ec2types.Filter{Name: aws.String("subnet-id"), Values: []string{subnetID}}),
	})
	if err != nil {
		return "", fmt.Errorf("DescribeNetworkInterfaces: %w", err)
	}
	if len(out.NetworkInterfaces) > 0 && out.NetworkInterfaces[0].NetworkInterfaceId != nil {
		id := *out.NetworkInterfaces[0].NetworkInterfaceId
		slog.Info("eni: reusing existing", "eni_id", id)
		return id, nil
	}

	tags := tagging.ForInstance(instanceName, tagging.ComponentENI, name)
	c, err := s.clients.EC2.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String("nakama-managed ENI"),
		Groups:      []string{sgID},
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeNetworkInterface, tags),
		},
	})
	if err != nil {
		return "", fmt.Errorf("CreateNetworkInterface: %w", err)
	}
	if c.NetworkInterface == nil || c.NetworkInterface.NetworkInterfaceId == nil {
		return "", errors.New("CreateNetworkInterface returned nil")
	}
	id := *c.NetworkInterface.NetworkInterfaceId
	slog.Info("eni: created", "eni_id", id)
	return id, nil
}

type runInstanceArgs struct {
	Name           string
	AMIID          string
	InstanceType   string
	EBSGiB         int32
	ENIID          string
	UserDataBase64 string
}

// runInstance launches the EC2 instance with the prepared ENI and
// User Data. The IAM instance profile is set by name (created in
// nakama aws init).
func (s *Service) runInstance(ctx context.Context, in runInstanceArgs) (string, error) {
	tags := tagging.ForInstance(in.Name, tagging.ComponentEC2, naming.EC2(in.Name))
	volTags := tagging.ForInstance(in.Name, tagging.ComponentEBS, naming.EC2(in.Name)+"-root")

	out, err := s.clients.EC2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String(in.AMIID),
		InstanceType: ec2types.InstanceType(in.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:        aws.Int32(0),
			NetworkInterfaceId: aws.String(in.ENIID),
		}},
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs: &ec2types.EbsBlockDevice{
				VolumeSize:          aws.Int32(in.EBSGiB),
				VolumeType:          ec2types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true),
			},
		}},
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(naming.InstanceProfile()),
		},
		UserData: aws.String(in.UserDataBase64),
		TagSpecifications: []ec2types.TagSpecification{
			awshelper.EC2TagSpec(ec2types.ResourceTypeInstance, tags),
			awshelper.EC2TagSpec(ec2types.ResourceTypeVolume, volTags),
		},
	})
	if err != nil {
		return "", fmt.Errorf("RunInstances: %w", err)
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
		return "", errors.New("RunInstances returned no instance")
	}
	id := *out.Instances[0].InstanceId
	slog.Info("instance: launched", "instance_id", id)
	return id, nil
}
