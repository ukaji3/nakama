# ADR 0031: CLI に慣れていない利用者のために、ブラウザで操作する管理画面を置く

- Status: Accepted
- Date: 2026-11-28
- Issue: [#139](https://github.com/ukaji3/nakama/issues/139)
- 関連: ADR 0029 (ローカルのルーター。この ADR で `control.nakama.localhost` の転送を足す)、ADR 0027 (ブラウザは既定で SSM)、ADR 0022 (Windows のダブルクリック。引数なしの挙動を変えるときに改める)、ADR 0016 (`nakama aws login` の実装)、ADR 0012 (code の自動ログイン)

## Context

nakama の操作は、すべて CLI のコマンドである。CLI に慣れていない利用者から、ブラウザの画面で操作したいという要望があった。利用者が示した形は次のとおり。

- 引数なしで `nakama` を実行すると、ブラウザで `http://control.nakama.localhost:18888/` が開く。
- Login のボタンで `nakama aws login` が実行される。そのあとインスタンスの一覧が出て、code と kirocrew のボタンで CLI と同じ操作ができる。
- 機能は、login、init、create、edit、一覧、code、kirocrew、backup、restore など CLI を網羅する。
- ほかのプログラムから HTTP で操作されないよう、セッション管理を必ず行う。

管理画面は AWS の資格情報で動き、インスタンスの作成や削除もできる。そのため、誰がこの画面に要求を送れるかを先に確かめた。

- ループバックのポートには、同じ機械のどのプロセスも接続できる。ほかの OS のユーザーのプロセスも含む。
- ブラウザの中では、ほかのサイトのページも `http://control.nakama.localhost:18888/` に要求を送れる。Chromium は PSL に無い TLD の最後のラベルを TLD とみなすので (`net/base/registry_controlled_domains` の `GetDomainAndRegistryImpl` が `INCLUDE_UNKNOWN_REGISTRIES` を使う)、`*.nakama.localhost` はすべて同じサイト `nakama.localhost` になる。code-server の中のページ (`/proxy/<port>/` で開く開発中のアプリなど) からの要求には、SameSite=Strict の Cookie も付く。
- 同じ OS のユーザーで動くプログラムは、状態ファイルもブラウザの Cookie も読める。CLI を直接実行することもできる。ここは今の CLI と同じ信頼の境界で、画面の側では防げない。

試作 (`.cache` に置いた使い捨ての Go のサーバーと、Chrome DevTools Protocol と WebDriver で動かした headless の Chrome 158 と Firefox 160) で、次のことを確かめた (2026-11)。

- http の `*.localhost` でも、Secure と `__Host-` の Cookie は保存され、送られる (両方)。
- 兄弟のホストのページは、`Domain=nakama.localhost` の Cookie を植えられ、管理画面はそれを受け取る。`__Host-` の Cookie は植えられない (両方)。
- 兄弟のホストからの要求 (fetch、no-cors の POST、iframe、フォームの POST) には、Strict を含むすべての Cookie が付き、`Sec-Fetch-Site: same-site` になる。別のサイトからの要求には Lax の Cookie も付かない (両方)。カスタムヘッダー付きの要求は、プリフライトで止まって送られない。
- 別のサイトから 127.0.0.1 のコールバックを経て 303 で戻ると、Lax の Cookie は付き、Strict の Cookie は付かない (両方)。`nakama aws login` のサインインはこの形で戻ってくる。
- 同じオリジンへのフォームの POST の Origin は、`Referrer-Policy: no-referrer` では `null` になり、`same-origin` では管理画面のオリジンになる (Chrome で、両方の設定を切り替えて確かめた)。
- CSP の `form-action 'self'` だけのとき、フォームの送信先からの別のオリジンへの 303 を、Chrome は止め、Firefox は通す。
- クリックに応じた `target="_blank"` のフォームの POST は新しいタブを開く。クリックの 6 秒後の `window.open` はブロックされる (両方)。
- 管理画面のオリジンから code-server の `/login` へフォームで POST すると、code-server にログインできる (両方。既存のインスタンスで確かめた)。
- Linux では、`/proc/net/tcp{,6}` の uid の列から、ループバックの接続の相手のユーザーが分かる (試作のすべての要求で特定できた)。この機械の `/proc` は `hidepid` なしでマウントされていて、ほかのユーザーのコマンドラインも読める。

## Decision

1. **置き場所**: 管理画面のサーバーは、`nakama ui` (のちに引数なしの `nakama`) のプロセスの中で、前面で動かす。127.0.0.1 の空きポートで待ち受け、`~/.config/nakama/panel.json` (0600) に PID・ポート・制御のポート・制御のトークン・ルーターのポート・版・開始時刻を書く。ルーターは `control.nakama.localhost:<ルーターのポート>` を、そこへ中継するだけにする。ルーターは今までどおり AWS の資格情報を持たない。
   - ウィンドウ (ターミナル) を閉じるか Ctrl-C を押すと、管理画面は止まる (unix では SIGHUP も)。Windows でコンソールを閉じたときは、Go が SIGTERM として届けるので、状態ファイルを消してから終わる。
   - ルーターは、管理画面が応答するあいだは終わらない (tunnel のデーモンと同じく数える)。管理画面が止まっているときにこのアドレスを開くと、ルーターは `nakama ui` を示すページを返す。
   - 2 回目の `nakama ui` は、動いている管理画面に制御のチャネル (`POST /sign-in-code`) で新しいサインインのコードを作らせ、ブラウザを開いて終わる。別の版の管理画面は止めてから起動する (ルーターと同じ)。
2. **サインイン**: `nakama ui` は 1 回限りのコード (256 ビットの乱数、有効 60 秒) を作り、`http://control.nakama.localhost:<ポート>/session?c=<コード>` をブラウザで開く。このコードを使ったときだけセッションができる。ブラウザを起動できなければ、アドレスを表示せずに `--browser` を案内するエラーにする (Issue #118 と同じ)。
3. **セッション**: セッションはプロセスのメモリにだけ置き、30 分使わなければ終わり、サインインから 12 時間で必ず終わる。数は 32、未使用のコードは 16 までで、古いものから消す。同じブラウザがもう一度サインインすると、前のセッションは終わる。ログアウトのボタンを置く。
   - Cookie は `__Host-nakama-panel` で、Secure、HttpOnly、Path=/、Domain なし、SameSite=Lax。`__Host-` は兄弟のホストに植えられないようにするため。Strict にしないのは、AWS のサインインから戻った直後に Cookie が付かないため。
4. **要求の検査** (`internal/panel`): SameSite は守りにならないので、次をすべて行う。
   - Host が `control.nakama.localhost:<ルーターのポート>` と一致すること (大文字と末尾のドットは受け付ける)。違えば 421 (DNS rebinding の防御)。
   - Linux では、接続の相手のソケットの持ち主が、nakama を実行しているユーザーであること (`internal/peeruid`)。ルーターはブラウザの接続を、管理画面はルーター (または直接の接続) を調べる。見つからなければ拒む。ほかの OS にはこの手段が無いので調べない (Windows ではほかのユーザーのコマンドラインは読めない。macOS は調べていない)。
   - サインインのページ、ログアウトの後のページ、スタイルシートとスクリプト以外は、セッションを求める。
   - `Sec-Fetch-Dest` が iframe などの埋め込みなら拒む。
   - API の読み取りは `Sec-Fetch-Site: same-origin` を求める。
   - 状態を変える要求は、POST、`Sec-Fetch-Site: same-origin`、管理画面自身の Origin、`http.CrossOriginProtection` の検査、セッションの CSRF トークン (フォームの `csrf` か `X-Nakama-CSRF` ヘッダー) をすべて求める。GET では何も変えない。
   - すべての応答に、`Content-Security-Policy` (`default-src 'none'`、`script-src 'self'`、`form-action 'self'`、`frame-ancestors 'none'`。インラインのスクリプトは使わない)、`X-Frame-Options: DENY`、`Referrer-Policy: same-origin`、`Cross-Origin-Opener-Policy: same-origin`、`Cross-Origin-Resource-Policy: same-origin`、`X-Content-Type-Options: nosniff`、`Cache-Control: no-store` を付ける。フォームが管理画面の外へ出るページ (AWS のサインイン、code-server、Kiro Crew) は、その送り先を `form-action` に書く。
   - サインインのコード、Cookie、CSRF トークン、パスワード、トークンはログに出さない。
5. **操作の実行**:
   - AWS のログイン、code、kirocrew は、管理画面のプロセスから呼ぶ。ログインは `signin.Login` で、サインインの後に AWS が戻す 127.0.0.1 のコールバックから 303 で管理画面に戻す。プロファイルの保存と有効化は `nakama aws login` と共通の処理にする。code と kirocrew は、管理画面のオリジンから自動で送信するフォームで code-server と Kiro Crew に移る。ワンショットのサーバーは使わない。
   - 読み取り (一覧、詳細、backup list、acl list、profile list、tunnel status、doctor、version) も、管理画面のプロセスで行う。
   - 状態を変える操作 (start、stop、init、create、delete、edit、update、provision、backup の create / note / delete、restore、cloudfront、acl の変更、tunnel の stop / restart) は、CLI のコマンド (`nakama aws <command> … --yes -o json`) を、ウィンドウから切り離したプロセスとして起動する。進捗と結果は `~/.config/nakama/ops/<id>/` (0600) に書かせ、画面はそれを表示する。ウィンドウを閉じても操作は続き、次に開いた画面で途中経過が見える。中断は CLI の Ctrl-C と同じ扱いにする。ログは 7 日で消し、`--show-password` は付けない。
6. **確認と同時実行**: 元に戻せない操作 (delete、restore、backup delete、acl delete all、acl prune) は対象の名前を入力させ、停止や料金を伴う操作 (stop、edit、update、init の NAT 方式の変更、cloudfront add / delete) は確認のダイアログを出してから、CLI に `--yes` を渡す。同じインスタンスへの状態の変更は、画面では 1 つずつにする。init はほかの操作と並べない。ターミナルからの CLI の実行は防げないので、画面に注意を書く。
7. **画面に載せるもの**: login、profile (list / save / load / unload / delete)、init、doctor、version (`--check`)、list (`--show-nat`)、info、start、stop、create、delete、edit、update、provision、backup (list / create / note / delete)、restore、code と kirocrew (`--internet`、`--no-auth`)、cloudfront add / delete (`--kirocrew`)、acl (list / add / delete / prune)、tunnel (status / stop / restart / suggest)。載せないもの: shell (対話の端末。code-server のターミナルを案内する)、ssh setup / ssh proxy、cli shell、tunnel start の任意の `-L`、login の `--remote` (画面と同じ端末のブラウザを使うので不要)、code / kirocrew の `--print-url` / `--browser` / `--local-port` / `--timeout`。パスワードは、詳細の「パスワードを表示」を押したときだけ出す。create の `--setup` / `--dotfiles` は、パスか URL を文字で入れる。
8. **画面の作り**: html/template と少しの素の JavaScript で作り、`go:embed` でバイナリに入れる。npm やビルドの手順は足さない。日本語と英語を、ブラウザの Accept-Language で選ぶ (どちらも無ければ英語)。
9. **順序**: 最初は Issue #139 の 10 本のプルリクエストに分ける予定だった。1 本目 (#140: ルーターの転送、管理画面のプロセス、セッション管理) のあと、利用者の指示 (「使うことができないような意味の無い機能ごとに PR を作成するな」) で、残りの機能 (AWS のログインとアカウント、一覧と詳細、操作の実行基盤と全操作、code / kirocrew、ポート転送、診断と版、引数なしの `nakama`、Windows のダブルクリック、ドキュメント) を 1 本にまとめた。リリースはそれがマージされてから。
10. **入口**: 引数なしの `nakama` は、標準入力と標準出力が端末のときだけ管理画面を開き (`nakama ui` と同じ)、それ以外 (スクリプト、パイプ) は今までどおりヘルプを出す。`nakama ui` は表に出し、`--browser` を受け付ける。Windows で nakama.exe をダブルクリックすると、今までどおり PowerShell を開き (ADR 0022)、同じプロセスで管理画面も動かしてブラウザを開く。PowerShell の Ctrl-C は管理画面を止めず、ウィンドウを閉じると両方が終わる。管理画面のログは捨て、ウィンドウには管理画面のアドレスか起動の失敗を 1 行だけ出す。画面とルーターのページが案内するコマンドは `nakama` にした。
11. **操作の実行の仕組み** (`internal/service/ops.go`、隠しコマンド `nakama aws op-runner --id <id>`): 管理画面は `op.json` (コマンドラインとインスタンス) を書き、標準出力と標準エラーを `stdout` と `log` に向けて、runner を tunnel のデーモンと同じく切り離して起動する (Windows は DETACHED_PROCESS)。runner はコマンドの木を自分のプロセスで実行し、127.0.0.1 の制御のチャネル (`/status`、`/stop`、bearer token、`runner.json`) に答え、終了コードを `exit.json` に書く。`/stop` は `exit.json` に中止と書いてから終了コード 130 で終わる (Ctrl-C と同じく、コマンドの途中で止まる)。状態は、`exit.json` があれば成功・失敗・中止、runner が答えれば実行中、どちらも無ければ中断 (起動から 30 秒までは実行中とみなす)。同じインスタンスの操作と init との重なりは、開始のときに断る。7 日を過ぎた終わった操作は消す。
12. **ログインの戻り**: `signin.LoginOptions.RedirectURL` を足し、コールバックは固定のページの代わりに管理画面の `/login/done?login=ok|failed` へ 303 で戻す (結果だけで、認可コードは渡さない)。トークンの交換とプロファイルの保存は、その間に裏で終わるので、`/login/done` は 1 秒ごとに読み直して待つ。

## Consequences

- 管理画面を使うあいだ、ウィンドウを 1 つ開いておく必要がある。代わりに、いつ動いているかが目に見え、閉じれば確実に止まる。時間のかかる操作は切り離したプロセスで動くので、閉じても途中で止まらない。
- ルーターの役割が 1 つ増える。中継するだけで、資格情報は持たない。ルーターが別の版に入れ替わっても、管理画面のプロセスとセッションはそのまま残る。
- Linux 以外では、ほかの OS のユーザーからの接続を接続の段階では拒めない。サインインのコードとセッション Cookie を持たない要求は、どの OS でも拒む。
- 同じ OS のユーザーで動くプログラムからは守れない。これは今の CLI と同じで、ドキュメントに書く。
- Safari (macOS 26) が http の `*.localhost` で `__Host-` の Cookie を受け付けなければ、Cookie の名前を変える必要がある (利用者が確かめる)。
- 操作のログ (`~/.config/nakama/ops/<id>/`) にはコマンドの出力が残る。パスワードは含まない (`--show-password` を付けず、create の JSON はそれが無ければパスワードを空にする)。

## 確認

- `internal/panel/panel_test.go`: Cookie の属性、コードの 1 回限りと期限、同じブラウザの再サインイン、セッションの無い要求、Host、ほかの OS のユーザー、埋め込み、API の読み取り、状態の変更 (Origin・`Sec-Fetch-Site`・CSRF の組み合わせ)、アイドルと 12 時間、数の上限、すべての応答のヘッダー、スタイルシート、言語の選択、ログに秘密が出ないこと。主な検査を 1 つずつ外すと、どれかのテストが失敗することを確かめた。
- `internal/peeruid/peeruid_linux_test.go`: `/proc` の表記、本物の IPv4 / IPv6 の接続、ほかのユーザー、見つからないときに拒むこと。
- `internal/service/panel_test.go`: 状態ファイル、制御のチャネル、2 つ目の管理画面、残った状態ファイル、ルーターの中継 (Host をそのまま、転送用のヘッダーを渡さない)、止まっているときのページ、ほかのユーザー、ルーターが管理画面のあいだ終わらないこと。
- `internal/cli/ui_test.go`: 起動、動いている管理画面の再利用、別の版の置き換え、ブラウザを起動できないとき、`--browser`、隠しコマンド。
- 一時的な設定ディレクトリと別のルーターのポートで、ビルドした nakama の `nakama ui` を headless の Chrome で開き、サインイン、`/api/session`、ログアウト、使用済みのコード、2 回目の `nakama ui`、SIGTERM での停止、ルーターが 1 分後に終わることを確かめた。
- 機能をまとめたプルリクエストでは、`internal/panel/pages_test.go` (全ページを日英で描画、フォームから組み立てる CLI のコマンドライン、受け付けない値、確認の 2 段階と名前の入力、重なりのメッセージ、戻り先の制限、開くときの進み方と送り先の検査、ログインの往復と失敗、アカウントの操作、テンプレートと処理が使う文言がすべてあること)、`internal/service/ops_test.go` (記録、状態、重なり、中止、残った runner、出力の末尾、整理、起動の失敗)、`internal/signin/callback_test.go` (コールバックの戻り)、`internal/cli/ui_test.go` (表に出た `ui`、端末でないときのヘルプ、ダブルクリックの横の管理画面、runner の終了コード、code の 1 回限りのページと同じフォーム) を足した。
- 実際の AWS アカウントで、一時的な設定ディレクトリと別のルーターのポートを使い、ビルドした nakama の管理画面を headless の Chrome で操作して確かめた。結果は Issue #139 の PR に書く。
- Safari (macOS 26)、Windows、本物の AWS のサインインを最後まで (利用者の資格情報が要る) は、利用者が確かめる。

## Alternatives considered

1. **ルーターの中に管理画面を置く**: 利用者の最初の案に一番近い。退けた理由: ルーターが AWS の資格情報と時間のかかる操作を持つことになる。別の版の CLI が `code` を実行するとルーターが入れ替わり、画面と実行中の操作が止まる。管理画面の不具合が code-server と Kiro Crew の通信全体に及ぶ。
2. **専用の常駐デーモン**: ウィンドウを閉じても画面が残る。退けた理由: 起動、状態、入れ替え、アイドルでの終了の仕組みが要り、Windows ではデーモンの子プロセスすべてに DETACHED_PROCESS が要る (Issue #126)。時間のかかる操作だけを切り離せば、画面のプロセスは前面でよい。
3. **状態を変える操作も画面のプロセスの中で呼ぶ**: 退けた理由: ウィンドウを閉じると操作が止まり、create・restore・delete の途中なら記録の無いリソースが残りうる (Ctrl-C と同じ)。CLI を起動すれば結果も CLI と同じになる。
4. **SameSite=Strict で守る**: `*.nakama.localhost` が同じサイトなので、code-server の中のページからの要求を防げない。AWS のサインインから戻った直後にも Cookie が付かない。
5. **`Referrer-Policy: no-referrer`**: 同じオリジンのフォームの POST の Origin が `null` になり、Origin を確かめられない。
6. **アドレスを表示して利用者に開かせる**: Issue #118 で利用者が退けた形。
7. **React などのフレームワークと npm**: ビルドの手順と依存 (THIRD-PARTY-NOTICES の対象) が増える。画面は html/template で足りる。

## References

- Issue #139、PoC の結果 (Issue の本文)
- Fetch Metadata (`Sec-Fetch-Site`): MDN、2023 年 3 月から主要なブラウザで使える
- Go の `net/http.CrossOriginProtection`
- Chromium `net/base/registry_controlled_domains/registry_controlled_domain.cc` (`GetDomainAndRegistryImpl`)
- RFC 6265bis の Cookie の接頭辞 (`__Host-`)
