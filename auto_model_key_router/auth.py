"""鉴权抽象：默认实现是「本地 API key + 固定 visitor key」，宿主可整体替换。

嵌入宿主时，宿主通常已经有自己的身份体系（session cookie、JWT、网关注入的身份
头）。在此之前只有两个选择，都不好：把 ``local_api_key`` 留空 —— 那等于**整体
关闭鉴权**；或者让调用方额外再拿一套 AMKR 的 key。``create_app(authenticator=...)``
让宿主直接复用自身身份体系。

返回值刻意保留 ``full`` / ``visitor`` 这套词表：visitor 不是「权限更小」而是
**模型级**权限（只能用标了 allow_visitor 的 Key、不能用 unified-model、拿不到
指标与配置接口），因此不能用 ``is_admin: bool`` 表达。
"""

from __future__ import annotations

import hmac
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Literal

from fastapi import Request, WebSocket

from .config import RouterConfig
from .visitor import is_visitor_api_key


AuthMode = Literal["full", "visitor"]


@dataclass(frozen=True)
class AuthContext:
    """一次成功的鉴权结果。

    ``mode`` 决定权限范围，也是框架唯一解释的字段。调用方身份留给宿主自己 ——
    钩子手上就有 ``request``，往 ``request.state`` 里放即可，不必由框架代管。
    """

    mode: AuthMode

    @property
    def is_full(self) -> bool:
        return self.mode == "full"

    @property
    def visitor_only(self) -> bool:
        return self.mode == "visitor"


# 钩子签名：拿到请求与当前生效的配置，返回 None 表示未通过鉴权。
Authenticator = Callable[[Request, RouterConfig], Awaitable[AuthContext | None]]


def request_api_key(request: Request) -> str:
    """取出请求携带的凭据，支持 ``Authorization: Bearer`` 与 ``x-api-key``。"""
    authorization = request.headers.get("authorization", "")
    if authorization.lower().startswith("bearer "):
        return authorization[7:].strip()
    return request.headers.get("x-api-key", "")


def mode_from_api_key(api_key: str, local_api_key: str) -> AuthMode | None:
    """纯函数：把凭据折算成权限模式。所有实现共用这套判定。

    ``local_api_key`` 为空表示鉴权整体关闭，一律按完整权限处理（既有语义）。
    """
    if not local_api_key:
        return "full"
    # 比较 bytes：hmac.compare_digest 对含非 ASCII 的 str 会抛 TypeError，而 HTTP
    # 头是字节、由 Starlette 按 latin-1 解码 —— 调用方塞一个非 ASCII 凭据就能让
    # 服务端 500 而不是 401（改造前实测可复现）。
    if api_key and hmac.compare_digest(
        api_key.encode("utf-8"), local_api_key.encode("utf-8")
    ):
        return "full"
    if is_visitor_api_key(api_key):
        return "visitor"
    return None


async def default_authenticator(
    request: Request, config: RouterConfig
) -> AuthContext | None:
    """默认实现：本地 API key 为完整权限，固定 visitor key 为受限权限。"""
    mode = mode_from_api_key(request_api_key(request), config.local_api_key)
    return AuthContext(mode) if mode is not None else None


async def authorize(request: Request, config: RouterConfig) -> AuthContext | None:
    """所有接口统一的鉴权入口。

    走 ``app.state.authenticator``（宿主可在 ``create_app`` 时替换），未配置时用
    默认实现。挂载场景下 ``request.app`` 会正确指向 AMKR 自己的实例，因此钩子
    在嵌入时同样生效。
    """
    authenticator = (
        getattr(request.app.state, "authenticator", None) or default_authenticator
    )
    return await authenticator(request, config)


def request_from_websocket_token(websocket: WebSocket, token: str) -> Request:
    """把 WebSocket 的首帧 token 折算成 HTTP 请求，以复用同一个鉴权钩子。

    握手无法带自定义头，所以 AMKR 的凭据只能走首帧；但宿主的 cookie 是**在**握手
    头里的，因此这里保留原始头（session 鉴权能用），只额外补一个
    ``Authorization``，让宿主写一个 HTTP 钩子就同时覆盖两条路径。
    """
    scope = dict(websocket.scope)
    scope.update(
        type="http",
        http_version="1.1",
        method="GET",
        scheme="https" if websocket.url.scheme == "wss" else "http",
        headers=[
            *websocket.scope.get("headers", []),
            (b"authorization", f"Bearer {token}".encode("utf-8")),
        ],
    )

    async def receive() -> dict[str, object]:
        # 鉴权只读 header；给它一个立即结束的 body 即可。
        return {"type": "http.request", "body": b"", "more_body": False}

    return Request(scope, receive)
