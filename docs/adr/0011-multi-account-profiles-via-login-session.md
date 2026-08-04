# ADR 0011: マルチアカウント切替 (`nakama aws profile`) は login_session の参照コピーで実現する

- Status: Accepted
- Date: 2026-07-30
- Context: nakama v1.x のマルチ AWS アカウント対応

## Context

nakama の資格情報解決は「SDK default chain → 失敗時 `aws configure
export-credentials` fallback」(internal/awsx) である。`aws login` は
セッション識別子を `~/.aws/config` の **default profile の
`login_session`** に書き込むため、別アカウントへ `aws login` すると
識別子が上書きされ、旧アカウントに対して nakama を使えなくなる。
複数アカウントの code-server 環境を並行運用するには、アカウントごとの
セッションを共存させる仕組みが必要になった。

AWS CLI の `aws login` の仕様 (確認済み):

- `login_session` の値は **identity ARN 形式の識別子であり秘密情報ではない**。
- 実トークン (access token / refresh token) は `~/.aws/login/cache/` に
  セッション名別ファイルで保存され、AWS CLI が管理する。
- refresh token の有効期間は最長 12 時間。

つまり `login_session` は CLI のトークンキャッシュへの「ポインタ」であり、
この値さえ保持していれば、キャッシュ側のトークンが生きている限り
`aws configure export-credentials` で当該セッションの短期資格情報を
取り出せる。

## Decision

1. **login_session の参照コピー方式を採用する。**
   `nakama aws profile save` が現在の identity (STS GetCallerIdentity、
   **active profile を無視した default chain / fallback** で解決) と
   `aws configure get login_session` の値をアカウント ID キーで
   `$XDG_CONFIG_HOME/nakama/profiles.json` (既定 `~/.config/nakama`,
   dir 0700 / file 0600, temp+rename のアトミック書き込み) に保存する。
   保存のたびに profiles.json から AWS CLI config 形式の
   `~/.config/nakama/aws_config` を全体再生成し、
   `[profile nakama-<accountID>]` + `login_session` + `region` を書く。
   子プロセス (`aws` CLI) には `AWS_CONFIG_FILE` でこのファイルを渡す。
   **ユーザーの `~/.aws/config` / `~/.aws/credentials` には一切書き込まない。**
2. **秘密情報は保存しない。** profiles.json / aws_config に書くのは
   account ID・alias・login_session (ARN 形識別子)・identity ARN・保存時刻
   のみ。アクセスキー・トークンは AWS CLI のキャッシュに残したまま、
   解決は常に `aws configure export-credentials` へ委譲する。
3. **active profile は fail-closed。** `nakama aws profile load` で
   active を設定すると、`NewClients` は default chain を**試さず**
   `AWSCLICredentialsProvider{Profile: "nakama-<accountID>", ConfigFile:
   <nakama aws_config>}` のみで解決し、STS probe で検証する。probe 失敗時に
   別チェーンへフォールバックしない (意図しないアカウントへのリソース作成を
   防ぐ)。エラーには alias / account と復旧手順 (該当アカウントへの
   `aws login` 再実行、または `nakama aws profile unload`) を含める。
   probe の account ID が active の account ID と一致しない場合も
   エラーとする (セッション差し替えの検知)。
4. **active profile は `AWS_PROFILE` 環境変数より優先する。**
   `load` は明示的なユーザー操作であり、環境変数より意図が新しい・強い
   と解釈する。従来動作に戻す手段は `unload` として常に提供する。
   active 未設定時の解決フローは従来と完全に同一。
5. **12 時間制限と再 login での復活。** refresh token は最長 12h で失効する。
   失効後も profiles.json のエントリは有効なままであり、該当アカウントへ
   `aws login` し直せばセッションが復活する——ただし「同一 identity の
   再 login が同名のキャッシュファイルを更新する (= 保存済み login_session
   が再び有効になる)」ことは**未検証の仮説**である。識別子が変わる場合は
   `nakama aws profile save` の再実行で上書き登録すれば足りる。
   `profile list` の STATUS 列 (valid / expired / unknown) は
   export-credentials の成否による best-effort 判定とする。
6. **profile サブコマンド自身は `NewClients` を使わない軽量パスで動く。**
   `save` の STS は default chain のみの専用解決
   (`awsx.ResolveCallerIdentity`)、`list` / `load` / `delete` / `unload`
   は AWS SDK を使わない (検証は `AWSCLICredentialsProvider` の直接利用)。
   これにより active profile が壊れていても profile 操作 (特に `unload`)
   は常に可能で、復旧手段が自壊しない。

## Consequences

- 複数アカウントの `aws login` セッションが nakama 専有 config 上で共存し、
  `profile load` だけでアカウントを切り替えられる。
- nakama が保存するのは識別子のみで、資格情報の保存場所・失効管理は
  AWS CLI 側の責務のまま変わらない (攻撃面の増加が最小)。
- fail-closed のため、セッション失効中は該当アカウントの操作が
  エラーになる (可用性より誤アカウント操作防止を優先)。
- `aws login` 以外の資格情報ソース (SSO, アクセスキー等) は `profile save`
  の対象外 (login_session が無いためエラー)。従来どおり default chain /
  `AWS_PROFILE` で利用する。
- AWS CLI が login_session / export-credentials の仕様を変更した場合は
  追従が必要 (既存の fallback 実装と同じ依存)。

## Alternatives considered

1. **資格情報スナップショットの保存** (`export-credentials` の出力を
   nakama 側で永続化): 秘密情報をディスクに永続化することになり、
   かつ短期資格情報は 15 分〜12 時間で失効するため保存する価値が乏しい。
   却下。
2. **ユーザーの `~/.aws/config` に named profile を nakama が書き込む**:
   ユーザー所有ファイルへの書き込みは所有権侵害であり、`aws configure` や
   他ツールとの編集競合・破損リスクがある。却下。
3. **`AWS_PROFILE` ラッパー方式** (default profile を退避せず、ユーザーに
   named profile 運用を強いる / nakama がラップする): `aws login` は
   default profile の login_session を上書きするため、再 login のたびに
   退避が消える。根本解決にならない。却下。
