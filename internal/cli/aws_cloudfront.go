package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/service"
)

// `nakama aws cloudfront add|delete <name>` (Issue #120, ADR 0028)
// gives an existing instance the CloudFront entry path that
// `create --cloudfront` builds, or removes it again. With --kirocrew
// (Issue #129) they also build, or only remove, Kiro Crew's own
// distribution, which `kirocrew enable|disable` did before.

// cloudfrontBackend is the slice of the service layer the cloudfront
// commands use; tests substitute a fake.
type cloudfrontBackend interface {
	AddCloudFront(ctx context.Context, name string) (*service.AddCloudFrontOutput, error)
	PlanDeleteCloudFront(ctx context.Context, name string) (*service.DeleteCloudFrontPlan, error)
	DeleteCloudFront(ctx context.Context, p *service.DeleteCloudFrontPlan) (*service.DeleteCloudFrontOutput, error)
	EnableKiroCrewWeb(ctx context.Context, name string) (*service.EnableKiroCrewWebOutput, error)
	DisableKiroCrewWeb(ctx context.Context, name string) (*service.DisableKiroCrewWebOutput, error)
	allowlistChecker
}

// awsCloudFrontBackend is the production cloudfrontBackend.
type awsCloudFrontBackend struct {
	*service.Service
	ip awsx.IPDiscoverer
}

// PublicIPv4 delegates to the awsx IP discoverer.
func (b awsCloudFrontBackend) PublicIPv4(ctx context.Context) (string, error) {
	return b.ip.PublicIPv4(ctx)
}

type cloudfrontDeps struct {
	connect func(ctx context.Context) (cloudfrontBackend, error)
	// confirm reads the y/N answer (stdin in production).
	confirm func() (bool, error)
}

func defaultCloudfrontDeps() cloudfrontDeps {
	return cloudfrontDeps{
		connect: func(ctx context.Context) (cloudfrontBackend, error) {
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return nil, err
			}
			return awsCloudFrontBackend{Service: service.NewService(clients), ip: clients.IPDiscoverer}, nil
		},
		confirm: readYesNo,
	}
}

func newAWSCloudFrontCommand() *cobra.Command {
	return newAWSCloudFrontCommandWithDeps(defaultCloudfrontDeps())
}

func newAWSCloudFrontCommandWithDeps(deps cloudfrontDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloudfront",
		Short: "Add or delete the CloudFront access of an existing instance",
		Long: `Instances are reached over SSM by default (` + "`nakama aws code`" + `,
shell, ssh, tunnel) and have no internet entry path (ADR 0028). These
commands give an existing instance one, or take it away again:

  browser -> https://<distribution>.cloudfront.net (WAF allowlist)
          -> CloudFront VPC origin -> the instance's code-server

  nakama aws cloudfront add <name>                  build it (as create --cloudfront does)
  nakama aws cloudfront add <name> --kirocrew       build it with Kiro Crew's own distribution too
  nakama aws code --internet <name>                 open code-server through it
  nakama aws kirocrew --internet <name>             open Kiro Crew through it (needs --kirocrew)
  nakama aws cloudfront delete <name> --kirocrew    remove only Kiro Crew's
  nakama aws cloudfront delete <name>               remove it (Kiro Crew's too)

` + "`nakama aws info`" + ` and ` + "`nakama aws list`" + ` show whether an instance has it.`,
	}
	var (
		yes            bool
		addKiroCrew    bool
		deleteKiroCrew bool
	)
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove the CloudFront access of an instance (idempotent; the instance is kept)",
		Long: `Removes the instance's internet entry path and keeps the instance:

  1. the security group's inbound rule from the CloudFront managed prefix
     list, first, so nothing reaches the instance through CloudFront
     from then on, even if a later step fails
  2. the Kiro Crew distribution (` + "`add --kirocrew`" + `), when there is one
  3. the code-server distribution (disabled, then deleted)
  4. the Kiro Crew redirect function, and the VPC origin
  5. with Kiro Crew web access and a running instance, its nginx site and
     code-server proxy settings on the instance
  6. the records under /nakama/<name>/meta/

Disabling a distribution and deleting the VPC origin take several
minutes each; the command waits. It prompts first unless --yes is given.
The URLs stop working, and adding CloudFront again gives new ones.
code-server stays reachable over SSM (` + "`nakama aws code <name>`" + `).

--kirocrew removes only Kiro Crew's access and keeps code-server's: the
redirect from code-server's /proxy/5476/ links, the CloudFront function
and Kiro Crew's distribution, and, when the instance is running, the
nginx site and the code-server proxy settings on it. It prompts first
too, when there is a distribution to remove. Kiro Crew stays reachable
over SSM (` + "`nakama aws kirocrew <name>`" + `).

Safe to re-run: it continues where a stopped run left off. On an instance
without CloudFront it only removes a leftover inbound rule (bare
instances created before ADR 0028 have one).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if deleteKiroCrew {
				return runCloudFrontDeleteKiroCrew(cmd, args[0], yes, deps)
			}
			return runCloudFrontDelete(cmd, args[0], yes, deps)
		},
	}
	del.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	del.Flags().BoolVar(&deleteKiroCrew, "kirocrew", false,
		"Remove only the Kiro Crew distribution and its parts on the instance; keep the code-server one")

	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Give an instance CloudFront access (idempotent)",
		Long: `Builds the instance's internet entry path, as ` + "`create --cloudfront`" + `
