package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// SSMAPI wraps the subset of Systems Manager used by nakama:
//
//   - Parameter Store reads/writes for storing per-instance metadata
//     and the code-server password (SecureString).
//   - Run Command (SendCommand) for in-instance operations such as
//     `growpart` + `resize2fs` after `nakama aws edit --ebs`.
type SSMAPI interface {
	// Parameter Store
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, opts ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	GetParameters(ctx context.Context, in *ssm.GetParametersInput, opts ...func(*ssm.Options)) (*ssm.GetParametersOutput, error)
	GetParametersByPath(ctx context.Context, in *ssm.GetParametersByPathInput, opts ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
	PutParameter(ctx context.Context, in *ssm.PutParameterInput, opts ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(ctx context.Context, in *ssm.DeleteParameterInput, opts ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
	DeleteParameters(ctx context.Context, in *ssm.DeleteParametersInput, opts ...func(*ssm.Options)) (*ssm.DeleteParametersOutput, error)

	// Run Command
	SendCommand(ctx context.Context, in *ssm.SendCommandInput, opts ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(ctx context.Context, in *ssm.GetCommandInvocationInput, opts ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

func newSSMClient(cfg aws.Config) SSMAPI {
	return ssm.NewFromConfig(cfg)
}
