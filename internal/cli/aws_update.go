package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSUpdateCommand() *cobra.Command {
	var keep int32
	var yes bool
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update an instance (auto-backup, apt dist-upgrade, code-server, AWS CLI, Kiro Crew)",
		Long: `Brings an existing nakama instance up to date in one step:

  1. Takes a pre-update backup (root EBS snapshot).
  2. Starts the instance if it is stopped.
  3. Runs ` + "`apt-get update && apt-get dist-upgrade -y`" + `, installs the
     latest code-server, updates the AWS CLI (when it was installed with
     AWS's installer; instances created before nakama did that keep their
     snap) and runs ` + "`kirocrew update`" + ` and ` + "`kirocrew restart`" + ` (when Kiro Crew
     is installed), via SSM Run Command. The full output is in
     /var/log/nakama-update.log on the instance.
  4. Reboots only if a kernel/library update requires it (was-running case).
  5. Restores the instance's original power state (stops it again if it
     was stopped), even if the update fails.

A failed Kiro Crew update does not fail the command; it is reported as a
warning. Restarting Kiro Crew ends its running sessions.`,
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

			if out.KiroCrew == service.KiroCrewFailed {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: kirocrew update or kirocrew restart failed on %s; run them there as ubuntu "+
						"(\"nakama aws connect %s\") to see why\n", name, name)
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, out, updateSummary(name, out))
		},
	}
	cmd.Flags().Int32Var(&keep, "keep", 0, "Keep only the newest N backups after the pre-update snapshot (0 = keep all)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

// updateSummary is the text output of a finished update. Pure function:
// unit-tested.
func updateSummary(name string, out *service.UpdateResult) string {
	parts := []string{"backup " + out.BackupLabel}
	if out.CodeServerVersion != "" {
		parts = append(parts, "code-server "+out.CodeServerVersion)
	}
	if out.AWSCLIVersion != "" {
		parts = append(parts, "AWS CLI "+out.AWSCLIVersion)
	}
	switch out.KiroCrew {
	case service.KiroCrewUpdated:
		parts = append(parts, "Kiro Crew updated and restarted")
	case service.KiroCrewFailed:
		parts = append(parts, "Kiro Crew update FAILED")
	}
	msg := fmt.Sprintf("updated: %s (%s)", name, strings.Join(parts, ", "))
	switch {
	case out.Rebooted:
		msg += " [rebooted]"
	case out.RebootPending:
		msg += " [kernel update applies on next start]"
	}
	return msg
}

// confirmUpdate prompts before an update, which is disruptive (apt
// dist-upgrade, code-server and Kiro Crew restarts, possible
// reboot/start/stop).
func confirmUpdate(w interface{ Write([]byte) (int, error) }, name string) (bool, error) {
	if _, err := fmt.Fprintf(asWriter(w),
		"Update %q (auto-backup, apt dist-upgrade, code-server, AWS CLI, Kiro Crew)? "+
			"The instance may be started, rebooted, and/or stopped, and Kiro Crew is restarted. [y/N]: ", name); err != nil {
		return false, err
	}
	return readYesNo()
}
