//go:build windows

package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Tests of the console of the tunnel daemon's session-runner children
// on Windows (Issue #126). nakama.exe is a console program; started by
// a process without a console (the daemon runs with DETACHED_PROCESS),
// it must not get a console (conhost.exe and a window) of its own.
// Before the fix the Windows CI runner reported
// "child console: window=196768 cp=437" here.

// helperDetachedSpawner is a helper mode: the test binary, started with
// the daemon's process flags, spawns one session runner the way the
// daemon does.
const helperDetachedSpawner = "detached-spawner"

var (
	testKernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleWindow = testKernel32.NewProc("GetConsoleWindow")
	procGetConsoleCP     = testKernel32.NewProc("GetConsoleCP")
)

func init() {
	platformTestHelpers[helperDetachedSpawner] = runDetachedSpawner
	reportConsole = consoleState
}

// consoleState describes this process's console: the console window
// handle and the input code page. Both are 0 without a console.
func consoleState() string {
	w, _, _ := procGetConsoleWindow.Call()
	cp, _, _ := procGetConsoleCP.Call()
	return fmt.Sprintf("window=%d cp=%d", w, cp)
}

// runDetachedSpawner reports its own console, then runs one fake
// session runner with the daemon's options and prints what it wrote.
func runDetachedSpawner() int {
	fmt.Printf("spawner console: %s\n", consoleState())
	_ = os.Setenv(testHelperEnv, helperFakeSessionRunner)
	_ = os.Setenv("NAKAMA_TEST_FAKE_CONSOLE", "1")
	_ = os.Setenv("NAKAMA_TEST_FAKE_EXIT", "0")
	var out bytes.Buffer
	res, err := spawnSessionRunnerIO(context.Background(), sessionRunRequest{payload: []byte("{}")}, daemonSessionRunnerIO(&out))
	fmt.Print(out.String())
	if err != nil {
		fmt.Printf("spawn error: %v\n", err)
		return 1
	}
	fmt.Printf("child exit: %d\n", res.code)
	return 0
}

// TestDaemonSessionRunnerHasNoConsole starts the helper exactly like the
// tunnel daemon (tunnelDaemonSysProcAttr), lets it run a session runner
// like the daemon does, and requires that the child has no console.
func TestDaemonSessionRunnerHasNoConsole(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe) // #nosec G204 -- the test binary itself
	cmd.Env = append(withoutEnv(os.Environ(), testHelperEnv), testHelperEnv+"="+helperDetachedSpawner)
	cmd.SysProcAttr = tunnelDaemonSysProcAttr()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("detached spawner: %v\n%s", err, out.String())
	}
	t.Logf("helper output:\n%s", out.String())
	text := out.String()
	if !strings.Contains(text, "spawner console: window=0 cp=0") {
		t.Fatalf("the helper must have no console, like the daemon; otherwise this test proves nothing:\n%s", text)
	}
	if !strings.Contains(text, "child exit: 0") {
		t.Fatalf("the session runner did not run:\n%s", text)
	}
	if !strings.Contains(text, "child console: window=0 cp=0") {
		t.Errorf("the daemon's session runner got a console of its own (a conhost.exe and a window per session):\n%s", text)
	}
}
