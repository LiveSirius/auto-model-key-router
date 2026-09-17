#!/usr/bin/env python3
"""生成 internal/servicestatus 的对拍语料（Python 侧为参照实现）。

service_status.py 分两层，语料也分两块：

1. **采集层**（collect_windows_task_status / collect_systemd_user_status）——
   命令执行器由生成器替换成按「响应计划」返回的桩，于是 Windows 计划任务与
   systemd 两条分支能在同一台机器上跑完；真正读文件的地方只有 systemd 的 unit
   文件，语料把它建在临时目录里，并把记录下来的路径前缀换成 ``<root>``
   （Go 测试用同样的方式换算自己的临时目录），因此语料与平台无关。
2. **解析层**（parse_* / command_output / first_value / local_xml_name）——
   纯函数，直接喂文本。

用法::

    python scripts/gen_servicestatus_corpus.py           # 写入语料
    python scripts/gen_servicestatus_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router.service_status import (  # noqa: E402
    StatusDetail,
    collect_systemd_user_status,
    collect_windows_task_status,
    command_output,
    first_value,
    local_xml_name,
    parse_key_value_lines,
    parse_systemctl_properties,
    parse_systemd_unit_file,
    parse_windows_task_xml,
)

DEFAULT_DIR = REPO_ROOT / "internal" / "servicestatus" / "testdata"

TASK_NAME = "AutoModelKeyRouter"
SERVICE_NAME = "auto-model-key-router.service"
PYTHON_REL = "venv/bin/python"
CONFIG_REL = "amkr/router-config.json"
SERVICE_REL = "unit/auto-model-key-router.service"
MISSING_SERVICE_REL = "unit/missing.service"

# --------------------------------------------------------------------------- #
# 真实形状的输入（取自 tests/test_tui.py:2914 / 2948 的夹具）
# --------------------------------------------------------------------------- #

TASK_XML = """<?xml version="1.0" encoding="UTF-16"?>
<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers><BootTrigger><Enabled>true</Enabled></BootTrigger></Triggers>
  <Principals><Principal><UserId>SYSTEM</UserId><RunLevel>HighestAvailable</RunLevel></Principal></Principals>
  <Settings><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
  <Actions><Exec><Command>C:\\Python\\pythonw.exe</Command><Arguments>-m auto_model_key_router.main --config C:\\config.json --serve-foreground</Arguments><WorkingDirectory>C:\\app</WorkingDirectory></Exec></Actions>
