package cli

import "github.com/spf13/cobra"

type createFlags struct {
	name              string
	instanceType      string
	codeServerVersion string
}

func newAWSCreateCommand() *cobra.Command {
	f := &createFlags{}
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a new code-server EC2 instance and CloudFront distribution",
		Long: `Provisions:

  - A dedicated ENI in the private subnet (Private IP retained across stop/start)
  - A security group allowing inbound from the CloudFront managed prefix list
  - An EC2 instance running Ubuntu 26.04 with code-server installed via cloud-init
  - A CloudFront VPC origin pointing at the EC2 instance
  - A CloudFront distribution attached to the WAF web ACL ` + "(`nakama-waf`)" + `
  - SSM Parameter Store entries for the password and instance metadata

The whole flow takes ~15-20 minutes (CloudFront propagation dominates).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
	cmd.Flags().StringVar(&f.name, "name", "", "Logical name of the instance (required, [a-z0-9][a-z0-9-]*)")
	cmd.Flags().StringVar(&f.instanceType, "instance-type", "", "EC2 instance type (required, e.g. t3.medium)")
	cmd.Flags().StringVar(&f.codeServerVersion, "code-server-version", "", "Pin a specific code-server version (default: latest)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("instance-type")
	return cmd
}
