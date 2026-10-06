package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/session-manager-plugin/src/config"
	smplog "github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/session-manager-plugin/src/message"
	smpsession "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"
	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/sessionutil"
)

func intPtr(n int) *int { return &n }

func TestRemoteExitError(t *testing.T) {
	if err := remoteExitError(0); err != nil {
		t.Errorf("status 0 must be success, got %v", err)
	}
	for _, code := range []int{1, 7, 74, 255} {
		var ec *ExitCodeError
		if err := remoteExitError(code); !errors.As(err, &ec) || ec.Code != code || !ec.Remote {
			t.Errorf("remoteExitError(%d) = %#v, want the status itself, marked remote", code, err)
		}
	}
	// Statuses a process exit code cannot carry are nakama's 255 with an
	// explanation, never a silent (and possibly truncated-to-0) code.
	for code, want := range map[int]string{-1: "killed by a signal", 256: "does not fit"} {
		var ec *ExitCodeError
		err := remoteExitError(code)
		if !errors.As(err, &ec) || ec.Code != localFailureExitCode || ec.Remote || !strings.Contains(err.Error(), want) {
			t.Errorf("remoteExitError(%d) = %#v, want 255 with %q", code, err, want)
		}
	}
}

func TestExitCodeErrorText(t *testing.T) {
	for e, want := range map[*ExitCodeError]string{
		{Code: 1}:                   "session runner exited with code 1",
		{Code: 3, Remote: true}:     "remote command exited with status 3",
		{Code: 255, Message: "why"}: "why",
	} {
		if got := e.Error(); got != want {
			t.Errorf("%#v.Error() = %q, want %q", e, got, want)
		}
	}
}

func TestSessionStatusRoundTrip(t *testing.T) {
	for _, code := range []int{-1, 0, 1, 7, DisconnectExitCode, 255} {
		got := parseSessionStatus([]byte(formatSessionStatus(code)))
		if got == nil || *got != code {
			t.Errorf("round trip of %d = %v", code, got)
		}
	}
	for _, none := range []string{"", "\n", "remote-exit\n", "remote-exit x\n", "other 3\n"} {
		if got := parseSessionStatus([]byte(none)); got != nil {
			t.Errorf("parseSessionStatus(%q) = %d, want nil", none, *got)
		}
	}
	if got := parseSessionStatus([]byte("remote-exit 1\nremote-exit 2\n")); got == nil || *got != 2 {
		t.Errorf("the last status wins, got %v", got)
	}
}

// testCommandSession returns a commandSession wired to buffers, with
// exit recorded instead of ending the test process.
const testMarker = "0123456789abcdef0123456789abcdef"

func testCommandSession(status io.Writer) (cs *commandSession, stdout, stderr *bytes.Buffer, exits *[]int) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	exits = &[]int{}
	cs = newCommandSession(testMarker, stdout, stderr, status, nil, 0) // hint off by default
	cs.exit = func(code int) { *exits = append(*exits, code) }
	return cs, stdout, stderr, exits
}

// framed renders what the wrapper prints for the given outputs.
func framed(marker, stdout, stderr string, exit int) string {
	h := "\n" + commandFramePrefix + marker + " "
	return h + "o " + strconv.Itoa(len(stdout)) + "\n" + stdout +
		h + "e " + strconv.Itoa(len(stderr)) + "\n" + stderr +
		h + "x " + strconv.Itoa(exit) + "\n"
}

func payloadMessage(t message.PayloadType, data string) message.ClientMessage {
	return message.ClientMessage{PayloadType: uint32(t), Payload: []byte(data)}
}

func TestCommandSessionInitializeWiresTheDataChannel(t *testing.T) {
	cs, _, _, exits := testCommandSession(&bytes.Buffer{})
	dc := newRecordingDataChannel()
	logger := smplog.Logger(false, "nakama-test")
	cs.Initialize(logger, &smpsession.Session{DataChannel: dc, SessionId: "s-1"})
	if len(dc.handlers) != 1 {
		t.Fatalf("want one output handler registered, got %d", len(dc.handlers))
	}
	if dc.ws.onMessage == nil {
		t.Fatal("the websocket message handler must be set")
	}
	dc.ws.onMessage([]byte("raw"))
	if len(dc.stops) != 1 {
		t.Fatalf("incoming messages must reach OutputMessageHandler with a stop handler, got %d", len(dc.stops))
	}
	dc.stops[0]() // the channel closed
	if len(*exits) != 1 {
		t.Errorf("the stop handler handed over must be the session's Stop, exits = %v", *exits)
	}
}