does:

  1. an inbound rule on the instance's security group: code-server's
     port (8080/tcp) from the CloudFront managed prefix list
  2. a CloudFront VPC origin pointing at the instance
  3. a CloudFront distribution in front of it, attached to the shared WAF
     web ACL ` + "(`nakama-waf`)" + `, so only addresses in the allowlist
     (` + "`nakama aws acl add`" + `) get through

The instance must be running. The VPC origin and the distribution each
take several minutes to deploy (~15-20 minutes in all); the command
waits. Safe to re-run: it continues where a stopped run left off and
never creates a second VPC origin or distribution.

Open code-server through it with ` + "`nakama aws code --internet <name>`" + `.

--kirocrew also builds Kiro Crew's own distribution, which
` + "`nakama aws kirocrew --internet <name>`" + ` opens (ADR 0020): a CloudFront
distribution in front of the same VPC origin, a loopback-only nginx proxy
on the instance, code-server's host-based proxy for that distribution,
and a redirect of code-server's /proxy/5476/ links to it. code-server
restarts once (open tabs reconnect), and it takes up to ~15 minutes
more. On an instance that already has CloudFront, it adds only Kiro
Crew's part. CloudFront and WAF add request charges.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCloudFrontAdd(cmd, args[0], addKiroCrew, deps)
		},
	}
	add.Flags().BoolVar(&addKiroCrew, "kirocrew", false,
		"Also build the Kiro Crew distribution used by kirocrew --internet")

	cmd.AddCommand(add, del)
	return cmd
}

// cloudFrontAddResult is the machine-readable output of
// `cloudfront add --kirocrew`: the fields of a plain add, plus Kiro
// Crew's part.
type cloudFrontAddResult struct {
	*service.AddCloudFrontOutput `yaml:",inline"`
	KiroCrew                     *service.EnableKiroCrewWebOutput `json:"kirocrew" yaml:"kirocrew"`
}

