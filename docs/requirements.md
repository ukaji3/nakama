# nakama 要件定義書 (v1)

> Status: 初版 (実装フェーズ着手時点)
> Last updated: 2026-05-28

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
- 認証: AWS SSO / Profile / 環境変数 のいずれかで `aws sts get-caller-identity` が通ること
- 必要 IAM 権限: EC2, VPC, CloudFront (incl. VPC origins), WAFv2, SSM Parameter Store, IAM (instance role 用), Resource Groups Tagging API
- ビルド環境: Go 1.22+
- 実行環境: Linux / macOS

## 4. 機能要件

### 4.1 サブコマンド一覧

| カテゴリ | サブコマンド | 機能 |
|---|---|---|
| 基盤 | `nakama aws init` | nakama 環境基盤 (VPC / Subnet / NAT GW / WAF / IP set / IAM) を作成または再利用 |
| アクセス制御 | `nakama aws acl add [IP\|CIDR]` | アクセス元 IP を WAFv2 IP set に追加 (省略時は `https://checkip.amazonaws.com/` で自動取得) |
| アクセス制御 | `nakama aws acl list` | IP set の内容を一覧 |
| アクセス制御 | `nakama aws acl delete <IP\|CIDR>` | IP set からエントリ削除 |
| インスタンス | `nakama aws create --name <n> --instance-type <t>` | EC2 (Ubuntu 26.04, EBS 20 GiB) を private subnet に作成、code-server を導入、CloudFront distribution を VPC Origin 経由で作成、パスワードを Parameter Store に保存 |
| インスタンス | `nakama aws list` | 作成済み code-server インスタンスを一覧 |
| インスタンス | `nakama aws info <n> [--show-password]` | 接続先 URL、ステータス、パスワード (`--show-password` 時のみ) を表示 |
| インスタンス | `nakama aws start <n>` | EC2 起動 |
| インスタンス | `nakama aws stop <n>` | EC2 停止 |
| インスタンス | `nakama aws delete <n> [--yes] [--force]` | EC2 / CloudFront / 関連リソースを依存逆順で削除 |
| インスタンス | `nakama aws edit <n> --ebs <size-gb>` | EBS の拡張 (縮小不可) |

### 4.2 共通フラグ

| フラグ | 意味 |
|---|---|
| `-v`, `--verbose` | slog の Debug レベル出力 |
| `--dry-run` | 全 mutation API を抑制し、予定アクションを列挙のみ |
| `-o`, `--output {table\|json\|yaml}` | 出力フォーマット |

### 4.3 入力バリデーション

- `<name>`: `^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$` (DNS 安全)
- `<IP>`: IPv4 単独 (`/32` 自動付与) または IPv4 CIDR。**IPv6 は v1 非対応**
- `<instance-type>`: AWS API レイヤで検証 (`InvalidInstanceType` を伝播)
- `--ebs <size>`: 現在のサイズより大きい整数 (GiB)

## 5. 非機能要件

### 5.1 セキュリティ

- code-server パスワードは SSM SecureString (KMS デフォルト鍵 `aws/ssm`) に保存。CLI 出力では `--show-password` を要求
- ブラウザ ⇄ CloudFront 間は HTTPS / WSS 強制 (`Viewer Protocol Policy: redirect-to-https`)
- CloudFront ⇄ EC2 間は VPC 内クローズ通信 (HTTP:8080)
- EC2 SG inbound = CloudFront managed prefix list `com.amazonaws.global.cloudfront.origin-facing` のみ
- WAFv2 IP set による送信元制御。default action は **block**
- Microsoft Marketplace は使用不可。code-server は Open VSX を使用 (上流仕様)

### 5.2 可用性

- v1 は単一 AZ NAT GW (コスト優先)
- 単点障害が発生しても再 `init` で復旧可能 (タグベースの冪等性)

### 5.3 観測性

- 全 AWS API 呼び出しは slog で `level=DEBUG`、結果は `level=INFO`
- CloudFront / VPC origin / EC2 status の待機ループ進捗を `level=INFO` に出力

### 5.4 性能

- `init` 所要時間: 約 3〜5 分 (NAT GW がボトルネック)
- `create` 所要時間: 約 15〜20 分 (CloudFront propagation がボトルネック)
- `delete` 所要時間: 約 15〜20 分 (CloudFront disable + delete)

## 6. 状態管理

- ローカル DB を持たない。**AWS リソースのタグから動的取得**
- 一覧取得: `ResourceGroupsTaggingAPI` で `nakama:managed=true` を絞り込み
- per-instance: `nakama:name=<name>` を AND 条件として付加

## 7. 制限事項 (v1 で対応しない)

- IPv6 ACL
- 複数リージョン
- 独自ドメイン / ACM 証明書
- 部分失敗時の自動ロールバック (孤児リソースは利用者が手動で `delete`)
- マルチテナント (1 アカウント = 1 環境)
- インスタンスタイプの edit 経由変更
- code-server のアップグレード操作
- バックアップ / スナップショット

## 8. 受け入れ基準

1. `nakama aws init` 後、`aws ec2 describe-vpcs --filters Name=tag:nakama:managed,Values=true` が VPC を 1 件返す
2. `nakama aws acl add` 直後にブラウザから distribution URL にアクセスし、code-server ログイン画面が表示される
3. パスワードでログイン後、ターミナル / ファイルエクスプローラが操作可能で、Browser Devtools で `wss://...` の `101 Switching Protocols` が確認できる
4. `nakama aws acl delete` 後、403 が返る
5. `nakama aws delete` 後、当該インスタンス関連リソースが全て消失している (タグ検索でヒットしない)

## 9. リスク・オープン事項

| ID | 内容 | 対応方針 |
|---|---|---|
| R1 | EC2 の Private IP が stop/start 後に維持されるか保証なし | ENI を専用作成しアタッチすることで固定 |
| R2 | VPC Origin の deploy 待機が最大 15 分 | UI で進捗を slog 出力。タイムアウトは 20 分 |
| R3 | code-server の Microsoft Marketplace 不可は仕様 | README / FAQ に明記 |
| R4 | `apne1-az3` の AZ ID 解決はアカウント依存 | `ec2:DescribeAvailabilityZones` で AZ ID を取得し除外 |
| R5 | NAT GW の月額コスト | README に明示。stop で EC2 課金は止まるが NAT GW は止まらない旨を明記 |
