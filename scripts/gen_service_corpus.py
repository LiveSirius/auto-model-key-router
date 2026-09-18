#!/usr/bin/env python3
"""生成 ``auto_model_key_router.service``（service.py，720 行）的差分对拍语料。

设计要点：

1. **全部真实副作用都被换成脚本化桩**。这个模块会真的注册计划任务、写 systemd
   unit、杀进程，因此生成脚本把 ``subprocess.run`` / ``subprocess.Popen`` /
   ``urlopen`` / ``time`` / ``platform.system`` / ``Path.home`` / ``Path.cwd`` /
   ``os.getlogin`` / ``archive_current_log`` 全部替换成受控桩，只记录**决策结果**：
   执行了哪些命令、参数是什么、写了什么内容的面板。生成过程本身也不碰宿主系统。
2. **路径占位符**。面板与 unit 文本里会出现主目录、工作目录、配置路径、可执行文件
   与日志路径。这些值在 Go 测试里是临时目录，因此语料统一替换成
   ``$HOME`` / ``$CWD`` / ``$CONFIG`` / ``$EXE`` / ``$LOG`` / ``$BIN``，
   Go 侧把实际值替换回占位符再比对（与 gen_ops_api_corpus.py 同一套做法）。
3. **面板用受控 Console 渲染**。``internal/tui`` 的富文本降级模型与 rich 在
   ``force_terminal=True, safe_box=False, color_system=None, width=100`` 下逐字节一致
   （gen_tui_corpus.py 已锁定），因此这里的面板文本可以直接与 Go 的
   ``service.RenderText`` 比较。唯一不对拍版式的是系统服务状态表（Go 用固定列宽），
   该段只记录**行数据**。
   服务动作分派走**真实的** ``ops_api._run_service_action``，只把它的 ``_render_text``
   换成同一个受控渲染器，这样分派表与拼接逻辑（``"\n\n".join``）也是对拍对象。
4. **刻意不覆盖的路径**（无法安全执行、也无法对拍，Go 侧由 fake 接缝测试覆盖）：
   真实 ``Popen`` 创建进程、真实 ``taskkill``、真实 ``signal``、真实 UAC 提权窗口、
   ``ctypes.windll.shell32.IsUserAnAdmin``。语料记录的是这四条路径**之前**的决策
   （命令行、标志位、脚本文本）。

用法::

    python -X utf8 scripts/gen_service_corpus.py
    python -X utf8 scripts/gen_service_corpus.py --check
"""

from __future__ import annotations

import argparse
import io
import json
import re
import shutil
import subprocess
import sys
import tempfile
from contextlib import ExitStack
from dataclasses import replace
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from rich.console import Console  # noqa: E402

from auto_model_key_router import ops_api, service, service_status  # noqa: E402
from auto_model_key_router.config import (  # noqa: E402
    RouterConfig,
    empty_config_dict,
    save_config_data,
)

DEFAULT_PATH = REPO_ROOT / "internal" / "service" / "testdata" / "service_corpus.json"
CORPUS_VERSION = 1

BASE_DIR = Path(tempfile.gettempdir()) / "amkr_service_corpus"
HOME = BASE_DIR / "home"
CWD = BASE_DIR / "cwd"
CONFIG = BASE_DIR / "router-config.json"
EXE = BASE_DIR / "python" / "python.exe"
LOG = BASE_DIR / "logs" / "server.log"
BIN = BASE_DIR / "bin" / "amkr"
ARCHIVE = BASE_DIR / "logs" / "server.20260102-030405.log"
PID_FILE = LOG.with_name("server.pid")
SERVICE_UNIT = HOME / ".config" / "systemd" / "user" / service.SYSTEMD_USER_SERVICE_NAME

PLACEHOLDERS: dict[str, Path] = {
    "$HOME": HOME,
    "$CWD": CWD,
    "$CONFIG": CONFIG,
    "$EXE": EXE,
    "$LOG": LOG,
    "$BIN": BIN,
    "$ARCHIVE": ARCHIVE,
}

ANSI = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07")


# --------------------------------------------------------------------------- #
# 受控渲染与占位符
# --------------------------------------------------------------------------- #

def render(renderable) -> str:
    """把 rich 渲染对象转成纯文本（去 ANSI、去首尾空白），并做路径占位符替换。"""
    console = Console(
        width=100,
        height=25,
        file=io.StringIO(),
        force_terminal=True,
        color_system=None,
        legacy_windows=False,
        safe_box=False,
        record=True,
    )
    console.print(renderable)
    text = ANSI.sub("", console.export_text()).strip()
    return subst(text)


