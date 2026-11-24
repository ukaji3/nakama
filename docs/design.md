# nakama 設計書 (v1)

> Status: v0.28.1 時点の実装と、ブラウザ用の転送のルーター (ADR 0029、§8.31) に合わせて改訂
> Last updated: 2026-11-22

要件定義書: [requirements.md](requirements.md)

## 1. アーキテクチャ概要

既定では、インスタンスへの受信の規則は無く、ブラウザも端末も SSM を通って届く (§8.29、ADR 0027)。ブラウザは手元のルーター (§8.31、ADR 0029) に `http://<code-server|kirocrew>.<名前>.nakama.localhost:18888` でつなぎ、ルーターが Host でインスタンスの転送を選ぶ。

```
[User Browser] ──HTTP──> [nakama ルーター :18888] ──> [tunnel デーモン 127.0.0.1:<空きポート>] ──SSM (HTTPS)──> [SSM エージェント]
                (*.nakama.localhost)                   (インスタンスごと)                                       ├── code-server:8080
                                                                                                                └── Kiro Crew 127.0.0.1:5476
```

CloudFront の経路は、`create --cloudfront` か `cloudfront add` で足したインスタンスだけが持つ (§8.30、ADR 0028)。`code --internet` / `kirocrew --internet` はこの経路を使う。

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
                              └── SG: inbound なし。CloudFront を足したときだけ
                                      CloudFront managed prefix list から 8080/tcp
                                      outbound 0.0.0.0/0
                                       │
                                       ▼
                       [Private Subnet] ──> [NAT (Public Subnet, --nat で選ぶ。§8.28)] ──> [IGW] ──> Internet
                                              ├── gateway : NAT Gateway + EIP (AWS が管理、常時稼働。新しい環境の既定)
                                              └── instance: NAT インスタンス (t4g.nano)。電源は code-server の
                                                            ライフサイクルに連動 (全停止で自動停止。ADR 0007)

[SSM Parameter Store]
  /nakama/<name>/code-server/password   (SecureString, KMS aws/ssm)
  /nakama/<name>/code-server/version    (String)
  /nakama/<name>/meta/instance-id       (String)
  /nakama/<name>/meta/eni-id            (String)
  /nakama/<name>/meta/security-group-id (String)
  /nakama/<name>/meta/distribution-id   (String, CloudFront があるときだけ。§8.30)
  /nakama/<name>/meta/distribution-domain (String, CloudFront があるときだけ)
  /nakama/<name>/meta/vpc-origin-id     (String, CloudFront があるときだけ)
  /nakama/<name>/meta/cloudfront-deleting (String, "true"。cloudfront delete の実行中と、途中で止まったときだけ。§8.30)
  /nakama/<name>/meta/iam-role          (String, --with-admin/--with-role 時のみ)
  /nakama/<name>/meta/instance-profile  (String, --with-admin/--with-role 時のみ)
  /nakama/<name>/meta/kind              (String, "bare"。--bare 時のみ。未記録は code-server。§8.17)
  /nakama/<name>/meta/kirocrew-distribution-id     (String, `cloudfront add --kirocrew` 時のみ。§8.19)
  /nakama/<name>/meta/kirocrew-distribution-domain (String, `cloudfront add --kirocrew` 時のみ。§8.19)
  /nakama/_meta/initialized             (String, "true")
  /nakama/_meta/version                 (String, semver)
  /nakama/_meta/nat-mode                (String, "instance" | "gateway"。init が選んだ外向き通信の方式。未記録は instance。§8.28)
  /nakama/_meta/acl/<cidr>              (String, RFC3339 timestamp。acl add のたびに書き直す (最後に追加・確認した時刻)。CIDR の "/" は "_" に置換。例: 203.0.113.5_32)
```

- `--bare` のインスタンスは、code-server・VPC Origin・CloudFront distribution・パスワードを持たない (§8.17)。パスワードと distribution 系のメタキーも書かない。
- `nakama aws cloudfront add --kirocrew` は、code-server の distribution (無ければ先に作る) に加えて、Kiro Crew 専用の distribution をもう 1 つ作る。オリジンは同じ VPC Origin (code-server:8080) で、WAF も共有する (§8.19、ADR 0020)。
- 共有の WAF (IP set と web ACL) は、CloudFront を持つインスタンスが無くても `init` が作って残す (ADR 0028)。

## 2. パッケージ構成

```
github.com/ukaji3/nakama
├── cmd/nakama/                  // エントリーポイント (Windows のダブルクリック起動の判定を含む。ADR 0022)
├── internal/
│   ├── cli/                     // cobra コマンド木 (aws_<command>.go)
│   ├── service/                 // ユースケース層 (init, instance, acl, backup/restore, connect, tunnel, router, ssh, kirocrew, doctor など)
│   ├── awsx/                    // AWS SDK v2 ラッパー interface (mockable) と資格情報解決 (§13)
│   ├── awshelper/               // サービス横断の補助 (タグ型の変換、ポーリング、エラー判定)
│   ├── signin/                  // `aws login` プロトコルのネイティブ実装 (OAuth 2.0 + PKCE + DPoP。ADR 0016、§13.3)
│   ├── proxy/                   // outbound のプロキシ解決 (環境変数 + Windows WinHTTP/PAC。ADR 0015、§14)
│   ├── cliproxy/                // `aws cli shell` 用のプロキシ・TLS 傍受の検出と AWS_CA_BUNDLE の生成 (ADR 0018)
│   ├── userdata/                // cloud-init User Data テンプレ (//go:embed。code-server / bare / restore)
│   ├── awscli/                  // インスタンスに AWS CLI v2 を入れる・更新するシェル手順の唯一の定義 (ADR 0024)
│   ├── kiro/                    // インスタンスに Kiro CLI / Kiro Crew を入れるシェル手順の唯一の定義 (§8.18)
│   ├── browser/                 // ブラウザ起動 (detached, 注入可能な Launcher。ADR 0012)
│   ├── naming/                  // リソース命名規約
│   ├── tagging/                 // タグスキーマ
│   ├── profilestore/            // マルチアカウントプロファイルの永続化 (~/.config/nakama, ADR 0011)
│   ├── config/                  // 固定値 (region, CIDR, AZ, SSM パスなど)
│   └── version/                 // ビルド時に -ldflags で注入される version 情報と、最新リリースとの比較 (§8.24)
├── third_party/
│   ├── session-manager-plugin/  // 案3: 公式 plugin を版固定で取り込み (Apache-2.0。取り込み元は PINNED.md)
│   └── twinj-uuid/              // plugin が使う twinj/uuid。公開版と API 互換が無いため同梱 (§12)
├── tools/
│   └── gen-third-party-notices/ // THIRD-PARTY-NOTICES の生成と鮮度検査 (`make notices` / CI)
├── scripts/
│   └── vendor-ssm-plugin.sh     // session-manager-plugin の取り込みスクリプト
└── docs/
    ├── requirements.md
    ├── design.md
    ├── adr/
    └── tutorial/                // 利用者向け HTML チュートリアル (図の生成・検査スクリプトは tutorial/tools/)
```

## 3. リソース命名規約

すべて `internal/naming` に集約。

| リソース種別 | 命名 |
|---|---|
| VPC | `nakama-vpc` |
| IGW | `nakama-igw` |
| NAT インスタンス | `nakama-nat` |
| NAT インスタンス SG | `nakama-nat-sg` |
| NAT Gateway (gateway の方式。§8.28) | `nakama-natgw` (ADR 0007 より前の NAT Gateway と同じ名前。ADR 0026 の Decision 8) |
| NAT Gateway の EIP | `nakama-natgw-eip` |
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
| `nakama:instance-type` / `nakama:architecture` / `nakama:kind` / `nakama:iam-role` | バックアップの取得元インスタンスの設定 (タイプ、`x86_64` / `arm64`、`vscode` / `bare`、`--with-admin` / `--with-role` のロール名)。削除済みインスタンスの restore が既定値に使う (Issue #96、ADR 0023)。値が無いものは付けない。Issue #96 より前のバックアップには無い |
| `Name` | 標準 AWS Name タグ。命名規約に従う |

`component` の取りうる値は `internal/tagging.Component*` を参照。

## 5. AWS SDK ラッパー

`internal/awsx` は SDK の `*Client` を直接公開せず、利用するメソッドだけを
切り出した interface を定義する。

| interface | カバー範囲 |
|---|---|
| `EC2API` | VPC, AvailabilityZones (Describe), Subnet (Modify: MapPublicIpOnLaunch), IGW, NAT GW (Create/Describe/Delete。gateway の方式の NAT Gateway と、instance の方式での削除。§8.28), EIP (Allocate/Describe/Release), RTB (Describe/Create/AssociateRoute), Route (Create/Replace), ENI, SG, Instance (Run/Describe/Start/Stop/Reboot/Terminate/ModifyAttribute incl. SourceDestCheck), InstanceTypes (Describe), Volume (Create/Attach/Detach/Delete/Modify), Snapshot (Create/Describe/Delete), ManagedPrefixList, Tags (Create/Delete) |
| `CloudFrontAPI` | VpcOrigin (Create/Get/Update/Delete/List), Distribution (CRUD + GetDistributionConfig + Tagging), Function (Create/Describe/Update/Publish/Delete。Kiro Crew 画面へのリダイレクト用。§8.19) |
| `WAFAPI` | IPSet (CRUD), WebACL (Create/Get/List/Update; Delete は共有・恒久リソースのため非対応)。コンストラクタで強制的に **us-east-1** に切り替え (`CLOUDFRONT` scope の制約) |
| `SSMAPI` | Parameter Store + Run Command (`SendCommand` / `GetCommandInvocation`) + Session Manager (`StartSession` / `TerminateSession`) |
| `TaggingAPI` | `ResourceGroupsTaggingAPI.GetResources` のみ |
| `STSAPI` | `GetCallerIdentity` (AWS アカウント ID / 呼び出し元 ARN 解決。ARN は `--with-admin`/`--with-role` プリフライトの PolicySourceArn に使用) |
| `IAMAPI` | Role, InstanceProfile, AttachRolePolicy, PutRolePolicy, SimulatePrincipalPolicy, ListInstanceProfilesForRole, ListAttachedRolePolicies |
| `AMIResolver` | SSM 公式パラメータ経由で Ubuntu 26.04 AMI をアーキ別 (amd64 / arm64) に解決 (`LatestUbuntu2604(arch)`) |
| `IPDiscoverer` | `PublicIPv4`: `https://checkip.amazonaws.com/` に 1 回 GET し、Public IPv4 を返す (`code` / `doctor`)。`DetectPublicIPv4s`: checkip と `https://ifconfig.io/ip` に 3 回ずつ問い合わせ、公開の IPv4 アドレスを重複なしで返す (`acl add` / `acl delete all --keep-mine` / `acl prune`。§8.23) |

`Clients` 構造体が上記をすべて束ね、`NewClients(ctx)` がエントリポイント。

## 6. CLI 層 (cobra)

