# nakama 要件定義書 (v1)

> Status: v1 実装済 (`connect` / `update` / NAT インスタンス電源連動 (ADR 0007) を含む)
> Last updated: 2026-08-26

## 1. 目的・スコープ

ローカル開発時の Node.js 等のメモリ不足を解消するため、AWS 上に
[coder/code-server](https://github.com/coder/code-server) を実行する EC2 を
立ち上げ、CloudFront の VPC Origin (Private Origin) 経由で WSS アクセスする。
`nakama` CLI でその一連のライフサイクルを管理する。

対象ユーザは個人開発者 (1 AWS アカウント = 1 nakama 環境を想定)。

## 2. 用語定義

| 用語 | 意味 |
|---|---|
| nakama 環境 | `nakama aws init` で構築する VPC 等の共通基盤一式 |
| nakama インスタンス | `nakama aws create` で個別に作成する EC2 + CloudFront + 関連リソースの単位 |
| 管理タグ | `nakama:managed=true`。リソース帰属の判定に使用 |

## 3. 前提条件

- AWS リージョン: `ap-northeast-1` (東京) **固定**
- 利用 AZ: `ap-northeast-1a` / `ap-northeast-1c` (`apne1-az3` は CloudFront VPC Origins 非対応のため除外)
- 認証: `nakama aws login` (ネイティブ sign-in、aws CLI 不要。ADR 0016)、または AWS SSO / Profile / 環境変数など STS GetCallerIdentity が通る任意の資格情報。既存の `aws login` セッション (`~/.aws/config` の login_session + `~/.aws/login/cache/`) もネイティブに解決・リフレッシュされる
- `nakama aws login` 利用時の追加 IAM 要件 (**呼び出し側プリンシパル**): IAM ユーザー/ロールには AWS 管理ポリシー `SignInLocalDevelopmentAccess` が必要 (アカウント root は不要)
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用), Resource Groups Tagging API
- `connect` 利用時の追加 IAM 権限 (**呼び出し側プリンシパル**): `ssm:StartSession` / `ssm:TerminateSession` / `ssm:ResumeSession`。接続先 EC2 の instance role 側は既存 `AmazonSSMManagedInstanceCore` で充足
- `connect` 利用時のクライアント前提: 追加バイナリ不要 (plugin を nakama 本体に取り込むため。詳細は design.md §12)
- ビルド環境: Go 1.23+
- 実行環境: Linux / macOS / Windows
- ネットワーク: HTTP(S) プロキシ環境では自動検出で対応 (優先順位: `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` 環境変数 → Windows のみ OS 設定 (WinHTTP/IE 静的設定・PAC)。macOS/Linux の OS 設定検出は非対応で環境変数のみ。TLS 検証は OS 証明書ストアを使用するため企業 CA の追加設定は不要。ADR 0015)

## 4. 機能要件

### 4.1 サブコマンド一覧

