# nakama 設計書 (v1)

> Status: 初版
> Last updated: 2026-05-28

要件定義書: [requirements.md](requirements.md)

## 1. アーキテクチャ概要

```
[User Browser] ──HTTPS / WSS──> [CloudFront Distribution]
                                       │  ── associated ──> [WAFv2 web ACL]
                                       │                      └─ IP set (allowlist)
                                       │
                                       │  (VPC Origin: HTTP:8080)
                                       ▼
                          [Service-managed ENI in private subnet]
                                       │
                                       ▼
                          [EC2: Ubuntu 26.04, code-server@8080]
                              ├── EBS 20 GiB (gp3, 初期値)
                              ├── ENI (固定 Private IP)
                              └── SG: inbound from CloudFront managed prefix list (8080/tcp)
                                      outbound 0.0.0.0/0
                                       │
                                       ▼
                       [Private Subnet] ──> [NAT GW (Public Subnet)] ──> [IGW] ──> Internet

[SSM Parameter Store]
  /nakama/<name>/code-server/password   (SecureString, KMS aws/ssm)
  /nakama/<name>/code-server/version    (String)
  /nakama/<name>/meta/instance-id       (String)
  /nakama/<name>/meta/eni-id            (String)
  /nakama/<name>/meta/distribution-id   (String)
  /nakama/<name>/meta/distribution-domain (String)
  /nakama/<name>/meta/vpc-origin-id     (String)
  /nakama/_meta/initialized             (String, "true")
  /nakama/_meta/version                 (String, semver)
  /nakama/_meta/acl/<ip>                (String, RFC3339 timestamp)
```

## 2. パッケージ構成

```
github.com/ukaji3/nakama
├── cmd/nakama/                  // エントリーポイント
├── internal/
│   ├── cli/                     // cobra コマンド木
│   ├── awsx/                    // AWS SDK v2 ラッパー interface (mockable)
│   ├── service/                 // ユースケース層 (init, instance, acl)
│   ├── userdata/                // cloud-init User Data テンプレ (//go:embed)
│   ├── naming/                  // リソース命名規約
│   ├── tagging/                 // タグスキーマ
│   ├── config/                  // 固定値 (region, CIDR, AZ, SSM パスなど)
│   └── version/                 // ビルド時に -ldflags で注入される version 情報
└── docs/
    ├── requirements.md
    ├── design.md
    └── adr/
```

## 3. リソース命名規約

すべて `internal/naming` に集約。

| リソース種別 | 命名 |
|---|---|
| VPC | `nakama-vpc` |
| IGW | `nakama-igw` |
| NAT GW | `nakama-natgw` |
| NAT GW EIP | `nakama-natgw-eip` |
| Public Subnet | `nakama-public-<az-suffix>` (例: `nakama-public-1a`) |
| Private Subnet | `nakama-private-<az-suffix>` |
| Public Route Table | `nakama-rtb-public` |
| Private Route Table | `nakama-rtb-private` |
| WAF web ACL | `nakama-waf` |
| WAF IP set | `nakama-allowed-ips` |
| EC2 Name タグ | `nakama-<name>` |
| Security Group | `nakama-sg-<name>` |
| ENI | `nakama-eni-<name>` |
| IAM Instance Profile | `nakama-instance-profile` |
| IAM Instance Role | `nakama-instance-role` |
| SSM Parameter | `/nakama/<name>/...` または `/nakama/_meta/...` |

## 4. タグスキーマ

| タグキー | 用途 |
|---|---|
| `nakama:managed=true` | 全 nakama 管理リソースに付与 |
| `nakama:name=<name>` | per-instance リソース |
| `nakama:component=<component>` | リソース種別の機械可読ラベル |
| `Name` | 標準 AWS Name タグ。命名規約に従う |

`component` の取りうる値は `internal/tagging.Component*` を参照。

## 5. AWS SDK ラッパー

`internal/awsx` は SDK の `*Client` を直接公開せず、利用するメソッドだけを
切り出した interface を定義する。

