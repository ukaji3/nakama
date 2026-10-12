# ADR 0022: Windows で nakama.exe をダブルクリックすると、設定済みの PowerShell を開く

- Status: Accepted
- Date: 2026-10-13
- Issue: [#85](https://github.com/ukaji3/nakama/issues/85)

## Context

Windows には、Linux の `~/.local/bin` のような「最初から PATH に入っているユーザーのフォルダー」がない。そのため、nakama.exe を使うには、置き場所を手作業で PATH に追加する必要があった (チュートリアル第 1 章の旧手順: PowerShell で `[Environment]::SetEnvironmentVariable("Path", …, "User")` を実行し、ウィンドウを開き直す)。aws CLI を会社のプロキシの下で使うには、さらに `nakama aws cli shell` (ADR 0018) を実行する必要があった。

nakama.exe をエクスプローラーからダブルクリックすると、cobra の既定の動作 (mousetrap) で「This is a command line tool. You need to open cmd.exe and run it from there.」と表示され、5 秒後に `exit(1)` で閉じていた。cobra 1.8.1 の `preExecHook` は、親プロセスが `explorer.exe` なら引数の有無に関係なくこの動作をする。

調べて分かったこと:

- **Ctrl-C**: キーボードの Ctrl-C は、そのコンソールにつながっているすべてのプロセスに届く。Go のプログラムは、Windows では `signal.Notify(os.Interrupt)` をしていないと Ctrl-C / Ctrl-Break で終わる (os/signal のドキュメントの「Windows」の節)。`signal.Ignore` では防げない。ランタイムの `sigignore` は Windows では空の関数で、`wanted` のビットが落ちるだけなので、`ctrlHandler` から既定のハンドラーに回り、プロセスが終わる。`cli shell` はサブシェルの実行中に何も登録していなかったので、サブシェルの中で Ctrl-C を押すと nakama が終わるはずだった (コードから導いた結論。実機では未確認)。nakama が終わると、元のシェルとサブシェルが同じコンソールの入力を取り合う。

## Decision

### ダブルクリックの判定と起動するシェル

- `main` で cobra より先に判定する。条件は「引数がない」かつ「親プロセスの実体 (`QueryFullProcessImageName`) の名前が `explorer.exe`」(mousetrap と同じ基準)。引数付きでエクスプローラーから起動した場合 (引数付きのショートカットなど) は、今までどおり cobra に任せる。その場合は mousetrap の案内が出て 1 で終わる (wine で確認)。ショートカットからコマンドを実行できるようにするかどうかは、この ADR の範囲外とした。
- 起動するのは **Windows PowerShell** (`<システムディレクトリ>\WindowsPowerShell\v1.0\powershell.exe -NoLogo`)。サポート対象のすべての Windows に入っていて、チュートリアルの手順も PowerShell で書いているため。システムディレクトリは `GetSystemDirectory` で Windows に問い合わせる。PATH や `%SystemRoot%` からは探さないので、環境変数や nakama.exe と同じフォルダーにあるプログラムが代わりに起動されることはない。見つからない場合は `ComSpec` を使い、それも無ければ `cmd.exe` を使う。
- ダブルクリックしたときのウィンドウは Windows が開く。nakama はそのウィンドウを閉じずに、シェルを子プロセスとして同じコンソールで動かす。シェルはユーザーのホームフォルダーで開く (エクスプローラーの既定では、nakama.exe のフォルダーで開いてしまうため)。
- シェルの終了コードを nakama の終了コードにする。シェルを起動できなかった場合は、エラーを表示し、Enter を待ってから閉じる (表示がすぐ消えて読めない状態にしない)。

### シェルに渡す設定

- **PATH**: nakama.exe のフォルダー (`os.Executable` のシンボリックリンクを解決したもの) を **PATH の先頭**に入れる。Windows の規則に合わせ、変数名は大文字小文字を区別せず (通常の名前は `Path`)、区切りは `;` とする。同じフォルダーが PATH の後ろにもある場合は、大文字小文字、末尾の `\`、引用符の違いを無視して取り除き、PATH が伸び続けないようにする。`;` を含むフォルダー名は引用符で囲む。
- **このウィンドウの中だけ**: レジストリにもユーザー環境変数にも書かない (ADR 0018 の不変条件をそのまま保つ)。恒久的に PATH へ登録したい場合は、チュートリアルで利用者自身が登録する手順を案内する。
- **プロキシと CA**: `cli shell` と同じ検出 (`cliproxy.Detector`) を行い、同じ環境変数 (`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` / `AWS_CA_BUNDLE` / `NAKAMA_AWS_CLI_SHELL=1`) を入れる。表示も `cli shell` と同じ部品 (`printDetection`) を使う。
- **検出に失敗しても、シェルを開く**: オフライン、NTLM プロキシ、Ctrl-C による中断などで検出が失敗した場合は、理由と対処 (`cli shell` と同じ案内) を表示し、PATH だけを設定したシェルを開く。開いたばかりのウィンドウが、何もできないまま閉じるよりもよいため。nakama 自身はプロキシを自分で解決するので (ADR 0015)、このシェルでも nakama は使える。この場合、ネスト防止の印は付けない。そのため、同じウィンドウで `nakama aws cli shell` を実行して検出をやり直せる。
- 検出は最大 15 秒かかることがあるので、その間の Ctrl-C は検出の取り消しとして扱う。

### `cli shell` の変更

- サブシェルの PATH の先頭にも nakama.exe のフォルダーを入れる。`nakama aws cli shell` を絶対パスで実行した場合も、サブシェルの中では `nakama` と打てる。
- ネストしたときのメッセージは、ダブルクリックで開いたウィンドウの場合も分かる文言にする。

### Ctrl-C

- シェルの実行中は `signal.Notify(os.Interrupt)` で Ctrl-C / Ctrl-Break を受け取り、何もせずに捨てる (`runShell`)。Ctrl-C の処理はシェル自身が行う。ダブルクリックの場合は、プロセスの最初から最後まで登録したままにする (検出中の Ctrl-C は検出の取り消しに使う)。
- ウィンドウを閉じたとき (`CTRL_CLOSE_EVENT`) の動作は変えない。Notify しているのは `os.Interrupt` だけなので、ランタイムは閉じるイベントを既定のハンドラーに回し、nakama とシェルの両方が終わる。

## Consequences

- Windows での使い始めは「zip を展開して、好きな場所に置き、ダブルクリック」で済む。チュートリアル第 1 章をこの手順に書き換え、手作業での PATH の登録は任意の補足に回した。
- ダブルクリックで開いたウィンドウでは、aws CLI も会社のプロキシの下でそのまま動く (検出に成功した場合)。
- PATH はそのウィンドウの中だけで有効なので、スタートメニューなどから別に開いた PowerShell では `nakama` は見つからない。これは、レジストリを書かないという選択 (利用者の判断) の結果で、チュートリアルの「つまずきやすい点」に書いた。
- 起動するたびに検出が走るので、ウィンドウが使えるようになるまで数秒かかることがある (プローブのタイムアウトは 15 秒、Ctrl-C で飛ばせる)。
- `cli shell` のサブシェルで Ctrl-C を押しても、nakama が終わらなくなる。
- テスト: 純粋な関数 (PATH の結合、判定、シェルの選択) とランチャーの流れ (偽の検出とシェル) は、すべての OS で動かす。Windows では、本物の子プロセスで `runShell` の環境・フォルダー・終了コードを確かめる。また、自分のプロセスグループに Ctrl-Break を送り、`runShell` を通した場合は nakama が生き残ること、Notify しない場合は終わること (対照) を確かめる。開発時には、wine 上で `explorer.exe` という名前の小さな起動役から本物の nakama.exe を引数なしで起動し、判定、PATH、ネスト防止の印、検出に失敗したときの動作、シェルの終了コードを通しで確かめた (wine の PowerShell は代用品なので、cmd.exe への代替経路で確認)。実際の Windows のエクスプローラーからのダブルクリックは、自動テストでは確かめていない。

## Alternatives considered

- **PATH をユーザー環境変数に登録する** (初回に確認する、または自動): どの PowerShell からでも `nakama` が使えるようになる。ただし、レジストリとユーザー環境変数を書かないという方針 (ADR 0018、README、チュートリアル) を変えることになり、取り消しのコマンドも必要になる。利用者の判断で、今回は採らなかった。
- **cobra の mousetrap の文言だけを変える** (`cobra.MousetrapHelpText`): 「コマンドプロンプトから実行してください」と案内するだけになり、PATH の問題は残る。
- **新しいコンソールウィンドウ (`CREATE_NEW_CONSOLE`) や Windows Terminal (`wt.exe`) を起動する**: ダブルクリックした時点で Windows がウィンドウを開いているので、別のウィンドウを開くと 2 つになる。Windows Terminal は入っていない環境がある。
- **PowerShell 7 (`pwsh.exe`) を優先する**: 入っていない環境が多く、チュートリアルの手順は Windows PowerShell で確かめている。必要なら、ウィンドウの中で `pwsh` を起動すればよい (PATH とプロキシの設定は引き継がれる)。
- **検出に失敗したらシェルを開かない** (`cli shell` と同じ動作): ダブルクリックで開いたウィンドウが、何もできないまま閉じることになる。
- **`signal.Ignore(os.Interrupt)`**: Windows では効かない (Context を参照)。

## References

- ADR 0015 (プロキシの自動解決)、ADR 0018 (`cli shell`)
- os/signal のドキュメント「Windows」の節、`runtime/signal_windows.go` (`sigignore` は空実装)、`runtime/os_windows.go` (`ctrlHandler`)
- cobra 1.8.1 の `command_win.go` (`preExecHook`)、mousetrap v1.1.0 (`StartedByExplorer`)
