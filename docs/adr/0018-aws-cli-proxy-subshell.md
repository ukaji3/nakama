# ADR 0018: `nakama aws cli shell` — aws CLI の企業プロキシ対応は設定済みサブシェルで提供する

- Status: Accepted
- Date: 2026-08-29
- Issue: [#42](https://github.com/ukaji3/nakama/issues/42)

## Context

企業の TLS インスペクション (MITM) プロキシ配下の Windows では、aws CLI は既定で動作しない。

- aws CLI のプロキシ構成手段は環境変数 (`HTTP_PROXY` / `HTTPS_PROXY`) のみで、OS のプロキシ設定 (WinHTTP 静的設定 / PAC) を読まない。NTLM/Negotiate プロキシ認証は非対応 (公式ドキュメント明記)
- TLS 検証は同梱 CA バンドル (certifi 由来) のみを信頼し、GPO で配布された企業 CA (Windows 証明書ストア) を読まない → `CERTIFICATE_VERIFY_FAILED`。OS ストア対応の要望 (aws/aws-cli#6193) は closed-for-staleness
- 公式の対処は「企業 CA を PEM 化して `AWS_CA_BUNDLE` / `ca_bundle` で指定 + 環境変数でプロキシ設定」だが、証明書のエクスポートや `setx` は一般ユーザーには難しい

nakama 自身は ADR 0015 でプロキシ自動解決 (env → WinHTTP/PAC、`internal/proxy`) と platform verifier による OS 証明書ストア利用を実装済みであり、この知識を aws CLI が読める形に変換できる。

## Decision

### 提供形態: 設定済みサブシェル (恒久的な状態変更をしない)

`nakama aws cli shell` は、検出結果を注入した**新しいシェル**を同一コンソール内に起動する (aws-vault の `exec` と同型)。exit で元のシェルに戻る。

- 子プロセスは親の環境変数を継承する、という OS の基本則だけに依存する。外部プロセスから親シェルの環境変数を書き換えるサポートされた API は存在せず (`set` はシェル内部コマンドだから効く)、`setx` 相当の恒久設定は全アプリケーションに波及する副作用を持つ
- レジストリ・ユーザー環境変数・`~/.aws/config` (ADR 0011/0016 の不変条件)・親シェルには一切変更を残さない。ディスクに書くのは CA バンドル PEM (公開証明書のみ) 1 ファイル
- 起動するシェルは親プロセスの実体 (`QueryFullProcessImageName`) が既知のシェル (powershell.exe / pwsh.exe / cmd.exe) ならそれ、不明なら `ComSpec`、`--shell` で明示指定可
- ネスト防止のためマーカー環境変数 `NAKAMA_AWS_CLI_SHELL=1` を注入し、二重起動を拒否する

### 検出エンジン: OS が信頼するチェーンだけを書き出す (`internal/cliproxy`)

1. ADR 0015 の解決器で AWS エンドポイント向けプロキシを解決 (env 優先、次に WinHTTP/PAC)
2. プロキシ経由で STS エンドポイントへ TLS プローブし、**OS 証明書ストア (platform verifier) の検証に通ったチェーンのみ**を扱う。GPO 配布された企業 CA は通り、真の攻撃者のチェーンは通らないため、攻撃者の CA を信頼バンドルへ書き出す事故が構造的に起きない
3. 検証済みチェーンのルートを同梱の Amazon Trust Services (ATS) ルート 5 枚と照合し、ATS 以外なら「TLS インスペクションあり」と判定
4. インスペクション時のみ `AWS_CA_BUNDLE` 用 PEM を生成: 企業 CA チェーン (リーフ除く) + ATS ルート 5 枚。`AWS_CA_BUNDLE` は既定バンドルの**置き換え**であるため、ATS ルートを同梱しないとインスペクション除外リストに載った AWS エンドポイントの検証が壊れる
5. 生成した設定そのもの (プロキシ + バンドル) で疎通を再確認し、結果をバナー表示する

### ATS ルートの同梱

`internal/cliproxy/certs/` に Amazon Root CA 1–4 + Starfield Services Root CA G2 を `go:embed` する。取得時に amazontrust.com と Ubuntu ca-certificates の 2 ソースで DER の SHA-256 を照合し、テストがフィンガープリントをピン留めする (改竄・取り違えは CI で検出)。

### プロキシ認証の扱い

CONNECT が 407 を返す場合は `Proxy-Authenticate` のスキームを読み、Basic ありなら「URL 埋め込みで自力設定」、NTLM/Negotiate のみなら「ローカル認証リレー (px / Cntlm) の導入」を案内してエラー終了する。aws CLI が Basic しか話せない以上、環境変数の注入だけでは解決できない (SSPI リレーの内蔵は本 ADR のスコープ外)。

### Windows 専用

コマンドは Windows 以外では「Windows 専用オプション」の案内を表示してエラー終了する。Linux は環境変数が慣習として機能し、macOS はプロキシ自動検出が CGO_ENABLED=0 のため非対応 (ADR 0015) であり、サブシェルを提供しても検出部が機能しない。

## Consequences

- MITM プロキシ配下の Windows で `nakama aws cli shell` → シェル内で `aws s3 ls` が追加設定なしで動く。exit すれば痕跡が残らない
- 効果はサブシェル内に限定される。恒久設定 (setx 相当) はユーザー裁定によりスコープ外とした — 全プロセスへの波及と undo 管理という別の複雑さを持ち込むため
- 環境変数は標準のものなので、サブシェル内では aws CLI 以外の AWS SDK 系ツールにも効き得る
- 既存の Windows Terminal ウィンドウの新タブ等、環境ブロックの継承経路に依存する注意点は発生しない (サブシェル方式のため)

## Alternatives considered

1. **ユーザー環境変数の恒久設定 (`setx` 相当)** — 全アプリに波及し、実行したシェル自身には反映されない (Microsoft の setx 仕様)。undo の管理も必要。ユーザー裁定で不採用
2. **`~/.aws/config` の `ca_bundle` 書き込み** — nakama の「ユーザーの `~/.aws/config` は変更しない」不変条件 (ADR 0011/0016) に抵触し、プロキシ側はどのみち環境変数が必要
3. **`aws.cmd` シムを PATH 先頭に配置** — 「aws にだけ効く」を厳密に満たすが、PATH 汚染と aws CLI 更新への追従問題を持ち込む
4. **eval パターン (`--env | Invoke-Expression`)** — PowerShell 限定で、一般ユーザーに追加の作法を要求する
5. **企業 CA チェーンのみのバンドル (ATS 同梱なし)** — インスペクション除外ドメイン混在環境で公開 CA の検証が壊れる
6. **Windows 証明書ストア全体の PEM 化** — Windows はルートをオンデマンド取得するため列挙が完全である保証がなく、ユーザー固有の不要な信頼も持ち込む。観測チェーン + ATS の合成で十分

## References

- ADR 0015 (プロキシ自動解決と platform verifier)、ADR 0011/0016 (`~/.aws/config` 不変条件)
- ADR 0022 (nakama.exe のダブルクリックで同じ設定のシェルを開く。サブシェルの PATH に nakama のフォルダーを足し、実行中の Ctrl-C で nakama が終わらないようにした)
- AWS CLI User Guide: cli-configure-proxy / cli-chap-troubleshooting (SSL certificate errors)
- aws/aws-cli#6193 (Use System Root CA Store on Windows — closed-for-staleness)
- Microsoft Learn: setx (現在のウィンドウには反映されない仕様)
