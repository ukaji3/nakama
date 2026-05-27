package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
)

// instanceNamePattern is the validation regex for the user-supplied
// nakama instance name. Constraints:
//   - 2-32 chars
//   - lower-case alphanumeric and hyphens
//   - must start and end with an alphanumeric
var instanceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

// ValidateInstanceName returns nil iff name conforms to the v1
// nakama instance-name conventions.
func ValidateInstanceName(name string) error {
	if !instanceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid instance name %q: must match %s",
			name, instanceNamePattern.String())
	}
	return nil
}

// SSM paths used by per-instance metadata.
func ssmPasswordPath(name string) string {
	return naming.SSMPath(name, "code-server", "password")
}
func ssmVersionPath(name string) string {
	return naming.SSMPath(name, "code-server", "version")
}
func ssmInstanceIDPath(name string) string {
	return naming.SSMPath(name, "meta", "instance-id")
}
func ssmENIIDPath(name string) string {
	return naming.SSMPath(name, "meta", "eni-id")
}
func ssmDistributionIDPath(name string) string {
	return naming.SSMPath(name, "meta", "distribution-id")
}
func ssmDistributionDomainPath(name string) string {
	return naming.SSMPath(name, "meta", "distribution-domain")
}
func ssmVPCOriginIDPath(name string) string {
	return naming.SSMPath(name, "meta", "vpc-origin-id")
}
func ssmSGIDPath(name string) string {
	return naming.SSMPath(name, "meta", "security-group-id")
}

// generatePassword returns a cryptographically secure 32-char
// password drawn from [A-Za-z0-9].
func generatePassword() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, config.PasswordLength)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// requireInitialized errors out unless /nakama/_meta/initialized is
// present in SSM. nakama aws init is the prerequisite.
func (s *Service) requireInitialized(ctx context.Context) error {
	out, err := s.clients.SSM.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(naming.SSMMetaInitialized()),
	})
	if err != nil {
		// Treat ParameterNotFound as "not initialized" with a clearer message.
		var apiErr interface{ ErrorCode() string }
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ParameterNotFound" {
			return errors.New("nakama base infrastructure not initialized; run \"nakama aws init\" first")
		}
		return fmt.Errorf("GetParameter(%s): %w", naming.SSMMetaInitialized(), err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil || strings.TrimSpace(*out.Parameter.Value) != "true" {
		return errors.New("nakama base infrastructure not initialized; run \"nakama aws init\" first")
	}
	return nil
}
