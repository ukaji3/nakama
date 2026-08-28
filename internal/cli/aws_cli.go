package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/cliproxy"
	"github.com/ukaji3/nakama/internal/service"
)

// newAWSCLICommand returns the `nakama aws cli` command group:
// helpers that configure the aws CLI itself (as opposed to the AWS
// resources nakama manages). See ADR 0018.
func newAWSCLICommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cli",
		Short: "Helpers that configure the aws CLI itself",
	}

	shell := &cobra.Command{
		Use:   "shell",
		Short: "Start a subshell where the aws CLI works behind the corporate proxy (Windows)",
		Long: `Detect the OS proxy configuration (WinHTTP / PAC) and, when a
TLS-intercepting corporate proxy re-signs AWS traffic, export the
corporate CA chain — verified against the Windows certificate store —
as an AWS_CA_BUNDLE file. Then start a new shell (same kind as the
parent shell; override with --shell) with HTTP_PROXY, HTTPS_PROXY,
NO_PROXY and AWS_CA_BUNDLE set so that the aws CLI works without any
manual setup. Type "exit" to return; nothing is changed outside the
subshell: no registry values, no ~/.aws/config edits, no parent-shell
variables.

This command is Windows-only: on other systems nakama relies on
HTTP(S)_PROXY being set conventionally, and the aws CLI can be pointed
at a corporate CA with the AWS_CA_BUNDLE environment variable.`,
		Args: cobra.NoArgs,
		RunE: runCLIShell,
	}
	shell.Flags().String("shell", "", "Shell executable to start (default: same kind as the parent shell)")

	cmd.AddCommand(shell)
	return cmd
}

// runCLIShell implements `nakama aws cli shell`.
func runCLIShell(cmd *cobra.Command, _ []string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("`nakama aws cli shell` is a Windows-only option: it exports the Windows proxy "+
			"configuration (WinHTTP/PAC) and the corporate CA from the Windows certificate store to the aws CLI. "+
			"On %s, set HTTP_PROXY/HTTPS_PROXY and AWS_CA_BUNDLE manually instead", runtime.GOOS)
	}
	if os.Getenv("NAKAMA_AWS_CLI_SHELL") == "1" {
		return errors.New("already inside a `nakama aws cli shell` session; type `exit` first")
	}
	shellOverride, err := cmd.Flags().GetString("shell")
	if err != nil {
		return err
	}

	ctx := withCancel(cmd)
	out := cmd.OutOrStdout()
	det, err := cliproxy.NewDetector().Detect(ctx)
	if err != nil {
		var authErr *cliproxy.ProxyAuthError
		if errors.As(err, &authErr) {
			if authErr.SupportsBasic() {
				return fmt.Errorf("%w\nThe aws CLI supports HTTP Basic proxy authentication only as credentials "+
					"embedded in the proxy URL; set HTTPS_PROXY=http://user:password@proxy:port yourself and retry", authErr)
			}
			return fmt.Errorf("%w\nThe aws CLI cannot authenticate to NTLM/Negotiate proxies. Run a local "+
				"authenticating relay (e.g. px or Cntlm) and set HTTP(S)_PROXY to it, then retry", authErr)
		}
		return err
	}

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
	_, _ = fmt.Fprintf(out, "starting a configured shell (type `exit` to return)\n")

	code, err := spawnConfiguredShell(cliproxy.Env(os.Environ(), det), shellOverride)
	if err != nil {
		return err
	}
	if code != 0 {
		return &service.ExitCodeError{Code: code}
	}
	return nil
}

// shellBaseName lowercases the last path element, accepting both
// Windows and POSIX separators (pure helper, unit-tested on any OS).
func shellBaseName(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		p = p[i+1:]
	}
	return strings.ToLower(p)
}

// defaultShell picks the shell to spawn: the parent process when it is
// a known shell (so PowerShell users stay in PowerShell and cmd users
// in cmd), otherwise ComSpec, otherwise cmd.exe.
func defaultShell(parentExe, comspec string) string {
	switch shellBaseName(parentExe) {
	case "powershell.exe", "pwsh.exe", "cmd.exe":
		return parentExe
	}
	if comspec != "" {
		return comspec
	}
	return "cmd.exe"
}
