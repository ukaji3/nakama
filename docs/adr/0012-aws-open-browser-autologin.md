# ADR 0012: `nakama aws open` の自動ログインはローカル one-shot ページからの top-level form POST で実現する

- Status: Accepted
- Date: 2026-08-05
- Context: nakama v1.x のブラウザ起動・自動ログイン (`nakama aws open`)

## Context

`nakama aws info <name> --show-password` で得たパスワードをブラウザの
ログインフォームへ手動コピーする手順は、インスタンスを開くたびに発生する
摩擦であり、パスワードがクリップボードや端末スクロールバックに残る。
`nakama aws open <name>` は「URL をブラウザで開き、ログイン済みの状態で
着地する」ことを目的とする。

code-server の認証仕様 (ソースコードで確認済み):

- 認証は **cookie のみ**。`src/node/http.ts` の `authenticated()` は
  `req.cookies[req.cookieSessionName]` (既定 `code-server-session`) だけを
  見る。**URL / クエリパラメータによる認証手段は存在しない**ため、
  「トークン付き URL を開くだけ」の実装は不可能。
- `POST /login` は form-urlencoded の `password` / `base` / `href` と
  クエリ `?to=<path>` を受け取り、成功時に
  `res.cookie(cookieSessionName, hashedPassword, getCookieOptions(req))` +
  `to` (既定 `/`) への 302 を返す。
- `/login` には **CSRF トークンも Origin チェックも存在しない**。
  `ensureOrigin` が適用されるのは WebSocket ルート
  (routes/vscode.ts の wsRouter, domainProxy.ts, pathProxy.ts) のみで、
  `app.router.use("/login", login.router)` には適用されない。
- ログイン成功はレートリミッタ (2/min + 12/hour) を消費しない
  (失敗時のみ消費)。
- cookie は `sameSite: "lax"`。`domain` / `path` は form の `base` / `href`
  から計算される。

一方、CLI プロセスは**ブラウザの cookie store に直接書き込めない**
(各ブラウザ固有の暗号化ストアであり、外部からの書込みはサポート外)。
したがって「nakama が /login を叩いて得た cookie をブラウザへ注入する」
方式は成立しない。cookie をブラウザに first-party として受け取らせるには、
**ブラウザ自身に /login へ top-level navigation で POST させる**しかない。

## Decision

1. **METHOD A (one-shot localhost ページからの top-level form POST) を
   既定動作とする。** `open` は SSM からパスワードを取得し、
   127.0.0.1 のランダムポートに単発 HTTP サーバを立て、自動 submit する
   HTML フォーム (`method=post`,
   `action=https://<distribution-domain>/login?to=/`, hidden の
   `password` / `base=.` / `href`) を 1 回だけ配信してブラウザを
   その URL で起動する。ブラウザは top-level navigation で /login へ
   POST し、Set-Cookie を first-party で受け取ってログイン済みで着地する。
   hidden フィールドは code-server 自身の対話ログインフォームと同一構成に
   し、cookie の domain / path 計算を対話ログインと完全一致させる。
2. **ローカルサーバのセキュリティ特性**: bind は 127.0.0.1 のみ
   (他ホストから到達不能)、ポートはカーネル割当 (port 0)、ページのパスは
   crypto/rand による 128 bit 以上の URL-safe ランダムトークン、配信は
   **厳密に 1 回** (2 回目以降は 410 Gone)、配信完了後は即時 shutdown、
   `Cache-Control: no-store` / `Referrer-Policy: no-referrer` /
   `<meta name="referrer" content="no-referrer">` 付与、外部リソース参照
   なし。パスワードは**メモリ内のレンダリング済みページにのみ**存在し、
   ディスクへの書き込み・ログ出力は一切しない。埋め込み値は html/template
   で文脈依存エスケープする (引用符や `<` を含むパスワードでも属性から
   脱出できない)。ブラウザがページを取得しないまま `--timeout`
   (既定 30s) が経過したらサーバを畳む。
