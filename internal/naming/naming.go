// Package naming centralizes resource name construction so that every
// AWS resource created by nakama is identifiable both by tag and by name.
//
// Conventions:
//   - All resources start with the prefix "nakama-".
//   - Per-instance resources include the instance <name>.
//   - Names are stable across re-runs; uniqueness comes from the <name> field.
package naming

import "strings"

// Prefix is the global namespace for all nakama-managed AWS resources.
const Prefix = "nakama"

// VPC returns the canonical VPC name.
func VPC() string { return Prefix + "-vpc" }

// IGW returns the canonical Internet Gateway name.
func IGW() string { return Prefix + "-igw" }

// NATGW returns the canonical NAT Gateway name.
func NATGW() string { return Prefix + "-natgw" }

// NATGWEIP returns the EIP name allocated for the NAT Gateway.
func NATGWEIP() string { return Prefix + "-natgw-eip" }

// PublicSubnet returns the public subnet name in the given AZ suffix.
// Example: PublicSubnet("1a") -> "nakama-public-1a".
func PublicSubnet(azSuffix string) string {
	return Prefix + "-public-" + azSuffix
}

// PrivateSubnet returns the private subnet name in the given AZ suffix.
func PrivateSubnet(azSuffix string) string {
	return Prefix + "-private-" + azSuffix
}

// PublicRouteTable returns the public route-table name.
func PublicRouteTable() string { return Prefix + "-rtb-public" }

// PrivateRouteTable returns the private route-table name.
func PrivateRouteTable() string { return Prefix + "-rtb-private" }

// WAFWebACL returns the shared WAFv2 web ACL name.
func WAFWebACL() string { return Prefix + "-waf" }

// WAFIPSet returns the shared WAFv2 IP set name used for ACL management.
func WAFIPSet() string { return Prefix + "-allowed-ips" }

// EC2 returns the EC2 instance Name tag for the given nakama instance name.
func EC2(name string) string { return Prefix + "-" + name }

// Snapshot returns the EBS snapshot Name tag for a backup.
// Example: Snapshot("dev01","before-x") -> "nakama-backup-dev01-before-x".
func Snapshot(name, label string) string {
	return Prefix + "-backup-" + name + "-" + label
}

// SecurityGroup returns the per-instance security-group name.
func SecurityGroup(name string) string { return Prefix + "-sg-" + name }

// ENI returns the per-instance ENI name.
func ENI(name string) string { return Prefix + "-eni-" + name }

// InstanceProfile returns the IAM instance-profile name shared by all
// nakama EC2 instances.
func InstanceProfile() string { return Prefix + "-instance-profile" }

// InstanceRole returns the IAM role name attached to InstanceProfile.
func InstanceRole() string { return Prefix + "-instance-role" }

// AdminInstanceRole returns the IAM role name carrying
// AdministratorAccess, shared by instances created with
// `nakama aws create --with-admin` (ADR 0010).
func AdminInstanceRole() string { return Prefix + "-instance-role-admin" }

// AdminInstanceProfile returns the IAM instance-profile name that
// wraps AdminInstanceRole (ADR 0010).
func AdminInstanceProfile() string { return Prefix + "-instance-profile-admin" }

// CustomInstanceProfile returns the IAM instance-profile name created
// for a user-supplied role passed via `--with-role <role>` when the
// role has no existing instance profile (ADR 0010).
func CustomInstanceProfile(role string) string { return Prefix + "-instance-profile-" + role }

// SSMPath returns a Parameter Store path under /nakama/<name>/<segments...>.
// Segments are joined with "/"; leading slash is added.
func SSMPath(instanceName string, segments ...string) string {
	parts := append([]string{"", Prefix, instanceName}, segments...)
	return strings.Join(parts, "/")
}

// SSMMetaInitialized returns the path used as the init() marker.
func SSMMetaInitialized() string { return "/" + Prefix + "/_meta/initialized" }

// SSMMetaVersion returns the path that records the nakama version
// that performed the last init().
func SSMMetaVersion() string { return "/" + Prefix + "/_meta/version" }

// NATInstance returns the canonical NAT instance name (ADR 0007).
func NATInstance() string { return Prefix + "-nat" }

// NATInstanceRole is the IAM role attached to the shared NAT
// instance. It is deliberately separate from InstanceRole: the NAT
// node needs SSM manageability only and must not be able to read
// code-server passwords or meta parameters.
func NATInstanceRole() string { return Prefix + "-nat-role" }

// NATInstanceProfile is the instance profile wrapping NATInstanceRole.
func NATInstanceProfile() string { return Prefix + "-nat-instance-profile" }

// NATInstanceSG returns the security group name for the NAT instance.
func NATInstanceSG() string { return Prefix + "-nat-sg" }
