#!/usr/bin/env python3
"""生成 ``auto_model_key_router.main``（main.py，202 行）CLI 的差分对拍语料。

做法：**真实调用** ``main.main()``，但把它的全部副作用换成记录桩——服务启停、系统
服务注册、统一模型切换、配置写入、Terminal UI、dashboard 渲染、版本检查、以及
``RouterConfig.load``。于是每个用例记录下来的都是参照实现**真实做出的决策**：

* 选中了哪条分支（哪条 if/elif 命中，还是走到了默认的 run_terminal_ui）；
* 该分支拿到的配置（host/port/local_api_key）；
* ``--webui`` / ``--no-ops`` 要写进配置的键与值；
* 统一模型切换的五个实参；
* 标准输出（``--show-api-key`` / ``--version``）与退出码。

另有几段纯函数语料：``router_address_text``、``--show-config`` 的「运行概览」行与
模型表行、``--check-update`` 的面板内容、以及被砍掉的三个 flag 清单。

刻意不覆盖的：真正启动服务、真正写配置、真正联网检查版本——这些在 Go 侧由注入接缝
（cliEnv）承担，语料只对拍决策。

用法::

    python -X utf8 scripts/gen_amkr_cli_corpus.py
    python -X utf8 scripts/gen_amkr_cli_corpus.py --check
"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import re
import shutil
import sys
import tempfile
from dataclasses import replace
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from rich.console import Console  # noqa: E402
from rich.table import Table  # noqa: E402

from auto_model_key_router import dashboard, main, update  # noqa: E402
from auto_model_key_router.config import (  # noqa: E402
    RouterConfig,
    empty_config_dict,
    migrate_config_data,
    save_config_data,
)
from auto_model_key_router.update import VersionCheckResult  # noqa: E402

DEFAULT_PATH = REPO_ROOT / "cmd" / "amkr" / "testdata" / "cli_corpus.json"
CORPUS_VERSION = 1
BASE_DIR = Path(tempfile.gettempdir()) / "amkr_cli_corpus"
CONFIG = BASE_DIR / "router-config.json"
EXAMPLE_CONFIG = REPO_ROOT / "router-config.example.json"
ANSI = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07")

# 被产品决策砍掉的 flag：--show-logs（logs_tui，决策 7）、--update 与
# --restart-service-after-update（update.py 自更新，决策 8）。
DROPPED_FLAGS = ["--show-logs", "--update", "--restart-service-after-update"]

DUMMY_PANEL = "STUB-PANEL"
VERSION = main.__dict__.get("__version__") or "4.1.0"


# --------------------------------------------------------------------------- #
# 受控渲染
# --------------------------------------------------------------------------- #

def controlled_console() -> Console:
    return Console(
        width=100,
        height=25,
        file=io.StringIO(),
        force_terminal=True,
        color_system=None,
        legacy_windows=False,
        safe_box=False,
        record=True,
    )


def render_lines(renderable) -> list[str]:
    console = controlled_console()
    console.print(renderable)
    text = ANSI.sub("", console.export_text())
    lines = text.split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    return lines


# --------------------------------------------------------------------------- #
# 记录桩
# --------------------------------------------------------------------------- #

class Journal:
    """按真实调用顺序记录所有被替换的副作用。"""

    def __init__(self):
        self.entries: list[dict] = []

    def record(self, name: str, record: dict) -> None:
        self.entries.append({"name": name, **record})

    def actions(self) -> list[str]:
        return [entry["name"] for entry in self.entries]

    def first(self) -> dict:
        return self.entries[0] if self.entries else {}


class RecordingConfigService:
    """替换 ConfigService：把 mutation 作用在空 dict 上以取出**要写入的键值**。"""

    def __init__(self, journal: Journal):
        self.journal = journal

    def __call__(self, path):
        journal = self.journal

        class Service:
            def __init__(self, inner_path):
                self.path = str(inner_path)

            def update(self, mutation):
                payload = _Payload()
                mutation(payload)
                journal.record("config-update", {"path": self.path, "payload": dict(payload)})

        return Service(path)


class _Payload(dict):
    """让 `data.update(key=value)` 既能写进自身又能被读出来。"""

    def update(self, *args, **kwargs):  # type: ignore[override]
        dict.update(self, *args, **kwargs)
        return self


def make_stub(journal: Journal, name: str, fixture: RouterConfig, returns_config: bool = False):
    def stub(*args, **kwargs):
        record: dict = {"args": [str(arg) for arg in args]}
        for arg in args:
            if isinstance(arg, RouterConfig):
                record["config"] = {
                    "host": arg.host,
                    "port": arg.port,
                    "local_api_key": arg.local_api_key,
                }
        if name == "switch-unified":
            record["switch"] = {
                "target": args[1],
                "model": None if args[2] is None else str(args[2]),
                "key": None if args[3] is None else str(args[3]),
                "update_key": bool(kwargs.get("update_key", False)),
            }
        journal.record(name, record)
        return fixture if returns_config else DUMMY_PANEL

    return stub


def prepare_filesystem() -> None:
    if BASE_DIR.exists():
        shutil.rmtree(BASE_DIR, ignore_errors=True)
    BASE_DIR.mkdir(parents=True, exist_ok=True)
    data = empty_config_dict()
    data["log_file_path"] = str(BASE_DIR / "logs" / "server.log")
    data["metrics_db_path"] = str(BASE_DIR / "metrics.db")
    data["endpoint_capabilities_path"] = str(BASE_DIR / "caps.json")
    data["local_api_key"] = "amkr-cli-corpus-key"
    data["host"] = "127.0.0.1"
    data["port"] = 8123
    save_config_data(CONFIG, data)


def sample_configs() -> list[dict]:
    """--show-config 的三个夹具配置（dict 形式，Go 侧用同一份重建）。"""
    raw = json.loads(EXAMPLE_CONFIG.read_text(encoding="utf-8"))
    migrated = migrate_config_data(raw)
    migrated["log_file_path"] = str(CONFIG)
    zero_host = json.loads(json.dumps(migrated))
    zero_host["host"] = "0.0.0.0"
    no_models = json.loads(json.dumps(migrated))
    no_models["models"] = {}
    # unified_model 必须整个删掉：引用不存在的模型（config.py:1009）或残留半截计划
    # （config.py:1015）都会让 from_dict 拒绝加载，而 None 表示「没有统一模型」。
    no_models.pop("unified_model", None)
    # tasks 同理引用模型，也必须一起清掉。
    no_models["tasks"] = {}
    no_models["port"] = 8000
    return [
        {"name": "example", "config": migrated},
        {"name": "zero-host", "config": zero_host},
        {"name": "no-models", "config": no_models},
    ]


def run_case(argv: list[str], env: dict | None = None, load_error: Exception | None = None) -> dict:
    """执行一次 main.main()，返回决策记录。"""
    fixture = RouterConfig.load(CONFIG)
    journal = Journal()
    replacements: list[dict] = []

    def fake_replace(config, **kwargs):
        replacements.append(kwargs)
        return replace(config, **kwargs)

    class StubRouterConfig:
        @classmethod
        def load(cls, path=None):
            if load_error is not None:
                raise load_error
            return fixture

    stdout = io.StringIO()
    patches = [
        mock.patch.object(main, "RouterConfig", StubRouterConfig),
        mock.patch.object(main, "replace", fake_replace),
        mock.patch.object(main, "ConfigService", RecordingConfigService(journal)),
        mock.patch.object(main, "console", Console(file=io.StringIO(), record=True, width=100)),
        mock.patch.object(main, "clear_terminal_history", lambda: None),
        mock.patch.object(main, "render_version_check_result", lambda result: DUMMY_PANEL),
        mock.patch.object(main, "check_latest_version", make_stub(journal, "check-update", fixture)),
        mock.patch.object(main, "update_latest_version", make_stub(journal, "update-dropped", fixture)),
        mock.patch.object(main, "restart_service_after_update", make_stub(journal, "restart-after-update", fixture)),
        mock.patch.object(main, "render_logs", make_stub(journal, "show-logs", fixture)),
        mock.patch.object(main, "run_terminal_ui", make_stub(journal, "terminal-ui", fixture)),
        mock.patch.object(main, "start_service_background", make_stub(journal, "background-start", fixture)),
        mock.patch.object(main, "start_service_foreground", make_stub(journal, "foreground", fixture)),
        mock.patch.object(main, "stop_background_service", make_stub(journal, "background-stop", fixture)),
        mock.patch.object(main, "background_status_panel", make_stub(journal, "background-status", fixture)),
        mock.patch.object(main, "service_status_panel", make_stub(journal, "service-status", fixture)),
        mock.patch.object(main, "manage_system_service", make_stub(journal, "system-service", fixture)),
        mock.patch.object(main, "switch_unified_target", make_stub(journal, "switch-unified", fixture, True)),
        mock.patch.object(main, "render_config", make_stub(journal, "show-config", fixture)),
        mock.patch.object(main, "unified_model_status_panel", make_stub(journal, "show-unified-model", fixture)),
    ]

    with contextlib.ExitStack() as stack:
        for patch in patches:
            stack.enter_context(patch)
        if env is not None:
            stack.enter_context(mock.patch.dict("os.environ", env, clear=False))
        stack.enter_context(mock.patch.object(sys, "argv", argv))
        exit_code = 0
        # argparse 在参数错误时会把用法与错误文本写到 stderr；语料只关心退出码，
        # 这里吞掉以免污染生成脚本的输出。
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(io.StringIO()):
            try:
                main.main()
            except SystemExit as exc:
                exit_code = int(exc.code or 0)

    branch_entries = [entry for entry in journal.entries if entry["name"] != "config-update"]
    first = branch_entries[0] if branch_entries else {}
    return {
        "argv": argv[1:],
        "exit": exit_code,
        # 默认无参数时参照实现走到 dashboard 的 Terminal UI（已砍，见 Go 侧默认动作决策）。
        "actions": journal.actions(),
        "config": first.get("config"),
        "switch": first.get("switch"),
        "system_action": first.get("args", [None, None])[1] if first.get("name") == "system-service" else None,
        "load_error": load_error is not None,
        "updates": [entry["payload"] for entry in journal.entries if entry["name"] == "config-update"],
        "replacements": replacements,
        "stdout": stdout.getvalue(),
    }


def flag_cases() -> list[dict]:
    fixture_path = str(CONFIG)
    cases: list[tuple[list[str], dict | None]] = [
        # 决策：24 个 flag 里被移植的那些。
        (["amkr", "--config", fixture_path, "--serve"], None),
        (["amkr", "--config", fixture_path, "--serve-foreground"], None),
        (["amkr", "--config", fixture_path, "--stop"], None),
        (["amkr", "--config", fixture_path, "--status"], None),
        (["amkr", "--config", fixture_path, "--install-service"], None),
        (["amkr", "--config", fixture_path, "--service", "install"], None),
        (["amkr", "--config", fixture_path, "--service", "install-user"], None),
        (["amkr", "--config", fixture_path, "--service", "uninstall"], None),
        (["amkr", "--config", fixture_path, "--service", "start"], None),
        (["amkr", "--config", fixture_path, "--service", "stop"], None),
        (["amkr", "--config", fixture_path, "--service", "restart"], None),
        (["amkr", "--config", fixture_path, "--service", "status"], None),
        (["amkr", "--config", fixture_path, "--check-update"], None),
        (["amkr", "--config", fixture_path, "--show-address"], None),
        (["amkr", "--config", fixture_path, "--show-api-key"], None),
        (["amkr", "--config", fixture_path, "--get-api-key"], None),
        (["amkr", "--config", fixture_path, "--get-key"], None),
        (["amkr", "--config", fixture_path, "--show-config"], None),
        (["amkr", "--config", fixture_path, "--show-unified-model"], None),
        (["amkr", "--config", fixture_path, "--switch-model", "gpt-4o-mini"], None),
        (["amkr", "--config", fixture_path, "--switch-key", "auto"], None),
        (["amkr", "--config", fixture_path, "--switch-key", "main"], None),
        (["amkr", "--config", fixture_path, "--switch-model", "gpt-4o-mini", "--unified-target", "image.primary"], None),
        (["amkr", "--config", fixture_path], None),
        # host/port 覆盖（含「假值不覆盖」的怪癖）。
        (["amkr", "--config", fixture_path, "--status", "--host", "0.0.0.0"], None),
        (["amkr", "--config", fixture_path, "--status", "--port", "9000"], None),
        (["amkr", "--config", fixture_path, "--status", "--port", "0"], None),
        (["amkr", "--config", fixture_path, "--status", "--host", ""], None),
        (["amkr", "--config", fixture_path, "--status", "--host", "0.0.0.0", "--port", "9000"], None),
        # --webui / --no-webui / --no-ops（写配置）。
        (["amkr", "--config", fixture_path, "--status", "--webui"], None),
        (["amkr", "--config", fixture_path, "--status", "--no-webui"], None),
        (["amkr", "--config", fixture_path, "--status", "--no-ops"], None),
        (["amkr", "--config", fixture_path, "--status", "--webui", "--no-ops"], None),
        (["amkr", "--config", fixture_path, "--status", "--no-webui", "--webui"], None),
        # 组合：链序靠前者优先。
        (["amkr", "--config", fixture_path, "--stop", "--status"], None),
        (["amkr", "--config", fixture_path, "--show-config", "--show-address"], None),
        (["amkr", "--config", fixture_path, "--serve", "--serve-foreground"], None),
        (["amkr", "--config", fixture_path, "--switch-model", "m", "--show-api-key"], None),
        (["amkr", "--config", fixture_path, "--check-update", "--status"], None),
        # 参数错误（argparse 退出码 2）。
        (["amkr", "--config", fixture_path, "--nope"], None),
        (["amkr", "--config", fixture_path, "--port", "abc", "--status"], None),
        (["amkr", "--config", fixture_path, "--service", "bogus"], None),
        (["amkr", "--config", fixture_path, "--unified-target", "bogus", "--switch-model", "m"], None),
        (["amkr", "--config", fixture_path, "--switch-model"], None),
        # 被砍掉的三个 flag：参照实现仍然接受，Go 侧必须是「未定义」（见 Go 的具名测试）。
        (["amkr", "--config", fixture_path, "--show-logs"], None),
        (["amkr", "--config", fixture_path, "--show-logs", "5"], None),
        (["amkr", "--config", fixture_path, "--update"], None),
        (["amkr", "--config", fixture_path, "--restart-service-after-update"], None),
        # --version 由 argparse 在解析阶段直接打印并退出 0。
        (["amkr", "--version"], None),
        # 默认配置文件路径（AMKR_CONFIG 在参照实现里走不到，见 Go 侧具名差异测试）。
        (["amkr", "--status"], {"AMKR_CONFIG": fixture_path}),
        # 配置加载失败 → 退出码 1。
        (["amkr", "--config", fixture_path, "--status"], None),
    ]
    cases = list(cases)
    records = []
    for index, (argv, env) in enumerate(cases):
        if index == len(cases) - 1:
            record = run_case(argv, env, load_error=ValueError("配置版本不对"))
        else:
            record = run_case(argv, env)
        record["id"] = index
        if record["argv"] and "--config" in record["argv"]:
            position = record["argv"].index("--config")
            if position + 1 < len(record["argv"]):
                record["argv"][position + 1] = "$CONFIG"
        records.append(record)
    return records


def address_text_cases() -> list[dict]:
    cases = []
    fixture = RouterConfig.load(CONFIG)
    for host, port in (("127.0.0.1", 8123), ("0.0.0.0", 80), ("::1", 8123), ("[::1]", 9000), ("localhost", 8000)):
        config = replace(fixture, host=host, port=port)
        cases.append({"host": host, "port": port, "expected": main.router_address_text(config)})
    return cases


class RecordingTable(Table):
    """记录 add_row 的表格，用来取出模型配置表的**行数据**。"""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.recorded: list[list[str]] = []

    def add_row(self, *cells, **kwargs):
        self.recorded.append([str(cell) for cell in cells])
        return super().add_row(*cells, **kwargs)


def config_summary_cases() -> list[dict]:
    cases = []
    tables: list[RecordingTable] = []
    factory = lambda *args, **kwargs: _capture_table(tables, args, kwargs)
    with mock.patch.object(dashboard, "quick_metrics_items", lambda path: []), mock.patch.object(
        dashboard, "visitor_feature_available", lambda: True
    ), mock.patch.object(dashboard, "is_service_healthy", lambda *a, **k: False), mock.patch.object(
        dashboard, "Table", factory
    ):
        for sample in sample_configs():
            tables.clear()
            config = RouterConfig.from_dict(sample["config"])
            renderables = dashboard.config_renderables(config, CONFIG)
            summary = renderables[0].renderable
            rows = list(tables[0].recorded) if tables else []
            cases.append(
                {
                    "name": sample["name"],
                    "config_json": json.dumps(sample["config"], ensure_ascii=False, sort_keys=False),
                    "healthy": False,
                    "visitor_installed": True,
                    "summary_lines": render_lines(summary),
                    "model_rows": rows,
                    "has_warning": config.host == "0.0.0.0",
                }
            )
    return cases


def _capture_table(store: list, args, kwargs):
    table = RecordingTable(*args, **kwargs)
    store.append(table)
    return table


def version_check_cases() -> list[dict]:
    results = [
        VersionCheckResult(current_version="4.1.0", latest_version="4.2.0", source="pypi",
                           release_url="https://pypi.org/project/auto-model-key-router/4.2.0/"),
        VersionCheckResult(current_version="4.1.0", latest_version="4.1.0", source="github",
                           latest_tag="v4.1.0"),
        VersionCheckResult(current_version="4.1.0", error="PyPI 与 GitHub 都不可达"),
        VersionCheckResult(current_version="4.1.0", latest_version="4.2.0", source="github",
                           fallback_error="HTTP 404", latest_tag="v4.2.0"),
        VersionCheckResult(current_version="4.1.0"),
    ]
    cases = []
    for result in results:
        panel = update.render_version_check_result(result)
        content = str(panel.renderable)
        lines = content.split("\n")
        manual = None
        if "手动更新命令:" in lines:
            manual = lines[lines.index("手动更新命令:") + 1]
            manual = manual.removeprefix("[bold]").removesuffix("[/bold]")
        cases.append(
            {
                "result": {
                    "current_version": result.current_version,
                    "latest_version": result.latest_version,
                    "latest_tag": result.latest_tag,
                    "release_url": result.release_url,
                    "source": result.source,
                    "fallback_error": result.fallback_error,
                    "error": result.error,
                    "update_available": result.update_available,
                },
                "content": content,
                "manual_command": manual,
            }
        )
    return cases


def version_output_case() -> dict:
    record = run_case(["amkr", "--version"])
    stdout = record["stdout"].replace(VERSION, "$VERSION")
    return {"stdout": stdout, "exit": record["exit"]}


def build_corpus() -> dict:
    prepare_filesystem()
    return {
        "corpus_version": CORPUS_VERSION,
        "version": VERSION,
        "dropped_flags": DROPPED_FLAGS,
        "flags": flag_cases(),
        "address_text": address_text_cases(),
        "config_summary": config_summary_cases(),
        "version_check": version_check_cases(),
        "version_output": version_output_case(),
    }


def main_entry() -> int:
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
            print(f"语料已过期: {target}", file=sys.stderr)
            return 1
        print(f"语料是最新的: {target}")
        return 0
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
    total = sum(len(value) if isinstance(value, list) else 1 for value in corpus.values())
    print(f"已写入 {target}（{total} 个用例）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main_entry())
