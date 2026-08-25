# Vendored session-manager-plugin

- Upstream: https://github.com/aws/session-manager-plugin.git
- Tag: `1.2.814.0`
- Commit: `dcff8da8cdecd53789bd06a193bd2032665dccd8`
- Imported by: `scripts/vendor-ssm-plugin.sh`
- License: Apache-2.0 (see LICENSE / NOTICE)

Driven as a library by `internal/service/connect.go`. Resolved as a Go
module via a hand-maintained `go.mod` + `replace` directives in the repo
root `go.mod`. `twinj/uuid` is vendored under `third_party/twinj-uuid`
because its published versions are not API-compatible with this snapshot.

To update: run `scripts/vendor-ssm-plugin.sh [tag]`, then
`go mod tidy && go build ./...`, and adjust go.mod requires if deps changed.
**Re-apply the local patches below after re-vendoring.**

## Local patches

Minimal, environment-gated deviations from the upstream snapshot. Both
variables leave the upstream behavior completely unchanged when unset.

- `SSM_PLUGIN_SKIP_CLIENT_CONFIGURE` —
  `src/sessionmanagerplugin/session/sessionhandler.go`
  (`OpenDataChannel`): when set to `true`, skips the plugin's own
  credential-chain lookup so the `Signer`/`Credentials` injected by
  nakama (`internal/service/connect_session.go`) are used as-is.
- `SSM_PLUGIN_DISCONNECT_EXIT_CODE` (ADR 0014) — when set to an integer,
  permanent disconnects terminate the process with that exit code via
  the `nakamaExitOnPermanentDisconnect` helper
  (`src/sessionmanagerplugin/session/sessionhandler.go`) instead of
  `os.Exit(0)` / hanging. Patched sites (all marked with
  `// NAKAMA PATCH:` comments):
  1. `ResumeSessionHandler` (`sessionhandler.go`): server-side session
     timed out (`ResumeSession` returned an empty `TokenValue`; typical
     after EC2 hibernate/stop) — upstream exits 0.
  2. `OpenDataChannel` `SetOnError` callback (`sessionhandler.go`):
     reconnect retries (`RepeatableExponentialRetryer`) exhausted —
     upstream only logs and the session hangs.
  3. `handleStreamMessageResendTimeout` (`session.go`): stream resend
     timed out and the session was terminated — upstream returns from
     the goroutine and the process hangs.

  `nakama aws connect` sets this variable on its session-runner child
  process so the supervisor can distinguish a permanent disconnect
  (reconnect with a fresh `StartSession`) from a normal session end.
