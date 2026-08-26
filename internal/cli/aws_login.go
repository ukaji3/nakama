package cli

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/browser"
	"github.com/ukaji3/nakama/internal/profilestore"
	"github.com/ukaji3/nakama/internal/signin"
)

// loginDeps bundles the collaborators of `nakama aws login` so tests
// can substitute fakes. The command never constructs awsx.NewClients:
// the sign-in flow (internal/signin) and the STS confirmation
// (native login-session provider) run on their own lightweight path.
type loginDeps struct {
	// login runs the sign-in flow (default: signin.Login).
	login func(ctx context.Context, opts signin.LoginOptions) (*signin.LoginResult, error)
	// identity confirms the fresh session via STS GetCallerIdentity
	// resolved through the native login-session provider.
	identity func(ctx context.Context, loginSession string) (accountID, callerARN string, err error)
	// openBrowser launches the user's browser (default:
	// internal/browser). A failure must degrade to URL display only.
	openBrowser func(ctx context.Context, url string) error
	// newStore returns the profile store (tests inject a temp BaseDir).
	newStore func() *profilestore.Store
	// now is the clock (tests freeze it).
	now func() time.Time
}

func defaultLoginDeps() loginDeps {
	return loginDeps{
		login:    signin.Login,
		identity: awsx.ResolveLoginSessionIdentity,
		openBrowser: func(ctx context.Context, url string) error {
			return browser.New("").Open(ctx, url)
		},
		newStore: profilestore.New,
		now:      time.Now,
	}
}

// loginResultOutput is the machine-readable output of `aws login`.
type loginResultOutput struct {
	Account  string `json:"account" yaml:"account"`
	Alias    string `json:"alias" yaml:"alias"`
	Identity string `json:"identity" yaml:"identity"`
	SavedAt  string `json:"saved_at" yaml:"saved_at"`
}

func newAWSLoginCommand() *cobra.Command {
	return newAWSLoginCommandWithDeps(defaultLoginDeps())
}

func newAWSLoginCommandWithDeps(deps loginDeps) *cobra.Command {
	var (
		remote bool
		alias  string
	)
	cmd := &cobra.Command{
		Use:   "login [--remote] [--name <alias>]",
		Short: "Sign in with AWS Management Console credentials (no aws CLI required)",
		Long: `Signs in for local development using your AWS Management Console session,
natively implementing the ` + "`aws login`" + ` protocol (OAuth 2.0 authorization code
+ PKCE + DPoP; ADR 0016). The IAM user/role needs the AWS managed policy
SignInLocalDevelopmentAccess (the account root user does not).

The temporary credentials and the refresh token are cached in the same
AWS-CLI-compatible store (~/.aws/login/cache/), so ` + "`aws login`" + ` and nakama can
share sessions. On success the session is saved to nakama's profile registry
and activated (equivalent to ` + "`nakama aws profile save` + `load`" + `), so every
nakama command targets this account immediately. Your ~/.aws/config is never
modified.

With --remote (SSH / headless hosts) the authorization URL is only displayed;
open it in a browser on another device and paste the verification code shown
after signing in.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			errOut := cmd.ErrOrStderr()

			opts := signin.LoginOptions{
				Remote: remote,
				OnAuthURL: func(url string) {
					// Always show the URL, even when a browser opens:
					// the launch is fire-and-forget and may silently
					// land on the wrong profile or fail late.
					_, _ = fmt.Fprintf(errOut, "Open the following URL in your browser and sign in:\n\n  %s\n\n", url)
					if remote {
						return
					}
					if err := deps.openBrowser(ctx, url); err != nil {
						_, _ = fmt.Fprintf(errOut, "Could not launch a browser (%v); open the URL manually.\n", err)
					}
					_, _ = fmt.Fprintln(errOut, "Waiting for the sign-in to complete in your browser (Ctrl-C to abort)...")
				},
				Prompt: func(context.Context) (string, error) {
					_, _ = fmt.Fprint(errOut, "Enter the verification code displayed in your browser: ")
					reader := bufio.NewReader(cmd.InOrStdin())
					line, err := reader.ReadString('\n')
					if err != nil && line == "" {
						return "", err
					}
					return strings.TrimSpace(line), nil
				},
			}

			res, err := deps.login(ctx, opts)
			if err != nil {
				return err
			}

			// Confirm the identity through the freshly cached session
			// (STS GetCallerIdentity via the native provider).
			accountID, callerARN, err := deps.identity(ctx, res.LoginSession)
			if err != nil {
				return fmt.Errorf("signed in, but confirming the identity via STS failed: %w", err)
			}

			// Register and activate the profile (same registry logic
			// as `nakama aws profile save` + `load`).
			resolvedAlias := alias
			if resolvedAlias == "" {
				resolvedAlias = accountID
			}
			store := deps.newStore()
			data, err := store.Load()
			if err != nil {
				return err
			}
			if err := checkAliasCollision(data, accountID, resolvedAlias); err != nil {
				return err
			}
			savedAt := deps.now().Format(time.RFC3339)
			data.Upsert(profilestore.Profile{
				AccountID:    accountID,
				Alias:        resolvedAlias,
				LoginSession: res.LoginSession,
				IdentityARN:  callerARN,
				SavedAt:      savedAt,
			})
			data.Active = accountID
			if err := store.Save(data); err != nil {
				return err
			}

			out := loginResultOutput{
				Account:  accountID,
				Alias:    resolvedAlias,
				Identity: callerARN,
				SavedAt:  savedAt,
			}
			text := fmt.Sprintf("logged in:\n"+
				"  account  : %s\n"+
				"  alias    : %s\n"+
				"  identity : %s\n\n"+
				"The profile has been saved and activated; all nakama commands now target this account.",
				out.Account, out.Alias, out.Identity)
			return renderOutput(cmd.OutOrStdout(), global.Output, out, text)
		},
	}
	cmd.Flags().BoolVar(&remote, "remote", false,
		"Do not start a local callback server or open a browser; display the URL and prompt for the pasted verification code (for SSH/headless hosts)")
	cmd.Flags().StringVar(&alias, "name", "",
		"Alias for the saved profile (default: the AWS account ID)")
	return cmd
}
