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
	if err := cli.NewRootCommand().Execute(); err != nil {
		// cobra already prints the error; we just translate to an exit code.
		fmt.Fprintln(os.Stderr)
		// `nakama aws connect` propagates the exit code of its session
		// child unchanged (ADR 0014).
		var ec *service.ExitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.Code)
		}
		os.Exit(1)
	}
}
