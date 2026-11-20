package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

type createFlags struct {
	name              string
	instanceType      string
	codeServerVersion string
	showPassword      bool
	withAdmin         bool
	withRole          string
	setupScript       string
	dotfiles          string
	preset            string
	bare              bool
	noKiro            bool
	cloudFront        bool
}

// createInputFromFlags maps the parsed CLI flags to the service-layer
// input. Split out of RunE so the mapping is unit-testable (flag
// mutual exclusion itself is enforced by cobra via
// MarkFlagsMutuallyExclusive).
func createInputFromFlags(f *createFlags) service.CreateInstanceInput {
	return service.CreateInstanceInput{
		Name:              f.name,
		InstanceType:      f.instanceType,
		CodeServerVersion: f.codeServerVersion,
		WithAdmin:         f.withAdmin,
		WithRole:          f.withRole,
		Bare:              f.bare,
		NoKiro:            f.noKiro,
		CloudFront:        f.cloudFront,
		Provision: service.ProvisionSpec{
			SetupScript: f.setupScript,
			Dotfiles:    f.dotfiles,
			Preset:      f.preset,
		},
	}
}

func newAWSCreateCommand() *cobra.Command {
	f := &createFlags{}
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a new code-server EC2 instance (reached over SSM; CloudFront with --cloudfront)",
		Long: `Provisions:

  - A dedicated ENI in the private subnet (Private IP retained across stop/start)
  - A security group with no inbound rule
  - An EC2 instance running Ubuntu 26.04 with code-server installed via cloud-init
  - SSM Parameter Store entries for the password and instance metadata

Open code-server with ` + "`nakama aws open <name>`" + ` (an SSM port forward)
and terminals with connect / ssh / tunnel. Nothing on the instance is
reachable from the internet.

--cloudfront also builds an internet entry path, for
` + "`nakama aws open --internet`" + `:

  - An inbound rule on the security group from the CloudFront managed prefix list
  - A CloudFront VPC origin pointing at the EC2 instance
  - A CloudFront distribution attached to the WAF web ACL ` + "(`nakama-waf`)" + `

It takes ~15-20 minutes more (CloudFront propagation), and adds
CloudFront and WAF request charges. ` + "`nakama aws cloudfront add`" + ` builds the
same thing later, and ` + "`nakama aws cloudfront delete`" + ` removes it.

With --with-admin the instance gets a shared nakama-managed IAM role
carrying AdministratorAccess (for CDK/CloudFormation work from inside
code-server); with --with-role <role> an existing IAM role of your
choice is attached instead. Both preflight the caller's IAM
permissions via SimulatePrincipalPolicy and abort before creating any
resource when the permissions are insufficient.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			in := createInputFromFlags(f)
			// Fail fast on a bad preset name or an unreadable setup
			// script, before any resource is created.
			if err := service.ValidateProvisionSpec(in.Provision); err != nil {
				return err
			}
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			out, err := service.NewService(clients).CreateInstance(ctx, in)
			if err != nil {
				return err
			}
			// The generated password is not shown (or serialized) unless
			// explicitly requested, mirroring `info --show-password`, so
			// terminal scrollback and CI logs stay secret-free by default.
			passwordLine := "  password          : (use `nakama aws info " + out.Name + " --show-password`)"
			if f.showPassword {
				passwordLine = "  password          : " + out.Password
			} else {
				out.Password = ""
			}
			fallback := "instance created\n" +
				"  name              : " + out.Name + "\n" +
				"  instance_id       : " + out.InstanceID + "\n"
			switch {
			case out.Bare:
				// No code-server, so no URL and no password exist.
				fallback += "  kind              : bare (no code-server / CloudFront)\n" +
					"  access            : nakama aws ssh setup " + out.Name + "  |  nakama aws connect " + out.Name
			case out.CloudFront:
				fallback += "  url               : " + out.URL + "\n" +
					"  open              : nakama aws open " + out.Name + "  (over SSM; --internet for the URL above)\n" +
					passwordLine
			default:
				fallback += "  cloudfront        : none (add it with `nakama aws cloudfront add " + out.Name + "`)\n" +
					"  open              : nakama aws open " + out.Name + "  (over SSM)\n" +
					passwordLine
			}
			if out.IAMRole != "" {
				fallback += "\n  iam_role          : " + out.IAMRole
			}
			if out.InstanceProfile != "" {
				fallback += "\n  instance_profile  : " + out.InstanceProfile
			}
			if p := out.Provision; p != nil {
				fallback += "\n  provision         : " + strings.Join(p.Steps, ", ")
				if p.Failed {
					// The instance is usable; only provisioning failed.
					fallback += "\n  provision WARNING : " + p.Err +
						"\n                      log: " + p.LogPath + " on the instance" +
						"\n                      retry: nakama aws setup rerun " + out.Name + rerunFlags(f)
				}
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, out, fallback)
		},
	}
	cmd.Flags().StringVar(&f.name, "name", "", "Logical name of the instance (required, [a-z0-9][a-z0-9-]*)")
	cmd.Flags().StringVar(&f.instanceType, "instance-type", "", "EC2 instance type (required, e.g. t3.medium)")
	cmd.Flags().StringVar(&f.codeServerVersion, "code-server-version", "", "Pin a specific code-server version (default: latest)")
	cmd.Flags().BoolVar(&f.showPassword, "show-password", false, "Print the generated code-server password in plain text")
	cmd.Flags().BoolVar(&f.withAdmin, "with-admin", false, "Attach a shared nakama-managed IAM role with AdministratorAccess to the instance")
	cmd.Flags().StringVar(&f.withRole, "with-role", "", "Attach an existing IAM role (by name) to the instance")
	cmd.Flags().StringVar(&f.setupScript, "setup", "",
		"Shell script to run after create (local path or https URL), executed as ubuntu")
	cmd.Flags().StringVar(&f.dotfiles, "dotfiles", "",
		"Dotfiles repository to clone into ~/.dotfiles and apply (install.sh/setup.sh/bootstrap.sh, else symlinks)")
	cmd.Flags().BoolVar(&f.bare, "bare", false,
		"Create a lightweight box without code-server, CloudFront or a password (use connect/ssh/tunnel)")
	cmd.Flags().BoolVar(&f.noKiro, "no-kiro", false,
		"Skip the Kiro CLI / Kiro Crew install that is performed by default")
	cmd.Flags().BoolVar(&f.cloudFront, "cloudfront", false,
		"Also build the CloudFront entry path (VPC origin, distribution, WAF) used by `open --internet`")
	cmd.Flags().StringVar(&f.preset, "preset", "",
		"Bundled provisioning recipe to apply ("+strings.Join(service.ProvisionPresetNames(), ", ")+")")
	cmd.MarkFlagsMutuallyExclusive("with-admin", "with-role")
	// A bare instance has no code-server, so pinning its version would
	// be silently meaningless (Issue #61).
	cmd.MarkFlagsMutuallyExclusive("bare", "code-server-version")
	// Nor is there a code-server to put behind CloudFront (ADR 0028).
	cmd.MarkFlagsMutuallyExclusive("bare", "cloudfront")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("instance-type")
	return cmd
}

// rerunFlags renders the provisioning flags of this invocation so the
// warning can show a copy-pasteable rerun command.
func rerunFlags(f *createFlags) string {
	var b strings.Builder
	if f.preset != "" {
		b.WriteString(" --preset " + f.preset)
	}
	if f.dotfiles != "" {
		b.WriteString(" --dotfiles " + f.dotfiles)
	}
	if f.setupScript != "" {
		b.WriteString(" --setup " + f.setupScript)
	}
	return b.String()
}
