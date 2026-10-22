package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		Short: "Add a source IP to the allowlist (auto-detects caller's IPs if omitted)",
		Long: `Adds an IPv4 address or CIDR block to the WAFv2 IP set
` + "`nakama-allowed-ips`" + `. /32 is appended automatically when only a single
IP is given.

When no argument is supplied, nakama detects the caller's public IPv4
address: it asks https://checkip.amazonaws.com/ and https://ifconfig.io/ip
three times each, every time over a new connection (direct connections
use IPv4, as "curl -4" does), and adds every distinct address it gets.
A network with redundant exits can show a different address per
connection, and a browser can leave by any of them. Answers that are not
public IPv4 addresses are ignored. If some checks fail, it warns and adds
what the others found; if all fail, it adds nothing.`,
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
			res, err := svc.AddACL(ctx, input)
			if err != nil {
				return err
			}
			warnFailedIPChecks(cmd.ErrOrStderr(), res.Detection,
				"if your network has more than one exit, one may be missing; run \"nakama aws acl add\" again or add its address")
			data, text := aclAddOutput(res.Added)
			return renderOutput(cmd.OutOrStdout(), global.Output, data, text)
		},
	}
}

// aclAddOutput is the structured and the text output of "acl add". One
// entry keeps the output from before Issue #104 ({"added": "<cidr>"});
// several make a list, as "acl delete" does.
func aclAddOutput(added []string) (any, string) {
	text := "added: " + strings.Join(added, "\nadded: ")
	if len(added) == 1 {
		return map[string]string{"added": added[0]}, text
	}
	return map[string][]string{"added": added}, text
}

// warnFailedIPChecks prints a warning for each check site that failed
// some of the IP checks, then what that may mean (consequence).
func warnFailedIPChecks(w io.Writer, det *awsx.DetectedIPs, consequence string) {
	lines := det.FailureSummary()
	if len(lines) == 0 {
		return
	}
	for _, l := range lines {
		_, _ = fmt.Fprintf(w, "warning: IP check: %s\n", l)
	}
	_, _ = fmt.Fprintf(w, "warning: %s\n", consequence)
}

// yourIPs names the caller's detected IPs: "your IP A" or "one of your
// IPs A, B".
func yourIPs(ips []string) string {
	if len(ips) == 1 {
		return "your IP " + ips[0]
	}
	return "one of your IPs " + strings.Join(ips, ", ")
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

// aclDeleteAll is the argument that selects every entry.
const aclDeleteAll = "all"

func newAWSACLDeleteCommand() *cobra.Command {
	var keepMine, yes bool
	cmd := &cobra.Command{
		Use:   "delete <IP|CIDR>... | all",
		Short: "Remove source IPs from the allowlist (\"all\" removes every entry)",
		Long: `Removes one or more IPv4 addresses or CIDR blocks from the allowlist
in a single update. If any of them is not in the allowlist, nothing is
removed.

"delete all" removes every entry, and the added-at records with them.
Browsers then get 403 from every nakama environment (Kiro Crew included),
including yours, until an address is added again with
"nakama aws acl add"; connect, ssh and tunnel do not use the allowlist.
It lists what it removes and asks first (--yes skips the question).
With --keep-mine, entries that contain one of your current public IPs
(detected as "acl add" does) are kept.`,
		Example: `  nakama aws acl delete 203.0.113.10
  nakama aws acl delete 203.0.113.10 198.51.100.0/24
  nakama aws acl delete all --keep-mine
  nakama aws acl delete all --yes`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			all, err := aclDeleteMode(args, keepMine)
			if err != nil {
				return err
			}
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			svc := service.NewService(clients)
			if all {
				return runACLDeleteAll(ctx, cmd, svc, keepMine, yes)
			}
			deleted, err := svc.DeleteACLs(ctx, args)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				// Unchanged single-entry output.
				return renderOutput(cmd.OutOrStdout(), global.Output,
					map[string]string{"deleted": args[0]}, "deleted: "+args[0])
			}
			return renderOutput(cmd.OutOrStdout(), global.Output,
				map[string][]string{"deleted": deleted}, "deleted: "+strings.Join(deleted, "\ndeleted: "))
		},
	}
	cmd.Flags().BoolVar(&keepMine, "keep-mine", false,
		"With \"all\": keep the entries that contain one of your current public IPs")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt of \"delete all\"")
	return cmd
}

// aclDeleteMode validates the arguments and reports whether they ask
// for "delete all".
func aclDeleteMode(args []string, keepMine bool) (bool, error) {
	for _, a := range args {
		if a == aclDeleteAll {
			if len(args) > 1 {
				return false, errors.New(`"all" cannot be combined with IP addresses`)
			}
			return true, nil
		}
	}
	if keepMine {
		return false, errors.New(`--keep-mine applies only to "delete all"`)
	}
	return false, nil
}

