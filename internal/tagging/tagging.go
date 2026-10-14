// Package tagging centralizes the tag schema applied to every AWS
// resource created by nakama. Filtering by these tags is the only
// mechanism nakama uses to discover its own resources.
package tagging

// Common tag keys.
const (
	// KeyManaged is set on every nakama-owned resource.
	KeyManaged = "nakama:managed"
	// KeyName identifies a specific nakama instance (per-instance resources).
	KeyName = "nakama:name"
	// KeyComponent labels a resource's role within nakama.
	KeyComponent = "nakama:component"
	// KeyBackupTime records a snapshot's creation time (RFC3339).
	KeyBackupTime = "nakama:backup-time"
	// KeyBackupLabel is the human-readable selector for a backup.
	KeyBackupLabel = "nakama:backup-label"
	// KeyNote holds a free-text memo attached to a backup.
	KeyNote = "nakama:note"
	// KeyInstanceType, KeyArchitecture, KeyKind and KeyIAMRole record,
	// on a backup, the instance it was taken from. `nakama aws restore`
	// reads them to rebuild the instance after it was deleted
	// (Issue #96). Backups taken before then carry none of them.
	KeyInstanceType = "nakama:instance-type"
	KeyArchitecture = "nakama:architecture"
	KeyKind         = "nakama:kind"
	KeyIAMRole      = "nakama:iam-role"
	// NameTag is the standard AWS Name tag, kept separate from KeyName.
	NameTag = "Name"
)

// Common tag values.
const (
	ValueManagedTrue = "true"
)

// Component values (used as tag value for KeyComponent).
const (
	ComponentVPC           = "vpc"
	ComponentSubnetPublic  = "subnet-public"
	ComponentSubnetPrivate = "subnet-private"
	ComponentIGW           = "igw"
	ComponentNATGW         = "natgw"
	ComponentEIP           = "eip"
	ComponentRouteTable    = "rtb"
	ComponentEC2           = "ec2"
	ComponentENI           = "eni"
	ComponentSG            = "sg"
	ComponentEBS           = "ebs"
	ComponentCloudFront    = "cloudfront"
	ComponentVPCOrigin     = "vpc-origin"
	ComponentWAF           = "waf"
	ComponentIAM           = "iam"
	ComponentBackup        = "backup"
)

// Base returns the minimum tag set every nakama-managed resource carries.
// Callers should append additional tags such as KeyName or NameTag as needed.
func Base() map[string]string {
	return map[string]string{
		KeyManaged: ValueManagedTrue,
	}
}

// ForInstance returns the tag set for a per-instance resource.
// nameTag is the value to use for the AWS Name tag.
func ForInstance(instanceName, component, nameTag string) map[string]string {
	return map[string]string{
		KeyManaged:   ValueManagedTrue,
		KeyName:      instanceName,
		KeyComponent: component,
		NameTag:      nameTag,
	}
}

// ForShared returns the tag set for a shared (non per-instance) resource.
func ForShared(component, nameTag string) map[string]string {
	return map[string]string{
		KeyManaged:   ValueManagedTrue,
		KeyComponent: component,
		NameTag:      nameTag,
	}
}

// BackupSource describes the instance a backup was taken from
// (Issue #96). Empty fields are left out of the tag set.
type BackupSource struct {
	InstanceType string // e.g. "t3.medium"
	Architecture string // "x86_64" or "arm64"
	Kind         string // "vscode" or "bare"
	IAMRole      string // set only with create --with-admin / --with-role
}

// ForBackup returns the tag set for a backup (EBS snapshot).
// createdAt is an RFC3339 timestamp; note may be empty.
func ForBackup(instanceName, label, nameTag, createdAt, note string, src BackupSource) map[string]string {
	m := map[string]string{
		KeyManaged:     ValueManagedTrue,
		KeyName:        instanceName,
		KeyComponent:   ComponentBackup,
		KeyBackupLabel: label,
		KeyBackupTime:  createdAt,
		NameTag:        nameTag,
	}
	for k, v := range map[string]string{
		KeyNote:         note,
		KeyInstanceType: src.InstanceType,
		KeyArchitecture: src.Architecture,
		KeyKind:         src.Kind,
		KeyIAMRole:      src.IAMRole,
	} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

// ComponentNATInstance identifies the NAT instance (ADR 0007),
// kept distinct from ComponentNATGW so that reconcile can exclude the
// NAT instance when counting code-server instances.
const ComponentNATInstance = "nat-instance"
