# ADR 0016: `nakama aws login` — AWS CLI に依存しないネイティブ login

- Status: Accepted
- Date: 2026-08-26
- Context: issue #25 (nakama aws login: AWS CLI に依存しないネイティブ login (監査プロキシ対応))

## Context

企業の監査プロキシ (TLS 傍受) 環境では、AWS CLI (Python 製) が certifi
の CA バンドルを既定で使用し OS 証明書ストアを参照しないため、
`aws login` が CERT エラーで失敗する。主要ユースケース
「ブラウザで AWS Web コンソールにログイン → `aws login` → nakama 利用」
がその環境では成立しない。

さらに nakama 自身の資格情報解決も従来は
`aws configure export-credentials` へのシェルアウト
(旧 internal/awsx/awscli_credentials.go) であり、キャッシュ失効時の
リフレッシュも Python CLI 経由で同じ CERT エラーになる。login コマンド
の追加だけでは不十分で、**リフレッシュまでネイティブ化**する必要がある。

nakama は Go 製で、TLS は platform verifier (OS 証明書ストア、ADR 0015)、
プロキシは internal/proxy (環境変数 → Windows OS 設定) を使うため、
ネイティブ実装なら監査プロキシ環境で追加設定なしに動作する。

### プロトコル要約 (AWS CLI v2 ソース精読で確認した確定事実)

`aws login` = OAuth 2.0 authorization code + PKCE + **DPoP (RFC 9449,
ES256/P-256)**。

- エンドポイント (aws パーティション): `https://{region}.signin.aws.amazon.com`
  (nakama は internal/config の Region に固定)
- client_id: `arn:aws:signin:::devtools/same-device` (通常) /
  `arn:aws:signin:::devtools/cross-device` (`--remote`)
- 認可: `GET /v1/authorize?response_type=code&client_id=...&state={uuid4}`
  `&code_challenge_method=SHA-256&scope=openid&redirect_uri=...`
  `&code_challenge={base64url-no-pad(sha256(verifier))}`。
  PKCE verifier は `[A-Za-z0-9-._~]` から crypto/rand で選んだ 64 文字
- トークン: `POST /v1/token` (JSON ボディ直接、SigV4 署名なし、
  `DPoP` ヘッダー必須)。auth_code 交換は
  `{clientId, grantType:"authorization_code", code, codeVerifier, redirectUri}`、
  リフレッシュは `{clientId, grantType:"refresh_token", refreshToken}`
- DPoP: ES256 JWT。header に P-256 公開鍵 JWK (x/y は 32 バイト
  big-endian 座標の base64url no-pad)、payload は
  `{htm:"POST", htu:<フル URL>, iat, jti:uuid4}`、署名は r||s 各 32
  バイト固定長の base64url no-pad
- 応答: `{accessToken:{accessKeyId,secretAccessKey,sessionToken},`
  `tokenType:"aws_sigv4", expiresIn(≤900), refreshToken, idToken(auth_code時のみ)}`
- エラー: HTTP 401 (`TOKEN_EXPIRED`/`AUTHCODE_EXPIRED`)、
  403 (`USER_CREDENTIALS_CHANGED`/`INSUFFICIENT_PERMISSIONS`)、ボディ
  `{error, message}`
- `login_session` = idToken (JWT) の `sub` クレーム (ARN 形状)。
  アカウント ID はその ARN の第 5 フィールド
- トークンキャッシュ: `~/.aws/login/cache/{hex(sha256(login_session))}.json`
  (`AWS_LOGIN_CACHE_DIRECTORY` で上書き可、dir 0700 / file 0600)。
  `dpopKey` は SEC1 EC 秘密鍵 PEM。リフレッシュは失効 5 分前から、
  refresh token は回転 (accountId は旧トークンから維持、
  idToken/dpopKey/clientId/tokenType は不変)
- same-device の redirect_uri: `http://127.0.0.1:{port}/oauth/callback`
  (nakama は CLI と異なり **127.0.0.1 のみに bind**)。cross-device は
  `{base}/v1/sessions/confirmation` + ユーザーが base64(`state=...&code=...`)
  を貼り付け
