# nakama 要件定義書 (v1)

> Status: v1 実装済 (`shell` (旧 `connect`) / `update` / NAT インスタンス電源連動 (ADR 0007) / NAT Gateway との切り替え (ADR 0026) / CloudFront のインスタンスごとの任意化 (ADR 0028) を含む)
> Last updated: 2026-11-20

## 1. 目的・スコープ

ローカル開発時の Node.js 等のメモリ不足を解消するため、AWS 上に
[coder/code-server](https://github.com/coder/code-server) を実行する EC2 を
立ち上げ、Session Manager (SSM) のポート転送でブラウザからアクセスする。
インターネットからの経路 (CloudFront の VPC Origin) は、インスタンスごとに求めたときだけ作る (ADR 0028)。
`nakama` CLI でその一連のライフサイクルを管理する。

対象ユーザは個人開発者 (1 AWS アカウント = 1 nakama 環境を想定)。

## 2. 用語定義

| 用語 | 意味 |
|---|---|
| nakama 環境 | `nakama aws init` で構築する VPC 等の共通基盤一式 |
| nakama インスタンス | `nakama aws create` で個別に作成する EC2 + 関連リソース (と、求めたときの CloudFront) の単位 |
| 管理タグ | `nakama:managed=true`。リソース帰属の判定に使用 |

## 3. 前提条件

- AWS リージョン: `ap-northeast-1` (東京) **固定**
- 利用 AZ: `ap-northeast-1a` / `ap-northeast-1c` (`apne1-az3` は CloudFront VPC Origins 非対応のため除外)
- 認証: `nakama aws login` (ネイティブ sign-in、aws CLI 不要。ADR 0016)、または AWS SSO / Profile / 環境変数など STS GetCallerIdentity が通る任意の資格情報。既存の `aws login` セッション (`~/.aws/config` の login_session + `~/.aws/login/cache/`) もネイティブに解決・リフレッシュされる
- `nakama aws login` 利用時の追加 IAM 要件 (**呼び出し側プリンシパル**): IAM ユーザー/ロールには AWS 管理ポリシー `SignInLocalDevelopmentAccess` が必要 (アカウント root は不要)
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用), Resource Groups Tagging API。機能別のアクション粒度の一覧は [付録 A. 機能別 IAM 権限マトリクス](#付録-a-機能別-iam-権限マトリクス) を参照 (現在の資格情報での可否は `nakama aws doctor` が表示する)
- `shell` 利用時の追加 IAM 権限 (**呼び出し側プリンシパル**): `ssm:StartSession` / `ssm:TerminateSession` / `ssm:ResumeSession`。接続先 EC2 の instance role 側は既存 `AmazonSSMManagedInstanceCore` で充足
- `shell` 利用時のクライアント前提: 追加バイナリ不要 (plugin を nakama 本体に取り込むため。詳細は design.md §12)
- ビルド環境: Go 1.26+ (`go.mod` の `go` 行)
- 実行環境: Linux / macOS / Windows
- ネットワーク: HTTP(S) プロキシ環境では自動検出で対応 (優先順位: `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` 環境変数 → Windows のみ OS 設定 (WinHTTP/IE 静的設定・PAC)。macOS/Linux の OS 設定検出は非対応で環境変数のみ。TLS 検証は OS 証明書ストアを使用するため企業 CA の追加設定は不要。ADR 0015)
- 版の確認の通信先: `nakama version --check` と `nakama aws doctor` は GitHub (`api.github.com`) にも問い合わせる。ほかのコマンドは問い合わせない。届かなくても nakama の動作には影響しない (警告だけ)

## 4. 機能要件

### 4.1 サブコマンド一覧

| カテゴリ | サブコマンド | 機能 |
|---|---|---|
| 基盤 | `nakama aws init [--nat gateway\|instance] [--yes]` | nakama 環境基盤 (VPC / Subnet / NAT / WAF / IP set / IAM) を作成または再利用。外向き通信の中継 (NAT) は `--nat` で選ぶ (ADR 0026): `gateway` は public subnet の NAT Gateway (EIP 付き、常時稼働)、`instance` は電源連動の NAT インスタンス (ADR 0007)。`--nat` が無ければ記録した方式 (`/nakama/_meta/nat-mode`) のまま。記録の無い既存環境は `instance`、新しい環境は `gateway`。新しい NAT を用意して確かめ、private の経路を張り替えてから、もう一方を片付ける (NAT Gateway の削除と EIP の解放、または NAT インスタンスの terminate と SG の削除。NAT インスタンスの IAM ロールは残す)。既存環境で方式が変わるときは、内容と稼働中のインスタンスを示して確認を取る (`--yes` で省略)。新しい環境を `gateway` で作るときは料金の注記を出す。`instance` では NAT インスタンスの転送を確認し、壊れていれば直す。直らなければ失敗する (Issue #87。gateway からの切り替えでは経路を張り替える前に確認する) |
| アクセス制御 | `nakama aws acl add [IP\|CIDR]` | アクセス元 IP を WAFv2 IP set に追加。省略時は `https://checkip.amazonaws.com/` と `https://ifconfig.io/ip` に 3 回ずつ問い合わせ、見つかった公開の IPv4 アドレスを重複なしですべて追加する (出口を冗長化したネットワークのため。Issue #104) |
| アクセス制御 | `nakama aws acl list` | IP set の内容を一覧。ADDED_AT (`acl add` で最後に追加・確認した時刻) と AGE (それからの経過日数) を表示する。nakama の外で足したエントリはどちらも空 |
| アクセス制御 | `nakama aws acl delete <IP\|CIDR>...` | IP set からエントリを削除。複数を指定すると 1 回の更新でまとめて消す。1 つでも未登録なら何も消さずにエラー (Issue #99) |
| アクセス制御 | `nakama aws acl delete all [--keep-mine] [--yes]` | IP set のエントリをすべて削除し、SSM の追加日時 (IP set に無い残骸を含む) も消す。消す一覧を表示し、全環境がブラウザから 403 になることを示して確認を取る (`--yes` で省略)。`--keep-mine` は現在の接続元 IP (`acl add` と同じ方法で調べる。複数のことがある) のどれかを含むエントリを残す (Issue #99 / #104) |
| アクセス制御 | `nakama aws acl prune --older-than <期間> [--yes]` | `acl add` で最後に追加・確認した時刻が `<期間>` より前のエントリを、1 回の更新で消し、その追加日時も消す。現在の接続元 IP (`acl add` と同じ方法で調べる) のどれかを含むエントリは古くても残し、追加日時の無いエントリ (nakama の外で足したもの) は消さずに警告する。消すもの・残すものを表示して確認を取る (`--yes` で省略)。時刻は `acl add` を実行したときだけ更新され、`code` やブラウザでの利用では更新されない |
| インスタンス | `nakama aws create <n> --instance-type <t> [--with-admin\|--with-role <role>]` | `<n>` はインスタンス名 (2〜32 文字、英小文字・数字・ハイフン、先頭と末尾は英数字。`--name <n>` は非推奨として v0.30.x の間は受け付け、位置引数と違う値ならエラー。ADR 0030)。EC2 (Ubuntu 26.04, EBS 20 GiB) を private subnet に作成、code-server を導入、パスワードを Parameter Store に保存。SG には受信の規則を付けない。CloudFront は作らない (`--cloudfront` を付けたときだけ、SG の CloudFront の規則・VPC Origin・distribution を作る。`--bare` とは排他。ADR 0028)。インスタンスタイプの CPU アーキ (x86_64 / arm64=Graviton) を自動判定し対応する Ubuntu AMI を選択。`--with-admin` は AdministratorAccess を持つ共有ロール `nakama-instance-role-admin` を、`--with-role <role>` は既存の任意 IAM ロールをインスタンスに付与 (両者は排他)。いずれの場合も**リソース作成前**に `iam:SimulatePrincipalPolicy` で呼び出し元の権限をプリフライトし、権限不足なら何も作らずエラー (ADR 0010)。user-data は **Kiro CLI / Kiro Crew / gateway サービスを既定で導入**する (`--no-kiro` でスキップ。Issue #68) |
| インスタンス | `nakama aws create ... --bare` | code-server / CloudFront distribution / VPC Origin / パスワードを作らず EC2 + ENI + instance role + SSM メタデータのみを用意する軽量モード。`/nakama/<n>/meta/kind=bare` で種別を記録 (未記録は従来の code-server インスタンス)。`list` の TYPE 列は BARE。`code` は `ssh setup` / `shell` を案内してエラー。`update` は OS 更新のみ。`shell` / `ssh` / `tunnel` / `backup` / `restore` / `edit` / `start` / `stop` / `delete` は通常どおり動作する。Kiro の既定導入は通常インスタンスと同じ |
| インスタンス | `nakama aws create ... --no-kiro` | user-data での Kiro CLI / Kiro Crew の既定導入をスキップする。導入手順は `internal/kiro/install.sh` を唯一の情報源とし、user-data と `--preset kiro` が同一のスクリプトを共有する (Issue #68) |
| インスタンス | `nakama aws create ... [--setup <path\|https URL>] [--dotfiles <git URL>] [--preset <name>]` | 作成完了後にプロビジョニングを実行する (SSM Run Command、ubuntu ユーザー)。順序は preset → dotfiles → setup で、各ステップは独立に成否を報告する。`--preset` は同梱レシピ (`kiro` = Kiro CLI + Kiro Crew + gateway の systemd 登録。**既定で user-data が実行する内容と同一**で、再実行・後入れの手段として提供する)。`--dotfiles` は `~/.dotfiles` へ clone し `install.sh`/`setup.sh`/`bootstrap.sh` があれば実行、無ければ `$HOME` へシンボリックリンク。`--setup` はローカルパス (内容を転送) または https URL (curl 取得)。http は拒否。ログはインスタンスの `/var/log/nakama-provision.log`。**プロビジョニング失敗は create を失敗させず**警告 + 再実行案内を表示する。冪等性はスクリプト作成者の責務 |
| インスタンス | `nakama aws provision <n> [--setup ...] [--dotfiles ...] [--preset ...]` | 既存インスタンスに対して同じプロビジョニングを実行する (create 時の失敗からの復旧、後からの適用)。フラグを 1 つも指定しない場合はエラー。`setup rerun <n>` は非推奨の古い名前として v0.30.x の間は動く (ADR 0030) |
| インスタンス | `nakama aws cloudfront add <n> [--kirocrew]` | 既存の code-server インスタンス (running) に CloudFront の経路を足す: SG の受信の規則 (CloudFront managed prefix list から 8080/tcp)、VPC Origin、共有の WAF を付けた distribution。各リソースは作った直後に記録し、途中で止まっても再実行で続きから進む (2 つ目は作らない)。そろっていれば何も作らない。完了時に、この端末の IP が WAF の許可リストに無ければ警告する。Kiro Crew の distribution は `--kirocrew` のときだけ作る (code-server の分の後に。内容は `kirocrew` の行。ADR 0028、ADR 0030)。Kiro Crew の分が失敗したら、code-server の分はできていることと、同じコマンドで続きから進むことを示す |
| インスタンス | `nakama aws cloudfront delete <n> [--kirocrew] [--yes]` | CloudFront の経路を外し、インスタンスは残す。消すもの (distribution とドメイン、Kiro Crew の distribution、VPC Origin、SG の規則) を示して確認を取る (`--yes` で省略)。SG の規則を最初に外し、Kiro Crew の distribution、code-server の distribution、Function、VPC Origin、(running なら) インスタンス上の Kiro Crew 用の設定、記録の順に片付ける。途中で止まっても再実行で続きから進み、その間は `cloudfront add` を受け付けない。CloudFront の無いインスタンス (bare を含む) では、残っている SG の規則を外すだけ (ADR 0028)。`--kirocrew` は Kiro Crew の分 (redirect、Function、Kiro Crew の distribution、running ならインスタンス上の設定) だけを消し、code-server の distribution は残す。Kiro Crew の distribution があれば確認を取り (`--yes` で省略)、無ければ確認せずに残りだけを片付ける (ADR 0030) |
| インスタンス | `nakama aws list` | 作成済み code-server インスタンスを一覧 (TYPE 列で VSCODE/BARE 種別を表示)。`--show-nat` で NAT インスタンス (TYPE NAT、稼働状況) と NAT Gateway (TYPE NATGW、状態) も、外向きの IP とともに表示 |
| インスタンス | `nakama aws info <n> [--show-password]` | ステータス、CloudFront の有無と URL、パスワード (`--show-password` 時のみ) を表示。CloudFront が無ければ開き方 (`code`) と `cloudfront add` を案内する |
| インスタンス | `nakama aws start <n>` | EC2 起動 |
| インスタンス | `nakama aws stop <n>` | EC2 停止 |
| インスタンス | `nakama aws delete <n> [--yes] [--force]` | EC2 / CloudFront / 関連リソースを依存逆順で削除 |
| インスタンス | `nakama aws edit <n> --ebs <size-gb>` | EBS の拡張 (縮小不可) |
| インスタンス | `nakama aws edit <n> --instance-type <t> [--yes]` | EC2 インスタンスタイプ変更。停止が必要なため running は stop→変更→start で元状態へ復帰。`--ebs` とは排他。アーキ跨ぎ (x86_64⇄arm64) の変更は不可 |
| インスタンス | `nakama aws update <n> [--keep N] [--yes]` | インスタンスを最新化: 更新前に自動バックアップ → (stopped なら start) → `apt-get dist-upgrade` + code-server を latest へ + AWS CLI (公式のインストールがあるとき) を最新へ + `kirocrew update` / `kirocrew restart` (Kiro Crew があるとき。失敗は警告) → 必要時のみ reboot → 元の電源状態へ復帰。詳しい出力はインスタンスの `/var/log/nakama-update.log` |
| バックアップ | `nakama aws backup create <n> [--label <label>] [--note <text>] [--keep N]` | ルート EBS のスナップショットを取得。`--label` 省略時は取得時刻から自動生成。`--note` で自由記述のメモを付加 (例 `"X を導入済み"`)。`--keep N` 指定時は直近 N 世代を残し古いものを削除 |
| バックアップ | `nakama aws backup list [<n>]` | 当該インスタンスのバックアップを一覧 (ラベル / 取得時刻 / サイズ / 状態 / メモ。AWS の snapshot-id は既定では非表示、`-o json` でのみ表示)。名前を省略すると全インスタンスのバックアップを NAME 列付きで一覧する (インスタンス削除後にバックアップの帰属を辿れるようにするため) |
| バックアップ | `nakama aws backup note <n> --label <label> --note <text>` | 取得後にメモを設定・更新する。`--note` は必須 (省略はエラー)。空文字を明示指定 (`--note ""`) するとメモを消去 |
| バックアップ | `nakama aws backup delete <n> --label <label> [--yes]` | ラベルで指定したバックアップを削除。消すもの (ラベル、スナップショット、大きさ、作成時刻、メモ) を示して確認を取る (`-y` / `--yes` で省略。標準入力が無ければ何も消さずにエラー。ADR 0030) |
| バックアップ | `nakama aws restore <n> [--label <label>] [--yes]` | インスタンスが**ある**とき: インプレース復元。`--label` 省略時は最新バックアップへ復元。インスタンスを停止しスナップショットから作成したボリュームをルートに差し替え (旧ルートは削除) |
| バックアップ | `nakama aws restore <n> [--label <label>] [--instance-type <t>] [--bare] [--with-admin \| --with-role <role>] [--cloudfront] [--yes]` | インスタンスが**削除済み**のとき: 同じ名前のインスタンスを作り直して復元 (Issue #96、ADR 0023)。`create` と同じリソースを作り、ルートをバックアップから作ったボリュームに差し替える。ソフトウェアは入れ直さない。code-server のパスワード・プライベート IP は新しくなり、SSH のホスト鍵はバックアップのものを引き継ぐ。CloudFront は `--cloudfront` のときだけ作り (URL は新しい)、元のインスタンスに CloudFront があったバックアップ (タグ `nakama:cloudfront`) でフラグが無ければ警告する (ADR 0028)。完了後は停止状態 |
| 接続 | `nakama aws shell <n>` | AWS Systems Manager Session Manager 経由で対話シェルにログイン (SSH 不要・受信ポート開放なし)。`connect <n>` は非推奨の古い名前として v0.30.x の間は動く (ADR 0030) |
| 接続 | `nakama aws shell <n> <command> [args...]` | リモートで単発コマンドを実行 (既定は非対話。`-t` / `--tty` で対話 PTY)。非対話では stdout と stderr を分けて出し、リモートの終了コードで終わる (受け取れなければ 255)。標準入力は読まない |
| 接続 | `nakama aws tunnel [start] <n> -L <spec> [-L <spec>...]` | バックグラウンドに常駐してポート転送 (ADR 0017)。`start` は省略可。複数 `-L` 可 (転送ごとに SSM セッション)。既に転送中なら転送一覧を表示して exit 1。ローカルポートはデーモンが持ち、セッションが切れても閉じずに上限なく張り直す。5 分使われない転送は次の接続まで張り直さない (ADR 0027) |
| 接続 | `nakama aws tunnel stop <n>` | ポート転送を停止し、常駐デーモンも終了する |
| 接続 | `nakama aws tunnel status [<n>]` | 転送ごとの状態 (connecting / active / reconnecting / idle / dead) と、code / kirocrew が足した転送の用途を表示する。名前を省略するとこのマシンに記録された全インスタンスの転送を NAME 列付きで一覧し、デーモン不応答のものは stale として表示して残骸の状態ファイルを掃除する |
| 接続 | `nakama aws tunnel suggest <n>` | インスタンス上で listen 中の TCP ポートを SSM Run Command (`ss`/`netstat`) で列挙し、`-L` スペック案とそのまま実行できる tunnel コマンドを表示する。読み取り専用で自動転送はしない。22 番は `nakama aws ssh` を案内し提案から除外する |
| 接続 | `nakama aws tunnel restart <n>` | 状態ファイルに記録された転送スペック (code / kirocrew が足した転送を含む) のまま再接続する (stop → start の合成。古い版のデーモンの入れ替えや dead になった転送の復旧手段。ADR 0017) |
| 接続 | `nakama aws ssh setup <n> [--identity <pub>] [--no-key] [--port <p>]` | 公開鍵を ubuntu の `authorized_keys` に冪等追加し (SSM Run Command 経由、既存なら追加しない)、ssh config スニペットを `~/.config/nakama/ssh/<n>.conf` (0600) に生成して `Include` 行を案内する。`~/.ssh/config` は変更しない。多行入力 (秘密鍵の誤指定) や非 OpenSSH 公開鍵形式は拒否 (ADR 0019) |
| 接続 | `nakama aws ssh proxy <n> [--port <p>]` | `AWS-StartSSHSession` のセッションを開き自プロセスの stdin/stdout を中継する (OpenSSH の `ProxyCommand` 用)。1 プロセス 1 セッションで、再接続は ssh クライアントの責務。デスクトップ VS Code Remote-SSH / JetBrains Gateway / scp / rsync が利用可能になる (ADR 0019) |
| 接続 | `nakama aws kirocrew <n> [--internet] [--local-port <port>] [--no-auth] [--print-url] [--browser <cmd>] [--timeout <duration>]` | Kiro Crew の Web 画面をブラウザで開く。既定は SSM のポート転送でインスタンスの Kiro Crew (127.0.0.1:5476) へ直接届け (ADR 0027)、手元のルーターの `http://kirocrew.<n>.nakama.localhost:18888` を開く (ADR 0029。`--local-port` を付けると `http://localhost:<port>`)。トークンは 127.0.0.1 の 1 回限りのページから渡す。CloudFront は要らず、bare でも使える。`--internet` は **code-server 経由で** CloudFront から開く (ADR 0020)。その経路は `cloudfront add <n> --kirocrew` で作る (旧 `kirocrew enable <n>`。非推奨として v0.30.x の間は動く。ADR 0030)。code-server の distribution が無ければ先に作り、続けて Kiro Crew 専用の CloudFront distribution、インスタンス上の nginx (127.0.0.1:15476)、code-server の proxy-domain と trusted-origins、既存の distribution の `/proxy/5476*` を新しい distribution へ 302 で転送する Function を作る (冪等)。`--internet` で開くときは、code-server の自動ログイン (ADR 0012 の方式) と Kiro Crew のトークン受け渡しを、1 回のブラウザ起動で行う。トークンは RSA-OAEP で暗号化して受け取る。Kiro Crew は CloudFront に直接さらさない。`cloudfront delete <n> --kirocrew` (旧 `kirocrew disable <n>`)、`cloudfront delete`、delete で片付ける |
| 接続 | `nakama aws code <n> [--internet] [--local-port <port>] [--no-auth] [--print-url] [--browser <cmd>] [--timeout <duration>]` | code-server をブラウザで開き、パスワードログインを自動通過する (ADR 0012)。既定は SSM のポート転送で、インスタンスの tunnel デーモンに転送を足し (ADR 0027)、手元のルーターの `http://code-server.<n>.nakama.localhost:18888` を開く (ADR 0029。`--local-port` を付けると `http://localhost:<port>`)。`--internet` は CloudFront の URL を開く。パスワードを SSM から取得し、127.0.0.1 の one-shot ローカルページから `/login` へ top-level form POST する (METHOD A)。パスワード取得失敗・timeout 内未取得などで自動ログイン不能な場合は、理由を stderr に出して URL を開く動作へ退避し終了コード 0 (METHOD C fallback)。ランチャー不在/失敗のとき、SSM ではエラー (URL を表示しない)、`--internet` では URL を表示して終了コード 0。fatal (未存在・非 running・bare、`--internet` で CloudFront が無い (`cloudfront add` を案内) か distribution のドメインが未記録) はエラー。`--internet` で呼び出し元 IP が WAF allowlist に無い場合は警告 (中断しない)。パスワードはディスク・ログ・`-o json\|yaml` 出力に一切残さない。`open <n>` は非推奨の古い名前として v0.30.x の間は動く (ADR 0030) |
| 認証 | `nakama aws login [--remote] [--alias <alias>]` | AWS Management Console のセッションを使いネイティブにサインイン (OAuth 2.0 authorization code + PKCE + DPoP、aws CLI 不要。ADR 0016)。ブラウザ起動 (不可なら URL 表示で待機継続) → 127.0.0.1 のコールバックで認可コード受領 (10 分タイムアウト、state 検証) → AWS CLI 互換キャッシュ (`~/.aws/login/cache/`) へ保存 → STS で識別確認 → nakama profile へ**自動 save & load**。`--remote` は SSH/headless 向けにブラウザの検証コード貼り付けで完了。`~/.aws/config` は変更しない。要 `SignInLocalDevelopmentAccess` ポリシー。`--name <alias>` は非推奨の古い名前として受け付け、`--alias` と違う値ならブラウザを開く前にエラー (ADR 0030) |
| 診断 | `nakama aws doctor [-o json]` | 環境を読み取り専用で一括診断し、各項目を ok / warn / fail + 対処案内で表示する: プロキシ解決 (ADR 0015 の解決器) と AWS 到達性、TLS インスペクション検出 (ADR 0018 の検出エンジン再利用)、資格情報の解決元 (アカウント / identity / active profile)、自 IP の WAF 許可リスト登録、外向き通信 (egress。記録した NAT の方式、private の経路の向き先、NAT インスタンス / NAT Gateway の状態。経路の先が使えなければ fail、切り替えの残りや記録とのずれは warn。ADR 0026)、機能別の IAM 権限充足 (付録 A のマトリクスを SimulatePrincipalPolicy で判定し、機能ごとに ok / 不足アクションを表示)、ローカル tunnel 状態、nakama の版が最新か (`version --check` と同じ確認。古い・確認できないときは warn、開発ビルドは skip)。fail があれば終了コード非 0。資格情報未解決でもローカル診断は継続。機微情報は出力しない |
| 管理画面 | `nakama` (引数なし) / `nakama ui [--browser <command>]` | ブラウザの管理画面を開く (ADR 0031)。引数なしの `nakama` は、標準入力と標準出力が端末のときだけ開き、それ以外はヘルプを出す。管理画面はこのコマンドのプロセスで動き、ウィンドウを閉じるか Ctrl-C で止まる。`http://control.nakama.localhost:18888/` (ルーターのポート) を、1 回限りのコード付きでブラウザに開かせ、AWS のログインとアカウント、インスタンスの一覧と詳細、作成・削除・電源・タイプ・ディスク・更新・プロビジョニング・バックアップ・復元・CloudFront・許可リスト・init・ポート転送・診断・版の確認、code と kirocrew を画面から行う。状態を変える操作は、対応する `nakama aws …` のコマンドを、ウィンドウから切り離したプロセスで実行する。動いている管理画面があれば、新しいコードでブラウザを開くだけで終わる。ブラウザを起動できなければ、アドレスを表示せずにエラーにする |
| 診断 | `nakama version [--check]` | 版・コミット・ビルド日時を表示する (通信しない)。`--check` は GitHub の Releases API (`https://api.github.com/repos/ukaji3/nakama/releases/latest`) に問い合わせ、今の版・最新の版・リリースページの URL と、最新 / 古い / 手元の方が新しい / 開発ビルド (比較しない) のどれかを表示する。問い合わせの失敗 (遮断、レート制限、5 秒のタイムアウト、壊れた応答) は警告だけで、古い場合も含めて終了コードは 0 |
| CLI 支援 | `nakama aws cli shell [--shell <path>]` | **Windows 専用**。OS のプロキシ設定 (WinHTTP/PAC) を自動解決し、TLS インスペクション検出時は OS ストア検証済みの企業 CA チェーン + Amazon Trust Services ルートを `AWS_CA_BUNDLE` 用 PEM に書き出した上で、`HTTP(S)_PROXY`/`NO_PROXY`/`AWS_CA_BUNDLE` を注入したサブシェル (親シェルと同種を自動判別) を起動する。exit で復帰し、親シェル・レジストリ・`~/.aws/config` に恒久的変更を残さない。NTLM/Negotiate 認証プロキシは検出して回避策を案内。他 OS では Windows 専用である旨を表示してエラー (ADR 0018)。サブシェルの PATH の先頭に nakama.exe のフォルダーを入れる (ADR 0022) |
| 起動支援 | nakama.exe のダブルクリック | **Windows 専用**。引数なしでエクスプローラーから起動されたとき、Windows PowerShell を同じウィンドウで開く。PATH の先頭に nakama.exe のフォルダーを入れ、`cli shell` と同じプロキシ・CA の設定を注入する。設定はそのウィンドウの中だけで、レジストリ・ユーザー環境変数は変更しない。検出に失敗しても理由を表示して PATH だけのシェルを開く。シェルの実行中は Ctrl-C で nakama が終わらない (ADR 0022) |
| プロファイル | `nakama aws profile save [--alias <alias>]` | 現在の `aws login` セッション (login_session 識別子 + STS identity) をアカウント ID キーで nakama のレジストリ (`~/.config/nakama`) に保存。active profile は無視して「今 login しているもの」を保存し、同一アカウントの再 save は上書き。alias 省略時はアカウント ID。login_session が無い資格情報 (SSO・アクセスキー等) はエラー (ADR 0011)。`--name` は login と同じく非推奨 (ADR 0030) |
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
- `acl delete all`: `all` は単独で指定する (IP との併用はエラー)。確認の後に追加されたエントリは消さない。元から空なら何もせず正常終了。`--keep-mine` は `acl add` と同じ方法で接続元 IP を調べ、1 つも検出できなければ何も消さずにエラー。どのエントリにも含まれていなければ警告する。`--keep-mine` は `all` 専用
- `acl prune --older-than <期間>`: 必須。正の整数の日数 (`30d`) か Go の duration (`36h`)。0 以下や `1.5d` はエラー。時刻がちょうど境界 (現在 − 期間) のエントリは古くない。古いエントリがあるときだけ接続元 IP を調べ、1 つも検出できなければ何も消さずにエラー。SSM の追加日時を読めなければ何も消さずにエラー。古いエントリが無ければ IP set を更新しない。確認の後に追加されたエントリと、確認の後に `acl add` で確かめ直されたエントリは消さない
- `<instance-type>`: `ec2:DescribeInstanceTypes` で存在を事前検証し CPU アーキ (x86_64 / arm64) を判定 (create / edit 双方)。存在しないタイプはインスタンスを停止せず即時エラー (`instance type %q does not exist`)。Graviton (arm64) タイプ指定時は arm64 版 Ubuntu AMI を自動選択する。`edit` での**アーキ跨ぎ変更は不可** (別アーキへは delete → create)
- `--ebs <size>`: 現在のサイズより大きい整数 (GiB)
- `--with-admin` / `--with-role <role>` (create と、削除済みインスタンスの restore): 相互排他。`--with-role` のロール名は `^[\w+=,.@-]{1,64}$` (IAM ロール命名規則) で事前検証し、`iam:GetRole` で存在確認 (未存在は即エラー)。trust policy が `ec2.amazonaws.com` の `sts:AssumeRole` を許可していないロールも即エラー。指定ロールに AmazonSSMManagedInstanceCore / AdministratorAccess のいずれも付いていない場合は警告 (SSM 接続・パスワード取得が動かない可能性)。いずれのフラグでも、リソース作成前に `iam:SimulatePrincipalPolicy` で `iam:PassRole` (+ 未作成分の `iam:CreateRole` / `iam:AttachRolePolicy` / `iam:CreateInstanceProfile` / `iam:AddRoleToInstanceProfile`) を検査し、拒否があれば何も作らずエラー (fail-closed。呼び出し元には `iam:SimulatePrincipalPolicy` 権限が追加で必要)
- `update <n>` (受理状態): 対象は **running または stopped のみ** (`pending`/`stopping`/`terminated` 等は即エラー)。stopped は内部で start→更新→stop と遷移し、元状態へ戻す。code-server は常に latest、apt は `dist-upgrade -y` (非対話: `DEBIAN_FRONTEND=noninteractive` + `--force-confdef/--force-confold`)。reboot は「元 running かつ `/var/run/reboot-required` 存在時のみ」実施 (元 stopped は最終 stop で反映されるため reboot しない)
- `--keep <N>`: 1 以上の整数。省略時は自動削除なし (手動 `backup delete` のみ)。`update` でも自動バックアップの世代数制御に使用
- restore で削除済みインスタンスを作り直すときの設定: バックアップのタグ (`backup create` が記録する、インスタンスタイプ・CPU アーキ・種類・IAM ロール名) を既定とする。`--instance-type` はタイプを上書きするが、タグのアーキと違えば何も作らずにエラー。タグの無いバックアップ (Issue #96 より前に取得) では `--instance-type` が必須で、bare なら `--bare` も要る。この場合アーキを検査できないため警告を出す。IAM ロールは自動では付け直さず、元にロールがあれば `--with-admin` / `--with-role` を案内する警告を出す。インスタンスが存在するときにこれらのフラグを指定するとエラー
- `--label <label>`: バックアップの人間可読な識別子。`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`。当該 `<name>` 内で一意 (重複はエラー)。`restore` / `backup delete` での選択に使用。利用者は AWS の snapshot-id を意識しない
- `--note <text>`: バックアップの自由記述メモ。最大 255 文字、Unicode 可 (日本語・空白可)。`backup note` では必須で、空文字の明示指定でメモ消去 (省略による無警告消去を防ぐ)。識別子ではないため一意性は不要
- `-L <spec>` (tunnel 専用): `<localPort>:<remotePort>` または `<localPort>:<host>:<remotePort>`。各ポートは 1–65535。`<host>` 省略時は接続先 instance 自身。複数指定可 (転送ごとに SSM セッションを 1 本張る)。start 系呼び出しでは 1 つ以上必須
- `-t` / `--tty` (shell 専用): コマンド実行を対話 (PTY) モードにする。省略時は非対話。PTY ではリモートの終了コードを受け取れない (Session Manager の制約) ため、終了コードは 0 になる
- `--retry <n>` (shell 専用): セッションが恒久的に切断された際の自動再接続の最大試行回数。0 以上の整数。省略時の既定は対話シェル = 5、コマンド実行モード (`--tty` 含む) = 0 (非冪等コマンドの再実行を避けるため)。明示指定した値はモード不問で尊重。`0` で無効。再接続は新規セッション (シェル状態は失われる) で、指数バックオフ (1s〜30s cap)。セッションが 60 秒以上安定してから切断された場合は試行回数をリセット。資格情報失効時は再試行せず `aws login` を促してエラー (詳細は ADR 0014)。`tunnel` の転送 (code / kirocrew が足したものを含む) はこのフラグの対象外で、回数の上限なく張り直す (ADR 0027)
- `code` / `kirocrew` のフラグ: `--no-auth` は自動のサインインをスキップ (code はパスワード取得と one-shot サーバ、kirocrew の SSM ではトークンの受け渡し)。`--print-url` はブラウザを起動せず URL を表示 (auth 有効時は one-shot ログイン URL も表示し、timeout までローカルサーバを維持)。`--browser <cmd>` はブラウザ起動コマンドの上書き (URL は末尾引数として付加。`$BROWSER` 環境変数も尊重、優先度は `--browser` > `$BROWSER` > OS 既定)。`--timeout <duration>` は one-shot ページの取得待ち上限 (既定 30s)。`--internet` は CloudFront の経路を使う。`--local-port <port>` は SSM の転送を、そのローカルポート (1–65535) でも待ち受けさせ、ルーターを使わずに `http://localhost:<port>` を開く。省略時の転送のローカルポートはシステムが選ぶ空きポートで、ブラウザはルーターを通る (ADR 0029)。`--internet` との併用はエラー。`--local-port` のポートが使用中ならエラーにし (別のポートへ逃げない)、nakama の別の転送が使っていればそのインスタンス名を示す。同じインスタンスの同じ先への転送が既にあれば、それを使う (`--local-port` で別のポートを明示したらエラー)。ルーターのポート (既定 18888、`NAKAMA_ROUTER_PORT`) が使用中なら、エラーにして `NAKAMA_ROUTER_PORT` を案内する (別のポートへ移らない)
- shell の位置引数: `<name>` の後続をすべてリモートコマンドとして扱う (cobra `SetInterspersed(false)`)。`--retry` 等の nakama フラグは `<name>` より前に置く
- `login` / `profile save` の `--alias <alias>`: alias が**別アカウント**の alias またはアカウント ID と衝突する場合はエラー (`profile load <alias>` の曖昧化防止)。自アカウントのエントリ上書きは可
- `profile load/delete <account|alias>`: アカウント ID 完全一致を優先し、次に alias 一致。見つからない場合は保存済み候補一覧付きのエラー

## 5. 非機能要件

### 5.1 セキュリティ

- code-server パスワードは SSM SecureString (KMS デフォルト鍵 `aws/ssm`) に保存。CLI 出力では `--show-password` を要求
- EC2 SG inbound = なし。CloudFront を足したインスタンス (`create --cloudfront` / `cloudfront add`) だけ、CloudFront managed prefix list `com.amazonaws.global.cloudfront.origin-facing` から 8080/tcp (ADR 0028)
- CloudFront を足したインスタンスでは、ブラウザ ⇄ CloudFront 間は HTTPS / WSS 強制 (`Viewer Protocol Policy: redirect-to-https`)、CloudFront ⇄ EC2 間は VPC 内クローズ通信 (HTTP:8080)
- WAFv2 IP set による送信元制御。default action は **block**
- バックアップ (EBS スナップショット) の暗号化は元ルートボリュームの設定を継承 (専用 KMS 鍵は導入しない)
- `shell` は AWS Systems Manager Session Manager を使用 (受信ポート開放・SSH 鍵不要)。通信は SSM への outbound のみで、data-plane は WSS + SigV4 署名
- `--with-admin` で作成したインスタンスは AdministratorAccess を持つため、code-server にサインインできる者 (SSM の転送を張れる者、CloudFront があれば WAF allowlist 通過者も) が実質的にアカウント管理者権限を持つ。IAM と ACL の管理を厳格に行うこと (ADR 0010)
- `nakama aws profile` のレジストリ (`~/.config/nakama`, dir 0700 / file 0600) には**秘密情報を保存しない**。保存するのはアカウント ID・alias・login_session (ARN 形式の識別子)・identity ARN・保存時刻のみで、実トークンは AWS CLI 管理の `~/.aws/login/cache/` に残る。ユーザーの `~/.aws/config` / `~/.aws/credentials` へは書き込まない (ADR 0011)
- active profile 使用時の資格情報解決は fail-closed: 解決失敗時に別チェーンへフォールバックせず、意図しないアカウントへのリソース作成を防ぐ (ADR 0011)
- `nakama aws code` の自動ログイン (ADR 0012) はパスワードを**メモリ内にのみ**保持する: 127.0.0.1 バインド・カーネル割当ポート・crypto/rand 128 bit の単回使用トークンパス・厳密 1 回配信 (再取得は 410)・`Cache-Control: no-store` / `Referrer-Policy: no-referrer`・html/template によるエスケープ・timeout 経過で自動シャットダウン。ディスク書き込み・ログ出力・`-o json|yaml` への出力は一切しない。code-server の `/login` に CSRF / Origin チェックが無いことに依存するため、将来の code-server 側変更で自動ログインが無効化される可能性がある (その場合も METHOD C fallback で URL オープン自体は動作)
- Microsoft Marketplace は使用不可。code-server は Open VSX を使用 (上流仕様)
- 外向き通信の中継 (ADR 0026): `gateway` (NAT Gateway) では、nakama が OS を管理するホストを public subnet に置かない。`instance` (NAT インスタンス) は、OS (カーネル、SSM エージェント、iptables) の更新が初回起動時と init の修理のときだけで、SG は VPC の CIDR からの全プロトコルを受け付け、全インスタンスの外向き通信が通る。攻撃面を減らすなら `gateway` を選ぶ (新しい環境の既定)
- NAT インスタンスの IAM ロール (`nakama-nat-role`) は SSM の管理だけで Parameter Store を読めない。gateway に切り替えても残す (戻すときに使う)
- ブラウザからの接続 (ADR 0027): `code` / `kirocrew` は既定で SSM のポート転送を使い、WAF の許可リストへの登録は要らない。転送のローカルポートは 127.0.0.1 と ::1 だけで待ち受けるが、同じ機械のほかのプロセスからは届く (code-server はパスワード、Kiro Crew はトークンを求める)。SSM 経由の Kiro Crew は要求をこの機械からとみなし、ブラウザから設定の変更と秘密情報の表示を許す。インスタンスは既定で CloudFront を持たず、SG に受信の規則も無い (ADR 0028)。CloudFront を足したインスタンスで `--internet` を使わなくなったら、`cloudfront delete` で外す
- Kiro Crew のトークンは、SSM の経路でも 127.0.0.1 の 1 回限りのページから渡し、ブラウザのコマンドラインに載せない (期限内に読まれなかったときだけ、トークン付きの URL を直接開く)
- ブラウザ用の転送のルーター (ADR 0029): 127.0.0.1 と ::1 だけで待ち受け、`<code-server|kirocrew>.<名前>.nakama.localhost:<自分のポート>` 以外の Host には 421 を返す (DNS rebinding の防御)。AWS の資格情報を持たず、tunnel の状態ファイルを読むだけ。`Forwarded`・`X-Forwarded-*`・`X-Real-IP` を転送先に渡さない。Kiro Crew へは Host を `localhost:5476` にし、Origin と Referer はルーター自身のオリジンを指すときだけ `http://localhost:5476` に置き換える (ほかのオリジンは Kiro Crew がそのまま拒む)。同じ機械のほかのプロセスから届くのは、転送のローカルポートと同じ。状態ファイル (`~/.config/nakama/router.json`) は 0600 で、制御のトークンを含む
- ブラウザの管理画面 (ADR 0031): `nakama ui` のプロセスの中で 127.0.0.1 だけで待ち受け、ルーターが `control.nakama.localhost:<ポート>` を中継する。`nakama ui` がブラウザに渡す 1 回限りのコード (256 ビット、60 秒) を使ったときだけセッションができ、セッションはメモリにだけ置く (アイドル 30 分、最長 12 時間)。Cookie は `__Host-` 付き・Secure・HttpOnly・SameSite=Lax。`*.nakama.localhost` はすべて同じサイトなので SameSite に頼らず、状態を変える要求には `Sec-Fetch-Site: same-origin`・管理画面の Origin・`http.CrossOriginProtection`・CSRF トークンを、API の読み取りには `Sec-Fetch-Site: same-origin` を求め、埋め込みを拒む。Host が違えば 421。Linux では、接続の相手のソケットが nakama を実行しているユーザーのものでなければ、ルーターと管理画面が拒む。同じ OS のユーザーで動くプログラムは状態ファイルや Cookie を読めるので防げない (CLI と同じ)。状態ファイル (`~/.config/nakama/panel.json`) は 0600 で、制御のトークンを含む。操作の記録 (`~/.config/nakama/ops/<id>/`、0700 / 0600) はコマンドの出力とログを 7 日間残す。パスワードは含めない (`--show-password` を付けない)。runner の制御のチャネルは 127.0.0.1 の bearer token。パスワードの表示は、詳細の画面で頼んだとき (POST) だけ

### 5.2 可用性

- 外向き通信の中継は単一 AZ (1a)。`gateway` は zonal の NAT Gateway (AWS が管理、常時稼働。ADR 0026)、`instance` は NAT インスタンス (`t4g.nano`, コスト優先。ADR 0007)
- `instance` の NAT インスタンスの電源は code-server のライフサイクルに連動 (全停止時は自動停止)。`gateway` では NAT の電源を操作しない
- 単点障害が発生しても再 `init` で復旧可能 (タグベースの冪等性。`failed` の NAT Gateway は init が作り直す)
- SSM の転送 (code / kirocrew / tunnel) は、切断 (通信エラー、ネットワークの切り替え、スリープ、アイドルタイムアウト、インスタンスの停止と起動、資格情報の期限切れ) のあと、回数の上限なく張り直す。応答の返らない切断には、code / kirocrew の転送で最長およそ 1 分で気づく (ADR 0027)
- ブラウザ用の転送のルーターは、利用者ごとに 1 つのプロセスである。止まっていれば、code / kirocrew をもう一度実行すると起動し直す。tunnel のデーモンも管理画面も無い状態が 1 分続くと自分で終わる。何日動き続けても、tunnel のデーモンとの制御の接続を手元に溜めない (要求ごとに閉じる。Issue #135)。ルーターを使えないときは `--local-port` で直接開ける (ADR 0029)

### 5.3 観測性

- 全 AWS API 呼び出しは slog で `level=DEBUG`、結果は `level=INFO`
- CloudFront / VPC origin / EC2 status の待機ループ進捗を `level=INFO` に出力

### 5.4 性能

- `init` 所要時間: 約 2〜4 分 (NAT インスタンスの起動待機が主。NAT Gateway より短い)。NAT インスタンスの転送の確認に、正常なら約 10 秒、停止中なら 1〜2 分、修理が要るなら数分を足す
- `init` で NAT Gateway を作るとき (新しい環境の既定、または切り替え) は、NAT Gateway が `available` になるまで待つ (数分、上限 10 分)。切り替えでは、古い NAT の片付け (NAT Gateway の削除、または NAT インスタンスの terminate) の待ちも足す
- `gateway` では `create` / `start` / `update` が NAT の起動を待たない
- `create` 所要時間: CloudFront なしでは EC2 が `running` になるまで (実測で約 20 秒。code-server の導入は cloud-init が続ける)。`--cloudfront` では約 15〜20 分 (CloudFront propagation がボトルネック)
- `cloudfront add` 所要時間: 実測で約 5 分 (VPC Origin と distribution の deploy)。`cloudfront delete` は Kiro Crew ありで実測約 14 分 (distribution 1 つの disable の反映が約 3 分、VPC Origin の削除が約 7 分)
- `delete` 所要時間: CloudFront があれば約 15〜20 分 (CloudFront disable + delete)。無ければ EC2 の terminate の待ちだけ
- SSM の転送は CloudFront より遅い (実測でセッション 1 本あたり約 750 KB/s。並行する接続の合計も同じ)。ブラウザの初回の読み込みに数秒から十数秒かかりうる (ADR 0027)

## 6. 状態管理

- ローカル DB を持たない。**AWS リソースのタグから動的取得**
- 一覧取得: `ResourceGroupsTaggingAPI` で `nakama:managed=true` を絞り込み
- per-instance: `nakama:name=<name>` を AND 条件として付加
- 例外: `nakama aws profile` のレジストリのみローカル状態 (`$XDG_CONFIG_HOME/nakama`、未設定時 `~/.config/nakama`) を持つ。これは AWS リソースではなく「どのアカウントの `aws login` セッションを使うか」というクライアント側の選択であり、タグでは表現できないため (ADR 0011)
- 例外: `nakama aws tunnel` のデーモン状態 (`~/.config/nakama/tunnels/<name>.json`, 0600。PID・制御ポート・制御トークン・転送一覧・開始時刻) もローカル状態を持つ。常駐プロセスはクライアント側にのみ存在し、タグでは表現できないため (ADR 0017)
- 例外: ブラウザ用の転送のルーターの状態 (`~/.config/nakama/router.json`, 0600。PID・ポート・制御ポート・制御トークン・版・開始時刻)。tunnel の状態のディレクトリの外に置く (`router` は有効なインスタンス名のため。ADR 0029)
- 例外: 管理画面の操作の記録 (`~/.config/nakama/ops/<id>/`。コマンドライン・結果・ログ・終了コード。7 日で消す。ADR 0031)
- 例外: ブラウザの管理画面の状態 (`~/.config/nakama/panel.json`, 0600。PID・画面のポート・制御ポート・ルーターのポート・制御トークン・版・開始時刻)。`nakama ui` が動くあいだだけある。セッションはメモリにだけ置き、ファイルに書かない (ADR 0031)
- 外向き通信の方式 (ADR 0026): init が選んだ方式を SSM の `/nakama/_meta/nat-mode` (`instance` / `gateway`) に記録する。記録を読むのは init (`--nat` が無いときの方式) と doctor (実際の経路とのずれの検出) だけ。ほかのコマンドは、NAT インスタンスがあるか (無ければ NAT Gateway) で判断する

## 7. 制限事項 (v1 で対応しない)

- IPv6 ACL
- 複数リージョン
- 独自ドメイン / ACM 証明書
- 部分失敗時の自動ロールバック (create / restore は各リソースを作った直後に記録するので、途中で失敗・中断しても利用者が `delete` で片付ける。受け入れ基準 30・31)
- マルチテナント (1 アカウント = 1 環境)
- code-server の任意バージョンへの固定アップグレード (`update` は latest 固定。特定版へのダウングレード/固定は非対応)
- 自動 / スケジュール バックアップ (DLM・EventBridge 等)。v1 は手動 `backup create` のみ
- ルート EBS 以外 (追加データボリューム) のバックアップ
- 期間ベース (N 日経過で削除) の世代管理。v1 は件数ベース (`--keep N`) のみ
- スナップショットのクロスリージョン / クロスアカウントコピー
- 別名インスタンスとしての復元 (同じ名前での作り直しのみ。Issue #96)
- `shell` でのファイル転送 (scp / sftp 相当)
- `shell` セッションログの S3 / CloudWatch Logs への記録 (Session Manager logging 設定)
- `tunnel` のポートごとの転送量 (バイト数) 計測・表示
- `tunnel` の資格情報失効 (login refresh token 最長 12h) 後の自動復旧 (該当転送は dead 表示。再 `nakama aws login` + `tunnel restart <n>` で復旧)
- macOS の OS プロキシ設定 (システム設定) の自動検出 (リリースバイナリは CGO_ENABLED=0 のため。環境変数での指定は可。ADR 0015)
- WebSocket (CONNECT トンネル上のアップグレード) を遮断する監査型プロキシ環境での `shell` 動作保証
- nakama 自身の更新 (自己更新) と、版の自動・定期的な確認。版の確認は `version --check` と `doctor` を実行したときだけで、結果は保存せず、GitHub のトークンも使わない
- Regional NAT Gateway、NAT Gateway の電源連動 (全停止時の削除と再作成)、インスタンスごとの NAT の選択 (ADR 0026)
- この版より前の nakama との混在: 古い版の `init` は NAT Gateway を削除して NAT インスタンスへ戻す。`gateway` に切り替える前に、すべての端末の nakama を更新する (ADR 0026)
- ルーターの URL を開けない環境: macOS 15 以前の Safari と、`*.localhost` を DNS に問い合わせるツール (`--local-port` で `http://localhost:<port>` を使う)。ルーターのインスタンスの一覧のページと、インスタンスの任意のポートへの振り分け。別の AWS プロファイルにある同じ名前のインスタンスを、ルーターの名前で区別すること (ADR 0029)

## 8. 受け入れ基準

1. `nakama aws init` 後、`aws ec2 describe-vpcs --filters Name=tag:nakama:managed,Values=true` が VPC を 1 件返す
2. CloudFront を足したインスタンス (`cloudfront add`) で、`nakama aws acl add` 直後にブラウザから distribution URL にアクセスし、code-server ログイン画面が表示される
3. パスワードでログイン後、ターミナル / ファイルエクスプローラが操作可能で、Browser Devtools で `wss://...` の `101 Switching Protocols` が確認できる
4. `nakama aws acl delete` 後、403 が返る
5. `nakama aws delete` 後、当該インスタンス関連リソースが全て消失している (タグ検索でヒットしない)
6. `nakama aws backup create <n>` 後、`nakama aws backup list <n>` に当該スナップショットが現れ、`completed` になる
7. `nakama aws backup create <n> --keep 1` を 2 回実行後、保持されるスナップショットは直近 1 件のみ
8. `nakama aws restore <n> --yes` 後、インスタンスを start すると最新バックアップ取得時点のファイルが復元されている (snapshot-id を一切指定していない)
9. `nakama aws shell <n>` で対話シェルのプロンプトが表示され、コマンドが実行できる
10. `nakama aws shell <n> ls` でリモートの実行結果が表示され、セッションが終了する。標準入力が端末・`/dev/null`・パイプのどれでも同じ結果になる。`shell <n> false` は 1、`shell <n> sh -c 'exit 7'` は 7 で終わり、リモートの stderr は手元の stderr に出る。数 MB の出力も欠けずに届き、途中で切れた場合は成功扱いにせず 255 で終わる。対話シェルは標準入力をファイルにしても動き、入力の終わりでセッションが終わる (Issue #70、#75、ADR 0021)
11. `nakama aws tunnel <n> -L 8080:8080 -L 5432:db.internal:5432` 実行後プロンプトが即返り、`localhost:8080` / `localhost:5432` への接続がそれぞれの宛先に到達する。同じ `<n>` への再実行は「既に転送中」+ 転送一覧を表示して exit 1。`tunnel status <n>` が転送ごとの状態を表示し、`tunnel restart <n>` が記録済みの転送スペックのまま再接続し (状態ファイルが無ければエラー)、`tunnel stop <n>` でデーモンと全転送が終了して状態ファイルが消える。`tunnel status` (名前省略) は全インスタンスの転送を NAME 列付きで一覧し、転送が無ければ「転送なし」を表示して終了コード 0。`tunnel suggest <n>` は listen 中ポートと `-L` 案を表示する (8080=code-server が含まれる)。一時的なセッション確立失敗 (session-runner の一般エラー終了) は再接続枠内で自動再試行され、枠の枯渇時のみ dead になる (Detail に exit code と枯渇を明記)
12. `nakama aws edit <n> --instance-type <t>` 後、`nakama aws info <n>` の instance_type が `<t>` になり、変更前に running だったインスタンスは再び running になる。存在しないタイプ指定時はインスタンスを停止せず即時エラー。現インスタンスとアーキの異なるタイプ指定時も停止せず即時エラー
13. `nakama aws update <n>` 実行で、更新前にバックアップが1件作成され (`backup list` に出現)、code-server が latest へ更新され (`info` の code_server_version が更新)、`apt-get dist-upgrade` が適用される。実行前 stopped のインスタンスは実行後も stopped に戻る。元 running でカーネル更新があった場合のみ reboot される
14. (手動 E2E) TLS インスペクションを行う企業プロキシ配下の Windows で `nakama aws cli shell` を実行すると、プロキシ・CA・疎通確認のバナーが表示されてサブシェルが開き、その中で `aws s3 ls` が追加設定なしで成功する。`exit` 後、親シェル・レジストリ・`~/.aws/config` に変更が残らない。Windows 以外の OS では Windows 専用オプションである案内を表示して終了する
15. `nakama aws ssh setup <n>` 実行後、表示された `Include` 行を `~/.ssh/config` に追加すると `ssh nakama-<n>` で ubuntu としてログインでき、デスクトップ版 VS Code の Remote-SSH からも同じ Host 名で接続できる (手動 E2E)。同コマンドの再実行で `authorized_keys` に重複エントリが増えない。`~/.ssh/config` は変更されない。sshd 不在・権限不足時は原因の分かるエラーになる

16. `nakama aws doctor` が固定項目 (proxy / tls / credentials / waf-allowlist / tunnels / version) に加えて機能別の capability 行 (付録 A の Simulate 対象機能ごとに 1 行) を表示し、全権限がある環境では fail が 0 で終了コード 0 になる。資格情報が解決できない環境でも proxy / tls の診断結果が表示され、AWS 依存項目は skip 表示になる。プロキシ認証要求・エンドポイント未到達は fail、機能に必要なアクションの不足 (不足分を detail に列挙)・WAF 未登録・権限検証不能 (`iam:SimulatePrincipalPolicy` なし)・dead な転送・nakama の版が古い・版を確認できない (基準 28) は warn になる。判定は Resource `*` でのシミュレーションのためリソース限定ポリシーでは偽陰性があり得る (その旨を対処案内に含める)。実行によって AWS リソースとローカル設定が一切変更されない

17. `nakama aws create --preset kiro` の完了後、インスタンス上に kiro-cli が導入され Kiro Crew の gateway が systemd で稼働している (Issue #20 の内容)。`--setup ./s.sh` はスクリプトが ubuntu として実行され、`--dotfiles <URL>` はリポジトリが `~/.dotfiles` に配置されて慣例どおり適用される。いずれかのステップが失敗しても create は成功扱いで終了し、警告・ログパス・再実行コマンドが表示される。`nakama aws provision <n>` で同じステップを再実行できる (手動 E2E)

18. `nakama aws create <n> --bare` は CloudFront distribution / VPC Origin / パスワードを作らずに完了し、`list` で TYPE=BARE と表示される。`code <n>` は `ssh setup` / `shell` の案内を伴うエラーになる。`shell` / `ssh setup` / `tunnel` / `backup` / `update` (OS のみ) が動作し、`delete <n>` が CloudFront 関連の削除を試みずに完了する (手動 E2E)

19. 付録 A の権限マトリクスは `internal/service/permissions.go` を唯一の情報源とし、実装との乖離が双方向のテストで検出される: 実装が呼ぶ AWS API のアクションが表に無ければ失敗し、表にあるが実装から抽出されず暗黙宣言にも無いアクションも失敗する。docs の表は生成ブロックであり、コードと不一致なら CI が失敗する

20. `nakama aws create` の user-data に Kiro CLI / Kiro Crew / gateway サービスの導入手順が含まれ、`--bare` でも同じ内容が入る。`kirocrew service install` の前に `~/.kiro` の group/world 書き込み権限を落とす (AppArmor profile が group-writable なディレクトリを拒否するため)。Kiro 導入の失敗で user-data 全体が失敗せず、警告と `provision --preset kiro` の案内が出る。`--no-kiro` を付けると user-data に Kiro の記述が一切残らない。導入手順は `internal/kiro/install.sh` を唯一の情報源とし、`--preset kiro` も同じスクリプトを使う (Issue #68)。導入が終わると gateway のサービスが登録され、127.0.0.1:5476 で応答する。導入スクリプトはスタブを相手に実際に実行し、Kiro Crew 未導入・導入済みのどちらでも service install まで進むことをテストで確かめる (Issue #79)

21. `nakama aws cloudfront add <n> --kirocrew` (旧 `kirocrew enable <n>`) を実行すると、Kiro Crew 専用の CloudFront distribution (オリジンのカスタムヘッダー `X-Forwarded-Host: 15476.kirocrew.nakama.internal`、既存の VPC Origin と WAF を共用)、インスタンス上の nginx (127.0.0.1:15476 だけで待ち受け、Origin を検査し、Host と Origin を書き換え、X-Forwarded-Proto を付与し、X-Forwarded-For は残す)、code-server の管理ブロック (proxy-domain と、crew だけの trusted-origins)、`VSCODE_PROXY_URI` を既定のパス方式 (`./proxy/{{port}}/`) に固定する systemd drop-in、既存の distribution の `/proxy/5476*` と `/absproxy/5476*` を crew へ 302 で転送する設定 (既定の behavior の全項目を写したもの) が用意される。何度実行しても結果は同じで、途中で失敗しても再実行で続きから進み、distribution が重複しない。既存の distribution の構成、`code` の動作、Ports パネルとターミナルのリンクの書き換え先 (`/proxy/<port>/`) は変わらない。`nakama aws kirocrew <n>` は、code-server のパスワード入力なしで Kiro Crew の画面を開く (トークンは暗号化して受け取り、画面にもログにも出さない)。`cloudfront delete <n> --kirocrew` (旧 `kirocrew disable <n>`) と `delete <n>` は、Kiro Crew 用 distribution を VPC Origin より先に消し、関連するものをすべて取り除く (手動 E2E)

22. (手動 E2E) Windows で nakama.exe をエクスプローラーからダブルクリックすると、Windows PowerShell のウィンドウが開き、nakama.exe のフォルダーが PATH の先頭にあることと、プロキシ・CA・疎通確認の結果が表示される。そのウィンドウでは、どのフォルダーからでも `nakama version` が動き、検出に成功していれば `aws s3 ls` も追加設定なしで動く。オフラインでもウィンドウは開き、`nakama` を打てる。`exit` でウィンドウが閉じ、レジストリ・ユーザー環境変数に変更は残らない。同時にブラウザで管理画面が開き (ADR 0031)、ウィンドウを閉じると管理画面も止まる。シェルの中で Ctrl-C を押しても nakama は終わらない (自動テストでは Ctrl-Break を受けた `runShell` の生存を確認する)

23. `nakama aws init` で作った NAT インスタンス (t4g.nano、最新の AL2023 AMI) では、初回起動の設定が OOM なしで完了する。`dnf --releasever=latest upgrade` が適用され、`iptables-nft` だけが入り、MASQUERADE のルールが入る。init は最後に NAT インスタンスの転送を確かめる。転送が壊れた NAT インスタンス (`iptables` が無い、`nakama-nat.service` が失敗) では、init が修理して `nat forwarding: repaired` と表示する。直らなければ init は失敗する。停止中の NAT インスタンスは、確認の後に停止に戻る。`shell` / `ssh` が `TargetNotConnected` で失敗したときは、NAT インスタンスと init を案内する (Issue #87)

24. `nakama aws delete <n>` で削除したインスタンスに対し、`nakama aws restore <n> --yes` を実行すると同じ名前のインスタンスができる。start するとバックアップ時点のファイルがあり、新しいパスワードでログインでき (`--cloudfront` を付けたときは新しい URL でも)、SSH のホスト鍵の指紋は削除前と同じ。bare のバックアップからは bare のインスタンスができる。アーキの合わないタイプの指定とタグの無いバックアップでの `--instance-type` の省略は、何も作らずにエラーになる (Issue #96)

25. `nakama aws acl delete <IP1> <IP2>` で両方が 1 回の更新で消え、片方が未登録なら何も消えない。`nakama aws acl delete all --keep-mine` は確認の後、接続元 IP を含むエントリだけを残し、ブラウザからのアクセスは通り続ける。`nakama aws acl delete all` は確認の後に IP set を空にし、ブラウザからのアクセスは 403 になる。どちらも消したエントリの追加日時が SSM から消える (Issue #99)

26. `nakama aws create` で作ったインスタンス (code-server 版・bare 版、x86_64・arm64) では、AWS CLI v2 が AWS 公式のインストールスクリプトで ubuntu ユーザーの `~/.local/aws-cli` に入り (`~/.local/bin/aws`)、CPU に合った版で、権限の昇格を禁じた状態 (`setpriv --no-new-privs`) でも動く。snap の `aws-cli` と `/usr/local/bin/aws` は無い。`nakama aws update <n>` は AWS CLI を更新し、Kiro Crew があれば `kirocrew update` と `kirocrew restart` を実行して、結果に AWS CLI の版と Kiro Crew の結果を出す。Issue #101 より前に作ったインスタンスの snap の AWS CLI には触れない。update の出力が長くても再起動の要否が失われない (Issue #101)

27. 引数なしの `nakama aws acl add` は、`https://checkip.amazonaws.com/` と `https://ifconfig.io/ip` に 3 回ずつ、毎回新しい接続で問い合わせる (チェックサイトへの直接の接続は IPv4)。見つかった公開の IPv4 アドレスを重複なしで、1 回の更新ですべて追加する。IPv6・プライベートアドレス・IP でない答えは使わない。一部の問い合わせが失敗したら警告して見つかったものを追加し、全部失敗したら何も追加せずにエラーになる。追加が 1 つなら出力は従来と同じ。`acl delete all --keep-mine` は同じ方法で調べ、見つかったどれかを含むエントリを残す (Issue #104)

28. `nakama version --check` は GitHub の最新リリース (draft と prerelease を除く) と比べて、今の版・最新の版・リリースページの URL を表示する。版は数値で比べ (v0.9.0 < v0.10.0、v0.24.0 と 0.24.0 は同じ、プレリリースはその版より前)、開発ビルド (`dev` やコミットのハッシュなど semver でないもの、`make build` の `git describe` の形) は比較しない。問い合わせに失敗しても (遮断、レート制限、5 秒のタイムアウト、壊れた応答) 警告だけで、古い場合も含めて終了コードは 0。引数なしの `nakama version` とほかのコマンドは GitHub に問い合わせず、出力も変わらない。`nakama aws doctor` の `version` 行は、最新 (または手元の方が新しい) なら ok、古ければ warn (新しい版と入手先)、確認できなければ warn、開発ビルドなら skip で、どれも doctor の終了コードを変えない。テストは本物の api.github.com に問い合わせない

29. `nakama aws acl prune --older-than 30d` は、`acl add` で最後に追加・確認してから 30 日を超えたエントリだけを、確認の後に 1 回の更新で消し、その追加日時を SSM から消す。現在の接続元 IP を含むエントリと追加日時の無いエントリは残り (後者は警告が出る)、ブラウザからのアクセスは通り続ける。古いエントリが無ければ IP set は更新されない。`acl list` の AGE は `acl add` からの経過日数を示す

30. `nakama aws create` が途中で失敗しても Ctrl-C で中断しても、それまでに作った SG・ENI・EC2・VPC Origin・distribution は、作った直後 (待機に入る前) に SSM に記録されている。作成の API が返った直後の Ctrl-C でも記録は残る (restore の作り直しも同じ)。そこから `nakama aws delete <n>` を実行すると記録されたものがすべて消え、同じ名前で作り直せる。成功したときに残る記録は、code-server 版・bare 版とも変わらない。フェイクを相手に、ENI の作成・`RunInstances`・`running` の待機と、`--cloudfront` のときの private DNS の解決・VPC Origin の `Deployed` 待ち・distribution の作成・distribution の `Deployed` 待ちのそれぞれで失敗とキャンセルを、SG・ENI の作成と `RunInstances` が返った直後にキャンセルを起こし、テストで確かめる

31. SG・ENI・EC2 を作った直後の記録の書き込みが失敗しても、`nakama aws delete <n>` はそれらをその名前の nakama のタグで探して消す。SSM に ID があるものはタグで探さない。別の名前のもの (`dev01` に対する `dev010`・`dev0` のような前方一致を含む)、`nakama:managed` タグの無いもの、terminated・shutting-down の EC2 は消さない。同じ名前のものが複数あればすべて消し、見つけて消した ID をログに出す。タグでの検索が権限で拒否されたら、探せなかったことと足りない権限名を警告して続け、記録されたものを消す。ほかのエラーでは何も消さずに止まる。フェイクを相手に、SG・ENI の作成直後と `RunInstances` の直後 (create と restore) で記録の書き込みを失敗させ、フィルタを無視する API も相手にして、テストで確かめる (設計 §8.27)

32. 外向き通信の方式 (ADR 0026、設計 §8.28): 新しい環境で `nakama aws init` を実行すると、public subnet に NAT Gateway (EIP 付き) ができ、private の `0.0.0.0/0` がそれを向き、NAT インスタンスは作られない (料金の注記が出る)。記録の無い既存環境 (この版より前の init で作った環境) で `nakama aws init` を実行しても、NAT インスタンスのまま何も変わらない。`init --nat gateway` は確認を取り (稼働中のインスタンス名とセッションが切れることを表示。`--yes` で省略)、NAT Gateway が `available` になってから経路を張り替え、その後に NAT インスタンスを terminate して SG を削除する (IAM ロールは残る)。`init --nat instance` は NAT インスタンスを作って転送を確かめてから経路を張り替え、NAT Gateway を削除して EIP を解放する。どちらも、経路を張り替える前に失敗したら古い NAT が残り、init の再実行で完了する。同じ方式の init をもう一度実行しても何も変わらない。gateway の環境では、`create` / `start` / `update` / `restore` が NAT Gateway が `available` かを確かめ (使えなければ init を案内してエラー)、`stop` / `delete` などは NAT に触れない。`list --show-nat` は NAT Gateway の行と外向きの IP を、`doctor` の `egress` 行は方式・経路・NAT の状態を表示する。フェイクを相手に、両方向の切り替えの順序、NAT Gateway の作成の失敗、`failed` の NAT Gateway の作り直し、転送しない NAT インスタンスへの切り替えの失敗、2 回目の init を、テストで確かめる

33. ブラウザからの接続 (ADR 0027、設計 §8.29): `nakama aws code <n>` は、インスタンスの tunnel デーモンに code-server への転送を足し (デーモンが無ければ起動し、同じ先への転送があれば使い回す)、ルーターの URL (ADR 0029、受け入れ基準 35。`--local-port` では `http://localhost:<port>`) が応答してからブラウザを開いてパスワードログインを自動で通す。`nakama aws kirocrew <n>` は Kiro Crew (インスタンスの 127.0.0.1:5476) への転送で、同じくルーターの URL を開き、トークンでサインインさせる (CloudFront は不要、bare でも動く)。どちらも WAF の許可リストに依らず、`--internet` を付けたときだけ CloudFront を使う。`--local-port` のポートが使用中ならエラーにし、nakama の別の転送ならそのインスタンス名を示す。ブラウザを起動できなければ、URL を表示せずにエラーにする。転送は、サーバー側の終了 (exit 0)、通信エラー、スロットリング、5xx、TargetNotConnected のあとも、回数の上限なくローカルポートを開いたまま張り直す。資格情報のエラーは 60 秒ごとに再試行し、`nakama aws login` で戻る。応答の返らないセッションは、ブラウザの接続があるあいだ HTTP の確認で検知して張り替える。5 分使われていない転送は張り直さず、次の接続で張る。code-server 4.107.0 以降のインスタンスでは、create と update が `cookie-suffix` にインスタンス名を設定する。`nakama aws delete` はこの機械の同名の tunnel デーモンを止める。plugin の待ち受けを模した本物のソケットを相手に、再接続・待ち・検知・idle・終了の条件を、テストで確かめる

34. CloudFront はインスタンスごとの任意 (ADR 0028、設計 §8.30): `nakama aws create` は既定で CloudFront を作らず (VPC Origin・distribution なし)、SG に受信の規則が 1 つも無い。WAF の web ACL も prefix list も引かない。それでも `nakama aws code <n>` は SSM で開ける。`create --cloudfront` は従来どおり CloudFront の経路を作り、`--bare` とは同時に指定できない。`nakama aws cloudfront add <n>` は running の code-server インスタンスに SG の規則 (CloudFront managed prefix list から 8080/tcp)、VPC Origin、共有の WAF を付けた distribution を作って `Deployed` を待ち、URL を表示する。各段階で失敗しても Ctrl-C で中断しても、再実行で続きから進み、VPC Origin と distribution は 1 つずつしかできない。もう一度実行しても何も作らない。bare・停止中・未初期化・記録の無いインスタンスは何も作らずにエラーになる。`nakama aws cloudfront delete <n>` は確認の後 (`--yes` で省略)、SG の規則を最初に外し、Kiro Crew の distribution (あれば)、code-server の distribution、Function、VPC Origin、(running なら) インスタンス上の Kiro Crew 用の設定、記録の順に片付ける。インスタンスと SSM の経路はそのまま使える。途中で止まっても再実行で完了し、その間 `cloudfront add` は片付けを先に求める。disable の反映中の distribution は反映を待ってから消す。CloudFront の無いインスタンスでは残っている SG の規則を外すだけで、もう一度実行しても何も変わらない。`code --internet` は CloudFront が無ければ `cloudfront add` を、`kirocrew --internet` は Kiro Crew の distribution が無ければ `cloudfront add --kirocrew` を案内する。`restore` の作り直しは `--cloudfront` のときだけ CloudFront を作り、CloudFront のあったインスタンスのバックアップでフラグが無ければ警告する。`doctor` の waf-allowlist は、CloudFront を持つインスタンスが無ければ skip になる。既存のインスタンスの CloudFront は自動では消えない。フェイクを相手に順序・再開・前提の拒否を、一時インスタンスを相手に add・2 回目の add・Kiro Crew の分の add・delete・中断した delete の再開を、確かめる

35. ブラウザ用の転送のルーター (ADR 0029、設計 §8.31): `nakama aws code <n>` と `nakama aws kirocrew <n>` は、`--local-port` が無ければ手元のルーター (127.0.0.1 と ::1 の 18888。`NAKAMA_ROUTER_PORT` で変更) を起動し (動いていれば使い、別の版や別のポートのものは止めて起動し直す)、転送のローカルポートに空きポートを使って、`http://code-server.<n>.nakama.localhost:18888` と `http://kirocrew.<n>.nakama.localhost:18888` を開く。2 つのインスタンスを同時に開いても、ポートを選ぶ必要は無く、サインインと画面の設定はインスタンスごとに残る。手元で Kiro Crew が 5476 を使っていても開ける。ルーターは code-server に Host をそのまま渡し、Kiro Crew には Host を `localhost:5476` に、ルーター自身の Origin と Referer を `http://localhost:5476` に置き換え、`Location` を戻し、転送用のヘッダーを渡さない (Kiro Crew は設定の変更と秘密情報の表示を許す)。知らない Host には 421 を返す。転送の無いインスタンスには、開くためのコマンドを示すページを返す。ルーターのポートが使用中なら、code / kirocrew はエラーにして `NAKAMA_ROUTER_PORT` を案内する。`--local-port <port>` ではルーターを使わず `http://localhost:<port>` を開く。`tunnel status` はルーターの状態と転送の URL を表示する。ルーターは tunnel のデーモンも管理画面 (受け入れ基準 37) も無い状態が 1 分続くと終わる。本物のループバックのソケットとフェイクを相手に、書き換え・WebSocket・遊休の接続・エラーのページ・起動と置き換え・終了をテストで確かめ、一時インスタンスを相手に code と kirocrew をルーター経由で確かめる

36. コマンドとフラグの名前の整理 (ADR 0030、設計 §8.32): `nakama aws code <n>`・`shell <n> [cmd...]`・`provision <n>`・`cloudfront add <n> --kirocrew`・`cloudfront delete <n> --kirocrew`・`create <n>`・`login --alias` / `profile save --alias` が、それぞれ古い `open <n>`・`connect <n> [cmd...]`・`setup rerun <n>`・`kirocrew enable <n>`・`kirocrew disable <n>`・`create --name <n>`・`--name` と同じ処理をする。古い名前は v0.30.x の間は同じ引数とフラグで動き、ヘルプと補完には出ず、実行すると新しい名前を stderr に出す (stdout と `-o json` は変わらない)。`connect` も `<name>` の後をすべてリモートのコマンドとして送る。`create` の位置引数と `--name`、`--alias` と `--name` を違う値で同時に指定すると、AWS を呼ぶ前 (login ではブラウザを開く前) にエラーになる。`shell -t <n> top` は `--tty` と同じ。`cloudfront delete <n> --kirocrew` と `backup delete <n> --label <l>` は消すものを示して確認を取り、`--yes` で省略でき、標準入力が無ければ何も消さない。`doctor` の項目名は `code`・`shell`・`cloudfront --kirocrew`・`update / provision` になる。フラグの説明はすべて大文字 (または sshd のようなプログラム名) で始まる。これらをテストで確かめ、一時インスタンスを相手に新しい名前と古い名前を確かめる

37. ブラウザの管理画面の入口とセッション (ADR 0031、設計 §8.33): `nakama ui` (と、端末で引数なしの `nakama`) は、ルーターを起動し (受け入れ基準 35 と同じく、動いていれば使い、別の版や別のポートのものは置き換える)、自分のプロセスで管理画面を動かし、`http://control.nakama.localhost:<ルーターのポート>/session?c=<コード>` をブラウザで開く。ブラウザはそのコードで一度だけサインインでき、`/` へ移る。同じコードは二度と使えず、60 秒たったコードも使えない。ブラウザを起動できなければ、アドレスを表示せずに `--browser` を案内するエラーにする。管理画面が動いているときの `nakama ui` は、新しいコードでブラウザを開いて終わる (別の版の管理画面なら止めてから起動する)。ウィンドウを閉じるか Ctrl-C で管理画面は止まり、`panel.json` を消す。ルーターは管理画面が応答するあいだ終わらず、管理画面が無いときの `control.nakama.localhost` には `nakama ui` を示すページを返す。ルーターは Host をそのまま渡し、転送用のヘッダーを渡さない。セッションの無い要求、別の Host (421)、ほかの OS のユーザーの接続 (Linux。403)、埋め込み、`Sec-Fetch-Site` が same-origin でない API の要求、Origin・`Sec-Fetch-Site`・CSRF トークンのどれかが合わない状態の変更 (ログアウト) は拒まれる。セッションは 30 分使わないか、サインインから 12 時間で終わる。すべての応答に CSP などの安全のためのヘッダーが付く。画面は Accept-Language で日本語か英語になる。`nakama ui` はヘルプに出る。端末でないときの引数なしの `nakama` はヘルプを出し、知らない引数はエラーのまま。Windows で nakama.exe をダブルクリックすると、PowerShell のウィンドウと同時に管理画面がブラウザに開き、PowerShell の Ctrl-C では止まらず、ウィンドウを閉じると止まる。これらをテストで確かめ、headless の Chrome で、サインイン・ログアウト・使用済みのコード・2 回目の `nakama ui`・停止・ルーターの終了を確かめる

38. 管理画面の操作 (ADR 0031、設計 §8.33): AWS の資格情報が無ければ、一覧の代わりにログインのボタンと保存したアカウントが出る。ログインのボタンで AWS のサインインのページへ移り (`nakama aws login` と同じクライアント ID とコールバック)、サインインのあと管理画面へ戻り、プロファイルが保存・有効化されて、アカウントがヘッダーに出る。保存したアカウントの切り替え・削除・既定への戻し・保存は、`nakama aws profile` と同じ結果になる。一覧・詳細・バックアップ・許可リスト・ポート転送・診断・版の画面は、対応するコマンドと同じものを示す。パスワードは「パスワードを表示」を押したときだけ出る。code / kirocrew のボタンは新しいタブで開き、進み具合を示してから、`nakama aws code` / `kirocrew` と同じ転送とサインインで code-server / Kiro Crew に入る (`--internet` と `--no-auth` も選べる)。状態を変える操作は、フォームの値を `--flag=value` (名前とアドレスは検査して位置引数) にした `nakama aws … -o json` を、ウィンドウから切り離したプロセスで実行し、`~/.config/nakama/ops/<id>/` に結果とログと終了コードを残す。管理画面を閉じても操作は続き、開き直すと状態が分かる。停止や料金を伴う操作はコマンドを示して確認を取り、元に戻せない操作 (delete、restore、backup delete、acl delete all、acl prune) は名前の入力を求める。同じインスタンスの操作と、init とほかの操作は重ならない。中止は Ctrl-C と同じで、記録は中止になる。7 日を過ぎた記録は消える。これらを偽の Backend と、実行器を自分のプロセスで動かすテストで確かめ、一時的なインスタンスを相手に、管理画面から create・code・stop・start・backup create・note・restore・delete・backup delete と、既存のインスタンスの code / kirocrew を確かめる

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
| init | nakama aws init | `ec2:AllocateAddress`, `ec2:AssociateRouteTable`, `ec2:AttachInternetGateway`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateInternetGateway`, `ec2:CreateNatGateway`, `ec2:CreateRoute`, `ec2:CreateRouteTable`, `ec2:CreateSecurityGroup`, `ec2:CreateSubnet`, `ec2:CreateTags`, `ec2:CreateVpc`, `ec2:DeleteNatGateway`, `ec2:DeleteSecurityGroup`, `ec2:DescribeAddresses`, `ec2:DescribeAvailabilityZones`, `ec2:DescribeInstances`, `ec2:DescribeInternetGateways`, `ec2:DescribeNatGateways`, `ec2:DescribeRouteTables`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSubnets`, `ec2:DescribeVpcs`, `ec2:ModifyInstanceAttribute`, `ec2:ModifySubnetAttribute`, `ec2:ModifyVpcAttribute`, `ec2:ReleaseAddress`, `ec2:ReplaceRoute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `ec2:TerminateInstances`, `iam:AddRoleToInstanceProfile`, `iam:AttachRolePolicy`, `iam:CreateInstanceProfile`, `iam:CreateRole`, `iam:GetInstanceProfile`, `iam:GetRole`, `iam:PassRole`, `iam:PutRolePolicy`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:PutParameter`, `ssm:SendCommand`, `wafv2:CreateIPSet`, `wafv2:CreateWebACL`, `wafv2:ListIPSets`, `wafv2:ListWebACLs` | 冪等。--nat (ADR 0026) の方式に合わせて NAT インスタンスか NAT Gateway (と EIP) を用意し、private の経路をそちらへ向けてから、もう一方を片付ける (NAT Gateway の削除と EIP の解放、または NAT インスタンスの terminate と SG の削除)。NAT インスタンスの方式では、NAT インスタンスの転送を Run Command で確認し、壊れていれば直す (停止中なら確認のために起動して戻す) |
| acl | nakama aws acl add / list / delete / prune | `ssm:DeleteParameter`, `ssm:GetParametersByPath`, `ssm:PutParameter`, `wafv2:GetIPSet`, `wafv2:ListIPSets`, `wafv2:UpdateIPSet` |  |
| create | nakama aws create | `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateNetworkInterface`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DescribeInstanceTypes`, `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ec2:DescribeNetworkInterfaces`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSubnets`, `ec2:DescribeVpcs`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:PutParameter`, `ssm:SendCommand` | CloudFront は作らない (--cloudfront のときは cloudfront の行の権限も要る)。NAT インスタンスの初回作成を含む。NAT Gateway の方式 (ADR 0026) では ec2:DescribeNatGateways で NAT Gateway が使えるかを確かめる |
| create --with-admin / --with-role | nakama aws create / restore の --with-admin / --with-role | `iam:AddRoleToInstanceProfile`, `iam:AttachRolePolicy`, `iam:CreateInstanceProfile`, `iam:CreateRole`, `iam:GetInstanceProfile`, `iam:GetRole`, `iam:ListAttachedRolePolicies`, `iam:ListInstanceProfilesForRole`, `iam:SimulatePrincipalPolicy` | create / restore 本体の権限に追加で必要 (ADR 0010 の事前検証を含む) |
| list / info | nakama aws list / info | `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ssm:GetParameter` | ec2:DescribeNatGateways は list --show-nat が NAT Gateway を表示するときだけ使い、拒否されたら警告して続ける |
| code | nakama aws code | `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ssm:GetParameter`, `ssm:GetParametersByPath`, `ssm:ResumeSession`, `ssm:StartSession`, `ssm:TerminateSession`, `wafv2:GetIPSet`, `wafv2:ListIPSets` | 既定の SSM 経由ではポート転送に ssm:StartSession などを使い、tunnel デーモンが再接続のたびに instance ID を読み直す。wafv2:ListIPSets / wafv2:GetIPSet と ssm:GetParametersByPath は --internet のときに許可リスト未登録の警告表示のためだけに使い、読めない場合は警告を出さずに続行する。ec2:DescribeNatGateways は TargetNotConnected の案内で NAT の方式を調べるときだけ使う |
| kirocrew | nakama aws kirocrew | `cloudfront:GetDistribution`, `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:ResumeSession`, `ssm:SendCommand`, `ssm:StartSession`, `ssm:TerminateSession` | トークンは RSA-OAEP で暗号化して Run Command で受け取る (ADR 0020)。既定の SSM 経由では Kiro Crew へポート転送する (ADR 0027)。cloudfront:GetDistribution と code-server のパスワード読み取りは --internet のときだけ使う |
| cloudfront | nakama aws cloudfront add / delete、create / restore の --cloudfront | `cloudfront:CreateDistribution`, `cloudfront:CreateVpcOrigin`, `cloudfront:DeleteDistribution`, `cloudfront:DeleteFunction`, `cloudfront:DeleteVpcOrigin`, `cloudfront:DescribeFunction`, `cloudfront:GetDistribution`, `cloudfront:GetDistributionConfig`, `cloudfront:GetVpcOrigin`, `cloudfront:TagResource`, `cloudfront:UpdateDistribution`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:DescribeInstances`, `ec2:DescribeManagedPrefixLists`, `ec2:RevokeSecurityGroupIngress`, `ssm:DeleteParameters`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:PutParameter`, `ssm:SendCommand`, `wafv2:ListWebACLs` | add は running のインスタンスに SG の受信ルール・VPC origin・distribution を作って Deployed まで待つ (ADR 0028)。delete は SG の受信ルールを最初に外し、Kiro Crew の distribution と CloudFront Function があればそれも消す (cloudfront:*Function と Run Command はそのときだけ)。create / restore の --cloudfront は、本体の権限に加えて add の分を使う |
| cloudfront --kirocrew | nakama aws cloudfront add / delete --kirocrew | `cloudfront:CreateDistribution`, `cloudfront:CreateFunction`, `cloudfront:DeleteDistribution`, `cloudfront:DeleteFunction`, `cloudfront:DescribeFunction`, `cloudfront:GetDistribution`, `cloudfront:GetDistributionConfig`, `cloudfront:PublishFunction`, `cloudfront:TagResource`, `cloudfront:UpdateDistribution`, `cloudfront:UpdateFunction`, `ec2:DescribeInstances`, `ssm:DeleteParameters`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:PutParameter`, `ssm:SendCommand` | Kiro Crew 専用の distribution と CloudFront Function を作成・削除する。add --kirocrew は code-server の distribution が無ければ cloudfront の add の分も使う |
| shell | nakama aws shell | `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ssm:GetParameter`, `ssm:ResumeSession`, `ssm:StartSession`, `ssm:TerminateSession` | ec2:DescribeNatGateways は TargetNotConnected の案内で NAT の方式を調べるときだけ使い、失敗しても案内の内容が一般的になるだけ |
| ssh | nakama aws ssh setup / proxy | `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:ResumeSession`, `ssm:SendCommand`, `ssm:StartSession`, `ssm:TerminateSession` | setup の公開鍵投入に SendCommand を使う。ec2:DescribeNatGateways は TargetNotConnected の 案内で NAT の方式を調べるときだけ使う |
| tunnel | nakama aws tunnel | `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:ResumeSession`, `ssm:SendCommand`, `ssm:StartSession`, `ssm:TerminateSession` | suggest の listen ポート調査に SendCommand を使う。status は資格情報不要。ec2:DescribeNatGateways は TargetNotConnected の案内で NAT の方式を調べるときだけ使う |
| start / stop | nakama aws start / stop | `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ec2:DescribeSecurityGroups`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetParameter` | NAT インスタンスの電源連動 (初回は作成) を含む。NAT Gateway の方式 (ADR 0026) では、start が ec2:DescribeNatGateways で NAT Gateway が使えるかを確かめるだけ |
| edit | nakama aws edit | `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DescribeInstanceTypes`, `ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`, `ec2:DescribeVolumes`, `ec2:ModifyInstanceAttribute`, `ec2:ModifyVolume`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:SendCommand` |  |
| update / provision | nakama aws update / provision | `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateSnapshot`, `ec2:CreateTags`, `ec2:DeleteSnapshot`, `ec2:DeleteTags`, `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSnapshots`, `ec2:DescribeVolumes`, `ec2:ModifyInstanceAttribute`, `ec2:RebootInstances`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetCommandInvocation`, `ssm:GetParameter`, `ssm:SendCommand` | update は更新前の自動バックアップを取得する |
| backup | nakama aws backup create / list / note / delete | `ec2:CreateSnapshot`, `ec2:CreateTags`, `ec2:DeleteSnapshot`, `ec2:DeleteTags`, `ec2:DescribeInstances`, `ec2:DescribeSnapshots`, `ec2:DescribeVolumes`, `ssm:GetParameter` |  |
| restore | nakama aws restore | `ec2:AttachVolume`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateNetworkInterface`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:CreateVolume`, `ec2:DeleteVolume`, `ec2:DescribeInstanceTypes`, `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ec2:DescribeNetworkInterfaces`, `ec2:DescribeSecurityGroups`, `ec2:DescribeSnapshots`, `ec2:DescribeSubnets`, `ec2:DescribeVolumes`, `ec2:DescribeVpcs`, `ec2:DetachVolume`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `iam:PassRole`, `ssm:GetParameter`, `ssm:PutParameter` | インスタンスが削除済みのときは、同じ名前のインスタンスを作り直すので create と同じ権限が要る (--cloudfront のときは cloudfront の行の権限も要る) |
| delete | nakama aws delete | `cloudfront:DeleteDistribution`, `cloudfront:DeleteFunction`, `cloudfront:DeleteVpcOrigin`, `cloudfront:DescribeFunction`, `cloudfront:GetDistribution`, `cloudfront:GetDistributionConfig`, `cloudfront:GetVpcOrigin`, `cloudfront:UpdateDistribution`, `ec2:AuthorizeSecurityGroupIngress`, `ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:DeleteNetworkInterface`, `ec2:DeleteSecurityGroup`, `ec2:DeleteVolume`, `ec2:DescribeInstances`, `ec2:DescribeNetworkInterfaces`, `ec2:DescribeSecurityGroups`, `ec2:DescribeVolumes`, `ec2:ModifyInstanceAttribute`, `ec2:RunInstances`, `ec2:StartInstances`, `ec2:StopInstances`, `ec2:TerminateInstances`, `iam:PassRole`, `ssm:DeleteParameters`, `ssm:GetParameter`, `ssm:GetParametersByPath` | CloudFront の無いインスタンスでは cloudfront:* を使わない。cloudfront:*Function は cloudfront add --kirocrew 済みのインスタンスでのみ使う。ec2:DescribeNetworkInterfaces は SSM に ENI の記録が無いときだけ使い、拒否されたら警告して続ける |
| doctor | nakama aws doctor | `ec2:DescribeInstances`, `ec2:DescribeNatGateways`, `ec2:DescribeRouteTables`, `iam:SimulatePrincipalPolicy`, `ssm:GetParameter`, `ssm:GetParametersByPath`, `wafv2:GetIPSet`, `wafv2:ListIPSets` | 読み取り専用。iam:SimulatePrincipalPolicy が無い場合も capability 判定が warn になるだけで他の診断は動く。egress の確認に ec2:Describe* と ssm:GetParameter を使う。waf-allowlist は CloudFront を持つインスタンスが無ければ skip にする |
<!-- END GENERATED: permissions-matrix -->

## 9. リスク・オープン事項

| ID | 内容 | 対応方針 |
|---|---|---|
| R1 | EC2 の Private IP が stop/start 後に維持されるか保証なし | ENI を専用作成しアタッチすることで固定 |
| R2 | VPC Origin の deploy 待機が最大 15 分 | UI で進捗を slog 出力。タイムアウトは 20 分 |
| R3 | code-server の Microsoft Marketplace 不可は仕様 | README / FAQ に明記 |
| R4 | `apne1-az3` の AZ ID 解決はアカウント依存 | `ec2:DescribeAvailabilityZones` で AZ ID を取得し除外 |
| R5 | NAT のegressコスト | NAT Gateway を廃し NAT インスタンス (`t4g.nano`) を採用 (ADR 0007)。電源を code-server のライフサイクルに連動させ、全停止時は NAT インスタンスも停止して固定費をゼロに近づける。EIP は付与せず停止中の public IPv4 課金も回避。ADR 0026 で NAT Gateway も選べるようになり (新しい環境の既定)、こちらは全停止時も時間料金と EIP の料金がかかる。料金を優先する利用者は `init --nat instance` を選ぶ |
| R6 | 稼働中スナップショットはクラッシュコンシステント | 重要時は `stop` 後に `backup create` する旨を明記。`restore` はインプレースで破壊的なため確認を要求 (`--yes` で省略)。削除済みインスタンスの復元も確認を要求し、URL とパスワードが変わることを示す |
| R7 | `--keep` 未指定スナップショットのコスト累積 | `--keep N` の利用を推奨。CLI は常駐しないため prune は `backup create` 実行時のみ |
| R8 | 取り込んだ `session-manager-plugin` の upstream 追随は手動 | 取り込み元 commit/tag を `third_party/session-manager-plugin/PINNED.md` に記録し、`scripts/vendor-ssm-plugin.sh` で定期的に re-sync |
| R9 | `shell` (と `ssh`・`tunnel`・SSM 経由の `code`・`kirocrew`) には呼び出し側プリンシパルに `ssm:StartSession` 等が必要 (instance role とは別) | 前提条件 §3 に明記。`init` では付与しない (利用者責務) |
| R10 | plugin 取り込みによるバイナリサイズ増 | PoC で実測し許容範囲を確認 |
| R11 | Session Manager の独自プロトコル変更・コマンド終了コード伝播の制約 | 取り込み版を pin。終了コードの非互換は制限事項として扱う |
| R12 | NAT インスタンスの攻撃面 (自前管理の OS、VPC 内からの到達、全外向き通信の経路) | ADR 0026 で NAT Gateway を選べるようにし、新しい環境の既定にした。NAT インスタンスを選んだ環境の強化 (SG の受信を絞る、定期的な更新) は別の Issue で扱う |
| R13 | CloudFront の経路 (distribution、VPC Origin、SG の CloudFront の許可) が、SSM を既定にしたあとも残る | ADR 0028 で、新しいインスタンスは CloudFront を持たず SG に受信の規則も無くした。既存のインスタンスは `cloudfront delete` で外す (自動では外さない)。code-server の `0.0.0.0:8080` の待ち受けは残る (SG に規則が無ければ VPC 内からも届かない) |
| R14 | SSM の転送の速度 (セッション単位で約 750 KB/s) と、Session Manager 側の切断 (アイドルタイムアウト、WebSocket の作り直し) | 転送はデーモンが張り直し、ローカルポートは閉じない (ADR 0027)。速度が足りなければ、転送 1 本に複数のセッションを張る案を別に検討する |
