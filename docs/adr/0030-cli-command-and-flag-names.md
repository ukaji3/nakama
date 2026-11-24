# ADR 0030: コマンドとフラグの名前を、開く先とする事に合わせて揃える

- Status: Accepted
- Date: 2026-11-24
- Issue: [#129](https://github.com/ukaji3/nakama/issues/129)
- 関連: ADR 0005・0009・0014・0021 (`connect`。この ADR で `shell` に改名)、ADR 0012・0027 (`open`。この ADR で `code` に改名)、ADR 0020・0028 (`kirocrew enable|disable` と `cloudfront add|delete`。この ADR で `--kirocrew` にまとめる)、ADR 0017 (サブコマンド名と重なるインスタンス名の予約)

## Context

`nakama aws open` は code-server を開くコマンドだが、名前は「開く」という動作しか表していない。Kiro Crew を開くコマンドは、開く先のサービスの名前 (`kirocrew`) である。`nakama aws connect` はインスタンスのシェルに入る (コマンドの実行もする) が、名前からは何につなぐのかが分からない。`tunnel` も `ssh` もインスタンスにつなぐ。利用者から、`open` を `code` に、`connect` を `shell` に変える要望があった (Issue #129)。

あわせて v0.29.1 の全コマンドのヘルプ (50 件) を読み比べ、次の不揃いを見つけた。

- **名前の渡し方**: インスタンスを扱うコマンドのうち、`create` だけが名前を `--name` で受ける。ほかはすべて位置引数で、`create` のフラグの多くを持つ `restore` も `restore <name>` である。
- **`--name` の意味**: `create` ではインスタンスの名前、`login` と `profile save` では保存するプロファイルの別名である。後の 2 つの Usage の行は、すでに `[--name <alias>]` だった。
- **CloudFront を作る・消す動詞**: `cloudfront add|delete` (code-server の distribution) と `kirocrew enable|disable` (Kiro Crew の distribution) の 2 組がある。`kirocrew enable` は code-server の distribution を前提にする。`kirocrew` には「開く」と「作る・消す」が同居し、`enable` と `disable` はインスタンス名として予約されている (`tunnel` の `start` などと同じ理由。ADR 0017)。
- **`setup rerun`**: `setup` はサブコマンドが 1 つ (`rerun`) だけのグループである。`setup` という語は `ssh setup` と、`create` などの `--setup` にもある。
- **確認**: 消すと戻せない操作のうち、`delete`・`cloudfront delete`・`acl delete all`・`acl prune`・`restore` は、消すものを示して確認し、`--yes` で省略できる。`backup delete` (スナップショットを消す) と `kirocrew disable` (distribution を消す) は確認しない。
- **`--tty`**: `shell` を ssh に似せるなら、PTY を求めるフラグには ssh と同じ `-t` がほしい。
- **`cli shell`**: Windows で aws CLI 用のサブシェルを手元に開くコマンドで、新しい `shell` と語が重なる。
- **ヘルプの表記**: `create --name` の説明 `[a-z0-9][a-z0-9-]*` は、実際の規則 (`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`。2〜32 文字、末尾はハイフン以外) と違う。フラグの説明のうち `list --show-nat` だけが小文字で始まる。

古い名前を残す仕組みとして、cobra v1.10.2 と pflag v1.0.9 の次の動きを、ソースで確かめた。

- `cobra.Command.Deprecated` を設定したコマンドは、ヘルプの一覧と補完から外れる (`IsAvailableCommand`)。実行すると、`Command "<名前>" is deprecated, <文>` を `OutOrStderr` に書く (`command.go` の `execute`)。書き先を指定しなければ stderr である。
- pflag の `MarkDeprecated` を付けたフラグは、ヘルプから外れる。使うと pflag が `Flag --<名前> has been deprecated, <文>` を書き、cobra が解析の後にそれを `OutOrStderr` に出す (`ParseFlags` の `flagErrorBuf`)。

## Decision

1. **コマンドの名前**: `open` を `code` に、`connect` を `shell` に、`setup rerun` を `provision` に変える。`provision` は、内部の型 `Provision` やログ `/var/log/nakama-provision.log` と同じ語である。
2. **Kiro Crew の CloudFront**: `kirocrew enable|disable` を `cloudfront add|delete <name> --kirocrew` に移す。
   - `cloudfront add --kirocrew` は、code-server の分を作り (すでにあれば何もしない)、続けて Kiro Crew の分を作る。後半が失敗したら、code-server の分はできていることと、同じコマンドで続きから進むことをエラーに書く。
   - `cloudfront delete --kirocrew` は、Kiro Crew の分だけを消す。distribution があれば、消すものを示して確認する。`--kirocrew` の無い `delete` は、今までどおり両方を消す。
   - これで、`code` と `kirocrew` はどちらも開くだけのコマンドになり、CloudFront を作る・消すのは `cloudfront` だけになる。
3. **名前の渡し方とフラグ**: `create` は名前を位置引数で受ける (`create <name>`)。`login` と `profile save` の別名は `--alias` にする。
4. **古い名前**: v0.30.x の間は、非表示の非推奨として同じ引数とフラグで動かし、新しい名前を stderr で知らせる。stdout と `-o json` の出力は変えない。
   - 古いコマンド (`open`・`connect`・`setup rerun`・`kirocrew enable|disable`) は cobra の `Deprecated` を使う。`open`・`connect`・`setup rerun` は、新しいコマンドの構築関数で作ったものの名前だけを替える。`kirocrew enable|disable` は、元の処理のまま残す (確認もしない)。
   - 古いフラグ (`create --name` と、`login` / `profile save` の `--name`) は pflag の `MarkDeprecated` を使う。新旧が違う値で同時に与えられたら、AWS を呼ぶ前に (`login` ではブラウザを開く前に) エラーにする。
   - 古い名前は v0.31.0 以降に、別の Issue で消す。
5. **`backup delete` の確認**: 消すもの (ラベル、スナップショット、大きさ、作成時刻、メモ) を示して確認し、`-y` / `--yes` で省略できるようにする。標準入力が EOF なら、何も消さずにエラーにする。消すのは、示したスナップショットそのものである。
6. **`shell` と `cli shell`**: `shell --tty` に `-t` を足す。`cli shell` は名前を変えず、両方のヘルプに違いを 1 行ずつ書く。
7. **doctor**: 権限の確認の項目名を、新しいコマンドの名前にする (`code`・`shell`・`cloudfront --kirocrew`・`update / provision`)。
8. **変えないもの**:
   - ルーターのホスト名 `code-server.<名前>.nakama.localhost`。サービスの名前で `kirocrew.` と対になっており、v0.29 で作られたブックマークも保てる。
   - service 層の `Connect`・`connect_session*.go`、syslog のタグ `nakama-connect`、隠しコマンド `session-runner`。これらは Session Manager のセッションの仕組みの名前で、コマンドの名前ではない。
   - `code` と `kirocrew` が共用する、ブラウザで開くための部品の名前 (`openFlags` など。この open は動詞)。
   - インスタンスの設定ファイルに書く印 (`# BEGIN nakama kirocrew (managed by …kirocrew enable…)` など) と、Run Command のコメント。消すときに、この文字列で探すため。
9. **ヘルプの表記**: `create` のヘルプに名前の規則を書く。フラグの説明は大文字で始める (sshd のようなプログラムの名前は例外)。どちらもテストで固定する。

## Consequences

- 利用者は新しい名前を覚える。古い名前は v0.30.x の間は動くので、スクリプトと手順書は順に書き換えられる。古い名前を使うと stderr に 1 行が増える。`connect <name> <command>` では、リモートのコマンドの stderr の前に出るので、stderr を読むスクリプトは影響を受けうる。
- 次の 2 つは、古い名前を使っていても挙動が変わる。v0.30.0 のリリースノートに Breaking change として書く。
  - `backup delete` が確認するようになり、スクリプトには `--yes` が要る。`cloudfront delete --kirocrew` も確認する (古い `kirocrew disable` は確認しないまま)。
  - `doctor -o json` の `feature` の値が変わる。
- `enable` と `disable` は、非推奨のサブコマンドを消すまでインスタンス名として予約したままになる。消したあとは予約を外せる。`tunnel` のサブコマンド名の予約 (ADR 0017) は残る。
- README・設計・要件・チュートリアルは新しい名前で書く。古い名前との対応表を、README とチュートリアル第 8 章に置く。過去の ADR の本文は変えず、Status に注記を足す。

## 確認

- 単体テスト: 古い名前が新しいものと同じ引数・フラグ・ヘルプの文を持ち、ヘルプの一覧に出ず、知らせが stderr だけに出ること (本物の `os.Stdout` と `os.Stderr` を差し替えて確かめる) と、`connect` が `<name>` の後ろをすべてリモートのコマンドとして扱うこと (`deprecated_test.go`)。`create` の位置引数と `--name` の組み合わせ (`aws_create_test.go`)。`--alias` と `--name` の組み合わせ、食い違いで sign-in も identity の解決も始めないこと (`aws_login_test.go`、`aws_profile_test.go`)。`cloudfront add --kirocrew` の順序・途中の失敗・JSON と YAML の出力、`cloudfront delete --kirocrew` の確認と後片付け (`aws_cloudfront_test.go`)。`kirocrew enable|disable` が元の処理のまま知らせを出すこと (`aws_kirocrew_test.go`)。`backup delete` の確認、`--yes`、答えが無いとき、知らないラベル (`aws_backup_test.go`、`backup_test.go`)。`-t` (`aws_shell_test.go`)。フラグの説明が大文字で始まること (`flag_usage_test.go`)。`Deprecated` の設定、`cloudfront delete --kirocrew` の確認の分岐、`EnableKiroCrewWeb` の呼び出し、`--show-nat` の説明の大文字を元に戻すと、テストが失敗することも確かめた。
- 実機 (2026-11-24、一時インスタンス `t4g.medium`、Ubuntu 26.04、Kiro Crew 入り。このブランチを Issue #130 の gorilla/websocket の更新に載せたバイナリ。Linux の手元から直接の接続。ブラウザの代わりに、1 回限りのページを読んでフォームを送るスクリプトを使った)
  - `create <name>` でインスタンスができた。`--name` を違う値で重ねたときと、名前が無いときは、AWS を呼ばずにエラーになった。
  - `code --local-port` で code-server に自動でサインインし、セッションの Cookie と画面 (200) を得た。古い `open` も同じに動き、知らせは stderr だけに出て、`-o json` の stdout はそのまま JSON として読めた。`kirocrew --local-port` はトークンでサインインした。
  - `shell <name> <cmd>` は stdout と stderr を分けて返し、終了コード 3 で終わった。古い `connect` は知らせを出して終了コード 4 を返した。`shell -t <name> tty` は `/dev/pts/N` を、`-t` なしは `not a tty` を返した。パイプから流した対話シェルも動いた。
  - `provision --setup` が手順を実行した。古い `setup rerun` は知らせを出して同じことをした。フラグが無ければエラーになった。
  - `backup delete` は、`n` で中止し、標準入力が無ければエラーで何も消さず、知らないラベルでは確認の前に止まり、`-y` で消した。
  - `doctor -o json` の項目名は `capability: code`・`shell`・`cloudfront --kirocrew`・`update / provision` で、古い名前は無かった。
  - `cloudfront add <name> --kirocrew` は約 25 分で終わり、2 つの distribution は WAF を通って応答した (code-server は 302、Kiro Crew は 401)。`code --internet` は自動でサインインした。`cloudfront delete --kirocrew` は、`n` で中止し、`y` で Kiro Crew の分 (distribution、nginx のサイト、code-server の drop-in) だけを消し、code-server の distribution は 302 を返し続けた。続けて `cloudfront delete --yes` と `delete` で片付けた。
  - 確かめていないこと: ルーター経由で開くこと (手元で動いている利用者のルーターを置き換えないよう `--local-port` を使った。ルーターの変更はエラーのページの文言だけ)。`kirocrew --internet` のサインイン (代わりのスクリプトでは、改名の前のバイナリでも同じく 401 になった。本物のブラウザでは確かめていない)。会社のプロキシの配下、Windows、macOS。

## Alternatives considered

1. **古い名前をすぐ消す**: 利用者のスクリプトと手順書が、更新した時点で壊れる。不採用。
2. **古い名前を恒久的な別名 (cobra の `Aliases`) にする**: ヘルプに 2 つの名前が出続け、どちらを使えばよいかが分からない。新しい名前も知らせない。不採用。
3. **ルーターのホスト名も `code.<名前>.nakama.localhost` にする**: v0.29 で作られたブックマークが使えなくなり、code-server のサインインの Cookie (ホストごと) も引き継がれない。ホスト名はサービスの名前で、`kirocrew.` と対になっている。不採用。
4. **service 層の名前も変える**: syslog のタグを変えると、利用者のログの検索条件が壊れる。Session Manager のセッションの仕組みの名前で、コマンドの名前ではない。不採用。
5. **`kirocrew enable|disable` を `kirocrew cloudfront add|delete` にする**: 入れ子が深くなり、`cloudfront` がインスタンス名として予約される。CloudFront を扱うコマンドも 2 か所に分かれたままになる。不採用。
6. **`setup rerun` を `setup <name>` にする**: `nakama aws setup dev01 --setup ./s.sh` のように、`setup` が重なる。不採用。
7. **`cli shell` も改名する**: `cli` の下にあり、aws CLI を設定するコマンドとして読める。手順書を書き換えてもらうほどの理由が無い。ヘルプで区別する。不採用。
8. **`backup delete` / `backup note` の `--label` を位置引数にする**: `restore --label` (省略時は最新) と同じ形で揃っている。不採用 (Issue #129 の範囲外)。

## References

- [spf13/cobra v1.10.2](https://github.com/spf13/cobra/tree/v1.10.2): `command.go` (`Deprecated`、`IsAvailableCommand`、`Print` と `OutOrStderr`、`ParseFlags` の `flagErrorBuf`)
- [spf13/pflag v1.0.9](https://github.com/spf13/pflag/tree/v1.0.9): `flag.go` (`MarkDeprecated`)
- Issue [#129](https://github.com/ukaji3/nakama/issues/129)
