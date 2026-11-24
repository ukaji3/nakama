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
		newAWSCLICommand(),
		newAWSACLCommand(),
		newAWSCreateCommand(),
		newAWSListCommand(),
		newAWSInfoCommand(),
		newAWSCodeCommand(),
		newAWSKirocrewCommand(),
		newAWSCloudFrontCommand(),
		newAWSStartCommand(),
		newAWSStopCommand(),
		newAWSDeleteCommand(),
		newAWSEditCommand(),
		newAWSBackupCommand(),
		newAWSRestoreCommand(),
		newAWSShellCommand(),
		newAWSTunnelCommand(),
		newAWSSSHCommand(),
		newAWSDoctorCommand(),
		newAWSProvisionCommand(),
		newAWSSessionRunnerCommand(),
		newAWSTunnelDaemonCommand(),
		newAWSRouterDaemonCommand(),
		newAWSUpdateCommand(),
		newAWSProfileCommand(),
		// The names before Issue #129 (ADR 0030): hidden, and pointing
		// to the new name on stderr.
		deprecatedCommand(newAWSCodeCommand(), "open", "nakama aws code"),
		deprecatedCommand(newAWSShellCommand(), "connect", "nakama aws shell"),
		newAWSSetupCommand(),
	)
	return cmd
}
