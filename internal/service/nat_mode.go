package service

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// Egress modes (ADR 0026). The private subnets reach the internet
// through exactly one of them; `nakama aws init --nat <mode>` chooses.
const (
	// NATModeInstance routes egress through the NAT instance (ADR 0007):
	// a t4g.nano in the public subnet whose power follows the
	// code-server instances, so it costs nothing while they are stopped.
	NATModeInstance = "instance"
	// NATModeGateway routes egress through a zonal NAT gateway with an
	// Elastic IP in the public subnet. AWS runs it, so there is no
	// operating system to patch or to break into; it is always on and
	// charged hourly while it exists.
	NATModeGateway = "gateway"
)

// defaultNATModeForNewEnvironment is the mode init sets up when no
// nakama VPC exists yet and --nat is not given. An existing environment
// keeps its mode, and one set up before ADR 0026 (no record) stays on
// the NAT instance.
const defaultNATModeForNewEnvironment = NATModeGateway

// ValidateNATMode reports whether mode is one of the egress modes.
func ValidateNATMode(mode string) error {
	switch mode {
	case NATModeInstance, NATModeGateway:
		return nil
	default:
		return fmt.Errorf("unknown NAT mode %q (want %q or %q)", mode, NATModeInstance, NATModeGateway)
	}
}

// recordedNATMode returns the egress mode the last init recorded under
// /nakama/_meta/nat-mode, or "" when nothing is recorded. A value this
// version does not know is an error rather than a guess: managing the
// wrong NAT could cut the egress of every instance.
func (s *Service) recordedNATMode(ctx context.Context) (string, error) {
	v, err := s.ssmGetOptional(ctx, naming.SSMMetaNATMode())
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", nil
	}
	if err := ValidateNATMode(v); err != nil {
		return "", fmt.Errorf("%s holds %q, which this nakama does not know (was it written by a newer version?): %w",
			naming.SSMMetaNATMode(), v, err)
	}
	return v, nil
}

// writeNATMode records the egress mode init sets up. init writes it
// before it changes anything, so a run that stops halfway is finished
// in the same mode by the next init.
func (s *Service) writeNATMode(ctx context.Context, mode string) error {
	if _, err := s.clients.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(naming.SSMMetaNATMode()),
		Value:     aws.String(mode),
		Type:      ssmtypes.ParameterTypeString,
		Overwrite: aws.Bool(true),
	}); err != nil {
		return fmt.Errorf("PutParameter(%s): %w", naming.SSMMetaNATMode(), err)
	}
	return nil
}

// InitPlan is what `nakama aws init` is about to do with the egress. It
// is read before anything changes, so the CLI can ask before a switch.
type InitPlan struct {
	// NATMode is the egress mode init sets up.
	NATMode string `json:"nat_mode"`
	// RecordedNATMode is the mode the last init recorded. It is empty for
	// a new environment and for one set up before ADR 0026.
	RecordedNATMode string `json:"recorded_nat_mode,omitempty"`
	// NewEnvironment reports that no nakama VPC exists yet.
	NewEnvironment bool `json:"new_environment"`
	// Switch reports that an existing environment changes its egress
	// mode from From to NATMode.
	Switch bool   `json:"switch"`
	From   string `json:"from,omitempty"`
	// ActiveInstances lists the code-server instances that are pending
	// or running when Switch is set: their outbound connections, and the
	// connect / ssh / tunnel sessions to them, drop when the private
	// route is repointed.
	ActiveInstances []string `json:"active_instances,omitempty"`
}

// PlanInit decides the egress mode for `nakama aws init` without
// changing anything. requested is the --nat value ("" when not given):
//
//   - --nat wins.
//   - Otherwise the recorded mode is kept.
//   - Otherwise an existing environment (the nakama VPC exists) keeps
//     the NAT instance: it was set up before ADR 0026, and installing a
//     new nakama must change neither its routes nor its bill.
//   - Otherwise (a new environment) the NAT gateway is the default.
func (s *Service) PlanInit(ctx context.Context, requested string) (*InitPlan, error) {
	p, err := s.planNATMode(ctx, requested)
	if err != nil {
		return nil, err
	}
	if p.Switch {
		if p.ActiveInstances, err = s.activeInstanceNames(ctx); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// planNATMode is PlanInit without the instance listing; Init uses it
// when it is called without a mode.
func (s *Service) planNATMode(ctx context.Context, requested string) (*InitPlan, error) {
	if requested != "" {
		if err := ValidateNATMode(requested); err != nil {
			return nil, err
		}
	}
	recorded, err := s.recordedNATMode(ctx)
	if err != nil {
		return nil, err
	}
	vpcID, err := s.findVPCByTags(ctx)
	if err != nil {
		return nil, err
	}
	p := &InitPlan{RecordedNATMode: recorded, NewEnvironment: vpcID == ""}

	current := recorded
	if current == "" && !p.NewEnvironment {
		current = NATModeInstance
	}
	switch {
	case requested != "":
		p.NATMode = requested
	case current != "":
		p.NATMode = current
	default:
		p.NATMode = defaultNATModeForNewEnvironment
	}
	if !p.NewEnvironment && current != p.NATMode {
		p.Switch = true
		p.From = current
	}
	return p, nil
}

// activeInstanceNames returns the names of the nakama code-server
// instances that are pending or running, sorted.
func (s *Service) activeInstanceNames(ctx context.Context) ([]string, error) {
	out, err := s.clients.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagging.KeyManaged), Values: []string{tagging.ValueManagedTrue}},
			{Name: aws.String("tag:" + tagging.KeyComponent), Values: []string{tagging.ComponentEC2}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeInstances(active code-server): %w", err)
	}
	var names []string
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			name := awshelper.EC2MapTags(i.Tags)[tagging.KeyName]
			if name == "" {
				name = aws.ToString(i.InstanceId)
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
