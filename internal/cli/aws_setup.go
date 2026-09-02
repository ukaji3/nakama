package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSSetupCommand returns the `nakama aws setup` group: re-running
// the provisioning steps of `create --setup/--dotfiles/--preset` on an
// existing instance (Issue #54).
func newAWSSetupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Provision an existing instance (setup script, dotfiles, presets)",
	}
	cmd.AddCommand(newAWSSetupRerunCommand())
	return cmd
}

// newAWSSetupRerunCommand returns `nakama aws setup rerun <name>`.
func newAWSSetupRerunCommand() *cobra.Command {
	var (
		setupScript string
		dotfiles    string
		preset      string
	)
	cmd := &cobra.Command{
		Use:   "rerun <name>",
		Short: "Run provisioning steps on an existing instance",
		Long: `Run the same provisioning steps as ` + "`nakama aws create`" + ` on an
instance that already exists — the recovery path when provisioning failed
during create, and the way to apply a changed setup script or dotfiles
repository later.

Steps run in the order preset, dotfiles, setup, as the ubuntu user, and
their combined output is appended to ` + service.ProvisionLogPath + ` on the
instance. Idempotency is the script author's responsibility: nakama runs
the steps as given.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec := service.ProvisionSpec{SetupScript: setupScript, Dotfiles: dotfiles, Preset: preset}
			if spec.IsZero() {
				return fmt.Errorf("nothing to do: pass at least one of --setup, --dotfiles or --preset (presets: %s)",
					strings.Join(service.ProvisionPresetNames(), ", "))
			}
			if err := service.ValidateProvisionSpec(spec); err != nil {
				return err
			}
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			res, err := service.NewService(clients).Provision(ctx, args[0], spec)
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, res,
				fmt.Sprintf("provisioned %s: %s\n  log: %s on the instance\n",
					res.Name, strings.Join(res.Steps, ", "), res.LogPath))
		},
	}
	cmd.Flags().StringVar(&setupScript, "setup", "",
		"Shell script to run (local path or https URL), executed as ubuntu")
	cmd.Flags().StringVar(&dotfiles, "dotfiles", "",
		"Dotfiles repository to clone into ~/.dotfiles and apply")
	cmd.Flags().StringVar(&preset, "preset", "",
		"Bundled provisioning recipe to apply ("+strings.Join(service.ProvisionPresetNames(), ", ")+")")
	return cmd
}
