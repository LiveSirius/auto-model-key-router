from __future__ import annotations

import asyncio
import hashlib
import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any, Literal

import httpx
from fastapi import FastAPI, Query, Request, WebSocket
from fastapi.responses import JSONResponse, Response, StreamingResponse

from . import __version__
from .auth import (
    Authenticator,
    authorize,
    default_authenticator,
    request_from_websocket_token,
)
from .config import RouterConfig
from .event_bus import EventBus
from .key_pool import KeyPool
from .management_api import register_management_api
from .metrics import MetricsStore
from .ops_api import register_ops_api
from .proxy_handler import handle_proxy_request
from .runtime import (
    RuntimeLease,
    RuntimeManager,
    RuntimeResources,
)
from .visitor import visitor_feature_available
from .webui import register_webui, webui_status
from .websocket_proxy import register_websocket_proxy


LOGGER = logging.getLogger("auto_model_key_router.app")


def _new_http_client(timeout: float) -> httpx.AsyncClient:
    return httpx.AsyncClient(
        timeout=timeout,
        limits=httpx.Limits(
            max_connections=100, max_keepalive_connections=30, keepalive_expiry=30
        ),
    )


def create_app(
    config: RouterConfig,
    config_path: str | Path | None = None,
    *,
    webui: bool | None = None,
    enable_ops: bool | None = None,
    authenticator: Authenticator | None = None,
) -> FastAPI:
    @asynccontextmanager
    async def lifespan(lifespan_app: FastAPI) -> AsyncIterator[None]:
        # lifespan_app 始终是 AMKR 自己的应用实例：独立运行时由 uvicorn 传入，
        # 挂载运行时由 mount_app 显式绑定（Starlette 不会为 Mount 的子应用跑
        # lifespan，所以挂载方必须自己把这里接上，否则下面的资源永不释放）。
        lifespan_app.state._metrics_broadcast_task = asyncio.create_task(
            _broadcast_metrics_loop()
        )
        try:
            yield
        finally:
            for attr in ("_metrics_broadcast_task",):
                task = getattr(lifespan_app.state, attr, None)
                if task is not None:
                    task.cancel()
                    try:
                        await task
                    except asyncio.CancelledError:
                        pass
            await lifespan_app.state.runtime_manager.close()

    app = FastAPI(title="Auto Model Key Router", version=__version__, lifespan=lifespan)
    # webui=None 表示跟随配置文件；True/False 是 CLI 的显式覆盖。
    app.state.webui_enabled = config.webui_enabled if webui is None else webui
    # 宿主可整体替换鉴权；默认实现保持「本地 API key + 固定 visitor key」。
    app.state.authenticator = authenticator or default_authenticator
    app.state.config_path = (
        str(Path(config_path).resolve()) if config_path is not None else ""
    )
    app.state.config_mtime = _config_mtime(app.state.config_path)
    app.state.config_reload_lock = asyncio.Lock()
    app.state.config_write_lock = asyncio.Lock()
    app.state.management_probes = {}
    app.state.management_probe_tasks = {}
    key_pool = KeyPool(config)
    metrics = MetricsStore(config.metrics_db_path)
    http_client = _new_http_client(config.request_timeout)
    app.state.runtime_manager = RuntimeManager(
        RuntimeResources(config, key_pool, metrics, http_client)
    )
    app.state.event_bus = EventBus()

    # metrics_snapshot 节流广播
    _metrics_dirty = asyncio.Event()
    _IDLE_BROADCAST_INTERVAL = 30.0

    async def _broadcast_metrics_snapshot() -> None:
        try:
            metrics = app.state.runtime_manager.current.metrics
            snapshot = await metrics.snapshot(since=metrics._started_at)
            await app.state.event_bus.broadcast("metrics_snapshot", snapshot)
        except Exception:
            LOGGER.debug("metrics_snapshot broadcast failed", exc_info=True)

    async def _broadcast_metrics_loop() -> None:
        while True:
            try:
                await asyncio.wait_for(
                    _metrics_dirty.wait(), timeout=_IDLE_BROADCAST_INTERVAL
                )
                _metrics_dirty.clear()
            except asyncio.TimeoutError:
                pass  # 空闲心跳，刷新 RPM/TPM 衰减
            if app.state.event_bus.client_count > 0:
                await _broadcast_metrics_snapshot()
            await asyncio.sleep(1.0)

    async def _on_metrics_recorded() -> None:
        _metrics_dirty.set()

    async def _on_client_count_change(count: int) -> None:
        await app.state.event_bus.broadcast("client_count", {"count": count})
        if count > 0:
            await _broadcast_metrics_snapshot()

    metrics.on_record = _on_metrics_recorded
    app.state.event_bus.on_client_count_change = _on_client_count_change
    app.state._metrics_broadcast_task: asyncio.Task | None = None

    register_management_api(app, _reload_config_if_changed)
    # 运维接口作用于「服务所在的这台机器」（启停后台进程、注册系统服务、改写
    # Claude Code/Codex 本地配置），嵌入到别人的进程里语义不成立，因此可关闭。
    # enable_ops=None 表示跟随配置文件，True/False 是显式覆盖。
    if enable_ops is None:
        enable_ops = config.ops_enabled
    if enable_ops:
        register_ops_api(app, _reload_config_if_changed)
    app.state.ops_enabled = enable_ops
    app.state.webui_mounted = register_webui(app, enabled=app.state.webui_enabled)

    @app.head("/", include_in_schema=False)
    async def root_probe() -> Response:
        return Response(status_code=204)

    @app.get("/health")
    async def health() -> dict[str, Any]:
        await _reload_config_if_changed(app.state)
        lease = await _acquire_runtime(app.state)
        runtime = lease.resources
        try:
            local_api_key = runtime.config.local_api_key
            visitor_key_count = sum(
                runtime.key_pool.visitor_key_count(model_id)
                for model_id in runtime.key_pool.model_ids
            )
            visitor_installed = visitor_feature_available()
            return {
                "status": "ok",
                "version": __version__,
                "models": runtime.key_pool.public_model_ids,
                "config_path": app.state.config_path,
                "local_auth_enabled": bool(local_api_key),
                "local_api_key_fingerprint": _key_fingerprint(local_api_key),
                "visitor_feature_installed": visitor_installed,
                "visitor_access_enabled": visitor_installed and visitor_key_count > 0,
                "visitor_key_count": visitor_key_count if visitor_installed else 0,
                "unified_model": runtime.key_pool.unified_route,
                "native_endpoint_states": runtime.key_pool.endpoint_capability_states(),
                "ops_enabled": bool(getattr(app.state, "ops_enabled", True)),
                **webui_status(app),
            }
        finally:
            await lease.release()

    @app.get("/v1/models")
    async def models(request: Request) -> Response:
        await _reload_config_if_changed(app.state)
        lease = await _acquire_runtime(app.state)
        try:
            runtime = lease.resources
            auth = await authorize(request, runtime.config)
            if auth is None:
                return JSONResponse(
                    {"error": {"message": "本地 API key 验证失败"}}, status_code=401
                )
            return JSONResponse(
                {
                    "object": "list",
                    "data": [
                        {
                            "id": model_id,
                            "object": "model",
                            "owned_by": "auto-model-key-router",
                        }
                        for model_id in runtime.key_pool.available_model_ids(
                            visitor_only=auth.visitor_only
                        )
                    ],
                }
            )
        finally:
            await lease.release()

    @app.get("/metrics")
    async def metrics(
        request: Request,
        hours: float = Query(default=24, gt=0, le=8760),
        all_history: bool = False,
    ):
        await _reload_config_if_changed(app.state)
        lease = await _acquire_runtime(app.state)
        try:
            auth = await authorize(request, lease.resources.config)
            if auth is None or not auth.is_full:
                return JSONResponse(
                    {"error": {"message": "本地 API key 验证失败"}}, status_code=401
                )
            # hours=None 会全表扫描（16 万行实测 3–5 秒），而 snapshot 与 record
            # 共用同一把锁，于是这个只读接口会把代理写路径一起卡住。全量改由
            # 显式的 all_history=true 触发。
            return await lease.resources.metrics.snapshot(
                hours=None if all_history else hours
            )
        finally:
            await lease.release()

    @app.get("/metrics/requests")
    async def metric_requests(
        request: Request,
        hours: float = Query(default=24, gt=0, le=720),
        all_history: bool = False,
        caller_type: Literal["local", "visitor"] | None = None,
        model_id: str | None = Query(default=None, min_length=1, max_length=512),
        requested_model_id: str | None = Query(default=None, min_length=1, max_length=512),
        provider_id: str | None = Query(default=None, min_length=1, max_length=512),
        pool_name: str | None = Query(default=None, min_length=1, max_length=512),
        upstream_model_id: str | None = Query(default=None, min_length=1, max_length=512),
        key_name: str | None = Query(default=None, min_length=1, max_length=512),
        status_code: int | None = Query(default=None, ge=100, le=599),
        success: bool | None = None,
        attributed: bool | None = None,
        limit: int = Query(default=50, ge=1, le=200),
        before_id: int | None = Query(default=None, ge=1),
    ):
        await _reload_config_if_changed(app.state)
        lease = await _acquire_runtime(app.state)
        try:
            auth = await authorize(request, lease.resources.config)
            if auth is None or not auth.is_full:
                return JSONResponse(
                    {"error": {"message": "本地 API key 验证失败"}}, status_code=401
                )
            return await lease.resources.metrics.request_history(
                hours=None if all_history else hours,
                caller_type=caller_type,
                model_id=model_id,
                requested_model_id=requested_model_id,
                provider_id=provider_id,
                pool_name=pool_name,
                upstream_model_id=upstream_model_id,
                key_name=key_name,
                status_code=status_code,
                success=success,
                attributed=attributed,
                limit=limit,
                before_id=before_id,
            )
        finally:
            await lease.release()

    @app.get("/metrics/series")
    async def metric_series(
        request: Request,
        hours: float = Query(default=1, gt=0, le=720),
        bucket_seconds: int = Query(default=60, ge=15, le=86400),
        caller_type: Literal["local", "visitor"] | None = None,
        model_id: str | None = Query(default=None, min_length=1, max_length=512),
        requested_model_id: str | None = Query(default=None, min_length=1, max_length=512),
        provider_id: str | None = Query(default=None, min_length=1, max_length=512),
        pool_name: str | None = Query(default=None, min_length=1, max_length=512),
        upstream_model_id: str | None = Query(default=None, min_length=1, max_length=512),
        key_name: str | None = Query(default=None, min_length=1, max_length=512),
        status_code: int | None = Query(default=None, ge=100, le=599),
        success: bool | None = None,
        attributed: bool | None = None,
    ):
        await _reload_config_if_changed(app.state)
        lease = await _acquire_runtime(app.state)
        try:
            auth = await authorize(request, lease.resources.config)
            if auth is None or not auth.is_full:
                return JSONResponse(
                    {"error": {"message": "本地 API key 验证失败"}}, status_code=401
                )
            try:
                return await lease.resources.metrics.time_series(
                    hours=hours,
                    bucket_seconds=bucket_seconds,
                    caller_type=caller_type,
                    model_id=model_id,
                    requested_model_id=requested_model_id,
                    provider_id=provider_id,
                    pool_name=pool_name,
                    upstream_model_id=upstream_model_id,
                    key_name=key_name,
                    status_code=status_code,
                    success=success,
                    attributed=attributed,
                )
            except ValueError as exc:
                return JSONResponse(
                    {"error": {"message": str(exc)}}, status_code=422
                )
        finally:
            await lease.release()

    @app.websocket("/ws/events")
    async def ws_events(websocket: WebSocket) -> None:
        await websocket.accept()
        event_bus: EventBus = app.state.event_bus
        config = app.state.runtime_manager.current.config

        async def verify(candidate: str) -> bool:
            # 复用统一鉴权钩子：握手没法带自定义头，token 只能来自首帧；折算出
            # Request 后宿主的 cookie 鉴权同样能用。事件流只对完整权限开放。
            auth = await authorize(
                request_from_websocket_token(websocket, candidate), config
            )
            return auth is not None and auth.is_full

        if not await event_bus.authenticate(websocket, verify):
            return
        try:
            await websocket.send_json({"type": "connected", "data": {}})
            while True:
                try:
                    await websocket.receive_text()
                except Exception:
                    break
        finally:
            await event_bus.disconnect(websocket)

    async def proxy(path: str, request: Request) -> Response:
        await _reload_config_if_changed(app.state)
        lease = await _acquire_runtime(app.state)
        lease.resources.metrics.acquire_active()
        _metrics_dirty.set()
        try:
            response = await handle_proxy_request(path, request, lease.resources)
        except BaseException:
            lease.resources.metrics.release_active()
            await lease.release()
            raise
        if isinstance(response, StreamingResponse):
            response.body_iterator = lease.wrap_stream(
                _wrap_active_stream(response.body_iterator, lease.resources.metrics)
            )
        else:
            lease.resources.metrics.release_active()
            await lease.release()
        return response

    app.add_api_route(
        "/v1/{path:path}", proxy, methods=["GET", "POST", "PUT", "PATCH", "DELETE"]
    )
    register_websocket_proxy(app, proxy)
    return app


