// Package service hosts the use-case layer of nakama.
//
// The CLI layer (internal/cli) only parses flags and constructs
// inputs; all real work happens here. The package depends on the
// AWS SDK wrappers in internal/awsx, which are mockable interfaces.
//
// Sub-features (init, acl, instance lifecycle) live in dedicated
// files within this package so that the public API surface stays
// small and discoverable.
package service

import (
	"github.com/ukaji3/nakama/internal/awsx"
)

// Service holds the AWS clients and shared utilities used by every
// use-case method. Construct via NewService.
type Service struct {
	clients *awsx.Clients

	// connect holds the process-supervision hooks used by Connect
	// (`nakama aws connect` auto-reconnect, ADR 0014). nil selects the
	// production defaults; tests inject fakes. See connect_session.go.
	connect *connectHooks

	// tunnel holds the process/network hooks used by the `nakama aws
	// tunnel` orchestration (ADR 0017). nil selects the production
	// defaults; tests inject fakes. See tunnel_start.go.
	tunnel *tunnelHooks

	// ssh holds the session hook used by SSHProxy (ADR 0019). nil runs
	// the relay session in this process; tests inject fakes.
	ssh *sshHooks
}

// NewService returns a Service backed by the given AWS clients.
func NewService(c *awsx.Clients) *Service {
	return &Service{clients: c}
}
