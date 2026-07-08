# nakama 設計書 (v1)

> Status: 初版
> Last updated: 2026-07-08

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
                       [Private Subnet] ──> [NAT インスタンス (Public Subnet, t4g.nano)] ──> [IGW] ──> Internet
                                                     └── 電源は code-server のライフサイクルに連動 (全停止で自動停止)

[SSM Parameter Store]
  /nakama/<name>/code-server/password   (SecureString, KMS aws/ssm)
  /nakama/<name>/code-server/version    (String)
  /nakama/<name>/meta/instance-id       (String)
  /nakama/<name>/meta/eni-id            (String)
  /nakama/<name>/meta/security-group-id (String)
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
├── third_party/
│   └── session-manager-plugin/  // 案3: 公式 plugin を版固定で取り込み (Apache-2.0)
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
| NAT インスタンス | `nakama-nat` |
| NAT インスタンス SG | `nakama-nat-sg` |
| Public Subnet | `nakama-public-<az-suffix>` (例: `nakama-public-1a`) |
| Private Subnet | `nakama-private-<az-suffix>` |
| Public Route Table | `nakama-rtb-public` |
| Private Route Table | `nakama-rtb-private` |
| WAF web ACL | `nakama-waf` |
| WAF IP set | `nakama-allowed-ips` |
| EC2 Name タグ | `nakama-<name>` |
| EBS Snapshot Name タグ | `nakama-backup-<name>-<label>` |
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
| `nakama:component=<component>` | リソース種別の機械可読ラベル (スナップショットは `backup`) |
| `nakama:backup-time=<RFC3339>` | スナップショットの取得時刻。`backup list` の並び順と件数ベース prune の判定に使用 |
| `nakama:backup-label=<label>` | バックアップの人間可読ラベル。`restore` / `backup delete` の選択キー。AWS snapshot-id を UI から隠蔽するための間接層 |
| `nakama:note=<text>` | バックアップの自由記述メモ。`CreateTags` の上書きで後から編集可能。`backup list` に表示 |
| `Name` | 標準 AWS Name タグ。命名規約に従う |

`component` の取りうる値は `internal/tagging.Component*` を参照。

## 5. AWS SDK ラッパー

`internal/awsx` は SDK の `*Client` を直接公開せず、利用するメソッドだけを
切り出した interface を定義する。

| interface | カバー範囲 |
|---|---|
| `EC2API` | VPC, AvailabilityZones (Describe), Subnet (Modify: MapPublicIpOnLaunch), IGW, NAT GW (Describe/Delete, 旧 NAT GW 移行専用。ADR 0007 で新規作成はしない), EIP (Allocate/Describe/Release), RTB (Describe/Create/AssociateRoute), Route (Create/Replace), ENI, SG, Instance (Run/Describe/Start/Stop/Reboot/Terminate/ModifyAttribute incl. SourceDestCheck), InstanceTypes (Describe), Volume (Create/Attach/Detach/Delete/Modify), Snapshot (Create/Describe/Delete), ManagedPrefixList, Tags (Create/Delete) |
| `CloudFrontAPI` | VpcOrigin (Create/Get/Update/Delete/List), Distribution (CRUD + Tagging) |
| `WAFAPI` | IPSet (CRUD), WebACL (Create/Get/List/Update; Delete は共有・恒久リソースのため非対応)。コンストラクタで強制的に **us-east-1** に切り替え (`CLOUDFRONT` scope の制約) |
| `SSMAPI` | Parameter Store + Run Command (`SendCommand` / `GetCommandInvocation`) + Session Manager (`StartSession` / `TerminateSession`) |
| `TaggingAPI` | `ResourceGroupsTaggingAPI.GetResources` のみ |
| `STSAPI` | `GetCallerIdentity` (AWS アカウント ID 解決) |
| `IAMAPI` | Role, InstanceProfile, AttachRolePolicy, PutRolePolicy |
| `AMIResolver` | SSM 公式パラメータ経由で Ubuntu 26.04 AMI をアーキ別 (amd64 / arm64) に解決 (`LatestUbuntu2604(arch)`) |
| `IPDiscoverer` | `https://checkip.amazonaws.com/` に GET し、Public IPv4 を返す |

`Clients` 構造体が上記をすべて束ね、`NewClients(ctx)` がエントリポイント。

## 6. CLI 層 (cobra)

- ルート: `nakama` (PersistentPreRunE で slog ハンドラ初期化)
- 親コマンド: `nakama aws`
- `backup` は `acl` と同様のサブコマンド群: `backup {create|list|note|delete}`。`restore` は破壊的操作のため `start`/`stop` と並ぶ独立コマンド
- グローバルフラグ: `--verbose`, `--output`
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