- IAM ユーザー/ロールには管理ポリシー `SignInLocalDevelopmentAccess` が
  必要 (root は不要)

## Decision

### 1. `~/.aws/config` には書き込まない

AWS CLI の `aws login` はログイン完了時に `~/.aws/config` の default
プロファイルへ `login_session` を書き込むが、nakama は ADR 0011 の方針
どおりユーザーの `~/.aws/config` を**一切変更しない**。代わりに
`nakama aws login` は完了時に nakama 自身のプロファイルレジストリ
(`~/.config/nakama`, ADR 0011) へ自動 save & load し (STS
GetCallerIdentity で識別確認後)、直後から全 nakama コマンドが新しい
アカウントで動作する。

### 2. トークンキャッシュは AWS CLI と共有する (バイト互換)

キャッシュのディレクトリ・ファイル名 (sha256)・JSON 構造・`dpopKey` の
PEM 形式を AWS CLI と完全互換に保つ。同じ login_session なら同じ
ファイルを両ツールが読み書きするため、既存の
「`aws login` → `nakama aws profile save`」フローのセッションも nakama が
ネイティブにリフレッシュでき、逆に nakama で作ったセッションを AWS CLI
がそのまま使える。書き込みは temp+rename のアトミック方式 (profilestore
と同一パターン)。

### 3. 資格情報解決のシェルアウトを完全に置換する

`AWSCLICredentialsProvider` (`aws configure export-credentials`) と
`LoginSessionResolver` (`aws configure get login_session`) を削除し、

- active nakama profile のパス: profilestore の login_session から
  `signin.RefreshingCredentialsProvider` (キャッシュ読取り → 5 分閾値
  判定 → DPoP リフレッシュ → キャッシュ更新) で解決。fail-closed
  セマンティクス (ADR 0011) は不変。
- 既定チェーンのフォールバック: `~/.aws/config` (`AWS_CONFIG_FILE`
  尊重) の対象セクション (`AWS_PROFILE` 設定時は `[profile <name>]`、それ以外は `[default]`) を**読み取り専用の自前 INI パース**で
  参照し、`login_session` があれば同じ provider で解決。

これにより nakama の実行に aws CLI バイナリが不要になる。全 HTTP は
internal/proxy 経由・platform verifier の TLS (ADR 0015) で行われるため
監査プロキシ環境でも CERT エラーが発生しない。

### 4. `--remote` は実装、`logout` は見送り

SSH/headless 環境向けの cross-device フロー (`--remote`) は AWS CLI と
同等に実装する。一方 `aws logout` 相当 (キャッシュ削除) は、
`nakama aws profile delete` + キャッシュファイルの手動削除で代替可能で
需要も未確認のため v1 では見送る。

## Consequences

- 監査プロキシ環境で「ブラウザで AWS コンソールログイン →
  `nakama aws login`」だけで nakama が使える (aws CLI 不要)。
- 既存フロー (`aws login` → `nakama aws profile save`) も引き続き動作し、
  そのセッションのリフレッシュもネイティブ化される。
- `nakama aws profile list` の STATUS 判定は export-credentials の成否
  ではなくネイティブのキャッシュ読取り+リフレッシュの成否になる
  (aws CLI 不在による "unknown" は発生しなくなる)。
- エラーメッセージは sign-in の error コード
  (TOKEN_EXPIRED / USER_CREDENTIALS_CHANGED / INSUFFICIENT_PERMISSIONS
  等) に応じた対処 (再 `nakama aws login`、SignInLocalDevelopmentAccess
  ポリシー付与など) を含む。トークン・秘密鍵・idToken はログ/エラー/
  argv/環境変数に一切出さない。

## Risks

- **API 変更の可能性**: signin API (`/v1/authorize`, `/v1/token`) は
  botocore/SDK にモデル公開されているが公式ドキュメントは薄く、AWS が
  将来変更する可能性がある。キャッシュを CLI 互換に保つ限り、変更時は
  「`aws login` + `nakama aws profile save`」の旧フローへ即時退避できる
  (リフレッシュ経路が壊れた場合も同様に aws CLI での再ログインで復旧)。
