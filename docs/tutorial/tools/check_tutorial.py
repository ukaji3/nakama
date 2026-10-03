#!/usr/bin/env python3
"""チュートリアル HTML の整合性を検査する。

標準ライブラリのみを使用する。docs/tutorial/ を対象に次を検査する:

1. 内部リンク切れ (ファイル・アンカーの両方)
2. 画像の実体 / alt 属性 / figure ごとの figcaption
3. 本文に登場する nakama コマンドとフラグが実仕様に存在するか
4. <code> の外に生で書かれたコマンド (検査漏れの温床)
5. HTML の構造 (タグの対応、必須要素、重複 id、見出し階層)
6. 表のヘッダー関連付け (th の scope)
7. 全ページで目次が揃っているか、aria-current が自分のページを指しているか
8. 生成した図がすべてどこかのページで使われているか
9. (NAKAMA_BIN を指定した場合) 検査用のコマンド定数が実バイナリと一致するか

使い方:
    python3 docs/tutorial/tools/check_tutorial.py
    NAKAMA_BIN=/path/to/nakama python3 docs/tutorial/tools/check_tutorial.py
終了コード 0 = 問題なし、1 = 問題あり。

この検査は視覚的な問題 (コントラスト比、レイアウト崩れ) を見ないので、
図の重なりは tools/gen_diagrams.py 側の検査で、配色は目視で確認する。
"""

from __future__ import annotations

import html.parser
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)  # docs/tutorial

PAGES = [
    "index.html",
    "01-install.html",
    "02-proxy-cert.html",
    "03-init.html",
    "04-create.html",
    "05-use.html",
    "06-operate.html",
    "07-troubleshoot.html",
    "08-reference.html",
]

# 実バイナリの --help から確認したコマンド (第 1〜8 章の記述と照合する)
KNOWN_COMMANDS = {
    "nakama",
    "nakama version",
    "nakama completion",
    # サブコマンドを束ねるグループ (単体では実行しないが実在する)
    "nakama aws",
    "nakama aws acl",
    "nakama aws backup",
    "nakama aws cli",
    "nakama aws profile",
    "nakama aws setup",
    "nakama aws ssh",
    "nakama aws login",
    "nakama aws profile save",
    "nakama aws profile list",
    "nakama aws profile load",
    "nakama aws profile unload",
    "nakama aws profile delete",
    "nakama aws init",
    "nakama aws acl add",
    "nakama aws acl list",
    "nakama aws acl delete",
    "nakama aws create",
    "nakama aws list",
    "nakama aws info",
    "nakama aws open",
    "nakama aws start",
    "nakama aws stop",
    "nakama aws connect",
    "nakama aws ssh setup",
    "nakama aws ssh proxy",
    "nakama aws tunnel",
    "nakama aws tunnel start",
    "nakama aws tunnel status",
    "nakama aws tunnel suggest",
    "nakama aws tunnel restart",
    "nakama aws tunnel stop",
    "nakama aws edit",
    "nakama aws update",
    "nakama aws backup create",
    "nakama aws backup list",
    "nakama aws backup note",
    "nakama aws backup delete",
    "nakama aws restore",
    "nakama aws delete",
    "nakama aws doctor",
    "nakama aws setup rerun",
    "nakama aws cli shell",
}

# 実バイナリの --help から確認したフラグ (コマンド名 -> 使用可能なフラグ)
KNOWN_FLAGS = {
    "nakama": {"--help", "--output", "-o", "--verbose", "-v"},
    "nakama aws login": {"--remote", "--name"},
    "nakama aws profile save": {"--name"},
    "nakama aws create": {
        "--name", "--instance-type", "--bare", "--preset", "--setup", "--dotfiles",
        "--code-server-version", "--with-admin", "--with-role", "--show-password",
        "--no-kiro",
    },
    "nakama aws list": {"--show-nat"},
    "nakama aws info": {"--show-password"},
    "nakama aws open": {"--no-auth", "--print-url", "--browser", "--timeout"},
    "nakama aws connect": {"--tty", "--retry"},
    "nakama aws ssh setup": {"--identity", "--no-key", "--port"},
    "nakama aws ssh proxy": {"--port"},
    "nakama aws tunnel": {"-L", "--forward"},
    "nakama aws tunnel start": {"-L", "--forward"},
    "nakama aws edit": {"--ebs", "--instance-type", "--yes", "-y"},
    "nakama aws update": {"--keep", "--yes", "-y"},
    "nakama aws backup create": {"--label", "--note", "--keep"},
    "nakama aws backup note": {"--label", "--note"},
    "nakama aws backup delete": {"--label"},
    "nakama aws restore": {"--label", "--yes", "-y"},
    "nakama aws delete": {"--force", "--yes", "-y"},
    "nakama aws setup rerun": {"--setup", "--dotfiles", "--preset"},
    "nakama aws cli shell": {"--shell"},
}

