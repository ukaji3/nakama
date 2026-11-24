package cli

import (
	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/service"
)

// newAWSRouterDaemonCommand returns the hidden process behind the nakama
// router (Issue #124, ADR 0029). It is an internal re-exec target, not
// a user-facing command: `code` and `kirocrew` spawn it detached from
// the terminal, and it routes
// http://<code-server|kirocrew>.<instance>.nakama.localhost:<port>/ to
// the tunnel daemons' browser forwards until it is told to stop, or no
// tunnel daemon has run for a minute. It needs no AWS credentials.
func newAWSRouterDaemonCommand() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:    "router-daemon",
		Short:  "Internal: the local router of the browser forwards (used by code and kirocrew)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return service.RunRouterDaemon(withCancel(cmd), service.RouterDaemonInput{Port: port})
		},
	}
	cmd.Flags().IntVar(&port, "port", service.DefaultRouterPort, "router port on 127.0.0.1 and ::1")
	return cmd
}
