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

	// WithAdmin attaches the shared nakama-managed admin instance role
	// (AdministratorAccess) to the instance. Mutually exclusive with
	// WithRole (ADR 0010).
	WithAdmin bool
	// WithRole attaches an existing, user-supplied IAM role (by name)
	// to the instance. Mutually exclusive with WithAdmin (ADR 0010).
	WithRole string

	// Provision declares post-create provisioning (setup script,
	// dotfiles, preset). The zero value provisions nothing (Issue #54).
	Provision ProvisionSpec

	// Bare creates a lightweight development box: no code-server, no
	// password, no CloudFront distribution and no VPC origin. Reached
	// with connect / ssh / tunnel instead of a browser (Issue #52).
	Bare bool

	// NoKiro skips the Kiro CLI / Kiro Crew install that the
	// cloud-init user-data performs by default (Issue #68). It is
	// expressed negatively because installing is the default; the
	// user-data parameter is the positive InstallKiro.
	NoKiro bool
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
	Password           string `json:"password,omitempty"` // cleared by the CLI unless --show-password
	IAMRole            string `json:"iam_role,omitempty"`
	InstanceProfile    string `json:"instance_profile,omitempty"`

	// Bare reports that the instance carries no code-server and no
	// CloudFront distribution (Issue #52).
	Bare bool `json:"bare,omitempty"`

	// Provision reports the post-create provisioning outcome when
	// --setup / --dotfiles / --preset were used (Issue #54). A failed
	// provisioning does not fail the create: the instance exists and is
	// usable, so the CLI warns and points at the rerun command.
	Provision *ProvisionResult `json:"provision,omitempty"`
}