def subst(value) -> str:
    text = str(value)
    for placeholder, real in sorted(PLACEHOLDERS.items(), key=lambda item: -len(str(item[1]))):
        text = text.replace(str(real), placeholder)
    return text


# --------------------------------------------------------------------------- #
# 脚本化桩
# --------------------------------------------------------------------------- #

class FakeRun:
    """替换 subprocess.run：按脚本返回（脚本耗尽后重复最后一条），并记录命令。"""

    def __init__(self, script: list[dict] | None = None):
        self.script = [dict(entry) for entry in (script or [])]
        self.calls: list[list[str]] = []

    def __call__(self, command, *args, **kwargs):
        self.calls.append([str(part) for part in command])
        if not self.script:
            entry: dict = {}
        elif len(self.script) == 1:
            entry = self.script[0]
        else:
            entry = self.script.pop(0)
        return subprocess.CompletedProcess(
            command,
            int(entry.get("code", 0)),
            entry.get("stdout", ""),
            entry.get("stderr", ""),
        )


class FakeProcess:
    def __init__(self, pid: int):
        self.pid = pid


class FakePopen:
    """替换 subprocess.Popen：只记录参数，不创建进程。"""

    def __init__(self, pid: int = 4242):
        self.pid = pid
        self.calls: list[dict] = []

    def __call__(self, command, **kwargs):
        recorded = {"command": [str(part) for part in command]}
        for key, value in kwargs.items():
            if key in {"stdout", "stderr", "stdin"}:
                recorded[key] = value is not None
            elif key == "env":
                recorded["env"] = dict(value)
            else:
                recorded[key] = value
        self.calls.append(recorded)
        return FakeProcess(self.pid)


class FakeResponse:
    def __init__(self, status: int, body: str):
        self.status = status
        self._body = body.encode("utf-8")

    def read(self) -> bytes:
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class FakeURL:
    """替换 urlopen：按脚本返回响应或抛异常。"""

    def __init__(self, script: list[dict]):
        self.script = [dict(entry) for entry in script]
        self.calls: list[dict] = []

    def __call__(self, url, timeout=None, **kwargs):
        self.calls.append({"url": str(url), "timeout": timeout})
        if not self.script:
            entry: dict = {"status": 200, "body": "{}"}
        elif len(self.script) == 1:
            entry = self.script[0]
        else:
            entry = self.script.pop(0)
        error = entry.get("error")
        if error == "oserror":
            raise OSError(error)
        if error == "urlerror":
            raise service.URLError(error)
        if error == "valueerror":
            raise ValueError(error)
        return FakeResponse(int(entry.get("status", 200)), entry.get("body", "{}"))


class FakeClock:
    """替换 time：monotonic 由脚本给出，sleep 只记录时长。"""

    def __init__(self, values: list[float] | None = None):
        self.values = list(values or [])
        self.index = 0
        self.slept: list[float] = []

    def monotonic(self) -> float:
        if self.index < len(self.values):
            value = self.values[self.index]
            self.index += 1
            return value
        return self.values[-1] if self.values else 0.0

    def sleep(self, seconds: float) -> None:
        self.slept.append(seconds)


class FakeArchive:
    """替换 archive_current_log：返回脚本化结果并记录调用。"""

    def __init__(self, result: str | None = None):
        self.result = result
        self.calls: list[str] = []

    def __call__(self, log_file_path):
        self.calls.append(str(log_file_path))
        return self.result


