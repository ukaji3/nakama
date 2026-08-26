# ADR 0015: Windows クライアント対応と OS プロキシ自動検出

- Status: Accepted
- Date: 2026-08-26
- Context: issue #23 (Windows 対応・企業プロキシ環境対応)

## Context

nakama v1 のクライアント対応 OS は Linux / macOS のみで
(requirements.md §7 に「Windows クライアントでの `connect` 動作保証」を
明示的に除外)、`GOOS=windows` ではビルド自体が通らなかった。また企業
ネットワークでは HTTP(S) プロキシが OS 設定 (Windows の
「インターネット オプション」/ WinHTTP、PAC スクリプト) で配布され、
環境変数 `HTTP(S)_PROXY` が設定されていないことが多い。nakama の全
outbound 経路 (AWS SDK 既定クライアント / Session Manager data-channel
の WebSocket / checkip) は `http.ProxyFromEnvironment` に依存しており、
この環境では素通しでの直接接続を試みて失敗する。

ソース調査で確認した事実:

- `GOOS=windows` のビルド障害は
  `internal/service/connect_session.go` の `syscall.SIGTSTP` /
  `syscall.SIGQUIT` / `SIGTERM` 参照のみ。
- 同ファイルの `cmd.ExtraFiles` は Windows 非対応 (os/exec 公式 doc:
  "ExtraFiles is not supported on Windows")。`connect` の親プロセス
  監督方式 (ADR 0014) は秘匿の TokenValue を含む payload JSON を
  fd 3 (ExtraFiles) で子へ受け渡すため、Windows では代替経路が必要。
- vendored plugin の WebSocket 接続
  (`third_party/session-manager-plugin/.../communicator/websocketutil.go`)
  は gorilla/websocket の **package 変数 `websocket.DefaultDialer`** を
  参照する。呼び出し側 (nakama) が `DefaultDialer.Proxy` を差し替えれば
  vendored コードへのパッチは不要。
- `github.com/mattn/go-ieproxy` v0.0.12 は env 優先 → OS 設定
  (PAC → 静的) → env フォールバックの解決を提供する。Windows 実装は
  cgo 不要。macOS の OS 設定検出は cgo 必須で、`CGO_ENABLED=0` ビルド
  (nakama のリリースバイナリ) ではビルドタグ
  `!windows && (!darwin || !cgo)` により env フォールバックになる。
  Linux は `http.ProxyFromEnvironment` 相当。

## Decision

### 1. Windows 対応: OS 依存部分のビルドタグ分割

`internal/service/connect_session.go` の共通ロジックは維持し、OS 固有の
関数群を `connect_session_unix.go` (`//go:build !windows`) /
`connect_session_windows.go` に分割する。

- **シグナル処理** `ignoreTerminalSignals() (restore func())`:
  unix = `signal.Ignore(os.Interrupt, SIGTSTP, SIGQUIT)` (従来と同一)。
  windows = `signal.Ignore(os.Interrupt)` のみ (SIGTSTP / SIGQUIT は
  Windows に存在しない)。
- **子プロセス停止** `terminateSessionRunner(p *os.Process) error`:
  unix = `p.Signal(SIGTERM)` (従来と同一)。windows = `p.Kill()`
  (Windows に SIGTERM は無く、TerminateProcess が唯一の手段)。
- **payload 受け渡し** `attachSessionPayload(cmd *exec.Cmd, r *os.File) error`:
  - unix = 現行どおり `cmd.ExtraFiles = []*os.File{r}` (fd 3)。
  - windows = pipe の読み口ハンドルに `SetHandleInformation`
    (golang.org/x/sys/windows) で `HANDLE_FLAG_INHERIT` を付与し、
    `syscall.SysProcAttr.AdditionalInheritedHandles` で子へ継承する。
    継承されたハンドルは子プロセスでも**同じ数値**を持つため、その
    **ハンドル番号 (非秘匿の整数) のみ**を専用環境変数
    `NAKAMA_SESSION_PAYLOAD_HANDLE` で子に伝える。
  - 子側 (`internal/cli`) も `openSessionPayload()` を
    `aws_session_runner_unix.go` (fd 3 を `os.NewFile`) /
    `aws_session_runner_windows.go` (env のハンドル番号を parse して
    `os.NewFile`) に分割する。
  - **TokenValue を argv・環境変数・ディスク・ログに置かない不変条件
    (ADR 0014) は維持される**: 環境変数を通るのはハンドル番号という
    参照情報だけで、payload 本体は従来どおり pipe の中にのみ存在する。

unix での既存挙動 (シグナル・fd 3・exit code 分類・リトライポリシー) は
不変。CI には windows-latest のテストジョブを追加する (`-race` は
Windows で cgo / gcc に依存するため付けない)。リリース
(.goreleaser.yaml) は goos に windows (amd64 / arm64) を追加し、
アーカイブは zip とする。

### 2. OS プロキシ自動検出: go-ieproxy による二段構え

新規パッケージ `internal/proxy` を設け、全 outbound 経路のプロキシ解決を
一元化する。

