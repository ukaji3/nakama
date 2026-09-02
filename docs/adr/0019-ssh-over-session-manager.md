# ADR 0019: ssh アクセスは SSM の stdio 中継 (ProxyCommand) で提供する

- Status: Accepted
- Date: 2026-09-03
- Issue: [#51](https://github.com/ukaji3/nakama/issues/51)

## Context

nakama の開発体験はブラウザの code-server に限られていた。一方で開発者の多くはデスクトップ版 VS Code (Remote-SSH)、JetBrains Gateway、素の `ssh`/`scp`/`rsync`、git over ssh を使いたい。これらはすべて **ssh トランスポート**を前提とする。

nakama の不変条件は「受信ポートを開けない・public IP を持たない・SSH 鍵を AWS に預けない」であり、素朴に sshd をインターネットに晒す選択肢はない。既存資産として (a) vendored session-manager-plugin に stdio 中継モード (`StandardStreamForwarding`) が取り込み済み、(b) SSM Run Command でインスタンス上のコマンドを実行する経路 (`update` / `edit` が使用) がある。

## Decision

### 中継: `AWS-StartSSHSession` + プロセス内 stdio 中継

`nakama aws ssh proxy <name>` は AWS 管理ドキュメント `AWS-StartSSHSession` (パラメータ `portNumber=22`) でセッションを開き、**自プロセスの stdin/stdout** をデータチャネルに中継する。OpenSSH の `ProxyCommand` から使う。SSM エージェントがインスタンス内部から localhost:22 に繋ぐため、セキュリティグループの受信許可も public IP も不要。

- **supervisor (ADR 0014) は使わない**: `ProxyCommand` プロセスの寿命は ssh クライアントが管理し、再接続も ssh 側の責務 (`ServerAliveInterval` 等)。1 プロセス 1 セッションが正しい
- **子プロセス (session-runner) を挟まない**: 中継は「この プロセスの実 stdin/stdout」を掴む必要がある。ProxyCommand プロセス自体が使い捨てなので、プラグインが `os.Exit` する設計との衝突もない
- プラグインはセッション開始時に `Starting session with SessionId: ...` を stdout に書く。ssh クライアントはサーバー識別文字列 (`SSH-2.0-...`) を探す過程で非該当行を読み飛ばす仕様 (RFC 4253 §4.2) のため無害であり、公式の `aws ssm start-session` を使う ProxyCommand レシピが成立しているのと同じ理由で機能する

### 認証: 公開鍵を authorized_keys に冪等投入

`nakama aws ssh setup <name>` が利用者の公開鍵 (`~/.ssh/id_ed25519.pub` などを自動検出、`--identity` で明示可) を SSM Run Command 経由で ubuntu の `authorized_keys` に追加する (既存なら追加しない)。

- 秘密鍵はローカルに留まり、AWS 側には**公開鍵のみ**が渡る
- 多行入力 (= 秘密鍵の誤指定) と OpenSSH 公開鍵形式でない入力を拒否する。エラーメッセージには鍵素材を出さない
- 代替として検討した **EC2 Instance Connect** (`SendSSHPublicKey` で 60 秒有効の一時鍵を push) は、鍵が短命という利点がある一方で ①インスタンスに `ec2-instance-connect` パッケージが必要 (既存インスタンスでは不在の可能性) ②呼び出し元に追加 IAM 権限が必要 ③接続ごとに API 呼び出しが増える。互換性を優先して不採用とした (将来 `--instance-connect` として追加する余地は残す)

### ssh config: 生成はするが `~/.ssh/config` は書き換えない

スニペットは `~/.config/nakama/ssh/<name>.conf` (0600) に書き出し、利用者に `Include <path>` の 1 行追加を案内する。`~/.aws/config` を変更しない不変条件 (ADR 0011/0016) と同じ思想で、**ユーザーの ssh 設定も nakama は書き換えない**。`ProxyCommand` には `os.Executable()` の絶対パスを埋め込み (PATH 非依存)、空白を含むパスは引用する (Windows)。

## Consequences

- デスクトップ VS Code / JetBrains Gateway / `ssh` / `scp` / `rsync` / git over ssh がそのまま使える。nakama は「ブラウザ IDE 提供ツール」から「エディタを選ばない開発ボックス」になる
- ファイル転送の専用コマンドは不要になった (`scp`/`rsync -e ssh` が使える)
- 接続断からの復帰は ssh クライアントの設定に委ねられる (`tunnel` / `connect` の自動再接続とは非対称)
- code-server を使わない構成 (`create --bare`、Issue #52) が意味を持つようになる
- 呼び出し元に必要な IAM は `connect` と同じ (`ssm:StartSession` 系 + Run Command 系)。新規権限は不要

## Alternatives considered

1. **sshd を CloudFront/NLB 経由で公開** — 受信経路を開ける設計変更になり、不変条件に反する
2. **`tunnel` でローカル 22 番に転送して `ssh -p` する** — 動くが、ポート衝突・常駐デーモン・ssh config の煩雑さが増える。ProxyCommand は 1 行で済む
3. **EC2 Instance Connect** — 上記のとおり互換性の理由で不採用
4. **`~/.ssh/config` への直接追記** — 利用者の設定ファイルを書き換える不可逆な副作用を持つため不採用

## References

- ADR 0009 (connect の ubuntu ログイン方針)、ADR 0014 (再接続 supervisor — ssh proxy では使わない)、ADR 0017 (tunnel)
- RFC 4253 §4.2 (Protocol Version Exchange: 識別文字列より前の行は読み飛ばす)
- AWS Systems Manager User Guide: `AWS-StartSSHSession` / Session Manager 経由の SSH
