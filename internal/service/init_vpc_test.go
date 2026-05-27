package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakeEC2 is a hand-rolled mock for awsx.EC2API. Unimplemented
// methods panic via the nil embedded interface, which lets each
// test override only the small surface it exercises.
type fakeEC2 struct {
	awsx.EC2API // nil; calling un-overridden methods panics

	describeAZ func(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error)
	describeVpcs func(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	createVpc   func(context.Context, *ec2.CreateVpcInput, ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error)
	modifyVpc   func(context.Context, *ec2.ModifyVpcAttributeInput, ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error)
}

func (f *fakeEC2) DescribeAvailabilityZones(ctx context.Context, in *ec2.DescribeAvailabilityZonesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
	return f.describeAZ(ctx, in, opts...)
}
func (f *fakeEC2) DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return f.describeVpcs(ctx, in, opts...)
}
func (f *fakeEC2) CreateVpc(ctx context.Context, in *ec2.CreateVpcInput, opts ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
	return f.createVpc(ctx, in, opts...)
}
func (f *fakeEC2) ModifyVpcAttribute(ctx context.Context, in *ec2.ModifyVpcAttributeInput, opts ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error) {
	return f.modifyVpc(ctx, in, opts...)
}

func newServiceWithEC2(ec2 awsx.EC2API) *Service {
	return &Service{clients: &awsx.Clients{EC2: ec2}}
}

func TestAZSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"ap-northeast-1a", "1a"},
		{"ap-northeast-1c", "1c"},
		{"us-east-1b", "1b"},
		{"x", "x"},
		{"", ""},
	}
	for _, c := range cases {
		if got := azSuffix(c.in); got != c.want {
			t.Errorf("azSuffix(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestInitAZSanityCheck_OK(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{
		describeAZ: func(_ context.Context, in *ec2.DescribeAvailabilityZonesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
			return &ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					{ZoneName: aws.String("ap-northeast-1a"), ZoneId: aws.String("apne1-az4")},
					{ZoneName: aws.String("ap-northeast-1c"), ZoneId: aws.String("apne1-az1")},
				},
			}, nil
		},
	}
	if err := newServiceWithEC2(f).initAZSanityCheck(context.Background()); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestInitAZSanityCheck_RejectsExcludedAZID(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{
		describeAZ: func(_ context.Context, _ *ec2.DescribeAvailabilityZonesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
			return &ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					{ZoneName: aws.String("ap-northeast-1a"), ZoneId: aws.String("apne1-az3")},
					{ZoneName: aws.String("ap-northeast-1c"), ZoneId: aws.String("apne1-az1")},
				},
			}, nil
		},
	}
	err := newServiceWithEC2(f).initAZSanityCheck(context.Background())
	if err == nil {
		t.Fatal("expected error for apne1-az3 mapping, got nil")
	}
	if !strings.Contains(err.Error(), "apne1-az3") {
		t.Errorf("error %q should mention apne1-az3", err.Error())
	}
}

func TestInitAZSanityCheck_MissingZone(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{
		describeAZ: func(_ context.Context, _ *ec2.DescribeAvailabilityZonesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
			return &ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					{ZoneName: aws.String("ap-northeast-1a"), ZoneId: aws.String("apne1-az4")},
				},
			}, nil
		},
	}
	err := newServiceWithEC2(f).initAZSanityCheck(context.Background())
	if err == nil {
		t.Fatal("expected error for missing AZ, got nil")
	}
}

func TestInitVPC_ReusesExisting(t *testing.T) {
	t.Parallel()
	modifyCalls := 0
	createCalls := 0
	f := &fakeEC2{
		describeVpcs: func(_ context.Context, _ *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
			return &ec2.DescribeVpcsOutput{
				Vpcs: []ec2types.Vpc{{VpcId: aws.String("vpc-existing")}},
			}, nil
		},
		createVpc: func(_ context.Context, _ *ec2.CreateVpcInput, _ ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
			createCalls++
			return nil, errors.New("CreateVpc must not be called when an existing VPC was found")
		},
		modifyVpc: func(_ context.Context, _ *ec2.ModifyVpcAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error) {
			modifyCalls++
			return &ec2.ModifyVpcAttributeOutput{}, nil
		},
	}

	res := &InitResult{PrivateSubnetIDs: map[string]string{}}
	if err := newServiceWithEC2(f).initVPC(context.Background(), res); err != nil {
		t.Fatalf("initVPC: %v", err)
	}
	if res.VPCID != "vpc-existing" {
		t.Errorf("VPCID=%q, want vpc-existing", res.VPCID)
	}
	if createCalls != 0 {
		t.Errorf("CreateVpc called %d times, want 0", createCalls)
	}
	if modifyCalls != 2 {
		t.Errorf("ModifyVpcAttribute called %d times, want 2 (DnsSupport + DnsHostnames)", modifyCalls)
	}
}

func TestInitVPC_CreatesWhenMissing(t *testing.T) {
	t.Parallel()
	createCalls := 0
	modifyCalls := 0
	f := &fakeEC2{
		describeVpcs: func(_ context.Context, _ *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
			return &ec2.DescribeVpcsOutput{}, nil
		},
		createVpc: func(_ context.Context, in *ec2.CreateVpcInput, _ ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
			createCalls++
			if in.CidrBlock == nil || *in.CidrBlock != "10.20.0.0/16" {
				t.Errorf("unexpected CidrBlock: %v", in.CidrBlock)
			}
			if len(in.TagSpecifications) != 1 || in.TagSpecifications[0].ResourceType != ec2types.ResourceTypeVpc {
				t.Errorf("expected TagSpecifications with ResourceTypeVpc, got %v", in.TagSpecifications)
			}
			return &ec2.CreateVpcOutput{Vpc: &ec2types.Vpc{VpcId: aws.String("vpc-new")}}, nil
		},
		modifyVpc: func(_ context.Context, _ *ec2.ModifyVpcAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error) {
			modifyCalls++
			return &ec2.ModifyVpcAttributeOutput{}, nil
		},
	}

	res := &InitResult{PrivateSubnetIDs: map[string]string{}}
	if err := newServiceWithEC2(f).initVPC(context.Background(), res); err != nil {
		t.Fatalf("initVPC: %v", err)
	}
	if res.VPCID != "vpc-new" {
		t.Errorf("VPCID=%q, want vpc-new", res.VPCID)
	}
	if createCalls != 1 {
		t.Errorf("CreateVpc called %d times, want 1", createCalls)
	}
	if modifyCalls != 2 {
		t.Errorf("ModifyVpcAttribute called %d times, want 2", modifyCalls)
	}
}

func TestInitVPC_PropagatesDescribeError(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	f := &fakeEC2{
		describeVpcs: func(_ context.Context, _ *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
			return nil, want
		},
	}
	err := newServiceWithEC2(f).initVPC(context.Background(), &InitResult{})
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped %v, got %v", want, err)
	}
}
