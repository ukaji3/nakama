package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// defaultShellRetries is the reconnect budget applied when --retry is
// not given, for the interactive shell where re-running is safe.
// Command mode defaults to 0 because re-running a possibly
// non-idempotent remote command on reconnect is dangerous.
const defaultShellRetries = 5

func newAWSShellCommand() *cobra.Command {
	var tty bool
	var retries int
	cmd := &cobra.Command{
		Use:   "shell <name> [command [args...]]",
		Short: "Log in to a nakama instance over SSM Session Manager",
		Long: `Open an interactive shell on a nakama instance via AWS Systems Manager
Session Manager (no SSH, no inbound ports). You are logged in as the
"ubuntu" user (Session Manager authenticates as ssm-user, which nakama
switches from via sudo); a single "exit" ends the session. For
background port forwarding, see "nakama aws tunnel". The shell is on
the instance; "nakama aws cli shell" (Windows) is a different command
that starts a shell on this machine set up for the aws CLI.

Append a command to run it as ubuntu, non-interactively (like
"ssh host ls"): its stdout and stderr arrive separately, and nakama
exits with the command's exit status (255 when nakama itself failed or
the status was not reported). Standard input is not read in this mode:
Session Manager does not deliver it to the command.

--tty (-t, as in ssh) runs the command in a PTY. In a PTY, and in the
shell, input is sent to the remote terminal. It may also come from a
file or a pipe (the remote terminal echoes it, and its end is sent as
Ctrl-D, which ends the shell), except on Windows, which needs a
console. Session Manager reports no exit status for a PTY.

When the session is permanently disconnected (e.g. the instance was
hibernated or the network dropped for too long), nakama reconnects
automatically with a fresh session, up to --retry times (default 5 for
the shell, 0 for command mode). A reconnected shell is a new session:
previous shell state is lost.

Flags must precede <name>: everything after <name> is sent to the
instance as the remote command (ssh-style).

Examples:
  nakama aws shell dev01
  nakama aws shell dev01 ls -la
  nakama aws shell dev01 test -d /srv/app || echo missing
  nakama aws shell dev01 'cd /srv/app && git status'
  nakama aws shell -t dev01 top
  nakama aws shell dev01 < setup-steps.sh
  nakama aws shell --retry 0 dev01`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			in := service.ConnectInput{Name: args[0], Command: args[1:], TTY: tty}
			if retries < 0 {
				return fmt.Errorf("invalid --retry %d: must be >= 0", retries)
			}
			in.MaxRetries = resolveShellRetries(
				cmd.Flags().Changed("retry"), retries, len(in.Command) > 0)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			return silenceRemoteExit(cmd, service.NewService(clients).Connect(ctx, in))
		},
	}
	// Treat everything after <name> as the remote command, so flags like
	// --retry must precede <name> (ssh-style).
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "Run the command in an interactive PTY")
	cmd.Flags().IntVar(&retries, "retry", defaultShellRetries,
		"Max automatic reconnect attempts after a permanent disconnect (0 = disabled; default 0 in command mode)")
	return cmd
}

// silenceRemoteExit returns err unchanged and, when it carries the
// remote command's own exit status, keeps cobra from printing it:
// ssh-style, the status is the exit code and nothing else (ADR 0021).
func silenceRemoteExit(cmd *cobra.Command, err error) error {
	var ec *service.ExitCodeError
	if errors.As(err, &ec) && ec.Remote {
		cmd.SilenceErrors = true
	}
	return err
}

// resolveShellRetries picks the reconnect budget: an explicit --retry
// wins in every mode; otherwise the interactive shell defaults to
// defaultShellRetries while command mode (including --tty) defaults
// to 0, because reconnecting re-runs the remote command and that is not
// safe for non-idempotent commands.
func resolveShellRetries(explicit bool, value int, commandMode bool) int {
	if explicit {
		return value
	}
	if commandMode {
		return 0
	}
	return defaultShellRetries
}
