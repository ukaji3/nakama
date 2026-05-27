package cli

import "github.com/spf13/cobra"

func newAWSStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Start the EC2 instance backing a nakama instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
}

func newAWSStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop the EC2 instance backing a nakama instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
}
