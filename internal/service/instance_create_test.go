package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

// describeStep models one programmed DescribeInstances response.
// Either err is set (returned as-is) or state is set (wrapped in an
// otherwise-empty Reservations/Instances slice).
type describeStep struct {
	err   error
	state ec2types.InstanceStateName
}

// fakeEC2DescribeSeq is a minimal awsx.EC2API fake that returns a
// pre-programmed sequence of DescribeInstances responses. Only
// DescribeInstances is implemented; everything else inherits the
// nil embedded interface and would panic if called.
type fakeEC2DescribeSeq struct {
	awsx.EC2API
	calls    int
	sequence []describeStep
}

func (f *fakeEC2DescribeSeq) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.calls >= len(f.sequence) {
		return nil, errors.New("DescribeInstances called more times than the test programmed")
	}
	step := f.sequence[f.calls]
	f.calls++
	if step.err != nil {
		return nil, step.err
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{
				InstanceId: aws.String("i-test"),
				State:      &ec2types.InstanceState{Name: step.state},
			}},
		}},
	}, nil
}

// notFound returns an AWS API error whose code matches the one EC2
// emits when an instance ID is not yet visible after RunInstances.
func notFound() error {
	return &smithy.GenericAPIError{
		Code:    "InvalidInstanceID.NotFound",
		Message: "The instance ID 'i-test' does not exist",
		Fault:   smithy.FaultClient,
	}
}

// invokeUntilDone repeatedly calls predicate until it returns done=true,
// an error, or the step budget is exhausted. It does NOT sleep, so it
// keeps unit tests fast (the production code uses awshelper.Poll for
// the interval).
func invokeUntilDone(t *testing.T, predicate func(context.Context) (bool, error), maxSteps int) error {
	t.Helper()
	for i := 0; i < maxSteps; i++ {
		done, err := predicate(context.Background())
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	t.Fatalf("predicate did not converge within %d steps", maxSteps)
	return nil
}

func TestCheckInstanceRunning_TolerantOfNotFound(t *testing.T) {
	t.Parallel()
	f := &fakeEC2DescribeSeq{
		sequence: []describeStep{
			{err: notFound()},
			{state: ec2types.InstanceStateNamePending},
			{state: ec2types.InstanceStateNameRunning},
		},
	}
	s := newServiceWithEC2(f)
	if err := invokeUntilDone(t, func(ctx context.Context) (bool, error) {
		return s.checkInstanceRunning(ctx, "i-test")
	}, 5); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if f.calls != 3 {
		t.Errorf("DescribeInstances calls = %d, want 3", f.calls)
	}
}

func TestCheckInstanceRunning_ImmediatelyRunning(t *testing.T) {
	t.Parallel()
	f := &fakeEC2DescribeSeq{
		sequence: []describeStep{
			{state: ec2types.InstanceStateNameRunning},
		},
	}
	s := newServiceWithEC2(f)
	done, err := s.checkInstanceRunning(context.Background(), "i-test")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if !done {
		t.Errorf("expected done=true on running state")
	}
}

func TestCheckInstanceRunning_TerminalStateFails(t *testing.T) {
	t.Parallel()
	cases := []ec2types.InstanceStateName{
		ec2types.InstanceStateNameTerminated,
		ec2types.InstanceStateNameShuttingDown,
	}
	for _, st := range cases {
		st := st
		t.Run(string(st), func(t *testing.T) {
			t.Parallel()
			f := &fakeEC2DescribeSeq{sequence: []describeStep{{state: st}}}
			s := newServiceWithEC2(f)
			_, err := s.checkInstanceRunning(context.Background(), "i-test")
			if err == nil {
				t.Fatalf("expected error for state %s, got nil", st)
			}
			if !strings.Contains(err.Error(), "terminal state") {
				t.Errorf("error %q should mention 'terminal state'", err.Error())
			}
		})
	}
}

func TestCheckInstanceRunning_PropagatesUnknownError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	f := &fakeEC2DescribeSeq{sequence: []describeStep{{err: sentinel}}}
	s := newServiceWithEC2(f)
	_, err := s.checkInstanceRunning(context.Background(), "i-test")
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped %v, got %v", sentinel, err)
	}
}

func TestCheckInstanceRunning_PendingStateContinues(t *testing.T) {
	t.Parallel()
	f := &fakeEC2DescribeSeq{sequence: []describeStep{{state: ec2types.InstanceStateNamePending}}}
	s := newServiceWithEC2(f)
	done, err := s.checkInstanceRunning(context.Background(), "i-test")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if done {
		t.Errorf("expected done=false on pending state")
	}
}
