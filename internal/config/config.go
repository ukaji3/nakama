// Package config holds runtime-configuration values that are
// considered fixed for v1 of nakama (see docs/requirements.md).
//
// Values are intentionally hard-coded so that operators cannot
// drift the deployment topology unintentionally. A future version
// may externalize a subset to a config file.
package config

// Region is the only AWS region nakama operates in (v1).
const Region = "ap-northeast-1"

// VPCCIDR is the CIDR block for the nakama VPC.
const VPCCIDR = "10.20.0.0/16"

// Subnet CIDRs.
const (
	PublicSubnet1aCIDR  = "10.20.0.0/24"
	PrivateSubnet1aCIDR = "10.20.10.0/24"
	PrivateSubnet1cCIDR = "10.20.11.0/24"
)

// AvailabilityZones is the ordered list of AZs nakama uses.
// ap-northeast-1d (apne1-az3) is intentionally excluded because
// CloudFront VPC origins is not supported in that AZ.
//
// Ref: https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html
//
//	"Asia Pacific (Tokyo) | ap-northeast-1 (except AZ apne1-az3)"
var AvailabilityZones = []string{"ap-northeast-1a", "ap-northeast-1c"}

// Code-server defaults.
const (
	// CodeServerPort is the port on which code-server listens inside the EC2.
	CodeServerPort = 8080

	// EBSDefaultGiB is the default root EBS size at create time.
	EBSDefaultGiB int32 = 20

	// PasswordLength is the length of the auto-generated code-server password.
	PasswordLength = 32
)

// UbuntuAMISSMParameter is the SSM public parameter path for the
// latest Ubuntu 26.04 LTS amd64 AMI in the configured region.
//
// Verified via:
//
//	aws ssm get-parameters-by-path \
//	  --path /aws/service/canonical/ubuntu/server/26.04 --recursive \
//	  --region ap-northeast-1
const UbuntuAMISSMParameter = "/aws/service/canonical/ubuntu/server/26.04/stable/current/amd64/hvm/ebs-gp3/ami-id"

// CheckIPURL is the endpoint nakama queries to discover the caller's
// public IP when `nakama aws acl add` is invoked without an argument.
const CheckIPURL = "https://checkip.amazonaws.com/"

// CloudFrontVPCOriginsPrefixListName is the AWS-managed prefix list
// whose addresses are used by CloudFront's service-managed ENIs to
// reach VPC origins. The actual ID is region-specific and resolved
// at runtime via ec2:DescribeManagedPrefixLists.
const CloudFrontVPCOriginsPrefixListName = "com.amazonaws.global.cloudfront.origin-facing"
