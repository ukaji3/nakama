# ADR 0010: 用途別 instance role (`--with-admin` / `--with-role`) と SimulatePrincipalPolicy プリフライト

- Status: Accepted
- Date: 2026-07-15
- Context: nakama v1.x `create` の IAM ロール付与機能

## Context

`nakama aws create` が起動する EC2 には、`init` が作成する共有ベースロール
`nakama-instance-role` (AmazonSSMManagedInstanceCore + `/nakama/*` への
SSM inline policy のみ) が一律に付与されてきた。しかし VS Code Server 内から
CDK / CloudFormation を操作したいユースケースでは、インスタンスに
AdministratorAccess 相当の権限が必要になる。また、利用者が自前で権限設計した
既存 IAM ロールをそのまま付与したいケースもある。

一方で、ロール付与には呼び出し元プリンシパルに `iam:PassRole` (および
ロール / プロファイルが未作成の場合はその作成権限) が必要であり、権限不足の
まま `create` を進めると、SG / ENI / EC2 / CloudFront などのリソースを
作成した後の `RunInstances` 段階で初めて失敗し、孤児リソースが残る。
nakama v1 は部分失敗時の自動ロールバックを持たない (requirements §7) ため、
**リソースを一切作る前に権限を検査して fail-fast する**必要がある。

## Decision

1. **用途別の共有ロールを追加し、ベースロールは昇格させない。**
   - `--with-admin`: 専用共有ロール `nakama-instance-role-admin` +
     インスタンスプロファイル `nakama-instance-profile-admin` を `create` 時に
     遅延 upsert (冪等、`init` の lookup-then-create パターンを踏襲) し、
     `arn:aws:iam::aws:policy/AdministratorAccess` のみを付与する
     (SSMCore / inline SSM policy は AdministratorAccess が包含するため不要)。
     trust policy はベースロールと同一 (`ec2.amazonaws.com` の
     `sts:AssumeRole`)。
   - `--with-role <name>`: 既存のユーザーロールを付与する。**ユーザーロールは
     一切変更しない** (AttachRolePolicy / PutRolePolicy を実行しない)。
     `iam:GetRole` で存在を確認し、trust policy が EC2 の AssumeRole を
     許可していなければエラー。`iam:ListInstanceProfilesForRole` で既存
     プロファイルがあれば先頭を再利用、なければ
     `nakama-instance-profile-<role>` を作成してロールを追加する。
     AmazonSSMManagedInstanceCore / AdministratorAccess のいずれも付いていない
     場合は警告のみ出す (SSM 接続や userdata のパスワード取得が動かない
     可能性の告知。ブロックはしない)。
   - フラグ省略時は従来どおり共有ベースプロファイルを使い、挙動は不変。
2. **`iam:SimulatePrincipalPolicy` によるプリフライトを、リソース作成前に実行する。**
   - 実行位置は `CreateInstance` の環境チェック (`requireInitialized` /
     `assertNameAvailable`) 直後、パスワード保存やリソース作成より前。
   - 検証アクション: 常に `iam:PassRole` (Resource=対象ロール ARN、
     ContextEntries に `iam:PassedToService=ec2.amazonaws.com`)。
     `--with-admin` でロール未作成なら `iam:CreateRole` /
     `iam:AttachRolePolicy`、プロファイル未作成なら
     `iam:CreateInstanceProfile` / `iam:AddRoleToInstanceProfile` を追加。
   - `PolicySourceArn` には STS GetCallerIdentity の ARN を使う。assumed-role
     セッション ARN (`arn:aws:sts::<acct>:assumed-role/<Role>/<Session>`) は
     policy source として無効なため、`arn:aws:iam::<acct>:role/<Role>` へ
     変換してから渡す (IAM user / root ARN はそのまま)。
   - `EvaluationResults` に `allowed` 以外が 1 つでもあれば、拒否された
     アクション名を列挙してエラー。**Simulate 呼び出し自体の失敗も
     fail-closed** とし、`iam:SimulatePrincipalPolicy` 権限が必要な旨を
     メッセージに含めて中断する。`IsTruncated` は Marker ループで全ページ処理。
