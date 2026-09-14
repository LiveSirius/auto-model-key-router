from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import TypeVar

import anyio
import httpx
from fastapi import FastAPI

from auto_model_key_router.app import create_app
from auto_model_key_router.config import RouterConfig
from auto_model_key_router.webui import register_webui, webui_status


T = TypeVar("T")


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


def create_file_backed_app(
    tmp_path: Path, *, webui: bool = False
) -> tuple[FastAPI, Path]:
    path = tmp_path / "router-config.json"
    path.write_text(json.dumps(config_data(tmp_path, webui=webui)), encoding="utf-8")
    return create_app(RouterConfig.load(path), path), path


def test_webui_enabled_defaults_to_false_and_round_trips(tmp_path: Path) -> None:
    path = tmp_path / "router-config.json"
    data = config_data(tmp_path)
    data.pop("webui_enabled")
    path.write_text(json.dumps(data), encoding="utf-8")

    # 旧配置没有这个字段时必须安全降级为关闭。
    assert RouterConfig.load(path).webui_enabled is False

    data["webui_enabled"] = True
    path.write_text(json.dumps(data), encoding="utf-8")
    assert RouterConfig.load(path).webui_enabled is True


def test_create_app_reports_webui_disabled_by_default(tmp_path: Path) -> None:
    app, _ = create_file_backed_app(tmp_path)

    async def requests(client: httpx.AsyncClient) -> dict:
        return (await client.get("/health")).json()

    health = run_client(app, requests)

    assert health["webui_enabled"] is False
    assert health["webui_mounted"] is False
    assert health["webui_path"] is None


def test_register_webui_returns_false_when_disabled(tmp_path: Path) -> None:
    assert register_webui(FastAPI(), enabled=False) is False


def test_webui_status_reflects_mount_state() -> None:
    app = FastAPI()

    before = webui_status(app)
    assert before["webui_enabled"] is False
    assert before["webui_mounted"] is False
    assert before["webui_path"] is None

    app.state.webui_mounted = True
    after = webui_status(app)
    assert after["webui_mounted"] is True
    # 挂载后必须把访问路径告诉前端。
    assert after["webui_path"] == "/ui"


def test_webui_status_separates_intent_from_mount_state(tmp_path: Path) -> None:
    # 改配置只改"意图"，不会重新挂载。两个字段必须分别如实汇报，否则前端无法
    # 提示"需要重启服务"；这里直接驱动挂载状态，避免依赖资产是否随包安装。
    app, _ = create_file_backed_app(tmp_path, webui=True)

    app.state.webui_mounted = False
    stopped = webui_status(app)
    assert stopped["webui_enabled"] is True
    assert stopped["webui_mounted"] is False
    assert stopped["webui_path"] is None

    app.state.webui_mounted = True
    running = webui_status(app)
    assert running["webui_enabled"] is True
    assert running["webui_mounted"] is True
    assert running["webui_path"] == "/ui"
