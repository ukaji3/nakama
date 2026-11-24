package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Manage root-EBS snapshot backups of a nakama instance",
	}
	cmd.AddCommand(
		newAWSBackupCreateCommand(),
		newAWSBackupListCommand(),
		newAWSBackupNoteCommand(),
		newAWSBackupDeleteCommand(),
	)
	return cmd
}

func newAWSBackupCreateCommand() *cobra.Command {
	var label, note string
	var keep int32
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Take a snapshot of the instance's root EBS volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			b, err := service.NewService(clients).CreateBackup(ctx, service.CreateBackupInput{
				Name: args[0], Label: label, Note: note, Keep: keep,
			})
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, b,
				fmt.Sprintf("backup created: %s (%s)", b.Label, b.State))
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Human-readable backup label (default: timestamp)")
	cmd.Flags().StringVar(&note, "note", "", "Free-text memo to attach to the backup")
	cmd.Flags().Int32Var(&keep, "keep", 0, "Keep only the newest N backups (0 = keep all)")
	return cmd
}

func newAWSBackupListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list [name]",
		Short: "List backups of a nakama instance (all instances if name is omitted)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			svc := service.NewService(clients)
			if len(args) == 0 {
				backups, err := svc.ListAllBackups(ctx)
				if err != nil {
					return err
				}
				return renderOutput(cmd.OutOrStdout(), global.Output, backups, formatBackupTableAll(backups))
			}
			backups, err := svc.ListBackups(ctx, args[0])
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, backups, formatBackupTable(backups))
		},
	}
}

func newAWSBackupNoteCommand() *cobra.Command {
	var label, note string
	cmd := &cobra.Command{
		Use:   "note <name> --label <label> --note <text>",
		Short: "Set or clear the memo on an existing backup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Require an explicit --note: without this check, leaving
			// the flag off would silently wipe the existing memo.
			if !cmd.Flags().Changed("note") {
				return errors.New("--note is required (pass --note \"\" to clear the memo)")
			}
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			b, err := service.NewService(clients).SetBackupNote(ctx, args[0], label, note)
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, b,
				fmt.Sprintf("note updated: %s -> %q", b.Label, b.Note))
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Label of the backup to annotate (required)")
	cmd.Flags().StringVar(&note, "note", "", "Memo text (required; pass an empty string to clear)")
	_ = cmd.MarkFlagRequired("label")
	return cmd
}

// backupDeleteBackend is the slice of the service layer `backup delete`
// uses; tests substitute a fake.
type backupDeleteBackend interface {
	FindBackup(ctx context.Context, name, label string) (*service.Backup, error)
	DeleteFoundBackup(ctx context.Context, b *service.Backup) error
}

type backupDeleteDeps struct {
	connect func(ctx context.Context) (backupDeleteBackend, error)
	// confirm reads the y/N answer (stdin in production).
	confirm func() (bool, error)
}

func defaultBackupDeleteDeps() backupDeleteDeps {
	return backupDeleteDeps{
		connect: func(ctx context.Context) (backupDeleteBackend, error) {
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return nil, err
			}
			return service.NewService(clients), nil
		},
		confirm: readYesNo,
	}
}

func newAWSBackupDeleteCommand() *cobra.Command {
	return newAWSBackupDeleteCommandWithDeps(defaultBackupDeleteDeps())
}

func newAWSBackupDeleteCommandWithDeps(deps backupDeleteDeps) *cobra.Command {
	var (
		label string
		yes   bool
	)
	cmd := &cobra.Command{
		Use:   "delete <name> --label <label>",
		Short: "Delete a backup by label",
		Long: `Deletes the snapshot of the backup with the given label. A deleted
backup cannot be restored from. It shows the backup and asks first
unless --yes is given; a script without a terminal must pass --yes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ctx := withCancel(cmd)
			backend, err := deps.connect(ctx)
			if err != nil {
				return err
			}
			b, err := backend.FindBackup(ctx, name, label)
			if err != nil {
				return err
			}
			if !yes {
				if _, err := io.WriteString(cmd.OutOrStdout(), backupDeletePrompt(name, b)); err != nil {
					return err
				}
				ok, err := deps.confirm()
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("aborted")
				}
			}
			if err := backend.DeleteFoundBackup(ctx, b); err != nil {
				return err
			}
			res := map[string]string{"name": name, "label": label, "action": "deleted"}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, "backup deleted: "+label)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Label of the backup to delete (required)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	_ = cmd.MarkFlagRequired("label")
	return cmd
}

// backupDeletePrompt shows the backup about to be deleted. Pure
// function: unit-tested.
func backupDeletePrompt(name string, b *service.Backup) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Delete backup %q of %q?\n", b.Label, name)
	fmt.Fprintf(&s, "  snapshot   : %s (%d GiB, created %s)\n", b.SnapshotID, b.SizeGiB, b.CreatedAt)
	if b.Note != "" {
		fmt.Fprintf(&s, "  note       : %s\n", b.Note)
	}
	s.WriteString("A deleted backup cannot be restored from. [y/N]: ")
	return s.String()
}

func formatBackupTable(backups []service.Backup) string {
	if len(backups) == 0 {
		return "(no backups)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s  %-20s  %-10s  %-5s  %s\n", "LABEL", "CREATED_AT", "STATE", "GiB", "NOTE")
	for _, bk := range backups {
		fmt.Fprintf(&b, "%-20s  %-20s  %-10s  %-5d  %s\n", bk.Label, bk.CreatedAt, bk.State, bk.SizeGiB, bk.Note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatBackupTableAll renders backups across all instances, adding a
// leading NAME column so backups of deleted instances remain identifiable.
func formatBackupTableAll(backups []service.Backup) string {
	if len(backups) == 0 {
		return "(no backups)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s  %-20s  %-20s  %-10s  %-5s  %s\n", "NAME", "LABEL", "CREATED_AT", "STATE", "GiB", "NOTE")
	for _, bk := range backups {
		fmt.Fprintf(&b, "%-16s  %-20s  %-20s  %-10s  %-5d  %s\n", bk.Name, bk.Label, bk.CreatedAt, bk.State, bk.SizeGiB, bk.Note)
	}
	return strings.TrimRight(b.String(), "\n")
}
