package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/config"
)

// fakeTypeEC2 overrides DescribeInstanceTypes with a fixed response.
type fakeTypeEC2 struct {
	fakeEC2
	out *ec2.DescribeInstanceTypesOutput
	err error
}

func (f *fakeTypeEC2) DescribeInstanceTypes(_ context.Context, _ *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return f.out, f.err
}

// typeInfo is a helper to build a DescribeInstanceTypesOutput advertising
// the given architectures for one instance type.
func typeInfo(t string, archs ...string) *ec2.DescribeInstanceTypesOutput {
	supported := make([]ec2types.ArchitectureType, 0, len(archs))
	for _, a := range archs {
		supported = append(supported, ec2types.ArchitectureType(a))
	}
	return &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: []ec2types.InstanceTypeInfo{{
			InstanceType:  ec2types.InstanceType(t),
			ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: supported},
		}},
	}
}

func TestResolveInstanceArchitecture(t *testing.T) {
	invalid := &smithy.GenericAPIError{Code: "InvalidInstanceType", Message: "not found"}

	tests := []struct {
		name     string
		out      *ec2.DescribeInstanceTypesOutput
		err      error
		wantArch string
		wantErr  bool
	}{
		{name: "arm64 (graviton)", out: typeInfo("t4g.medium", config.ArchARM64), wantArch: config.ArchARM64},
		{name: "x86_64 (intel)", out: typeInfo("t3.medium", config.ArchX8664), wantArch: config.ArchX8664},
		{name: "dual prefers x86_64", out: typeInfo("dual.large", config.ArchX8664, config.ArchARM64), wantArch: config.ArchX8664},
		{name: "unsupported arch only", out: typeInfo("weird.large", "i386"), wantErr: true},
		{name: "nil ProcessorInfo", out: &ec2.DescribeInstanceTypesOutput{InstanceTypes: []ec2types.InstanceTypeInfo{{InstanceType: "t3.medium"}}}, wantErr: true},
		{name: "empty list", out: &ec2.DescribeInstanceTypesOutput{}, wantErr: true},
		{name: "InvalidInstanceType", err: invalid, wantErr: true},
		{name: "other api error", err: errors.New("boom"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newServiceWithEC2(&fakeTypeEC2{out: tt.out, err: tt.err})
			got, err := svc.resolveInstanceArchitecture(context.Background(), "some.type")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got arch=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantArch {
				t.Fatalf("arch=%q, want %q", got, tt.wantArch)
			}
		})
	}
}

func TestSelectArchitecture(t *testing.T) {
	t.Parallel()
	arch := func(vs ...string) []ec2types.ArchitectureType {
		out := make([]ec2types.ArchitectureType, 0, len(vs))
		for _, v := range vs {
			out = append(out, ec2types.ArchitectureType(v))
		}
		return out
	}
	tests := []struct {
		name     string
		archs    []ec2types.ArchitectureType
		wantArch string
		wantErr  bool
	}{
		{name: "arm64 only", archs: arch("arm64"), wantArch: config.ArchARM64},
		{name: "x86_64 only", archs: arch("x86_64"), wantArch: config.ArchX8664},
		{name: "both -> x86_64", archs: arch("x86_64", "arm64"), wantArch: config.ArchX8664},
		{name: "both reversed -> x86_64", archs: arch("arm64", "x86_64"), wantArch: config.ArchX8664},
		{name: "i386 only -> error", archs: arch("i386"), wantErr: true},
		{name: "empty -> error", archs: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectArchitecture("x.type", tt.archs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantArch {
				t.Fatalf("arch=%q, want %q", got, tt.wantArch)
			}
		})
	}
}

// fakeArchEditEC2 serves DescribeInstanceTypes (arch per type) and
// DescribeInstances (current instance) for the edit cross-arch guard.
type fakeArchEditEC2 struct {
	awsx.EC2API
	archByType map[string]string
	curType    string
	state      ec2types.InstanceStateName
}

func (f *fakeArchEditEC2) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	t := string(in.InstanceTypes[0])
	a, ok := f.archByType[t]
	if !ok {
		return &ec2.DescribeInstanceTypesOutput{}, nil
	}
	return typeInfo(t, a), nil
}

func (f *fakeArchEditEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{
				InstanceId:   aws.String("i-test"),
				InstanceType: ec2types.InstanceType(f.curType),
				State:        &ec2types.InstanceState{Name: f.state},
			}},
		}},
	}, nil
}

// fakeArchSSM returns a fixed instance-id for any GetParameter.
type fakeArchSSM struct{ awsx.SSMAPI }

func (fakeArchSSM) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String("i-test")}}, nil
}

func TestEditInstanceType_CrossArchRejected(t *testing.T) {
	svc := &Service{clients: &awsx.Clients{
		EC2: &fakeArchEditEC2{
			archByType: map[string]string{
				"t3.medium":  config.ArchX8664,
				"t4g.medium": config.ArchARM64,
			},
			curType: "t3.medium",
			state:   ec2types.InstanceStateNameStopped,
		},
		SSM: fakeArchSSM{},
	}}

	_, err := svc.EditInstanceType(context.Background(), EditInstanceTypeInput{
		Name:    "dev01",
		NewType: "t4g.medium",
	})
	if err == nil {
		t.Fatalf("expected cross-architecture change to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "architecture") {
		t.Fatalf("expected an architecture error, got: %v", err)
	}
}
