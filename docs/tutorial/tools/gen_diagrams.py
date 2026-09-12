#!/usr/bin/env python3
"""nakama チュートリアル用の SVG 図解を生成する。

標準ライブラリのみを使用する (追加依存なし)。実行すると
docs/tutorial/img/ に SVG を書き出す。図を直接編集するのではなく、
このスクリプトを編集して再生成すること。

    python3 docs/tutorial/tools/gen_diagrams.py

アクセシビリティ:
- すべての図に role="img" と <title> / <desc> を付ける
- 色だけで情報を伝えない (文字ラベルを併記する)
- 本文テキストとの色コントラストは 4.5:1 以上を目安にする
"""

from __future__ import annotations

import html
import os
import sys
import unicodedata
from dataclasses import dataclass, field

# はみ出し・重なり検出の記録 (main が終了コードに反映する)
OVERFLOWS: list[str] = []
COLLISIONS: list[str] = []


def text_width(text: str, font_size: int) -> float:
    """描画幅の概算。全角は font_size、半角は約 0.55 倍として数える。"""
    total = 0.0
    for ch in text:
        if unicodedata.east_asian_width(ch) in ("W", "F", "A"):
            total += font_size
        else:
            total += font_size * 0.55
    return total


def check_fit(text: str, font_size: int, avail: float, where: str) -> None:
    """テキストが領域に収まるかを検査し、超えていれば記録する。"""
    w = text_width(text, font_size)
    if w > avail:
        OVERFLOWS.append(f"{where}: {w:.0f}px > {avail:.0f}px  |  {text}")

# ---------------------------------------------------------------- palette

FONT = "Segoe UI, Meiryo, Hiragino Kaku Gothic ProN, sans-serif"
MONO = "Consolas, Menlo, monospace"

INK = "#111827"        # 本文色
MUTED = "#4b5563"      # 補助テキスト (白背景で 7.5:1)
LINE = "#374151"       # 矢印・枠線

STYLES = {
    # name: (fill, stroke, text)
    "plain": ("#ffffff", "#9ca3af", INK),
    "step": ("#eef2ff", "#4338ca", INK),
    "aws": ("#fff7ed", "#c2410c", INK),
    "local": ("#f0f9ff", "#0369a1", INK),
    "danger": ("#fef2f2", "#b91c1c", INK),
    "ok": ("#ecfdf5", "#047857", INK),
    "neutral": ("#f3f4f6", "#6b7280", INK),
    "note": ("#fffbeb", "#b45309", INK),
}


