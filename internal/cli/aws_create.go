package cli

import (
	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

type createFlags struct {
	name              string
	instanceType      string
	codeServerVersion string
	withAdmin         bool
	withRole          string
}

// createInputFromFlags maps the parsed CLI flags to the service-layer
// input. Split out of RunE so the mapping is unit-testable (flag
// mutual exclusion itself is enforced by cobra via
// MarkFlagsMutuallyExclusive).
func createInputFromFlags(f *createFlags) service.CreateInstanceInput {
	return service.CreateInstanceInput{
		Name:              f.name,
		InstanceType:      f.instanceType,
		CodeServerVersion: f.codeServerVersion,
		WithAdmin:         f.withAdmin,
		WithRole:          f.withRole,
	}
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

The whole flow takes ~15-20 minutes (CloudFront propagation dominates).

With --with-admin the instance gets a shared nakama-managed IAM role
carrying AdministratorAccess (for CDK/CloudFormation work from inside
code-server); with --with-role <role> an existing IAM role of your
choice is attached instead. Both preflight the caller's IAM
permissions via SimulatePrincipalPolicy and abort before creating any
resource when the permissions are insufficient.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			out, err := service.NewService(clients).CreateInstance(ctx, createInputFromFlags(f))
			if err != nil {
				return err
			}
			fallback := "instance created\n" +
				"  name              : " + out.Name + "\n" +
				"  instance_id       : " + out.InstanceID + "\n" +
				"  url               : " + out.URL + "\n" +
				"  password          : " + out.Password
			if out.IAMRole != "" {
				fallback += "\n  iam_role          : " + out.IAMRole
			}
			if out.InstanceProfile != "" {
				fallback += "\n  instance_profile  : " + out.InstanceProfile
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, out, fallback)
		},
	}
	cmd.Flags().StringVar(&f.name, "name", "", "Logical name of the instance (required, [a-z0-9][a-z0-9-]*)")
	cmd.Flags().StringVar(&f.instanceType, "instance-type", "", "EC2 instance type (required, e.g. t3.medium)")
	cmd.Flags().StringVar(&f.codeServerVersion, "code-server-version", "", "Pin a specific code-server version (default: latest)")
	cmd.Flags().BoolVar(&f.withAdmin, "with-admin", false, "Attach a shared nakama-managed IAM role with AdministratorAccess to the instance")
	cmd.Flags().StringVar(&f.withRole, "with-role", "", "Attach an existing IAM role (by name) to the instance")
	cmd.MarkFlagsMutuallyExclusive("with-admin", "with-role")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("instance-type")
	return cmd
}
