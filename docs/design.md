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
  /nakama/<name>/meta/iam-role          (String, --with-admin/--with-role 時のみ)
  /nakama/<name>/meta/instance-profile  (String, --with-admin/--with-role 時のみ)
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
│   ├── browser/                 // ブラウザ起動 (detached, 注入可能な Launcher。ADR 0012)
│   ├── naming/                  // リソース命名規約
│   ├── tagging/                 // タグスキーマ
│   ├── profilestore/            // マルチアカウントプロファイルの永続化 (~/.config/nakama, ADR 0011)
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
| IAM Role (NAT インスタンス用, SSM core のみ) | `nakama-nat-role` |
| IAM Instance Profile (NAT インスタンス用) | `nakama-nat-instance-profile` |
| IAM Instance Role | `nakama-instance-role` |
| IAM Instance Role (admin, `--with-admin`) | `nakama-instance-role-admin` |
| IAM Instance Profile (admin, `--with-admin`) | `nakama-instance-profile-admin` |
| IAM Instance Profile (`--with-role <role>` 用に新設する場合) | `nakama-instance-profile-<role>` |
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
| `STSAPI` | `GetCallerIdentity` (AWS アカウント ID / 呼び出し元 ARN 解決。ARN は `--with-admin`/`--with-role` プリフライトの PolicySourceArn に使用) |
| `IAMAPI` | Role, InstanceProfile, AttachRolePolicy, PutRolePolicy, SimulatePrincipalPolicy, ListInstanceProfilesForRole, ListAttachedRolePolicies |
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
    Name       string `json:"name"`           // nakama:name (帰属インスタンス)
    Label      string `json:"label"`
    Note       string `json:"note,omitempty"`
    CreatedAt  string `json:"created_at"`     // nakama:backup-time
    State      string `json:"state"`          // pending|completed|error
    SizeGiB    int32  `json:"size_gib"`
    SnapshotID string `json:"snapshot_id"`    // table 出力では非表示
}

func (s *Service) CreateBackup(ctx, CreateBackupInput) (*Backup, error)
func (s *Service) ListBackups(ctx, name string) ([]Backup, error)
func (s *Service) ListAllBackups(ctx) ([]Backup, error) // 全インスタンス横断 (backup list 名前省略時)
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
2. (`--with-admin` / `--with-role` 指定時のみ) **IAM ロール解決 + プリフライト + upsert** (ADR 0010):
   - 対象ロール / インスタンスプロファイルと現在の存在状態を解決 (`--with-admin` は `nakama-instance-role-admin` / `nakama-instance-profile-admin`。`--with-role` は `iam:GetRole` で存在確認・trust policy 検証・`iam:ListInstanceProfilesForRole` で既存プロファイル再利用判定)
   - `iam:SimulatePrincipalPolicy` で `iam:PassRole` (+ 未作成分の Create/Attach/Add 系アクション) を検査。拒否が 1 つでもあれば**何もリソースを作らずエラー** (Simulate 自体の失敗も fail-closed)。PolicySourceArn は GetCallerIdentity の ARN (assumed-role は IAM role ARN へ正規化)
   - ロール / プロファイルを冪等に upsert (`--with-role` の既存プロファイル再利用時は no-op。ユーザーロールへの書き込みは一切しない)
3. インスタンスタイプの CPU アーキを解決 (`DescribeInstanceTypes` の `SupportedArchitectures`。存在検証も兼ね、不正な型はここで即時エラー) → 対応する Ubuntu 26.04 AMI (amd64 / arm64) を SSM で解決
4. SSM PutParameter で 32 文字パスワードを SecureString として保存
5. `internal/awsx.EC2API.DescribeManagedPrefixLists` で CloudFront prefix list ID 取得
6. SG 作成 → 8080 inbound (prefix list)
7. ENI 作成 (Private subnet 1a)
8. NAT インスタンスを起動 (`ensureNATInstanceRunning`。停止中なら start して `running` まで待機。ADR 0007 の電源連動)
9. `RunInstances` (User Data に code-server インストールスクリプト埋め込み。User Data は AWS CLI v2 を snap (`snap install aws-cli --classic`) で導入する。Ubuntu の apt `awscli` は v1 と古いため purge し、`/snap/bin/aws` を絶対パス参照 + `/usr/local/bin/aws` へ symlink して非ログイン shell からも解決させる)。IAM instance profile は手順 2 で選択したもの (フラグ省略時は共有ベースプロファイル `nakama-instance-profile`)
10. EC2 が `running` まで待機
11. CloudFront `CreateVpcOrigin` (HTTP:8080)
12. VpcOrigin が `Deployed` まで待機
13. CloudFront `CreateDistribution` (Cache=Disabled, OriginRequestPolicy=AllViewer, WAF=ARN)
14. SSM に meta 情報を書き込み (Distribution `Deployed` 待機の**前**に永続化。待機が timeout しても `delete` で掃除できるようにするため)。`--with-admin` / `--with-role` 時は `meta/iam-role` / `meta/instance-profile` も記録
15. Distribution が `Deployed` まで待機
16. URL + パスワード (+ 付与した IAM ロール / プロファイル) を stdout に出力

### 8.3 `nakama aws delete`

