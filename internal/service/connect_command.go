package service

// Command and terminal modes of the session-runner child (Issues #70
// and #75, ADR 0021).
//
// The vendored session-manager-plugin maps both NonInteractiveCommands
// and InteractiveCommands sessions to its Standard_Stream handler
// (shellsession.ShellSession, see datachannel/streaming.go), which
// breaks `connect <name> <command>` in two ways:
//
//   - It reads the local standard input and ends the session with
//     io.EOF as soon as that input ends (#70). For a non-interactive
//     command this is pointless as well as harmful: the SSM agent never
//     writes client input to the command's stdin (it only turns control
//     bytes into signals).
//   - It prints every payload to stdout and always exits 0, so the
//     remote stderr and exit status are lost (#75).
//
// Command mode therefore replaces Standard_Stream, for this child
// process only, with commandSession, which never touches stdin or the
// terminal and forwards Ctrl-C and friends. The remote command runs
// inside a small wrapper (commandWrapper): its stdout and stderr go to
// private temporary files, and once it has finished the wrapper prints
// them, then its exit status, as frames delimited by a per-run random
// marker. commandSession decodes the frames (frameDecoder) back into
// stdout, stderr and the status.
//
// The agent's own separateOutputStream mode would do this without a
// wrapper, but agent 3.3.4793.0 (the snap on the developer instance
// when this was written) closes that session about 2 s after the
// command ends whatever is still queued: outputs beyond about 300 KB
// arrived truncated. 3.3.4851.0
// fixed it ("Fix non-interactive session big file transfer issues").
// The plain mode used here sends a file paced by the agent and arrived
// complete (1.3 MB tested). Because the status frame is the last thing
// sent, a truncated transfer is always detected: no status, exit 255.
//
// Terminal mode keeps ShellSession (escape sequences, resize, raw
// keystrokes) and, when the local input is not a terminal, feeds it a
// pipe that never ends, sending Ctrl-D in place of the end of input.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/aws/session-manager-plugin/src/config"
	smplog "github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/session-manager-plugin/src/message"
	smpsession "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"
	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/sessionutil"
)

// sessionMode tells the session-runner child how to drive the plugin.
type sessionMode string

const (
	// sessionModeRaw leaves the plugin as it is (tunnel, ssh).
	sessionModeRaw sessionMode = ""
	// sessionModeCommand runs a non-interactive command through
	// commandSession.
	sessionModeCommand sessionMode = "command"
	// sessionModeTerminal is the interactive shell and --tty.
	sessionModeTerminal sessionMode = "terminal"
)

// localFailureExitCode is what connect exits with in command mode when
// the remote command's own status is not available: nakama failed, the
// session broke, or the agent did not report it. ssh uses the same
// value, so a script can tell it from the remote command's status.
const localFailureExitCode = 255

// sessionStatusRemoteExit prefixes the line the child writes to the
// status pipe once the agent reported the remote exit status.
const sessionStatusRemoteExit = "remote-exit"

// formatSessionStatus renders the status line for code.
func formatSessionStatus(code int) string {
	return fmt.Sprintf("%s %d\n", sessionStatusRemoteExit, code)
}

// parseSessionStatus returns the remote exit status the child reported,
// or nil when it reported none.
func parseSessionStatus(b []byte) *int {
	var found *int
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), sessionStatusRemoteExit+" ")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
			found = &n
		}
	}
	return found
}

// newCommandMarker returns the per-run frame marker: random, so that no
// output of the remote command can forge a frame.
func newCommandMarker() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate output marker: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// commandFramePrefix starts every frame header line:
// "\n" + commandFramePrefix + marker + " " + kind + " " + number + "\n".
// The leading newline lets a header follow any output; the decoder
// discards it.
const commandFramePrefix = "@@nakama-frame "

// Frame kinds: stdout and stderr carry the byte count of the body that
// follows; exit carries the status and ends the stream.
const (
	frameStdout = "o"
	frameStderr = "e"
	frameExit   = "x"
)

