package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSDoctorCommand returns `nakama aws doctor`: a read-only
// environment diagnosis that answers "why doesn't it work here?" in one
// command (Issue #53).
func newAWSDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose this machine's environment (proxy, TLS, credentials, WAF, SSM)",
		Long: `Check everything nakama needs and report each item as ok / warn / fail
with concrete advice:

  proxy           the resolved proxy (environment variables, or the
                  Windows WinHTTP/PAC settings) and AWS reachability
  tls             whether a TLS-intercepting proxy re-signs AWS traffic
                  and whether the OS trust store accepts it
  credentials     which account and identity nakama operates as
  waf-allowlist   whether this machine's public IP may reach code-server
  ssm-permission  whether the caller may use connect / tunnel / ssh
  tunnels         locally recorded tunnels and dead forwards

The command is strictly read-only: it changes no AWS resource and no
local configuration, and never prints credentials, tokens or passwords.
It exits non-zero when any check fails, so it is usable in scripts. Use
-o json to attach the report to a support request.`,
		Args: cobra.NoArgs,
		// The exit code carries the diagnosis; usage output and a
		// duplicated error line would only add noise.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			// Credential resolution failure is itself a finding, so the
			// diagnosis continues with the local checks.
			clients, clientErr := awsx.NewClients(ctx)
			report := service.Diagnose(ctx, service.DoctorOptions{Clients: clients, ClientErr: clientErr})

			format, err := cmd.Flags().GetString("output")
			if err != nil || format == "" {
				format = global.Output
			}
			if err := renderOutput(cmd.OutOrStdout(), format, report, renderDoctorText(report)); err != nil {
				return err
			}
			if report.Fails > 0 {
				// Silenced above: the report itself is the message.
				return &service.ExitCodeError{Code: 1}
			}
			return nil
		},
	}
	return cmd
}

// renderDoctorText renders the human-readable report. Multi-line
// details (AWS SDK errors often carry recovery hints on their own
// lines) are indented under the check so the columns stay readable.
func renderDoctorText(r *service.DoctorReport) string {
	var b strings.Builder
	for _, c := range r.Checks {
		lines := strings.Split(strings.TrimRight(c.Detail, "\n"), "\n")
		_, _ = fmt.Fprintf(&b, "%-6s %-15s %s\n", doctorMarker(c.Status), c.Name, lines[0])
		for _, l := range lines[1:] {
			_, _ = fmt.Fprintf(&b, "%-6s %-15s %s\n", "", "", strings.TrimSpace(l))
		}
		if c.Advice != "" {
			_, _ = fmt.Fprintf(&b, "%-6s %-15s -> %s\n", "", "", c.Advice)
		}
	}
	_, _ = fmt.Fprintf(&b, "\n%d ok, %d warn, %d fail", r.OKs, r.Warns, r.Fails)
	if r.Skips > 0 {
		_, _ = fmt.Fprintf(&b, ", %d skipped", r.Skips)
	}
	_, _ = fmt.Fprintln(&b)
	return b.String()
}

// doctorMarker is the leading status column.
func doctorMarker(s service.CheckStatus) string {
	switch s {
	case service.CheckOK:
		return "[ ok ]"
	case service.CheckWarn:
		return "[warn]"
	case service.CheckFail:
		return "[FAIL]"
	default:
		return "[skip]"
	}
}