@dataclass
class Svg:
    """SVG を組み立てる最小のビルダー。"""

    width: int
    height: int
    title: str
    desc: str
    slug: str = "fig"
    parts: list[str] = field(default_factory=list)
    # 重なり検査用: 引いた線分と、置いた文字の外接矩形
    lines: list[tuple[float, float, float, float]] = field(default_factory=list)
    texts: list[tuple[float, float, float, float, str]] = field(default_factory=list)

    def add(self, markup: str) -> None:
        self.parts.append(markup)

    # -- primitives ----------------------------------------------------

    def box(
        self,
        x: int,
        y: int,
        w: int,
        h: int,
        lines: list[str] | str,
        style: str = "plain",
        radius: int = 10,
        font_size: int = 15,
        bold_first: bool = True,
        mono: bool = False,
        dashed: bool = False,
        uniform: bool = False,
    ) -> None:
        """角丸の箱と中央寄せの複数行テキスト。

        uniform=True のときは全行を同じ大きさ・同じ濃さで描く (並列な
        項目を並べる箱で、1 行目だけ強調されるのを避けるため)。
        """
        fill, stroke, text = STYLES[style]
        dash = ' stroke-dasharray="6 4"' if dashed else ""
        self.add(
            f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{radius}" '
            f'fill="{fill}" stroke="{stroke}" stroke-width="2"{dash} />'
        )
        if isinstance(lines, str):
            lines = [lines]
        line_h = font_size + 6
        total = line_h * len(lines)
        start = y + (h - total) / 2 + font_size
        family = MONO if mono else FONT
        for i, line in enumerate(lines):
            if uniform:
                weight, size, color = "400", font_size, text
            else:
                weight = "600" if (bold_first and i == 0) else "400"
                size = font_size if i == 0 else font_size - 1
                color = text if i == 0 else MUTED
            if line:
                check_fit(line, size, w - 20, f"{self.title} box({x},{y})")
            self.add(
                f'<text x="{x + w / 2:.0f}" y="{start + i * line_h:.0f}" '
                f'text-anchor="middle" font-family="{family}" font-size="{size}" '
                f'font-weight="{weight}" fill="{color}">{html.escape(line)}</text>'
            )

    def label(
        self,
        x: int,
        y: int,
        text: str,
        anchor: str = "start",
        size: int = 13,
        color: str = MUTED,
        weight: str = "400",
        mono: bool = False,
        register: bool = True,
    ) -> None:
        family = MONO if mono else FONT
        w = text_width(text, size)
        if anchor == "start":
            check_fit(text, size, self.width - x - 8, f"{self.title} label({x},{y})")
            x0 = x
        elif anchor == "middle":
            x0 = x - w / 2
        else:
            x0 = x - w
        # ベースラインから上に文字高、下に少しの余白。register=False は
        # arrow() が線の中点へ意図的に重ねて置くラベル。
        if register:
            self.texts.append((x0, y - size, x0 + w, y + size * 0.25, text))
        self.add(
            f'<text x="{x}" y="{y}" text-anchor="{anchor}" font-family="{family}" '
            f'font-size="{size}" font-weight="{weight}" fill="{color}">'
            f"{html.escape(text)}</text>"
        )

    def arrow(
        self,
        x1: int,
        y1: int,
        x2: int,
        y2: int,
        text: str = "",
        color: str = LINE,
        dashed: bool = False,
        text_dy: int = -8,
        text_anchor: str = "middle",
    ) -> None:
        dash = ' stroke-dasharray="6 4"' if dashed else ""
        self.lines.append((x1, y1, x2, y2))
        marker = "arrow-danger" if color == STYLES["danger"][1] else "arrow"
        self.add(
            f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{color}" '
            f'stroke-width="2"{dash} marker-end="url(#{marker})" />'
        )
        if text:
            mx, my = (x1 + x2) / 2, (y1 + y2) / 2
            self.label(int(mx), int(my + text_dy), text, anchor=text_anchor,
                       color=color, register=False)

    def bidir(self, x1: int, y1: int, x2: int, y2: int, text: str = "", color: str = LINE) -> None:
        self.lines.append((x1, y1, x2, y2))
        self.add(
            f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{color}" '
            f'stroke-width="2" marker-start="url(#arrow-back)" marker-end="url(#arrow)" />'
        )
        if text:
            self.label(int((x1 + x2) / 2), int((y1 + y2) / 2 - 8), text,
                       anchor="middle", color=color, register=False)

    def group_frame(self, x: int, y: int, w: int, h: int, caption: str, color: str = "#9ca3af") -> None:
        """点線の囲みと左上のキャプション (論理的なまとまりを示す)。"""
        self.add(
            f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" fill="none" '
            f'stroke="{color}" stroke-width="1.5" stroke-dasharray="4 4" />'
        )
        self.label(x + 12, y + 20, caption, size=13, color=MUTED, weight="600")

    def badge(self, x: int, y: int, text: str, style: str = "neutral") -> None:
        """小さなラベル (状態や注記)。幅は文字数から概算する。"""
        fill, stroke, ink = STYLES[style]
        w = 24 + int(text_width(text, 12))
        check_fit(text, 12, w - 12, f"{self.title} badge({x},{y})")
        self.add(
            f'<rect x="{x}" y="{y}" width="{w}" height="24" rx="12" fill="{fill}" '
            f'stroke="{stroke}" stroke-width="1.5" />'
        )
        self.add(
            f'<text x="{x + w / 2:.0f}" y="{y + 17}" text-anchor="middle" '
            f'font-family="{FONT}" font-size="12" font-weight="600" fill="{ink}">'
            f"{html.escape(text)}</text>"
        )
        return None

    # -- collision check -----------------------------------------------

    def check_collisions(self) -> None:
        """線分が文字の矩形を通過していないかを調べる。

        線分を細かく分割して各点が矩形の内側にあるかを見る (図の規模では
        十分な精度で、依存も増えない)。arrow(text=...) が線の中点へ意図的
        に置くラベルは register=False なので検査対象に入らない。
        """
        for (x1, y1, x2, y2) in self.lines:
            steps = max(int(abs(x2 - x1) + abs(y2 - y1)), 1)
            for (tx0, ty0, tx1, ty1, text) in self.texts:
                for i in range(steps + 1):
                    t = i / steps
                    px, py = x1 + (x2 - x1) * t, y1 + (y2 - y1) * t
                    if tx0 <= px <= tx1 and ty0 <= py <= ty1:
                        COLLISIONS.append(
                            f"{self.title}: line ({x1},{y1})-({x2},{y2}) crosses text "
                            f"at ({tx0:.0f},{ty0:.0f}) | {text}")
                        break
                else:
                    continue
                break

    # -- output --------------------------------------------------------

    def render(self) -> str:
        defs = (
            "<defs>"
            f'<marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" '
            f'markerHeight="7" orient="auto-start-reverse">'
            f'<path d="M 0 0 L 10 5 L 0 10 z" fill="{LINE}" /></marker>'
            f'<marker id="arrow-back" viewBox="0 0 10 10" refX="1" refY="5" markerWidth="7" '
            f'markerHeight="7" orient="auto">'
            f'<path d="M 10 0 L 0 5 L 10 10 z" fill="{LINE}" /></marker>'
            f'<marker id="arrow-danger" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" '
            f'markerHeight="7" orient="auto-start-reverse">'
            f'<path d="M 0 0 L 10 5 L 0 10 z" fill="{STYLES["danger"][1]}" /></marker>'
            "</defs>"
        )
        body = "\n  ".join(self.parts)
        # id は図ごとに固有にする (同一ページに複数の SVG を inline で置いた
        # 場合の衝突を避ける)。height="auto" は SVG では無効な値なので寸法
        # 属性は付けず、CSS 側で制御する。
        tid, did = f"{self.slug}-title", f"{self.slug}-desc"
        return (
            f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {self.width} {self.height}" '
            f'role="img" aria-labelledby="{tid}" aria-describedby="{did}" '
            f'font-family="{FONT}">\n'
            f'  <title id="{tid}">{html.escape(self.title)}</title>\n'
            f'  <desc id="{did}">{html.escape(self.desc)}</desc>\n'
            f"  {defs}\n"
            f'  <rect width="{self.width}" height="{self.height}" fill="#ffffff" />\n'
            f"  {body}\n"
            f"</svg>\n"
        )


