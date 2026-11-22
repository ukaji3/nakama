# nakama

`nakama` は、ブラウザで使える VS Code ([coder/code-server](https://github.com/coder/code-server)) の開発環境を、AWS 上に数コマンドで作って管理する CLI です。手元のパソコンでは重い開発 (Node.js のビルドなど) を、AWS のマシンに任せられます。

```bash
nakama aws login                       # ブラウザの AWS コンソールのセッションでサインイン
nakama aws init                        # 共通の土台 (VPC / NAT / WAF) を作る (何度実行してもよい)
nakama aws create --name dev01 --instance-type t3.medium
nakama aws open dev01                  # SSM 経由でブラウザで開く (パスワードの入力も自動)
```

初めての人は、[チュートリアル](docs/tutorial/index.html)から読んでください。

## 特徴

- **ブラウザだけで開発**: code-server に、手元の `localhost` から Session Manager (SSM) のポート転送でつなぐ。開発環境ごとに `http://code-server.<名前>.nakama.localhost:18888` のアドレスが付くので、いくつ開いてもポートを覚えなくてよい。開発環境にはインターネットから届く入口が無く、切れても自動でつなぎ直す。必要な開発環境にだけ、CloudFront 経由の HTTPS の入口 (IP の許可リスト制) を足せる (`nakama aws cloudfront add`)。手元に入れるのは nakama だけ
- **外向き通信の中継を選べる**: AWS が管理する NAT Gateway (新しい環境の既定。nakama が管理する OS が無い。常時課金) か、開発環境の電源に合わせて自動で止まる NAT インスタンス (止めればほぼ無料) を `init --nat` で選び、あとから切り替えられる
- **aws CLI も SSH も不要**: サインインも、シェル (`connect`)・ポート転送 (`tunnel`)・ssh 接続 (`ssh`) も nakama だけで動く。受信ポートは開けない
- **会社のプロキシにそのまま対応**: Windows のプロキシ設定と OS の証明書ストアを自動で使う
- **安全側の既定**: ブラウザは SSM 経由 (IAM で認証)。開発環境の security group は受信の規則を持たない。CloudFront は求めたときだけ作り、アクセスを IP の許可リスト制にする。パスワードは AWS 上で管理し、手元のディスクには残さない。`~/.aws/config` は書き換えない
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

**ソースから** (Go 1.26 以上): `go install github.com/ukaji3/nakama/cmd/nakama@latest`

**新しい版の確認**: `nakama version --check` (と `nakama aws doctor`) は GitHub (`api.github.com`) に問い合わせ、使っている版が最新かを表示します。古ければ、上と同じ手順で入れ替えてください。nakama が GitHub に問い合わせるのは、この 2 つのコマンドのときだけです。

## 主なコマンド

| コマンド | 用途 |
|---|---|
| `nakama aws login` / `profile` | サインイン / 複数の AWS アカウントの切り替え |
| `nakama aws init` | 共通の土台を作る。外向き通信の中継 (`--nat gateway` / `--nat instance`) の選択と切り替え、NAT インスタンスの点検と修理も行う |
| `nakama aws acl add` / `list` / `delete` / `prune` | CloudFront 経由 (`--internet`) のアクセスを許可する IP の管理 (`prune` は長く `acl add` していない IP をまとめて消す)。CloudFront を足した開発環境でだけ使う |
| `nakama aws create` | 開発環境を作る (`--bare` で code-server なしの軽量版、`--cloudfront` で CloudFront の入口も作る) |
| `nakama aws cloudfront add` / `delete` | 既存の開発環境に CloudFront の入口 (`open --internet` 用) を足す / 外す |
| `nakama aws list` / `info` | 一覧 / 詳細 |
| `nakama aws open` | ブラウザで開く (既定は SSM 経由で、`http://code-server.<名前>.nakama.localhost:18888` を開く。`--local-port` でローカルのポート、`--internet` で CloudFront 経由 (`cloudfront add` 済みのとき)) |
| `nakama aws connect` | シェルに入る、またはコマンドを 1 つ実行する |
| `nakama aws ssh setup` | 手元の VS Code・JetBrains・ssh からつなぐ設定を作る |
| `nakama aws tunnel` | ポート転送 (バックグラウンドで動き続け、切れてもつなぎ直す)。`open` / `kirocrew` の転送もここで確かめて止める |
| `nakama aws kirocrew` | Kiro Crew の画面をブラウザで開く (`open` と同じく既定は SSM 経由で、`http://kirocrew.<名前>.nakama.localhost:18888` を開く) |
| `nakama aws start` / `stop` / `delete` | 起動 / 停止 / 削除 |
| `nakama aws backup` / `restore` | バックアップ / 復元 |
| `nakama aws update` / `edit` | OS・code-server・AWS CLI・Kiro Crew の更新 / 大きさやディスクの変更 |
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

- **`create --with-admin`**: 開発環境に AdministratorAccess のロールを付けます。code-server にログインできる人は誰でも、AWS アカウントの管理者権限を使えるようになります。SSM の転送を張れる IAM の権限と、CloudFront を足したときは許可する IP を、最小限にしてください。
- **WebSocket を遮断するプロキシ**: この下では、`open`・`kirocrew` (SSM 経由)・`connect`・`ssh`・`tunnel` が使えません。プロキシの管理者に、WebSocket を許可してもらう必要があります。CloudFront を足せば (`cloudfront add`)、CloudFront 経由 (`--internet`) なら開けることがあります。
- **SSM 経由の速さ**: CloudFront 経由より遅く、初めて開くときは読み込みに数秒から十数秒かかることがあります。
- **`*.nakama.localhost` のアドレス**: Chrome・Edge・Firefox と、macOS 26 以降の Safari で開けます。それより前の macOS の Safari や、このアドレスを引けないツールでは、`--local-port <番号>` で `http://localhost:<番号>` を開いてください。手元の 18888 番が使われているときは、環境変数 `NAKAMA_ROUTER_PORT` で別の番号にできます。
- **拡張機能**: code-server は [Open VSX](https://open-vsx.org/) から拡張機能を入れます。Microsoft の Marketplace は、利用規約で Microsoft 製品専用とされているので使えません。

## しくみと料金

```
[ブラウザ] ──HTTP──> [手元の nakama ルーター (*.nakama.localhost:18888)] ──> [nakama の tunnel] ──SSM──> [EC2 (private subnet): code-server]
[ブラウザ] ──HTTPS──> [CloudFront + WAF (IP 許可リスト)] ──> [EC2]          (cloudfront add した環境で --internet のとき)
                                                                  │
[EC2] ──外向き通信──> [NAT Gateway または NAT インスタンス (t4g.nano)] ──> インターネット
```

- 開発環境は、インターネットから直接届かない場所 (private subnet) に置かれ、security group は受信の規則を持ちません。ブラウザは SSM のポート転送を通ってつながります。CloudFront の入口は、`create --cloudfront` か `nakama aws cloudfront add` で足した開発環境だけが持ち、`nakama aws cloudfront delete` で外せます。この機能より前に作った開発環境は CloudFront を持ったままなので、使わないなら `cloudfront delete` で外してください。
- 外向き通信の中継は、`nakama aws init --nat` で選びます。あとから切り替えることもできます。
  - **NAT Gateway** (`--nat gateway`、新しい環境の既定): AWS が管理するので、nakama が更新する OS がありません。常に動いていて、開発環境を全部止めても時間料金と固定 IP (EIP) の料金がかかり、通信量にも料金がかかります。外向きの IP は固定です。
  - **NAT インスタンス** (`--nat instance`): 小さな EC2 で、開発環境が 1 台でも動いている間だけ起動します。全部止めると、残る料金はディスクと WAF の基本料金くらいです (WAF の web ACL は、CloudFront を持つ開発環境が無くても残り、時間に応じた料金がかかります)。OS は nakama が管理します (初回起動時と `init` の修理のときに更新)。
- この機能より前に作った環境は、NAT インスタンスのままです。詳しくは[チュートリアル第 3 章](docs/tutorial/03-init.html#cost)を見てください。

## ドキュメント

- [チュートリアル](docs/tutorial/index.html): インストールから日々の運用、困ったときの対処まで
- [コマンド早見表](docs/tutorial/08-reference.html): 全コマンドとオプション、環境変数、ファイルの置き場所
- 開発者向けの資料: [要件定義](docs/requirements.md)、[設計](docs/design.md)、[設計判断の記録 (ADR)](docs/adr/)

## ライセンス

[MIT License](LICENSE)。同梱する第三者コンポーネントのライセンスは [THIRD-PARTY-NOTICES](THIRD-PARTY-NOTICES) にあります。
