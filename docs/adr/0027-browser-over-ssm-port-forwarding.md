# ADR 0027: ブラウザからの接続は既定で SSM のポート転送にし、転送の再接続を作り直す

- Status: Accepted
- Date: 2026-11-20
- Issue: [#118](https://github.com/ukaji3/nakama/issues/118)
- 関連: ADR 0012 (`open` の自動ログイン。送信先が CloudFront に限られなくなる)、ADR 0014 (`connect` の再接続。tunnel の転送には使わなくなる)、ADR 0017 (`tunnel`。中継プロキシを採らなかった判断と再接続の方針を、この ADR が置き換える)、ADR 0020 (Kiro Crew の Web 画面。`--internet` の経路として残る)

## Context

インターネットからの攻撃が増えていることを受けて、利用者から攻撃面を減らしたいという要望があった。`nakama aws open` と `nakama aws kirocrew` は、CloudFront の distribution と WAF の IP 許可リストを通ってインスタンスに届く。Session Manager (SSM) のポート転送なら、インスタンスへの受信は要らず、IAM で認証される。そこで、ブラウザからの接続は既定で SSM を通し、CloudFront は `--internet` を付けたときだけ使う。

SSM のポート転送は `nakama aws tunnel` (ADR 0017) にすでにある。ただし、コードを読んだところ、SSM でよく起きる切断のあと、転送が dead のまま戻らない経路があった。

- サーバー側がセッションを閉じると (Session Manager のアイドルタイムアウト、最大セッション時間、TerminateSession)、plugin は ChannelClosed を受けて `os.Exit(0)` で終わる (`portsession.go` の `s.Stop`)。監督ループは exit 0 を正常終了とみなし、転送は dead ("session ended") になっていた。アイドルタイムアウトは既定 20 分で、ポート転送にもかかる (plugin 1.2.323.0 / 1.2.339.0 のリリースノート)。
- 再接続のときの StartSession の通信エラー (スリープから戻った直後でネットワークがまだ無いなど)、5xx、スロットリングは、致命的なエラーとして扱われていた (`classifyStartSessionError` は APIError 以外を Fatal にし、テストもそれを固定していた)。
- 再試行は 5 回 (待ち時間の合計は約 31 秒) で尽きていた。
- 応答が返らなくなった切断 (Wi-Fi の切り替え、スリープ) に、plugin はなかなか気づかない。websocket の読み取りには期限が無く、ping は 5 分ごとに送るだけで応答を確かめない (`websocketchannel.go`)。送信データの再送の打ち切り (`ResendMaxAttempt = 3000`) は 5 分以上かかり、送るデータが無ければ起きない。
- ローカルのポートは plugin が持っていたので、再接続の間は閉じていた。その間のブラウザの再読み込みは接続を拒否され、別のプロセスにポートを取られることもありえた。plugin は smux の `OpenStream` に失敗すると、受け付けた接続を閉じずに放置する (`muxportforwarding.go` の `handleClientConnections`)。
- デーモンは instance ID を起動時に 1 回しか読まず、Windows の OS のプロキシ設定もプロセスごとに 1 回しか読まなかった (go-ieproxy の `sync.Once`)。

ブラウザの経路として使うために、実機のインスタンスで次のことを確かめた (読み取りだけ)。

- **Kiro Crew** (0.4.1rc1): Host の検査はポートを見ない (`build_allowed_hosts`: 「`localhost:8777` のような SSH トンネルのローカルポートでも `localhost` に一致する」)。CSRF と WebSocket の Origin 検査は、許可リストに無くても、ループバックの Origin が Host と一致すれば通す (`check_origin` の same-origin loopback fallback)。Cookie は Host のポートで名前が変わる (`mc_token_<port>`)。`/api/health` / `/api/live` / `/api/ready` は Host とトークンの検査から外れている (`PROBE_PATHS`)。ループバックからの、転送ヘッダーの無い要求は「この機械からの要求」として扱われ、設定の変更と秘密情報の表示が許される (`is_direct_local_request`。SSH の `-L` もこれに当たると明記されている)。
- **code-server** (4.141.0): `/healthz` は認証なしで応答する。`cookie-suffix` (4.107.0 で追加) を設定すると、Cookie の名前が `code-server-session-<suffix>` になる。知らない設定項目があると起動しない (`Unknown option`)。VS Code の再接続の猶予は既定で 10800000 ms (3 時間)。
- **速度**: SSM の転送 1 本で、受信は約 750 KB/s だった。並行して 6 本の接続で取っても合計は同じで、上限はセッション単位である。gzip で取ると、3.2 MB の JS が 0.8 MB になり約 1 秒で届いた。

## Decision

1. **既定は SSM**。`nakama aws open <名前>` は code-server (インスタンスの 8080) へ、`nakama aws kirocrew <名前>` は Kiro Crew の gateway (インスタンスのループバックの 5476) へ、SSM のポート転送を張ってからブラウザを開く。`--internet` を付けると、今までの CloudFront の経路 (ADR 0012 / ADR 0020) になる。
2. **ローカルポート**: 既定はインスタンス側と同じ番号 (8080 / 5476)。インスタンスで `kirocrew token` が出す `http://localhost:5476?token=…` も、そのまま手元のブラウザで開ける。`--local-port` で変えられる (`--internet` とは併用できない)。使用中ならエラーにし、nakama の別の転送が使っていればそのインスタンス名を示す。空きポートへ勝手に逃げない。ブラウザに開く URL は `http://localhost:<ポート>` にする。
3. **転送は tunnel のデーモンに足す**。インスタンスごとに 1 つのデーモン (ADR 0017) の制御チャネルに `POST /forwards` を足し、open / kirocrew はそこに用途 (purpose) 付きの転送を追加する。同じリモートポートへの転送 (`tunnel -L` で張ったものを含む) があれば再利用する。`--local-port` を明示して別のポートを求めたときはエラーにする。dead になった転送と同じローカルポートへの追加は、その転送を置き換える。状態ファイルには用途付きの形 (`code-server=8080:8080`) で記録し、`tunnel restart` も用途ごと戻す。用途の無い転送は今までどおりの形なので、古い状態ファイルはそのまま読める。`POST /forwards` を知らない古いデーモンには、`tunnel restart` を案内する。open は転送を足した後、HTTP の要求が転送を通って応答するまで最大 60 秒待つ。待っても使えなければ、デーモンが見た最後の理由を示し、TargetNotConnected なら NAT の確認方法を案内する。停止は `nakama aws tunnel stop <名前>`、状態は `tunnel status` で見る。
4. **ローカルポートはデーモンが持つ**。デーモンが 127.0.0.1 と ::1 の利用者のポートを持ち続け、plugin にはデーモンが選んだ別の空きポートで待ち受けさせ、その間を中継する (ADR 0017 の不採用案 6 の判断を改める)。再接続の間もポートは閉じず、届いた接続は新しいセッションができるまで最大 30 秒待つ。セッションが終わると、そのセッションを通していた接続は閉じる (ブラウザ側が張り直す)。::1 を使える機械で ::1 のポートが取られていれば、使用中として扱う (ブラウザは `localhost` を ::1 で引くことがある)。
5. **再接続の条件** (`forwardRunner`): nakama 自身が止めたとき以外は、子プロセスがどう終わっても張り直す (exit 0、exit 74、それ以外)。StartSession と instance ID の読み取りの失敗は、次のように扱う。
   - 再試行する: AWS からの応答が無いもの (DNS、接続、TLS、タイムアウト)、TargetNotConnected、スロットリング、5xx、429
   - 60 秒ごとに再試行する: 資格情報のエラー (ExpiredToken など)。nakama の login プロバイダはトークンキャッシュを取得のたびに読み直すので、`nakama aws login` をやり直せば戻る
   - 転送を終える: インスタンスの記録が無い (削除された)、それ以外の 4xx (InvalidDocument など、待っても直らないもの)

   回数の上限は設けない。待ち時間は 1 秒から倍々に 30 秒まで、60 秒以上使えたセッションのあとは 1 秒に戻す。新しいセッションの前には instance ID を読み直し、Windows では OS のプロキシ設定を読み直す (`proxy.Reload`)。
6. **セッションは必要なときだけ張る**。直前 5 分以内にローカルの接続があれば、切れたらすぐ張り直す。無ければ張り直さず、次の接続が来たときに張る (状態 `idle`)。ブラウザを閉じて放っておけば、アイドルタイムアウトで閉じたセッションは閉じたままになり、Session Manager の設定の意図を損なわない。
7. **切断の検知**: open / kirocrew の転送 (用途の分かるもの) では、ローカルの接続があるあいだ、デーモンが転送の経路を通して HTTP の要求を送る (code-server は `/healthz`、Kiro Crew は `/api/health`。Host はブラウザと同じ `localhost:<ポート>`。どの HTTP 応答でも健全とみなす)。15 秒ごとに送り、10 秒で応答が無ければ失敗、3 回続いたらそのセッションを捨てて張り直す (最長でおよそ 1 分で気づく)。セッションの確立 (plugin が待ち受けを始めるまで) にも 30 秒の期限を設ける。捨てたセッションは TerminateSession で閉じる (失敗しても続ける)。`tunnel -L` の転送は中身の形式が分からないので、HTTP の検知はしない。
8. **Kiro Crew は直接**: SSM の経路は Kiro Crew の gateway へ直接転送する。`kirocrew enable` は要らず、bare のインスタンスでも使える。code-server のパスワードは使わず、Kiro Crew のトークンだけで入る。トークンは 127.0.0.1 の 1 回限りのページから GET のフォームで渡し、ブラウザのコマンドラインに載せない。この経路では Kiro Crew は要求を「この機械から」とみなし、ブラウザから設定の変更と秘密情報の表示ができる (ADR 0020 が `tunnel -L 5476:5476` として案内していた経路と同じ)。ADR 0020 の「必ず code-server を経由させる」は、`--internet` の経路の要件として残す。
9. **Cookie の衝突を避ける**: Cookie はポートで分かれないので、複数のインスタンスを別のローカルポートで同時に開くと、code-server の Cookie が上書きし合う。code-server の `cookie-suffix` にインスタンス名を設定する。create の user data は config.yaml を書いた直後、`update` は code-server を更新した直後 (再起動の前) に、`nakama_set_cookie_suffix` が設定する。4.107.0 より古い code-server には書かない (起動しなくなるため)。利用者が自分で設定した値は変えない。Kiro Crew の Cookie はもともとポートで分かれる。
10. **ブラウザは nakama が開く**。SSM の経路では、ブラウザを起動できなければエラーにし、URL を表示して利用者に開かせることはしない (`--browser` を案内する)。URL が要るときは `--print-url` を明示する。1 回限りのページが期限内に読まれなかったときは、今までどおり URL を直接 (Kiro Crew ではトークン付きで) ブラウザに開かせる。`--internet` の経路は今までどおり。
11. **`tunnel -L` にも適用する**。4〜6 (デーモンがポートを持つこと、再接続の条件、必要なときだけ張ること) は `tunnel -L` の転送にも効く。監督ループが共通で、同じ理由で落ちていたため。
12. **`nakama aws delete` は、この機械の同名の tunnel デーモンを止める**。消えたインスタンスへの再試行でローカルポートを持ち続けないため。失敗しても delete は成功のまま。
13. **`connect` は変えない**。対話シェルとコマンド実行の再接続は ADR 0014 のまま。tunnel 専用だった `RetryChildFailures` (Issue #45) は使う場所が無くなったので消した。
14. **出力と権限**: JSON / YAML の出力に `via` (`ssm` / `internet`) と `local_port` を足す。`tunnel status` に用途の名前 (`8080:8080 (code-server)`) と状態 `idle` が出る。権限表では open と kirocrew の行に `sessionActions` (`ssm:StartSession` など) と `ec2:DescribeNatGateways` を足す。

## Consequences

- 利用者は自分の IP を WAF の許可リストに入れなくてよくなる。CloudFront の distribution、VPC Origin、SG の CloudFront の許可、code-server の `0.0.0.0:8080` は残るので、インスタンス側の攻撃面はまだ変わらない。CloudFront をインスタンスごとに作るかどうかを選べるようにする件は、別に扱う。それまでは、許可リストを空にすれば (`nakama aws acl delete all`)、Web ACL の既定の動作が Block なので CloudFront の経路はすべて 403 になる。
- 速度は CloudFront より遅い (測ったところ、セッション 1 本で約 750 KB/s)。初回の読み込みは数秒から十数秒かかりうる。Kiro Crew の `/assets/` は immutable で 1 年キャッシュされるので、2 回目からは速い。
- localhost のポートには、同じ機械のほかのプロセスからも届く。code-server はパスワード、Kiro Crew はトークンを求める。Kiro Crew は SSM 経由の要求をこの機械からの要求として扱う (Decision 8)。
- セッションを張り替えると、そのセッションを通していた TCP 接続は切れる。code-server の画面 (VS Code) は自分で接続し直し、サーバー側は既定で 3 時間、作業の状態を保つ。plugin が同じセッションのまま再接続できたとき (短い切断) は、コードを読んだかぎりでは接続も保たれる (実機では未確認)。
- 5 分使われなかった転送は、次の接続でセッションを張るまで数秒待たせる。ブラウザがつながっている間は、HTTP の検知の要求もセッションを生かし続ける。
- plugin の待ち受けポートは、空いているのを確かめてから plugin が使うまで数秒の間がある。その間に別のプロセスが取ると、そのセッションは失敗して次の試行で別のポートを選ぶ。
- 用途付きの形で記録された状態ファイルを、この ADR より前の版の `tunnel restart` は読めない (エラーになるだけで、動いているデーモンは止めない)。この ADR より前の版のデーモンには、open が転送を足せない (`tunnel restart` で新しい版のデーモンに替える)。
- open は、ブラウザを開いた後も転送を残す。止めるまで、または 5 分使われずにセッションを閉じるまで、ローカルポートは開いたまま。

## 確認

- `tunnel_forward_test.go`: 本物のループバックのソケットで plugin の待ち受けを模し、中継、サーバー側の終了 (exit 0) のあとの再接続とポートが閉じないこと、セッションを待つ接続、通信エラー・スロットリング・TargetNotConnected・5xx の再試行、資格情報のエラーからの復帰、インスタンスの削除と致命的なエラーでの終了 (ポートを放すこと)、応答しなくなったセッションの検知と張り替え (その接続が閉じること)、接続が無いときは検知しないこと、`idle` と次の接続での復帰、確立の期限、確立前の子の失敗、60 秒で待ち時間が戻ることを確かめた。
- `tunnel_browser_test.go`: デーモンの起動、転送の再利用、`--local-port` の不一致、稼働中のデーモンへの追加 (dead の置き換え)、古いデーモン、ほかのインスタンスが使うポート、応答を待つこと、TargetNotConnected の案内、dead での中止を確かめた。`tunnel_daemon_test.go` は追加・重複・置き換え・状態ファイルの書き換えを確かめた。
- `aws_open_ssm_test.go`: 1 回限りのページの送信先 (`http://localhost:<ポート>/login`)、`--local-port`、フラグの検査、ブラウザを起動できないときのエラー (URL を出さないこと)、JSON の出力、Kiro Crew のトークンのページ (GET、パスワード無し) を確かめた。
- `instance_update_run_test.go`: 更新のスクリプトを bash で実行し、`cookie-suffix` を書くこと、既にある値と 4.107.0 より古い code-server では書かないことを確かめた。
- 実機での通しの確認 (Wi-Fi の断と切り替え、スリープ、アイドルタイムアウト、インスタンスの停止と起動、資格情報の期限切れ) は、この ADR の時点では行っていない。

## Alternatives considered

1. **open をフォアグラウンドで動かし続ける** (Ctrl-C まで): 寿命は分かりやすいが、端末を閉じると切れ、端末を 1 つ専有する。tunnel のデーモン (状態の表示、停止、再起動、OS のログ) を使い回せない。不採用。
2. **ブラウザ用に別のデーモン**: 同じインスタンスに 2 つのデーモンができ、stop と status の対象が分かれる。不採用。
3. **ローカルポートを plugin に持たせたまま** (今までの tunnel): 再接続の間ポートが閉じ、取られる恐れと plugin の接続の放置が残る。不採用。
4. **plugin を直して websocket に読み取りの期限と pong の確認を入れる**: すべてのセッションに効くが、Session Manager 側が ping に pong を返すかを確かめられていない。返さなければ、通信の無いセッションを誤って切る。HTTP の検知は転送の経路全体 (plugin の放置も含む) を確かめられる。不採用。
5. **常に張り直す** (使われていなくても): 単純だが、アイドルタイムアウトを事実上無効にし、止まったインスタンスへの再試行も続く。不採用。
6. **Kiro Crew を nginx (15476) 経由にして制限を保つ**: `kirocrew enable` が必須になり、nginx の Origin の検査も変える必要がある。SSM の経路は IAM で認証されているので、制限を保つ利点は小さい。不採用。
7. **`<名前>.localhost` で Cookie を分ける**: インスタンスの設定を変えずに済む。ただし Safari は hosts への追記が要るという情報があり、Kiro Crew の Host の検査も `<名前>.localhost` を通さない。不採用。
8. **plugin の待ち受けを unix ソケットにする** (`localConnectionType: unix`): ポートを選ぶ間の競合が無くなる。ただし、`AWS-StartPortForwardingSession` の properties にこの項目が無く、独自の SSM ドキュメントと権限が要る。不採用。
9. **転送 1 本に複数のセッションを張って速くする**: 上限はセッション単位なので速くなる見込みはあるが、セッションの数と管理が増える。必要になったら別に扱う。

## References

- Issue #118
- `third_party/session-manager-plugin/src/sessionmanagerplugin/session/portsession/` (`portsession.go`、`muxportforwarding.go`)、`src/communicator/websocketchannel.go`、`src/datachannel/streaming.go`、`src/config/config.go`
- session-manager-plugin RELEASENOTES (1.2.323.0 "Disable smux keep alive to use idle session timeout feature"、1.2.339.0 "Fix idle session timeout for port sessions")
- AWS Systems Manager User Guide: Specify an idle session timeout value
- code-server CHANGELOG 4.107.0 (`--cookie-suffix`)、`out/common/http.js` (Cookie の名前)、`out/node/routes/index.js` (`/healthz`)
- Kiro Crew: `kiro_crew/dashboard/urls.py` (`build_allowed_hosts`、`build_allowed_origins`、`should_canonicalize_host`)、`origin.py` (`check_origin`、`is_direct_local_request`、`PROBE_PATHS`)、`refresh_tokens.py` (`mc_token_<port>`)
