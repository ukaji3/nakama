# ADR 0005: `nakama aws connect` via Session Manager (vendored plugin)

- Status: Accepted
- Date: 2026-05-30
- Context: nakama v1.x 機能追加 (`connect`)

## Context

nakama インスタンスは private subnet にあり、受信ポートを開けず
(SG inbound は CloudFront managed prefix list の 8080/tcp のみ)、
SSH も鍵も使わない設計である。一方で利用者が SSH のように
インスタンス内部にログインし、コマンド実行・ポートフォワードを
行いたいという要望がある。

AWS Systems Manager Session Manager は受信ポート開放なしで
シェル / コマンド / ポートフォワードを提供する。接続先 EC2 の
instance role には既に `AmazonSSMManagedInstanceCore` が付与済で
(ADR 範囲外の `init` で構成)、`nakama aws edit` の SSM Run Command が
dev インスタンスで動作した実績がある。

要望として nakama は **単一バイナリ配布** を維持したい。AWS CLI の
`aws ssm start-session` は外部の `session-manager-plugin` バイナリを
必要とするため、単一バイナリと両立しない。

## Decision

`nakama aws connect <name>` を追加し、Session Manager を Go で直接
駆動する。data-plane (WSS + AWS 独自プロトコル) は AWS 公式
`aws/session-manager-plugin` のソースを **`third_party/` に版固定で
取り込む (案3)** ことで再利用し、外部バイナリ依存をなくす。

### コマンド面

- `nakama aws connect <name>` — 対話シェル
- `nakama aws connect <name> <command> [args...]` — 単発コマンド
  実行 (既定は非対話。`--tty` で対話 PTY)
- `nakama aws connect <name> -L <local>:<remote>` — ポートフォワード
  (接続先 instance のポートへ)
- `nakama aws connect <name> -L <local>:<host>:<remote>` — instance
  経由で別ホストへポートフォワード

`<name>` の後続はすべてリモートコマンドとして扱う
(cobra `SetInterspersed(false)`)。`-L` 等の nakama フラグは
`<name>` より前に置く。シェル / コマンド / ポートフォワードは排他。

### モードと SSM ドキュメントの対応

| モード | DocumentName | Parameters |
|---|---|---|
| 対話シェル | 既定 (`SSM-SessionManagerRunShell`) | — |
| コマンド (既定) | `AWS-StartNonInteractiveCommand` | `command` |
| コマンド (`--tty`) | `AWS-StartInteractiveCommand` | `command` |
| ポートフォワード | `AWS-StartPortForwardingSession` | `portNumber`, `localPortNumber` |
| 〃 (リモートホスト) | `AWS-StartPortForwardingSessionToRemoteHost` | `host`, `portNumber`, `localPortNumber` |

`ssm:StartSession` 自体は既存の `aws-sdk-go-v2/ssm` にあるため、
control-plane は新規 SDK 依存なしで呼べる。

### 取り込み (案3) の方式

- 公式 plugin は Go module 未公開 (リポジトリ root に go.mod が無く、
  `vendor/` 方式。pkg.go.dev でも pseudo-version 扱い)。直接 import は
  バージョン固定が pseudo-version でしか効かず、非公式 internal
  パッケージへの依存となるため脆弱。
- よって **特定 commit/tag のソースを `third_party/session-manager-plugin/`
  にコピー**し、コピーに go.mod を付与して依存を pin。nakama の
  go.mod に `replace github.com/aws/session-manager-plugin =>
  ./third_party/session-manager-plugin` を追加して公式 import パスを
  保ったままローカル固定する。
- 取り込みパッケージ: `session` (+ `shellsession` / `portsession`) /
  `datachannel` / `communicator` / `message` / `encryption` / `log` /
  `config` / `sessionutil` / `retry` / `websocketutil` / `version`。
- Apache-2.0 のため `LICENSE` / `NOTICE` を取り込みツリーに保持。
  取り込み元 commit を本 ADR の References に追記する (取り込み実施時)。

## Rationale

- 単一バイナリ要望を満たすのは「公式コードを Go に取り込む」案のみ
  (外部 CLI / plugin 同梱は別実行ファイルを伴う)。
- 直接 import (脆弱) と完全自前実装 (AWS 独自プロトコルを数千行規模で
  再実装・保守) の中間として、**版固定コピー** が安定性と実装量の
  バランスで最良。
- instance 側は既存 `AmazonSSMManagedInstanceCore` で充足し、サーバ
  構成の変更が不要。

## Consequences

- **upstream 追随は手動**。取り込み元 commit を固定し、更新時は
  re-copy + pin 更新を行う (リスク R8)。
- **呼び出し側 IAM 権限が新たに必要**。`ssm:StartSession` /
  `TerminateSession` / `ResumeSession` を利用者プリンシパルに要求する
  (instance role とは別。`init` では付与しない。リスク R9)。
- **バイナリサイズ増**。WSS + 暗号 + 端末処理が加わる。PoC で実測する
  (リスク R10)。
- **コマンドのリモート終了コード伝播は Session Manager の制約を受ける**。
  非互換は制限事項として扱う (リスク R11)。
- **ファイル転送・セッションログ記録・Windows クライアントは対象外**
  (requirements.md §7)。
- crash-consistency 等のインフラ前提は既存のまま変わらない。

## Alternatives Considered

- **案1: 外部 `aws ssm start-session` (CLI + plugin) を fork-exec**:
  実装は最小だが aws CLI と session-manager-plugin の事前 install が
  必要で、単一バイナリ要望に反する。
- **案2: plugin バイナリをリリース tar に同梱**: ユーザの追加 install は
  aws CLI のみで済むが、単一実行ファイルにはならない (tar に 2 実体)。
- **案3-import: 公式 plugin を Go module として直接 import**: 単一
  バイナリだが go.mod 未公開・非公式 internal API への pseudo-version
  依存となり、AWS 側変更に対して脆弱。
- **案4: data-plane プロトコルを完全自前実装**: 外部依存ゼロだが公式
  protocol 仕様が存在せず (実装の正は plugin ソースのみ)、数千行規模 +
  恒久保守が重い。

採用した **案3 (版固定コピー + replace)** は、単一バイナリを満たしつつ
import-direct の脆弱性を回避し、完全自前実装の保守コストを避ける。

## References

- <https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager.html>
- <https://docs.aws.amazon.com/systems-manager/latest/APIReference/API_StartSession.html>
- <https://github.com/aws/session-manager-plugin> (取り込み元。固定 commit は取り込み実施時に追記)
- session-manager-plugin は Apache-2.0
