// Command nakama is the entry point of the nakama CLI.
//
// nakama provisions and manages code-server EC2 instances on AWS,
// fronted by Amazon CloudFront via VPC Origins (private origin).
//
// See docs/requirements.md for the full requirements specification.
package main

import (
	"fmt"
	"os"

	"github.com/ukaji3/nakama/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		// cobra already prints the error; we just translate to an exit code.
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
}
