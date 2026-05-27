package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	nakamaConfig "github.com/ukaji3/nakama/internal/config"
)

// AMIResolver resolves the latest Ubuntu 26.04 AMI ID for the
// configured region by reading Canonical's published SSM public
// parameter (see internal/config.UbuntuAMISSMParameter).
type AMIResolver interface {
	LatestUbuntu2604AMD64(ctx context.Context) (string, error)
}

type ssmAMIResolver struct {
	ssm *ssm.Client
}

func newAMIResolver(cfg aws.Config) AMIResolver {
	return &ssmAMIResolver{ssm: ssm.NewFromConfig(cfg)}
}

func (r *ssmAMIResolver) LatestUbuntu2604AMD64(ctx context.Context) (string, error) {
	out, err := r.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(nakamaConfig.UbuntuAMISSMParameter),
	})
	if err != nil {
		return "", err
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", errEmptyAMIParameter
	}
	return *out.Parameter.Value, nil
}
