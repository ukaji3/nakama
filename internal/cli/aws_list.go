package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all nakama-managed code-server instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			rows, err := service.NewService(clients).ListInstances(ctx)
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, rows, formatInstanceTable(rows))
		},
	}
}

func formatInstanceTable(rows []service.InstanceSummary) string {
	if len(rows) == 0 {
		return "(no nakama instances)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s  %-19s  %-9s  %-12s  %s\n",
		"NAME", "INSTANCE_ID", "STATE", "TYPE", "DISTRIBUTION_DOMAIN")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-20s  %-19s  %-9s  %-12s  %s\n",
			r.Name, r.InstanceID, r.State, r.InstanceType, r.DistributionDomain)
	}
	return strings.TrimRight(b.String(), "\n")
}