func TestCommandSessionForwardsControlSignalsAndNeverReadsStdin(t *testing.T) {
	// Data waiting on stdin must still be there afterwards: command mode
	// does not read it (#70).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := w.WriteString("typed-ahead"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	cs, _, _, _ := testCommandSession(&bytes.Buffer{})
	dc := newRecordingDataChannel()
	cs.Session = smpsession.Session{DataChannel: dc}
	registered := make(chan chan<- os.Signal, 1)
	cs.notify = func(c chan<- os.Signal, sig ...os.Signal) {
		if len(sig) != len(sessionutil.ControlSignals) {
			t.Errorf("notify for %v, want %v", sig, sessionutil.ControlSignals)
		}
		registered <- c
	}
	done := make(chan error, 1)
	go func() { done <- cs.SetSessionHandlers(smplog.Logger(false, "nakama-test")) }()

	c := <-registered
	var want []byte
	for _, sig := range sessionutil.ControlSignals {
		c <- sig
		want = append(want, sessionutil.SignalsByteMap[sig])
	}
	deadline := time.Now().Add(5 * time.Second)
	for !bytes.Equal(dc.sent(message.Output), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := dc.sent(message.Output); !bytes.Equal(got, want) {
		t.Errorf("control bytes sent = %q, want %q", got, want)
	}
	close(c)
	if err := <-done; err != nil {
		t.Errorf("SetSessionHandlers = %v", err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "typed-ahead" {
		t.Errorf("stdin was read: %q left of %q", rest, "typed-ahead")
	}
}

func TestInstallCommandSessionTakesOverStandardStream(t *testing.T) {
	prevPlugin := smpsession.SessionRegistry[config.ShellPluginName]
	prevStdout := os.Stdout
	t.Cleanup(func() {
		smpsession.SessionRegistry[config.ShellPluginName] = prevPlugin
		os.Stdout = prevStdout
	})
	status := &bytes.Buffer{}
	installCommandSession(testMarker, status)
	cs, ok := smpsession.SessionRegistry[config.ShellPluginName].(*commandSession)
	if !ok {
		t.Fatalf("Standard_Stream is %T, want *commandSession", smpsession.SessionRegistry[config.ShellPluginName])
	}
	if cs.decoder.stdout != prevStdout || cs.decoder.stderr != os.Stderr || cs.stderr != os.Stderr || cs.status != status {
		t.Error("the command's stdout must be the real stdout, its stderr stderr, and the status the given writer")
	}
	if !bytes.Contains(cs.decoder.header, []byte(testMarker)) {
		t.Error("the decoder must use the given marker")
	}
	if os.Stdout != os.Stderr {
		t.Error("the plugin's banners (written to os.Stdout) must go to stderr")
	}
}

func TestEscapeTerminalInput(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"~.", "~."}, // the stream does not start at a line start, as in ShellSession
		{"ls\n~.\n", "ls\n~~.\n"},
		{"a\r~?", "a\r~~?"},
		{"x~y\n", "x~y\n"},
		{"\n~~", "\n~~~"},
		{"\n~-\n~x", "\n~~-\n~~x"},
	} {
		atLineStart := false
		if got := string(escapeTerminalInput([]byte(tc.in), &atLineStart)); got != tc.want {
			t.Errorf("escapeTerminalInput(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The line-start state carries across chunks.
	atLineStart := false
	got := string(escapeTerminalInput([]byte("echo\n"), &atLineStart)) + string(escapeTerminalInput([]byte("~."), &atLineStart))
	if got != "echo\n~~." {
		t.Errorf("across chunks = %q", got)
	}
}

type failingReader struct{ data string }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.data == "" {
		return 0, errors.New("boom")
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestCopyTerminalInput(t *testing.T) {
	for in, want := range map[string]string{
		"":             "\x04",
		"exit 0\n":     "exit 0\n\x04",
		"partial":      "partial\n\x04", // a last line without newline is run, not left half-typed
		"a\r":          "a\r\x04",
		"x\n~.\nend\n": "x\n~~.\nend\n\x04",
	} {
		var dst bytes.Buffer
		if err := copyTerminalInput(&dst, strings.NewReader(in)); err != nil {
			t.Errorf("copyTerminalInput(%q) = %v", in, err)
		}
		if dst.String() != want {
			t.Errorf("copyTerminalInput(%q) wrote %q, want %q", in, dst.String(), want)
		}
	}
	// A read error still ends the remote side, and is reported.
	var dst bytes.Buffer
	if err := copyTerminalInput(&dst, &failingReader{data: "ls"}); err == nil || dst.String() != "ls\n\x04" {
		t.Errorf("read error: err=%v wrote %q", err, dst.String())
	}
}

func TestPipeTerminalInputNeverReachesEOF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("terminal mode does not pipe stdin on Windows (the console API reads keystrokes)")
	}
	r, err := pipeTerminalInput(strings.NewReader("ls\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var got []byte
	buf := make([]byte, 64)
	for !bytes.HasSuffix(got, []byte{0x04}) {
		if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, err := r.Read(buf)
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "ls\n\x04" {
		t.Errorf("got %q", got)
	}
	// After the copy finished, the write end must stay open even through
	// garbage collection (os.File closes on finalization), or ShellSession
	// would see EOF after all (#70).
	runtime.GC()
	runtime.GC()
	if err := r.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("after the input ended: read %q, %v; want no data and no EOF (deadline exceeded)", buf[:n], err)
	}
}

// runShellEscape runs the vendored ShellSession on input in a helper
// process and returns what it would send to the instance.
func runShellEscape(t *testing.T, input string, raw bool) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	dir := t.TempDir()
	cmd.Env = append(os.Environ(),
		testHelperEnv+"="+helperShellEscape,
		"NAKAMA_TEST_SHELL_INPUT_HEX="+hex.EncodeToString([]byte(input)),
		// If "~." ever reached ShellSession it would call TerminateSession;
		// make that fail fast and offline.
		"AWS_PROFILE=nakama-test-no-such-profile",
		"AWS_CONFIG_FILE="+filepath.Join(dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE="+filepath.Join(dir, "credentials"),
		"AWS_EC2_METADATA_DISABLED=true",
	)
	if raw {
		cmd.Env = append(cmd.Env, "NAKAMA_TEST_SHELL_RAW=1")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	// "~?" help text, if it ever fires, precedes the hex dump.
	lines := strings.Split(string(out), "\n")
	sent, err := hex.DecodeString(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		t.Fatalf("helper output %q: %v", out, err)
	}
	return string(sent)
}

func TestTerminalInputSurvivesShellSessionEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows ShellSession reads the console, not stdin")
	}
	// Without escaping, ShellSession eats "~?" at a line start (and would
	// terminate the session on "~."), which shows the helper really
	// exercises its escape handling.
	if got := runShellEscape(t, "a\n~?\nb\n", true); got == "a\n~?\nb\n" {
		t.Fatalf("control: ShellSession did not interpret the raw escape (got %q); the test below would prove nothing", got)
	}
	input := "echo start\n~.\n~?\n~-\n~~\n~x\nno~escape\r~.\n~"
	if got := runShellEscape(t, input, false); got != input {
		t.Errorf("ShellSession delivered %q, want the input unchanged %q", got, input)
	}
}

// spawnFakeRunner runs spawnSessionRunnerIO against the test binary in
// fake-session-runner mode.
func spawnFakeRunner(t *testing.T, req sessionRunRequest, status string, exit int) (sessionRunResult, string) {
	t.Helper()
	t.Setenv(testHelperEnv, helperFakeSessionRunner)
	t.Setenv("NAKAMA_TEST_FAKE_STATUS", status)
	t.Setenv("NAKAMA_TEST_FAKE_EXIT", strconv.Itoa(exit))
	var out bytes.Buffer
	res, err := spawnSessionRunnerIO(context.Background(), req, sessionRunnerIO{stdout: &out, stderr: &out})
	if err != nil {
		t.Fatalf("spawnSessionRunnerIO: %v\n%s", err, out.String())
	}
	return res, out.String()
}

func TestSpawnSessionRunnerReportsRemoteStatus(t *testing.T) {
	payload := []byte(`{"sessionId":"s-1","mode":"command"}`)
	res, out := spawnFakeRunner(t, sessionRunRequest{payload: payload, reportStatus: true}, formatSessionStatus(7), 0)
	if !strings.Contains(out, "payload="+string(payload)) {
		t.Errorf("the payload must still arrive on its own handle next to the status pipe:\n%s", out)
	}
	if !strings.Contains(out, "status=open") || strings.Contains(out, "status-env-still-set") {
		t.Errorf("the child must open the status pipe and drop the variable:\n%s", out)
	}
	if res.code != 0 || res.remoteExit == nil || *res.remoteExit != 7 {
		t.Errorf("result = code %d remote %v, want code 0 remote 7", res.code, res.remoteExit)
	}
}

func TestSpawnSessionRunnerWithoutStatus(t *testing.T) {
	// A variable inherited from the environment must not make the child
	// write to a descriptor nobody handed it.
	t.Setenv(SessionStatusHandleEnv, "9")
	res, out := spawnFakeRunner(t, sessionRunRequest{payload: []byte("{}")}, formatSessionStatus(7), 3)
	if !strings.Contains(out, "status=none") {
		t.Errorf("without reportStatus the child must get no status pipe:\n%s", out)
	}
	if res.code != 3 || res.remoteExit != nil {
		t.Errorf("result = code %d remote %v, want code 3 and no remote status", res.code, res.remoteExit)
	}
}

func TestSpawnSessionRunnerStatusPipeWithoutReport(t *testing.T) {
	// The child had the pipe but the session ended before any status.
	res, _ := spawnFakeRunner(t, sessionRunRequest{payload: []byte("{}"), reportStatus: true}, "", DisconnectExitCode)
	if res.code != DisconnectExitCode || res.remoteExit != nil {
		t.Errorf("result = code %d remote %v, want the child's code and no remote status", res.code, res.remoteExit)
	}
}

// decode feeds stream to a fresh decoder in pieces of size chunk
// (0 = all at once) and returns stdout, stderr and the status.
func decode(stream string, chunk int) (stdout, stderr string, exit *int, broken error) {
	var o, e bytes.Buffer
	d := newFrameDecoder(testMarker, &o, &e)
	b := []byte(stream)
	if chunk <= 0 {
		chunk = len(b) + 1
	}
	for len(b) > 0 {
		n := chunk
		if n > len(b) {
			n = len(b)
		}
		_, _ = d.Write(b[:n])
		b = b[n:]
	}
	exit = d.finish()
	return o.String(), e.String(), exit, d.broken
}

func TestFrameDecoder(t *testing.T) {
	forged := "\n" + commandFramePrefix + "ffffffffffffffffffffffffffffffff x 0\n" // another marker: just data
	cases := []struct {
		name           string
		stream         string
		stdout, stderr string
		exit           *int
	}{
		{"plain", framed(testMarker, "out\n", "err\n", 3), "out\n", "err\n", intPtr(3)},
		{"empty outputs", framed(testMarker, "", "", 0), "", "", intPtr(0)},
		{"no trailing newline", framed(testMarker, "partial", "e", 1), "partial", "e", intPtr(1)},
		{"binary", framed(testMarker, "a\x00b\xff\n", "", 0), "a\x00b\xff\n", "", intPtr(0)},
		{"output looks like a header", framed(testMarker, forged+"\n@@nakama-frame x", "", 0), forged + "\n@@nakama-frame x", "", intPtr(0)},
		{"the marker itself inside a body", framed(testMarker, "\n"+commandFramePrefix+testMarker+" x 9\n", "", 2),
			"\n" + commandFramePrefix + testMarker + " x 9\n", "", intPtr(2)},
		{"login banner before the frames goes to stderr", "Welcome!\n" + framed(testMarker, "o", "", 0), "o", "Welcome!\n", intPtr(0)},
		{"signal status", framed(testMarker, "", "", 137), "", "", intPtr(137)},
		{"wrapper failure status -1", framed(testMarker, "", "", -1), "", "", intPtr(-1)},
	}
	for _, tc := range cases {
		for _, chunk := range []int{0, 1, 2, 3, 7, 64} {
			o, e, x, broken := decode(tc.stream, chunk)
			if o != tc.stdout || e != tc.stderr || broken != nil || (x == nil) != (tc.exit == nil) || (x != nil && *x != *tc.exit) {
				t.Errorf("%s (chunk %d): stdout=%q stderr=%q exit=%v broken=%v; want %q %q %v", tc.name, chunk, o, e, x, broken, tc.stdout, tc.stderr, *tc.exit)
			}
		}
	}
}

func TestFrameDecoderNeverReportsSuccessForAnIncompleteStream(t *testing.T) {
	full := framed(testMarker, strings.Repeat("0123456789\n", 1000), "warn\n", 0)
	// Every proper prefix of the stream, i.e. a transfer cut anywhere,
	// must yield no status: a truncated output is never a success.
	for cut := 0; cut < len(full); cut += 97 {
		for _, chunk := range []int{0, 5} {
			if _, _, x, _ := decode(full[:cut], chunk); x != nil {
				t.Fatalf("cut at %d/%d (chunk %d): status %d reported for a truncated stream", cut, len(full), chunk, *x)
			}
		}
	}
	// The whole stream minus its final newline is still complete enough
	// to carry the status line? No: the header needs its newline.
	if _, _, x, _ := decode(strings.TrimSuffix(full, "\n"), 0); x != nil {
		t.Errorf("an unterminated exit header must not count")
	}
}

func TestFrameDecoderRejectsMalformedHeaders(t *testing.T) {
	h := "\n" + commandFramePrefix + testMarker + " "
	for _, stream := range []string{
		h + "o -5\n",                             // negative length
		h + "q 3\nabc" + h + "x 0\n",             // unknown kind
		h + "o abc\n" + h + "x 0\n",              // not a number
		h + "x 99999999999\n",                    // out of range status
		h + strings.Repeat("9", 64),              // header never ends
		h + "o 3\nabc" + h + "x\n" + h + "x 0\n", // header without a number
	} {
		o, e, x, broken := decode(stream, 0)
		if x != nil || broken == nil {
			t.Errorf("stream %q: exit=%v broken=%v; want no status and an error (stdout=%q stderr=%q)", stream, x, broken, o, e)
		}
	}
}

func TestCommandSessionDecodesFramesAndReportsStatus(t *testing.T) {
	status := &bytes.Buffer{}
	cs, stdout, stderr, exits := testCommandSession(status)
	logger := smplog.Logger(false, "nakama-test")
	stream := framed(testMarker, "out-1\nout-2\n", "err-1\n", 3)
	// The agent splits the stream arbitrarily across messages.
	for i := 0; i < len(stream); i += 5 {
		end := i + 5
		if end > len(stream) {
			end = len(stream)
		}
		if ready, err := cs.ProcessStreamMessagePayload(logger, payloadMessage(message.Output, stream[i:end])); !ready || err != nil {
			t.Fatalf("ProcessStreamMessagePayload = %v, %v", ready, err)
		}
	}
	// The agent's own error messages are not command output.
	_, _ = cs.ProcessStreamMessagePayload(logger, payloadMessage(message.StdErr, "agent: something\n"))
	if got := stdout.String(); got != "out-1\nout-2\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := stderr.String(); got != "err-1\nagent: something\n" {
		t.Errorf("stderr = %q", got)
	}
	if status.Len() != 0 {
		t.Errorf("the status must wait for the end of the session, got %q", status.String())
	}
	cs.Stop()
	if got := status.String(); got != "remote-exit 3\n" {
		t.Errorf("status = %q", got)
	}
	if len(*exits) != 1 || (*exits)[0] != 0 {
		t.Errorf("Stop must end the process with 0 (the status travels on the pipe), exits = %v", *exits)
	}
}

func TestCommandSessionStopWithoutStatus(t *testing.T) {
	logger := smplog.Logger(false, "nakama-test")
	// Truncated: the exit frame never arrived.
	status := &bytes.Buffer{}
	cs, stdout, _, exits := testCommandSession(status)
	full := framed(testMarker, "abcdef", "", 0)
	_, _ = cs.ProcessStreamMessagePayload(logger, payloadMessage(message.Output, full[:len(full)-12]))
	cs.Stop()
	if status.Len() != 0 {
		t.Errorf("a truncated stream must report no status: %q", status.String())
	}
	if stdout.String() != "abcdef" || len(*exits) != 1 {
		t.Errorf("stdout=%q exits=%v", stdout.String(), *exits)
	}
	// Malformed: explained on stderr, no status.
	status2 := &bytes.Buffer{}
	cs2, _, stderr2, _ := testCommandSession(status2)
	_, _ = cs2.ProcessStreamMessagePayload(logger, payloadMessage(message.Output, "\n"+commandFramePrefix+testMarker+" o -1\n"))
	cs2.Stop()
	if status2.Len() != 0 || !strings.Contains(stderr2.String(), "nakama: malformed output frame") {
		t.Errorf("status=%q stderr=%q", status2.String(), stderr2.String())
	}
	// Without a status pipe (nil writer) Stop still ends the process.
	cs3, _, _, exits3 := testCommandSession(nil)
	_, _ = cs3.ProcessStreamMessagePayload(logger, payloadMessage(message.Output, framed(testMarker, "", "", 5)))
	cs3.Stop()
	if len(*exits3) != 1 {
		t.Errorf("exits = %v", *exits3)
	}
}

func TestNewCommandMarker(t *testing.T) {
	a, err := newCommandMarker()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newCommandMarker()
	if len(a) != 32 || a == b || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("markers %q, %q: want two different 32-char hex strings", a, b)
	}
}

// runWrapper executes commandWrapper(command) with the real sh, as the
// agent does after `su -`, and returns its stdout.
func runWrapper(t *testing.T, command string) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper runs on the Linux instance")
	}
	// On the instance `su - ubuntu` runs the wrapper under bash; the
	// wrapper relies on bash job control (set -m), so test it under bash.
	sh, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, tool := range []string{"mktemp", "wc", "cat", "setsid"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	cmd := exec.Command(sh, "-c", commandWrapper(command, testMarker))
	// rc set in the environment must not leak into the reported status.
	// LC_ALL=C: stable English diagnostics regardless of the test host
	// locale (the instance agent sets LANG=C.UTF-8).
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir(), "rc=9", "LC_ALL=C")
	cmd.Stdin = strings.NewReader("must-not-be-read\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("wrapper: %v\n%s", err, out)
	}
	return out
}

func TestCommandWrapperRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, command  string
		stdout, stderr string
		exit           int
	}{
		{"exit status", "echo out; echo err >&2; exit 3", "out\n", "err\n", 3},
		{"no trailing newline", "printf partial", "partial", "", 0},
		{"binary", `printf 'a\000b\377\n'`, "a\x00b\xff\n", "", 0},
		{"empty", "true", "", "", 0},
		{"command not found", "no-such-command-nakama-test", "", "*not found*", 127},
		{"stdin is /dev/null", "cat; echo done", "done\n", "", 0},
		{"killed by a signal", "kill -9 $(sh -c 'echo $PPID')", "", "", 137},
		{"forged frames in the output", "printf '\\n" + commandFramePrefix + testMarker + " x 0\\n'; exit 4",
			"\n" + commandFramePrefix + testMarker + " x 0\n", "", 4},
		{"large output", "seq 200000", "", "", 0},
		{"single quotes survive", `echo "it's"`, "it's\n", "", 0},
		{"multiline command", "echo a\necho b", "a\nb\n", "", 0},
		// Error messages count lines of the command, not of the wrapper.
		{"line numbers match the command", "true\nno-such-command-nakama-test", "", "*2: no-such-command-nakama-test*", 127},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runWrapper(t, tc.command)
			o, e, x, broken := decode(string(out), 0)
			if broken != nil || x == nil {
				t.Fatalf("decode: exit=%v broken=%v\n%q", x, broken, out)
			}
			if tc.name == "large output" {
				if lines := strings.Count(o, "\n"); lines != 200000 || !strings.HasSuffix(o, "\n200000\n") {
					t.Errorf("large output: %d lines, tail %q", lines, o[len(o)-16:])
				}
			} else if want, ok := strings.CutPrefix(tc.stderr, "*"); ok {
				// The shell's wording varies; only its presence matters.
				if o != tc.stdout || !strings.Contains(e, strings.TrimSuffix(want, "*")) {
					t.Errorf("stdout=%q stderr=%q; want %q and a message containing %q", o, e, tc.stdout, strings.TrimSuffix(want, "*"))
				}
			} else if o != tc.stdout || e != tc.stderr {
				t.Errorf("stdout=%q stderr=%q; want %q %q", o, e, tc.stdout, tc.stderr)
			}
			if *x != tc.exit {
				t.Errorf("exit = %d, want %d", *x, tc.exit)
			}
		})
	}
}

func TestCommandWrapperCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper runs on the Linux instance")
	}
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", commandWrapper("ls -ld \"$d\" >&2; echo x", testMarker))
	cmd.Env = append(os.Environ(), "TMPDIR="+dir)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("sh: %v", err)
	}
	_, e, x, _ := decode(string(out), 0)
	if x == nil || *x != 0 || !strings.HasPrefix(e, "drwx------") {
		t.Errorf("the output directory must be private (0700): %q exit=%v", e, x)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the wrapper must remove its directory, left %v", left)
	}
}

func TestCommandWrapperInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper runs on the Linux instance")
	}
	for _, tool := range []string{"bash", "mktemp", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	// A command that prints, then sleeps far longer than the test, then
	// would print again. If the interrupt works, the second line never
	// appears, the exit status is non-zero, and the wrapper returns long
	// before the sleep would.
	for _, tc := range []struct {
		name, command string
		wantExit      int // -1 => just assert non-zero
	}{
		// Each case sleeps a distinct number of seconds so that
		// waitForCommandToSleep finds exactly its own sleep.
		{"bare command dies from SIGINT", "echo OUT; sleep 731; echo LATE", 130},
		{"command handles SIGINT", `trap 'echo BYE; exit 42' INT; echo OUT; sleep 732; echo LATE`, 42},
		{"command ignores SIGINT (force-killed)", `trap '' INT; echo OUT; sleep 733; echo LATE`, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			cmd := exec.Command("bash", "-c", commandWrapper(tc.command, testMarker))
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "LC_ALL=C")
			cmd.Stdin = strings.NewReader("")
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Interrupt only once the command has printed OUT and is in its
			// sleep, as when a user presses Ctrl-C on a running command. A
			// fixed delay raced on a loaded machine: the signal could land
			// before the command had installed its own trap or printed.
			waitForCommandToSleep(t, tmp, tc.command)
			start := time.Now()
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("the wrapper did not exit after the interrupt")
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("the wrapper took %v after the interrupt; the command was not cut short", elapsed)
			}
			o, e, x, broken := decode(out.String(), 0)
			if broken != nil || x == nil {
				t.Fatalf("decode: exit=%v broken=%v\n%q", x, broken, out.String())
			}
			if !strings.HasPrefix(o, "OUT\n") || strings.Contains(o, "LATE") {
				t.Errorf("stdout = %q, want the output printed before the interrupt and nothing from after the sleep", o)
			}
			if tc.wantExit >= 0 && *x != tc.wantExit {
				t.Errorf("exit = %d, want %d (stderr %q)", *x, tc.wantExit, e)
			}
			if *x == 0 {
				t.Errorf("an interrupted command must not report success (stderr %q)", e)
			}
		})
	}
}