1. 確認プロンプト
2. Distribution → disable → wait → delete
3. VpcOrigin → delete
4. EC2 → terminate → wait
5. 孤児 EBS ボリュームの掃除: `nakama:managed` + `nakama:name=<name>` + `nakama:component=ebs` タグ付きで `available` のボリュームを削除 (過去の restore が DeleteOnTermination=false のまま残したルートボリューム対策。terminate 直後の detach 遷移は短時間ポーリングで追従し、in-use のボリュームは削除しない)
6. ENI → delete (terminate 完了後)
7. SG → delete
8. SSM `/nakama/<name>/*` を全削除
9. NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerStateExcluding`。削除対象を除外し、他に稼働中の code-server が無ければ NAT インスタンスも停止。ADR 0007)

### 8.4 `nakama aws backup create`

1. SSM Parameter Store (`/nakama/<name>/meta/instance-id`) で instance-id を解決し、その EC2 のルートボリューム ID を取得
2. ラベルを確定 (`--label` 省略時は取得時刻から生成、例 `2026-05-29-1402`)。`<name>` 内で重複する場合はエラー
3. `CreateSnapshot` (ルートボリューム)。`--note` 指定時はスナップショットの `Description` にも設定 (コンソール表示用)。タグ `nakama:managed/name/component=backup/backup-time=<RFC3339>/backup-label=<label>` と、メモがあれば `nakama:note=<text>` を付与
4. `--keep N` 指定時: 当該 `<name>` のスナップショットを `backup-time` 降順で並べ、N 件超過分を `DeleteSnapshot` (prune)
5. ラベル / 取得時刻 / 状態 / メモを出力 (snapshot-id は `-o json` のみ)

### 8.6 `nakama aws backup note`

1. `--label` で当該 `<name>` のスナップショットを解決
2. `--note <text>` 指定時: `CreateTags` で `nakama:note=<text>` を上書き。空文字明示時: `DeleteTags` で `nakama:note` を除去 (`--note` の省略は CLI がエラーにする)
3. 更新後のラベル / メモを出力

> Description はスナップショット作成後に変更できないため、メモ編集の正は常に `nakama:note` タグ側とする。

### 8.5 `nakama aws restore`

1. 対象スナップショットを解決: `--label` 指定時はタグ `nakama:backup-label` で、省略時は `backup-time` 最新のものを選ぶ。利用者は snapshot-id を指定しない
2. 確認 (`--yes` 必須)
3. instance-id を SSM で解決し、現ルートのデバイス名 (例 `/dev/sda1`) と旧ルートボリューム ID / AZ / ボリュームタイプを記録 (stop より前に取得)
4. インスタンスを `stop` → `stopped` まで待機
5. スナップショットから新ボリュームを `CreateVolume` (同 AZ / 同タイプ、暗号化は継承)
6. 旧ルートを `DetachVolume` → 新ボリュームを同デバイス名で `AttachVolume`
7. `ModifyInstanceAttribute` で新ルートに `DeleteOnTermination=true` を再設定 (起動後アタッチのボリュームは既定 false のため。これを怠ると `delete` 後にルート EBS が available のまま残存する)
8. 旧ルートボリュームを `DeleteVolume`
9. NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerState`。インスタンスは停止のまま残るため、他に稼働中が無ければ NAT インスタンスも停止。ADR 0007)
10. インスタンスは停止のまま終了 (利用者が `start`)

### 8.7 `nakama aws connect`

共通フロー: instance-id を `/nakama/<name>/meta/instance-id` から解決 → モードに応じた `DocumentName`/`Parameters` で `ssm:StartSession` → 返却の `SessionId`/`StreamUrl`/`TokenValue` で取り込んだ plugin の `session.Session` を構築 → `Execute()` で data-channel (WSS) を確立。セッション本体の実行は hidden な子プロセス (`session-runner`) が担い、恒久切断時は親が自動再接続する (§8.11 / ADR 0014)。

| モード | 起動形 | DocumentName | Parameters | session サブタイプ |
|---|---|---|---|---|
| 対話シェル | `connect <n>` | `AWS-StartInteractiveCommand` | `command=exec sudo su - ubuntu` | `Standard_Stream` |
| コマンド | `connect <n> <cmd...>` | `AWS-StartNonInteractiveCommand` (既定) / `AWS-StartInteractiveCommand` (`--tty`) | `command=sudo su - ubuntu -c '<cmd>'` | `Standard_Stream` |

ポートフォワードは `nakama aws tunnel` に一本化した (§8.12 / ADR 0017)。

1. `<name>` → instance-id 解決 (未存在ならエラー)
2. モード判定 (シェル / コマンドは排他)。`DocumentName` / `Parameters` を組立
3. `ssm:StartSession`
4. credential / region / SSM endpoint を解決 (既存 `awsx` の `aws login` fallback を流用) し `session.Session` を構築
5. シェル・コマンドは端末 raw mode + リサイズ送信 + 制御シグナル転送
6. Ctrl-C / EOF / リモート切断で `TerminateSession`

> ログインユーザーは **ubuntu**(code-server の稼働ユーザー)。Session Manager は `ssm-user` として接続するが、既定で付与される passwordless sudo (`/etc/sudoers.d/ssm-agent-users` の `ssm-user ALL=(ALL) NOPASSWD:ALL`) を用いて ubuntu に切り替える。対話シェルは `exec sudo su - ubuntu` で ssm-user シェルを置換するため、単一の `exit` でセッションが終了・切断する。コマンドは `sudo su - ubuntu -c '<cmd>'`(内側はシングルクォートで安全化)で ubuntu 実行。ポートフォワードはユーザーコンテキスト不要のため対象外。詳細は ADR 0009。

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

### 8.10 `nakama aws open`

code-server の URL をブラウザで開き、パスワードログインを自動通過する
(根拠と代替案の比較は ADR 0012)。code-server の認証は cookie のみで
URL トークン認証が存在せず、CLI はブラウザの cookie store に書き込めない
ため、「ブラウザ自身に `/login` へ top-level form POST させる」方式を取る。

1. **preflight**: `GetInstanceInfo(ctx, name, showPassword)` で情報取得
   (`--no-auth` 時は showPassword=false)。パスワード読取のみが失敗した
   場合は showPassword=false で再取得し、成功すれば自動ログインだけを
   スキップして継続 (METHOD C fallback)。インスタンス未存在は即エラー
2. **fatal チェック**: distribution domain 未設定 → エラー (provisioning
   中の可能性を案内)。state が running 以外 → エラー
   (`nakama aws start <name>` を案内)
3. **WAF soft check**: `IPDiscoverer.PublicIPv4` + `ListACL` で呼び出し元
   IP が allowlist に**CIDR containment** (net/netip) で含まれるか確認。
   未収載なら stderr に警告 (`nakama aws acl add` を案内)。チェック自体の
   失敗は debug ログのみで続行 (絶対に open を壊さない)
4. **METHOD A (既定)**: crypto/rand 128 bit の URL-safe トークンを採番し、
   `127.0.0.1:0` (カーネル割当ポート) に one-shot HTTP サーバを起動。
   `/<token>` だけが自動 submit フォーム
   (`POST https://<domain>/login?to=/`、hidden の `password` / `base=.` /
   `href=https://<domain>/login` は code-server の対話ログインフォームと
   同一構成) を **1 回だけ**返す (他パスは 404、再取得は 410 Gone)。
   `Cache-Control: no-store` / `Referrer-Policy: no-referrer` 付与、
   html/template でエスケープ、外部リソース無し。ブラウザを one-shot URL
   で起動し、ページ配信を待つ (上限 `--timeout`, 既定 30s)。配信後は
   猶予付き Shutdown。JS 無効環境向けに可視の submit ボタンも置く
