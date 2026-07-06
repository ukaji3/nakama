package service

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// fakeListEC2 answers DescribeInstances differently for the code-server
// component (ComponentEC2) and the NAT instance (ComponentNATInstance),
// so ListInstances can be exercised with and without --show-nat.
type fakeListEC2 struct {
	awsx.EC2API

	codeServers []ec2types.Instance // returned for the ComponentEC2 filter

	natID    string
	natState ec2types.InstanceStateName
	natType  ec2types.InstanceType
}

func (f *fakeListEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	comp := ""
	for _, flt := range in.Filters {
		if flt.Name != nil && *flt.Name == "tag:"+tagging.KeyComponent && len(flt.Values) > 0 {
			comp = flt.Values[0]
		}
	}
	switch comp {
	case tagging.ComponentEC2:
		if len(f.codeServers) == 0 {
			return &ec2.DescribeInstancesOutput{}, nil
		}
		return &ec2.DescribeInstancesOutput{
			Reservations: []ec2types.Reservation{{Instances: f.codeServers}},
		}, nil
	case tagging.ComponentNATInstance:
		if f.natID == "" {
			return &ec2.DescribeInstancesOutput{}, nil
		}
		return &ec2.DescribeInstancesOutput{
			Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
				InstanceId:   aws.String(f.natID),
				InstanceType: f.natType,
				State:        &ec2types.InstanceState{Name: f.natState},
				NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
					NetworkInterfaceId: aws.String("eni-" + f.natID),
					Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
				}},
			}}}},
		}, nil
	default:
		return &ec2.DescribeInstancesOutput{}, nil
	}
}

// fakeListSSM makes ssmGetOptional resolve to "" so ListInstances does
// not require a real Parameter Store for the distribution domain lookup.
type fakeListSSM struct{ awsx.SSMAPI }

func (fakeListSSM) GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return nil, errors.New("parameter not found")
}

func newServiceForList(ec2api awsx.EC2API) *Service {
	return &Service{clients: &awsx.Clients{EC2: ec2api, SSM: fakeListSSM{}}}
}

func codeServerInstance(name, id, itype string, state ec2types.InstanceStateName) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: ec2types.InstanceType(itype),
		State:        &ec2types.InstanceState{Name: state},
		Tags:         []ec2types.Tag{{Key: aws.String(tagging.KeyName), Value: aws.String(name)}},
	}
}

func TestListInstances_VSCodeRowsSortedAndTyped(t *testing.T) {
	t.Parallel()
	f := &fakeListEC2{
		codeServers: []ec2types.Instance{
			codeServerInstance("dev02", "i-2", "t3.large", ec2types.InstanceStateNameRunning),
			codeServerInstance("dev01", "i-1", "t3.medium", ec2types.InstanceStateNameStopped),
		},
		// NAT exists but must NOT appear when showNAT is false.
		natID: "i-nat", natState: ec2types.InstanceStateNameRunning, natType: "t4g.nano",
	}
	s := newServiceForList(f)

	rows, err := s.ListInstances(context.Background(), false)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "dev01" || rows[1].Name != "dev02" {
		t.Fatalf("rows not sorted by name: %q, %q", rows[0].Name, rows[1].Name)
	}
	for _, r := range rows {
		if r.Type != instanceKindVSCode {
			t.Fatalf("code-server row %q has Type %q, want %q", r.Name, r.Type, instanceKindVSCode)
		}
	}
	if rows[0].InstanceType != "t3.medium" || rows[0].State != "stopped" {
		t.Fatalf("dev01 row unexpected: %+v", rows[0])
	}
}

func TestListInstances_ShowNATAppendsNATRow(t *testing.T) {
	t.Parallel()
	f := &fakeListEC2{
		codeServers: []ec2types.Instance{
			codeServerInstance("dev01", "i-1", "t3.medium", ec2types.InstanceStateNameRunning),
		},
		natID: "i-nat", natState: ec2types.InstanceStateNameStopped, natType: "t4g.nano",
	}
	s := newServiceForList(f)

	rows, err := s.ListInstances(context.Background(), true)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (1 vscode + 1 nat), got %d: %+v", len(rows), rows)
	}
	nat := rows[len(rows)-1] // NAT row is appended after the sorted vscode rows
	if nat.Type != instanceKindNAT {
		t.Fatalf("NAT row Type = %q, want %q", nat.Type, instanceKindNAT)
	}
	if nat.Name != naming.NATInstance() {
		t.Fatalf("NAT row Name = %q, want %q", nat.Name, naming.NATInstance())
	}
	if nat.State != "stopped" {
		t.Fatalf("NAT row State = %q, want stopped", nat.State)
	}
	if nat.InstanceType != "t4g.nano" {
		t.Fatalf("NAT row InstanceType = %q, want t4g.nano", nat.InstanceType)
	}
	if nat.InstanceID != "i-nat" {
		t.Fatalf("NAT row InstanceID = %q, want i-nat", nat.InstanceID)
	}
}

func TestListInstances_ShowNATNoNATInstance(t *testing.T) {
	t.Parallel()
	f := &fakeListEC2{
		codeServers: []ec2types.Instance{
			codeServerInstance("dev01", "i-1", "t3.medium", ec2types.InstanceStateNameRunning),
		},
		natID: "", // NAT instance not provisioned yet
	}
	s := newServiceForList(f)

	rows, err := s.ListInstances(context.Background(), true)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row (no NAT row when NAT absent), got %d: %+v", len(rows), rows)
	}
	if rows[0].Type != instanceKindVSCode {
		t.Fatalf("row Type = %q, want %q", rows[0].Type, instanceKindVSCode)
	}
}