# 値を取るフラグ (次のトークンはその値なので、フラグ名として検査しない)
VALUE_FLAGS = {
    "--name", "--instance-type", "--preset", "--setup", "--dotfiles",
    "--code-server-version", "--with-role", "--label", "--note", "--keep",
    "--ebs", "--retry", "--identity", "--port", "--timeout", "--browser",
    "--shell", "--output", "-o", "-L", "--forward",
}

# 名前の後ろに任意の外部コマンドを取るコマンド (それ以降は nakama の
# フラグではないので検査を打ち切る)
PASSTHROUGH_COMMANDS = {"nakama aws connect"}

problems: list[str] = []


def problem(page: str, msg: str) -> None:
    problems.append(f"{page}: {msg}")


class Collector(html.parser.HTMLParser):
    """id / リンク / 画像 / タグの入れ子を集める。"""

    VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input",
            "link", "meta", "param", "source", "track", "wbr"}

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.ids: list[str] = []
        self.links: list[str] = []
        self.images: list[dict[str, str]] = []
        self.stack: list[str] = []
        self.unbalanced: list[str] = []
        self.figure_depth = 0
        self.figures = 0
        self.figcaptions = 0
        # figure ごとの figcaption 数 (ページ合計では相殺されてしまうため)
        self.figcaption_per_figure: list[int] = []
        self.headings: list[int] = []
        self.th_total = 0
        self.th_scoped = 0
        self.aria_current: list[str] = []
        self.toc_links: list[str] = []
        self.in_toc = False
        self.h1_count = 0
        self.title = ""
        self._in_title = False
        self.has_lang = False
        self.has_charset = False
        self.has_viewport = False

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        a = {k: (v or "") for k, v in attrs}
        if tag == "html" and a.get("lang"):
            self.has_lang = True
        if tag == "meta":
            if a.get("charset"):
                self.has_charset = True
            if a.get("name") == "viewport":
                self.has_viewport = True
        if tag == "title":
            self._in_title = True
        if "id" in a:
            self.ids.append(a["id"])
        if tag == "a":
            if a.get("href"):
                self.links.append(a["href"])
                if self.in_toc:
                    self.toc_links.append(a["href"])
            if a.get("aria-current"):
                self.aria_current.append(a.get("href", ""))
        if tag == "nav" and "toc" in a.get("class", ""):
            self.in_toc = True
        if tag == "img":
            self.images.append({"src": a.get("src", ""), "alt": a.get("alt", "\0")})
        if tag == "figure":
            self.figures += 1
            self.figure_depth += 1
            self.figcaption_per_figure.append(0)
        if tag == "figcaption" and self.figure_depth > 0:
            self.figcaptions += 1
            if self.figcaption_per_figure:
                self.figcaption_per_figure[-1] += 1
        if tag == "th":
            self.th_total += 1
            if a.get("scope"):
                self.th_scoped += 1
        if tag in ("h1", "h2", "h3", "h4", "h5", "h6"):
            self.headings.append(int(tag[1]))
        if tag == "h1":
            self.h1_count += 1
        if tag not in self.VOID:
            self.stack.append(tag)

    def handle_endtag(self, tag: str) -> None:
        if tag == "title":
            self._in_title = False
        if tag == "nav" and self.in_toc:
            self.in_toc = False
        if tag == "figure":
            self.figure_depth -= 1
        if tag in self.VOID:
            return
        if not self.stack:
            self.unbalanced.append(f"</{tag}> without an opening tag")
            return
        if self.stack[-1] == tag:
            self.stack.pop()
        elif tag in self.stack:
            # 途中のタグが閉じられていない
            while self.stack and self.stack[-1] != tag:
                self.unbalanced.append(f"<{self.stack.pop()}> was not closed before </{tag}>")
            if self.stack:
                self.stack.pop()
        else:
            self.unbalanced.append(f"</{tag}> does not match <{self.stack[-1]}>")

    def handle_data(self, data: str) -> None:
        if self._in_title:
            self.title += data


