from __future__ import annotations

import json
import shutil
import subprocess
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import TypeVar

import anyio
import httpx
import pytest
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


# —— 前端鉴权流程回归 ——
# 曾经的问题：localStorage 里存着失效 Key 时，app.js 只判断"有没有 Key"就认为已授权，
# 各页面于是带着 401 加载并把报错缓存进模块级 state，界面永远停在
# "读取设置失败: AMKR 请求失败（HTTP 401）"，且没有任何回到验证页的路径。
# 这些行为只在浏览器里体现，所以用 node 直接驱动真实的 webui 模块来锁住。

PROBE = Path(__file__).with_name("webui_auth_probe.mjs")

PROBE_SCENARIOS = [
    "stale_key_prompts_login",
    "no_key_prompts_login",
    "valid_key_renders_page",
    "wrong_key_submit_shows_error",
    "correct_key_submit_reloads",
    "mid_session_401_returns_to_login",
    "login_input_survives_health_poll",
    "auth_disabled_no_login",
    "deeplink_without_key_stays_on_login",
    "unreachable_service_stays_on_login",
]


def _node_executable() -> str | None:
    return shutil.which("node")


@pytest.mark.parametrize("scenario", PROBE_SCENARIOS)
def test_webui_auth_flow(scenario: str) -> None:
    node = _node_executable()
    if node is None:
        pytest.skip("未安装 node，跳过 WebUI 前端鉴权流程校验")

    result = subprocess.run(
        [node, str(PROBE), scenario],
        capture_output=True,
        text=True,
        encoding="utf-8",
        cwd=str(PROBE.parent),
    )
    assert result.returncode == 0, (
        f"{scenario} 失败：\nstdout: {result.stdout}\nstderr: {result.stderr}"
    )
    payload = json.loads(result.stdout.strip().splitlines()[-1])
    assert payload["failed"] == [], f"{scenario} 未通过的断言: {payload['failed']}"