### 7.1 backup / restore のシグネチャ (確定)

`internal/service/backup.go` / `restore.go` に実装する。スナップショットはタグ検索で発見し、`snapshot-id` は Service 層内部に閉じる (`Backup.SnapshotID` は `-o json` 用にのみ公開)。

```go
type CreateBackupInput struct { Name, Label, Note string; Keep int32 }

type Backup struct {
    Label      string `json:"label"`
    Note       string `json:"note,omitempty"`
    CreatedAt  string `json:"created_at"`     // nakama:backup-time
    State      string `json:"state"`          // pending|completed|error
    SizeGiB    int32  `json:"size_gib"`
    SnapshotID string `json:"snapshot_id"`    // table 出力では非表示
}

func (s *Service) CreateBackup(ctx, CreateBackupInput) (*Backup, error)
func (s *Service) ListBackups(ctx, name string) ([]Backup, error)
func (s *Service) SetBackupNote(ctx, name, label, note string) (*Backup, error)
func (s *Service) DeleteBackup(ctx, name, label string) error
func (s *Service) Restore(ctx, RestoreInput) error // RestoreInput{Name, Label}; Label 空=最新
```

- ラベル検証: `^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$` (`internal/service` のローカル regexp)。空ラベルは取得時刻スラッグ `2006-01-02-1504` を採番。
- メモ検証: 255 文字以内 (`utf8.RuneCountInString`)。
- 命名/タグは `internal/naming.Snapshot(name,label)` / `internal/tagging.ForBackup(instanceName,label,nameTag,createdAt,note)` を新設。

## 8. ユースケース別シーケンス (要旨)

### 8.1 `nakama aws init`

1. AZ サニティチェック: `ec2:DescribeAvailabilityZones` で AZ ID を解決し、`apne1-az3` (CloudFront VPC Origins 非対応) を除外
2. タグ検索で `nakama-vpc` の有無を判定。なければ作成
3. IGW / Subnet を冪等に作成 (public subnet は MapPublicIpOnLaunch=true)
4. IAM instance role / profile を冪等に作成 (NAT インスタンスがこのプロファイルを使うため先に作成)
5. NAT インスタンス (`t4g.nano`, AL2023 arm64) を public subnet に作成または再利用し、SourceDestCheck を無効化 (新規作成時は `running` まで待機)
6. RTB / Routes を冪等に作成。private default route は NAT インスタンスの ENI 宛て。既存の `0.0.0.0/0` が旧 NAT Gateway 宛てなら `ReplaceRoute` で張り替え
7. 旧 NAT Gateway が存在すれば削除し、その EIP を解放 (ADR 0007 への移行。ルート張り替え後に実施するため egress は途切れない)
8. WAF IP set / web ACL を冪等に作成
9. SSM `/nakama/_meta/*` に状態を記録

### 8.2 `nakama aws create`

1. 環境チェック (`/nakama/_meta/initialized` 存在)
2. インスタンスタイプの CPU アーキを解決 (`DescribeInstanceTypes` の `SupportedArchitectures`。存在検証も兼ね、不正な型はここで即時エラー) → 対応する Ubuntu 26.04 AMI (amd64 / arm64) を SSM で解決
3. SSM PutParameter で 32 文字パスワードを SecureString として保存
4. `internal/awsx.EC2API.DescribeManagedPrefixLists` で CloudFront prefix list ID 取得
5. SG 作成 → 8080 inbound (prefix list)
6. ENI 作成 (Private subnet 1a)
7. NAT インスタンスを起動 (`ensureNATInstanceRunning`。停止中なら start して `running` まで待機。ADR 0007 の電源連動)
8. `RunInstances` (User Data に code-server インストールスクリプト埋め込み)
9. EC2 が `running` まで待機
10. CloudFront `CreateVpcOrigin` (HTTP:8080)
11. VpcOrigin が `Deployed` まで待機
12. CloudFront `CreateDistribution` (Cache=Disabled, OriginRequestPolicy=AllViewer, WAF=ARN)
13. SSM に meta 情報を書き込み (Distribution `Deployed` 待機の**前**に永続化。待機が timeout しても `delete` で掃除できるようにするため)
14. Distribution が `Deployed` まで待機
15. URL + パスワードを stdout に出力

### 8.3 `nakama aws delete`

