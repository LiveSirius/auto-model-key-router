"""嵌入场景回归：把 AMKR 挂到宿主应用下必须真的能用。

锁住的行为（都曾在实测中坏掉）：
- Starlette 不会为 Mount 的子应用运行 lifespan，直接 parent.mount() 会让指标广播
  任务永不启动、httpx client 与 sqlite 连接永不关闭；mount_app 必须把子应用
  lifespan 链进父应用。
- 挂载后所有接口位于前缀之下，且宿主 openapi 不应混入子应用路由。
- 运维接口默认关闭：嵌入到别人的进程里，启停本机服务/改写 Agent 配置语义不成立。
"""

from __future__ import annotations

import asyncio
import json
import threading
from collections.abc import Awaitable, Callable
from contextlib import asynccontextmanager
from pathlib import Path
from typing import TypeVar

import anyio
import httpx
import pytest
from fastapi import FastAPI

from auto_model_key_router import (
    AuthContext,
    KeyPool,
    RouterConfig,
    create_app,
    mount_app,
)
from auto_model_key_router.webui import webui_status


T = TypeVar("T")
AUTH_HEADERS = {"Authorization": "Bearer local-key"}
MOUNT = "/amkr"


def run_client(app: FastAPI, action: Callable[[httpx.AsyncClient], Awaitable[T]]) -> T:
    return anyio.run(_run_client, app, action)


async def _run_client(
    app: FastAPI, action: Callable[[httpx.AsyncClient], Awaitable[T]]
) -> T:
    async with app.router.lifespan_context(app):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://testserver"
        ) as client:
            return await action(client)


def config_data(tmp_path: Path, *, webui: bool = False) -> dict[str, object]:
    return {
        "host": "127.0.0.1",
        "port": 8000,
        "request_timeout": 10,
        "max_retries": 1,
        "key_failure_threshold": 1,
        "key_cooldown_seconds": 60,
        "endpoint_capabilities_path": str(tmp_path / "endpoint-capabilities.json"),
        "upstream_health_check_interval": 0,
        "metrics_db_path": str(tmp_path / "metrics.sqlite3"),
        "log_file_path": str(tmp_path / "server.log"),
        "local_api_key": "local-key",
        "webui_enabled": webui,
        "models": [],
    }


def write_config(tmp_path: Path, *, webui: bool = False) -> Path:
    path = tmp_path / "router-config.json"
    path.write_text(json.dumps(config_data(tmp_path, webui=webui)), encoding="utf-8")
    return path


def build(tmp_path: Path, parent: FastAPI, *, webui: bool = False, **kwargs):
    """把 AMKR 挂到 parent 上（带配置文件路径，管理接口才可持久化）。"""
    path = write_config(tmp_path, webui=webui)
    return mount_app(parent, MOUNT, RouterConfig.load(path), path, **kwargs)


def test_public_api_is_importable() -> None:
    assert callable(create_app)
    assert callable(mount_app)
    assert KeyPool is not None
    assert RouterConfig is not None


def test_mount_app_requires_config_or_app() -> None:
    with pytest.raises(ValueError, match="需要 config 或现成的 app"):
        mount_app(FastAPI(), MOUNT)


def test_mounted_endpoints_live_under_prefix(tmp_path: Path) -> None:
    parent = FastAPI()
    build(tmp_path, parent)

    async def requests(client: httpx.AsyncClient) -> dict[str, int]:
        return {
            "health": (await client.get(f"{MOUNT}/health")).status_code,
            "models": (
                await client.get(f"{MOUNT}/v1/models", headers=AUTH_HEADERS)
            ).status_code,
            "metrics": (
                await client.get(f"{MOUNT}/metrics", headers=AUTH_HEADERS)
            ).status_code,
            # 没有前缀的路径属于宿主，必须 404 而不是被 AMKR 接管。
            "bare_health": (await client.get("/health")).status_code,
            "bare_models": (await client.get("/v1/models")).status_code,
        }

    results = run_client(parent, requests)

    assert results["health"] == 200
    assert results["models"] == 200
    assert results["metrics"] == 200
    assert results["bare_health"] == 404
    assert results["bare_models"] == 404


