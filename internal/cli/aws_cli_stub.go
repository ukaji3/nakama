//go:build !windows

package cli

import "errors"

// spawnConfiguredShell is unreachable on non-Windows systems: the
// runCLIShell gate rejects them first. The stub keeps the package
// compiling everywhere.
func spawnConfiguredShell(_ []string, _ string) (int, error) {
	return -1, errors.New("`nakama aws cli shell` is a Windows-only option")
}