# ---------------------------------------------------------------- diagrams


def d_overview() -> Svg:
    """全体像: ブラウザ経路と CLI 経路、外向き通信。"""
    s = Svg(
        900,
        440,
        "nakama の全体像",
        "手元の PC のブラウザは CloudFront と WAF を通って AWS 上の code-server に "
        "つながり、nakama CLI は AWS API と SSM Session Manager を通ってインスタンスを "
        "操作する。インスタンスからインターネットへの通信は NAT インスタンスを経由する。",
    )
    s.group_frame(20, 55, 200, 290, "あなたの PC")
    s.box(35, 90, 170, 70, ["ブラウザ", "Edge / Chrome"], "local")
    s.box(35, 250, 170, 70, ["nakama CLI", "コマンド操作"], "local")

    s.group_frame(260, 30, 620, 330, "AWS (ap-northeast-1 / 東京)")
    s.box(300, 90, 190, 70, ["CloudFront + WAF", "許可した IP だけ通す"], "aws")
    s.box(620, 160, 220, 90, ["EC2 インスタンス", "code-server (VS Code)", "プライベートサブネット"], "aws")
    s.box(300, 250, 190, 70, ["SSM Session Manager", "SSH なしで中に入る"], "aws")
    s.box(620, 296, 220, 56, ["NAT インスタンス", "外向き通信のみ"], "aws")

    s.arrow(205, 120, 296, 120, "HTTPS")
    s.arrow(492, 132, 616, 172, "")
    s.label(500, 196, "VPC Origin (HTTP)", size=12)
    s.arrow(205, 280, 296, 280, "AWS API")
    s.arrow(492, 272, 616, 248, "")
    s.label(500, 300, "SSH なし・ポート開放なし", size=12)
    s.arrow(730, 250, 730, 292, "")
    s.label(740, 276, "apt / git などの通信", size=12)
    s.arrow(730, 354, 730, 386, "")
    s.label(730, 402, "インターネット", size=12, anchor="middle")

    s.label(20, 380, "受信ポートの開放は不要 (インスタンスは公開 IP を持ちません)", size=13, color=MUTED)
    s.label(20, 403, "パスワードは AWS 側に保管され、PC には保存されません", size=13, color=MUTED)
    return s