def test_mount_app_runs_child_lifespan(tmp_path: Path) -> None:
    # 这是最关键的一条：不加 lifespan 桥接时 broadcast task 为 None 且
    # http_client.is_closed 为 False（资源泄漏）。
    parent = FastAPI()
    amkr = build(tmp_path, parent)
    observed: dict[str, bool] = {}

    async def action(client: httpx.AsyncClient) -> None:
        observed["task_started"] = amkr.state._metrics_broadcast_task is not None
        response = await client.get(f"{MOUNT}/health")
        assert response.status_code == 200

    run_client(parent, action)

    assert observed["task_started"] is True
    # 退出 lifespan 后 runtime 必须已关闭：httpx client 关闭即代表 shutdown 跑过。
    assert amkr.state.runtime_manager.current.http_client.is_closed is True


def test_mount_app_runs_parent_lifespan_too(tmp_path: Path) -> None:
    events: list[str] = []

    @asynccontextmanager
    async def parent_lifespan(_app: FastAPI):
        events.append("parent-startup")
        try:
            yield
        finally:
            events.append("parent-shutdown")

    parent = FastAPI(lifespan=parent_lifespan)
    build(tmp_path, parent)

    async def action(client: httpx.AsyncClient) -> None:
        assert (await client.get(f"{MOUNT}/health")).status_code == 200

    run_client(parent, action)

    assert events == ["parent-startup", "parent-shutdown"]


def test_mount_app_ops_disabled_by_default(tmp_path: Path) -> None:
    parent = FastAPI()
    build(tmp_path, parent)

    async def requests(client: httpx.AsyncClient) -> dict[str, int]:
        return {
            # 运维接口作用于宿主机，嵌入时默认不注册。
            "logs": (
                await client.get(f"{MOUNT}/api/logs", headers=AUTH_HEADERS)
            ).status_code,
            "service": (
                await client.post(f"{MOUNT}/api/service/start_amkr", headers=AUTH_HEADERS)
            ).status_code,
            # 配置类管理接口照常可用。
            "settings": (
                await client.get(f"{MOUNT}/api/settings", headers=AUTH_HEADERS)
            ).status_code,
            "providers": (
                await client.get(f"{MOUNT}/api/providers", headers=AUTH_HEADERS)
            ).status_code,
        }

    results = run_client(parent, requests)

    assert results["logs"] == 404
    assert results["service"] == 404
    assert results["settings"] == 200
    assert results["providers"] == 200


def test_mount_app_can_opt_into_ops(tmp_path: Path) -> None:
    parent = FastAPI()
    build(tmp_path, parent, enable_ops=True)

    async def requests(client: httpx.AsyncClient) -> httpx.Response:
        return await client.get(f"{MOUNT}/api/tool", headers=AUTH_HEADERS)

    response = run_client(parent, requests)

    assert response.status_code == 200
    assert response.json()["version"]


def test_mount_app_accepts_prebuilt_app(tmp_path: Path) -> None:
    parent = FastAPI()
    path = write_config(tmp_path)
    amkr = create_app(RouterConfig.load(path), path, enable_ops=False)
    returned = mount_app(parent, MOUNT, app=amkr)

    assert returned is amkr
    assert amkr.state.mount_path == MOUNT

    async def requests(client: httpx.AsyncClient) -> httpx.Response:
        return await client.get(f"{MOUNT}/health")

    assert run_client(parent, requests).status_code == 200


def test_mount_app_reports_prefixed_webui_path(tmp_path: Path) -> None:
    # 资产缺失（未随包安装）时 register_webui 会返回 False，此时只校验路径推导。
    parent = FastAPI()
    amkr = build(tmp_path, parent, webui=True)

    status = webui_status(amkr)
    if amkr.state.webui_mounted:
        assert status["webui_path"] == f"{MOUNT}/ui"

        async def requests(client: httpx.AsyncClient) -> dict[str, int]:
            return {
                "ui": (await client.get(f"{MOUNT}/ui/")).status_code,
                "health": (await client.get(f"{MOUNT}/health")).status_code,
            }

        results = run_client(parent, requests)
        assert results["ui"] == 200
        assert results["health"] == 200
    else:
        assert status["webui_path"] is None


