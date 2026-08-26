package cli

import (
	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSTunnelDaemonCommand returns the hidden daemon process behind
// `nakama aws tunnel` (ADR 0017). It is an internal re-exec target,
// not a user-facing command: `tunnel start` spawns it detached from
// the terminal, and it supervises one SSM port-forwarding session per
// --forward until told to stop over its control channel. Forward
// specs carry no secrets, so plain flags are fine (unlike the
// session-runner payload, which travels over an inherited pipe).
func newAWSTunnelDaemonCommand() *cobra.Command {
	var name string
	var forwards []string
	cmd := &cobra.Command{
		Use:    "tunnel-daemon",
		Short:  "Internal: background port-forwarding daemon (used by tunnel)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			specs, err := service.ParseForwardSpecs(forwards)
			if err != nil {
				return err
			}
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			return service.NewService(clients).RunTunnelDaemon(ctx, service.TunnelDaemonInput{
				Name:     name,
				Forwards: specs,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "nakama instance name")
	cmd.Flags().StringArrayVar(&forwards, "forward", nil, "port forward spec (repeatable)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("forward")
	return cmd
}