def d_flow_first_time() -> Svg:
    """初回セットアップの 5 ステップ。"""
    s = Svg(
        920,
        250,
        "初回セットアップの流れ",
        "サインイン、基盤の作成、自分の IP の許可、開発環境の作成、ブラウザで開く、"
        "の 5 ステップで開発を始められる。",
    )
    steps = [
        ("1. サインイン", "nakama aws login", "ブラウザで承認", "約 1 分"),
        ("2. 基盤を作る", "nakama aws init", "1 回だけ (共有)", "約 3 分"),
        ("3. IP を許可", "nakama aws acl add", "自宅/会社ごとに", "数秒"),
        ("4. 環境を作る", "nakama aws create", "1 人で何台でも", "約 15 分"),
        ("5. 使い始める", "nakama aws open", "ブラウザが開く", "数秒"),
    ]
    x = 20
    for i, (title, cmd, note, dur) in enumerate(steps):
        s.box(x, 60, 155, 95, [title, cmd, note], "step", font_size=14, mono=False)
        s.badge(x + 34, 168, dur, "neutral")
        if i < len(steps) - 1:
            s.arrow(x + 158, 107, x + 178, 107, "")
        x += 180
    s.label(20, 215, "2 の init は AWS アカウントに 1 回だけ。3 は接続元のネットワークが変わったときに追加します。",
            size=13, color=MUTED)
    return s


