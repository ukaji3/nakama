# nakama

> MIT License. See [LICENSE](LICENSE).

`nakama` は、ブラウザだけで使える VS Code ([coder/code-server](https://github.com/coder/code-server)) を AWS 上に数コマンドで構築・管理する CLI ツールです。ローカル開発で Node.js などがメモリ不足になるケースを想定し、開発環境を AWS にオフロードします。

```bash
nakama aws login          # ブラウザの AWS コンソールセッションでサインイン (aws CLI 不要)
nakama aws init           # 基盤 (VPC / NAT / WAF) を作成 (冪等)
nakama aws acl add        # 自分のグローバル IP を許可リストに追加
nakama aws create --name dev01 --instance-type t3.medium
nakama aws open dev01     # ブラウザが開き、パスワードログインも自動通過
```

## 特徴

- **ブラウザだけで開発**: EC2 上の code-server に CloudFront 経由の HTTPS/WSS でアクセス。ローカルには何もインストール不要 (nakama 本体を除く)
- **止めればコストほぼゼロ**: egress は NAT Gateway ではなく小型 NAT インスタンス (`t4g.nano`) を使い、その電源は code-server の稼働に自動連動。全停止時は固定費がほぼ発生しません ([アーキテクチャとコスト](#アーキテクチャとコスト))
- **aws CLI 不要**: サインイン (`login`) は OAuth 2.0 + PKCE + DPoP のネイティブ実装 (ADR 0016)。複数 AWS アカウントの切り替え (`profile`) も内蔵
- **SSH 不要・受信ポート開放なし**: シェルログイン (`connect`) もポート転送 (`tunnel`) も SSM Session Manager 経由。`session-manager-plugin` の追加インストールも不要
- **企業プロキシにそのまま対応**: 環境変数 → Windows の WinHTTP/PAC の順で自動解決し、TLS 検証は OS 証明書ストアを使用 (ADR 0015)。aws CLI 本体を使いたい場合も `nakama aws cli shell` で設定済みサブシェルを提供 (ADR 0018)
- **安全側の既定**: アクセスは WAF の IP 許可リスト制、パスワードは SSM SecureString 管理でディスクに残さず、`~/.aws/config` には一切手を触れません
- **運用コマンド一式**: バックアップ/復元 (`backup` / `restore`)、OS と code-server の一括更新 (`update`)、インスタンスタイプ/EBS の変更 (`edit`)

## インストール

### バイナリ (Linux / macOS / Windows)

[Releases](https://github.com/ukaji3/nakama/releases) から OS / アーキテクチャに合うアーカイブを取得し、`PATH` の通った場所に置きます。

| OS | Arch | 形式 |
|---|---|---|
| Linux | `amd64`, `arm64` | `tar.gz` |
| macOS | `amd64`, `arm64`, `universal` (Intel + Apple Silicon の単一バイナリ) | `tar.gz` |
| Windows | `amd64`, `arm64` | `zip` |

例 (Linux amd64、`vX.Y.Z` は対象タグ):

```sh
VERSION=X.Y.Z
curl -fsSL "https://github.com/ukaji3/nakama/releases/download/v${VERSION}/nakama_${VERSION}_linux_amd64.tar.gz" -o /tmp/nakama.tar.gz
mkdir -p /tmp/nakama && tar -xzf /tmp/nakama.tar.gz -C /tmp/nakama
sudo install -m 0755 /tmp/nakama/nakama /usr/local/bin/nakama
nakama version
```

`checksums.txt` で SHA-256 を検証してください:

```sh
curl -fsSLO "https://github.com/ukaji3/nakama/releases/download/v${VERSION}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing
```

### ソースから (`go install`)

```sh
go install github.com/ukaji3/nakama/cmd/nakama@latest
```

### リポジトリをクローンして自前ビルド

```sh
git clone https://github.com/ukaji3/nakama.git
cd nakama
make build
./bin/nakama version
```

## 前提条件

- リージョン: `ap-northeast-1` (東京) 固定
- 認証: `nakama aws login` によるネイティブサインイン (**aws CLI 不要**、要 `SignInLocalDevelopmentAccess` 管理ポリシー。ADR 0016)、または SSO / Profile / 環境変数など任意の AWS 資格情報
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用)
- `connect` 利用時は呼び出し側プリンシパルに `ssm:StartSession` 等が追加で必要
- ビルド環境: Go 1.23+ (クロスコンパイル可: linux / darwin / windows)
- 実行環境 (クライアント OS): Linux / macOS / Windows

## 使い方

### サインイン (`login`)

```bash
nakama aws login                 # ブラウザで AWS コンソールのセッションを使ってサインイン → profile 自動登録・アクティブ化
nakama aws login --name dev      # 保存する profile の別名を指定
nakama aws login --remote        # SSH/headless 環境: URL を表示し、ブラウザに出る検証コードを貼り付け
```

`login` は AWS CLI の `aws login` と同じプロトコル (OAuth 2.0 authorization code + PKCE + DPoP) のネイティブ実装です (ADR 0016)。**aws CLI のインストールは不要**で、IAM ユーザー/ロールに AWS 管理ポリシー `SignInLocalDevelopmentAccess` が付与されていれば (root は不要)、ブラウザの AWS コンソールセッションからサインインできます。取得したトークンは AWS CLI 互換のキャッシュ (`~/.aws/login/cache/`) に保存されるため `aws login` とセッションを共有でき、完了時に nakama profile へ自動 save & load されて直後から全コマンドが使えます。**ユーザーの `~/.aws/config` は変更しません**。

### 複数 AWS アカウントの切り替え (`profile`)

```bash
aws login                        # アカウント A にログイン (AWS CLI を使う場合。nakama aws login でも可)
nakama aws profile save          # 現在のログインセッションを保存 (--name a-account で別名指定可)
aws login                        # アカウント B にログイン (~/.aws/config の login_session は上書きされる)
nakama aws profile save --name b-account
nakama aws profile list          # 保存済みを一覧 (ACTIVE 列の * が現在 nakama が使うアカウント)
nakama aws profile load a-account  # nakama が操作するアカウントを選択
nakama aws profile unload        # 選択を解除し既定の認証チェーンに戻す
nakama aws profile delete b-account
```

`profile` はログインセッションの識別子 (`login_session`) を nakama 側にコピーして、複数アカウントのセッションを共存させる仕組みです (ADR 0011)。

- **保存されるのは識別子のみ**: `login_session` は ARN 形状の非機密な識別子で、アクセスキーやトークンではありません。実際の短期資格情報とリフレッシュトークンは AWS CLI 互換のトークンキャッシュ `~/.aws/login/cache/` (0600) にあり、nakama はこれをネイティブに読み書き・リフレッシュします (ADR 0016)。レジストリとして書くのは `~/.config/nakama/profiles.json` と `~/.config/nakama/aws_config` (いずれも 0600) だけで、**ユーザーの `~/.aws/config` は変更しません**
- **有効期限**: リフレッシュトークンの寿命は最長 12 時間。期限切れは該当アカウントへ `nakama aws login` (または `aws login`) し直せば復活します (`profile list` の STATUS 列で確認可)
- **fail-closed**: `load` 済みセッションが解決できない場合、他アカウントへ暗黙にフォールバックせずエラーになります (意図しないアカウントへリソースを作らないため)。active な profile は `AWS_PROFILE` 環境変数より優先されます

### 基盤の作成とアクセス制御 (`init` / `acl`)

```bash
# 基盤 (VPC / Subnet / NAT インスタンス / WAF) の作成 (冪等)
# 既存の NAT Gateway があれば削除し EIP を解放して NAT インスタンスへ移行する
nakama aws init

# アクセス元 IP の許可リスト操作 (未登録 IP からは code-server に到達できない)
nakama aws acl add               # 自分のグローバル IP を自動取得して追加
nakama aws acl add 203.0.113.10  # 明示指定
nakama aws acl list
nakama aws acl delete 203.0.113.10
```

### インスタンスの作成と管理 (`create` / `list` / `info` / `start` / `stop` / `edit` / `update` / `delete`)

```bash
nakama aws create --name dev01 --instance-type t3.medium
nakama aws create --name arm01 --instance-type t4g.large   # Graviton (ARM64) も指定可 (AMI は arm64 を自動選択)
nakama aws create --name ops01 --instance-type t3.medium --with-admin        # AdministratorAccess 付きロールを付与 (CDK/CloudFormation 用)
nakama aws create --name app01 --instance-type t3.medium --with-role my-role # 任意の既存 IAM ロールを付与
nakama aws list                  # TYPE 列で VSCODE 種別を表示
nakama aws list --show-nat       # NAT インスタンスの稼働状況 (TYPE=NAT, running/stopped) も表示
nakama aws info dev01 --show-password
nakama aws stop dev01
nakama aws start dev01
nakama aws edit dev01 --ebs 40                      # EBS の拡張 (縮小不可)
nakama aws edit dev01 --instance-type m6a.2xlarge   # 停止→変更→元の状態へ復帰 (--yes で確認省略, アーキ跨ぎ変更は不可)
nakama aws update dev01                             # 自動backup→apt dist-upgrade + code-server latest→元の電源状態へ復帰
nakama aws update dev01 --keep 5 --yes              # 自動backupを直近5世代に制限・確認省略
nakama aws delete dev01
```

`create --with-admin` は AdministratorAccess を持つ共有 IAM ロール (`nakama-instance-role-admin`) を、`--with-role <role>` は既存の任意 IAM ロールをインスタンスに付与します (両者は排他)。いずれもリソース作成前に `iam:SimulatePrincipalPolicy` で呼び出し元の権限 (`iam:PassRole` 等) をチェックし、権限不足なら**何もリソースを作らずに**エラー終了します (呼び出し元には `iam:SimulatePrincipalPolicy` 権限が追加で必要。ADR 0010)。

> **セキュリティ注意 (`--with-admin`)**: 付与されるのは AdministratorAccess です。CloudFront + WAF の IP allowlist を通過して code-server にログインできる者は、その開発環境を通じて**実質的に AWS アカウントの管理者権限を行使できます**。`acl` の許可 IP は最小限に保ち、パスワードの取り扱いに注意してください。`--with-role` で SSM 権限 (AmazonSSMManagedInstanceCore) のないロールを指定した場合、`connect` / `update` および初期セットアップのパスワード取得が動作しない可能性があるため、実行時に警告が表示されます。

### ブラウザで開く (`open`)

```bash
nakama aws open dev01                # ブラウザで開いてパスワードログインを自動通過
nakama aws open dev01 --no-auth      # 自動ログインなしで URL を開くだけ
nakama aws open dev01 --print-url    # ブラウザを起動せず URL を表示 (headless / SSH 向け)
```

`open` は code-server の URL をブラウザで開き、パスワードログインを自動通過します (ADR 0012)。パスワードを SSM から取得し、127.0.0.1 上の 1 回限りのローカルページ (ランダムトークン付き URL) からログインフォームを自動送信する方式のため、**パスワードはディスクに書き込まれず**、メモリ内にのみ存在します。自動ログインができない環境 (ブラウザ起動不可など) では、理由を表示した上で URL を開く/表示するだけの動作に退避します。URL への到達には呼び出し元 IP が WAF allowlist (`nakama aws acl add`) に登録されている必要があります (未登録なら警告が表示されます)。

### バックアップと復元 (`backup` / `restore`)

```bash
nakama aws backup create dev01 --note "X を導入済み"  # ルート EBS スナップショット。ラベルは省略時に自動採番
nakama aws backup create dev01 --label before-upgrade --keep 5
nakama aws backup list dev01
nakama aws backup list                         # 名前省略時は全インスタンスのバックアップを一覧 (NAME 列付き)
nakama aws backup note dev01 --label before-upgrade --note "アップグレード前の安定版"
nakama aws restore dev01                       # 最新バックアップへインプレース復元
nakama aws restore dev01 --label before-upgrade --yes
nakama aws backup delete dev01 --label before-upgrade
```

### インスタンスにログインする (`connect`)

```bash
nakama aws connect dev01                       # 対話シェル (ubuntu ユーザー・exit で切断)
nakama aws connect dev01 ls -la                # 単発コマンド実行 (ubuntu 実行・ssh 風)
nakama aws connect --tty dev01 top             # 対話 (PTY) でコマンド実行 (フラグは name より前に置く)
```

`connect` は AWS Systems Manager Session Manager を使用します (SSH 不要・受信ポート開放なし)。`session-manager-plugin` の取り込みにより**追加バイナリのインストールは不要**です。呼び出し側のプリンシパルには `ssm:StartSession` / `ssm:TerminateSession` / `ssm:ResumeSession` 権限が必要です (接続先 EC2 の instance role は `init` が付与する `AmazonSSMManagedInstanceCore` で充足)。ログインユーザーは **ubuntu** (code-server の稼働ユーザー) です。Session Manager は `ssm-user` として接続しますが、既定の passwordless sudo を用いて ubuntu に切り替え、対話シェルは単一の `exit` で切断します (ADR 0009)。

**自動再接続** (ADR 0014): セッションが恒久的に切断された場合 (インスタンスのハイバネートや長時間のネットワーク断など)、`connect` は新規セッションで自動的に再接続します。対話シェルでは既定で有効 (最大 5 回、指数バックオフ)、コマンド実行モードでは非冪等コマンドの再実行を避けるため既定で無効です。`--retry <n>` で回数を明示でき (`--retry 0` で無効化)、明示した値はどのモードでも尊重されます。セッションが 60 秒以上安定してから切断された場合は試行回数がリセットされます。**再接続後のシェルは新しいセッション**であり、以前のシェル状態 (カレントディレクトリ・環境変数・実行中プロセス) は失われます。AWS 資格情報が失効している場合は再試行せず、`aws login` を促してエラー終了します。

### バックグラウンド常駐のポート転送 (`tunnel`)

```bash
nakama aws tunnel dev01 -L 8080:8080               # localhost:8080 -> instance:8080 (start は省略可)
nakama aws tunnel start dev01 -L 8080:8080 -L 5432:db.internal:5432  # 複数ポート・別ホスト経由も可
nakama aws tunnel status dev01                     # 転送ごとの状態を表示 (active / reconnecting / dead)
nakama aws tunnel status                           # 全インスタンスの転送を一覧 (NAME 列付き)
nakama aws tunnel suggest dev01                    # インスタンスで listen 中のポートと -L 案を表示
nakama aws tunnel restart dev01                    # 既存設定のまま再接続 (login し直し後の復旧に)
nakama aws tunnel stop dev01                       # 転送を停止しデーモンも終了
```

`tunnel` はバックグラウンドに常駐するデーモンがポート転送を維持し、ターミナルを専有しません (ADR 0017)。複数の `-L` を同時に指定できます (転送ごとに SSM セッションを 1 本使用)。切断時は `connect` と同じポリシー (最大 5 回・指数バックオフ・60 秒以上安定していれば試行回数リセット) で自動再接続するため、長時間のトンネルが日をまたいでも維持されます。再接続時のセッション確立失敗 (PC のスリープ復帰直後にネットワークが未復帰のケースなど) も同じ再接続枠で再試行されます。デーモンのログは OS 標準ログ (Linux/macOS: syslog、Windows: イベントログ) に出力され、状態は `~/.config/nakama/tunnels/<name>.json` (0600) にのみ保存されて `tunnel stop` で削除されます。AWS 資格情報が失効した場合、該当転送は `status` で dead と表示されます — `nakama aws login` (または `aws login`) の後に `tunnel restart <name>` で既存設定のまま再接続できます。`tunnel status` は名前を省略するとこのマシンに記録された全インスタンスの転送を一覧します (デーモンが応答しないものは stale として表示し、残骸の状態ファイルを掃除します)。`tunnel suggest <name>` はインスタンス上で listen しているポートを調べて `-L` の案とそのまま実行できるコマンドを表示します (読み取り専用で、自動では転送しません)。`start` / `stop` / `status` / `restart` / `suggest` はサブコマンド名と曖昧になるため、インスタンス名としては予約され `create` で拒否されます。

### ローカルの VS Code / JetBrains / ssh から使う (`ssh`)

```bash
nakama aws ssh setup dev01           # 公開鍵を authorized_keys に登録 + ssh config スニペット生成
nakama aws ssh setup dev01 --identity ~/.ssh/id_ed25519.pub   # 鍵を明示
nakama aws ssh setup dev01 --no-key  # スニペットのみ生成 (鍵は自前管理)
ssh nakama-dev01                     # Include を追加した後はこれだけで接続
```

`ssh` は SSM Session Manager 上に ssh を通す仕組みです (ADR 0019)。`setup` が生成するスニペットを `~/.ssh/config` から `Include` すると、**デスクトップ版 VS Code の Remote-SSH、JetBrains Gateway、素の `ssh` / `scp` / `rsync` / git over ssh** がそのまま使えます。受信ポートの開放も public IP も不要で、鍵は公開鍵だけがインスタンスに渡ります (秘密鍵はローカルに留まります)。

- **`~/.ssh/config` は変更しません**: スニペットは `~/.config/nakama/ssh/<name>.conf` (0600) に書き出され、表示される `Include` 行を利用者が自分の config に 1 行追加します
- `ProxyCommand` には nakama 実行ファイルの絶対パスが埋め込まれます (PATH 非依存、Windows の空白入りパスも引用済み)
- 切断時の再接続は ssh クライアント側の設定 (`ServerAliveInterval` など) に従います。`connect` / `tunnel` の自動再接続 (ADR 0014) は適用されません
- 必要な IAM 権限は `connect` と同じです

### 環境の自己診断 (`doctor`)

```bash
nakama aws doctor            # 各項目を ok / warn / fail + 対処案内で表示
nakama aws doctor -o json    # サポート依頼への添付用
```

`doctor` は nakama が必要とするものを一括で点検します: プロキシ解決 (環境変数 / Windows の WinHTTP・PAC) と AWS 到達性、TLS インスペクションの有無と OS 証明書ストアでの検証結果、資格情報の解決元 (アカウント・identity・active profile)、自分のグローバル IP が WAF 許可リストに載っているか、`connect` / `tunnel` / `ssh` に必要な権限 (`ssm:StartSession` 等)、ローカルに記録された tunnel の状態。

**完全に読み取り専用**で、AWS リソースもローカル設定も一切変更しません。資格情報・トークン・パスワードは出力しません。fail が 1 つでもあれば終了コードが非 0 になります (資格情報が解決できない場合もプロキシ・TLS のローカル診断は実行されます)。

## 企業プロキシ環境での利用

nakama の全アウトバウンド通信 (AWS API / `connect` の WebSocket / `login` のサインイン・トークンリフレッシュ / グローバル IP 取得) は、以下の優先順位でプロキシを自動解決します (ADR 0015)。

1. **環境変数** — `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` (小文字も可) が設定されていればそれを使用
2. **OS のプロキシ設定** — 環境変数が未設定の場合:
   - **Windows**: 「インターネット オプション」/ WinHTTP の静的設定と **PAC スクリプト**を自動検出 (AD 等でプロキシ設定のみ配布される環境でも追加設定不要)
   - **macOS**: OS 設定の自動検出は**非対応** (リリースバイナリは CGO_ENABLED=0 のため)。プロキシ利用時は環境変数を設定してください
   - **Linux**: 従来どおり環境変数のみ

TLS 検証は Go 標準の platform verifier により **OS の証明書ストア**を使用するため、AD 等で配布された企業 CA (TLS 傍受プロキシの CA を含む) は Windows / macOS では自動的に信頼されます。CA バンドルの追加設定は不要です。

サインインと資格情報のリフレッシュも**すべて nakama がネイティブに行う**ため (aws CLI へのシェルアウトなし。ADR 0016)、Python 製 AWS CLI が certifi の CA バンドル起因で起こす CERT エラーは発生しません。監査プロキシ環境でも「ブラウザで AWS コンソールにログイン → `nakama aws login`」だけで利用を開始できます。

> **運用注意**: 監査型プロキシが WebSocket (CONNECT トンネル上のアップグレード) を遮断する環境では、`connect` (Session Manager の data-channel は WSS) は利用できません。この場合はプロキシ管理者に WebSocket の許可を依頼してください。また、アイドル接続を積極的に切断するプロキシ配下で `connect` / `tunnel` の切断が多発する場合は、環境変数 `SSM_PLUGIN_PING_INTERVAL_SECONDS` (秒、既定 300) で WebSocket keepalive の間隔を短くできます (例: `30`)。

### aws CLI 本体を企業プロキシ環境で使う (Windows: `cli shell`)

nakama 自身は上記の自動解決で動きますが、**aws CLI 本体**は OS のプロキシ設定も Windows 証明書ストアも読まないため、TLS インスペクション (MITM) プロキシ配下では既定で CERT エラーになります。Windows では次の 1 コマンドで、aws CLI がそのまま使えるサブシェルを開けます (ADR 0018):

```
nakama aws cli shell
```

- OS のプロキシ設定 (WinHTTP / PAC) を自動解決し、TLS インスペクションを検出すると **Windows 証明書ストアで検証済み**の企業 CA チェーン + Amazon Trust Services ルートを `AWS_CA_BUNDLE` 用 PEM (`~/.config/nakama/aws-ca-bundle.pem`) に書き出します
- `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` / `AWS_CA_BUNDLE` を注入した新しいシェル (親シェルと同種を自動判別、`--shell` で指定可) を同じウィンドウ内に開きます。中では `aws s3 ls` などが追加設定なしで動きます
- `exit` で元のシェルへ戻ります。レジストリ・ユーザー環境変数・`~/.aws/config`・親シェルには**何も残しません**
- プロキシが NTLM/Negotiate 認証を要求する場合は検出して回避策 (px / Cntlm などのローカル認証リレー) を案内します (aws CLI は Basic 認証のみ対応のため)
- Windows 以外の OS では Windows 専用オプションである旨を表示します (Linux/macOS は環境変数 `HTTP(S)_PROXY` / `AWS_CA_BUNDLE` を直接設定してください)

## アーキテクチャとコスト

```
[User Browser] ──HTTPS/WSS──> [CloudFront] ──HTTP:8080──> [EC2 (private subnet)]
                                    │                          └── code-server
                                    └── WAFv2 IP set (ACL)

[Private Subnet] ──0.0.0.0/0──> [NAT インスタンス (public subnet)] ──> [IGW] ──> Internet
                                    └── code-server が 1 台でも起動中のときだけ稼働

[Parameter Store] ── /nakama/<name>/code-server/password (SecureString)
                  └─ /nakama/<name>/meta/* (instance-id, eni-id, distribution-id 等)
```

egress (private subnet からのアウトバウンド通信) は NAT Gateway ではなく小型の **NAT インスタンス** (`t4g.nano`, Amazon Linux 2023 arm64) を経由します。NAT インスタンスの電源は code-server インスタンスのライフサイクルに連動します。

- code-server インスタンスを 1 台でも起動 (`create` / `start`) すると NAT インスタンスも起動
- すべての code-server インスタンスが停止・削除されると NAT インスタンスも自動停止

これにより、常時課金される NAT Gateway の固定費を避け、全停止時のコストをゼロに近づけます。NAT インスタンスは EIP を持たず public subnet の自動割当 public IPv4 を使うため、停止中は public IPv4 の時間課金も発生しません (起動のたびに IP は変わりますが、inbound は CloudFront 経由のみで固定 IP を必要としません)。

## ステータス

主要サブコマンド (`login` / `init` / `acl` / `create` / `list` / `info` / `open` / `start` / `stop` / `delete` / `edit` / `backup` / `restore` / `connect` / `tunnel` / `ssh` / `update` / `profile` / `doctor` / `cli shell`) は実装済み。

## ドキュメント

- [docs/requirements.md](docs/requirements.md) — 要件定義
- [docs/design.md](docs/design.md) — 設計
- [docs/adr/](docs/adr/) — Architecture Decision Records

## ライセンス

[MIT License](LICENSE)。同梱する第三者コンポーネントのライセンスは [THIRD-PARTY-NOTICES](THIRD-PARTY-NOTICES) を参照してください。