1. 確認プロンプト
2. Distribution → disable → wait → delete
3. VpcOrigin → delete
4. EC2 → terminate → wait
5. ENI → delete (terminate 完了後)
6. SG → delete
7. SSM `/nakama/<name>/*` を全削除
8. NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerStateExcluding`。削除対象を除外し、他に稼働中の code-server が無ければ NAT インスタンスも停止。ADR 0007)

### 8.4 `nakama aws backup create`

1. SSM Parameter Store (`/nakama/<name>/meta/instance-id`) で instance-id を解決し、その EC2 のルートボリューム ID を取得
2. ラベルを確定 (`--label` 省略時は取得時刻から生成、例 `2026-05-29-1402`)。`<name>` 内で重複する場合はエラー
3. `CreateSnapshot` (ルートボリューム)。`--note` 指定時はスナップショットの `Description` にも設定 (コンソール表示用)。タグ `nakama:managed/name/component=backup/backup-time=<RFC3339>/backup-label=<label>` と、メモがあれば `nakama:note=<text>` を付与
4. `--keep N` 指定時: 当該 `<name>` のスナップショットを `backup-time` 降順で並べ、N 件超過分を `DeleteSnapshot` (prune)
5. ラベル / 取得時刻 / 状態 / メモを出力 (snapshot-id は `-o json` のみ)

### 8.6 `nakama aws backup note`

1. `--label` で当該 `<name>` のスナップショットを解決
2. `--note` 指定時: `CreateTags` で `nakama:note=<text>` を上書き。空文字/省略時: `DeleteTags` で `nakama:note` を除去
3. 更新後のラベル / メモを出力

> Description はスナップショット作成後に変更できないため、メモ編集の正は常に `nakama:note` タグ側とする。

### 8.5 `nakama aws restore`

1. 対象スナップショットを解決: `--label` 指定時はタグ `nakama:backup-label` で、省略時は `backup-time` 最新のものを選ぶ。利用者は snapshot-id を指定しない
2. 確認 (`--yes` 必須)
3. instance-id を SSM で解決し、現ルートのデバイス名 (例 `/dev/sda1`) と旧ルートボリューム ID / AZ / ボリュームタイプを記録 (stop より前に取得)
4. インスタンスを `stop` → `stopped` まで待機
5. スナップショットから新ボリュームを `CreateVolume` (同 AZ / 同タイプ、暗号化は継承)
6. 旧ルートを `DetachVolume` → 新ボリュームを同デバイス名で `AttachVolume`
7. 旧ルートボリュームを `DeleteVolume`
8. NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerState`。インスタンスは停止のまま残るため、他に稼働中が無ければ NAT インスタンスも停止。ADR 0007)
9. インスタンスは停止のまま終了 (利用者が `start`)

### 8.7 `nakama aws connect`

共通フロー: instance-id を `/nakama/<name>/meta/instance-id` から解決 → モードに応じた `DocumentName`/`Parameters` で `ssm:StartSession` → 返却の `SessionId`/`StreamUrl`/`TokenValue` で取り込んだ plugin の `session.Session` を構築 → `Execute()` で data-channel (WSS) を確立。

| モード | 起動形 | DocumentName | Parameters | session サブタイプ |
|---|---|---|---|---|
| 対話シェル | `connect <n>` | (既定 `SSM-SessionManagerRunShell`) | — | `Standard_Stream` |
| コマンド | `connect <n> <cmd...>` | `AWS-StartNonInteractiveCommand` (既定) / `AWS-StartInteractiveCommand` (`--tty`) | `command` | `Standard_Stream` |
| ポートフォワード | `connect <n> -L L:R` | `AWS-StartPortForwardingSession` | `portNumber`, `localPortNumber` | `Port` |
| 〃 (リモートホスト) | `connect <n> -L L:host:R` | `AWS-StartPortForwardingSessionToRemoteHost` | `host`, `portNumber`, `localPortNumber` | `Port` |

1. `<name>` → instance-id 解決 (未存在ならエラー)
2. モード判定 (シェル / コマンド / ポートフォワードは排他)。`DocumentName` / `Parameters` を組立
3. `ssm:StartSession`
4. credential / region / SSM endpoint を解決 (既存 `awsx` の `aws login` fallback を流用) し `session.Session` を構築
5. シェル・コマンドは端末 raw mode + リサイズ送信 + 制御シグナル転送、ポートフォワードはローカル listener を起動
6. Ctrl-C / EOF / リモート切断で `TerminateSession`

### 8.8 `nakama aws edit --instance-type`

1. instance-id を解決
2. `ec2:DescribeInstanceTypes` で対象タイプの存在検証と CPU アーキ解決 (`SupportedArchitectures`)。無ければ**インスタンスを停止せず即時エラー**
3. 現 state / type を取得。同一タイプなら no-op
4. **アーキ整合チェック**: 現タイプと新タイプの CPU アーキが異なる場合、**インスタンスを停止せず即時エラー** (ルートボリュームはアーキ固有のため in-place 変更不可。別アーキへは `delete` → `create` で移行)
5. running (= stopped 以外) なら `StopInstances` → `stopped` まで待機 (元が running かを記録)
6. `ModifyInstanceAttribute(InstanceType)`
7. 元が running なら `StartInstances` → `running` まで待機して元状態へ復帰
8. `--ebs` とは排他。型変更は停止を伴うため確認プロンプト (`--yes` で抑制)

