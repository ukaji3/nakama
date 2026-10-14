# ADR 0023: 削除済みのインスタンスを、バックアップから同じ名前で作り直す

- Status: Accepted
- Date: 2026-10-15
- Issue: [#96](https://github.com/ukaji3/nakama/issues/96)
- 関連: ADR 0004 (バックアップとインプレース復元)、ADR 0010 (インスタンスロール)

## Context

ADR 0004 の `restore` は、今あるインスタンスのルートボリュームを差し替える (インプレース) ことしかできなかった。`delete` の後もバックアップ (EBS スナップショット) は残り、名前を省いた `backup list` にも出る。しかし `restore` は SSM の instance-id が無いと「not found」で止まる。チュートリアル 06 §5 にも「削除した環境は nakama で復元できない」と書いていた。ADR 0004 は「別名のインスタンスとしての復元」を、`create` の流れをやり直す必要があって重いという理由で見送っていた。

作り直すのに足りない情報があった。バックアップのタグは名前・ラベル・取得時刻・メモだけで、インスタンスタイプ、CPU アーキテクチャ、種類 (code-server か bare か)、IAM ロールは記録していなかった。code-server のパスワードは作成時の user-data で `config.yaml` に書かれ、元の値は `delete` で SSM から消える。

調べて分かったこと:

- cloud-init の `cc_ssh` は once-per-instance で、`ssh_deletekeys` の既定は true。インスタンス ID が変わると、ホスト鍵を消して作り直す。鍵ファイルが残っていれば作り直さない (`cc_ssh.py`)。
- `runcmd` と `scripts_user` も once-per-instance。復元したボリュームで初めて起動すると、新しいインスタンス ID なので、新しいインスタンスの user-data が実行される。

## Decision

### モード

`nakama aws restore <name>` の動作は、SSM に instance-id の記録があるかどうかで決まる。

- **記録がある**: 今までどおりインプレースで復元する。作り直し用のフラグ (`--instance-type` / `--bare` / `--with-admin` / `--with-role`) はエラーにする (黙って無視しない)。記録はあるのに EC2 が無い (terminated か NotFound) 場合は、二つのインスタンスのリソースが混ざらないように、`delete --force` で片付けるよう案内してエラーにする。
- **記録が無く、その名前のバックアップがある**: 同じ名前のインスタンスを作り直す。

どちらも、読み取りだけの計画 (`PlanRestore`) を表示してから確認を取る。作り直しの確認には、タイプ・アーキ・種類・ロールと、「URL とパスワードが新しくなる」ことを示す。

### 作り方

最新の Ubuntu 26.04 で EC2 を起動し、`create` と同じリソース (SG、ENI、VPC origin、distribution、SSM のメタデータ) を作る。そのあと、インプレース復元と同じ手順でルートをバックアップから作ったボリュームに差し替え、起動に使ったボリュームを消す。土台の発見と SG・ENI の作成は `prepareInstanceNetwork` として `create` と共通にした。各リソースは作った直後に SSM に記録する。途中で失敗しても `delete` で片付けられ、記録ができた後なら、もう一度 `restore` するとインプレースで続きを行える。自動の巻き戻しはしない (`create` と同じ)。

### 復元したディスクの初回起動

user-data は `create` のスクリプトではなく、専用の cloud-config (`userdata.RenderRestore`) にする。

- **ソフトウェアは入れ直さない**。code-server、Kiro、apt のパッケージは、バックアップ時点のものがそのまま残る。
- **`ssh_deletekeys: false`**。バックアップのホスト鍵を残すので、`ssh setup` で作った known_hosts がそのまま使える。
- **code-server のパスワードだけを新しくする**。新しいパスワードを SSM に置き、初回起動スクリプトが `config.yaml` の `password:` の行だけを置き換えて code-server を再起動する。スクリプトは設定ファイルが無ければ何もしない。このため、最初に起動する新しいボリューム (後で捨てる) では何も起きず、差し替えたボリュームで初めて起動したときに動く。xtrace は使わない (CWE-532)。

### 設定の決め方

`backup create` (と `update` の自動バックアップ) は、取得元のインスタンスタイプ、アーキ、種類、IAM ロール名をタグ (`nakama:instance-type` / `architecture` / `kind` / `iam-role`) に記録する。作り直すときはこれを既定にする。

- `--instance-type` はタイプを上書きできる。ただしタグのアーキと違えば、何も作らずにエラーにする (別のアーキのインスタンスでは、ディスクの OS が起動しない)。
- タグの無い古いバックアップでは `--instance-type` を必須にし、bare なら `--bare` も要る。アーキを検査できないので、その旨を警告する。
- **IAM ロールは自動では付け直さない**。`--with-admin` / `--with-role` を指定したときだけ、`create` と同じ事前検証をしてから付ける。元にロールがあれば、そのことと付けるためのフラグを警告で示す。AdministratorAccess を、確認なしに付け直さないため。

### 変わるもの

URL (distribution を作り直すため)、code-server のパスワード、プライベート IP は新しくなる。Kiro Crew の画面を使っていた場合は、`kirocrew enable` のやり直しが要る。インスタンスは停止のまま終わる (インプレース復元と同じ)。

## Consequences

- `restore` に必要な IAM 権限は、`create` の権限を含むようになった (付録 A。`--bare` は `cloudfront:*` を使わない)。インプレース復元しか使わない利用者にとっては、doctor の判定が厳しくなる。
- 復元したインスタンスは、バックアップ時点のソフトウェアのまま動く。最新にするには `nakama aws update` を使う。
- 新しいタグは、これから取るバックアップにだけ付く。既存のバックアップは `--instance-type` の指定が要り、アーキの取り違えは nakama では防げない (起動しないので、`delete` してやり直す)。
- バックアップの暗号化は元のボリュームを引き継ぐ (ADR 0004 と同じ)。

## Alternatives considered

- **バックアップから AMI を登録して起動する** (`RegisterImage`): 差し替えの手間はなくなる。しかし `ec2:RegisterImage` / `DeregisterImage` の権限が増え、AMI が残っている間はスナップショットを消せない (`backup delete` が失敗する)。後片付けも増えるので採らなかった。
- **`create` の user-data をそのまま使う**: 起動のたびにソフトウェアを入れ直すことになり、バックアップ時点の状態が崩れる。ホスト鍵も作り直される。
- **元のパスワードを残す**: 元の値は `delete` で SSM から消えており、取り戻す手段が無い。ディスクの `config.yaml` の値を SSM に読み戻す方法もあるが、ディスクを読むために一度起動して Run Command を待つ必要があり、手順が増える。nakama は `open` でパスワードを自動入力するので、新しくしても利用者の手間はほとんど増えない。
- **IAM ロールをタグから自動で付け直す**: 手間は減るが、AdministratorAccess のような強い権限を、確認なしに付け直すことになる。
- **別の名前で復元する**: 今回の要件ではないので、対象外のままにした。

## References

- cloud-init のモジュール (`cc_ssh` の `ssh_deletekeys`、`runcmd`、`scripts_user` の頻度): <https://docs.cloud-init.io/en/latest/reference/modules.html>
- `cc_ssh.py` (鍵ファイルが既にあれば作り直さない): <https://github.com/canonical/cloud-init/blob/main/cloudinit/config/cc_ssh.py>
- EBS スナップショットからのボリュームの作成: <https://docs.aws.amazon.com/ebs/latest/userguide/ebs-restoring-volume.html>