// CreateInstance provisions everything needed to expose a new
// nakama code-server instance through CloudFront. The flow is
// described in docs/design.md §8.2.
func (s *Service) CreateInstance(ctx context.Context, in CreateInstanceInput) (*CreateInstanceOutput, error) {
	if err := ValidateNewInstanceName(in.Name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.InstanceType) == "" {
		return nil, errors.New("instance-type is required")
	}
	if err := userdata.ValidatePinnedVersion(in.CodeServerVersion); err != nil {
		return nil, err
	}
	if in.WithAdmin && in.WithRole != "" {
		return nil, errors.New("--with-admin and --with-role are mutually exclusive")
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

	// --with-admin / --with-role: resolve the dedicated instance role,
	// preflight the caller's IAM permissions (fail-closed) and provision
	// the role/profile BEFORE any other resource is created, so that a
	// permission failure leaves nothing behind (ADR 0010). Without the
	// flags this block is skipped entirely and the shared base profile
	// from `nakama aws init` is used, preserving the previous behavior.
	var roleSel *instanceProfileSelection
	if in.WithAdmin || in.WithRole != "" {
		var err error
		roleSel, err = s.ensureInstanceProfileForCreate(ctx, in)
		if err != nil {
			return nil, err
		}
	}

	slog.Info("create: starting", "name", in.Name, "instance_type", in.InstanceType, "ebs_gib", in.EBSGiB)

	// Resolve (and validate) the CPU architecture required by the
	// requested instance type. This fails fast for a non-existent type
	// and selects the matching Ubuntu AMI (amd64 vs arm64/Graviton).
	arch, err := s.resolveInstanceArchitecture(ctx, in.InstanceType)
	if err != nil {
		return nil, err
	}

	amiID, err := s.clients.AMIResolver.LatestUbuntu2604(ctx, arch)
	if err != nil {
		return nil, fmt.Errorf("resolve ubuntu 26.04 %s ami: %w", arch, err)
	}
	slog.Info("create: ami resolved", "architecture", arch, "ami_id", amiID)

	// A bare instance has no code-server, so there is no password to
	// generate or store (Issue #52).
	var password string
	if !in.Bare {
		if password, err = generatePassword(); err != nil {
			return nil, fmt.Errorf("generate password: %w", err)
		}
		if err := s.putPasswordSecure(ctx, in.Name, password); err != nil {
			return nil, fmt.Errorf("store password: %w", err)
		}
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

	var ud string
	if in.Bare {
		ud, err = userdata.RenderBare(userdata.BareParams{
			InstanceName: in.Name,
			Region:       config.Region,
			InstallKiro:  !in.NoKiro,
		})
	} else {
		ud, err = userdata.RenderCodeServer(userdata.CodeServerParams{
			InstanceName:         in.Name,
			Region:               config.Region,
			Port:                 config.CodeServerPort,
			SSMPasswordParameter: ssmPasswordPath(in.Name),
			SSMVersionParameter:  ssmVersionPath(in.Name),
			PinnedVersion:        in.CodeServerVersion,
			InstallKiro:          !in.NoKiro,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("render user data: %w", err)
	}

	// The code-server instance installs itself from the internet during
	// cloud-init, so the shared NAT instance must be running (and thus
	// providing egress) before we launch it.
	if err := s.ensureNATInstanceRunning(ctx); err != nil {
		return nil, fmt.Errorf("start NAT instance: %w", err)
	}

	instanceID, err := s.runInstance(ctx, runInstanceArgs{
		Name:                in.Name,
		AMIID:               amiID,
		InstanceType:        in.InstanceType,
		EBSGiB:              in.EBSGiB,
		ENIID:               eniID,
		UserDataBase64:      base64.StdEncoding.EncodeToString([]byte(ud)),
		InstanceProfileName: selectedProfileName(roleSel),
	})
	if err != nil {
		return nil, fmt.Errorf("run instance: %w", err)
	}

	if err := s.waitInstanceRunning(ctx, instanceID); err != nil {
		return nil, fmt.Errorf("wait instance running: %w", err)
	}

	// The browser entry path (private DNS -> VPC origin -> distribution)
	// exists only for code-server instances. Skipping it is what makes
	// --bare fast: no CloudFront deploy wait (Issue #52).
	var privateDNS, vpcOriginID, distID, distDomain string
	if !in.Bare {
		privateDNS, err = s.privateDNSNameForInstance(ctx, instanceID)
		if err != nil {
			return nil, fmt.Errorf("resolve private DNS: %w", err)
		}
		slog.Info("create: private DNS resolved", "private_dns", privateDNS)

		vpcOriginID, err = s.createInstanceVPCOrigin(ctx, in.Name, instanceID)
		if err != nil {
			return nil, fmt.Errorf("create vpc origin: %w", err)
		}
	}

	if !in.Bare {
		distID, distDomain, err = s.createInstanceDistribution(ctx, in.Name, vpcOriginID, privateDNS, base.WAFWebACLARN)
		if err != nil {
			return nil, fmt.Errorf("create distribution: %w", err)
		}
	}

	// Persist instance metadata BEFORE waiting for the distribution
	// to be Deployed. This way, if the Deployed wait times out,
	// `nakama aws delete <name>` can still clean up everything.
	if err := s.writeInstanceMeta(ctx, in.Name, instanceMetaEntries(in.Name, instanceMetaValues{
		InstanceID:         instanceID,
		ENIID:              eniID,
		SecurityGroupID:    sgID,
		VPCOriginID:        vpcOriginID,
		DistributionID:     distID,
		DistributionDomain: distDomain,
		IAMRole:            selectedRoleName(roleSel),
		InstanceProfile:    selectedProfileName(roleSel),
		Bare:               in.Bare,
	})); err != nil {
		return nil, fmt.Errorf("write instance metadata: %w", err)
	}

	if !in.Bare {
		if err := s.waitDistributionDeployed(ctx, distID); err != nil {
			return nil, fmt.Errorf("wait distribution: %w", err)
		}
	}

	out := &CreateInstanceOutput{
		Name:               in.Name,
		InstanceID:         instanceID,
		ENIID:              eniID,
		SecurityGroupID:    sgID,
		VPCOriginID:        vpcOriginID,
		DistributionID:     distID,
		DistributionDomain: distDomain,
		Password:           password,
		Bare:               in.Bare,
		IAMRole:            selectedRoleName(roleSel),
		InstanceProfile:    selectedProfileName(roleSel),
	}
	if distDomain != "" {
		out.URL = "https://" + distDomain
	}
	slog.Info("create: completed",
		"name", in.Name,
		"instance_id", instanceID,
		"distribution_id", distID,
		"url", out.URL,
	)

	// Provisioning runs last, on the finished instance, and is
	// best-effort: the instance and all its resources exist by now, so a
	// failing user script must not fail the create (Issue #54). The
	// outcome travels back in the result for the CLI to surface.
	if !in.Provision.IsZero() {
		slog.Info("create: provisioning", "name", in.Name, "steps", provisionStepNames(in.Provision))
		pres, err := s.Provision(ctx, in.Name, in.Provision)
		if pres == nil {
			pres = &ProvisionResult{Name: in.Name, Steps: provisionStepNames(in.Provision), LogPath: ProvisionLogPath}
		}
		if err != nil {
			pres.Failed = true
			if pres.Err == "" {
				pres.Err = err.Error()
			}
			slog.Warn("create: provisioning failed", "name", in.Name, "error", err)
		}
		out.Provision = pres
	}
	return out, nil
}

// selectedRoleName returns the IAM role name of the selection, or ""
// when the create runs without --with-admin/--with-role.
func selectedRoleName(sel *instanceProfileSelection) string {
	if sel == nil {
		return ""
	}
	return sel.roleName
}

// selectedProfileName returns the instance-profile name of the
// selection, or "" (=> the shared base profile) without the flags.
func selectedProfileName(sel *instanceProfileSelection) string {
	if sel == nil {
		return ""
	}
	return sel.profileName
}

// instanceMetaValues carries the resource IDs persisted as SSM
// parameters under /nakama/<name>/meta/*. IAMRole/InstanceProfile are
// only written when non-empty (--with-admin / --with-role).
type instanceMetaValues struct {
	InstanceID         string
	ENIID              string
	SecurityGroupID    string
	VPCOriginID        string
	DistributionID     string
	DistributionDomain string
	IAMRole            string
	InstanceProfile    string
	// Bare marks the instance as having no code-server / distribution.
	Bare bool
}

// instanceMetaEntries maps the metadata values to their SSM parameter
// paths. Split out of CreateInstance so the key set is unit-testable.
func instanceMetaEntries(name string, v instanceMetaValues) map[string]string {
	m := map[string]string{
		ssmInstanceIDPath(name): v.InstanceID,
		ssmENIIDPath(name):      v.ENIID,
		ssmSGIDPath(name):       v.SecurityGroupID,
	}
	// A bare instance has no browser entry path, and SSM rejects empty
	// parameter values, so those keys are simply absent (Issue #52).
	for path, value := range map[string]string{
		ssmVPCOriginIDPath(name):        v.VPCOriginID,
		ssmDistributionIDPath(name):     v.DistributionID,
		ssmDistributionDomainPath(name): v.DistributionDomain,
	} {
		if value != "" {
			m[path] = value
		}
	}
	if v.Bare {
		m[ssmKindPath(name)] = instanceKindBare
	}
	if v.IAMRole != "" {
		m[ssmIAMRolePath(name)] = v.IAMRole
	}
	if v.InstanceProfile != "" {
		m[ssmInstanceProfilePath(name)] = v.InstanceProfile
	}
	return m
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
		return s.checkInstanceRunning(ctx, instanceID)
	})
}

// checkInstanceRunning is the polling predicate behind waitInstanceRunning.
// It is split out so it can be unit-tested without paying Poll's interval
// delay.
//
// It tolerates InvalidInstanceID.NotFound: EC2 has eventual consistency,
// and a freshly created instance ID may not be visible to DescribeInstances
// for a brief window after RunInstances returns. Treat that as "still
// pending" and let the caller retry.
func (s *Service) checkInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		if awshelper.IsAPIError(err, "InvalidInstanceID.NotFound") {
			slog.Info("instance: not yet visible to DescribeInstances (eventual consistency), retrying", "instance_id", instanceID)
			return false, nil
		}
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
