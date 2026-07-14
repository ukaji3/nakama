package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// IAMAPI wraps IAM calls used by `nakama aws init` to provision a
// shared instance role that grants EC2 instances permission to
// read the SSM Parameter Store password and report status back,
// and by `nakama aws create --with-admin` / `--with-role` to select
// a dedicated instance role after a SimulatePrincipalPolicy
// preflight (ADR 0010).
type IAMAPI interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(ctx context.Context, in *iam.CreateRoleInput, opts ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	AttachRolePolicy(ctx context.Context, in *iam.AttachRolePolicyInput, opts ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, opts ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, opts ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	CreateInstanceProfile(ctx context.Context, in *iam.CreateInstanceProfileInput, opts ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error)
	AddRoleToInstanceProfile(ctx context.Context, in *iam.AddRoleToInstanceProfileInput, opts ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error)
	SimulatePrincipalPolicy(ctx context.Context, in *iam.SimulatePrincipalPolicyInput, opts ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error)
	ListInstanceProfilesForRole(ctx context.Context, in *iam.ListInstanceProfilesForRoleInput, opts ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)
	ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, opts ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
}

func newIAMClient(cfg aws.Config) IAMAPI {
	return iam.NewFromConfig(cfg)
}
