# ADR 0024: AWS CLI を AWS 公式のインストーラーで ubuntu ユーザーに入れ、update で AWS CLI と Kiro Crew を更新する

- Status: Accepted
- Date: 2026-10-20
- Issue: [#101](https://github.com/ukaji3/nakama/issues/101)
- 関連: ADR 0006 (`nakama aws update`)

## Context

EC2 の作成時 (user-data) に、AWS CLI v2 を snap (`snap install aws-cli --classic`) で入れ、root 用に `/usr/local/bin/aws` へリンクしていた。Ubuntu の apt の `awscli` は v1 なので使わない。

snap のアプリは setuid の snap-confine を通って起動する。Kiro Crew のエージェントは OS のサンドボックス (unprivileged user namespace) の中で動き、そこでは setuid が効かない。この Ubuntu で、権限の昇格を禁じた状態 (`setpriv --no-new-privs`) で snap の aws を実行すると「snap-confine is packaged without necessary permissions」で失敗した。AWS 公式のインストーラーで入れた aws は、同じ条件で動いた。

調べて分かったこと:

- AWS CLI のユーザーガイドは、Linux のインストール方法としてインストールスクリプト (`https://awscli.amazonaws.com/v2/install.sh`) を推奨している。スクリプトは `uname -m` から x86_64 / aarch64 の zip を選び、PGP 署名を検証してからインストールする。gpg が無いと、警告を出して検証を飛ばす。
- 既定のインストール先は `$XDG_DATA_HOME/aws-cli` (既定は `~/.local/share/aws-cli`) で、`aws` のリンクは `$XDG_BIN_HOME` (既定は `~/.local/bin`) に作られる。スクリプトを再実行すると、古い版は更新され、最新なら何もしない (実測)。
- `kirocrew update` は、インストールの形 (wheel、git、外部管理) を判定して更新する。対話の確認は無い。`kirocrew restart` は、サービスが動いていれば `systemctl restart` する (system のユニットには sudo を使う)。成功すると、ダッシュボードのアクセストークン付き URL を表示する。
- SSM の GetCommandInvocation が返す stdout は、先頭の 24,000 字だけである。update のスクリプトはすべての出力 (xtrace と apt) を stdout に出し、最後の行で再起動の要否を返していた。そのため、出力が長いと最後の行が切れ、再起動が必要でも行わなかった。

## Decision

### インストール

- AWS 公式のインストールスクリプトを、ubuntu ユーザーで実行する (`internal/awscli` の `nakama_install_awscli`)。`XDG_DATA_HOME=~/.local` と `XDG_BIN_HOME=~/.local/bin` を渡して、`~/.local/aws-cli` に入れ、`~/.local/bin/aws` にリンクする。CPU の種類の判定と署名の検証は、スクリプトに任せる。
- 署名の検証を飛ばさせないため、gpg が無ければ失敗にする。テンプレートの apt に `gnupg` を加える。
- スクリプトは、ファイルに落としてから実行する (`curl | bash` は使わない)。途中で切れたダウンロードを実行しないためである。
- `~/.bashrc` に `~/.local/bin` の PATH の行を足す。Kiro の導入と同じ行なので、どちらが先でも 1 行だけになる。
- create (code-server 版と bare 版) では snap と `/usr/local/bin/aws` を使わない。

### root からの呼び出し

nakama が root で行う処理 (作成時のパスワードの取得と版の記録、update の版の記録、復元の初回起動) は、`nakama_aws` を通して AWS CLI を呼ぶ。

- 公式版は ubuntu が書き換えられる場所にある。サンドボックスの中のエージェントも ubuntu として動くので、root で実行すると、差し替えられたときに root の権限を取られる。そこで root では実行せず、ubuntu として実行する。
- 環境は空にし (`env -i`)、`HOME=/nonexistent`、`AWS_CONFIG_FILE=/dev/null`、`AWS_SHARED_CREDENTIALS_FILE=/dev/null` を渡す。ubuntu の AWS の設定は効かず、インスタンスロールで動く。
- 公式版が無いディスク (この変更より前に作ったインスタンスや、そのバックアップからの復元) では、`/snap/bin/aws` を使う。
- root 用の `/usr/local/bin/aws` は作らない。`sudo aws` は使えなくなる。

### `nakama aws update`

- **AWS CLI**: 公式版があれば、インストールスクリプトを再実行して更新する。失敗は update の失敗にする (code-server と同じ)。公式版が無ければ (snap) 何もしない。
- **Kiro Crew**: `~/.local/bin/kirocrew` があれば、ubuntu で `kirocrew update` を実行し、成功したら `kirocrew restart` を実行する。出力は捨てる (restart がトークン付きの URL を表示するため)。失敗しても update は失敗にせず、警告を出す。OS と code-server の更新はその時点で終わっているからである。
- **出力**: 詳しい出力はインスタンスの `/var/log/nakama-update.log` に書き、Run Command の stdout には結果の印 (`NAKAMA_REBOOT_REQUIRED`、`NAKAMA_AWSCLI`、`NAKAMA_KIROCREW`) だけを出す。失敗したときは、ログの末尾を stderr に出す。
- 結果に `aws_cli_version` と `kiro_crew` (`updated` か `failed`) を加え、要約の行に表示する。

### 既存のインスタンス

nakama は移行しない (snap はそのまま、update で公式版を新しく入れることもしない)。利用者が手で移行する。同じ場所 (`~/.local/aws-cli`、`~/.local/bin`) に入れれば、以後の update が更新する。

## Consequences

- 新しいインスタンスでは、AWS CLI が Kiro Crew のサンドボックスの中からも動く。CPU の種類は気にしなくてよい。
- `sudo aws` は使えない。root で AWS CLI が要る利用者のスクリプトは、ubuntu として実行する必要がある (`sudo -u ubuntu aws …`、または `nakama_aws` と同じ形)。
- AWS CLI は、snap と違って自動では更新されない。`nakama aws update` で更新する。
- update は Kiro Crew を再起動するので、動いているセッションは切れる。
- 既存のインスタンスは snap のままで、サンドボックスの中からは AWS CLI を使えない。
- インストールスクリプトの埋め込みの署名の鍵は、2027-07-01 に期限が切れる。AWS がスクリプトを更新すれば、nakama は毎回取得するので追随する。

## Alternatives considered

- **zip を直接ダウンロードして `./aws/install -i ~/.local/aws-cli -b ~/.local/bin`**: 依頼の例の方法。CPU の種類で URL を選ぶ処理と、署名の検証 (鍵の埋め込みと期限の管理) と、更新時の `--update` の判定を nakama が持つことになる。公式のスクリプトはそれらを持ち、AWS が保守している。インストール先は同じ (`--install-dir` / `--bin-dir` と同じ場所) にした。
- **`--system` で `/usr/local/aws-cli` に入れる**: root の所有なので、root から実行しても安全で、`sudo aws` も使える。しかし依頼は ubuntu のホームへの導入で、ubuntu (とエージェント) が自分で更新できなくなる。
- **root でも公式版をそのまま実行する**: 単純だが、ubuntu が書き換えられるファイルを root で実行することになる。
- **update で snap を消して公式版を入れる (既存のインスタンスも移行する)**: 利用者の判断で、既存のインスタンスは手で移行することにした。
- **`aws update` コマンドで更新する**: AWS CLI のユーザーガイドが案内する更新方法。実体はインストールスクリプトの再実行と同じで、nakama は導入と同じ断片で更新できる。

## References

- AWS CLI のインストールと更新: <https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html>
- AWS CLI のインストールスクリプト: <https://awscli.amazonaws.com/v2/install.sh>
- GetCommandInvocation (`StandardOutputContent` は先頭 24,000 字): <https://docs.aws.amazon.com/systems-manager/latest/APIReference/API_GetCommandInvocation.html>
