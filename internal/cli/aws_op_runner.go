package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/service"
)

// newAWSOpRunnerCommand returns the hidden process that runs one
// operation of the control panel (Issue #139, ADR 0031): the panel
// starts it detached from its window, with standard output and error
// redirected to the operation's files, and it runs the recorded command
// line through the same command tree as the CLI.
func newAWSOpRunnerCommand() *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:    "op-runner",
		Short:  "Internal: runs one operation of the control panel",
		Hidden: true,
		Args:   cobra.NoArgs,
		// The recorded command reports its own errors on stderr (the
		// operation's log); the runner adds nothing to it.
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			code, err := service.RunOperation(id, runRecordedCommand, os.Exit)
			if err != nil {
				return err
			}
			if code != 0 {
				// The command already reported its error on stderr.
				return &service.ExitCodeError{Code: code, Remote: true}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "Operation id")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

// runRecordedCommand runs args through a fresh command tree and returns
// the exit code the nakama binary would end with (cmd/nakama).
func runRecordedCommand(args []string) int {
	root := NewRootCommand()
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ec *service.ExitCodeError
	if errors.As(err, &ec) {
		return ec.Code
	}
	return 1
}
