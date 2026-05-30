package cli

import "github.com/spf13/cobra"

func newAWSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "aws",
		Short: "AWS resource management subcommands",
	}
	cmd.AddCommand(
		newAWSInitCommand(),
		newAWSACLCommand(),
		newAWSCreateCommand(),
		newAWSListCommand(),
		newAWSInfoCommand(),
		newAWSStartCommand(),
		newAWSStopCommand(),
		newAWSDeleteCommand(),
		newAWSEditCommand(),
		newAWSBackupCommand(),
		newAWSRestoreCommand(),
		newAWSConnectCommand(),
		newAWSUpdateCommand(),
	)
	return cmd
}
