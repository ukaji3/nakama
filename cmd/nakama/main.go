// Command nakama is the entry point of the nakama CLI.
//
// nakama provisions and manages code-server EC2 instances on AWS,
// fronted by Amazon CloudFront via VPC Origins (private origin).
//
// See docs/requirements.md for the full requirements specification.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/ukaji3/nakama/internal/cli"
	"github.com/ukaji3/nakama/internal/service"
)

func main() {
	// Double-clicked in Explorer (Windows): open a ready-to-use shell
	// instead of cobra's "This is a command line tool" splash (ADR 0022).
	if code, handled := cli.HandleDoubleClick(); handled {
		os.Exit(code)
	}
	if err := cli.NewRootCommand().Execute(); err != nil {
		var ec *service.ExitCodeError
		isExitCode := errors.As(err, &ec)
		// cobra already printed the error; the blank line separates it.
		// A remote command's exit status (`nakama aws connect <name>
		// <command>`, ADR 0021) is reported like ssh does: no output.
		if !isExitCode || !ec.Remote {
			fmt.Fprintln(os.Stderr)
		}
		if isExitCode {
			os.Exit(ec.Code)
		}
		os.Exit(1)
	}
}