| カテゴリ | サブコマンド | 機能 |
|---|---|---|
| 基盤 | `nakama aws init` | nakama 環境基盤 (VPC / Subnet / NAT インスタンス / WAF / IP set / IAM) を作成または再利用。既存の NAT Gateway があれば削除し EIP を解放して NAT インスタンスへ移行 |
| アクセス制御 | `nakama aws acl add [IP\|CIDR]` | アクセス元 IP を WAFv2 IP set に追加 (省略時は `https://checkip.amazonaws.com/` で自動取得) |
| アクセス制御 | `nakama aws acl list` | IP set の内容を一覧 |
| アクセス制御 | `nakama aws acl delete <IP\|CIDR>` | IP set からエントリ削除 |
| インスタンス | `nakama aws create --name <n> --instance-type <t> [--with-admin\|--with-role <role>]` | EC2 (Ubuntu 26.04, EBS 20 GiB) を private subnet に作成、code-server を導入、CloudFront distribution を VPC Origin 経由で作成、パスワードを Parameter Store に保存。インスタンスタイプの CPU アーキ (x86_64 / arm64=Graviton) を自動判定し対応する Ubuntu AMI を選択。`--with-admin` は AdministratorAccess を持つ共有ロール `nakama-instance-role-admin` を、`--with-role <role>` は既存の任意 IAM ロールをインスタンスに付与 (両者は排他)。いずれの場合も**リソース作成前**に `iam:SimulatePrincipalPolicy` で呼び出し元の権限をプリフライトし、権限不足なら何も作らずエラー (ADR 0010) |
| インスタンス | `nakama aws list` | 作成済み code-server インスタンスを一覧 (TYPE 列で VSCODE/NAT 種別を表示)。`--show-nat` で NAT インスタンスの稼働状況も表示 |
| インスタンス | `nakama aws info <n> [--show-password]` | 接続先 URL、ステータス、パスワード (`--show-password` 時のみ) を表示 |
| インスタンス | `nakama aws start <n>` | EC2 起動 |
| インスタンス | `nakama aws stop <n>` | EC2 停止 |
| インスタンス | `nakama aws delete <n> [--yes] [--force]` | EC2 / CloudFront / 関連リソースを依存逆順で削除 |
| インスタンス | `nakama aws edit <n> --ebs <size-gb>` | EBS の拡張 (縮小不可) |
| インスタンス | `nakama aws edit <n> --instance-type <t> [--yes]` | EC2 インスタンスタイプ変更。停止が必要なため running は stop→変更→start で元状態へ復帰。`--ebs` とは排他。アーキ跨ぎ (x86_64⇄arm64) の変更は不可 |
| インスタンス | `nakama aws update <n> [--keep N] [--yes]` | インスタンスを最新化: 更新前に自動バックアップ → (stopped なら start) → `apt-get dist-upgrade` + code-server を latest へ → 必要時のみ reboot → 元の電源状態へ復帰 |
| バックアップ | `nakama aws backup create <n> [--label <label>] [--note <text>] [--keep N]` | ルート EBS のスナップショットを取得。`--label` 省略時は取得時刻から自動生成。`--note` で自由記述のメモを付加 (例 `"X を導入済み"`)。`--keep N` 指定時は直近 N 世代を残し古いものを削除 |
| バックアップ | `nakama aws backup list [<n>]` | 当該インスタンスのバックアップを一覧 (ラベル / 取得時刻 / サイズ / 状態 / メモ。AWS の snapshot-id は既定では非表示、`-o json` でのみ表示)。名前を省略すると全インスタンスのバックアップを NAME 列付きで一覧する (インスタンス削除後にバックアップの帰属を辿れるようにするため) |
| バックアップ | `nakama aws backup note <n> --label <label> [--note <text>]` | 取得後にメモを設定・更新する。`--note` 省略時 (または空文字) はメモを消去 |
| バックアップ | `nakama aws backup delete <n> --label <label>` | ラベルで指定したバックアップを削除 |
| バックアップ | `nakama aws restore <n> [--label <label>] [--yes]` | インプレース復元。`--label` 省略時は最新バックアップへ復元。インスタンスを停止しスナップショットから作成したボリュームをルートに差し替え (旧ルートは削除) |
| 接続 | `nakama aws connect <n>` | AWS Systems Manager Session Manager 経由で対話シェルにログイン (SSH 不要・受信ポート開放なし) |
| 接続 | `nakama aws connect <n> <command> [args...]` | リモートで単発コマンドを実行 (既定は非対話。`--tty` で対話 PTY) |
| 接続 | `nakama aws tunnel [start] <n> -L <spec> [-L <spec>...]` | バックグラウンドに常駐してポート転送 (ADR 0017)。`start` は省略可。複数 `-L` 可 (転送ごとに SSM セッション)。既に転送中なら転送一覧を表示して exit 1 |
| 接続 | `nakama aws tunnel stop <n>` | ポート転送を停止し、常駐デーモンも終了する |
| 接続 | `nakama aws tunnel status <n>` | 転送ごとの状態 (active / reconnecting / dead) を表示する |
| 接続 | `nakama aws open <n> [--no-auth] [--print-url] [--browser <cmd>] [--timeout <duration>]` | code-server の CloudFront URL をブラウザで開き、パスワードログインを自動通過する (ADR 0012)。パスワードを SSM から取得し、127.0.0.1 の one-shot ローカルページから `/login` へ top-level form POST する (METHOD A)。パスワード取得失敗・ランチャー不在/失敗・timeout 内未取得などで自動ログイン不能な場合は、理由を stderr に出して URL を開く/表示するだけの動作へ退避し終了コード 0 (METHOD C fallback)。fatal (未存在・非 running・distribution 未設定) のみエラー。呼び出し元 IP が WAF allowlist に無い場合は警告 (中断しない)。パスワードはディスク・ログ・`-o json\|yaml` 出力に一切残さない |
| 認証 | `nakama aws login [--remote] [--name <alias>]` | AWS Management Console のセッションを使いネイティブにサインイン (OAuth 2.0 authorization code + PKCE + DPoP、aws CLI 不要。ADR 0016)。ブラウザ起動 (不可なら URL 表示で待機継続) → 127.0.0.1 のコールバックで認可コード受領 (10 分タイムアウト、state 検証) → AWS CLI 互換キャッシュ (`~/.aws/login/cache/`) へ保存 → STS で識別確認 → nakama profile へ**自動 save & load**。`--remote` は SSH/headless 向けにブラウザの検証コード貼り付けで完了。`~/.aws/config` は変更しない。要 `SignInLocalDevelopmentAccess` ポリシー |
| プロファイル | `nakama aws profile save [--name <alias>]` | 現在の `aws login` セッション (login_session 識別子 + STS identity) をアカウント ID キーで nakama のレジストリ (`~/.config/nakama`) に保存。active profile は無視して「今 login しているもの」を保存し、同一アカウントの再 save は上書き。alias 省略時はアカウント ID。login_session が無い資格情報 (SSO・アクセスキー等) はエラー (ADR 0011) |
| プロファイル | `nakama aws profile list` | 保存済みプロファイルを一覧 (ACTIVE / ACCOUNT / ALIAS / IDENTITY / SAVED / STATUS)。STATUS はネイティブのトークンキャッシュ読取り+リフレッシュ (internal/signin) の成否による best-effort 判定 (valid / expired / unknown) |
| プロファイル | `nakama aws profile load <account\|alias>` | 指定プロファイルを active に設定。以後の全 nakama コマンドはこのアカウントの資格情報**のみ**で動作 (fail-closed、`AWS_PROFILE` より優先)。セッション失効時は警告の上で active は設定 (該当アカウントへの `nakama aws login` (または `aws login`) 再実行で復活) |
| プロファイル | `nakama aws profile delete <account\|alias>` | プロファイルを登録解除。active だった場合は active もクリアし、その旨を表示 |
| プロファイル | `nakama aws profile unload` | active をクリアし、従来の default chain 解決へ戻す |

