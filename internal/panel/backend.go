package panel

import (
	"context"

	"github.com/ukaji3/nakama/internal/service"
	"github.com/ukaji3/nakama/internal/version"
)

// Backend is what the panel does with AWS and the local machine. The
// CLI implements it (internal/cli/ui_backend.go) with the same code the
// commands use; tests use a fake. Reads, the AWS sign-in and opening
// code-server / Kiro Crew run in the panel's process; every
// state-changing action is an operation: the CLI command run in a
// detached process (ADR 0031 decision 5).
type Backend interface {
	// Account is who the panel acts as; an error means no usable AWS
	// credentials (the panel offers the sign-in).
	Account(ctx context.Context) (*Account, error)
	// InvalidateCredentials drops cached credentials (after a sign-in
	// or a profile change).
	InvalidateCredentials()
	// Login runs `nakama aws login`: onAuthURL gets the AWS sign-in
	// address for the browser; it returns when the sign-in finished.
	Login(ctx context.Context, req LoginRequest, onAuthURL func(string)) (*LoginResult, error)
	// Profiles lists the saved profiles with their session status.
	Profiles(ctx context.Context) ([]Profile, error)
	// RunQuick runs a short local CLI command (a profile change) and
	// returns its standard output; the error carries its stderr.
	RunQuick(ctx context.Context, args []string) (string, error)

	Instances(ctx context.Context, showNAT bool) ([]service.InstanceSummary, error)
	Instance(ctx context.Context, name string, withPassword bool) (*service.InstanceInfo, error)
	// Backups lists the backups of name, or of every instance (also
	// deleted ones) when name is empty.
	Backups(ctx context.Context, name string) ([]service.Backup, error)
	ACL(ctx context.Context) ([]service.ACLEntry, error)
	Tunnels(ctx context.Context) ([]service.TunnelOverview, error)
	SuggestTunnel(ctx context.Context, name string) (*service.TunnelSuggestion, error)
	Doctor(ctx context.Context) *service.DoctorReport
	CheckVersion(ctx context.Context) version.CheckResult

	// OpenCode and OpenKiroCrew prepare what `nakama aws code` and
	// `nakama aws kirocrew` open, and return the navigation the panel
	// hands to the browser instead of starting one. progress reports
	// the stage reached.
	OpenCode(ctx context.Context, name string, o OpenOptions, progress func(OpenStage)) (*Navigation, error)
	OpenKiroCrew(ctx context.Context, name string, o OpenOptions, progress func(OpenStage)) (*Navigation, error)

	StartOperation(ctx context.Context, in service.OperationInput) (*service.Operation, error)
	Operations(ctx context.Context) ([]service.Operation, error)
	Operation(ctx context.Context, id string) (*service.Operation, error)
	OperationOutput(id string) (stdout, log string, err error)
	CancelOperation(ctx context.Context, id string) error
}

// Account is the AWS identity the panel acts as.
type Account struct {
	AccountID string
	Identity  string
	// Alias is the active nakama profile's alias ("" without one).
	Alias string
}

// LoginRequest parameterizes Backend.Login.
type LoginRequest struct {
	// Alias names the profile (the account id when empty).
	Alias string
	// RedirectURL is where the sign-in callback sends the browser.
	RedirectURL string
}

// LoginResult is the outcome of a sign-in.
type LoginResult struct {
	AccountID, Alias, Identity string
}

// Profile is one saved profile (`nakama aws profile list`).
type Profile struct {
	Active                                    bool
	AccountID, Alias, Identity, Saved, Status string
}

// OpenOptions are the flags of code / kirocrew the panel offers.
type OpenOptions struct {
	// Internet opens through CloudFront (--internet).
	Internet bool
	// NoAuth skips the automatic sign-in (--no-auth).
	NoAuth bool
}

// OpenStage is a step of opening code-server or Kiro Crew.
type OpenStage string

// Open stages, in order.
const (
	StageInstance OpenStage = "instance"
	StageForward  OpenStage = "forward"
	StageSignIn   OpenStage = "signin"
)

// Navigation is a top-level form submission the browser performs: a
// POST of the code-server password, or a GET with the Kiro Crew token.
type Navigation struct {
	Method string // "POST" or "GET"
	// Action is the absolute URL the form goes to.
	Action string
	Fields []Field
	// Notes are warnings to show before leaving (in English, from the
	// CLI's own wording).
	Notes []string
}

// Field is one hidden form field of a Navigation (it may be secret).
type Field struct{ Name, Value string }
