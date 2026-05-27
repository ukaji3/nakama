package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

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
                    Triggers ` + "`growpart` and `resize2fs/xfs_growfs`" + ` on the
                    instance via SSM Run Command.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)

			if f.ebsGiB <= 0 {
				return errors.New("--ebs <size-gb> is required")
			}

			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			err = service.NewService(clients).EditEBS(ctx, service.EditEBSInput{
				Name:      args[0],
				NewSizeGB: f.ebsGiB,
			})
			if err != nil {
				return err
			}
			res := map[string]any{
				"name":   args[0],
				"action": "ebs-expand",
				"new_gb": f.ebsGiB,
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, res,
				fmt.Sprintf("ebs expanded: %s -> %dGiB", args[0], f.ebsGiB))
		},
	}
	cmd.Flags().Int32Var(&f.ebsGiB, "ebs", 0, "New EBS root volume size in GiB (must be > current size)")
	return cmd
}