class Sandbox:
    """把 service 模块的全部外部依赖替换成桩，退出时还原。"""

    def __init__(
        self,
        *,
        platform_name: str = "Windows",
        is_admin: bool = True,
        run_script: list[dict] | None = None,
        url_script: list[dict] | None = None,
        clock_values: list[float] | None = None,
        popen_pid: int = 4242,
        archive_result: str | None = None,
        console_script: Path | None = None,
        user: str = "amkr-user",
    ):
        self.platform_name = platform_name
        self.is_admin = is_admin
        self.user = user
        self.run = FakeRun(run_script)
        self.url = FakeURL(url_script if url_script is not None else [{"error": "oserror"}])
        self.clock = FakeClock(clock_values)
        self.popen = FakePopen(popen_pid)
        self.archive = FakeArchive(archive_result)
        self.console_script = console_script
        self._stack = ExitStack()

    def __enter__(self) -> "Sandbox":
        stack = self._stack
        # 健康检查缓存是**模块级**字典（service.py:39），跨用例会互相污染；每个
        # sandbox 都从「空缓存的新进程」开始，语料才不依赖用例顺序。
        service._service_status_cache.clear()
        stack.enter_context(mock.patch.object(service.subprocess, "run", self.run))
        stack.enter_context(mock.patch.object(service.subprocess, "Popen", self.popen))
        stack.enter_context(mock.patch.object(service, "urlopen", self.url))
        stack.enter_context(mock.patch.object(service, "time", self.clock))
        stack.enter_context(mock.patch.object(service.platform, "system", lambda: self.platform_name))
        stack.enter_context(mock.patch.object(service, "is_windows_admin", lambda: self.is_admin))
        stack.enter_context(mock.patch.object(service, "archive_current_log", self.archive))
        stack.enter_context(mock.patch.object(service, "background_python_executable", lambda: EXE))
        stack.enter_context(
            mock.patch.object(
                service,
                "console_script_executable",
                lambda: self.console_script.resolve() if self.console_script else None,
            )
        )
        stack.enter_context(mock.patch.object(Path, "home", classmethod(lambda cls: HOME)))
        stack.enter_context(mock.patch.object(Path, "cwd", classmethod(lambda cls: CWD)))
        stack.enter_context(mock.patch.object(service.os, "getlogin", lambda: self.user))
        # sys.executable 出现在 UAC 提权脚本里（service.py:484）；不固定它语料就会随
        # 生成机的 Python 安装路径变化，--check 在任何别的机器上都失效。
        stack.enter_context(mock.patch.object(service.sys, "executable", str(EXE)))
        return self

    def __exit__(self, *exc):
        self._stack.close()
        return False


def prepare_filesystem() -> None:
    """重建固定夹具目录（每次生成前清空，保证语料稳定）。"""
    if BASE_DIR.exists():
        shutil.rmtree(BASE_DIR, ignore_errors=True)
    for directory in (HOME, CWD, EXE.parent, LOG.parent, BIN.parent):
        directory.mkdir(parents=True, exist_ok=True)

    data = empty_config_dict()
    data["log_file_path"] = str(LOG)
    data["metrics_db_path"] = str(BASE_DIR / "metrics.db")
    data["endpoint_capabilities_path"] = str(BASE_DIR / "endpoint_capabilities.json")
    data["local_api_key"] = "amkr-corpus-key"
    data["host"] = "127.0.0.1"
    data["port"] = 8123
    save_config_data(CONFIG, data)


def tasklist_stdout(pid: int) -> str:
    return f'"python.exe","{pid}","Console","1","10,000 K"\n'


# --------------------------------------------------------------------------- #
# 语料各段
# --------------------------------------------------------------------------- #

def pid_file_path_cases() -> list[dict]:
    cases = []
    for log_path in ("C:/data/logs/server.log", "logs/server.log", "server.log", "/var/log/amkr/router.log"):
        config = replace(RouterConfig.load(CONFIG), log_file_path=log_path)
        cases.append({"log_file_path": log_path, "expected": subst(service.pid_file_path(config))})
    return cases


def read_pid_cases() -> list[dict]:
    target = BASE_DIR / "read-pid.txt"
    cases = []
    for content in ("4242", "  4242\n", "+7", "-7", "", "abc", "42 43", "0"):
        target.write_text(content, encoding="utf-8")
        cases.append({"content": content, "expected": service.read_pid(target)})
    target.write_text("1", encoding="utf-8")
    target.unlink()
    cases.append({"content": None, "expected": service.read_pid(target)})
    return cases


def process_running_windows_cases() -> list[dict]:
    samples = [
        tasklist_stdout(4242),
        '"python.exe","993","Console","1","10 K"\n',
        "INFO: No tasks are running which match the specified criteria.\n",
        "",
        '"python.exe","42420","Console","1","1 K"\n',
    ]
    cases = []
    with Sandbox() as sandbox:
        for pid in (4242, 42420, 1):
            for sample in samples:
                sandbox.run.script = [{"code": 0, "stdout": sample}]
                expected = service.is_process_running(pid)
                cases.append(
                    {
                        "pid": pid,
                        "tasklist_stdout": sample,
                        "expected": expected,
                        "command": sandbox.run.calls[-1],
                    }
                )
    return cases


def powershell_quote_cases() -> list[dict]:
    values = ["C:/Program Files/amkr/amkr.exe", "it's", "", "a'b'c", "中文 路径", "$env:PATH"]
    return [{"value": value, "expected": service.powershell_quote(value)} for value in values]


