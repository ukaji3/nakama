# ADR 0028: CloudFront はインスタンスごとの任意にし、既定では作らない

- Status: Accepted
- Date: 2026-11-20
- Issue: [#120](https://github.com/ukaji3/nakama/issues/120)
- 関連: ADR 0001 (CloudFront VPC Origins。すべての code-server インスタンスに作る前提を、この ADR が改める)、ADR 0002 (WAF。共有の IP set と web ACL は init が作るまま)、ADR 0020 (Kiro Crew の Web 画面。code-server の distribution の上に作る)、ADR 0023 (削除済みインスタンスの作り直し)、ADR 0027 (ブラウザは既定で SSM)

## Context

ADR 0027 で、ブラウザからの接続は既定で SSM のポート転送になった。ところが `nakama aws create` は、今もすべての code-server インスタンスに CloudFront の VPC origin と distribution を作っていた (`instance_create.go`。削除済みインスタンスの作り直しも同じ。`restore.go`)。さらに `prepareInstanceNetwork` は、bare を含むすべてのインスタンスの SG に、CloudFront managed prefix list から 8080/tcp への受信の規則を付けていた。

SSM のポート転送 (`AWS-StartPortForwardingSession`) は、インスタンス内の localhost につなぐ (`tunnel.go`)。SSM の経路は、この受信の規則も distribution も使わない。インターネットからの攻撃を減らすには、使わない入口を作らないのがよい。利用者からは次の 3 点を求められた。

- CloudFront は基本作らない
- オプションで CloudFront を足せる
- 既存のインスタンスに CloudFront を足せて、外せる

設計の前に確かめたこと:

- **AWS のドキュメント** ([Restrict access with VPC origins](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html)): VPC origin にするリソースは、完全にデプロイされ Active でなければならない。SG の規則は、CloudFront managed prefix list からの許可 (VPC origin を作る前に足してよい) か、サービスが作る `CloudFront-VPCOrigins-Service-SG` からの許可 (VPC origin を作った後でないと足せない。より狭い) のどちらか。VPC origin の Deployed までは最大 15 分。
- **WAF の料金**: web ACL とルールは、存在する時間に応じて日割りで課金される ([AWS WAF pricing](https://aws.amazon.com/waf/pricing/) の FAQ)。金額 (web ACL が月 $5、ルールが月 $1) は第三者の記事の値で、公式の料金表では確かめていない。
- **既存のインスタンス**: この ADR の時点で、利用者の 3 台すべてに CloudFront があり、うち 2 台には Kiro Crew の distribution もあった。

## Decision

1. **create の既定は CloudFront なし**。VPC origin、distribution、SG の受信の規則のどれも作らない。WAF の web ACL と prefix list も引かない。`--cloudfront` を付けたときだけ、従来どおり作る。`--bare` と `--cloudfront` は同時に指定できない。出力の `url` は空になり、JSON に `cloudfront` (bool) を足す。テキストの出力は「CloudFront なし」と、開き方 (`nakama aws open <name>`) を示す。
2. **SG の受信の規則は CloudFront があるときだけ**。新しい SG は受信の規則を持たない。規則は `cloudfront add` (と `--cloudfront`) で足し、`cloudfront delete` で外す。SG の説明文は後から変えられないので、新しい SG の説明文は規則があることを前提にしない。prefix list の方式 (ADR 0001) のままにする。
3. **`nakama aws cloudfront add <name>`**: running の code-server インスタンスに、create と同じ部品を同じ順に作る。SG の規則 → VPC origin (作った直後に記録) → Deployed 待ち → distribution (作った直後に記録) → Deployed 待ち。create・restore・add は同じ関数 (`ensureInstanceCloudFront`) を使う。途中で止まっても、再実行で続きから進む。記録した部品が CloudFront から消えていれば作り直し、すべてそろっていれば何も作らない。停止中・bare・未初期化のインスタンスは何も作らずにエラーにする。Kiro Crew の distribution は作らない (`kirocrew enable` で別に作る)。完了時に、この端末の IP が WAF の許可リストに無ければ警告する。
4. **`nakama aws cloudfront delete <name>`**: インスタンスは残し、Kiro Crew の CloudFront も一緒に消す (同じ VPC origin を使うので、code-server の分だけは消せない)。順序は次のとおり。
   1. 印 (`/nakama/<name>/meta/cloudfront-deleting`) を書く
   2. SG の規則を外す。ここから先が失敗しても、CloudFront からインスタンスへは届かない
   3. Kiro Crew の distribution、code-server の distribution、Kiro Crew の Function、VPC origin
   4. Kiro Crew の CloudFront があった場合、running ならインスタンス上の nginx のサイトと code-server の proxy-domain を外す。停止中なら残して、`kirocrew disable` での後片付けを案内する
   5. 記録と印をまとめて消す

   消すもの (distribution とドメイン、Kiro Crew の distribution、VPC origin、SG の規則) を示して確認を取る (`--yes` で省略)。CloudFront の無いインスタンスでは確認せず、残っている規則を外すだけにする (この ADR より前の bare は規則を持っている)。`--force` は付けず、再実行で続きから進む。
5. **途中で止まった delete は印で見分ける**。印がある間、`cloudfront add` は delete を先に終えるよう求めるエラーにする。実機では、削除中の VPC origin は消えるまで `Deploying` を返し、状態の値では見分けられなかった (Consequences)。disable された distribution (nakama は削除のときだけ disable する) も、同じく delete を案内するエラーにする。
6. **片付けは `nakama aws delete` と共通にする** (`deleteCloudFrontResources`)。delete の順序と `--force` の動作は変えない。distribution の削除は、すでに disable されていても Deployed を待ってから行う (Consequences の DistributionNotDisabled)。この修正は `nakama aws delete` にも効く。
7. **restore の作り直し** (ADR 0023) も既定で CloudFront を作らず、`--cloudfront` で作る。新しく取るバックアップは、インスタンスに distribution があれば `nakama:cloudfront=true` のタグを持つ。作り直しの計画は、このタグがあって `--cloudfront` が無いときに警告する (IAM ロールのタグと同じ扱い)。インプレース復元は変えない (`--cloudfront` はエラー)。
8. **共有の WAF は init が作るまま**。`acl` と doctor が IP set の存在を前提にしているため。
9. **doctor**: `waf-allowlist` は、CloudFront を持つインスタンスが無ければ skip にし、あればその名前を示して従来どおり判定する。IAM 権限は増えない (`ec2:DescribeInstances` と `ssm:GetParameter` はすでに doctor の行にある)。
10. **案内**: `open --internet`・`kirocrew --internet`・`kirocrew enable` は、CloudFront が無ければ `cloudfront add` を案内するエラーにする。`info` は CloudFront が無ければ `cloudfront : none` と開き方を出し、JSON に `cloudfront` を足す。`list` は変えない。
11. **既存のインスタンスは移行しない**。この版に上げても CloudFront は残る。外すかどうかは利用者が `cloudfront delete` で決める。
12. **権限**: 権限表の `create` と `restore` の行から CloudFront の操作を外し、`cloudfront` の行 (add と delete、`--cloudfront`) を足す。`ec2:RevokeSecurityGroupIngress` が新しく要る。`--with-admin` の警告文は、SSM の経路でも code-server にサインインできる者が管理者権限を持つ、という書き方に直した。

## Consequences

- 新しいインスタンスは、インターネットからの入口も SG の受信の規則も持たない。ブラウザ・端末とも、IAM で認証された SSM の経路だけで届く。
- create は CloudFront を待たなくなった。実機では、EC2 が `running` になって約 20 秒で返った (code-server の導入は cloud-init が続ける)。
- 破壊的な変更: create の出力の `url` は空になる。URL を使うスクリプトは `--cloudfront` を付けるか、作った後に `cloudfront add` を実行する。
- `cloudfront add` は実機で約 5 分かかった。`cloudfront delete` は、Kiro Crew ありで約 14 分かかった (distribution 1 つの disable の反映が約 3 分、VPC origin の削除が約 7 分)。足し直すと URL は変わる。
- 実機で `cloudfront delete` を始めて 15 秒後に CloudFront の URL の `/healthz` を取ると 504 だった (始める前は 200)。その時点で distribution の disable はまだ反映の途中だった。
- 実機の 1 回の観察では、`DeleteVpcOrigin` の後、VPC origin は消えるまで `Deploying` を返した。状態の値は API リファレンスに列挙されていないので、途中で止まった delete は印で見分ける (Decision 5)。
- 実機で delete を disable の直後に止めて再実行すると、`DeleteDistribution` が 409 `DistributionNotDisabled` を返した。そのときの distribution は `Enabled=false`、`Status=InProgress` だった。それまでの `deleteDistribution` は Enabled=true のときだけ Deployed を待っていたので、Decision 6 で、disable 済みでも待つようにした。
- 既存のインスタンスは、利用者が `cloudfront delete` を実行するまで CloudFront を持ち続ける。この ADR より前の bare のインスタンスも、`cloudfront delete` を実行するまで SG の規則を持ち続ける。
- CloudFront を持つインスタンスが 0 台でも、共有の WAF web ACL とルールの料金は時間に応じてかかり続ける。
- code-server は今も `0.0.0.0:8080` で待ち受ける。SG に受信の規則が無ければ、VPC 内のほかのホストからも届かない。待ち受けを 127.0.0.1 に絞ることは扱わなかった。

## 確認

- `cloudfront_test.go`: add の順序と作る部品の形 (instance ARN、private DNS、WAF)、2 回目の add、各段階での失敗と Ctrl-C からの再開 (VPC origin と distribution は 1 つずつ)、消えた部品の作り直し、ドメインの記録し直し、前提の拒否 (bare、停止中、記録なし、未初期化、SG の記録なし、印、disable された distribution、VPC origin の無い distribution、Kiro Crew だけの distribution)。delete の順序 (規則が最初、VPC origin が最後)、Kiro Crew が無いとき Function とインスタンスに触れないこと、停止中、各段階からの再開と再開中の add の拒否、disable の反映待ち、CloudFront の無いインスタンスの規則、add → delete の往復、`nakama aws delete` との共通化 (`--force` で続けること)、`revokeSGIngress` の応答の扱い。順序のテストは、delete の手順を入れ替えたコードで失敗することを確かめた。
- `instance_create_record_test.go`・`restore_test.go`・`backup_test.go`・`doctor_test.go`・`aws_cloudfront_test.go`・`aws_open_test.go`・`aws_restore_test.go`。
- 実機 (一時インスタンス。確認の後に削除した): CloudFront なしの create で SG の受信の規則が 0 本、`open --local-port 18080` で SSM 経由の `/healthz` が 200。`open --internet` と `kirocrew enable` が `cloudfront add` を案内すること。`cloudfront add` (約 5 分) の後、SG の規則が 1 本 (tcp 8080、prefix list)、CloudFront 経由の `/healthz` が 200。2 回目の add は何も作らない。`kirocrew enable` の後の `cloudfront delete`: 確認を断ると何も変わらない。`--yes` で両方の distribution、Function、VPC origin、SG の規則、記録が消え、インスタンス上の nginx のサイトと proxy-domain も消えた。2 回目の delete は何もしない。中断した delete の後の add は拒否され、再開した delete は (修正の後) 完了した。`doctor` の waf-allowlist は、CloudFront のあるインスタンスの名前を示した。

## Alternatives considered

1. **CloudFront を既定のまま、`--no-cloudfront` を足す**: 入口を減らすという目的と逆の既定になる。不採用。
2. **`cloudfront enable` / `disable`** (`kirocrew` と同じ動詞): 利用者の言う「追加・削除」と、`acl add` / `delete` の動詞に合わせた。不採用。
3. **`edit <name> --cloudfront`**: `edit` はインスタンスの停止と起動を伴う変更の置き場で、待ち時間の長さも性質も違う。不採用。
4. **Kiro Crew の CloudFront があれば delete をエラーにして `kirocrew disable` を先に求める**: 一度で入口をなくせない。不採用。
5. **WAF を最初の `cloudfront add` で作り、最後の delete で消す**: 料金は減るが、init・acl・doctor の前提が変わる。不採用。
6. **SG の規則を `CloudFront-VPCOrigins-Service-SG` からの許可に絞る** (AWS の Option 2): より狭いが、VPC origin を作った後でないと足せず、サービスの SG を名前で探すことになる。別に扱う。
7. **途中で止まった delete を VPC origin の状態で見分ける**: 状態の値はドキュメントに無く、実機では削除中も `Deploying` だった。不採用。
8. **既存のインスタンスを自動で移行する**: 利用者の URL と Kiro Crew の画面が予告なく消える。不採用。
9. **code-server の待ち受けを 127.0.0.1 にする**: `cloudfront add` のたびに再起動が要る。SG に規則が無ければ届かないので、別に扱う。

## References

- Issue #120
- Amazon CloudFront Developer Guide: Restrict access with VPC origins (Prerequisites、Security Group Configuration)
- Amazon EC2 API Reference: RevokeSecurityGroupIngress (`UnknownIpPermissions`)
- AWS WAF pricing (FAQ: web ACL とルールの課金)
