package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSProvisionCommand returns `nakama aws provision <name>`: the
// provisioning steps of `create --preset/--dotfiles/--setup` on an
// existing instance (Issue #54). It was `nakama aws setup rerun` before
// Issue #129.
func newAWSProvisionCommand() *cobra.Command {
	var (
		setupScript string
		dotfiles    string
		preset      string
	)
	cmd := &cobra.Command{
		Use:   "provision <name>",
		Short: "Run provisioning steps (preset, dotfiles, setup script) on an existing instance",
		Long: `Runs the provisioning steps of ` + "`nakama aws create`" + ` (--preset,
--dotfiles, --setup) on an instance that already exists. It is the
recovery path when provisioning failed during create, and the way to
apply a changed setup script or dotfiles repository later.

Steps run in the order preset, dotfiles, setup, as the ubuntu user, and
their combined output is appended to ` + service.ProvisionLogPath + ` on the
instance. Idempotency is the script author's responsibility: nakama runs
the steps as given.`,
		Example: `  nakama aws provision dev01 --preset kiro
  nakama aws provision dev01 --dotfiles https://github.com/example/dotfiles
  nakama aws provision dev01 --setup ./setup.sh`,
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

// newAWSSetupCommand returns the `nakama aws setup` group of the names
// before Issue #129 (ADR 0030): `setup rerun <name>` runs `provision`.
// Both are hidden and name the replacement on stderr.
func newAWSSetupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:        "setup",
		Short:      "Provision an existing instance (setup script, dotfiles, presets)",
		Deprecated: `use "nakama aws provision <name>" instead`,
	}
	cmd.AddCommand(deprecatedCommand(newAWSProvisionCommand(), "rerun", "nakama aws provision"))
	return cmd
}