func runCloudFrontAdd(cmd *cobra.Command, name string, kiroCrew bool, deps cloudfrontDeps) error {
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	if kiroCrew {
		_, _ = fmt.Fprintf(stderr, "adding CloudFront to %s, Kiro Crew's included; the VPC origin and each distribution "+
			"take several minutes to deploy (up to ~35 minutes in all), code-server restarts once, and the command waits\n", name)
	} else {
		_, _ = fmt.Fprintf(stderr, "adding CloudFront to %s; the VPC origin and the distribution take several minutes "+
			"each to deploy (~15-20 minutes in all), and the command waits\n", name)
	}
	out, err := backend.AddCloudFront(ctx, name)
	if err != nil {
		return err
	}
	// The URL answers 403 to an address outside the allowlist; say so
	// now rather than at the first open. Best-effort.
	warnIfNotAllowlisted(ctx, backend, stderr)

	head := "CloudFront added"
	if out.AlreadyPresent {
		head = "CloudFront already in place"
	}
	if !kiroCrew {
		text := fmt.Sprintf("%s\n  name : %s\n  url  : %s\n\nOpen it with: nakama aws code --internet %s",
			head, out.Name, out.URL, out.Name)
		return renderOutput(cmd.OutOrStdout(), global.Output, out, text)
	}
	crew, err := backend.EnableKiroCrewWeb(ctx, name)
	if err != nil {
		return fmt.Errorf("code-server's CloudFront access is in place (%s), but Kiro Crew's is not: %w\n"+
			"Run `nakama aws cloudfront add %s --kirocrew` again to continue", out.URL, err, name)
	}
	text := fmt.Sprintf("%s, Kiro Crew's included\n  name     : %s\n  url      : %s\n  kirocrew : %s\n\n"+
		"Open them with:\n  nakama aws code --internet %s\n  nakama aws kirocrew --internet %s",
		head, out.Name, out.URL, crew.URL, out.Name, out.Name)
	return renderOutput(cmd.OutOrStdout(), global.Output, cloudFrontAddResult{AddCloudFrontOutput: out, KiroCrew: crew}, text)
}

// runCloudFrontDeleteKiroCrew removes only Kiro Crew's CloudFront
// access (`cloudfront delete --kirocrew`, formerly `kirocrew disable`).
// It asks first when there is a distribution to remove, like a full
// delete; without one it only clears leftovers, as disable always did.
func runCloudFrontDeleteKiroCrew(cmd *cobra.Command, name string, yes bool, deps cloudfrontDeps) error {
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	plan, err := backend.PlanDeleteCloudFront(ctx, name)
	if err != nil {
		return err
	}
	hasCrew := plan.KiroCrewDistributionID != ""
	if hasCrew {
		if !yes {
			if _, err := io.WriteString(cmd.OutOrStdout(), kiroCrewDeletePrompt(plan)); err != nil {
				return err
			}
			ok, err := deps.confirm()
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("aborted")
			}
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "deleting Kiro Crew's CloudFront access of %s; disabling its distribution "+
			"takes several minutes, and the command waits\n", name)
	}
	out, err := backend.DisableKiroCrewWeb(ctx, name)
	if err != nil {
		return err
	}
	return renderOutput(cmd.OutOrStdout(), global.Output, out, kiroCrewDeleteSummary(name, hasCrew, out))
}

// kiroCrewDeletePrompt lists what `cloudfront delete --kirocrew` is
// about to remove. Pure function: unit-tested.
func kiroCrewDeletePrompt(p *service.DeleteCloudFrontPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Delete the Kiro Crew CloudFront access of %q?\n", p.Name)
	fmt.Fprintf(&b, "  kirocrew distribution : %s%s\n", p.KiroCrewDistributionID, domainSuffix(p.KiroCrewDomain))
	b.WriteString("Its URL stops working, and adding it again gives a new one. code-server's CloudFront access is kept.\n")
	fmt.Fprintf(&b, "Kiro Crew stays reachable over SSM (nakama aws kirocrew %s). [y/N]: ", p.Name)
	return b.String()
}

