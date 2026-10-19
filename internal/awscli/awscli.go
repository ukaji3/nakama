// Package awscli carries the one copy of the shell steps that install,
// update and run the AWS CLI v2 on a nakama instance (Issue #101).
//
// The cloud-init user-data (code-server and bare), the restore user-data
// and the `nakama aws update` scripts all include them, so the install
// location and the way root scripts call the CLI cannot drift apart.
//
// Both fragments only define shell functions; the including script, run
// as root by bash, calls them (see install.sh and run.sh).
package awscli

import _ "embed"

//go:embed install.sh
var installScript string

//go:embed run.sh
var runScript string

// InstalledBin is the AWS CLI the official installer puts in the ubuntu
// user's home; its presence tells an official install from a snap one.
const InstalledBin = "/home/ubuntu/.local/aws-cli/v2/current/bin/aws"

// InstallScript returns the fragment that defines
// nakama_install_awscli: install, or update, the AWS CLI v2 for the
// ubuntu user with AWS's install script. The function returns non-zero
// when the install fails.
func InstallScript() string { return installScript }

// RunScript returns the fragment that defines nakama_aws, which runs the
// AWS CLI for nakama's own calls from root scripts: the official install
// as the ubuntu user with ubuntu's AWS settings ignored, or the snap on
// a disk that predates Issue #101.
func RunScript() string { return runScript }
