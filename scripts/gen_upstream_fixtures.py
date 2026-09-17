#!/usr/bin/env python3
"""生成 upstream 假上游交互的回放语料（Python 侧为参照实现）。

Go 侧 ``internal/upstream`` 要与 Python 版代理行为对拍，但 Python 测试里的假上游
全部是**内联闭包**：``httpx.MockTransport(handler)`` 的 ``handler`` 定义在测试函数
内部，无法 import，也没有任何 cassette/golden 文件可复用。因此本脚本换一条路：
**真正把测试跑一遍**，在 ``MockTransport`` 这一层拦截「请求 → 响应」，把可观测
契约原样落盘。语料来自真实测试路径，而不是照着测试源码手抄，避免抄错或抄成
「文档说应该怎样」。

为什么在 ``MockTransport.handle_async_request`` 上拦截，而不是解析源码：

1. 闭包无法 import，静态解析只能得到「大概」，拿不到真正的 header/body 字节；
2. 拦截点位于 HTTP 客户端与「上游」之间，两侧都是真实对象——请求是代理真正发
   出的（含 auth、超时扩展、body 序列化顺序），响应是 handler 真正合成的；
3. 参数化测试会跑多次，每次都成为一条独立记录，天然覆盖失败切换等序列。

**不会改动任何测试行为**：拦截器只读取 ``request``/``response``，绝不消费响应流
（消费会提前触发 SSE、破坏超时类测试），也不修改请求。

语料刻意剔除 ``user-agent``：它随 httpx/starlette 版本变化，不属于 AMKR 的对外
契约；保留它会让语料在升级依赖后无意义地过期。其余 header、body 字节、状态码、
响应头与 **SSE 分块边界**全部保留——分块边界是 ``_stream_upstream`` 拆分/刷写逻辑
的可观测契约。

无法建模的两类站点会在运行时统计并打印（不写入语料）：

- handler 从未被调用（例如上游请求在鉴权阶段就被拦下）；
- handler 被调用但被代理的超时**取消**，没有产生响应。

用法::

    python scripts/gen_upstream_fixtures.py           # 写入语料
    python scripts/gen_upstream_fixtures.py --check   # 只校验语料是否最新

站点用 AST 扫描整个 ``tests/``，只驱动**确实含假上游**的测试文件；当前全部 73 个
站点都在 ``tests/test_app.py``。

注意：``--check`` 需要重跑测试来重放交互，因此耗时与 ``pytest tests/test_app.py``
相当（约 2~3 分钟），比其它纯函数语料生成器慢，这是「用真实实现产出语料」的代价。
"""

from __future__ import annotations

import argparse
import ast
import base64
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

DEFAULT_OUTPUT = (
    REPO_ROOT / "internal" / "upstream" / "testdata" / "python_upstream_fixtures.jsonl"
)
TESTS_DIR = REPO_ROOT / "tests"

# 随依赖版本变化的请求头，不构成契约，落盘前剔除（见模块 docstring）。
VOLATILE_REQUEST_HEADERS = frozenset({"user-agent"})


# --------------------------------------------------------------------------- #
# 静态定位：枚举 tests/ 下所有 httpx.MockTransport 站点
# --------------------------------------------------------------------------- #

def _handler_lines(node: ast.AST) -> dict[str, list[int]]:
    """**某个测试函数内**的「嵌套函数名 → 定义行列表」。

    必须按测试函数分别建表：几乎每个测试都把自己的闭包命名为 ``handler``，
    若在整个模块上建一张扁平表，后定义的会覆盖先定义的，归属就会全部指错。
    同一名字可能定义多次，因此保留全部行号，由调用方取「transport 之前最近的一个」。
    """
    lines: dict[str, list[int]] = {}
    for sub in ast.walk(node):
        if isinstance(sub, (ast.FunctionDef, ast.AsyncFunctionDef)):
            lines.setdefault(sub.name, []).append(sub.lineno)
    return lines


