#!/usr/bin/env python3
"""生成 internal/proxy 的差分对拍语料。

语料由**真实 Python 应用**（FastAPI + ASGI，非 TestClient）产出：每条用例都会在
独立临时目录里建库建配置、把应用的 http_client 换成脚本化的 MockTransport，然后
发一次下游请求，记录：

* ``status`` —— 下游状态码；
* ``body`` —— 下游响应体（UTF-8 文本，二进制体用 base64 表达，见 body_b64）；
* ``headers`` —— 下游响应头（小写键、去空白、剔掉逐跳头与日期类头）；
* ``upstreams`` —— 依次发生的上游请求（方法、路径、query、关键请求头、请求体），
  用来断言两条实现的**路由与回退序列**完全一致，而不只是最终响应。

对拍的价值在于：Go 侧只读语料、不需要 Python 解释器；``--check`` 是新鲜度闸门。

用法::

    python scripts/gen_proxy_handler_corpus.py
    python scripts/gen_proxy_handler_corpus.py --check
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import contextlib
import json
import sys
import tempfile
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import httpx  # noqa: E402

from auto_model_key_router.app import create_app  # noqa: E402
from auto_model_key_router.config import RouterConfig  # noqa: E402
OUTPUT = REPO_ROOT / "internal" / "proxy" / "testdata" / "handler.jsonl"

# 下游响应里必须剔除的头：
#   * date / server 由服务器注入，两次运行必然不同；
#   * transfer-encoding 是分帧细节（Go 侧用 chunked，Python 用 content-length 时
#     也允许不同）；
#   * content-length 会在「是否分块」变化时变化，但它是可观测契约的一部分，
#     所以**不**剔除——两侧都必须算对。
_DROP_HEADERS = {"date", "server", "transfer-encoding", "connection", "keep-alive"}


# --- 应用与上游脚本 -----------------------------------------------------------


class ScriptedTransport(httpx.AsyncBaseTransport):
    """按路径消费脚本化响应，并记录每一次上游请求。"""

    def __init__(self, routes: dict[str, list[dict[str, Any]]]) -> None:
        self.routes = {key: list(value) for key, value in routes.items()}
        self.calls: list[dict[str, Any]] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        body = await request.aread()
        path = request.url.path
        query = request.url.query.decode("latin-1") if request.url.query else ""
        self.calls.append(
            {
                "method": request.method,
                "path": path,
                "query": query,
                "authorization": request.headers.get("authorization"),
                "accept_encoding": request.headers.get("accept-encoding"),
                "content_type": request.headers.get("content-type"),
                "anthropic_version": request.headers.get("anthropic-version"),
                "body": body.decode("utf-8", errors="replace"),
            }
        )
        queue = self.routes.get(path)
        if not queue:
            # 未脚本化的路径：返回一个明确的 500，让「路由错了」在状态码上暴露，
            # 而不是被当成一次正常的成功。
            return httpx.Response(
                500,
                headers={"content-type": "application/json"},
                json={"error": {"message": f"未脚本化的上游路径: {path}"}},
            )
        spec = queue.pop(0) if len(queue) > 1 else queue[0]
        return _build_response(spec)

    async def aclose(self) -> None:  # pragma: no cover - 接口要求
        return None


def _build_response(spec: dict[str, Any]) -> httpx.Response:
    if spec.get("error"):
        raise httpx.ConnectError("脚本化的传输失败")

    headers = dict(spec.get("headers") or {})
    status = int(spec.get("status", 200))
    if spec.get("chunks") is not None:
        chunks = [chunk.encode("utf-8") for chunk in spec["chunks"]]
        stream = httpx.AsyncByteStream  # 占位，避免 linter 误报未使用

        class _Chunks(httpx.AsyncByteStream):
            def __init__(self, items: list[bytes]) -> None:
                self.items = items

            async def __aiter__(self):
                for item in self.items:
                    yield item

        break_after = spec.get("break_after")

        class _Chunks(httpx.AsyncByteStream):
            def __init__(self, items: list[bytes]) -> None:
                self.items = items

            async def __aiter__(self):
                for index, item in enumerate(self.items):
                    if break_after is not None and index >= break_after:
                        # 复刻「上游流中途断开」：async generator 抛 httpx.ReadError，
                        # 参照实现把它当 httpx.RequestError 兜底捕获 ⇒ failed=True，
                        # 且**不补**收尾事件（那些写在 try 内、循环之后）。
                        raise httpx.ReadError("脚本化的中途断开")
                    yield item

        del stream
        return httpx.Response(status, headers=headers, stream=_Chunks(chunks))
    raw = spec.get("body")
    content = raw.encode("utf-8") if isinstance(raw, str) else b""
    return httpx.Response(status, headers=headers, content=content)


def _split_request(request: dict[str, Any]) -> dict[str, Any]:
    """把下游请求的 path 与 query 拆开记录。

    Go 侧不能自己拆：语料里的 path 会被直接交给 Handle 作为路由参数，若它带着
    ``?``，``_upstream_path`` 会把它当成路径的一部分（实测会拼出
    ``v1/chat/completions?x=1``），从而与参照实现产生虚假差异。
    """
    result = dict(request)
    path, _, query = request["path"].partition("?")
    result["path"] = path
    result["query"] = query
    return result


def _normalize_headers(headers: httpx.Headers) -> dict[str, str]:
    result: dict[str, str] = {}
    for key, value in headers.items():
        lower = key.lower()
        if lower in _DROP_HEADERS:
            continue
        result[lower] = value.strip()
    return result


async def run_case(case: dict[str, Any], tmp_path: Path) -> dict[str, Any]:
    config = _build_config(case["config"], tmp_path)
    app = create_app(config)
    transport = ScriptedTransport(case.get("upstream", {}))
    async with app.router.lifespan_context(app):
        # 应用的 create_app 自建了 http_client；这里整体替换成脚本化版本，等价于
        # tests/test_app.py 里 `app.state.runtime_manager.current.http_client = ...`。
        runtime = app.state.runtime_manager.current
        await runtime.http_client.aclose()
        runtime.http_client = httpx.AsyncClient(transport=transport)

        request = case["request"]
        # 默认补上本地凭据；用例显式给了 authorization 时以用例为准（鉴权用例
        # 正是靠这一点覆盖 401/访客分支）。no_auth=True 表示「完全不带凭据」。
        headers = dict(request.get("headers") or {})
        if "authorization" not in headers and not request.get("no_auth"):
            headers["authorization"] = "Bearer " + config.local_api_key
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app),
            base_url="http://testserver",
            follow_redirects=False,
        ) as client:
            response = await client.request(
                request.get("method", "POST"),
                request["path"],
                headers=headers,
                content=(request.get("body") or "").encode("utf-8"),
            )

    body_bytes = response.content
    record: dict[str, Any] = {
        "name": case["name"],
        "note": case.get("note", ""),
        # config 与 request 是 Go 侧重建场景所需的输入。
        "config": case["config"],
        "request": _split_request(case["request"]),
        "status": response.status_code,
        "headers": _normalize_headers(response.headers),
        # upstream_calls 是**实际发生**的上游调用序列，用于逐条断言路由与回退。
        # upstream 是输入脚本，会被重新序列化后一并写入，便于 Go 侧构造假上游。
        "upstream": case.get("upstream", {}),
        "upstream_calls": [
            {
                "method": call["method"],
                "path": call["path"],
                "query": call["query"],
                "authorization": call["authorization"],
                "accept_encoding": call["accept_encoding"],
                "content_type": call["content_type"],
                "body": call["body"],
            }
            for call in transport.calls
        ],
    }
    try:
        record["body"] = body_bytes.decode("utf-8")
    except UnicodeDecodeError:
        record["body_b64"] = base64.b64encode(body_bytes).decode("ascii")
    return record


# --- 配置构造 -----------------------------------------------------------------


def _build_config(raw: dict[str, Any], tmp_path: Path) -> RouterConfig:
    """把语料里的紧凑配置描述展开成 RouterConfig。

    刻意**不走** from_dict：语料要能精确控制 KeyConfig / ModelConfig 的每个字段
    （尤其是 upstream_model 与 provider），而 from_dict 的 providers 展开会引入
    与代理层无关的复杂度。
    """
    data = dict(raw)
    models = []
    for model_data in data.pop("models"):
        model_data = dict(model_data)
        model_data["keys"] = tuple(
            _key_from_dict(key_data, index, model_data["id"])
            for index, key_data in enumerate(model_data.pop("keys"))
        )
        models.append(_model_from_dict(model_data))
    unified = data.pop("unified_model", None)
    tasks = data.pop("tasks", None)
    return RouterConfig(
        host="127.0.0.1",
        port=8000,
        request_timeout=10,
        stream_first_byte_timeout=data.pop("stream_first_byte_timeout", 60),
        stream_idle_timeout=data.pop("stream_idle_timeout", 60),
        max_retries=data.pop("max_retries", 1),
        key_failure_threshold=data.pop("key_failure_threshold", 2),
        key_cooldown_seconds=data.pop("key_cooldown_seconds", 60),
        endpoint_capabilities_path=str(tmp_path / "endpoint-capabilities.json"),
        metrics_db_path=str(tmp_path / "metrics.sqlite3"),
        log_file_path=str(tmp_path / "server.log"),
        local_api_key=data.pop("local_api_key", "local-key"),
        models=tuple(models),
        upstream_routes=data.pop("upstream_routes", {}),
        unified_model=_unified_from_dict(unified) if unified else None,
        tasks=tuple(_task_from_dict(task) for task in tasks) if tasks else (),
        **data,
    )


def _key_from_dict(raw: dict[str, Any], index: int, model_id: str):
    from auto_model_key_router.config import KeyConfig

    return KeyConfig(
        name=raw.get("name", f"{model_id}-key-{index}"),
        api_key=raw.get("api_key", "sk-test"),
        base_url=raw.get("base_url", "https://upstream.test"),
        enabled=raw.get("enabled", True),
        allow_visitor=raw.get("allow_visitor", False),
        upstream_model=raw.get("upstream_model", ""),
        provider=raw.get("provider"),
    )


def _model_from_dict(raw: dict[str, Any]):
    from auto_model_key_router.config import ModelConfig

    return ModelConfig(
        id=raw["id"],
        keys=raw["keys"],
        aliases=tuple(raw.get("aliases") or ()),
        routing_mode=raw.get("routing_mode", "round_robin"),
        reasoning_effort=raw.get("reasoning_effort"),
        native_first=raw.get("native_first", False),
        hidden_aliases=tuple(raw.get("hidden_aliases") or ()),
    )


def _unified_from_dict(raw: dict[str, Any]):
    from auto_model_key_router.config import (
        RoutePlan,
        RouteTarget,
        UnifiedModelConfig,
    )

    def plan(value):
        return RoutePlan(
            primary=RouteTarget(**value["primary"]),
            fallback=RouteTarget(**value["fallback"]) if value.get("fallback") else None,
        )

    return UnifiedModelConfig(
        default=plan(raw["default"]),
        image=plan(raw["image"]) if raw.get("image") else None,
        embeddings=plan(raw["embeddings"]) if raw.get("embeddings") else None,
    )


def _task_from_dict(raw: dict[str, Any]):
    from auto_model_key_router.config import TaskConfig

    return TaskConfig(
        name=raw["name"],
        model=raw["model"],
        fallback_model=raw.get("fallback_model"),
        params=raw.get("params") or {},
    )


# --- 用例集 -------------------------------------------------------------------


def model_spec(model_id: str, keys: list[dict[str, Any]], **extra: Any) -> dict[str, Any]:
    result = {
        "id": model_id,
        "keys": keys,
        "aliases": extra.pop("aliases", []),
        "routing_mode": extra.pop("routing_mode", "round_robin"),
        "reasoning_effort": extra.pop("reasoning_effort", None),
        "native_first": extra.pop("native_first", False),
        "hidden_aliases": extra.pop("hidden_aliases", []),
    }
    result.update(extra)
    return result


def key(name: str, **extra: Any) -> dict[str, Any]:
    result = {"name": name, "api_key": "sk-" + name, "base_url": "https://upstream.test"}
    result.update(extra)
    return result


def chat_ok(model_id: str = "vendor-model") -> dict[str, Any]:
    return {
        "status": 200,
        "headers": {"content-type": "application/json"},
        "body": json.dumps(
            {
                "id": "cmpl-1",
                "object": "chat.completion",
                "model": model_id,
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "hi"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
            },
            separators=(",", ":"),
        ),
    }


def chat_error(status: int, message: str = "boom", extra: dict[str, Any] | None = None):
    payload = {"error": {"message": message}}
    if extra:
        payload["error"].update(extra)
    return {
        "status": status,
        "headers": {"content-type": "application/json"},
        "body": json.dumps(payload, ensure_ascii=False, separators=(",", ":")),
    }


def sse_chunks(*chunks: str) -> dict[str, Any]:
    return {
        "status": 200,
        "headers": {"content-type": "text/event-stream"},
        "chunks": list(chunks),
    }


def sse_step_with_break(chunks: list[str], *, break_after: int) -> dict[str, Any]:
    """构造「先到若干块、然后上游中途断开」的脚本化响应。"""
    return {
        "status": 200,
        "headers": {"content-type": "text/event-stream"},
        "chunks": chunks,
        "break_after": break_after,
    }


def case(
    name: str,
    note: str,
    config: dict[str, Any],
    request: dict[str, Any],
    upstream: dict[str, list[dict[str, Any]]],
) -> dict[str, Any]:
    request = {"method": "POST", **request}
    return {
        "name": name,
        "note": note,
        "config": config,
        "request": request,
        "upstream": upstream,
    }


SINGLE = {"models": [model_spec("vendor-model", [key("k1")])]}
MULTI = {
    "models": [
        model_spec("vendor-model", [key("k1"), key("k2"), key("k3")], routing_mode="round_robin")
    ]
}
MULTI_ALIAS = {
    "models": [
        model_spec(
            "vendor-model",
            [key("k1"), key("k2")],
            aliases=["alias-model"],
        )
    ]
}


def build_cases() -> list[dict[str, Any]]:
    cases: list[dict[str, Any]] = []
    add = cases.append

    # --- 鉴权与请求体边界 -----------------------------------------------------
    add(
        case(
            "auth_missing",
            "缺少凭据 ⇒ 401；不触上游",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[]}',
                "no_auth": True,
            },
            {},
        )
    )
    add(
        case(
            "auth_wrong",
            "错误凭据 ⇒ 401",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "headers": {"authorization": "Bearer nope"},
                "body": '{"model":"vendor-model"}',
            },
            {},
        )
    )
    add(
        case(
            "auth_visitor_unified",
            "访客访问 unified-model ⇒ 403",
            {**SINGLE, "unified_model": {"default": {"primary": {"model": "vendor-model"}}}},
            {
                "path": "/v1/chat/completions",
                "headers": {"authorization": "Bearer amkr-visitor"},
                "body": '{"model":"unified-model","messages":[]}',
            },
            {},
        )
    )
    add(
        case(
            "body_empty",
            "空体 ⇒ 400 缺少 model 字段（参照行为）",
            SINGLE,
            {"path": "/v1/chat/completions", "body": ""},
            {},
        )
    )
    add(
        case(
            "body_invalid_json",
            "非法 JSON ⇒ 静默变 {} ⇒ 400「缺少 model 字段」（不是解析错误）",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":'},
            {},
        )
    )
    add(
        case(
            "body_not_object",
            "数组体 ⇒ 静默变 {} ⇒ 400",
            SINGLE,
            {"path": "/v1/chat/completions", "body": "[1,2,3]"},
            {},
        )
    )
    add(
        case(
            "body_model_null",
            "显式 model:null 仍然**有** model 键 ⇒ 不走字节透传，走「缺少 model」",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":null,"messages":[]}'},
            {},
        )
    )
    add(
        case(
            "body_model_empty_string",
            "model 为空串（假值）⇒ 400",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"","messages":[]}'},
            {},
        )
    )
    add(
        case(
            "model_unknown",
            "未配置的模型 ⇒ 404 且带配置提示",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"nope","messages":[]}'},
            {},
        )
    )
    add(
        case(
            "model_via_alias",
            "别名解析到真实模型 ID，上游收到真实 ID",
            MULTI_ALIAS,
            {"path": "/v1/chat/completions", "body": '{"model":"alias-model","messages":[]}'},
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "count_tokens",
            "count_tokens 本地计算、不转发、不写指标行",
            SINGLE,
            {
                "path": "/v1/messages/count_tokens",
                "body": '{"model":"vendor-model","messages":[{"role":"user","content":"你好"}]}',
            },
            {},
        )
    )
    add(
        case(
            "count_tokens_unknown_model",
            "count_tokens 在模型校验之后 ⇒ 未配置模型仍 404",
            SINGLE,
            {"path": "/v1/messages/count_tokens", "body": '{"model":"nope","messages":[]}'},
            {},
        )
    )

    # --- 非流式：三条协议 ------------------------------------------------------
    add(
        case(
            "chat_nonstream",
            "chat/completions 非流式透传上游 JSON",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "chat_stream",
            "chat/completions 流式：SSE 事件原样转发",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {
                "/v1/chat/completions": [
                    sse_chunks(
                        'data: {"choices":[{"delta":{"content":"he"}}]}\n\n',
                        'data: {"choices":[{"delta":{"content":"llo"}}]}\n\n',
                        "data: [DONE]\n\n",
                    )
                ]
            },
        )
    )
    add(
        case(
            "chat_stream_split_event",
            "一个 SSE 事件被 TCP 分片切开 ⇒ 未完成事件不外发",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {
                "/v1/chat/completions": [
                    sse_chunks(
                        'data: {"choices":[{"delta":{"content":"a"}}]}\n',
                        '\ndata: {"choices":[{"delta":{"content":"b"}}]}\n\n',
                    )
                ]
            },
        )
    )
    add(
        case(
            "chat_stream_crlf",
            "SSE 分隔符为 \\r\\n\\r\\n 时同样切分",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {"/v1/chat/completions": [sse_chunks("data: {}\r\n\r\ndata: {}\r\n\r\n")]},
        )
    )
    add(
        case(
            "chat_stream_unterminated",
            "流结束时未完成的事件被**外发**（参照实现把残余 yield 出去）",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {"/v1/chat/completions": [sse_chunks("data: partial")]},
        )
    )
    add(
        case(
            "chat_stream_non_sse",
            "上游 content-type 非 SSE ⇒ 逐块原样转发，不做事件切分",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {
                "/v1/chat/completions": [
                    {
                        "status": 200,
                        "headers": {"content-type": "application/octet-stream"},
                        "chunks": ["raw-1", "raw-2"],
                    }
                ]
            },
        )
    )
    add(
        case(
            "messages_nonstream",
            "messages 非流式：上游 chat JSON 转 Anthropic message",
            SINGLE,
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}',
            },
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "messages_stream",
            "messages 流式：OpenAI SSE 重建为 Anthropic SSE（含 message_start/stop）",
            SINGLE,
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/chat/completions": [
                    sse_chunks(
                        'data: {"choices":[{"delta":{"content":"你"}}]}\n\n',
                        'data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}\n\n',
                        "data: [DONE]\n\n",
                    )
                ]
            },
        )
    )
    add(
        case(
            "messages_stream_tool_call",
            "messages 流式工具调用：content_block_start/delta/stop 与 input_json_delta",
            SINGLE,
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":16,"stream":true,"tools":[{"name":"f","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/chat/completions": [
                    sse_chunks(
                        'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\\"a\\":"}}]}}]}\n\n',
                        'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}\n\n',
                        "data: [DONE]\n\n",
                    )
                ]
            },
        )
    )
    add(
        case(
            "messages_stream_empty",
            "上游无内容事件 ⇒ 补一对空文本块 + end_turn",
            SINGLE,
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}',
            },
            {"/v1/chat/completions": [sse_chunks("data: [DONE]\n\n")]},
        )
    )
    add(
        case(
            "responses_nonstream",
            "responses 非流式：转 Responses 形态",
            SINGLE,
            {
                "path": "/v1/responses",
                "body": '{"model":"vendor-model","input":"hi"}',
            },
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "responses_stream",
            "responses 流式：重建 response.* 事件",
            SINGLE,
            {
                "path": "/v1/responses",
                "body": '{"model":"vendor-model","input":"hi","stream":true}',
            },
            {
                "/v1/chat/completions": [
                    sse_chunks(
                        'data: {"choices":[{"delta":{"content":"hey"}}]}\n\n',
                        "data: [DONE]\n\n",
                    )
                ]
            },
        )
    )
    add(
        case(
            "embeddings_nonstream",
            "embeddings：input 不被改写成 messages，只替换 model",
            SINGLE,
            {"path": "/v1/embeddings", "body": '{"model":"vendor-model","input":"hi"}'},
            {
                "/v1/embeddings": [
                    {
                        "status": 200,
                        "headers": {"content-type": "application/json"},
                        "body": '{"object":"list","data":[]}',
                    }
                ]
            },
        )
    )
    add(
        case(
            "images_generations",
            "images/generations 走配置的 images 路径",
            SINGLE,
            {"path": "/v1/images/generations", "body": '{"model":"vendor-model","prompt":"x"}'},
            {
                "/v1/images/generations": [
                    {
                        "status": 200,
                        "headers": {"content-type": "application/json"},
                        "body": '{"created":1,"data":[]}',
                    }
                ]
            },
        )
    )
    add(
        case(
            "images_edits_nonjson",
            "images/edits：参照实现把 multipart 当 JSON ⇒ 400 缺少 model",
            SINGLE,
            {
                "path": "/v1/images/edits",
                "headers": {"content-type": "multipart/form-data; boundary=xyz"},
                "body": "--xyz\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nvendor-model\r\n--xyz--\r\n",
            },
            {},
        )
    )
    add(
        case(
            "unsupported_path",
            "未知 v1 子路径 ⇒ 按 v1/<path> 转发",
            SINGLE,
            {"path": "/v1/audio/transcriptions", "body": '{"model":"vendor-model"}'},
            {
                "/v1/audio/transcriptions": [
                    {
                        "status": 200,
                        "headers": {"content-type": "application/json"},
                        "body": '{"ok":true}',
                    }
                ]
            },
        )
    )

    # --- 重试与冷却 -----------------------------------------------------------
    add(
        case(
            "retry_500_then_ok",
            "多 key：首个 500 可重试 ⇒ 换 key 后成功",
            {**MULTI, "max_retries": 1},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_error(500), chat_ok()]},
        )
    )
    add(
        case(
            "retry_401_then_ok",
            "401 **可重试**（会换 key）",
            {**MULTI, "max_retries": 1},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_error(401, "bad key"), chat_ok()]},
        )
    )
    add(
        case(
            "retry_403_then_ok",
            "403 同样可重试",
            {**MULTI, "max_retries": 1},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_error(403, "forbidden"), chat_ok()]},
        )
    )
    add(
        case(
            "retry_429_cooldown",
            "429 立即冷却并带 retry-after；换 key 后成功",
            {**MULTI, "max_retries": 1},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    {
                        "status": 429,
                        "headers": {
                            "content-type": "application/json",
                            "retry-after": "7",
                        },
                        "body": '{"error":{"message":"slow down"}}',
                    },
                    chat_ok(),
                ]
            },
        )
    )
    add(
        case(
            "retry_400_no_retry",
            "400 不可重试 ⇒ 直接返回上游错误体",
            {**MULTI, "max_retries": 1},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_error(400, "bad request")]},
        )
    )
    add(
        case(
            "retry_521_structured",
            "521 非 JSON 体 ⇒ 结构化 Cloudflare 错误",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    {
                        "status": 521,
                        "headers": {"content-type": "text/html"},
                        "body": "<html>down</html>",
                    }
                ]
            },
        )
    )
    add(
        case(
            "retry_sole_key_500",
            "单 key：attempts = max_retries+1，同一个 key 重试",
            {**SINGLE, "max_retries": 2},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_error(500), chat_error(500), chat_ok()]},
        )
    )
    add(
        case(
            "retry_only_first_mode",
            "only_first 路由模式 ⇒ max_retries+1 且不换 key",
            {
                "models": [
                    model_spec(
                        "vendor-model",
                        [key("k1"), key("k2")],
                        routing_mode="only_first",
                    )
                ],
                "max_retries": 1,
            },
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_error(500), chat_ok()]},
        )
    )
    add(
        case(
            "retry_pinned_key",
            "显式指定 key ⇒ max_retries+1 且只用该 key",
            MULTI,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model[k2]","messages":[]}',
            },
            {"/v1/chat/completions": [chat_error(500), chat_ok()]},
        )
    )
    add(
        case(
            "retry_multi_key_count",
            "多 key 无指定 ⇒ attempts = key_count（只轮换一遍）",
            {**MULTI, "max_retries": 5},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    chat_error(500),
                    chat_error(500),
                    chat_error(500),
                ]
            },
        )
    )
    add(
        case(
            "retry_all_keys_fail_stream",
            "流式请求在所有重试都失败后把最后的错误体返回",
            {**MULTI, "max_retries": 1},
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {"/v1/chat/completions": [chat_error(503), chat_error(503), chat_error(503)]},
        )
    )
    add(
        case(
            "tool_error_retry_success",
            "400 且与工具相关 ⇒ 过滤非 function 工具重试一次",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"tools":[{"type":"function","function":{"name":"f"}},{"type":"web_search"}]}',
            },
            {
                "/v1/chat/completions": [
                    chat_error(400, "tools: unknown tool type"),
                    chat_ok(),
                ]
            },
        )
    )
    add(
        case(
            "tool_error_retry_failure",
            "工具重试也失败 ⇒ 返回**原始** 400 错误体",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}',
            },
            {
                "/v1/chat/completions": [
                    chat_error(400, "tool schema invalid"),
                    chat_error(400, "still bad"),
                ]
            },
        )
    )
    add(
        case(
            "tool_error_on_embeddings_not_retried",
            "embeddings 不走 tool 重试路径",
            SINGLE,
            {"path": "/v1/embeddings", "body": '{"model":"vendor-model","input":"hi"}'},
            {"/v1/embeddings": [chat_error(400, "unknown tool function")]},
        )
    )

    # --- 原生端点与回退 -------------------------------------------------------
    add(
        case(
            "native_first_probe_supported",
            "native_first：先探测 /v1/messages，404 判不支持 ⇒ 回退 chat 且缓存负结果",
            {
                "models": [model_spec("vendor-model", [key("k1")], native_first=True)],
            },
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/messages": [
                    {
                        "status": 404,
                        "headers": {"content-type": "application/json"},
                        "body": '{"error":"not found"}',
                    }
                ],
                "/v1/chat/completions": [chat_ok()],
            },
        )
    )
    add(
        case(
            "native_first_probe_401_supported",
            "401 说明端点**存在** ⇒ 直接用原生路径，不转换",
            {
                "models": [model_spec("vendor-model", [key("k1")], native_first=True)],
            },
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/messages": [
                    {
                        "status": 401,
                        "headers": {"content-type": "application/json"},
                        "body": '{"error":"unauthorized"}',
                    },
                    {
                        "status": 200,
                        "headers": {"content-type": "application/json"},
                        "body": '{"type":"message","content":[{"type":"text","text":"native"}]}',
                    },
                ],
            },
        )
    )
    add(
        case(
            "native_route_configured",
            "顶层 upstream_routes 配了 anthropic ⇒ 即使 native_first=false 也走原生",
            {
                "models": [model_spec("vendor-model", [key("k1")])],
                "upstream_routes": {
                    "https://upstream.test": {"anthropic": "v1/messages"}
                },
            },
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/messages": [
                    {
                        "status": 200,
                        "headers": {"content-type": "application/json"},
                        "body": '{"type":"message","content":[]}',
                    }
                ],
            },
        )
    )
    add(
        case(
            "native_fallback_terminal",
            "已缓存支持、但请求时返回 501 ⇒ 回退 chat 并改写缓存",
            {
                "models": [model_spec("vendor-model", [key("k1")], native_first=True)],
            },
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/messages": [
                    {
                        "status": 400,
                        "headers": {"content-type": "application/json"},
                        "body": '{"type":"message","content":[]}',
                    },
                    {
                        "status": 501,
                        "headers": {"content-type": "application/json"},
                        "body": '{"error":"not implemented"}',
                    },
                ],
                "/v1/chat/completions": [chat_ok()],
            },
        )
    )
    add(
        case(
            "responses_native_probe",
            "responses：探测原生 /v1/responses，405 判不支持 ⇒ 回退 chat 转换",
            {
                "models": [model_spec("vendor-model", [key("k1")])],
            },
            {"path": "/v1/responses", "body": '{"model":"vendor-model","input":"hi"}'},
            {
                "/v1/responses": [
                    {
                        "status": 405,
                        "headers": {"content-type": "application/json"},
                        "body": '{"error":"method not allowed"}',
                    }
                ],
                "/v1/chat/completions": [chat_ok()],
            },
        )
    )

    # --- 上游传输失败 ---------------------------------------------------------
    add(
        case(
            "upstream_request_error",
            "上游连接失败 ⇒ 502「上游请求失败: ConnectError」",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [{"error": True}]},
        )
    )
    add(
        case(
            "upstream_request_error_stream",
            "流式请求上游连接失败同样 502（还没开始写下游）",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {"/v1/chat/completions": [{"error": True}]},
        )
    )

    # --- unified-model 与任务备选 ---------------------------------------------
    unified_config = {
        "models": [
            model_spec("primary-model", [key("pk1")], routing_mode="only_first"),
            model_spec("fallback-model", [key("fk1")], routing_mode="only_first"),
        ],
        "unified_model": {
            "default": {
                "primary": {"model": "primary-model"},
                "fallback": {"model": "fallback-model"},
            }
        },
        "max_retries": 1,
    }
    add(
        case(
            "unified_primary_ok",
            "unified 首选成功 ⇒ 无 X-AMKR-Fallback",
            unified_config,
            {"path": "/v1/chat/completions", "body": '{"model":"unified-model","messages":[]}'},
            {"/v1/chat/completions": [chat_ok("primary-model")]},
        )
    )
    add(
        case(
            "unified_fallback_used",
            "unified 首选失败 ⇒ 切备选并补 X-AMKR-Fallback: true",
            unified_config,
            {"path": "/v1/chat/completions", "body": '{"model":"unified-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    chat_error(500),
                    chat_error(500),
                    chat_ok("fallback-model"),
                ]
            },
        )
    )
    add(
        case(
            "unified_fallback_missing",
            "unified 无备选 ⇒ 直接返回首选错误",
            {
                "models": [model_spec("primary-model", [key("pk1")])],
                "unified_model": {"default": {"primary": {"model": "primary-model"}}},
                "max_retries": 1,
            },
            {"path": "/v1/chat/completions", "body": '{"model":"unified-model","messages":[]}'},
            {"/v1/chat/completions": [chat_error(500), chat_error(500)]},
        )
    )
    task_config = {
        "models": [
            model_spec("task-model", [key("tk1")]),
            model_spec("task-fallback", [key("tf1")]),
        ],
        "tasks": [
            {
                "name": "TASK_1",
                "model": "task-model",
                "fallback_model": "task-fallback",
                "params": {"temperature": 0.5, "stop": ["x"]},
            }
        ],
        "max_retries": 1,
    }
    add(
        case(
            "task_ok",
            "任务名路由：模型与采样参数由任务固定",
            task_config,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"TASK_1","messages":[{"role":"user","content":"hi"}]}',
            },
            {"/v1/chat/completions": [chat_ok("task-model")]},
        )
    )
    add(
        case(
            "task_conflict",
            "任务已固定参数 temperature ⇒ 400 并列出冲突参数",
            task_config,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"TASK_1","temperature":0.9,"messages":[]}',
            },
            {},
        )
    )
    add(
        case(
            "task_key_not_allowed",
            "任务不允许指定 Key ⇒ 400",
            task_config,
            {"path": "/v1/chat/completions", "body": '{"model":"TASK_1[x]","messages":[]}'},
            {},
        )
    )
    add(
        case(
            "task_fallback_used",
            "任务首选失败 ⇒ 切任务备选模型",
            task_config,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"TASK_1","messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/chat/completions": [
                    chat_error(500),
                    chat_error(500),
                    chat_ok("task-fallback"),
                ]
            },
        )
    )

    # --- 请求头与响应头 -------------------------------------------------------
    add(
        case(
            "headers_forwarding",
            "头部净化：剔除 authorization/host/x-api-key 等，强制 Bearer + identity",
            SINGLE,
            {
                "path": "/v1/chat/completions?x=1&y=%20z",
                "headers": {
                    "authorization": "Bearer local-key",
                    "x-api-key": "downstream-key",
                    "anthropic-version": "2024-01-01",
                    "cookie": "a=b",
                    "x-custom": "keep-me",
                },
                "body": '{"model":"vendor-model","messages":[]}',
            },
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "response_headers_filtered",
            "响应头剔除 content-encoding/content-length/transfer-encoding/connection",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    {
                        "status": 200,
                        "headers": {
                            "content-type": "application/json",
                            "transfer-encoding": "chunked",
                            "connection": "keep-alive",
                            "x-upstream": "yes",
                        },
                        "body": '{"ok":true}',
                    }
                ]
            },
        )
    )
    add(
        case(
            "response_headers_no_content_type",
            "上游没给 content-type ⇒ 下游不带 content-type",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [{"status": 200, "body": '{"ok":true}'}]},
        )
    )
    add(
        case(
            "error_non_json_body",
            "上游错误体非 JSON ⇒ message 用宽容解码的原文",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    {
                        "status": 503,
                        "headers": {"content-type": "text/plain"},
                        "body": "bad gateway",
                    }
                ]
            },
        )
    )
    add(
        case(
            "error_empty_body_anthropic",
            "messages 错误体为空 ⇒ Anthropic 信封 + 兜底文案",
            SINGLE,
            {"path": "/v1/messages", "body": '{"model":"vendor-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    {"status": 502, "headers": {"content-type": "text/plain"}, "body": ""}
                ]
            },
        )
    )
    add(
        case(
            "anthropic_non_json_upstream",
            "messages 成功但上游非 JSON ⇒ 502 无法转换",
            SINGLE,
            {"path": "/v1/messages", "body": '{"model":"vendor-model","messages":[]}'},
            {
                "/v1/chat/completions": [
                    {
                        "status": 200,
                        "headers": {"content-type": "text/plain"},
                        "body": "not json",
                    }
                ]
            },
        )
    )
    add(
        case(
            "responses_non_json_upstream",
            "responses 成功但上游非 JSON ⇒ 502 无法转换",
            SINGLE,
            {"path": "/v1/responses", "body": '{"model":"vendor-model","input":"hi"}'},
            {
                "/v1/chat/completions": [
                    {
                        "status": 200,
                        "headers": {"content-type": "text/plain"},
                        "body": "not json",
                    }
                ]
            },
        )
    )

    # --- 访问者与 hidden alias ----------------------------------------------
    add(
        case(
            "visitor_allowed",
            "访客可访问标记 allow_visitor 的 key（模型名带 amkr- 前缀）",
            {
                "models": [
                    model_spec(
                        "vendor-model",
                        [key("k1", allow_visitor=True)],
                    )
                ]
            },
            {
                "path": "/v1/chat/completions",
                "headers": {"authorization": "Bearer amkr-visitor"},
                "body": '{"model":"amkr-vendor-model","messages":[]}',
            },
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "visitor_denied",
            "访客用真实模型 ID ⇒ 403（访客路由表只认 amkr- 前缀）",
            {
                "models": [
                    model_spec(
                        "vendor-model",
                        [key("k1", allow_visitor=True)],
                    )
                ]
            },
            {
                "path": "/v1/chat/completions",
                "headers": {"authorization": "Bearer amkr-visitor"},
                "body": '{"model":"vendor-model","messages":[]}',
            },
            {},
        )
    )
    add(
        case(
            "visitor_no_visitor_key",
            "模型存在但无允许访客的 key ⇒ 403",
            {"models": [model_spec("vendor-model", [key("k1")])]},
            {
                "path": "/v1/chat/completions",
                "headers": {"authorization": "Bearer amkr-visitor"},
                "body": '{"model":"amkr-vendor-model","messages":[]}',
            },
            {},
        )
    )
    add(
        case(
            "hidden_alias",
            "hidden_alias 可直接调用但不出现在 /v1/models",
            {
                "models": [
                    model_spec("vendor-model", [key("k1")], hidden_aliases=["secret-model"])
                ]
            },
            {"path": "/v1/chat/completions", "body": '{"model":"secret-model","messages":[]}'},
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "upstream_model_override",
            "key.upstream_model 覆盖上游收到的 model 字段",
            {"models": [model_spec("vendor-model", [key("k1", upstream_model="real-vendor")])]},
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_ok()]},
        )
    )

    # --- 上游请求体：任务参数、reasoning_effort、stream_options ----------------
    add(
        case(
            "body_task_params_applied",
            "任务固定参数盖上调用方 body（reasoning_effort 除外）",
            {
                "models": [model_spec("task-model", [key("tk1")])],
                "tasks": [
                    {
                        "name": "TASK_P",
                        "model": "task-model",
                        "params": {"temperature": 0.2, "reasoning_effort": "high"},
                    }
                ],
            },
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"TASK_P","messages":[{"role":"user","content":"hi"}]}',
            },
            {"/v1/chat/completions": [chat_ok("task-model")]},
        )
    )
    add(
        case(
            "body_stream_options_injected",
            "流式且非原生 ⇒ 注入 stream_options.include_usage",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {"/v1/chat/completions": [sse_chunks("data: [DONE]\n\n")]},
        )
    )
    add(
        case(
            "body_reasoning_effort_model_level",
            "模型级 reasoning_effort 覆盖载荷里的值",
            {
                "models": [
                    model_spec("vendor-model", [key("k1")], reasoning_effort="low")
                ]
            },
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"reasoning_effort":"high"}',
            },
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "body_passthrough_no_model",
            "payload 无 model 键 ⇒ 字节级透传（但请求体内有别的 model 时不会走到这里）",
            SINGLE,
            {"path": "/v1/chat/completions", "body": '{"model":"vendor-model","messages":[]}'},
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "body_responses_input_adapt",
            "Responses 的 input 被改写成 chat 的 messages",
            SINGLE,
            {"path": "/v1/responses", "body": '{"model":"vendor-model","input":"hello"}'},
            {"/v1/chat/completions": [chat_ok()]},
        )
    )
    add(
        case(
            "body_anthropic_messages_adapt",
            "Anthropic 的 messages/system 被改写成 chat 形态",
            SINGLE,
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":16,"system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}',
            },
            {"/v1/chat/completions": [chat_ok()]},
        )
    )

    # --- 流式错误与提前结束 ---------------------------------------------------
    add(
        case(
            "stream_upstream_empty_200",
            "上游 200 但一个字节都没有 ⇒ 下游也一个字节都没有",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {
                "/v1/chat/completions": [
                    {
                        "status": 200,
                        "headers": {"content-type": "text/event-stream"},
                        "chunks": [],
                    }
                ]
            },
        )
    )
    add(
        case(
            "chat_stream_mid_error",
            "原样转发流：中途断开时已收到的完整事件仍然外发，不重试、不发错误帧",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {
                "/v1/chat/completions": [
                    sse_step_with_break(
                        ['data: {"i":1}\n\n', 'data: {"i":2}\n\n'], break_after=1
                    )
                ]
            },
        )
    )
    add(
        case(
            "chat_stream_mid_error_with_residual",
            "原样转发流：中途断开时**未完成**的残余也会外发（参照实现的 except 分支）",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {
                "/v1/chat/completions": [
                    sse_step_with_break(
                        ['data: {"i":1}\n\n', "data: partial", "unused"],
                        break_after=2,
                    )
                ]
            },
        )
    )
    add(
        case(
            "messages_stream_mid_error",
            "Anthropic 重建流：中途断开时不补 content_block_stop / message_delta / message_stop",
            SINGLE,
            {
                "path": "/v1/messages",
                "body": '{"model":"vendor-model","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}',
            },
            {
                "/v1/chat/completions": [
                    sse_step_with_break(
                        [
                            'data: {"choices":[{"delta":{"content":"hi"}}]}\n\n',
                            'data: {"choices":[{"delta":{"content":"!"}}]}\n\n',
                        ],
                        break_after=1,
                    )
                ]
            },
        )
    )
    add(
        case(
            "responses_stream_mid_error",
            "Responses 重建流：中途断开时不补 output_item.done / response.completed",
            SINGLE,
            {
                "path": "/v1/responses",
                "body": '{"model":"vendor-model","input":"hi","stream":true}',
            },
            {
                # responses 路径总是先探测原生端点；405 判不支持 ⇒ 回退 chat。
                "/v1/responses": [{"status": 405, "headers": {"content-type": "application/json"}, "body": '{"error":"method not allowed"}'}],
                "/v1/chat/completions": [
                    sse_step_with_break(
                        [
                            'data: {"choices":[{"delta":{"content":"hi"}}]}\n\n',
                            'data: {"choices":[{"delta":{"content":"!"}}]}\n\n',
                        ],
                        break_after=1,
                    )
                ],
            },
        )
    )
    add(
        case(
            "stream_status_400",
            "流式请求但上游 400 ⇒ 缓冲成 JSON 错误（不进入 SSE 路径）",
            SINGLE,
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {"/v1/chat/completions": [chat_error(400, "bad")]},
        )
    )
    add(
        case(
            "stream_status_500_after_retries",
            "流式请求所有重试耗尽 ⇒ 返回错误 JSON",
            {**MULTI, "max_retries": 1},
            {
                "path": "/v1/chat/completions",
                "body": '{"model":"vendor-model","messages":[],"stream":true}',
            },
            {"/v1/chat/completions": [chat_error(500), chat_error(500), chat_error(500)]},
        )
    )

    return cases


async def build_corpus() -> list[dict[str, Any]]:
    records = []
    for case_data in build_cases():
        with tempfile.TemporaryDirectory() as raw_tmp:
            record = await run_case(case_data, Path(raw_tmp))
        records.append(record)
    return records


def serialize(records: list[dict[str, Any]]) -> str:
    lines = [
        # **不排序**：任务固定参数的键顺序是上游请求体的可观测契约，sort_keys=True
        # 会把它按字典序重排，从而把「Go 侧顺序错了」伪装成通过。
        # 确定性来自生成器按固定顺序构造字典，不依赖排序。
        json.dumps(record, ensure_ascii=False, separators=(",", ":"))
        for record in records
    ]
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="只校验语料是否最新")
    arguments = parser.parse_args()

    records = asyncio.run(build_corpus())
    text = serialize(records)

    if arguments.check:
        if not OUTPUT.exists():
            print(f"语料缺失: {OUTPUT}", file=sys.stderr)
            return 1
        current = OUTPUT.read_text(encoding="utf-8")
        if current != text:
            # 逐行找出第一条不一致，便于定位。
            current_lines = current.splitlines()
            fresh_lines = text.splitlines()
            for index, (left, right) in enumerate(zip(current_lines, fresh_lines)):
                if left != right:
                    print(f"第 {index + 1} 行不一致:\n  已存: {left[:400]}\n  新算: {right[:400]}",
                          file=sys.stderr)
                    break
            else:
                print(f"行数不同：已存 {len(current_lines)}，新算 {len(fresh_lines)}",
                      file=sys.stderr)
            print(
                f"语料已过期: {OUTPUT}\n"
                "请运行 python scripts/gen_proxy_handler_corpus.py 重新生成。",
                file=sys.stderr,
            )
            return 1
        print(f"语料最新: {OUTPUT.name}（{len(records)} 条）")
        return 0

    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    # newline="\n" 保证在 Windows 上也是 LF，避免 CRLF 让逐字节校验随平台漂移。
    with OUTPUT.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
    print(f"已写入 {OUTPUT}（{len(records)} 条）")
    return 0


if __name__ == "__main__":
    with contextlib.suppress(BrokenPipeError):
        raise SystemExit(main())