func runACLDeleteAll(ctx context.Context, cmd *cobra.Command, svc *service.Service, keepMine, yes bool) error {
	plan, err := svc.PlanACLDeleteAll(ctx, keepMine)
	if err != nil {
		return err
	}
	if plan.KeepMine {
		warnFailedIPChecks(cmd.ErrOrStderr(), plan.Detection,
			"if your network has more than one exit, the entries for one that was not detected are deleted too")
	}
	if plan.KeepMine && len(plan.Keep) == 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: no entry contains %s, so --keep-mine keeps nothing\n", yourIPs(plan.MyIPs))
	}
	// Asking is about access; removing only stale added-at records
	// changes nobody's access.
	if len(plan.Delete) > 0 && !yes {
		if _, err := io.WriteString(cmd.OutOrStdout(), aclDeleteAllPrompt(plan)); err != nil {
			return err
		}
		ok, err := readYesNo()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
	}
	res, err := svc.DeleteACLAll(ctx, plan)
	if err != nil {
		return err
	}
	return renderOutput(cmd.OutOrStdout(), global.Output, res, aclDeleteAllSummary(plan, res))
}

// aclDeleteAllPrompt renders the confirmation question. Pure function:
// unit-tested.
func aclDeleteAllPrompt(p *service.ACLDeleteAllPlan) string {
	var b strings.Builder
	total := len(p.Delete) + len(p.Keep)
	switch {
	case len(p.Keep) > 0:
		fmt.Fprintf(&b, "Delete %d of %s from the allowlist (WAF IP set nakama-allowed-ips):\n", len(p.Delete), entryCount(total))
	case total == 1:
		b.WriteString("Delete the only entry of the allowlist (WAF IP set nakama-allowed-ips):\n")
	default:
		fmt.Fprintf(&b, "Delete all %d entries from the allowlist (WAF IP set nakama-allowed-ips):\n", total)
	}
	writeACLEntries(&b, p.Delete)
	if len(p.Keep) > 0 {
		fmt.Fprintf(&b, "Keep (contains %s):\n", yourIPs(p.MyIPs))
		writeACLEntries(&b, p.Keep)
		b.WriteString("Browsers at the deleted addresses will get 403 from every nakama environment (Kiro Crew included).\n")
	} else {
		b.WriteString("Afterwards every nakama environment (Kiro Crew included) answers 403 to every browser,\n" +
			"yours included, until an address is added with \"nakama aws acl add\".\n")
	}
	b.WriteString("connect, ssh and tunnel are not affected. [y/N]: ")
	return b.String()
}

// entryCount renders a count of allowlist entries ("1 entry", "2 entries").
func entryCount(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

func writeACLEntries(b *strings.Builder, entries []service.ACLEntry) {
	for _, e := range entries {
		added := e.AddedAt
		if added == "" {
			added = "(added outside nakama)"
		} else {
			added = "added " + added
		}
		fmt.Fprintf(b, "  %-20s  %s\n", e.CIDR, added)
	}
}

// aclDeleteAllSummary is the text output after "delete all".
func aclDeleteAllSummary(p *service.ACLDeleteAllPlan, r *service.ACLDeleteAllResult) string {
	var b strings.Builder
	for _, c := range r.Deleted {
		fmt.Fprintf(&b, "deleted: %s\n", c)
	}
	switch {
	case len(p.Delete) == 0 && len(p.Keep) == 0:
		b.WriteString("the allowlist is already empty")
	case len(r.Deleted) == 0 && len(r.Gone) == 0:
		fmt.Fprintf(&b, "nothing to delete: every entry contains %s", yourIPs(p.MyIPs))
	case len(r.Remaining) == 0:
		fmt.Fprintf(&b, "deleted %s; the allowlist is now empty (add your IP with \"nakama aws acl add\")", entryCount(len(r.Deleted)))
	default:
		fmt.Fprintf(&b, "deleted %s; remaining: %s", entryCount(len(r.Deleted)), strings.Join(r.Remaining, ", "))
	}
	if len(r.Gone) > 0 {
		fmt.Fprintf(&b, "\nalready removed by someone else: %s", strings.Join(r.Gone, ", "))
	}
	if len(r.Deleted) == 0 && r.TimestampsRemoved > 0 {
		fmt.Fprintf(&b, "\nremoved %d added-at records left without an entry", r.TimestampsRemoved)
	}
	return b.String()
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
