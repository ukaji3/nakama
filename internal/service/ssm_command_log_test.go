package service

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakeRunCommandSSM answers SendCommand / GetCommandInvocation with a
// scripted terminal result, recording what was sent. reply, when set,
// computes the result from the script (used by tests that need the
// remote side to react to the input, e.g. the encrypted token exchange).
type fakeRunCommandSSM struct {
	awsx.SSMAPI

	mu       sync.Mutex
	scripts  []string
	comments []string

	status ssmtypes.CommandInvocationStatus
	stdout string
	stderr string
	reply  func(script string) (ssmtypes.CommandInvocationStatus, string, string)
}

func (f *fakeRunCommandSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, strings.Join(in.Parameters["commands"], "\n"))
	f.comments = append(f.comments, aws.ToString(in.Comment))
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd-1")}}, nil
}

func (f *fakeRunCommandSSM) GetCommandInvocation(_ context.Context, _ *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, stdout, stderr := f.status, f.stdout, f.stderr
	if f.reply != nil && len(f.scripts) > 0 {
		status, stdout, stderr = f.reply(f.scripts[len(f.scripts)-1])
	}
	if status == "" {
		status = ssmtypes.CommandInvocationStatusSuccess
	}
	return &ssm.GetCommandInvocationOutput{
		Status:                status,
		StandardOutputContent: aws.String(stdout),
		StandardErrorContent:  aws.String(stderr),
	}, nil
}

func (f *fakeRunCommandSSM) lastScript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scripts) == 0 {
		return ""
	}
	return f.scripts[len(f.scripts)-1]
}

// captureLogs swaps the default slog logger for one writing every level
// to a buffer. Not safe with t.Parallel: the logger is process-global.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestRemoteScriptOutputIsNeverLogged pins the fix for remote output
// being printed at the default log level: stdout of a Run Command can
// carry secrets, so neither INFO nor DEBUG may contain it (Issue #71).
func TestRemoteScriptOutputIsNeverLogged(t *testing.T) {
	const secret = "NAKAMA_KIROCREW_TOKEN_RSA c2VjcmV0LXNob3VsZC1ub3QtbGVhaw=="
	logs := captureLogs(t)
	fake := &fakeRunCommandSSM{stdout: secret + "\n"}
	svc := NewService(&awsx.Clients{SSM: fake, Region: "ap-northeast-1"})

	got, err := svc.runRemoteScript(context.Background(), "i-0123456789abcdef0", "echo hi", "test", time.Minute)
	if err != nil {
		t.Fatalf("runRemoteScript: %v", err)
	}
	if !strings.Contains(got, secret) {
		t.Fatalf("the caller must still receive the output, got %q", got)
	}
	if strings.Contains(logs.String(), "c2VjcmV0") {
		t.Errorf("remote stdout leaked into the logs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "stdout_bytes=") {
		t.Errorf("the success log should record the output size instead:\n%s", logs.String())
	}
}

// TestRemoteScriptSuccessIsNotInfo pins that the success line moved to
// DEBUG: it carried the output before, so at INFO it reached every
// terminal running `tunnel suggest` or `update`.
func TestRemoteScriptSuccessIsNotInfo(t *testing.T) {
	logs := captureLogs(t)
	fake := &fakeRunCommandSSM{stdout: "ok\n"}
	svc := NewService(&awsx.Clients{SSM: fake, Region: "ap-northeast-1"})
	if _, err := svc.runRemoteScript(context.Background(), "i-0123456789abcdef0", "true", "test", time.Minute); err != nil {
		t.Fatalf("runRemoteScript: %v", err)
	}
	found := false
	for _, line := range strings.Split(logs.String(), "\n") {
		// Match any spelling of the success line ("update: command
		// success" before the fix) so the assertion cannot pass vacuously.
		if !strings.Contains(line, "command success") {
			continue
		}
		found = true
		if !strings.Contains(line, "level=DEBUG") {
			t.Errorf("the success line must be DEBUG, got: %s", line)
		}
	}
	if !found {
		t.Errorf("no success line was logged at all:\n%s", logs.String())
	}
}

// TestRemoteScriptFailureKeepsStdoutForCallers pins that a failing
// script still hands its stdout to the caller, which parses result
// markers (NAKAMA_KIROCREW_ERROR) emitted before a non-zero exit.
func TestRemoteScriptFailureKeepsStdoutForCallers(t *testing.T) {
	fake := &fakeRunCommandSSM{
		status: ssmtypes.CommandInvocationStatusFailed,
		stdout: "NAKAMA_KIROCREW_ERROR kirocrew is not installed\n",
		stderr: "exit 1",
	}
	svc := NewService(&awsx.Clients{SSM: fake, Region: "ap-northeast-1"})
	got, err := svc.runRemoteScript(context.Background(), "i-0123456789abcdef0", "false", "test", time.Minute)
	if err == nil {
		t.Fatal("a failed command must return an error")
	}
	if !strings.Contains(got, "NAKAMA_KIROCREW_ERROR") {
		t.Errorf("stdout must be returned alongside the error, got %q", got)
	}
}
