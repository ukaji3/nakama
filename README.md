# nakama

> All Rights Reserved. Proprietary software. License: Private.

`nakama` は AWS 上で [coder/code-server](https://github.com/coder/code-server) を実行する EC2 インスタンスを、CloudFront VPC Origin (Private Origin) 経由で WSS アクセス可能な状態で構築・管理する CLI ツールです。

ローカル開発で Node.js などがメモリ不足になるケースを想定し、開発環境を AWS にオフロードすることを目的としています。

## アーキテクチャ概要

```
[User Browser] ──HTTPS/WSS──> [CloudFront] ──HTTP:8080──> [EC2 (private subnet)]
                                    │                          └── code-server
                                    └── WAFv2 IP set (ACL)

[Parameter Store] ── /nakama/<name>/code-server/password (SecureString)
                  └─ /nakama/<name>/meta/* (instance-id, eni-id, distribution-id 等)
```

## 前提条件

- リージョン: `ap-northeast-1` (東京) 固定
- AWS CLI 同等の認証 (SSO / Profile / 環境変数) が利用可能
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用)
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
# 基盤 (VPC / Subnet / NAT GW / WAF) の作成 (冪等)
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
nakama aws delete dev01
```

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

主要サブコマンド (`init` / `acl` / `create` / `list` / `info` / `start` / `stop` / `delete` / `edit`) は実装済み。

## ドキュメント

- [docs/requirements.md](docs/requirements.md) — 要件定義
- [docs/design.md](docs/design.md) — 設計
- [docs/adr/](docs/adr/) — Architecture Decision Records

## ライセンス

Private (All Rights Reserved). 外部公開・再配布・派生物の作成は許可されません。
