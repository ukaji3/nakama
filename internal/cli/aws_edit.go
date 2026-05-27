package cli

import "github.com/spf13/cobra"

type editFlags struct {
	ebsGiB int32
}

func newAWSEditCommand() *cobra.Command {
	f := &editFlags{}
	cmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Modify a nakama instance (currently: EBS volume expansion only)",
		Long: `Currently supported edits:

  --ebs <size-gb>   Expand the root EBS volume to the given size (cannot shrink).
                    Triggers ` + "`growpart` and `resize2fs`" + ` on the instance via
                    SSM Run Command.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errNotImplemented
		},
	}
	cmd.Flags().Int32Var(&f.ebsGiB, "ebs", 0, "New EBS root volume size in GiB (must be > current size)")
	return cmd
}