def test_host_openapi_does_not_absorb_child_routes(tmp_path: Path) -> None:
    parent = FastAPI()
    amkr = build(tmp_path, parent)

    # 宿主的 schema 应保持干净；AMKR 的 schema 在自己身上仍完整可用。
    assert parent.openapi()["paths"] == {}
    assert "/health" in amkr.openapi()["paths"]


def test_mounted_websocket_events_endpoint_is_reachable(tmp_path: Path) -> None:
    from fastapi.testclient import TestClient

    parent = FastAPI()
    build(tmp_path, parent)

    with TestClient(parent) as client:
        with client.websocket_connect(f"{MOUNT}/ws/events") as ws:
            # authenticate 要求客户端先发认证消息（query 参数不参与校验）。
            ws.send_text(json.dumps({"type": "auth", "token": "local-key"}))
            # 认证成功即触发 client_count 广播；这条事件此前因为漏 await 从未发出过。
            first = ws.receive_json()

    assert first["type"] == "client_count"
    assert first["data"]["count"] == 1


def test_events_websocket_broadcasts_client_count(tmp_path: Path) -> None:
    """独立运行时也要真的发出 client_count（此前漏 await，事件从未送达）。"""
    import warnings

    from fastapi.testclient import TestClient

    app = create_app(RouterConfig.load(write_config(tmp_path)))

    with warnings.catch_warnings():
        warnings.simplefilter("error", RuntimeWarning)
        with TestClient(app) as client:
            with client.websocket_connect("/ws/events") as ws:
                ws.send_text(json.dumps({"type": "auth", "token": "local-key"}))
                message = ws.receive_json()

    assert message["type"] == "client_count"
    assert message["data"]["count"] == 1


def test_metrics_store_close_waits_for_inflight_query(tmp_path: Path) -> None:
    """关连接必须等查询线程收尾，否则会踩到已关闭的 sqlite 句柄（进程级崩溃）。

    这里模拟「任务被取消」：查询进行中取消等待，随后立刻 close()。若 close 不等
    在途线程，sqlite 会在原生层报 access violation 而不是抛 Python 异常。
    """
    import time

    from auto_model_key_router.metrics import MetricsStore

    store = MetricsStore(tmp_path / "metrics.sqlite3")
    started = threading.Event()
    original = store._snapshot_sync

    def slow_snapshot(*args):
        started.set()
        time.sleep(0.3)
        return original(*args)

    store._snapshot_sync = slow_snapshot

    async def scenario() -> None:
        task = asyncio.ensure_future(store.snapshot())
        await asyncio.to_thread(started.wait, 5.0)
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass
        # 取消后立即关闭：必须安全串行，不能崩。
        await store.close()

    anyio.run(scenario)
    assert store._closed is True


# --- 可插拔鉴权钩子 ---------------------------------------------------------


def host_auth(header_value: str):
    """宿主式鉴权：认 ``X-Host-User`` 头，而不是 AMKR 自己的 key。"""

    async def authenticator(request, config):
        if request.headers.get(header_value, "") == "admin":
            return AuthContext("full")
        return None

    return authenticator


def test_custom_authenticator_replaces_local_api_key(tmp_path: Path) -> None:
    """宿主已有身份体系时，不该再要求调用方持有一套 AMKR 的 key。"""
    parent = FastAPI()
    build(tmp_path, parent, authenticator=host_auth("x-host-user"))

    async def requests(client: httpx.AsyncClient) -> dict[str, int]:
        host = await client.get(
            f"{MOUNT}/metrics", headers={"X-Host-User": "admin"}
        )
        local_key = await client.get(f"{MOUNT}/metrics", headers=AUTH_HEADERS)
        # 未通过钩子 -> 401，而不是回退到默认的本地 key 比较。
        anonymous = await client.get(f"{MOUNT}/metrics")
        # 无鉴权的接口不受影响。
        health = await client.get(f"{MOUNT}/health")
        return {
            "host": host.status_code,
            "local_key": local_key.status_code,
            "anonymous": anonymous.status_code,
            "health": health.status_code,
        }

    results = run_client(parent, requests)

    assert results["host"] == 200
    assert results["local_key"] == 401
    assert results["anonymous"] == 401
    assert results["health"] == 200


