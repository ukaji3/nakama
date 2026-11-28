# ADR 0029: ブラウザ用の転送は、ホスト名で振り分けるローカルのルーターを通す

- Status: Accepted (ADR 0031 で、`control.nakama.localhost` を管理画面へ中継することと、管理画面が動いているあいだはルーターが終わらないことを足した)
- Date: 2026-11-22
- Issue: [#124](https://github.com/ukaji3/nakama/issues/124)
- 関連: ADR 0027 (ブラウザは既定で SSM。決定 2 の固定のローカルポートと、不採用案 7 の判断を、この ADR が改める)、ADR 0017 (tunnel のデーモン)、ADR 0020 (Kiro Crew の Web 画面と、その Host の検査)、ADR 0012 (open の自動ログイン)

## Context

ADR 0027 で、`nakama aws open` は `http://localhost:8080` を、`nakama aws kirocrew` は `http://localhost:5476` を開くようになった。ローカルのポートはインスタンスの数によらず固定である。複数のインスタンスを同時に開くには、`--local-port` で別のポートを選び、どのポートがどのインスタンスかを覚えておく必要がある。手元で Kiro Crew を動かしていると、5476 は使えない。どちらも、利用者にとって意味の無い情報である。

ポートを変えても、Cookie はホストでしか分かれない (RFC 6265)。そのため ADR 0027 は、code-server の `cookie-suffix` にインスタンス名を入れて衝突を避けていた。Kiro Crew は画面の設定をオリジンごとの localStorage に持つので、ポートが変わると設定が消える。ADR 0027 の不採用案 7 (`<名前>.localhost`) は、Safari が `*.localhost` を引けないという情報と、Kiro Crew の Host の検査を理由に退けていた。

利用者から、ブラウザとポート転送の間にローカルのプロキシを挟み、`http://<用途>.<インスタンス名>.nakama.localhost:<ポート>/` の Host で振り分ける案が出された。次のことを調べた (2026-11)。

- **名前の解決**: Chrome・Edge・Firefox (84 以降) は、`*.localhost` をブラウザ自身がループバックのアドレスに解決する。macOS 26.0 の mDNSResponder (2881.0.25。apple-oss-distributions/distribution-macOS の `macos-260`) は、`localhost` の下の名前の問い合わせを `localhost` に書き換え (`mDNS.c` の `mDNS_StartQuery_internal`)、`/etc/hosts` の記録で答え、DNS には送らない (RFC 6761 §6.3)。macOS 15.6 (2600.140.3) にはこの処理が無い。macOS 26 の Safari で `http://aaa.localhost:8000` が Chrome と同じように開けることを、利用者が実機で確かめた。この点で、不採用案 7 の Safari の理由は macOS 26 では成り立たない。
- **secure context**: WebKit は `localhost` と `*.localhost` を信頼できるオリジンとして扱う (`SecurityOrigin.cpp` の `isLocalhostAddress`)。W3C の Secure Contexts も、名前をループバックに固定して解決するブラウザでは同じ扱いにしている。code-server の webview と Clipboard は secure context を前提にする。
- **プロキシ**: Chrome は `*.localhost` を常にプロキシの対象から外し、PAC でも変えられない (Chromium の `net/docs/proxy.md`)。Firefox は確かめていない。
- **HTTPS への自動の切り替え**: Chrome 154 で「常に安全な接続を使用する」(公開サイト向けの版) が既定になった。Chromium 158 では、localhost、プライベート IP と既定以外のポートは切り替えられないことが、第三者に観察されている (Playwright の Issue #43871)。
- **Kiro Crew** (0.13.0): Host の許可は `localhost`・`127.0.0.1`・`::1`・`kirocrew.localhost` と、許可されたオリジンのホスト名だけで、ポートは見ない (`build_allowed_hosts`)。`kirocrew.<名前>.nakama.localhost` は `403 Host header not allowed.` になる。CSRF と WebSocket の Origin は `http://localhost:5476` などに限られる (`check_origin`)。`Forwarded`・`X-Forwarded-*`・`X-Real-IP` のどれかがあると、この機械からの要求とみなさず、設定の変更と秘密情報の表示を拒む (`is_direct_local_request`)。Cookie は Domain 属性なしで、名前は Host のポートで決まる (`mc_token_<ポート>`)。これらは、手元の Kiro Crew の関数を直接呼んで確かめた。インスタンスも同じ版だった。
- **code-server** (main の `src/node/http.ts`): `*.localhost` では Cookie に Domain 属性を付けない (`getCookieDomain`)。WebSocket を受けるとき、Origin のホストと Host (または `X-Forwarded-Host`) の一致を求める (`authenticateOrigin`)。
- **試作**: 一時的な Go のルーターを、すでにあった転送の前に置いて確かめた。
  - code-server: ログインで Cookie (`code-server-session-<名前>`、Domain なし、Path `/`、HttpOnly、SameSite=Lax) が付く。ルーターのオリジンからの WebSocket は 101 で、無関係なオリジンからは 403 になる。Host を `localhost` に書き換えて直接送ると 403 になるので、Host はそのまま渡す必要がある。
  - Kiro Crew: Host と Origin を書き換えると、画面は 200 になる。トークン無しの WebSocket は、直接送ったときと同じ `Token required` で、Host と Origin の検査は通っていた。クライアントが付けた `X-Forwarded-For` などをルーターが捨てると、`/api/health` は (この機械からの要求にだけ見せる) 版を返した。
  - Chrome と Firefox で、code-server の画面・ターミナル・Markdown のプレビュー、Kiro Crew の画面と設定を確かめた。

## Decision

1. **ルーター**: 利用者ごとに 1 つのプロセス (隠しコマンド `nakama aws router-daemon`) が、127.0.0.1 と ::1 の固定のポートで待ち受ける。既定のポートは 18888 で、`NAKAMA_ROUTER_PORT` で変えられる。`http://<code-server|kirocrew>.<名前>.nakama.localhost:<ポート>/` への要求を、そのインスタンスの tunnel デーモンが持つブラウザ用の転送へ中継する。
   - 8888 は Jupyter の既定のポートなので避けた。
   - 80 番は使わない。特権が要るうえ、既定のポートでは HTTPS に切り替えられうる。
2. **振り分け**: tunnel の状態ファイルとデーモンの `/status` から、インスタンスの 8080 (code-server) か 5476 (Kiro Crew) への、dead でない転送 (ホスト指定なし) を選ぶ (`findBrowserForward`)。open が使い回すのと同じ転送で、`tunnel -L 28080:8080` のような転送も含む。答えは 2 秒覚え、転送が応答しなければ忘れる。ルーターは AWS の資格情報を持たず、状態ファイルを書き換えない。
3. **中継の規則**:
   - code-server には、Host をそのまま渡す。WebSocket の Origin の検査が Host と照合するため。
   - Kiro Crew には、Host を `localhost:5476` に置き換える。ルーター自身のオリジンを指す Origin と Referer は `http://localhost:5476` に置き換え、それ以外の値は変えない (Kiro Crew がそのまま拒む)。応答の `Location` が `http://localhost:5476` を指していれば、ルーターのオリジンに戻す。`127.0.0.1` には置き換えない。Kiro Crew が `localhost` へ 302 で移すため。
   - `Forwarded`・`X-Forwarded-*`・`X-Real-IP` は、転送先に渡さない。Kiro Crew が、この機械からの要求として扱い続けるようにするため。Go の `httputil.ReverseProxy` は、`Director` ではなく `Rewrite` を使う (`Director` は `X-Forwarded-For` を足す)。
   - これら以外の Host には 421 を返す (DNS rebinding の防御)。nakama の名前でも、ポートが違えば 421 にする。
4. **ブラウザ用の転送のポート**: `--local-port` が無ければ、システムが選んだ空きポートを使う (127.0.0.1 と ::1 の両方で空いているもの)。デーモンが bind する前に別のプログラムに取られたら、別のポートで足し直す。`--local-port` があれば、今までどおりそのポートで待ち受けて `http://localhost:<ポート>` を開き、ルーターは起動しない。ルーターが動いていれば、その転送にもルーターから届く。
5. **開けるかの確認**: open と kirocrew は、ルーターを通して `/healthz` と `/api/health` が応答するまで待つ。ルーター自身のエラーのページ (`X-Nakama-Router-Error` の付いた応答) は、応答とみなさない。
6. **起動と終了**:
   - open と kirocrew が、必要なときにルーターを起動する (`EnsureRouter`)。tunnel のデーモンと同じく、端末から切り離して起動し、OS のログに書く (syslog のタグ `nakama-router`。Windows ではイベントログのソース `nakama`)。
   - 状態は `~/.config/nakama/router.json` (0600。制御のトークンを含む) に置く。tunnel の状態のディレクトリの中には置かない。`router` は有効なインスタンス名なので。
   - 別の版の nakama が起動したルーター、または別のポートのルーターが動いていれば、止めて今の版で起動し直す。開いている WebSocket は切れ、VS Code と Kiro Crew がつなぎ直す。新しいポートが使用中なら、動いているルーターは止めずにエラーにする。
   - tunnel のデーモンが 1 つも応答しない状態が 1 分続いたら、ルーターは自分で終わる。
   - 専用のコマンドは作らない。
7. **ポートが使用中**: open と kirocrew をエラーで止め、`NAKAMA_ROUTER_PORT` で別のポートを指定するよう案内する。nakama の転送がそのポートを使っていれば、そのインスタンス名も示す。別のポートへ自動では移らない (ADR 0027 の決定 2 と同じ考え方)。
8. **転送の無いインスタンス**: 502 のページで、次にすることを示す。
   - 転送が無い: `nakama aws open <名前>` (Kiro Crew は `nakama aws kirocrew <名前>`)。
   - 転送が dead: その理由と `nakama aws tunnel status <名前>`。
   - 転送が応答しない: デーモンの最後の理由。TargetNotConnected なら `nakama aws start <名前>` も示す。

   ルーターから tunnel のデーモンは起動しない。
9. **遊休の接続**: ルーターから転送への keep-alive の接続は、10 秒使われなければ閉じる。デーモンはローカルの接続が開いている間を「使われている」とみなす (ADR 0027 の決定 6)。ルーターが遊休の接続を持ち続けると、SSM のセッションと HTTP の確認が無駄に続くため。
10. **表示と出力**: `tunnel status` に、ルーターの状態と、ルーターが振り分ける転送の URL を出す。JSON では転送の `url` に入れる。open と kirocrew の JSON の `url` はルーターの URL になり、`local_port` は実際の転送のポートになる。破壊的変更としてリリースノートに書く。
11. **前提**: インスタンス名はこの機械の中で一意とする (tunnel の状態ファイルと同じ前提)。AWS のプロファイルはホスト名に入れない。Safari は macOS 26 以降を対象にする。
12. **範囲外**: インスタンスの一覧のページ (`http://nakama.localhost:<ポート>/`) と、インスタンスの任意のポートへの振り分け (`<ポート>.<名前>.nakama.localhost`)。

## Consequences

- ポートを選ばずに、複数のインスタンスを同時に開ける。インスタンスごとにオリジンが分かれるので、code-server のサインインと Kiro Crew の画面の設定は、インスタンスごとに残る。手元の Kiro Crew の 5476 とも重ならない。
- Public Suffix List に `localhost` は無いので、`*.nakama.localhost` はすべて同じサイト (`nakama.localhost`) になる見込みである (ブラウザでは確かめていない)。SameSite の Cookie では、インスタンスどうしを分けられない。今の `localhost:<ポート>` (すべて同じホスト) よりは分かれる。
- ルーターが止まっているか、ポートを取られていると、`--local-port` を使わない限りブラウザから入れない。open か kirocrew をもう一度実行すれば、ルーターは起動し直す。
- インスタンスで `kirocrew token` が出す `http://localhost:5476?token=…` と、Slack に届くダッシュボードのリンクは、ルーターの URL ではない。使うには `--local-port 5476` で開くか、リンクのオリジンを書き換える。
- Kiro Crew の Host と Origin の検査に、ルーターの書き換えを合わせている。Kiro Crew の更新でこの検査が変われば、合わせ直す必要がある (ADR 0020 の `X-Forwarded-Host` と同じ種類の依存)。
- Origin を書き換えるので、Kiro Crew の CSRF の検査の一部を、ルーターが担う。書き換えるのは、Origin がルーター自身のオリジンを指すときだけである。
- macOS 15 以前の Safari と、`*.localhost` を DNS に問い合わせるツールは、ルーターの URL を開けない。そのときは `--local-port` を使う。
- 同じ機械のほかのプロセスからルーターに届くのは、今のローカルのポートと同じである。code-server はパスワードを、Kiro Crew はトークンを求める。
- 開いた転送の状態ファイルには、毎回ちがうローカルのポートが記録される (`code-server=<ポート>:8080`)。`tunnel restart` は同じポートで張り直す。

## 確認

- `router_test.go`: Host の名前の判定 (大文字、末尾のドット、ポートの違い、知らない用途、インスタンス名の規則)。本物の reverse proxy と、要求を記録する転送先を使って、次を確かめた。
  - code-server には Host と Origin がそのまま届く。
  - Kiro Crew には Host `localhost:5476` が届き、ルーター自身の Origin と Referer だけが書き換わる。`Location` はルーターのオリジンに戻る (`http://localhost:54760` のような別のポートは戻さない)。
  - どちらにも転送用のヘッダーが届かない。
  - 知らない Host には 421、転送が無い・dead・応答しないときは、次にすることを示すページを返す (HTML はエスケープする)。
  - WebSocket の upgrade と event stream が通る。遊休の接続が閉じる。
  - ルーターのエラーのページを、開けるかの確認で応答とみなさない。
  - 状態ファイルと `/status` から転送を選び、答えを覚え、忘れる。
- `router_daemon_test.go`: 本物のループバックのソケットで、次を確かめた。状態ファイルの場所と 0600、制御のチャネルとトークン、127.0.0.1 と ::1 での待ち受け、停止とポートの解放、tunnel が無いときの終了 (tunnel がある間は終わらない)、ポートの使用中、残った状態ファイルの置き換え、動いている別のルーターに触れないこと。tunnel のデーモンの `/status` を通して転送まで届くことも、通しで確かめた。
- `router_start_test.go`: 起動、使い回し、別の版や別のポートのルーターの置き換え、新しいポートが使用中なら動いているルーターを止めないこと、残った状態ファイル、使用中のポートの案内 (nakama の転送ならそのインスタンス名)、起動の時間切れ、不正な `NAKAMA_ROUTER_PORT`、`tunnel status` の URL。
- `tunnel_browser_test.go` / `tunnel_browser_router_test.go`: 空きポートの選び直し (::1 で使用中、デーモンでの競合、選び直しの上限)、ルーターを起動できないときに何も始めないこと、`--local-port` ではルーターを使わないこと。
- `aws_open_ssm_test.go` / `aws_tunnel_test.go`: 1 回限りのページの送信先がルーターのオリジンであること、ルーターのポートが使用中のときの案内、JSON の `url` と `local_port`、`tunnel status` のルーターの表示。
- 次の変更を入れたコードで、上のテストが失敗することを確かめた: code-server の Host を置き換える、Kiro Crew の Origin を置き換えない、`X-Real-IP` を消さない、遊休の時間を長くする、ルーターのページを応答とみなす、open がルーターの URL を使わない、tunnel が無くても終わらない、版の違いを無視する。
- 実機 (一時インスタンス、Kiro Crew 0.13.1。確認の後に削除した)。ブラウザの代わりに、1 回限りのページを読んでフォームを送るスクリプトを `--browser` に渡した。
  - `open`: ルーターが起動し、転送は空きポートで張られ、約 2 秒で開いた。1 回限りのページの送信先はルーターの URL で、ログインの Cookie は `code-server-session-<名前>` (Domain なし、Path `/`、HttpOnly、SameSite=Lax)。画面は 200、ルーターのオリジンからの WebSocket は 101。
  - `kirocrew`: トークンで `mc_token_5476` と `mc_refresh_5476` (どちらも Domain なし) が付き、画面は 200、`/api/ws` は 101。`/api/health` は版を返した (この機械からの要求として扱われた)。`/api/kiro-prerequisite` は、ルーター経由でも直接 (Host `localhost:5476`) でも、Cookie ありで 200、無しで 403 `Token required` と同じ答えだった。
  - 2 回目の open は転送を使い回した。転送がある状態で `--local-port` に別のポートを指定するとエラーになった。nakama の別の転送が使っているポートを `NAKAMA_ROUTER_PORT` に指定すると、そのインスタンス名と案内を出してエラーになり、動いているルーターはそのまま残った。別の版の nakama で open すると、ルーターが入れ替わった。
  - 以前の版のデーモンが持つ固定ポートの転送にも、ルーターから届いた。`tunnel status` にルーターと URL が出た。インスタンスを消した後は、転送が無いことを示すページになった。
  - このとき、tunnel が無いインスタンスのエラーのページで、題名から用途が抜けていた (「No  forward for …」)。直して、テストを足した。
- Safari (macOS 26) と Linux での動作は、このブランチのビルドで利用者が確かめた。

## Alternatives considered

1. **使用中なら空きポートへ自動で移る** (ルーターなし): 実装は小さく、ブラウザの名前の解決に依らない。ただし、ポートが変わるとブックマークが切れ、Kiro Crew の画面の設定 (オリジンごとの localStorage) も消える。どのポートがどのインスタンスかも分からない。不採用。
2. **すべてのインスタンスの転送を 1 つのデーモンに集める**: ADR 0017 の、インスタンスごとの停止・状態・資格情報の扱いを作り直すことになる。不採用。
3. **TCP の最初の要求の Host だけを読んでつなぐ**: 実装は小さいが、Kiro Crew の Host と Origin を要求ごとに書き換えられない。不採用。
4. **インスタンスごとに 127.0.0.x を割り当てる**: macOS では、127.0.0.1 以外のアドレスを使うのに root での設定が要る。番号も覚えにくい。不採用。
5. **ブラウザに PAC や転送用のプロキシを設定させる**: 利用者のブラウザの設定を変える必要がある。不採用。
6. **パスで分ける** (`localhost:<ポート>/<名前>/…`): Kiro Crew の画面はルートからの絶対パスを前提にしている (ADR 0020)。Cookie と localStorage も、インスタンスの間で混ざる。不採用。
7. **インスタンスの Kiro Crew に、`KIROCREW_CORS_ORIGINS` でルーターのオリジンを許可させる**: ルーターでの書き換えは要らなくなる。ただし、インスタンスごとの設定と Kiro Crew の再起動が要り、ルーターのポートを変えるたびに直す必要がある。governance のポリシーがある版では拒まれうる。不採用。
8. **Kiro Crew の `dashboard.url` をルーターの URL にする**: `dashboard.url` のポートは、gateway が待ち受けるポートになる。不採用。
9. **ルーターから tunnel のデーモンを起動する** (ブックマークだけで開ける): ルーターを起動したときの資格情報とプロファイルを、使い続けることになる。不採用。

## References

- Issue #124
- RFC 6761 §6.3 (localhost の名前)、RFC 6265 (Cookie はポートで分かれない)
- W3C Secure Contexts §3.1 (Is origin potentially trustworthy?)
- apple-oss-distributions/mDNSResponder (`mDNSCore/mDNS.c` の `mDNS_StartQuery_internal`、`AnswerNewQuestion`)、apple-oss-distributions/distribution-macOS (`macos-260`、`macos-265`、`macos-156` の mDNSResponder)
- WebKit `Source/WebCore/page/SecurityOrigin.cpp` (`isLocalhostAddress`)、コミット 9ba559bfd313
- Chromium `net/docs/proxy.md` (implicit bypass rules)
- Google Security Blog "HTTPS by default" (2025-10-28)、microsoft/playwright Issue #43871
- Kiro Crew 0.13.0: `kiro_crew/dashboard/urls.py` (`build_allowed_hosts`、`build_allowed_origins`、`should_canonicalize_host`)、`origin.py` (`check_host`、`check_origin`、`is_direct_local_request`)、`token_auth_tokens.py` (`_cookie_port_from_host`)
- code-server `src/node/http.ts` (`getCookieDomain`、`authenticateOrigin`)
- Go `net/http/httputil` (`ReverseProxy.Rewrite`、`ProxyRequest.SetURL`)
