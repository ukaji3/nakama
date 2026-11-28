package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/ukaji3/nakama/internal/version"
)

// Operations of the control panel (Issue #139, ADR 0031 decision 5):
// every state-changing action of the panel runs the CLI command itself
// (`nakama aws <command> … --yes -o json`) in a process detached from
// the panel's window, so that closing the window does not stop a
// create, a restore or a delete halfway. The hidden command
// `nakama aws op-runner --id <id>` is that process: it runs the command
// tree in-process with its standard output and error redirected to
// files, answers a control channel (alive? stop!), and records the exit
// status. Everything lives in ~/.config/nakama/ops/<id>/ (0700 / 0600):
//
//	op.json      what to run (the arguments after `nakama`) and for which instance
//	runner.json  the runner's PID and control channel (while it runs)
//	stdout       the command's result (-o json)
//	log          its standard error (progress and errors)
//	exit.json    the exit code, when the command ended (or was stopped)

// OperationState says where an operation is.
type OperationState string

// Operation states.
const (
	OperationRunning     OperationState = "running"
	OperationSucceeded   OperationState = "succeeded"
	OperationFailed      OperationState = "failed"
	OperationCancelled   OperationState = "cancelled"
	OperationInterrupted OperationState = "interrupted"
)

// OperationInput describes an operation to start.
type OperationInput struct {
	// Title is what the panel shows (e.g. "create dev01").
	Title string
	// Instance is the instance the operation changes ("" for none).
	Instance string
	// Exclusive operations (init) run alone.
	Exclusive bool
	// Args are the CLI arguments after `nakama`.
	Args []string
}

// Operation is a started operation as recorded on disk.
type Operation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Instance  string    `json:"instance,omitempty"`
	Exclusive bool      `json:"exclusive,omitempty"`
	Args      []string  `json:"args"`
	StartedAt time.Time `json:"startedAt"`
	Version   string    `json:"version"`

	// Filled in when read.
	State    OperationState `json:"-"`
	ExitCode int            `json:"-"`
	EndedAt  time.Time      `json:"-"`
}

// Done reports whether the operation has ended.
func (o *Operation) Done() bool { return o.State != OperationRunning }

type operationRunner struct {
	PID         int    `json:"pid"`
	ControlPort int    `json:"controlPort"`
	Token       string `json:"token"`
}

type operationExit struct {
	Code      int       `json:"code"`
	Cancelled bool      `json:"cancelled,omitempty"`
	EndedAt   time.Time `json:"endedAt"`
}

// OperationConflictError reports that a running operation blocks a new
// one (the same instance, or an exclusive operation).
type OperationConflictError struct {
	Running *Operation
}

func (e *OperationConflictError) Error() string {
	return fmt.Sprintf("%q is still running; wait for it to finish", e.Running.Title)
}

// operationIDPattern keeps ids from the browser inside the ops dir.
var operationIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

// ErrUnknownOperation reports an id that names no operation.
var ErrUnknownOperation = errors.New("no such operation")

// operationStartGrace is how long an operation counts as running before
// its runner has recorded its control channel.
var operationStartGrace = 30 * time.Second

// opsHooks are the side effects of the operations (tests substitute
// fakes).
var opsHooks = struct {
	spawn func(id string, stdout, stderr *os.File) error
	now   func() time.Time
}{spawn: spawnOperationRunner, now: time.Now}

// opsDir is ~/.config/nakama/ops.
func opsDir() (string, error) {
	dir, err := tunnelStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "ops"), nil
}

func opDir(id string) (string, error) {
	if !operationIDPattern.MatchString(id) {
		return "", ErrUnknownOperation
	}
	dir, err := opsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id), nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path) // #nosec G304 -- paths under the ops dir, ids validated
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// opsMu serializes the conflict check and the start of operations
// within this process (the panel).
var opsMu sync.Mutex