def shlex_join_cases() -> list[dict]:
    argument_sets = [
        [str(BIN), "--config", str(CONFIG), "--serve-foreground"],
        ["/opt/My App/bin/amkr", "--config", "/tmp/a b/c.json"],
        ["/opt/it's/amkr", "--config", "/tmp/'x'.json"],
        ["/usr/bin/amkr", "--config", "中文目录/配置.json"],
        [],
        [""],
        ["--flag=value", "a:b,c.d/e-f_g@h%i+j", "~tilde"],
    ]
    return [{"args": args, "expected": subst(service.__dict__["shlex"].join(args))} for args in argument_sets]


def windows_registered_cases() -> list[dict]:
    cases = []
    for list_code, xml_code in ((0, 0), (0, 1), (1, 0), (1, 1)):
        with Sandbox(run_script=[{"code": list_code}, {"code": xml_code}]) as sandbox:
            cases.append(
                {
                    "list_code": list_code,
                    "xml_code": xml_code,
                    "expected": service.is_windows_task_registered(),
                    "commands": sandbox.run.calls,
                }
            )
    return cases


def windows_manage_cases() -> list[dict]:
    actions = [
        "install-user",
        "install",
        "uninstall",
        "start",
        "stop",
        "restart",
        "status",
        "install-elevated",
        "start-elevated",
        "status-elevated",
    ]
    cases = []
    for action in actions:
        for is_admin in (True, False):
            if action.endswith("-elevated"):
                responses = [{"code": 0, "stdout": f"OK {action}"}]
            elif not is_admin and action in {"install", "uninstall", "start", "stop", "restart"}:
                responses = [{"code": 0, "stdout": f"OK {action}"}]
            else:
                responses = [{"code": 0, "stdout": f"OK {action}"} for _ in range(4)]
            PID_FILE.unlink(missing_ok=True)
            with Sandbox(is_admin=is_admin, run_script=responses, archive_result=None) as sandbox:
                try:
                    result = service.manage_windows_task(EXE.resolve(), CONFIG, action)
                    panel, error = render(result), None
                except Exception as exc:  # pragma: no cover - 只用于记录异常类型
                    panel, error = None, type(exc).__name__
                cases.append(
                    {
                        "action": action,
                        "is_admin": is_admin,
                        "executable": str(EXE),
                        "config_path": str(CONFIG),
                        "run_script": responses,
                        "commands": [[subst(part) for part in call] for call in sandbox.run.calls],
                        "panel": panel,
                        "error": error,
                    }
                )
    PID_FILE.unlink(missing_ok=True)
    with Sandbox(is_admin=True) as sandbox:
        error = None
        try:
            service.manage_windows_task(EXE.resolve(), CONFIG, "bogus")
        except Exception as exc:
            error = type(exc).__name__
        cases.append({"action": "bogus", "is_admin": True, "commands": [], "panel": None, "error": error})
    return cases


def elevation_cases() -> list[dict]:
    cases = []
    for action in ("install", "uninstall", "start", "stop", "restart"):
        for code, stdout, stderr in ((0, "", ""), (1, "", "用户取消"), (1, "fallback", ""), (1, "", "")):
            with Sandbox(
                is_admin=False, run_script=[{"code": code, "stdout": stdout, "stderr": stderr}]
            ) as sandbox:
                panel = service.manage_windows_task(EXE.resolve(), CONFIG, action)
                cases.append(
                    {
                        "action": action,
                        "result_code": code,
                        "result_stdout": stdout,
                        "result_stderr": stderr,
                        "commands": [[subst(part) for part in call] for call in sandbox.run.calls],
                        "panel": render(panel),
                    }
                )
    return cases


def console_script_cases() -> list[dict]:
    cases = []
    existing = BASE_DIR / "bin" / "installed-amkr"
    existing.parent.mkdir(parents=True, exist_ok=True)
    existing.write_text("", encoding="utf-8")
    for argv0, which_map in (
        (str(existing), {}),
        ("amkr", {"amkr": str(BIN)}),
        ("main.py", {"amkr": str(BIN)}),
        ("main.py", {"auto-model-key-router": str(existing)}),
        ("main.py", {}),
        ("auto-model-key-router", {}),
    ):
        with mock.patch.object(service.sys, "argv", [argv0]), mock.patch.object(
            service.shutil, "which", lambda name, mapping=which_map: mapping.get(name)
        ):
            result = service.console_script_executable()
        cases.append(
            {
                "argv0": subst(argv0),
                "which": {key: subst(value) for key, value in which_map.items()},
                "expected": subst(result) if result is not None else None,
            }
        )
    return cases


