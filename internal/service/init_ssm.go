package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/version"
)

// initSSMMarkers writes /nakama/_meta/initialized=true and
// /nakama/_meta/version=<nakama-ver> so subsequent commands can
// detect that init() has run.
func (s *Service) initSSMMarkers(ctx context.Context) error {
	pairs := []struct {
		Name  string
		Value string
	}{
		{naming.SSMMetaInitialized(), "true"},
		{naming.SSMMetaVersion(), version.Version},
	}
	for _, p := range pairs {
		_, err := s.clients.SSM.PutParameter(ctx, &ssm.PutParameterInput{
			Name:      aws.String(p.Name),
			Value:     aws.String(p.Value),
			Type:      ssmtypes.ParameterTypeString,
			Overwrite: aws.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("PutParameter(%s): %w", p.Name, err)
		}
		slog.Debug("ssm marker written", "name", p.Name, "value", p.Value)
	}
	return nil
}