// StartOperation records an operation and starts its runner. It refuses
// while a running operation touches the same instance, or while an
// exclusive one runs (or when the new one is exclusive and anything
// runs).
func StartOperation(ctx context.Context, in OperationInput) (*Operation, error) {
	if len(in.Args) == 0 {
		return nil, errors.New("an operation needs a command")
	}
	opsMu.Lock()
	defer opsMu.Unlock()
	ops, err := ListOperations(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ops {
		o := &ops[i]
		if o.Done() {
			continue
		}
		if in.Exclusive || o.Exclusive || (in.Instance != "" && o.Instance == in.Instance) {
			return nil, &OperationConflictError{Running: o}
		}
	}
	dir, err := opsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the operations directory: %w", err)
	}
	now := opsHooks.now().UTC()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	op := &Operation{
		ID:        now.Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix),
		Title:     in.Title,
		Instance:  in.Instance,
		Exclusive: in.Exclusive,
		Args:      append([]string(nil), in.Args...),
		StartedAt: now,
		Version:   version.String(),
		State:     OperationRunning,
	}
	d := filepath.Join(dir, op.ID)
	if err := os.Mkdir(d, 0o700); err != nil {
		return nil, fmt.Errorf("create the operation directory: %w", err)
	}
	if err := writeJSONFile(filepath.Join(d, "op.json"), op); err != nil {
		return nil, fmt.Errorf("record the operation: %w", err)
	}
	stdout, err := os.OpenFile(filepath.Join(d, "stdout"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- under the ops dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = stdout.Close() }()
	logf, err := os.OpenFile(filepath.Join(d, "log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- under the ops dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = logf.Close() }()
	if err := opsHooks.spawn(op.ID, stdout, logf); err != nil {
		_ = writeJSONFile(filepath.Join(d, "exit.json"), operationExit{Code: -1, EndedAt: opsHooks.now().UTC()})
		_, _ = fmt.Fprintf(logf, "could not start the operation: %v\n", err)
		return nil, fmt.Errorf("start the operation: %w", err)
	}
	return op, nil
}

// operationRunnerArgs is the argv (after the binary) of the hidden
// runner; it must match internal/cli (aws_op_runner.go).
var operationRunnerArgs = []string{"aws", "op-runner", "--id"}

// spawnOperationRunner starts the runner detached from this process and
// its window, like the tunnel daemons (Windows: DETACHED_PROCESS, no
// console window).
func spawnOperationRunner(id string, stdout, stderr *os.File) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the nakama binary: %w", err)
	}
	cmd := exec.Command(exe, append(append([]string{}, operationRunnerArgs...), id)...) // #nosec G204 -- the re-exec'd nakama binary and a validated id
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = tunnelDaemonSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// readOperation loads one operation and works out its state.
func readOperation(ctx context.Context, id string) (*Operation, error) {
	d, err := opDir(id)
	if err != nil {
		return nil, err
	}
	var op Operation
	if err := readJSONFile(filepath.Join(d, "op.json"), &op); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrUnknownOperation
		}
		return nil, err
	}
	var ex operationExit
	switch err := readJSONFile(filepath.Join(d, "exit.json"), &ex); {
	case err == nil:
		op.ExitCode, op.EndedAt = ex.Code, ex.EndedAt
		switch {
		case ex.Cancelled:
			op.State = OperationCancelled
		case ex.Code == 0:
			op.State = OperationSucceeded
		default:
			op.State = OperationFailed
		}
		return &op, nil
	case !os.IsNotExist(err):
		return nil, err
	}
	var rn operationRunner
	if err := readJSONFile(filepath.Join(d, "runner.json"), &rn); err == nil {
		qctx, cancel := context.WithTimeout(ctx, tunnelControlTimeout)
		err := controlRequest(qctx, rn.ControlPort, rn.Token, http.MethodGet, "/status", nil, nil)
		cancel()
		if err == nil {
			op.State = OperationRunning
			return &op, nil
		}
		// The runner may have ended between the two reads.
		if err := readJSONFile(filepath.Join(d, "exit.json"), &ex); err == nil {
			return readOperation(ctx, id)
		}
		op.State = OperationInterrupted
		return &op, nil
	}
	if opsHooks.now().Sub(op.StartedAt) < operationStartGrace {
		op.State = OperationRunning
		return &op, nil
	}
	op.State = OperationInterrupted
	return &op, nil
}

// GetOperation returns one operation.
func GetOperation(ctx context.Context, id string) (*Operation, error) {
	return readOperation(ctx, id)
}

// ListOperations returns the recorded operations, newest first.
func ListOperations(ctx context.Context) ([]Operation, error) {
	dir, err := opsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ops []Operation
	for _, e := range entries {
		if !e.IsDir() || !operationIDPattern.MatchString(e.Name()) {
			continue
		}
		op, err := readOperation(ctx, e.Name())
		if err != nil {
			continue // half-written by a start that failed, or removed meanwhile
		}
		ops = append(ops, *op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].StartedAt.After(ops[j].StartedAt) })
	return ops, nil
}