3. **METHOD C (プレーンに URL を開く / 表示する) へ graceful fallback
   する。** パスワード取得失敗・ローカル listener の bind 失敗・
   ブラウザランチャー不在/起動失敗・timeout 内にページ未取得のいずれでも、
   自動ログインなしで URL を開く (ランチャー不在時は表示する) 動作へ
   退避し、理由と `nakama aws info <name> --show-password` の案内を
   stderr に出す。URL を開けた/表示できた場合の終了コードは 0。
   fatal エラー (インスタンス未存在・非 running・distribution domain
   未設定) のみ非 0 で終了する。
4. **`--no-auth` / `--print-url` / `--browser` / `--timeout` を提供する。**
   `--no-auth` はパスワード取得とローカルサーバを完全にスキップして
   METHOD C を明示利用する。`--print-url` はブラウザを起動せず URL を
   表示する (auth 有効時は one-shot URL も表示し、timeout までサーバを
   維持する。headless / SSH 環境向け)。`--browser <cmd>` はランチャー
   コマンドの上書き、`$BROWSER` 環境変数も尊重する。
5. **WAF allowlist の soft check を行う。** 呼び出し元の public IPv4 を
   取得し、WAF IP set のエントリと **CIDR containment** (文字列一致では
   ない) で照合する。未収載なら「403 になる。`nakama aws acl add` を」と
   stderr に警告する (中断はしない)。チェック自体の失敗は debug ログのみで
   `open` を壊さない。
6. **JSON / YAML 出力にパスワードを含めない。** `-o json|yaml` の結果
   オブジェクトは name / url / method / opened / auto_login / note のみ。

## Consequences

- パスワードの手動コピーが不要になり、クリップボード・スクロールバック
  への露出が消える。パスワードはディスクに書かれず、メモリ内の 1 回限りの
  ページとしてのみ存在する。
- **リスク (明示)**: この方式は `/login` に CSRF トークン・Origin
  チェックが**無い**ことに依存する。将来の code-server リリースが
  `/login` に Origin チェック等を追加した場合、METHOD A は機能しなくなる。
  その場合も METHOD C fallback により `open` 自体は退化して動き続ける
  (自動ログインのみ失われる)。
- **残存リスク**: one-shot ページ配信までの短い時間窓の間、同一ホスト上の
  ローカルプロセスがトークンを推測 (128 bit のため実質不可能) または
  観測 (プロセス一覧の引数には URL は出ないが、当該ユーザー権限の
  プロセスがブラウザ経由等で知り得た場合) してページを先取りすると、
  パスワードを 1 回だけ読める。bind が 127.0.0.1 のみ・単回配信・
  timeout により窓は最小化しているが、ゼロではない。
- ローカルの一時ポートを開くため、環境によってはローカルファイアウォールの
  確認ダイアログが出る可能性がある。
- **未検証 (UNVERIFIED)**: 「この form POST が CloudFront を経由して
  実際に cookie を確立する」というエンドツーエンドの動作は、実 AWS に
  対してまだ実施していない。CloudFront 設定 (POST 許可 + CachingDisabled +
  AllViewer origin request policy で Host / Origin / Cookie / body が
  オリジンへ届くこと) と code-server のソース仕様の組み合わせから成立する
  はずだが、実環境での確認は今後の宿題である。失敗した場合も METHOD C
  fallback により実害は「自動ログインが効かない」に留まる。

## Alternatives considered

1. **トークン付き URL を開くだけの方式**: code-server に URL / クエリ
   認証が存在しないため不可能。却下。
2. **一時 HTML ファイル (file://) を開く方式**: ローカルサーバ不要で
   実装は簡単だが、**パスワードを含む HTML がディスクに永続化**され、
   削除タイミングの制御も難しい (ブラウザが読む前に消せない)。
   tmpfs が保証されない環境もある。却下。
3. **nakama が /login を叩いて得た hashed cookie 値の保存・再利用**:
   cookie 値を得ても**ブラウザの cookie store へ注入する手段が無い**ため
   目的を達成できない。加えて hashed password 相当の資格情報を
   ディスクに置くことになる。却下。
4. **クリップボードへのパスワードコピー + URL オープン**: 手動貼り付けが
   残り、クリップボード履歴ツールへの露出という別のリスクを生む。却下。
