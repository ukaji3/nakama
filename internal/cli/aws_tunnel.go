package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSTunnelCommand returns the `nakama aws tunnel` command group
// (ADR 0017): background port forwarding kept alive by a detached
// daemon, so the terminal is not occupied. Running `tunnel <name>`
// without a subcommand behaves like `tunnel start <name>`; the
// subcommand names start/stop/status are therefore reserved instance
// names (rejected by `create`).
func newAWSTunnelCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel [start] <name> -L <spec> [-L <spec>...]",
		Short: "Forward local ports to a nakama instance in the background",
		Long: `Start a background daemon that forwards local ports to a nakama
instance over SSM Session Manager, without occupying the terminal.
"start" may be omitted. Each -L forward uses one SSM session; on
disconnect the daemon reconnects automatically with the same policy as
connect (ADR 0014). Daemon logs go to the OS log (unix: syslog tag
nakama-tunnel, Windows: Application event log, source nakama).

-L specs take <local>:<remote> (to the instance itself) or
<local>:<host>:<remote> (through the instance to another host).

Examples:
  nakama aws tunnel dev01 -L 8080:8080
  nakama aws tunnel start dev01 -L 8080:8080 -L 5432:db.internal:5432
  nakama aws tunnel status dev01
  nakama aws tunnel stop dev01`,
		Args: cobra.ExactArgs(1),
		RunE: runTunnelStart,
	}
	addTunnelForwardFlag(cmd)

	start := &cobra.Command{
		Use:   "start <name> -L <spec> [-L <spec>...]",
		Short: "Start background port forwarding (same as omitting start)",
		Args:  cobra.ExactArgs(1),
		RunE:  runTunnelStart,
	}
	addTunnelForwardFlag(start)

	stop := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop port forwarding and shut the daemon down",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			outcome, err := service.NewService(clients).StopTunnel(ctx, args[0])
			if err != nil {
				return err
			}
			if outcome.WasRunning {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "tunnel stopped for %s\n", args[0])
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "tunnel daemon for %s was already gone; cleaned up stale state\n", args[0])
			}
			return nil
		},
	}

	status := &cobra.Command{
		Use:   "status <name>",
		Short: "Show the state of each port forward",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			st, err := service.NewService(clients).GetTunnelStatus(ctx, args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), formatTunnelStatus(st, time.Now()))
			return nil
		},
	}

	cmd.AddCommand(start, stop, status)
	return cmd
}

// addTunnelForwardFlag registers the repeatable -L flag. Parent and
// start each get their own flag storage (cobra flag values must not be
// shared across commands).
func addTunnelForwardFlag(cmd *cobra.Command) {
	cmd.Flags().StringArrayP("forward", "L", nil,
		"Port forward: <local>:<remote> or <local>:<host>:<remote> (repeatable)")
}

// runTunnelStart implements both `tunnel <name>` and
// `tunnel start <name>`.
func runTunnelStart(cmd *cobra.Command, args []string) error {
	specs, err := cmd.Flags().GetStringArray("forward")
	if err != nil {
		return err
	}
	forwards, err := service.ParseForwardSpecs(specs)
	if err != nil {
		return err
	}
	ctx := withCancel(cmd)
	clients, err := awsx.NewClients(ctx)
	if err != nil {
		return err
	}
	name := args[0]
	st, err := service.NewService(clients).StartTunnel(ctx, name, forwards)
	if err != nil {
		var already *service.TunnelAlreadyRunningError
		if errors.As(err, &already) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "a tunnel is already running for %s:\n", name)
			_, _ = fmt.Fprint(cmd.ErrOrStderr(), formatTunnelStatus(already.Status, time.Now()))
			return fmt.Errorf("stop it first with `nakama aws tunnel stop %s`", name)
		}
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "tunnel started for %s (pid %d):\n", name, st.PID)
	_, _ = fmt.Fprint(cmd.OutOrStdout(), formatTunnelStatus(st, time.Now()))
	return nil
}

// formatTunnelStatus renders the per-forward state table shown by
// `tunnel status` and after `tunnel start`.
func formatTunnelStatus(st *service.TunnelStatus, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-24s  %-12s  %-10s  %s\n", "FORWARD", "STATE", "SINCE", "DETAIL")
	for _, f := range st.Forwards {
		since := "-"
		if !f.Since.IsZero() {
			since = now.Sub(f.Since).Round(time.Second).String()
		}
		fmt.Fprintf(&b, "%-24s  %-12s  %-10s  %s\n", f.Spec, string(f.State), since, f.Detail)
	}
	return b.String()
}
