// Package config holds runtime-configuration values that are
// considered fixed for v1 of nakama (see docs/requirements.md).
//
// Values are intentionally hard-coded so that operators cannot
// drift the deployment topology unintentionally. A future version
// may externalize a subset to a config file.
package config

import "fmt"

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

// Ubuntu 26.04 LTS SSM public parameter paths (published by Canonical)
// for the latest AMI in the configured region, keyed by CPU
// architecture. Graviton (arm64) instance types resolve the arm64 AMI;
// Intel/AMD (x86_64) types resolve the amd64 AMI.
//
// Verified via:
//
//	aws ssm get-parameter --region ap-northeast-1 \
//	  --name /aws/service/canonical/ubuntu/server/26.04/stable/current/<arch>/hvm/ebs-gp3/ami-id
//
// where <arch> is amd64 or arm64.
const (
	UbuntuAMISSMParameterAMD64 = "/aws/service/canonical/ubuntu/server/26.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
	UbuntuAMISSMParameterARM64 = "/aws/service/canonical/ubuntu/server/26.04/stable/current/arm64/hvm/ebs-gp3/ami-id"
)

// CPU architecture identifiers as reported by EC2 DescribeInstanceTypes
// (ProcessorInfo.SupportedArchitectures) and accepted by
// UbuntuAMISSMParameterFor.
const (
	ArchX8664 = "x86_64"
	ArchARM64 = "arm64"
)

// UbuntuAMISSMParameterFor returns the Canonical SSM public parameter
// path for the latest Ubuntu 26.04 AMI of the given CPU architecture.
// arch must be an EC2 architecture value (ArchX8664 or ArchARM64).
func UbuntuAMISSMParameterFor(arch string) (string, error) {
	switch arch {
	case ArchX8664:
		return UbuntuAMISSMParameterAMD64, nil
	case ArchARM64:
		return UbuntuAMISSMParameterARM64, nil
	default:
		return "", fmt.Errorf("unsupported CPU architecture %q (want %s or %s)", arch, ArchX8664, ArchARM64)
	}
}

// CheckIPURL is the endpoint nakama queries to discover the caller's
// public IP when `nakama aws acl add` is invoked without an argument,
// and the one `open` and `doctor` ask for a quick check.
const CheckIPURL = "https://checkip.amazonaws.com/"

// IfconfigIOURL is the second endpoint `acl add` queries (Issue #104).
// /ip answers with the address alone, whatever the User-Agent.
const IfconfigIOURL = "https://ifconfig.io/ip"

// CloudFrontVPCOriginsPrefixListName is the AWS-managed prefix list
// whose addresses are used by CloudFront's service-managed ENIs to
// reach VPC origins. The actual ID is region-specific and resolved
// at runtime via ec2:DescribeManagedPrefixLists.
const CloudFrontVPCOriginsPrefixListName = "com.amazonaws.global.cloudfront.origin-facing"

// NAT instance settings (ADR 0007). The NAT Gateway is replaced by a
// small NAT instance whose power state is tied to the code-server
// lifecycle to avoid the NAT Gateway's fixed hourly cost.
const (
	// NATInstanceType is the EC2 instance type for the NAT instance.
	NATInstanceType = "t4g.nano"

	// NATInstanceAMISSMParameter is the SSM public parameter path for the
	// latest Amazon Linux 2023 arm64 AMI, used for the NAT instance.
	// NOTE: verify the path exists at deploy time, e.g.:
	//   aws ssm get-parameters-by-path --path /aws/service/ami-amazon-linux-latest
	NATInstanceAMISSMParameter = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
)
