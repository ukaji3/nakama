# nakama

> All Rights Reserved. Proprietary software. License: Private.

`nakama` は AWS 上で [coder/code-server](https://github.com/coder/code-server) を実行する EC2 インスタンスを、CloudFront VPC Origin (Private Origin) 経由で WSS アクセス可能な状態で構築・管理する CLI ツールです。

ローカル開発で Node.js などがメモリ不足になるケースを想定し、開発環境を AWS にオフロードすることを目的としています。

## アーキテクチャ概要

```
[User Browser] ──HTTPS/WSS──> [CloudFront] ──HTTP:8080──> [EC2 (private subnet)]
                                    │                          └── code-server
                                    └── WAFv2 IP set (ACL)

[Private Subnet] ──0.0.0.0/0──> [NAT インスタンス (public subnet)] ──> [IGW] ──> Internet
                                    └── code-server が 1 台でも起動中のときだけ稼働

[Parameter Store] ── /nakama/<name>/code-server/password (SecureString)
                  └─ /nakama/<name>/meta/* (instance-id, eni-id, distribution-id 等)
```

egress (private subnet からのアウトバウンド通信) は NAT Gateway ではなく
小型の **NAT インスタンス** (`t4g.nano`, Amazon Linux 2023 arm64) を経由します。
NAT インスタンスの電源は code-server インスタンスのライフサイクルに連動します。

- code-server インスタンスを 1 台でも起動 (`create` / `start`) すると NAT インスタンスも起動。
- すべての code-server インスタンスが停止・削除されると NAT インスタンスも自動停止。

これにより、常時課金される NAT Gateway の固定費を避け、全停止時のコストをゼロに近づけます。
NAT インスタンスは EIP を持たず public subnet の自動割当 public IPv4 を使うため、
停止中は public IPv4 の時間課金も発生しません (起動のたびに IP は変わりますが、
inbound は CloudFront 経由のみで固定 IP を必要としません)。

## 前提条件

- リージョン: `ap-northeast-1` (東京) 固定
- 認証: `nakama aws login` によるネイティブサインイン (**aws CLI 不要**、要 `SignInLocalDevelopmentAccess` 管理ポリシー。ADR 0016)、または SSO / Profile / 環境変数など任意の AWS 資格情報
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用)
- `connect` 利用時は呼び出し側プリンシパルに `ssm:StartSession` 等が追加で必要
- ビルド環境: Go 1.23+ (クロスコンパイル可: linux / darwin / windows)
- 実行環境 (クライアント OS): Linux / macOS / Windows

## インストール

```bash
go install github.com/ukaji3/nakama/cmd/nakama@latest
# または
git clone https://github.com/ukaji3/nakama.git
cd nakama
make build
```

## 使い方

```bash
# サインイン (aws CLI 不要のネイティブ login。ADR 0016)
nakama aws login                 # ブラウザで AWS コンソールのセッションを使ってサインイン → profile 自動登録・アクティブ化
nakama aws login --name dev      # 保存する profile の別名を指定
nakama aws login --remote        # SSH/headless 環境: URL を表示し、ブラウザに出る検証コードを貼り付け

# 複数 AWS アカウントの切り替え (従来の aws login セッションの保存・選択も引き続き可能)
aws login                        # アカウント A にログイン (AWS CLI を使う場合)
nakama aws profile save          # 現在のログインセッションを保存 (--name a-account で別名指定可)
aws login                        # アカウント B にログイン (~/.aws/config の login_session は上書きされる)
nakama aws profile save --name b-account
nakama aws profile list          # 保存済みを一覧 (ACTIVE 列の * が現在 nakama が使うアカウント)
nakama aws profile load a-account  # nakama が操作するアカウントを選択
nakama aws profile unload        # 選択を解除し既定の認証チェーンに戻す
nakama aws profile delete b-account

# 基盤 (VPC / Subnet / NAT インスタンス / WAF) の作成 (冪等)
# 既存の NAT Gateway があれば削除し EIP を解放して NAT インスタンスへ移行する
nakama aws init

# アクセス元 IP の許可リスト操作
nakama aws acl add               # 自分のグローバル IP を自動取得して追加
nakama aws acl add 203.0.113.10  # 明示指定
nakama aws acl list
nakama aws acl delete 203.0.113.10

# code-server インスタンスのライフサイクル
nakama aws create --name dev01 --instance-type t3.medium
nakama aws create --name arm01 --instance-type t4g.large   # Graviton (ARM64) も指定可 (AMI は arm64 を自動選択)
nakama aws create --name ops01 --instance-type t3.medium --with-admin        # AdministratorAccess 付きロールを付与 (CDK/CloudFormation 用)
nakama aws create --name app01 --instance-type t3.medium --with-role my-role # 任意の既存 IAM ロールを付与
nakama aws list                  # TYPE 列で VSCODE 種別を表示
nakama aws list --show-nat       # NAT インスタンスの稼働状況 (TYPE=NAT, running/stopped) も表示
nakama aws info dev01 --show-password
nakama aws open dev01                # ブラウザで開いてパスワードログインを自動通過
nakama aws open dev01 --no-auth      # 自動ログインなしで URL を開くだけ
nakama aws open dev01 --print-url    # ブラウザを起動せず URL を表示 (headless / SSH 向け)
nakama aws stop dev01
nakama aws start dev01
nakama aws edit dev01 --ebs 40
nakama aws edit dev01 --instance-type m6a.2xlarge   # 停止→変更→元の状態へ復帰 (--yes で確認省略, アーキ跨ぎ変更は不可)
nakama aws update dev01                         # 自動backup→apt dist-upgrade + code-server latest→元の電源状態へ復帰
nakama aws update dev01 --keep 5 --yes          # 自動backupを直近5世代に制限・確認省略
nakama aws delete dev01

# バックアップ (ルート EBS スナップショット) / 復元
nakama aws backup create dev01 --note "X を導入済み"  # ラベルは省略時に自動採番
nakama aws backup create dev01 --label before-upgrade --keep 5
nakama aws backup list dev01
nakama aws backup list                         # 名前省略時は全インスタンスのバックアップを一覧 (NAME 列付き)
nakama aws backup note dev01 --label before-upgrade --note "アップグレード前の安定版"
nakama aws restore dev01                       # 最新バックアップへインプレース復元
nakama aws restore dev01 --label before-upgrade --yes
nakama aws backup delete dev01 --label before-upgrade

# インスタンスへのログイン (SSM Session Manager 経由・SSH 不要・受信ポート開放なし)
nakama aws connect dev01                       # 対話シェル (ubuntu ユーザー・exit で切断)
nakama aws connect dev01 ls -la                # 単発コマンド実行 (ubuntu 実行・ssh 風)
nakama aws connect dev01 --tty top             # 対話 (PTY) でコマンド実行
nakama aws connect dev01 -L 8080:8080          # ポートフォワード: localhost:8080 -> instance:8080
nakama aws connect dev01 -L 5432:db.internal:5432  # instance 経由で別ホストへ転送
```