def mount_app(
    parent: FastAPI,
    path: str,
    config: RouterConfig | None = None,
    config_path: str | Path | None = None,
    *,
    app: FastAPI | None = None,
    webui: bool | None = None,
    enable_ops: bool = False,
    authenticator: Authenticator | None = None,
) -> FastAPI:
    """把 AMKR 挂到 parent 的 path 前缀下，并把它的 lifespan 接进父应用。

    Starlette 只会运行顶层应用的 lifespan，``Mount`` 的子应用生命周期会被静默
    跳过。直接 ``parent.mount()`` 的话，AMKR 的指标广播任务不会启动、runtime 的
    httpx client 与 sqlite 连接也不会关闭，所以这里必须把子应用 lifespan 链进
    父应用；调用方不需要自己 ``async with app.router.lifespan_context(app)``。

    挂载后所有接口位于 ``path`` 之下（``path="/amkr"`` → ``/amkr/health``、
    ``/amkr/v1/chat/completions``），WebUI 位于 ``path + /ui/``，其前端会自动把
    API 基址推导到同一前缀。必须在父应用开始处理请求前调用。

    ``enable_ops`` 默认关闭：运维接口会启停本机后台服务、注册系统服务、改写
    Claude Code / Codex 配置，嵌入到别人的进程里语义不成立。

    ``authenticator`` 可整体替换鉴权（见 ``auth.py``）：宿主已有自己的身份体系时，
    用它表达「已登录用户 = full」，不必再让调用方多持一套 AMKR 的 key。
    """
    if app is None:
        if config is None:
            raise ValueError("mount_app 需要 config 或现成的 app")
        app = create_app(
            config,
            config_path,
            webui=webui,
            enable_ops=enable_ops,
            authenticator=authenticator,
        )
    prefix = "/" + path.strip("/")
    # 记下挂载前缀，供 /health 与 /api/tool 汇报真实的 WebUI 访问路径。
    app.state.mount_path = "" if prefix == "/" else prefix

    inner_lifespan = app.router.lifespan_context
    outer_lifespan = parent.router.lifespan_context

    @asynccontextmanager
    async def lifespan(parent_app: FastAPI) -> AsyncIterator[None]:
        async with inner_lifespan(app):
            async with outer_lifespan(parent_app):
                yield

    parent.router.lifespan_context = lifespan
    parent.mount(prefix, app, name="amkr")
    return app


