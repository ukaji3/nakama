//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The test binary doubles as the processes these tests need, chosen by
// helperModeEnv: a "shell" that reports what it was started with, a
// "shell" that waits for Ctrl-Break, and a nakama stand-in that runs
// such a shell with runShell (or, as the control, without it).
const (
	helperModeEnv   = "NAKAMA_TEST_HELPER_MODE"
	helperReportEnv = "NAKAMA_TEST_HELPER_REPORT"
	helperReadyEnv  = "NAKAMA_TEST_HELPER_READY"
	helperProbeEnv  = "NAKAMA_TEST_HELPER_PROBE"
)

func TestHelperProcess(*testing.T) {
	switch os.Getenv(helperModeEnv) {
	case "report":
		wd, _ := os.Getwd()
		_ = os.WriteFile(os.Getenv(helperReportEnv),
			[]byte(wd+"\n"+os.Getenv("Path")+"\n"+os.Getenv(helperProbeEnv)), 0o600)
		os.Exit(7)
	case "wait-break":
		// A shell that ends on Ctrl-C, as cmd and PowerShell end the
		// running command: exit 5 on the event, 9 if it never comes.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		_ = os.WriteFile(os.Getenv(helperReadyEnv), []byte("ready"), 0o600)
		select {
		case <-ch:
			os.Exit(5)
		case <-time.After(20 * time.Second):
			os.Exit(9)
		}
	case "nakama-runshell", "nakama-plain":
		exe, _ := os.Executable()
		args := []string{"-test.run=^TestHelperProcess$"}
		env := append(os.Environ(), helperModeEnv+"=wait-break")
		var code int
		if os.Getenv(helperModeEnv) == "nakama-runshell" {
			code, _ = runShell(exe, args, env, "")
		} else {
			// The control: what runCLIShell did before, no Notify.
			c := exec.Command(exe, args...)
			c.Env = env
			err := c.Run()
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			}
		}
		// Reached only if Ctrl-Break did not end this process.
		_ = os.WriteFile(os.Getenv(helperReportEnv), []byte(strconv.Itoa(code)), 0o600)
		os.Exit(0)
	}
}

func helperCommand(t *testing.T, mode string, extraEnv ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(exe, "-test.run=^TestHelperProcess$")
	c.Env = append(os.Environ(), append([]string{helperModeEnv + "=" + mode}, extraEnv...)...)
	return c
}

func TestRunShellPassesEnvDirAndExitCode(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.txt")
	env := prependWindowsPath(os.Environ(), `C:\nakama-test-folder`)
	env = append(env, helperModeEnv+"=report", helperReportEnv+"="+report, helperProbeEnv+"=probe-value")

	code, err := runShell(exe, []string{"-test.run=^TestHelperProcess$"}, env, dir)
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want the shell's 7", code)
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the shell did not run: %v", err)
	}
	lines := strings.SplitN(string(b), "\n", 3)
	if len(lines) != 3 {
		t.Fatalf("report = %q", b)
	}
	got, err1 := os.Stat(lines[0])
	want, err2 := os.Stat(dir)
	if err1 != nil || err2 != nil || !os.SameFile(got, want) {
		t.Errorf("the shell ran in %q, want %q", lines[0], dir)
	}
	if !strings.HasPrefix(lines[1], `C:\nakama-test-folder;`) {
		t.Errorf("Path in the shell = %q", lines[1])
	}
	if lines[2] != "probe-value" {
		t.Errorf("the environment was not passed: %q", lines[2])
	}
}

func TestRunShellReportsStartFailure(t *testing.T) {
	_, err := runShell(filepath.Join(t.TempDir(), "no-such-shell.exe"), nil, os.Environ(), "")
	if err == nil || !strings.Contains(err.Error(), "start shell") {
		t.Errorf("err = %v", err)
	}
}

// ctrlBreakOutcome starts a nakama stand-in in its own process group,
// waits until its shell is running, sends Ctrl-Break to the group (the
// shell and nakama both receive it, as with Ctrl-C at the keyboard),
// and returns the stand-in's exit code and what it reported afterwards
// ("" if it did not get that far).
func ctrlBreakOutcome(t *testing.T, mode string) (int, string) {
	t.Helper()
	tmp := t.TempDir()
	ready := filepath.Join(tmp, "ready")
	report := filepath.Join(tmp, "report")
	c := helperCommand(t, mode, helperReadyEnv+"="+ready, helperReportEnv+"="+report)
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- c.Wait() }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatal("the shell did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(c.Process.Pid)); err != nil { // #nosec G115 -- a process ID fits in uint32
		_ = c.Process.Kill()
		<-waited
		// Skipping is for local runs without a console (wine, a service).
		// In CI it would hide the very check, so there it fails instead:
		// the job passing then proves the event was delivered.
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("GenerateConsoleCtrlEvent: %v (CI must be able to deliver the event)", err)
		}
		t.Skipf("GenerateConsoleCtrlEvent: %v (no console to deliver the event)", err)
	}
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		_ = c.Process.Kill()
		t.Fatal("the stand-in did not end")
	}
	b, _ := os.ReadFile(report)
	return c.ProcessState.ExitCode(), string(b)
}

// TestRunShellSurvivesCtrlBreak pins why runShell registers
// os.Interrupt: without it, Ctrl-C in the subshell ends nakama, and the
// window's original shell and the subshell then read the same console.
func TestRunShellSurvivesCtrlBreak(t *testing.T) {
	code, report := ctrlBreakOutcome(t, "nakama-runshell")
	if code != 0 || report != "5" {
		t.Errorf("nakama exited %d after reporting %q; want it to outlive the event (0) and return the shell's code (5)", code, report)
	}
}

// TestCtrlBreakEndsAProgramWithoutNotify is the control: the same
// event does end a Go program that runs the shell without registering,
// so the test above cannot pass for want of a delivered event.
func TestCtrlBreakEndsAProgramWithoutNotify(t *testing.T) {
	code, report := ctrlBreakOutcome(t, "nakama-plain")
	if report != "" || code == 0 {
		t.Errorf("exit %d, report %q: the control expects the event to end the program before it reports", code, report)
	}
	t.Logf("without Notify the program ended with exit code %#x", uint32(code)) // #nosec G115 -- display only
}

func TestWindowsPowerShellPath(t *testing.T) {
	p := windowsPowerShellPath()
	if p == "" {
		t.Skip("Windows PowerShell is not installed on this system")
	}
	if !strings.EqualFold(filepath.Base(p), "powershell.exe") || !filepath.IsAbs(p) {
		t.Errorf("windowsPowerShellPath() = %q", p)
	}
}

func TestHandleDoubleClickIgnoresOtherParents(t *testing.T) {
	// `go test` is the parent here, not Explorer.
	if code, handled := HandleDoubleClick(); handled {
		t.Fatalf("handled (code %d) although not started from Explorer", code)
	}
}
