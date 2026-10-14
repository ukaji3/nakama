# nakama

`nakama` は、ブラウザで使える VS Code ([coder/code-server](https://github.com/coder/code-server)) の開発環境を、AWS 上に数コマンドで作って管理する CLI です。手元のパソコンでは重い開発 (Node.js のビルドなど) を、AWS のマシンに任せられます。

```bash
nakama aws login                       # ブラウザの AWS コンソールのセッションでサインイン
nakama aws init                        # 共通の土台 (VPC / NAT / WAF) を作る (何度実行してもよい)
nakama aws acl add                     # 自分のグローバル IP を許可する
nakama aws create --name dev01 --instance-type t3.medium
nakama aws open dev01                  # ブラウザで開く (パスワードの入力も自動)
```

初めての人は、[チュートリアル](docs/tutorial/index.html)から読んでください。

## 特徴

- **ブラウザだけで開発**: code-server に CloudFront 経由の HTTPS でつなぐ。手元に入れるのは nakama だけ
- **止めればほぼ無料**: 外向き通信の中継 (NAT インスタンス) は、開発環境の電源に合わせて自動で止まる
- **aws CLI も SSH も不要**: サインインも、シェル (`connect`)・ポート転送 (`tunnel`)・ssh 接続 (`ssh`) も nakama だけで動く。受信ポートは開けない
- **会社のプロキシにそのまま対応**: Windows のプロキシ設定と OS の証明書ストアを自動で使う
- **安全側の既定**: アクセスは IP の許可リスト制。パスワードは AWS 上で管理し、手元のディスクには残さない。`~/.aws/config` は書き換えない
- **Kiro CLI と Kiro Crew を最初から導入** (`create --no-kiro` で省略)

## インストール

[Releases](https://github.com/ukaji3/nakama/releases) から、OS とアーキテクチャに合うアーカイブを取得します (Linux / macOS は `tar.gz`、Windows は `zip`。macOS には Intel と Apple Silicon の両方で動く `universal` もあります)。`checksums.txt` で SHA-256 を確かめてください。

**Windows**: zip を展開し、`nakama.exe` を好きな場所に置いて**ダブルクリック**します。PowerShell のウィンドウが開き、そのウィンドウでは `nakama` とだけ打てば動きます。会社のプロキシの設定も自動で入ります。レジストリやユーザー環境変数は変更しません。

**macOS / Linux** (例: Linux amd64、`X.Y.Z` は版):

```sh
VERSION=X.Y.Z
curl -fsSLO "https://github.com/ukaji3/nakama/releases/download/v${VERSION}/nakama_${VERSION}_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/ukaji3/nakama/releases/download/v${VERSION}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing
tar -xzf "nakama_${VERSION}_linux_amd64.tar.gz" nakama && sudo install -m 0755 nakama /usr/local/bin/
```

**ソースから** (Go 1.25 以上): `go install github.com/ukaji3/nakama/cmd/nakama@latest`

## 主なコマンド

| コマンド | 用途 |
|---|---|
| `nakama aws login` / `profile` | サインイン / 複数の AWS アカウントの切り替え |
| `nakama aws init` | 共通の土台を作る。NAT インスタンスの点検と修理も行う |
| `nakama aws acl add` / `list` / `delete` | ブラウザからのアクセスを許可する IP の管理 |
| `nakama aws create` | 開発環境を作る (`--bare` で code-server なしの軽量版) |
| `nakama aws list` / `info` | 一覧 / 詳細 |
| `nakama aws open` | ブラウザで開く |
| `nakama aws connect` | シェルに入る、またはコマンドを 1 つ実行する |
| `nakama aws ssh setup` | 手元の VS Code・JetBrains・ssh からつなぐ設定を作る |
| `nakama aws tunnel` | ポート転送 (バックグラウンドで動き続ける) |
| `nakama aws kirocrew` | Kiro Crew の画面をブラウザで開く |
| `nakama aws start` / `stop` / `delete` | 起動 / 停止 / 削除 |
| `nakama aws backup` / `restore` | バックアップ / 復元 |
| `nakama aws update` / `edit` | OS と code-server の更新 / 大きさやディスクの変更 |
| `nakama aws setup rerun` | 初期セットアップ (スクリプト、dotfiles) のやり直し |
| `nakama aws doctor` | 環境の自己診断 (読み取り専用) |
| `nakama aws cli shell` | aws CLI を会社のプロキシの下で使えるシェルを開く (Windows) |

オプションは `nakama aws <コマンド> --help` で確かめられます。全コマンドの一覧は[チュートリアル第 8 章](docs/tutorial/08-reference.html)にあります。

## 前提条件

- **リージョン**: 東京 (`ap-northeast-1`) 固定です。
- **権限**: `nakama aws login` でサインインするには、AWS 管理ポリシー `SignInLocalDevelopmentAccess` が必要です。SSO や環境変数など、ほかの方法で用意した AWS の資格情報も使えます。
  - 機能ごとに必要な IAM 権限は、[要件定義の付録 A](docs/requirements.md#付録-a-機能別-iam-権限マトリクス) にあります。今の資格情報で足りているかは、`nakama aws doctor` で確かめられます。
- **手元の OS**: Linux / macOS / Windows に対応しています。

## 注意

- **`create --with-admin`**: 開発環境に AdministratorAccess のロールを付けます。code-server にログインできる人は誰でも、AWS アカウントの管理者権限を使えるようになります。許可する IP は最小限にしてください。
- **WebSocket を遮断するプロキシ**: この下では、`connect`・`ssh`・`tunnel` が使えません。プロキシの管理者に、WebSocket を許可してもらう必要があります。
- **拡張機能**: code-server は [Open VSX](https://open-vsx.org/) から拡張機能を入れます。Microsoft の Marketplace は、利用規約で Microsoft 製品専用とされているので使えません。

## しくみと料金

```
[ブラウザ] ──HTTPS──> [CloudFront + WAF (IP 許可リスト)] ──> [EC2 (private subnet): code-server]
                                                                  │
[EC2] ──外向き通信──> [NAT インスタンス (t4g.nano)] ──> インターネット
```

- 開発環境は、インターネットから直接届かない場所 (private subnet) に置かれます。入口は CloudFront だけです。
- 外向き通信の中継には、常時課金の NAT Gateway ではなく、小さな NAT インスタンスを使います。NAT インスタンスは、開発環境が 1 台でも動いている間だけ起動します。全部止めると、残る料金はディスクと WAF の基本料金くらいです。詳しくは[チュートリアル第 3 章](docs/tutorial/03-init.html#cost)を見てください。

## ドキュメント

- [チュートリアル](docs/tutorial/index.html): インストールから日々の運用、困ったときの対処まで
- [コマンド早見表](docs/tutorial/08-reference.html): 全コマンドとオプション、環境変数、ファイルの置き場所
- 開発者向けの資料: [要件定義](docs/requirements.md)、[設計](docs/design.md)、[設計判断の記録 (ADR)](docs/adr/)

## ライセンス

[MIT License](LICENSE)。同梱する第三者コンポーネントのライセンスは [THIRD-PARTY-NOTICES](THIRD-PARTY-NOTICES) にあります。
