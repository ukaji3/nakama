# Vendored session-manager-plugin

- Upstream: https://github.com/aws/session-manager-plugin.git
- Tag: `1.2.835.0`
- Commit: `930a08e65d3a378eeeebb7f1bcf67eae7d860ae0`
- Imported by: `scripts/vendor-ssm-plugin.sh`
- License: Apache-2.0 (see LICENSE / NOTICE)

Driven as a library by `internal/service/connect.go`. Resolved as a Go
module via a hand-maintained `go.mod` + `replace` directives in the repo
root `go.mod`. `twinj/uuid` is vendored under `third_party/twinj-uuid`
because its published versions are not API-compatible with this snapshot.

To update: run `scripts/vendor-ssm-plugin.sh [tag]`, then
`go mod tidy && go build ./...`, and adjust go.mod requires if deps changed.
**Re-apply the local patches below after re-vendoring** (the script
overwrites this file too; restore this section).

## Upstream environment gates nakama relies on

- `SSM_PLUGIN_SKIP_CLIENT_CONFIGURE` — an **upstream** feature (present
  since at least 1.2.814.0) in `OpenDataChannel`
  (`src/sessionmanagerplugin/session/sessionhandler.go`): when set to
  `true`, skips the plugin's own credential-chain lookup so the
  `Signer`/`Credentials` injected by nakama
  (`internal/service/connect_session.go`) are used as-is. Not a local
  patch (a pre-1.2.835.0 revision of this file misattributed it); verify
  it still exists after re-vendoring.

## Local patches

Minimal, environment-gated deviations from the upstream snapshot,
all marked with `// NAKAMA PATCH:` comments. The variable leaves the
upstream behavior completely unchanged when unset.

- `SSM_PLUGIN_DISCONNECT_EXIT_CODE` (ADR 0014) — when set to an integer,
  permanent disconnects terminate the process with that exit code via
  the `nakamaExitOnPermanentDisconnect` helper
  (`src/sessionmanagerplugin/session/sessionhandler.go`, defined above
  `Stop`) plus a `strconv` import. Patched sites:
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

- `NakamaAWSConfigProvider` (Issue #49) — an exported hook in
  `src/sessionmanagerplugin/session/sessionhandler.go` (+ an `aws`
  import). When non-nil, `GetResumeSessionParams` resolves its
  `aws.Config` through it instead of the default AWS credential chain
  (`sdkutil.GetConfigWithQuickCheck`), so both the ResumeSession
  control-plane call and the data-channel re-signing use nakama's
  credential resolution (profile registry / native login token cache,
  proxy-aware HTTP client). The session-runner child installs it in
  `internal/service/connect_session.go` (`installPluginResumeConfig`).
  Nil leaves the upstream behavior completely unchanged.

## Known integration caveats

- The in-place resume control plane (`GetResumeSessionParams`) uses
  the **default AWS credential chain** upstream; nakama environments
  rely on the `NakamaAWSConfigProvider` patch above to make in-place
  resume work at all. If a future re-vendor drops that patch,
  in-place resume silently degrades to full session replacement via
  the disconnect exit code + a fresh `StartSession` (functional but
  slower, and it burns supervisor reconnect budget).