def strip_code_text(src: str) -> str:
    """本文からコマンドの引用部分 (code / pre) だけを取り出す。

    <pre><code>...</code></pre> のように入れ子になっている場合、抽出した
    文字列に内側のタグが残って行頭判定が壊れるため、タグを落とす。
    """
    blocks = re.findall(r"<(?:code|pre)[^>]*>(.*?)</(?:code|pre)>", src, re.S)
    return "\n".join(re.sub(r"<[^>]+>", "", b) for b in blocks)


def check_commands(page: str, src: str) -> None:
    """引用されたコマンド名とフラグが実仕様に存在するかを確認する。"""
    text = html.unescape(strip_code_text(src))
    for raw in text.splitlines():
        line = raw.strip()
        # コメント部分を除去
        line = re.sub(r"\s+#.*$", "", line)
        # "nakama" の直後が空白か行末のものだけがコマンド行 (nakama.exe や
        # nakama_0.17.0_windows_amd64.zip のようなファイル名を除く)
        if not re.match(r"^nakama(\s|$)", line):
            continue
        tokens = line.split()
        # 最長一致でコマンド名を決める (nakama aws tunnel start ... の形)
        cmd = None
        for n in (4, 3, 2, 1):
            cand = " ".join(tokens[:n])
            if cand in KNOWN_COMMANDS:
                cmd = cand
                break
        if cmd is None:
            problem(page, f"unknown command: {line}")
            continue
        allowed = KNOWN_FLAGS.get(cmd, set()) | KNOWN_FLAGS["nakama"]
        rest = tokens[len(cmd.split()):]
        skip_next = False
        for i, tok in enumerate(rest):
            if skip_next:
                skip_next = False
                continue
            if tok.startswith("-"):
                flag, eq, _ = tok.partition("=")
                if flag not in allowed:
                    problem(page, f"unknown flag {flag} for `{cmd}`: {line}")
                elif flag in VALUE_FLAGS and not eq:
                    skip_next = True
                continue
            # 非フラグの引数。外部コマンドを取るコマンドでは、インスタンス名の
            # 後ろは実行されるコマンドなので検査を打ち切る。
            if cmd in PASSTHROUGH_COMMANDS and i + 1 < len(rest):
                break


def check_plaintext_commands(page: str, src: str) -> None:
    """<code> の外に生で書かれた nakama コマンドを検出する。

    コマンドは必ず <code> で囲む方針なので、素のテキストに現れたものは
    書き忘れ (かつ検査対象から漏れる) を意味する。
    """
    body = re.sub(r"<(code|pre)[^>]*>.*?</\1>", "", src, flags=re.S)
    body = re.sub(r"<[^>]+>", " ", body)
    for m in re.finditer(r"nakama\s+aws\s+[a-z][a-z-]*", html.unescape(body)):
        problem(page, f"command outside <code>: {m.group(0)}")


def check_known_commands_against_binary(binary: str) -> None:
    """KNOWN_COMMANDS が実バイナリと一致しているかを確認する (任意)。

    NAKAMA_BIN に実行ファイルを指定したときだけ動く。定数の陳腐化を
    検出するための仕組み。
    """
    import subprocess

    def sub_commands(path: list[str]) -> set[str]:
        out = subprocess.run([binary, *path, "--help"], capture_output=True, text=True)
        found: set[str] = set()
        in_block = False
        for line in out.stdout.splitlines():
            if line.startswith("Available Commands:"):
                in_block = True
                continue
            if in_block:
                if not line.strip():
                    break
                found.add(line.split()[0])
        return found

    stack = [["aws"]]
    actual = {"nakama", "nakama aws", "nakama version", "nakama completion"}
    while stack:
        path = stack.pop()
        for sub in sub_commands(path):
            if sub in ("help", "completion"):
                continue
            full = path + [sub]
            actual.add("nakama " + " ".join(full))
            stack.append(full)
    missing = actual - KNOWN_COMMANDS
    extra = KNOWN_COMMANDS - actual
    for m in sorted(missing):
        problem("KNOWN_COMMANDS", f"the binary has `{m}` but the checker does not know it")
    for e in sorted(extra):
        if e.count(" ") >= 1:  # "nakama" 単体などは対象外
            problem("KNOWN_COMMANDS", f"the checker lists `{e}` but the binary does not have it")