// TestCommandWrapperInterruptKeepsTheStatusOfAFastExit pins the race CI
// hit (Issue #79): the command handles SIGINT and exits at once, so it
// is gone and reaped by the time the wrapper's trap returns and wait
// comes back early. The wrapper must still report the command's own
// status (42), not the 130 of the interrupted wait. Without a sleep in
// the command the window is wide open on every run, so this fails
// deterministically against the old liveness-only loop.
func TestCommandWrapperInterruptKeepsTheStatusOfAFastExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper runs on the Linux instance")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// The command busy-waits on a marker file instead of sleeping, so the
	// only thing between SIGINT and its exit is its own trap.
	const command = `trap 'exit 42' INT; echo OUT; while :; do :; done`
	for i := 0; i < 5; i++ {
		tmp := t.TempDir()
		cmd := exec.Command("bash", "-c", commandWrapper(command, testMarker))
		cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "LC_ALL=C")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			matches, _ := filepath.Glob(filepath.Join(tmp, "tmp.*", "o"))
			if len(matches) == 1 {
				if b, _ := os.ReadFile(matches[0]); strings.HasPrefix(string(b), "OUT\n") {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("the wrapper did not exit after the interrupt")
		}
		_, _, x, broken := decode(out.String(), 0)
		if broken != nil || x == nil {
			t.Fatalf("run %d: decode: exit=%v broken=%v\n%q", i, x, broken, out.String())
		}
		if *x != 42 {
			t.Fatalf("run %d: exit = %d, want 42: the command's own status after it handled SIGINT", i, *x)
		}
	}
}

