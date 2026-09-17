#!/usr/bin/env python3
"""生成 internal/tui 的对拍语料（Python 侧为参照实现）。

tui.py 的交互部分（Live + 真实终端）没法确定性重放，但**版式与按键解析**可以：
生成器把 tui.console 换成一个固定尺寸、强制终端、关闭 safe_box 的 Console，
把 msvcrt / os.read / select / time.monotonic 换成脚本化桩，于是下列纯逻辑能在
同一台机器上跑出确定结果，Go 侧用同样的输入重放：

* ``markup``          标记解析 → 纯文本（rich.markup.render 的等价物）
* ``width``           终端单元格宽度（rich.cells.cell_len）
* ``truncate``        按宽度截断与省略号（no_wrap / overflow="ellipsis"）
* ``wrap``            折行（Text.wrap + divide_line(fold=True)）
* ``panel``           面板/标题/分组/旗标 → 纯文本行（rich.panel.Panel 的版式）
* ``menu``            菜单表格（**版式刻意不同**，见 diverges 标记）
* ``fit``             fit_terminal_lines
* ``frame``           terminal_frame_state 的几何 + 渲染行
* ``scroll``          content_scroll_offset
* ``viewport``        content_viewport_height
* ``wheel``           should_handle_wheel（注入单调时钟）
* ``mouse``           parse_sgr_mouse_sequence
* ``key_windows``     read_key 的 msvcrt 分支（脚本化 msvcrt）
* ``key_posix``       _read_posix_key_impl（脚本化 os.read/select/stdin）
* ``prompt_visible``  prompt_text 里输入框的可见值截断（脚本化按键 + 捕获 Text）

语料里的每一行都是 rich 渲染出来后再剥掉 ANSI 的**纯文本**，因此与颜色系统无关。

用法::

    python scripts/gen_tui_corpus.py           # 写入语料
    python scripts/gen_tui_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import re
import sys
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from rich.align import Align  # noqa: E402
from rich.console import Console, Group  # noqa: E402
from rich.text import Text  # noqa: E402

from auto_model_key_router import tui  # noqa: E402

DEFAULT_DIR = REPO_ROOT / "internal" / "tui" / "testdata"
ANSI = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07")


# --------------------------------------------------------------------------- #
# 受控终端
# --------------------------------------------------------------------------- #

def install_console(width: int, height: int) -> Console:
    """换掉 tui.console：固定尺寸、强制终端、关闭 safe_box。

    safe_box 默认在非终端上为真，rich 会把 ROUNDED 边框替换成 SQUARE；Go 侧恒定
    使用 ROUNDED（等于真实 Unicode 终端下的表现），因此这里显式关掉它，
    让语料只反映一种边框。
    """
    console = Console(
        width=width,
        height=height,
        file=io.StringIO(),
        force_terminal=True,
        color_system=None,
        legacy_windows=False,
        safe_box=False,
        record=True,
    )
    tui.console = console
    return console


def plain_lines(console: Console, renderable) -> list[str]:
    """渲染并剥 ANSI，返回行列表（丢掉 print 追加的末尾换行）。"""
    console.print(renderable)
    text = ANSI.sub("", console.export_text())
    lines = text.split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    return lines


def render_spec_lines(spec: dict, width: int, height: int) -> list[str]:
    console = install_console(width, height)
    return plain_lines(console, build_renderable(spec))


# --------------------------------------------------------------------------- #
# 渲染节点的中立描述（Go 侧有一一对应的解码器）
# --------------------------------------------------------------------------- #

def build_renderable(spec):
    if spec is None:
        return None
    kind = spec["kind"]
    if kind == "string":
        # 直接给 rich 一个 str：rich 会解析标记（Console.render_str）。
        return spec["value"]
    if kind == "text":
        # rich.text.Text：**不**解析标记（tui.py:107-108 的 shortcut_text）。
        return Text(spec["value"])
    if kind == "page_title":
        return tui.page_title(spec["title"], spec.get("subtitle"))
    if kind == "panel":
        content = build_renderable(spec.get("content"))
        if content is None:
            content = ""
        return tui.section_panel(
            content, spec["title"], spec.get("border", "cyan"), spec.get("subtitle")
        )
    if kind == "shortcut":
        return tui.shortcut_text(spec["value"])
    if kind == "group":
        return Group(*[build_renderable(item) for item in spec["items"]])
    if kind == "menu":
        return tui.menu_table([tuple(pair) for pair in spec["options"]], spec["selected"])
    if kind == "checkbox":
        return tui.checkbox_menu_table(
            [tuple(pair) for pair in spec["options"]], spec["selected"], set(spec["checked"])
        )
    if kind == "app_flag":
        return tui.app_flag_title(spec["title"], spec["subtitle"], spec["version"])
    if kind == "align":
        return Align.center(build_renderable(spec["child"]))
    raise AssertionError(f"未知渲染节点: {kind}")


# --------------------------------------------------------------------------- #
# 桩
# --------------------------------------------------------------------------- #

class FakeTime:
    """替代 tui.time：单调时钟每次调用 +1 秒，sleep 为空操作。

    这样 read_windows_char_if_available 的「等待超时」会立刻结束，语义退化成
    「有缓冲字符就读，否则返回 None」，既确定又不用真的睡 0.2 秒。
    """

    def __init__(self, now: float = 0.0, step: float = 1.0) -> None:
        self.now = now
        self.step = step

    def monotonic(self) -> float:
        value = self.now
        self.now += self.step
        return value

    def sleep(self, seconds: float) -> None:
        return None


class ScriptedMsvcrt:
    """替代 tui.msvcrt：按脚本吐宽字符。"""

    def __init__(self, script: list[str]) -> None:
        self.script = list(script)
        self.index = 0

    def getwch(self) -> str:
        if self.index >= len(self.script):
            # Python 的 getwch 会阻塞；脚本化时明确失败，避免语料悄悄吞掉分支。
            raise AssertionError("msvcrt 脚本已耗尽")
        char = self.script[self.index]
        self.index += 1
        return char

    def kbhit(self) -> bool:
        return self.index < len(self.script)


class ScriptedOS:
    """替代 tui.os：只实现 read(fd, n)。"""

    def __init__(self, script: list[int]) -> None:
        self.script = list(script)
        self.index = 0

    def read(self, fd: int, count: int) -> bytes:
        if self.index >= len(self.script):
            return b""
        chunk = self.script[self.index:self.index + count]
        self.index += count
        return bytes(chunk)


class ScriptedSelect:
    """替代 tui.select：有剩余字节就报告可读。"""

    def __init__(self, os_stub: ScriptedOS) -> None:
        self.os_stub = os_stub

    def select(self, readers, writers, errors, timeout):
        if self.os_stub.index < len(self.os_stub.script):
            return (list(readers), [], [])
        return ([], [], [])


class ScriptedStdin:
    def fileno(self) -> int:
        return 0


class ScriptedSys:
    def __init__(self) -> None:
        self.stdin = ScriptedStdin()
        self.platform = sys.platform


class FakeLive:
    """替代 rich.live.Live：只记录最后一次 update 的渲染对象。

    prompt_text 用 `live.update(render(), refresh=True)` 刷新界面，这里把渲染
    对象存下来，就能在不启动真终端的情况下拿到真实 Python 生成的界面树。
    """

    last: "FakeLive | None" = None

    def __init__(self, renderable, **kwargs) -> None:
        self.renderable = renderable
        FakeLive.last = self

    def __enter__(self) -> "FakeLive":
        return self

    def __exit__(self, *exc_info) -> bool:
        return False

    def update(self, renderable, refresh: bool = False) -> None:
        self.renderable = renderable
        FakeLive.last = self


# --------------------------------------------------------------------------- #
# 用例
# --------------------------------------------------------------------------- #

MARKUP_INPUTS = [
    "plain text",
    "",
    "[red]x[/red]",
    "[bold cyan]标题[/bold cyan]",
    "a [dim]b[/dim] c",
    "[[escaped]]",
    "[]",
    "价格 [100] 元",
    "中文[red]红[/red]字",
    "[link=https://example.com]链接[/link]",
    "[bold]未闭合",
    "[red][bold]嵌套[/bold][/red]",
    r"转义 \[red] 不是标签",
    "[dim]换行\n内容[/dim]",
    "[not a style]整段被吃掉",
    "[#ff0000]十六进制色[/#ff0000]",
    "[on blue]背景[/on blue]",
    "前缀[red]中段[/red]后缀",
]

WIDTH_INPUTS = [
    "",
    "abc",
    "中文",
    "中文abc",
    "🎯",
    "cafe\u0301",
    "a\tb",
    "Ａ０",
    "…",
    "│─╭╮",
    "Ｈｅｌｌｏ",
]

WRAP_INPUTS: list[tuple[str, int]] = [
    ("hello world foo", 20),
    ("hello world foo", 10),
    ("hello world", 6),
    ("a b c d e f g", 6),
    ("word  double", 8),
    ("中文内容测试字符串很长", 16),
    ("mixed 中文 and english words", 16),
    ("trailing space ", 8),
    (" lead", 8),
    ("x" * 40, 16),
    ("x" * 40, 7),
    ("aa bb cc dd ee ff gg hh", 14),
    ("", 10),
    ("   ", 10),
    ("one\ntwo", 10),
    ("超长英文词 supercalifragilisticexpialidocious 结尾", 12),
]

PANEL_SPECS: list[dict] = [
    {"name": "short_ascii", "width": 40, "spec": {"kind": "panel", "title": "标题", "content": {"kind": "string", "value": "hello"}}},
    {"name": "with_subtitle", "width": 40, "spec": {"kind": "panel", "title": "标题", "subtitle": "副标题", "content": {"kind": "string", "value": "hello"}}},
    {"name": "with_border", "width": 40, "spec": {"kind": "panel", "title": "操作出错", "border": "red", "content": {"kind": "string", "value": "[red]boom[/red]\n\n[dim]dim text[/dim]"}}},
    {"name": "multiline", "width": 40, "spec": {"kind": "panel", "title": "T", "content": {"kind": "string", "value": "line1\nline2"}}},
    {"name": "empty_content", "width": 40, "spec": {"kind": "panel", "title": "T", "content": {"kind": "string", "value": ""}}},
    {"name": "cjk_content", "width": 40, "spec": {"kind": "panel", "title": "标题", "content": {"kind": "string", "value": "中文内容 abc"}}},
    {"name": "wrap_long", "width": 40, "spec": {"kind": "panel", "title": "T", "content": {"kind": "string", "value": "x" * 80}}},
    {"name": "wrap_words", "width": 40, "spec": {"kind": "panel", "title": "T", "content": {"kind": "string", "value": "aa bb cc dd ee ff gg hh ii jj kk ll mm nn"}}},
    {"name": "narrow_20", "width": 20, "spec": {"kind": "panel", "title": "标题", "content": {"kind": "string", "value": "hello world"}}},
    {"name": "narrow_10", "width": 10, "spec": {"kind": "panel", "title": "标题", "subtitle": "副标题", "content": {"kind": "string", "value": "hello"}}},
    {"name": "narrow_6", "width": 6, "spec": {"kind": "panel", "title": "标题", "content": {"kind": "string", "value": "hello"}}},
    {"name": "narrow_5", "width": 5, "spec": {"kind": "panel", "title": "T", "content": {"kind": "string", "value": "hi"}}},
    {"name": "narrow_4", "width": 4, "spec": {"kind": "panel", "title": "T", "content": {"kind": "string", "value": "hi"}}},
    {"name": "long_title", "width": 40, "spec": {"kind": "panel", "title": "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "content": {"kind": "string", "value": "hi"}}},
    {"name": "long_subtitle", "width": 40, "spec": {"kind": "panel", "title": "T", "subtitle": "S" * 50, "content": {"kind": "string", "value": "hi"}}},
    {"name": "nested_panel", "width": 40, "spec": {"kind": "panel", "title": "外层", "content": {"kind": "panel", "title": "内层", "border": "blue", "content": {"kind": "string", "value": "inner body"}}}},
    {"name": "group_content", "width": 40, "spec": {"kind": "panel", "title": "T", "content": {"kind": "group", "items": [{"kind": "string", "value": "first"}, {"kind": "string", "value": "second"}]}}},
    {"name": "page_title_sub", "width": 30, "spec": {"kind": "page_title", "title": "主菜单", "subtitle": "副标题"}},
    {"name": "page_title_plain", "width": 30, "spec": {"kind": "page_title", "title": "主菜单"}},
    {"name": "shortcut_short", "width": 40, "spec": {"kind": "shortcut", "value": "↑/↓ 选择  ·  Enter 确认"}},
    {"name": "shortcut_long", "width": 40, "spec": {"kind": "shortcut", "value": "↑/↓ 选择  ·  Enter 确认  ·  PgUp/PgDn 翻阅窗体  ·  数字快捷键  ·  q/Ctrl+C 返回"}},
    {"name": "align_text", "width": 40, "spec": {"kind": "align", "child": {"kind": "text", "value": "居中\nsecond"}}},
    {"name": "align_group", "width": 30, "spec": {"kind": "align", "child": {"kind": "group", "items": [{"kind": "string", "value": "one"}, {"kind": "string", "value": "two"}]}}},
    {"name": "empty_panel", "width": 40, "spec": {"kind": "panel", "title": "T"}},
    {"name": "service_like", "width": 60, "spec": {"kind": "panel", "title": "后台服务", "border": "green", "content": {"kind": "string", "value": "PID: 12345\n状态: 运行中\n端口: 8765"}}},
]

APP_FLAG_CASES = [
    {"name": "flag_default", "width": 70, "title": "Auto Model Key Router", "subtitle": "本地路由", "version": "1.2.3"},
    {"name": "flag_narrow", "width": 50, "title": "AMKR", "subtitle": "副标题", "version": "0.0.1"},
    {"name": "flag_wide", "width": 100, "title": "Auto Model Key Router", "subtitle": "统一模型路由", "version": "9.9.9"},
]

MENU_CASES = [
    {"name": "menu_three", "width": 40, "options": [["1", "启动服务"], ["2", "停止服务"], ["0", "返回"]], "selected": 1, "checkbox": False, "checked": []},
    {"name": "menu_first", "width": 40, "options": [["1", "启动服务"], ["2", "停止服务"], ["0", "返回"]], "selected": 0, "checkbox": False, "checked": []},
    {"name": "menu_ascii", "width": 50, "options": [["y", "是"], ["n", "否"]], "selected": 0, "checkbox": False, "checked": []},
    {"name": "checkbox_two", "width": 40, "options": [["1", "启动服务"], ["2", "停止服务"]], "selected": 0, "checkbox": True, "checked": [0]},
    {"name": "checkbox_none", "width": 40, "options": [["1", "启动服务"], ["2", "停止服务"]], "selected": 1, "checkbox": True, "checked": []},
    {"name": "checkbox_all", "width": 40, "options": [["1", "启动服务"], ["2", "停止服务"], ["3", "重启"]], "selected": 2, "checkbox": True, "checked": [0, 1, 2]},
]

FIT_CASES = [
    {"lines": ["L0", "L1", "L2", "L3"], "height": 0, "preserve_bottom": True},
    {"lines": ["L0", "L1", "L2", "L3"], "height": 1, "preserve_bottom": True},
    {"lines": ["L0", "L1", "L2", "L3"], "height": 1, "preserve_bottom": False},
    {"lines": ["L0", "L1", "L2", "L3"], "height": 2, "preserve_bottom": True},
    {"lines": ["L0", "L1", "L2", "L3"], "height": 2, "preserve_bottom": False},
    {"lines": ["L0", "L1", "L2", "L3"], "height": 3, "preserve_bottom": True},
    {"lines": ["L0", "L1", "L2", "L3"], "height": 5, "preserve_bottom": True},
    {"lines": [], "height": 3, "preserve_bottom": True},
    {"lines": ["only"], "height": 1, "preserve_bottom": True},
]

FRAME_CASES = [
    {
        "name": "basic", "width": 40, "height": 12, "offset": 0,
        "renderables": [{"kind": "string", "value": "line1"}, {"kind": "string", "value": "line2"}],
        "footer": {"kind": "shortcut", "value": "↑/↓ 选择  ·  Enter 确认"},
    },
    {
        "name": "scroll_offset", "width": 40, "height": 12, "offset": 5,
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(30)]}],
        "footer": {"kind": "shortcut", "value": "foot"},
    },
    {
        "name": "preserve_bottom", "width": 40, "height": 12, "offset": 3, "preserve_bottom": True,
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(30)]}],
        "footer": {"kind": "shortcut", "value": "foot"},
    },
    {
        "name": "focus_text", "width": 40, "height": 12, "offset": 0, "focus_text": "row15",
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(30)]}],
        "footer": {"kind": "shortcut", "value": "foot"},
    },
    {
        "name": "no_footer", "width": 40, "height": 12, "offset": 0,
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(30)]}],
        "footer": None,
    },
    {
        "name": "offset_clamped_negative", "width": 40, "height": 12, "offset": -3,
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(30)]}],
        "footer": {"kind": "shortcut", "value": "foot"},
    },
    {
        "name": "offset_clamped_high", "width": 40, "height": 12, "offset": 999,
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(30)]}],
        "footer": {"kind": "shortcut", "value": "foot"},
    },
    {
        "name": "footer_taller_than_inner", "width": 30, "height": 4, "offset": 0,
        "renderables": [{"kind": "string", "value": "a"}, {"kind": "string", "value": "b"}],
        "footer": {"kind": "string", "value": "F1\nF2\nF3"},
    },
    {
        "name": "panel_body", "width": 40, "height": 12, "offset": 0,
        "renderables": [{"kind": "panel", "title": "内层", "content": {"kind": "string", "value": "body"}}],
        "footer": {"kind": "shortcut", "value": "foot"},
    },
    {
        "name": "custom_title", "width": 40, "height": 10, "offset": 0, "frame_title": "自定义",
        "renderables": [{"kind": "string", "value": "x"}],
        "footer": None,
    },
    {
        "name": "cjk_body", "width": 36, "height": 12, "offset": 0,
        "renderables": [{"kind": "string", "value": "中文正文内容"}, {"kind": "string", "value": "第二行"}],
        "footer": {"kind": "shortcut", "value": "↑/↓ 选择"},
    },
    {
        "name": "empty_body", "width": 40, "height": 8, "offset": 0,
        "renderables": [],
        "footer": {"kind": "shortcut", "value": "foot"},
    },
    # 高度 < 3 行时 terminal_frame_state 走退化分支（tui.py:155-158）：不套面板，
    # 直接输出正文。这两条钉住退化分支的几何与行内容。
    {
        "name": "height_2_degenerate", "width": 40, "height": 2, "offset": 0,
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(10)]}],
        "footer": {"kind": "shortcut", "value": "foot"},
        "note": "高度 2：退化分支，无面板，只输出前 2 行。",
    },
    {
        "name": "height_1_degenerate", "width": 40, "height": 1, "offset": 0,
        "renderables": [{"kind": "group", "items": [{"kind": "string", "value": f"row{i:02d}"} for i in range(10)]}],
        "footer": {"kind": "shortcut", "value": "foot"},
        "note": "高度 1：退化分支，只输出第一行。",
    },
]

SCROLL_CASES = [
    ("scroll_up", 5, 10, 3),
    ("scroll_up", 0, 10, 3),
    ("scroll_down", 5, 10, 3),
    ("scroll_down", 10, 10, 3),
    ("page_up", 5, 10, 4),
    ("page_up", 2, 10, 4),
    ("page_down", 5, 10, 4),
    ("page_down", 9, 10, 4),
    ("home", 7, 10, 4),
    ("end", 0, 10, 4),
    ("enter", 5, 10, 4),
    ("up", 5, 10, 4),
    ("", 5, 10, 4),
]

WHEEL_CASES = [
    ("scroll_up", None, 0.0, 10.0),
    ("scroll_down", None, 0.0, 10.0),
    ("scroll_up", "scroll_up", 10.0, 10.1),
    ("scroll_up", "scroll_up", 10.0, 10.2),
    ("scroll_up", "scroll_up", 10.0, 10.16),
    ("scroll_down", "scroll_up", 10.0, 10.05),
    ("scroll_up", "scroll_down", 10.0, 10.05),
    ("enter", "scroll_up", 10.0, 10.05),
    ("q", None, 0.0, 0.0),
]

MOUSE_CASES = [
    "64;10;5M",
    "65;10;5M",
    "64;10;5m",
    "65;10;5m",
    "63;10;5M",
    "0;1;1M",
    "64;10;5X",
    "",
    "M",
    "abc;1;1M",
    "64M",
    "<64;10;5M",
    "70;3;4M",
]

KEY_WINDOWS_CASES: list[dict] = [
    {"name": "plain_a", "script": ["a"]},
    {"name": "enter_cr", "script": ["\r"]},
    {"name": "enter_lf", "script": ["\n"]},
    {"name": "ctrl_c", "script": ["\x03"]},
    {"name": "ctrl_u", "script": ["\x15"]},
    {"name": "backspace", "script": ["\b"]},
    {"name": "delete", "script": ["\x7f"]},
    {"name": "cjk", "script": ["中"]},
    {"name": "arrow_up_ext", "script": ["\x00", "H"]},
    {"name": "arrow_down_ext", "script": ["\x00", "P"]},
    {"name": "arrow_left_ext", "script": ["\xe0", "K"]},
    {"name": "arrow_right_ext", "script": ["\xe0", "M"]},
    {"name": "page_up_ext", "script": ["\x00", "I"]},
    {"name": "page_down_ext", "script": ["\x00", "Q"]},
    {"name": "home_ext", "script": ["\x00", "G"]},
    {"name": "end_ext", "script": ["\x00", "O"]},
    {"name": "unknown_ext", "script": ["\x00", "Z"]},
    {"name": "esc_alone", "script": ["\x1b"]},
    {"name": "esc_arrow_up", "script": ["\x1b", "[", "A"]},
    {"name": "esc_arrow_down", "script": ["\x1b", "[", "B"]},
    {"name": "esc_arrow_left", "script": ["\x1b", "[", "D"]},
    {"name": "esc_arrow_right", "script": ["\x1b", "[", "C"]},
    {"name": "esc_ss3_up", "script": ["\x1b", "O", "A"]},
    {"name": "esc_page_up", "script": ["\x1b", "[", "5", "~"]},
    {"name": "esc_page_down", "script": ["\x1b", "[", "6", "~"]},
    {"name": "esc_home", "script": ["\x1b", "[", "H"]},
    {"name": "esc_end", "script": ["\x1b", "[", "F"]},
    {"name": "esc_wheel_up", "script": ["\x1b", "[", "<", "6", "4", ";", "1", "0", ";", "5", "M"]},
    {"name": "esc_wheel_down", "script": ["\x1b", "[", "<", "6", "5", ";", "1", "0", ";", "5", "M"]},
    {"name": "esc_bad_mouse", "script": ["\x1b", "[", "<", "6", "3", ";", "1", "0", ";", "5", "M"]},
    {"name": "esc_unknown", "script": ["\x1b", "[", "Z"]},
    {"name": "esc_lone_bracket", "script": ["\x1b", "["]},
    {"name": "esc_other", "script": ["\x1b", "x"]},
]

KEY_POSIX_CASES: list[dict] = [
    {"name": "plain_a", "script": [ord("a")]},
    {"name": "enter_cr", "script": [0x0D]},
    {"name": "ctrl_c", "script": [0x03]},
    {"name": "utf8_multibyte", "script": [0xE4, 0xB8, 0xAD]},
    {"name": "esc_alone", "script": [0x1B]},
    {"name": "esc_arrow_up", "script": [0x1B, ord("["), ord("A")]},
    {"name": "esc_arrow_down", "script": [0x1B, ord("["), ord("B")]},
    {"name": "esc_arrow_left", "script": [0x1B, ord("["), ord("D")]},
    {"name": "esc_arrow_right", "script": [0x1B, ord("["), ord("C")]},
    {"name": "esc_ss3_up", "script": [0x1B, ord("O"), ord("A")]},
    {"name": "esc_page_up", "script": [0x1B, ord("["), ord("5"), ord("~")]},
    {"name": "esc_page_up_no_tilde", "script": [0x1B, ord("["), ord("5"), ord("x")]},
    {"name": "esc_page_down", "script": [0x1B, ord("["), ord("6"), ord("~")]},
    {"name": "esc_home", "script": [0x1B, ord("["), ord("H")]},
    {"name": "esc_end", "script": [0x1B, ord("["), ord("F")]},
    {"name": "esc_wheel_up", "script": [0x1B, ord("["), ord("<"), ord("6"), ord("4"), ord(";"), ord("1"), ord("0"), ord(";"), ord("5"), ord("M")]},
    {"name": "esc_wheel_down", "script": [0x1B, ord("["), ord("<"), ord("6"), ord("5"), ord(";"), ord("1"), ord("0"), ord(";"), ord("5"), ord("M")]},
    {"name": "esc_x10_mouse", "script": [0x1B, ord("["), ord("M"), 32, 33, 34]},
    {"name": "esc_unknown", "script": [0x1B, ord("["), ord("Z")]},
    {"name": "esc_other", "script": [0x1B, ord("x")]},
    {"name": "eof", "script": []},
    {"name": "ctrl_then_char", "script": [0x15, ord("a")]},
]

PROMPT_CASES: list[dict] = [
    {"name": "ascii_short", "prompt": "输入", "keys": list("hello") + ["enter"]},
    {"name": "ascii_long", "prompt": "输入", "keys": list("x" * 40) + ["enter"]},
    {"name": "password", "prompt": "API key", "password": True, "keys": list("secret-value-1234") + ["enter"]},
    {"name": "cjk_value", "prompt": "输入", "keys": list("中文字符串测试内容很长很长") + ["enter"]},
    {"name": "backspace", "prompt": "输入", "keys": list("abc") + ["\b", "\b", "d"] + ["enter"]},
    {"name": "ctrl_u", "prompt": "输入", "keys": list("abcdefghij") + ["\x15", "x"] + ["enter"]},
    {"name": "empty", "prompt": "输入", "keys": ["enter"]},
    {"name": "exact_width", "prompt": "输入", "keys": list("y" * 24) + ["enter"]},
]


def run_key_windows(case: dict) -> dict:
    msvcrt = ScriptedMsvcrt(case["script"])
    clock = FakeTime()
    with mock.patch.object(tui, "msvcrt", msvcrt), mock.patch.object(tui, "time", clock):
        key = tui.read_key()
    return {
        "name": case["name"],
        "script": case["script"],
        "want_key": key,
        "want_consumed": msvcrt.index,
    }


def run_key_posix(case: dict) -> dict:
    os_stub = ScriptedOS(case["script"])
    select_stub = ScriptedSelect(os_stub)
    # Windows 上 tui.py 不会 import select（tui.py:13-18 的分支），因此用 create=True
    # 临时补一个同名模块属性，只为驱动 _read_posix_key_impl。
    with mock.patch.object(tui, "os", os_stub), \
            mock.patch.object(tui, "select", select_stub, create=True), \
            mock.patch.object(tui, "sys", ScriptedSys()):
        key = tui._read_posix_key_impl()
    return {
        "name": case["name"],
        "script": case["script"],
        "want_key": key,
        "want_consumed": os_stub.index,
    }


def run_prompt(case: dict) -> dict:
    """用脚本化按键跑真实的 prompt_text，抓出输入框的可见值。"""
    install_console(40, 12)
    captured: dict = {}

    def fake_terminal_frame(renderables, footer=None, preserve_bottom=False, **kwargs):
        captured["renderables"] = renderables
        return "FRAME"

    keys = list(case["keys"])
    side_effect = keys + ["cancel"]

    with mock.patch.object(tui, "terminal_frame", fake_terminal_frame), \
            mock.patch.object(tui, "read_key_responsive", side_effect=side_effect), \
            mock.patch.object(tui, "posix_input_mode", lambda: contextlib.nullcontext()), \
            mock.patch.object(tui, "Live", FakeLive):
        try:
            value = tui.prompt_text(
                "输入标题",
                case["prompt"],
                password=case.get("password", False),
            )
        except KeyboardInterrupt:
            value = None

    renderables = captured.get("renderables") or []
    visible = ""
    status = None
    for renderable in renderables:
        # section_panel(input_line, "输入", "cyan") 与 section_panel(status, "提示", "yellow")
        title = getattr(renderable, "title", None)
        content = getattr(renderable, "renderable", None)
        if title is not None and "输入" in str(title) and isinstance(content, Text):
            visible = content.plain.split("\n")[1].removesuffix("▌")
        elif title is not None and "提示" in str(title):
            status = content
    return {
        "name": case["name"],
        "prompt": case["prompt"],
        "password": case.get("password", False),
        "keys": case["keys"],
        "max_visible": max(40 - 14, 8),
        "want_visible": visible,
        "want_value": value,
    }


def build_corpus() -> dict:
    corpus: dict = {}

    corpus["markup"] = []
    corpus["markup_error"] = []
    for text in MARKUP_INPUTS:
        try:
            want = Text.from_markup(text).plain
        except Exception as exc:  # noqa: BLE001 - 记录 rich 的报错类型
            corpus["markup_error"].append({
                "input": text,
                "python_error": type(exc).__name__,
                "note": "rich 会抛 MarkupError；Go 侧 StripMarkup 不抛异常，只是尽力解析（刻意差异）。",
            })
            continue
        corpus["markup"].append({"input": text, "want": want})
    for text in ["[/]", "[red]", "[/bold]", "[]x[/]", "已闭合[/bold]"]:
        try:
            Text.from_markup(text)
        except Exception as exc:  # noqa: BLE001 - 记录 rich 的报错类型
            corpus["markup_error"].append({
                "input": text,
                "python_error": type(exc).__name__,
                "note": "rich 会抛 MarkupError；Go 侧 StripMarkup 不抛异常，只是尽力解析（刻意差异）。",
            })

    console = install_console(80, 25)
    corpus["width"] = [
        {"input": text, "want": console.measure(Text(text)).maximum}
        for text in WIDTH_INPUTS
    ]

    truncate_cases = []
    for text, width in [
        ("", 5), ("abc", 5), ("abcdef", 5), ("abcdef", 1), ("abcdef", 0),
        ("中文内容测试", 6), ("中文内容测试", 5), ("🎯🎯🎯", 4),
        ("a" * 10, 3), ("Ｈｅｌｌｏ", 4), ("abcdef", -1),
    ]:
        for ellipsis in (False, True):
            console = install_console(80, 25)
            options = console.options.update(width=max(width, 1))
            rendered = console.render_lines(
                Text(text, no_wrap=True, overflow="ellipsis" if ellipsis else None),
                options,
                pad=False,
            )
            truncate_cases.append({
                "input": text,
                "width": width,
                "ellipsis": ellipsis,
                "want": [ANSI.sub("", "".join(segment.text for segment in line)) for line in rendered],
            })
    corpus["truncate"] = truncate_cases

    corpus["wrap"] = []
    for text, width in WRAP_INPUTS:
        console = install_console(width, 25)
        rendered = console.render_lines(Text(text), console.options.update(width=width), pad=False)
        corpus["wrap"].append({
            "input": text,
            "width": width,
            "want": [ANSI.sub("", "".join(segment.text for segment in line)) for line in rendered],
        })

    corpus["panel"] = [
        {"name": case["name"], "width": case["width"], "spec": case["spec"],
         "want": render_spec_lines(case["spec"], case["width"], 25)}
        for case in PANEL_SPECS
    ]

    corpus["app_flag"] = [
        {"name": case["name"], "width": case["width"], "title": case["title"],
         "subtitle": case["subtitle"], "version": case["version"],
         "want": render_spec_lines(
             {"kind": "app_flag", "title": case["title"], "subtitle": case["subtitle"], "version": case["version"]},
             case["width"], 25)}
        for case in APP_FLAG_CASES
    ]

    corpus["menu"] = []
    for case in MENU_CASES:
        spec = ({"kind": "checkbox", "options": case["options"], "selected": case["selected"], "checked": case["checked"]}
                if case["checkbox"]
                else {"kind": "menu", "options": case["options"], "selected": case["selected"]})
        corpus["menu"].append({
            "name": case["name"],
            "width": case["width"],
            "checkbox": case["checkbox"],
            "options": case["options"],
            "selected": case["selected"],
            "checked": case["checked"],
            "diverges": "rich 表格用 expand=True 的列宽分配算法；Go 侧用固定列宽，信息等价、间距不同。",
            "want_python": render_spec_lines(spec, case["width"], 25),
        })

    corpus["fit"] = [
        {
            "lines": case["lines"],
            "height": case["height"],
            "preserve_bottom": case["preserve_bottom"],
            "want": [
                "".join(segment.text for segment in line)
                for line in tui.fit_terminal_lines(
                    [[__import__("rich.segment", fromlist=["Segment"]).Segment(text)] for text in case["lines"]],
                    case["height"],
                    case["preserve_bottom"],
                )
            ],
        }
        for case in FIT_CASES
    ]

    corpus["frame"] = []
    for case in FRAME_CASES:
        width, height = case["width"], case["height"]
        console = install_console(width, height)
        state = tui.terminal_frame_state(
            [build_renderable(spec) for spec in case["renderables"]],
            build_renderable(case["footer"]) if case["footer"] is not None else None,
            offset=case.get("offset", 0),
            focus_text=case.get("focus_text"),
            preserve_bottom=case.get("preserve_bottom", False),
            frame_title=case.get("frame_title", tui.WINDOW_TITLE),
        )
        lines = plain_lines(console, state.renderable)
        corpus["frame"].append({
            "name": case["name"],
            "width": width,
            "height": height,
            "offset": case.get("offset", 0),
            "focus_text": case.get("focus_text"),
            "preserve_bottom": case.get("preserve_bottom", False),
            "frame_title": case.get("frame_title", tui.WINDOW_TITLE),
            "renderables": case["renderables"],
            "footer": case["footer"],
            "want_lines": lines,
            "want_offset": state.offset,
            "want_max_offset": state.max_offset,
            "want_viewport_height": state.viewport_height,
        })

    corpus["scroll"] = [
        {"key": key, "offset": offset, "max_offset": max_offset, "viewport_height": viewport_height,
         "want": tui.content_scroll_offset(key, offset, max_offset, viewport_height)}
        for key, offset, max_offset, viewport_height in SCROLL_CASES
    ]

    corpus["wheel"] = []
    for key, last_key, last_at, now in WHEEL_CASES:
        clock = FakeTime(now=now, step=0.0)
        with mock.patch.object(tui, "time", clock):
            handled, new_key, new_at = tui.should_handle_wheel(key, last_key, last_at)
        corpus["wheel"].append({
            "key": key,
            "last_key": last_key,
            "last_at": last_at,
            "now": now,
            "want_handled": handled,
            "want_key": new_key,
            "want_at": new_at,
        })

    corpus["mouse"] = [
        {"sequence": sequence, "want_key": tui.parse_sgr_mouse_sequence(sequence)}
        for sequence in MOUSE_CASES
    ]

    corpus["viewport"] = []
    for height in (1, 2, 3, 5, 12, 25, 40):
        for option_count in (0, 1, 3, 4, 5, 10):
            install_console(40, height)
            corpus["viewport"].append({
                "height": height,
                "option_count": option_count,
                "want": tui.content_viewport_height(option_count),
            })

    corpus["key_windows"] = [run_key_windows(case) for case in KEY_WINDOWS_CASES]
    corpus["key_posix"] = [run_key_posix(case) for case in KEY_POSIX_CASES]
    corpus["prompt_visible"] = [run_prompt(case) for case in PROMPT_CASES]

    return corpus


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"


def count_cases(corpus: dict) -> int:
    total = 0
    for key, value in corpus.items():
        if isinstance(value, list):
            total += len(value)
        elif isinstance(value, dict):
            total += len(value)
    return total


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    path = args.output_dir / "tui_corpus.json"
    corpus = build_corpus()
    content = render(corpus)

    if args.check:
        if not path.exists():
            print(f"语料缺失: {path}", file=sys.stderr)
            return 1
        if path.read_text(encoding="utf-8") != content:
            print(f"语料已过期: {path}", file=sys.stderr)
            return 1
        print(f"语料最新: {path}（{count_cases(corpus)} 条用例）")
        return 0

    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8", newline="\n")
    print(f"已写入语料: {path}（{count_cases(corpus)} 条用例，{len(content.splitlines())} 行）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