- ルート: `nakama` (PersistentPreRunE で slog ハンドラ初期化)
- 親コマンド: `nakama aws`
- `backup` は `acl` と同様のサブコマンド群: `backup {create|list|note|delete}`。`restore` は破壊的操作のため `start`/`stop` と並ぶ独立コマンド
- グローバルフラグ: `--verbose` (`-v`), `--output` (`-o`, `table` | `json` | `yaml`)
- CLI 層はフラグを解析して `internal/service` の `*Input` を組み立て、表示を担う。AWS リソースへの処理は Service 層に委譲する。CLI が `awsx` / `signin` を直接呼ぶのは、`awsx.NewClients` と、`login` / `profile` の資格情報まわり (§13.1) だけ
- **コマンドとフラグの名前** (ADR 0030 / Issue #129): インスタンスを扱うコマンドは、名前を位置引数で受ける (`create <name>` も)。ブラウザで開くコマンドは、開く先の名前を持つ (`code`、`kirocrew`)。CloudFront を作る・消すのは `cloudfront add` / `delete` だけ (Kiro Crew の分は `--kirocrew`)。改名した古い名前は、`deprecatedCommand` と pflag の `MarkDeprecated` で v0.30.x の間は受け付ける (§8.32)
- `cmd/nakama` は、Service 層の `*service.ExitCodeError` を受け取ると、その終了コードで終了する (`shell <name> <command>` のリモート終了コードの伝搬。ADR 0021)

## 7. Service 層

`internal/service` の `Service` (`NewService(*awsx.Clients)`) が全ユースケースを持つ。テスト用のフック (`connect` / `tunnel` / `ssh`) は nil なら本番の既定を使う。

```go
// instance create (internal/service/instance_create.go)
type CreateInstanceInput struct {
    Name              string
    InstanceType      string
    EBSGiB            int32         // 0 なら config.EBSDefaultGiB (20)
    CodeServerVersion string        // 空なら最新
    WithAdmin         bool          // ADR 0010。WithRole と排他
    WithRole          string        // ADR 0010。WithAdmin と排他
    Provision         ProvisionSpec // --setup / --dotfiles / --preset (§8.16)
    Bare              bool          // §8.17
    NoKiro            bool          // §8.18
}

type CreateInstanceOutput struct {
    Name, InstanceID, ENIID, SecurityGroupID, VPCOriginID string
    DistributionID, DistributionDomain, URL               string
    Password                 string // SSM に SecureString で書いた生パスワード。CLI は --show-password が無ければ消す
    IAMRole, InstanceProfile string
    Bare                     bool
    Provision                *ProvisionResult // 失敗しても create は失敗にしない
}

func (s *Service) CreateInstance(ctx context.Context, in CreateInstanceInput) (*CreateInstanceOutput, error)
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
    // 取得元インスタンスの設定 (Issue #96)。古いバックアップでは空
    InstanceType, Architecture, Kind, IAMRole string // json: instance_type, architecture, kind, iam_role
}

func (s *Service) CreateBackup(ctx, CreateBackupInput) (*Backup, error)
func (s *Service) ListBackups(ctx, name string) ([]Backup, error)
func (s *Service) ListAllBackups(ctx) ([]Backup, error) // 全インスタンス横断 (backup list 名前省略時)
func (s *Service) SetBackupNote(ctx, name, label, note string) (*Backup, error)
func (s *Service) DeleteBackup(ctx, name, label string) error
// RestoreInput{Name, Label, InstanceType, Bare, WithAdmin, WithRole}。Label 空=最新。
// InstanceType 以降は削除済みインスタンスの作り直しにだけ使う (Issue #96)
func (s *Service) PlanRestore(ctx, RestoreInput) (*RestorePlan, error) // 読み取りだけ。Mode = in-place | new-instance
func (s *Service) Restore(ctx, *RestorePlan) (*RestoreResult, error)
```

- `restore` の CLI は、`PlanRestore` の結果 (モード、使うバックアップ、作り直すときのタイプ・種類・ロール、警告) を表示してから確認を取り、`Restore` を呼ぶ。

- ラベル検証: `^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$` (`internal/service` のローカル regexp)。空ラベルは取得時刻スラッグ `2006-01-02-1504` を採番。
- メモ検証: 255 文字以内 (`utf8.RuneCountInString`)。
- 命名/タグは `internal/naming.Snapshot(name,label)` / `internal/tagging.ForBackup(instanceName,label,nameTag,createdAt,note)` を新設。

## 8. ユースケース別シーケンス (要旨)

### 8.1 `nakama aws init`

0. 外向き通信の方式を決めて記録する (`planNATMode`、§8.28): `--nat` > 記録 (`/nakama/_meta/nat-mode`) > 既存環境 (nakama の VPC がある) は `instance` > 新しい環境は `gateway`。CLI は先に `PlanInit` (読み取りだけ) を呼び、既存環境で方式が変わるなら確認を取る。init は何かを変える前に方式を SSM に書く
1. AZ サニティチェック: `ec2:DescribeAvailabilityZones` で AZ ID を解決し、`apne1-az3` (CloudFront VPC Origins 非対応) を除外
2. タグ検索で `nakama-vpc` の有無を判定。なければ作成
3. IGW / Subnet を冪等に作成 (public subnet は MapPublicIpOnLaunch=true)
4. IAM instance role / profile を冪等に作成。`instance` では NAT インスタンス用のロール / プロファイルも作る (NAT インスタンスがこのプロファイルを使うため先に作成)。`gateway` では NAT 用を作らず、あれば残す
5. 外向き通信 (`initEgress`。経路を張り替える前に新しい NAT を用意して確かめ、張り替えた後に古い NAT を片付ける):
   - `instance`: NAT インスタンス (`t4g.nano`, AL2023 arm64) を public subnet に作成または再利用し、SourceDestCheck を無効化 (新規作成時は `running` まで待機)。private の `0.0.0.0/0` が NAT Gateway を向いていれば (gateway からの切り替え、または ADR 0007 より前の環境)、手順 8 の転送の確認をここで行う。RTB / Routes を冪等に作成し、private default route を NAT インスタンスの ENI に向ける (NAT Gateway 宛てなら `ReplaceRoute`)。その後、NAT Gateway をすべて削除して EIP を解放する (EIP が解放直後の NAT Gateway に握られていれば次回 init で解放)
   - `gateway`: NAT Gateway を public subnet に作成または再利用して `available` を待つ (§8.28)。RTB / Routes を冪等に作成し、private default route を NAT Gateway に向ける (NAT インスタンスの ENI 宛てなら `ReplaceRoute`)。その後、NAT インスタンスを terminate して SG を削除する
6. WAF IP set / web ACL を冪等に作成
7. (手順 5 で確認していない `instance` のみ) NAT インスタンスの転送を確認する (`initNATForwarding`、ADR 0007 の追記 / Issue #87)
   - 停止中なら起動し、終わったら電源を元に戻す (`reconcileNATInstancePowerState`)
   - Run Command で `natCheckScript` を実行する。初回起動の設定 (`cloud-final.service` が `activating` の間) を待ってから、`ip_forward`、`iptables`、MASQUERADE のルール、`nakama-nat.service` を調べる。SSM エージェントの登録待ち (`InvalidInstanceId`) は最大 5 分
   - 壊れていれば `natRepairScript` を実行する。user-data と同じ `nat_setup.sh` を、`systemd-run --unit=nakama-nat-repair` で動かす。その後、15 秒ごとに確認し (最大 25 分)、ユニットが `failed` になればログの末尾を付けて失敗にする
   - 結果は `InitResult.NATForwarding` (`ok` / `repaired`)
8. SSM `/nakama/_meta/*` に状態を記録

NAT インスタンスの user-data (`internal/service/nat_setup.sh`) の手順は次のとおり。
1. `ip_forward` を有効にする
2. `/var/tmp` に 1 GiB の一時スワップを作る (優先度 10、終了時に消す)
3. `dnf --releasever=latest upgrade` (セキュリティ更新、必須)
4. `iptables-nft` だけを入れる。どちらの dnf も `install_weak_deps=False`、3 回まで再試行する
5. `nakama-nat-setup` と `nakama-nat.service` を書いて、enable と restart をする
6. 結果を確かめる (`set -euo pipefail`、`|| true` なし)

### 8.2 `nakama aws create`

1. 環境チェック (`/nakama/_meta/initialized` 存在)
2. (`--with-admin` / `--with-role` 指定時のみ) **IAM ロール解決 + プリフライト + upsert** (ADR 0010):
   - 対象ロール / インスタンスプロファイルと現在の存在状態を解決 (`--with-admin` は `nakama-instance-role-admin` / `nakama-instance-profile-admin`。`--with-role` は `iam:GetRole` で存在確認・trust policy 検証・`iam:ListInstanceProfilesForRole` で既存プロファイル再利用判定)
   - `iam:SimulatePrincipalPolicy` で `iam:PassRole` (+ 未作成分の Create/Attach/Add 系アクション) を検査。拒否が 1 つでもあれば**何もリソースを作らずエラー** (Simulate 自体の失敗も fail-closed)。PolicySourceArn は GetCallerIdentity の ARN (assumed-role は IAM role ARN へ正規化)
   - ロール / プロファイルを冪等に upsert (`--with-role` の既存プロファイル再利用時は no-op。ユーザーロールへの書き込みは一切しない)
3. インスタンスタイプの CPU アーキを解決 (`DescribeInstanceTypes` の `SupportedArchitectures`。存在検証も兼ね、不正な型はここで即時エラー) → 対応する Ubuntu 26.04 AMI (amd64 / arm64) を SSM で解決。`--cloudfront` のときは、この前 (何かを作る前) に CloudFront prefix list ID (`DescribeManagedPrefixLists`) と共有の WAF web ACL (`ListWebACLs`) を引く (`resolveCloudFrontPrereqs`)。どちらかが無ければ何も作らずにエラー。`--bare` と `--cloudfront` は同時に指定できない
4. SSM PutParameter で 32 文字パスワードを SecureString として保存
5. SG 作成 → `meta/security-group-id` を記録。受信の規則は付けない。`--cloudfront` のときだけ、8080 の受信 (CloudFront prefix list) を足す (`ensureSGIngress`)。新しい SG の説明文は CloudFront の規則があることを前提にしない (ADR 0028)
6. ENI 作成 (Private subnet 1a) → `meta/eni-id` を記録
7. 外向き通信を用意する (`ensureEgress`、§8.28): NAT インスタンスがあれば起動する (停止中なら start して `running` まで待機。ADR 0007 の電源連動)。無ければ NAT Gateway が `available` かを確かめる (`pending` なら待つ。使えなければ init を案内してエラー)
8. `RunInstances` (User Data に code-server インストールスクリプト埋め込み。User Data は AWS CLI v2 を AWS 公式のインストールスクリプトで ubuntu ユーザーに導入する (§8.22、ADR 0024)。Ubuntu の apt `awscli` は v1 と古いため purge する)。IAM instance profile は手順 2 で選択したもの (フラグ省略時は共有ベースプロファイル `nakama-instance-profile`)
9. SSM に meta 情報 (`meta/instance-id` と ENI / SG。bare なら `meta/kind`、`--with-admin` / `--with-role` 時は `meta/iam-role` / `meta/instance-profile` も) を書き込み、EC2 が `running` まで待機
10. (`--cloudfront` のときだけ。`ensureInstanceCloudFront`、§8.30 の add と共通) private DNS を引く → CloudFront `CreateVpcOrigin` (HTTP:8080) → `meta/vpc-origin-id` を記録 → VpcOrigin が `Deployed` まで待機 → `CreateDistribution` (Cache=Disabled, OriginRequestPolicy=AllViewer, WAF=ARN) → `meta/distribution-id` / `meta/distribution-domain` を記録 → Distribution が `Deployed` まで待機
11. 出力。`--cloudfront` のときは URL、無いときは `cloudfront: none` と `nakama aws code <name>` (SSM 経由)。パスワードと付与した IAM ロール / プロファイルも出す。JSON には `cloudfront` (bool) が入る

CloudFront を作らない create は、EC2 が `running` になれば終わる (実機で約 20 秒。CloudFront の待ちが無いため)。

各リソースは作った直後、待機に入る前に SSM に記録する (restore の作り直し §8.5.1 と同じ)。記録の書き込みは呼び出し元のキャンセルを受けない context (`context.WithoutCancel` に 30 秒のタイムアウト) で行う。作成の API が返った直後に Ctrl-C が来ても記録は残り、create は次の待機か作成で止まる。途中で失敗しても Ctrl-C で中断しても、記録されたものは `delete <n>` (§8.3) で消え、同じ名前で作り直せる。自動の巻き戻しはしない。SG・ENI・EC2 は、記録の書き込みが失敗したときや作成の応答を受け取る前に中断されたとき (SSM に ID が無いとき) も、`delete <n>` がタグで探して消す (§8.27)。VPC Origin と distribution はタグで探さないので、記録より前に中断されると残る。IAM ロール / プロファイルはインスタンスをまたいで共有するので、`delete` の対象ではない。

### 8.3 `nakama aws delete`

1. 確認プロンプト
2. SSM `/nakama/<name>/meta/*` から各リソースの ID を読む。EC2・ENI・SG の ID が無ければ、その名前の nakama のタグで探す (§8.27)
3. CloudFront のリソース (記録があるものだけ。`deleteCloudFrontResources`、§8.30 の `cloudfront delete` と共通): Kiro Crew の distribution → code-server の distribution → Kiro Crew の Function → VpcOrigin。distribution は disable → `Deployed` を待つ → delete。すでに disable されていても `Deployed` を待ってから消す (反映中は DeleteDistribution が `DistributionNotDisabled` を返すため。実機で確認)
4. EC2 → terminate → wait (タグで見つけたものはすべて)
5. 孤児 EBS ボリュームの掃除: `nakama:managed` + `nakama:name=<name>` + `nakama:component=ebs` タグ付きで `available` のボリュームを削除 (過去の restore が DeleteOnTermination=false のまま残したルートボリューム対策。terminate 直後の detach 遷移は短時間ポーリングで追従し、in-use のボリュームは削除しない)
6. ENI → delete (terminate 完了後。タグで見つけたものはすべて)
7. SG → delete (タグで見つけたものはすべて)
8. SSM `/nakama/<name>/*` を全削除
9. NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerStateExcluding`。削除対象を除外し、他に稼働中の code-server が無ければ NAT インスタンスも停止。ADR 0007。NAT インスタンスが無ければ (gateway の方式) 何もしない。§8.28)
10. この機械の同名の tunnel デーモンを止める (`stopLocalTunnel`。失敗は警告。§8.29)

### 8.4 `nakama aws backup create`

1. SSM Parameter Store (`/nakama/<name>/meta/instance-id`) で instance-id を解決し、その EC2 のルートボリューム ID を取得
2. ラベルを確定 (`--label` 省略時は取得時刻から生成、例 `2026-05-29-1402`)。`<name>` 内で重複する場合はエラー
3. `CreateSnapshot` (ルートボリューム)。`--note` 指定時はスナップショットの `Description` にも設定 (コンソール表示用)。タグ `nakama:managed/name/component=backup/backup-time=<RFC3339>/backup-label=<label>` と、メモがあれば `nakama:note=<text>` を付与
4. `--keep N` 指定時: 当該 `<name>` のスナップショットを `backup-time` 降順で並べ、N 件超過分を `DeleteSnapshot` (prune)
5. ラベル / 取得時刻 / 状態 / メモを出力 (snapshot-id は `-o json` のみ)

### 8.5 `nakama aws restore`

モードは SSM の `/nakama/<name>/meta/instance-id` の有無で決まる。ある場合はインプレース復元 (下の 1〜10)。無い場合 (インスタンスが削除済み) は、同じ名前のインスタンスを作り直す (§8.5.1、ADR 0023)。記録はあるのに EC2 が無い (`terminated` か `NotFound`) 場合は、`delete --force` で残りを片付けるよう案内してエラーにする。

1. 対象スナップショットを解決: `--label` 指定時はタグ `nakama:backup-label` で、省略時は `backup-time` 最新のものを選ぶ。利用者は snapshot-id を指定しない
2. 確認 (`--yes` で省略)。作り直し用のフラグ (`--instance-type` / `--bare` / `--with-admin` / `--with-role` / `--cloudfront`) はエラー
3. instance-id を SSM で解決し、現ルートのデバイス名 (例 `/dev/sda1`) と旧ルートボリューム ID / AZ / ボリュームタイプを記録 (stop より前に取得)
4. インスタンスを `stop` → `stopped` まで待機
5. スナップショットから新ボリュームを `CreateVolume` (同 AZ / 同タイプ、暗号化は継承)
6. 旧ルートを `DetachVolume` → 新ボリュームを同デバイス名で `AttachVolume`
7. `ModifyInstanceAttribute` で新ルートに `DeleteOnTermination=true` を再設定 (起動後アタッチのボリュームは既定 false のため。これを怠ると `delete` 後にルート EBS が available のまま残存する)
8. 旧ルートボリュームを `DeleteVolume`
9. NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerState`。インスタンスは停止のまま残るため、他に稼働中が無ければ NAT インスタンスも停止。ADR 0007。gateway の方式では何もしない)
10. インスタンスは停止のまま終了 (利用者が `start`)

#### 8.5.1 削除済みインスタンスの作り直し (Issue #96)

計画 (`PlanRestore`、読み取りだけ。失敗すれば何も作らない):

1. 名前を `ValidateNewInstanceName` で検査する。その名前のバックアップが無ければエラー。`--label` (省略時は最新) で選んだバックアップが `completed` でなければエラー
2. `assertNameAvailable`: nakama の記録が無いのに同じ名前の EC2 があればエラー。`requireInitialized`
3. 種類: タグ `nakama:kind` を優先する。`vscode` のバックアップに `--bare` はエラー。タグが無ければ `--bare` に従う。bare に `--cloudfront` はエラー。`--cloudfront` のときは共有の WAF web ACL があることを確かめる
4. タイプ: `--instance-type` > タグ `nakama:instance-type`。どちらも無ければ `--instance-type` を求めるエラー。`resolveInstanceArchitecture` でアーキを決め、タグ `nakama:architecture` と違えばエラー
5. 警告: タグの無いバックアップでは、アーキを検査できないことを警告する。タグ `nakama:iam-role` があってフラグが無ければ、ロールを付け直さないことと、付けるフラグ (`nakama-instance-role-admin` なら `--with-admin`) を警告する。タグ `nakama:cloudfront=true` (CloudFront のあったインスタンスのバックアップ。ADR 0028 以降に取ったもの) があって `--cloudfront` が無ければ、CloudFront を作らないことと、`--cloudfront` / `cloudfront add` を警告する

実行 (`Restore`):

1. `--with-admin` / `--with-role` があれば、create と同じくロールを先に用意する (ADR 0010 の事前検証)
2. アーキに合う最新の Ubuntu 26.04 AMI を解決する。code-server なら新しいパスワードを生成して SSM に置く
3. `prepareInstanceNetwork` (create と共通): 土台の発見、SG と ENI の作成 (それぞれ作った直後に記録)
4. user-data は `userdata.RenderRestore` の cloud-config。`ssh_deletekeys: false` で、バックアップの SSH ホスト鍵を残す (cloud-init は新しいインスタンス ID を見ると鍵を消して作り直すため)。code-server なら初回起動スクリプトを置き、`runcmd` で実行する。スクリプトは `~/.config/code-server/config.yaml` が無ければ何もせず (起動直後の新しいボリュームの場合)、あれば SSM の新しいパスワードで `password:` の行だけを置き換えて code-server を再起動し、版を SSM に書き戻す。ソフトウェアは入れ直さない
5. 外向き通信を用意し (`ensureEgress`。NAT インスタンスの起動、または NAT Gateway の確認)、`runInstances` で EC2 を起動する (ルートは既定の 20 GiB。直後に差し替える)。instance-id / ENI / SG を記録してから `running` を待つ
6. `--cloudfront` のときだけ、private DNS → VPC origin → distribution を作り、それぞれ作った直後 (VPC origin は `Deployed` を待つ前) に記録する (失敗しても `delete` で片付けられるように。`ensureInstanceCloudFront`、create と共通)。SG の CloudFront の規則も手順 3 でこのときだけ付ける
7. インプレース復元の 3〜8 と同じ手順 (`replaceRootVolume`) でルートを差し替える。起動直後のボリュームは削除される
8. NAT インスタンスの電源を再調整し、`--cloudfront` なら distribution の `Deployed` を待つ。停止のまま終了する。パスワードとプライベート IP は新しく、`--cloudfront` なら URL も新しい。Kiro Crew の CloudFront の画面は `cloudfront add --kirocrew` のやり直しが要る

### 8.6 `nakama aws backup note`

1. `--label` で当該 `<name>` のスナップショットを解決
2. `--note <text>` 指定時: `CreateTags` で `nakama:note=<text>` を上書き。空文字明示時: `DeleteTags` で `nakama:note` を除去 (`--note` の省略は CLI がエラーにする)
3. 更新後のラベル / メモを出力

> Description はスナップショット作成後に変更できないため、メモ編集の正は常に `nakama:note` タグ側とする。

### 8.7 `nakama aws shell`

共通フロー: instance-id を `/nakama/<name>/meta/instance-id` から解決 → モードに応じた `DocumentName`/`Parameters` で `ssm:StartSession` → 返却の `SessionId`/`StreamUrl`/`TokenValue` で取り込んだ plugin の `session.Session` を構築 → `Execute()` で data-channel (WSS) を確立。セッション本体の実行は hidden な子プロセス (`session-runner`) が担い、恒久切断時は親が自動再接続する (§8.11 / ADR 0014)。

| モード | 起動形 | DocumentName | Parameters | session サブタイプ |
|---|---|---|---|---|
| 対話シェル | `shell <n>` | `AWS-StartInteractiveCommand` | `command=exec sudo su - ubuntu` | `Standard_Stream` |
| コマンド | `shell <n> <cmd...>` | `AWS-StartNonInteractiveCommand` | `command=sudo su - ubuntu -c '<ラッパー(cmd)>'` | nakama の `commandSession` (§8.20) |
| コマンド (PTY) | `shell -t <n> <cmd...>` (`--tty`) | `AWS-StartInteractiveCommand` | `command=sudo su - ubuntu -c '<cmd>'` | `Standard_Stream` |

ポートフォワードは `nakama aws tunnel` に一本化した (§8.12 / ADR 0017)。

1. `<name>` → instance-id 解決 (未存在ならエラー)
2. モード判定 (シェル / コマンドは排他)。`DocumentName` / `Parameters` を組立
3. `ssm:StartSession`
4. credential / region / SSM endpoint を解決 (既存 `awsx` の `aws login` fallback を流用) し `session.Session` を構築
5. 対話シェルと `--tty` は端末 raw mode + リサイズ送信 + 制御シグナル転送。標準入力が端末でなければ、終わらないパイプで ShellSession に渡し、入力の終わりで Ctrl-D を送る (§8.20)
6. コマンド実行は標準入力を読まず、制御シグナルだけを転送する。stdout / stderr / 終了コードは枠付きの出力から取り出す (§8.20)
7. Ctrl-C / リモート切断で `TerminateSession`

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
4. **外向き通信の用意** (`ensureEgress`、§8.28): 更新はインターネットから apt / code-server を取得するため egress が必要。start の前に無条件で、NAT インスタンスがあれば起動し (ADR 0007)、無ければ NAT Gateway が `available` かを確かめる。続いて `wasStopped` なら `StartInstances` → `running` 待ち → **SSM エージェント online 待ち** (`SendCommand` を `InvalidInstanceId` の間リトライ)
5. SSM Run Command (`AWS-RunShellScript`, `executionTimeout` 延長) で 1 スクリプト実行:
   - `apt-get update` → `apt-get dist-upgrade -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold` (`DEBIAN_FRONTEND=noninteractive`)
   - code-server `install.sh`(latest) → `systemctl restart code-server@ubuntu`
   - `code-server --version` を `/nakama/<name>/code-server/version` へ書き戻し (best-effort。`nakama_aws` 経由、§8.22)
   - AWS CLI: 公式のインストールがあれば (`~/.local/aws-cli/v2/current/bin/aws`) 公式のインストールスクリプトを再実行して更新し、版を `NAKAMA_AWSCLI=<版>` で返す。失敗は update の失敗。Issue #101 より前のインスタンス (snap) には何もしない (Issue #101)
   - Kiro Crew: `~/.local/bin/kirocrew` があれば ubuntu で `kirocrew update` → `kirocrew restart` (update が失敗したら restart しない)。出力は捨てる (restart がダッシュボードのアクセストークン付き URL を表示するため)。結果を `NAKAMA_KIROCREW=updated|failed` で返し、失敗は警告に留める (Issue #101)
   - 末尾に `/var/run/reboot-required` の有無を `NAKAMA_REBOOT_REQUIRED=0|1` として返す
   - **出力の扱い** (Issue #101): GetCommandInvocation が返す stdout は先頭 24,000 字だけなので、詳しい出力 (xtrace と apt) はインスタンスの `/var/log/nakama-update.log` に書き、stdout には上の `NAKAMA_*` の印だけを出す。失敗したときはログの末尾 (3,500 バイト) を stderr に出し、nakama のエラーに含める
6. **reboot 判定**: 元 running かつ `NAKAMA_REBOOT_REQUIRED=1` のときのみ `RebootInstances`。元 stopped は reboot せず (手順 7 の stop と次回 start で新カーネルが反映される)
7. `wasStopped` なら `StopInstances` → `stopped` 待ち。**手順 5 が失敗しても電源状態は元へ戻す** (バックアップ済のため復旧可能)。その後 NAT インスタンスの電源を再調整 (`reconcileNATInstancePowerState`。復元後の電源状態に対し、他に稼働中が無ければ NAT も停止。best-effort。ADR 0007。gateway の方式では何もしない)
8. 結果出力: 更新後 code-server version / AWS CLI version / Kiro Crew の結果 / backup label / reboot 実施有無 / (元 stopped 時) 次回起動で反映の注記。Kiro Crew が failed なら stderr に警告

> 設計判断の根拠は ADR 0006 を参照。

### 8.10 `nakama aws code`

code-server をブラウザで開き、パスワードログインを自動通過する
(根拠と代替案の比較は ADR 0012)。code-server の認証は cookie のみで
URL トークン認証が存在せず、CLI はブラウザの cookie store に書き込めない
ため、「ブラウザ自身に `/login` へ top-level form POST させる」方式を取る。

既定の送信先は SSM のポート転送で、ブラウザはルーターの URL
(`http://code-server.<名前>.nakama.localhost:<ポート>`、§8.31) を開く。`--local-port`
を付けたときは `http://localhost:<ポート>`。CloudFront (`https://<domain>`) は
`--internet` のときだけ使う (ADR 0027、§8.29)。以下の手順はどれにも共通で、違いは
§8.29 と §8.31 に書く。

1. **preflight**: `GetInstanceInfo(ctx, name, showPassword)` で情報取得
   (`--no-auth` 時は showPassword=false)。パスワード読取のみが失敗した
   場合は showPassword=false で再取得し、成功すれば自動ログインだけを
   スキップして継続 (METHOD C fallback)。インスタンス未存在は即エラー
2. **fatal チェック**: `--internet` で CloudFront が無い (`InstanceInfo.CloudFront`
   が false) → `nakama aws cloudfront add` を案内するエラー (ADR 0028、§8.30)。
   distribution はあるのにドメインが記録されていない → `cloudfront add` の
   再実行を案内するエラー。bare → エラー。state が running 以外 →
   エラー (`nakama aws start <name>` を案内)
3. **WAF soft check** (`--internet` のときだけ): `IPDiscoverer.PublicIPv4` + `ListACL` で呼び出し元
   IP が allowlist に**CIDR containment** (net/netip) で含まれるか確認。
   未収載なら stderr に警告 (`nakama aws acl add` を案内)。チェック自体の
   失敗は debug ログのみで続行 (絶対に code を壊さない)
4. **METHOD A (既定)**: crypto/rand 128 bit の URL-safe トークンを採番し、
   `127.0.0.1:0` (カーネル割当ポート) に one-shot HTTP サーバを起動。
   `/<token>` だけが自動 submit フォーム
   (`POST <origin>/login?to=/`、hidden の `password` / `base=.` /
   `href=<origin>/login` は code-server の対話ログインフォームと
   同一構成。`<origin>` はルーターの URL、`http://localhost:<ポート>` (`--local-port`) か `https://<domain>`) を **1 回だけ**返す (他パスは 404、再取得は 410 Gone)。
   `Cache-Control: no-store` / `Referrer-Policy: no-referrer` 付与、
   html/template でエスケープ、外部リソース無し。ブラウザを one-shot URL
   で起動し、ページ配信を待つ (上限 `--timeout`, 既定 30s)。配信後は
   猶予付き Shutdown。JS 無効環境向けに可視の submit ボタンも置く
5. **METHOD C fallback**: パスワード取得失敗 / listener bind 失敗 /
   timeout 内未取得のいずれでも、プレーン URL を開く動作へ退避。理由と
   `nakama aws info <name> --show-password` の案内を stderr に出す。
   ランチャー不在・起動失敗のときは、`--internet` では URL を stdout に表示して終了コード 0、
   SSM では URL を表示せずにエラーにする (`--browser` を案内。ADR 0027 の Decision 10)
6. **フラグ**: `--no-auth` (METHOD C を明示利用)、`--print-url`
   (ブラウザ非起動。auth 有効時は plain URL と one-shot URL の両方を表示し
   timeout までサーバ維持)、`--browser <cmd>`
   (起動コマンド上書き。優先度 `--browser` > `$BROWSER` > OS 既定
   (darwin=`open`, linux=`xdg-open`, windows=`rundll32` best effort))、
   `--timeout <duration>`、`--internet`、`--local-port <port>` (SSM のときだけ。ルーターを使わず、そのポートで開く)
7. **出力**: `-o json|yaml` は name / url / via (`ssm`|`internet`) / local_port / method (`auto-login`|`plain`) /
   opened / auto_login (`completed`|`skipped`|`timeout`) / note の
   構造体。**パスワードはいかなる出力にも含めない**

実装: `internal/cli/aws_code.go` (one-shot サーバ含む)、
`internal/browser` (detached なブラウザ起動、`Launcher` interface で注入
可能)。テストは fake backend / fake launcher / httptest 相当のローカル
listener で AWS・実ブラウザ・外部ネットワークに触れない。

> CloudFront 経由で form POST が実際に cookie を確立するかの E2E は
> 実 AWS 未検証 (ADR 0012 の UNVERIFIED 項参照)。

### 8.11 `nakama aws shell` の自動再接続 (ADR 0014)

vendored plugin は正常終了・恒久切断のいずれも `os.Exit(0)` またはプロセス宙吊りで終える設計のため、切断検知は**親プロセス監督方式 (child re-exec)** で行う。

```
[nakama aws shell]                       [nakama aws session-runner (hidden, 子プロセス)]
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
  │       他 → ExitCodeError として伝播 (cmd/nakama が同一コードで exit。コマンド実行は 255)
  │       ※ コマンド実行では、子が状態パイプで返したリモートの終了コードを最優先する (§8.20)
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
  │    DETACHED_PROCESS)                  │        → 子の終了: バックオフ再接続 (上限なし,
  ├ readiness ポーリング                   │           60s 安定でリセット) / 認証エラー:
  │   (状態ファイル + 制御 ping,           │           60 秒ごとに再試行 (§8.29)
  │    タイムアウト付き)                   ├ ログ: unix=syslog / windows=Event Log (OS に
  └ 転送一覧を表示して即終了               │   ローテーション委譲)。子の stdout/stderr も回収
                                          └ stop: 全子終了 → TerminateSession (best-effort)
                                             → 状態ファイル削除 → exit
```

- **SSM ドキュメント**: `-L L:R` → `AWS-StartPortForwardingSession` (`portNumber`, `localPortNumber`)、`-L L:host:R` → `AWS-StartPortForwardingSessionToRemoteHost` (+`host`)。1 セッション 1 ポートのため複数 `-L` はセッションを転送数ぶん並行に張る。利用者のローカルポートはデーモンが持ち、`localPortNumber` にはデーモンが選んだ別の空きポートを渡して中継する (ADR 0027、§8.29)
- **再接続**: 転送ごとの監督は `forwardRunner` (§8.29)。exit 0 を含む子の終了すべて、StartSession の通信エラー・5xx・スロットリングで、回数の上限なく張り直す。Issue #45 の `RetryChildFailures` はこれに含まれたので無くなった
- **in-place resume の資格情報** (Issue #49): vendored plugin の ResumeSession 制御プレーンとデータチャネル再署名は上流では default 資格情報チェーンを使うため、nakama login のみの環境では resume が構造的に失敗する。session-runner が `NakamaAWSConfigProvider` (NAKAMA PATCH) に nakama 解決済み `aws.Config` (プロキシ対応 HTTPClient 込み) を注入し、WebSocket 断のたびにフルセッション置換へ落ちずに in-place resume が機能するようにする (詳細は third_party/session-manager-plugin/PINNED.md)
- **stop の経路**: 制御チャネル経由のグレースフル停止が正 (Windows の外部 Kill は子を道連れにしないため)。unix ではデーモンが SIGTERM でも同じ停止経路に合流する
- **Windows のコンソール** (Issue #126): nakama.exe はコンソールアプリケーション (PE の subsystem が `WINDOWS_CUI`) である。コンソールを持たないデーモンが、フラグ無しで子を起動すると、Windows は子ごとに新しいコンソール (conhost.exe とウィンドウ) を作る (Windows の CI で、子が `GetConsoleWindow` と `GetConsoleCP` に 0 以外を返すことを確かめた)。そのため、デーモンの session-runner の子は `DETACHED_PROCESS` で起動する (`daemonSessionRunnerIO` の `noConsole`、`detachConsole`)。子の stdout / stderr はデーモンのログへのパイプなので、コンソールは要らない。`CREATE_NO_WINDOW` はウィンドウを出さないが、conhost.exe をセッションごとに作るので使わない。`shell` の子は端末のコンソールを引き継ぐ
- **横断一覧と提案** (Issue #55): `tunnel status` の名前省略時は状態ファイルディレクトリを走査し (`listTunnelStateNames`)、各インスタンスへ `GetTunnelStatus` を実行して NAME 列付きで一覧する。デーモン不応答は `NoTunnelError` として stale 表示 (単一指定時と同じ掃除が働く)。`tunnel suggest <n>` は `runRemoteScript` で `ss -ltnH` (無ければ `netstat -ltn`) を実行し、ポート重複の畳み込み・プロセス名付きエントリ優先・昇順ソートを経て `-L` 案を出す。22 番は `ssh` の案内に回し提案コマンドから除外。`suggest` も曖昧性回避のため予約インスタンス名に追加
- **status**: CLI が状態ファイル → `GET /status` で転送ごとの {spec, purpose, state (connecting/active/reconnecting/idle/dead), 直近エラー, 直近の AWS のエラーコード, 開始時刻, 再接続の回数} を取得して表示。デーモン不在 (ping 不達) なら stale 掃除の上「転送なし」を表示
- **restart**: 状態ファイルに記録された転送スペック (用途付きの形を含む) のまま stop → start を合成 (`-L` は受け付けない。変更は stop → start で)。スペックの再検証は stop の**前**に行い、壊れた状態ファイルのために生きているデーモンを止めない。新デーモンは CLI プロセスの現行資格情報チェーンと nakama の版で起動する。資格情報のエラーは動いているデーモンが 60 秒ごとに再試行するので、主な用途は古い版のデーモンの入れ替えと dead の転送の復旧。状態ファイルが無い場合はエラー (推測で張らない)
- **予約名**: `start` / `stop` / `status` / `restart` は `tunnel <name>` 省略形と曖昧になるため create の新規名として拒否 (`service.ValidateNewInstanceName`。既存インスタンス操作の `ValidateInstanceName` は不変)
- **転送量計測は非対応** (要件から除外。ADR 0017 Alternatives 6)

### 8.13 `nakama aws cli shell` — aws CLI 用の設定済みサブシェル (ADR 0018)

aws CLI は OS のプロキシ設定 (WinHTTP/PAC) も Windows 証明書ストアも読まないため、企業の TLS インスペクション環境では既定で動かない。nakama の検出能力 (ADR 0015) を aws CLI の公式構成面 (環境変数 + `AWS_CA_BUNDLE`) に変換し、それを注入したサブシェルを提供する。Windows 専用 (他 OS では Windows 専用オプションである旨を表示してエラー)。

- **検出エンジン** (`internal/cliproxy`): ADR 0015 の解決器でプロキシを解決 → STS エンドポイントへ TLS プローブ → **OS ストア検証済みチェーン**のルートを同梱の Amazon Trust Services (ATS) ルート 5 枚 (取得時に 2 ソース照合、テストで SHA-256 をピン留め) と照合 → ATS 以外なら TLS インスペクションと判定し、企業 CA チェーン (リーフ除く) + ATS ルートを `~/.config/nakama/aws-ca-bundle.pem` (0600) へ書き出す。ATS 同梱は「`AWS_CA_BUNDLE` は既定バンドルの置き換え」であることへの対策 (インスペクション除外エンドポイントの検証を壊さない)。最後に生成した設定そのもので疎通を再確認しバナー表示する
- **env 注入**: `HTTP_PROXY`/`HTTPS_PROXY` (解決したプロキシ。直結なら設定しない)、`NO_PROXY` (既存値へ localhost 系をマージ)、`AWS_CA_BUNDLE` (インスペクション時のみ)、`NAKAMA_AWS_CLI_SHELL=1` (ネスト防止マーカー)。既存の同名変数は大文字小文字を無視して置換する
- **サブシェル**: 親プロセスの実体 (`QueryFullProcessImageName`) が powershell.exe / pwsh.exe / cmd.exe ならそれを、不明なら `ComSpec` を起動 (`--shell` で明示指定可)。stdio 継承で同一ウィンドウ内、シェルの exit code は伝搬。親シェル・レジストリ・ユーザー環境変数・`~/.aws/config` (ADR 0011/0016 の不変条件) には一切変更を残さない
- **プロキシ認証の診断**: CONNECT が 407 を返す場合は `Proxy-Authenticate` のスキームを読み、Basic あり → プロキシ URL への資格情報埋め込みを案内 / NTLM・Negotiate のみ → ローカル認証リレー (px / Cntlm) を案内してエラー終了 (aws CLI は Basic のみ対応のため)
- **PATH** (ADR 0022): サブシェルの PATH の先頭に nakama.exe のフォルダー (`os.Executable` を `EvalSymlinks` したもの) を入れる。結合は Windows の規則 (`prependWindowsPath`: 変数名は大文字小文字を区別しない、`;` 区切り、同じフォルダーの重複は大文字小文字・末尾の `\`・引用符を無視して除去、`;` を含む名前は引用符で囲む)
- **Ctrl-C** (ADR 0022): シェルの実行中は `signal.Notify(os.Interrupt)` で受けて捨てる (`runShell`)。Windows の Go は Notify が無いと Ctrl-C / Ctrl-Break で終了し、`signal.Ignore` は効かない (`sigignore` が空実装) ため

### 8.13.1 nakama.exe のダブルクリック — 設定済みの PowerShell (ADR 0022 / Issue #85)

- **判定** (`main` で cobra より先): 引数なし、かつ親プロセスの実体が `explorer.exe` (`isDoubleClick`)。それ以外は cobra に任せる (エクスプローラーから引数付きで起動した場合は、今までどおり cobra の mousetrap が「This is a command line tool.」を表示して 1 で終わる)
- **シェル**: `GetSystemDirectory` 配下の `WindowsPowerShell\v1.0\powershell.exe -NoLogo`。無ければ `ComSpec`、それも無ければ `cmd.exe` (`launcherShell`)。PATH からは探さない。開始フォルダーはホーム。終了コードは伝える。起動に失敗したらエラーを表示し、Enter を待ってから 1 で終わる
- **流れ** (`doubleClickLauncher.launch`。依存はフィールドで注入し、どの OS でもテストする): 版と PATH の行を表示 → `cliproxy.Detector` で検出 (Ctrl-C で取り消せる) → 成功なら `printDetection` と `cliShellEnv` (cli shell と同じ環境 + PATH)、失敗なら理由 (`explainDetectionError`) と再試行の案内を表示し、PATH だけを足した環境 (ネスト防止の印なし。ウィンドウ内で `cli shell` をやり直せる) → シェルを起動
- **Ctrl-C**: プロセスの最初から最後まで `os.Interrupt` を Notify する。ウィンドウを閉じるイベント (`CTRL_CLOSE_EVENT`) は既定の処理のままで、nakama とシェルが一緒に終わる
- レジストリ・ユーザー環境変数は変更しない

### 8.14 `nakama aws ssh` — ssh over Session Manager (ADR 0019)

デスクトップ版 VS Code (Remote-SSH) / JetBrains Gateway / 素の ssh・scp・rsync を、受信ポート開放なしで使えるようにする。

- **`ssh proxy <n>`**: `AWS-StartSSHSession` (`portNumber=22`) でセッションを開き、**自プロセスの stdin/stdout** をデータチャネルに中継する (vendored plugin の `StandardStreamForwarding`)。OpenSSH の `ProxyCommand` から使う。supervisor (§8.11 / ADR 0014) は使わない — ProxyCommand プロセスの寿命と再接続は ssh クライアントの責務。session-runner 子も挟まない (実 stdio を掴む必要があるため)。恒久切断時に hang しないよう `SSM_PLUGIN_DISCONNECT_EXIT_CODE` をプロセス内で設定し、即終了して ssh に接続断を伝える (Issue #61)。プラグインの開始バナーは ssh が識別文字列より前の行を読み飛ばす仕様 (RFC 4253 §4.2) により無害
- **`ssh setup <n>`**: 公開鍵 (`~/.ssh/id_ed25519.pub` 等を自動検出、`--identity` で明示) を SSM Run Command で ubuntu の `authorized_keys` に冪等追加し (`grep -qxF` で既存判定)、ssh config スニペットを `~/.config/nakama/ssh/<n>.conf` (0600) に書き出して `Include` 行を案内する。**`~/.ssh/config` は変更しない** (`~/.aws/config` 不変更と同じ思想)。多行入力 (秘密鍵の誤指定) と非 OpenSSH 公開鍵形式は拒否し、エラーに鍵素材を出さない
- **リモートコマンド実行の共通化**: `runRemoteScript` (SSM Run Command + `InvalidInstanceId` リトライ + stdout 回収) を service 層に用意し、鍵投入で使用する (`update` の既存経路と同型)

### 8.15 `nakama aws doctor` — 環境自己診断 (Issue #53)

企業環境での「動かない」の一次切り分けを 1 コマンドに集約する。**読み取り専用**が不変条件 (AWS リソース・ローカル設定を一切変更しない)。

- **構成**: `service.Diagnose(ctx, DoctorOptions)` が `DoctorReport{Checks[], OKs/Warns/Fails/Skips}` を返し、CLI が表形式または `-o json` で描画して fail>0 なら `ExitCodeError{1}`
- **資格情報未解決でも動く**: CLI は `awsx.NewClients` の失敗を診断結果 (credentials=fail) として扱い、ローカル診断 (proxy / tls) を継続する。AWS 依存項目は skip (終了コードに影響しない)
- **各項目の実装**: proxy/tls = ADR 0018 の `cliproxy.Detector` を再利用 (`ProxyAuthError` は方式別の案内に変換) / credentials = `Clients.AccountID`・`CallerARN` + profilestore の active profile / waf-allowlist = `ListACL` + `IPDiscoverer.PublicIPv4` を CIDR 包含判定。CloudFront を持つインスタンスが 1 台も無ければ (`ListInstances` の distribution-domain で数える) skip にし、あれば詳細にその名前を付ける (ADR 0028) / capability = 後述の権限マトリクス / tunnels = 状態ファイル走査 + `GetTunnelStatus` / version = `version.Checker` (§8.24) / egress = `InspectEgress` (記録した方式、private の経路の向き先、NAT インスタンスと NAT Gateway の一覧) を `evaluateEgress` で判定 (§8.28)
- **version 行**: AWS の外 (api.github.com) に問い合わせる唯一の項目で、資格情報が無くても最後に実行する。最新と、手元の方が新しい場合は ok、古ければ warn (Advice に新しい版のリリースページ)、確認できなければ warn (遮断されている場合の手での確かめ方を案内)、開発ビルドは skip。fail にはしない (GitHub に届くかと版の新しさは、nakama が動くかに関係しないため)
- **機能別 capability 判定 (Issue #63)**: `internal/service/permissions.go` の `featurePermissionMatrix` が「機能 → 必要 IAM アクション」の唯一の情報源。doctor は Simulate 対象行のアクション和集合を `SimulatePrincipalPolicy` にチャンク (50) + Marker ページネーションで問い合わせ、機能ごとに ok / warn (不足アクション列挙) を 1 行ずつ表示する。不足は warn であり fail ではない (意図して権限を絞ったプリンシパルは「壊れた環境」ではない。fail は環境異常に予約)。判定は Resource `*` 評価のためリソース限定ポリシーで偽陰性があり得る旨を Advice に含める
- **マトリクスの鮮度保証**: 双方向の乖離検出テスト (permissions_test.go)。順方向 = パッケージソースから `.clients.<SVC>.<Method>(` を抽出し、対応アクションが表に無ければ失敗。逆方向 = 表のアクションが抽出されず `implicitActions` (ResumeSession = vendored plugin 内部 / PassRole = EC2 が要求 / GetCallerIdentity = awsx 直呼び) にも無ければ失敗。暗黙宣言自体も「抽出されるようになったら失敗」で陳腐化を検出。requirements.md 付録 A は生成ブロックで、表とドキュメントの不一致も CI が検出する (`NAKAMA_UPDATE_PERMISSIONS_DOCS=1` で再生成)
- **テスト容易性**: 検出・ACL・IP・tunnel 一覧/状態・profile を `DoctorOptions` の unexported フックで差し替え、ネットワークなしで全分岐を検証する

### 8.16 プロビジョニング宣言 (`create --setup` / `--dotfiles` / `--preset`、Issue #54)

- **適用点**: `CreateInstance` の最終段 (メタデータ書き込み済みの後。`--cloudfront` なら distribution が Deployed になった後)。**best-effort** — 失敗しても create は成功扱いで `CreateInstanceOutput.Provision` に結果を載せ、CLI が警告 + ログパス + `provision` コマンドを表示する (インスタンスは作成済みで使用可能なため)
- **事前検証**: CLI が `ValidateProvisionSpec` を **AWS 呼び出し前**に実行し、未知の preset・読めないローカルスクリプト・http URL (経路上での改変を許すため拒否) を即座に弾く
- **実行シェルの前提**: aws:runShellScript は commands を `_script.sh` に書き出し**ファイルとして** `sh` 起動する (amazon-ssm-agent の runshellscript.go で確認)。つまり shebang が解釈器を決め、shebang の無いスクリプトは /bin/sh (Ubuntu では dash) で動く。bash 構文 (process substitution・pipefail) を使うスクリプトは必ず `#!/bin/bash` で始めること (Issue #61)
- **リモートスクリプト**: `runRemoteScript` (§8.14 と共通) で 1 本のスクリプトを実行。全出力を `/var/log/nakama-provision.log` に tee し、各ステップを `NAKAMA_STEP_BEGIN/OK/FAILED` マーカーで囲んでサブシェル実行する (1 ステップの失敗が他を隠さない・中断しない)。ステップ順は preset → dotfiles → setup (後段が前段の成果に依存できる)
- **各ステップ**: preset = 同梱レシピ (`kiro` は `internal/kiro.InstallScript()` をそのまま使う。`sudo -u ubuntu -H` で実行) / dotfiles = `~/.dotfiles` へ clone (既存なら `pull --ff-only` で再実行可) → `install.sh`/`setup.sh`/`bootstrap.sh` があれば実行、無ければドットファイルを `$HOME` へリンク / setup = https は `curl` 取得、ローカルは**引用付き heredoc** で内容を転送 (外側シェルの展開を防止。マーカー衝突は拒否)
- **`provision`** (Issue #129 までは `setup rerun`): 同じ `Provision` を既存インスタンスに適用する復旧・再適用手段。冪等性はスクリプト作成者の責務であることを docs に明記

### 8.17 `nakama aws create --bare` — 軽量開発ボックス (Issue #52)

§8.14 の ssh 経路が入ったことで、ブラウザ IDE を使わない構成が実用になった。`--bare` は code-server 一式を作らない。

- **作らないもの**: パスワード (生成も SSM 保存もしない)、code-server の user-data、private DNS 解決、VPC Origin、CloudFront distribution とその Deployed 待ち。ADR 0028 以降は code-server のインスタンスも既定で CloudFront を作らないので、bare との違いは code-server とパスワードの有無になった。ADR 0028 より前に作った bare のインスタンスの SG には CloudFront の受信の規則が残っており、`cloudfront delete <name>` で外せる (§8.30)
- **user-data**: `internal/userdata/bare.tmpl.sh` (SSM エージェントの起動確認・sshd 有効化・aws CLI (§8.22) と基本ツールの導入のみ)。`shell` / `ssh` / `tunnel` が成立する最小構成
- **種別の記録**: `/nakama/<name>/meta/kind = bare`。**未記録は従来の code-server インスタンス**として扱うため、既存インスタンスへの移行処理は不要。空値の PutParameter は SSM が拒否するため、bare では distribution 系のメタキー自体を書かない
- **各コマンドの対応**: `list` は kind を読んで TYPE=BARE 表示 / `info` は Kind を返し bare ではパスワードを読まない / `code` は bare を検出して `ssh setup`・`shell` を案内するエラー / `update` は OS 更新のみのスクリプト (reboot マーカーは共通) / `delete` は空の distribution・VPC Origin メタを既にスキップする構造だったためそのまま動作 / NAT 電源連動は通常インスタンスと同一 (egress が必要)

### 8.18 Kiro CLI / Kiro Crew の既定導入 (Issue #68)

Issue #20 の内容を `--preset kiro` (§8.16) として一般化していたが、常用構成であるため user-data での既定導入に変更した。

- **単一の情報源**: 導入手順は `internal/kiro/install.sh` に 1 つだけ置き、`go:embed` で `internal/kiro.InstallScript()` として公開する。user-data (通常・bare の両テンプレート) と `--preset kiro` の双方がこの同一文字列を使う。テストが「preset == InstallScript()」を固定しているため、片方だけが更新されることがない
- **Kiro Crew の導入コマンド** (Issue #79 で訂正): `kiro-cli crew -y -- --version`。`kiro-cli crew` は未導入なら Kiro Crew を入れ (`-y` で確認を省く)、残りの引数を Kiro Crew の CLI にそのまま渡す。最初の版の `crew -y install` は `kirocrew install` (存在しないサブコマンド) になって exit 2 で止まり、gateway の service install まで進まなかった。文字列だけを見るテストでは見つからなかったため、kiro-cli と kirocrew の引数処理を模したスタブで本物の bash を使ってスクリプトを実行し、service install まで到達することを確かめるテスト (`internal/kiro/install_run_test.go`) を加えた
- **実行経路**: cloud-init は root で動くため、スクリプト自身が `sudo -u ubuntu -H bash -lc` で ubuntu に降りる。インストール先が `$HOME/.local/bin` (公式インストーラの `LINUX_INSTALL_DIR`) であり、`kirocrew service install` 内部の sudo も Ubuntu AMI の passwordless sudo で非対話に通る
- **PATH**: 公式インストーラは shell の rc を変更せず PATH 行を表示するだけなので、`~/.bashrc` への追記をこちらで行う (`grep -qF` による冪等化)
- **必要パッケージ**: インストーラは Linux で `unzip` を必須依存としてチェックする。通常テンプレートには無かったため `unzip unar` を追加した (bare テンプレートは `unzip` があったので `unar` を追加)
- **AppArmor 制約**: `kirocrew service install` が導入する AppArmor profile は unprivileged userns の許可を含むため、`~/.kiro` が group/world 書き込み可能な間は導入を拒否する (他の local user が `~/.kiro/crew-venv/bin/kirocrew` を差し替えて許可を得られてしまう)。ubuntu ユーザーは private group により umask 0002 で動くためツリーは 0775 で作られる。したがって service install の**直前**に `chmod -R go-w "$HOME/.kiro"` を実行する。この順序はテストで固定
- **best-effort**: 両テンプレートは `set -euxo pipefail` で動くため、導入失敗が user-data 全体を落とすと code-server の起動や SSM へのバージョン記録に波及する。スクリプトは `if sudo ...; then ... else WARNING fi` の構造で自身の失敗を吸収し、再実行コマンド (`provision --preset kiro`) を案内する。内側のシェルは `set -euo pipefail` を自前で設定し、最初の失敗で止まる (pipefail はインストーラのダウンロード失敗が空スクリプト = exit 0 として通過するのを防ぐ)
- **配置順**: 通常テンプレートでは code-server の起動とバージョン記録の後 (遅いダウンロードが利用開始を遅らせない)。bare テンプレートではホームの `chown -R` の後 (`~/.kiro` のモードを後から触らない)
- **オプトアウト**: `create --no-kiro` → `CreateInstanceInput.NoKiro` → `userdata.*Params.InstallKiro` (正の意味に反転)。テンプレートは `{{if .InstallKiro}}` で節全体を囲むため、無効時は節コメントすら残らない

### 8.19 Kiro Crew の Web 画面 (`nakama aws kirocrew`、ADR 0020 / Issue #71、#73)

code-server のパス転送 (`/proxy/5476/`) では、Kiro Crew の Host 検査とルートからの絶対パス前提の 2 点に阻まれて画面が開かない (ADR 0020 の Context)。そこで、Kiro Crew 専用の CloudFront distribution、code-server のホスト名による振り分け、インスタンス上の nginx を組み合わせる。

- **経路**: `https://<crew>` → CloudFront (オリジンのカスタムヘッダー `X-Forwarded-Host: 15476.kirocrew.nakama.internal`) → code-server:8080 (パスワード認証 → `proxy-domain` の照合でポート 15476 を選び、パスを変えずに転送) → nginx 127.0.0.1:15476 (Origin を検査し、Host と Origin を `localhost:5476` に書き換え、`X-Forwarded-Proto: https` を付ける。`X-Forwarded-For` は残す) → Kiro Crew 127.0.0.1:5476
- **構成の組み立て**: `distributionConfig()` を code-server 用と Kiro Crew 用で共用する。共通の内容は、VPC Origin が 1 つ、CachingDisabled、AllViewer、全メソッド許可、redirect-to-https、共有の WAF。Kiro Crew 用はこれにオリジンのカスタムヘッダーを足すだけである。code-server 用の構成は、関数に切り出す前と 1 バイトも変わっていない (テストで固定)
- **リダイレクト**: 既存の distribution に `/proxy/5476*` と `/absproxy/5476*` の behavior を先頭に足す。中身は、既定の behavior (`GetDistributionConfig` が返したもの) の全項目を写し、Function の関連付けだけを viewer-request の CloudFront Function 1 件に差し替えたもの (Lambda@Edge は 0 件)。UpdateDistribution は、API が読み出し時に埋める項目 (`SmoothStreaming` など) が欠けた behavior を拒否するので、全項目を写す (`kirocrewCacheBehavior`。#73)。既定の behavior に欠けた項目があれば、API の既定値で補う。SDK に項目が増えたときは、reflect を使ったテストで検出する。Function は `https://<crew>` + 残りのパス + クエリへの 302 を返す。パスの照合は厳密に行うので、`/proxy/54760` は素通しになる。behavior の追加と削除は冪等で、利用者が足した behavior は残す。内容が変わらないときは UpdateDistribution を呼ばない
- **enable** (`cloudfront add --kirocrew`、Issue #129 までは `kirocrew enable`。冪等、途中から再実行できる): code-server の distribution が前提である。CLI は先に `AddCloudFront` (冪等) を呼ぶので、その distribution は必ずある。`EnableKiroCrewWeb` は、それが無いとき (非推奨の `kirocrew enable` から呼ばれたとき) に `cloudfront add --kirocrew` を案内するエラーにする (ADR 0028、§8.30)。`GetKiroCrewWeb` (`kirocrew --internet`) も、Kiro Crew の distribution が無ければ `cloudfront add --kirocrew` を案内する。`EnableKiroCrewWeb` は次の順に進む。① インスタンスの確認 (bare は拒否、停止中は `start` を案内) → ② 既存の distribution の構成から VPC Origin と WAF を読む → ③ Kiro Crew 用 distribution を作り、**作った直後に ID とドメインをメタデータへ書く** → ④ Run Command でインスタンス側を設定する (nginx の導入と設定、`VSCODE_PROXY_URI` を固定する systemd drop-in、`config.yaml` の管理ブロック、code-server の再起動。結果は `NAKAMA_KIROCREW_OK` / `NAKAMA_KIROCREW_ERROR` で返す) → ⑤ Function を作成または更新して LIVE に公開 → ⑥ 既存の distribution に behavior を足す → ⑦ 2 つの distribution が Deployed になるのを並行して待つ。④ が失敗したら ⑥ は行わないので、壊れた設定へ誘導されることはない
- **disable / delete**: `cloudfront delete` (§8.30) は Kiro Crew の分も消すので、disable (`cloudfront delete --kirocrew`、Issue #129 までは `kirocrew disable`) は Kiro Crew の画面だけをやめるときに使う。CLI は `PlanDeleteCloudFront` で Kiro Crew の distribution を確かめ、あれば確認してから (`--yes` で省略。非推奨の `kirocrew disable` は確認しない) `DisableKiroCrewWeb` を呼ぶ。distribution が無ければ確認せず、残りの部品だけを片付ける。disable は、behavior を外して反映を待つ → Function を削除する (CloudFront が使用中と返す間は再試行) → Kiro Crew 用 distribution を削除する → インスタンスが起動していれば後片付けのスクリプトを実行する (停止中なら飛ばして、そのことを知らせる) → 最後にメタデータを消す、の順に進む。delete は、Kiro Crew 用 distribution を**既存の distribution と VPC Origin より先に**消し (どちらも同じ VPC Origin を参照しているため)、Function は既存の distribution を消した後に消す
- **トークン**: `MintKiroCrewToken` が、リクエストのたびに RSA-4096 の鍵を作り、公開鍵を埋め込んだスクリプトを Run Command で実行する。スクリプトは `kirocrew token` の出力からループバックの URL を取り出し、`openssl pkeyutl` (OAEP、SHA-256、MGF1 も SHA-256) で暗号化して `NAKAMA_KIROCREW_TOKEN_RSA <base64>` として返す。復号した値は `[A-Za-z0-9._-]` の 20〜1024 バイトかを確かめ、エラーメッセージにも値を含めない。Run Command の成功ログには、出力の中身を書かずバイト数だけを DEBUG で出す
- **ブラウザの起動**: 1 回限りログインページ (ADR 0012) を `newOneShotLoginServerFor(…, loginTarget{origin, label, extra})` に汎用化した。Kiro Crew では送信先を `https://<crew>/login?to=/&token=…`、href を crew にする。`code` が生成する HTML は変わっていない (テストで固定)。`--no-auth` を付けると `https://<crew>/?token=…` を直接開くので、code-server のログイン画面が先に出る。以上は `--internet` のときの動きで、既定の SSM の経路は §8.29
- **Ports パネル**: code-server は、`proxy-domain` があって `VSCODE_PROXY_URI` が未設定だと、`VSCODE_PROXY_URI=//{{port}}.<proxy-domain>` を自分で設定する (`src/node/cli.ts`)。このままでは Ports パネルとターミナルのリンクがすべて `<port>.kirocrew.nakama.internal` を指してしまう。そこで enable では、code-server を再起動する前に drop-in (`/etc/systemd/system/code-server@ubuntu.service.d/nakama-kirocrew.conf`) を置き、`VSCODE_PROXY_URI=./proxy/{{port}}/` (code-server 自身の既定値) に固定する。利用者が `VSCODE_PROXY_URI` を自分で設定していれば触らない。disable は drop-in を消し、code-server の再起動は 1 回にまとめる
- **メタデータ**: `/nakama/<名前>/meta/kirocrew-distribution-id` と `kirocrew-distribution-domain`。`info` は後者から `kirocrew_url` を表示する
- **予約名**: `enable` と `disable` は (非推奨の) サブコマンド名なので、インスタンス名として予約する。サブコマンドを消すまで予約は残す

### 8.20 `nakama aws shell <n> <cmd>` のコマンド実行と標準入力 (ADR 0021 / Issue #70、#75)

vendor した plugin は NonInteractiveCommands も `Standard_Stream` (ShellSession) で扱うため、標準入力の EOF でセッションが終わり (#70)、終了コードと stderr も伝わらなかった (#75)。子プロセスに payload の `mode` を渡して動作を切り替える。

- **mode**: 空 (raw) = tunnel / ssh proxy (従来どおり)、`command` = コマンド実行、`terminal` = 対話シェルと `--tty`
- **ラッパー** (`commandWrapper`): `mktemp -d` (0700) のファイルに `( cmd ) </dev/null` の stdout と stderr を書き、コマンドが終わってから `\n@@nakama-frame <目印> o <長さ>\n` + 本文、`e` も同様、最後に `x <終了コード>` を出す。目印は実行ごとの 32 文字の乱数で、payload の `marker` で子に渡す。コマンドは 1 行目から始まるので、エラーの行番号はコマンドの行番号になる
- **commandSession**: 子プロセスで `session.Register` により `Standard_Stream` を上書きする (plugin は変更しない)。`Output` を `frameDecoder` に流し (任意の分割に対応、最初の見出しより前は stderr へ)、それ以外の種類 (agent のエラー) は stderr へ。標準入力は読まず、制御シグナル (Ctrl-C / Ctrl-\ / Ctrl-Z) だけを送る。plugin の表示 (`Starting session…`) は `os.Stdout = os.Stderr` で stderr に回す
- **状態パイプ**: リモートの終了コードは `remote-exit <n>` の 1 行で親に返す。unix は `ExtraFiles` の 2 本目 (fd 4)、Windows は継承ハンドル。番号は `NAKAMA_SESSION_STATUS_HANDLE` で伝え、子は受け取ったら環境変数を消して close-on-exec を付ける。親は要求しないときにこの変数を環境から取り除く
- **終了コード**: リモートの終了コード (0〜255) をそのまま返し、何も表示しない (`ExitCodeError{Remote}`、cobra の SilenceErrors、main の空行も出さない)。終了コードを受け取れなかったとき・子が失敗したとき・範囲外のときは 255 と理由。リモートの 74 は切断扱いしない。`--retry` の再接続は、終了コードを受け取る前に切れた場合だけ
- **terminal**: 標準入力が端末でなければ (Windows 以外)、`os.Pipe` に差し替えて `copyTerminalInput` で中継する。行頭の `~` を二重にし (ShellSession のエスケープ回避)、入力が尽きたら Ctrl-D (最後が改行でなければ先に改行)。書き込み側は閉じない (保持して GC で閉じないようにする)
- **separateOutputStream を使わない理由**: agent 3.3.4793.0 は送信を約 2 秒で打ち切り、約 300 KB を超える出力が途切れる (3.3.4851.0 で修正)。既定のモードは 5 MB まで全部届くことを実機で確認した

### 8.21 `nakama aws acl delete` — 複数の削除と `delete all` (Issue #99)

- 複数指定 (`DeleteACLs`): 入力を正規化して重複を除き、IP set を読む。1 つでも無ければ何も変えずに `ErrACLEntryNotFound` (入力のままの表記で列挙)。全部あれば 1 回の `UpdateIPSet` で消し、追加日時を消す。
- `delete all` は計画 (`PlanACLDeleteAll`、読み取りだけ) と実行 (`DeleteACLAll`) に分ける。計画は IP set と SSM の追加日時を読み、消すエントリ・残すエントリ・SSM にだけある残骸 (Stale) を決める。`--keep-mine` は `acl add` と同じ `IPDiscoverer.DetectPublicIPv4s` で接続元 IP を調べ、そのどれかを含む CIDR を残す (1 つも検出できなければエラー。締め出しを防ぐため黙って続けない。§8.23)。CLI は計画を表示して確認を取る。
- 実行は IP set を読み直し、計画にあって今もあるエントリだけを除いた一覧で 1 回の `UpdateIPSet` をする (確認の後に追加されたエントリは残る。読み直しで得た lock token により、その間の変更は `WAFOptimisticLockException` で拒否される)。消すものが無ければ更新しない。`Addresses` は空でも nil にしない (SDK の必須項目)。
- 追加日時は、消したエントリ、計画の後に他から消されたエントリ、まだエントリの無い残骸の分を `DeleteParameter` で消す (`ParameterNotFound` は無視)。IP set が正なので、失敗は警告に留める。
- IP set を空にすると、Web ACL の既定 (Block) により、すべての distribution (Kiro Crew を含む) がブラウザに 403 を返す。SSM 経由の `shell` / `ssh` / `tunnel` は影響を受けない。

### 8.22 AWS CLI の導入と、root からの呼び出し (ADR 0024 / Issue #101)

- **導入**: `internal/awscli/install.sh` の `nakama_install_awscli` が、AWS 公式のインストールスクリプト (`https://awscli.amazonaws.com/v2/install.sh`) を ubuntu ユーザーで実行する。`XDG_DATA_HOME=~/.local` / `XDG_BIN_HOME=~/.local/bin` を渡し、`~/.local/aws-cli` に入れて `~/.local/bin/aws` にリンクを作る。スクリプトは `uname -m` から x86_64 / aarch64 の zip を選び、PGP 署名を検証する。gpg が無いと警告だけで検証を飛ばすため、nakama は gpg が無ければ失敗にする (テンプレートの apt に `gnupg` を入れる)。スクリプトはファイルに落としてから実行する (途中で切れたダウンロードを実行しない)。再実行すると古い版を更新し、最新なら何もしない
- **snap を使わない理由**: snap のアプリは setuid の snap-confine で起動する。Kiro Crew のサンドボックス (unprivileged user namespace) の中では setuid が効かないため動かない
- **PATH**: `~/.bashrc` に `export PATH="$HOME/.local/bin:$PATH"` を足す (Kiro の導入と同じ行。`grep -qF` で重複させない)。ログインシェルは `~/.profile` が `~/.local/bin` を加える
- **root からの呼び出し** (`internal/awscli/run.sh` の `nakama_aws`): user-data のパスワード取得と版の記録、update の版の記録、復元の初回起動スクリプトが使う。公式版は ubuntu が書き換えられる場所にあるため root では実行せず、`sudo -u ubuntu env -i PATH=/usr/bin:/bin HOME=/nonexistent AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_PAGER=` で実行する。ubuntu の AWS の設定は効かず、インスタンスロールで動く。公式版が無いディスク (Issue #101 より前の作成、またはそのバックアップからの復元) では `/snap/bin/aws` を使う。root 用の `/usr/local/bin/aws` は作らない
- **既存のインスタンス**: nakama は移行しない (snap はそのまま、update で公式版を入れない)。利用者が手で同じ場所 (`~/.local/aws-cli`、`~/.local/bin`) に入れれば、以後の update が更新する

### 8.23 `nakama aws acl add` の接続元 IP の検出 (ADR 0025 / Issue #104)

引数なしの `acl add`、`acl delete all --keep-mine`、`acl prune` (§8.25) は、`IPDiscoverer.DetectPublicIPv4s` で接続元の IP を調べる。出口を冗長化したネットワーク (接続ごとに出口が選ばれる) で、見えた出口をすべて登録するため。

- **チェックサイト** (`ipCheckSites`): `https://checkip.amazonaws.com/` と `https://ifconfig.io/ip`。ifconfig.io の `/` は User-Agent によって HTML を返すので、平文の `/ip` を使う。
- **問い合わせ**: それぞれに `IPCheckRounds` (3) 回、計 6 回を同時に行う (1 回の上限は 5 秒)。
- **接続** (`newIPCheckTransport`): keep-alive を切り、1 回ごとに新しい接続にする (HTTP/2 の接続も 1 回きり、プロキシ経由なら毎回 CONNECT)。チェックサイトのホストへ直接ダイヤルするときは `tcp4` にする (`ipCheckNetwork`。`curl -4` と同じ)。プロキシへのダイヤルは変えない。
- **答えの検査** (`publicIPv4`): 本文 (64 バイトまで) を IP として読み、IPv4 (IPv4-mapped は IPv4 に直す) で、グローバルユニキャストで、プライベートでも CGNAT (100.64.0.0/10) でもないものだけを受け付ける。checkip は X-Forwarded-For の先頭の値を返すので、TLS を中継するプロキシの下では社内の IP が返ることがある。
- **結果** (`DetectedIPs`): `IPs` (重複なし、昇順)、`Checks` (6)、`Failures` (サイトとエラー)。1 つも無ければエラー (`FailureSummary` の内容を含む)。
- **`AddACL`**: 見つかった IP を /32 に正規化し、IP set に無いものだけを足して、1 回の `UpdateIPSet` で更新する (無ければ更新しない)。追加日時はすべて書き直す。引数があるときは検出しない。
- **CLI**: 失敗した問い合わせがあれば、サイトごとに `warning: IP check: <サイト>: N of 3 checks failed: <最初のエラー>` と、取りこぼしたかもしれないことを stderr に出す。追加したものが 1 つなら出力は今までと同じ (`added: X` / `{"added": "X"}`)、複数なら 1 行ずつと配列。
- **`--keep-mine`**: 見つかったどれかを含むエントリを残す。確認の文と結果は「your IP X」または「one of your IPs X, Y」と書く。JSON の結果には `my_ips` を加え、`my_ip` (先頭) も残す。
- **`code` / `doctor`**: `PublicIPv4` (checkip に 1 回) のまま。

### 8.24 `nakama version --check` — 新しい版の確認

利用者は Releases から手で入れるので、新しい版に気づけるように、頼まれたときだけ GitHub に最新のリリースを問い合わせる。自己更新はしない。

- **問い合わせ先**: `GET https://api.github.com/repos/ukaji3/nakama/releases/latest` (`config.LatestReleaseURL`)。GitHub は draft でも prerelease でもない最新のリリースを返す。`Accept: application/vnd.github+json`、`X-GitHub-Api-Version: 2022-11-28`、`User-Agent: nakama/<版>` を付け、トークンは送らない (未認証の上限は送信元 IP ごとに 1 時間 60 回)。
- **問い合わせる場面**: `version --check` と `aws doctor` (§8.15) だけ。引数なしの `version` (CI のスモークテストも含む) とほかのコマンドは通信せず、出力も変えない。定期的な確認・結果のキャッシュはしない。
- **通信** (`version.Checker`): `proxy.NewTransport()` (§14) と、全体で 5 秒の `http.Client.Timeout` (`version.CheckTimeout`)。クライアントは `Check` のたびに作るので、`--check` が無ければトランスポートも作らない。応答は 1 MiB まで読み、`tag_name` だけを使う。リリースページの URL は応答の `html_url` を使わず、検証したタグから `config.ReleasesURL + "/tag/" + <tag>` を組み立てる。
- **比較** (`internal/version/check.go`): semver 2.0.0 の優先順位。先頭の `v` は無視する (タグは `v0.24.0`、GoReleaser のリリースバイナリは `{{.Version}}` の `0.24.0` を報告する)。数字は数値で比べ、プレリリースはその版より前、プレリリースの識別子は数字なら数値・それ以外は ASCII 順、ビルドメタデータは無視する。golang.org/x/mod/semver は依存グラフに無く、比較は数十行で済むので、依存は足さない。
- **開発ビルド**: 版が semver でないもの (`dev`、`git describe --always` のハッシュ) と、`make build` の `git describe --tags --dirty` の形 (`v0.24.0-3-g9010bd5`、`-dirty` 付き) は比較しない。後者は semver としてはタグのプレリリースに見え、古いと誤って言うため。最新の版と URL は表示する。
- **結果** (`version.CheckResult`): Status は up-to-date / outdated / ahead (手元の方が新しい) / development / unknown。接続できない、タイムアウト、200 以外、壊れた JSON、版でないタグは unknown とその理由 (`Err`) になり、呼び出し側は警告だけにする。`X-RateLimit-Remaining: 0` の 403 / 429 はレート制限として伝える。
- **CLI**: `current :` / `latest  :` / `release :` / `status  :` の 4 行。unknown のときは `current :` の行と、stderr の `warning: could not check for a newer version: <理由>`。終了コードは古くても失敗しても 0。`-o` は効かない (引数なしの `version` と同じ)。
- **テスト**: `httptest` のサーバーで各 Status、HTTP エラー、レート制限、タイムアウト、壊れた JSON、プロキシの環境変数の尊重を確かめる。doctor のテストは確認の関数を差し替える。どのテストも api.github.com に問い合わせない。

### 8.25 `nakama aws acl prune` — 古い許可 IP の整理

`acl add` は追加日時 (`/nakama/_meta/acl/<cidr>`) を毎回書き直すので、それは「最後に `acl add` で確かめた時刻」になる。`acl prune --older-than <期間>` はそれが古いエントリを消す。形は `acl delete all` (§8.21) の計画と実行の組と同じ。

- **期間** (CLI の `parseACLAge`): 正の整数の日数 (`30d`) か、正の Go の duration (`36h`)。境界 (cutoff) は「現在 − 期間」で、AWS を呼ぶ前に検査する。
- **計画** (`PlanACLPrune`、読み取りだけ): IP set と追加日時を読み、エントリを分ける。追加日時が読めない・解釈できないものは `Unrecorded` (nakama の外で足したもの。消さない)、cutoff より前 (`Before`。ちょうどは古くない) は古い、それ以外は `Keep`。古いものがあるときだけ `DetectPublicIPv4s` で接続元 IP を調べ (§8.23)、どれかを含む古いエントリは `Mine` として残す。判定は `--keep-mine` と同じ `cidrContainsAny`。IP が 1 つも検出できなければエラー (締め出しを防ぐ)。追加日時の読み取り (`fetchACLTimestamps`) の失敗もエラーにする (読めないと全部が「nakama の外」に見え、利用者に誤った警告を出すため)。`list` と `delete all` は今までどおり警告だけで続ける。
- **CLI**: `Unrecorded` があれば stderr に警告する。消すものがあれば、消すもの・残すもの (`Mine`、`Keep`、`Unrecorded`) を経過日数付きで表示して確認を取る (`--yes` で省略)。
- **実行** (`PruneACL`): IP set を読み直す。消すものが無ければ、結果の残りの一覧を作るだけで IP set は更新しない (追加日時も読まない)。あれば追加日時も読み直し、計画の `Delete` のうち、今も IP set にあり、今も cutoff より古いものだけを除いた一覧で、読み直しの lock token 付きの 1 回の `UpdateIPSet` をする。計画の後に足されたエントリは計画に無いので残り、その間に `acl add` で確かめ直されたエントリ (`Refreshed`) も残る。他から消されていたもの (`Gone`) は数えるだけ。
- **後片付け**: 消したエントリの追加日時と、`Gone` のうち今も古い追加日時を `DeleteParameter` で消す (新しい追加日時は進行中の `acl add` のものなので触らない)。失敗は警告に留める。
- **`acl list`**: ADDED_AT の隣に AGE (`now − 追加日時` の切り捨ての日数、未来なら 0d、記録が無ければ `-`) を出す。JSON / YAML の出力は変えない。
- **時刻が更新される操作**: `acl add` だけ。`code` (§8.10 の WAF soft check) や code-server の利用では更新しない。毎日 `code` だけ使う利用者のエントリも古く見えるので、`prune` の前に `acl add` を勧める (ヘルプ、チュートリアル 3 章)。
- **権限**: 使う API (`ListIPSets`、`GetIPSet`、`UpdateIPSet`、`GetParametersByPath`、`DeleteParameter`) は `acl` の行にすべてある。追加の IAM 権限は要らない。

### 8.27 `nakama aws delete` — 記録の無い SG・ENI・EC2 をタグで探す

create / restore が SG・ENI・EC2 を作ってから SSM に記録するまでの間に止まると (記録の書き込みの失敗、作成の応答を受け取る前の中断)、その ID は SSM に無い。`delete <n>` は、SSM に ID が無い種類についてだけ、その名前の nakama のタグで探す (`recordedOrTagged`)。SSM に ID があればそれだけを使い、タグでは探さない (従来どおり)。

- **SG**: `DescribeSecurityGroups`。タグは `upsertInstanceSecurityGroup` と同じ (`nakama:managed=true` + `Name=nakama-sg-<n>` + `nakama:name=<n>`、`instanceResourceTags`)。`vpc-id` のフィルタは付けない (`DescribeVpcs` の権限を要らなくするため)。
- **ENI**: `DescribeNetworkInterfaces`。タグは `upsertInstanceENI` と同じ (`Name=nakama-eni-<n>`)。`subnet-id` のフィルタは付けない。
- **EC2**: `DescribeInstances`。タグは `runInstance` が付けるもの (`nakama:managed=true` + `nakama:name=<n>` + `nakama:component=ec2` + `Name=nakama-<n>`)。状態は pending / running / stopping / stopped (terminated と shutting-down は除く)。
- **ほかの名前を拾わない**: EC2 のタグのフィルタは、ワイルドカードを含まない値では完全一致で、インスタンス名は `[a-z0-9-]` なので、`dev01` で `dev010` や `dev0` は拾わない。返った結果もタグ (EC2 は状態も) をもう一度確かめ (`carriesNakamaTags`)、合わないものは消さない。名前をまたいで残骸を探す掃除はしない。
- **複数見つかったとき**: どれもその名前のタグを持つので、すべて消す。見つけた ID (`delete: found by tags, not in SSM`) と消した ID (`instance: terminated` / `eni: deleted` / `sg: deleted`) をログに出す。
- **探せないとき**: 権限で拒否された (`UnauthorizedOperation` / `AccessDenied`) ときは、記録の無いものを探せなかったことと足りない権限名 (`missing_permission=ec2:DescribeInstances` / `ec2:DescribeNetworkInterfaces` / `ec2:DescribeSecurityGroups`) を警告して続け、記録されたものを消す。前の権限表に合わせて絞ったポリシーでも delete が動く。ほかのエラーでは、何も消さずにエラーで止まる (`--force` でも。SSM の読み取りの失敗と同じ理由で、黙って成功にしない)。
- **権限**: `ec2:DescribeNetworkInterfaces` を `delete` の行に足した (SSM に ENI の記録が無いときだけ使う)。`ec2:DescribeInstances` と `ec2:DescribeSecurityGroups` は前からある。
- **範囲外**: VPC Origin と distribution はタグで探さない。

### 8.28 外向き通信の方式 — NAT Gateway と NAT インスタンス (ADR 0026 / Issue #115)

private subnet の外向き通信は、`gateway` (NAT Gateway) か `instance` (NAT インスタンス、ADR 0007) のどちらかを通る。VPC に 1 つで、private の経路表の `0.0.0.0/0` が向く先で決まる。

- **方式の決め方** (`planNATMode`、`PlanInit` は読み取りだけ): `--nat` > `/nakama/_meta/nat-mode` の記録 > nakama の VPC があれば `instance` (ADR 0026 より前の環境は変えない) > `gateway` (新しい環境)。記録に知らない値があればエラー (新しい版が書いた値を推測しない)。`PlanInit` は、既存環境で方式が変わるとき `Switch` と `From`、稼働中 (pending / running) の code-server インスタンスの名前を返す
- **CLI** (`nakama aws init --nat gateway|instance [--yes]`): `--nat` の値を AWS を呼ぶ前に検査する。`Switch` なら、作るもの・消すもの・料金・稼働中のインスタンス (外向きの接続と shell / ssh / tunnel のセッションが張り替えの瞬間に切れる) を表示して y/N を聞く (`--yes` で省略)。新しい環境の `gateway` では聞かず、料金の注記を stderr に出す。その後 `Init(InitInput{NATMode: plan.NATMode})`
- **記録**: init は AZ の確認の後、何かを作る前に方式を記録する (`writeNATMode`)。途中で止まった init は、次の init が同じ方式で続ける。新しい環境で VPC を作った後に止まっても、既存環境として `instance` にはならない
- **`initEgress` の順序** (新しい NAT → 経路 → 古い NAT。経路を張り替える前に失敗したら古い NAT が残る):
  - `gateway`: `initNATGateway` → `initRouteTables(routeTargetNATGateway)` → `removeNATInstance`
  - `instance`: `initNATInstance` → (private の経路が NAT Gateway を向いていれば `initNATForwarding`) → `initRouteTables(routeTargetNetworkInterface)` → `removeNATGateway`。切り替えでないときの転送の確認は、今までどおり init の最後 (WAF の後)
- **`initNATGateway`**: VPC 内の nakama の NAT Gateway (削除済み・削除中を除く) から `available` > `pending` > `failed` の順に 1 つ選ぶ (`preferredNATGateway`。残りは警告だけ)。`failed` なら削除して待ち、作り直す。無ければ EIP を用意し (`nakama-natgw-eip` のタグで、関連付けの無いものを再利用。無ければ `AllocateAddress`)、`CreateNatGateway` (public subnet、`ConnectivityType: public`、作成時にタグ `nakama:managed` / `nakama:component=natgw` / `Name=nakama-natgw`)。`available` を待つ (10 秒ごと、最大 10 分。`failed` などはエラー)。最後に、使っている EIP 以外のタグ付きの EIP を解放する (`releaseNATGatewayEIPs`。関連付けのあるものは残して次の init に回す)。作成時のタグにより、作った直後に中断しても次の init が見つける
- **`removeNATInstance`**: VPC 内の nakama の NAT インスタンス (terminated・shutting-down を除く) をすべて terminate して待ち、`nakama-nat-sg` を削除する (`deleteSG` が `DependencyViolation` を再試行)。NAT インスタンスの IAM ロールとプロファイルは残す。gateway の init は NAT 用のロールを作らない (`initIAM`)
- **`removeNATGateway`**: VPC 内の nakama の NAT Gateway をすべて削除して待ち、タグ付きの EIP を解放する。ADR 0007 より前の環境の移行も兼ねる (同じ名前のため)
- **実行時** (create / start / update / 削除済みインスタンスの restore): `ensureEgress` は、NAT インスタンスがあれば起動し、無ければ NAT Gateway が `available` かを確かめる (`pending` なら待つ。無い・`failed` なら init を案内してエラー)。stop / delete / edit / update の後 / restore の電源の再調整 (`reconcileNATInstancePowerStateExcluding`) は、NAT インスタンスが無ければ何もしない。判断に記録を使わないのは、instance の利用者に新しい権限 (`ec2:DescribeNatGateways`) を求めないため。gateway では NAT インスタンスを terminate するので、リソースと記録は一致する。切り替えの途中で両方があるときは NAT インスタンスを起動し、どちらの経路でも外へ出られるようにする
- **表示**: `InitResult` に `NATMode`、`NATGatewayID` / `NATGatewayEIPAllocationID` / `NATGatewayPublicIP`、片付けたもの (`DeletedNATGatewayID`、`ReleasedNATGatewayEIPID`、`TerminatedNATInstanceID`、`DeletedNATInstanceSGID`。カンマ区切り) を載せる。init の表は方式に合わせて NAT の行を出し分ける。`list --show-nat` は NAT インスタンス (TYPE `NAT`) と NAT Gateway (TYPE `NATGW`) の行を出し、外向きの IP を `PublicIP` (表では最後の列に `egress <IP>`) で示す。`ec2:DescribeNatGateways` が拒否されたら警告して NAT Gateway の行を省く
- **doctor の `egress` 行** (`InspectEgress` → `evaluateEgress`): 経路表が無ければ warn (init を案内)。経路の先が使えない (消えた NAT Gateway、`available` でない NAT Gateway、どの NAT インスタンスのものでもない ENI、既定の経路が無い、ほかの何かを向く) なら fail。切り替えの残り (gateway なのに NAT インスタンスがある、instance なのに NAT Gateway がある) と、記録 (未記録は instance) と実際の経路の不一致は warn。詳細は「<方式> mode (recorded / none recorded)」、NAT の説明、見つかった問題の順に `; ` でつなぐ
- **`TargetNotConnected` の案内**: shell / ssh / tunnel が StartSession の `TargetNotConnected` で諦めるとき、`currentNATMode` (NAT インスタンス → NAT Gateway の順に探す) で見つかった NAT に合わせた確認方法を案内する。見つからない・調べられないときは両方を挙げる
- **古い版との混在** (ADR 0026 の Decision 8): NAT Gateway の名前は `nakama-natgw` のまま。ADR 0026 より前の版の init は、この NAT Gateway を削除して NAT インスタンスへ戻す。新しい版の doctor が記録とのずれを warn で知らせ、`init` で gateway に戻せる
- **権限**: 付録 A。init に `ec2:AllocateAddress` / `ec2:CreateNatGateway` / `ec2:DeleteSecurityGroup` / `ec2:TerminateInstances`。`egressActions` (create / start / update / restore) と `natLookupActions` (shell / ssh / tunnel) と list / doctor に `ec2:DescribeNatGateways`。doctor に `ec2:DescribeRouteTables` / `ec2:DescribeInstances` / `ssm:GetParameter`
- **テスト**: `init_egress_test.go` の状態を持つフェイク (NAT インスタンス、SG、経路表、NAT Gateway、EIP。変更の呼び出しを順に記録) で、両方向の切り替えの順序、失敗時に古い NAT が残ること、`failed` の NAT Gateway の作り直し、2 回目の init、`initIAM` の NAT 用ロールを確かめる。`nat_mode_test.go` は方式の決め方、`egress_test.go` は `InspectEgress` と doctor の判定、`nat_instance_test.go` は `ensureEgress` と電源の再調整を確かめる。実際の AWS での確認は手動 (§9)

### 8.29 ブラウザからの接続を SSM で — `code` / `kirocrew` と転送の監督 (ADR 0027 / Issue #118)

`code` と `kirocrew` は、既定でインスタンスの tunnel デーモン (§8.12) に転送を足し、ルーター (§8.31) の URL をブラウザで開く。`--local-port` のときは `http://localhost:<ポート>`。CloudFront の経路 (§8.10、§8.19) は `--internet` のときだけ。

```
ブラウザ ─ http://code-server.<名前>.nakama.localhost:18888 (127.0.0.1 / ::1)
  → nakama ルーター (§8.31。Host で転送を選ぶ)
  → 127.0.0.1:<空きポート> (`--local-port` ならそのポート)
  → tunnel デーモン (forwardRunner。ポートを持ち続け、接続を中継する)
  → 127.0.0.1:<空きポート> (session-runner 子の plugin。セッションごとに選び直す)
  → SSM (AWS-StartPortForwardingSession) → インスタンスの code-server:8080 / Kiro Crew 127.0.0.1:5476
```

- **転送の追加** (`EnsureBrowserForward`、`tunnel_browser.go`): `--local-port` が無ければ、まずルーターを起動するか確かめる (`EnsureRouter`、§8.31。失敗すれば何も始めない)。状態ファイルのデーモンが応答すれば、同じリモートポート (ホスト指定なし) の dead でない転送を再利用する (`findBrowserForward`。ルーターが選ぶのと同じ転送。`--local-port` の明示と食い違えば `ErrLocalPortMismatch`)。無ければ `POST /forwards` で転送を足す (404 は古いデーモン → `tunnel restart` を案内)。デーモンが無ければ `StartTunnel` で起動する。ローカルポートは、`--local-port` があればそのポート (`code-server=18080:8080`)。無ければ、システムが選んだ 127.0.0.1 の空きポートのうち ::1 でも空いているもの (`pickFreeLocalPort`。`code-server=<空きポート>:8080`)。デーモンが bind する前に取られて 409 が返れば、別のポートで足し直す (5 回まで)。`--local-port` のポートがふさがっていれば、状態ファイルからその転送のインスタンスを探して示す (`LocalPortInUseError`)。その後、HTTP (`/healthz` / `/api/health`) が応答するまで最大 60 秒待つ。ルーターを使うときはルーターを通して送り (Host はルーターの名前)、ルーター自身のエラーのページ (`X-Nakama-Router-Error`) は応答とみなさない。dead になれば理由を返し、時間切れでは最後の理由と、`LastErrorCode` が TargetNotConnected なら NAT の案内を返す
- **デーモン** (`tunnel_daemon.go`): 転送ごとに `startForward` がローカルポートを bind し (`listenLoopback`。::1 が使える機械で ::1 がふさがっていれば使用中)、`forwardRunner` を動かす。同じローカルポートの dead の転送は置き換え、生きている転送とは衝突 (409)。足した転送は状態ファイルに書き直す (`rewriteTunnelState`。トークンが一致するときだけ、一時ファイル + rename)。起動時の転送の bind に失敗したら、その転送を dead にして続ける。子の出力は転送ごとの `lineWriter` で OS のログへ
- **forwardRunner** (`tunnel_forward.go`): 
  - セッション 1 回: instance ID を読む → 空きポートを選ぶ → StartSession → 子を起動 (`SSM_PLUGIN_DISCONNECT_EXIT_CODE` 付き) → 空きポートに接続できるまで待つ (30 秒、200 ms ごと) → 接続を中継する → 子が終わるか検知が失敗するまで
  - 中継: 届いた接続はセッションができるまで最大 30 秒待ち、そのセッションの世代に結び付く。セッションが終わると、その世代の接続を閉じる。待っている接続は残す
  - 判断: 子の終了はすべて再試行。StartSession と instance ID の読み取りの失敗は `classifyForwardError` (応答なし・TargetNotConnected・スロットリング・5xx・429 → 再試行、資格情報 → 60 秒ごと、その他の 4xx → dead)。instance ID が無ければ dead。待ち時間は `backoffDelay` (1 秒から 30 秒)、60 秒以上使えたら 1 秒に戻す。再試行の前に `proxy.Reload`
  - 必要なときだけ: ローカルの接続が開いているか、5 分以内に見たときだけ張り直す。そうでなければ `idle` で次の接続を待つ
  - 検知: 用途 (`ForwardPurpose`) のある転送で、接続が開いているあいだ 15 秒ごとに HTTP (Host `localhost:<ポート>`) を送り、10 秒で失敗、3 回続いたら子を止め、TerminateSession してから張り直す。確立の時間切れと確立前の子の失敗、子の一般的な失敗のあとも TerminateSession する
  - 時間の値はパッケージ変数で、テストが短くする
- **code の SSM 経路** (`runAWSCodeSSM`): `GetInstanceInfo` (bare と停止中は拒否。distribution は要らない) → `EnsureBrowserForward` → 1 回限りのページの送信先を `<オリジン>/login` (ルーターの URL か `http://localhost:<ポート>`) にした METHOD A → METHOD C。ブラウザを起動できなければ (`browserLaunchError`) URL を出さずにエラー。WAF の確認はしない。ルーターのポートが使用中なら `RouterPortInUseError` (`NAKAMA_ROUTER_PORT` の案内) をそのまま返す
- **kirocrew の SSM 経路** (`runKirocrewOpenSSM`): `GetInstanceInfo` (停止中は拒否。bare も可) → `EnsureBrowserForward` → `MintKiroCrewToken` (`readKirocrewInstance` は bare を拒否しない) → 1 回限りのページ (`tokenPageTemplate`、`GET <オリジン>/` に hidden の `token`)。期限内に読まれなければ `<オリジン>/?token=…` を直接開く。`--no-auth` はトークン無しで開く。`EnableKiroCrewWeb` / `GetKiroCrewWeb` は使わない
- **Cookie**: user data と update の `nakama_set_cookie_suffix` (`internal/userdata/cookie_suffix.sh`) が、code-server 4.107.0 以降の config.yaml に `cookie-suffix: <名前>` を足す (既にあれば触らない)。restore は config.yaml のパスワードの行だけを替えるので、そのまま残る。ルーターの URL ではホスト名がインスタンスごとに違うので、Cookie はそれだけで分かれる (`--local-port` で `localhost` のポートを並べたときのために残す)
- **delete**: 最後に `stopLocalTunnel` でこの機械の同名のデーモンを止める (失敗は警告)
- **テスト**: `tunnel_forward_test.go` (本物のソケットで plugin を模し、再接続・待ち・検知・idle を確かめる)、`tunnel_browser_test.go`、`tunnel_browser_router_test.go`、`tunnel_daemon_test.go`、`aws_code_ssm_test.go`、`instance_update_run_test.go` の cookie-suffix。パッケージの `TestMain` は tunnel の状態ディレクトリを一時ディレクトリに向け、テストが利用者のデーモンに触れないようにする

### 8.30 CloudFront はインスタンスごとの任意 — `create --cloudfront` と `cloudfront add` / `delete` (ADR 0028 / Issue #120)

インスタンスは既定で CloudFront を持たず、SG にも受信の規則が無い (§1)。CloudFront の経路は、`create --cloudfront` / `restore --cloudfront` (削除済みの作り直し) で作るか、あとから `nakama aws cloudfront add <name>` で足し、`nakama aws cloudfront delete <name>` で外す。実装は `internal/service/cloudfront.go`、CLI は `internal/cli/aws_cloudfront.go`。

- **作る部品** (`ensureInstanceCloudFront`、`instance_create_cf.go`。create / restore / add で共通): private DNS を引く (distribution を作るときだけ) → `CreateVpcOrigin` → `meta/vpc-origin-id` を記録 → `Deployed` を待つ → `CreateDistribution` → `meta/distribution-id` / `meta/distribution-domain` を記録。渡された部品 (すでにあるもの) は作らない。distribution の `Deployed` 待ちは呼び出し側 (create はすぐ、restore はルートの差し替えの後、add は最後)
- **前提の部品** (`resolveCloudFrontPrereqs`): CloudFront prefix list と共有の WAF web ACL。どちらも何かを作る前に引く
- **add** (`AddCloudFront`): 次の順に進む。途中で止まっても再実行で続きから進み、2 つ目の VPC origin や distribution は作らない
  1. 記録を読む (`readCloudFrontMeta`)。インスタンスが無い、bare、`init` していない、running でない、SG の記録が無い、のいずれかならエラー (VPC origin は完全にデプロイされた Active なリソースにしか作れないため、running を求める)
  2. `meta/cloudfront-deleting` があれば、途中で止まった delete の片付けを先に求めるエラー
  3. 記録された VPC origin と distribution を CloudFront に問い合わせる (`existingInstanceCloudFront`)。無くなっていれば記録を捨てて作り直す。distribution が disable されていれば (nakama は削除のときだけ disable する)、delete で片付けるよう案内するエラー。distribution があって VPC origin が無い、Kiro Crew の distribution だけがある、のは nakama が作らない組み合わせなので、delete → add を案内するエラー。distribution のドメインは CloudFront の値で記録し直す
  4. SG に CloudFront の受信の規則を足す (`ensureSGIngress`。すでにあれば何もしない)
  5. `ensureInstanceCloudFront` → distribution の `Deployed` を待つ
  6. 出力 (`AddCloudFrontOutput`、両方すでにあったときは `already_present`)。CLI は、この端末の IP が WAF の許可リストに無ければ警告する (`warnIfNotAllowlisted`、失敗しても続ける)
- **delete** (`PlanDeleteCloudFront` → 確認 → `DeleteCloudFront`): 計画は読み取りだけで、確認の画面に消す distribution (ドメイン付き)、Kiro Crew の distribution、VPC origin、SG の規則を並べる (`--yes` で省略。CloudFront が無ければ確認しない)。実行は次の順
  0. CloudFront があれば `meta/cloudfront-deleting=true` を書く
  1. SG の CloudFront の受信の規則を外す (`revokeSGIngress`。規則が無ければ AWS は成功で `UnknownIpPermissions` に返す。古い挙動の `InvalidPermission.NotFound` と、SG が無い `InvalidGroup.NotFound` も「無い」と扱う)。ここでインスタンスへの CloudFront からの経路は切れる (実機では、delete を始めて 15 秒後の `/healthz` が 504 だった)
  2. `deleteCloudFrontResources` (`nakama aws delete` と共通。§8.3 の手順 3)。force なし
  3. Kiro Crew の distribution があった場合、インスタンスが running なら `kirocrewDisableScript` (nginx のサイト、code-server の proxy-domain と drop-in) を Run Command で実行する。running でなければ飛ばし、出力の `instance_skipped` と、`cloudfront delete --kirocrew` での後片付けを案内する
  4. `meta/` の distribution-id / distribution-domain / vpc-origin-id / kirocrew-* / cloudfront-deleting をまとめて消す
- **途中で止まった delete の印**: 実機での 1 回の観察では、`DeleteVpcOrigin` の後、VPC origin は消えるまで `Deploying` を返し続けた (削除中を表す状態は返らなかった。状態の値は API リファレンスに列挙されていない)。状態からは「途中で止まった delete」を見分けられないので、SSM の印 (`meta/cloudfront-deleting`) で見分ける。`nakama aws delete` は `/nakama/<name>/` ごと消すので、この印も消える
- **bare と古いインスタンス**: delete は bare でも受け付ける。CloudFront が無いインスタンスでは手順 1 だけを行い、規則が残っていれば外す (ADR 0028 より前の bare)。既存の CloudFront は自動では消さない
- **restore**: `RestoreInput.CloudFront` / `RestorePlan.CloudFront`。バックアップの作成 (`backupSource`) は `meta/distribution-id` があれば `nakama:cloudfront=true` を付け、作り直しの計画は、このタグがあって `--cloudfront` が無いときに警告する (§8.5.1)
- **表示**: `info` は CloudFront が無ければ `cloudfront : none` と開き方を出し、JSON に `cloudfront` (bool) を入れる。`code --internet` は CloudFront が無いとき `cloudfront add` を、`kirocrew --internet` は Kiro Crew の distribution が無いとき `cloudfront add --kirocrew` を案内する。`list` は変えない (DISTRIBUTION_DOMAIN が空)
- **権限**: 権限表の `create` / `restore` の行は CloudFront の操作を含まない。`cloudfront` の行が add / delete と `--cloudfront` の分 (`cloudFrontBuildActions` と `cloudFrontTeardownActions`。`ec2:RevokeSecurityGroupIngress` を含む) を持つ
- **テスト**: `cloudfront_test.go` (順序、各段階での失敗と Ctrl-C からの再開、無くなった部品の作り直し、前提の拒否、delete の順序・Kiro Crew の有無・停止中・再開・反映待ち、bare の残りの規則、add → delete の往復、`nakama aws delete` との共通化)、`instance_create_record_test.go` (`--cloudfront` の有無ごとの記録)、`restore_test.go`、`backup_test.go`、`doctor_test.go`、`aws_cloudfront_test.go`

### 8.31 ブラウザ用の転送のルーター — `*.nakama.localhost` (ADR 0029 / Issue #124)

ブラウザは、インスタンスの転送のローカルポートではなく、利用者ごとに 1 つのルーターにつなぐ。ルーターは Host の名前からインスタンスと用途を読み、その転送へ中継する。ポートを選ぶ必要も覚える必要も無く、インスタンスごとにオリジンが分かれる。実装は `internal/service/router.go` (中継)、`router_daemon.go` (プロセス)、`router_start.go` (CLI 側)、CLI は隠しコマンド `internal/cli/aws_router_daemon.go`。

```
ブラウザ ─ http://code-server.dev01.nakama.localhost:18888/   (*.localhost はブラウザか OS がループバックに解決)
  → nakama ルーター 127.0.0.1:18888 / [::1]:18888 (nakama aws router-daemon。利用者ごとに 1 つ)
      Host → (code-server, dev01) → ~/.config/nakama/tunnels/dev01.json → デーモンの GET /status
      → 8080 への dead でない転送のローカルポート (findBrowserForward)
  → 127.0.0.1:<空きポート> (dev01 の tunnel デーモン) → SSM → インスタンス
```

- **アドレス** (`parseRouterHost`): `<code-server|kirocrew>.<名前>.nakama.localhost:<ポート>`。名前の部分はインスタンス名の正規表現そのもの (`instanceNamePattern`)。大文字と末尾のドットを受け付ける。ポートがルーターのポートでなければ、nakama の名前でも受け付けない
- **ポート** (`RouterPort`): 既定 18888、`NAKAMA_ROUTER_PORT` (1–65535) で変える。8888 (Jupyter の既定) と 80 (特権、既定のポートでは HTTPS への自動の切り替えを受けうる) は避けた
- **振り分け** (`tunnelRoutes`): tunnel の状態ファイル → `queryTunnelStatus` → `findBrowserForward(status, "8080" | "5476")` (ホスト指定のない、dead でない最初の転送。`EnsureBrowserForward` の使い回しと同じ関数)。答えは 2 秒 (`routerLookupTTL`) 覚え、中継が失敗したら忘れる。状態ファイルは読むだけで、消さない (残りの掃除は CLI のコマンドがする)。失敗は `routeError` (`routeNoTunnel` / `routeDaemonGone` / `routeNoForward` / `routeForwardDead`)
- **中継** (`routerHandler`、`httputil.ReverseProxy` の `Rewrite`): `Rewrite` は `Forwarded` と `X-Forwarded-For` / `-Host` / `-Proto` を落とし、自分では足さない (`Director` は `X-Forwarded-For` を足す)。`X-Real-IP` は自分で消す。用途ごとの書き換え (`rewriteRouterRequest`)
  - code-server: `Out.Host = In.Host` (Host をそのまま。WebSocket の `authenticateOrigin` が Origin と Host を照合する)
  - Kiro Crew: Host を `localhost:5476` に。Origin がルーター自身のオリジンと一致すれば `http://localhost:5476` に、Referer がルーターのオリジンの URL なら同じオリジンの URL に置き換える (それ以外は変えない)。応答の `Location` が `http://localhost:5476` の URL なら、ルーターのオリジンに戻す (`rewriteKiroCrewLocation`。`http://localhost:54760` のような別のポートは対象外)。`127.0.0.1` に置き換えないのは、Kiro Crew が Cookie の無い文書の GET を `localhost` へ 302 で移すため
  - WebSocket (101) と event stream は `ReverseProxy` がそのまま扱う (`text/event-stream` と長さの分からない応答はすぐ flush する)
- **転送先への接続** (`newRouterTransport`): 127.0.0.1 だけ、プロキシを使わない、HTTP/1.1、keep-alive の遊休は 10 秒 (`routerUpstreamIdleTimeout`)。デーモンは開いているローカルの接続を利用中とみなす (§8.29 の「必要なときだけ」) ので、遊休の接続を持ち続けない
- **エラーのページ** (`writeRouterPage`): `Cache-Control: no-store`、`X-Nakama-Router-Error: <題名>`、html/template。知らない Host は 421。転送が無い・デーモンが応答しない・dead は 502 で、`nakama aws code <名前>` (Kiro Crew は `kirocrew`)、dead なら理由と `tunnel status` を示す。中継が失敗したときは、デーモンの最後の理由を添え、`LastErrorCode` が TargetNotConnected なら `nakama aws start <名前>` を先に示す (`upstreamErrorPage`)。`httpHealthCheck` は `X-Nakama-Router-Error` の付いた応答を失敗として扱うので、code の待ちがルーターのページで終わることはない
- **プロセス** (`RunRouterDaemon` → `runRouterDaemon`): `listenLoopback` でポートを取る (失敗なら状態を書かずに終わる) → 制御のチャネル (127.0.0.1:0、bearer token) → 状態ファイル `~/.config/nakama/router.json` (0600。PID・ポート・制御ポート・トークン・版・開始時刻。tunnel の状態のディレクトリの外。`router` は有効なインスタンス名なので)。O_EXCL で作れなければ、前の持ち主に問い合わせ、応答すれば (別のポートのルーター) 終わり、応答しなければ置き換える → 中継と制御を始める (`GET /status`、`POST /stop`。unix では SIGTERM / SIGINT も停止) → `watchRouterIdle` が 15 秒ごとに応答する tunnel のデーモンを数え (`countLiveTunnels`。読むだけ)、0 が 1 分続いたら止める。ログは `openDaemonLogger("nakama-router")` (syslog のタグ。Windows はイベントログのソース `nakama`)
- **CLI 側** (`EnsureRouter`。`EnsureBrowserForward` が `--local-port` の無いときに呼ぶ): 状態ファイルのルーターが応答し、版 (`version.String()`) とポートが同じなら使う。応答しなければ状態ファイルを消す。版かポートが違えば、ポートが違うときは新しいポートが空いているかを先に確かめ (空いていなければ、動いているルーターを止めずに `RouterPortInUseError`)、`POST /stop` で止めて状態ファイルが消えるまで待つ (停止の要求が失敗したら、そのルーターは動いているので状態ファイルを残し、何も起動せずに pid・ポート・版を示すエラーにする)。そのあとポートが 127.0.0.1 と ::1 で空いているかを確かめ (`RouterPortInUseError`。nakama の転送が使っていれば `tunnelHoldingPort` でそのインスタンス名も示し、`NAKAMA_ROUTER_PORT=<ポート+1>` の例を出す)、隠しコマンドを tunnel のデーモンと同じく切り離して起動し、状態ファイルと `/status` が応答するまで 15 秒待つ。2 つの CLI が同時に起動しても、ポートを取った 1 つが両方に使われる
- **表示**: `GetTunnelStatus` は、ルーターが動いていれば、用途ごとに `findBrowserForward` の転送へ `URL` (`http://<用途>.<名前>.nakama.localhost:<ポート>/`) を入れる (デーモンは送らない。CLI 側で埋める)。`tunnel status` は表の後に `router: running on localhost:<ポート> (pid N)` と、URL の一覧 (名前を省いたときは名前付き) を出す。ブラウザ用の転送があるのにルーターが無ければ、`router: not running` と code / kirocrew での起動を案内する。`GetRouterStatus` は資格情報なしで動き、応答の無いルーターの状態ファイルを消す
- **テスト**: `router_test.go` (名前の判定、書き換え、転送用のヘッダー、421、エラーのページ、WebSocket、event stream、遊休の接続、ルーターのページを応答とみなさないこと、`tunnelRoutes`)、`router_daemon_test.go` (本物のソケット。状態ファイル、制御、両方のループバック、停止、tunnel が無いときの終了、使用中のポート、残った状態ファイル、別のルーター、tunnel のデーモンの `/status` を通した通し)、`router_start_test.go`、`tunnel_browser_router_test.go`、`aws_tunnel_test.go`。パッケージの `TestMain` と `withTunnelStateDir` は、tunnel の状態のディレクトリを一時ディレクトリの下の `tunnels` に向け、`router.json` もその一時ディレクトリに置かれるようにする

### 8.32 コマンドとフラグの名前の整理 (ADR 0030 / Issue #129)

v0.30.0 で、次のように名前を変えた。古い名前は v0.30.x の間は動き、削除は v0.31.0 以降に別の Issue で行う。

| 古い名前 | 新しい名前 |
|---|---|
| `open <name>` | `code <name>` |
| `connect <name> [cmd...]` | `shell <name> [cmd...]` (`--tty` に `-t` を足した) |
| `setup rerun <name>` | `provision <name>` |
| `kirocrew enable <name>` | `cloudfront add <name> --kirocrew` |
| `kirocrew disable <name>` | `cloudfront delete <name> --kirocrew` |
| `create --name <name>` | `create <name>` |
| `login --name` / `profile save --name` | `--alias` |

- **古いコマンド** (`deprecatedCommand`、`internal/cli/deprecated.go`): 新しいコマンドの構築関数をもう 1 回呼び、`Use` の最初の語を古い名前に替え、`Deprecated` に `use "<新しい名前>" instead` を入れる。cobra は `Deprecated` のコマンドをヘルプと補完から外し、実行のときに `Command "<古い名前>" is deprecated, …` を `OutOrStderr` (既定は stderr) に書く。stdout と `-o json` は変わらない。引数とフラグは新しいコマンドと同じになる (`connect` の `SetInterspersed(false)` も)。`setup` のグループも `Deprecated` にした。`kirocrew enable` / `disable` は、元の処理のまま `Deprecated` にした (確認もしない)
- **古いフラグ**: `create --name` と、`login` / `profile save` の `--name` は、pflag の `MarkDeprecated` で隠す。使うと、cobra が解析の後に `Flag --name has been deprecated, …` を stderr に書く。新旧の両方が違う値で与えられたら、`resolveCreateName` / `aliasFlags.resolve` が、AWS を呼ぶ前に (login ではブラウザを開く前に) エラーにする
- **`create <name>`**: ヘルプに名前の規則 (2〜32 文字、英小文字・数字・ハイフン、先頭と末尾は英数字。`instanceNamePattern`) を書く。以前のフラグの説明 `[a-z0-9][a-z0-9-]*` は長さと末尾を表していなかった
- **`cloudfront add --kirocrew`**: `AddCloudFront` (冪等) → `warnIfNotAllowlisted` → `EnableKiroCrewWeb` (§8.19)。後半が失敗したら、code-server の分はできていることと、同じコマンドで続きから進むことをエラーに書く。JSON は、add の結果 (`AddCloudFrontOutput` を埋め込む。YAML は inline) に `kirocrew` (`EnableKiroCrewWebOutput`) を足したもの。`--kirocrew` が無ければ、出力は以前と同じ
- **`cloudfront delete --kirocrew`**: `PlanDeleteCloudFront` → Kiro Crew の distribution があれば確認 (`kiroCrewDeletePrompt`。`--yes` で省略) → `DisableKiroCrewWeb`。distribution が無ければ確認せず、残りの部品だけを片付ける
- **`backup delete` の確認**: `FindBackup` で対象を引き、ラベル・スナップショット・大きさ・作成時刻・メモを示して確認する (`backupDeletePrompt`。`-y` / `--yes` で省略)。確認した `Backup` をそのまま `DeleteFoundBackup` に渡すので、示したものと別のスナップショットは消さない。標準入力が EOF なら `readYesNo` がエラーを返し、何も消さない
- **doctor の項目名**: `code`、`shell`、`cloudfront --kirocrew`、`update / provision`。`-o json` の `feature` の値も変わる
- **変えないもの**: ルーターのホスト名 (`code-server.<名前>.nakama.localhost`。サービスの名前で、`kirocrew.` と対になる)。service 層の `Connect` / `connect_session*.go`、syslog のタグ `nakama-connect`、隠しコマンド `session-runner` (SSM のセッションの仕組みの名前)。`code` と `kirocrew` が共用する `openFlags` などの部品 (この open は動詞)。インスタンスの設定ファイルに書く印 (`# BEGIN nakama kirocrew (managed by …kirocrew enable…)` など。消すときにこの文字列で探す) と、Run Command のコメント (`nakama: kirocrew enable <名前>`)
- **テスト**: `deprecated_test.go` (古い名前が同じ引数とフラグで動く、ヘルプに出ない、案内が stderr だけに出る)、`aws_create_test.go`、`aws_login_test.go`、`aws_profile_test.go`、`aws_cloudfront_test.go`、`aws_kirocrew_test.go`、`aws_backup_test.go`、`aws_shell_test.go`、`flag_usage_test.go` (フラグの説明が大文字で始まる)、`backup_test.go`

## 9. テスト戦略

| 層 | テスト方針 |
|---|---|
| `internal/userdata` / `internal/awscli` / `internal/kiro` | テンプレ展開のユニットテストに加え、埋め込んだシェルを本物の bash で、外部コマンドを模したスタブを PATH の先頭に置いて実行する (文字列の照合だけでは見つからない不具合のため。§8.18)。埋め込みスクリプトの改行が LF であることも検査する (`lf_test.go`) |
| `internal/naming` / `internal/tagging` | ピュア関数のテーブル駆動テスト |
| `internal/awsx` | 資格情報解決の分岐は `resolverHooks` を差し替えて検証する。IP 検出は `httptest` のサーバーで検証する。実 API は呼ばない |
| `internal/signin` / `internal/proxy` / `internal/cliproxy` / `internal/version` | `httptest` のサーバーと、差し替えた依存 (時計、HTTP クライアント、`t.Setenv` による環境変数) で検証する |
| `internal/service` | AWS interface を自家製 fake (`fakeCreateEC2` など) で差し替えたユニットテスト。Run Command で送るスクリプトの一部 (`update`、`kirocrew`、`shell` のコマンド実行など) は bash とスタブで実行する。CloudFront の構成のように SDK の型に項目が増えうるものは、reflect で項目を列挙して漏れを検出する |
| `internal/cli` | cobra のフラグ解析と委譲テスト |
| 実 AWS E2E | 手動 (CI には載せない) |

## 10. CI

GitHub Actions (`.github/workflows/ci.yml`) で以下を main への push と PR で実行する。Actions は commit SHA で固定する。

- Linux (ubuntu-latest): `go mod tidy` で差分が出ないこと、`gofmt -s` (third_party を除く)、`go vet ./...`、`go test -race -count=1 ./...`、ビルドと `--help` / `version` のスモークテスト
- Windows (windows-latest): `go vet ./...` + `go test -count=1 ./...` (`-race` なし。race ランタイムが Windows では cgo / gcc に依存するため) + ビルド & スモークテスト
- `govulncheck ./...`
- `golangci-lint run` (設定は `.golangci.yml`。gosec などを有効化)
- THIRD-PARTY-NOTICES の鮮度 (`go run ./tools/gen-third-party-notices -check`)
- `goreleaser check` と、publish しない snapshot ビルド

タグの push では `.github/workflows/release.yml` が GoReleaser でリリースを作る。依存の更新は Dependabot (`.github/dependabot.yml`) が提案する。

## 11. 用語注

- **VPC Origin** = CloudFront が VPC 内のリソース (ALB / NLB / EC2) をオリジンにできる新機能。2026 年 5 月時点で東京リージョン対応済 ([source](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html))
- **WebSocket on VPC Origins** = 2026 年 5 月 GA ([source](https://aws.amazon.com/about-aws/whats-new/2026/05/amazon-cloudfront-websockets-vpc-origins/))

## 12. `nakama aws shell` (Issue #129 までは `connect`) の取り込み方針 (案3)

詳細根拠は [adr/0005-aws-connect-via-session-manager.md](adr/0005-aws-connect-via-session-manager.md)。

- AWS 公式 `aws/session-manager-plugin` は Go module 未公開 (リポジトリ root に go.mod が無く `vendor/` 方式)。そのため **特定 commit/tag のソースを `third_party/session-manager-plugin/` にコピーして版固定**する (案3)。
- コピーには go.mod を付与し依存 (aws-sdk-go-v2 / gorilla/websocket / twinj/uuid / golang.org/x/crypto / seelog 等) を pin。nakama の go.mod には次のように書き、公式 import パスを保ったままローカル固定する (require の版はダミーの pseudo-version):

```
require github.com/aws/session-manager-plugin v0.0.0-00010101000000-000000000000
replace github.com/aws/session-manager-plugin => ./third_party/session-manager-plugin
replace github.com/twinj/uuid => ./third_party/twinj-uuid
```

- `twinj/uuid` は、公開されている版が取り込み版の plugin と API 互換でないため、plugin の `vendor/` にあった版を `third_party/twinj-uuid/` に同梱して replace する。

- 取り込むパッケージ: `src/sessionmanagerplugin/session` (+ `shellsession` / `portsession`) / `datachannel` / `communicator` / `message` / `encryption` / `log` / `config` / `sessionutil` / `retry` / `websocketutil` / `version`。`ssmcli` 系は取り込まない。
- Apache-2.0 のため取り込みツリーに `LICENSE` / `NOTICE` を保持。取り込み元の tag / commit と、取り込み後に当て直すローカルパッチは `third_party/session-manager-plugin/PINNED.md` に記録する。取り込みは `scripts/vendor-ssm-plugin.sh [tag]` で行う。
- 追加実装:
  - `internal/awsx`: `SSMAPI` に `StartSession` / `TerminateSession` を追加し、解決済 `aws.Credentials` + region + SSM endpoint を `session.Session` 構築用に取り出す口を設ける
  - `internal/service/connect.go`: instance-id 解決、モード→`DocumentName`/`Parameters` 組立、`StartSession`、plugin セッション起動
  - `internal/cli/aws_shell.go`: cobra コマンド (`--tty` / `-t` / 位置引数、`SetInterspersed(false)`)、`aws.go` の `AddCommand` に配線 (ポートフォワードは `aws_tunnel.go` へ移管。§8.12)
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
   aws-sdk-go-v2/config v1.32.0 以降は、既定チェーン自体が
   `login_session` を解決する (`credentials/logincreds`)。そのため、
   `aws login` / `nakama aws login` が書いたプロファイルは、通常ここで
   解決される。このとき、トークンの更新とキャッシュの書き換えは SDK が
   行う (形式の違いは §13.3)。
3. **login_session fallback** (SDK が login_session を解釈しなかった
   頃からの互換レイヤ。2 が失敗したときだけ使う)。ユーザーの AWS config (`AWS_CONFIG_FILE` 尊重、
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
| `TokenCache` | AWS CLI バイト互換のトークンキャッシュ (`AWS_LOGIN_CACHE_DIRECTORY` または `~/.aws/login/cache`、ファイル名 = hex(sha256(login_session)) + ".json"、dir 0700 / file 0600、temp+rename のアトミック書き込み)。`accessToken.expiresAt` は AWS CLI と同じ `2006-01-02T15:04:05Z` で書き、読むときは任意の RFC 3339 を受け付けて UTC にそろえる (SDK for Go v2 が書き戻すファイルは、小数秒と手元の時差を含む。例: `2026-10-15T03:00:00.123456789+09:00`) |
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
`--alias` で上書き、collision 検査は profile save と共通) を行う。

## 14. アウトバウンド通信のプロキシ解決 (ADR 0015)

nakama の outbound HTTP(S) 経路は 6 つあり、いずれも `internal/proxy` を
通してプロキシを解決する (third_party は非改変):

| 経路 | 注入方法 |
|---|---|
| AWS SDK 全クライアント | `internal/awsx` の `loadConfig` で `config.WithHTTPClient(BuildableClient.WithTransportOptions(...))` により `Transport.Proxy = proxy.Func()` を設定。active profile / 既定チェーンの両パスが同一関数を通る |
| Session Manager data-channel (WSS) | `service.RunSessionRunner` (session-runner 子プロセス) で `websocket.DefaultDialer.Proxy = proxy.Func()`。vendored plugin の websocketutil はこの package 変数を参照するためパッチ不要。副作用は子プロセス内に閉じる |
| checkip / ifconfig.io (`IPDiscoverer`) | `internal/awsx/ip.go` で `proxy.NewTransport()` (http.DefaultTransport の Clone に Proxy 差し替え) を使用。`DetectPublicIPv4s` 用はさらに keep-alive を切り、チェックサイトへの直接の接続を IPv4 にする (§8.23) |
| AWS Sign-In のトークン API (`internal/signin`) | 既定の HTTP クライアントが `proxy.NewTransport()` + 30 秒タイムアウト (§13.3) |
| `nakama aws cli shell` の環境検出 (`internal/cliproxy`) | `proxy.Func()` で解決したプロキシを経由して STS エンドポイントに接続し、検証済みチェーンのルートで TLS 傍受を判定する (ADR 0018、§8.13) |
| GitHub Releases API (`internal/version`) | `version --check` と `doctor` のときだけ `api.github.com` に問い合わせる。`proxy.NewTransport()` + 5 秒のタイムアウト (§8.24) |

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
