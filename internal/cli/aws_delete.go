package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

type deleteFlags struct {
	yes   bool
	force bool
}

func newAWSDeleteCommand() *cobra.Command {
	f := &deleteFlags{}
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a nakama instance and its CloudFront / WAF / EC2 / SSM resources",
		Long: `Deletes, in dependency order:

  1. CloudFront distribution (disable, wait, delete)
  2. CloudFront VPC origin
  3. EC2 instance (terminate)
  4. ENI
  5. Security group
  6. SSM Parameter Store entries under /nakama/<name>/...

Prompts for confirmation unless --yes is given. --force additionally
ignores 'already gone' / mid-step errors during cleanup.

The whole flow takes ~15-20 minutes (CloudFront propagation dominates).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			name := args[0]

			if !f.yes {
				ok, err := confirmDestroy(cmd.OutOrStdout(), name)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted")
				}
			}

			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			err = service.NewService(clients).DeleteInstance(ctx, service.DeleteInstanceInput{
				Name:  name,
				Force: f.force,
			})
			if err != nil {
				return err
			}
			res := map[string]string{"name": name, "action": "deleted"}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, "deleted: "+name)
		},
	}
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&f.force, "force", false, "Continue past 'already gone' errors")
	return cmd
}

// confirmDestroy reads a y/N answer from stdin (case-insensitive).
func confirmDestroy(w interface{ Write([]byte) (int, error) }, name string) (bool, error) {
	if _, err := fmt.Fprintf(asWriter(w), "Delete nakama instance %q and all its AWS resources? [y/N]: ", name); err != nil {
		return false, err
	}
	return readYesNo()
}

// readYesNo reads a single y/N line from stdin (case-insensitive).
func readYesNo() (bool, error) {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

// asWriter is a tiny adapter so cobra's OutOrStdout can be passed
// to fmt.Fprintf (which requires io.Writer).
func asWriter(w interface{ Write([]byte) (int, error) }) interface {
	Write([]byte) (int, error)
} {
	return w
}
