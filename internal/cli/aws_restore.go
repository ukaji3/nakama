package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSRestoreCommand() *cobra.Command {
	var label string
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore <name>",
		Short: "Restore an instance's root volume from a backup (in place, destructive)",
		Long: `Stops the instance, creates a volume from the selected backup,
swaps it in as the root device, and deletes the previous root volume.
The instance is left stopped. Use --label to pick a backup; the most
recent backup is used when --label is omitted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			name := args[0]
			if !yes {
				ok, err := confirmRestore(cmd.OutOrStdout(), name)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted")
				}
			}
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			if err := service.NewService(clients).Restore(ctx, service.RestoreInput{Name: name, Label: label}); err != nil {
				return err
			}
			res := map[string]string{"name": name, "label": label, "action": "restored"}
			return renderOutput(cmd.OutOrStdout(), global.Output, res,
				fmt.Sprintf("restored %s (instance left stopped; run \"nakama aws start %s\")", name, name))
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Backup label to restore (default: latest)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

// confirmRestore reuses the same y/N reader as confirmDestroy.
func confirmRestore(w interface{ Write([]byte) (int, error) }, name string) (bool, error) {
	if _, err := fmt.Fprintf(asWriter(w), "Restore nakama instance %q in place? This deletes the current root volume. [y/N]: ", name); err != nil {
		return false, err
	}
	return readYesNo()
}