def main() -> None:
    parsed: dict[str, Collector] = {}
    sources: dict[str, str] = {}

    for page in PAGES:
        path = os.path.join(ROOT, page)
        if not os.path.exists(path):
            problem(page, "file is missing")
            continue
        with open(path, encoding="utf-8") as fh:
            src = fh.read()
        sources[page] = src
        c = Collector()
        c.feed(src)
        c.close()
        parsed[page] = c

        # -- 構造
        if c.unbalanced:
            for u in c.unbalanced:
                problem(page, f"tag nesting: {u}")
        if c.stack:
            problem(page, f"unclosed tags at EOF: {c.stack}")
        if not c.has_lang:
            problem(page, "<html> lacks lang")
        if not c.has_charset:
            problem(page, "missing <meta charset>")
        if not c.has_viewport:
            problem(page, "missing viewport meta")
        if c.h1_count != 1:
            problem(page, f"has {c.h1_count} <h1> elements, want exactly 1")
        if not c.title.strip():
            problem(page, "empty <title>")
        dupes = {i for i in c.ids if c.ids.count(i) > 1}
        if dupes:
            problem(page, f"duplicate id attributes: {sorted(dupes)}")
        if 'class="skip"' not in src:
            problem(page, "missing skip link")
        if "style.css" not in src:
            problem(page, "stylesheet is not linked")

        # -- 見出し階層 (h1 から始まり、1 段ずつ深くなること)
        if c.headings:
            if c.headings[0] != 1:
                problem(page, f"the first heading is h{c.headings[0]}, want h1")
            for prev, cur in zip(c.headings, c.headings[1:]):
                if cur > prev + 1:
                    problem(page, f"heading level jumps from h{prev} to h{cur}")

        # -- 表のヘッダー関連付け
        if c.th_total and c.th_scoped != c.th_total:
            problem(page, f"{c.th_total - c.th_scoped} of {c.th_total} <th> lack scope")

        # -- 図 (figure ごとに figcaption が 1 つ)
        for i, n in enumerate(c.figcaption_per_figure, start=1):
            if n != 1:
                problem(page, f"figure #{i} has {n} <figcaption>, want exactly 1")
        for img in c.images:
            if img["alt"] == "\0":
                problem(page, f"img without alt: {img['src']}")
            elif len(img["alt"]) < 20:
                problem(page, f"img alt is too short to describe the figure: {img['src']}")
            target = os.path.normpath(os.path.join(ROOT, img["src"]))
            if not os.path.exists(target):
                problem(page, f"image not found: {img['src']}")

        # -- 目次
        if c.toc_links[:len(PAGES)] != PAGES:
            problem(page, f"table of contents does not list all chapters in order: {c.toc_links[:len(PAGES)]}")
        if c.aria_current != [page]:
            problem(page, f"aria-current should mark only this page, got {c.aria_current}")

        check_commands(page, src)
        check_plaintext_commands(page, src)

    # -- リンク検査 (ファイルとアンカー)
    for page, c in parsed.items():
        for href in c.links:
            if href.startswith(("http://", "https://", "mailto:")):
                continue
            target, _, anchor = href.partition("#")
            if not target:
                if anchor and anchor not in c.ids:
                    problem(page, f"anchor #{anchor} does not exist on this page")
                continue
            path = os.path.normpath(os.path.join(ROOT, target))
            if not os.path.exists(path):
                problem(page, f"link target not found: {href}")
                continue
            if anchor and target in parsed and anchor not in parsed[target].ids:
                problem(page, f"anchor #{anchor} does not exist in {target}")

    # -- 生成した図がすべて使われているか
    img_dir = os.path.join(ROOT, "img")
    used = {os.path.basename(i["src"]) for c in parsed.values() for i in c.images}
    for name in sorted(os.listdir(img_dir)):
        if name.endswith(".svg") and name not in used:
            problem("img/", f"{name} is generated but not referenced by any page")

    if binary := os.getenv("NAKAMA_BIN"):
        check_known_commands_against_binary(binary)

    if problems:
        print(f"{len(problems)} problem(s) found:\n", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)
    print(f"checked {len(parsed)} pages: no problems found")


if __name__ == "__main__":
    main()
