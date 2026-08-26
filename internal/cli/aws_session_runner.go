package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSSessionRunnerCommand returns the hidden child command that
// executes a single Session Manager session for `nakama aws connect`
// (ADR 0014). It is an internal re-exec target, not a user-facing
// command: the vendored session-manager-plugin ends the whole process
// with os.Exit, so the session must run in a disposable child while the
// parent supervises exit codes and reconnects.
//
// The session payload (which contains the secret TokenValue) is
// received over an inherited pipe: fd 3 on unix, an inherited handle
// (announced via service.SessionPayloadHandleEnv) on Windows — see
// openSessionPayload in aws_session_runner_unix.go /
// aws_session_runner_windows.go.
func newAWSSessionRunnerCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "session-runner",
		Short:  "Internal: run one Session Manager session (used by connect)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			f, err := openSessionPayload()
			if err != nil {
				return err
			}
			payload, err := io.ReadAll(f)
			closeErr := f.Close()
			if err != nil {
				return fmt.Errorf("read session payload: %w", err)
			}
			if closeErr != nil {
				return fmt.Errorf("close session payload fd: %w", closeErr)
			}
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			return service.RunSessionRunner(ctx, clients, payload)
		},
	}
}