def _config_mtime(config_path: str) -> float:
    if not config_path:
        return 0.0
    try:
        return Path(config_path).stat().st_mtime
    except OSError:
        return 0.0


async def _reload_config_if_changed(state: Any) -> None:
    config_path = getattr(state, "config_path", "")
    mtime = _config_mtime(config_path)
    if not mtime or mtime == getattr(state, "config_mtime", 0.0):
        return
    async with state.config_reload_lock:
        mtime = _config_mtime(config_path)
        if not mtime or mtime == getattr(state, "config_mtime", 0.0):
            return
        try:
            config = await asyncio.to_thread(RouterConfig.load, config_path)
        except (OSError, ValueError):
            return
        old_runtime = state.runtime_manager.current
        old_config = old_runtime.config
        http_client = old_runtime.http_client
        metrics = old_runtime.metrics
        if old_config.request_timeout != config.request_timeout:
            http_client = _new_http_client(config.request_timeout)
        if old_config.metrics_db_path != config.metrics_db_path:
            metrics = await asyncio.to_thread(MetricsStore, config.metrics_db_path)
        try:
            key_pool = await asyncio.to_thread(KeyPool, config)
        except (OSError, ValueError, KeyError):
            return
        metrics.on_record = old_runtime.metrics.on_record
        await state.runtime_manager.replace(
            RuntimeResources(config, key_pool, metrics, http_client)
        )
        state.config_mtime = _config_mtime(config_path) or mtime
        event_bus: EventBus = state.event_bus
        if event_bus.client_count > 0:
            await event_bus.broadcast("config_change", {"reloaded": True})


async def _acquire_runtime(state: Any) -> RuntimeLease:
    await _reload_config_if_changed(state)
    return await state.runtime_manager.acquire()


async def _wrap_active_stream(
    iterator: AsyncIterator[bytes], metrics: MetricsStore
) -> AsyncIterator[bytes]:
    """包装流式响应迭代器，流结束时释放活跃计数。"""
    try:
        async for chunk in iterator:
            yield chunk
    finally:
        metrics.release_active()


def _key_fingerprint(api_key: str) -> str:
    if not api_key:
        return ""
    return hashlib.sha256(api_key.encode("utf-8")).hexdigest()[:12]
