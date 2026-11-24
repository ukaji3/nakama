package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSListCommand() *cobra.Command {
	var showNAT bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all nakama-managed code-server instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			rows, err := service.NewService(clients).ListInstances(ctx, showNAT)
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, rows, formatInstanceTable(rows))
		},
	}
	cmd.Flags().BoolVar(&showNAT, "show-nat", false,
		"Include the shared NAT instance (TYPE=NAT, running/stopped) and the NAT gateway (TYPE=NATGW) "+
			"with the egress IP address")
	return cmd
}

func formatInstanceTable(rows []service.InstanceSummary) string {
	if len(rows) == 0 {
		return "(no nakama instances)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s  %-21s  %-9s  %-6s  %-13s  %s\n",
		"NAME", "INSTANCE_ID", "STATE", "TYPE", "INSTANCE_TYPE", "DISTRIBUTION_DOMAIN")
	for _, r := range rows {
		last := r.DistributionDomain
		if last == "" && r.PublicIP != "" {
			// The NAT rows have no distribution; show where the
			// instances reach the internet from instead.
			last = "egress " + r.PublicIP
		}
		fmt.Fprintf(&b, "%-20s  %-21s  %-9s  %-6s  %-13s  %s\n",
			r.Name, r.InstanceID, r.State, strings.ToUpper(r.Type), r.InstanceType, last)
	}
	return strings.TrimRight(b.String(), "\n")
}
