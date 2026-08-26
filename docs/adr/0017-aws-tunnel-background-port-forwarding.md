# ADR 0017: `nakama aws tunnel` — バックグラウンド常駐のポート転送はデーモン + ループバック制御チャネルで実現する

- Status: Accepted
- Date: 2026-08-27
- Issue: [#29](https://github.com/ukaji3/nakama/issues/29)

## Context

ポート転送は従来 `nakama aws connect <name> -L <spec>` で提供していたが、フォアグラウンドでターミナルを専有するため、転送を維持したまま同じターミナルで作業を続けられない。また 1 回の呼び出しで 1 転送しか張れなかった。

要求は次のとおり:

1. `nakama aws tunnel start <name> -L<spec>...` でバックグラウンドに常駐してポート転送する (`start` 省略時も同挙動)
2. 既に転送中なら、その旨と転送一覧を表示して終了する
3. `nakama aws tunnel stop <name>` で転送を停止し常駐プロセスも終了する
4. `nakama aws tunnel status <name>` で転送ごとの状態を表示する
5. `connect -L` は削除する (破壊的変更)

前提となる技術的事実:

- SSM のポート転送ドキュメント (`AWS-StartPortForwardingSession(ToRemoteHost)`) は **1 セッション 1 ポート**。複数 `-L` は転送数ぶんのセッションを要する
- vendored session-manager-plugin は正常終了・恒久切断ともプロセスを終える設計のため、セッション本体は使い捨ての子プロセス (`session-runner`) で実行し親が監督する方式を既に採っている (ADR 0014)
- ローカル listener は plugin (子プロセス) が bind する (`localhost:<localPort>`)
- Windows の `Process.Kill()` は子プロセスを道連れにしない。デーモンを外部から kill するだけでは session-runner 子 ×N が孤児化して転送が生き残る

## Decision

### プロセスモデル: 3 層 (CLI → デーモン → session-runner ×N)

```
[nakama aws tunnel start]        [nakama aws tunnel-daemon (hidden, detached)]
  ├ preflight (名前/インスタンス/     ├ 状態ファイルを O_EXCL で作成
  │  既存デーモン liveness/           ├ 制御チャネル (127.0.0.1:0 + token) を listen
  │  ローカルポート空き)              ├ -L ごとに監督 goroutine:
  ├ tunnel-daemon を detach 起動 ──►  │   loop: ssm:StartSession →
  ├ 状態ファイル + 制御 ping を        │         session-runner 子を spawn (ADR 0014 と同じ
  │  ポーリング (readiness)           │         payload pipe / exit-code 74 ゲート) →
  └ 転送一覧を表示して即終了           │         恒久切断なら指数バックオフで再接続
                                     ├ ログは OS 標準ログへ (unix: syslog / windows: Event Log)
                                     └ stop 要求で全子を終了 + TerminateSession (best-effort)
```

- **detach**: unix は `Setsid`、Windows は `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`。stdio は破棄し、readiness は状態ファイル + 制御 ping のポーリングで確認する (パイプの継承を OS 別に実装するより単純)
- **再接続**: connect と同じポリシー (予算 5 回・指数バックオフ 1s〜30s cap・60 秒安定で予算リセット・認証エラーは即中断)。監督ループは ADR 0014 の実装を転送単位に一般化して共有する

### 制御チャネル: ループバック TCP + ランダムトークン

デーモンは `127.0.0.1:0` (カーネル割当ポート) で HTTP を listen し、`GET /status` と `POST /stop` を提供する。実ポートと crypto/rand 256 bit トークンは状態ファイルにのみ記録し、リクエストは `Authorization: Bearer` のトークン一致 (定数時間比較) を要求する。

- グレースフル停止 (全子プロセスの終了 → セッションの `ssm:TerminateSession` best-effort → 状態ファイル削除) をデーモン自身が実行できる。これは Windows で外部 kill が子を道連れにしない問題への直接の解
- Unix ドメインソケットは Windows (AF_UNIX) の Go 標準ライブラリ対応に不確実性があるため採らない。ループバック TCP は全対応 OS で確実に動作し、他ローカルユーザーからの操作はトークンで防ぐ

### 状態ファイル: `~/.config/nakama/tunnels/<name>.json` (0600)

内容: PID・制御ポート・トークン・転送一覧・開始時刻。`$XDG_CONFIG_HOME` を尊重する既存の profilestore と同じ規約に従う。

- start 時に既存ファイルがあれば制御 ping で liveness を確認し、生きていれば「既に転送中」+ 転送一覧を表示して exit 1 (要求した転送は開始されていないため)。死んでいれば stale として削除して続行
- デーモンは状態ファイルを `O_CREATE|O_EXCL` で作成する。preflight とデーモン起動の間の二重起動競合はこの排他で検出される

### ログ: OS 標準ログ (ローテーションを OS に委譲)

- **Linux / macOS**: `log/syslog` (facility `LOG_USER`、tag `nakama-tunnel`)。ローテーション・保持は journald / rsyslog / macOS unified logging が担う
- **Windows**: Application イベントログ (`golang.org/x/sys/windows/svc/eventlog`、source `nakama`)。イベントソースの事前登録 (要管理者権限) は行わないため、イベントビューアに「説明が見つからない」旨の定型文が付くが、メッセージ本文は完全に読める
- session-runner 子の stdout/stderr はデーモンがパイプで回収して同じログへ転送する

### 名前の予約

`start` / `stop` / `status` は正規のインスタンス名パターン (`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`) に合致し、`tunnel <name>` の省略形とサブコマンド名が曖昧になるため、**インスタンス予約名として `create` で拒否**する。既存インスタンスの操作 (connect / delete 等) は影響を受けない (検証は create の新規名にのみ適用)。

### `connect -L` の削除

ポート転送の入口を `tunnel` に一本化する。`ConnectInput` のポート転送フィールドと SSM ドキュメント組立のポート転送分岐は tunnel 側へ移設する。`connect --retry` の既定値記述から「ポートフォワード = 5」が消え、対話シェル = 5 / コマンド = 0 となる (ADR 0014 の該当記述はこの ADR が上書きする)。

## Consequences

- ターミナルを専有せず複数ポートを同時に転送できる。転送は `status` で観測、`stop` で確実に畳める
- デーモンは AWS 資格情報を自力でリフレッシュするが、login セッションの refresh token 失効 (最長 12h) 後は対話できないため自動復旧しない。該当転送は dead (auth) として `status` に表示し、再 `nakama aws login` + `tunnel start` のし直しを案内する
- 転送量 (バイト数) の計測は提供しない (要件から除外済み)。ローカル listener を plugin が直接 bind する現行構造をそのまま使う
- 再接続中の瞬断ではローカルポートへの接続が一時的に失敗する (listener は plugin と共に消えるため)。再接続完了で自動復旧する
- Windows の detach / 停止経路は CI で自動検証できない (ユーザー実機確認が必要)
- `log/syslog` は unix 系のみでビルドされるため、ログ出力はビルドタグで OS 分割する

## Alternatives considered

1. **`connect -L` のフォアグラウンド維持 + `&` などシェル任せ**: Windows で同等の慣用が無く、stop/status の一貫した UX を提供できない。不採用
2. **転送ごとに独立デーモン (1 転送 1 プロセス)**: stop/status の対象が分散し、状態ファイルと制御チャネルが転送数ぶん必要になる。1 インスタンス 1 デーモンに集約する方が管理が単純。不採用
3. **制御チャネルに Unix ドメインソケット**: Windows AF_UNIX の Go 標準対応が不確実。不採用 (ループバック TCP + トークンで代替)
4. **stop を PID への SIGTERM / Kill のみで実装**: Windows で子プロセスが孤児化し転送が生き残る。不採用 (制御チャネル経由のグレースフル停止を正とし、unix の SIGTERM はデーモン側のシグナルハンドラで同じ停止経路に合流させる)
5. **ログをファイル出力 + 自前ローテーション**: ローテーション実装の保守を抱え込む。OS 標準ログに委譲する方が運用が単純。不採用
6. **転送量計測のためのラッパープロキシ**: 要件から転送量が除外されたため不要。listener を二重にせず plugin 直 bind を維持する

## References

- ADR 0005: `nakama aws connect` は Session Manager 経由
- ADR 0014: 切断検知・自動再接続の親プロセス監督方式 (session-runner・exit code 74・監督ループ)
- ADR 0015: OS 別実装の分割方針 (unix / windows)
- `third_party/session-manager-plugin/PINNED.md`: vendored パッチ一覧
- [Issue #29](https://github.com/ukaji3/nakama/issues/29)
