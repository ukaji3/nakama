//go:build !windows

package cli

import (
	"fmt"
	"os"
)

// sessionPayloadFD is the file descriptor on which the Connect
// supervisor (internal/service, spawnSessionRunner) hands over the
// session payload: ExtraFiles[0] becomes fd 3 in the child. The payload
// contains the secret session TokenValue and therefore must not travel
// via argv or the environment; fd 3 keeps stdin free for the
// interactive session itself.
const sessionPayloadFD = 3

// openSessionPayload opens the payload pipe inherited from the Connect
// supervisor on fd 3.
func openSessionPayload() (*os.File, error) {
	f := os.NewFile(sessionPayloadFD, "nakama-session-payload")
	if f == nil {
		return nil, fmt.Errorf("session payload fd %d is not open; "+
			"`nakama aws session-runner` is an internal command spawned by `nakama aws connect`",
			sessionPayloadFD)
	}
	return f, nil
}
