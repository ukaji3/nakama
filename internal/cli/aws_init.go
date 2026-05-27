package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// errNotImplemented is returned by every stub command in this scaffold.
// As each command is implemented, the corresponding RunE will replace
// this with a real implementation.
var errNotImplemented = errors.New("not implemented yet (scaffold)")

func newAWSInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create or reuse the shared nakama base infrastructure (VPC, subnets, NAT GW, WAF)",
		Long: `Creates or reuses (idempotent) the shared nakama base infrastructure:

  - VPC ` + "(10.20.0.0/16)" + `
  - Internet Gateway
  - Public subnet ` + "(10.20.0.0/24)" + ` in ap-northeast-1a
  - Private subnets in ap-northeast-1a / ap-northeast-1c
  - NAT Gateway in the public subnet
  - Route tables
  - WAFv2 IP set (` + "`nakama-allowed-ips`" + `, empty)
  - WAFv2 web ACL (` + "`nakama-waf`" + `, default-block)
  - IAM instance profile ` + "(`nakama-instance-profile`)" + `

Existing nakama-managed resources (identified by tags) are reused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
}
