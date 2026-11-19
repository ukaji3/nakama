# ADR 0026: 外向き通信の中継に NAT Gateway も選べるようにし、NAT インスタンスと切り替えられるようにする

- Status: Accepted
- Date: 2026-11-19
- Issue: [#115](https://github.com/ukaji3/nakama/issues/115)
- 関連: ADR 0007 (NAT インスタンスの電源連動。その「NAT Gateway を使わない」という決定を、この ADR が一部置き換える)

## Context

private subnet の開発環境は、外向き通信 (apt、code-server や拡張機能の取得、SSM エージェントの接続) を NAT で中継する。ADR 0007 以来、その中継は NAT インスタンス (`t4g.nano`、Amazon Linux 2023) だけで、開発環境が止まっている間は NAT インスタンスも止めて固定費を避けてきた。

インターネットからの攻撃が増えていることを受けて、利用者から攻撃面を減らしたいという要望があった。NAT インスタンスは、nakama が OS を管理するホストを public subnet に置く。コードから分かる攻撃面は次のとおり。

- **自前で管理する OS**: カーネル、SSM エージェント、iptables。セキュリティ更新は、初回起動時と `init` による修理のときだけ (ADR 0007 の追記)。その間に公開された脆弱性は、次の修理まで残る。
- **VPC 内からの到達**: NAT インスタンスの SG は、VPC の CIDR (`10.20.0.0/16`) からの全プロトコルを許可している (`ensureNATSecurityGroupIngress`)。乗っ取られた開発環境 (悪意のある拡張機能や npm パッケージなど) から、NAT インスタンスのどのポートにも届く。
- **外向き通信の盗聴と改ざん**: NAT インスタンスを乗っ取ると、全インスタンスの外向き通信を見て書き換えられる。

インターネットから NAT インスタンスへ新しく張る接続は、SG の規則で許可されていない。NAT Gateway にして減るのは、上の 3 つである。

一方で、料金を抑えたいので NAT インスタンスを使い続けたい、という利用者もいる。NAT Gateway は、存在する間ずっと時間料金がかかる (開発環境を全部止めても残る)。さらに、通過する通信量 (connect / tunnel を含む) と EIP にも料金がかかる。

## Decision

1. **方式を 2 つにする**。`instance` (今の NAT インスタンス、ADR 0007) と `gateway` (NAT Gateway)。VPC に 1 つで、private subnet 2 つが共有する private の経路表の `0.0.0.0/0` がどちらかを向く。インスタンスごとには選べない。
2. **NAT Gateway は zonal にする**。public subnet 1a に置き、EIP を 1 つ付ける (`ConnectivityType: public`)。ADR 0007 より前と同じ構成で、作り方は分かっている。
3. **選び方**: `nakama aws init --nat gateway|instance`。init は、何かを変える前に、選んだ方式を SSM の `/nakama/_meta/nat-mode` に記録する。途中で止まった init は、次の init が同じ方式で続きをやる。`--nat` が無ければ、次の順に決める (`PlanInit`)。
   - 記録があれば、その方式
   - 記録が無く、nakama の VPC があれば `instance`。この ADR より前に作った環境は、新しい版を入れても、経路も料金も変わらない
   - 記録も VPC も無ければ (新しい環境) `gateway`。攻撃面の少ない方を既定にする
4. **切り替えの順序**: 新しい NAT を用意して確かめ、private の経路を `ReplaceRoute` で張り替え、最後に古い NAT を片付ける (`initEgress`)。経路を張り替える前に失敗したら、古い NAT のまま止まる。
   - **gateway へ**: EIP を用意し (タグの付いた未使用のものがあれば再利用)、NAT Gateway を作って `available` を待つ (最大 10 分)。経路を張り替えたら、NAT インスタンスを terminate し、その SG を削除する。NAT インスタンスの IAM ロールとインスタンスプロファイルは、戻すときのために残す。新しい環境の gateway では、NAT 用のロールを作らない。`failed` の NAT Gateway は削除して作り直す。使われていない EIP は解放する。
   - **instance へ**: NAT インスタンスを作り、経路を張り替える前に転送を確かめる (Issue #87 の確認と修理)。転送しない NAT インスタンスに経路を渡さないため。その後、NAT Gateway を削除し、EIP を解放する。切り替えでないとき (経路がすでに NAT インスタンスを向いているとき) は、確認は今までどおり init の最後に行う。
5. **確認を取る**。既存の環境で方式が変わるときは、CLI が何をするか (作るもの、消すもの、料金) を表示して y/N を聞く (`--yes` で省略)。稼働中のインスタンスがあれば、名前を挙げて、経路を張り替える瞬間に外向きの接続と connect / ssh / tunnel のセッションが切れることを伝える (connect と tunnel は再接続する)。新しい環境を gateway で作るときは聞かず、料金の注記を stderr に出す。
6. **NAT Gateway は常時動かす**。gateway では、create / start / stop / delete / edit / update / restore が NAT の電源を操作しない。外向き通信が要るコマンド (create、start、update、削除済みインスタンスの restore) は、NAT Gateway が `available` かを確かめ (`pending` なら待ち)、使えなければ init を案内してエラーにする (`ensureEgress`)。
7. **実行時の方式は、記録ではなくリソースから判断する**。NAT インスタンスがあれば NAT インスタンスの電源を扱い、無ければ NAT Gateway を確かめる。gateway では NAT インスタンスを terminate するので、両者は一致する。こうすると、NAT インスタンスの方式の利用者には新しい権限が要らない (`ec2:DescribeNatGateways` は gateway のときだけ呼ぶ)。記録を使うのは init と doctor だけ。
8. **名前は `nakama-natgw` / `nakama-natgw-eip` のまま**。ADR 0007 より前の NAT Gateway と同じ名前である。この ADR より前の版の init は、この名前の NAT Gateway を削除し、経路を NAT インスタンスへ戻す。別の端末で古い版の init を実行すると、gateway の環境は instance に戻る。それでも、課金される NAT Gateway が残ることはない。別の名前にすると、古い版の init は経路だけを NAT インスタンスへ張り替えるので、使われない NAT Gateway が課金されたまま残る。新しい版の doctor と init は、記録と実際の経路のずれを知らせる。
9. **表示**: init の出力に方式と NAT の ID を出す (gateway なら外向きの IP も)。`list --show-nat` は NAT Gateway の行 (TYPE `NATGW`、ID、状態、外向きの IP) も出す。`doctor` に `egress` の行を足す: 方式、経路の向き先、NAT の状態。経路の先が使えなければ fail、切り替えの残り (使われない NAT) や記録とのずれは warn。connect / ssh / tunnel の `TargetNotConnected` の案内は、見つかった NAT に合わせる。
10. **権限表**: init の行に `ec2:AllocateAddress`、`ec2:CreateNatGateway`、`ec2:DeleteSecurityGroup`、`ec2:TerminateInstances` を足す。create / start / update / restore は `ec2:DescribeNatGateways` を、list / info と connect / ssh / tunnel も `ec2:DescribeNatGateways` を使う。doctor は `ec2:DescribeInstances`、`ec2:DescribeNatGateways`、`ec2:DescribeRouteTables`、`ssm:GetParameter` を使う。list と connect 系では、拒否されても止まらない (list は警告、案内は一般的な文面になる)。

## Consequences

- gateway では、nakama が OS を管理するホストが無くなる。NAT Gateway には SG もログインできる OS も無く、VPC 内から踏み台にできない。外向きの IP が EIP で固定されるので、外部サービスの IP 許可リストにも登録できる。
- gateway では、開発環境を全部止めても、NAT Gateway の時間料金と EIP の料金が残る。通過する通信量にも料金がかかる。README の「止めればほぼ無料」は、instance を選んだときの説明になる。
- public subnet は残る。instance のために要り、zonal の NAT Gateway もそこに置く。public subnet の `MapPublicIpOnLaunch` も今までどおり。
- NAT Gateway は 1 つの AZ (1a) に置く。開発環境の ENI も 1a に作るので、AZ をまたぐ通信は増えない。1a の障害には弱い (NAT インスタンスと同じ)。
- 切り替えの間、経路を張り替える瞬間に、外向きの接続と SSM のセッションが切れる。NAT Gateway の作成で数分、NAT インスタンスの作成と確認で数分、古い NAT の削除で数分、init が延びる。
- この ADR より前の版の init を、gateway の環境で実行すると instance に戻る (Decision 8)。切り替える前に、すべての端末の nakama を新しい版にする。
- create / start などは、gateway のとき NAT の起動を待たない。そのぶん速くなる。

## 確認

- フェイクの EC2 / SSM / IAM を相手に、新しい環境の gateway、instance から gateway への切り替え (NAT Gateway が `available` になってから経路を張り替え、その後に NAT インスタンスを terminate する順序)、NAT Gateway の作成の失敗 (経路も NAT インスタンスも変わらない)、`failed` の NAT Gateway の作り直し、2 回目の init で何も変わらないこと、gateway から instance への切り替え (経路の張り替えより前に転送を確かめる順序)、転送しない NAT インスタンスへの切り替えの失敗 (経路と NAT Gateway が残る)、記録の無い既存環境が instance のままであることを、テストで確かめた。順序のテストは、順序を入れ替えた実装で失敗することも確かめた。
- 実際の AWS での確認は、この ADR の時点では行っていない。CreateNatGateway の所要時間、public の経路ができる前に作った NAT Gateway が `available` になること (ADR 0007 より前の実装と同じ順序)、切り替えの瞬間の SSM セッションの切断と再接続は、実機で確かめる必要がある。

## Alternatives considered

- **Regional NAT Gateway (自動モード)**: public subnet と EIP の管理が要らない。ただし、新しい AZ への展開に最大 60 分かかり、その間の通信は別の AZ で処理される。nakama の ENI が 1 つも無い状態で作ったときの動作は、文書から読み取れなかった。instance を残すかぎり public subnet は無くせないので、利点も小さい。将来の候補として残す。
- **NAT Gateway を電源に合わせて作り直す**: 全部止めたら削除し、起動のときに作る。固定費は消えるが、start が数分遅くなる。ADR 0007 で採用しなかった理由と同じ。
- **NAT インスタンスだけを残して強化する** (SG の受信を絞る、定期的に更新する): 攻撃面は減るが、OS を管理するホストは残る。instance を選んだ利用者のための強化として、別の Issue で扱う。
- **別の名前の NAT Gateway**: 古い版の init が経路だけを張り替え、使われない NAT Gateway が課金されたまま残る (Decision 8)。
- **SSM の VPC エンドポイント**: SSM の通信を NAT から外せる。ただし、apt や code-server の取得には一般の HTTPS の外向き通信が要り、NAT は無くせない。エンドポイントにも時間料金がかかる。別の Issue で扱う。
- **IPv6 と Egress-only Internet Gateway**: 時間料金が無く、管理するホストも無い。ただし、外向き通信の先が IPv6 に対応している必要があり、開発環境では現実的でない。
