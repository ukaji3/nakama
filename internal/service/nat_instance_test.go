package service

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/tagging"
)

// fakeNATEC2 mocks the EC2 surface used by the NAT-instance reconcile
// logic. DescribeInstances is answered differently depending on whether
// the caller filters by the NAT-instance component, the code-server
// component, or looks up the NAT instance by ID (the wait loops).
type fakeNATEC2 struct {
	*fakeRouteEC2 // unused route methods; embeds EC2API too

	natID    string
	natState ec2types.InstanceStateName

	// activeCodeServerIDs are code-server instances reported as running.
	activeCodeServerIDs []string

	started []string
	stopped []string
}

func newFakeNAT(natID string, natState ec2types.InstanceStateName, activeIDs ...string) *fakeNATEC2 {
	return &fakeNATEC2{
		fakeRouteEC2:        &fakeRouteEC2{},
		natID:               natID,
		natState:            natState,
		activeCodeServerIDs: activeIDs,
	}
}

func filterValue(in *ec2.DescribeInstancesInput, name string) (string, bool) {
	for _, f := range in.Filters {
		if f.Name != nil && *f.Name == name && len(f.Values) > 0 {
			return f.Values[0], true
		}
	}
	return "", false
}

func (f *fakeNATEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	// Lookup by ID (used by wait loops) -> report the NAT instance state.
	if len(in.InstanceIds) > 0 {
		return &ec2.DescribeInstancesOutput{
			Reservations: []ec2types.Reservation{{
				Instances: []ec2types.Instance{{
					InstanceId: aws.String(in.InstanceIds[0]),
					State:      &ec2types.InstanceState{Name: f.natState},
				}},
			}},
		}, nil
	}

	comp, _ := filterValue(in, "tag:"+tagging.KeyComponent)
	switch comp {
	case tagging.ComponentNATInstance:
		if f.natID == "" {
			return &ec2.DescribeInstancesOutput{}, nil
		}
		return &ec2.DescribeInstancesOutput{
			Reservations: []ec2types.Reservation{{
				Instances: []ec2types.Instance{{
					InstanceId: aws.String(f.natID),
					State:      &ec2types.InstanceState{Name: f.natState},
					NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
						NetworkInterfaceId: aws.String("eni-" + f.natID),
						Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
					}},
				}},
			}},
		}, nil
	case tagging.ComponentEC2:
		var instances []ec2types.Instance
		for _, id := range f.activeCodeServerIDs {
			instances = append(instances, ec2types.Instance{
				InstanceId: aws.String(id),
				State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			})
		}
		if len(instances) == 0 {
			return &ec2.DescribeInstancesOutput{}, nil
		}
		return &ec2.DescribeInstancesOutput{
			Reservations: []ec2types.Reservation{{Instances: instances}},
		}, nil
	default:
		return &ec2.DescribeInstancesOutput{}, nil
	}
}

func (f *fakeNATEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.started = append(f.started, in.InstanceIds...)
	f.natState = ec2types.InstanceStateNameRunning
	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeNATEC2) StopInstances(_ context.Context, in *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	f.stopped = append(f.stopped, in.InstanceIds...)
	f.natState = ec2types.InstanceStateNameStopped
	return &ec2.StopInstancesOutput{}, nil
}

func TestReconcileNAT_StartsWhenCodeServerActive(t *testing.T) {
	t.Parallel()
	f := newFakeNAT("i-nat", ec2types.InstanceStateNameStopped, "i-dev01")
	s := newServiceWithEC2(f)

	if err := s.reconcileNATInstancePowerState(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.started) != 1 || f.started[0] != "i-nat" {
		t.Errorf("expected NAT start, got started=%v", f.started)
	}
	if len(f.stopped) != 0 {
		t.Errorf("expected no stop, got stopped=%v", f.stopped)
	}
}

func TestReconcileNAT_StopsWhenNoCodeServerActive(t *testing.T) {
	t.Parallel()
	f := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning) // no active code-server
	s := newServiceWithEC2(f)

	if err := s.reconcileNATInstancePowerState(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.stopped) != 1 || f.stopped[0] != "i-nat" {
		t.Errorf("expected NAT stop, got stopped=%v", f.stopped)
	}
	if len(f.started) != 0 {
		t.Errorf("expected no start, got started=%v", f.started)
	}
}

func TestReconcileNAT_NoopWhenAlreadyStoppedAndIdle(t *testing.T) {
	t.Parallel()
	f := newFakeNAT("i-nat", ec2types.InstanceStateNameStopped)
	s := newServiceWithEC2(f)

	if err := s.reconcileNATInstancePowerState(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.started) != 0 || len(f.stopped) != 0 {
		t.Errorf("expected no power change, got started=%v stopped=%v", f.started, f.stopped)
	}
}

func TestReconcileNAT_NoopWhenAlreadyRunningAndActive(t *testing.T) {
	t.Parallel()
	f := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning, "i-dev01")
	s := newServiceWithEC2(f)

	if err := s.reconcileNATInstancePowerState(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.started) != 0 || len(f.stopped) != 0 {
		t.Errorf("expected no power change, got started=%v stopped=%v", f.started, f.stopped)
	}
}

func TestReconcileNAT_ExcludedInstanceTreatedInactive(t *testing.T) {
	t.Parallel()
	// The only active code-server is the one being stopped; excluding it
	// means the NAT instance should be stopped.
	f := newFakeNAT("i-nat", ec2types.InstanceStateNameRunning, "i-dev01")
	s := newServiceWithEC2(f)

	if err := s.reconcileNATInstancePowerStateExcluding(context.Background(), "i-dev01"); err != nil {
		t.Fatalf("reconcile excluding: %v", err)
	}
	if len(f.stopped) != 1 || f.stopped[0] != "i-nat" {
		t.Errorf("expected NAT stop when only active instance is excluded, got stopped=%v", f.stopped)
	}
}
