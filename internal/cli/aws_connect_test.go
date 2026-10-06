package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/service"
)

func TestResolveConnectRetries(t *testing.T) {
	tests := []struct {
		name        string
		explicit    bool
		value       int
		commandMode bool
		want        int
	}{
		{name: "shell default", explicit: false, value: defaultConnectRetries, commandMode: false, want: 5},
		{name: "command default disables retries", explicit: false, value: defaultConnectRetries, commandMode: true, want: 0},
		{name: "explicit retry wins in command mode", explicit: true, value: 3, commandMode: true, want: 3},
		{name: "explicit retry wins in shell mode", explicit: true, value: 7, commandMode: false, want: 7},
		{name: "explicit zero disables in shell mode", explicit: true, value: 0, commandMode: false, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveConnectRetries(tt.explicit, tt.value, tt.commandMode); got != tt.want {
				t.Errorf("resolveConnectRetries(%v, %d, %v) = %d, want %d",
					tt.explicit, tt.value, tt.commandMode, got, tt.want)
			}
		})
	}
}

func TestSilenceRemoteExit(t *testing.T) {
	for _, tc := range []struct {
		err    error
		silent bool
	}{
		{nil, false},
		{errors.New("StartSession: denied"), false},
		{&service.ExitCodeError{Code: 255, Message: "no status"}, false},
		{&service.ExitCodeError{Code: 3, Remote: true}, true},
		{fmt.Errorf("wrapped: %w", &service.ExitCodeError{Code: 1, Remote: true}), true},
	} {
		cmd := &cobra.Command{}
		if got := silenceRemoteExit(cmd, tc.err); !errors.Is(got, tc.err) && got != tc.err {
			t.Errorf("the error must be returned unchanged: %v", got)
		}
		if cmd.SilenceErrors != tc.silent {
			t.Errorf("silenceRemoteExit(%v): SilenceErrors = %v, want %v", tc.err, cmd.SilenceErrors, tc.silent)
		}
	}
}
