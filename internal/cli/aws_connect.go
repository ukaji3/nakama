package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSConnectCommand() *cobra.Command {
	var portForward string
	var tty bool
	cmd := &cobra.Command{
		Use:   "connect <name> [command [args...]]",
		Short: "Log in to a nakama instance over SSM Session Manager",
		Long: `Open an interactive shell on a nakama instance via AWS Systems Manager
Session Manager (no SSH, no inbound ports). Append a command to run it
non-interactively (like "ssh host ls"); use --tty for a PTY. Use
-L/--port-forward to forward a local port instead of opening a shell.

Examples:
  nakama aws connect dev01
  nakama aws connect dev01 ls -la
  nakama aws connect dev01 -L 8080:8080
  nakama aws connect dev01 -L 5432:db.internal:5432`,
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
	return cmd
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
