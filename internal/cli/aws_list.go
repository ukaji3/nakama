package cli

import "github.com/spf13/cobra"

func newAWSListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all nakama-managed code-server instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
}