// commandWrapper returns the shell program that runs command and frames
// its output. It runs as the ubuntu user (sudo su - ubuntu -c). The
// command runs in a subshell, as the -c argument always did, with
// stdin from /dev/null; its outputs go to files in a private directory
// (mktemp -d is 0700) that the EXIT trap removes. The wrapper's own
// lines never print the marker except as headers, and print nothing
// once the command has started but before it finished, so frames
// appear only after the command is done.
func commandWrapper(command, marker string) string {
	h := commandFramePrefix + marker
	// The command runs in its own process group (set -m gives the
	// background job a new group) so that Ctrl-C can take down the whole
	// subtree, not just the login shell su kills (Issue #77). On
	// INT/TERM/HUP the wrapper forwards SIGINT to that group and, as a
	// backstop for a command that ignores it, SIGKILLs the group after a
	// second — comfortably inside the ~2 s su waits before it SIGKILLs
	// the wrapper itself. The wait loop re-waits because a trapped signal
	// makes wait return early; it ends with the command's real status, so
	// Ctrl-C yields 130 (or whatever the command chose). The frames are
	// then emitted as before, so the output collected so far survives.
	//
	// exec 2>/dev/null silences the wrapper shell's own job-control
	// notices ("[1] Killed ..."); the command's stderr is captured
	// separately in $d/e. The redirections create $d/o and $d/e before
	// the command runs, so wc/cat always succeed.
	//
	// The command starts on the first line, so the shell's error messages
	// ("line 2: ...") count lines of the command itself.
	return `d=$(mktemp -d) || exit 255; trap 'rm -rf "$d"' EXIT; exec 2>/dev/null; set -m; ( ` + command + `
) </dev/null >"$d/o" 2>"$d/e" &
cmdpid=$!; set +m
trap 'kill -INT -"$cmdpid" 2>/dev/null; ( sleep 1; kill -KILL -"$cmdpid" 2>/dev/null ) &' INT TERM HUP
rc=0; while :; do wait "$cmdpid"; rc=$?; kill -0 "$cmdpid" 2>/dev/null || break; done
printf '\n` + h + ` ` + frameStdout + ` %s\n' "$(wc -c <"$d/o" | tr -d ' ')"; cat "$d/o"
printf '\n` + h + ` ` + frameStderr + ` %s\n' "$(wc -c <"$d/e" | tr -d ' ')"; cat "$d/e"
printf '\n` + h + ` ` + frameExit + ` %s\n' "$rc"
`
}

// frameDecoder turns the wrapper's framed output back into stdout,
// stderr and the exit status. It accepts the stream in pieces of any
// size. Output before the first frame (login banners from
// "su -", for example) is passed to stderr, so that stdout carries the
// command's stdout only.
type frameDecoder struct {
	stdout, stderr io.Writer
	header         []byte // "\n@@nakama-frame <marker> "

	buf       []byte // unparsed input
	body      io.Writer
	remaining int64 // bytes left in the current body
	exitCode  *int
	broken    error
}

func newFrameDecoder(marker string, stdout, stderr io.Writer) *frameDecoder {
	return &frameDecoder{
		stdout: stdout,
		stderr: stderr,
		header: []byte("\n" + commandFramePrefix + marker + " "),
	}
}

// maxFrameHeader bounds a header line after the marker ("x -1\n").
const maxFrameHeader = 32

// Write consumes the next piece of the stream. It never fails, so the
// plugin keeps acknowledging data; a malformed stream is remembered in
// broken and the rest goes to stderr.
func (d *frameDecoder) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for len(d.buf) > 0 {
		if d.broken != nil || d.exitCode != nil {
			// After the exit frame (the wrapper prints nothing more) or
			// after a malformed header, keep the bytes visible.
			_, _ = d.stderr.Write(d.buf)
			d.buf = d.buf[:0]
			break
		}
		if d.remaining > 0 {
			n := int64(len(d.buf))
			if n > d.remaining {
				n = d.remaining
			}
			_, _ = d.body.Write(d.buf[:n])
			d.buf = d.buf[n:]
			d.remaining -= n
			continue
		}
		i := bytes.Index(d.buf, d.header)
		if i < 0 {
			// Pass on everything that cannot be the start of a header.
			keep := len(d.header) - 1
			if keep > len(d.buf) {
				keep = len(d.buf)
			}
			if j := len(d.buf) - keep; j > 0 {
				_, _ = d.stderr.Write(d.buf[:j])
				d.buf = d.buf[j:]
			}
			if !mayStartHeader(d.buf, d.header) {
				_, _ = d.stderr.Write(d.buf)
				d.buf = d.buf[:0]
			}
			break
		}
		if i > 0 {
			_, _ = d.stderr.Write(d.buf[:i])
			d.buf = d.buf[i:]
		}
		rest := d.buf[len(d.header):]
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			if len(rest) > maxFrameHeader {
				d.broken = fmt.Errorf("malformed output frame header")
			}
			break // wait for the end of the header line
		}
		if err := d.startFrame(string(rest[:nl])); err != nil {
			d.broken = err
			continue
		}
		d.buf = rest[nl+1:]
	}
	return len(p), nil
}

