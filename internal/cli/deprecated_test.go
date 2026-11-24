package cli

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// renamedCommands are the commands Issue #129 renamed (ADR 0030): the
// path under `nakama aws` of the old name and of the new one.
var renamedCommands = []struct{ old, new string }{
	{"open", "code"},
	{"connect", "shell"},
}

// findCommand resolves a space-separated path below cmd, or nil.
func findCommand(t *testing.T, root *cobra.Command, path string) *cobra.Command {
	t.Helper()
	found, rest, err := root.Find(strings.Fields(path))
	if err != nil || len(rest) != 0 || found.CommandPath() != root.CommandPath()+" "+path {
		return nil
	}
	return found
}

// flagSummary lists a command's local flags as "name/shorthand/usage".
func flagSummary(cmd *cobra.Command) []string {
	var out []string
	for _, line := range strings.Split(cmd.LocalNonPersistentFlags().FlagUsages(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestRenamedCommandsKeepTheirOldNames: the old name runs the same
// command (same arguments and flags), but help lists only the new one.
func TestRenamedCommandsKeepTheirOldNames(t *testing.T) {
	t.Parallel()
	aws := newAWSCommand()
	var available []string
	for _, c := range aws.Commands() {
		if c.IsAvailableCommand() {
			available = append(available, c.Name())
		}
	}
	for _, r := range renamedCommands {
		oldCmd, newCmd := findCommand(t, aws, r.old), findCommand(t, aws, r.new)
		if oldCmd == nil || newCmd == nil {
			t.Fatalf("%s -> %s: old %v, new %v; want both commands", r.old, r.new, oldCmd != nil, newCmd != nil)
		}
		if !slices.Contains(available, r.new) || slices.Contains(available, r.old) {
			t.Errorf("help lists %v; want %q and not %q", available, r.new, r.old)
		}
		if want := `use "nakama aws ` + r.new + `" instead`; oldCmd.Deprecated != want {
			t.Errorf("%s: Deprecated = %q, want %q", r.old, oldCmd.Deprecated, want)
		}
		if got, want := strings.TrimPrefix(oldCmd.Use, r.old), strings.TrimPrefix(newCmd.Use, r.new); got != want {
			t.Errorf("%s: arguments %q, want those of %s: %q", r.old, got, r.new, want)
		}
		if got, want := flagSummary(oldCmd), flagSummary(newCmd); !slices.Equal(got, want) {
			t.Errorf("%s: flags\n%v\nwant those of %s:\n%v", r.old, got, r.new, want)
		}
		if oldCmd.Short != newCmd.Short || oldCmd.Long != newCmd.Long {
			t.Errorf("%s: help text differs from %s", r.old, r.new)
		}
	}
}

// TestDeprecatedConnectKeepsTheRemoteCommand: like `shell`, the old
// `connect` sends everything after <name> to the instance, flags too.
func TestDeprecatedConnectKeepsTheRemoteCommand(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"shell", "connect"} {
		cmd := findCommand(t, newAWSCommand(), name)
		if cmd == nil {
			t.Fatalf("no %s command", name)
		}
		if err := cmd.ParseFlags([]string{"--retry", "1", "dev01", "ls", "--tty", "-la"}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, want := cmd.Flags().Args(), []string{"dev01", "ls", "--tty", "-la"}; !slices.Equal(got, want) {
			t.Errorf("%s: args %v, want %v", name, got, want)
		}
		if tty, _ := cmd.Flags().GetBool("tty"); tty {
			t.Errorf("%s: --tty after <name> was taken as a flag", name)
		}
	}
}

// TestDeprecatedCommandNamesTheReplacementOnStderr runs a command under
// its old name with the process's real stdout and stderr (cobra writes
// the notice to os.Stderr when no writer is set, as in production) and
// checks that the notice goes to stderr only.
func TestDeprecatedCommandNamesTheReplacementOnStderr(t *testing.T) {
	// Not parallel: it swaps os.Stdout and os.Stderr.
	newCmd := func() *cobra.Command {
		return &cobra.Command{
			Use:  "fresh <name>",
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := io.WriteString(cmd.OutOrStdout(), "ran with "+args[0]+"\n")
				return err
			},
		}
	}
	root := &cobra.Command{Use: "nakama", SilenceUsage: true}
	root.AddCommand(newCmd(), deprecatedCommand(newCmd(), "stale", "nakama fresh"))
	root.SetArgs([]string{"stale", "dev01"})

	stdout, stderr := captureStdio(t, func() {
		if err := root.Execute(); err != nil {
			t.Errorf("execute: %v", err)
		}
	})
	if stdout != "ran with dev01\n" {
		t.Errorf("stdout %q, want only the command's output", stdout)
	}
	if want := "Command \"stale\" is deprecated, use \"nakama fresh\" instead\n"; stderr != want {
		t.Errorf("stderr %q, want %q", stderr, want)
	}
}

// captureStdio runs fn with os.Stdout and os.Stderr redirected to pipes
// and returns what was written to each.
func captureStdio(t *testing.T, fn func()) (string, string) {
	t.Helper()
	read := func(r *os.File, done chan<- string) {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outDone, errDone := make(chan string, 1), make(chan string, 1)
	go read(outR, outDone)
	go read(errR, errDone)
	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = savedOut, savedErr }()
	fn()
	os.Stdout, os.Stderr = savedOut, savedErr
	_ = outW.Close()
	_ = errW.Close()
	return <-outDone, <-errDone
}
