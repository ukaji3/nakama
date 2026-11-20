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
// `create --cloudfront` builds, or removes it again.

// cloudfrontBackend is the slice of the service layer the cloudfront
// commands use; tests substitute a fake.
type cloudfrontBackend interface {
	AddCloudFront(ctx context.Context, name string) (*service.AddCloudFrontOutput, error)
	PlanDeleteCloudFront(ctx context.Context, name string) (*service.DeleteCloudFrontPlan, error)
	DeleteCloudFront(ctx context.Context, p *service.DeleteCloudFrontPlan) (*service.DeleteCloudFrontOutput, error)
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
		Long: `Instances are reached over SSM by default (` + "`nakama aws open`" + `,
connect, ssh, tunnel) and have no internet entry path (ADR 0028). These
commands give an existing instance one, or take it away again:

  browser -> https://<distribution>.cloudfront.net (WAF allowlist)
          -> CloudFront VPC origin -> the instance's code-server

  nakama aws cloudfront add <name>       build it (as create --cloudfront does)
  nakama aws open --internet <name>      open code-server through it
  nakama aws cloudfront delete <name>    remove it (Kiro Crew's too)

` + "`nakama aws info`" + ` and ` + "`nakama aws list`" + ` show whether an instance has it.`,
	}
	var yes bool
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove the CloudFront access of an instance (idempotent; the instance is kept)",
		Long: `Removes the instance's internet entry path and keeps the instance:

  1. the security group's inbound rule from the CloudFront managed prefix
     list, first, so nothing reaches the instance through CloudFront
     from then on, even if a later step fails
  2. the Kiro Crew distribution (` + "`kirocrew enable`" + `), when there is one
  3. the code-server distribution (disabled, then deleted)
  4. the Kiro Crew redirect function, and the VPC origin
  5. with Kiro Crew web access and a running instance, its nginx site and
     code-server proxy settings on the instance
  6. the records under /nakama/<name>/meta/

Disabling a distribution and deleting the VPC origin take several
minutes each; the command waits. It prompts first unless --yes is given.
The URLs stop working, and adding CloudFront again gives new ones.
code-server stays reachable over SSM (` + "`nakama aws open <name>`" + `).

Safe to re-run: it continues where a stopped run left off. On an instance
without CloudFront it only removes a leftover inbound rule (bare
instances created before ADR 0028 have one).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCloudFrontDelete(cmd, args[0], yes, deps)
		},
	}
	del.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")

	cmd.AddCommand(
		&cobra.Command{
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

Open code-server through it with ` + "`nakama aws open --internet <name>`" + `.
Kiro Crew's own distribution is separate: ` + "`nakama aws kirocrew enable`" + `.
CloudFront and WAF add request charges.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runCloudFrontAdd(cmd, args[0], deps)
			},
		},
		del,
	)
	return cmd
}

func runCloudFrontAdd(cmd *cobra.Command, name string, deps cloudfrontDeps) error {
	ctx := withCancel(cmd)
	backend, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(stderr, "adding CloudFront to %s; the VPC origin and the distribution take several minutes "+
		"each to deploy (~15-20 minutes in all), and the command waits\n", name)
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
	text := fmt.Sprintf("%s\n  name : %s\n  url  : %s\n\nOpen it with: nakama aws open --internet %s",
		head, out.Name, out.URL, out.Name)
	return renderOutput(cmd.OutOrStdout(), global.Output, out, text)
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
	fmt.Fprintf(&b, "code-server stays reachable over SSM (nakama aws open %s). [y/N]: ", p.Name)
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
			"in place (harmless); run `nakama aws start %s` and `nakama aws kirocrew disable %s` to remove them",
			out.Name, out.Name, out.Name)
	}
	if !p.Bare {
		fmt.Fprintf(&b, "\nOpen code-server over SSM with: nakama aws open %s", out.Name)
	}
	return b.String()
}
