package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

type editFlags struct {
	ebsGiB       int32
	instanceType string
	yes          bool
}

func newAWSEditCommand() *cobra.Command {
	f := &editFlags{}
	cmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Modify a nakama instance (EBS expansion or instance-type change)",
		Long: `Exactly one edit must be requested per invocation:

  --ebs <size-gb>          Expand the root EBS volume to the given size
                           (cannot shrink). Runs ` + "`growpart` + `resize2fs/xfs_growfs`" + `
                           on the instance via SSM Run Command. Works while running.

  --instance-type <type>   Change the EC2 instance type. AWS requires the
                           instance to be stopped, so a running instance is
                           stopped, modified, and started again. The type is
                           validated first; an unknown type fails immediately.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			name := args[0]

			ebsRequested := f.ebsGiB > 0
			typeRequested := f.instanceType != ""
			switch {
			case ebsRequested && typeRequested:
				return errors.New("--ebs and --instance-type are mutually exclusive; run one at a time")
			case !ebsRequested && !typeRequested:
				return errors.New("one of --ebs <size-gb> or --instance-type <type> is required")
			}

			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			svc := service.NewService(clients)

			if typeRequested {
				if !f.yes {
					ok, err := confirmInstanceTypeChange(cmd.OutOrStdout(), name, f.instanceType)
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("aborted")
					}
				}
				out, err := svc.EditInstanceType(ctx, service.EditInstanceTypeInput{
					Name: name, NewType: f.instanceType,
				})
				if err != nil {
					return err
				}
				msg := fmt.Sprintf("instance type changed: %s %s -> %s", name, out.OldType, out.NewType)
				if out.NoOp {
					msg = fmt.Sprintf("instance type unchanged: %s already %s", name, out.NewType)
				} else if out.Restarted {
					msg += " (instance restarted)"
				} else {
					msg += " (instance left stopped)"
				}
				return renderOutput(cmd.OutOrStdout(), global.Output, out, msg)
			}

			err = svc.EditEBS(ctx, service.EditEBSInput{
				Name:      name,
				NewSizeGB: f.ebsGiB,
			})
			if err != nil {
				return err
			}
			res := map[string]any{
				"name":   name,
				"action": "ebs-expand",
				"new_gb": f.ebsGiB,
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, res,
				fmt.Sprintf("ebs expanded: %s -> %dGiB", name, f.ebsGiB))
		},
	}
	cmd.Flags().Int32Var(&f.ebsGiB, "ebs", 0, "New EBS root volume size in GiB (must be > current size)")
	cmd.Flags().StringVar(&f.instanceType, "instance-type", "", "New EC2 instance type (stops/starts the instance)")
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "Skip the confirmation prompt (instance-type change)")
	return cmd
}

// confirmInstanceTypeChange prompts before an instance-type change, which
// stops (and restarts) the instance.
func confirmInstanceTypeChange(w interface{ Write([]byte) (int, error) }, name, newType string) (bool, error) {
	if _, err := fmt.Fprintf(asWriter(w),
		"Change instance type of %q to %q? This stops and restarts the instance. [y/N]: ",
		name, newType); err != nil {
		return false, err
	}
	return readYesNo()
}
