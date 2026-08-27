package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

const (
	// ec2AssumeRolePolicy is the trust policy that lets EC2 instances
	// assume the nakama instance role.
	ec2AssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "ec2.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}`

	// ssmCoreManagedPolicyARN grants the agent the permissions
	// required by SSM Run Command, Session Manager, and Parameter
	// Store reads/writes scoped via additional inline policy.
	ssmCoreManagedPolicyARN = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
)

// nakamaInlinePolicyName is the name of the inline policy attached
// to the instance role to grant per-/nakama/* SSM access.
const nakamaInlinePolicyName = "nakama-instance-ssm-access"

// nakamaInlinePolicyDocument grants the two SSM Parameter Store
// operations the instance-side scripts actually perform:
//   - GetParameter on .../code-server/password (User Data pulls the
//     SecureString; KMS decrypt via `aws/ssm` needs no explicit grant)
//   - PutParameter on .../code-server/version (User Data and the
//     `update` Run Command write the running version back)
//
// The role is shared by every code-server instance, so the paths keep
// a wildcard segment for the instance name. meta/* is deliberately
// out of reach: instances must not be able to read or redirect the
// resource IDs (instance-id, eni-id, ...) that the CLI trusts.
const nakamaInlinePolicyDocument = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "NakamaReadCodeServerPassword",
      "Effect": "Allow",
      "Action": [
        "ssm:GetParameter"
      ],
      "Resource": "arn:aws:ssm:*:*:parameter/nakama/*/code-server/password"
    },
    {
      "Sid": "NakamaWriteCodeServerVersion",
      "Effect": "Allow",
      "Action": [
        "ssm:PutParameter"
      ],
      "Resource": "arn:aws:ssm:*:*:parameter/nakama/*/code-server/version"
    }
  ]
}`

func (s *Service) initIAM(ctx context.Context, res *InitResult) error {
	roleARN, err := s.upsertRole(ctx, naming.InstanceRole(),
		"nakama-managed EC2 instance role", s.attachRolePolicies)
	if err != nil {
		return err
	}
	res.InstanceRoleARN = roleARN

	profileARN, err := s.upsertProfile(ctx, naming.InstanceProfile(), naming.InstanceRole())
	if err != nil {
		return err
	}
	res.InstanceProfileARN = profileARN

	// The NAT instance gets its own role: SSM-managed for
	// troubleshooting, but with no Parameter Store access, so a
	// compromised NAT node cannot read code-server passwords or
	// redirect meta parameters.
	natRoleARN, err := s.upsertRole(ctx, naming.NATInstanceRole(),
		"nakama-managed NAT instance role (SSM core only, no Parameter Store access)",
		s.attachNATRolePolicies)
	if err != nil {
		return err
	}
	res.NATInstanceRoleARN = natRoleARN

	natProfileARN, err := s.upsertProfile(ctx, naming.NATInstanceProfile(), naming.NATInstanceRole())
	if err != nil {
		return err
	}
	res.NATInstanceProfileARN = natProfileARN
	return nil
}

// upsertRole creates or reuses an IAM role and (re-)applies its
// policies through attach, healing manual deletions idempotently.
func (s *Service) upsertRole(ctx context.Context, roleName, description string, attach func(context.Context, string) error) (string, error) {
	getOut, err := s.clients.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	switch {
	case err == nil:
		if getOut.Role == nil || getOut.Role.Arn == nil {
			return "", errors.New("GetRole returned nil")
		}
		slog.Info("iam role: reusing existing", "name", roleName, "arn", *getOut.Role.Arn)
		// Best-effort re-attach of policies so manual deletions are healed.
		if err := attach(ctx, roleName); err != nil {
			return "", err
		}
		return *getOut.Role.Arn, nil

	case awshelper.IsAPIError(err, "NoSuchEntity"):
		// fall through to creation
	default:
		return "", fmt.Errorf("GetRole(%s): %w", roleName, err)
	}

	tags := tagging.ForShared(tagging.ComponentIAM, roleName)
	c, err := s.clients.IAM.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(ec2AssumeRolePolicy),
		Description:              aws.String(description),
		Tags:                     awshelper.IAMTags(tags),
	})
	if err != nil {
		return "", fmt.Errorf("CreateRole: %w", err)
	}
	if c.Role == nil || c.Role.Arn == nil {
		return "", errors.New("CreateRole returned nil")
	}
	slog.Info("iam role: created", "name", roleName, "arn", *c.Role.Arn)

	if err := attach(ctx, roleName); err != nil {
		return "", err
	}
	return *c.Role.Arn, nil
}

func (s *Service) attachRolePolicies(ctx context.Context, roleName string) error {
	if _, err := s.clients.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(roleName),
		PolicyArn: aws.String(ssmCoreManagedPolicyARN),
	}); err != nil {
		return fmt.Errorf("AttachRolePolicy(%s): %w", ssmCoreManagedPolicyARN, err)
	}
	if _, err := s.clients.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(nakamaInlinePolicyName),
		PolicyDocument: aws.String(nakamaInlinePolicyDocument),
	}); err != nil {
		return fmt.Errorf("PutRolePolicy: %w", err)
	}
	return nil
}

// attachNATRolePolicies grants the NAT role SSM manageability only:
// no inline Parameter Store policy.
func (s *Service) attachNATRolePolicies(ctx context.Context, roleName string) error {
	if _, err := s.clients.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(roleName),
		PolicyArn: aws.String(ssmCoreManagedPolicyARN),
	}); err != nil {
		return fmt.Errorf("AttachRolePolicy(%s): %w", ssmCoreManagedPolicyARN, err)
	}
	return nil
}

func (s *Service) upsertProfile(ctx context.Context, name, roleName string) (string, error) {
	getOut, err := s.clients.IAM.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(name),
	})
	switch {
	case err == nil:
		if getOut.InstanceProfile == nil || getOut.InstanceProfile.Arn == nil {
			return "", errors.New("GetInstanceProfile returned nil")
		}
		slog.Info("iam instance profile: reusing existing", "name", name, "arn", *getOut.InstanceProfile.Arn)
		return *getOut.InstanceProfile.Arn, s.attachRoleToProfileIfNeeded(ctx, name, roleName, getOut.InstanceProfile.Roles)

	case awshelper.IsAPIError(err, "NoSuchEntity"):
		// create below
	default:
		return "", fmt.Errorf("GetInstanceProfile(%s): %w", name, err)
	}

	tags := tagging.ForShared(tagging.ComponentIAM, name)
	c, err := s.clients.IAM.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(name),
		Tags:                awshelper.IAMTags(tags),
	})
	if err != nil {
		return "", fmt.Errorf("CreateInstanceProfile: %w", err)
	}
	if c.InstanceProfile == nil || c.InstanceProfile.Arn == nil {
		return "", errors.New("CreateInstanceProfile returned nil")
	}
	slog.Info("iam instance profile: created", "name", name, "arn", *c.InstanceProfile.Arn)

	if err := s.attachRoleToProfileIfNeeded(ctx, name, roleName, nil); err != nil {
		return "", err
	}
	return *c.InstanceProfile.Arn, nil
}

func (s *Service) attachRoleToProfileIfNeeded(ctx context.Context, profileName, roleName string, current []iamtypes.Role) error {
	for _, r := range current {
		if r.RoleName != nil && *r.RoleName == roleName {
			return nil
		}
	}
	if _, err := s.clients.IAM.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
		RoleName:            aws.String(roleName),
	}); err != nil {
		// Already attached errors out with LimitExceeded; treat
		// as success so this stays idempotent.
		if awshelper.IsAPIError(err, "LimitExceeded") {
			return nil
		}
		return fmt.Errorf("AddRoleToInstanceProfile: %w", err)
	}
	slog.Info("iam: role added to instance profile", "profile", profileName, "role", roleName)
	return nil
}
