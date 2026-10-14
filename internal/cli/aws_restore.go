package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSRestoreCommand() *cobra.Command {
	var in service.RestoreInput
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore <name>",
		Short: "Restore an instance from a backup (in place, or as a new instance if it was deleted)",
		Long: `Restores the instance's disk from a backup. Use --label to pick a
backup; the most recent backup is used when --label is omitted.

If the instance exists, restore stops it, creates a volume from the
backup, swaps it in as the root device, and deletes the previous root
volume (destructive).

If the instance was deleted, restore builds a new instance of the same
name from the backup: a fresh instance with the resources create makes,
whose root volume is then replaced by the backup. Software is not
reinstalled. The URL and the code-server password are new; the SSH host
keys are the backup's. The instance type and kind (code-server or bare)
recorded on the backup are used; --instance-type overrides the type
within the same CPU architecture, and is required for backups taken
before nakama recorded it (add --bare for a bare instance). An IAM role
is attached only with --with-admin or --with-role.

Either way the instance is left stopped; start it with "nakama aws start".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			in.Name = args[0]
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			svc := service.NewService(clients)
			plan, err := svc.PlanRestore(ctx, in)
			if err != nil {
				return err
			}
			for _, w := range plan.Warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			if !yes {
				if _, err := io.WriteString(cmd.OutOrStdout(), restorePrompt(plan)); err != nil {
					return err
				}
				ok, err := readYesNo()
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted")
				}
			}
			res, err := svc.Restore(ctx, plan)
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, restoreSummary(res))
		},
	}
	cmd.Flags().StringVar(&in.Label, "label", "", "Backup label to restore (default: latest)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	cmd.Flags().StringVar(&in.InstanceType, "instance-type", "",
		"Deleted instance only: EC2 instance type of the new instance (default: the one recorded on the backup)")
	cmd.Flags().BoolVar(&in.Bare, "bare", false,
		"Deleted instance only: the backup is of a bare instance (needed only for backups that do not record it)")
	cmd.Flags().BoolVar(&in.WithAdmin, "with-admin", false,
		"Deleted instance only: attach the shared nakama-managed IAM role with AdministratorAccess")
	cmd.Flags().StringVar(&in.WithRole, "with-role", "",
		"Deleted instance only: attach an existing IAM role (by name)")
	cmd.MarkFlagsMutuallyExclusive("with-admin", "with-role")
	return cmd
}

// restorePrompt renders the confirmation question for a plan. Pure
// function: unit-tested.
func restorePrompt(p *service.RestorePlan) string {
	b := p.Backup
	if p.Mode == service.RestoreModeInPlace {
		return fmt.Sprintf("Restore nakama instance %q in place from backup %q (%s)? "+
			"This deletes the current root volume. [y/N]: ", p.Name, b.Label, b.CreatedAt)
	}
	kind := "code-server"
	if p.Bare {
		kind = "bare (no code-server / CloudFront)"
	}
	role := "none"
	switch {
	case p.WithAdmin:
		role = "nakama-managed role with AdministratorAccess (--with-admin)"
	case p.WithRole != "":
		role = p.WithRole
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "nakama instance %q no longer exists. Restore it as a new instance from backup %q (%s)?\n",
		p.Name, b.Label, b.CreatedAt)
	fmt.Fprintf(&sb, "  instance type : %s (%s)\n", p.InstanceType, p.Architecture)
	fmt.Fprintf(&sb, "  kind          : %s\n", kind)
	fmt.Fprintf(&sb, "  IAM role      : %s\n", role)
	if p.Bare {
		sb.WriteString("The disk is the backup's; nothing is reinstalled. The instance is left stopped.\n")
	} else {
		sb.WriteString("The disk is the backup's; nothing is reinstalled. The URL and the code-server password\n" +
			"are new. This takes about 15-20 minutes (CloudFront); the instance is left stopped.\n")
	}
	sb.WriteString("[y/N]: ")
	return sb.String()
}

// restoreSummary is the text output after a restore.
func restoreSummary(r *service.RestoreResult) string {
	next := fmt.Sprintf("run \"nakama aws start %s\"", r.Name)
	if r.Mode == service.RestoreModeInPlace {
		return fmt.Sprintf("restored %s in place from backup %q (instance left stopped; %s)", r.Name, r.Label, next)
	}
	s := fmt.Sprintf("restored %s from backup %q as a new instance (left stopped; %s)\n", r.Name, r.Label, next) +
		"  instance_id       : " + r.InstanceID + "\n" +
		"  instance_type     : " + r.InstanceType + "\n"
	if r.Bare {
		s += "  kind              : bare (no code-server / CloudFront)\n" +
			"  access            : nakama aws ssh setup " + r.Name + "  |  nakama aws connect " + r.Name
	} else {
		s += "  url               : " + r.URL + "\n" +
			"  password          : new (use `nakama aws info " + r.Name + " --show-password`)"
	}
	if r.IAMRole != "" {
		s += "\n  iam_role          : " + r.IAMRole
	}
	return s
}