def find_sites() -> list[dict]:
    """扫描 ``tests/`` 全部测试文件，列出每个 ``httpx.MockTransport(handler)`` 站点。

    用 AST 而不是正则：需要把 ``MockTransport`` 的实参名解析回 handler 函数定义，
    拿到行号才能在运行时把拦截到的调用**归属**回具体站点（同名 ``handler`` 只有
    定义行号能区分）。

    扫描**整个 tests/** 而不是写死单个文件：将来在别的测试文件里新增假上游时，
    新的测试文件会被自动纳入驱动范围，不会静默漏采集。
    """
    sites: list[dict] = []
    for path in sorted(TESTS_DIR.rglob("test_*.py")):
        relpath = path.relative_to(REPO_ROOT).as_posix()
        tree = ast.parse(path.read_bytes().decode("utf-8"))
        # 模块级 def 作为兜底：handler 也可能定义在模块顶层（被多个测试共用），
        # 此时测试函数内部找不到它。优先级低于测试内的闭包。
        module_lines = _handler_lines(tree)
        for node in tree.body:
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            nested_lines = _handler_lines(node)
            for sub in ast.walk(node):
                if not (
                    isinstance(sub, ast.Call)
                    and isinstance(sub.func, ast.Attribute)
                    and sub.func.attr == "MockTransport"
                ):
                    continue
                argument = sub.args[0] if sub.args else None
                handler_name = argument.id if isinstance(argument, ast.Name) else None
                candidates = [
                    line
                    for source in (nested_lines, module_lines)
                    for line in source.get(handler_name, [])
                    if line < sub.lineno  # 只认 transport 之前定义的那个函数
                ]
                sites.append(
                    {
                        "file": relpath,
                        "test": node.name,
                        "test_line": node.lineno,
                        "handler": handler_name,
                        "handler_line": max(candidates) if candidates else -1,
                        "transport_line": sub.lineno,
                    }
                )
    # (文件, 测试定义行, transport 行) 即源码顺序 = pytest 收集顺序，
    # 用作语料的**稳定排序键**：语料顺序不依赖字典/集合的迭代顺序。
    sites.sort(key=lambda site: (site["file"], site["test_line"], site["transport_line"]))
    return sites


def driven_files(sites: list[dict]) -> list[str]:
    """只运行含站点的测试文件——没有假上游的测试文件跑了对语料毫无贡献。"""
    return sorted({site["file"] for site in sites})


def site_key(entry: dict) -> tuple[str, str, int]:
    """静态站点与运行时事件的共同归属键：(文件, 测试名, handler 定义行)。

    handler 行号必须进键：同一文件里每个测试都有自己的闭包 ``handler``，名字全叫
    ``handler``，只有定义行号能区分是哪个站点。
    """
    return (entry["file"], entry["test"], entry["handler_line"])


# --------------------------------------------------------------------------- #
# 运行时采集：跑真实测试，在 MockTransport 边界拦截
# --------------------------------------------------------------------------- #

class _ExchangeRecorder:
    """pytest 插件：把每次假上游交互记成一条「请求 → 响应」。

    以插件实例（而非临时文件）注入，避免生成临时模块污染工作区。
    """

    def __init__(self) -> None:
        self.exchanges: list[dict] = []
        self.invocations: list[dict] = []
        self.current: dict[str, str] = {"file": "", "test": "", "case": ""}

    # -- pytest 钩子 ------------------------------------------------------- #

    def pytest_configure(self, config) -> None:  # noqa: ARG002 - pytest 钩子签名
        import httpx

        recorder = self

        async def handle_async_request(transport, request):  # noqa: ANN001
            return await recorder._intercept(transport, request)

        # 类级打桩：测试里每次 httpx.MockTransport(handler) 都会走到这里。
        httpx.MockTransport.handle_async_request = handle_async_request

    def pytest_runtest_setup(self, item) -> None:
        nodeid = item.nodeid
        self.current = {
            # nodeid 形如 "tests/test_app.py::test_x[case]"；文件也必须记下来，
            # 否则不同文件里的同名测试会互相覆盖归属。
            "file": nodeid.split("::")[0].replace("\\", "/"),
            "test": nodeid.split("::")[-1].split("[")[0],
            "case": nodeid.split("[", 1)[1].rstrip("]") if "[" in nodeid else "",
        }

    # -- 拦截 -------------------------------------------------------------- #

    async def _intercept(self, transport, request):  # noqa: ANN001
        import httpx

        await request.aread()
        site = self.current.copy()
        handler = transport.handler
        # handler 的 co_firstlineno 是运行时归属到静态站点的**唯一可靠键**：测试里
        # 同名 handler 会有多个（每个测试一个闭包），只有行号能区分。
        site["handler_line"] = getattr(
            getattr(handler, "__code__", None), "co_firstlineno", -1
        )
        invocation = {
            **site,
            "outcome": "responded",
        }
        self.invocations.append(invocation)

        try:
            response = handler(request)
            if not isinstance(response, httpx.Response):
                # handler 允许是 async 函数：MockTransport 会原样返回协程。
                response = await response
        except BaseException as exc:  # noqa: BLE001 - 取消/异常都要如实记录归属
            invocation["outcome"] = "cancelled"
            invocation["exception"] = type(exc).__name__
            raise

        invocation["outcome"] = "responded"
        # 只读快照：绝不消费响应流，否则会改变超时/流式测试的行为。
        self.exchanges.append({**site, **self._snapshot(request, response)})
        return response

    @staticmethod
    def _snapshot(request, response) -> dict:  # noqa: ANN001
        stream_kind, chunks, opaque = _describe_stream(response)
        timeout = request.extensions.get("timeout")
        joined = b"".join(chunks)
        return {
            "request": {
                "method": request.method,
                "url": str(request.url),
                "path": request.url.path,
                "query": request.url.query.decode("ascii", "replace"),
                "headers": _headers(request),
                "body": _body_field(request.content),
                "timeout": dict(timeout) if isinstance(timeout, dict) else None,
            },
            "response": {
                "status": response.status_code,
                "headers": _headers(response),
                "stream": stream_kind,
                "body_b64": base64.b64encode(joined).decode("ascii") if chunks else None,
                # 只有多分块时才额外记录边界：单块与 body_b64 等价，重复会让语料变噪。
                "chunks_b64": (
                    [base64.b64encode(chunk).decode("ascii") for chunk in chunks]
                    if stream_kind == "chunks"
                    else None
                ),
                "opaque_stream": opaque,
            },
        }


