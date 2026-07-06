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
		"include the shared NAT instance (running/stopped) as a TYPE=NAT row")
	return cmd
}

func formatInstanceTable(rows []service.InstanceSummary) string {
	if len(rows) == 0 {
		return "(no nakama instances)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s  %-19s  %-9s  %-6s  %-12s  %s\n",
		"NAME", "INSTANCE_ID", "STATE", "TYPE", "INSTANCE_TYPE", "DISTRIBUTION_DOMAIN")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-20s  %-19s  %-9s  %-6s  %-12s  %s\n",
			r.Name, r.InstanceID, r.State, strings.ToUpper(r.Type), r.InstanceType, r.DistributionDomain)
	}
	return strings.TrimRight(b.String(), "\n")
}
