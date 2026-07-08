package service

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/config"
)

// resolveInstanceArchitecture validates that the given EC2 instance
// type exists and returns the CPU architecture nakama should launch it
// with (config.ArchX8664 or config.ArchARM64), derived from
// DescribeInstanceTypes.ProcessorInfo.SupportedArchitectures.
//
// This doubles as the create/edit instance-type preflight: a
// non-existent type fails here (before any mutating call) rather than
// at RunInstances / ModifyInstanceAttribute.
//
// Graviton families (t4g, m6g/m7g, c6g/c7g, r6g/r7g, ...) report only
// "arm64"; Intel/AMD families report only "x86_64".
func (s *Service) resolveInstanceArchitecture(ctx context.Context, t string) (string, error) {
	out, err := s.clients.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{ec2types.InstanceType(t)},
	})
	if err != nil {
		if awshelper.IsAPIError(err, "InvalidInstanceType") {
			return "", fmt.Errorf("instance type %q does not exist", t)
		}
		return "", fmt.Errorf("DescribeInstanceTypes: %w", err)
	}
	if len(out.InstanceTypes) == 0 || out.InstanceTypes[0].ProcessorInfo == nil {
		return "", fmt.Errorf("instance type %q does not exist", t)
	}
	return selectArchitecture(t, out.InstanceTypes[0].ProcessorInfo.SupportedArchitectures)
}

// selectArchitecture picks the AMI architecture for an instance type
// from the architectures it advertises. x86_64 is preferred when a type
// supports both (not the case for current general-purpose families),
// preserving nakama's historical default; arm64 is used for arm64-only
// (Graviton) types.
func selectArchitecture(t string, archs []ec2types.ArchitectureType) (string, error) {
	var hasX86, hasARM bool
	for _, a := range archs {
		switch string(a) {
		case config.ArchX8664:
			hasX86 = true
		case config.ArchARM64:
			hasARM = true
		}
	}
	switch {
	case hasX86:
		return config.ArchX8664, nil
	case hasARM:
		return config.ArchARM64, nil
	default:
		return "", fmt.Errorf(
			"instance type %q advertises no architecture nakama can launch (supports %v; want %s or %s)",
			t, archs, config.ArchX8664, config.ArchARM64)
	}
}
