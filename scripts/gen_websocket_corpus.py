#!/usr/bin/env python3
"""生成 event_bus / websocket_proxy 的差分对拍语料。

两个模块都是「契约在数字里」的类型：关闭码 4001/4003、10 秒超时、节流的三条规则、
「空 body 一帧都不发、空 chunk 却要发一个空帧」、文本帧与二进制帧的判定大小写敏感。
这些都不会报错，只会让客户端拿到不同的东西，所以期望值必须由**真实 Python 模块**
产出，而不是手写。

产出的语料：

* ``internal/eventbus/testdata/auth.jsonl`` —— 鉴权握手的可观测结果。驱动真实的
  ``EventBus.authenticate``（注入脚本化的假 WebSocket），记录 ok/4001/4003、
  关闭原因、verify 是否被调用、订阅者数与计数回调。10 秒超时用「替换
  ``event_bus.asyncio`` 的 wait_for 并记录 timeout 实参」的方式观测，既不必真等
  10 秒，又能锁住那个常量。
* ``internal/eventbus/testdata/broadcast.jsonl`` —— 四个事件类型的**逐字节**帧文本。
  驱动真实的 ``EventBus.broadcast``，记录假客户端收到的文本。分隔符是 json.dumps
  的默认值（", " / ": "），这是本迁移里唯一不紧凑的 JSON 输出。
* ``internal/eventbus/testdata/throttle.jsonl`` —— 节流决策。app.py:113-124 的循环
  体是 ``create_app`` 里的闭包，无法 import，因此这里**逐行转写**循环体，用虚拟
  时钟替换 ``asyncio.wait_for`` / ``asyncio.sleep``（脚本化调度），并把
  ``_metrics_dirty.set()`` 投递到指定时刻。转写的每一行都有
  ``check_app_source()`` 断言 app.py 仍然存在，防止两边漂移。
* ``internal/eventbus/testdata/e2e.jsonl`` —— 用真实 FastAPI 应用 + TestClient 观测的
  关闭码与帧顺序，用来验证「假 WebSocket 模型 == 真服务器」。
* ``internal/wsproxy/testdata/proxy.jsonl`` —— 三段：
  ``synthesize``（驱动 ``_websocket_http_request``，记录折算出的 method/scheme/path/
  query/headers 以及 ``_upstream_headers`` 的结果）、``frames``（真实最小 FastAPI 应用
  + TestClient，记录帧序列与关闭码）、``close_code``（状态码 → 关闭码的全表）。

``expect`` 一律是 ``json.dumps(..., separators=(",", ":"))`` 的紧凑文本，因它是**数据**
而非响应体；真正要逐字节比的是其中的帧文本字段。

注意：生成与 ``--check`` 期间 stderr 上会出现一条来自参照实现的
``RuntimeError: boom`` 日志——那是 ``handler_raises`` / ``e2e`` 用例**故意**触发
异常路径时 FastAPI 记的日志（LOGGER.exception），不是生成失败。

用法::

    python scripts/gen_websocket_corpus.py
    python scripts/gen_websocket_corpus.py --check
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import copy
import json
import shutil
import sys
import tempfile
import time
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from fastapi import FastAPI, WebSocket as FastAPIWebSocket  # noqa: E402
from fastapi.responses import Response, StreamingResponse  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402
from starlette.websockets import WebSocket, WebSocketDisconnect  # noqa: E402

import auto_model_key_router.event_bus as event_bus_module  # noqa: E402
from auto_model_key_router.app import create_app  # noqa: E402
from auto_model_key_router.config import RouterConfig  # noqa: E402
from auto_model_key_router.event_bus import EventBus  # noqa: E402
from auto_model_key_router.proxy_support import _upstream_headers  # noqa: E402
from auto_model_key_router.websocket_proxy import (  # noqa: E402
    _websocket_close_code,
    _websocket_http_request,
    register_websocket_proxy,
)

EVENTBUS_DIR = REPO_ROOT / "internal" / "eventbus" / "testdata"
WSPROXY_DIR = REPO_ROOT / "internal" / "wsproxy" / "testdata"

# 参照实现里的两个节流常量；语料里带上它们，Go 侧断言自己的常量与之一致。
IDLE_BROADCAST_INTERVAL = 30.0
BROADCAST_MIN_INTERVAL = 1.0


# --------------------------------------------------------------------------- #
# 基础工具
# --------------------------------------------------------------------------- #


def compact(obj: object) -> str:
    """紧凑 JSON 文本，作为 expect 的统一载体。"""
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


def render(cases: list[dict]) -> str:
    """一行一个 case。外层键排序（expect 是字符串，不受影响）。"""
    return "\n".join(
        json.dumps(case, ensure_ascii=False, sort_keys=True) for case in cases
    ) + "\n"


def b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


# --------------------------------------------------------------------------- #
# eventbus / auth.jsonl
# --------------------------------------------------------------------------- #


class FakeWebSocket:
    """脚本化的假 WebSocket，只实现 EventBus 用到的三个方法。

    参照实现直接吃 starlette 的 WebSocket，但那个对象必须跑在真实 ASGI 服务器里；
    握手逻辑（超时/JSON 解析/短路/关闭码）与 socket 无关，所以这里用最小的替身，
    让每个输入都可控。e2e.jsonl 负责验证替身模型与真服务器一致。
    """

    def __init__(self, *, text: str | None = None, error: BaseException | None = None,
                 block_forever: bool = False) -> None:
        self._text = text
        self._error = error
        self._block_forever = block_forever
        self.closed: list[list[Any]] = []
        self.sent: list[str] = []

    async def receive_text(self) -> str:
        if self._block_forever:
            await asyncio.Event().wait()
        if self._error is not None:
            raise self._error
        assert self._text is not None
        return self._text

    async def send_text(self, text: str) -> None:
        self.sent.append(text)

    async def close(self, code: int = 1000, reason: str | None = None) -> None:
        self.closed.append([code, reason])


class TimeoutProbe:
    """替换 ``event_bus.asyncio`` 的探针：记录 wait_for 的 timeout 实参后立刻超时。

    真等 10 秒没必要，但那个常量必须被观测到——所以把 wait_for 换掉、把 timeout
    记录下来，再抛 asyncio.TimeoutError（与真实超时同类，except 分支照常命中）。

    必须同时暴露 TimeoutError：`except (asyncio.TimeoutError, ..., Exception)` 里的
    asyncio 也在 event_bus 的模块命名空间里查，缺了它会变成 AttributeError。
    """

    TimeoutError = asyncio.TimeoutError

    def __init__(self) -> None:
        self.timeouts: list[float] = []

    async def wait_for(self, awaitable, timeout):  # noqa: ANN001
        self.timeouts.append(timeout)
        awaitable.close()
        raise asyncio.TimeoutError


# AUTH_CASES 的字段：name / note / frame_kind / frame / verify
# frame_kind: text（文本首帧）/ binary（二进制首帧）/ disconnect（对端已断开）/
#             timeout（一直不来）
AUTH_CASES: list[dict[str, Any]] = [
    {"name": "auth_valid", "frame_kind": "text",
     "frame": '{"type": "auth", "token": "local-key"}', "verify": "ok",
     "note": "合法首帧 + verify 通过：加入订阅列表并回调 count=1"},
    {"name": "auth_verify_false", "frame_kind": "text",
     "frame": '{"type": "auth", "token": "wrong"}', "verify": "fail",
     "note": "verify 返回 False -> 4003；verify 被调用过一次"},
    {"name": "auth_type_mismatch", "frame_kind": "text",
     "frame": '{"type": "hello", "token": "local-key"}', "verify": "ok",
     "note": "type 不是 auth -> 4003，且 **verify 不会被调用**（短路）"},
    {"name": "auth_type_int", "frame_kind": "text",
     "frame": '{"type": 1, "token": "local-key"}', "verify": "ok",
     "note": "type 是数字 -> 4003，短路"},
    {"name": "auth_token_int", "frame_kind": "text",
     "frame": '{"type": "auth", "token": 1}', "verify": "ok",
     "note": "token 不是 str -> 4003，短路"},
    {"name": "auth_token_bool", "frame_kind": "text",
     "frame": '{"type": "auth", "token": true}', "verify": "ok",
     "note": "bool 不是 str（Python 的 isinstance(True, str) 为假）-> 4003"},
    {"name": "auth_token_null", "frame_kind": "text",
     "frame": '{"type": "auth", "token": null}', "verify": "ok",
     "note": "token 是 null -> 4003"},
    {"name": "auth_token_missing", "frame_kind": "text",
     "frame": '{"type": "auth"}', "verify": "ok",
     "note": "缺 token -> 4003"},
    {"name": "auth_extra_fields", "frame_kind": "text",
     "frame": '{"token": "local-key", "type": "auth", "extra": [1, 2]}',
     "verify": "ok", "note": "多余字段被忽略；键序无关"},
    {"name": "auth_empty_token", "frame_kind": "text",
     "frame": '{"type": "auth", "token": ""}', "verify": "ok",
     "note": "空串是合法 str，交给 verify 决定（这里通过）"},
    {"name": "auth_bad_json", "frame_kind": "text", "frame": "{not json",
     "verify": "ok", "note": "坏 JSON -> 4001"},
    {"name": "auth_empty_frame", "frame_kind": "text", "frame": "",
     "verify": "ok", "note": "空帧 -> 4001"},
    {"name": "auth_json_float", "frame_kind": "text", "frame": "1.5",
     "verify": "ok", "note": "合法 JSON 但不是对象 -> AttributeError 冒泡（实测）"},
    {"name": "auth_json_null", "frame_kind": "text", "frame": "null",
     "verify": "ok", "note": "null.get -> AttributeError，**不关连接**"},
    {"name": "auth_json_array", "frame_kind": "text", "frame": "[1, 2]",
     "verify": "ok", "note": "list.get -> AttributeError，不关连接"},
    {"name": "auth_json_string", "frame_kind": "text", "frame": '"auth"',
     "verify": "ok", "note": "str.get -> AttributeError，不关连接"},
    {"name": "auth_binary_frame", "frame_kind": "binary", "frame": None,
     "verify": "ok",
     "note": "二进制首帧：receive_text 取不到 text（KeyError 或 None）-> 4001"},
    {"name": "auth_disconnect", "frame_kind": "disconnect", "frame": None,
     "verify": "ok", "note": "首帧前对端断开 -> 4001"},
    {"name": "auth_timeout", "frame_kind": "timeout", "frame": None,
     "verify": "ok", "note": "10 秒内没有首帧 -> 4001；同时锁住 timeout=10.0"},
    {"name": "auth_verify_raises", "frame_kind": "text",
     "frame": '{"type": "auth", "token": "local-key"}', "verify": "raise",
     "note": "verify 抛异常：在 try 之外 -> 冒泡，不关连接"},
]


async def run_auth_case(case: dict[str, Any]) -> dict[str, Any]:
    frame_kind = case["frame_kind"]
    if frame_kind == "timeout":
        websocket = FakeWebSocket(block_forever=True)
    elif frame_kind == "disconnect":
        websocket = FakeWebSocket(error=WebSocketDisconnect(code=1000))
    elif frame_kind == "binary":
        # 参照实现收到二进制帧时 `message["text"]` 取不到键；用它真实的异常类型。
        websocket = FakeWebSocket(error=KeyError("text"))
    else:
        websocket = FakeWebSocket(text=case["frame"])

    bus = EventBus()
    callbacks: list[int] = []

    async def on_change(count: int) -> None:
        callbacks.append(count)

    bus.on_client_count_change = on_change
    verify_calls: list[str] = []

    async def verify(token: str) -> bool:
        verify_calls.append(token)
        if case["verify"] == "raise":
            raise RuntimeError("verifier exploded")
        return case["verify"] == "ok"

    probe: TimeoutProbe | None = None
    original_asyncio = event_bus_module.asyncio
    if frame_kind == "timeout":
        probe = TimeoutProbe()
        event_bus_module.asyncio = probe  # type: ignore[assignment]
    try:
        authenticated = await bus.authenticate(websocket, verify)
        outcome: dict[str, Any] = {"result": "returned", "authenticated": authenticated}
    except BaseException as error:  # noqa: BLE001 - 参照实现就是靠冒泡区分这两类
        outcome = {
            "result": "raised",
            "exception": type(error).__name__,
            "message": str(error),
        }
    finally:
        event_bus_module.asyncio = original_asyncio  # type: ignore[assignment]

    closed = websocket.closed[-1] if websocket.closed else None
    return {
        "name": case["name"],
        "kind": "auth",
        "note": case["note"],
        "frame_kind": frame_kind,
        "frame": case["frame"],
        "verify": case["verify"],
        "auth_timeout": probe.timeouts[0] if probe and probe.timeouts else None,
        "expect": compact({
            "outcome": outcome,
            "close_code": closed[0] if closed else None,
            "close_reason": closed[1] if closed else None,
            "verify_calls": verify_calls,
            "client_count": bus.client_count,
            "callbacks": callbacks,
        }),
    }


def build_auth_cases() -> list[dict[str, Any]]:
    return [asyncio.run(run_auth_case(case)) for case in AUTH_CASES]


# --------------------------------------------------------------------------- #
# eventbus / broadcast.jsonl
# --------------------------------------------------------------------------- #

# 快照形状取自 metrics.snapshot 的字段，但数值是造的：这里要锁的是**序列化**，
# 不是指标计算。
SNAPSHOT_DATA = {
    "requests": 3,
    "successes": 2,
    "failures": 1,
    "retries": 0,
    "prompt_tokens": 120,
    "completion_tokens": 30,
    "total_tokens": 150,
    "cached_tokens": 0,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0,
    "total_duration_ms": 1234,
    "min_duration_ms": 100,
    "max_duration_ms": 900,
    "avg_duration_ms": 411,
    "total_first_token_ms": 300,
    "min_first_token_ms": 50,
    "max_first_token_ms": 150,
    "avg_first_token_ms": 150,
    "status_codes": {"200": 2, "500": 1},
    "current_rpm": 1.5,
    "current_tpm": 0.0,
    "active_requests": 0,
    "router_status": "healthy",
    "started_at": "2026-07-14T12:00:00+08:00",
    "generated_at": "2026-07-14T12:34:56+08:00",
    "database_path": "<db>",
}

BROADCAST_CASES: list[dict[str, Any]] = [
    {"name": "connected", "event_type": "connected", "data": {}, "clients": 1,
     "note": "app.py:342 的空对象，无客户端时也不发"},
    {"name": "client_count_one", "event_type": "client_count", "data": {"count": 1},
     "clients": 1, "note": "app.py:130 的计数事件"},
    {"name": "client_count_zero", "event_type": "client_count", "data": {"count": 0},
     "clients": 1, "note": "计数可以为 0（断开时）"},
    {"name": "metrics_snapshot", "event_type": "metrics_snapshot",
     "data": SNAPSHOT_DATA, "clients": 1, "note": "嵌套对象 + 浮点 + 整数键"},
    {"name": "config_change", "event_type": "config_change",
     "data": {"reloaded": True}, "clients": 1, "note": "app.py:475 的 True -> true"},
    {"name": "unicode_and_escapes", "event_type": "config_change",
     "data": {"模型": "中文🚀", "quote": 'a"b\\c', "newline": "a\nb\tc",
              "control": "\u0001\u001f"},
     "clients": 1, "note": "ensure_ascii=False：非 ASCII 原样；控制字符仍转义"},
    {"name": "nested_arrays", "event_type": "metrics_snapshot",
     "data": {"series": [[1, 2.5], [], [None, True, False]],
              "obj": {"inner": {"deep": [1]}}},
     "clients": 1, "note": "空数组、null、bool 与多层嵌套的分隔符"},
    {"name": "special_floats", "event_type": "metrics_snapshot",
     "data": {"nan": float("nan"), "inf": float("inf"),
              "neg_inf": float("-inf"), "zero": 0.0, "neg_zero": -0.0,
              "exp": 1e-07, "big": 1e21, "small": 5e-324},
     "clients": 1, "note": "Python 默认 allow_nan：NaN/Infinity 是非标 JSON"},
    {"name": "no_clients", "event_type": "metrics_snapshot", "data": SNAPSHOT_DATA,
     "clients": 0,
     "note": "零订阅者：broadcast 连一帧都不发（event_bus.py:76-77 提前返回）"},
]


async def run_broadcast_case(case: dict[str, Any]) -> dict[str, Any]:
    websocket = FakeWebSocket(text='{"type": "auth", "token": "local-key"}')
    bus = EventBus()

    async def verify(_token: str) -> bool:
        return True

    if case["clients"] > 0:
        for _ in range(case["clients"]):
            await bus.authenticate(websocket, verify)
    websocket.sent.clear()
    await bus.broadcast(case["event_type"], copy.deepcopy(case["data"]))
    return {
        "name": case["name"],
        "kind": "broadcast",
        "note": case["note"],
        "event_type": case["event_type"],
        # data 以**文本**形式入语料（而不是 JSON 对象）有两个必要原因：
        #   1. Python 默认 allow_nan，data 里可能有 NaN/Infinity 字面量，标准 JSON
        #      解析器（Go 的 encoding/json）会直接拒绝整行；
        #   2. 对象键的**插入顺序**是序列化契约的一部分，走了 map 就丢了。
        "data_json": json.dumps(case["data"], ensure_ascii=False),
        "clients": case["clients"],
        "expect": compact(websocket.sent),
    }


def build_broadcast_cases() -> list[dict[str, Any]]:
    return [asyncio.run(run_broadcast_case(case)) for case in BROADCAST_CASES]


# --------------------------------------------------------------------------- #
# eventbus / throttle.jsonl
# --------------------------------------------------------------------------- #


class VirtualScheduler:
    """把 app.py:113-124 的循环体跑在虚拟时钟上。

    循环体见 ``broadcast_metrics_loop``（逐行转写）。这里替换两处 asyncio 原语：

    * ``wait_for(dirty.wait(), timeout)`` —— 若在虚拟超时点之前有预定的
      ``_metrics_dirty.set()``，就先把时钟推到那一刻并真的 ``set()``（Event 已置位时
      ``wait()`` 立刻返回；sleep 期间到达的写入同样会让下一轮立刻返回），否则把时钟
      推到超时点并抛 asyncio.TimeoutError；
    * ``sleep(1.0)`` —— 直接把时钟推后，不真的让出。

    下一次唤醒超过 ``until`` 时抛 StopSimulation 结束循环（对应 Go 侧
    ``if wake > now { break }``）。
    """

    def __init__(self, arrivals: list[float], count_at, until: float,
                 dirty: asyncio.Event) -> None:
        self.arrivals = sorted(arrivals)
        self.index = 0
        self.count_at = count_at
        self.until = until
        self.dirty = dirty
        self.now = 0.0
        self.broadcasts: list[float] = []
        self.builds = 0

    async def wait_for(self, awaitable, timeout):  # noqa: ANN001
        deadline = self.now + timeout
        nxt = self.arrivals[self.index] if self.index < len(self.arrivals) else None
        delivered = nxt is not None and nxt < deadline
        wake = max(self.now, nxt) if delivered else deadline
        if wake > self.until:
            awaitable.close()
            raise StopSimulation
        self.now = wake
        if not delivered:
            awaitable.close()
            raise asyncio.TimeoutError
        # clear() 的语义：Event 是**电平**不是队列，一次唤醒之后所有「已经发生过」的
        # 写入都被这次 clear 一并消费；只有 wake 之后才到达的写入能唤醒下一轮。
        while self.index < len(self.arrivals) and self.arrivals[self.index] <= wake:
            self.index += 1
        self.dirty.set()
        return await awaitable

    async def sleep(self, seconds: float) -> None:
        self.now += seconds

    def record(self) -> None:
        if self.count_at(self.now) > 0:
            self.broadcasts.append(round(self.now, 6))
            self.builds += 1


class StopSimulation(BaseException):
    """时间线走完，结束虚拟循环（不是 Exception，避免被参照实现的 except 吞掉）。"""


async def broadcast_metrics_loop(bus: EventBus, dirty: asyncio.Event, scheduler,
                                 broadcast_snapshot) -> None:  # noqa: ANN001
    """app.py:113-124 的**逐行**转写（只把 asyncio 的两处等待换成注入的调度器）。

    | 转写行                          | 参照实现         |
    | ------------------------------- | ---------------- |
    | wait_for(dirty.wait(), timeout) | app.py:116-119   |
    | dirty.clear()                   | app.py:119       |
    | client_count > 0 门禁           | app.py:122       |
    | broadcast_snapshot()            | app.py:123       |
    | sleep(1.0)                      | app.py:124       |
    """
    while True:
        try:
            await scheduler.wait_for(dirty.wait(), timeout=IDLE_BROADCAST_INTERVAL)
            dirty.clear()
        except asyncio.TimeoutError:
            pass  # 空闲心跳，刷新 RPM/TPM 衰减
        if bus.client_count > 0:
            await broadcast_snapshot()
        await scheduler.sleep(BROADCAST_MIN_INTERVAL)


async def run_throttle_case(case: dict[str, Any]) -> dict[str, Any]:
    steps = case["steps"]

    # count_at(t)：在 (上一步 now, 这一步 now] 区间内用这一步的订阅者数，与 Go 侧
    # Loop.Advance(now, dirtyAts, clientCount) 的「一次调用一个计数」一致。
    def count_at(moment: float) -> int:
        for step in steps:
            if moment <= step["now"]:
                return step["count"]
        return steps[-1]["count"]

    arrivals = [at for step in steps for at in step.get("dirty_at", [])]
    dirty = asyncio.Event()
    scheduler = VirtualScheduler(arrivals, count_at, case["until"], dirty)

    # 循环体里读的是 `app.state.event_bus.client_count`；这里换成「按虚拟时钟取值」的
    # 替身，语义相同（一次迭代一个计数），但可在语料里精确控制。
    class CountingBus:
        @property
        def client_count(self) -> int:
            return count_at(scheduler.now)

    async def broadcast_snapshot() -> None:
        scheduler.record()

    try:
        await broadcast_metrics_loop(CountingBus(), dirty, scheduler, broadcast_snapshot)
    except StopSimulation:
        pass

    return {
        "name": case["name"],
        "kind": "throttle",
        "note": case["note"],
        "idle_interval": IDLE_BROADCAST_INTERVAL,
        "min_interval": BROADCAST_MIN_INTERVAL,
        "until": case["until"],
        "steps": steps,
        "expect": compact({"fired": scheduler.broadcasts, "builds": scheduler.builds}),
    }


def steps(*entries: tuple[float, int, list[float]]) -> list[dict[str, Any]]:
    return [
        {"now": now, "count": count, "dirty_at": list(dirty_at)}
        for now, count, dirty_at in entries
    ]


THROTTLE_CASES: list[dict[str, Any]] = [
    {"name": "idle_no_clients", "until": 100.0, "note": "无订阅者：整条时间线零次构建",
     "steps": steps((100.0, 0, []))},
    {"name": "idle_one_client", "until": 100.0,
     "note": "空闲心跳：t=30 / 61 / 92（30 秒超时 + 末尾 1 秒 sleep）",
     "steps": steps((100.0, 1, []))},
    {"name": "steady_dirty_every_second", "until": 10.0,
     "note": "每秒都有写入：≤1 次/秒，t=1..10 各一次",
     "steps": steps(*[(float(second), 1, [float(second)]) for second in range(1, 11)])},
    {"name": "burst_within_one_second", "until": 3.5,
     "note": "同一秒内的多次写入只唤醒一次（Event 是电平语义，不是队列）：0.2 唤醒后"
             "进入 sleep 到 1.2，期间 0.5/1.0 的写入被同一次 clear 一并消费；"
             "1.5 的写入发生在 1.2 之后，于是下一次唤醒落在 2.2",
     "steps": steps((1.0, 1, [0.2, 0.5, 1.0]), (1.9, 1, [1.5, 1.9]),
                    (3.5, 1, []))},
    {"name": "dirty_before_first_timeout", "until": 40.0,
     "note": "t=5 的写入让第一轮提前唤醒（而不是等到 30 秒），随后 6 秒进入 sleep",
     "steps": steps((5.0, 1, [5.0]), (40.0, 1, []))},
    {"name": "client_appears_late", "until": 70.0,
     "note": "前 40 秒没人订阅：那两次唤醒不构建快照；40 秒后开始构建",
     "steps": steps((40.0, 0, []), (70.0, 1, []))},
    {"name": "client_leaves", "until": 70.0,
     "note": "30 秒时有订阅者，35 秒后归零：只有 t=30 那次构建",
     "steps": steps((35.0, 1, []), (70.0, 0, []))},
    {"name": "no_dirty_but_timeout_then_dirty", "until": 65.0,
     "note": "t=60 的写入落在第二次空闲唤醒（t=61）之前：t=61 那一轮立刻被唤醒，"
             "效果与超时相同但归因不同",
     "steps": steps((30.0, 1, []), (60.0, 1, [60.0]), (65.0, 1, []))},
    {"name": "dirty_inside_sleep_is_remembered", "until": 5.0,
     "note": "0.2 的写入广播后进入 sleep 到 1.2；0.5 的写入落在 sleep 期间（Event 已"
             "置位），因此下一轮在 1.2 **立刻**唤醒，而不是等到 31.2 的超时点",
     "steps": steps((0.2, 1, [0.2]), (1.2, 1, [0.5]), (5.0, 1, []))},
    {"name": "one_shot_dirty_then_idle", "until": 40.0,
     "note": "一次写入之后回到空闲节奏：t=2 广播，下一次是 30 秒超时点 32",
     "steps": steps((2.0, 1, [2.0]), (40.0, 1, []))},
]


def build_throttle_cases() -> list[dict[str, Any]]:
    return [asyncio.run(run_throttle_case(case)) for case in THROTTLE_CASES]


# --------------------------------------------------------------------------- #
# eventbus / e2e.jsonl —— 真服务器观测
# --------------------------------------------------------------------------- #


def minimal_config_data(directory: Path) -> dict[str, Any]:
    """与 tests/test_embedding.py:55 同形的最小配置（models 为空即可）。"""
    return {
        "host": "127.0.0.1",
        "port": 8000,
        "request_timeout": 10,
        "max_retries": 1,
        "key_failure_threshold": 1,
        "key_cooldown_seconds": 60,
        "endpoint_capabilities_path": str(directory / "endpoint-capabilities.json"),
        "upstream_health_check_interval": 0,
        "metrics_db_path": str(directory / "metrics.sqlite3"),
        "log_file_path": str(directory / "server.log"),
        "local_api_key": "local-key",
        "webui_enabled": False,
        "models": [],
    }


def build_event_bus_app(directory: Path):
    config_path = directory / "router-config.json"
    config_path.write_text(
        json.dumps(minimal_config_data(directory)), encoding="utf-8"
    )
    return create_app(RouterConfig.load(config_path), config_path)


E2E_CASES: list[dict[str, Any]] = [
    {"name": "e2e_valid_token", "send": {"kind": "text",
     "text": '{"type": "auth", "token": "local-key"}'},
     "note": "真服务器：client_count 先到（authenticate 里的回调），connected 最后到"},
    {"name": "e2e_wrong_token", "send": {"kind": "text",
     "text": '{"type": "auth", "token": "nope"}'},
     "note": "真服务器：4003"},
    {"name": "e2e_bad_json", "send": {"kind": "text", "text": "{oops"},
     "note": "真服务器：4001"},
    {"name": "e2e_binary_frame", "send": {"kind": "bytes", "b64": b64(b"\x00\x01")},
     "note": "真服务器：二进制首帧 -> 4001（receive_text 取不到 text）"},
    {"name": "e2e_json_null", "send": {"kind": "text", "text": "null"},
     "note": "真服务器：attribute error 冒泡，连接被服务器异常关闭（不是 4001/4003）"},
    {"name": "e2e_no_frame_timeout", "send": None, "wall_clock": True,
     "note": "真服务器：什么都不发，10 秒后 4001（本轮生成真的等满 10 秒）"},
]


def observe_event_bus_case(directory: Path, case: dict[str, Any]) -> dict[str, Any]:
    shutil.rmtree(directory, ignore_errors=True)
    directory.mkdir(parents=True, exist_ok=True)
    app = build_event_bus_app(directory)

    frames: list[str] = []
    close: dict[str, Any] | None = None
    raised: dict[str, Any] | None = None
    started = time.monotonic()
    # 整个块都要包：AttributeError 是在 ASGI 应用里抛的，TestClient 会把它从 portal
    # 线程重新抛到调用线程，落点可能在 receive()、也可能在任一 with 的 __exit__。
    try:
        with TestClient(app) as client:
            with client.websocket_connect("/ws/events") as websocket:
                if case["send"] is not None:
                    if case["send"]["kind"] == "text":
                        websocket.send_text(case["send"]["text"])
                    else:
                        websocket.send_bytes(
                            base64.b64decode(case["send"]["b64"])
                        )
                while True:
                    try:
                        message = websocket.receive()
                    except WebSocketDisconnect as error:
                        close = {"code": error.code, "reason": error.reason}
                        break
                    if message["type"] == "websocket.close":
                        close = {"code": message.get("code"),
                                 "reason": message.get("reason")}
                        break
                    frames.append(message.get("text") or "")
                    # 认证成功时服务器不会主动关闭（它会一直等下一帧），所以看到
                    # connected 就停；否则读下去会挂住。
                    if json.loads(frames[-1]).get("type") == "connected":
                        break
    except BaseException as error:  # noqa: BLE001
        raised = {"exception": type(error).__name__, "message": str(error)}
    elapsed = time.monotonic() - started

    # 帧的**事件类型**才是契约；metrics_snapshot 的载荷含 database_path/started_at
    # 一类的环境相关值，刻意不入语料（只记类型）。
    frame_types = []
    connected_frame = None
    for text in frames:
        try:
            parsed = json.loads(text)
        except json.JSONDecodeError:
            frame_types.append("<not-json>")
            continue
        event_type = parsed.get("type")
        frame_types.append(event_type)
        if event_type == "connected":
            connected_frame = text

    return {
        "name": case["name"],
        "kind": "e2e_auth",
        "note": case["note"],
        "send": case["send"],
        # 10.0 是 event_bus.py:37 的字面量；event_bus.py 没有把它抽成常量，所以这里
        # 只能记字面量（并在 auth.jsonl 里用替换 wait_for 的方式观测到它）。
        "auth_timeout": 10.0,
        "expect": compact({
            "frame_types": frame_types,
            "connected_frame": connected_frame,
            "close": close,
            "raised": raised,
            "waited_at_least_auth_timeout": elapsed >= 10.0
            if case.get("wall_clock") else None,
        }),
    }


def build_e2e_cases() -> list[dict[str, Any]]:
    with tempfile.TemporaryDirectory() as raw:
        directory = Path(raw) / "e2e"
        return [observe_event_bus_case(directory, case) for case in E2E_CASES]


# --------------------------------------------------------------------------- #
# wsproxy / proxy.jsonl
# --------------------------------------------------------------------------- #


def build_scope(*, scheme: str, path: str, query_string: bytes,
                raw_headers: list[tuple[bytes, bytes]]) -> dict[str, Any]:
    """把 ASGI scope 里与折算相关的部分写成 JSON 可存的形状。

    头的值与原始查询串都用 base64：它们要能被 Go 侧**逐字节**重建，而 Python 的
    ``str`` 装不下任意字节序列。
    """
    return {
        "scheme": scheme,
        "path": path,
        "query_b64": b64(query_string),
        "headers": [[b64(name), b64(value)] for name, value in raw_headers],
    }


def scope_to_asgi(scope: dict[str, Any]) -> dict[str, Any]:
    """把语料里的 scope 还原成真实的 ASGI scope。"""
    return {
        "type": "websocket",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "scheme": scope["scheme"],
        "path": scope["path"],
        "raw_path": scope["path"].encode("utf-8"),
        "query_string": base64.b64decode(scope["query_b64"]),
        "headers": [(base64.b64decode(name), base64.b64decode(value))
                    for name, value in scope["headers"]],
        "server": ["example.test", 443 if scope["scheme"] == "wss" else 80],
        "client": ["127.0.0.1", 54321],
    }


# 真实客户端会在握手里带上这些头；注意 Authorization / x-api-key **不在**剔除名单里。
HANDSHAKE_HEADERS: list[tuple[bytes, bytes]] = [
    (b"Host", b"example.test"),
    (b"Connection", b"Upgrade"),
    (b"Sec-WebSocket-Key", b"dGhlIHNhbXBsZSBub25jZQ=="),
    (b"UPGRADE", b"websocket"),
    (b"Sec-WebSocket-Version", b"13"),
    (b"Sec-WebSocket-Protocol", b"chat"),
    (b"Sec-WebSocket-Extensions", b"permessage-deflate"),
    (b"Authorization", b"Bearer local-key"),
    (b"x-api-key", b"visitor"),
    (b"X-Multi", b"first"),
    (b"x-multi", b"second"),
    (b"content-length", b"2"),
    (b"destination-addr", b"1.2.3.4"),
    (b"accept-encoding", b"gzip, deflate"),
    (b"anthropic-version", b"2023-06-01"),
    (b"anthropic-beta", b"prompt-caching-2024-07-31"),
    (b"Content-Type", b"application/json"),
    (b"User-Agent", b"amkr-test/1.0"),
]

SYNTHESIZE_CASES: list[dict[str, Any]] = [
    {"name": "full_handshake", "scheme": "ws", "path": "/v1/chat/completions",
     "query_string": b"trace=1&x=%E4%B8%AD", "headers": HANDSHAKE_HEADERS,
     "body": b'{"model": "m"}', "api_key": "sk-up",
     "note": "全部握手头 + 编码过的查询串：剔除 6 个握手头，其余原样（大小写保留）"},
    {"name": "secure_wss", "scheme": "wss", "path": "/v1/messages",
     "query_string": b"", "headers": [(b"authorization", b"Bearer local-key")],
     "body": b"{}", "api_key": "sk-up",
     "note": "wss -> scope scheme=https（实测可达）"},
    {"name": "no_headers", "scheme": "ws", "path": "/v1/chat/completions",
     "query_string": b"", "headers": [], "body": b"{}", "api_key": "sk-up",
     "note": "没有头部：上游只剩 Authorization 与 Accept-Encoding"},
    {"name": "only_handshake_headers", "scheme": "ws", "path": "/v1/chat/completions",
     "query_string": b"", "body": b"{}", "api_key": "sk-up",
     "headers": [(b"connection", b"Upgrade"), (b"upgrade", b"websocket"),
                 (b"sec-websocket-key", b"k"), (b"sec-websocket-version", b"13"),
                 (b"sec-websocket-protocol", b"p"),
                 (b"sec-websocket-extensions", b"e")],
     "note": "只有握手头：全部剔除，头部为空"},
    {"name": "empty_path", "scheme": "ws", "path": "/v1/", "query_string": b"",
     "headers": [], "body": b"", "api_key": "sk-up",
     "note": "path 路由参数为空串"},
    {"name": "binary_body", "scheme": "ws", "path": "/v1/images/generations",
     "query_string": b"", "body": b"\xff\xfe\x00\x01", "api_key": "sk-up",
     "headers": [(b"content-type", b"multipart/form-data; boundary=x")],
     "note": "二进制请求体：原样进入折算请求的 body"},
    {"name": "cookie_auth", "scheme": "ws", "path": "/v1/chat/completions",
     "query_string": b"", "body": b"{}", "api_key": "sk-up",
     "headers": [(b"cookie", b"amkr_session=abc"), (b"x-api-key", b"visitor-key")],
     "note": "cookie 与 x-api-key 都保留在折算请求里（由代理层决定用途）"},
]


def run_synthesize_case(case: dict[str, Any]) -> dict[str, Any]:
    recorded_scope = build_scope(scheme=case["scheme"], path=case["path"],
                                query_string=case["query_string"],
                                raw_headers=case["headers"])
    scope = scope_to_asgi(recorded_scope)

    async def receive() -> dict[str, Any]:
        return {"type": "websocket.receive", "bytes": case["body"]}

    async def send(_message: dict[str, Any]) -> None:
        return None

    websocket = WebSocket(scope, receive=receive, send=send)
    request = _websocket_http_request(websocket, case["body"])
    return {
        "name": case["name"],
        "kind": "synthesize",
        "note": case["note"],
        "scope": recorded_scope,
        "body": b64(case["body"]),
        "api_key": case["api_key"],
        "expect": compact({
            "method": request.method,
            "scope_scheme": request.scope["scheme"],
            "http_version": request.scope["http_version"],
            "url_path": request.url.path,
            "query": request.url.query,
            # 原始字节对：**大小写与重复都保留**（Python 在字节层比较）。
            "headers": [[b64(name), b64(value)]
                        for name, value in request.scope["headers"]],
            "upstream_headers": _upstream_headers(request, case["api_key"]),
        }),
    }


# frames 用例：真实最小 FastAPI 应用 + register_websocket_proxy。
E2E_FRAME_CASES: list[dict[str, Any]] = [
    {"name": "json_ok", "url": "/v1/chat/completions",
     "headers": {"Authorization": "Bearer local-key"},
     "send": {"kind": "text", "text": '{"model": "m"}'},
     "response": {"status": 200, "content_type": "application/json",
                  "chunks": [{"text": '{"id": "ok"}'}]},
     "note": "单个非流式响应 -> 一帧文本 + 1000"},
    {"name": "json_with_charset", "url": "/v1/messages?trace=1",
     "headers": {"Authorization": "Bearer local-key"},
     "send": {"kind": "text", "text": '{"model": "m", "stream": true}'},
     "response": {"status": 200, "content_type": "application/json; charset=utf-8",
                  "chunks": [{"text": '{"a": 1}'}]},
     "note": "content-type 含 json -> 文本帧；查询串保留"},
    {"name": "sse_stream", "url": "/v1/chat/completions",
     "headers": {"x-api-key": "local-key"},
     "send": {"kind": "text", "text": '{"model": "m", "stream": true}'},
     "response": {"status": 200, "content_type": "text/event-stream", "streaming": True,
                  "chunks": [{"text": "data: one\n\n"}, {"text": "data: two\n\n"},
                             {"text": "data: [DONE]\n\n"}]},
     "note": "流式：一个 chunk 一帧，顺序不变"},
    {"name": "sse_empty_chunk", "url": "/v1/chat/completions",
     "headers": {"x-api-key": "local-key"},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "text/event-stream", "streaming": True,
                  "chunks": [{"text": ""}, {"text": "x"}]},
     "note": "流式里的空 chunk **会**发出一个空帧（非流式空 body 则一帧都不发）"},
    {"name": "empty_body", "url": "/v1/chat/completions",
     "headers": {"authorization": "Bearer local-key"},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "application/json",
                  "chunks": [{"text": ""}]},
     "note": "非流式空 body -> 零帧，直接关闭 1000"},
    {"name": "binary_body", "url": "/v1/images/generations",
     "headers": {"authorization": "Bearer local-key"},
     "send": {"kind": "bytes", "b64": b64(b"\xff\xfe\x00binary")},
     "response": {"status": 200, "content_type": "application/octet-stream",
                  "chunks": [{"b64": b64(b"\x00\x01\xff")}]},
     "note": "非文本 content-type -> 二进制帧；二进制请求体原样转发"},
    {"name": "no_content_type", "url": "/v1/chat/completions",
     "headers": {"authorization": "Bearer local-key"},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": None,
                  "chunks": [{"text": '{"id": 1}'}]},
     "note": "没有 content-type -> 二进制帧（既不以 text/ 开头，也不含 json）"},
    {"name": "uppercase_json_content_type", "url": "/v1/chat/completions",
     "headers": {"authorization": "Bearer local-key"},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "Application/JSON",
                  "chunks": [{"text": '{"id": 1}'}]},
     "note": "判定大小写敏感：Application/JSON 走**二进制**帧"},
    {"name": "text_plain", "url": "/v1/chat/completions", "headers": {},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "text/plain; charset=utf-8",
                  "chunks": [{"text": "hello"}]},
     "note": "text/ 前缀 -> 文本帧"},
    {"name": "invalid_utf8_text", "url": "/v1/chat/completions", "headers": {},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "text/plain",
                  "chunks": [{"b64": b64(b"a\xe4\xb8Xc")}]},
     "note": "文本帧前做 decode(errors=replace)：截断的中 -> U+FFFD（最大子部分）"},
    {"name": "invalid_utf8_binary", "url": "/v1/chat/completions", "headers": {},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "application/octet-stream",
                  "chunks": [{"b64": b64(b"a\xe4\xb8Xc")}]},
     "note": "二进制帧不做解码：非法字节原样发出"},
    {"name": "error_401", "url": "/v1/chat/completions", "headers": {},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 401, "content_type": "application/json",
                  "chunks": [{"text": '{"error": {"message": "x"}}'}]},
     "note": "4xx -> 1008"},
    {"name": "error_500_streaming", "url": "/v1/chat/completions", "headers": {},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 500, "content_type": "text/plain", "streaming": True,
                  "chunks": [{"text": "boom"}]},
     "note": "5xx -> 1011，且帧照发"},
    {"name": "empty_path", "url": "/v1/", "headers": {}, "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "application/json",
                  "chunks": [{"text": "{}"}]},
     "note": "路由参数为空串"},
    {"name": "nested_path", "url": "/v1/a/b/c", "headers": {},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "application/json",
                  "chunks": [{"text": "{}"}]},
     "note": "多段路径"},
    {"name": "unicode_json", "url": "/v1/chat/completions",
     "headers": {"authorization": "Bearer local-key"},
     "send": {"kind": "text", "text": "{}"},
     "response": {"status": 200, "content_type": "application/json",
                  "chunks": [{"text": '{"n": "中文"}'}]},
     "note": "非 ASCII 原样发出（不转义）"},
    {"name": "handler_raises", "url": "/v1/chat/completions", "headers": {},
     "send": {"kind": "text", "text": "{}"}, "fail": "raise",
     "response": None, "note": "ProxyHandler 抛异常 -> 1011"},
    {"name": "client_disconnect", "url": "/v1/chat/completions", "headers": {},
     "send": None, "response": None,
     "note": "客户端不发消息直接断开 -> 服务器直接 return，不关连接"},
]


def build_python_response(spec: dict[str, Any]) -> Response:
    chunks = [
        base64.b64decode(chunk["b64"]) if "b64" in chunk else chunk["text"].encode("utf-8")
        for chunk in spec["chunks"]
    ]
    if spec.get("streaming"):
        async def iterator():
            for item in chunks:
                yield item

        response: Response = StreamingResponse(iterator(), status_code=spec["status"])
    else:
        response = Response(b"".join(chunks), status_code=spec["status"])
    content_type = spec.get("content_type")
    if content_type is not None:
        # Response.headers 是可变映射；不设 media_type 时 Starlette 根本不会加
        # content-type，所以这里只在下发时才写入。
        response.headers["content-type"] = content_type
    return response


def read_ws_client(websocket) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:  # noqa: ANN001
    frames: list[dict[str, Any]] = []
    close: dict[str, Any] | None = None
    while True:
        try:
            message = websocket.receive()
        except WebSocketDisconnect as error:
            close = {"code": error.code, "reason": error.reason}
            break
        except BaseException as error:  # noqa: BLE001
            close = {"raised": type(error).__name__, "message": str(error)}
            break
        if message["type"] == "websocket.close":
            close = {"code": message.get("code"), "reason": message.get("reason")}
            break
        if message.get("bytes") is not None:
            frames.append({"kind": "bytes", "b64": b64(message["bytes"])})
        else:
            frames.append({"kind": "text", "text": message.get("text") or ""})
    return frames, close


def run_frame_case(case: dict[str, Any]) -> dict[str, Any]:
    app = FastAPI()
    recorded: list[dict[str, Any]] = []

    async def handler(path: str, request: Any) -> Response:
        body = await request.body()
        recorded.append({
            "path": path,
            "method": request.method,
            "scope_scheme": request.scope["scheme"],
            "url_path": request.url.path,
            "query": request.url.query,
            "body": b64(body),
        })
        if case.get("fail") == "raise":
            raise RuntimeError("boom")
        return build_python_response(case["response"])

    register_websocket_proxy(app, handler)

    frames: list[dict[str, Any]] = []
    close: dict[str, Any] | None = None
    with TestClient(app) as client:
        with client.websocket_connect(case["url"], headers=case["headers"]) as websocket:
            if case["send"] is None:
                # 不发消息直接断开：参照实现走 receive() 的 disconnect 分支直接 return
                # （websocket_proxy.py:31-32）。客户端这一侧能观测到的只有「自己关了」。
                websocket.close()
                close = {"code": 1000, "reason": "", "client_initiated": True}
                recorded.append({"disconnected_before_frame": True})
            else:
                if case["send"]["kind"] == "text":
                    websocket.send_text(case["send"]["text"])
                else:
                    websocket.send_bytes(base64.b64decode(case["send"]["b64"]))
                frames, close = read_ws_client(websocket)

    return {
        "name": case["name"],
        "kind": "frames",
        "note": case["note"],
        "url": case["url"],
        "headers": case["headers"],
        "send": case["send"],
        "response": case["response"],
        "fail": case.get("fail"),
        "expect": compact({"recorded": recorded, "frames": frames, "close": close}),
    }


CLOSE_CODE_CASES: list[dict[str, Any]] = [
    {"name": f"close_code_{status}", "kind": "close_code", "note": note,
     "status": status, "expect": compact(_websocket_close_code(status))}
    for status, note in [
        (200, "成功 -> 1000"), (204, "无内容 -> 1000"), (302, "重定向 -> 1000"),
        (399, "3xx 边界 -> 1000"), (400, "4xx 下边界 -> 1008"),
        (401, "鉴权失败 -> 1008"), (404, "不存在 -> 1008"), (429, "限流 -> 1008"),
        (499, "4xx 上边界 -> 1008"), (500, "服务端错误 -> 1011"),
        (503, "不可用 -> 1011"), (521, "Cloudflare -> 1011"),
    ]
]


def build_wsproxy_cases() -> list[dict[str, Any]]:
    cases = [run_synthesize_case(case) for case in SYNTHESIZE_CASES]
    cases.extend(run_frame_case(case) for case in E2E_FRAME_CASES)
    cases.extend(CLOSE_CODE_CASES)
    return cases


# --------------------------------------------------------------------------- #
# 转写的防漂移断言
# --------------------------------------------------------------------------- #


def check_app_source() -> None:
    """断言被转写的 app.py 片段仍然存在。

    app.py:113-124 的循环是 create_app() 里的闭包，外部拿不到（它只作为 asyncio
    任务挂在 lifespan 内的 state 上），所以节流语料用的是逐行转写。为免转写与参照
    实现悄悄漂移，这里在生成/校验时直接检查源码文本。
    """
    source = (REPO_ROOT / "auto_model_key_router" / "app.py").read_text(encoding="utf-8")
    required = [
        "_IDLE_BROADCAST_INTERVAL = 30.0",
        "await asyncio.wait_for(",
        "_metrics_dirty.wait(), timeout=_IDLE_BROADCAST_INTERVAL",
        "_metrics_dirty.clear()",
        "if app.state.event_bus.client_count > 0:",
        "await _broadcast_metrics_snapshot()",
        "await asyncio.sleep(1.0)",
        'await app.state.event_bus.broadcast("metrics_snapshot", snapshot)',
        'await app.state.event_bus.broadcast("client_count", {"count": count})',
        'await event_bus.broadcast("config_change", {"reloaded": True})',
        'await websocket.send_json({"type": "connected", "data": {}})',
    ]
    missing = [line for line in required if line not in source]
    if missing:
        raise SystemExit(f"app.py 的转写依据已变化，请同步语料生成器: {missing}")

    event_bus_source = (
        REPO_ROOT / "auto_model_key_router" / "event_bus.py"
    ).read_text(encoding="utf-8")
    for line in ["timeout=10.0", "code=4001", "code=4003", '"auth failed"',
                 '"auth timeout or invalid message"']:
        if line not in event_bus_source:
            raise SystemExit(f"event_bus.py 的契约片段已变化: {line}")


# --------------------------------------------------------------------------- #
# 主流程
# --------------------------------------------------------------------------- #


def build_corpus_files() -> dict[Path, str]:
    check_app_source()
    return {
        EVENTBUS_DIR / "auth.jsonl": render(build_auth_cases()),
        EVENTBUS_DIR / "broadcast.jsonl": render(build_broadcast_cases()),
        EVENTBUS_DIR / "throttle.jsonl": render(build_throttle_cases()),
        EVENTBUS_DIR / "e2e.jsonl": render(build_e2e_cases()),
        WSPROXY_DIR / "proxy.jsonl": render(build_wsproxy_cases()),
    }


def check() -> int:
    status = 0
    for path, content in build_corpus_files().items():
        if not path.exists():
            print(f"语料缺失: {path}", file=sys.stderr)
            status = 1
            continue
        if path.read_bytes().decode("utf-8") != content:
            print(f"语料已过期: {path}", file=sys.stderr)
            status = 1
    if status == 0:
        print("语料最新")
    return status


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    if args.check:
        return check()

    files = build_corpus_files()
    total = 0
    for path, content in files.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8", newline="\n")
        lines = content.count("\n")
        total += lines
        print(f"已写入 {lines} 条语料: {path}")
    print(f"共 {total} 条语料")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
