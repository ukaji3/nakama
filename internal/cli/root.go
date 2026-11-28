// Package cli builds the nakama command tree using cobra: root.go holds
// the root command and the global flags, and each `nakama aws <command>`
// lives in aws_<command>.go.
package cli

import (
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// GlobalFlags are the flags shared across every nakama command.
type GlobalFlags struct {
	// Verbose increases log verbosity (slog level Debug).
	Verbose bool
	// Output is the human/machine output format.
	Output string // table | json | yaml
}

// global is the singleton flag set; populated by cobra during parsing.
var global = &GlobalFlags{Output: "table"}

// NewRootCommand returns the configured `nakama` root command.
// It is exported so that tests can re-invoke a fresh command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "nakama",
		Short: "Manage AWS-hosted code-server instances in private subnets, reached over SSM",
		Long: `nakama provisions and manages cloud development environments on AWS.

Run nakama without arguments to open the control panel in your browser
(the same as ` + "`nakama ui`" + `); the commands below do the same from the command
line.

Each instance runs coder/code-server on a private-subnet EC2 instance
with no inbound rule. The browser reaches it through an AWS Systems
Manager (SSM) Session Manager port forward (` + "`nakama aws code`" + `), and
terminals through SSM as well (shell, ssh, tunnel).

An instance can also get an internet entry path: a CloudFront
distribution that uses the VPC Origins feature, restricted by an AWS
WAFv2 IP set managed via the ` + "`nakama aws acl`" + ` subcommands. It is built
only on request (` + "`nakama aws create --cloudfront`" + `, ` + "`nakama aws cloudfront add`" + `).`,
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			configureLogger(cmd.ErrOrStderr(), global.Verbose)
			return nil
		},
		// Without arguments, in a terminal, nakama opens the control
		// panel (Issue #139, ADR 0031); scripts and pipes get the help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isInteractive() {
				return cmd.Help()
			}
			return runUI(withCancel(cmd), cmd.ErrOrStderr(), "", defaultUIDeps())
		},
	}

	pf := root.PersistentFlags()
	pf.BoolVarP(&global.Verbose, "verbose", "v", false, "Enable debug-level logging")
	pf.StringVarP(&global.Output, "output", "o", "table", "Output format: table | json | yaml")

	root.AddCommand(
		newAWSCommand(),
		newVersionCommand(),
		newUICommand(),
	)

	return root
}

// configureLogger installs the default slog logger.
func configureLogger(w io.Writer, verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	if w == nil {
		w = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)
}