### 4.2 共通フラグ

| フラグ | 意味 |
|---|---|
| `-v`, `--verbose` | slog の Debug レベル出力 |
| `-o`, `--output {table\|json\|yaml}` | 出力フォーマット |

### 4.3 入力バリデーション

- `<name>`: `^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$` (DNS 安全)。`start` / `stop` / `status` は `tunnel` サブコマンド名と曖昧になるため**予約名**とし、`create` の新規名として拒否する (既存インスタンスの操作は影響しない。ADR 0017)
- `<IP>`: IPv4 単独 (`/32` 自動付与) または IPv4 CIDR。**IPv6 は v1 非対応**
- `<instance-type>`: `ec2:DescribeInstanceTypes` で存在を事前検証し CPU アーキ (x86_64 / arm64) を判定 (create / edit 双方)。存在しないタイプはインスタンスを停止せず即時エラー (`instance type %q does not exist`)。Graviton (arm64) タイプ指定時は arm64 版 Ubuntu AMI を自動選択する。`edit` での**アーキ跨ぎ変更は不可** (別アーキへは delete → create)
- `--ebs <size>`: 現在のサイズより大きい整数 (GiB)
- `--with-admin` / `--with-role <role>` (create 専用): 相互排他。`--with-role` のロール名は `^[\w+=,.@-]{1,64}$` (IAM ロール命名規則) で事前検証し、`iam:GetRole` で存在確認 (未存在は即エラー)。trust policy が `ec2.amazonaws.com` の `sts:AssumeRole` を許可していないロールも即エラー。指定ロールに AmazonSSMManagedInstanceCore / AdministratorAccess のいずれも付いていない場合は警告 (SSM 接続・パスワード取得が動かない可能性)。いずれのフラグでも、リソース作成前に `iam:SimulatePrincipalPolicy` で `iam:PassRole` (+ 未作成分の `iam:CreateRole` / `iam:AttachRolePolicy` / `iam:CreateInstanceProfile` / `iam:AddRoleToInstanceProfile`) を検査し、拒否があれば何も作らずエラー (fail-closed。呼び出し元には `iam:SimulatePrincipalPolicy` 権限が追加で必要)
- `update <n>` (受理状態): 対象は **running または stopped のみ** (`pending`/`stopping`/`terminated` 等は即エラー)。stopped は内部で start→更新→stop と遷移し、元状態へ戻す。code-server は常に latest、apt は `dist-upgrade -y` (非対話: `DEBIAN_FRONTEND=noninteractive` + `--force-confdef/--force-confold`)。reboot は「元 running かつ `/var/run/reboot-required` 存在時のみ」実施 (元 stopped は最終 stop で反映されるため reboot しない)
- `--keep <N>`: 1 以上の整数。省略時は自動削除なし (手動 `backup delete` のみ)。`update` でも自動バックアップの世代数制御に使用
- `--label <label>`: バックアップの人間可読な識別子。`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`。当該 `<name>` 内で一意 (重複はエラー)。`restore` / `backup delete` での選択に使用。利用者は AWS の snapshot-id を意識しない
- `--note <text>`: バックアップの自由記述メモ。最大 255 文字、Unicode 可 (日本語・空白可)。空文字でメモ消去。識別子ではないため一意性は不要
- `-L <spec>` (tunnel 専用): `<localPort>:<remotePort>` または `<localPort>:<host>:<remotePort>`。各ポートは 1–65535。`<host>` 省略時は接続先 instance 自身。複数指定可 (転送ごとに SSM セッションを 1 本張る)。start 系呼び出しでは 1 つ以上必須
- `--tty` (connect 専用): コマンド実行を対話 (PTY) モードにする。省略時は非対話
- `--retry <n>` (connect 専用): セッションが恒久的に切断された際の自動再接続の最大試行回数。0 以上の整数。省略時の既定は対話シェル = 5、コマンド実行モード (`--tty` 含む) = 0 (非冪等コマンドの再実行を避けるため)。明示指定した値はモード不問で尊重。`0` で無効。再接続は新規セッション (シェル状態は失われる) で、指数バックオフ (1s〜30s cap)。セッションが 60 秒以上安定してから切断された場合は試行回数をリセット。資格情報失効時は再試行せず `aws login` を促してエラー (詳細は ADR 0014)。`tunnel` の転送はフラグなしで同じポリシー (予算 5・バックオフ・安定リセット) を常に適用 (ADR 0017)
- `open` のフラグ: `--no-auth` はパスワード取得と one-shot サーバを完全にスキップ。`--print-url` はブラウザを起動せず URL を表示 (auth 有効時は one-shot ログイン URL も表示し、timeout までローカルサーバを維持。headless / SSH 向け)。`--browser <cmd>` はブラウザ起動コマンドの上書き (URL は末尾引数として付加。`$BROWSER` 環境変数も尊重、優先度は `--browser` > `$BROWSER` > OS 既定)。`--timeout <duration>` は one-shot ページの取得待ち上限 (既定 30s)
- connect の位置引数: `<name>` の後続をすべてリモートコマンドとして扱う (cobra `SetInterspersed(false)`)。`--retry` 等の nakama フラグは `<name>` より前に置く
- `profile save --name <alias>`: alias が**別アカウント**の alias またはアカウント ID と衝突する場合はエラー (`profile load <alias>` の曖昧化防止)。自アカウントのエントリ上書きは可
- `profile load/delete <account|alias>`: アカウント ID 完全一致を優先し、次に alias 一致。見つからない場合は保存済み候補一覧付きのエラー

