package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// sessionPayloadFD is the file descriptor on which the Connect
// supervisor (internal/service, spawnSessionRunner) hands over the
// session payload: ExtraFiles[0] becomes fd 3 in the child. The payload
// contains the secret session TokenValue and therefore must not travel
// via argv or the environment; fd 3 keeps stdin free for the
// interactive session itself.
const sessionPayloadFD = 3

// newAWSSessionRunnerCommand returns the hidden child command that
// executes a single Session Manager session for `nakama aws connect`
// (ADR 0014). It is an internal re-exec target, not a user-facing
// command: the vendored session-manager-plugin ends the whole process
// with os.Exit, so the session must run in a disposable child while the
// parent supervises exit codes and reconnects.
func newAWSSessionRunnerCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "session-runner",
		Short:  "Internal: run one Session Manager session (used by connect)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			f := os.NewFile(sessionPayloadFD, "nakama-session-payload")
			if f == nil {
				return fmt.Errorf("session payload fd %d is not open; "+
					"`nakama aws session-runner` is an internal command spawned by `nakama aws connect`",
					sessionPayloadFD)
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