> `connect` は AWS Systems Manager Session Manager を使用します。`session-manager-plugin` の取り込みにより**追加バイナリのインストールは不要**です。呼び出し側のプリンシパルには `ssm:StartSession` / `ssm:TerminateSession` / `ssm:ResumeSession` 権限が必要です (接続先 EC2 の instance role は `init` が付与する `AmazonSSMManagedInstanceCore` で充足)。ログインユーザーは **ubuntu**(code-server の稼働ユーザー)です。Session Manager は `ssm-user` として接続しますが、既定の passwordless sudo を用いて ubuntu に切り替え、対話シェルは単一の `exit` で切断します (詳細は ADR 0009)。
>
> **自動再接続**: セッションが恒久的に切断された場合 (インスタンスのハイバネートや長時間のネットワーク断など)、`connect` は新規セッションで自動的に再接続します (詳細は ADR 0014)。対話シェルとポートフォワードでは既定で有効 (最大 5 回、指数バックオフ)、コマンド実行モードでは非冪等コマンドの再実行を避けるため既定で無効です。`--retry <n>` で回数を明示でき (`--retry 0` で無効化)、明示した値はどのモードでも尊重されます。セッションが 60 秒以上安定してから切断された場合は試行回数がリセットされるため、長時間のトンネルが日をまたいで予算切れすることはありません。ハイバネートから復帰したインスタンスへはバックオフ付きで接続を再試行します。**再接続後のシェルは新しいセッション**であり、以前のシェル状態 (カレントディレクトリ・環境変数・実行中プロセス) は失われます。AWS 資格情報が失効している場合は再試行せず、`aws login` を促してエラー終了します。

> `open` は code-server の URL をブラウザで開き、パスワードログインを自動通過します (詳細は ADR 0012)。パスワードを SSM から取得し、127.0.0.1 上の 1 回限りのローカルページ (ランダムトークン付き URL) からログインフォームを自動送信する方式のため、**パスワードはディスクに書き込まれず**、メモリ内にのみ存在します。自動ログインができない環境 (ブラウザ起動不可など) では、理由を表示した上で URL を開く/表示するだけの動作に退避します。`--no-auth` で自動ログインなし、`--print-url` でブラウザを起動せず URL 表示 (headless / SSH 向け) を選べます。URL への到達には呼び出し元 IP が WAF allowlist (`nakama aws acl add`) に登録されている必要があります (未登録なら警告が表示されます)。

> `create --with-admin` は AdministratorAccess を持つ共有 IAM ロール (`nakama-instance-role-admin`) を、`--with-role <role>` は既存の任意 IAM ロールをインスタンスに付与します (両者は排他)。いずれもリソース作成前に `iam:SimulatePrincipalPolicy` で呼び出し元の権限 (`iam:PassRole` 等) をチェックし、権限不足なら**何もリソースを作らずに**エラー終了します (呼び出し元には `iam:SimulatePrincipalPolicy` 権限が追加で必要。詳細は ADR 0010)。
>
> **セキュリティ注意 (`--with-admin`)**: 付与されるのは AdministratorAccess です。CloudFront + WAF の IP allowlist を通過して code-server にログインできる者は、その開発環境を通じて**実質的に AWS アカウントの管理者権限を行使できます**。`acl` の許可 IP は最小限に保ち、パスワードの取り扱いに注意してください。`--with-role` で SSM 権限 (AmazonSSMManagedInstanceCore) のないロールを指定した場合、`connect` / `update` および初期セットアップのパスワード取得が動作しない可能性があるため、実行時に警告が表示されます。

