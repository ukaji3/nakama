package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// Instance category values used in InstanceSummary.Type and the
// `nakama aws list` TYPE column.
const (
	instanceKindVSCode = "vscode"
	instanceKindNAT    = "nat"
	// instanceKindBare marks instances created with --bare: no
	// code-server, no CloudFront distribution (Issue #52).
	instanceKindBare = "bare"
)

// InstanceSummary is one row of `nakama aws list`.
type InstanceSummary struct {
	Name               string `json:"name"`
	Type               string `json:"type"`
	InstanceID         string `json:"instance_id"`
	State              string `json:"state"`
	InstanceType       string `json:"instance_type"`
	DistributionDomain string `json:"distribution_domain,omitempty"`
}

// ListInstances returns every nakama-managed code-server EC2 instance
// (any state other than terminated/shutting-down). When showNAT is true,
// the shared NAT instance is appended as an additional row (Type "nat")
// so its power state (running/stopped) can be inspected alongside the
// code-server instances.
func (s *Service) ListInstances(ctx context.Context, showNAT bool) ([]InstanceSummary, error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagging.KeyManaged), Values: []string{tagging.ValueManagedTrue}},
			{Name: aws.String("tag:" + tagging.KeyComponent), Values: []string{tagging.ComponentEC2}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeInstances: %w", err)
	}

	var summaries []InstanceSummary
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			tags := awshelper.EC2MapTags(i.Tags)
			name := tags[tagging.KeyName]
			id := ""
			if i.InstanceId != nil {
				id = *i.InstanceId
			}
			state := ""
			if i.State != nil {
				state = string(i.State.Name)
			}
			summary := InstanceSummary{
				Name:         name,
				Type:         instanceKindVSCode,
				InstanceID:   id,
				State:        state,
				InstanceType: string(i.InstanceType),
			}
			if name != "" {
				dom, err := s.ssmGetOptional(ctx, ssmDistributionDomainPath(name))
				if err != nil {
					return nil, err
				}
				summary.DistributionDomain = dom
				kind, err := s.ssmGetOptional(ctx, ssmKindPath(name))
				if err != nil {
					return nil, err
				}
				if kind == instanceKindBare {
					summary.Type = instanceKindBare
				}
			}
			summaries = append(summaries, summary)
		}
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })

	if showNAT {
		nat, err := s.findNATInstance(ctx, "")
		if err != nil {
			return nil, err
		}
		if nat.InstanceID != "" {
			summaries = append(summaries, InstanceSummary{
				Name:         naming.NATInstance(),
				Type:         instanceKindNAT,
				InstanceID:   nat.InstanceID,
				State:        string(nat.State),
				InstanceType: string(nat.InstanceType),
			})
		}
	}
	return summaries, nil
}

// InstanceInfo is the detail view emitted by `nakama aws info`.
type InstanceInfo struct {
	Name               string `json:"name"`
	InstanceID         string `json:"instance_id"`
	State              string `json:"state"`
	InstanceType       string `json:"instance_type,omitempty"`
	ENIID              string `json:"eni_id,omitempty"`
	SecurityGroupID    string `json:"security_group_id,omitempty"`
	VPCOriginID        string `json:"vpc_origin_id,omitempty"`
	DistributionID     string `json:"distribution_id,omitempty"`
	DistributionDomain string `json:"distribution_domain,omitempty"`
	URL                string `json:"url,omitempty"`
	CodeServerVersion  string `json:"code_server_version,omitempty"`
	Password           string `json:"password,omitempty"` // populated only when showPassword=true
	// Kind is "vscode" (the default) or "bare" (Issue #52).
	Kind string `json:"kind,omitempty"`
}

// GetInstanceInfo returns the consolidated state of a nakama instance,
// including distribution domain and (optionally) the code-server
// password decrypted from SSM.
func (s *Service) GetInstanceInfo(ctx context.Context, name string, showPassword bool) (*InstanceInfo, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}

	info := &InstanceInfo{Name: name}

	for _, f := range []struct {
		dst  *string
		path string
	}{
		{&info.InstanceID, ssmInstanceIDPath(name)},
		{&info.ENIID, ssmENIIDPath(name)},
		{&info.SecurityGroupID, ssmSGIDPath(name)},
		{&info.VPCOriginID, ssmVPCOriginIDPath(name)},
		{&info.DistributionID, ssmDistributionIDPath(name)},
		{&info.DistributionDomain, ssmDistributionDomainPath(name)},
		{&info.CodeServerVersion, ssmVersionPath(name)},
		{&info.Kind, ssmKindPath(name)},
	} {
		v, err := s.ssmGetOptional(ctx, f.path)
		if err != nil {
			return nil, err
		}
		*f.dst = v
	}
	if info.DistributionDomain != "" {
		info.URL = "https://" + info.DistributionDomain
	}
	if info.Kind == "" {
		info.Kind = instanceKindVSCode
	}

	if info.InstanceID == "" {
		return nil, fmt.Errorf("nakama instance %q not found (no /nakama/%s/meta/instance-id)", name, name)
	}

	st, itype, err := s.fetchInstanceStateAndType(ctx, info.InstanceID)
	if err != nil {
		return nil, err
	}
	info.State = st
	info.InstanceType = itype

	if showPassword && info.Kind != instanceKindBare {
		pwd, err := s.ssmGetSecure(ctx, ssmPasswordPath(name))
		if err != nil {
			return nil, fmt.Errorf("read password: %w", err)
		}
		info.Password = pwd
	}

	return info, nil
}

// fetchInstanceStateAndType returns ("","",nil) when the instance is
// not found (e.g. it was terminated externally).
func (s *Service) fetchInstanceStateAndType(ctx context.Context, instanceID string) (string, string, error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		// AWS returns InvalidInstanceID.NotFound when the instance has
		// been terminated and the EC2 records have been garbage-collected.
		if awshelper.IsAPIError(err, "InvalidInstanceID.NotFound") {
			return "", "", nil
		}
		return "", "", fmt.Errorf("DescribeInstances: %w", err)
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return "", "", nil
	}
	i := out.Reservations[0].Instances[0]
	st := ""
	if i.State != nil {
		st = string(i.State.Name)
	}
	return st, string(i.InstanceType), nil
}

// ssmGetOptional returns the value of an SSM parameter, or "" when
// the parameter does not exist. Unlike the historical behavior, any
// other failure (authorization, throttling, network) is returned as
// an error: treating those as "missing" made DeleteInstance skip
// resources and report success during SSM outages.
func (s *Service) ssmGetOptional(ctx context.Context, name string) (string, error) {
	out, err := s.clients.SSM.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(name),
	})
	if err != nil {
		if awshelper.IsAPIError(err, "ParameterNotFound") {
			return "", nil
		}
		return "", fmt.Errorf("GetParameter(%s): %w", name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", nil
	}
	return *out.Parameter.Value, nil
}

// ssmGetSecure retrieves a SecureString parameter, returning an error
// (rather than empty string) so that the caller can distinguish a
// missing password from a successful empty string.
func (s *Service) ssmGetSecure(ctx context.Context, name string) (string, error) {
	out, err := s.clients.SSM.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", errors.New("nil parameter value")
	}
	return *out.Parameter.Value, nil
}