def d_tls_compare() -> Svg:
    """通常の HTTPS と、監査プロキシが介在する HTTPS の比較。"""
    s = Svg(
        900,
        500,
        "通常の HTTPS と監査プロキシがある場合の違い",
        "通常は Amazon が発行した証明書をそのまま検証できる。監査プロキシは通信を"
        "いったん開いて中身を検査し、会社が発行した証明書で封をし直すため、"
        "会社の証明書を信頼していないソフトはエラーになる。",
    )
    # -- 上段: 通常
    s.label(20, 40, "A. 監査プロキシがない場合", size=16, color=INK, weight="600")
    s.box(20, 55, 150, 70, ["あなたの PC", "ブラウザ / CLI"], "local")
    s.box(690, 55, 190, 70, ["AWS", "sts.amazonaws.com など"], "aws")
    s.arrow(172, 90, 686, 90, "暗号化されたまま通過 (誰も中を見られない)")
    s.box(330, 140, 240, 55, ["証明書の発行者: Amazon", "→ 世界共通の信頼リストにある"], "ok",
          font_size=14)
    s.arrow(450, 138, 450, 128, "")
    s.badge(596, 152, "検証 OK", "ok")

    # -- 下段: 監査プロキシあり
    s.label(20, 250, "B. 会社の監査プロキシがある場合", size=16, color=INK, weight="600")
    s.box(20, 265, 150, 70, ["あなたの PC", "ブラウザ / CLI"], "local")
    s.box(330, 258, 220, 85, ["会社の監査プロキシ", "①中身を検査するため開封", "②会社の証明書で封じ直す"], "danger",
          font_size=14)
    s.box(710, 265, 170, 70, ["AWS", "本来の相手"], "aws")
    s.arrow(172, 300, 326, 300, "")
    s.arrow(554, 300, 706, 300, "")
    s.box(300, 375, 280, 55, ["PC に届く証明書の発行者: 会社", "→ 世界共通の信頼リストには無い"], "danger",
          font_size=14)
    s.arrow(440, 373, 440, 347, "")
    s.box(600, 368, 285, 34, ["ブラウザ: OK (会社の証明書を信頼済み)"], "ok", font_size=13, radius=8)
    s.box(600, 410, 285, 34, ["aws CLI: エラー (信頼していない)"], "danger", font_size=13, radius=8)
    s.label(20, 478, "B の状態は正常な会社の運用です。壊れているわけではなく、"
                     "「会社の証明書を信頼するソフトかどうか」で結果が変わります。", size=13, color=MUTED)
    return s


def d_cert_store() -> Svg:
    """誰がどの信頼リストを見るか。"""
    s = Svg(
        900,
        455,
        "ソフトごとに参照する信頼リストが違う",
        "Windows の証明書ストアには会社の証明書が配布済みで、ブラウザと nakama 本体は"
        "それを読む。aws CLI は自分に同梱された公的な証明書リストしか読まないため、"
        "会社の証明書を別途ファイルで渡す必要がある。",
    )
    s.label(20, 34, "信頼リスト (どの発行者を信じるかの一覧)", size=15, color=INK, weight="600")

    s.box(40, 55, 350, 120, [
        "Windows 証明書ストア",
        "公的な CA + 会社の CA (IT が配布)",
        "→ 会社の監査プロキシも検証できる",
    ], "ok", font_size=15)
    s.box(500, 55, 350, 120, [
        "aws CLI に同梱された証明書リスト",
        "公的な CA のみ",
        "→ 会社の CA は入っていない",
    ], "danger", font_size=15)

    s.label(20, 208, "これを読むソフト", size=15, color=INK, weight="600")
    s.box(40, 255, 165, 60, ["Edge / Chrome", "画面は普通に開く"], "ok", font_size=14)
    s.box(225, 255, 165, 60, ["nakama 本体", "設定なしで動く"], "ok", font_size=14)
    s.box(500, 255, 350, 60, ["aws CLI (aws s3 ls など)", "既定では証明書エラーになる"], "danger", font_size=14)
    s.arrow(122, 253, 122, 216, "")
    s.arrow(307, 253, 307, 180, "")
    # 上の 2 本は信頼リストの箱 (下端 175) へ向かう
    s.arrow(675, 253, 675, 180, "")

    s.box(40, 345, 810, 85, [
        "nakama aws cli shell が橋渡しします",
        "Windows 証明書ストアで検証できた会社の証明書をファイル (.pem) に書き出し、",
        "aws CLI にそのファイルの場所を教えた専用シェルを開きます",
    ], "step", font_size=15)
    return s