// mayStartHeader reports whether tail could be the beginning of header.
func mayStartHeader(tail, header []byte) bool {
	for i := range tail {
		if bytes.HasPrefix(header, tail[i:]) {
			return true
		}
	}
	return false
}

// startFrame applies one header line ("o 123", "e 0", "x 3").
func (d *frameDecoder) startFrame(line string) error {
	kind, num, ok := strings.Cut(line, " ")
	n, err := strconv.ParseInt(num, 10, 64)
	if !ok || err != nil {
		return fmt.Errorf("malformed output frame header %q", line)
	}
	switch {
	case kind == frameStdout && n >= 0:
		d.body, d.remaining = d.stdout, n
	case kind == frameStderr && n >= 0:
		d.body, d.remaining = d.stderr, n
	case kind == frameExit:
		if n < -1 || n > 1<<20 {
			return fmt.Errorf("malformed exit status %d", n)
		}
		code := int(n)
		d.exitCode = &code
	default:
		return fmt.Errorf("malformed output frame header %q", line)
	}
	return nil
}

// finish flushes what is left and returns the exit status, or nil when
// the stream did not carry a complete one (truncated or malformed).
func (d *frameDecoder) finish() *int {
	if len(d.buf) > 0 {
		_, _ = d.stderr.Write(d.buf)
		d.buf = nil
	}
	if d.broken != nil || d.remaining > 0 {
		return nil
	}
	return d.exitCode
}

// remoteExitError maps the remote command's exit status to connect's
// result: nil for 0, otherwise an ExitCodeError that, like ssh, makes
// the status nakama's own exit code without printing anything.
func remoteExitError(code int) error {
	switch {
	case code == 0:
		return nil
	case code > 0 && code <= 255:
		return &ExitCodeError{Code: code, Remote: true}
	case code < 0:
		// The agent reports -1 when the command was killed by a signal
		// (exec.ExitError.ExitCode).
		return &ExitCodeError{Code: localFailureExitCode, Message: fmt.Sprintf(
			"the remote command did not exit normally (the SSM agent reported status %d; it was probably killed by a signal)", code)}
	default:
		return &ExitCodeError{Code: localFailureExitCode, Message: fmt.Sprintf(
			"the remote command exited with status %d, which does not fit in a process exit code", code)}
	}
}

// errNoRemoteExit is connect's result in command mode when the session
// ended without the agent reporting the command's exit status.
var errNoRemoteExit = &ExitCodeError{Code: localFailureExitCode, Message: "the session ended before the remote command reported its exit status " +
	"(the session was terminated or interrupted, or its output did not arrive complete); the command may still have run"}

// commandSession is the Standard_Stream handler for command mode.
type commandSession struct {
	smpsession.Session

	stderr io.Writer
	// status receives the remote exit status line; nil when the parent
	// did not ask for it.
	status io.Writer

	mu      sync.Mutex
	decoder *frameDecoder

	// hint, when non-nil, receives a one-line notice if the command
	// produces nothing for hintDelay: in command mode the output arrives
	// only once the command finishes, which looks like a hang for a
	// command that waits for input or prints a code (Issue #77).
	hint      io.Writer
	hintDelay time.Duration
	hintTimer *time.Timer
	hintOnce  sync.Once

	// notify and exit are signal.Notify and os.Exit outside tests.
	notify func(c chan<- os.Signal, sig ...os.Signal)
	exit   func(code int)
}

