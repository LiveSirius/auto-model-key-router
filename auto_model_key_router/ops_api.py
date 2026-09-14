# 桌面端（Keyloom）面板所需的操作型接口。
#
# 管理 API（management_api.py）已经覆盖了配置类资源（供应商 / Key / 路由 /
# 统一模型 / 设置 / 探测 / 迁移）。这里只补上“本机进程”类动作，它们的共同点是
# 都必须跑在服务所在的这台机器上，无法由前端直接完成：
#
# - 服务日志尾部（GET /api/logs）
# - AMKR CLI / 版本 / WebUI 开关（GET /api/tool、POST /api/tool/webui）
# - 后台服务与系统服务控制（POST /api/service/{action}）
# - Agent 集成（GET /api/integrations、POST /api/integrations/{agent}[/rollback]）
#
# 首屏状态复用已有且公开的 GET /health，不额外造一个快照接口。
#
# UI 渲染复用 service.py 已有的 Rich 组件：服务动作的返回值本来就是 Panel/Group，
# 这里用 Console(record=True) 渲染成纯文本返回，保证 WebUI 与 TUI 的提示文案、
# 权限说明、原始命令输出完全一致。

from __future__ import annotations

import asyncio
import io
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException, Request
from rich.console import Console

from . import __version__
from .agent_config import (
    AGENT_MODE_UNIFIED_MODEL,
    SUPPORTED_AGENTS,
    SUPPORTED_AGENT_MODES,
    AgentConfigError,
    agent_display_name,
    configure_agent,
    get_agent_config_status,
    rollback_agent,
)
from .config import RouterConfig
from .config_service import ConfigService
from .management_api import APIModel, ReloadConfig, _authorization_mode
from .webui import webui_status


LOG_TAIL_BYTES = 65536

# 与 Keyloom 的 AmkrServiceAction 对齐；映射到 amkr 内部的服务动作名。
# 'uninstall_amkr' 是取消登录自启（与系统级任务同名，故共用 uninstall）。
_SERVICE_TARGETS: dict[str, tuple[str, str | None]] = {
    "start_amkr": ("background-start", None),
    "stop_amkr": ("background-stop", None),
    "restart_amkr": ("background-restart", None),
    "status_amkr": ("system", "status"),
    "install_user_amkr": ("system", "install-user"),
    "uninstall_amkr": ("system", "uninstall"),
    "install_system_amkr": ("system", "install"),
    "uninstall_system_amkr": ("system", "uninstall"),
    "start_system_amkr": ("system", "start"),
    "stop_system_amkr": ("system", "stop"),
    "restart_system_amkr": ("system", "restart"),
}


class IntegrationRequest(APIModel):
    mode: str = AGENT_MODE_UNIFIED_MODEL


class WebUIRequest(APIModel):
    enabled: bool


def _render_text(renderable: Any) -> str:
    """把 Rich 渲染对象转成纯文本，供 WebUI 的 <pre> 直接展示。"""
    if renderable is None:
        return ""
    if isinstance(renderable, str):
        return renderable
    buffer = io.StringIO()
    console = Console(
        file=buffer,
        record=True,
        width=100,
        force_terminal=False,
        no_color=True,
        highlight=False,
        legacy_windows=False,
    )
    console.print(renderable)
    return console.export_text(styles=False).strip()


def _tail(path: Path, limit: int) -> tuple[str, bool]:
    """读取文件末尾 limit 字节；返回 (文本, 是否被截断)。"""
    size = path.stat().st_size
    with path.open("rb") as handle:
        if size > limit:
            handle.seek(size - limit)
        raw = handle.read()
    return raw.decode("utf-8", errors="replace"), size > limit


def _run_service_action(action: str, config_path: Path, config: RouterConfig) -> str:
    # service 依赖 .app.create_app，这里延迟导入以打断 app -> ops_api -> service 环。
    from .service import manage_system_service, start_service_background, stop_background_service

    kind, argument = _SERVICE_TARGETS[action]
    if kind == "background-start":
        return _render_text(start_service_background(config_path, config))
    if kind == "background-stop":
        return _render_text(stop_background_service(config))
    if kind == "background-restart":
        stopped = _render_text(stop_background_service(config))
        started = _render_text(start_service_background(config_path, config))
        return "\n\n".join(part for part in (stopped, started) if part)
    return _render_text(manage_system_service(config_path, str(argument)))


