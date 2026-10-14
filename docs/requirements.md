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
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用), Resource Groups Tagging API。機能別のアクション粒度の一覧は [付録 A. 機能別 IAM 権限マトリクス](#付録-a-機能別-iam-権限マトリクス) を参照 (現在の資格情報での可否は `nakama aws doctor` が表示する)
- `connect` 利用時の追加 IAM 権限 (**呼び出し側プリンシパル**): `ssm:StartSession` / `ssm:TerminateSession` / `ssm:ResumeSession`。接続先 EC2 の instance role 側は既存 `AmazonSSMManagedInstanceCore` で充足
- `connect` 利用時のクライアント前提: 追加バイナリ不要 (plugin を nakama 本体に取り込むため。詳細は design.md §12)
- ビルド環境: Go 1.26+ (`go.mod` の `go` 行)
- 実行環境: Linux / macOS / Windows
- ネットワーク: HTTP(S) プロキシ環境では自動検出で対応 (優先順位: `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` 環境変数 → Windows のみ OS 設定 (WinHTTP/IE 静的設定・PAC)。macOS/Linux の OS 設定検出は非対応で環境変数のみ。TLS 検証は OS 証明書ストアを使用するため企業 CA の追加設定は不要。ADR 0015)

## 4. 機能要件

### 4.1 サブコマンド一覧

| カテゴリ | サブコマンド | 機能 |
|---|---|---|
| 基盤 | `nakama aws init` | nakama 環境基盤 (VPC / Subnet / NAT インスタンス / WAF / IP set / IAM) を作成または再利用。既存の NAT Gateway があれば削除し EIP を解放して NAT インスタンスへ移行。最後に NAT インスタンスの転送を確認し、壊れていれば直す。直らなければ失敗する (Issue #87) |
| アクセス制御 | `nakama aws acl add [IP\|CIDR]` | アクセス元 IP を WAFv2 IP set に追加 (省略時は `https://checkip.amazonaws.com/` で自動取得) |
| アクセス制御 | `nakama aws acl list` | IP set の内容を一覧 |
| アクセス制御 | `nakama aws acl delete <IP\|CIDR>` | IP set からエントリ削除 |
| インスタンス | `nakama aws create --name <n> --instance-type <t> [--with-admin\|--with-role <role>]` | EC2 (Ubuntu 26.04, EBS 20 GiB) を private subnet に作成、code-server を導入、CloudFront distribution を VPC Origin 経由で作成、パスワードを Parameter Store に保存。インスタンスタイプの CPU アーキ (x86_64 / arm64=Graviton) を自動判定し対応する Ubuntu AMI を選択。`--with-admin` は AdministratorAccess を持つ共有ロール `nakama-instance-role-admin` を、`--with-role <role>` は既存の任意 IAM ロールをインスタンスに付与 (両者は排他)。いずれの場合も**リソース作成前**に `iam:SimulatePrincipalPolicy` で呼び出し元の権限をプリフライトし、権限不足なら何も作らずエラー (ADR 0010)。user-data は **Kiro CLI / Kiro Crew / gateway サービスを既定で導入**する (`--no-kiro` でスキップ。Issue #68) |
| インスタンス | `nakama aws create ... --bare` | code-server / CloudFront distribution / VPC Origin / パスワードを作らず EC2 + ENI + instance role + SSM メタデータのみを用意する軽量モード。`/nakama/<n>/meta/kind=bare` で種別を記録 (未記録は従来の code-server インスタンス)。`list` の TYPE 列は BARE。`open` は `ssh setup` / `connect` を案内してエラー。`update` は OS 更新のみ。`connect` / `ssh` / `tunnel` / `backup` / `restore` / `edit` / `start` / `stop` / `delete` は通常どおり動作する。Kiro の既定導入は通常インスタンスと同じ |
| インスタンス | `nakama aws create ... --no-kiro` | user-data での Kiro CLI / Kiro Crew の既定導入をスキップする。導入手順は `internal/kiro/install.sh` を唯一の情報源とし、user-data と `--preset kiro` が同一のスクリプトを共有する (Issue #68) |
| インスタンス | `nakama aws create ... [--setup <path\|https URL>] [--dotfiles <git URL>] [--preset <name>]` | 作成完了後にプロビジョニングを実行する (SSM Run Command、ubuntu ユーザー)。順序は preset → dotfiles → setup で、各ステップは独立に成否を報告する。`--preset` は同梱レシピ (`kiro` = Kiro CLI + Kiro Crew + gateway の systemd 登録。**既定で user-data が実行する内容と同一**で、再実行・後入れの手段として提供する)。`--dotfiles` は `~/.dotfiles` へ clone し `install.sh`/`setup.sh`/`bootstrap.sh` があれば実行、無ければ `$HOME` へシンボリックリンク。`--setup` はローカルパス (内容を転送) または https URL (curl 取得)。http は拒否。ログはインスタンスの `/var/log/nakama-provision.log`。**プロビジョニング失敗は create を失敗させず**警告 + 再実行案内を表示する。冪等性はスクリプト作成者の責務 |
| インスタンス | `nakama aws setup rerun <n> [--setup ...] [--dotfiles ...] [--preset ...]` | 既存インスタンスに対して同じプロビジョニングを実行する (create 時の失敗からの復旧、後からの適用)。フラグを 1 つも指定しない場合はエラー |
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
| バックアップ | `nakama aws backup note <n> --label <label> --note <text>` | 取得後にメモを設定・更新する。`--note` は必須 (省略はエラー)。空文字を明示指定 (`--note ""`) するとメモを消去 |
| バックアップ | `nakama aws backup delete <n> --label <label>` | ラベルで指定したバックアップを削除 |
| バックアップ | `nakama aws restore <n> [--label <label>] [--yes]` | インプレース復元。`--label` 省略時は最新バックアップへ復元。インスタンスを停止しスナップショットから作成したボリュームをルートに差し替え (旧ルートは削除) |
| 接続 | `nakama aws connect <n>` | AWS Systems Manager Session Manager 経由で対話シェルにログイン (SSH 不要・受信ポート開放なし) |
| 接続 | `nakama aws connect <n> <command> [args...]` | リモートで単発コマンドを実行 (既定は非対話。`--tty` で対話 PTY)。非対話では stdout と stderr を分けて出し、リモートの終了コードで終わる (受け取れなければ 255)。標準入力は読まない |
| 接続 | `nakama aws tunnel [start] <n> -L <spec> [-L <spec>...]` | バックグラウンドに常駐してポート転送 (ADR 0017)。`start` は省略可。複数 `-L` 可 (転送ごとに SSM セッション)。既に転送中なら転送一覧を表示して exit 1 |
| 接続 | `nakama aws tunnel stop <n>` | ポート転送を停止し、常駐デーモンも終了する |
| 接続 | `nakama aws tunnel status [<n>]` | 転送ごとの状態 (active / reconnecting / dead) を表示する。名前を省略するとこのマシンに記録された全インスタンスの転送を NAME 列付きで一覧し、デーモン不応答のものは stale として表示して残骸の状態ファイルを掃除する |
| 接続 | `nakama aws tunnel suggest <n>` | インスタンス上で listen 中の TCP ポートを SSM Run Command (`ss`/`netstat`) で列挙し、`-L` スペック案とそのまま実行できる tunnel コマンドを表示する。読み取り専用で自動転送はしない。22 番は `nakama aws ssh` を案内し提案から除外する |
| 接続 | `nakama aws tunnel restart <n>` | 状態ファイルに記録された転送スペックのまま再接続する (stop → start の合成。資格情報失効などで dead になった転送の復旧手段。ADR 0017) |
| 接続 | `nakama aws ssh setup <n> [--identity <pub>] [--no-key] [--port <p>]` | 公開鍵を ubuntu の `authorized_keys` に冪等追加し (SSM Run Command 経由、既存なら追加しない)、ssh config スニペットを `~/.config/nakama/ssh/<n>.conf` (0600) に生成して `Include` 行を案内する。`~/.ssh/config` は変更しない。多行入力 (秘密鍵の誤指定) や非 OpenSSH 公開鍵形式は拒否 (ADR 0019) |
| 接続 | `nakama aws ssh proxy <n> [--port <p>]` | `AWS-StartSSHSession` のセッションを開き自プロセスの stdin/stdout を中継する (OpenSSH の `ProxyCommand` 用)。1 プロセス 1 セッションで、再接続は ssh クライアントの責務。デスクトップ VS Code Remote-SSH / JetBrains Gateway / scp / rsync が利用可能になる (ADR 0019) |
| 接続 | `nakama aws kirocrew <n> [--no-auth] [--print-url] [--browser <cmd>] [--timeout <duration>]` / `kirocrew enable <n>` / `kirocrew disable <n>` | Kiro Crew の Web 画面を **code-server 経由で**ブラウザに開く (ADR 0020)。enable は、Kiro Crew 専用の CloudFront distribution、インスタンス上の nginx (127.0.0.1:15476)、code-server の proxy-domain と trusted-origins、既存の distribution の `/proxy/5476*` を新しい distribution へ 302 で転送する Function を作る (冪等)。開くときは、code-server の自動ログイン (ADR 0012 の方式) と Kiro Crew のトークン受け渡しを、1 回のブラウザ起動で行う。トークンは RSA-OAEP で暗号化して受け取る。Kiro Crew は CloudFront に直接さらさない。disable と delete で片付ける。bare は対象外 |
| 接続 | `nakama aws open <n> [--no-auth] [--print-url] [--browser <cmd>] [--timeout <duration>]` | code-server の CloudFront URL をブラウザで開き、パスワードログインを自動通過する (ADR 0012)。パスワードを SSM から取得し、127.0.0.1 の one-shot ローカルページから `/login` へ top-level form POST する (METHOD A)。パスワード取得失敗・ランチャー不在/失敗・timeout 内未取得などで自動ログイン不能な場合は、理由を stderr に出して URL を開く/表示するだけの動作へ退避し終了コード 0 (METHOD C fallback)。fatal (未存在・非 running・distribution 未設定) のみエラー。呼び出し元 IP が WAF allowlist に無い場合は警告 (中断しない)。パスワードはディスク・ログ・`-o json\|yaml` 出力に一切残さない |
| 認証 | `nakama aws login [--remote] [--name <alias>]` | AWS Management Console のセッションを使いネイティブにサインイン (OAuth 2.0 authorization code + PKCE + DPoP、aws CLI 不要。ADR 0016)。ブラウザ起動 (不可なら URL 表示で待機継続) → 127.0.0.1 のコールバックで認可コード受領 (10 分タイムアウト、state 検証) → AWS CLI 互換キャッシュ (`~/.aws/login/cache/`) へ保存 → STS で識別確認 → nakama profile へ**自動 save & load**。`--remote` は SSH/headless 向けにブラウザの検証コード貼り付けで完了。`~/.aws/config` は変更しない。要 `SignInLocalDevelopmentAccess` ポリシー |
| 診断 | `nakama aws doctor [-o json]` | 環境を読み取り専用で一括診断し、各項目を ok / warn / fail + 対処案内で表示する: プロキシ解決 (ADR 0015 の解決器) と AWS 到達性、TLS インスペクション検出 (ADR 0018 の検出エンジン再利用)、資格情報の解決元 (アカウント / identity / active profile)、自 IP の WAF 許可リスト登録、機能別の IAM 権限充足 (付録 A のマトリクスを SimulatePrincipalPolicy で判定し、機能ごとに ok / 不足アクションを表示)、ローカル tunnel 状態。fail があれば終了コード非 0。資格情報未解決でもローカル診断は継続。機微情報は出力しない |
| CLI 支援 | `nakama aws cli shell [--shell <path>]` | **Windows 専用**。OS のプロキシ設定 (WinHTTP/PAC) を自動解決し、TLS インスペクション検出時は OS ストア検証済みの企業 CA チェーン + Amazon Trust Services ルートを `AWS_CA_BUNDLE` 用 PEM に書き出した上で、`HTTP(S)_PROXY`/`NO_PROXY`/`AWS_CA_BUNDLE` を注入したサブシェル (親シェルと同種を自動判別) を起動する。exit で復帰し、親シェル・レジストリ・`~/.aws/config` に恒久的変更を残さない。NTLM/Negotiate 認証プロキシは検出して回避策を案内。他 OS では Windows 専用である旨を表示してエラー (ADR 0018)。サブシェルの PATH の先頭に nakama.exe のフォルダーを入れる (ADR 0022) |
| 起動支援 | nakama.exe のダブルクリック | **Windows 専用**。引数なしでエクスプローラーから起動されたとき、Windows PowerShell を同じウィンドウで開く。PATH の先頭に nakama.exe のフォルダーを入れ、`cli shell` と同じプロキシ・CA の設定を注入する。設定はそのウィンドウの中だけで、レジストリ・ユーザー環境変数は変更しない。検出に失敗しても理由を表示して PATH だけのシェルを開く。シェルの実行中は Ctrl-C で nakama が終わらない (ADR 0022) |
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

- `<name>`: `^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$` (DNS 安全)。`start` / `stop` / `status` / `restart` は `tunnel` サブコマンド名と曖昧になるため**予約名**とし、`create` の新規名として拒否する (既存インスタンスの操作は影響しない。ADR 0017)
- `<IP>`: IPv4 単独 (`/32` 自動付与) または IPv4 CIDR。**IPv6 は v1 非対応**
- `<instance-type>`: `ec2:DescribeInstanceTypes` で存在を事前検証し CPU アーキ (x86_64 / arm64) を判定 (create / edit 双方)。存在しないタイプはインスタンスを停止せず即時エラー (`instance type %q does not exist`)。Graviton (arm64) タイプ指定時は arm64 版 Ubuntu AMI を自動選択する。`edit` での**アーキ跨ぎ変更は不可** (別アーキへは delete → create)
- `--ebs <size>`: 現在のサイズより大きい整数 (GiB)
- `--with-admin` / `--with-role <role>` (create 専用): 相互排他。`--with-role` のロール名は `^[\w+=,.@-]{1,64}$` (IAM ロール命名規則) で事前検証し、`iam:GetRole` で存在確認 (未存在は即エラー)。trust policy が `ec2.amazonaws.com` の `sts:AssumeRole` を許可していないロールも即エラー。指定ロールに AmazonSSMManagedInstanceCore / AdministratorAccess のいずれも付いていない場合は警告 (SSM 接続・パスワード取得が動かない可能性)。いずれのフラグでも、リソース作成前に `iam:SimulatePrincipalPolicy` で `iam:PassRole` (+ 未作成分の `iam:CreateRole` / `iam:AttachRolePolicy` / `iam:CreateInstanceProfile` / `iam:AddRoleToInstanceProfile`) を検査し、拒否があれば何も作らずエラー (fail-closed。呼び出し元には `iam:SimulatePrincipalPolicy` 権限が追加で必要)
- `update <n>` (受理状態): 対象は **running または stopped のみ** (`pending`/`stopping`/`terminated` 等は即エラー)。stopped は内部で start→更新→stop と遷移し、元状態へ戻す。code-server は常に latest、apt は `dist-upgrade -y` (非対話: `DEBIAN_FRONTEND=noninteractive` + `--force-confdef/--force-confold`)。reboot は「元 running かつ `/var/run/reboot-required` 存在時のみ」実施 (元 stopped は最終 stop で反映されるため reboot しない)
- `--keep <N>`: 1 以上の整数。省略時は自動削除なし (手動 `backup delete` のみ)。`update` でも自動バックアップの世代数制御に使用
- `--label <label>`: バックアップの人間可読な識別子。`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`。当該 `<name>` 内で一意 (重複はエラー)。`restore` / `backup delete` での選択に使用。利用者は AWS の snapshot-id を意識しない
- `--note <text>`: バックアップの自由記述メモ。最大 255 文字、Unicode 可 (日本語・空白可)。`backup note` では必須で、空文字の明示指定でメモ消去 (省略による無警告消去を防ぐ)。識別子ではないため一意性は不要
- `-L <spec>` (tunnel 専用): `<localPort>:<remotePort>` または `<localPort>:<host>:<remotePort>`。各ポートは 1–65535。`<host>` 省略時は接続先 instance 自身。複数指定可 (転送ごとに SSM セッションを 1 本張る)。start 系呼び出しでは 1 つ以上必須
- `--tty` (connect 専用): コマンド実行を対話 (PTY) モードにする。省略時は非対話。PTY ではリモートの終了コードを受け取れない (Session Manager の制約) ため、終了コードは 0 になる
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

- `init` 所要時間: 約 2〜4 分 (NAT インスタンスの起動待機が主。NAT Gateway より短い)。NAT インスタンスの転送の確認に、正常なら約 10 秒、停止中なら 1〜2 分、修理が要るなら数分を足す
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
- `tunnel` の資格情報失効 (login refresh token 最長 12h) 後の自動復旧 (該当転送は dead 表示。再 `nakama aws login` + `tunnel restart <n>` で復旧)
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
10. `nakama aws connect <n> ls` でリモートの実行結果が表示され、セッションが終了する。標準入力が端末・`/dev/null`・パイプのどれでも同じ結果になる。`connect <n> false` は 1、`connect <n> sh -c 'exit 7'` は 7 で終わり、リモートの stderr は手元の stderr に出る。数 MB の出力も欠けずに届き、途中で切れた場合は成功扱いにせず 255 で終わる。対話シェルは標準入力をファイルにしても動き、入力の終わりでセッションが終わる (Issue #70、#75、ADR 0021)
11. `nakama aws tunnel <n> -L 8080:8080 -L 5432:db.internal:5432` 実行後プロンプトが即返り、`localhost:8080` / `localhost:5432` への接続がそれぞれの宛先に到達する。同じ `<n>` への再実行は「既に転送中」+ 転送一覧を表示して exit 1。`tunnel status <n>` が転送ごとの状態を表示し、`tunnel restart <n>` が記録済みの転送スペックのまま再接続し (状態ファイルが無ければエラー)、`tunnel stop <n>` でデーモンと全転送が終了して状態ファイルが消える。`tunnel status` (名前省略) は全インスタンスの転送を NAME 列付きで一覧し、転送が無ければ「転送なし」を表示して終了コード 0。`tunnel suggest <n>` は listen 中ポートと `-L` 案を表示する (8080=code-server が含まれる)。一時的なセッション確立失敗 (session-runner の一般エラー終了) は再接続枠内で自動再試行され、枠の枯渇時のみ dead になる (Detail に exit code と枯渇を明記)
12. `nakama aws edit <n> --instance-type <t>` 後、`nakama aws info <n>` の instance_type が `<t>` になり、変更前に running だったインスタンスは再び running になる。存在しないタイプ指定時はインスタンスを停止せず即時エラー。現インスタンスとアーキの異なるタイプ指定時も停止せず即時エラー
13. `nakama aws update <n>` 実行で、更新前にバックアップが1件作成され (`backup list` に出現)、code-server が latest へ更新され (`info` の code_server_version が更新)、`apt-get dist-upgrade` が適用される。実行前 stopped のインスタンスは実行後も stopped に戻る。元 running でカーネル更新があった場合のみ reboot される
14. (手動 E2E) TLS インスペクションを行う企業プロキシ配下の Windows で `nakama aws cli shell` を実行すると、プロキシ・CA・疎通確認のバナーが表示されてサブシェルが開き、その中で `aws s3 ls` が追加設定なしで成功する。`exit` 後、親シェル・レジストリ・`~/.aws/config` に変更が残らない。Windows 以外の OS では Windows 専用オプションである案内を表示して終了する
15. `nakama aws ssh setup <n>` 実行後、表示された `Include` 行を `~/.ssh/config` に追加すると `ssh nakama-<n>` で ubuntu としてログインでき、デスクトップ版 VS Code の Remote-SSH からも同じ Host 名で接続できる (手動 E2E)。同コマンドの再実行で `authorized_keys` に重複エントリが増えない。`~/.ssh/config` は変更されない。sshd 不在・権限不足時は原因の分かるエラーになる

16. `nakama aws doctor` が固定項目 (proxy / tls / credentials / waf-allowlist / tunnels) に加えて機能別の capability 行 (付録 A の Simulate 対象機能ごとに 1 行) を表示し、全権限がある環境では fail が 0 で終了コード 0 になる。資格情報が解決できない環境でも proxy / tls の診断結果が表示され、AWS 依存項目は skip 表示になる。プロキシ認証要求・エンドポイント未到達は fail、機能に必要なアクションの不足 (不足分を detail に列挙)・WAF 未登録・権限検証不能 (`iam:SimulatePrincipalPolicy` なし)・dead な転送は warn になる。判定は Resource `*` でのシミュレーションのためリソース限定ポリシーでは偽陰性があり得る (その旨を対処案内に含める)。実行によって AWS リソースとローカル設定が一切変更されない

17. `nakama aws create --preset kiro` の完了後、インスタンス上に kiro-cli が導入され Kiro Crew の gateway が systemd で稼働している (Issue #20 の内容)。`--setup ./s.sh` はスクリプトが ubuntu として実行され、`--dotfiles <URL>` はリポジトリが `~/.dotfiles` に配置されて慣例どおり適用される。いずれかのステップが失敗しても create は成功扱いで終了し、警告・ログパス・再実行コマンドが表示される。`nakama aws setup rerun <n>` で同じステップを再実行できる (手動 E2E)

18. `nakama aws create --name <n> --bare` は CloudFront distribution / VPC Origin / パスワードを作らずに完了し (deploy 待ちがないため通常より短時間)、`list` で TYPE=BARE と表示される。`open <n>` は `ssh setup` / `connect` の案内を伴うエラーになる。`connect` / `ssh setup` / `tunnel` / `backup` / `update` (OS のみ) が動作し、`delete <n>` が CloudFront 関連の削除を試みずに完了する (手動 E2E)

19. 付録 A の権限マトリクスは `internal/service/permissions.go` を唯一の情報源とし、実装との乖離が双方向のテストで検出される: 実装が呼ぶ AWS API のアクションが表に無ければ失敗し、表にあるが実装から抽出されず暗黙宣言にも無いアクションも失敗する。docs の表は生成ブロックであり、コードと不一致なら CI が失敗する

20. `nakama aws create` の user-data に Kiro CLI / Kiro Crew / gateway サービスの導入手順が含まれ、`--bare` でも同じ内容が入る。`kirocrew service install` の前に `~/.kiro` の group/world 書き込み権限を落とす (AppArmor profile が group-writable なディレクトリを拒否するため)。Kiro 導入の失敗で user-data 全体が失敗せず、警告と `setup rerun --preset kiro` の案内が出る。`--no-kiro` を付けると user-data に Kiro の記述が一切残らない。導入手順は `internal/kiro/install.sh` を唯一の情報源とし、`--preset kiro` も同じスクリプトを使う (Issue #68)。導入が終わると gateway のサービスが登録され、127.0.0.1:5476 で応答する。導入スクリプトはスタブを相手に実際に実行し、Kiro Crew 未導入・導入済みのどちらでも service install まで進むことをテストで確かめる (Issue #79)

21. `nakama aws kirocrew enable <n>` を実行すると、Kiro Crew 専用の CloudFront distribution (オリジンのカスタムヘッダー `X-Forwarded-Host: 15476.kirocrew.nakama.internal`、既存の VPC Origin と WAF を共用)、インスタンス上の nginx (127.0.0.1:15476 だけで待ち受け、Origin を検査し、Host と Origin を書き換え、X-Forwarded-Proto を付与し、X-Forwarded-For は残す)、code-server の管理ブロック (proxy-domain と、crew だけの trusted-origins)、`VSCODE_PROXY_URI` を既定のパス方式 (`./proxy/{{port}}/`) に固定する systemd drop-in、既存の distribution の `/proxy/5476*` と `/absproxy/5476*` を crew へ 302 で転送する設定 (既定の behavior の全項目を写したもの) が用意される。何度実行しても結果は同じで、途中で失敗しても再実行で続きから進み、distribution が重複しない。既存の distribution の構成、`open` の動作、Ports パネルとターミナルのリンクの書き換え先 (`/proxy/<port>/`) は変わらない。`nakama aws kirocrew <n>` は、code-server のパスワード入力なしで Kiro Crew の画面を開く (トークンは暗号化して受け取り、画面にもログにも出さない)。`kirocrew disable <n>` と `delete <n>` は、Kiro Crew 用 distribution を VPC Origin より先に消し、関連するものをすべて取り除く (手動 E2E)

22. (手動 E2E) Windows で nakama.exe をエクスプローラーからダブルクリックすると、Windows PowerShell のウィンドウが開き、nakama.exe のフォルダーが PATH の先頭にあることと、プロキシ・CA・疎通確認の結果が表示される。そのウィンドウでは、どのフォルダーからでも `nakama version` が動き、検出に成功していれば `aws s3 ls` も追加設定なしで動く。オフラインでもウィンドウは開き、`nakama` を打てる。`exit` でウィンドウが閉じ、レジストリ・ユーザー環境変数に変更は残らない。シェルの中で Ctrl-C を押しても nakama は終わらない (自動テストでは Ctrl-Break を受けた `runShell` の生存を確認する)

23. `nakama aws init` で作った NAT インスタンス (t4g.nano、最新の AL2023 AMI) では、初回起動の設定が OOM なしで完了する。`dnf --releasever=latest upgrade` が適用され、`iptables-nft` だけが入り、MASQUERADE のルールが入る。init は最後に NAT インスタンスの転送を確かめる。転送が壊れた NAT インスタンス (`iptables` が無い、`nakama-nat.service` が失敗) では、init が修理して `nat forwarding: repaired` と表示する。直らなければ init は失敗する。停止中の NAT インスタンスは、確認の後に停止に戻る。`connect` / `ssh` が `TargetNotConnected` で失敗したときは、NAT インスタンスと init を案内する (Issue #87)

## 付録 A. 機能別 IAM 権限マトリクス

呼び出し側プリンシパルに必要な IAM アクションの機能別一覧。`internal/service/permissions.go` の表から生成される (受け入れ基準 19)。現在の資格情報でどの機能が使えるかは `nakama aws doctor` が同じ表に基づいて判定・表示する。

- インスタンス側 (instance role) の権限はこの表の対象外: `init` が付与する `AmazonSSMManagedInstanceCore` で充足する
- doctor の判定は Resource `*` でのポリシーシミュレーションであり、リソース ARN を限定した allow ポリシーは「不足」と表示され得る (偽陰性)

<!-- BEGIN GENERATED: permissions-matrix -->
<!-- このブロックは internal/service/permissions.go から生成されます。直接編集せず、
     NAKAMA_UPDATE_PERMISSIONS_DOCS=1 go test ./internal/service/ -run TestPermissionsDocsSection
     で再生成してください。 -->
| 機能 | コマンド | 必要な IAM アクション | 備考 |
|---|---|---|---|
| all commands | 資格情報を使う全コマンド | `sts:GetCallerIdentity` | 資格情報の疎通確認。IAM ポリシーで拒否できない操作のため doctor のシミュレーション対象外 |
| init | nakama aws init | `ec2:AssociateRouteTable`, `ec2:AttachInternetGateway`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateInternetGateway`, `ec2:CreateRoute`, `ec2:CreateRouteTable`, `ec2:CreateSecurityGroup`, `ec2:CreateSubnet`, `ec2:CreateTags`, `ec2:CreateVpc`, `ec2:DeleteNatGateway`, `ec2:DescribeAddresses`, `ec2:DescribeAvailabilityZones`, `ec2:DescribeInstances`, `ec2:DescribeInternetGateways`, `ec2:DescribeNatGateways`, `ec2:DescribeRouteTables`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSubnets`, `ec2:DescribeVpcs`, `ec2:ModifyInstanceAttribute`, `ec2:ModifySubnetAttribute`, `ec2:ModifyVpcAttribute`, `ec2:ReleaseAddress`, `ec2:ReplaceRoute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:AddRoleToInstanceProfile`, `iam:AttachRolePolicy`, `iam:CreateInstanceProfile`, `iam:CreateRole`, `iam:GetInstanceProfile`, `iam:GetRole`, `iam:PassRole`, `iam:PutRolePolicy`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:PutParameter`, `ssm:SendCommand`, `wafv2:CreateIPSet`, `wafv2:CreateWebACL`, `wafv2:ListIPSets`, `wafv2:ListWebACLs` | 冪等。既存 NAT Gateway の削除と EIP 解放を含む。最後に NAT インスタンスの転送を Run Command で確認し、壊れていれば直す (停止中なら確認のために起動して戻す) |
| acl | nakama aws acl add / list / delete | `ssm:DeleteParameter`, `ssm:GetParametersByPath`, `ssm:PutParameter`, `wafv2:GetIPSet`, `wafv2:UpdateIPSet` |  |
| create | nakama aws create | `cloudfront:CreateDistribution`, `cloudfront:CreateVpcOrigin`, `cloudfront:GetDistribution`, `cloudfront:GetVpcOrigin`, `cloudfront:TagResource`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateNetworkInterface`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DescribeInstanceTypes`, `ec2:DescribeInstances`, `ec2:DescribeManagedPrefixLists`, `ec2:DescribeNetworkInterfaces`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSubnets`, `ec2:DescribeVpcs`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:PutParameter`, `ssm:SendCommand`, `wafv2:ListWebACLs` | --bare は cloudfront:* を使わない。NAT インスタンスの初回作成を含む |
| create --with-admin / --with-role | nakama aws create --with-admin / --with-role | `iam:AddRoleToInstanceProfile`, `iam:AttachRolePolicy`, `iam:CreateInstanceProfile`, `iam:CreateRole`, `iam:GetInstanceProfile`, `iam:GetRole`, `iam:ListAttachedRolePolicies`, `iam:ListInstanceProfilesForRole`, `iam:SimulatePrincipalPolicy` | create 本体の権限に追加で必要 (ADR 0010 の事前検証を含む) |
| list / info | nakama aws list / info | `ec2:DescribeInstances`, `ssm:GetParameter` |  |
| open | nakama aws open | `ec2:DescribeInstances`, `ssm:GetParameter`, `ssm:GetParametersByPath`, `wafv2:GetIPSet` | wafv2:GetIPSet と ssm:GetParametersByPath は許可リスト未登録の 警告表示のためだけに使い、読めない場合は警告を出さずに続行する |
| kirocrew | nakama aws kirocrew | `cloudfront:GetDistribution`, `ec2:DescribeInstances`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:SendCommand` | トークンは RSA-OAEP で暗号化して Run Command で受け取る (ADR 0020)。code-server のパスワード読み取りは open と同じ |
| kirocrew enable / disable | nakama aws kirocrew enable / disable | `cloudfront:CreateDistribution`, `cloudfront:CreateFunction`, `cloudfront:DeleteDistribution`, `cloudfront:DeleteFunction`, `cloudfront:DescribeFunction`, `cloudfront:GetDistribution`, `cloudfront:GetDistributionConfig`, `cloudfront:PublishFunction`, `cloudfront:TagResource`, `cloudfront:UpdateDistribution`, `cloudfront:UpdateFunction`, `ec2:DescribeInstances`, `ssm:DeleteParameters`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:PutParameter`, `ssm:SendCommand` | Kiro Crew 専用の distribution と CloudFront Function を作成・削除する |
| connect | nakama aws connect | `ec2:DescribeInstances`, `ssm:GetParameter`, `ssm:ResumeSession`, `ssm:StartSession`, `ssm:TerminateSession` |  |
| ssh | nakama aws ssh setup / proxy | `ec2:DescribeInstances`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:ResumeSession`, `ssm:SendCommand`, `ssm:StartSession`, `ssm:TerminateSession` | setup の公開鍵投入に SendCommand を使う |
| tunnel | nakama aws tunnel | `ec2:DescribeInstances`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:ResumeSession`, `ssm:SendCommand`, `ssm:StartSession`, `ssm:TerminateSession` | suggest の listen ポート調査に SendCommand を使う。status は資格情報不要 |
| start / stop | nakama aws start / stop | `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetParameter` | NAT インスタンスの電源連動 (初回は作成) を含む |
| edit | nakama aws edit | `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DescribeInstanceTypes`, `ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`, `ec2:DescribeVolumes`, `ec2:ModifyInstanceAttribute`, `ec2:ModifyVolume`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:SendCommand` |  |
| update / setup rerun | nakama aws update / setup rerun | `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateSnapshot`, `ec2:CreateTags`, `ec2:DeleteSnapshot`, `ec2:DeleteTags`, `ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSnapshots`, `ec2:DescribeVolumes`, `ec2:ModifyInstanceAttribute`, `ec2:RebootInstances`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:SendCommand` | update は更新前の自動バックアップを取得する |
| backup | nakama aws backup create / list / note / delete | `ec2:CreateSnapshot`, `ec2:CreateTags`, `ec2:DeleteSnapshot`, `ec2:DeleteTags`, `ec2:DescribeInstances`, `ec2:DescribeSnapshots`, `ec2:DescribeVolumes`, `ssm:GetParameter` |  |
| restore | nakama aws restore | `ec2:AttachVolume`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:CreateVolume`, `ec2:DeleteVolume`, `ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSnapshots`, `ec2:DescribeVolumes`, `ec2:DetachVolume`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetParameter` |  |
| delete | nakama aws delete | `cloudfront:DeleteDistribution`, `cloudfront:DeleteFunction`, `cloudfront:DeleteVpcOrigin`, `cloudfront:DescribeFunction`, `cloudfront:GetDistribution`, `cloudfront:GetDistributionConfig`, `cloudfront:GetVpcOrigin`, `cloudfront:UpdateDistribution`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DeleteNetworkInterface`, `ec2:DeleteSecurityGroup`, `ec2:DeleteVolume`, `ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`, `ec2:DescribeVolumes`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `ec2:TerminateInstances`, `iam:PassRole`, `ssm:DeleteParameters`, `ssm:GetParameter`, `ssm:GetParametersByPath` | --bare で作成したインスタンスは cloudfront:* を使わない。cloudfront:*Function は kirocrew enable 済みのインスタンスでのみ使う |
| doctor | nakama aws doctor | `iam:SimulatePrincipalPolicy`, `ssm:GetParametersByPath`, `wafv2:GetIPSet` | 読み取り専用。iam:SimulatePrincipalPolicy が無い場合も capability 判定が warn になるだけで他の診断は動く |
<!-- END GENERATED: permissions-matrix -->

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
