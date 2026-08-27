package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EditInstanceTypeInput specifies a change of the EC2 instance type.
type EditInstanceTypeInput struct {
	Name    string
	NewType string
}

// EditInstanceTypeResult reports the outcome of an instance-type edit.
type EditInstanceTypeResult struct {
	InstanceID string `json:"instance_id"`
	OldType    string `json:"old_type"`
	NewType    string `json:"new_type"`
	Restarted  bool   `json:"restarted"` // true if nakama stopped+started a running instance
	NoOp       bool   `json:"no_op"`     // true if the instance already had NewType
}

// EditInstanceType changes the EC2 instance type. AWS requires the
// instance to be stopped for ModifyInstanceAttribute(InstanceType), so
// a running instance is stopped, modified, and started again to restore
// its prior state. The target type is validated up front so an invalid
// type fails immediately without stopping the instance.
func (s *Service) EditInstanceType(ctx context.Context, in EditInstanceTypeInput) (*EditInstanceTypeResult, error) {
	if err := ValidateInstanceName(in.Name); err != nil {
		return nil, err
	}
	if in.NewType == "" {
		return nil, errors.New("--instance-type is required")
	}

	instanceID, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if err != nil {
		return nil, err
	}
	if instanceID == "" {
		return nil, fmt.Errorf("nakama instance %q not found", in.Name)
	}

	// Preflight: validate the target type exists and resolve the CPU
	// architecture it requires. This fails immediately, before the
	// instance is stopped.
	newArch, err := s.resolveInstanceArchitecture(ctx, in.NewType)
	if err != nil {
		return nil, err
	}

	state, curType, err := s.fetchInstanceStateAndType(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if state == "" {
		return nil, fmt.Errorf("instance %s not found (terminated?)", instanceID)
	}

	res := &EditInstanceTypeResult{InstanceID: instanceID, OldType: curType, NewType: in.NewType}
	if curType == in.NewType {
		res.NoOp = true
		return res, nil
	}

	// Guard: nakama does not support changing an instance's CPU
	// architecture in place. The root volume holds architecture-specific
	// OS and code-server binaries, and EC2 rejects a type whose
	// architecture does not match the AMI the volume was built from.
	curArch, err := s.resolveInstanceArchitecture(ctx, curType)
	if err != nil {
		return nil, fmt.Errorf("resolve current architecture (%s): %w", curType, err)
	}
	if newArch != curArch {
		return nil, fmt.Errorf(
			"cannot change instance %q architecture in place: current type %s is %s, requested %s is %s; "+
				"delete and recreate the instance to switch architecture",
			in.Name, curType, curArch, in.NewType, newArch)
	}

	wasRunning := state == string(ec2types.InstanceStateNameRunning)
	if state != string(ec2types.InstanceStateNameStopped) {
		slog.Info("instance-type: stopping instance before modify", "instance_id", instanceID, "state", state)
		if _, err := s.clients.EC2.StopInstances(ctx, &ec2.StopInstancesInput{
			InstanceIds: []string{instanceID},
		}); err != nil {
			return nil, fmt.Errorf("StopInstances: %w", err)
		}
		if err := s.waitInstanceStopped(ctx, instanceID); err != nil {
			return nil, fmt.Errorf("wait stopped: %w", err)
		}
	}

	slog.Info("instance-type: modifying", "instance_id", instanceID, "from", curType, "to", in.NewType)
	if _, err := s.clients.EC2.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(instanceID),
		InstanceType: &ec2types.AttributeValue{Value: aws.String(in.NewType)},
	}); err != nil {
		return nil, fmt.Errorf("ModifyInstanceAttribute: %w", err)
	}

	if wasRunning {
		slog.Info("instance-type: restarting instance", "instance_id", instanceID)
		if _, err := s.clients.EC2.StartInstances(ctx, &ec2.StartInstancesInput{
			InstanceIds: []string{instanceID},
		}); err != nil {
			return nil, fmt.Errorf("StartInstances: %w", err)
		}
		if err := s.waitInstanceRunning(ctx, instanceID); err != nil {
			return nil, fmt.Errorf("wait running: %w", err)
		}
		res.Restarted = true
	}

	// Reconcile the shared NAT instance against the final power state.
	// The transient stop during the type change does not stop the NAT
	// instance; only the end state matters.
	if err := s.reconcileNATInstancePowerState(ctx); err != nil {
		slog.Warn("instance-type: NAT instance reconcile failed", "error", err)
	}
	return res, nil
}