- `proxy.Func()` は二段構えで解決する:
  1. **環境変数優先**: `golang.org/x/net/http/httpproxy` の
     `FromEnvironment()` を**呼び出しごとに**評価し、`HTTP_PROXY` /
     `HTTPS_PROXY` のいずれかが設定済みならその `ProxyFunc` を使う
     (`NO_PROXY` も尊重)。`http.ProxyFromEnvironment` は `sync.Once` で
     環境変数をキャッシュするため決定的なテスト (`t.Setenv`) ができず、
     採用しない。
  2. **OS 設定へ委譲**: env 未設定時は `ieproxy.GetProxyFunc()` に委譲
     する。Windows では WinHTTP / IE の静的設定と PAC スクリプトを
     読む (cgo 不要)。
- 注入箇所は 3 つ (third_party 非改変):
  1. `internal/awsx` の `config.LoadDefaultConfig` に
     `WithHTTPClient(BuildableClient.WithTransportOptions(...))` で
     `Transport.Proxy = proxy.Func()` を設定 (active profile 経由 /
     既定チェーンの両パスが同一関数を通る)。
  2. `internal/service.RunSessionRunner` (session-runner 子プロセス) で
     `websocket.DefaultDialer.Proxy = proxy.Func()`。package 変数の
     書き換えだが、副作用は WebSocket を dial する唯一のプロセスである
     子プロセス内に閉じる。
  3. `internal/awsx/ip.go` の checkip クライアントに
     `proxy.NewTransport()` (http.DefaultTransport の Clone に Proxy を
     差し替えたもの)。

### 3. CA 証明書はコード変更不要

TLS 検証に Go 標準の既定 (crypto/x509 の platform verifier) を使う。
Windows / macOS では OS の証明書ストアで検証されるため、AD 等で配布
された企業 CA (TLS 傍受プロキシの CA を含む) は自動的に信頼される。
nakama は TLS 設定をどこでも上書きしていないことを確認済みで、
コード変更は行わない。

## Consequences

- Windows クライアントで `nakama` の全サブコマンド (connect 含む) が
  ビルド・動作可能になる。requirements.md §7 の除外項目を削除。
- 企業プロキシ環境 (特に Windows + PAC 配布) で環境変数の手動設定
  なしに動作する。macOS では OS 設定の自動検出は効かず
  (CGO_ENABLED=0 のため)、従来どおり環境変数での指定が必要。Linux は
  従来から環境変数のみで、挙動は実質不変。
- プロキシ解決の一段目が `http.ProxyFromEnvironment` から
  `httpproxy.FromEnvironment()` の毎回評価に変わる。プロセス実行中に
  環境変数を書き換えない通常利用では観測可能な差は無い。
- 依存が増える: `github.com/mattn/go-ieproxy` v0.0.12 (直接)、
  `golang.org/x/net` / `golang.org/x/sys` (直接昇格)、
  `github.com/gorilla/websocket` (直接昇格)、`golang.org/x/text` (間接)。
- 監査型プロキシが WebSocket (CONNECT のアップグレード) を遮断する
  環境では `connect` (Session Manager data-channel) は動作しない。
  これはプロキシ検出では解決できない運用上の制約 (README に明記)。
- Windows 上の子プロセスの停止は `Kill()` (即時終了) になり、unix の
  SIGTERM のような猶予は無い。session-runner はもともと親の監督下で
  使い捨てにされる設計 (ADR 0014) のため許容する。

## Alternatives considered

- **vendored websocketutil へのパッチ (Dialer 差し替え)**:
  websocketutil は package 変数 `websocket.DefaultDialer` を参照して
  おり、nakama 側からの差し替えで足りる。vendored パッチは再 vendoring
  コストを増やすだけで不要。却下。
- **Windows レジストリ直読み (自前実装)**: 静的設定は読めても
  PAC (WPAD / JavaScript 評価) の再実装が非現実的。go-ieproxy は
  WinHTTP API 経由で PAC を含めて解決する。却下。
- **Windows でも環境変数必須とする (プロキシ検出を実装しない)**:
  企業配布の Windows 端末では OS 設定 (PAC) のみ配布され、ユーザーに
  環境変数の恒久設定を求めるのは issue #23 の要求を満たさない。却下。
- **payload の受け渡しを Windows では一時ファイルや環境変数にする**:
  TokenValue が argv/env/ディスクに乗り、ADR 0014 の不変条件に反する。
  ハンドル継承 + ハンドル番号の env 通知なら秘匿値は pipe から出ない。
  却下。
- **カスタム CA バンドルのオプション追加**: Go の platform verifier が
  OS ストアを使うため不要。必要になった時点で検討する。却下。

## References

- issue #23 (Windows 対応・OS プロキシ自動検出)
- ADR 0014 (`connect` の親プロセス監督方式と TokenValue の受け渡し)
- os/exec doc: "ExtraFiles is not supported on Windows"
  <https://pkg.go.dev/os/exec#Cmd>
- `github.com/mattn/go-ieproxy` v0.0.12
  <https://github.com/mattn/go-ieproxy>
- Go platform verifier (crypto/x509, Go 1.18+ の Windows / macOS
  システム証明書ストア利用)
