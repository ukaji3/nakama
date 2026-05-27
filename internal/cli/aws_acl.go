package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSACLCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "acl",
		Short: "Manage the WAFv2 IP set used to gate CloudFront access",
	}
	cmd.AddCommand(
		newAWSACLAddCommand(),
		newAWSACLListCommand(),
		newAWSACLDeleteCommand(),
	)
	return cmd
}

func newAWSACLAddCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add [IP|CIDR]",
		Short: "Add a source IP to the allowlist (auto-detects caller's IP if omitted)",
		Long: `Adds an IPv4 address or CIDR block to the WAFv2 IP set
` + "`nakama-allowed-ips`" + `. When no argument is supplied, nakama queries
` + "https://checkip.amazonaws.com/" + ` to determine the caller's public
IP address. /32 is appended automatically when only a single IP is given.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			input := ""
			if len(args) == 1 {
				input = args[0]
			}
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			svc := service.NewService(clients)
			canonical, err := svc.AddACL(ctx, input)
			if err != nil {
				return err
			}
			res := map[string]string{"added": canonical}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, "added: "+canonical)
		},
	}
}

func newAWSACLListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List allowlisted source IPs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			entries, err := service.NewService(clients).ListACL(ctx)
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, entries, formatACLTable(entries))
		},
	}
}

func newAWSACLDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <IP|CIDR>",
		Short: "Remove a source IP from the allowlist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			err = service.NewService(clients).DeleteACL(ctx, args[0])
			if err != nil {
				if errors.Is(err, service.ErrACLEntryNotFound) {
					return fmt.Errorf("%w: %s", err, args[0])
				}
				return err
			}
			res := map[string]string{"deleted": args[0]}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, "deleted: "+args[0])
		},
	}
}

func formatACLTable(entries []service.ACLEntry) string {
	if len(entries) == 0 {
		return "(allowlist is empty)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s  %s\n", "CIDR", "ADDED_AT")
	for _, e := range entries {
		fmt.Fprintf(&b, "%-20s  %s\n", e.CIDR, e.AddedAt)
	}
	return strings.TrimRight(b.String(), "\n")
}
