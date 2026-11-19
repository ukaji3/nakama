package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/ukaji3/nakama/internal/naming"
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

	// natGateways answers DescribeNatGateways (ADR 0026). By ID, the
	// first natGatewayPolls lookups report pending and the later ones
	// natGatewayReady (or the listed state when that is empty).
	natGateways     []ec2types.NatGateway
	natGatewayPolls int
	natGatewayReady ec2types.NatGatewayState
	natGatewayErr   error

	started []string
	stopped []string
}

// natGateway builds a nakama NAT gateway for the fakes.
func natGateway(id string, state ec2types.NatGatewayState, publicIP string) ec2types.NatGateway {
	return ec2types.NatGateway{
		NatGatewayId: aws.String(id),
		State:        state,
		VpcId:        aws.String("vpc-1"),
		SubnetId:     aws.String("subnet-public"),
		Tags:         ec2Tags(tagging.ForShared(tagging.ComponentNATGW, naming.NATGW())),
		NatGatewayAddresses: []ec2types.NatGatewayAddress{{
			AllocationId: aws.String("eipalloc-" + id),
			PublicIp:     aws.String(publicIP),
			IsPrimary:    aws.Bool(true),
		}},
	}
}

// foreignNATGateway is an available NAT gateway that is not nakama's.
func foreignNATGateway(id string) ec2types.NatGateway {
	gw := natGateway(id, ec2types.NatGatewayStateAvailable, "198.51.100.99")
	gw.Tags = ec2Tags(map[string]string{tagging.NameTag: "someone-elses-nat"})
	return gw
}

// filterNATGateways applies the DescribeNatGateways filters to the fake's
// NAT gateways (matchEC2Filters).
func filterNATGateways(gws []ec2types.NatGateway, filters []ec2types.Filter) ([]ec2types.NatGateway, error) {
	var out []ec2types.NatGateway
	for _, gw := range gws {
		ok, err := matchEC2Filters(filters, gw.Tags, map[string]string{"vpc-id": aws.ToString(gw.VpcId)})
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, gw)
		}
	}
	return out, nil
}