def d_cli_shell() -> Svg:
    """cli shell が内部で行う 5 ステップと出力。"""
    s = Svg(
        900,
        520,
        "nakama aws cli shell の動作",
        "プロキシ設定の検出、実際の通信の観測、Windows 証明書ストアでの検証、"
        "証明書ファイルの生成、環境変数を設定した新しいシェルの起動、という順で動く。"
        "元のシェルや Windows の設定は変更しない。",
    )
    steps = [
        ("① 会社のプロキシ設定を読む", "Windows の設定 (WinHTTP) と PAC を自動検出"),
        ("② AWS に試しに接続してみる", "実際に届く証明書を観測する"),
        ("③ Windows 証明書ストアで検証", "OS が信頼していない証明書は採用しない"),
        ("④ 証明書ファイルを作る", "会社の CA + Amazon の公式ルート証明書"),
        ("⑤ 設定済みシェルを開く", "環境変数を入れた新しいシェルを起動"),
    ]
    y = 55
    for i, (title, note) in enumerate(steps):
        style = "step" if i < 4 else "ok"
        s.box(40, y, 470, 62, [title, note], style, font_size=15)
        if i < len(steps) - 1:
            s.arrow(275, y + 64, 275, y + 76, "")
        y += 78

    s.box(560, 120, 300, 190, [
        "シェルの中で設定される環境変数",
        "HTTP_PROXY / HTTPS_PROXY",
        "NO_PROXY",
        "AWS_CA_BUNDLE (証明書ファイルの場所)",
        "",
        "→ この中では aws s3 ls などが",
        "   追加設定なしで動きます",
    ], "neutral", font_size=14)
    s.arrow(515, 400, 556, 310, "")

    s.box(40, 450, 820, 55, [
        "exit と入力すれば元のシェルに戻り、痕跡は残りません",
        "レジストリ・ユーザー環境変数・~/.aws/config・元のシェルは一切変更しません",
    ], "note", font_size=14)
    return s


def d_access_paths() -> Svg:
    """4 つのアクセス方法の使い分け。"""
    s = Svg(
        920,
        420,
        "4 つのアクセス方法の使い分け",
        "ブラウザで開く open、ターミナルに入る connect、デスクトップのエディタから使う ssh、"
        "ポートを手元に持ってくる tunnel の 4 つがある。",
    )
    rows = [
        ("open", "ブラウザで VS Code を開く", ["CloudFront 経由", "(自分の IP の許可が必要)"],
         ["画面だけで完結したい"], "ok"),
        ("connect", "ターミナルに入る", ["SSM 経由"],
         ["ログを見る・コマンドを叩く"], "step"),
        ("ssh", "手元の VS Code / scp / git", ["SSM 経由"],
         ["デスクトップのエディタを使いたい"], "step"),
        ("tunnel", "ポートを手元に持ってくる", ["SSM 経由"],
         ["localhost:3000 で", "開発サーバーを見る"], "step"),
    ]
    s.label(25, 40, "コマンド", size=13, color=MUTED, weight="600")
    s.label(190, 40, "できること", size=13, color=MUTED, weight="600")
    s.label(425, 40, "通り道", size=13, color=MUTED, weight="600")
    s.label(640, 40, "こんなときに", size=13, color=MUTED, weight="600")
    y = 55
    for cmd, what, path, when, style in rows:
        s.box(25, y, 155, 60, [f"nakama aws {cmd}"], style, font_size=13, mono=True)
        s.box(190, y, 225, 60, [what], "plain", font_size=14, bold_first=False, uniform=True)
        s.box(425, y, 205, 60, path, "plain", font_size=13, uniform=True)
        s.box(640, y, 255, 60, when, "plain", font_size=13, uniform=True)
        y += 72
    s.box(25, 350, 870, 55, [
        "open だけが CloudFront (インターネット) 経由で、IP の許可が必要です",
        "connect / ssh / tunnel は SSM 経由なので IP 許可は不要です (AWS の権限で守られます)",
    ], "note", font_size=14)
    return s