### 8.9 `nakama aws update`

更新内容: code-server を **latest** へ + `apt-get update && apt-get dist-upgrade -y` (非対話)。更新前に自動バックアップを取得し、必要なら start/stop/reboot を nakama が管理する。

1. instance-id を解決。現 state を取得し **running / stopped 以外は即エラー** (`wasStopped := state==stopped`)
2. 確認プロンプト (`--yes` で抑制)
3. **自動バックアップ**: `CreateBackup`(label `preupdate-<UTC yyyy-mm-dd-HHMM>`, note `"auto backup before nakama aws update"`, `--keep` 連動)。スナップショット完了は待たない (point-in-time は API 呼出時点で確保)。電源状態は変えずに取得
4. **NAT インスタンス起動** (`ensureNATInstanceRunning`): 更新はインターネットから apt / code-server を取得するため egress が必要。start の前に無条件で NAT を起動 (ADR 0007)。続いて `wasStopped` なら `StartInstances` → `running` 待ち → **SSM エージェント online 待ち** (`SendCommand` を `InvalidInstanceId` の間リトライ)
5. SSM Run Command (`AWS-RunShellScript`, `executionTimeout` 延長) で 1 スクリプト実行:
   - `apt-get update` → `apt-get dist-upgrade -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold` (`DEBIAN_FRONTEND=noninteractive`)
   - code-server `install.sh`(latest) → `systemctl restart code-server@ubuntu`
   - `code-server --version` を `/nakama/<name>/code-server/version` へ書き戻し (best-effort)
   - 末尾に `/var/run/reboot-required` の有無を `NAKAMA_REBOOT_REQUIRED=0|1` として stdout 出力
6. **reboot 判定**: 元 running かつ `NAKAMA_REBOOT_REQUIRED=1` のときのみ `RebootInstances`。元 stopped は reboot せず (手順 7 の stop と次回 start で新カーネルが反映される)
7. `wasStopped` なら `StopInstances` → `stopped` 待ち。**手順 5 が失敗しても電源状態は元へ戻す** (バックアップ済のため復旧可能)。その後 NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerState`。復元後の電源状態に対し、他に稼働中が無ければ NAT も停止。best-effort。ADR 0007)
8. 結果出力: 更新後 code-server version / backup label / reboot 実施有無 / (元 stopped 時) 次回起動で反映の注記

> 設計判断の根拠は ADR 0006 を参照。

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

## 12. `nakama aws connect` の取り込み方針 (案3)

詳細根拠は [adr/0005-aws-connect-via-session-manager.md](adr/0005-aws-connect-via-session-manager.md)。

- AWS 公式 `aws/session-manager-plugin` は Go module 未公開 (リポジトリ root に go.mod が無く `vendor/` 方式)。そのため **特定 commit/tag のソースを `third_party/session-manager-plugin/` にコピーして版固定**する (案3)。
- コピーには go.mod を付与し依存 (aws-sdk-go-v2 / gorilla/websocket / twinj/uuid / golang.org/x/crypto / seelog 等) を pin。nakama の go.mod に下記を追加し、公式 import パスを保ったままローカル固定する:

```
require github.com/aws/session-manager-plugin v1.x.x   // 取り込み版に対応する placeholder
replace github.com/aws/session-manager-plugin => ./third_party/session-manager-plugin
```

- 取り込むパッケージ: `src/sessionmanagerplugin/session` (+ `shellsession` / `portsession`) / `datachannel` / `communicator` / `message` / `encryption` / `log` / `config` / `sessionutil` / `retry` / `websocketutil` / `version`。`ssmcli` 系は取り込まない。
- Apache-2.0 のため取り込みツリーに `LICENSE` / `NOTICE` を保持。取り込み元 commit は ADR 0005 に記録。
- 追加実装:
  - `internal/awsx`: `SSMAPI` に `StartSession` / `TerminateSession` を追加し、解決済 `aws.Credentials` + region + SSM endpoint を `session.Session` 構築用に取り出す口を設ける
  - `internal/service/connect.go`: instance-id 解決、モード→`DocumentName`/`Parameters` 組立、`StartSession`、plugin セッション起動
  - `internal/cli/aws_connect.go`: cobra コマンド (`-L` / `--tty` / 位置引数、`SetInterspersed(false)`)、`aws.go` の `AddCommand` に配線
- テスト方針: argv/Parameters 組立と引数解析はピュア関数化してユニットテスト。実セッションは手動 E2E (CI 非搭載)。
