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
// subcommand names start/stop/status/restart are therefore reserved
// instance names (rejected by `create`).
func newAWSTunnelCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel [start] <name> -L <spec> [-L <spec>...]",
		Short: "Forward local ports to a nakama instance in the background",
		Long: `Start a background daemon that forwards local ports to a nakama
instance over SSM Session Manager, without occupying the terminal.
"start" may be omitted. Daemon logs go to the OS log (unix: syslog tag
nakama-tunnel, Windows: Application event log, source nakama).

The daemon holds each local port itself (on 127.0.0.1 and ::1) and
relays it through an SSM port-forwarding session (ADR 0027). When a
session is lost (network errors, a network change, sleep, the Session
Manager idle timeout) the port stays open and the daemon reconnects by
itself, with no attempt limit; a connection that arrives meanwhile
waits up to 30 seconds for the new session. A forward that has not been
used for 5 minutes does not reconnect until the next connection
arrives (state "idle"). After a credential error the daemon retries
every minute, so ` + "`nakama aws login`" + ` is enough to bring it back.
` + "`nakama aws open`" + ` and ` + "`nakama aws kirocrew`" + ` add their forwards to the
same daemon, on a free local port, and reach them through the nakama
router (ADR 0029).

-L specs take <local>:<remote> (to the instance itself) or
<local>:<host>:<remote> (through the instance to another host).

Examples:
  nakama aws tunnel dev01 -L 8080:8080
  nakama aws tunnel start dev01 -L 8080:8080 -L 5432:db.internal:5432
  nakama aws tunnel status dev01
  nakama aws tunnel restart dev01
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

	restart := &cobra.Command{
		Use:   "restart <name>",
		Short: "Restart port forwarding with the same forwards (e.g. after re-login)",
		Long: `Stop the tunnel daemon (if it is still alive) and start a new one
with the forwards recorded in the tunnel state file (including those
` + "`open`" + ` and ` + "`kirocrew`" + ` added). The new daemon picks up this
process's current AWS credentials and nakama version. A running daemon
already retries after credential errors by itself, so this is mostly
for a daemon started by an older nakama, or for dead forwards. To
change the forwards instead, use stop and start.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			name := args[0]
			st, err := service.NewService(clients).RestartTunnel(ctx, name)
			if err != nil {
				var notRunning *service.NoTunnelError
				if errors.As(err, &notRunning) {
					return fmt.Errorf("no tunnel state for %s to restart; start one with `nakama aws tunnel %s -L <local>:<remote>`", name, name)
				}
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "tunnel restarted for %s (pid %d):\n", name, st.PID)
			_, _ = fmt.Fprint(cmd.OutOrStdout(), formatTunnelStatus(st, time.Now()))
			return nil
		},
	}

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
		Use:   "status [name]",
		Short: "Show the state of each port forward (all instances when name is omitted)",
		Long: `Show every port forward and its state (connecting / active /
reconnecting / idle / dead), and the nakama router with the addresses
of the code-server and Kiro Crew forwards
(http://<code-server|kirocrew>.<name>.nakama.localhost:<port>/).

With no name, list the tunnels of all instances recorded on this machine
(same convention as ` + "`nakama aws backup list`" + `), adding a NAME column.
Instances whose daemon no longer answers are reported as stale and their
leftover state file is cleaned up.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			// Status needs no AWS credentials: it reads the local state
			// files and asks each daemon over its loopback control
			// channel. That matters exactly when credentials have
			// expired and the user wants to see which forwards died.
			svc := service.NewService(nil)
			// The router section is best-effort: a router that cannot be
			// read is shown as not running.
			router, err := svc.GetRouterStatus(ctx)
			if err != nil {
				router = nil
			}
			if len(args) == 0 {
				items, err := svc.ListTunnelStatus(ctx)
				if err != nil {
					return err
				}
				var statuses []*service.TunnelStatus
				for _, it := range items {
					if it.Status != nil {
						statuses = append(statuses, it.Status)
					}
				}
				return renderOutput(cmd.OutOrStdout(), global.Output, items,
					formatTunnelOverview(items, time.Now())+formatRouterSection(router, statuses, true))
			}
			st, err := svc.GetTunnelStatus(ctx, args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), formatTunnelStatus(st, time.Now())+
				formatRouterSection(router, []*service.TunnelStatus{st}, false))
			return nil
		},
	}

	suggest := &cobra.Command{
		Use:   "suggest <name>",
		Short: "List the ports listening on the instance and the -L specs for them",
		Long: `Inspect which TCP ports are listening on the instance (read-only: it
runs ss/netstat over SSM Run Command) and print the ` + "`-L`" + ` specs that
would forward them, plus one ready-to-run tunnel command. Nothing is
forwarded automatically.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			sug, err := service.NewService(clients).SuggestTunnel(ctx, args[0])
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, sug, formatTunnelSuggestion(sug))
		},
	}

	cmd.AddCommand(start, restart, stop, status, suggest)
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
	fmt.Fprintf(&b, "%-26s  %-12s  %-10s  %s\n", "FORWARD", "STATE", "SINCE", "DETAIL")
	for _, f := range st.Forwards {
		since := "-"
		if !f.Since.IsZero() {
			since = now.Sub(f.Since).Round(time.Second).String()
		}
		fmt.Fprintf(&b, "%-26s  %-12s  %-10s  %s\n", forwardLabel(f), string(f.State), since, f.Detail)
	}
	return b.String()
}