def d_cost() -> Svg:
    """電源連動によるコストの考え方。"""
    s = Svg(
        900,
        390,
        "止めればコストがほぼゼロになる仕組み",
        "外向き通信に使う NAT インスタンスの電源は code-server インスタンスの稼働に"
        "連動する。すべて停止すればディスク代だけが残る。",
    )
    s.label(20, 36, "1 台でも起動しているとき", size=15, color=INK, weight="600")
    s.box(20, 50, 200, 60, ["code-server", "起動中"], "ok", font_size=14)
    s.box(260, 50, 200, 60, ["NAT インスタンス", "自動で起動"], "ok", font_size=14)
    s.arrow(222, 80, 256, 80, "連動")
    s.box(500, 50, 380, 60, ["料金: インスタンス + NAT + ディスク", "使っている時間だけ課金"], "neutral", font_size=14)

    s.label(20, 166, "すべて停止したとき", size=15, color=INK, weight="600")
    s.box(20, 180, 200, 60, ["code-server", "停止 (stop)"], "neutral", font_size=14)
    s.box(260, 180, 200, 60, ["NAT インスタンス", "自動で停止"], "neutral", font_size=14)
    s.arrow(222, 210, 256, 210, "連動")
    s.box(500, 180, 380, 60, ["料金: ディスク (EBS) の保管料のみ", "固定費はほぼゼロ"], "ok", font_size=14)

    s.box(20, 280, 860, 90, [
        "作業を終えたら nakama aws stop <名前> を習慣にしてください",
        "停止してもディスクの中身 (インストールしたもの・書いたコード) は残ります。",
        "完全に削除するのは nakama aws delete <名前> です (こちらは中身も消えます)。",
    ], "note", font_size=15)
    return s


def d_backup_restore() -> Svg:
    """バックアップと復元の関係。"""
    s = Svg(
        900,
        370,
        "バックアップと復元",
        "backup create でその時点のディスクの複製 (スナップショット) を作り、"
        "restore で選んだ世代の内容にディスクを戻す。restore は現在の内容を上書きする。",
    )
    s.box(20, 60, 200, 70, ["現在のディスク", "作業中の状態"], "local", font_size=15)
    labels = [
        ("世代 1", "初期セットアップ後"),
        ("世代 2", "before-upgrade"),
        ("世代 3", "最新 (自動取得)"),
    ]
    x = 320
    for name, note in labels:
        s.box(x, 45, 170, 65, [name, note], "neutral", font_size=14)
        x += 195
    s.arrow(222, 80, 316, 78, "backup create")
    s.label(320, 130, "スナップショット (増えていく) — 世代数は --keep で制限できます", size=13, color=MUTED)

    s.box(20, 200, 200, 70, ["ディスクの中身が", "選んだ世代に戻る"], "danger", font_size=15)
    s.arrow(700, 150, 232, 200, "", color=STYLES["danger"][1])
    s.label(250, 155, "restore --label <世代>", size=13, color=STYLES["danger"][1])
    s.box(280, 200, 600, 70, [
        "restore は現在の内容を上書きします (元に戻せません)",
        "念のため restore の前にもう 1 つ backup create しておくと安全です",
    ], "note", font_size=14)
    s.box(20, 300, 860, 50, [
        "update はアップデート前に自動でバックアップを取ります (失敗しても戻せます)",
    ], "ok", font_size=14)
    return s


def d_doctor_map() -> Svg:
    """doctor が点検する範囲。"""
    s = Svg(
        900,
        430,
        "doctor が点検する範囲",
        "doctor は手元の環境 (プロキシ、証明書)、AWS への資格情報、IP 許可、"
        "機能ごとの権限、ポート転送の状態を読み取り専用で点検する。",
    )
    s.label(20, 36, "手元の環境", size=15, color=INK, weight="600")
    s.box(20, 50, 250, 66, ["proxy", "会社のプロキシを見つけられたか"], "local", font_size=14)
    s.box(20, 126, 250, 66, ["tls", "監査プロキシの有無と検証結果"], "local", font_size=14)
    s.box(20, 202, 250, 66, ["tunnels", "ポート転送が生きているか"], "local", font_size=14)

    s.label(330, 36, "AWS 側", size=15, color=INK, weight="600")
    s.box(330, 50, 270, 66, ["credentials", "どのアカウントとして動くか"], "aws", font_size=14)
    s.box(330, 126, 270, 66, ["waf-allowlist", "自分の IP が許可されているか"], "aws", font_size=14)

    s.label(650, 36, "機能ごとの権限", size=15, color=INK, weight="600")
    s.box(650, 50, 230, 218, [
        "capability: create",
        "capability: connect",
        "capability: backup",
        "capability: delete",
        "…など機能ごとに 1 行",
        "",
        "足りない権限は",
        "その場に列挙されます",
    ], "step", font_size=14, uniform=True)

    s.box(20, 300, 860, 50, [
        "3 つの結果: ok = 問題なし / warn = 使えない機能がある / fail = 直さないと動かない",
    ], "neutral", font_size=15)
    s.box(20, 360, 860, 55, [
        "完全に読み取り専用です (AWS のリソースも PC の設定も変更しません)",
        "パスワードや資格情報は表示しないので、出力をそのまま人に見せて相談できます",
    ], "ok", font_size=14)
    return s


