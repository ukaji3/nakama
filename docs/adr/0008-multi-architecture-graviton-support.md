# ADR 0008: code-server インスタンスの Graviton (ARM64) 対応

- Status: Accepted
- Date: 2026-07-09
- Context: nakama v1.x インスタンスタイプ拡張 (コスト/性能最適化)

## Context

code-server 用 EC2 のインスタンスタイプは、これまで Intel/AMD (x86_64) 前提で
実装されていた。AMI 解決は Canonical の SSM public parameter のうち **amd64 固定**
のパス (`.../current/amd64/hvm/ebs-gp3/ami-id`) を読み、`create` は常にこの amd64
AMI を起動していた (`AMIResolver.LatestUbuntu2604AMD64`)。

AWS Graviton (ARM64) インスタンス (t4g / m6g・m7g / c6g・c7g / r6g・r7g 系) は
同等 x86 比で価格性能が良く、開発環境のコスト最適化に有効である。しかし
Graviton タイプ (例 `t4g.large`) を指定すると、arm64 インスタンスに amd64 AMI を
起動しようとして `RunInstances` が失敗し、実質 Graviton は利用できなかった。

前提の確認 (2026-07, ap-northeast-1):

- code-server は amd64 と arm64 の両方をリリースしている
  (出典: coder.com/docs/code-server/install「We currently offer releases for amd64
  and arm64」)。user-data は公式 `https://code-server.dev/install.sh` を使用しており、
  このスクリプトが実行環境のアーキを自動判定して arm64 版 `.deb` を導入する。
  → **code-server 導入処理はアーキ非依存で、arm64 でも無変更で動作する。**
- arm64 版 Ubuntu 26.04 の SSM public parameter が実在する
  (`/aws/service/canonical/ubuntu/server/26.04/stable/current/arm64/hvm/ebs-gp3/ami-id`
  → `ami-09cecef4f38e6ab05`, LastModified 2026-07-01)。
- `ec2:DescribeInstanceTypes` の `ProcessorInfo.SupportedArchitectures` で
  各タイプのアーキを判定できる (実測: `t4g.medium`/`m7g.large`/`c7g.large` は
  `["arm64"]`、`t3.medium`/`m6a.2xlarge` は `["x86_64"]`)。

## Decision

1. **インスタンスタイプの CPU アーキを自動判定し、対応する Ubuntu AMI を選択する。**
   - `create` はインスタンスタイプを `DescribeInstanceTypes` で解決し、
     `SupportedArchitectures` からアーキ (`x86_64` / `arm64`) を導出する
     (`resolveInstanceArchitecture` / `selectArchitecture`)。
   - この解決は**存在検証も兼ねる**: 存在しないタイプはここで即時エラーとなり、
     以前のように `RunInstances` まで進んで失敗することがなくなる。
   - 両アーキを advertise する型 (現行の汎用ファミリには存在しない) は、
     歴史的既定を保つため **x86_64 を優先**する。arm64 のみの型 (Graviton) は arm64。

2. **AMI 解決をアーキ別に一般化する。**
   - `config` に amd64 / arm64 の SSM パラメータ定数と
     `UbuntuAMISSMParameterFor(arch)` を追加。
   - `awsx.AMIResolver` を `LatestUbuntu2604AMD64()` から
     `LatestUbuntu2604(ctx, arch)` に一般化する (D1)。

3. **既存インスタンスのアーキ跨ぎ変更 (`edit --instance-type`) は拒否する。** (D2)
   - `edit` は新タイプと現タイプ双方のアーキを解決し、異なる場合は
     **インスタンスを停止せず即時エラー**とする。
   - 根拠: ルートボリュームにはアーキ固有の OS / code-server バイナリが載っており、
     `ModifyInstanceAttribute(InstanceType)` で別アーキの型へ変更しても起動しない。
     別アーキへの移行は `delete` → `create` で行う。

4. **code-server の導入経路は変更しない。**
   - 公式 install.sh がアーキ自動判定するため user-data テンプレートは無変更。

## Consequences

- Graviton (arm64) タイプで `create` できるようになり、開発環境の価格性能を
  選択できる。
- `create` にインスタンスタイプの存在検証が加わる (副次的改善)。以前は create 側に
  型検証が無く `RunInstances` 任せだった。
- 追加の AWS 読み取り: `create` / `edit` で `ec2:DescribeInstanceTypes` を呼ぶ
  (`edit` は新旧タイプで 2 回)。いずれも読み取り専用で影響は小さい。
- `edit` はアーキ跨ぎを明示拒否するため、利用者は別アーキへ移りたい場合に
  delete → create の手順を要する。UX 上の制約だが、破損した起動不能インスタンスを
  作るより安全と判断。
- NAT インスタンス (ADR 0007) は従来どおり AL2023 arm64 (`t4g.nano`) で独立して
  解決され、本 ADR の Ubuntu アーキ解決とは経路が別である。

## Alternatives considered

- **arm64 専用メソッドを追加 (`LatestUbuntu2604ARM64`)**: メソッドが増え、
  呼び分けが分散する。アーキ引数への一般化 (`LatestUbuntu2604(arch)`) の方が
  拡張時 (将来の別アーキ/別 OS) にも素直なため不採用。
- **タイプ名の接尾辞 (例 `g` を含む) でアーキ推定**: 命名規約は将来変わり得るうえ
  例外もあり不確実。`DescribeInstanceTypes` の権威的な `SupportedArchitectures` を
  用いる方が堅牢なため不採用。
- **`edit` のアーキ跨ぎを警告のみで許可**: 起動不能な状態を招きやすく、
  復旧に手間がかかる。明示エラーで事前に止める方が安全なため不採用。
- **arm64 用に別の user-data / インストール手順を用意**: 公式 install.sh が
  アーキ自動判定するため不要。分岐を持たない方が保守しやすい。