def test_custom_authenticator_visitor_mode_is_model_level(tmp_path: Path) -> None:
    """/v1/models 必须保留 visitor 的模型级语义，不能退化成布尔权限。"""

    async def authenticator(request, config):
        if request.headers.get("x-role") == "visitor":
            return AuthContext("visitor")
        if request.headers.get("x-role") == "admin":
            return AuthContext("full")
        return None

    app = create_app(
        RouterConfig.load(write_config(tmp_path)), authenticator=authenticator
    )

    async def requests(client: httpx.AsyncClient) -> dict[str, int]:
        visitor = await client.get("/v1/models", headers={"X-Role": "visitor"})
        admin = await client.get("/v1/models", headers={"X-Role": "admin"})
        # visitor 拿不到指标接口 —— 这正是「不是布尔权限」的证据。
        visitor_metrics = await client.get("/metrics", headers={"X-Role": "visitor"})
        return {
            "visitor": visitor.status_code,
            "admin": admin.status_code,
            "visitor_metrics": visitor_metrics.status_code,
        }

    results = run_client(app, requests)

    assert results["visitor"] == 200
    assert results["admin"] == 200
    assert results["visitor_metrics"] == 401


def test_custom_authenticator_covers_websocket(tmp_path: Path) -> None:
    """鉴权钩子必须同时覆盖 WebSocket —— 它此前是一条独立的校验路径。

    钩子只认 ``Authorization`` 头，而 WebSocket 握手带不了这个头，所以这里真正
    验证的是首帧 token 被折算成了请求凭据。
    """
    from fastapi.testclient import TestClient

    seen: list[str] = []

    async def authenticator(request, config):
        token = request.headers.get("authorization", "").removeprefix("Bearer ").strip()
        seen.append(token)
        return AuthContext("full") if token == "host-token" else None

    parent = FastAPI()
    build(tmp_path, parent, authenticator=authenticator)

    with TestClient(parent) as client:
        # 首帧 token 就是候选凭据，交给同一个钩子判定。
        with client.websocket_connect(f"{MOUNT}/ws/events") as ws:
            ws.send_text(json.dumps({"type": "auth", "token": "host-token"}))
            first = ws.receive_json()

        # AMKR 的本地 key 不再是有效凭据：钩子拒绝后收不到任何事件。
        with pytest.raises(Exception):
            with client.websocket_connect(f"{MOUNT}/ws/events") as rejected:
                rejected.send_text(json.dumps({"type": "auth", "token": "local-key"}))
                rejected.receive_json()

    assert first["type"] == "client_count"
    assert first["data"]["count"] == 1
    assert seen == ["host-token", "local-key"]


def test_non_ascii_credential_is_rejected_not_crashed(tmp_path: Path) -> None:
    """非 ASCII 凭据必须 401，不能 500。

    HTTP 头是字节、由 Starlette 按 latin-1 解码，因此能把非 ASCII 字符串送到
    hmac.compare_digest —— 它对此抛 TypeError。修复前这会变成 500。
    """
    app = create_app(RouterConfig.load(write_config(tmp_path)))
    scope = {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": "GET",
        "scheme": "http",
        "path": "/v1/models",
        "raw_path": b"/v1/models",
        "query_string": b"",
        "root_path": "",
        # "密钥" 的 UTF-8 字节，latin-1 解码后是非 ASCII 字符串。
        "headers": [(b"authorization", b"Bearer " + bytes([0xE5, 0xAF, 0x86]))],
        "client": ("127.0.0.1", 1234),
        "server": ("127.0.0.1", 8000),
    }

    async def scenario() -> int:
        status = {"code": None}

        async def receive():
            return {"type": "http.request", "body": b"", "more_body": False}

        async def send(message):
            if message["type"] == "http.response.start":
                status["code"] = message["status"]

        async with app.router.lifespan_context(app):
            await app(scope, receive, send)
        return status["code"]

    assert anyio.run(scenario) == 401