## 5. 非機能要件

### 5.1 セキュリティ

- code-server パスワードは SSM SecureString (KMS デフォルト鍵 `aws/ssm`) に保存。CLI 出力では `--show-password` を要求
- ブラウザ ⇄ CloudFront 間は HTTPS / WSS 強制 (`Viewer Protocol Policy: redirect-to-https`)
- CloudFront ⇄ EC2 間は VPC 内クローズ通信 (HTTP:8080)
- EC2 SG inbound = CloudFront managed prefix list `com.amazonaws.global.cloudfront.origin-facing` のみ
- WAFv2 IP set による送信元制御。default action は **block**
- バックアップ (EBS スナップショット) の暗号化は元ルートボリュームの設定を継承 (専用 KMS 鍵は導入しない)
- `connect` は AWS Systems Manager Session Manager を使用 (受信ポート開放・SSH 鍵不要)。通信は SSM への outbound のみで、data-plane は WSS + SigV4 署名
- `--with-admin` で作成したインスタンスは AdministratorAccess を持つため、code-server へ到達できる者 (CloudFront + WAF allowlist 通過者) が実質的にアカウント管理者権限を持つ。ACL の管理を厳格に行うこと (ADR 0010)
- `nakama aws profile` のレジストリ (`~/.config/nakama`, dir 0700 / file 0600) には**秘密情報を保存しない**。保存するのはアカウント ID・alias・login_session (ARN 形式の識別子)・identity ARN・保存時刻のみで、実トークンは AWS CLI 管理の `~/.aws/login/cache/` に残る。ユーザーの `~/.aws/config` / `~/.aws/credentials` へは書き込まない (ADR 0011)
- active profile 使用時の資格情報解決は fail-closed: 解決失敗時に別チェーンへフォールバックせず、意図しないアカウントへのリソース作成を防ぐ (ADR 0011)
- `nakama aws open` の自動ログイン (ADR 0012) はパスワードを**メモリ内にのみ**保持する: 127.0.0.1 バインド・カーネル割当ポート・crypto/rand 128 bit の単回使用トークンパス・厳密 1 回配信 (再取得は 410)・`Cache-Control: no-store` / `Referrer-Policy: no-referrer`・html/template によるエスケープ・timeout 経過で自動シャットダウン。ディスク書き込み・ログ出力・`-o json|yaml` への出力は一切しない。code-server の `/login` に CSRF / Origin チェックが無いことに依存するため、将来の code-server 側変更で自動ログインが無効化される可能性がある (その場合も METHOD C fallback で URL オープン自体は動作)
- Microsoft Marketplace は使用不可。code-server は Open VSX を使用 (上流仕様)