// waitForCommandToSleep blocks until the wrapper started under tmp has
// captured the command's "OUT" line and the command's own `sleep <n>`
// (taken from command) is running, i.e. the command has passed its
// trap and print. Without pgrep it settles for the OUT line plus a
// short grace period.
func waitForCommandToSleep(t *testing.T, tmp, command string) {
	t.Helper()
	sleepCmd := ""
	if i := strings.Index(command, "sleep "); i >= 0 {
		sleepCmd = strings.Fields(command[i:])[0] + " " + strings.TrimSuffix(strings.Fields(command[i:])[1], ";")
	}
	_, pgrepErr := exec.LookPath("pgrep")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if matches, _ := filepath.Glob(filepath.Join(tmp, "tmp.*", "o")); len(matches) == 1 {
			if b, err := os.ReadFile(matches[0]); err == nil && strings.HasPrefix(string(b), "OUT\n") {
				if pgrepErr != nil || sleepCmd == "" {
					time.Sleep(300 * time.Millisecond)
					return
				}
				if exec.Command("pgrep", "-fx", sleepCmd).Run() == nil {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the command did not reach %q in time", sleepCmd)
}

// syncBuffer is a bytes.Buffer safe for the hint goroutine to write
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// hintSession builds a commandSession with the hint armed to hintDelay,
// writing to a synchronized buffer; exit is recorded.
func hintSession(hintDelay time.Duration) (cs *commandSession, hint *syncBuffer) {
	hint = &syncBuffer{}
	cs = newCommandSession(testMarker, &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}, hint, hintDelay)
	cs.exit = func(int) {}
	cs.notify = func(chan<- os.Signal, ...os.Signal) {} // no real signals in tests
	return cs, hint
}

func TestCommandSessionHint(t *testing.T) {
	logger := smplog.Logger(false, "nakama-test")

	t.Run("fires when nothing arrives in time", func(t *testing.T) {
		cs, hint := hintSession(30 * time.Millisecond)
		go func() { _ = cs.SetSessionHandlers(logger) }() // arms the timer
		deadline := time.Now().Add(2 * time.Second)
		for hint.Len() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !strings.Contains(hint.String(), "--tty") || !strings.Contains(hint.String(), "still running") {
			t.Errorf("hint = %q, want advice to use --tty", hint.String())
		}
	})

	t.Run("output cancels the hint", func(t *testing.T) {
		cs, hint := hintSession(50 * time.Millisecond)
		cs.armHint()
		_, _ = cs.ProcessStreamMessagePayload(logger, payloadMessage(message.Output, "data"))
		time.Sleep(150 * time.Millisecond)
		if hint.Len() != 0 {
			t.Errorf("output arrived, so no hint should print; got %q", hint.String())
		}
	})

	t.Run("a quick Stop cancels the hint", func(t *testing.T) {
		cs, hint := hintSession(50 * time.Millisecond)
		cs.armHint()
		cs.Stop()
		time.Sleep(150 * time.Millisecond)
		if hint.Len() != 0 {
			t.Errorf("the session ended before the delay; got %q", hint.String())
		}
	})

	t.Run("off when not armed", func(t *testing.T) {
		cs, _, _, _ := testCommandSession(&bytes.Buffer{}) // hint nil
		cs.armHint()
		if cs.hintTimer != nil {
			t.Error("the hint must not be armed without a hint writer")
		}
	})
}