// kiroCrewDeleteSummary is the text output after
// `cloudfront delete --kirocrew`.
func kiroCrewDeleteSummary(name string, hadCrew bool, out *service.DisableKiroCrewWebOutput) string {
	text := fmt.Sprintf("Kiro Crew's CloudFront access deleted for %s; code-server's is kept", name)
	if !hadCrew {
		text = fmt.Sprintf("%s had no Kiro Crew distribution; any leftover parts of it were removed", name)
	}
	if out.InstanceSkipped {
		text += fmt.Sprintf("\nnote: %s is not running, so its nginx site and code-server proxy settings were left in place "+
			"(harmless); run `nakama aws start %s` and `nakama aws cloudfront delete %s --kirocrew` to remove them",
			name, name, name)
	}
	return text + fmt.Sprintf("\nOpen Kiro Crew over SSM with: nakama aws kirocrew %s", name)
}

func runCloudFrontDelete(cmd *cobra.Command, name string, yes bool, deps cloudfrontDeps) error {
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	plan, err := backend.PlanDeleteCloudFront(ctx, name)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	if plan.HasCloudFront() {
		if !yes {
			if _, err := io.WriteString(cmd.OutOrStdout(), cloudFrontDeletePrompt(plan)); err != nil {
				return err
			}
			ok, err := deps.confirm()
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("aborted")
			}
		}
		_, _ = fmt.Fprintf(stderr, "deleting CloudFront of %s; disabling a distribution and deleting the VPC origin "+
			"take several minutes each, and the command waits\n", name)
	}
	out, err := backend.DeleteCloudFront(ctx, plan)
	if err != nil {
		return err
	}
	return renderOutput(cmd.OutOrStdout(), global.Output, out, cloudFrontDeleteSummary(plan, out))
}

// cloudFrontDeletePrompt lists what is about to go. Pure function:
// unit-tested.
func cloudFrontDeletePrompt(p *service.DeleteCloudFrontPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Delete the CloudFront access of %q?\n", p.Name)
	if p.DistributionID != "" {
		fmt.Fprintf(&b, "  distribution          : %s%s\n", p.DistributionID, domainSuffix(p.DistributionDomain))
	}
	if p.KiroCrewDistributionID != "" {
		fmt.Fprintf(&b, "  kirocrew distribution : %s%s\n", p.KiroCrewDistributionID, domainSuffix(p.KiroCrewDomain))
	}
	if p.VPCOriginID != "" {
		fmt.Fprintf(&b, "  vpc origin            : %s\n", p.VPCOriginID)
	}
	if p.SecurityGroupID != "" {
		fmt.Fprintf(&b, "  security group rule   : inbound %d/tcp from the CloudFront managed prefix list (%s)\n",
			config.CodeServerPort, p.SecurityGroupID)
	}
	b.WriteString("The URLs stop working, and adding CloudFront again gives new ones.\n")
	fmt.Fprintf(&b, "code-server stays reachable over SSM (nakama aws code %s). [y/N]: ", p.Name)
	return b.String()
}

func domainSuffix(domain string) string {
	if domain == "" {
		return ""
	}
	return " (https://" + domain + ")"
}

// cloudFrontDeleteSummary is the text output after a delete.
func cloudFrontDeleteSummary(p *service.DeleteCloudFrontPlan, out *service.DeleteCloudFrontOutput) string {
	var b strings.Builder
	switch {
	case p.HasCloudFront():
		fmt.Fprintf(&b, "CloudFront deleted for %s", out.Name)
		if out.KiroCrewDistributionID != "" {
			b.WriteString(" (Kiro Crew's included)")
		}
	case out.IngressRuleRemoved:
		fmt.Fprintf(&b, "%s has no CloudFront; removed the leftover inbound rule from its security group", out.Name)
	default:
		fmt.Fprintf(&b, "%s has no CloudFront; nothing to delete", out.Name)
	}
	if out.InstanceSkipped {
		fmt.Fprintf(&b, "\nnote: %s is not running, so its Kiro Crew nginx site and code-server proxy settings were left "+
			"in place (harmless); run `nakama aws start %s` and `nakama aws cloudfront delete %s --kirocrew` to remove them",
			out.Name, out.Name, out.Name)
	}
	if !p.Bare {
		fmt.Fprintf(&b, "\nOpen code-server over SSM with: nakama aws code %s", out.Name)
	}
	return b.String()
}