def d_bare_vs_full() -> Svg:
    """通常の create と --bare の違い。"""
    s = Svg(
        900,
        400,
        "通常の create と --bare の違い",
        "通常は code-server と CloudFront を含む一式を作る。--bare は EC2 と SSM だけを"
        "作るので、ブラウザでは開けないが作成が速い。",
    )
    s.label(20, 36, "nakama aws create (通常)", size=15, color=INK, weight="600")
    s.box(20, 50, 420, 210, [
        "作られるもの",
        "・EC2 インスタンス",
        "・code-server (ブラウザ版 VS Code)",
        "・CloudFront + VPC Origin",
        "・ログイン用パスワード (AWS 側に保管)",
        "",
        "作成時間: 約 15 分 (CloudFront の反映待ち)",
    ], "step", font_size=14)
    s.box(20, 275, 420, 100, [
        "使えるもの",
        "open / connect / ssh / tunnel / backup /",
        "restore / edit / update / start / stop / delete",
    ], "ok", font_size=14)

    s.label(470, 36, "nakama aws create --bare", size=15, color=INK, weight="600")
    s.box(470, 50, 410, 210, [
        "作られるもの",
        "・EC2 インスタンス",
        "・SSM 経由の接続のみ",
        "",
        "(code-server / CloudFront / パスワードなし)",
        "",
        "作成時間: 数分 (待ち時間が短い)",
    ], "neutral", font_size=14)
    s.box(470, 275, 410, 100, [
        "使えるもの",
        "connect / ssh / tunnel / backup / restore /",
        "edit / update (OS のみ) / start / stop / delete",
        "open は使えません (案内が出ます)",
    ], "note", font_size=14)
    return s


# ---------------------------------------------------------------- driver

DIAGRAMS = {
    "overview": d_overview,
    "flow-first-time": d_flow_first_time,
    "tls-compare": d_tls_compare,
    "cert-store": d_cert_store,
    "cli-shell": d_cli_shell,
    "access-paths": d_access_paths,
    "cost": d_cost,
    "backup-restore": d_backup_restore,
    "doctor-map": d_doctor_map,
    "bare-vs-full": d_bare_vs_full,
}


def main() -> None:
    out_dir = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "img")
    os.makedirs(out_dir, exist_ok=True)
    for name, builder in DIAGRAMS.items():
        svg = builder()
        svg.slug = name
        svg.check_collisions()
        path = os.path.join(out_dir, f"{name}.svg")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(svg.render())
        print(f"wrote {os.path.relpath(path)} ({svg.width}x{svg.height})")
    failed = False
    if OVERFLOWS:
        failed = True
        print("\ntext overflow detected (widen the box or split the line):", file=sys.stderr)
        for o in OVERFLOWS:
            print(f"  {o}", file=sys.stderr)
    if COLLISIONS:
        failed = True
        print("\nline/text collision detected (move the label off the line):", file=sys.stderr)
        for c in COLLISIONS:
            print(f"  {c}", file=sys.stderr)
    if failed:
        sys.exit(1)
    print("\nno text overflow and no line/text collision detected")


if __name__ == "__main__":
    main()