3. **ライフサイクル**: 追加したロール / プロファイルはインスタンスの `delete`
   では削除しない (共有ベースロールと同じ扱い)。SSM メタに
   `/nakama/<name>/meta/iam-role` / `.../instance-profile` を記録し、
   `delete` のプレフィックス削除でメタのみ消える。
4. `--with-admin` 指定時は、AdministratorAccess を持つ開発環境が
   CloudFront + WAF allowlist 越しに露出することを実行時に警告する
   (slog.Warn 1 行)。

## Consequences

- 権限不足の呼び出し元が `--with-admin` / `--with-role` を使うと、
  **何もリソースが作られる前に**拒否アクション名つきでエラーになる。
- `--with-admin` のロール / プロファイルはアカウントに 1 組だけ作られ、
  以後の create で再利用される。削除は手動 (nakama は消さない)。
- プリフライトには呼び出し元に `iam:SimulatePrincipalPolicy` が追加で必要に
  なる。持たない場合はプリフライトを実行できない旨のエラーで fail-closed する
  (権限があるのに進めない誤許可よりも安全側に倒す)。
- SimulatePrincipalPolicy は SCP / permissions boundary / セッション
  ポリシーを完全には再現しないため、プリフライト通過後に実際の API が
  拒否される可能性は残る (その場合は従来どおり途中失敗)。逆に
  過剰拒否となるケースは fail-closed の仕様として許容する。
- `--with-role` はユーザーロールを変更しないため、SSM 権限のないロールでは
  `connect` / `update` / userdata のパスワード取得が動かない。これは
  警告で告知し、利用者の権限設計を尊重する。
- 新規作成直後の instance profile は IAM の結果整合性により
  `RunInstances` から見えるまで数秒かかることがある。プリフライト・
  provision を create フローの最初に置くことで、SG / ENI 作成や NAT 起動の
  待ち時間が伝播猶予として働く。

## Alternatives considered

- **共有ベースロール `nakama-instance-role` へ AdministratorAccess を直接
  Attach**: 実装は最小だが、既存・将来の**全**インスタンス (NAT インスタンス
  含む) が管理者権限に昇格してしまいブラスト半径が大きすぎる。不採用。
- **`RunInstances` の DryRun による権限チェック**: `DryRunOperation` /
  `UnauthorizedOperation` で EC2 側の権限は検査できるが、`iam:PassRole` の
  評価は instance profile が実在しないと検証できず、ロール / プロファイル
  作成権限 (`iam:CreateRole` 等) は対象外。プリフライトの目的 (IAM 書き込み
  前の検査) を満たさないため不採用。
- **プリフライトなしで実行し、失敗時にロールバック**: v1 は自動ロールバックを
  持たない方針 (requirements §7) であり、CloudFront 作成後の失敗は掃除に
  15〜20 分かかる。事前検査のほうが利用者コストが低い。不採用。
- **per-instance の admin ロール (`nakama-instance-role-admin-<name>`)**:
  delete 時の掃除が必要になり IAM リソースが増殖する。AdministratorAccess は
  差別化の余地がなく共有で十分。不採用。
- **`--with-role` 指定ロールへの SSMCore 自動 Attach**: ユーザーロールへの
  書き込みは所有権の侵害であり、最小権限で設計されたロールを nakama が
  勝手に拡張すべきでない。警告に留める。不採用。

## References

- ADR 0007 (NAT インスタンス。共有ベースプロファイルを NAT も使用)
- <https://docs.aws.amazon.com/IAM/latest/APIReference/API_SimulatePrincipalPolicy.html>
- <https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_switch-role-ec2_instance-profiles.html>
- <https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_passrole.html>