def _headers(message) -> dict:  # noqa: ANN001
    """请求/响应头 → 稳定 dict；剔除随依赖版本变化的项。"""
    import httpx

    headers = message.headers
    if isinstance(message, httpx.Request):
        return {
            key: value
            for key, value in headers.multi_items()
            if key.lower() not in VOLATILE_REQUEST_HEADERS
        }
    return dict(headers.multi_items())


def _body_field(content: bytes) -> dict:
    """请求体：能无损按 UTF-8 解码就存文本，否则退回 base64。

    文本形式让语料可读、可直接 diff；base64 分支保证任意字节都不丢。
    """
    try:
        return {"encoding": "utf-8", "value": content.decode("utf-8")}
    except UnicodeDecodeError:
        return {"encoding": "base64", "value": base64.b64encode(content).decode("ascii")}


def _describe_stream(response) -> tuple[str, list[bytes], dict | None]:  # noqa: ANN001
    """还原响应体的分块结构，**不消费流**。

    httpx 对 ``json=`` / ``content=`` 合成的响应会把字节放在 ``_content``，且
    ``stream`` 是同一份字节的 ``ByteStream``；对 ``stream=自定义`` 的响应则只有
    流对象。两者都从 ``__dict__`` 里读缓存字段，因此不会触发 ``__aiter__``。

    自定义流若没有可读的字节属性（如 ``HangingStream``、``BrokenStream``：它们靠
    sleep/抛错来表达超时），就无法回放，只记录类名，供 Go 侧手工构造等价物。
    """
    content = response.__dict__.get("_content")
    if isinstance(content, bytes):
        return "bytes", [content], None

    stream = response.stream
    label = f"{type(stream).__module__}.{type(stream).__qualname__}"
    cache = getattr(stream, "__dict__", {})
    buffered = cache.get("_stream")
    if isinstance(buffered, bytes):
        return "bytes", [buffered], None
    chunked = cache.get("chunks")
    if isinstance(chunked, (list, tuple)) and all(
        isinstance(chunk, bytes) for chunk in chunked
    ):
        kind = "chunks" if len(chunked) > 1 else "bytes"
        return kind, list(chunked), None
    return "opaque", [], {"class": label}


# --------------------------------------------------------------------------- #
# 组装语料
# --------------------------------------------------------------------------- #

