package service

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// fakeTypeEC2 overrides DescribeInstanceTypes only.
type fakeTypeEC2 struct {
	fakeEC2
	out *ec2.DescribeInstanceTypesOutput
	err error
}

func (f *fakeTypeEC2) DescribeInstanceTypes(_ context.Context, _ *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return f.out, f.err
}

func TestAssertInstanceTypeExists(t *testing.T) {
	found := &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: []ec2types.InstanceTypeInfo{{InstanceType: ec2types.InstanceTypeT3Medium}},
	}
	empty := &ec2.DescribeInstanceTypesOutput{}
	invalid := &smithy.GenericAPIError{Code: "InvalidInstanceType", Message: "not found"}

	tests := []struct {
		name    string
		out     *ec2.DescribeInstanceTypesOutput
		err     error
		wantErr bool
	}{
		{name: "exists", out: found, wantErr: false},
		{name: "empty list", out: empty, wantErr: true},
		{name: "InvalidInstanceType", err: invalid, wantErr: true},
		{name: "other api error", err: errors.New("boom"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newServiceWithEC2(&fakeTypeEC2{out: tt.out, err: tt.err})
			err := svc.assertInstanceTypeExists(context.Background(), "t3.medium")
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
