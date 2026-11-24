package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// deprecatedCommand turns cmd, a fresh instance of a renamed command,
// into the command under its old name (Issue #129, ADR 0030). The old
// name keeps working with the same flags and arguments, but cobra
// hides it from help and completion and, when it runs, prints
// `Command "<old>" is deprecated, use "<replacement>" instead` to
// stderr, so stdout (including -o json) is unchanged. The old names
// work through v0.30.x and are removed in a later release.
func deprecatedCommand(cmd *cobra.Command, oldName, replacement string) *cobra.Command {
	fields := strings.Fields(cmd.Use)
	if len(fields) == 0 {
		fields = []string{oldName}
	} else {
		fields[0] = oldName
	}
	cmd.Use = strings.Join(fields, " ")
	cmd.Aliases = nil
	cmd.Deprecated = fmt.Sprintf("use %q instead", replacement)
	return cmd
}
