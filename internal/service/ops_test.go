package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the control panel's operations (Issue #139, ADR 0031
// decision 5): the records, the states, conflicts, stopping, output and
// pruning. The runner runs in-process (RunOperation) instead of a
// detached nakama. Not parallel: the state directory and opsHooks.

type fakeRunner struct {
	mu      sync.Mutex
	release map[string]chan int // id -> exit code to finish with
	exited  map[string]int
}

func withFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	withTunnelStateDir(t)
	fr := &fakeRunner{release: map[string]chan int{}, exited: map[string]int{}}
	prev := opsHooks
	t.Cleanup(func() { opsHooks = prev })
	opsHooks.spawn = func(id string, stdout, stderr *os.File) error {
		ch := make(chan int, 1)
		fr.mu.Lock()
		fr.release[id] = ch
		fr.mu.Unlock()
		_, _ = stdout.WriteString(`{"name":"dev01"}` + "\n")
		_, _ = stderr.WriteString("level=INFO msg=working\n")
		started := make(chan struct{})
		go func() {
			_, _ = RunOperation(id, func(args []string) int {
				close(started)
				return <-ch
			}, func(code int) {
				fr.mu.Lock()
				fr.exited[id] = code
				fr.mu.Unlock()
				ch <- code // ends run as the real exit would end the process
			})
		}()
		<-started
		return nil
	}
	return fr
}

func (fr *fakeRunner) finish(id string, code int) {
	fr.mu.Lock()
	ch := fr.release[id]
	fr.mu.Unlock()
	ch <- code
}