> `login` は AWS CLI の `aws login` と同じプロトコル (OAuth 2.0 authorization code + PKCE + DPoP) のネイティブ実装です (詳細は ADR 0016)。**aws CLI のインストールは不要**で、IAM ユーザー/ロールに AWS 管理ポリシー `SignInLocalDevelopmentAccess` が付与されていれば (root は不要)、ブラウザの AWS コンソールセッションからサインインできます。取得したトークンは AWS CLI 互換のキャッシュ (`~/.aws/login/cache/`) に保存されるため `aws login` とセッションを共有でき、完了時に nakama profile へ自動 save & load されて直後から全コマンドが使えます。**ユーザーの `~/.aws/config` は変更しません**。`--remote` は SSH/headless 環境向けで、表示された URL を別デバイスのブラウザで開き、表示される検証コードを貼り付けて完了します。従来の「`aws login` → `nakama aws profile save`」フローも引き続き利用でき、そのセッションのリフレッシュも nakama がネイティブに行います。

> `profile` は `aws login` / `nakama aws login` のセッション識別子 (`login_session`) を nakama 側にコピーして、複数アカウントのセッションを共存させる仕組みです (詳細は ADR 0011)。
>
> - **保存されるのは識別子のみ**: `login_session` は ARN 形状の非機密な識別子で、アクセスキーやトークンではありません。実際の短期資格情報とリフレッシュトークンは AWS CLI 互換のトークンキャッシュ `~/.aws/login/cache/` (0600) にあり、nakama はこれをネイティブに読み書き・リフレッシュします (ADR 0016)。レジストリとして書くのは `~/.config/nakama/profiles.json` と `~/.config/nakama/aws_config` (いずれも 0600) だけで、**ユーザーの `~/.aws/config` は変更しません**。
> - **有効期限**: リフレッシュトークンの寿命は最長 12 時間です。期限切れの場合は該当アカウントへ `nakama aws login` (または `aws login`) し直せば復活します (`profile list` の STATUS 列で確認可)。
> - **fail-closed**: `load` 済みの状態で当該セッションが解決できない場合、他のアカウントへ暗黙にフォールバックせずエラーになります (意図しないアカウントへリソースを作らないため)。`unload` で既定の認証チェーンに戻せます。
> - active な profile は `AWS_PROFILE` 環境変数より優先されます。

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

## 企業プロキシ環境での利用

nakama の全アウトバウンド通信 (AWS API / `connect` の WebSocket / `login` のサインイン・トークンリフレッシュ / グローバル IP 取得) は、以下の優先順位でプロキシを自動解決します (詳細は ADR 0015)。

1. **環境変数** — `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` (小文字も可) が設定されていればそれを使用
2. **OS のプロキシ設定** — 環境変数が未設定の場合:
   - **Windows**: 「インターネット オプション」/ WinHTTP の静的設定と **PAC スクリプト**を自動検出 (AD 等でプロキシ設定のみ配布される環境でも追加設定不要)
   - **macOS**: OS 設定の自動検出は**非対応** (リリースバイナリは CGO_ENABLED=0 のため)。プロキシ利用時は環境変数を設定してください
   - **Linux**: 従来どおり環境変数のみ

TLS 検証は Go 標準の platform verifier により **OS の証明書ストア**を使用するため、AD 等で配布された企業 CA (TLS 傍受プロキシの CA を含む) は Windows / macOS では自動的に信頼されます。CA バンドルの追加設定は不要です。

サインインと資格情報のリフレッシュ (`nakama aws login` および既存 `aws login` セッションの解決) も**すべて nakama がネイティブに行う**ため (aws CLI へのシェルアウトなし。ADR 0016)、Python 製 AWS CLI が certifi の CA バンドル起因で起こす CERT エラーは発生しません。監査プロキシ環境でも「ブラウザで AWS コンソールにログイン → `nakama aws login`」だけで利用を開始できます。

> **運用注意**: 監査型プロキシが WebSocket (CONNECT トンネル上のアップグレード) を遮断する環境では、`connect` (Session Manager の data-channel は WSS) は利用できません。この場合はプロキシ管理者に WebSocket の許可を依頼してください。

## ステータス

主要サブコマンド (`login` / `init` / `acl` / `create` / `list` / `info` / `open` / `start` / `stop` / `delete` / `edit` / `backup` / `restore` / `connect` / `update` / `profile`) は実装済み。

## ドキュメント

- [docs/requirements.md](docs/requirements.md) — 要件定義
- [docs/design.md](docs/design.md) — 設計
- [docs/adr/](docs/adr/) — Architecture Decision Records

## ライセンス

Private (All Rights Reserved). 外部公開・再配布・派生物の作成は許可されません。