var _ smpsession.ISessionPlugin = (*commandSession)(nil)

// commandHintDelay is how long a command may produce nothing before the
// hint is printed.
const commandHintDelay = 10 * time.Second

// commandHintText tells the user why nothing is showing and what to do.
const commandHintText = "nakama: the remote command is still running; in this mode its output appears only after it finishes.\n" +
	"If it is waiting for input or showing a code (e.g. kiro-cli login), press Ctrl-C and re-run with --tty.\n"

func newCommandSession(marker string, stdout, stderr, status, hint io.Writer, hintDelay time.Duration) *commandSession {
	return &commandSession{
		stderr:    stderr,
		status:    status,
		decoder:   newFrameDecoder(marker, stdout, stderr),
		hint:      hint,
		hintDelay: hintDelay,
		notify:    signal.Notify,
		exit:      os.Exit,
	}
}

// installCommandSession makes command mode take over Standard_Stream in
// this process. The plugin prints its own banners ("Starting session
// with SessionId: ...") to os.Stdout; pointing os.Stdout at stderr
// keeps the command's stdout clean, like ssh. The hint is armed only on
// an interactive terminal, where a human is watching and could be
// misled by the delayed output.
func installCommandSession(marker string, status io.Writer) {
	var hint io.Writer
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) {
		hint = os.Stderr
	}
	smpsession.Register(newCommandSession(marker, os.Stdout, os.Stderr, status, hint, commandHintDelay))
	os.Stdout = os.Stderr
}

// Name registers the handler under Standard_Stream, the name the plugin
// maps NonInteractiveCommands sessions to.
func (*commandSession) Name() string { return config.ShellPluginName }

// Initialize wires the handler into the data channel, as
// ShellSession.Initialize does.
func (c *commandSession) Initialize(log smplog.T, sessionVar *smpsession.Session) {
	c.Session = *sessionVar
	c.DataChannel.RegisterOutputStreamHandler(c.ProcessStreamMessagePayload, true)
	c.DataChannel.GetWsChannel().SetOnMessage(func(input []byte) {
		_ = c.DataChannel.OutputMessageHandler(log, c.Stop, c.SessionId, input)
	})
}

// armHint starts the hint timer once the session is running. The hint
// fires only if nothing arrives before hintDelay; the first payload or
// Stop cancels it (cancelHint).
func (c *commandSession) armHint() {
	if c.hint == nil || c.hintDelay <= 0 {
		return
	}
	t := time.AfterFunc(c.hintDelay, func() {
		c.hintOnce.Do(func() { _, _ = io.WriteString(c.hint, commandHintText) })
	})
	c.mu.Lock()
	c.hintTimer = t
	c.mu.Unlock()
}

// cancelHint stops the hint timer; output is arriving or the session
// ended, so the command is not stuck waiting.
func (c *commandSession) cancelHint() {
	c.mu.Lock()
	t := c.hintTimer
	c.mu.Unlock()
	if t != nil {
		t.Stop()
	}
}

// SetSessionHandlers forwards Ctrl-C, Ctrl-\ and (on unix) Ctrl-Z as
// the control bytes the agent turns into signals for the command, and
// blocks for the life of the session, which ends in Stop. Standard
// input is never read: the agent would discard it (#70).
func (c *commandSession) SetSessionHandlers(log smplog.T) error {
	c.armHint()
	signals := make(chan os.Signal, 1)
	c.notify(signals, sessionutil.ControlSignals...)
	for sig := range signals {
		b, ok := sessionutil.SignalsByteMap[sig]
		if !ok {
			continue
		}
		if err := c.DataChannel.SendInputDataMessage(log, message.Output, []byte{b}); err != nil {
			_ = log.Errorf("Failed to send control signal: %v", err)
		}
	}
	return nil
}

// ProcessStreamMessagePayload routes one payload from the agent: the
// framed output goes through the decoder, anything else (the agent's
// own error messages) to stderr.
func (c *commandSession) ProcessStreamMessagePayload(_ smplog.T, msg message.ClientMessage) (bool, error) {
	c.cancelHint() // data is arriving: the command has finished and is sending
	c.mu.Lock()
	defer c.mu.Unlock()
	if message.PayloadType(msg.PayloadType) == message.Output {
		_, _ = c.decoder.Write(msg.Payload)
	} else {
		_, _ = c.stderr.Write(msg.Payload)
	}
	return true, nil
}