def systemd_registered_cases() -> list[dict]:
    cases = []
    for file_exists in (False, True):
        for stdout in ("LoadState=loaded\n", "LoadState=not-found\n", "LoadState=\n", ""):
            if file_exists:
                SERVICE_UNIT.parent.mkdir(parents=True, exist_ok=True)
                SERVICE_UNIT.write_text("[Unit]\n", encoding="utf-8")
            else:
                SERVICE_UNIT.unlink(missing_ok=True)
            with Sandbox(platform_name="Linux", run_script=[{"code": 0, "stdout": stdout}]) as sandbox:
                cases.append(
                    {
                        "file_exists": file_exists,
                        "show_stdout": stdout,
                        "run_script": [{"code": 0, "stdout": stdout}],
                        "expected": service.is_systemd_user_service_registered(),
                        "commands": sandbox.run.calls,
                    }
                )
    SERVICE_UNIT.unlink(missing_ok=True)
    return cases


def systemd_manage_cases() -> list[dict]:
    cases = []
    for action in ("install", "uninstall", "start", "stop", "restart", "status"):
        responses = [{"code": 0, "stdout": f"OK {action}"} for _ in range(6)]
        PID_FILE.unlink(missing_ok=True)
        SERVICE_UNIT.unlink(missing_ok=True)
        with Sandbox(
            platform_name="Linux", run_script=responses, console_script=None, archive_result=None
        ) as sandbox:
            try:
                result = service.manage_systemd_user_service(EXE.resolve(), CONFIG, action)
                panel, error = render(result), None
            except Exception as exc:  # pragma: no cover
                panel, error = None, type(exc).__name__
            unit_text = SERVICE_UNIT.read_text(encoding="utf-8") if SERVICE_UNIT.exists() else None
            cases.append(
                {
                    "action": action,
                    "executable": str(EXE),
                    "config_path": str(CONFIG),
                    "run_script": responses,
                    "commands": [[subst(part) for part in call] for call in sandbox.run.calls],
                    "unit_text": subst(unit_text) if unit_text is not None else None,
                    "panel": panel,
                    "error": error,
                }
            )
        SERVICE_UNIT.unlink(missing_ok=True)
    # 额外的 unit 文本形态：console script 命中 / 路径含空格与单引号。
    for console_script in (BIN, CWD / "My App" / "bin" / "amkr", CWD / "it's" / "amkr"):
        SERVICE_UNIT.unlink(missing_ok=True)
        extra_responses = [{"code": 0, "stdout": "OK"} for _ in range(6)]
        with Sandbox(
            platform_name="Linux",
            run_script=extra_responses,
            console_script=console_script,
            archive_result=None,
        ) as sandbox:
            service.manage_systemd_user_service(EXE.resolve(), CONFIG, "install")
            cases.append(
                {
                    "action": "install",
                    "executable": str(EXE),
                    "config_path": str(CONFIG),
                    "console_script": subst(console_script),
                    "run_script": extra_responses,
                    "unit_text": subst(SERVICE_UNIT.read_text(encoding="utf-8")),
                    "commands": [[subst(part) for part in call] for call in sandbox.run.calls],
                    "panel": None,
                    "error": None,
                }
            )
        SERVICE_UNIT.unlink(missing_ok=True)
    return cases


def systemd_service_command_cases() -> list[dict]:
    cases = []
    for console_script in (None, BIN):
        with Sandbox(platform_name="Linux", console_script=console_script):
            cases.append(
                {
                    "console_script": subst(console_script) if console_script else None,
                    "config_path": str(CONFIG),
                    "executable": str(EXE),
                    "expected": [subst(part) for part in service.systemd_service_command(EXE, CONFIG)],
                }
            )
    return cases


def background_start_cases() -> list[dict]:
    cases = []
    scenarios = [
        {"healthy": True, "existing_pid": None, "archived": None},
        {"healthy": False, "existing_pid": None, "archived": None},
        {"healthy": False, "existing_pid": 777, "archived": str(ARCHIVE)},
    ]
    for scenario in scenarios:
        url_script = (
            [{"status": 200, "body": '{"status":"ok"}'}]
            if scenario["healthy"]
            else [{"error": "oserror"}]
        )
        run_script = []
        if scenario["existing_pid"] is not None:
            run_script.append({"code": 0, "stdout": tasklist_stdout(scenario["existing_pid"])})
        PID_FILE.unlink(missing_ok=True)
        if scenario["existing_pid"] is not None and not scenario["healthy"]:
            PID_FILE.write_text(str(scenario["existing_pid"]), encoding="utf-8")
        with Sandbox(
            run_script=run_script, url_script=url_script, archive_result=scenario["archived"]
        ) as sandbox:
            panel = service.start_service_background(CONFIG, RouterConfig.load(CONFIG))
            cases.append(
                {
                    "healthy": scenario["healthy"],
                    "existing_pid": scenario["existing_pid"],
                    "archive_result": subst(scenario["archived"]) if scenario["archived"] else None,
                    "run_script": run_script,
                    "responses": url_script,
                    "commands": [[subst(part) for part in call] for call in sandbox.run.calls],
                    "url_calls": [subst(call["url"]) for call in sandbox.url.calls],
                    "spawn": spawn_records(sandbox.popen.calls),
                    "archive_calls": [subst(call) for call in sandbox.archive.calls],
                    "pid_file": PID_FILE.read_text(encoding="utf-8") if PID_FILE.exists() else None,
                    "panel": render(panel),
                }
            )
        PID_FILE.unlink(missing_ok=True)
    return cases


