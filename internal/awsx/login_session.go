package awsx

import (
	"context"
	"strings"
	"time"
)

// LoginSessionResolver reads the `login_session` value from the user's
// normal AWS CLI configuration by shelling out to
// `aws configure get login_session`.
//
// It deliberately does NOT set AWS_CONFIG_FILE: `nakama aws profile
// save` must observe the config that `aws login` actually wrote (the
// user's ~/.aws/config or whatever their environment points at).
// AWS_PROFILE is honoured implicitly because the child process inherits
// the caller's environment.
//
// The value is an identity-ARN-shaped session identifier, not a secret
// (ADR 0011); the actual tokens stay in ~/.aws/login/cache/.
type LoginSessionResolver struct {
	// Timeout bounds a single aws CLI invocation. Defaults to 10s.
	Timeout time.Duration
	// commandRunner is overridable for tests; nil means real exec.
	commandRunner commandRunnerFunc
}

// Resolve returns the login_session value from the user's AWS CLI
// config. When the key is unset, the aws CLI exits non-zero with empty
// output, which surfaces here as an error; callers should treat both
// an error and an empty string as "current credentials do not come
// from `aws login`".
func (r *LoginSessionResolver) Resolve(ctx context.Context) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runner := r.commandRunner
	if runner == nil {
		runner = defaultCommandRunner
	}

	// env=nil: inherit the caller's environment (AWS_PROFILE respected,
	// AWS_CONFIG_FILE untouched -> the user's normal config).
	out, err := runner(cctx, nil, "aws", "configure", "get", "login_session")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
