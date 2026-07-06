# ADR 0007: NAT Gateway を電源連動 NAT インスタンスへ置換

- Status: Accepted
- Date: 2026-07-06
- Context: nakama v1.x コスト最適化 (egress 経路)

## Context

nakama の code-server 用 EC2 は private subnet にあり、アウトバウンド通信
(apt, code-server インストール/更新, 拡張機能取得等) のために egress 経路を
必要とする。v1 ではこれを単一 AZ の **NAT Gateway** で提供していた。

NAT Gateway は作成している限り時間課金される固定費 (ap-northeast-1 で
概ね月 $30〜40 + データ処理料) が発生する。code-server インスタンスは
「使うときだけ起動、普段は停止」という利用形態を想定しているため、EC2 を
停止しても NAT Gateway の固定費が残る点が主要なコスト要因だった
(要件リスク R5)。

## Decision

1. **NAT Gateway を廃し、NAT インスタンスを採用する。**
   - インスタンスタイプ `t4g.nano` (Graviton, 最小サイズ)。
   - AMI は Amazon Linux 2023 arm64 を SSM public parameter
     (`/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64`) から解決。
   - public subnet に配置し、user-data で `net.ipv4.ip_forward=1` と
     iptables `MASQUERADE` を設定、`nakama-nat.service` (systemd oneshot) として
     stop/start をまたいで再適用されるようにする。
   - `SourceDestCheck` を無効化し、ルーティング対象にできるようにする。
   - private route table の `0.0.0.0/0` を NAT インスタンスの primary ENI 宛てにする。

2. **電源を code-server のライフサイクルに連動させる。**
   - code-server インスタンスが 1 台でも `pending`/`running` なら NAT インスタンスを起動。
   - すべての code-server インスタンスが停止/削除されたら NAT インスタンスを停止。
   - 連動は「操作の最終状態」で判定する (`reconcileNATInstancePowerState`)。
     - `create` / `start` / `update`(要 egress): NAT を先に起動してから code-server を起動。
     - `stop` / `delete`: 操作後に稼働中 code-server が無ければ NAT を停止
       (対象インスタンスは判定から除外し、状態伝播を待たずに判断する)。
     - `restore`: 完了時 (インスタンス停止状態) に reconcile。
     - `edit --instance-type`: 型変更中の一時停止では NAT を止めず、最終状態でのみ reconcile。

3. **NAT インスタンスに EIP を付けない。**
   - public subnet の自動割当 public IPv4 を使う。
   - AWS はすべての public IPv4 に時間課金するため、EIP を付けると停止中も
     未アタッチ課金が残る。自動割当なら停止時に IP が解放され課金も止まる。
   - inbound は CloudFront (VPC Origin) 経由のみで、NAT インスタンスの固定 IP は不要。
     起動のたびに public IP が変わっても影響しない。

4. **`nakama aws init` を移行手段にする (破壊的移行を明示)。**
   - init は IAM instance profile 作成 → NAT インスタンス作成 → ルート張り替え
     (旧 NAT Gateway 宛ての `0.0.0.0/0` は `ReplaceRoute`) の順で実行する。
   - ルート張り替え後に旧 NAT Gateway を削除し、その EIP を解放する。
     順序により egress は途切れない。
   - 冪等: NAT インスタンス/ルート/削除対象いずれも「無ければ何もしない」。
     EIP が解放直後の NAT Gateway に握られている場合は次回 init で再試行する。

## Consequences

- 全 code-server 停止時の egress 固定費が実質ゼロになる (NAT インスタンス停止 +
  public IPv4 課金停止 + EBS 8 GiB 相当のごく小さなストレージ料のみ残る)。
- 追加 AWS 操作: `ec2:ModifySubnetAttribute`, `ec2:DeleteNatGateway`,
  `ec2:ReleaseAddress`, `ec2:ReplaceRoute`, および NAT インスタンス起動用の
  `ec2:RunInstances` / `ec2:ModifyInstanceAttribute(SourceDestCheck)`。
- 可用性は NAT Gateway (マネージド, AZ 冗長) より下がる。単一 NAT インスタンスの
  障害時は egress が止まるが、再 `init` またはインスタンス再起動で復旧できる。
  開発用途 (単一利用者・停止許容) では許容範囲と判断。
- NAT インスタンスは自前管理 (カーネル更新等) が必要。AL2023 の自動更新に委ね、
  必要時に手動再作成する。
- `start`/`create`/`update` に NAT 起動待ちが加わるため、初回起動時に数十秒〜
  数分の追加待機が発生し得る。

## Alternatives considered

- **NAT Gateway を維持し stop 時に delete/再作成**: NAT Gateway の作成には数分かかり、
  作成/削除を毎回行うと `start`/`stop` が大幅に遅くなる。EIP 再割当や
  ルート更新の手間も増える。固定費は消せるが UX 劣化が大きく不採用。
- **NAT インスタンスに EIP を付与 (固定 IP)**: 運用は分かりやすいが、停止中も
  public IPv4 の未アタッチ課金が残り、コスト最適化の主目的と衝突するため不採用。
- **egress を廃止 (VPC エンドポイント + キャッシュのみ)**: apt/code-server/拡張の
  取得に一般的な HTTPS egress が要るため、開発用途では非現実的。
- **fck-nat 等の専用 NAT AMI**: 最適化された選択肢だが、外部 AMI 依存と追加学習を
  避け、AL2023 + iptables の自前構成で十分と判断。将来必要なら差し替え可能。