// Stop runs when the channel is closed: it hands the remote exit status
// to the parent and ends the process, as the plugin's handlers do.
func (c *commandSession) Stop() {
	c.cancelHint()
	c.mu.Lock()
	code := c.decoder.finish()
	broken := c.decoder.broken
	c.mu.Unlock()
	if broken != nil {
		_, _ = fmt.Fprintf(c.stderr, "nakama: %v\n", broken)
	}
	if code != nil && c.status != nil {
		_, _ = io.WriteString(c.status, formatSessionStatus(*code))
	}
	c.exit(0)
}

// keepTerminalInputOpen holds the write ends of the terminal input
// pipes. An *os.File closes its descriptor when it is garbage
// collected; the write end must stay open for the life of the process
// or ShellSession would see EOF after all (#70).
var (
	keepTerminalInputOpenMu sync.Mutex
	keepTerminalInputOpen   []*os.File
)

// pipeNonTerminalStdin replaces os.Stdin with a never-ending pipe fed by
// copyTerminalInput when the local input is not a terminal, so terminal
// mode works with `< file`, `| ...` or a closed stdin. A real terminal
// is left alone: ShellSession puts it in cbreak mode and reads
// keystrokes from it.
func pipeNonTerminalStdin() error {
	if runtime.GOOS == "windows" {
		// The Windows ShellSession reads keystrokes through the console
		// API, not os.Stdin, so redirected input cannot be fed to it
		// this way; it still needs a console (ADR 0021).
		return nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	r, err := pipeTerminalInput(os.Stdin)
	if err != nil {
		return err
	}
	os.Stdin = r
	return nil
}

// pipeTerminalInput returns the read end of a pipe that receives src
// through copyTerminalInput and is never closed by the writer.
func pipeTerminalInput(src io.Reader) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create terminal input pipe: %w", err)
	}
	keepTerminalInputOpenMu.Lock()
	keepTerminalInputOpen = append(keepTerminalInputOpen, w)
	keepTerminalInputOpenMu.Unlock()
	go func() { _ = copyTerminalInput(w, src) }()
	return r, nil
}

// copyTerminalInput copies src to dst for ShellSession and, when src
// ends, sends Ctrl-D so the remote side ends the session the way a
// shell ends at end of input. A last line without a newline gets one
// first, so it is run rather than left half-typed. dst is not closed.
func copyTerminalInput(dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	atLineStart := false // ShellSession starts "not after a newline" too
	var last byte
	seen := false
	for {
		n, err := src.Read(buf)
		if chunk := buf[:n]; len(chunk) > 0 {
			if _, werr := dst.Write(escapeTerminalInput(chunk, &atLineStart)); werr != nil {
				return werr
			}
			last, seen = chunk[len(chunk)-1], true
		}
		if err == nil {
			continue
		}
		eof := []byte{0x04}
		if seen && last != '\n' && last != '\r' {
			eof = []byte{'\n', 0x04}
		}
		if _, werr := dst.Write(eof); werr != nil {
			return werr
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

// escapeTerminalInput doubles a "~" at the start of a line so that
// ShellSession's ssh-like escapes ("~." terminates the session, "~?"
// prints help) never fire on piped data; ShellSession turns "~~" back
// into "~". atLineStart carries the state across chunks and mirrors
// ShellSession's own tracking (shellsession.go, handleEscapeSequence):
// a line start follows '\n' or '\r', and the stream does not begin at
// one. TestTerminalInputSurvivesShellSessionEscapes checks the pair
// against the vendored ShellSession.
func escapeTerminalInput(chunk []byte, atLineStart *bool) []byte {
	out := make([]byte, 0, len(chunk)+8)
	for _, b := range chunk {
		switch {
		case *atLineStart && b == '~':
			out = append(out, '~', '~')
			*atLineStart = false
		case b == '\n' || b == '\r':
			out = append(out, b)
			*atLineStart = true
		default:
			out = append(out, b)
			*atLineStart = false
		}
	}
	return out
}
