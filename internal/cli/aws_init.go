package cli

import (
	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create or reuse the shared nakama base infrastructure (VPC, subnets, NAT instance, WAF)",
		Long: `Creates or reuses (idempotent) the shared nakama base infrastructure:

  - VPC ` + "(10.20.0.0/16)" + `
  - Internet Gateway (attached to the VPC)
  - Public subnet ` + "(10.20.0.0/24)" + ` in ap-northeast-1a
  - Private subnets in ap-northeast-1a / ap-northeast-1c
  - IAM instance role + instance profile
  - NAT instance in the public subnet (` + "`t4g.nano`" + `, started; power state
    follows the code-server lifecycle to avoid a fixed hourly cost)
  - Public / private route tables and subnet associations
    (private egress -> NAT instance ENI)
  - Legacy NAT Gateway + its EIP are deleted/released if present
    (migration from the pre-NAT-instance topology)
  - WAFv2 IP set (` + "`nakama-allowed-ips`" + `, empty)
  - WAFv2 web ACL (` + "`nakama-waf`" + `, default-block + IP-set allow rule)
  - SSM markers under /nakama/_meta/*

All resources are tagged with nakama:managed=true. Existing
nakama-managed resources (identified by tags / name) are reused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)

			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			svc := service.NewService(clients)

			res, err := svc.Init(ctx)
			if err != nil {
				return err
			}

			fallback := initSummary(res)
			return renderOutput(cmd.OutOrStdout(), global.Output, res, fallback)
		},
	}
}

// initSummary renders a human-readable summary of an InitResult.
func initSummary(r *service.InitResult) string {
	s := "nakama init complete\n" +
		"  vpc                : " + r.VPCID + "\n" +
		"  internet gateway   : " + r.InternetGatewayID + "\n" +
		"  public subnet      : " + r.PublicSubnetID + "\n" +
		"  private subnets    : " + joinMapValues(r.PrivateSubnetIDs) + "\n" +
		"  nat instance       : " + r.NATInstanceID + "\n" +
		"  nat instance eni   : " + r.NATInstanceENIID + "\n" +
		"  nat instance sg    : " + r.NATInstanceSGID + "\n" +
		"  public route table : " + r.PublicRouteTableID + "\n" +
		"  private route table: " + r.PrivateRouteTableID + "\n" +
		"  waf web acl        : " + r.WAFWebACLARN + "\n" +
		"  waf ip set         : " + r.WAFIPSetARN + "\n" +
		"  iam role           : " + r.InstanceRoleARN + "\n" +
		"  iam instance prof. : " + r.InstanceProfileARN
	if r.DeletedNATGatewayID != "" {
		s += "\n  deleted legacy natgw: " + r.DeletedNATGatewayID
	}
	if r.ReleasedNATGatewayEIPID != "" {
		s += "\n  released legacy eip : " + r.ReleasedNATGatewayEIPID
	}
	return s
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
