//go:build windows

package cli

import (
	"fmt"
	"os"
	"strconv"

	"github.com/ukaji3/nakama/internal/service"
)

// openSessionPayload opens the payload pipe inherited from the Connect
// supervisor. On Windows ExtraFiles is not supported, so the
// supervisor marks the pipe's read handle inheritable and announces
// its numeric value (a non-secret integer) through
// service.SessionPayloadHandleEnv; the payload itself — which contains
// the secret session TokenValue — stays inside the pipe and never
// travels via argv or the environment.
func openSessionPayload() (*os.File, error) {
	v := os.Getenv(service.SessionPayloadHandleEnv)
	if v == "" {
		return nil, fmt.Errorf("%s is not set; "+
			"`nakama aws session-runner` is an internal command spawned by `nakama aws connect`",
			service.SessionPayloadHandleEnv)
	}
	h, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", service.SessionPayloadHandleEnv, err)
	}
	f := os.NewFile(uintptr(h), "nakama-session-payload")
	if f == nil {
		return nil, fmt.Errorf("session payload handle %s is not a valid handle", v)
	}
	return f, nil
}
