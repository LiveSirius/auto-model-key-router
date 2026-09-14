from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import TypeVar

import anyio
import httpx
from fastapi import FastAPI

from auto_model_key_router.app import create_app
from auto_model_key_router.config import RouterConfig, load_config_data


T = TypeVar("T")
AUTH_HEADERS = {"Authorization": "Bearer local-key"}


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


def config_data(tmp_path: Path) -> dict[str, object]:
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
        "webui_enabled": False,
        "models": [],
    }


def create_file_backed_app(tmp_path: Path) -> tuple[FastAPI, Path]:
    path = tmp_path / "router-config.json"
    path.write_text(json.dumps(config_data(tmp_path)), encoding="utf-8")
    return create_app(RouterConfig.load(path), path), path


def test_ops_endpoints_require_local_key(tmp_path: Path) -> None:
    app, _ = create_file_backed_app(tmp_path)

    async def requests(client: httpx.AsyncClient) -> dict[str, int]:
        return {
            "logs": (await client.get("/api/logs")).status_code,
            "tool": (await client.get("/api/tool")).status_code,
            "integrations": (await client.get("/api/integrations")).status_code,
            "service": (await client.post("/api/service/start_amkr")).status_code,
            "authorized_logs": (
                await client.get("/api/logs", headers=AUTH_HEADERS)
            ).status_code,
        }

    results = run_client(app, requests)

    assert results["logs"] == 401
    assert results["tool"] == 401
    assert results["integrations"] == 401
    assert results["service"] == 401
    assert results["authorized_logs"] == 200


def test_logs_returns_tail_and_handles_missing_file(tmp_path: Path) -> None:
    app, path = create_file_backed_app(tmp_path)
    log_path = Path(config_data(tmp_path)["log_file_path"])

    async def requests(client: httpx.AsyncClient) -> dict[str, object]:
        missing = (await client.get("/api/logs", headers=AUTH_HEADERS)).json()
        log_path.write_text("\n".join(f"line-{i}" for i in range(50)), encoding="utf-8")
        present = (await client.get("/api/logs", headers=AUTH_HEADERS)).json()
        return {"missing": missing, "present": present}

    results = run_client(app, requests)

    assert results["missing"]["text"] == ""
    assert results["missing"]["error"] is None
    assert "line-49" in results["present"]["text"]
    assert results["present"]["truncated"] is False
    assert results["present"]["path"] == str(log_path)


def test_unknown_service_action_is_rejected(tmp_path: Path) -> None:
    app, _ = create_file_backed_app(tmp_path)

    async def requests(client: httpx.AsyncClient) -> httpx.Response:
        return await client.post("/api/service/definitely_not_real", headers=AUTH_HEADERS)

    response = run_client(app, requests)

    assert response.status_code == 422
    assert "不支持的服务动作" in response.json()["detail"]


def test_webui_toggle_persists_to_config_and_reports_mount_state(tmp_path: Path) -> None:
    app, path = create_file_backed_app(tmp_path)

    async def requests(client: httpx.AsyncClient) -> dict[str, object]:
        enabled = await client.post(
            "/api/tool/webui", headers=AUTH_HEADERS, json={"enabled": True}
        )
        disabled = await client.post(
            "/api/tool/webui", headers=AUTH_HEADERS, json={"enabled": False}
        )
        return {"enabled": enabled, "disabled": disabled}

    results = run_client(app, requests)
    enabled = results["enabled"]
    disabled = results["disabled"]

    assert enabled.status_code == 200
    assert enabled.json()["enabled"] is True
    # 挂载发生在进程启动时：热重载配置不会让 /ui 突然出现，需重启服务。
    assert enabled.json()["webui_mounted"] is False
    assert disabled.json()["enabled"] is False

    data = load_config_data(path)
    assert data["webui_enabled"] is False
    assert RouterConfig.load(path).webui_enabled is False


def test_webui_toggle_rejects_invalid_payload(tmp_path: Path) -> None:
    app, _ = create_file_backed_app(tmp_path)

    async def requests(client: httpx.AsyncClient) -> tuple[httpx.Response, httpx.Response]:
        # extra="forbid" 拒绝未知字段；列表无法强转成 bool，同样应被拒绝。
        unknown = await client.post(
            "/api/tool/webui", headers=AUTH_HEADERS, json={"enabled": True, "x": 1}
        )
        bad_type = await client.post(
            "/api/tool/webui", headers=AUTH_HEADERS, json={"enabled": []}
        )
        return unknown, bad_type

    unknown, bad_type = run_client(app, requests)

    assert unknown.status_code == 422
    assert bad_type.status_code == 422


def test_integrations_lists_supported_agents_with_isolated_errors(
    tmp_path: Path, monkeypatch
) -> None:
    app, _ = create_file_backed_app(tmp_path)

    def explode(agent: str) -> object:
        raise RuntimeError(f"boom:{agent}")

    async def requests(client: httpx.AsyncClient) -> httpx.Response:
        return await client.get("/api/integrations", headers=AUTH_HEADERS)

    # 单个 Agent 读取失败不应该让整个接口 500。
    monkeypatch.setattr("auto_model_key_router.ops_api.get_agent_config_status", explode)
    response = run_client(app, requests)

    assert response.status_code == 200
    agents = response.json()["integrations"]
    assert [item["agent"] for item in agents] == ["claude-code", "codex", "pi-agent"]
    assert all(isinstance(item["error"], str) for item in agents)


def test_integration_apply_validates_agent_and_mode(tmp_path: Path) -> None:
    app, _ = create_file_backed_app(tmp_path)

    async def requests(client: httpx.AsyncClient) -> tuple[httpx.Response, httpx.Response]:
        unknown = await client.post(
            "/api/integrations/nope", headers=AUTH_HEADERS, json={"mode": "native"}
        )
        bad_mode = await client.post(
            "/api/integrations/codex", headers=AUTH_HEADERS, json={"mode": "bogus"}
        )
        return unknown, bad_mode

    unknown, bad_mode = run_client(app, requests)

    assert unknown.status_code == 404
    assert bad_mode.status_code == 422
    assert "集成模式必须是 native 或 unified-model" in bad_mode.json()["detail"]
