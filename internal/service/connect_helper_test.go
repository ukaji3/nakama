package service

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/aws/session-manager-plugin/src/communicator"
	"github.com/aws/session-manager-plugin/src/datachannel"
	smplog "github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/session-manager-plugin/src/message"
	smpsession "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"
	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/shellsession"
)

// testHelperEnv turns the test binary into a helper process. Tests that
// need a real child (the session-runner handle protocol, the vendored
// ShellSession, which ends its process with os.Exit) re-execute the
// test binary with it set.
const testHelperEnv = "NAKAMA_TEST_HELPER"

const (
	helperFakeSessionRunner = "fake-session-runner"
	helperShellEscape       = "shell-escape"
)

func TestMain(m *testing.M) {
	switch os.Getenv(testHelperEnv) {
	case helperFakeSessionRunner:
		os.Exit(runFakeSessionRunner())
	case helperShellEscape:
		os.Exit(runShellEscapeHelper())
	}
	os.Exit(m.Run())
}

// runFakeSessionRunner stands in for `nakama aws session-runner` when
// spawnSessionRunnerIO re-executes the test binary: it opens the
// inherited payload and status handles exactly as the real child does,
// echoes what it received, writes NAKAMA_TEST_FAKE_STATUS to the status
// pipe and exits with NAKAMA_TEST_FAKE_EXIT.
func runFakeSessionRunner() int {
	f, err := OpenSessionPayload()
	if err != nil {
		fmt.Fprintln(os.Stderr, "payload:", err)
		return 90
	}
	payload, err := io.ReadAll(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read payload:", err)
		return 90
	}
	fmt.Printf("payload=%s\n", payload)
	st, err := OpenSessionStatus()
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		return 91
	}
	if _, leaked := os.LookupEnv(SessionStatusHandleEnv); leaked {
		fmt.Println("status-env-still-set")
	}
	if st == nil {
		fmt.Println("status=none")
	} else {
		fmt.Println("status=open")
		if _, err := io.WriteString(st, os.Getenv("NAKAMA_TEST_FAKE_STATUS")); err != nil {
			fmt.Fprintln(os.Stderr, "write status:", err)
			return 92
		}
	}
	code, _ := strconv.Atoi(os.Getenv("NAKAMA_TEST_FAKE_EXIT"))
	return code
}

// runShellEscapeHelper feeds NAKAMA_TEST_SHELL_INPUT_HEX to the vendored
// ShellSession through os.Stdin, as terminal mode does, and prints (hex)
// what ShellSession would send to the instance. Unless
// NAKAMA_TEST_SHELL_RAW is set the input first goes through
// escapeTerminalInput. Closing the pipe ends ShellSession's input loop.
func runShellEscapeHelper() int {
	input, err := hex.DecodeString(os.Getenv("NAKAMA_TEST_SHELL_INPUT_HEX"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode input:", err)
		return 90
	}
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipe:", err)
		return 90
	}
	go func() {
		data := input
		if os.Getenv("NAKAMA_TEST_SHELL_RAW") == "" {
			atLineStart := false
			data = escapeTerminalInput(input, &atLineStart)
		}
		_, _ = w.Write(data)
		_ = w.Close()
	}()
	os.Stdin = r
	shellsession.GetTerminalSizeCall = func(int) (int, int, error) { return 80, 24, nil }

	logger := smplog.Logger(false, "nakama-test")
	dc := newRecordingDataChannel()
	sess := &shellsession.ShellSession{}
	sess.Initialize(logger, &smpsession.Session{DataChannel: dc, SessionId: "helper"})
	_ = sess.SetSessionHandlers(logger) // returns once its stdin reaches EOF
	fmt.Print(hex.EncodeToString(dc.sent(message.Output)))
	return 0
}

// recordingDataChannel is the part of the plugin's data channel that
// the session handlers use, recording what they send.
type recordingDataChannel struct {
	datachannel.IDataChannel

	mu       sync.Mutex
	inputs   []recordedInput
	handlers []datachannel.OutputStreamDataMessageHandler
	stops    []datachannel.Stop
	ws       *recordingWsChannel
}

type recordedInput struct {
	payloadType message.PayloadType
	data        []byte
}

func newRecordingDataChannel() *recordingDataChannel {
	return &recordingDataChannel{ws: &recordingWsChannel{}}
}

func (d *recordingDataChannel) SendInputDataMessage(_ smplog.T, payloadType message.PayloadType, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inputs = append(d.inputs, recordedInput{payloadType: payloadType, data: append([]byte(nil), data...)})
	return nil
}

func (d *recordingDataChannel) RegisterOutputStreamHandler(h datachannel.OutputStreamDataMessageHandler, _ bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers = append(d.handlers, h)
}

func (d *recordingDataChannel) OutputMessageHandler(_ smplog.T, stop datachannel.Stop, _ string, _ []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops = append(d.stops, stop)
	return nil
}

func (d *recordingDataChannel) GetWsChannel() communicator.IWebSocketChannel { return d.ws }

// sent concatenates the payloads of one type, in order.
func (d *recordingDataChannel) sent(payloadType message.PayloadType) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []byte
	for _, in := range d.inputs {
		if in.payloadType == payloadType {
			out = append(out, in.data...)
		}
	}
	return out
}

type recordingWsChannel struct {
	communicator.IWebSocketChannel

	mu        sync.Mutex
	onMessage func([]byte)
}

func (w *recordingWsChannel) SetOnMessage(f func([]byte)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onMessage = f
}
