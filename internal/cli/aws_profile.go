package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/profilestore"
)

// profileDeps bundles the collaborators of the `nakama aws profile`
// subcommands so tests can substitute fakes. The profile commands run
// on a lightweight path that never constructs awsx.NewClients:
//   - save resolves STS identity via the default chain / aws CLI
//     fallback only (ignoring any active nakama profile on purpose);
//   - list / load / delete / unload use no AWS SDK calls at all;
//     session validation shells out to `aws configure
//     export-credentials` against nakama's own generated config.
type profileDeps struct {
	// newStore returns the profile store (tests inject a temp BaseDir).
	newStore func() *profilestore.Store
	// callerIdentity resolves the CURRENT identity via the default
	// chain + export-credentials fallback, ignoring the active profile.
	callerIdentity func(ctx context.Context) (accountID, callerARN string, err error)
	// loginSession reads login_session from the user's normal AWS CLI
	// config (AWS_PROFILE respected, AWS_CONFIG_FILE untouched).
	loginSession func(ctx context.Context) (string, error)
	// checkProfile verifies that the given profile in nakama's
	// generated aws_config can still yield credentials.
	checkProfile func(ctx context.Context, profileName, configFile string) error
	// now is the clock (tests freeze it).
	now func() time.Time
}

func defaultProfileDeps() profileDeps {
	return profileDeps{
		newStore:       profilestore.New,
		callerIdentity: awsx.ResolveCallerIdentity,
		loginSession: func(ctx context.Context) (string, error) {
			return (&awsx.LoginSessionResolver{}).Resolve(ctx)
		},
		checkProfile: func(ctx context.Context, profileName, configFile string) error {
			p := &awsx.AWSCLICredentialsProvider{Profile: profileName, ConfigFile: configFile}
			_, err := p.Retrieve(ctx)
			return err
		},
		now: time.Now,
	}
}

func newAWSProfileCommand() *cobra.Command {
	return newAWSProfileCommandWithDeps(defaultProfileDeps())
}

func newAWSProfileCommandWithDeps(deps profileDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Save and switch between multiple AWS account login sessions",
		Long: `Manages nakama's multi-account profile registry (ADR 0011).

` + "`aws login`" + ` stores its session identifier (login_session) in the default
profile of ~/.aws/config, so logging into another account overwrites it.
` + "`nakama aws profile save`" + ` copies that identifier (an ARN-shaped, non-secret
value) into nakama's own config under ~/.config/nakama, letting sessions for
multiple accounts coexist. ` + "`load`" + ` selects which saved account every other
nakama command operates on; ` + "`unload`" + ` returns to the default credential chain.

No secrets are stored: the actual tokens stay in ~/.aws/login/cache/ and are
managed by the AWS CLI. Refresh tokens last at most 12 hours; an expired
session comes back by running ` + "`aws login`" + ` for that account again.`,
	}
	cmd.AddCommand(
		newAWSProfileSaveCommand(deps),
		newAWSProfileListCommand(deps),
		newAWSProfileLoadCommand(deps),
		newAWSProfileDeleteCommand(deps),
		newAWSProfileUnloadCommand(deps),
	)
	return cmd
}

// profileSaveResult is the machine-readable output of `profile save`.
type profileSaveResult struct {
	Account  string `json:"account" yaml:"account"`
	Alias    string `json:"alias" yaml:"alias"`
	Identity string `json:"identity" yaml:"identity"`
	SavedAt  string `json:"saved_at" yaml:"saved_at"`
}