### 5.2 可用性

- v1 は単一 AZ NAT インスタンス (`t4g.nano`, コスト優先。ADR 0007)
- NAT インスタンスの電源は code-server のライフサイクルに連動 (全停止時は自動停止)
- 単点障害が発生しても再 `init` で復旧可能 (タグベースの冪等性)

### 5.3 観測性

- 全 AWS API 呼び出しは slog で `level=DEBUG`、結果は `level=INFO`
- CloudFront / VPC origin / EC2 status の待機ループ進捗を `level=INFO` に出力

### 5.4 性能

- `init` 所要時間: 約 2〜4 分 (NAT インスタンスの起動待機が主。NAT Gateway より短い)
- `create` 所要時間: 約 15〜20 分 (CloudFront propagation がボトルネック)
- `delete` 所要時間: 約 15〜20 分 (CloudFront disable + delete)

## 6. 状態管理

- ローカル DB を持たない。**AWS リソースのタグから動的取得**
- 一覧取得: `ResourceGroupsTaggingAPI` で `nakama:managed=true` を絞り込み
- per-instance: `nakama:name=<name>` を AND 条件として付加
- 例外: `nakama aws profile` のレジストリのみローカル状態 (`$XDG_CONFIG_HOME/nakama`、未設定時 `~/.config/nakama`) を持つ。これは AWS リソースではなく「どのアカウントの `aws login` セッションを使うか」というクライアント側の選択であり、タグでは表現できないため (ADR 0011)
- 例外: `nakama aws tunnel` のデーモン状態 (`~/.config/nakama/tunnels/<name>.json`, 0600。PID・制御ポート・制御トークン・転送一覧・開始時刻) もローカル状態を持つ。常駐プロセスはクライアント側にのみ存在し、タグでは表現できないため (ADR 0017)

## 7. 制限事項 (v1 で対応しない)

- IPv6 ACL
- 複数リージョン
- 独自ドメイン / ACM 証明書
- 部分失敗時の自動ロールバック (孤児リソースは利用者が手動で `delete`)
- マルチテナント (1 アカウント = 1 環境)
- code-server の任意バージョンへの固定アップグレード (`update` は latest 固定。特定版へのダウングレード/固定は非対応)
- 自動 / スケジュール バックアップ (DLM・EventBridge 等)。v1 は手動 `backup create` のみ
- ルート EBS 以外 (追加データボリューム) のバックアップ
- 期間ベース (N 日経過で削除) の世代管理。v1 は件数ベース (`--keep N`) のみ
- スナップショットのクロスリージョン / クロスアカウントコピー
- 別名インスタンスとしての復元 (v1 はインプレース復元のみ)
- `connect` でのファイル転送 (scp / sftp 相当)
- `connect` セッションログの S3 / CloudWatch Logs への記録 (Session Manager logging 設定)
- `tunnel` のポートごとの転送量 (バイト数) 計測・表示
- `tunnel` の資格情報失効 (login refresh token 最長 12h) 後の自動復旧 (該当転送は dead 表示。再 `nakama aws login` + `tunnel start` のし直しで復旧)
- macOS の OS プロキシ設定 (システム設定) の自動検出 (リリースバイナリは CGO_ENABLED=0 のため。環境変数での指定は可。ADR 0015)
- WebSocket (CONNECT トンネル上のアップグレード) を遮断する監査型プロキシ環境での `connect` 動作保証