def spawn_records(calls: list[dict]) -> list[dict]:
    records = []
    for call in calls:
        env = call.get("env") or {}
        records.append(
            {
                "command": [subst(part) for part in call["command"]],
                "cwd": subst(call.get("cwd")),
                "has_stdout": bool(call.get("stdout")),
                "has_stderr": bool(call.get("stderr")),
                "has_stdin": bool(call.get("stdin")),
                "start_new_session": bool(call.get("start_new_session", False)),
                "creationflags": int(call.get("creationflags", 0)),
                "close_fds": bool(call.get("close_fds", False)),
                "log_archived_env": env.get("AMKR_LOG_ARCHIVED"),
            }
        )
    return records


def background_stop_cases() -> list[dict]:
    cases = []
    # checks 是「每次 tasklist 看到的存活状态」序列；脚本耗尽后重复最后一条，
    # 因此 [True] 表示进程永远杀不掉（走满 20 次轮询）。
    scenarios = [
        {"pid": None, "checks": []},
        {"pid": 4242, "checks": [False]},
        {"pid": 4242, "checks": [True, False]},
        {"pid": 4242, "checks": [True, True]},
    ]
    for scenario in scenarios:
        PID_FILE.unlink(missing_ok=True)
        if scenario["pid"] is not None:
            PID_FILE.write_text(str(scenario["pid"]), encoding="utf-8")
        run_script = []
        for index, running in enumerate(scenario["checks"]):
            if index > 0:
                run_script.append({"code": 0, "stdout": ""})  # taskkill
            run_script.append(
                {"code": 0, "stdout": tasklist_stdout(scenario["pid"]) if running else ""}
            )
        with Sandbox(run_script=run_script) as sandbox:
            panel = service.stop_background_service(RouterConfig.load(CONFIG))
            cases.append(
                {
                    "pid_file": scenario["pid"],
                    "checks": scenario["checks"],
                    "run_script": run_script,
                    "commands": [[subst(part) for part in call] for call in sandbox.run.calls],
                    "sleeps": sandbox.clock.slept,
                    "pid_file_exists": PID_FILE.exists(),
                    "panel": render(panel),
                }
            )
        PID_FILE.unlink(missing_ok=True)
    return cases


def health_cases() -> list[dict]:
    cases = []
    detail = '{"config_path":"%s","local_api_key_fingerprint":"abcd1234"}' % CONFIG
    scenarios = [
        {"host": "127.0.0.1", "port": 8123, "responses": [{"status": 200, "body": "{}"}, {"status": 200, "body": detail}]},
        {"host": "0.0.0.0", "port": 8123, "responses": [{"status": 200, "body": "{}"}, {"status": 200, "body": '{"status":"ok"}'}]},
        {"host": "::", "port": 9999, "responses": [{"status": 200, "body": "{}"}, {"status": 200, "body": "not json"}]},
        {"host": "127.0.0.1", "port": 8123, "responses": [{"status": 500, "body": "boom"}]},
        {"host": "127.0.0.1", "port": 8123, "responses": [{"error": "oserror"}]},
        {"host": "127.0.0.1", "port": 8123, "responses": [{"status": 200, "body": "{}"}, {"status": 200, "body": "[1,2]"}]},
        {"host": "127.0.0.1", "port": 8123, "responses": [{"status": 200, "body": "{}"}, {"status": 200, "body": '{"config_path":123,"local_api_key_fingerprint":null}'}]},
        {"host": "127.0.0.1", "port": 8123, "responses": [{"status": 200, "body": "{}"}, {"error": "valueerror"}]},
    ]
    for scenario in scenarios:
        with Sandbox(url_script=scenario["responses"]) as sandbox:
            healthy = service.is_service_healthy(scenario["host"], scenario["port"], use_cache=False)
            result = service.service_health(scenario["host"], scenario["port"], use_cache=False)
            cases.append(
                {
                    "host": scenario["host"],
                    "port": scenario["port"],
                    "responses": scenario["responses"],
                    "healthy": healthy,
                    "service_health": result is not None,
                    "config_path": subst(result.get("config_path") or "") if result else None,
                    "fingerprint": (result.get("local_api_key_fingerprint") or "") if result else None,
                    "url_calls": [
                        {"url": subst(call["url"]), "timeout": call["timeout"]} for call in sandbox.url.calls
                    ],
                }
            )
    return cases


