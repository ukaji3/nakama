// Package cli builds the nakama command tree using cobra.
//
// Sub-package layout:
//
//	cli/
//	  root.go           // root command and global flags
//	  aws.go            // `nakama aws` parent command
//	  aws_init.go       // `nakama aws init`
//	  aws_acl.go        // `nakama aws acl {add|list|delete}`
//	  aws_create.go     // `nakama aws create`
//	  aws_list.go       // `nakama aws list`
//	  aws_info.go       // `nakama aws info`
//	  aws_lifecycle.go  // `nakama aws {start|stop}`
//	  aws_delete.go     // `nakama aws delete`
//	  aws_edit.go       // `nakama aws edit`
//	  version.go        // `nakama version`
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
	// DryRun suppresses every mutating AWS API call.
	DryRun bool
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
		Short: "Manage AWS-hosted code-server instances behind CloudFront VPC Origins",
		Long: `nakama provisions and manages cloud development environments on AWS.

Each instance runs coder/code-server on a private-subnet EC2 instance and
is exposed over HTTPS/WSS through a CloudFront distribution that uses the
VPC Origins feature. Access from the public internet is restricted by an
AWS WAFv2 IP set managed via the ` + "`nakama aws acl`" + ` subcommands.`,
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			configureLogger(cmd.ErrOrStderr(), global.Verbose)
			return nil
		},
	}

	pf := root.PersistentFlags()
	pf.BoolVarP(&global.Verbose, "verbose", "v", false, "Enable debug-level logging")
	pf.BoolVar(&global.DryRun, "dry-run", false, "Suppress all mutating AWS calls and print intended actions")
	pf.StringVarP(&global.Output, "output", "o", "table", "Output format: table | json | yaml")

	root.AddCommand(
		newAWSCommand(),
		newVersionCommand(),
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
