package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// defaultConnectRetries is the reconnect budget applied when --retry is
// not given, for the modes where re-running is safe (interactive shell
// and port-forward). Command mode defaults to 0 because re-running a
// possibly non-idempotent remote command on reconnect is dangerous.
const defaultConnectRetries = 5

func newAWSConnectCommand() *cobra.Command {
	var portForward string
	var tty bool
	var retries int
	cmd := &cobra.Command{
		Use:   "connect <name> [command [args...]]",
		Short: "Log in to a nakama instance over SSM Session Manager",
		Long: `Open an interactive shell on a nakama instance via AWS Systems Manager
Session Manager (no SSH, no inbound ports). You are logged in as the
"ubuntu" user (Session Manager authenticates as ssm-user, which nakama
switches from via sudo); a single "exit" ends the session. Append a
command to run it as ubuntu, non-interactively (like "ssh host ls"); use
--tty for a PTY. Use -L/--port-forward to forward a local port instead of
opening a shell.

When the session is permanently disconnected (e.g. the instance was
hibernated or the network dropped for too long), nakama reconnects
automatically with a fresh session, up to --retry times (default 5 for
shell and port-forward, 0 for command mode). A reconnected shell is a
new session: previous shell state is lost.

Examples:
  nakama aws connect dev01
  nakama aws connect dev01 ls -la
  nakama aws connect dev01 -L 8080:8080
  nakama aws connect dev01 -L 5432:db.internal:5432
  nakama aws connect dev01 --retry 0`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			in := service.ConnectInput{Name: args[0], Command: args[1:], TTY: tty}
			if portForward != "" {
				if len(in.Command) > 0 {
					return fmt.Errorf("--port-forward cannot be combined with a command")
				}
				local, host, remote, err := parsePortForward(portForward)
				if err != nil {
					return err
				}
				in.LocalPort, in.RemoteHost, in.RemotePort = local, host, remote
			}
			if retries < 0 {
				return fmt.Errorf("invalid --retry %d: must be >= 0", retries)
			}
			in.MaxRetries = resolveConnectRetries(
				cmd.Flags().Changed("retry"), retries, len(in.Command) > 0)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			return service.NewService(clients).Connect(ctx, in)
		},
	}
	// Treat everything after <name> as the remote command, so flags like
	// -L must precede <name> (ssh-style).
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVarP(&portForward, "port-forward", "L", "",
		"Port forward: <local>:<remote> or <local>:<host>:<remote>")
	cmd.Flags().BoolVar(&tty, "tty", false, "Run the command in an interactive PTY")
	cmd.Flags().IntVar(&retries, "retry", defaultConnectRetries,
		"Max automatic reconnect attempts after a permanent disconnect (0 = disabled; default 0 in command mode)")
	return cmd
}

// resolveConnectRetries picks the reconnect budget: an explicit --retry
// wins in every mode; otherwise shell and port-forward default to
// defaultConnectRetries while command mode (including --tty) defaults
// to 0, because reconnecting re-runs the remote command and that is not
// safe for non-idempotent commands.
func resolveConnectRetries(explicit bool, value int, commandMode bool) int {
	if explicit {
		return value
	}
	if commandMode {
		return 0
	}
	return defaultConnectRetries
}

// parsePortForward parses an -L spec: "<local>:<remote>" forwards to the
// instance itself; "<local>:<host>:<remote>" forwards through the
// instance to <host>.
func parsePortForward(s string) (local, host, remote string, err error) {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 2:
		local, host, remote = parts[0], "", parts[1]
	case 3:
		local, host, remote = parts[0], parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("invalid -L %q: want <local>:<remote> or <local>:<host>:<remote>", s)
	}
	if err := validatePort(local); err != nil {
		return "", "", "", fmt.Errorf("local port: %w", err)
	}
	if err := validatePort(remote); err != nil {
		return "", "", "", fmt.Errorf("remote port: %w", err)
	}
	return local, host, remote, nil
}

func validatePort(p string) error {
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q is not a valid port (1-65535)", p)
	}
	return nil
}