</Task>"""

TASK_LIST = """TaskName: \\AutoModelKeyRouter
Status: Running
Run As User: SYSTEM
Last Run Time: 2026/6/10 12:00:00
Next Run Time: N/A
Last Result: 0x0"""

SYSTEMD_SHOW = """LoadState=loaded
ActiveState=active
SubState=running
UnitFileState=enabled
FragmentPath=/home/tester/.config/systemd/user/auto-model-key-router.service
ExecMainPID=1234
MainPID=1234
Result=success
ExecMainStatus=0
NRestarts=2
NeedDaemonReload=no"""

SYSTEMD_STATUS = ("● auto-model-key-router.service - Auto Model Key Router\n"
                  "     Active: active (running)")

UNIT_FILE = "\n".join([
    "[Unit]",
    "Description=Auto Model Key Router",
    "",
    "[Service]",
    "WorkingDirectory=/opt/amkr",
    "ExecStart=/usr/bin/python -m auto_model_key_router.main --config /etc/amkr.json",
    "; 分号不是注释，会被当成未知键",
    "# 井号才是注释",
    "Restart=always",
    "",
    "[Install]",
    "WantedBy=default.target",
])


# --------------------------------------------------------------------------- #
# 桩与路径换算
# --------------------------------------------------------------------------- #

def make_runner(responses: list[dict], calls: list[list[str]]):
    """按计划返回命令结果；计划用尽后继续被调用说明分支变了，直接报错。"""
    remaining = list(responses)

    def run(command):
        calls.append([str(part) for part in command])
        if not remaining:
            raise AssertionError(f"命令调用次数超出计划: {command!r}")
        step = remaining.pop(0)
        if "error" in step:
            raise OSError(step["error"])
        return subprocess.CompletedProcess(
            command, step["code"], step.get("stdout", ""), step.get("stderr", "")
        )

    return run


def portable(value, root: Path):
    """把临时目录前缀换成 <root>，并把它后面那一段路径的分隔符统一成 "/"。

    只有 systemd 的「Unit 文件」行会带上绝对路径；这样语料在 Windows 生成、
    在 Linux CI 上重放时逐字节一致。
    """
    prefix = str(root)
    if not prefix or prefix not in value:
        return value
    head, _, tail = value.partition(prefix)
    end = len(tail)
    for index, char in enumerate(tail):
        if char in "\n\r\t\"'":
            end = index
            break
    return f"{head}<root>{tail[:end].replace(chr(92), '/')}{tail[end:]}"


def portable_detail(detail: StatusDetail, root: Path) -> dict:
    return {
        "title": portable(detail.title, root),
        "content": portable(detail.content, root),
        "style": detail.style,
    }


def status_record(status, root: Path) -> dict:
    return {
        "registered": status.registered,
        "rows": [[portable(label, root), portable(value, root)]
                 for label, value in status.rows],
        "details": [portable_detail(detail, root) for detail in status.details],
    }


# --------------------------------------------------------------------------- #
# 采集层用例
# --------------------------------------------------------------------------- #

def run_windows(case: dict, root: Path) -> dict:
    calls: list[list[str]] = []
    runner = make_runner(case["responses"], calls)
    record: dict = {
        "name": case["name"],
        "op": "collect_windows_task_status",
        "task_name": TASK_NAME,
        "python": PYTHON_REL,
        "config": CONFIG_REL,
        "responses": case["responses"],
    }
    if case.get("note"):
        record["note"] = case["note"]
    try:
        status = collect_windows_task_status(
            root.joinpath(*PYTHON_REL.split("/")),
            root.joinpath(*CONFIG_REL.split("/")),
            TASK_NAME,
            runner,
        )
    except OSError:
        record["want_error"] = True
        record["want_calls"] = calls
        return record
    record["want_calls"] = calls
    record["want_status"] = status_record(status, root)
    return record


def run_systemd(case: dict, root: Path) -> dict:
    calls: list[list[str]] = []
    runner = make_runner(case["responses"], calls)
    service_rel = case.get("service_path", MISSING_SERVICE_REL)
    record: dict = {
        "name": case["name"],
        "op": "collect_systemd_user_status",
        "service_name": SERVICE_NAME,
        "python": PYTHON_REL,
        "config": CONFIG_REL,
        "service_path": service_rel,
        "responses": case["responses"],
    }
    if case.get("note"):
        record["note"] = case["note"]
    if "unit_file_text" in case:
        record["unit_file_text"] = case["unit_file_text"]
    if "unit_file_hex" in case:
        record["unit_file_hex"] = case["unit_file_hex"]

    service_path = root.joinpath(*service_rel.split("/"))
    if "unit_file_hex" in case:
        service_path.parent.mkdir(parents=True, exist_ok=True)
        service_path.write_bytes(bytes.fromhex(case["unit_file_hex"]))
    elif "unit_file_text" in case:
        service_path.parent.mkdir(parents=True, exist_ok=True)
        # 用 bytes 写：语料里的 \r\n 必须原样落盘，不能被换行翻译改写。
        service_path.write_bytes(case["unit_file_text"].encode("utf-8"))

    try:
        status = collect_systemd_user_status(
            root.joinpath(*PYTHON_REL.split("/")),
            root.joinpath(*CONFIG_REL.split("/")),
            service_path,
            SERVICE_NAME,
            runner,
        )
    except UnicodeDecodeError:
        record["want_error"] = True
        record["want_error_kind"] = "UnicodeDecodeError"
        record["want_calls"] = calls
        return record
    except OSError:
        record["want_error"] = True
        record["want_calls"] = calls
        return record
    record["want_calls"] = calls
    record["want_status"] = status_record(status, root)
    return record


def collect_cases() -> list[dict]:
    def ok(stdout: str = "", stderr: str = "", code: int = 0) -> dict:
        return {"code": code, "stdout": stdout, "stderr": stderr}

    def err(message: str) -> dict:
        return {"error": message}

    def windows(name: str, responses: list[dict], note: str = "") -> dict:
        return {"name": name, "responses": responses, "note": note}

    def systemd(name: str, responses: list[dict], *, service_path=MISSING_SERVICE_REL,
                unit_file_text=None, unit_file_hex=None, note: str = "") -> dict:
        case = {"name": name, "responses": responses, "service_path": service_path,
                "note": note}
        if unit_file_text is not None:
            case["unit_file_text"] = unit_file_text
        if unit_file_hex is not None:
            case["unit_file_hex"] = unit_file_hex
        return case

    return [
        # ---------- Windows 计划任务 ----------
        windows("windows_registered_full",
                [ok(TASK_LIST), ok(TASK_XML)],
                note="经典组合：列表命令给运行状态，XML 给触发器与执行程序"),
        windows("windows_xml_failed", [ok(TASK_LIST), ok("", "错误: 系统找不到指定的文件。", 1)],
                note="XML 查询失败时不做 XML 解析，注册状态仍由列表命令决定"),
        windows("windows_both_failed_empty",
                [ok("", "", 1), ok("", "", 1)],
                note="两条都失败且没有输出：黄色详情提示「未找到计划任务」"),
        windows("windows_list_failed_with_stderr",
                [ok("", "错误: 系统找不到指定的文件。", 1), ok(TASK_XML)],
                note="列表失败但 XML 成功：仍算已注册，原始状态用红色展示 stderr"),
        windows("windows_list_ok_without_output", [ok(""), ok(TASK_XML)],
                note="列表命令成功但没有输出：提示「命令执行成功，但没有返回详细内容。」"),
        windows("windows_localized_keys",
                [ok("状态: 就绪\n运行身份: SYSTEM\n上次运行时间: 2026/6/10 12:00:00\n"
                    "下次运行时间: 2026/6/11 12:00:00\n上次结果: 0x0"),
                 ok("<Task><Command>pythonw.exe</Command></Task>")],
                note="中文版 schtasks：first_value 要在英文键与中文键之间回退"),
        windows("windows_empty_xml_values",
                [ok("Status: Ready"), ok("<Task><Command>   </Command><Arguments/>"
                                          "<WorkingDirectory> C:\\app </WorkingDirectory></Task>")],
                note="<Command> 只有空白 → 值为空串（不是 -），"
                     "<Arguments/> 没有文本 → 缺省为 -"),
        windows("windows_xml_without_namespace",
                [ok("Status: Ready\nLast Result: 0x1"),
                 ok("<Task><Settings><StartWhenAvailable>true</StartWhenAvailable>"
                    "</Settings><Exec><Command>x.exe</Command></Exec></Task>")]),
        windows("windows_runner_error_first", [err("模拟 OSError: schtasks 不存在")],
                note="第一条命令就抛异常：Python 直接向上传播，第二条不执行"),
        windows("windows_runner_error_second",
                [ok(TASK_LIST), err("模拟 OSError: schtasks 不存在")],
                note="第二条命令抛异常：同样向上传播"),

        # ---------- systemd 用户服务 ----------
        systemd("systemd_registered_full",
                [ok(SYSTEMD_SHOW), ok(SYSTEMD_STATUS)],
                service_path=SERVICE_REL, unit_file_text=UNIT_FILE,
                note="unit 文件存在即算已注册"),
        systemd("systemd_unit_missing_but_loaded",
                [ok(SYSTEMD_SHOW), ok(SYSTEMD_STATUS)],
                note="unit 文件不存在但 LoadState=loaded：仍算已注册，"
                     "行里显示「未找到: <路径>」"),
        systemd("systemd_not_registered",
                [ok("LoadState=not-found\nActiveState=inactive\nResult=exit-code\n"
                    "ExecMainStatus=1", "Unit auto-model-key-router.service could not be found.", 4),
                 ok("", "Unit auto-model-key-router.service could not be found.", 4)],
                note="LoadState=not-found 且没有 unit 文件：未注册，原始状态为红色"),
        systemd("systemd_load_state_empty", [ok("LoadState="), ok("", "", 1)],
                note="LoadState 为空串同样算未注册（Python 排除 None/\"\"/not-found）"),
        systemd("systemd_load_state_absent", [ok("ActiveState=active"), ok("")],
                note="完全没有 LoadState 键：算未注册，但命令成功会给出「没有详细内容」"),
        systemd("systemd_mainpid_fallback",
                [ok("LoadState=loaded\nMainPID=\nExecMainPID=4321\n"
                    "ActiveState=active\nSubState=\nUnitFileState=static\n"
                    "Result=\nExecMainStatus=\nNRestarts=\nNeedDaemonReload="),
                 ok(SYSTEMD_STATUS)],
                note="MainPID 为空时退回 ExecMainPID；SubState 为空时运行状态只有 active"),
        systemd("systemd_run_state_empty",
                [ok("LoadState=loaded\nActiveState=\nSubState="), ok(SYSTEMD_STATUS)],
                note="两个状态都为空：运行状态为 -"),
        systemd("systemd_show_with_blank_key",
                [ok("=abc\nLoadState=loaded\nno-equals-line\nKey = value "),
                 ok(SYSTEMD_STATUS)],
                note="show 的属性解析不过滤空键（与列表命令的解析不同）"),
        systemd("systemd_unit_file_crlf",
                [ok(SYSTEMD_SHOW), ok(SYSTEMD_STATUS)],
                service_path=SERVICE_REL,
                unit_file_text=UNIT_FILE.replace("\n", "\r\n"),
                note="unit 文件是 CRLF：Python 按通用换行读入，详情里应显示为 LF"),
        systemd("systemd_unit_file_whitespace_only",
                [ok(SYSTEMD_SHOW), ok(SYSTEMD_STATUS)],
                service_path=SERVICE_REL, unit_file_text="   \n\t\n",
                note="unit 文件只有空白：unit_text 为真会加详情，但 strip 后内容为空串"),
        systemd("systemd_unit_file_empty",
                [ok(SYSTEMD_SHOW), ok(SYSTEMD_STATUS)],
                service_path=SERVICE_REL, unit_file_text="",
                note="空 unit 文件：不加「服务文件」详情，键全部缺省为 -"),
        systemd("systemd_unit_file_invalid_utf8",
                [ok(SYSTEMD_SHOW), ok(SYSTEMD_STATUS)],
                service_path=SERVICE_REL, unit_file_hex="5b536572766963655d0afffe00",
                note="unit 文件不是合法 UTF-8：Python 抛 UnicodeDecodeError"),
        systemd("systemd_runner_error_first", [err("模拟 OSError: systemctl 不存在")],
                note="第一条命令抛异常：第二条与读文件都不进行"),
        systemd("systemd_runner_error_second", [ok(SYSTEMD_SHOW),
                                                err("模拟 OSError: systemctl 不存在")]),
    ]


# --------------------------------------------------------------------------- #
# 解析层用例
# --------------------------------------------------------------------------- #

TASK_XMLS: list[tuple[str, str]] = [
    ("xml_full_namespaced", TASK_XML),
    ("xml_bom_prefixed", "\ufeff<Task><Command>x</Command></Task>"),
    ("xml_double_bom", "\ufeff\ufeff<Task><Command>x</Command></Task>"),
    ("xml_malformed", "<Task><Command>x</Task>"),
    ("xml_empty", ""),
    ("xml_blank", "   \n "),
    ("xml_self_closing_root", "<Task/>"),
    ("xml_empty_root", "<Task></Task>"),
    ("xml_no_root_text", "hello"),
    ("xml_whitespace_text", "<Command>   </Command>"),
    ("xml_self_closing_tracked", "<Command/>"),
    ("xml_text_then_child", "<Command>x<Foo/></Command>"),
    ("xml_child_then_text", "<Command><Foo/>x</Command>"),
    ("xml_cdata_mixed", "<Command>x<![CDATA[y]]>z</Command>"),
    ("xml_entity", "<Command>a&amp;b</Command>"),
    ("xml_numeric_refs", "<Command>&#65;&#x42;</Command>"),
    ("xml_unknown_entity", "<Command>a&foo;b</Command>"),
    ("xml_comment_split_text", "<Command>a<!--c-->b</Command>"),
    ("xml_duplicate_tracked",
     "<Task><Command>first</Command><Command>second</Command></Task>"),
    ("xml_triggers_many",
     "<Triggers><CalendarTrigger/><TimeTrigger/><BootTrigger/></Triggers>"),
    ("xml_trigger_exact", "<Trigger/>"),
    ("xml_triggers_container_text", "<Triggers>ignored</Triggers>"),
    ("xml_trailing_element", "<Task/><Task/>"),
    ("xml_trailing_text", "<Task/>trailing"),
    ("xml_decl_not_first", '  <?xml version="1.0"?><Task><Command>x</Command></Task>'),
    ("xml_decl_newline_first", '\n<?xml version="1.0"?><Task><Command>x</Command></Task>'),
    ("xml_decl_first", '<?xml version="1.0"?><Task><Command>x</Command></Task>'),
    ("xml_pi_stylesheet",
     '<?xml-stylesheet href="x"?><Task><Command>x</Command></Task>'),
    ("xml_comment_before_root", "<!-- hi --><Task><Command>x</Command></Task>"),
    ("xml_comment_inside", "<Task><!-- c --><Command>x</Command></Task>"),
    ("xml_prefixed_ns",
     '<t:Task xmlns:t="urn:x"><t:Command>x</t:Command><t:BootTrigger/></t:Task>'),
    ("xml_uppercase_tags", "<task><command>x</command></task>"),
    ("xml_tracked_with_child_and_ws",
     "<Settings>\n  <ExecutionTimeLimit>PT1H</ExecutionTimeLimit>\n</Settings>"),
    ("xml_tricky_exec",
     "<Exec><Command>a</Command><WorkingDirectory>b</WorkingDirectory>"
     "<Arguments>c</Arguments></Exec>"),
    ("xml_text_before_child_tracked",
     "<Arguments>\n    -x\n    <Foo/>\n  </Arguments>"),
    ("xml_invalid_tag", "<a}b><Command>x</Command></a}b>"),
    ("xml_value_with_backslash",
     "<Command>C:\\Python\\pythonw.exe</Command>"),
    ("xml_tab_around_value", "<Command>\tpythonw.exe\t</Command>"),
    ("xml_unicode_value", "<Command>中文 路径/程序.exe</Command>"),
    ("xml_trigger_with_tracked_names",
     "<Task><BootTrigger/><Enabled>false</Enabled></Task>"),
]

KEY_VALUE_TEXTS = [
    "TaskName: \\AutoModelKeyRouter\nStatus: Running\nLast Result: 0x0",
    "no colon here\nKey: value",
    ":empty key\n  : spaces",
    "Key: a:b:c",
    "Key:\nOther:  ",
    "a:1\vb:2",
    "a:1\rc:2",
    "a:1\r\nc:2",
    "a:1\x1cc:2",
    "a:1\x1dc:2",
    "a:1\x1ec:2",
    "a:1\x85c:2",
    "a:1\u2028c:2",
    "a:1\u2029c:2",
    "a:1\fc:2",
    "Key: value ",
    "Key\t: value",
    "   ",
    "",
    "\n\n",
    "Status: Ready\r\n",
]

SYSTEMCTL_TEXTS = [
    SYSTEMD_SHOW,
    "=abc",
    "no equals",
    "",
    "A=B=C",
    "Key = value ",
    "a=1\x85b=2",
    "LoadState=loaded\r\nActiveState=active\r\n",
]

UNIT_TEXTS = [
    UNIT_FILE,
    "=abc",
    "Key = value ",
    "# only comment",
    "; semicolon only",
    "   ",
    "",
    "a=1\x85b=2",
    "Key: not-equals",
    "[Service]\r\nExecStart=/usr/bin/x\r\n",
]

FIRST_VALUE_CASES: list[tuple[dict, list[str]]] = [
    ({"a": "1", "b": "", "c": "3"}, ["a", "b"]),
    ({"a": "1", "b": "", "c": "3"}, ["b", "c"]),
    ({"a": "1", "b": "", "c": "3"}, ["x", "a"]),
    ({"a": "1", "b": "", "c": "3"}, ["b"]),
    ({"a": "1", "b": "", "c": "3"}, ["x", "y"]),
    ({"a": "1"}, []),
    ({}, ["a"]),
    ({"": "empty-key"}, ["", "a"]),
]

COMMAND_OUTPUT_CASES: list[tuple[str, str, int]] = [
    ("out", "err", 0),
    ("", "err", 0),
    ("", "", 0),
    ("out", "", 1),
    ("", "err", 1),
    ("", "", 1),
    ("  out  ", "", 0),
    ("\n", "", 0),
    ("\t \n ", "  ", 0),
    ("", "\n ", 1),
    ("line1\nline2\n", "", 0),
]

LOCAL_XML_NAMES = ["{ns}Command", "Command", "{a}{b}Command", "", "}x", "{x}", "Trigger"]


def parser_cases() -> list[dict]:
    cases: list[dict] = []
    for name, text in TASK_XMLS:
        cases.append({"name": name, "op": "parse_windows_task_xml", "input": text,
                      "want_map": parse_windows_task_xml(text)})
    for index, text in enumerate(KEY_VALUE_TEXTS):
        cases.append({"name": f"key_value_{index}", "op": "parse_key_value_lines",
                      "input": text, "want_map": parse_key_value_lines(text)})
    for index, text in enumerate(SYSTEMCTL_TEXTS):
        cases.append({"name": f"systemctl_props_{index}", "op": "parse_systemctl_properties",
                      "input": text, "want_map": parse_systemctl_properties(text)})
    for index, text in enumerate(UNIT_TEXTS):
        cases.append({"name": f"unit_file_{index}", "op": "parse_systemd_unit_file",
                      "input": text, "want_map": parse_systemd_unit_file(text)})
    for index, (values, keys) in enumerate(FIRST_VALUE_CASES):
        cases.append({"name": f"first_value_{index}", "op": "first_value",
                      "values": values, "keys": keys,
                      "want_value": first_value(values, *keys)})
    for index, (stdout, stderr, code) in enumerate(COMMAND_OUTPUT_CASES):
        cases.append({"name": f"command_output_{index}", "op": "command_output",
                      "result": {"stdout": stdout, "stderr": stderr, "code": code},
                      "want_text": command_output(
                          subprocess.CompletedProcess(["cmd"], code, stdout, stderr))})
    for index, tag in enumerate(LOCAL_XML_NAMES):
        cases.append({"name": f"local_xml_name_{index}", "op": "local_xml_name",
                      "input": tag, "want_text": local_xml_name(tag)})
    return cases


# --------------------------------------------------------------------------- #
# 组装
# --------------------------------------------------------------------------- #

def build_corpus() -> dict:
    cases: list[dict] = []

    root = Path(tempfile.mkdtemp(prefix="amkr-servicestatus-corpus-"))
    try:
        for case in collect_cases():
            if case["name"].startswith("windows_"):
                cases.append(run_windows(case, root))
            else:
                cases.append(run_systemd(case, root))
    finally:
        shutil.rmtree(root, ignore_errors=True)

    cases.extend(parser_cases())
    return {"cases": cases}


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    path = args.output_dir / "servicestatus_corpus.json"
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