## 8. 受け入れ基準

1. `nakama aws init` 後、`aws ec2 describe-vpcs --filters Name=tag:nakama:managed,Values=true` が VPC を 1 件返す
2. `nakama aws acl add` 直後にブラウザから distribution URL にアクセスし、code-server ログイン画面が表示される
3. パスワードでログイン後、ターミナル / ファイルエクスプローラが操作可能で、Browser Devtools で `wss://...` の `101 Switching Protocols` が確認できる
4. `nakama aws acl delete` 後、403 が返る
5. `nakama aws delete` 後、当該インスタンス関連リソースが全て消失している (タグ検索でヒットしない)
6. `nakama aws backup create <n>` 後、`nakama aws backup list <n>` に当該スナップショットが現れ、`completed` になる
7. `nakama aws backup create <n> --keep 1` を 2 回実行後、保持されるスナップショットは直近 1 件のみ
8. `nakama aws restore <n> --yes` 後、インスタンスを start すると最新バックアップ取得時点のファイルが復元されている (snapshot-id を一切指定していない)
9. `nakama aws connect <n>` で対話シェルのプロンプトが表示され、コマンドが実行できる
10. `nakama aws connect <n> ls` でリモートの実行結果が表示され、セッションが終了する
11. `nakama aws tunnel <n> -L 8080:8080 -L 5432:db.internal:5432` 実行後プロンプトが即返り、`localhost:8080` / `localhost:5432` への接続がそれぞれの宛先に到達する。同じ `<n>` への再実行は「既に転送中」+ 転送一覧を表示して exit 1。`tunnel status <n>` が転送ごとの状態を表示し、`tunnel stop <n>` でデーモンと全転送が終了して状態ファイルが消える
12. `nakama aws edit <n> --instance-type <t>` 後、`nakama aws info <n>` の instance_type が `<t>` になり、変更前に running だったインスタンスは再び running になる。存在しないタイプ指定時はインスタンスを停止せず即時エラー。現インスタンスとアーキの異なるタイプ指定時も停止せず即時エラー
13. `nakama aws update <n>` 実行で、更新前にバックアップが1件作成され (`backup list` に出現)、code-server が latest へ更新され (`info` の code_server_version が更新)、`apt-get dist-upgrade` が適用される。実行前 stopped のインスタンスは実行後も stopped に戻る。元 running でカーネル更新があった場合のみ reboot される

## 9. リスク・オープン事項

| ID | 内容 | 対応方針 |
|---|---|---|
| R1 | EC2 の Private IP が stop/start 後に維持されるか保証なし | ENI を専用作成しアタッチすることで固定 |
| R2 | VPC Origin の deploy 待機が最大 15 分 | UI で進捗を slog 出力。タイムアウトは 20 分 |
| R3 | code-server の Microsoft Marketplace 不可は仕様 | README / FAQ に明記 |
| R4 | `apne1-az3` の AZ ID 解決はアカウント依存 | `ec2:DescribeAvailabilityZones` で AZ ID を取得し除外 |
| R5 | NAT のegressコスト | NAT Gateway を廃し NAT インスタンス (`t4g.nano`) を採用 (ADR 0007)。電源を code-server のライフサイクルに連動させ、全停止時は NAT インスタンスも停止して固定費をゼロに近づける。EIP は付与せず停止中の public IPv4 課金も回避 |
| R6 | 稼働中スナップショットはクラッシュコンシステント | 重要時は `stop` 後に `backup create` する旨を明記。`restore` はインプレースで破壊的なため `--yes` を要求 |
| R7 | `--keep` 未指定スナップショットのコスト累積 | `--keep N` の利用を推奨。CLI は常駐しないため prune は `backup create` 実行時のみ |
| R8 | 取り込んだ `session-manager-plugin` の upstream 追随は手動 | 取り込み元 commit/tag を ADR 0005 に記録し、定期的に re-sync |
| R9 | `connect` には呼び出し側プリンシパルに `ssm:StartSession` 等が必要 (instance role とは別) | 前提条件 §3 に明記。`init` では付与しない (利用者責務) |
| R10 | plugin 取り込みによるバイナリサイズ増 | PoC で実測し許容範囲を確認 |
| R11 | Session Manager の独自プロトコル変更・コマンド終了コード伝播の制約 | 取り込み版を pin。終了コードの非互換は制限事項として扱う |