def health_cache_cases() -> list[dict]:
    """TTL=2s：t=0 探测 → 命中；t=0.5 命中缓存（不再探测）；t=3.0 过期重新探测。"""
    case = {
        "host": "127.0.0.1",
        "port": 8123,
        "clock": [0.0, 0.5, 3.0],
        "responses": [{"status": 200, "body": "{}"}, {"error": "oserror"}],
        "expected": [],
        "url_calls": [],
    }
    with Sandbox(url_script=case["responses"], clock_values=case["clock"]) as sandbox:
        for _ in range(3):
            case["expected"].append(service.is_service_healthy(case["host"], case["port"], use_cache=True))
        case["url_calls"] = [subst(call["url"]) for call in sandbox.url.calls]
    return [case]


def background_status_cases() -> list[dict]:
    cases = []
    scenarios = [
        {"healthy": False, "config_path_field": None, "fingerprint": None},
        {"healthy": True, "config_path_field": str(CONFIG), "fingerprint": "abcd1234"},
        {"healthy": True, "config_path_field": str(BASE_DIR / "other-config.json"), "fingerprint": ""},
    ]
    for scenario in scenarios:
        if scenario["healthy"]:
            body = '{"config_path":"%s","local_api_key_fingerprint":"%s"}' % (
                scenario["config_path_field"] or "",
                scenario["fingerprint"] or "",
            )
            url_script = [{"status": 200, "body": "{}"}, {"status": 200, "body": body}]
        else:
            url_script = [{"error": "oserror"}]
        with Sandbox(url_script=url_script) as sandbox:
            panel = service.background_status_panel(RouterConfig.load(CONFIG), CONFIG)
            cases.append(
                {
                    "healthy": scenario["healthy"],
                    "config_path_field": subst(scenario["config_path_field"]) if scenario["config_path_field"] else None,
                    "fingerprint": scenario["fingerprint"],
                    "responses": url_script,
                    "panel": render(panel),
                    "url_calls": [subst(call["url"]) for call in sandbox.url.calls],
                }
            )
    return cases


def note_cases() -> list[dict]:
    cases = []
    for platform_name in ("Windows", "Linux", "Darwin"):
        with Sandbox(platform_name=platform_name):
            cases.append(
                {"platform": platform_name, "expected": render(service.service_registration_note_panel())}
            )
    return cases


def status_render_cases() -> list[dict]:
    cases = []
    rows = (
        ("平台", "Windows 计划任务"),
        ("任务名", "AutoModelKeyRouter"),
        ("注册状态", ""),
        ("运行状态", "Ready"),
        ("执行程序", "C:\\Program Files\\amkr\\amkr.exe"),
    )
    for registered in (True, False):
        status = service.SystemServiceStatus(
            registered=registered,
            rows=rows,
            details=(
                service_status.StatusDetail("Windows 原始状态", "RAW TEXT", "blue"),
                service_status.StatusDetail("其他", "未找到计划任务，或 schtasks 不可用。", "yellow"),
            ),
        )
        cases.append(
            {
                "registered": registered,
                "rows": [[label, value] for label, value in rows],
                "expected_rows": [
                    [
                        label,
                        (
                            ("[green]已注册[/green]" if registered else "[yellow]未注册[/yellow]")
                            if label == "注册状态"
                            else value
                        ),
                    ]
                    for label, value in rows
                ],
                "detail_panels": [
                    render(service.section_panel(detail.content, detail.title, detail.style))
                    for detail in status.details
                ],
            }
        )
    return cases


