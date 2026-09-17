#!/usr/bin/env python3
"""生成 internal/clipboard 的对拍语料（Python 侧为参照实现）。

clipboard.py 的分支几乎全在「宿主环境」上（platform.system / shutil.which /
os.environ / sys.stdout / subprocess.run）。生成器把这些全局逐个替换成受控桩，
于是三个系统分支、命令回退顺序、失败文案都能在**同一台机器**上跑完，
Go 侧用注入的 Env 重放同一批输入。

语料记录的是纯数据（命令表、stdin、退回的文案、stdout 写出的内容），
因此与平台无关。

覆盖：

* ``clipboard_commands`` / ``paste_commands`` —— 三系统的命令表与 which 组合
* ``is_remote_terminal``                     —— SSH_* 变量存在但为空不算远程
* ``copy_to_terminal_clipboard``             —— OSC 52 序列与 stdout 失败回退
* ``copy_to_clipboard`` / ``paste_from_clipboard`` —— 回退顺序与最终文案

用法::

    python scripts/gen_clipboard_corpus.py           # 写入语料
    python scripts/gen_clipboard_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import shutil
import subprocess
import sys
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router import clipboard  # noqa: E402

DEFAULT_DIR = REPO_ROOT / "internal" / "clipboard" / "testdata"
WHICH_PREFIX = "/usr/bin/"


# --------------------------------------------------------------------------- #
# 桩
# --------------------------------------------------------------------------- #

class RecordingWriter:
    """替代 sys.stdout：记录写入内容，或按配置抛异常。"""

    def __init__(self, mode: str) -> None:
        self.mode = mode
        self.chunks: list[str] = []

    def write(self, text: str) -> int:
        if self.mode == "oserror":
            raise OSError("模拟写入失败")
        if self.mode == "valueerror":
            raise ValueError("模拟已关闭的流")
        self.chunks.append(text)
        return len(text)

    def flush(self) -> None:
        return None

    @property
    def written(self) -> str:
        return "".join(self.chunks)


def make_runner(plan: list[dict], calls: list[dict]):
    """按 plan 逐个返回结果；plan 用尽后继续被调用说明回退逻辑变了，直接失败。"""
    remaining = list(plan)

    def run(command, **kwargs):
        calls.append({
            "command": [str(part) for part in command],
            "stdin": kwargs.get("input") or "",
        })
        if not remaining:
            raise AssertionError(f"subprocess.run 调用次数超出计划: {command!r}")
        step = remaining.pop(0)
        if "error" in step:
            raise OSError(step["error"])
        return subprocess.CompletedProcess(
            command, step["code"], step.get("stdout", ""), step.get("stderr", "")
        )

    return run


# --------------------------------------------------------------------------- #
# 执行
# --------------------------------------------------------------------------- #

def run_case(case: dict) -> dict:
    system = case.get("system", "Linux")
    which_names = case.get("which") or []
    env_map = case.get("env") or {}
    stdout_mode = case.get("stdout_mode", "available")
    plan = case.get("plan") or []
    text = case.get("text", "")

    calls: list[dict] = []
    writer = RecordingWriter(stdout_mode)
    stdout_object = None if stdout_mode == "nil" else writer

    record: dict = {
        "name": case["name"],
        "op": case["op"],
        "system": system,
        "which": list(which_names),
        "env": env_map,
        "stdout_mode": stdout_mode,
    }
    if case.get("note"):
        record["note"] = case["note"]
    if "text" in case:
        record["text"] = text
    if plan:
        record["plan"] = plan

    with mock.patch.object(platform, "system", lambda: system), \
            mock.patch.object(
                shutil, "which",
                lambda name: f"{WHICH_PREFIX}{name}" if name in which_names else None,
            ), \
            mock.patch.object(subprocess, "run", make_runner(plan, calls)), \
            mock.patch.object(sys, "stdout", stdout_object), \
            mock.patch.dict(os.environ, env_map, clear=True):
        op = case["op"]
        if op == "clipboard_commands":
            record["want_commands"] = clipboard.clipboard_commands()
        elif op == "paste_commands":
            record["want_commands"] = clipboard.paste_commands()
        elif op == "is_remote_terminal":
            record["want_bool"] = clipboard.is_remote_terminal()
        elif op == "copy_to_terminal_clipboard":
            record["want_ok"] = clipboard.copy_to_terminal_clipboard(text)
            record["want_written"] = writer.written
        elif op == "copy_to_clipboard":
            ok, detail = clipboard.copy_to_clipboard(text)
            record["want_ok"] = ok
            record["want_detail"] = detail
            record["want_calls"] = calls
            record["want_written"] = writer.written
        elif op == "paste_from_clipboard":
            ok, detail = clipboard.paste_from_clipboard()
            record["want_ok"] = ok
            record["want_detail"] = detail
            record["want_calls"] = calls
        else:
            raise AssertionError(f"未知操作: {op}")

    return record


# --------------------------------------------------------------------------- #
# 用例
# --------------------------------------------------------------------------- #

def build_cases() -> list[dict]:
    cases: list[dict] = []

    def add(**kwargs) -> None:
        cases.append(kwargs)

    # ---------- clipboard_commands ----------
    add(name="write_windows_always_clip", op="clipboard_commands",
        system="Windows", which=[],
        note="Windows 固定用 clip，不查 which")
    add(name="write_darwin_with_pbcopy", op="clipboard_commands",
        system="Darwin", which=["pbcopy"])
    add(name="write_darwin_without_pbcopy", op="clipboard_commands",
        system="Darwin", which=["xclip"],
        note="macOS 不退化到 Linux 的命令")
    add(name="write_linux_all", op="clipboard_commands",
        system="Linux", which=["wl-copy", "xclip", "xsel"],
        note="三条都收集，按 wl-copy → xclip → xsel 的顺序")
    add(name="write_linux_only_xclip", op="clipboard_commands",
        system="Linux", which=["xclip"])
    add(name="write_linux_only_xsel", op="clipboard_commands",
        system="Linux", which=["xsel"])
    add(name="write_linux_only_wl_copy", op="clipboard_commands",
        system="Linux", which=["wl-copy"])
    add(name="write_linux_none", op="clipboard_commands",
        system="Linux", which=[])
    add(name="write_freebsd_falls_through", op="clipboard_commands",
        system="FreeBSD", which=["xsel"],
        note="非 Windows/Darwin 一律走 Linux 分支")
    add(name="write_empty_system", op="clipboard_commands",
        system="", which=["wl-copy"])

    # ---------- paste_commands ----------
    add(name="read_windows_all_powershells", op="paste_commands",
        system="Windows", which=["powershell", "powershell.exe", "pwsh"],
        note="每个能找到的可执行文件各生成一条命令")
    add(name="read_windows_only_pwsh", op="paste_commands",
        system="Windows", which=["pwsh"])
    add(name="read_windows_only_exe", op="paste_commands",
        system="Windows", which=["powershell.exe"])
    add(name="read_windows_none", op="paste_commands",
        system="Windows", which=[],
        note="一个都没有时仍返回默认 powershell 命令，让执行阶段报真实错误")
    add(name="read_darwin_with_pbpaste", op="paste_commands",
        system="Darwin", which=["pbpaste"])
    add(name="read_darwin_without_pbpaste", op="paste_commands",
        system="Darwin", which=["xsel"])
    add(name="read_linux_all", op="paste_commands",
        system="Linux", which=["wl-paste", "xclip", "xsel"])
    add(name="read_linux_only_xclip", op="paste_commands",
        system="Linux", which=["xclip"])
    add(name="read_linux_none", op="paste_commands", system="Linux", which=[])

    # ---------- is_remote_terminal ----------
    add(name="remote_no_env", op="is_remote_terminal", env={})
    add(name="remote_connection", op="is_remote_terminal",
        env={"SSH_CONNECTION": "10.0.0.1 51234 10.0.0.2 22"})
    add(name="remote_client", op="is_remote_terminal",
        env={"SSH_CLIENT": "10.0.0.1 51234 22"})
    add(name="remote_tty", op="is_remote_terminal", env={"SSH_TTY": "/dev/pts/0"})
    add(name="remote_empty_value_ignored", op="is_remote_terminal",
        env={"SSH_CONNECTION": "", "SSH_CLIENT": "", "SSH_TTY": ""},
        note="变量存在但为空串不算远程（Python 用真值判断）")
    add(name="remote_unrelated_env", op="is_remote_terminal",
        env={"PATH": "/usr/bin", "AMKR_REMOTE": "1", "SSH_AUTH_SOCK": "/tmp/x"},
        note="SSH_AUTH_SOCK 不在名单里")

    # ---------- copy_to_terminal_clipboard ----------
    add(name="osc52_ascii", op="copy_to_terminal_clipboard", text="hello",
        stdout_mode="available")
    add(name="osc52_empty_text", op="copy_to_terminal_clipboard", text="",
        stdout_mode="available")
    add(name="osc52_unicode", op="copy_to_terminal_clipboard",
        text="中文与 emoji 🎯", stdout_mode="available")
    add(name="osc52_control_chars", op="copy_to_terminal_clipboard",
        text="a\nb\x07c", stdout_mode="available")
    add(name="osc52_stdout_none", op="copy_to_terminal_clipboard", text="hello",
        stdout_mode="nil", note="sys.stdout is None → False")
    add(name="osc52_stdout_oserror", op="copy_to_terminal_clipboard", text="hello",
        stdout_mode="oserror", note="写入抛 OSError → False")
    add(name="osc52_stdout_valueerror", op="copy_to_terminal_clipboard", text="hello",
        stdout_mode="valueerror", note="写入抛 ValueError → False")

    # ---------- copy_to_clipboard ----------
    add(name="copy_empty_text", op="copy_to_clipboard", text="",
        system="Windows", stdout_mode="available",
        note="空文本直接拒绝，不查命令也不执行")
    add(name="copy_remote_stdout_ok", op="copy_to_clipboard", text="hello",
        system="Linux", which=[], env={"SSH_CONNECTION": "1"},
        stdout_mode="available",
        note="远程终端优先走 OSC 52，不执行任何外部命令")
    add(name="copy_remote_stdout_none_falls_back", op="copy_to_clipboard",
        text="hello", system="Windows", which=[], env={"SSH_TTY": "/dev/pts/1"},
        stdout_mode="nil", plan=[{"code": 0}],
        note="OSC 52 不可用时回退到外部命令")
    add(name="copy_remote_no_commands", op="copy_to_clipboard", text="hello",
        system="Darwin", which=[], env={"SSH_CLIENT": "1"},
        stdout_mode="nil", note="终端剪贴板与外部命令都不可用")
    add(name="copy_no_commands", op="copy_to_clipboard", text="hello",
        system="Linux", which=[], stdout_mode="available")
    add(name="copy_windows_success", op="copy_to_clipboard", text="hello 世界",
        system="Windows", which=[], stdout_mode="available",
        plan=[{"code": 0, "stdout": "", "stderr": ""}])
    add(name="copy_darwin_success", op="copy_to_clipboard", text="hello",
        system="Darwin", which=["pbcopy"], stdout_mode="available",
        plan=[{"code": 0}])
    add(name="copy_linux_first_error_then_success", op="copy_to_clipboard",
        text="hello", system="Linux", which=["wl-copy", "xclip"],
        stdout_mode="available",
        plan=[{"error": "模拟 OSError: 找不到 wl-copy"}, {"code": 0}],
        note="第一条抛异常记一条原因，继续试第二条")
    add(name="copy_all_fail_stderr", op="copy_to_clipboard", text="hello",
        system="Linux", which=["wl-copy", "xclip"], stdout_mode="available",
        plan=[{"code": 1, "stderr": "第一个错误"}, {"code": 2, "stderr": "第二个错误"}],
        note="最终只报最后一条原因")
    add(name="copy_failure_detail_from_stdout", op="copy_to_clipboard",
        text="hello", system="Linux", which=["xclip"], stdout_mode="available",
        plan=[{"code": 3, "stdout": "stdout 里的原因", "stderr": ""}])
    add(name="copy_failure_detail_exit_code", op="copy_to_clipboard", text="hello",
        system="Linux", which=["xclip"], stdout_mode="available",
        plan=[{"code": 4, "stdout": "", "stderr": ""}],
        note="stdout/stderr 都为空时用「退出码 N」")
    add(name="copy_failure_detail_whitespace", op="copy_to_clipboard",
        text="hello", system="Linux", which=["xclip"], stdout_mode="available",
        plan=[{"code": 5, "stdout": "不该被选中", "stderr": "   \n  "}],
        note="stderr 全是空白也算「有内容」，不会被 stdout 顶替，strip 后为空串")
    add(name="copy_failure_detail_multiline", op="copy_to_clipboard", text="hello",
        system="Linux", which=["xclip"], stdout_mode="available",
        plan=[{"code": 1, "stdout": "", "stderr": "\n  boom  \n"}],
        note="只 strip 首尾空白，中间的换行保留")

    # ---------- paste_from_clipboard ----------
    add(name="paste_no_commands", op="paste_from_clipboard", system="Darwin",
        which=[], stdout_mode="available")
    add(name="paste_windows_success", op="paste_from_clipboard", system="Windows",
        which=["powershell"], stdout_mode="available",
        plan=[{"code": 0, "stdout": "hello\n"}],
        note="返回值不 strip，保留末尾换行")
    add(name="paste_whitespace_only", op="paste_from_clipboard", system="Windows",
        which=["powershell"], stdout_mode="available",
        plan=[{"code": 0, "stdout": "  \n\t "}],
        note="全是空白算「没有可粘贴内容」")
    add(name="paste_empty_stdout", op="paste_from_clipboard", system="Windows",
        which=["powershell"], stdout_mode="available", plan=[{"code": 0}])
    add(name="paste_first_error_then_success", op="paste_from_clipboard",
        system="Windows", which=["powershell", "powershell.exe"],
        stdout_mode="available",
        plan=[{"error": "模拟 OSError: 找不到 powershell"},
              {"code": 0, "stdout": "from exe\n"}])
    add(name="paste_all_fail", op="paste_from_clipboard", system="Linux",
        which=["wl-paste", "xclip"], stdout_mode="available",
        plan=[{"code": 1, "stderr": ""}, {"code": 2, "stderr": "xclip 失败"}])
    add(name="paste_all_error", op="paste_from_clipboard", system="Linux",
        which=["xsel"], stdout_mode="available",
        plan=[{"error": "模拟 OSError: xsel 不可执行"}],
        note="全部抛异常时报最后一条异常文本")
    add(name="paste_unicode", op="paste_from_clipboard", system="Linux",
        which=["xclip"], stdout_mode="available",
        plan=[{"code": 0, "stdout": "剪贴板内容 🎯"}])
    add(name="paste_windows_none_falls_to_default", op="paste_from_clipboard",
        system="Windows", which=[], stdout_mode="available",
        plan=[{"error": "模拟 OSError: 找不到 powershell"}],
        note="默认命令失败时把错误报给用户")

    return cases


def build_corpus() -> dict:
    return {"cases": [run_case(case) for case in build_cases()]}


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    path = args.output_dir / "clipboard_corpus.json"
    content = render(build_corpus())

    if args.check:
        if not path.exists():
            print(f"语料缺失: {path}", file=sys.stderr)
            return 1
        if path.read_text(encoding="utf-8") != content:
            print(f"语料已过期: {path}", file=sys.stderr)
            return 1
        print(f"语料最新: {path}")
        return 0

    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8", newline="\n")
    print(f"已写入语料: {path} ({len(content.splitlines())} 行)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