func newAWSProfileSaveCommand(deps profileDeps) *cobra.Command {
	var alias string
	cmd := &cobra.Command{
		Use:   "save [--name <alias>]",
		Short: "Save the current `aws login` session under nakama's profile registry",
		Long: `Resolves the CURRENT AWS identity (default credential chain, ignoring any
active nakama profile), reads the login_session identifier that ` + "`aws login`" + `
wrote to your AWS CLI config, and stores both in nakama's registry keyed by
account ID. Re-saving the same account overwrites the previous entry.
The active profile selection is not changed by this command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)

			// 1. Identify whatever the user is logged into RIGHT NOW.
			//    The active nakama profile is ignored on purpose: the
			//    thing being saved is the user's current `aws login`.
			accountID, callerARN, err := deps.callerIdentity(ctx)
			if err != nil {
				return fmt.Errorf("resolve current AWS identity: %w", err)
			}

			// 2. Capture the login_session identifier from the user's
			//    normal AWS CLI config.
			session, err := deps.loginSession(ctx)
			if err != nil || session == "" {
				return errors.New("current credentials do not come from `aws login` (no login_session " +
					"found in the AWS CLI config); `nakama aws profile save` supports aws login sessions only")
			}

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
			// 3. Upsert keyed by account ID; `active` stays untouched.
			data.Upsert(profilestore.Profile{
				AccountID:    accountID,
				Alias:        resolvedAlias,
				LoginSession: session,
				IdentityARN:  callerARN,
				SavedAt:      savedAt,
			})
			if err := store.Save(data); err != nil {
				return err
			}

			res := profileSaveResult{
				Account:  accountID,
				Alias:    resolvedAlias,
				Identity: callerARN,
				SavedAt:  savedAt,
			}
			text := fmt.Sprintf("saved profile:\n"+
				"  account  : %s\n"+
				"  alias    : %s\n"+
				"  identity : %s\n\n"+
				"Switch nakama to this account with: nakama aws profile load %s",
				res.Account, res.Alias, res.Identity, res.Alias)
			return renderOutput(cmd.OutOrStdout(), global.Output, res, text)
		},
	}
	cmd.Flags().StringVar(&alias, "name", "",
		"Alias for the saved profile (default: the AWS account ID)")
	return cmd
}

// checkAliasCollision rejects an alias that already identifies a
// DIFFERENT saved account (either as its alias or as its account ID),
// which would make `profile load <alias>` ambiguous.
func checkAliasCollision(data *profilestore.Data, accountID, alias string) error {
	for _, p := range data.Profiles {
		if p.AccountID == accountID {
			continue // overwriting our own entry is fine
		}
		if p.Alias == alias || p.AccountID == alias {
			return fmt.Errorf("alias %q already refers to account %s; choose another --name", alias, p.AccountID)
		}
	}
	return nil
}

// profileRow is one row of `profile list` (also its JSON/YAML shape).
type profileRow struct {
	Active   bool   `json:"active" yaml:"active"`
	Account  string `json:"account" yaml:"account"`
	Alias    string `json:"alias" yaml:"alias"`
	Identity string `json:"identity" yaml:"identity"`
	Saved    string `json:"saved" yaml:"saved"`
	Status   string `json:"status" yaml:"status"`
}

// Profile status values shown in the STATUS column.
const (
	profileStatusValid   = "valid"
	profileStatusExpired = "expired"
	profileStatusUnknown = "unknown"
)

// classifyProfileStatus maps a checkProfile error to the STATUS column
// value. This is best-effort: a failing export-credentials run most
// likely means the login session expired, while an inconclusive
// failure (aws CLI missing, timeout/cancellation) yields "unknown".
func classifyProfileStatus(err error) string {
	switch {
	case err == nil:
		return profileStatusValid
	case errors.Is(err, exec.ErrNotFound),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return profileStatusUnknown
	default:
		return profileStatusExpired
	}
}

func newAWSProfileListCommand(deps profileDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved profiles with their session status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := withCancel(cmd)
			store := deps.newStore()
			data, err := store.Load()
			if err != nil {
				return err
			}
			configPath, err := store.AWSConfigPath()
			if err != nil {
				return err
			}

			rows := make([]profileRow, 0, len(data.Profiles))
			for _, p := range data.Profiles {
				status := classifyProfileStatus(
					deps.checkProfile(ctx, profilestore.ProfileName(p.AccountID), configPath))
				rows = append(rows, profileRow{
					Active:   p.AccountID == data.Active,
					Account:  p.AccountID,
					Alias:    p.Alias,
					Identity: p.IdentityARN,
					Saved:    p.SavedAt,
					Status:   status,
				})
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, rows, formatProfileTable(rows))
		},
	}
}

func formatProfileTable(rows []profileRow) string {
	if len(rows) == 0 {
		return "(no saved profiles; run `nakama aws profile save` after `aws login`)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-6s  %-12s  %-16s  %-50s  %-25s  %s\n",
		"ACTIVE", "ACCOUNT", "ALIAS", "IDENTITY", "SAVED", "STATUS")
	for _, r := range rows {
		mark := ""
		if r.Active {
			mark = "*"
		}
		fmt.Fprintf(&b, "%-6s  %-12s  %-16s  %-50s  %-25s  %s\n",
			mark, r.Account, r.Alias, r.Identity, r.Saved, r.Status)
	}
	return strings.TrimRight(b.String(), "\n")
}

// profileNotFoundError builds a lookup failure that lists every saved
// candidate so the user can see what keys are valid.
func profileNotFoundError(data *profilestore.Data, key string) error {
	if len(data.Profiles) == 0 {
		return fmt.Errorf("profile %q not found; no profiles are saved yet — run `nakama aws profile save` after `aws login`", key)
	}
	var candidates []string
	for _, p := range data.Profiles {
		candidates = append(candidates, fmt.Sprintf("%s (%s)", p.AccountID, p.Alias))
	}
	return fmt.Errorf("profile %q not found; saved profiles: %s", key, strings.Join(candidates, ", "))
}

func newAWSProfileLoadCommand(deps profileDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "load <account|alias>",
		Short: "Make a saved profile the account all nakama commands operate on",
		Long: `Sets the active profile. Every subsequent nakama command resolves AWS
credentials exclusively through this profile (fail-closed: no fallback to
another account) until ` + "`nakama aws profile unload`" + ` is run. The active
profile takes precedence over the AWS_PROFILE environment variable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			store := deps.newStore()
			data, err := store.Load()
			if err != nil {
				return err
			}
			p, ok := data.Find(args[0])
			if !ok {
				return profileNotFoundError(data, args[0])
			}
			configPath, err := store.AWSConfigPath()
			if err != nil {
				return err
			}

			// Best-effort validation. A failure does NOT abort the
			// switch: the session can be revived later by running
			// `aws login` for that account again.
			if err := deps.checkProfile(ctx, profilestore.ProfileName(p.AccountID), configPath); err != nil {
				// Best-effort diagnostic; a failing write to stderr must
				// not turn a successful profile switch into an error.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: could not obtain credentials for account %s (alias %s): %v\n"+
						"The login session has probably expired; run `aws login` for account %s to revive it.\n"+
						"Loading the profile anyway.\n",
					p.AccountID, p.Alias, err, p.AccountID)
			}

			data.Active = p.AccountID
			if err := store.Save(data); err != nil {
				return err
			}

			res := map[string]string{"loaded": p.AccountID, "alias": p.Alias}
			text := fmt.Sprintf("loaded profile %s (account %s); all nakama commands now target this account",
				p.Alias, p.AccountID)
			return renderOutput(cmd.OutOrStdout(), global.Output, res, text)
		},
	}
}