def register_ops_api(app: FastAPI, reload_config: ReloadConfig) -> None:
    async def authorized_config(request: Request) -> RouterConfig:
        state = request.app.state
        await reload_config(state)
        lease = await state.runtime_manager.acquire()
        try:
            config = lease.resources.config
            if _authorization_mode(request, config.local_api_key) != "full":
                raise HTTPException(status_code=401, detail="本地 API key 验证失败")
            return config
        finally:
            await lease.release()

    def config_path() -> Path:
        raw = str(getattr(app.state, "config_path", "") or "")
        if not raw:
            raise HTTPException(
                status_code=409, detail="未设置配置文件路径，无法执行本机操作"
            )
        path = Path(raw)
        if not path.is_file():
            raise HTTPException(status_code=409, detail=f"配置文件不存在: {path}")
        return path

    @app.get("/api/logs", tags=["ops"])
    async def read_logs(request: Request) -> dict[str, Any]:
        config = await authorized_config(request)
        path = Path(config.log_file_path)
        if not path.is_file():
            return {"text": "", "truncated": False, "path": str(path), "error": None}
        try:
            text, truncated = await asyncio.to_thread(_tail, path, LOG_TAIL_BYTES)
        except OSError as exc:
            return {"text": "", "truncated": False, "path": str(path), "error": str(exc)}
        return {"text": text, "truncated": truncated, "path": str(path), "error": None}

    @app.get("/api/tool", tags=["ops"])
    async def get_tool(request: Request) -> dict[str, Any]:
        from .update import check_latest_version

        await authorized_config(request)
        result = await asyncio.to_thread(check_latest_version, timeout=3.0)
        return {
            "version": __version__,
            "latest_version": result.latest_version,
            "update_available": bool(result.update_available),
            "release_url": result.release_url,
            "source": result.source,
            "error": result.error,
            **webui_status(app),
        }

    @app.post("/api/tool/webui", tags=["ops"])
    async def set_webui(request: Request, payload: WebUIRequest) -> dict[str, Any]:
        await authorized_config(request)
        path = await asyncio.to_thread(config_path)

        def mutation(data: dict[str, Any]) -> None:
            data["webui_enabled"] = payload.enabled

        await asyncio.to_thread(ConfigService(path).update, mutation)
        # 配置写入后热重载即可让 runtime 看到新值；挂载状态要重启进程才变。
        await reload_config(app.state)
        return {"enabled": payload.enabled, **webui_status(app)}

    @app.post("/api/service/{action}", tags=["ops"])
    async def run_service(request: Request, action: str) -> dict[str, Any]:
        if action not in _SERVICE_TARGETS:
            raise HTTPException(status_code=422, detail=f"不支持的服务动作: {action}")
        config = await authorized_config(request)
        path = config_path()
        try:
            text = await asyncio.to_thread(_run_service_action, action, path, config)
        except (OSError, ValueError, KeyError) as exc:
            raise HTTPException(status_code=500, detail=f"服务操作失败: {exc}") from exc
        return {"action": action, "text": text}

    @app.get("/api/integrations", tags=["ops"])
    async def list_integrations(request: Request) -> dict[str, Any]:
        await authorized_config(request)
        return {
            "integrations": [
                _integration_entry(agent) for agent in SUPPORTED_AGENTS
            ]
        }

    @app.post("/api/integrations/{agent}", tags=["ops"])
    async def apply_integration(
        request: Request, agent: str, payload: IntegrationRequest
    ) -> dict[str, Any]:
        if agent not in SUPPORTED_AGENTS:
            raise HTTPException(status_code=404, detail=f"不支持的集成: {agent}")
        if payload.mode not in SUPPORTED_AGENT_MODES:
            raise HTTPException(
                status_code=422, detail="集成模式必须是 native 或 unified-model"
            )
        config = await authorized_config(request)
        try:
            result = await asyncio.to_thread(
                configure_agent, agent, config, mode=payload.mode
            )
        except (OSError, AgentConfigError, ValueError) as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc
        return {
            "integration": _integration_entry(agent),
            "target_path": str(result.target_path),
            "backup_path": str(result.backup_path),
            "router_url": result.router_url,
            "extra_target_paths": [str(path) for path in result.extra_target_paths],
        }

    @app.post("/api/integrations/{agent}/rollback", tags=["ops"])
    async def rollback_integration(request: Request, agent: str) -> dict[str, Any]:
        if agent not in SUPPORTED_AGENTS:
            raise HTTPException(status_code=404, detail=f"不支持的集成: {agent}")
        await authorized_config(request)
        try:
            result = await asyncio.to_thread(rollback_agent, agent)
        except (OSError, AgentConfigError, ValueError) as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc
        return {
            "integration": _integration_entry(agent),
            "target_path": str(result.target_path),
            "restored": result.restored,
        }


def _integration_entry(agent: str) -> dict[str, Any]:
    """读取单个 Agent 的集成状态；读取失败只影响这一个 Agent。

    这里刻意捕获 Exception：/api/integrations 只是三个相互独立的只读状态汇总，
    任何一个 Agent 探测异常都不应该让另外两个也无法显示（Keyloom 同样按 Agent
    隔离错误）。失败原因原样放进 error 字段交给前端展示。
    """
    try:
        status = get_agent_config_status(agent)
    except Exception as exc:  # noqa: BLE001 - 见上方说明：按 Agent 隔离错误
        return {
            "agent": agent,
            "display_name": agent_display_name(agent),
            "target_path": "",
            "target_exists": False,
            "backup_available": False,
            "current_is_applied": False,
            "mode": None,
            "error": f"{type(exc).__name__}: {exc}",
        }
    return {
        "agent": agent,
        "display_name": agent_display_name(agent),
        "target_path": str(status.target_path),
        "target_exists": Path(status.target_path).exists(),
        "backup_available": status.backup_available,
        "current_is_applied": status.current_is_applied,
        "mode": status.mode,
        "error": None,
    }
