package awsx

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ResolveLoginSession reads the `login_session` value for the given
// profile from the user's AWS CLI configuration WITHOUT shelling out
// to the aws binary (ADR 0016). An empty profile means [default];
// otherwise the [profile <name>] section is consulted (falling back to
// [default] like the AWS CLI does when the named section is absent is
// deliberately NOT done: `aws login --profile X` writes to [profile X]
// and resolving a different section would cross accounts). The file
// honoured is AWS_CONFIG_FILE when set, otherwise ~/.aws/config. The
// file is only ever read, never written.
//
// A missing file or a missing key yields ("", nil): callers treat an
// empty result as "current credentials do not come from `aws login`".
// The value is an identity-ARN-shaped session identifier, not a secret
// (ADR 0011); the actual tokens stay in ~/.aws/login/cache/.
func ResolveLoginSession(profile string) (string, error) {
	path, err := awsConfigFilePath()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path) // #nosec G304 -- fixed AWS config path (env override is the documented AWS_CONFIG_FILE contract)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read AWS config %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return loginSessionFromConfig(f, profile), nil
}

// ResolveCurrentLoginSession resolves the login_session for the
// profile selected by AWS_PROFILE / AWS_DEFAULT_PROFILE, or [default]
// when neither is set. `nakama aws profile save` uses it to capture
// whatever session the user is currently logged into.
func ResolveCurrentLoginSession() (string, error) {
	return ResolveLoginSession(profileFromEnv())
}

// awsConfigFilePath resolves the AWS CLI config file location:
// AWS_CONFIG_FILE when non-empty, else ~/.aws/config.
func awsConfigFilePath() (string, error) {
	if p := lookupEnvNonEmpty("AWS_CONFIG_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for ~/.aws/config: %w", err)
	}
	return filepath.Join(home, ".aws", "config"), nil
}

// loginSessionFromConfig scans an AWS-CLI-format INI document for the
// login_session key of the requested profile's section ([default] for
// an empty or "default" profile, [profile <name>] otherwise — the AWS
// CLI's section naming). The parser is deliberately minimal but
// tolerant:
//
//   - section headers: [default] / [profile name] (case-insensitive,
//     surrounding whitespace ignored);
//   - full-line comments starting with '#' or ';' are skipped;
//   - keys are matched case-insensitively with whitespace around the
//     '=' separator ignored;
//   - values in other sections are ignored.
func loginSessionFromConfig(r io.Reader, profile string) string {
	wantSection := "default"
	if profile != "" && !strings.EqualFold(profile, "default") {
		wantSection = "profile " + profile
	}
	inSection := false
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.Join(strings.Fields(line[1:len(line)-1]), " ")
			inSection = strings.EqualFold(section, wantSection)
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "login_session") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
