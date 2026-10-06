# ADR 0021: connect のコマンド実行は出力を枠で包んで受け取り、標準入力を読まない

- Status: Accepted
- Date: 2026-10-06
- Issue: [#70](https://github.com/ukaji3/nakama/issues/70)、[#75](https://github.com/ukaji3/nakama/issues/75)、[#77](https://github.com/ukaji3/nakama/issues/77)

## Context

`nakama aws connect <名前> <コマンド>` には不具合が 2 つあった。どちらも v0.17.1 から存在し、vendor した session-manager-plugin 1.2.835.0 の作りに由来する。

1. **標準入力が EOF になるとセッションが終わる (#70)**: plugin は NonInteractiveCommands と InteractiveCommands のセッションを、どちらも通常のシェル用の `Standard_Stream` (ShellSession) に振り分ける (`datachannel/streaming.go`)。ShellSession は手元の標準入力を読み続け、EOF になると `io.EOF` で終わる。そのため、標準入力が `/dev/null` やパイプのときは、リモートの出力が届く前に `Error: EOF` (exit 1) で終わっていた。リモートのコマンドは最後まで実行されるので、失敗だと判断して再実行すると 2 回実行される。標準入力を `/dev/null` にした対話シェルでは、セッションも残った。
2. **終了コードと stderr が伝わらない (#75)**: plugin は受け取ったデータを種類に関係なく stdout に書き、セッションが終わると必ず `os.Exit(0)` する。`connect dev false` も 0 で終わり、リモートの stderr は手元の stdout に混ざっていた。

調べて分かったこと:

- **agent はコマンド実行モードで入力をコマンドに渡さない**: amazon-ssm-agent の `InputStreamMessageHandler` (`agent/session/shell/shell_unix.go`) は、NonInteractiveCommands では受け取ったバイトのうち制御シグナルに当たるもの (Ctrl-C など) だけを処理し、残りは捨てる。コマンドの標準入力は `/dev/null` になる。
- **agent の `separateOutputStream=true` は大きな出力が途切れる**: このモードでは stdout / stderr / 終了コードが別々に送られるが、developer に snap で入っている agent 3.3.4793.0 では、コマンドが終わってから約 2 秒で送信を打ち切る (`PrepareToCloseChannel`)。実測では、168,894 バイトは全部届き、348,894 バイトは約 300 KB で途切れた。agent 3.3.4851.0 の「Fix non-interactive session big file transfer issues」(commit 24323cf2) で直っている。
- **既定のモード (`separateOutputStream` なし) は大きな出力も全部届く**: agent は出力をいったんファイルに書き、送信の完了を確かめながら送る。実測では、1.3 MB と 5 MB の出力が全部届いた (5 MB は sha256 も一致)。ただし、stdout と stderr が 1 本にまとめられ、終了コードは送られない。

## Decision

session-runner の子プロセスに、セッションの種類ごとの動作 (mode) を payload で渡す。

| mode | 使う場面 | 子プロセスの動作 |
|---|---|---|
| (空) raw | tunnel、ssh proxy | 従来どおり (plugin をそのまま使う) |
| `command` | `connect <名前> <コマンド>` | 下記のラッパーと commandSession |
| `terminal` | 対話シェル、`--tty` | ShellSession のまま。標準入力が端末でなければ差し替える |

### コマンド実行 (`command`)

- **ラッパー**: リモートのコマンドを、次の小さなシェルスクリプトで包む (`commandWrapper`)。コマンドを `set -m` の背景ジョブ (`( コマンド ) &`) として**独自のプロセスグループ**で起動し、stdout と stderr を `mktemp -d` で作った本人だけが読めるディレクトリのファイルに書く。コマンドが終わったら、それぞれの長さを書いた見出しの後に本文を出し、最後に終了コードを出す。見出しは `\n@@nakama-frame <目印> o|e|x <数>\n` の形で、目印は実行のたびに作る 32 文字の乱数 (`newCommandMarker`) なので、コマンドの出力が見出しを偽装することはできない。ディレクトリは終了時に消す。コマンドは `su - ubuntu` のログインシェル (bash) で動くので、ラッパーも bash を前提にする (`set -m` のジョブ制御)。
- **Ctrl-C (#77)**: 手元で Ctrl-C を押すと、plugin が制御バイトを agent に送り、agent は `execCmd.Signal` で `sudo` 1 プロセスにだけ SIGINT を送る。sudo→su と伝わり、su は直接の子 (ラッパーの bash) にだけ SIGTERM を送って約 2 秒後に SIGKILL する (実測)。素朴に `( コマンド )` を実行すると、su がラッパーを殺し、ラッパーは出力を出す前に死に、その下のコマンドは孤児として残る。そこでラッパーは INT/TERM/HUP を trap し、コマンドのプロセスグループに SIGINT を転送 (無視するコマンド向けに 1 秒後 SIGKILL のバックストップ) してから、収集済みの出力と終了コードを出す。`wait` はトラップで早期復帰するので、`kill -0` でコマンドの実際の終了まで待ち直し、終了コードはコマンドが選んだ値 (素のコマンドは SIGINT で 130) になる。ラッパー自身のジョブ制御通知は `exec 2>/dev/null` で消す (コマンドの stderr は別に捕まえている)。su の「Session terminated, killing shell…」は su 自身の出力で、stderr に残る。
- **受け取り**: 子プロセスでは、`Standard_Stream` を nakama の `commandSession` に差し替える (`session.Register` は同名の登録を上書きするので、vendor した plugin は変更しない)。`commandSession` は受け取ったデータを `frameDecoder` に流して、stdout / stderr / 終了コードに戻す。見出しより前の出力 (ログイン時のメッセージなど) は stderr に出す。plugin が出す `Starting session…` などの表示も stderr に回し、stdout にはコマンドの出力だけが出るようにする。
- **標準入力**: 読まない (agent が捨てるため)。Ctrl-C / Ctrl-\ / Ctrl-Z は、従来どおりシグナルとしてリモートに送る。
- **止まって見えることへの案内 (#77)**: このモードでは出力がコマンドの終了後にしか届かないので、`kiro-cli login` のように入力や確認コードを待つコマンドは止まって見える。手元の標準入力と stderr がどちらも端末のとき、コマンドが 10 秒経っても何も出さなければ、`commandSession` が stderr に 1 行の案内を出す (「終了後に出力が出る。入力やコードを待つなら `--tty` で」)。最初の出力が届いた時点、またはセッション終了で取り消す。端末でないとき (スクリプト) やすぐ終わるコマンドでは出さない。
- **終了コードの受け渡し**: 子プロセス自身の終了コードは、nakama の内部の都合 (1 = 子のエラー、74 = 切断。ADR 0014) に使っているので、リモートの終了コードは別のパイプで親に返す。unix では fd 4、Windows では継承ハンドルで渡し、番号を環境変数 `NAKAMA_SESSION_STATUS_HANDLE` で伝える。子は受け取ったらすぐ環境変数を消し、descriptor に close-on-exec を付ける。
- **nakama の終了コード** (ssh と同じ約束):
  - リモートのコマンドの終了コード (0〜255) を、そのまま nakama の終了コードにする。このときは何も表示しない。
  - 終了コードを受け取れずにセッションが終わったとき (中断、途中で切れた転送、壊れた見出し) は 255 にして、理由を表示する。見出しの最後は終了コードなので、途中で切れた転送は必ずここに当たり、成功に見えることはない。
  - 子プロセスが失敗したときも 255 にする (元の終了コードはメッセージに含める)。
  - リモートが 74 を返しても、切断とは扱わない (ADR 0014 で残っていた衝突はこれで解消する)。`--retry` を付けたときの再接続は、終了コードを受け取る前にセッションが切れた場合だけ行う。

### 対話シェルと `--tty` (`terminal`)

- 標準入力が端末なら、今までと同じ (ShellSession が端末を cbreak モードにしてキー入力を読む)。
- 端末でなければ (`< file`、パイプ、閉じた標準入力)、終わらないパイプに差し替える。手元の入力が尽きたら Ctrl-D を送り (最後の行が改行で終わっていなければ、先に改行を送る)、リモートのシェルが終わるのを待つ。行頭の `~` は二重にして、ShellSession の `~.` などのエスケープが入力データで誤って働かないようにする (ShellSession は `~~` を `~` に戻す)。書き込み側のファイルはプロセスが終わるまで閉じない (GC で閉じると EOF になってしまうため)。
- Windows では対象外とする。Windows 版の ShellSession は標準入力ではなくコンソールの API でキー入力を読むので、入力をリダイレクトできない。
- PTY のセッションには終了コードを返す仕組みが Session Manager に無い。`--tty` は従来どおり 0 で終わる。

## Consequences

- `connect <名前> <コマンド>` が、スクリプトから ssh と同じように使えるようになった。終了コードで成否を判断でき、stdout と stderr も分かれる。
- **出力はコマンドが終わってから届く**。これは以前と同じで、既定のモードでは agent が出力をファイルに貯めてから送るため。実測では、「1 行出して 5 秒待ってもう 1 行」というコマンドの 2 行が、修正前も修正後も約 6 秒後にまとめて届いた。長く動くコマンドの途中経過を見たいときは `--tty` を使う。
- 出力はインスタンスの一時ディレクトリに一度書かれる。agent も自分のファイルに書くので、大きな出力ではディスクを 2 倍使う。
- コマンドの標準入力は `/dev/null` で、手元からデータを渡すことはできない (以前から同じ。agent が入力を捨てる)。ファイルを送るときは `ssh` / `scp` (ADR 0019) を使う。
- 引数は空白でつないで、リモートのシェルが解釈する (ssh と同じ)。`connect dev echo "it's"` は、リモートでは `echo it's` になるので構文エラーになる。修正前も同じ構文エラーだったが、修正前は終了コードが 0 だったので気づけなかった。今は 255 で、エラーの内容が stderr に出る。
- 終了コード 255 は、リモートのコマンドが返した 255 と区別できない (ssh と同じ)。
- 対話シェルにファイルやパイプで入力を流せるようになった (`connect dev < steps.sh`)。入力はリモートの端末に表示される。
- tunnel と ssh proxy は raw mode なので変わらない。
- テストは実プロセスで行う: 親子のハンドルの受け渡し (テストバイナリを偽の session-runner として起動)、vendor した本物の ShellSession にエスケープした入力を流す確認、本物の `sh` / `bash` でラッパーを実行して解析器と往復させる確認。

## Alternatives considered

1. **agent の `separateOutputStream=true` を使う**: ラッパーが要らず、出力も逐次届く。ただし、今配布されている agent では約 300 KB を超える出力が途切れる (Context)。途中で切れても終了コードは届いてしまうので、出力の欠けに気づけない。agent 3.3.4851.0 以上が十分に行き渡ったら、そちらに戻すことを考える。
2. **vendor した plugin にパッチを当てる**: ShellSession の EOF 処理や表示を直接変えるやり方。パッチの再適用の手間が増えるうえ、`session.Register` で差し替えれば同じことができるので採らない。
3. **Run Command (`AWS-RunShellScript`) で実行する**: 終了コードと stdout / stderr を分けて取れる。ただし実行結果が SSM に最大 30 日残り、`GetCommandInvocation` で読める出力は 24,000 文字まで、Ctrl-C も届かない。connect で必要な `ssm:StartSession` 以外の権限も要る。
4. **手元の標準入力を読んでリモートに送る**: agent がコマンド実行モードの入力を捨てるので、意味が無い。
5. **終了コードを子プロセスの終了コードで返す**: 1 と 74 は nakama の内部で使っているので、リモートの終了コードと区別できなくなる。

## 追記: Ctrl-C とヒント (#77)

最初の版 (PR #76) では、Ctrl-C で出力が消え、リモートのコマンドが孤児として残っていた。原因は、ラッパーをそのまま `( コマンド )` で実行していたため、su がラッパーを殺すと出力前に死に、下のコマンドが残ること。上の「Ctrl-C」の項のとおり、コマンドを独自のプロセスグループで起動し、trap でグループごと止めてから出力を出すようにした。あわせて、長く無出力のときに `--tty` を促すヒントを追加した。mega と developer で、素のコマンド (130)・SIGINT を処理するコマンド (そのコマンドの終了コード)・SIGINT を無視するコマンド (強制終了) のいずれでも、出力が残り孤児が出ないことを確かめた。

## References

- ADR 0005 (connect の plugin 取り込み)、ADR 0009 (ubuntu への切り替え)、ADR 0014 (自動再接続と終了コード 74)、ADR 0015 (OS 別のハンドル受け渡し)、ADR 0019 (ssh over Session Manager)
- session-manager-plugin 1.2.835.0: `src/datachannel/streaming.go` (セッション種別の振り分け)、`src/sessionmanagerplugin/session/shellsession/` (`handleKeyboardInput`、`handleEscapeSequence`、`Stop`)、`src/sessionmanagerplugin/session/session.go` (`Register`)
- amazon-ssm-agent: `agent/session/shell/shell.go` (`processCommandsWithExec`、`processCommandsWithOutputStreamSeparate`)、`agent/session/shell/shell_unix.go` (`InputStreamMessageHandler`、`StartCommandExecutor`)、`agent/session/datachannel/datachannel.go` (`PrepareToCloseChannel`、3.3.4851.0 の `WaitForBufferToDrain`)、RELEASENOTES 3.3.4851.0