func newAWSProfileDeleteCommand(deps profileDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <account|alias>",
		Short: "Remove a saved profile from the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := deps.newStore()
			data, err := store.Load()
			if err != nil {
				return err
			}
			p, ok := data.Find(args[0])
			if !ok {
				return profileNotFoundError(data, args[0])
			}

			wasActive := data.Active == p.AccountID
			data.Remove(p.AccountID)
			if wasActive {
				data.Active = ""
			}
			if err := store.Save(data); err != nil {
				return err
			}

			res := map[string]any{"deleted": p.AccountID, "alias": p.Alias, "was_active": wasActive}
			text := fmt.Sprintf("deleted profile %s (account %s)", p.Alias, p.AccountID)
			if wasActive {
				text += "\nIt was the active profile; the active selection has been cleared and " +
					"nakama now uses the default AWS credential chain"
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, text)
		},
	}
}

func newAWSProfileUnloadCommand(deps profileDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "unload",
		Short: "Clear the active profile (return to the default credential chain)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := deps.newStore()
			data, err := store.Load()
			if err != nil {
				return err
			}
			previous := data.Active
			data.Active = ""
			if err := store.Save(data); err != nil {
				return err
			}

			res := map[string]string{"unloaded": previous}
			text := "active profile cleared; nakama now uses the default AWS credential chain"
			if previous == "" {
				text = "no active profile was set; nakama keeps using the default AWS credential chain"
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, text)
		},
	}
}