// forwardLabel is the FORWARD column: the -L spec, plus what nakama
// opened it for (`open` / `kirocrew`).
func forwardLabel(f service.ForwardStatus) string {
	if f.Purpose == service.ForwardPurposeNone {
		return f.Spec
	}
	return f.Spec + " (" + string(f.Purpose) + ")"
}

// formatTunnelOverview renders the cross-instance listing (NAME column,
// same shape as `backup list` without a name).
func formatTunnelOverview(items []service.TunnelOverview, now time.Time) string {
	var rows int
	for _, it := range items {
		if it.Status != nil {
			rows += len(it.Status.Forwards)
		} else {
			rows++
		}
	}
	if rows == 0 {
		return "no tunnels\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s  %-26s  %-12s  %-10s  %s\n", "NAME", "FORWARD", "STATE", "SINCE", "DETAIL")
	for _, it := range items {
		switch {
		case it.Status != nil:
			for _, f := range it.Status.Forwards {
				since := "-"
				if !f.Since.IsZero() {
					since = now.Sub(f.Since).Round(time.Second).String()
				}
				fmt.Fprintf(&b, "%-16s  %-26s  %-12s  %-10s  %s\n", it.Name, forwardLabel(f), string(f.State), since, f.Detail)
			}
		case it.Stale:
			fmt.Fprintf(&b, "%-16s  %-26s  %-12s  %-10s  %s\n", it.Name, "-", "stale", "-",
				"the daemon does not answer; leftover state removed")
		default:
			fmt.Fprintf(&b, "%-16s  %-26s  %-12s  %-10s  %s\n", it.Name, "-", "error", "-", it.Err)
		}
	}
	return b.String()
}

// formatTunnelSuggestion renders the listening ports and the ready-made
// tunnel command.
func formatTunnelSuggestion(s *service.TunnelSuggestion) string {
	if len(s.Ports) == 0 {
		return "no listening TCP ports found on " + s.Name + "\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s  %-22s  %-14s  %-12s  %s\n", "PORT", "ADDRESS", "PROCESS", "SPEC", "NOTE")
	for _, p := range s.Ports {
		fmt.Fprintf(&b, "%-8d  %-22s  %-14s  %-12s  %s\n", p.Port, p.Address, dashIfEmpty(p.Process), p.Spec, p.Label)
	}
	if s.Command != "" {
		fmt.Fprintf(&b, "\nForward all of them:\n    %s\n", s.Command)
	}
	return b.String()
}

// formatRouterSection renders the router line of `tunnel status` and
// the browser addresses of the forwards the router serves. It is empty
// when none of the forwards is a browser forward and the router does
// not run. withName adds the instance name to each address line.
func formatRouterSection(router *service.RouterStatus, statuses []*service.TunnelStatus, withName bool) string {
	type entry struct{ name, url string }
	var urls []entry
	browser := false
	for _, st := range statuses {
		for _, f := range st.Forwards {
			if f.Purpose != service.ForwardPurposeNone {
				browser = true
			}
			if f.URL != "" {
				urls = append(urls, entry{st.Name, f.URL})
			}
		}
	}
	if router == nil {
		if !browser {
			return ""
		}
		return "\nrouter: not running; `nakama aws open <name>` and `nakama aws kirocrew <name>` start it\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nrouter: running on localhost:%d (pid %d)\n", router.Port, router.PID)
	if len(urls) > 0 {
		b.WriteString("open in a browser:\n")
		for _, u := range urls {
			if withName {
				fmt.Fprintf(&b, "  %-16s  %s\n", u.name, u.url)
			} else {
				fmt.Fprintf(&b, "  %s\n", u.url)
			}
		}
	}
	return b.String()
}

// dashIfEmpty keeps table columns aligned for missing values.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
