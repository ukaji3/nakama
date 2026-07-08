package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	nakamaConfig "github.com/ukaji3/nakama/internal/config"
)

// AMIResolver resolves the latest Ubuntu 26.04 AMI ID for the
// configured region by reading Canonical's published SSM public
// parameters (see internal/config.UbuntuAMISSMParameterFor).
type AMIResolver interface {
	// LatestUbuntu2604 returns the latest Ubuntu 26.04 LTS AMI ID for
	// the given CPU architecture. arch must be an EC2 architecture
	// value: config.ArchX8664 ("x86_64") or config.ArchARM64 ("arm64").
	LatestUbuntu2604(ctx context.Context, arch string) (string, error)
}

type ssmAMIResolver struct {
	ssm *ssm.Client
}

func newAMIResolver(cfg aws.Config) AMIResolver {
	return &ssmAMIResolver{ssm: ssm.NewFromConfig(cfg)}
}

// LatestUbuntu2604 implements AMIResolver by reading the Canonical SSM
// public parameter for the latest Ubuntu 26.04 LTS AMI of the given
// architecture in the configured region.
func (r *ssmAMIResolver) LatestUbuntu2604(ctx context.Context, arch string) (string, error) {
	param, err := nakamaConfig.UbuntuAMISSMParameterFor(arch)
	if err != nil {
		return "", err
	}
	out, err := r.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(param),
	})
	if err != nil {
		return "", err
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", errEmptyAMIParameter
	}
	return *out.Parameter.Value, nil
}
