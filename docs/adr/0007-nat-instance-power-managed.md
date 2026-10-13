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
  必要時に手動再作成する。(2026-10-13 訂正: AL2023 は AMI のリリースに固定され、自動では
  更新されない。初回起動時と init による修理のときに `dnf --releasever=latest upgrade` を
  実行する。下の追記を参照)
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

## 追記 (2026-10-13, Issue #87): t4g.nano での設定の失敗と、init での確認

### 起きたこと

2026-09-30 の AL2023 AMI (kernel 6.18) で作られた NAT インスタンスで、user-data の
`dnf install -y iptables-services` が OOM killer に止められた。t4g.nano のメモリは 412 MiB で、
zram のスワップ (同じ大きさ) も使い切っていた。user-data は `|| true` でこの失敗を無視したので、
`nakama-nat.service` は `iptables: command not found` で失敗した。その結果、NAT インスタンスは何も
転送しなかった。後ろのインスタンスは外に出られず、SSM エージェントが登録されないので、`connect` は
`TargetNotConnected` で失敗した。init は成功と表示していた。6 月の AMI (kernel 6.1) では、同じ
dnf が成功している。

### 決定

1. **設定の手順を 1 つのスクリプトにまとめる** (`internal/service/nat_setup.sh`、`go:embed`)。
   user-data と、init による修理の両方で、同じスクリプトを使う。
2. **t4g.nano のままメモリを節約する**。インスタンスタイプは変えない (料金の判断はこの ADR のとおり)。
   - 入れるのは `iptables-nft` だけ (`iptables` コマンドが nf_tables で動く)。ルールは起動のたびに
     `nakama-nat.service` が入れるので、`iptables-services` (保存したルールの復元) は使わない
   - `install_weak_deps=False`
   - dnf の間だけ、ディスクに 1 GiB の一時スワップ (`/var/tmp`、優先度は zram より低い) を作り、
     終わったら消す
   - dnf は 3 回まで再試行する
3. **セキュリティ更新は必須**。AL2023 の dnf は AMI のリリースに固定されていて、引数なしの
   `dnf upgrade` は何も更新しない。そのため `dnf --releasever=latest upgrade` を毎回実行する
   (初回起動時と修理のとき)。失敗したら設定全体を失敗にする。
4. **失敗を隠さない**。`|| true` をやめ、最後に `ip_forward`、`nakama-nat.service`、MASQUERADE
   のルールを確かめる。
5. **init の最後に、Run Command で転送を確認する** (`initNATForwarding`)。
   - 確認するのは `ip_forward`、`iptables` コマンド、MASQUERADE のルール、`nakama-nat.service`
   - 壊れていれば、設定スクリプトを `systemd-run` の一時的なユニット (`nakama-nat-repair`) で動かして
     直す。dnf が SSM エージェントを更新・再起動しても、修理が止まらないようにするため。その後、直る
     まで確認を繰り返す (最大 25 分)
   - 直らなければ、ログの末尾を付けて init を失敗にする
   - 停止中の NAT インスタンスは、確認のために起動し、終わったら電源を元に戻す (稼働中の code-server
     が無ければ止める)
   - 確認の前には、初回起動の設定 (`cloud-final.service`) が終わるのを、systemd の状態で待つ。
     `cloud-init status --wait` は使わない。OOM で cloud-final.service ごと止められると、cloud-init が
     完了を記録せず、永久に待つため (実機で確認)
6. **`connect` / `ssh` の `TargetNotConnected` に案内を付ける**。NAT インスタンスと init を案内する。

### 確認

アカウント 886252662208 に、一時的な t4g.nano を立てて確かめた。

- 2026-10-12 の AMI で新しい手順を実行した。約 50 秒で完了し、OOM は 0 件。ルール 3 本が入り、
  一時スワップは消えた
- 古い手順で壊れた状態を作り (報告と同じ OOM と `iptables_missing`)、本物の確認と修理を実行した。
  2026-09-30 の AMI では、49 パッケージ (kernel、amazon-ssm-agent を含む) を更新して 96 秒で直り、
  修理中の OOM は 0 件だった
- 最初の実機確認で、AL2023 の `mkswap` には `-q` が無いことが分かった (util-linux 2.37)。失敗を
  隠さない作りなので、すぐ検出できた。テストのスタブは、AL2023 にあるオプションだけを受け付ける
  ようにした

### 結果

- init は、NAT インスタンスの転送が動いていることを確かめてから成功する。壊れた NAT インスタンスは、
  init を再実行すれば直る
- init の時間が延びる。正常なら確認で約 10 秒、停止中なら起動と SSM エージェントの登録待ちで
  1〜2 分、修理が要るなら数分
- init の権限表に、Run Command (`ssm:SendCommand` / `ssm:GetCommandInvocation`) と NAT
  インスタンスの起動・停止を追加した。NAT インスタンスの作成に要るアクション (`ec2:RunInstances`
  など) も漏れていたので、一緒に追加した
