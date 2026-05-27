package cli

import "github.com/spf13/cobra"

type deleteFlags struct {
	yes   bool
	force bool
}

func newAWSDeleteCommand() *cobra.Command {
	f := &deleteFlags{}
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a nakama instance and its CloudFront / WAF / EC2 / SSM resources",
		Long: `Deletes, in dependency order:

  1. CloudFront distribution (disable, wait, delete)
  2. CloudFront VPC origin
  3. EC2 instance (terminate)
  4. ENI
  5. Security group
  6. SSM Parameter Store entries under /nakama/<name>/...

Prompts for confirmation unless --yes is given. --force additionally
ignores 'already gone' errors during cleanup.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&f.force, "force", false, "Continue past 'already gone' errors")
	return cmd
}
