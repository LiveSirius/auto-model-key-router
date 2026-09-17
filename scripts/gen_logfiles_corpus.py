#!/usr/bin/env python3
"""生成 internal/logfiles 的对拍语料（Python 侧为参照实现）。

log_files.py 是纯文件系统操作，语料必须**与机器无关**才能在 CI（Linux）上重放：

* 所有夹具都建在临时目录里，语料只记录**相对于该目录的 POSIX 路径**；
  Go 测试建自己的临时目录，按同样的相对路径摆放、比对。
* ``archive_current_log`` 内部用 ``datetime.now()``，语料改为注入固定时刻
  （Python 没有这个参数，生成器直接调用 ``next_log_archive_path`` 的逻辑无法
  覆盖它，所以这里用 ``unittest.mock`` 把 ``datetime`` 钉住，见 ``run_archive``）。
* 夹具文件名刻意**大小写一致**：pathlib 在 Windows 上 glob 不区分大小写，
  而 Go 侧（filepath.Match）一律区分。把大小写差异写进语料会让 CI 上的 Linux
  与生成机上的 Windows 得到两份不同的期望值。该差异由 Go 的具名测试单独锁定。

覆盖三个函数：

1. ``next_log_archive_path`` —— 时间戳、后缀回退、同名碰撞编号、空名字报错
2. ``archive_current_log``   —— 非空归档 / 空文件截断 / 缺失即建
3. ``archived_log_paths``    —— 匹配、过滤目录、排除自身、按名字倒序

用法::

    python scripts/gen_logfiles_corpus.py           # 写入语料
    python scripts/gen_logfiles_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
import tempfile
from datetime import datetime
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router import log_files  # noqa: E402

DEFAULT_DIR = REPO_ROOT / "internal" / "logfiles" / "testdata"

# 注入的固定时刻：archive_current_log 的归档名带时间戳，不钉住就无法逐字节比对。
FIXED_NOW = datetime(2026, 1, 2, 3, 4, 5)
FIXED_NOW_TEXT = "2026-01-02T03:04:05"

# 语料里 arg 以 "/" 开头或为空时按原样交给被测函数（这两种输入在 Python 里都是
# 「没有名字的路径」，Go 侧也会先报错、不碰文件系统），其余相对路径拼到临时根下。
VERBATIM_PREFIX = "/"


# --------------------------------------------------------------------------- #
# 夹具与执行
# --------------------------------------------------------------------------- #

def materialize(root: Path, fixture: list[dict]) -> None:
    root.mkdir(parents=True, exist_ok=True)
    for entry in fixture:
        target = root.joinpath(*entry["path"].split("/"))
        if entry.get("dir"):
            target.mkdir(parents=True, exist_ok=True)
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        # 用 write_bytes 而不是 write_text：避免 Windows 把 \n 翻成 \r\n，
        # 否则同一份语料在 Linux 上会得到不同的文件长度。
        target.write_bytes(entry.get("text", "").encode("utf-8"))


def snapshot(root: Path) -> list[dict]:
    """列出 root 下的普通文件（相对 POSIX 路径 + 文本内容），按路径排序。"""
    files = [item for item in root.rglob("*") if item.is_file()]
    result = []
    for item in sorted(files, key=lambda p: p.relative_to(root).as_posix()):
        result.append({
            "path": item.relative_to(root).as_posix(),
            "text": item.read_bytes().decode("utf-8"),
        })
    return result


def resolve(root: Path, arg: str) -> Path:
    if arg == "" or arg.startswith(VERBATIM_PREFIX):
        return Path(arg)
    return root.joinpath(*arg.split("/"))


def relative(root: Path, path: Path) -> str:
    return path.relative_to(root).as_posix()


def run_archive(root: Path, arg: str) -> Path | None:
    """执行 archive_current_log，并把内部时钟钉成 FIXED_NOW。

    log_files 在函数内部调用 ``datetime.now()``，没有注入点；这里替换模块里的
    ``datetime`` 名字，效果等价于把 now 参数传进去。
    """
    with mock.patch.object(log_files, "datetime") as patched:
        patched.now.return_value = FIXED_NOW
        return log_files.archive_current_log(str(resolve(root, arg)))


def run_case(case: dict) -> dict:
    """在独立临时目录里跑一条用例，返回可落盘的期望值。"""
    root = Path(tempfile.mkdtemp(prefix="amkr-logfiles-corpus-"))
    try:
        materialize(root, case.get("fixture") or [])
        arg = case["arg"]
        op = case["op"]
        record: dict = {
            "name": case["name"],
            "op": op,
            "arg": arg,
            "fixture": case.get("fixture") or [],
        }
        if case.get("now"):
            record["now"] = FIXED_NOW_TEXT
        if case.get("note"):
            record["note"] = case["note"]

        if op == "next_log_archive_path":
            now = FIXED_NOW if case.get("now") else None
            try:
                result = log_files.next_log_archive_path(resolve(root, arg), now)
            except ValueError:
                record["want_error"] = True
                return record
            record["want"] = relative(root, result)
            return record

        if op == "archive_current_log":
            try:
                result = run_archive(root, arg)
            except OSError:
                # 父路径被普通文件占位等：Python 抛的 OSError 子类随平台不同
                # （Windows 是 FileExistsError，Linux 是 NotADirectoryError），
                # 只记录「必须失败」。
                record["want_error"] = True
                return record
            record["want_archived"] = result is not None
            if result is not None:
                record["want"] = relative(root, result)
            record["want_tree"] = snapshot(root)
            return record

        if op == "archived_log_paths":
            paths = log_files.archived_log_paths(str(resolve(root, arg)))
            record["want_paths"] = [relative(root, item) for item in paths]
            return record

        raise AssertionError(f"未知操作: {op}")
    finally:
        shutil.rmtree(root, ignore_errors=True)


# --------------------------------------------------------------------------- #
# 用例
# --------------------------------------------------------------------------- #

def build_cases() -> list[dict]:
    cases: list[dict] = []

    def add(name: str, op: str, arg: str, *, fixture=None, now=False, note="") -> None:
        cases.append({
            "name": name, "op": op, "arg": arg,
            "fixture": fixture or [], "now": now, "note": note,
        })

    # ---------- next_log_archive_path ----------
    add("next_plain", "next_log_archive_path", "logs/server.log", now=True,
        note="常规：stem + 时间戳 + 原后缀")
    add("next_no_extension", "next_log_archive_path", "logs/server", now=True,
        note="无后缀回退成 .log")
    add("next_trailing_dot", "next_log_archive_path", "logs/server.", now=True,
        note="Python 的 PurePath.suffix 对结尾的点返回空串，走 .log 回退")
    add("next_dotfile", "next_log_archive_path", "logs/.hidden", now=True,
        note="点开头的名字没有后缀，回退成 .log")
    add("next_multi_extension", "next_log_archive_path", "logs/archive.tar.gz",
        now=True, note="后缀取最后一段 .gz，stem 保留 archive.tar")
    add("next_upper_extension", "next_log_archive_path", "logs/x.Server.LOG",
        now=True)
    add("next_collision_1", "next_log_archive_path", "logs/server.log", now=True,
        fixture=[{"path": "logs/server.20260102-030405.log", "text": "old"}],
        note="候选已存在时补 .1")
    add("next_collision_2", "next_log_archive_path", "logs/server.log", now=True,
        fixture=[{"path": "logs/server.20260102-030405.log", "text": "old"},
                 {"path": "logs/server.20260102-030405.1.log", "text": "old"}])
    add("next_collision_dir_counts", "next_log_archive_path", "logs/server.log",
        now=True,
        fixture=[{"path": "logs/server.20260102-030405.log", "text": "old"},
                 {"path": "logs/server.20260102-030405.1.log", "dir": True}],
        note="python 的 exists() 对目录同样为真，编号会跳过该目录")
    add("next_empty_name", "next_log_archive_path", "",
        note="空路径没有名字，Python 抛 ValueError")
    add("next_root_path", "next_log_archive_path", "/",
        note="根路径没有名字，Python 抛 ValueError")
    add("next_sibling_prefix", "next_log_archive_path", "logs/server.log",
        now=True,
        fixture=[{"path": "logs/server.log.20260102-030405", "text": "unrelated"}],
        note="只按构造出的候选名判存在，不按前缀猜")

    # ---------- archive_current_log ----------
    add("archive_nonempty", "archive_current_log", "case/app.log", now=True,
        fixture=[{"path": "case/app.log", "text": "第一行\n第二行\n"}],
        note="非空日志：改名归档 + 原路径重建空文件")
    add("archive_missing_file", "archive_current_log", "case/deep/app.log",
        now=True, note="文件与父目录都不存在：建目录 + 建空文件，返回 None")
    add("archive_empty_file", "archive_current_log", "case/app.log", now=True,
        fixture=[{"path": "case/app.log", "text": ""}],
        note="空文件不归档，只截断（结果等价）")
    add("archive_no_extension", "archive_current_log", "case/server", now=True,
        fixture=[{"path": "case/server", "text": "payload"}])
    add("archive_collision", "archive_current_log", "case/app.log", now=True,
        fixture=[{"path": "case/app.log", "text": "current"},
                 {"path": "case/app.20260102-030405.log", "text": "previous"}],
        note="归档名已占用时补 .1，旧归档保留")
    add("archive_only_dirs_missing", "archive_current_log", "a/b/c/app.log",
        now=True, note="多级父目录一次建齐")
    add("archive_blocked_parent", "archive_current_log", "blocked/app.log",
        now=True, fixture=[{"path": "blocked", "text": "i am a file"}],
        note="父路径被普通文件占位：Python 抛 OSError 子类，Go 只需返回错误")

    # ---------- archived_log_paths ----------
    add("archived_none", "archived_log_paths", "case/server.log",
        fixture=[{"path": "case/server.log", "text": "current"}])
    add("archived_missing_parent", "archived_log_paths", "nope/server.log")
    add("archived_sorted_desc", "archived_log_paths", "case/server.log",
        fixture=[
            {"path": "case/server.log", "text": "current"},
            {"path": "case/server.20260101-000000.log", "text": "a"},
            {"path": "case/server.20260102-000000.log", "text": "b"},
            {"path": "case/server.20260102-000000.1.log", "text": "c"},
            {"path": "case/other.20260102-000000.log", "text": "d"},
        ],
        note="按文件名倒序；不同 stem 的不算")
    add("archived_excludes_self", "archived_log_paths",
        "case/server.20260101-000000.log",
        fixture=[{"path": "case/server.20260101-000000.log", "text": "a"}],
        note="归档文件自己也能匹配模式，但被 candidate != path 排除")
    add("archived_filters_directories", "archived_log_paths", "case/server.log",
        fixture=[
            {"path": "case/server.log", "text": "current"},
            {"path": "case/server.dir.20260101-000000.log", "dir": True},
            {"path": "case/server.dir.20260101-000000.log/inner.log", "text": "x"},
            {"path": "case/server.20260101-000000.log", "text": "a"},
        ],
        note="目录不算归档（is_file 过滤）")
    add("archived_suffix_fallback", "archived_log_paths", "case/server",
        fixture=[
            {"path": "case/server", "text": "current"},
            {"path": "case/server.20260101-000000.log", "text": "a"},
            {"path": "case/server.20260101-000000.txt", "text": "b"},
        ],
        note="无后缀的日志文件按 .log 找归档")
    add("archived_multi_extension", "archived_log_paths", "case/archive.tar.gz",
        fixture=[
            {"path": "case/archive.tar.gz", "text": "current"},
            {"path": "case/archive.tar.20260101-000000.gz", "text": "a"},
            {"path": "case/archive.tar.20260101-000000.log", "text": "b"},
        ])
    add("archived_dotfile", "archived_log_paths", "case/.log",
        fixture=[
            {"path": "case/.log", "text": "current"},
            {"path": "case/.log.20260101-000000.log", "text": "a"},
        ],
        note="点开头的名字没有后缀，模式是 '.log.*.log'；pathlib 不过滤隐藏文件")
    add("archived_class_pattern", "archived_log_paths", "case/a[b].log",
        fixture=[
            {"path": "case/a[b].log", "text": "current"},
            {"path": "case/ab.20260101-000000.log", "text": "a"},
            {"path": "case/ac.20260101-000000.log", "text": "b"},
            {"path": "case/a[b].20260101-000000.log", "text": "c"},
        ],
        note="stem 里的 [] 会被当成字符类：两个实现（fnmatch / filepath.Match）"
             "在这里恰好一致，故可入语料")
    add("archived_parent_is_file", "archived_log_paths", "case.log/server.log",
        fixture=[{"path": "case.log", "text": "i am a file"}],
        note="父路径是普通文件：pathlib 吞掉 OSError 返回空列表")
    add("archived_upper_extension", "archived_log_paths", "case/x.Server.LOG",
        fixture=[
            {"path": "case/x.Server.LOG", "text": "current"},
            {"path": "case/x.Server.20260101-000000.LOG", "text": "a"},
        ],
        note="大小写完全一致，避免把 pathlib 的 Windows 大小写不敏感写进语料")

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

    path = args.output_dir / "logfiles_corpus.json"
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