func (f *fakeNATEC2) DescribeNatGateways(_ context.Context, in *ec2.DescribeNatGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	if f.natGatewayErr != nil {
		return nil, f.natGatewayErr
	}
	if len(in.NatGatewayIds) > 0 {
		for _, gw := range f.natGateways {
			if aws.ToString(gw.NatGatewayId) != in.NatGatewayIds[0] {
				continue
			}
			if f.natGatewayPolls > 0 {
				f.natGatewayPolls--
				gw.State = ec2types.NatGatewayStatePending
			} else if f.natGatewayReady != "" {
				gw.State = f.natGatewayReady
			}
			return &ec2.DescribeNatGatewaysOutput{NatGateways: []ec2types.NatGateway{gw}}, nil
		}
		return &ec2.DescribeNatGatewaysOutput{}, nil
	}
	gws, err := filterNATGateways(f.natGateways, in.Filter)
	if err != nil {
		return nil, err
	}
	return &ec2.DescribeNatGatewaysOutput{NatGateways: gws}, nil
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

// TestReconcileNAT_NoopWithoutNATInstance covers the gateway mode
// (ADR 0026): without a NAT instance there is nothing to power, even
// when a code-server instance runs, and no NAT gateway is touched.
func TestReconcileNAT_NoopWithoutNATInstance(t *testing.T) {
	t.Parallel()
	f := newFakeNAT("", "", "i-dev01")
	f.natGateways = []ec2types.NatGateway{natGateway("nat-1", ec2types.NatGatewayStateAvailable, "203.0.113.10")}
	s := newServiceWithEC2(f)

	for _, exclude := range []string{"", "i-dev01"} {
		if err := s.reconcileNATInstancePowerStateExcluding(context.Background(), exclude); err != nil {
			t.Fatalf("reconcile (exclude %q): %v", exclude, err)
		}
	}
	if len(f.started) != 0 || len(f.stopped) != 0 {
		t.Errorf("expected no power change, got started=%v stopped=%v", f.started, f.stopped)
	}
}

func TestEnsureEgress_StartsTheNATInstance(t *testing.T) {
	t.Parallel()
	f := newFakeNAT("i-nat", ec2types.InstanceStateNameStopped)
	// A NAT gateway left by a switch in progress does not change the
	// answer: the NAT instance is started, which keeps either route working.
	f.natGateways = []ec2types.NatGateway{natGateway("nat-1", ec2types.NatGatewayStateAvailable, "203.0.113.10")}
	if err := newServiceWithEC2(f).ensureEgress(context.Background()); err != nil {
		t.Fatalf("ensureEgress: %v", err)
	}
	if len(f.started) != 1 || f.started[0] != "i-nat" {
		t.Errorf("expected the NAT instance to start, got started=%v", f.started)
	}
}

func TestEnsureEgress_GatewayMode(t *testing.T) {
	shortNATGatewayWaits(t)
	cases := []struct {
		name    string
		gws     []ec2types.NatGateway
		polls   int
		wantErr []string
	}{
		{name: "available", gws: []ec2types.NatGateway{natGateway("nat-1", ec2types.NatGatewayStateAvailable, "203.0.113.10")}},
		{name: "pending is waited for", gws: []ec2types.NatGateway{natGateway("nat-1", ec2types.NatGatewayStatePending, "203.0.113.10")}, polls: 2},
		{name: "available one is preferred over a failed one", gws: []ec2types.NatGateway{
			natGateway("nat-old", ec2types.NatGatewayStateFailed, ""),
			natGateway("nat-1", ec2types.NatGatewayStateAvailable, "203.0.113.10"),
		}},
		{name: "failed", gws: []ec2types.NatGateway{natGateway("nat-1", ec2types.NatGatewayStateFailed, "")},
			wantErr: []string{"nat-1", "failed", "nakama aws init"}},
		{name: "neither NAT", wantErr: []string{"no nakama NAT instance or NAT gateway", "nakama aws init"}},
		// A NAT gateway that is not nakama's does not count.
		{name: "only a foreign NAT gateway", gws: []ec2types.NatGateway{foreignNATGateway("nat-other")},
			wantErr: []string{"no nakama NAT instance or NAT gateway", "nakama aws init"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeNAT("", "")
			f.natGateways = c.gws
			if c.polls > 0 {
				// Listed as pending; by ID pending for c.polls more
				// lookups, then available.
				f.natGatewayPolls = c.polls
				f.natGatewayReady = ec2types.NatGatewayStateAvailable
				defer func() {
					if f.natGatewayPolls != 0 {
						t.Errorf("the pending NAT gateway was not waited for (%d polls left)", f.natGatewayPolls)
					}
				}()
			}
			err := newServiceWithEC2(f).ensureEgress(context.Background())
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ensureEgress: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("ensureEgress succeeded, want an error")
				}
				for _, want := range c.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error lacks %q: %v", want, err)
					}
				}
			}
			if len(f.started) != 0 || len(f.stopped) != 0 {
				t.Errorf("no instance may be started or stopped in gateway mode: started=%v stopped=%v", f.started, f.stopped)
			}
		})
	}
}

func TestCurrentNATMode(t *testing.T) {
	t.Parallel()
	if got := newServiceWithEC2(newFakeNAT("i-nat", ec2types.InstanceStateNameRunning)).currentNATMode(context.Background()); got != NATModeInstance {
		t.Errorf("with a NAT instance: %q", got)
	}
	gw := newFakeNAT("", "")
	gw.natGateways = []ec2types.NatGateway{natGateway("nat-1", ec2types.NatGatewayStateAvailable, "203.0.113.10")}
	if got := newServiceWithEC2(gw).currentNATMode(context.Background()); got != NATModeGateway {
		t.Errorf("with a NAT gateway: %q", got)
	}
	none := newFakeNAT("", "")
	none.natGatewayErr = errors.New("denied")
	if got := newServiceWithEC2(none).currentNATMode(context.Background()); got != "" {
		t.Errorf("with neither (lookup failed): %q", got)
	}
}

// shortNATGatewayWaits shortens the NAT gateway waits for the test.
func shortNATGatewayWaits(t *testing.T) {
	t.Helper()
	pi, at, dt := natGatewayPollInterval, natGatewayAvailableTimeout, natGatewayDeletedTimeout
	natGatewayPollInterval, natGatewayAvailableTimeout, natGatewayDeletedTimeout = time.Millisecond, 2*time.Second, 2*time.Second
	t.Cleanup(func() { natGatewayPollInterval, natGatewayAvailableTimeout, natGatewayDeletedTimeout = pi, at, dt })
}