def build_records() -> tuple[list[dict], list[dict], list[dict]]:
    """返回 (语料记录, 全部站点, 全部调用事件)。"""
    import os

    import pytest

    # 先静态定位站点，再只驱动「确实含假上游」的测试文件。
    sites = find_sites()
    targets = driven_files(sites)
    recorder = _ExchangeRecorder()
    # pytest 需要以仓库根为 cwd 才能按 tests/ 的相对路径收集，并让测试内的相对路径
    # 行为与 CI 一致；脚本可能在任意目录被调用，因此显式切换。
    os.chdir(REPO_ROOT)
    exit_code = pytest.main(
        ["-q", "--no-header", "-p", "no:cacheprovider", *targets],
        plugins=[recorder],
    )
    if exit_code != pytest.ExitCode.OK:
        print(
            f"[upstream] 采集失败：{'、'.join(targets)} 未通过"
            f"（pytest 退出码 {int(exit_code)}），语料必须由全绿的真实测试产出",
            file=sys.stderr,
        )
        raise SystemExit(2)

    by_key: dict[tuple[str, str, int], list[dict]] = {}
    for record in recorder.exchanges:
        by_key.setdefault(site_key(record), []).append(record)

    records: list[dict] = []
    for site in sites:
        captured = by_key.get(site_key(site), [])
        site["captured"] = len(captured)
        for index, exchange in enumerate(captured):
            records.append(
                {
                    "id": f"{len(records) + 1:04d}",
                    "test": site["test"],
                    "case": exchange["case"],
                    "invocation": index,
                    "source": {
                        "file": site["file"],
                        "test_line": site["test_line"],
                        "handler_line": site["handler_line"],
                        "transport_line": site["transport_line"],
                    },
                    "request": exchange["request"],
                    "response": exchange["response"],
                }
            )
    return records, sites, recorder.invocations


def render(records: list[dict]) -> str:
    """JSONL：每行一条，键排序，无时间戳、无绝对路径。"""
    return (
        "\n".join(
            json.dumps(record, ensure_ascii=False, sort_keys=True) for record in records
        )
        + "\n"
    )


def report_coverage(sites: list[dict], events: list[dict]) -> None:
    """如实报告覆盖率：只打印，不写进语料（语料只放可回放的交互）。

    覆盖率必须诚实：Go 侧只能断言真正落盘的交互，漏掉的站点要显式列出，否则
    「语料全绿」会被误读成「行为已完全对齐」。
    """
    captured = sum(1 for site in sites if site["captured"])
    exchanges = sum(site["captured"] for site in sites)
    print(
        f"[upstream] MockTransport 站点 {len(sites)} 个，"
        f"取到响应 {captured} 个、交互 {exchanges} 条"
    )
    # 按站点汇总调用结局，逐站点给原因。
    outcomes: dict[tuple[str, str, int], set[str]] = {}
    for event in events:
        outcomes.setdefault(site_key(event), set()).add(event["outcome"])
    missing: list[str] = []
    for site in sites:
        if site["captured"]:
            continue
        seen = outcomes.get(site_key(site), set())
        if not seen:
            reason = "handler 未被调用（请求在到达上游前已返回）"
        elif "cancelled" in seen:
            reason = "handler 被超时取消，未产生响应"
        else:
            reason = "handler 已调用但响应不可快照"
        missing.append(
            f"{site['file']}::{site['test']}:{site['transport_line']} ({reason})"
        )
    if missing:
        print(f"[upstream] 未产出语料的站点 {len(missing)} 个（原因见括号）:")
        for entry in missing:
            print(f"  - {entry}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument(
        "--check",
        action="store_true",
        help="只校验现有语料与当前实现一致，不写盘（CI 用）",
    )
    args = parser.parse_args()

    records, sites, events = build_records()
    content = render(records)
    label = "upstream"

    if args.check:
        report_coverage(sites, events)
        if not args.output.exists():
            print(f"[{label}] 语料缺失: {args.output}", file=sys.stderr)
            return 1
        # 用 bytes 比较：避免平台换行转换掩盖差异（Python 3.12 无 read_text(newline=)）。
        existing = args.output.read_bytes().decode("utf-8")
        if existing != content:
            print(
                f"[{label}] 语料已过期: {args.output}\n"
                f"请重新运行: python {Path(__file__).name}",
                file=sys.stderr,
            )
            return 1
        print(f"[{label}] 语料最新: {args.output}")
        return 0

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(content, encoding="utf-8", newline="\n")
    report_coverage(sites, events)
    print(f"[{label}] 已写入 {len(records)} 条语料: {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
