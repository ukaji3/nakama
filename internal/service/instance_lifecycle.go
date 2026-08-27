package service

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// StartInstance starts the EC2 instance backing the named nakama
// instance. The instance metadata is resolved from
// /nakama/<name>/meta/instance-id.
//
// The shared NAT instance is started first so the code-server has
// working egress the moment it boots.
func (s *Service) StartInstance(ctx context.Context, name string) (string, error) {
	if err := ValidateInstanceName(name); err != nil {
		return "", err
	}
	id, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(name))
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("nakama instance %q not found", name)
	}
	if err := s.ensureNATInstanceRunning(ctx); err != nil {
		return "", fmt.Errorf("start NAT instance: %w", err)
	}
	if _, err := s.clients.EC2.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{id},
	}); err != nil {
		return "", fmt.Errorf("StartInstances: %w", err)
	}
	return id, nil
}

// StopInstance stops the EC2 instance backing the named nakama
// instance. The CloudFront distribution remains live; it will
// return 502 to viewers while the EC2 is stopped.
//
// After stopping, the shared NAT instance is reconciled: if no other
// code-server instance remains pending/running, the NAT instance is
// stopped too so it incurs no cost while everything is idle.
func (s *Service) StopInstance(ctx context.Context, name string) (string, error) {
	if err := ValidateInstanceName(name); err != nil {
		return "", err
	}
	id, err := s.ssmGetOptional(ctx, ssmInstanceIDPath(name))
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("nakama instance %q not found", name)
	}
	if _, err := s.clients.EC2.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{id},
	}); err != nil {
		return "", fmt.Errorf("StopInstances: %w", err)
	}
	if err := s.reconcileNATInstancePowerStateExcluding(ctx, id); err != nil {
		return "", fmt.Errorf("reconcile NAT instance: %w", err)
	}
	return id, nil
}