5. **METHOD C fallback**: パスワード取得失敗 / listener bind 失敗 /
   ランチャー不在・起動失敗 (`errors.Is(err, exec.ErrNotFound)` で判別) /
   timeout 内未取得のいずれでも、プレーン URL を開く (ランチャー不在時は
   stdout に表示する) 動作へ退避。理由と
   `nakama aws info <name> --show-password` の案内を stderr に出し、
   URL を開けた/表示できた場合は終了コード 0
6. **フラグ**: `--no-auth` (METHOD C を明示利用)、`--print-url`
   (ブラウザ非起動。auth 有効時は plain URL と one-shot URL の両方を表示し
   timeout までサーバ維持。headless / SSH 向け)、`--browser <cmd>`
   (起動コマンド上書き。優先度 `--browser` > `$BROWSER` > OS 既定
   (darwin=`open`, linux=`xdg-open`, windows=`rundll32` best effort))、
   `--timeout <duration>`
7. **出力**: `-o json|yaml` は name / url / method (`auto-login`|`plain`) /
   opened / auto_login (`completed`|`skipped`|`timeout`) / note の
   構造体。**パスワードはいかなる出力にも含めない**

実装: `internal/cli/aws_open.go` (one-shot サーバ含む)、
`internal/browser` (detached なブラウザ起動、`Launcher` interface で注入
可能)。テストは fake backend / fake launcher / httptest 相当のローカル
listener で AWS・実ブラウザ・外部ネットワークに触れない。

> CloudFront 経由で form POST が実際に cookie を確立するかの E2E は
> 実 AWS 未検証 (ADR 0012 の UNVERIFIED 項参照)。

### 8.11 `nakama aws connect` の自動再接続 (ADR 0014)

vendored plugin は正常終了・恒久切断のいずれも `os.Exit(0)` またはプロセス宙吊りで終える設計のため、切断検知は**親プロセス監督方式 (child re-exec)** で行う。

```
[nakama aws connect]                     [nakama aws session-runner (hidden, 子プロセス)]
  ├ instance-id 解決
  ├ loop:
  │   ├ ssm:StartSession
  │   ├ 子を spawn ──────────────────────► fd 3 から payload JSON (SessionId/StreamUrl/
  │   │   ├ stdin/stdout/stderr 継承        TokenValue/region/endpoint/instance-id) を読取
  │   │   ├ payload は fd 3 (pipe) 経由     awsx.NewClients → plugin Session を Execute
  │   │   └ env: SSM_PLUGIN_DISCONNECT_     (SSM_PLUGIN_SKIP_CLIENT_CONFIGURE=true)
  │   │        EXIT_CODE=74 (--retry>0 時)   ├ 正常終了 → exit 0
  │   ├ 子の実行中 SIGINT/SIGTSTP/SIGQUIT     ├ 恒久切断 (timeout/リトライ枯渇/再送
  │   │   を無視 (終了後に復元)               │  timeout) → exit 74 (env ゲートパッチ)
  │   └ 子の exit code で分類:               └ その他エラー → exit 1
  │       0  → 正常終了 (ループ終了)
  │       74 → 恒久切断: 60s 以上生存していたら試行カウンタをリセット →
  │            予算内なら通知 + 指数バックオフ (1s,2s,4s,8s,16s,30s cap) → StartSession から再実行
  │       他 → ExitCodeError としてそのまま伝播 (cmd/nakama が同一コードで exit)
  └ StartSession 失敗の分類 (smithy APIError コード):
      認証系 (ExpiredToken 等) → 中断し `aws login` を案内 /
      TargetNotConnected → 予算内でバックオフ再試行 / その他 → 即エラー
```

- TokenValue は秘匿値のため argv・環境変数・ディスクに置かず、inherited pipe でのみ受け渡す
- payload 受け渡しと親のシグナル処理は OS でメカニズムが異なる (ADR 0015。`connect_session_unix.go` / `connect_session_windows.go` に分割):
  - **unix**: pipe の読み口を `ExtraFiles` で fd 3 として継承 (上図)。親は子の実行中 SIGINT/SIGTSTP/SIGQUIT を無視し、ctx キャンセル時は SIGTERM で子を停止
  - **windows**: `ExtraFiles` 非対応のため、pipe の読み口ハンドルに `HANDLE_FLAG_INHERIT` を付与して `SysProcAttr.AdditionalInheritedHandles` で継承し、**ハンドル番号 (非秘匿の整数) のみ**を環境変数 `NAKAMA_SESSION_PAYLOAD_HANDLE` で子に通知 (payload 本体は pipe の中だけ)。親は SIGINT (^C) のみ無視 (SIGTSTP/SIGQUIT は存在しない)、ctx キャンセル時は `Kill()` で子を停止
- `--retry` 既定: シェル = 5、コマンドモード = 0 (明示指定はモード不問で尊重)。`--retry 0` では env ゲートを設定せず spawn するため子の挙動は従来の一発実行と同一
- 監督ループは `runSession` / `sleep` / `now` / `notify` を関数フィールド注入可能にし、実プロセス・実 AWS なしでユニットテスト (`internal/service/connect_reconnect_test.go`)
- vendored パッチの一覧は `third_party/session-manager-plugin/PINNED.md` の「Local patches」節を参照

### 8.12 `nakama aws tunnel` — バックグラウンド常駐ポート転送 (ADR 0017)

`connect -L` を置き換える常駐型ポート転送。プロセスは 3 層: CLI (即終了) → デーモン (hidden `tunnel-daemon`, detached) → 転送ごとの session-runner 子 ×N。

```
[nakama aws tunnel [start] <n> -L...]   [nakama aws tunnel-daemon (hidden, detached)]
  ├ preflight:                            ├ 状態ファイル O_CREATE|O_EXCL 作成 (~/.config/
  │   ├ ValidateInstanceName              │   nakama/tunnels/<n>.json, 0600: PID/制御
  │   ├ 既存デーモン liveness              │   ポート/token/転送一覧/開始時刻)
  │   │   (状態ファイル + 制御 ping。      ├ 制御チャネル listen (127.0.0.1:0, HTTP)
  │   │    生存なら一覧表示 + exit 1、     │   ├ GET /status (Bearer token, 定数時間比較)
  │   │    stale なら削除して続行)         │   └ POST /stop (同上) → graceful shutdown
  │   ├ instance-id 解決                  ├ -L ごとに監督 goroutine (§8.11 の一般化):
  │   └ ローカルポートの空き検査           │   loop: ssm:StartSession (Port ドキュメント)
  ├ tunnel-daemon を detach 起動           │        → session-runner spawn (payload pipe /
  │   (unix: Setsid / windows:            │           exit 74 ゲート。§8.11 と同一機構)
  │    DETACHED_PROCESS)                  │        → 恒久切断: バックオフ再接続 (予算 5,
  ├ readiness ポーリング                   │           60s 安定でリセット) / 認証エラー:
  │   (状態ファイル + 制御 ping,           │           当該転送を dead(auth) に遷移
  │    タイムアウト付き)                   ├ ログ: unix=syslog / windows=Event Log (OS に
  └ 転送一覧を表示して即終了               │   ローテーション委譲)。子の stdout/stderr も回収
                                          └ stop: 全子終了 → TerminateSession (best-effort)
                                             → 状態ファイル削除 → exit
```