def run_service_action_cases() -> list[dict]:
    scenarios = [
        {
            "action": "start_amkr",
            "kind": "background-start",
            "url_script": [{"error": "oserror"}],
            "run_script": [],
            "archive": None,
        },
        {
            "action": "stop_amkr",
            "kind": "background-stop",
            "url_script": [],
            "run_script": [
                {"code": 0, "stdout": tasklist_stdout(4242)},
                {"code": 0, "stdout": ""},
                {"code": 0, "stdout": ""},
            ],
            "archive": None,
            "pid": 4242,
        },
        {
            "action": "restart_amkr",
            "kind": "background-restart",
            "url_script": [{"error": "oserror"}],
            "run_script": [
                {"code": 0, "stdout": tasklist_stdout(4242)},
                {"code": 0, "stdout": ""},
                {"code": 0, "stdout": ""},
            ],
            "archive": None,
            "pid": 4242,
        },
        {"action": "status_amkr", "kind": "system", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"}], "archive": None},
        {"action": "install_user_amkr", "kind": "system", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"} for _ in range(3)], "archive": None},
        {"action": "install_system_amkr", "kind": "system", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"} for _ in range(3)], "archive": None},
        {"action": "uninstall_system_amkr", "kind": "system", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"} for _ in range(2)], "archive": None},
        {"action": "start_system_amkr", "kind": "system", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"}], "archive": None},
        {"action": "stop_system_amkr", "kind": "system", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"}], "archive": None},
        {"action": "restart_system_amkr", "kind": "system", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"} for _ in range(2)], "archive": None},
        {"action": "install_system_amkr", "kind": "system", "platform": "Linux", "url_script": [], "run_script": [{"code": 0, "stdout": "OK"} for _ in range(3)], "archive": None},
        {"action": "bogus_action", "kind": None, "url_script": [], "run_script": [], "archive": None, "error": "KeyError"},
    ]
    cases = []
    for scenario in scenarios:
        PID_FILE.unlink(missing_ok=True)
        SERVICE_UNIT.unlink(missing_ok=True)
        if scenario.get("pid"):
            PID_FILE.write_text(str(scenario["pid"]), encoding="utf-8")
        with Sandbox(
            platform_name=scenario.get("platform", "Windows"),
            run_script=scenario["run_script"],
            url_script=scenario["url_script"],
            archive_result=scenario["archive"],
            console_script=None,
        ) as sandbox:
            with mock.patch.object(ops_api, "_render_text", render):
                error = None
                try:
                    text = ops_api._run_service_action(scenario["action"], CONFIG, RouterConfig.load(CONFIG))
                except Exception as exc:
                    text, error = None, type(exc).__name__
            cases.append(
                {
                    "action": scenario["action"],
                    "kind": scenario["kind"],
                    "platform": scenario.get("platform", "Windows"),
                    "pid": scenario.get("pid"),
                    "run_script": scenario["run_script"],
                    "responses": scenario["url_script"],
                    "commands": [[subst(part) for part in call] for call in sandbox.run.calls],
                    "text": subst(text) if text is not None else None,
                    "error": error,
                }
            )
        PID_FILE.unlink(missing_ok=True)
        SERVICE_UNIT.unlink(missing_ok=True)
    return cases


# --------------------------------------------------------------------------- #
# 主流程
# --------------------------------------------------------------------------- #

def build_corpus() -> dict:
    prepare_filesystem()
    return {
        "corpus_version": CORPUS_VERSION,
        "placeholders": {key: str(value) for key, value in PLACEHOLDERS.items()},
        "pid_file_path": pid_file_path_cases(),
        "read_pid": read_pid_cases(),
        "process_running_windows": process_running_windows_cases(),
        "powershell_quote": powershell_quote_cases(),
        "shlex_join": shlex_join_cases(),
        "windows_task_registered": windows_registered_cases(),
        "windows_manage": windows_manage_cases(),
        "elevation": elevation_cases(),
        "console_script": console_script_cases(),
        "systemd_service_command": systemd_service_command_cases(),
        "systemd_registered": systemd_registered_cases(),
        "systemd_manage": systemd_manage_cases(),
        "background_start": background_start_cases(),
        "background_stop": background_stop_cases(),
        "health": health_cases(),
        "health_cache": health_cache_cases(),
        "background_status": background_status_cases(),
        "notes": note_cases(),
        "status_render": status_render_cases(),
        "run_service_action": run_service_action_cases(),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="只校验语料是否过期")
    parser.add_argument("--out", default=str(DEFAULT_PATH), help="语料输出路径")
    args = parser.parse_args()

    corpus = build_corpus()
    text = json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"
    target = Path(args.out)

    if args.check:
        if not target.exists():
            print(f"语料不存在: {target}", file=sys.stderr)
            return 1
        if target.read_text(encoding="utf-8") != text:
            print(f"语料已过期: {target}（重新运行本脚本以更新）", file=sys.stderr)
            return 1
        print(f"语料是最新的: {target}")
        return 0

    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
    sections = sum(len(value) if isinstance(value, list) else 1 for value in corpus.values())
    print(f"已写入 {target}（{sections} 个用例）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