| interface | カバー範囲 |
|---|---|
| `EC2API` | VPC, Subnet, IGW, NAT GW, RTB, ENI, SG, Instance, Volume, ManagedPrefixList, Tags |
| `CloudFrontAPI` | VpcOrigin (Create/Get/Update/Delete/List), Distribution (CRUD + Tagging) |
| `WAFAPI` | IPSet, WebACL (CRUD)。コンストラクタで強制的に **us-east-1** に切り替え (`CLOUDFRONT` scope の制約) |
| `SSMAPI` | Parameter Store + Run Command (`SendCommand` / `GetCommandInvocation`) |
| `TaggingAPI` | `ResourceGroupsTaggingAPI.GetResources` のみ |
| `IAMAPI` | Role, InstanceProfile, AttachRolePolicy, PutRolePolicy |
| `AMIResolver` | SSM 公式パラメータ経由で Ubuntu 26.04 AMI を解決 |
| `IPDiscoverer` | `https://checkip.amazonaws.com/` に GET し、Public IPv4 を返す |

`Clients` 構造体が上記をすべて束ね、`NewClients(ctx)` がエントリポイント。

## 6. CLI 層 (cobra)

- ルート: `nakama` (PersistentPreRunE で slog ハンドラ初期化)
- 親コマンド: `nakama aws`
- グローバルフラグ: `--verbose`, `--dry-run`, `--output`
- 各サブコマンドは `errNotImplemented` を返すスタブで開始 (本ドキュメント時点)
- 実装は `internal/service/*` に委譲する設計

## 7. Service 層 (本ドキュメント時点で未実装)

```go
// 例: instance create のシグネチャ案
type CreateInstanceInput struct {
    Name              string
    InstanceType      string
    EBSGiB            int32
    PinnedCSVersion   string
}

type CreateInstanceOutput struct {
    InstanceID         string
    DistributionID     string
    DistributionDomain string
    Password           string // SSM 経由で書き込んだ生パスワード
}
```

CLI はフラグから `*Input` を組み立て、Service を呼び出すだけにする。

## 8. ユースケース別シーケンス (要旨)

### 8.1 `nakama aws init`

1. `iam:GetServiceLinkedRole` チェック → CloudFront / WAF 用 SLR を必要なら作成
2. タグ検索で `nakama-vpc` の有無を判定。なければ作成
3. IGW / RTB / Subnet / NAT GW / EIP / Routes を冪等に作成
4. WAF IP set / web ACL を冪等に作成
5. IAM instance role / profile を冪等に作成
6. SSM `/nakama/_meta/*` に状態を記録

### 8.2 `nakama aws create`

1. 環境チェック (`/nakama/_meta/initialized` 存在)
2. SSM AMI 解決 → AMI ID 取得
3. SSM PutParameter で 32 文字パスワードを SecureString として保存
4. `internal/awsx.EC2API.DescribeManagedPrefixLists` で CloudFront prefix list ID 取得
5. SG 作成 → 8080 inbound (prefix list)
6. ENI 作成 (Private subnet 1a)
7. `RunInstances` (User Data に code-server インストールスクリプト埋め込み)
8. EC2 が `running` まで待機
9. CloudFront `CreateVpcOrigin` (HTTP:8080)
10. VpcOrigin が `Deployed` まで待機
11. CloudFront `CreateDistribution` (Cache=Disabled, OriginRequestPolicy=AllViewer, WAF=ARN)
12. Distribution が `Deployed` まで待機
13. SSM に meta 情報を書き込み
14. URL + パスワードを stdout に出力

### 8.3 `nakama aws delete`

1. 確認プロンプト
2. Distribution → disable → wait → delete
3. VpcOrigin → delete
4. EC2 → terminate → wait
5. ENI → delete (terminate 完了後)
6. SG → delete
7. SSM `/nakama/<name>/*` を全削除

## 9. テスト戦略

| 層 | テスト方針 |
|---|---|
| `internal/userdata` | テンプレ展開のユニットテスト |
| `internal/naming` / `internal/tagging` | ピュア関数のテーブル駆動テスト |
| `internal/awsx` | interface 設計のみテスト (実 API は呼ばない) |
| `internal/service` | 自家製 fake / `gomock` で AWS interface をモック化したユニットテスト |
| `internal/cli` | cobra のフラグ解析と委譲テスト |
| 実 AWS E2E | 手動 (CI には載せない) |

## 10. CI

GitHub Actions で以下を main / PR で実行:

- `go vet ./...`
- `go test -race ./...`
- `go build ./...`
- `govulncheck ./...`
- (任意) `golangci-lint`

## 11. 用語注

- **VPC Origin** = CloudFront が VPC 内のリソース (ALB / NLB / EC2) をオリジンにできる新機能。2026 年 5 月時点で東京リージョン対応済 ([source](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html))
- **WebSocket on VPC Origins** = 2026 年 5 月 GA ([source](https://aws.amazon.com/about-aws/whats-new/2026/05/amazon-cloudfront-websockets-vpc-origins/))