- **SSM ドキュメント**: `-L L:R` → `AWS-StartPortForwardingSession` (`portNumber`, `localPortNumber`)、`-L L:host:R` → `AWS-StartPortForwardingSessionToRemoteHost` (+`host`)。1 セッション 1 ポートのため複数 `-L` はセッションを転送数ぶん並行に張る。ローカル listener は vendored plugin (session-runner 子) が bind する
- **一般エラー終了の再試行** (Issue #45): exit 74 ゲートは「確立済みチャネルの喪失」にしか発火せず、再接続時のセッション確立失敗 (`Execute` → `OpenDataChannel` エラー。スリープ復帰直後の WSS dial 失敗など) は一般エラー (exit 1) になる。tunnel の forward 監視は `RetryChildFailures` で非 0・非 74 の子終了も同じ再接続予算の対象とする (バックオフ・60s 安定リセットも共通)。予算枯渇時は dead (Detail に exit code と枯渇を明記)。`connect` はこのフラグを使わない — 子の終了コードはリモートコマンドの終了コードとして無変更で伝搬する必要がある (§8.11 / ADR 0014)
- **in-place resume の資格情報** (Issue #49): vendored plugin の ResumeSession 制御プレーンとデータチャネル再署名は上流では default 資格情報チェーンを使うため、nakama login のみの環境では resume が構造的に失敗する。session-runner が `NakamaAWSConfigProvider` (NAKAMA PATCH) に nakama 解決済み `aws.Config` (プロキシ対応 HTTPClient 込み) を注入し、WebSocket 断のたびにフルセッション置換へ落ちずに in-place resume が機能するようにする (詳細は third_party/session-manager-plugin/PINNED.md)
- **stop の経路**: 制御チャネル経由のグレースフル停止が正 (Windows の外部 Kill は子を道連れにしないため)。unix ではデーモンが SIGTERM でも同じ停止経路に合流する
- **横断一覧と提案** (Issue #55): `tunnel status` の名前省略時は状態ファイルディレクトリを走査し (`listTunnelStateNames`)、各インスタンスへ `GetTunnelStatus` を実行して NAME 列付きで一覧する。デーモン不応答は `NoTunnelError` として stale 表示 (単一指定時と同じ掃除が働く)。`tunnel suggest <n>` は `runRemoteScript` で `ss -ltnH` (無ければ `netstat -ltn`) を実行し、ポート重複の畳み込み・プロセス名付きエントリ優先・昇順ソートを経て `-L` 案を出す。22 番は `ssh` の案内に回し提案コマンドから除外。`suggest` も曖昧性回避のため予約インスタンス名に追加
- **status**: CLI が状態ファイル → `GET /status` で転送ごとの {spec, state (active/reconnecting/dead), 直近エラー, 開始時刻, 再接続消費数} を取得して表示。デーモン不在 (ping 不達) なら stale 掃除の上「転送なし」を表示
- **restart**: 状態ファイルに記録された転送スペックのまま stop → start を合成 (`-L` は受け付けない。変更は stop → start で)。スペックの再検証は stop の**前**に行い、壊れた状態ファイルのために生きているデーモンを止めない。新デーモンは CLI プロセスの現行資格情報チェーンで起動するため、資格情報失効で dead(auth) になった転送の復旧手段 (`nakama aws login` → `tunnel restart <n>`)。状態ファイルが無い場合はエラー (推測で張らない)
- **予約名**: `start` / `stop` / `status` / `restart` は `tunnel <name>` 省略形と曖昧になるため create の新規名として拒否 (`service.ValidateNewInstanceName`。既存インスタンス操作の `ValidateInstanceName` は不変)
- **転送量計測は非対応** (要件から除外。ADR 0017 Alternatives 6)

### 8.13 `nakama aws cli shell` — aws CLI 用の設定済みサブシェル (ADR 0018)

aws CLI は OS のプロキシ設定 (WinHTTP/PAC) も Windows 証明書ストアも読まないため、企業の TLS インスペクション環境では既定で動かない。nakama の検出能力 (ADR 0015) を aws CLI の公式構成面 (環境変数 + `AWS_CA_BUNDLE`) に変換し、それを注入したサブシェルを提供する。Windows 専用 (他 OS では Windows 専用オプションである旨を表示してエラー)。

- **検出エンジン** (`internal/cliproxy`): ADR 0015 の解決器でプロキシを解決 → STS エンドポイントへ TLS プローブ → **OS ストア検証済みチェーン**のルートを同梱の Amazon Trust Services (ATS) ルート 5 枚 (取得時に 2 ソース照合、テストで SHA-256 をピン留め) と照合 → ATS 以外なら TLS インスペクションと判定し、企業 CA チェーン (リーフ除く) + ATS ルートを `~/.config/nakama/aws-ca-bundle.pem` (0600) へ書き出す。ATS 同梱は「`AWS_CA_BUNDLE` は既定バンドルの置き換え」であることへの対策 (インスペクション除外エンドポイントの検証を壊さない)。最後に生成した設定そのもので疎通を再確認しバナー表示する
- **env 注入**: `HTTP_PROXY`/`HTTPS_PROXY` (解決したプロキシ。直結なら設定しない)、`NO_PROXY` (既存値へ localhost 系をマージ)、`AWS_CA_BUNDLE` (インスペクション時のみ)、`NAKAMA_AWS_CLI_SHELL=1` (ネスト防止マーカー)。既存の同名変数は大文字小文字を無視して置換する
- **サブシェル**: 親プロセスの実体 (`QueryFullProcessImageName`) が powershell.exe / pwsh.exe / cmd.exe ならそれを、不明なら `ComSpec` を起動 (`--shell` で明示指定可)。stdio 継承で同一ウィンドウ内、シェルの exit code は伝搬。親シェル・レジストリ・ユーザー環境変数・`~/.aws/config` (ADR 0011/0016 の不変条件) には一切変更を残さない
- **プロキシ認証の診断**: CONNECT が 407 を返す場合は `Proxy-Authenticate` のスキームを読み、Basic あり → プロキシ URL への資格情報埋め込みを案内 / NTLM・Negotiate のみ → ローカル認証リレー (px / Cntlm) を案内してエラー終了 (aws CLI は Basic のみ対応のため)

### 8.14 `nakama aws ssh` — ssh over Session Manager (ADR 0019)

デスクトップ版 VS Code (Remote-SSH) / JetBrains Gateway / 素の ssh・scp・rsync を、受信ポート開放なしで使えるようにする。

- **`ssh proxy <n>`**: `AWS-StartSSHSession` (`portNumber=22`) でセッションを開き、**自プロセスの stdin/stdout** をデータチャネルに中継する (vendored plugin の `StandardStreamForwarding`)。OpenSSH の `ProxyCommand` から使う。supervisor (§8.11 / ADR 0014) は使わない — ProxyCommand プロセスの寿命と再接続は ssh クライアントの責務。session-runner 子も挟まない (実 stdio を掴む必要があるため)。恒久切断時に hang しないよう `SSM_PLUGIN_DISCONNECT_EXIT_CODE` をプロセス内で設定し、即終了して ssh に接続断を伝える (Issue #61)。プラグインの開始バナーは ssh が識別文字列より前の行を読み飛ばす仕様 (RFC 4253 §4.2) により無害
- **`ssh setup <n>`**: 公開鍵 (`~/.ssh/id_ed25519.pub` 等を自動検出、`--identity` で明示) を SSM Run Command で ubuntu の `authorized_keys` に冪等追加し (`grep -qxF` で既存判定)、ssh config スニペットを `~/.config/nakama/ssh/<n>.conf` (0600) に書き出して `Include` 行を案内する。**`~/.ssh/config` は変更しない** (`~/.aws/config` 不変更と同じ思想)。多行入力 (秘密鍵の誤指定) と非 OpenSSH 公開鍵形式は拒否し、エラーに鍵素材を出さない
- **リモートコマンド実行の共通化**: `runRemoteScript` (SSM Run Command + `InvalidInstanceId` リトライ + stdout 回収) を service 層に用意し、鍵投入で使用する (`update` の既存経路と同型)

### 8.15 `nakama aws doctor` — 環境自己診断 (Issue #53)

企業環境での「動かない」の一次切り分けを 1 コマンドに集約する。**読み取り専用**が不変条件 (AWS リソース・ローカル設定を一切変更しない)。

- **構成**: `service.Diagnose(ctx, DoctorOptions)` が `DoctorReport{Checks[], OKs/Warns/Fails/Skips}` を返し、CLI が表形式または `-o json` で描画して fail>0 なら `ExitCodeError{1}`
- **資格情報未解決でも動く**: CLI は `awsx.NewClients` の失敗を診断結果 (credentials=fail) として扱い、ローカル診断 (proxy / tls) を継続する。AWS 依存項目は skip (終了コードに影響しない)
- **各項目の実装**: proxy/tls = ADR 0018 の `cliproxy.Detector` を再利用 (`ProxyAuthError` は方式別の案内に変換) / credentials = `Clients.AccountID`・`CallerARN` + profilestore の active profile / waf-allowlist = `ListACL` + `IPDiscoverer.PublicIPv4` を CIDR 包含判定 / capability = 後述の権限マトリクス / tunnels = 状態ファイル走査 + `GetTunnelStatus`
- **機能別 capability 判定 (Issue #63)**: `internal/service/permissions.go` の `featurePermissionMatrix` が「機能 → 必要 IAM アクション」の唯一の情報源。doctor は Simulate 対象行のアクション和集合を `SimulatePrincipalPolicy` にチャンク (50) + Marker ページネーションで問い合わせ、機能ごとに ok / warn (不足アクション列挙) を 1 行ずつ表示する。不足は warn であり fail ではない (意図して権限を絞ったプリンシパルは「壊れた環境」ではない。fail は環境異常に予約)。判定は Resource `*` 評価のためリソース限定ポリシーで偽陰性があり得る旨を Advice に含める
- **マトリクスの鮮度保証**: 双方向の乖離検出テスト (permissions_test.go)。順方向 = パッケージソースから `.clients.<SVC>.<Method>(` を抽出し、対応アクションが表に無ければ失敗。逆方向 = 表のアクションが抽出されず `implicitActions` (ResumeSession = vendored plugin 内部 / PassRole = EC2 が要求 / GetCallerIdentity = awsx 直呼び) にも無ければ失敗。暗黙宣言自体も「抽出されるようになったら失敗」で陳腐化を検出。requirements.md 付録 A は生成ブロックで、表とドキュメントの不一致も CI が検出する (`NAKAMA_UPDATE_PERMISSIONS_DOCS=1` で再生成)
- **テスト容易性**: 検出・ACL・IP・tunnel 一覧/状態・profile を `DoctorOptions` の unexported フックで差し替え、ネットワークなしで全分岐を検証する

### 8.16 プロビジョニング宣言 (`create --setup` / `--dotfiles` / `--preset`、Issue #54)

- **適用点**: `CreateInstance` の最終段 (distribution が Deployed になり、メタデータ書き込み済みの後)。**best-effort** — 失敗しても create は成功扱いで `CreateInstanceOutput.Provision` に結果を載せ、CLI が警告 + ログパス + `setup rerun` コマンドを表示する (インスタンスは作成済みで使用可能なため)
- **事前検証**: CLI が `ValidateProvisionSpec` を **AWS 呼び出し前**に実行し、未知の preset・読めないローカルスクリプト・http URL (経路上での改変を許すため拒否) を即座に弾く
- **実行シェルの前提**: aws:runShellScript は commands を `_script.sh` に書き出し**ファイルとして** `sh` 起動する (amazon-ssm-agent の runshellscript.go で確認)。つまり shebang が解釈器を決め、shebang の無いスクリプトは /bin/sh (Ubuntu では dash) で動く。bash 構文 (process substitution・pipefail) を使うスクリプトは必ず `#!/bin/bash` で始めること (Issue #61)
- **リモートスクリプト**: `runRemoteScript` (§8.14 と共通) で 1 本のスクリプトを実行。全出力を `/var/log/nakama-provision.log` に tee し、各ステップを `NAKAMA_STEP_BEGIN/OK/FAILED` マーカーで囲んでサブシェル実行する (1 ステップの失敗が他を隠さない・中断しない)。ステップ順は preset → dotfiles → setup (後段が前段の成果に依存できる)
- **各ステップ**: preset = 同梱レシピ (`kiro` は `internal/kiro.InstallScript()` をそのまま使う。`sudo -u ubuntu -H` で実行) / dotfiles = `~/.dotfiles` へ clone (既存なら `pull --ff-only` で再実行可) → `install.sh`/`setup.sh`/`bootstrap.sh` があれば実行、無ければドットファイルを `$HOME` へリンク / setup = https は `curl` 取得、ローカルは**引用付き heredoc** で内容を転送 (外側シェルの展開を防止。マーカー衝突は拒否)
- **`setup rerun`**: 同じ `Provision` を既存インスタンスに適用する復旧・再適用手段。冪等性はスクリプト作成者の責務であることを docs に明記

### 8.17 `nakama aws create --bare` — 軽量開発ボックス (Issue #52)

§8.14 の ssh 経路が入ったことで、ブラウザ IDE を使わない構成が実用になった。`--bare` は code-server 一式を作らない。

- **作らないもの**: パスワード (生成も SSM 保存もしない)、code-server の user-data、private DNS 解決、VPC Origin、CloudFront distribution とその Deployed 待ち。**作成時間の短縮はこの待ちの省略による**
- **user-data**: `internal/userdata/bare.tmpl.sh` (SSM エージェントの起動確認・sshd 有効化・aws CLI と基本ツールの導入のみ)。`connect` / `ssh` / `tunnel` が成立する最小構成
- **種別の記録**: `/nakama/<name>/meta/kind = bare`。**未記録は従来の code-server インスタンス**として扱うため、既存インスタンスへの移行処理は不要。空値の PutParameter は SSM が拒否するため、bare では distribution 系のメタキー自体を書かない
- **各コマンドの対応**: `list` は kind を読んで TYPE=BARE 表示 / `info` は Kind を返し bare ではパスワードを読まない / `open` は bare を検出して `ssh setup`・`connect` を案内するエラー / `update` は OS 更新のみのスクリプト (reboot マーカーは共通) / `delete` は空の distribution・VPC Origin メタを既にスキップする構造だったためそのまま動作 / NAT 電源連動は通常インスタンスと同一 (egress が必要)

### 8.18 Kiro CLI / Kiro Crew の既定導入 (Issue #68)

Issue #20 の内容を `--preset kiro` (§8.16) として一般化していたが、常用構成であるため user-data での既定導入に変更した。

- **単一の情報源**: 導入手順は `internal/kiro/install.sh` に 1 つだけ置き、`go:embed` で `internal/kiro.InstallScript()` として公開する。user-data (通常・bare の両テンプレート) と `--preset kiro` の双方がこの同一文字列を使う。テストが「preset == InstallScript()」を固定しているため、片方だけが更新されることがない
- **実行経路**: cloud-init は root で動くため、スクリプト自身が `sudo -u ubuntu -H bash -lc` で ubuntu に降りる。インストール先が `$HOME/.local/bin` (公式インストーラの `LINUX_INSTALL_DIR`) であり、`kirocrew service install` 内部の sudo も Ubuntu AMI の passwordless sudo で非対話に通る
- **PATH**: 公式インストーラは shell の rc を変更せず PATH 行を表示するだけなので、`~/.bashrc` への追記をこちらで行う (`grep -qF` による冪等化)
- **必要パッケージ**: インストーラは Linux で `unzip` を必須依存としてチェックする。通常テンプレートには無かったため `unzip unar` を追加した (bare テンプレートは `unzip` があったので `unar` を追加)
- **AppArmor 制約**: `kirocrew service install` が導入する AppArmor profile は unprivileged userns の許可を含むため、`~/.kiro` が group/world 書き込み可能な間は導入を拒否する (他の local user が `~/.kiro/crew-venv/bin/kirocrew` を差し替えて許可を得られてしまう)。ubuntu ユーザーは private group により umask 0002 で動くためツリーは 0775 で作られる。したがって service install の**直前**に `chmod -R go-w "$HOME/.kiro"` を実行する。この順序はテストで固定
- **best-effort**: 両テンプレートは `set -euxo pipefail` で動くため、導入失敗が user-data 全体を落とすと code-server の起動や SSM へのバージョン記録に波及する。スクリプトは `if sudo ...; then ... else WARNING fi` の構造で自身の失敗を吸収し、再実行コマンド (`setup rerun --preset kiro`) を案内する。内側のシェルは `set -euo pipefail` を自前で設定し、最初の失敗で止まる (pipefail はインストーラのダウンロード失敗が空スクリプト = exit 0 として通過するのを防ぐ)
- **配置順**: 通常テンプレートでは code-server の起動とバージョン記録の後 (遅いダウンロードが利用開始を遅らせない)。bare テンプレートではホームの `chown -R` の後 (`~/.kiro` のモードを後から触らない)
- **オプトアウト**: `create --no-kiro` → `CreateInstanceInput.NoKiro` → `userdata.*Params.InstallKiro` (正の意味に反転)。テンプレートは `{{if .InstallKiro}}` で節全体を囲むため、無効時は節コメントすら残らない

### 8.19 Kiro Crew の Web 画面 (`nakama aws kirocrew`、ADR 0020 / Issue #71)

code-server のパス転送 (`/proxy/5476/`) では、Kiro Crew の Host 検査とルートからの絶対パス前提の 2 点に阻まれて画面が開かない (ADR 0020 の Context)。そこで、Kiro Crew 専用の CloudFront distribution、code-server のホスト名による振り分け、インスタンス上の nginx を組み合わせる。

- **経路**: `https://<crew>` → CloudFront (オリジンのカスタムヘッダー `X-Forwarded-Host: 15476.kirocrew.nakama.internal`) → code-server:8080 (パスワード認証 → `proxy-domain` の照合でポート 15476 を選び、パスを変えずに転送) → nginx 127.0.0.1:15476 (Origin を検査し、Host と Origin を `localhost:5476` に書き換え、`X-Forwarded-Proto: https` を付ける。`X-Forwarded-For` は残す) → Kiro Crew 127.0.0.1:5476
- **構成の組み立て**: `distributionConfig()` を code-server 用と Kiro Crew 用で共用する。共通の内容は、VPC Origin が 1 つ、CachingDisabled、AllViewer、全メソッド許可、redirect-to-https、共有の WAF。Kiro Crew 用はこれにオリジンのカスタムヘッダーを足すだけである。code-server 用の構成は、関数に切り出す前と 1 バイトも変わっていない (テストで固定)
- **リダイレクト**: 既存の distribution に `/proxy/5476*` と `/absproxy/5476*` の behavior を先頭に足す。既定の behavior と同じ設定に、viewer-request の CloudFront Function を付けたもの。Function は `https://<crew>` + 残りのパス + クエリへの 302 を返す。パスの照合は厳密に行うので、`/proxy/54760` は素通しになる。behavior の追加と削除は冪等で、利用者が足した behavior は残す。内容が変わらないときは UpdateDistribution を呼ばない
- **enable** (冪等、途中から再実行できる): 次の順に進む。① インスタンスの確認 (bare は拒否、停止中は `start` を案内) → ② 既存の distribution の構成から VPC Origin と WAF を読む → ③ Kiro Crew 用 distribution を作り、**作った直後に ID とドメインをメタデータへ書く** → ④ Run Command でインスタンス側を設定する (nginx の導入と設定、`config.yaml` の管理ブロック、code-server の再起動。結果は `NAKAMA_KIROCREW_OK` / `NAKAMA_KIROCREW_ERROR` で返す) → ⑤ Function を作成または更新して LIVE に公開 → ⑥ 既存の distribution に behavior を足す → ⑦ 2 つの distribution が Deployed になるのを並行して待つ。④ が失敗したら ⑥ は行わないので、壊れた設定へ誘導されることはない
- **disable / delete**: disable は、behavior を外して反映を待つ → Function を削除する (CloudFront が使用中と返す間は再試行) → Kiro Crew 用 distribution を削除する → インスタンスが起動していれば後片付けのスクリプトを実行する (停止中なら飛ばして、そのことを知らせる) → 最後にメタデータを消す、の順に進む。delete は、Kiro Crew 用 distribution を**既存の distribution と VPC Origin より先に**消し (どちらも同じ VPC Origin を参照しているため)、Function は既存の distribution を消した後に消す
- **トークン**: `MintKiroCrewToken` が、リクエストのたびに RSA-4096 の鍵を作り、公開鍵を埋め込んだスクリプトを Run Command で実行する。スクリプトは `kirocrew token` の出力からループバックの URL を取り出し、`openssl pkeyutl` (OAEP、SHA-256、MGF1 も SHA-256) で暗号化して `NAKAMA_KIROCREW_TOKEN_RSA <base64>` として返す。復号した値は `[A-Za-z0-9._-]` の 20〜1024 バイトかを確かめ、エラーメッセージにも値を含めない。Run Command の成功ログには、出力の中身を書かずバイト数だけを DEBUG で出す
- **ブラウザの起動**: 1 回限りログインページ (ADR 0012) を `newOneShotLoginServerFor(…, loginTarget{domain, label, extra})` に汎用化した。Kiro Crew では送信先を `https://<crew>/login?to=/&token=…`、href を crew にする。`open` が生成する HTML は変わっていない (テストで固定)。`--no-auth` を付けると `https://<crew>/?token=…` を直接開くので、code-server のログイン画面が先に出る
- **メタデータ**: `/nakama/<名前>/meta/kirocrew-distribution-id` と `kirocrew-distribution-domain`。`info` は後者から `kirocrew_url` を表示する
- **予約名**: `enable` と `disable` はサブコマンド名なので、インスタンス名として予約する

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
- Windows (windows-latest): `go vet ./...` + `go test -count=1 ./...` (`-race` なし。race ランタイムが Windows では cgo / gcc に依存するため) + ビルド & スモークテスト

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
  - `internal/cli/aws_connect.go`: cobra コマンド (`--tty` / 位置引数、`SetInterspersed(false)`)、`aws.go` の `AddCommand` に配線 (ポートフォワードは `aws_tunnel.go` へ移管。§8.12)
- テスト方針: argv/Parameters 組立と引数解析はピュア関数化してユニットテスト。実セッションは手動 E2E (CI 非搭載)。

## 13. 資格情報解決とマルチアカウントプロファイル (ADR 0011, ADR 0016)

### 13.1 解決優先順位

`awsx.NewClients` の資格情報解決は次の優先順位で行う。ADR 0016 以降、
すべてネイティブ実装 (internal/signin) で、**aws CLI へのシェルアウトは
存在しない**:

1. **active nakama profile** (`nakama aws profile load` /
   `nakama aws login` 済みの場合)。profilestore に保存された
   login_session から `signin.RefreshingCredentialsProvider` (§13.3) で
   解決し、STS GetCallerIdentity で probe する。**fail-closed**: 失敗
   しても default chain へフォールバックしない。probe の account ID が
   active の account ID と不一致の場合もエラー (セッション差し替え検知)。
   エラーメッセージには alias / account と復旧手順 (`nakama aws login`
   再実行 or `nakama aws profile unload`) を含める。
2. **default chain** (`AWS_PROFILE` / `AWS_DEFAULT_PROFILE` 尊重) + probe。
3. **login_session fallback** (SDK Go v2 は login_session を解釈しない
   ための互換レイヤ)。ユーザーの AWS config (`AWS_CONFIG_FILE` 尊重、
   なければ `~/.aws/config`) の対象セクション (`AWS_PROFILE` 設定時は `[profile <name>]`、それ以外は `[default]`) を**読み取り専用の
   自前 INI パース** (`awsx.ResolveDefaultLoginSession`) で参照し、
   `login_session` があれば 1 と同じ provider で解決。config への
   書き込みは一切行わない。

active profile は `AWS_PROFILE` より優先する (1 が 2/3 に先行)。
provider は `aws.NewCredentialsCache` (ExpiryWindow =
`signin.RefreshThreshold` = 5 分) でラップされ、失効 5 分前を切ると
provider の Retrieve が呼び直されてネイティブリフレッシュが走る。

`nakama aws profile` / `nakama aws login` サブコマンド群自身は
`NewClients` を経由しない: `save` は active を無視した default chain
専用解決 (`awsx.ResolveCallerIdentity`)、`login` は sign-in フロー後に
`awsx.ResolveLoginSessionIdentity` (login_session 直指定の STS probe)、
`list`/`load` のセッション検証は `awsx.CheckLoginSession`
(provider.Retrieve のみ、STS 不使用)、`delete`/`unload` は AWS SDK
不使用。これにより active profile が壊れていても `unload` による復旧が
常に可能。

### 13.2 ストレージ設計 (`internal/profilestore`)

- ディレクトリ: `$XDG_CONFIG_HOME/nakama` (未設定時 `~/.config/nakama`)。
  パーミッション 0700。テスト用に `Store.BaseDir` で注入可能。
- `profiles.json` (0600、temp+rename のアトミック書き込み):

  ```json
  {
    "version": 1,
    "active": "<accountID または空>",
    "profiles": [
      {
        "account_id": "111111111111",
        "alias": "dev",
        "login_session": "<ARN 形式の識別子>",
        "identity_arn": "arn:aws:sts::111111111111:assumed-role/...",
        "saved_at": "2026-07-30T00:00:00+09:00"
      }
    ]
  }
  ```

- `aws_config` (0600、同アトミック書き込み): 保存のたびに profiles.json
  から**全体を再生成** (差分パッチしない)。ADR 0016 以降 nakama 自身は
  このファイルを消費しない (資格情報解決はネイティブ)。互換/デバッグ用
  に生成を継続する (`AWS_CONFIG_FILE` を指せば aws CLI からも保存済み
  プロファイルを利用可能)。エントリごとに:

  ```ini
  [profile nakama-<accountID>]
  login_session = <ARN>
  region = ap-northeast-1   ; internal/config.Region
  ```

- **秘密情報 (アクセスキー / トークン) は一切保存しない**。login_session は
  AWS CLI のトークンキャッシュ (`~/.aws/login/cache/`) への参照識別子に
  すぎない。ユーザーの `~/.aws/config` / `~/.aws/credentials` へは
  書き込まない。

### 13.3 ネイティブ sign-in クライアント (`internal/signin`, ADR 0016)

`aws login` プロトコル (OAuth 2.0 authorization code + PKCE + DPoP) の
ネイティブ Go 実装。構成:

| 要素 | 役割 |
|---|---|
| `Login(ctx, LoginOptions)` | フロー全体のオーケストレーション。same-device (127.0.0.1 の一回限りコールバックサーバー、全体 10 分タイムアウト、state 検証) / cross-device (`--remote`、base64 貼り付けコードの parse + state 検証) の両フローを実装し、成功時にトークンをキャッシュへ保存して login_session と account ID を返す |
| `Client` | `POST {base}/v1/token` の薄いクライアント。DPoP ヘッダー付与、SigV4 なし、非 2xx を `*APIError` (error コード + 対処のみ、サーバーメッセージは含めない) にマップ。BaseURL / HTTPClient / 時計はテスト注入可能で、既定の HTTPClient は `proxy.NewTransport()` + 30s タイムアウト |
| `TokenCache` | AWS CLI バイト互換のトークンキャッシュ (`AWS_LOGIN_CACHE_DIRECTORY` または `~/.aws/login/cache`、ファイル名 = hex(sha256(login_session)) + ".json"、dir 0700 / file 0600、temp+rename のアトミック書き込み) |
| `RefreshingCredentialsProvider` | `aws.CredentialsProvider` 実装。キャッシュ読取り → 残り 5 分超ならそのまま返却 (ネットワークなし) → 以下なら DPoP リフレッシュ + refresh token 回転をキャッシュへ保存。mutex で並行 Retrieve に安全。`aws.NewCredentialsCache` (ExpiryWindow=5 分) でラップして使用 |
| DPoP/PKCE ヘルパ | ES256 JWT 生成 (P-256、r\|\|s 固定長署名)、SEC1 PEM の直列化/復元、64 文字 verifier 生成と S256 チャレンジ計算 |

セキュリティ不変条件: アクセストークン・refresh token・秘密鍵・idToken
を argv/環境変数/ログ/エラーメッセージに含めない。コールバックの
認可コードもログに出さない。エラーには sign-in の error コードと対処のみ
を載せる。

`nakama aws login` (internal/cli/aws_login.go) はこの上に乗り、
ブラウザ起動 (internal/browser、起動失敗時も URL 表示で待機継続) →
`signin.Login` → `awsx.ResolveLoginSessionIdentity` による STS 識別確認 →
profilestore への自動 save & load (alias 既定はアカウント ID、
`--name` で上書き、collision 検査は profile save と共通) を行う。

## 14. アウトバウンド通信のプロキシ解決 (ADR 0015)

nakama の outbound HTTP(S) 経路は 3 つあり、いずれも `internal/proxy` を
通してプロキシを解決する (third_party は非改変):

| 経路 | 注入方法 |
|---|---|
| AWS SDK 全クライアント | `internal/awsx` の `loadConfig` で `config.WithHTTPClient(BuildableClient.WithTransportOptions(...))` により `Transport.Proxy = proxy.Func()` を設定。active profile / 既定チェーンの両パスが同一関数を通る |
| Session Manager data-channel (WSS) | `service.RunSessionRunner` (session-runner 子プロセス) で `websocket.DefaultDialer.Proxy = proxy.Func()`。vendored plugin の websocketutil はこの package 変数を参照するためパッチ不要。副作用は子プロセス内に閉じる |
| checkip (`IPDiscoverer`) | `internal/awsx/ip.go` で `proxy.NewTransport()` (http.DefaultTransport の Clone に Proxy 差し替え) を使用 |

`proxy.Func()` の解決順序 (二段構え):

1. `golang.org/x/net/http/httpproxy` の `FromEnvironment()` を**呼び出し
   ごとに**評価し、`HTTP_PROXY` / `HTTPS_PROXY` (小文字可) のいずれかが
   設定済みならその `ProxyFunc` を使用 (`NO_PROXY` 尊重)。
   `http.ProxyFromEnvironment` は sync.Once キャッシュのため不採用
   (`t.Setenv` による決定的テストができない)。
2. env 未設定時は `ieproxy.GetProxyFunc()` (github.com/mattn/go-ieproxy
   v0.0.12) に委譲。Windows では WinHTTP / IE の静的設定と PAC を読む
   (cgo 不要)。macOS の OS 設定検出は cgo 必須のため CGO_ENABLED=0 の
   リリースバイナリでは env フォールバック。Linux は env のみ。

TLS 検証は Go 標準の platform verifier (OS 証明書ストア) のまま変更
しない。企業 CA (TLS 傍受プロキシ含む) は Windows / macOS の OS ストア
経由で自動的に信頼される。
