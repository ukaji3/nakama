# ADR 0014: `nakama aws connect` の切断検知・自動再接続は親プロセス監督方式 (child re-exec) で実現する

- Status: Accepted
- Date: 2026-08-26
- Context: nakama v1.x `connect` の耐障害性改善 (issue #21、ADR 0005 / 0009 の精緻化)

## Context

`nakama aws connect` (ADR 0005 / 0009) は vendored の
`session-manager-plugin` (third_party/session-manager-plugin、PINNED.md
参照) をライブラリとして駆動する。接続先インスタンスのハイバネート・
長時間のネットワーク断などでセッションが**恒久的に**失われた場合、
利用者は手動で `connect` をやり直す必要があった。特にポートフォワードを
長時間張るユースケースで摩擦が大きい。

plugin のソース調査で確認した事実 (取り込み版 tag `1.2.814.0`):

- **plugin は正常終了も異常終了も `os.Exit(0)` で終える設計**であり、
  `Session.Execute()` は呼び出し元へ戻らない。
  - 正常終了: ChannelClosed 受信 → `Session.Stop()`
    (`sessionmanagerplugin/session/sessionhandler.go`) が `os.Exit(0)`。
    shellsession / portsession の各 `Stop()` も同様。
  - 恒久切断 A: `ResumeSessionHandler` (sessionhandler.go) で
    `ResumeSession` の TokenValue が空 → `Session: %s timed out.` を
    出力して `os.Exit(0)`。ハイバネート復帰後の典型経路で、**正常終了と
    区別できない**。
  - 恒久切断 B: websocket の `SetOnError` → `retryParams.Call()`
    (`RepeatableExponentialRetryer`) が枯渇 → **ログ出力のみで
    セッションは宙吊り** (プロセスは生きたまま何もしない)。
  - 恒久切断 C: 再送 timeout → `handleStreamMessageResendTimeout`
    (session/session.go) が `TerminateSession` を呼んで goroutine を
    return → 以後は宙吊りに近い。
- plugin 内蔵の ResumeSession リトライは**瞬断** (サーバ側セッション
  生存中) を既にカバーしている。問題は上記 A/B/C の恒久切断のみ。

つまり in-process では「セッションが恒久的に死んだ」ことを検知する
手段がなく、検知できたとしても plugin が端末を raw mode で掴み
ブロッキング read している最中に安全に unwind する方法がない。

## Decision

**親プロセス監督方式 (child re-exec)** を採用する。

1. `service.Connect` を監督ループに変える。`ssm:StartSession` の後、
   **自バイナリを hidden サブコマンド `nakama aws session-runner` で
   再実行**し、子プロセスにセッション本体を実行させる。plugin の
   `os.Exit` は子プロセスを終えるだけになり、親は終了コードで結果を
   分類できる。
2. 子への受け渡しは SessionId / StreamUrl / TokenValue / region /
   instance-id / endpoint を JSON で **拡張 fd (ExtraFiles, fd 3)**
   経由で渡す。TokenValue は秘匿値のため argv・環境変数・ディスクには
   置かない。stdin はセッション入力 (対話シェル) に必要なため使わない。
   stdin/stdout/stderr は子へそのまま継承し、端末は子が所有する。
3. vendored plugin へ **env ゲート付き最小パッチ**を当てる (前例 =
   `SSM_PLUGIN_SKIP_CLIENT_CONFIGURE` と同じ様式)。新 env 変数
   `SSM_PLUGIN_DISCONNECT_EXIT_CODE` (整数文字列) が設定されている
   場合のみ、恒久切断 A/B/C の 3 経路で `os.Exit(そのコード)` する。
   未設定なら挙動は完全に不変。各パッチ箇所には `// NAKAMA PATCH:`
   コメントを付し、PINNED.md の「Local patches」節に列挙する。
4. 特別終了コードは nakama 側の定数 `service.DisconnectExitCode = 74`
   (BSD sysexits の EX_IOERR 由来。0/1/2 と衝突しない) で 1 箇所定義
   する。親はこのコードのみを「恒久切断」と解釈する。
5. 親のリトライポリシー:
   - 子 exit 0 → 正常終了 (リトライしない)。特別コード → stderr に
     `connection lost; reconnecting (attempt k/N)...` を出し、指数
     バックオフ (1s, 2s, 4s, 8s, 16s, 30s cap、ジッタなし) 後に
     **新規 StartSession** から再実行。それ以外のコード → そのコードの
     まま伝播 (`service.ExitCodeError` を cmd/nakama が os.Exit に変換)。
   - 子が **60 秒以上生存**してから切断された場合は試行カウンタを
     リセットする (長時間トンネルが日をまたいで予算切れしない、
     autossh 方式)。
   - 再接続時は資格情報とセッションを毎回取り直す。StartSession 失敗の
     分類: 資格情報失効/認証エラー → リトライ中断し `aws login` を促す /
     `TargetNotConnected` → 予算内でバックオフ再試行 (ハイバネート復帰
     直後に SSM Agent が未接続の典型) / それ以外 → 即エラー。判定は
     smithy の APIError コードで行う。
   - ctx キャンセルはバックオフ待機中も即座に respect する。
   - シェルモードの再接続時は「新しいセッション (以前のシェル状態は
     失われる)」旨を stderr に通知する。
6. CLI フラグ `--retry <n>`: 既定は対話シェル / ポートフォワード = 5、
   コマンド実行モード (`--tty` 含む) = 0 (非冪等コマンドの再実行は危険な
   ため)。`--retry` を明示した場合はモード不問でその値を尊重する。
   0 = 無効。無効時も実行は子プロセス方式に統一するが、
   `SSM_PLUGIN_DISCONNECT_EXIT_CODE` を設定せずに spawn するため、
   子の挙動 (ハイバネートタイムアウト時に exit 0 等) は従来の一発実行と
   完全に同一になる。
7. シグナル: 子の実行中、親は SIGINT / SIGTSTP / SIGQUIT を無視する
   (シェルモードでは ^C は子の plugin がリモートへ転送する。親が先に
   死ぬとセッションが孤児になる)。子終了後に処理を復元する。

## Consequences

- ハイバネートから復帰したインスタンスへ、`connect` が自動で再接続
  できる (シェルは新規セッションとして。ポートフォワードはリスナー
  再確立)。
- 監督ループ (リトライ・バックオフ・分類) が実プロセスを起動せずに
  ユニットテストできる (`runSession` / `sleep` / `now` を関数フィールド
  として注入)。
- `connect` 実行のたびに子プロセスが 1 つ増える (メモリ・プロセス数の
  微増)。再接続のたびに `awsx.NewClients` の STS プローブが走る分の
  レイテンシも増えるが、恒久切断からの回復という頻度の低い操作であり
  許容する。
- vendored plugin との差分が 2 ファイル・3 経路 + ヘルパー 1 つ分
  増える。再 vendoring 時にパッチの再適用が必要 (PINNED.md に手順と
  一覧を明記)。env 未設定時は挙動不変のため、パッチ起因のリグレッション
  リスクは env を設定する nakama 自身の子プロセスに閉じる。
- 終了コード 74 が nakama の内部プロトコルとして予約される。リモート
  コマンドが 74 を返すケースと衝突しうるが、コマンド実行モードの既定は
  リトライ無効 (= 検知ゲート自体が無効) であり、`--retry` を明示した
  利用者に限られる。

## Alternatives considered

- **in-process フック (plugin の os.Exit を差し替えて Execute から
  戻す)**: 恒久切断 B/C は検知箇所が goroutine / コールバック内にあり、
  plugin が端末を raw mode で掴んでブロッキング read している最中に
  セッションを安全に unwind する手段がない (panic/recover 越しの巻き
  戻しは端末状態・goroutine リークを残す)。却下。
- **外部プラグインバイナリ方式 (公式 session-manager-plugin を子
  プロセスとして起動)**: 追加バイナリのインストールが必要になり、
  「plugin 取り込みにより追加バイナリ不要」という既存の決定 (ADR 0005、
  design.md §12) を覆す。却下。
- **vendored plugin の全面書換 (os.Exit の除去とエラー戻り値化)**:
  upstream との差分が膨大になり、再 vendoring (tag 更新) が事実上
  不可能になる。env ゲート付き最小パッチなら差分は 3 経路 + ヘルパーに
  収まる。却下。
- **リトライなし (現状維持) / 利用者が手動再実行**: ポートフォワードの
  長時間利用で摩擦が大きく、issue #21 の要求を満たさない。却下。

## References

- issue #21 (connect の切断検知・自動再接続)
- ADR 0005 (`nakama aws connect` via Session Manager) / ADR 0009
  (ubuntu ログイン)
- `third_party/session-manager-plugin/PINNED.md` — Local patches
- autossh のカウンタリセット方式: <https://www.harding.motd.ca/autossh/>