// maxOperationOutput bounds what OperationOutput returns of each file:
// the end of a long log is the part that matters.
const maxOperationOutput = 256 << 10

func readTail(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- under the ops dir, ids validated
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > maxOperationOutput {
		if _, err := f.Seek(-maxOperationOutput, io.SeekEnd); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(f)
	return string(b), err
}

// OperationOutput returns the command's result (standard output) and
// its log (standard error), each cut to its last 256 KiB.
func OperationOutput(id string) (stdout, log string, err error) {
	d, err := opDir(id)
	if err != nil {
		return "", "", err
	}
	if stdout, err = readTail(filepath.Join(d, "stdout")); err != nil {
		return "", "", err
	}
	if log, err = readTail(filepath.Join(d, "log")); err != nil {
		return "", "", err
	}
	return stdout, log, nil
}

// CancelOperation stops a running operation the way Ctrl-C stops the
// CLI command.
func CancelOperation(ctx context.Context, id string) error {
	d, err := opDir(id)
	if err != nil {
		return err
	}
	var rn operationRunner
	if err := readJSONFile(filepath.Join(d, "runner.json"), &rn); err != nil {
		if os.IsNotExist(err) {
			return errors.New("the operation is not running")
		}
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, tunnelControlTimeout)
	defer cancel()
	return controlRequest(ctx, rn.ControlPort, rn.Token, http.MethodPost, "/stop", nil, nil)
}

// OperationRetention is how long finished operations are kept.
const OperationRetention = 7 * 24 * time.Hour

// PruneOperations removes operations that ended (or were interrupted)
// longer than maxAge ago.
func PruneOperations(ctx context.Context, maxAge time.Duration) error {
	ops, err := ListOperations(ctx)
	if err != nil {
		return err
	}
	dir, err := opsDir()
	if err != nil {
		return err
	}
	now := opsHooks.now()
	for _, op := range ops {
		if !op.Done() {
			continue
		}
		at := op.EndedAt
		if at.IsZero() {
			at = op.StartedAt
		}
		if now.Sub(at) > maxAge {
			if err := os.RemoveAll(filepath.Join(dir, op.ID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// RunOperation is the body of the hidden runner: it runs the recorded
// command with run (the CLI command tree, which writes to this process's
// standard output and error: the files StartOperation opened), answers
// the control channel while it runs, and records the exit code. A stop
// request records the operation as cancelled and ends the process at
// once through exit, as Ctrl-C ends the CLI.
func RunOperation(id string, run func(args []string) int, exit func(code int)) (int, error) {
	d, err := opDir(id)
	if err != nil {
		return 1, err
	}
	var op Operation
	if err := readJSONFile(filepath.Join(d, "op.json"), &op); err != nil {
		return 1, fmt.Errorf("read the operation: %w", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1, fmt.Errorf("listen on the operation's control channel: %w", err)
	}
	defer func() { _ = l.Close() }()
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return 1, err
	}
	rn := operationRunner{PID: os.Getpid(), ControlPort: l.Addr().(*net.TCPAddr).Port, Token: hex.EncodeToString(tok)}
	var exitOnce sync.Once
	finish := func(code int, cancelled bool) {
		exitOnce.Do(func() {
			_ = writeJSONFile(filepath.Join(d, "exit.json"), operationExit{Code: code, Cancelled: cancelled, EndedAt: opsHooks.now().UTC()})
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if !tunnelControlAuthorized(r, rn.Token) || r.Method != http.MethodGet {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "running\n")
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if !tunnelControlAuthorized(r, rn.Token) || r.Method != http.MethodPost {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		finish(130, true)
		_, _ = io.WriteString(w, "stopping\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(100 * time.Millisecond) // let the answer leave
			_, _ = fmt.Fprintln(os.Stderr, "nakama: the operation was stopped from the control panel")
			exit(130)
		}()
	})
	srv := newControlServer(mux)
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()
	if err := writeJSONFile(filepath.Join(d, "runner.json"), rn); err != nil {
		return 1, fmt.Errorf("record the operation's runner: %w", err)
	}
	code := run(op.Args)
	finish(code, false)
	return code, nil
}
