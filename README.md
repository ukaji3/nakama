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
- AWS CLI 同等の認証 (SSO / Profile / 環境変数) が利用可能
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用)
- `connect` 利用時は呼び出し側プリンシパルに `ssm:StartSession` 等が追加で必要
- ビルド環境: Go 1.23+

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
nakama aws list
nakama aws info dev01 --show-password
nakama aws stop dev01
nakama aws start dev01
nakama aws edit dev01 --ebs 40
nakama aws edit dev01 --instance-type m6a.2xlarge   # 停止→変更→元の状態へ復帰 (--yes で確認省略)
nakama aws update dev01                         # 自動backup→apt dist-upgrade + code-server latest→元の電源状態へ復帰
nakama aws update dev01 --keep 5 --yes          # 自動backupを直近5世代に制限・確認省略
nakama aws delete dev01

# バックアップ (ルート EBS スナップショット) / 復元
nakama aws backup create dev01 --note "X を導入済み"  # ラベルは省略時に自動採番
nakama aws backup create dev01 --label before-upgrade --keep 5
nakama aws backup list dev01
nakama aws backup note dev01 --label before-upgrade --note "アップグレード前の安定版"
nakama aws restore dev01                       # 最新バックアップへインプレース復元
nakama aws restore dev01 --label before-upgrade --yes
nakama aws backup delete dev01 --label before-upgrade

# インスタンスへのログイン (SSM Session Manager 経由・SSH 不要・受信ポート開放なし)
nakama aws connect dev01                       # 対話シェル
nakama aws connect dev01 ls -la                # 単発コマンド実行 (ssh 風)
nakama aws connect dev01 --tty top             # 対話 (PTY) でコマンド実行
nakama aws connect dev01 -L 8080:8080          # ポートフォワード: localhost:8080 -> instance:8080
nakama aws connect dev01 -L 5432:db.internal:5432  # instance 経由で別ホストへ転送
```

> `connect` は AWS Systems Manager Session Manager を使用します。`session-manager-plugin` の取り込みにより**追加バイナリのインストールは不要**です。呼び出し側のプリンシパルには `ssm:StartSession` / `ssm:TerminateSession` / `ssm:ResumeSession` 権限が必要です (接続先 EC2 の instance role は `init` が付与する `AmazonSSMManagedInstanceCore` で充足)。

## インストール

### バイナリ (Linux / macOS)

[Releases](https://github.com/ukaji3/nakama/releases) から OS / アーキテクチャに合うアーカイブを取得し、`PATH` の通った場所に置きます。

| OS | Arch |
|---|---|
| Linux | `amd64`, `arm64` |
| macOS | `amd64`, `arm64`, `universal` (Intel + Apple Silicon の単一バイナリ) |

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

## ステータス

主要サブコマンド (`init` / `acl` / `create` / `list` / `info` / `start` / `stop` / `delete` / `edit` / `backup` / `restore` / `connect` / `update`) は実装済み。

## ドキュメント

- [docs/requirements.md](docs/requirements.md) — 要件定義
- [docs/design.md](docs/design.md) — 設計
- [docs/adr/](docs/adr/) — Architecture Decision Records

## ライセンス

Private (All Rights Reserved). 外部公開・再配布・派生物の作成は許可されません。
