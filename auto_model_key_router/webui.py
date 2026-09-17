# 可选 WebUI：把随包发布的静态前端挂载到路由服务上。
#
# 设计取舍（配合 pyproject 的 package-data）：
# - 前端是预构建的静态资产，随 wheel 发布，运行时不需要 Node；
# - 依赖 FastAPI/Starlette 自带的 StaticFiles，不引入 aiofiles 等新依赖；
# - 是否可用由磁盘上是否存在 index.html 决定，是否启用由配置项 webui_enabled
#   （或 CLI --webui / --no-webui）决定，两者独立。

from __future__ import annotations

from pathlib import Path
from typing import Any

from fastapi import FastAPI
from starlette.staticfiles import StaticFiles


# 挂在代理通配路由 /v1/{path:path} 之前的路径前缀，避免被 /v1 吞掉。
WEBUI_MOUNT_PATH = "/ui"

_ASSET_DIR = Path(__file__).resolve().parent / "webui"
_INDEX_FILE = _ASSET_DIR / "index.html"


def webui_available() -> bool:
    """静态资产是否随当前安装一起发布（WebUI 是否可安装）。"""
    return _INDEX_FILE.is_file()


def register_webui(app: FastAPI, *, enabled: bool) -> bool:
    """按需挂载 WebUI，返回是否真的挂载了。

    enabled 为假时不注册任何路由，因此“未安装/未启用”的实例对外完全不暴露
    /ui（返回 404），也就不会有 SPA 首页被代理接口误伤的问题。挂载发生在
    进程启动阶段，运行中修改配置需要重启服务才会生效。
    """
    if not enabled or not webui_available():
        return False
    # html=True 让 /ui/ 返回 index.html，其余资源按文件名直出。
    app.mount(
        WEBUI_MOUNT_PATH,
        StaticFiles(directory=str(_ASSET_DIR), html=True),
        name="webui",
    )
    return True


def webui_status(app: FastAPI) -> dict[str, Any]:
    """给 /health 用的 WebUI 状态摘要。

    mounted 表示当前进程真的在提供 /ui；enabled 表示配置里的期望状态。两者不
    一致时说明改了开关但还没重启服务。

    path 会带上嵌入时的挂载前缀（独立运行是 /ui，挂在 /amkr 下是 /amkr/ui）。
    """
    mounted = bool(getattr(app.state, "webui_mounted", False))
    configured = _configured(app)
    available = webui_available()
    return {
        "webui_available": available,
        "webui_enabled": configured,
        "webui_mounted": mounted,
        "webui_path": webui_path(app) if mounted else None,
    }


def webui_path(app: FastAPI) -> str:
    """WebUI 的实际访问路径，含嵌入时的挂载前缀（无尾斜杠）。"""
    prefix = str(getattr(app.state, "mount_path", "") or "").rstrip("/")
    return f"{prefix}{WEBUI_MOUNT_PATH}"


def _configured(app: FastAPI) -> bool:
    manager = getattr(app.state, "runtime_manager", None)
    current = getattr(manager, "current", None)
    config = getattr(current, "config", None)
    if config is not None:
        return bool(getattr(config, "webui_enabled", False))
    return bool(getattr(app.state, "webui_enabled", False))