- **refresh token 回転の同時更新競合**: AWS CLI と nakama (または複数の
  nakama プロセス) が同一キャッシュを同時にリフレッシュすると、回転した
  refresh token を後勝ちで上書きし合う可能性がある。これは AWS CLI
  同士でも存在する既知の挙動で、ファイルロックは導入せず許容する
  (敗者は次回リフレッシュ失敗 → 再ログイン誘導)。書き込み自体は
  temp+rename でアトミックであり、破損ファイルは生じない。

## Alternatives considered

1. **AWS_CA_BUNDLE / REQUESTS_CA_BUNDLE の運用回避**: AWS CLI に企業 CA
   を環境変数で注入すれば CERT エラー自体は回避できるが、全利用者への
   配布・更新の運用負担が大きく、nakama の「追加設定なしで動く」方針
   (ADR 0015) に反する。→ 却下。
2. **シェルアウト維持 + CA 注入の自動化**: nakama が aws CLI 子プロセス
   に CA バンドルを注入する案。OS ストアから CA を抽出する処理が OS
   ごとに必要になり複雑な割に、aws CLI への依存 (インストール必須) が
   残る。→ 却下。
3. **nakama 独自のトークンキャッシュ**: 独自形式なら実装自由度は上がる
   が、`aws login` との相互運用 (退避経路) を失い、同一セッションの
   二重管理になる。→ 却下 (CLI 互換キャッシュを採用)。

## References

- issue #25
- AWS CLI v2 ソース: `awscli/customizations/login/{login,utils}.py`,
  `awscli/botocore/credentials.py` (LoginCredentialFetcher/LoginProvider),
  `awscli/botocore/utils.py` (build_dpop_header ほか),
  `awscli/customizations/sso/utils.py` (AuthCodeFetcher)
- RFC 7636 (PKCE), RFC 9449 (DPoP)
- ADR 0011 (マルチアカウントプロファイル), ADR 0015 (OS プロキシ / TLS)

## 追記 (2026-10-15, Issue #94): SDK for Go v2 が login_session を解決するようになった

aws-sdk-go-v2/config v1.32.0 (2025-11) から、既定の資格情報チェーンに AWS ログイン (`credentials/logincreds`) が入った。nakama は #94 でこの版以降の SDK に上げた。

- **解決の経路**: active profile が無いときは、`login_session` を含むプロファイルを、nakama のフォールバックより先に SDK が解決する。nakama のフォールバックは、SDK の経路が失敗したときの互換レイヤとして残す。
  - active profile の経路は変わらない (nakama の provider を明示的に渡す。fail-closed)。
  - SDK の更新処理も、nakama が `LoadDefaultConfig` に渡す HTTP クライアント (プロキシ対応) を使う。
- **キャッシュの形式**: SDK はトークンを更新すると、キャッシュを書き換える。そのときの形式が、AWS CLI や nakama と次の 2 点で異なる。
  - `accessToken.expiresAt` は `time.Time` の JSON (小数秒と手元の時差を含む)。nakama は `Z` の形しか読めず、日本の時刻設定ではキャッシュを「壊れている」と判定していた。#94 で、任意の RFC 3339 を読むように直した。
  - ID トークンを `identityToken` のキーで書く (`idToken` は消える)。nakama も AWS CLI も、トークンの使用と更新に ID トークンを必要としないので、影響しない。
  - nakama が書くときは、引き続き AWS CLI と同じ形式にする。
- **Risks の訂正**: 「書き込み自体は temp+rename でアトミック」は、nakama の書き込みについてのみ正しい。SDK の書き込みは `O_TRUNC` で開いて書く。そのため、書き込みの途中を別のプロセスが読むと、JSON の解析に失敗する可能性がある (書き込みが終われば、もう一度実行して読める)。nakama の側では対処できないので、許容する。
