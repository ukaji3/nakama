package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ukaji3/nakama/internal/cliproxy"
)

// This file holds the pieces shared by `nakama aws cli shell` (ADR 0018)
// and the shell opened by double-clicking nakama.exe (ADR 0022). They
// are pure string handling, so they are unit-tested on every OS even
// though only Windows uses them.

// prependWindowsPath returns env with dir as the first PATH entry,
// following the Windows rules: the variable name is case-insensitive
// (it is "Path" in a normal environment block), entries are separated
// by ';', and directories compare case-insensitively and ignoring a
// trailing separator. Other occurrences of dir are dropped so the list
// does not grow, and every other entry is kept as is. A directory that
// contains ';' is quoted, the form Windows uses for such entries.
func prependWindowsPath(env []string, dir string) []string {
	entry := dir
	if strings.Contains(dir, ";") {
		entry = `"` + dir + `"`
	}
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		// Entries such as "=C:=C:\work" (per-drive directories) have an
		// empty key and are never PATH.
		if !ok || found || !strings.EqualFold(key, "PATH") {
			out = append(out, kv)
			continue
		}
		found = true
		parts := []string{entry}
		if value != "" {
			for _, p := range strings.Split(value, ";") {
				if sameWindowsDir(p, dir) {
					continue
				}
				parts = append(parts, p)
			}
		}
		out = append(out, key+"="+strings.Join(parts, ";"))
	}
	if !found {
		out = append(out, "Path="+entry)
	}
	return out
}

// sameWindowsDir reports whether PATH entry p names dir: quotes and a
// trailing separator are ignored ("C:\" keeps its root separator) and
// the comparison ignores case, as the Windows file system does.
func sameWindowsDir(p, dir string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(strings.Trim(s, `"`))
		for len(s) > 3 && (strings.HasSuffix(s, `\`) || strings.HasSuffix(s, "/")) {
			s = s[:len(s)-1]
		}
		return s
	}
	a, b := norm(p), norm(dir)
	return a != "" && strings.EqualFold(a, b)
}

// executableDir returns the folder that holds the running nakama
// executable, the one put in front of PATH.
func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// cliShellEnv is the environment of a configured shell: the proxy and
// CA settings of det (cliproxy.Env, which also sets the nesting marker)
// and, when exeDir is known, nakama's folder in front of PATH.
func cliShellEnv(environ []string, det *cliproxy.Detection, exeDir string) []string {
	env := cliproxy.Env(environ, det)
	if exeDir != "" {
		env = prependWindowsPath(env, exeDir)
	}
	return env
}

// printDetection writes the summary of what a configured shell will
// receive: the proxy, the CA bundle and the end-to-end check.
func printDetection(out io.Writer, det *cliproxy.Detection) {
	switch {
	case det.ProxyURL == "":
		_, _ = fmt.Fprintf(out, "proxy:  none (direct connection)\n")
	case det.ProxyFromEnv:
		_, _ = fmt.Fprintf(out, "proxy:  %s (from environment variables)\n", det.ProxyURL)
	default:
		_, _ = fmt.Fprintf(out, "proxy:  %s (auto-detected from the Windows proxy settings)\n", det.ProxyURL)
	}
	if det.Intercepted {
		_, _ = fmt.Fprintf(out, "ca:     TLS interception detected (re-signed by %s)\n", det.RootSubject)
		_, _ = fmt.Fprintf(out, "        AWS_CA_BUNDLE=%s\n", det.BundlePath)
	} else {
		_, _ = fmt.Fprintf(out, "ca:     no TLS interception detected; the aws CLI's default CA bundle works\n")
	}
	if det.CheckErr != nil {
		_, _ = fmt.Fprintf(out, "check:  WARNING: verification with these settings failed: %v\n", det.CheckErr)
	} else {
		_, _ = fmt.Fprintf(out, "check:  AWS endpoint reachable with these settings\n")
	}
}

// explainDetectionError adds what the user can do when the detection
// failed because the proxy demands authentication the aws CLI cannot
// perform; other errors are returned unchanged.
func explainDetectionError(err error) error {
	var authErr *cliproxy.ProxyAuthError
	if !errors.As(err, &authErr) {
		return err
	}
	if authErr.SupportsBasic() {
		return fmt.Errorf("%w\nThe aws CLI supports HTTP Basic proxy authentication only as credentials "+
			"embedded in the proxy URL; set HTTPS_PROXY=http://user:password@proxy:port yourself and retry", err)
	}
	return fmt.Errorf("%w\nThe aws CLI cannot authenticate to NTLM/Negotiate proxies. Run a local "+
		"authenticating relay (e.g. px or Cntlm) and set HTTP(S)_PROXY to it, then retry", err)
}
