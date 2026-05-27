package service

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// StartInstance starts the EC2 instance backing the named nakama
// instance. The instance metadata is resolved from
// /nakama/<name>/meta/instance-id.
func (s *Service) StartInstance(ctx context.Context, name string) (string, error) {
	if err := ValidateInstanceName(name); err != nil {
		return "", err
	}
	id := s.ssmGetOptional(ctx, ssmInstanceIDPath(name))
	if id == "" {
		return "", fmt.Errorf("nakama instance %q not found", name)
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
func (s *Service) StopInstance(ctx context.Context, name string) (string, error) {
	if err := ValidateInstanceName(name); err != nil {
		return "", err
	}
	id := s.ssmGetOptional(ctx, ssmInstanceIDPath(name))
	if id == "" {
		return "", fmt.Errorf("nakama instance %q not found", name)
	}
	if _, err := s.clients.EC2.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{id},
	}); err != nil {
		return "", fmt.Errorf("StopInstances: %w", err)
	}
	return id, nil
}
