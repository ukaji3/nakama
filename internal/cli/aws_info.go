package cli

import "github.com/spf13/cobra"

type infoFlags struct {
	showPassword bool
}

func newAWSInfoCommand() *cobra.Command {
	f := &infoFlags{}
	cmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Show connection details for a nakama instance",
		Long: `Prints the CloudFront URL, instance state and metadata for the
named instance. The code-server password stored in SSM Parameter Store
is NOT printed unless ` + "`--show-password`" + ` is supplied.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
	cmd.Flags().BoolVar(&f.showPassword, "show-password", false, "Print the code-server password in plain text")
	return cmd
}
