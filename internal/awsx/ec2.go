package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// EC2API is the subset of the EC2 SDK surface nakama relies on.
//
// The interface is defined locally rather than re-exporting
// ec2.Client so that tests can build mocks without depending on
// the whole SDK.
type EC2API interface {
	// Availability Zones (used to exclude apne1-az3)
	DescribeAvailabilityZones(ctx context.Context, in *ec2.DescribeAvailabilityZonesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error)

	// VPC / subnet / gateway operations
	DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	CreateVpc(ctx context.Context, in *ec2.CreateVpcInput, opts ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error)
	ModifyVpcAttribute(ctx context.Context, in *ec2.ModifyVpcAttributeInput, opts ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	CreateSubnet(ctx context.Context, in *ec2.CreateSubnetInput, opts ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error)
	DescribeInternetGateways(ctx context.Context, in *ec2.DescribeInternetGatewaysInput, opts ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error)
	CreateInternetGateway(ctx context.Context, in *ec2.CreateInternetGatewayInput, opts ...func(*ec2.Options)) (*ec2.CreateInternetGatewayOutput, error)
	AttachInternetGateway(ctx context.Context, in *ec2.AttachInternetGatewayInput, opts ...func(*ec2.Options)) (*ec2.AttachInternetGatewayOutput, error)
	DescribeNatGateways(ctx context.Context, in *ec2.DescribeNatGatewaysInput, opts ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error)
	CreateNatGateway(ctx context.Context, in *ec2.CreateNatGatewayInput, opts ...func(*ec2.Options)) (*ec2.CreateNatGatewayOutput, error)
	AllocateAddress(ctx context.Context, in *ec2.AllocateAddressInput, opts ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error)
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	DescribeRouteTables(ctx context.Context, in *ec2.DescribeRouteTablesInput, opts ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
	CreateRouteTable(ctx context.Context, in *ec2.CreateRouteTableInput, opts ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error)
	CreateRoute(ctx context.Context, in *ec2.CreateRouteInput, opts ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error)
	AssociateRouteTable(ctx context.Context, in *ec2.AssociateRouteTableInput, opts ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error)

	// Prefix list (CloudFront managed)
	DescribeManagedPrefixLists(ctx context.Context, in *ec2.DescribeManagedPrefixListsInput, opts ...func(*ec2.Options)) (*ec2.DescribeManagedPrefixListsOutput, error)

	// Security group
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	CreateSecurityGroup(ctx context.Context, in *ec2.CreateSecurityGroupInput, opts ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, opts ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, opts ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)

	// ENI
	CreateNetworkInterface(ctx context.Context, in *ec2.CreateNetworkInterfaceInput, opts ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, opts ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	DeleteNetworkInterface(ctx context.Context, in *ec2.DeleteNetworkInterfaceInput, opts ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error)

	// Instances
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)

	// Volumes
	DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, opts ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	ModifyVolume(ctx context.Context, in *ec2.ModifyVolumeInput, opts ...func(*ec2.Options)) (*ec2.ModifyVolumeOutput, error)
	CreateVolume(ctx context.Context, in *ec2.CreateVolumeInput, opts ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error)
	DeleteVolume(ctx context.Context, in *ec2.DeleteVolumeInput, opts ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	AttachVolume(ctx context.Context, in *ec2.AttachVolumeInput, opts ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error)
	DetachVolume(ctx context.Context, in *ec2.DetachVolumeInput, opts ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error)

	// Snapshots (EBS backups)
	CreateSnapshot(ctx context.Context, in *ec2.CreateSnapshotInput, opts ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error)
	DescribeSnapshots(ctx context.Context, in *ec2.DescribeSnapshotsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
	DeleteSnapshot(ctx context.Context, in *ec2.DeleteSnapshotInput, opts ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)

	// Tagging
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(ctx context.Context, in *ec2.DeleteTagsInput, opts ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)
}

func newEC2Client(cfg aws.Config) EC2API {
	return ec2.NewFromConfig(cfg)
}
