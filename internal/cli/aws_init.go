package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSInitCommand() *cobra.Command {
	var natMode string
	var yes bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create or reuse the shared nakama base infrastructure (VPC, subnets, NAT, WAF)",
		Long: `Creates or reuses (idempotent) the shared nakama base infrastructure:

  - VPC ` + "(10.20.0.0/16)" + `
  - Internet Gateway (attached to the VPC)
  - Public subnet ` + "(10.20.0.0/24)" + ` in ap-northeast-1a
  - Private subnets in ap-northeast-1a / ap-northeast-1c
  - IAM instance role + instance profile
  - The NAT the private subnets reach the internet through (--nat):
      gateway   a NAT gateway with an Elastic IP in the public subnet.
                AWS runs it: no operating system to patch or break into.
                Always on, so it is charged hourly while it exists (also
                when every instance is stopped), plus per GB it carries
                and the Elastic IP.
      instance  a NAT instance (` + "`t4g.nano`" + `) in the public subnet whose
                power follows the code-server instances, so it costs
                almost nothing while they are stopped. nakama runs its
                operating system (updated on its first boot and when init
                repairs it).
  - Public / private route tables and subnet associations
    (private egress -> the NAT above)
  - The other NAT is removed after the route moves: the NAT gateway is
    deleted and its Elastic IP released, or the NAT instance terminated
    and its security group deleted (its IAM role is kept)
  - WAFv2 IP set (` + "`nakama-allowed-ips`" + `, empty)
  - WAFv2 web ACL (` + "`nakama-waf`" + `, default-block + IP-set allow rule),
    used by the instances given CloudFront (` + "`nakama aws cloudfront add`" + `)
  - SSM markers under /nakama/_meta/* (including the NAT mode)

Without --nat, init keeps the mode it recorded. An environment set up
before the NAT gateway option keeps the NAT instance, and a new one gets
the NAT gateway. Changing the mode of an existing environment asks first
(--yes skips the question): the new NAT is built and checked, the
private route is repointed (connections through the old NAT, including
connect / ssh / tunnel sessions, drop at that moment; connect and tunnel
reconnect), and the old NAT is removed.

All resources are tagged with nakama:managed=true. Existing
nakama-managed resources (identified by tags / name) are reused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if natMode != "" {
				if err := service.ValidateNATMode(natMode); err != nil {
					return fmt.Errorf("--nat: %w", err)
				}
			}
			ctx := withCancel(cmd)

			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			svc := service.NewService(clients)

			plan, err := svc.PlanInit(ctx, natMode)
			if err != nil {
				return err
			}
			switch {
			case plan.Switch && !yes:
				if _, err := io.WriteString(cmd.OutOrStdout(), initSwitchPrompt(plan)); err != nil {
					return err
				}
				ok, err := readYesNo()
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted")
				}
			case plan.NewEnvironment && plan.NATMode == service.NATModeGateway:
				_, _ = io.WriteString(cmd.ErrOrStderr(), initNATGatewayCostNote)
			}

			res, err := svc.Init(ctx, service.InitInput{NATMode: plan.NATMode})
			if err != nil {
				return err
			}

			fallback := initSummary(res)
			return renderOutput(cmd.OutOrStdout(), global.Output, res, fallback)
		},
	}
	cmd.Flags().StringVar(&natMode, "nat", "",
		`NAT for the private subnets' egress: "gateway" (NAT gateway, always on and charged hourly) `+
			`or "instance" (NAT instance, powered with the code-server instances). `+
			`Default: the recorded mode; "instance" for an environment set up before this option, "gateway" for a new one`)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt when the NAT mode changes")
	return cmd
}

// initNATGatewayCostNote is printed (to stderr) when init sets up a new
// environment with the NAT gateway, the default.
const initNATGatewayCostNote = "note: the private subnets reach the internet through a NAT gateway " +
	"(the default for a new environment). It is charged hourly while it exists, also when every instance is " +
	"stopped, plus per GB it carries and its Elastic IP. `nakama aws init --nat instance` uses the NAT instance " +
	"instead, which costs almost nothing while the instances are stopped.\n"