func waitState(t *testing.T, id string, want OperationState) *Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err := GetOperation(context.Background(), id)
		if err == nil && op.State == want {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s: state %v (%v), want %s", id, op, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOperationRunsAndRecordsItsExit(t *testing.T) {
	fr := withFakeRunner(t)
	op, err := StartOperation(context.Background(), OperationInput{Title: "create dev01", Instance: "dev01",
		Args: []string{"aws", "create", "dev01", "--instance-type=t3.medium", "-o", "json"}})
	if err != nil {
		t.Fatal(err)
	}
	if !operationIDPattern.MatchString(op.ID) || op.State != OperationRunning {
		t.Errorf("op = %+v", op)
	}
	dir, _ := opsDir()
	for _, f := range []string{"", "op.json", "stdout", "log", "runner.json"} {
		info, err := os.Stat(filepath.Join(dir, op.ID, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if want := os.FileMode(0o600); f == "" {
			want = 0o700
			if info.Mode().Perm() != want {
				t.Errorf("dir mode %v", info.Mode().Perm())
			}
		} else if info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", f, info.Mode().Perm(), want)
		}
	}
	got := waitState(t, op.ID, OperationRunning)
	if strings.Join(got.Args, " ") != "aws create dev01 --instance-type=t3.medium -o json" || got.Instance != "dev01" {
		t.Errorf("recorded %+v", got)
	}
	fr.finish(op.ID, 0)
	done := waitState(t, op.ID, OperationSucceeded)
	if done.ExitCode != 0 || done.EndedAt.IsZero() {
		t.Errorf("done = %+v", done)
	}
	stdout, log, err := OperationOutput(op.ID)
	if err != nil || !strings.Contains(stdout, `"name":"dev01"`) || !strings.Contains(log, "working") {
		t.Errorf("output %q %q %v", stdout, log, err)
	}

	op2, _ := StartOperation(context.Background(), OperationInput{Title: "start dev01", Instance: "dev01", Args: []string{"aws", "start", "dev01"}})
	fr.finish(op2.ID, 3)
	if failed := waitState(t, op2.ID, OperationFailed); failed.ExitCode != 3 {
		t.Errorf("failed = %+v", failed)
	}
	ops, err := ListOperations(context.Background())
	if err != nil || len(ops) != 2 || ops[0].ID != op2.ID {
		t.Errorf("list (newest first) = %+v, %v", ops, err)
	}
}

func TestOperationsDoNotOverlap(t *testing.T) {
	fr := withFakeRunner(t)
	a, err := StartOperation(context.Background(), OperationInput{Title: "update dev01", Instance: "dev01", Args: []string{"aws", "update", "dev01"}})
	if err != nil {
		t.Fatal(err)
	}
	var conflict *OperationConflictError
	if _, err := StartOperation(context.Background(), OperationInput{Title: "stop dev01", Instance: "dev01", Args: []string{"aws", "stop", "dev01"}}); !errors.As(err, &conflict) || conflict.Running.ID != a.ID {
		t.Errorf("same instance = %v", err)
	}
	if _, err := StartOperation(context.Background(), OperationInput{Title: "init", Exclusive: true, Args: []string{"aws", "init"}}); !errors.As(err, &conflict) {
		t.Errorf("init beside a running operation = %v", err)
	}
	b, err := StartOperation(context.Background(), OperationInput{Title: "start dev02", Instance: "dev02", Args: []string{"aws", "start", "dev02"}})
	if err != nil {
		t.Errorf("another instance must start: %v", err)
	}
	fr.finish(a.ID, 0)
	fr.finish(b.ID, 0)
	waitState(t, a.ID, OperationSucceeded)
	waitState(t, b.ID, OperationSucceeded)
	in, err := StartOperation(context.Background(), OperationInput{Title: "init", Exclusive: true, Args: []string{"aws", "init"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartOperation(context.Background(), OperationInput{Title: "start dev03", Instance: "dev03", Args: []string{"aws", "start", "dev03"}}); !errors.As(err, &conflict) {
		t.Errorf("anything beside init = %v", err)
	}
	fr.finish(in.ID, 0)
	waitState(t, in.ID, OperationSucceeded)
}

func TestCancelStopsAnOperationLikeCtrlC(t *testing.T) {
	fr := withFakeRunner(t)
	op, err := StartOperation(context.Background(), OperationInput{Title: "create dev01", Instance: "dev01", Args: []string{"aws", "create", "dev01"}})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, op.ID, OperationRunning)
	if err := CancelOperation(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	got := waitState(t, op.ID, OperationCancelled)
	if got.ExitCode != 130 {
		t.Errorf("cancelled = %+v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		fr.mu.Lock()
		code, ok := fr.exited[op.ID]
		fr.mu.Unlock()
		if ok {
			if code != 130 {
				t.Errorf("exit code %d", code)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner was not ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOperationStatesOfGoneRunners(t *testing.T) {
	withTunnelStateDir(t)
	dir, _ := opsDir()
	now := time.Now().UTC()
	prev := opsHooks
	t.Cleanup(func() { opsHooks = prev })
	opsHooks.now = func() time.Time { return now }
	write := func(id string, started time.Time, runner *operationRunner) {
		d := filepath.Join(dir, id)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = writeJSONFile(filepath.Join(d, "op.json"), Operation{ID: id, Title: id, Args: []string{"aws", "start", "x"}, StartedAt: started})
		if runner != nil {
			_ = writeJSONFile(filepath.Join(d, "runner.json"), runner)
		}
	}
	write("20261128T120000Z-00000001", now.Add(-time.Second), nil)                                                     // just started
	write("20261128T110000Z-00000002", now.Add(-time.Hour), nil)                                                       // never recorded a runner
	write("20261128T100000Z-00000003", now.Add(-time.Hour), &operationRunner{ControlPort: freePort(t), Token: "gone"}) // runner died
	for id, want := range map[string]OperationState{
		"20261128T120000Z-00000001": OperationRunning,
		"20261128T110000Z-00000002": OperationInterrupted,
		"20261128T100000Z-00000003": OperationInterrupted,
	} {
		if op, err := GetOperation(context.Background(), id); err != nil || op.State != want {
			t.Errorf("%s: %+v %v, want %s", id, op, err, want)
		}
	}
	for _, bad := range []string{"../../etc", "20261128T120000Z-0000000", "x", ""} {
		if _, err := GetOperation(context.Background(), bad); !errors.Is(err, ErrUnknownOperation) {
			t.Errorf("GetOperation(%q) = %v", bad, err)
		}
		if _, _, err := OperationOutput(bad); !errors.Is(err, ErrUnknownOperation) {
			t.Errorf("OperationOutput(%q) = %v", bad, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "not-an-operation"), 0o700); err != nil {
		t.Fatal(err)
	}
	if ops, err := ListOperations(context.Background()); err != nil || len(ops) != 3 || ops[0].ID != "20261128T120000Z-00000001" {
		t.Errorf("list = %+v, %v", ops, err)
	}

	// Pruning removes what ended (or was interrupted) more than maxAge
	// ago and keeps the rest.
	old := "20261120T000000Z-00000004"
	write(old, now.Add(-8*24*time.Hour), nil)
	_ = writeJSONFile(filepath.Join(dir, old, "exit.json"), operationExit{Code: 0, EndedAt: now.Add(-8 * 24 * time.Hour)})
	if err := PruneOperations(context.Background(), OperationRetention); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Errorf("an operation older than the retention must go: %v", err)
	}
	for _, keep := range []string{"20261128T120000Z-00000001", "20261128T110000Z-00000002", "20261128T100000Z-00000003"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s must stay: %v", keep, err)
		}
	}
}

func TestOperationOutputKeepsTheEnd(t *testing.T) {
	withTunnelStateDir(t)
	dir, _ := opsDir()
	id := "20261128T120000Z-000000aa"
	d := filepath.Join(dir, id)
	_ = os.MkdirAll(d, 0o700)
	big := strings.Repeat("x", maxOperationOutput) + "THE END"
	_ = os.WriteFile(filepath.Join(d, "log"), []byte(big), 0o600)
	_, log, err := OperationOutput(id)
	if err != nil || len(log) != maxOperationOutput || !strings.HasSuffix(log, "THE END") {
		t.Errorf("log tail: %d bytes, %v", len(log), err)
	}
}

func TestStartOperationRecordsAFailedSpawn(t *testing.T) {
	withTunnelStateDir(t)
	prev := opsHooks
	t.Cleanup(func() { opsHooks = prev })
	opsHooks.spawn = func(string, *os.File, *os.File) error { return errors.New("exec format error") }
	if _, err := StartOperation(context.Background(), OperationInput{Title: "start x", Instance: "x", Args: []string{"aws", "start", "x"}}); err == nil {
		t.Fatal("a failed spawn must be an error")
	}
	ops, _ := ListOperations(context.Background())
	if len(ops) != 1 || ops[0].State != OperationFailed {
		t.Errorf("the failed start must be recorded as failed: %+v", ops)
	}
	if _, log, _ := OperationOutput(ops[0].ID); !strings.Contains(log, "exec format error") {
		t.Errorf("log = %q", log)
	}
	if _, err := StartOperation(context.Background(), OperationInput{}); err == nil {
		t.Error("an operation without a command must be refused")
	}
}
