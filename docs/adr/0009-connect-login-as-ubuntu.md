# ADR 0009: `nakama aws connect` は ubuntu ユーザーでログインする

- Status: Accepted
- Date: 2026-07-09
- Context: nakama v1.x `connect` の UX 改善 (ADR 0005 の精緻化)

## Context

`nakama aws connect`(ADR 0005)は Session Manager でシェル / コマンド /
ポートフォワードを提供する。当初の対話シェルは `DocumentName` を指定せず
SSM 既定の `SSM-SessionManagerRunShell` で起動していたため、セッションは
**`ssm-user`** として開始されていた。

しかし code-server とその開発用ホーム・設定・ツール群は **`ubuntu`**
ユーザー配下にある(Ubuntu AMI の既定ユーザーであり、code-server も
`systemctl ... code-server@ubuntu` で ubuntu として稼働)。ssm-user で
ログインすると、利用者は毎回 `sudo su - ubuntu` を手動実行する必要があり、
その場合 `exit` を2回(ubuntu シェル→ssm-user シェル)入力しないと
切断されないという不便があった。

前提の確認(AWS 公式ドキュメント):

- Session Manager は既定で `ssm-user` として接続する。SSM Agent は
  `ssm-user` を作成し `/etc/sudoers.d/ssm-agent-users` に
  `ssm-user ALL=(ALL) NOPASSWD:ALL` を書き込む(既定でパスワードなし
  sudo が有効)。
  出典: docs.aws.amazon.com「Turn on or turn off ssm-user account
  administrative permissions」「Turn on Run As support for Linux and
  macOS managed nodes」。
- よって ssm-user から `sudo su - ubuntu` はパスワード入力なしで成功する。

## Decision

`connect` のシェル / コマンドを **ubuntu ユーザーで実行**する。実装は
Session Manager の StartSession ドキュメント選択のみで完結させ、
サーバ側(userdata)やアカウント全体のセッション設定は変更しない。

1. **対話シェル**: `DocumentName = AWS-StartInteractiveCommand`、
   `command = "exec sudo su - ubuntu"`。
   - `exec` により ssm-user のシェルプロセスを `sudo su - ubuntu` で
     **置換**する。ubuntu シェルを `exit` すると戻るべき親シェルが無く
     セッションが終了するため、**単一 `exit` で切断**できる。
2. **コマンド**(`connect <n> <cmd...>` / `--tty`): 既存の
   `AWS-StartNonInteractiveCommand` / `AWS-StartInteractiveCommand` の
   まま、`command = "sudo su - ubuntu -c '<cmd>'"` でラップして ubuntu
   実行に揃える。内側コマンドはシングルクォートで安全化する
   (`shellQuote`: 埋め込み `'` を `'\''` にエスケープ)。
3. **ポートフォワード**: ユーザーコンテキストを持たないため変更しない。
4. ログインユーザーは `ubuntu` 固定とし、`--user` フラグは設けない
   (必要になれば後日追加)。

## Consequences

- `connect <n>` が直接 ubuntu のログインシェルに入り、単一 `exit` で
  切断される。手動の `sudo su - ubuntu` と二重 `exit` が不要になる。
- `connect <n> <cmd>` / `--tty` も ubuntu 実行となり、ssh 風の一貫性が
  得られる(ホーム・PATH・環境が ubuntu のもの)。
- **ssm-user の passwordless sudo に依存**する。運用側が
  `ssm-user` の sudo を無効化(前掲ドキュメントの手順)した場合、
  `sudo su - ubuntu` はパスワードを要求し失敗する。nakama の既定運用は
  この sudo を無効化しないため通常は問題にならない。
- サーバ側 / アカウント設定を変更しないため、**既存インスタンスにも
  nakama の入れ替えだけで適用**され、`update` / `edit` の SSM Run
  Command(root・非対話)には影響しない。
- ADR 0005 の「対話シェルは既定 `SSM-SessionManagerRunShell`」という
  記述を本 ADR が更新する(ADR 0005 は歴史的記録として残す)。

## Alternatives considered

- **サーバ側で ssm-user のログイン時に `exec sudo su - ubuntu`**
  (`/etc/profile.d` 等): SSM 経由のログインが常に ubuntu になる利点は
  あるが、root の Run Command(`update`/`edit`)を壊さないための厳密な
  ガードが必要でブラスト半径が大きく、userdata 変更のため新規
  インスタンスにしか効かない。局所性と安全性でクライアント側案に劣るため
  不採用。
- **アカウント全体の「Run As」セッション設定ドキュメントで既定ユーザーを
  ubuntu に**: nakama が管理しない共有設定に副作用が及ぶため不採用。
- **`exec` を付けない `sudo su - ubuntu`**: 単一 `exit` では切断できる
  ものの、ラッパシェルが1段残る。プロセスツリーとシグナル伝播を素直に
  するため `exec` を採用。
- **`--user` フラグでユーザーを可変化**: 現状 ubuntu 固定で十分。YAGNI。

## References

- ADR 0005 (`nakama aws connect` via Session Manager)
- <https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-getting-started-ssm-user-permissions.html>
- <https://docs.aws.amazon.com/systems-manager/latest/userguide/session-preferences-run-as.html>
