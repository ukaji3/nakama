package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/version"
)

func newVersionCommand() *cobra.Command {
	return newVersionCommandWith(version.Checker{})
}

// newVersionCommandWith builds `nakama version` around checker, which
// the tests point at a local server.
func newVersionCommandWith(checker version.Checker) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long: `Print the version, commit and build date of this nakama binary.

With --check, ask GitHub (api.github.com) for the latest release instead,
and print this version, the latest one and the release page. Only
--check and ` + "`nakama aws doctor`" + ` contact GitHub. When the check fails (a
network that blocks api.github.com, the API rate limit), a warning is
printed and the exit code stays 0, as it does for an old version.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !check {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), version.String())
				return err
			}
			res := checker.Check(withCancel(cmd), version.Version)
			return writeVersionCheck(cmd.OutOrStdout(), cmd.ErrOrStderr(), res)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Ask GitHub whether a newer release is available")
	return cmd
}

// writeVersionCheck prints the outcome of `version --check`. A failed
// check is a warning on stderr, never an error.
func writeVersionCheck(stdout, stderr io.Writer, r version.CheckResult) error {
	if r.Status == version.StatusUnknown {
		_, err := fmt.Fprintf(stdout, "current : %s\n", r.Current)
		_, _ = fmt.Fprintf(stderr, "warning: could not check for a newer version: %v\n", r.Err)
		return err
	}
	var status string
	switch r.Status {
	case version.StatusUpToDate:
		status = "up to date"
	case version.StatusOutdated:
		status = "out of date; download " + r.Latest + " from the release page"
	case version.StatusAhead:
		status = "newer than the latest release"
	default:
		status = "development build; not compared with the latest release"
	}
	_, err := fmt.Fprintf(stdout, "current : %s\nlatest  : %s\nrelease : %s\nstatus  : %s\n",
		r.Current, r.Latest, r.URL, status)
	return err
}
