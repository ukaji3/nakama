#!/usr/bin/env python3
"""チュートリアル HTML の整合性を検査する。

標準ライブラリのみを使用する。docs/tutorial/ を対象に次を検査する:

1. 内部リンク切れ (ファイル・アンカーの両方)
2. 画像の実体があるか / alt 属性があるか / figcaption があるか
3. 本文に登場する nakama コマンドが実バイナリの仕様に存在するか
4. HTML の構造 (タグの対応、必須要素、重複 id)
5. 全ページで目次が揃っているか、aria-current が自分のページを指しているか
6. 生成した図がすべてどこかのページで使われているか

使い方:
    python3 docs/tutorial/tools/check_tutorial.py
終了コード 0 = 問題なし、1 = 問題あり。
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
        if tag == "figcaption" and self.figure_depth > 0:
            self.figcaptions += 1
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

        # -- 図
        if c.figures != c.figcaptions:
            problem(page, f"{c.figures} <figure> but {c.figcaptions} <figcaption>")
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

    if problems:
        print(f"{len(problems)} problem(s) found:\n", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)
    print(f"checked {len(parsed)} pages: no problems found")


if __name__ == "__main__":
    main()
