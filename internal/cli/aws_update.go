package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSUpdateCommand() *cobra.Command {
	var keep int32
	var yes bool
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update an instance (auto-backup, apt dist-upgrade, code-server latest)",
		Long: `Brings an existing nakama instance up to date in one step:

  1. Takes a pre-update backup (root EBS snapshot).
  2. Starts the instance if it is stopped.
  3. Runs ` + "`apt-get update && apt-get dist-upgrade -y`" + ` and installs the
     latest code-server, via SSM Run Command.
  4. Reboots only if a kernel/library update requires it (was-running case).
  5. Restores the instance's original power state (stops it again if it
     was stopped), even if the update fails.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			name := args[0]

			if !yes {
				ok, err := confirmUpdate(cmd.OutOrStdout(), name)
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("aborted")
				}
			}

			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			out, err := service.NewService(clients).Update(ctx, service.UpdateInput{
				Name: name,
				Keep: keep,
			})
			if err != nil {
				return err
			}

			msg := fmt.Sprintf("updated: %s (backup %s, code-server %s)", name, out.BackupLabel, out.CodeServerVersion)
			switch {
			case out.Rebooted:
				msg += " [rebooted]"
			case out.RebootPending:
				msg += " [kernel update applies on next start]"
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, out, msg)
		},
	}
	cmd.Flags().Int32Var(&keep, "keep", 0, "Keep only the newest N backups after the pre-update snapshot (0 = keep all)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

// confirmUpdate prompts before an update, which is disruptive (apt
// dist-upgrade, code-server restart, possible reboot/start/stop).
func confirmUpdate(w interface{ Write([]byte) (int, error) }, name string) (bool, error) {
	if _, err := fmt.Fprintf(asWriter(w),
		"Update %q (auto-backup, apt dist-upgrade, code-server latest)? "+
			"The instance may be started, rebooted, and/or stopped. [y/N]: ", name); err != nil {
		return false, err
	}
	return readYesNo()
}
