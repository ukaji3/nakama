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
manual setup, and with nakama's own folder first on PATH so that
"nakama" runs from any directory. Type "exit" to return; nothing is
changed outside the subshell: no registry values, no ~/.aws/config
edits, no parent-shell variables.

Double-clicking nakama.exe in Explorer opens the same kind of shell
(Windows PowerShell) in a new window (ADR 0022).

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
		return errors.New("already inside a shell configured by nakama (`nakama aws cli shell`, or the window " +
			"opened by double-clicking nakama.exe): the proxy settings are applied here; type `exit` first")
	}
	shellOverride, err := cmd.Flags().GetString("shell")
	if err != nil {
		return err
	}

	ctx := withCancel(cmd)
	out := cmd.OutOrStdout()
	det, err := cliproxy.NewDetector().Detect(ctx)
	if err != nil {
		return explainDetectionError(err)
	}

	exeDir, err := executableDir()
	if err != nil {
		// Only the convenience is lost: the shell still gets the proxy
		// settings, and nakama can be run by its full path.
		_, _ = fmt.Fprintf(out, "path:   WARNING: nakama's folder is unknown (%v); PATH is unchanged\n", err)
		exeDir = ""
	} else {
		_, _ = fmt.Fprintf(out, "path:   %s added in front of PATH\n", exeDir)
	}
	printDetection(out, det)
	_, _ = fmt.Fprintf(out, "starting a configured shell (type `exit` to return)\n")

	code, err := spawnConfiguredShell(cliShellEnv(os.Environ(), det, exeDir), shellOverride)
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
