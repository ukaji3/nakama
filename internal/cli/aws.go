package cli

import "github.com/spf13/cobra"

func newAWSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "aws",
		Short: "AWS resource management subcommands",
	}
	cmd.AddCommand(
		newAWSInitCommand(),
		newAWSLoginCommand(),
		newAWSACLCommand(),
		newAWSCreateCommand(),
		newAWSListCommand(),
		newAWSInfoCommand(),
		newAWSOpenCommand(),
		newAWSStartCommand(),
		newAWSStopCommand(),
		newAWSDeleteCommand(),
		newAWSEditCommand(),
		newAWSBackupCommand(),
		newAWSRestoreCommand(),
		newAWSConnectCommand(),
		newAWSSessionRunnerCommand(),
		newAWSUpdateCommand(),
		newAWSProfileCommand(),
	)
	return cmd
}
