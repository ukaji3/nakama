# ADR 0020: Kiro Crew の Web 画面は code-server のホスト名振り分けと専用プロキシで提供する

- Status: Accepted
- Date: 2026-10-06
- Issue: [#71](https://github.com/ukaji3/nakama/issues/71)、訂正 [#73](https://github.com/ukaji3/nakama/issues/73)

## Context

インスタンスには Kiro Crew が既定で入る (#68)。Kiro Crew の Web 画面 (gateway、127.0.0.1:5476 で待ち受け) は、`kirocrew token` が出す `http://localhost:5476?token=…` で開く。この URL を code-server のターミナルでクリックすると、code-server はリンクを `https://<CloudFront>/proxy/5476/?token=…` に書き換える。ところが、書き換え後の URL では画面が開かない。原因は互いに独立した 2 つの壁で、どちらもソースと実機で確かめた。

1. **Host の壁**: CloudFront (Managed-AllViewer ポリシー) はブラウザが送った Host をそのまま渡し、code-server の http-proxy も Host を書き換えずに転送する。Kiro Crew は DNS rebinding 対策として、許可リストに無い Host を `403 Host header not allowed.` で拒否する (`security_middleware.py`)。許可リストは `localhost` などのループバック名と `dashboard.url` のホスト名だけで、CSRF と WebSocket の Origin も同じリストで照合される (`origin.py`)。
2. **パスの壁**: code-server の `/proxy/<port>/` は先頭の `/proxy/<port>` を取り除いて転送する (`pathProxy.ts`)。一方、Kiro Crew 0.7.2 の画面はルートからの絶対パスを前提に作られている。配備済みのバンドルで数えたところ、JS は 683 ファイル / 59 MB、`` `/api/ `` が 702 箇所、`${location.host}/api/ws…` が 6 箇所、CSS の `url(/assets/…)` が 64 箇所あった。React Router にも basename の設定が無い。

利用者からの要件は 2 つある。**Kiro Crew を CloudFront に直接さらさず、必ず code-server を経由させること**。そして、**ブラウザは nakama から起動し、code-server のパスワード入力を省けること**。

## Decision

Kiro Crew 専用のホスト名 (新設する CloudFront distribution) を用意する。code-server の**ホスト名による振り分け (proxy-domain)** で転送先のポートを決め、インスタンス上の**専用プロキシ (nginx)** が Host と Origin を正規化してから Kiro Crew に渡す。

```
ブラウザ ─ https://<crew>.cloudfront.net/…
  → CloudFront (新設、Kiro Crew 用)
       オリジン = 既存の VPC Origin (code-server:8080)、WAF = 既存の Web ACL
       オリジンのカスタムヘッダー X-Forwarded-Host: 15476.kirocrew.nakama.internal
  → code-server:8080
       パスワード認証 → proxy-domain の照合で 15476 番を選び、パスを変えずに転送
  → nginx 127.0.0.1:15476
       Origin を検査 → Host と Origin を localhost:5476 に書き換え → X-Forwarded-Proto: https を付与
  → Kiro Crew 127.0.0.1:5476
```

- **ホスト名による振り分け**: code-server の `getHost()` は `Forwarded: host=`、`X-Forwarded-Host`、`Host` の順にホスト名を決める。CloudFront のオリジンのカスタムヘッダーで `X-Forwarded-Host` を固定値にすれば、独自ドメインが無くても `proxy-domain: kirocrew.nakama.internal` に一致させられる。このホスト名は DNS で解決されることはない。ブラウザが同名のヘッダーを送っても CloudFront が上書きするので、他のポートへ振り向けられることはない。振り分けはパスを削らないので、Kiro Crew をルート直下 (`/`) で配信できる。
- **nginx の役割** (127.0.0.1 だけで待ち受ける):
  - Origin が空か crew の URL のときだけ通す。それ以外は 403 を返す。この検査は Origin を書き換える前に行う必要がある。書き換えてしまうと、Kiro Crew 側の CSRF 照合は必ず通ってしまうからである。
  - Host と Origin を `localhost:5476` に書き換える。Kiro Crew の許可リストを広げずに済み、Cookie 名も従来どおり `mc_token_5476` になる。
  - `X-Forwarded-Proto: https` を付ける。Kiro Crew はループバックから来たこのヘッダーだけを信用するので、Cookie に Secure が付く。
  - `X-Forwarded-For` は書き換えない。Kiro Crew はこのヘッダーを見て「プロキシ経由のリクエスト」と判断し、設定の書き込みと秘密情報の表示をブラウザからは拒否する。外から届く画面としては、この挙動が妥当である。
  - WebSocket を中継する。`absolute_redirect off` を指定し、リダイレクト先を相対パスのまま返す。
- **code-server の設定**は、`config.yaml` に管理ブロックを追記するだけにとどめる。中身は `proxy-domain` と、crew のホスト名だけを完全一致で列挙した `trusted-origins` である。`trusted-origins` が必要なのは、code-server が WebSocket を受けるとき、Origin のホスト名と `getHost()` の結果 (= `X-Forwarded-Host`) の一致を確かめるためである。利用者が自分で `proxy-domain` や `trusted-origins` を設定している場合は、上書きせずに中止する。
- **Ports パネルはパス方式のまま固定する** (#73 で追加): code-server は `proxy-domain` が設定されていて `VSCODE_PROXY_URI` が未設定だと、`VSCODE_PROXY_URI=//{{port}}.<proxy-domain>` を自分で設定する (`src/node/cli.ts`)。このままだと Ports パネルとターミナルの `localhost:<port>` のリンクが、**すべてのポートについて**名前解決できない `<port>.kirocrew.nakama.internal` を指してしまう。そこで systemd の drop-in (`/etc/systemd/system/code-server@ubuntu.service.d/nakama-kirocrew.conf`) で `VSCODE_PROXY_URI=./proxy/{{port}}/` を先に設定する。これは `proxy-domain` が無いときの code-server 自身の既定値 (`rootBase + '/proxy/{{port}}/'`) と同じで、code-server のドキュメントにも相対パスが使えると書かれている。利用者が自分で `VSCODE_PROXY_URI` を設定している場合は触らない (code-server も既存の値は上書きしない)。drop-in は disable で消す。
- **既存の distribution** に `/proxy/5476*` と `/absproxy/5476*` の behavior を足し、そこにだけ CloudFront Function を付けて `https://<crew>/…` へ 302 で飛ばす (パスとクエリは引き継ぐ)。これで、ターミナルのリンクや Ports パネルからもそのまま入れる。足す behavior は、既定の behavior (`GetDistributionConfig` が返したもの) の**全項目を写した**うえで、Function の関連付けだけを nakama のものに差し替える (Lambda@Edge は 0 件)。UpdateDistribution は、読み出したときに API が埋めて返す項目 (`SmoothStreaming` など) が欠けた behavior を拒否するためである (#73。CreateDistribution は欠けていても受け付ける)。全項目を写せば、署名付き URL やレスポンスヘッダーのポリシーといった distribution の他の設定も、この 2 つのパスにそのまま効く。
- **ブラウザの起動**は `nakama aws kirocrew <名前>` が行う。ADR 0012 の 1 回限りログインページを流用し、フォームの送信先を `https://<crew>/login?to=/&token=<Kiro Crew のトークン>` にする。code-server はログインに成功すると `to` 以外のクエリを残したまま戻すので (`http.ts` の `redirect`)、code-server へのログインと Kiro Crew のトークン交換が 1 回のブラウザ起動で済む。
- **トークンの受け取り**は、インスタンス上で `kirocrew token` を実行し、その出力を Run Command で受け取る形にする。ただし、Run Command の実行結果は SSM に最大 30 日残り、`ssm:GetCommandInvocation` の権限があれば誰でも読める。そこで nakama がリクエストのたびに RSA-4096 の鍵をメモリ上で作り、公開鍵だけをインスタンスに渡す。インスタンス側は `openssl pkeyutl` (RSA-OAEP、SHA-256) でトークンを暗号化して返すので、SSM に残るのは暗号文だけになる。Session Manager を使えば履歴そのものが残らないが、そのコマンド実行モードは現在動かないので (#70)、採用していない。
- **構築と撤去**は `kirocrew enable` / `disable` で行い、どちらも冪等とする。crew の distribution ID は作成した直後にメタデータ (`/nakama/<名前>/meta/kirocrew-distribution-id`) へ書くので、途中で失敗しても再実行で distribution が重複しない。`delete` も同じものを片付ける。crew の distribution は VPC Origin を参照しているので、VPC Origin より先に削除する。
- 定数: gateway のポートは 5476、nginx のポートは 15476、proxy-domain は `kirocrew.nakama.internal`、Function 名は `nakama-<名前>-kirocrew`。

## Consequences

- 防御の層は 4 段になる: WAF の IP 許可リスト → code-server のパスワード → nginx の Origin 検査 → Kiro Crew のトークンと Host 検査。Kiro Crew が受け取る接続は、常にループバック経由で code-server から来る。
- crew のホスト名では code-server に別途ログインする必要がある (`cloudfront.net` は Public Suffix List に載っているので、2 つのホスト名の間で Cookie を共有できない)。このログインは `nakama aws kirocrew` が自動で行う。
- ブラウザから Kiro Crew の設定を書き換えたり、秘密情報を表示したりはできない。どちらも Kiro Crew 側の意図した仕様である。必要なときは `nakama aws tunnel <名前> -L 5476:5476` (ループバックへの直接接続) か、インスタンス上の CLI を使う。
- code-server の `trusted-origins` を crew のホスト名ひとつだけ緩める。この設定は code-server 全体の WebSocket の Origin 照合に効くので、ワイルドカードにはしない。
- code-server が `X-Forwarded-Host` を解釈する挙動は、ソース (`src/node/http.ts` の `getHost`) で確認したものであり、公式に約束された仕様ではない。code-server の更新で変わる可能性があるので、更新のたびに実機で確認する必要がある。
- `--bare` には code-server が無いので、この仕組みは使えない (`tunnel` を案内する)。
- インスタンスに nginx が入る (127.0.0.1 だけで待ち受ける。Ubuntu の既定サイトは消す)。enable と disable では code-server が 1 回再起動する。
- code-server の systemd に drop-in が 1 つ入る (`VSCODE_PROXY_URI` を既定値に固定するもの)。Ports パネルとリンクの書き換えは、enable の前と変わらない。
- CloudFront distribution が 1 つ増え、既存の distribution へのリクエストでは CloudFront Function が実行されるので、その分の料金がかかる。

## Alternatives considered

1. **パス転送 (`/proxy/5476/`) のまま、プロキシで中身を書き換える**: `/api/` が 702 箇所あり、WebSocket の接続先は実行時に組み立てられ、React Router は `location.pathname` をそのまま見る (スクリプトから偽装できない)。成り立たない。
2. **オリジンパスで `/proxy/15476` を付け直す (案 A')**: code-server の設定を変えずに済む。ただし、crew のホスト名の `/login` と `/_static/*` を常に code-server のログイン用に予約しなければならず、`/login` を持つ他のアプリへ広げられない。A 系では、`/login` が code-server に回されるのはログイン前だけである (`domainProxy.ts`)。
3. **Kiro Crew を CloudFront に直接さらす (`KIROCREW_BIND`)**: code-server の認証を飛ばしてしまう。接続元が CloudFront になり、Kiro Crew の HTTPS 判定とトークンの IP 固定がどう振る舞うかも分からない。利用者の要件にも反する。
4. **トンネル (`tunnel -L 5476:5476`) だけで済ませる**: ブラウザしか使えない端末からは使えず、code-server を経由させるという要件も満たさない。ループバックへの直接接続が必要な場面のための手段として、引き続き残す。
5. **独自ドメインとワイルドカードの CloudFront (案 B)**: 全ポートをルート直下で開けるようになり、Cookie も共有できる。ただし独自ドメイン、ACM、Route 53 が前提になる。将来の移行先であり、疑似ホスト名を本物のホスト名に置き換えれば、この ADR の nginx 層とコマンドはそのまま使える。
6. **Session Manager でトークンを受け取る**: 履歴が残らないので最も望ましい。ただし #70 が解決するまでは使えない。

## References

- ADR 0012 (open の自動ログイン)、ADR 0014 (自動再接続)、#68 (Kiro Crew の既定導入)、#70 (connect の EOF)
- code-server: `src/node/http.ts` (`getHost`、`authenticateOrigin`、`redirect`)、`src/node/cli.ts` (`proxy-domain` から `VSCODE_PROXY_URI` を導く処理)、`src/node/routes/domainProxy.ts`、`src/node/routes/pathProxy.ts`、`src/node/routes/login.ts`、`patches/proxy-uri.diff` (`proxyEndpointTemplate` の既定値)、`docs/guide.md` (`VSCODE_PROXY_URI` の相対パス)
- Kiro Crew: `src/kiro_crew/dashboard/origin.py`、`urls.py`、`token_auth.py`、`server_runtime/security_middleware.py`、`docs/guides/remote-and-mobile.md`
- CloudFront Developer Guide: Request and response behavior for custom origins (X-Forwarded-Proto の削除、X-Forwarded-For の付加)、Add custom headers to origin requests、CloudFront Functions event structure
- SSM User Guide: Running commands ("The history of each command is available for up to 30 days")

## 訂正の記録 (#73)

最初の版 (PR #72) には、本番で初めて `kirocrew enable` を実行したときに見つかった誤りが 2 つあった。上の Decision はどちらも訂正済みである。

1. 足す behavior が既定の behavior から 7 項目しか写していなかったため、UpdateDistribution が `InvalidArgument: The parameter SmoothStreaming flag is missing.` で拒否された。テストの fake は必須項目を検査していなかった。今は fake も、API と同じ形で返し、欠けた項目を拒否する。SDK が実際に送る XML の要素が既定の behavior と一致することもテストで確かめている。
2. 「`proxy-domain` を入れても Ports パネルは変わらない」と考えていたが誤りだった (`src/node/cli.ts` の自動設定を見落としていた)。本番の developer で `Using proxy URI in PORTS tab: //{{port}}.kirocrew.nakama.internal` を確認し、drop-in での固定を加えた。
