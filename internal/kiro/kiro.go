// Package kiro carries the one copy of the shell steps that install
// Kiro CLI and Kiro Crew on a nakama instance (Issue #68).
//
// Two call sites need the exact same steps and must not drift:
//
//   - internal/userdata embeds them in the cloud-init user-data, where
//     they run on every create unless `--no-kiro` is given.
//   - internal/service exposes them as the `kiro` provisioning preset,
//     so `nakama aws setup rerun <name> --preset kiro` can re-run them
//     on an existing instance.
//
// The fragment assumes it is executed as root by a bash script that
// may have `set -e` in effect; it drops to the ubuntu user itself and
// swallows its own failures (see install.sh).
package kiro

import _ "embed"

//go:embed install.sh
var installScript string

// InstallScript returns the shell fragment that installs Kiro CLI and
// Kiro Crew for the ubuntu user. The returned text carries no shebang
// and never aborts the script that includes it: a failing install is
// reported as a warning, because the instance is usable without it.
func InstallScript() string { return installScript }
