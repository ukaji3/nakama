package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSSSHCommand returns the `nakama aws ssh` group: ssh access to a
// nakama instance over SSM Session Manager, so desktop editors
// (VS Code Remote-SSH, JetBrains Gateway) and plain ssh/scp work
// without any inbound port. See ADR 0019.
func newAWSSSHCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Use ssh (and ssh-based editors) over SSM Session Manager",
	}
	cmd.AddCommand(newAWSSSHSetupCommand(), newAWSSSHProxyCommand())
	return cmd
}

// newAWSSSHSetupCommand returns `nakama aws ssh setup <name>`.
func newAWSSSHSetupCommand() *cobra.Command {
	var (
		identity string
		noKey    bool
		port     int
	)
	cmd := &cobra.Command{
		Use:   "setup <name>",
		Short: "Authorize your ssh key and generate an ssh config snippet",
		Long: `Authorize your ssh public key for the ubuntu user on the instance
(idempotently, over SSM Run Command) and write an ssh config snippet that
routes the connection through ` + "`nakama aws ssh proxy`" + `.

nakama never edits ~/.ssh/config: the snippet is written under
~/.config/nakama/ssh/ and the printed Include line adds it to your own
config. After that, ` + "`ssh nakama-<name>`" + `, VS Code Remote-SSH and
JetBrains Gateway all reach the instance without any inbound port.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			res, err := service.NewService(clients).SSHSetup(ctx, service.SSHSetupInput{
				Name:          args[0],
				PublicKeyPath: identity,
				SkipKey:       noKey,
				Port:          port,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case res.PublicKeyPath == "":
				_, _ = fmt.Fprintf(out, "key:      skipped (--no-key)\n")
			case res.KeyAlreadyPresent:
				_, _ = fmt.Fprintf(out, "key:      %s (already authorized)\n", res.PublicKeyPath)
			default:
				_, _ = fmt.Fprintf(out, "key:      %s (authorized for ubuntu)\n", res.PublicKeyPath)
			}
			_, _ = fmt.Fprintf(out, "snippet:  %s\n\n%s\n", res.SnippetPath, res.Snippet)
			_, _ = fmt.Fprintf(out, "Add this line to ~/.ssh/config (once per instance file):\n\n    %s\n\n", res.IncludeLine)
			_, _ = fmt.Fprintf(out, "Then: ssh %s   |   VS Code: Remote-SSH -> %s\n", res.Alias, res.Alias)
			return nil
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "",
		"Public key to authorize (default: ~/.ssh/id_ed25519.pub, id_ecdsa.pub or id_rsa.pub)")
	cmd.Flags().BoolVar(&noKey, "no-key", false, "Only generate the config snippet; do not touch authorized_keys")
	cmd.Flags().IntVar(&port, "port", 0, "sshd port on the instance (default 22)")
	return cmd
}

// newAWSSSHProxyCommand returns `nakama aws ssh proxy <name>`: the
// OpenSSH ProxyCommand target. It writes the ssh byte stream on stdout,
// so status output goes to stderr only.
func newAWSSSHProxyCommand() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "proxy <name>",
		Short: "Relay stdin/stdout to the instance's sshd (for ssh ProxyCommand)",
		Long: `Relay this process's stdin/stdout to the instance's sshd over an SSM
Session Manager data channel. Intended as an OpenSSH ProxyCommand:

    ssh -o ProxyCommand='nakama aws ssh proxy dev01' ubuntu@dev01

` + "`nakama aws ssh setup <name>`" + ` generates a ready-made config snippet.
Reconnection is the ssh client's responsibility, so this command runs a
single session and exits with it.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			return service.NewService(clients).SSHProxy(ctx, service.SSHProxyInput{Name: args[0], Port: port})
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "sshd port on the instance (default 22)")
	return cmd
}