// initSwitchPrompt explains a change of the NAT mode and asks for a y/N.
// Each mode names exactly what init will do; a mode this function does
// not know (PlanInit validates it, so only a programming error gets
// here) says so plainly instead of describing either NAT.
func initSwitchPrompt(p *service.InitPlan) string {
	var b strings.Builder
	switch p.NATMode {
	case service.NATModeGateway:
		b.WriteString("nakama aws init will move the egress of the private subnets from the NAT instance to a NAT gateway:\n" +
			"  - create a NAT gateway and an Elastic IP in the public subnet, and wait until it is available\n" +
			"  - repoint the private default route to the NAT gateway\n" +
			"  - terminate the NAT instance and delete its security group (its IAM role and instance profile are kept)\n" +
			"  The NAT gateway is charged hourly while it exists, also when every instance is stopped,\n" +
			"  plus per GB it carries (connect / tunnel traffic included) and its Elastic IP.\n")
	case service.NATModeInstance:
		b.WriteString("nakama aws init will move the egress of the private subnets from the NAT gateway to the NAT instance:\n" +
			"  - create the NAT instance (t4g.nano) in the public subnet and check that it forwards\n" +
			"  - repoint the private default route to the NAT instance\n" +
			"  - delete the NAT gateway and release its Elastic IP (the egress IP address changes)\n" +
			"  The NAT instance then runs only while a code-server instance runs; nakama runs its operating system.\n")
	default:
		fmt.Fprintf(&b, "nakama aws init will change the egress mode of the private subnets from %q to %q,\n"+
			"  which this version of nakama cannot describe.\n", p.From, p.NATMode)
	}
	if len(p.ActiveInstances) > 0 {
		fmt.Fprintf(&b, "Running instances: %s.\n"+
			"  Their outbound connections and the connect / ssh / tunnel sessions to them drop when the route is\n"+
			"  repointed (connect and tunnel reconnect).\n", strings.Join(p.ActiveInstances, ", "))
	}
	b.WriteString("Continue? [y/N]: ")
	return b.String()
}

// initSummary renders a human-readable summary of an InitResult.
func initSummary(r *service.InitResult) string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "\n  %-19s: %s", label, value)
	}
	b.WriteString("nakama init complete")
	line("vpc", r.VPCID)
	line("internet gateway", r.InternetGatewayID)
	line("public subnet", r.PublicSubnetID)
	line("private subnets", joinMapValues(r.PrivateSubnetIDs))
	line("nat mode", r.NATMode)
	switch r.NATMode {
	case service.NATModeGateway:
		line("nat gateway", r.NATGatewayID)
		line("nat gateway eip", r.NATGatewayEIPAllocationID)
		line("egress ip", r.NATGatewayPublicIP)
	default:
		line("nat instance", r.NATInstanceID)
		line("nat instance eni", r.NATInstanceENIID)
		line("nat instance sg", r.NATInstanceSGID)
		line("nat forwarding", r.NATForwarding)
	}
	line("public route table", r.PublicRouteTableID)
	line("private route table", r.PrivateRouteTableID)
	line("waf web acl", r.WAFWebACLARN)
	line("waf ip set", r.WAFIPSetARN)
	line("iam role", r.InstanceRoleARN)
	line("iam instance prof.", r.InstanceProfileARN)
	if r.NATInstanceRoleARN != "" {
		line("nat iam role", r.NATInstanceRoleARN)
	}
	if r.NATInstanceProfileARN != "" {
		line("nat instance prof.", r.NATInstanceProfileARN)
	}
	for _, removed := range []struct{ label, value string }{
		{"deleted natgw", r.DeletedNATGatewayID},
		{"released eip", r.ReleasedNATGatewayEIPID},
		{"terminated nat", r.TerminatedNATInstanceID},
		{"deleted nat sg", r.DeletedNATInstanceSGID},
	} {
		if removed.value != "" {
			line(removed.label, removed.value)
		}
	}
	return b.String()
}

func joinMapValues(m map[string]string) string {
	out := ""
	for k, v := range m {
		if out != "" {
			out += ", "
		}
		out += k + "=" + v
	}
	return out
}
