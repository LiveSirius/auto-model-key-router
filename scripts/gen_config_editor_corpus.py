#!/usr/bin/env python3
"""生成 internal/configeditor 的对拍语料。

语料来自**真实 Python 实现**（auto_model_key_router/config_editor.py），Go 测试只读它、
不需要 Python 解释器。`--check` 是 CI 的新鲜度闸门：语料与当前实现不一致时退出码 1，
防止有人改了 Python 却忘了重新生成。

用法：
    python scripts/gen_config_editor_corpus.py            # 生成
    python scripts/gen_config_editor_corpus.py --check    # 只校验

脚本化桩与 Python 测试里的 monkeypatch 等价：

  * ``config_editor.httpx.Client`` 换成带脚本化 transport 的工厂，逐条记录
    「方法 / URL / Authorization / 请求体（解析后）」——请求构造本身就是被对拍的语义；
  * ``config_editor.monotonic`` 换成固定步进的计数器，让 ``duration_ms`` 可复现；
  * ``config_editor.datetime`` 换成固定时刻，让 ``checked_at`` 可复现；
  * ``config_editor.time`` 换成固定秒数，让 ``expires_in_seconds`` 可复现。

脚本化的「网络错误」步骤带一个固定消息，Python 侧抛 ``httpx.ReadError(msg)``、
Go 侧注入等价的 ``errors.New(msg)``。两边 ``str(exc)`` 都是该消息，所以错误文本可以
逐字对拍；**真实**网络错误在两边的文本不可能一致（httpx 的 "timed out" vs net/http 的
"context deadline exceeded"），这部分只由 doc.go 记录，不进语料。
"""
from __future__ import annotations

import datetime as _datetime
import json
import shutil
import sys
import tempfile
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parent.parent
OUTPUT = REPO_ROOT / "internal" / "configeditor" / "testdata" / "config_editor_corpus.json"

sys.path.insert(0, str(REPO_ROOT))

import httpx  # noqa: E402

from auto_model_key_router import config_editor as ce  # noqa: E402

# 脚本化时刻：datetime.now(timezone.utc) 固定成这个值。
FIXED_EPOCH = 1_800_000_000.0


class ScriptedClock:
    """固定步进的单调钟：每次调用返回值递增 step 秒。"""

    def __init__(self, start: float = 1000.0, step: float = 0.25) -> None:
        self.value = start
        self.step = step

    def __call__(self) -> float:
        current = self.value
        self.value += self.step
        return current


class FakeDateTime:
    """只提供 config_editor 用到的那一个入口。"""

    @staticmethod
    def now(tz: Any = None) -> _datetime.datetime:
        return _datetime.datetime(2026, 1, 2, 3, 4, 5, tzinfo=tz)


class ScriptedTransport(httpx.BaseTransport):
    """按脚本逐步应答的传输层，并记录收到的每个请求。

    ``steps`` 是**共享列表**：消费后就地弹出，调用方据此断言脚本恰好用尽。
    """

    def __init__(self, steps: list[dict[str, Any]], recorder: list[dict[str, Any]]) -> None:
        self._steps = steps
        self._recorder = recorder

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self._recorder.append(record_request(request))
        if not self._steps:
            raise AssertionError(f"脚本已用尽，但仍有请求: {request.method} {request.url}")
        step = self._steps.pop(0)
        if "error" in step:
            raise make_error(step)
        body = (step.get("body") or "").encode("utf-8")
        return httpx.Response(step["status"], content=body)


def make_error(step: dict[str, Any]) -> httpx.RequestError:
    message = step.get("message", "脚本化失败")
    kind = step["error"]
    if kind == "connect":
        return httpx.ConnectError(message)
    if kind == "timeout":
        return httpx.ReadTimeout(message)
    if kind == "read":
        return httpx.ReadError(message)
    if kind == "protocol":
        return httpx.UnsupportedProtocol(message)
    raise AssertionError(f"未知的脚本化错误: {kind}")


def record_request(request: httpx.Request) -> dict[str, Any]:
    body: Any = None
    if request.content:
        try:
            body = json.loads(request.content.decode("utf-8"))
        except ValueError:
            body = None
    return {
        "method": request.method,
        "url": str(request.url),
        "authorization": request.headers.get("authorization"),
        "body": body,
    }


class HttpHarness:
    """把 config_editor 用到的 httpx.Client 换成脚本化工厂。"""

    steps: list[dict[str, Any]] | None = None

    def __init__(self) -> None:
        self.recorder: list[dict[str, Any]] = []
        self._real_client = httpx.Client
        self._real_get: Any = None

    def __enter__(self) -> "HttpHarness":
        recorder = self.recorder
        real_client = self._real_client
        harness = self

        def factory(**kwargs: Any) -> httpx.Client:
            steps = HttpHarness.steps
            if steps is None:
                raise AssertionError("脚本化 Client 在未装载脚本时被调用")
            kwargs["transport"] = ScriptedTransport(steps, recorder)
            return real_client(**kwargs)

        def get(url: Any, **kwargs: Any) -> httpx.Response:
            steps = HttpHarness.steps
            if steps is None:
                raise AssertionError("脚本化 httpx.get 在未装载脚本时被调用")
            request = httpx.Request("GET", url)
            recorder.append(record_request(request))
            if not steps:
                raise AssertionError(f"脚本已用尽，但仍有请求: GET {url}")
            step = steps.pop(0)
            if "error" in step:
                raise make_error(step)
            body = (step.get("body") or "").encode("utf-8")
            return httpx.Response(step["status"], content=body, request=request)

        httpx.Client = factory  # type: ignore[assignment]
        self._real_get = httpx.get
        httpx.get = get  # type: ignore[assignment]
        return self

    def __exit__(self, *exc: Any) -> None:
        httpx.Client = self._real_client  # type: ignore[assignment]
        if self._real_get is not None:
            httpx.get = self._real_get  # type: ignore[assignment]


def run_http_case(case: dict[str, Any], action: Any) -> list[dict[str, Any]]:
    """装载脚本、执行 action、返回记录到的请求；脚本有余则报错。"""
    steps: list[dict[str, Any]] = list(case.get("steps", []))
    HttpHarness.steps = steps
    with HttpHarness() as harness:
        case["_result"] = action()
    HttpHarness.steps = None
    if steps:
        raise AssertionError(f"脚本未用尽，剩余 {len(steps)} 步: {case.get('name')}")
    return harness.recorder


# --------------------------------------------------------------------------- #
# 各段语料
# --------------------------------------------------------------------------- #

PROVIDERS: dict[str, dict[str, Any]] = {
    "two_keys": {
        "base_url": "https://vendor.example.test",
        "keys": {"main": {"api_key": "sk-main"}, "other": {"api_key": "sk-other"}},
        "routes": {"openai": "custom/v1"},
    },
    "no_base_url": {"base_url": "", "keys": {}, "routes": {}},
    "missing_key": {"base_url": "https://vendor.example.test", "keys": {}, "routes": {}},
    "empty_api_key": {
        "base_url": "https://vendor.example.test",
        "keys": {"main": {"api_key": ""}},
        "routes": {},
    },
    "no_routes": {
        "base_url": "https://vendor.example.test",
        "keys": {"main": {"api_key": "sk-main"}},
    },
}


def copy(value: Any) -> Any:
    return json.loads(json.dumps(value))


def probe_payload_cases() -> list[dict[str, Any]]:
    cases = []
    for mode in ("openai", "anthropic", "responses", "images", "embeddings", "", "bogus"):
        for model_id in ("gpt-a", "模型-中", ""):
            cases.append(
                {
                    "mode": mode,
                    "model_id": model_id,
                    "payload": ce.probe_payload_for_mode(mode, model_id),
                }
            )
    return cases


ERROR_TEXT_BODIES = [
    "",
    "not json at all",
    "{}",
    "[]",
    '"just a string"',
    '{"error": {"message": "bad key"}}',
    '{"error": {"type": "invalid_request"}}',
    '{"error": {"message": "", "type": "x"}}',
    '{"error": {}}',
    '{"error": {"message": null, "type": null}}',
    '{"error": "boom"}',
    '{"error": 0}',
    '{"error": null}',
    '{"error": false}',
    '{"error": ["a", "b"]}',
    '{"error": 42}',
    '{"error": {"message": {"nested": 1}}}',
    '{"message": "hello"}',
    '{"message": ""}',
    '{"message": 0}',
    '{"error": "boom", "message": "hello"}',
    '{"detail": "nope"}',
    "x" * 200,
    '{"error": "' + "y" * 200 + '"}',
    '{"message": "' + "中" * 200 + '"}',
    "中文错误体" * 60,
    "{",
]


def probe_error_text_cases() -> list[dict[str, Any]]:
    cases = []
    for body in ERROR_TEXT_BODIES:
        request = httpx.Request("POST", "https://vendor.example.test/v1/chat/completions")
        response = httpx.Response(403, content=body.encode("utf-8"), request=request)
        cases.append({"body": body, "expected": ce._probe_error_text(response)})
    return cases


DISCOVER_SPECS: list[dict[str, Any]] = [
    {
        "name": "sorted_ids",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": '{"data": [{"id": "gpt-b"}, {"id": "gpt-a"}]}'}],
    },
    {
        "name": "empty_data_is_success",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": '{"object": "list", "data": []}'}],
    },
    {
        "name": "stringifies_ids",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [
            {
                "status": 200,
                "body": '{"data": [{"id": 42}, {"id": null}, {"id": ["x", "y"]},'
                ' {"id": true}, {"id": 1.5}]}',
            }
        ],
    },
    {
        "name": "trailing_slash_base_url",
        "base_url": "https://vendor.example.test/",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": '{"data": [{"id": "gpt-a"}]}'}],
    },
    {
        "name": "base_url_with_path",
        "base_url": "https://vendor.example.test/openai",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": '{"data": [{"id": "gpt-a"}]}'}],
    },
    {
        "name": "missing_data_key",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": "{}"}],
    },
    {
        "name": "data_not_list",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": '{"data": {}}'}],
    },
    {
        "name": "data_items_not_objects",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": '{"data": ["gpt-a"]}'}],
    },
    {
        "name": "data_item_missing_id",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": '{"data": [{}]}'}],
    },
    {
        "name": "top_level_list",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": "[]"}],
    },
    {
        "name": "invalid_json",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 200, "body": "{"}],
    },
    {
        "name": "unauthorized",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 401, "body": '{"error": "nope"}'}],
    },
    {
        "name": "redirect_is_failure",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 302, "body": ""}],
    },
    {
        "name": "server_error",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"status": 500, "body": "boom"}],
    },
    {
        "name": "network_error",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"error": "connect", "message": "脚本化连接失败"}],
    },
    {
        "name": "timeout",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-main",
        "steps": [{"error": "timeout", "message": "脚本化超时"}],
    },
    {
        "name": "relative_url_is_protocol_error",
        "base_url": "",
        "api_key": "sk-main",
        "steps": [{"error": "protocol", "message": "脚本化协议错误"}],
    },
    {
        "name": "non_ascii_key",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-测试",
        "steps": [],
    },
    {
        "name": "del_key_is_ascii_boundary",
        "base_url": "https://vendor.example.test",
        "api_key": "sk-\u007f",
        "steps": [{"status": 200, "body": '{"data": []}'}],
    },
]


# 「JSON 解析失败: <detail>」里的 detail 是解释器 JSON 解析器的文本（Python 是
# json.JSONDecodeError，Go 是 canonical 的解析错误），两边不可能一致。语料把它折成
# 这个哨兵，Go 测试退化成「前缀一致 + detail 非空 + 总长不超过 160 字符」。
DETAIL_SENTINEL = "<detail>"


def normalize_discover_error(error: str | None) -> str | None:
    if error is not None and error.startswith("JSON 解析失败: "):
        return "JSON 解析失败: " + DETAIL_SENTINEL
    return error


def discover_models_cases() -> list[dict[str, Any]]:
    cases = []
    for spec in DISCOVER_SPECS:
        requests = run_http_case(
            spec,
            lambda spec=spec: ce.discover_upstream_models_result(
                spec["base_url"], spec["api_key"], set(), timeout=15.0
            ),
        )
        models, error = spec.pop("_result")
        cases.append(
            {
                "name": spec["name"],
                "base_url": spec["base_url"],
                "api_key": spec["api_key"],
                "steps": spec["steps"],
                "requests": requests,
                "models": models,
                "error": normalize_discover_error(error),
            }
        )
    return cases


AVAILABILITY_KEY = {
    "name": "main",
    "api_key": "sk-main",
    "base_url": "https://vendor.example.test",
}


def availability_specs() -> list[dict[str, Any]]:
    ok = {"status": 200, "body": "{}"}
    return [
        {
            "name": "default_modes_with_response_override",
            "data": {
                "upstream_routes": {
                    "https://vendor.example.test": {"responses": "custom/responses"}
                }
            },
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": None,
            "steps": [ok, ok, ok],
        },
        {
            "name": "no_routes_uses_default_paths",
            "data": {},
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": None,
            "steps": [ok, ok, ok],
        },
        {
            "name": "trailing_slash_routes_key",
            "data": {
                "upstream_routes": {
                    "https://vendor.example.test/": {"openai": "custom/chat"}
                }
            },
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": None,
            "steps": [ok, ok, ok],
        },
        {
            "name": "only_responses_mode",
            "data": {},
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": ["responses"],
            "steps": [ok],
        },
        {
            "name": "images_mode_filtered_out",
            "data": {},
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": ["images", "embeddings"],
            "steps": [],
        },
        {
            "name": "empty_modes_means_default",
            "data": {},
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": [],
            "steps": [ok, ok, ok],
        },
        {
            "name": "duplicate_modes_are_kept",
            "data": {},
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": ["openai", "openai"],
            "steps": [ok, ok],
        },
        {
            "name": "key_without_name_uses_model_id",
            "data": {},
            "model_id": "model-a",
            "key": {"api_key": "sk-main", "base_url": "https://vendor.example.test"},
            "modes": ["openai"],
            "steps": [{"status": 403, "body": '{"error": {"message": "bad key"}}'}],
        },
        {
            "name": "key_falls_back_to_default_base_url",
            "data": {"default_base_url": "https://default.example.test"},
            "model_id": "model-a",
            "key": {"name": "main", "api_key": "sk-main"},
            "modes": ["anthropic"],
            "steps": [{"status": 500, "body": "not json"}],
        },
        {
            "name": "no_base_url_anywhere",
            "data": {},
            "model_id": "model-a",
            "key": {"name": "main", "api_key": "sk-main"},
            "modes": ["openai"],
            "steps": [ok],
        },
        {
            "name": "http_error_without_body",
            "data": {},
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": ["openai"],
            "steps": [{"status": 401, "body": ""}],
        },
        {
            "name": "network_error_keeps_status_none",
            "data": {},
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": ["openai", "anthropic"],
            "steps": [{"error": "read", "message": "脚本化读取失败"}, ok],
        },
        {
            "name": "non_ascii_key_short_circuits",
            "data": {},
            "model_id": "model-a",
            "key": {
                "name": "main",
                "api_key": "sk-测试",
                "base_url": "https://vendor.example.test",
            },
            "modes": None,
            "steps": [],
        },
        {
            "name": "legacy_per_key_routes_merge",
            "data": {
                "default_base_url": "https://vendor.example.test",
                "models": [
                    {
                        "id": "model-a",
                        "keys": [
                            {
                                "name": "main",
                                "api_key": "sk-main",
                                "upstream_routes": {"openai": "legacy/chat"},
                            }
                        ],
                    }
                ],
            },
            "model_id": "model-a",
            "key": AVAILABILITY_KEY,
            "modes": ["openai"],
            "steps": [ok],
        },
    ]


def availability_cases() -> list[dict[str, Any]]:
    cases = []
    for spec in availability_specs():
        requests = run_http_case(
            spec,
            lambda spec=spec: ce.probe_key_availability(
                spec["data"],
                spec["model_id"],
                spec["key"],
                timeout=15.0,
                modes=spec["modes"],
            ),
        )
        results = spec.pop("_result")
        cases.append(
            {
                "name": spec["name"],
                "data": spec["data"],
                "model_id": spec["model_id"],
                "key": spec["key"],
                "modes": spec["modes"],
                "steps": spec["steps"],
                "requests": requests,
                "results": [
                    {
                        "model_id": result.model_id,
                        "key_name": result.key_name,
                        "mode": result.mode,
                        "label": result.label,
                        "path": result.path,
                        "url": result.url,
                        "available": result.available,
                        "status_code": result.status_code,
                        "duration_ms": result.duration_ms,
                        "error": result.error,
                    }
                    for result in results
                ],
            }
        )
    return cases


def capability_specs() -> list[dict[str, Any]]:
    models_ok = {"status": 200, "body": '{"data": [{"id": "gpt-b"}, {"id": "gpt-a"}]}'}
    one = {"status": 200, "body": "{}"}
    return [
        {
            "name": "discovers_models_and_route_status",
            "provider": "two_keys",
            "key_name": "main",
            "modes": None,
            "steps": [models_ok, one, one, one],
        },
        {
            "name": "restricts_modes_but_keeps_discovery_error",
            "provider": "two_keys",
            "key_name": "main",
            "modes": ["openai", "responses"],
            "steps": [{"status": 401, "body": ""}],
        },
        {
            "name": "mode_restriction_only_checks_selected_modes",
            "provider": "no_routes",
            "key_name": "main",
            "modes": ["responses"],
            "steps": [models_ok, one],
        },
        {
            "name": "images_mode_passes_filter_but_probes_nothing",
            "provider": "no_routes",
            "key_name": "main",
            "modes": ["images"],
            "steps": [models_ok],
        },
        {
            "name": "empty_modes_probe_everything",
            "provider": "no_routes",
            "key_name": "main",
            "modes": [],
            "steps": [models_ok, one, one, one],
        },
        {
            "name": "failed_route_status_uses_status_code",
            "provider": "no_routes",
            "key_name": "main",
            "modes": ["openai"],
            "steps": [models_ok, {"status": 403, "body": ""}],
        },
        {
            "name": "failed_route_status_uses_error_text",
            "provider": "no_routes",
            "key_name": "main",
            "modes": ["openai"],
            "steps": [models_ok, {"status": 403, "body": '{"error": "bad key"}'}],
        },
        {
            "name": "network_error_route_status_has_no_status_code",
            "provider": "no_routes",
            "key_name": "main",
            "modes": ["openai"],
            "steps": [models_ok, {"error": "connect", "message": "脚本化连接失败"}],
        },
        {
            "name": "missing_base_url",
            "provider": "no_base_url",
            "key_name": "main",
            "modes": None,
            "steps": [],
        },
        {
            "name": "missing_key",
            "provider": "missing_key",
            "key_name": "main",
            "modes": None,
            "steps": [],
        },
        {
            "name": "empty_api_key",
            "provider": "empty_api_key",
            "key_name": "main",
            "modes": None,
            "steps": [],
        },
        {
            "name": "discovery_failure_skips_route_probe",
            "provider": "no_routes",
            "key_name": "main",
            "modes": None,
            "steps": [{"status": 401, "body": ""}],
        },
        {
            "name": "empty_model_list_probes_placeholder_model",
            "provider": "no_routes",
            "key_name": "main",
            "modes": ["openai"],
            "steps": [{"status": 200, "body": '{"data": []}'}, one],
        },
        {
            "name": "custom_provider_routes_are_used",
            "provider": "two_keys",
            "key_name": "other",
            "modes": ["openai"],
            "steps": [models_ok, one],
        },
    ]


def capability_cases() -> list[dict[str, Any]]:
    cases = []
    for spec in capability_specs():
        provider = copy(PROVIDERS[spec["provider"]])
        requests = run_http_case(
            spec,
            lambda spec=spec, provider=provider: ce.probe_key_capability(
                provider, spec["key_name"], modes=spec["modes"], timeout=15.0
            ),
        )
        capabilities = spec.pop("_result")
        cases.append(
            {
                "name": spec["name"],
                "provider": provider,
                "key_name": spec["key_name"],
                "modes": spec["modes"],
                "steps": spec["steps"],
                "requests": requests,
                "capabilities": capabilities,
            }
        )
    return cases


def provider_capabilities_specs() -> list[dict[str, Any]]:
    return [
        {
            "name": "records_models_and_errors",
            "provider": {
                "base_url": "https://gateway.example.test",
                "keys": {
                    "k1": {"api_key": "sk-1"},
                    "k2": {"api_key": "sk-2"},
                    "k3": {"api_key": "sk-3"},
                },
            },
            "key_names": ["k1", "k2", "k3"],
            "steps": [
                {"status": 200, "body": '{"data": [{"id": "gpt-a"}, {"id": "gpt-b"}]}'},
                {"status": 200, "body": '{"data": [{"id": "gpt-a"}, {"id": "gpt-c"}]}'},
                {"status": 401, "body": ""},
            ],
        },
        {
            "name": "empty_key_list",
            "provider": {"base_url": "https://gateway.example.test", "keys": {}},
            "key_names": [],
            "steps": [],
        },
        {
            "name": "missing_base_url_and_missing_keys",
            "provider": {"base_url": "", "keys": {"k1": {"api_key": "sk-1"}, "k2": {}}},
            "key_names": ["k1", "missing", "k2"],
            "steps": [],
        },
        {
            "name": "one_success_only",
            "provider": {
                "base_url": "https://gateway.example.test",
                "keys": {"k1": {"api_key": "sk-1"}, "k2": {"api_key": "sk-2"}},
            },
            "key_names": ["k1", "k2"],
            "steps": [
                {"status": 200, "body": '{"data": [{"id": "b"}, {"id": "a"}]}'},
                {"error": "connect", "message": "脚本化连接失败"},
            ],
        },
        {
            "name": "duplicate_key_names",
            "provider": {
                "base_url": "https://gateway.example.test",
                "keys": {"k1": {"api_key": "sk-1"}},
            },
            "key_names": ["k1", "k1"],
            "steps": [
                {"status": 200, "body": '{"data": [{"id": "gpt-a"}]}'},
                {"status": 200, "body": '{"data": [{"id": "gpt-b"}]}'},
            ],
        },
        {
            "name": "key_order_follows_argument_order",
            "provider": {
                "base_url": "https://gateway.example.test",
                "keys": {
                    "z": {"api_key": "sk-z"},
                    "a": {"api_key": "sk-a"},
                    "m": {"api_key": "sk-m"},
                },
            },
            "key_names": ["z", "m", "a"],
            "steps": [
                {"status": 200, "body": '{"data": [{"id": "z1"}]}'},
                {"status": 200, "body": '{"data": [{"id": "m1"}]}'},
                {"status": 200, "body": '{"data": [{"id": "a1"}]}'},
            ],
        },
    ]


def provider_capabilities_cases() -> list[dict[str, Any]]:
    cases = []
    for spec in provider_capabilities_specs():
        provider = copy(spec["provider"])
        requests = run_http_case(
            spec,
            lambda spec=spec, provider=provider: ce.probe_provider_key_capabilities(
                provider, spec["key_names"], timeout=15.0
            ),
        )
        result = spec.pop("_result")
        cases.append(
            {
                "name": spec["name"],
                "provider": provider,
                "key_names": spec["key_names"],
                "steps": spec["steps"],
                "requests": requests,
                "result": result,
            }
        )
    return cases


def upstream_routes_specs() -> list[dict[str, Any]]:
    return [
        {
            "name": "grouped_routes_match",
            "data": {"upstream_routes": {"https://vendor.example.test": {"openai": "a/v1"}}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "normalizes_trailing_slash",
            "data": {"upstream_routes": {"https://vendor.example.test/": {"openai": "a/v1"}}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "no_match",
            "data": {"upstream_routes": {"https://other.example.test": {"openai": "a/v1"}}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "non_dict_routes_skipped",
            "data": {"upstream_routes": {"https://vendor.example.test": ["a"]}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "missing_upstream_routes",
            "data": {"providers": {}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "upstream_routes_not_object",
            "data": {"upstream_routes": 5},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "legacy_per_key_routes",
            "data": {
                "default_base_url": "https://vendor.example.test",
                "models": [
                    {
                        "keys": [
                            {
                                "api_key": "k",
                                "upstream_routes": {"anthropic": "legacy/messages"},
                            }
                        ]
                    }
                ],
            },
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "legacy_overrides_grouped_and_keeps_position",
            "data": {
                "upstream_routes": {
                    "https://vendor.example.test": {
                        "openai": "grouped",
                        "anthropic": "grouped-m",
                    }
                },
                "models": [
                    {
                        "keys": [
                            {
                                "api_key": "k",
                                "base_url": "https://vendor.example.test",
                                "upstream_routes": {
                                    "openai": "legacy",
                                    "images": "legacy-img",
                                },
                            }
                        ]
                    }
                ],
            },
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "key_base_url_falls_back_to_openai_default",
            "data": {
                "models": [{"keys": [{"api_key": "k", "upstream_routes": {"openai": "x"}}]}]
            },
            "base_url": "https://api.openai.com",
        },
        {
            "name": "non_string_route_value",
            "data": {"upstream_routes": {"https://vendor.example.test": {"openai": 42}}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "empty_route_value_stays_in_the_map",
            "data": {"upstream_routes": {"https://vendor.example.test": {"openai": ""}}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "blank_grouped_url_key_raises",
            "data": {"upstream_routes": {"  ": {"openai": "a"}}},
            "base_url": "https://vendor.example.test",
        },
        {
            "name": "blank_base_url_raises",
            "data": {"upstream_routes": {"https://vendor.example.test": {"openai": "a"}}},
            "base_url": "   ",
        },
        {
            "name": "key_with_blank_base_url_raises",
            "data": {"models": [{"keys": [{"api_key": "k", "base_url": "  "}]}]},
            "base_url": "https://vendor.example.test",
        },
    ]


def upstream_routes_cases() -> list[dict[str, Any]]:
    cases = []
    for spec in upstream_routes_specs():
        try:
            routes = ce.upstream_routes_for_base_url(spec["data"], spec["base_url"])
        except ValueError as exc:
            cases.append(
                {
                    "name": spec["name"],
                    "data": spec["data"],
                    "base_url": spec["base_url"],
                    "error": True,
                    "message": str(exc),
                }
            )
            continue
        cases.append(
            {
                "name": spec["name"],
                "data": spec["data"],
                "base_url": spec["base_url"],
                "error": False,
                # 逐对记录以同时锁定合并顺序（Python 的 dict 覆盖不移动位置）。
                "pairs": [[key, value] for key, value in routes.items()],
            }
        )
    return cases


def service_management_base_url_cases() -> list[dict[str, Any]]:
    specs = [
        {"name": "defaults", "data": {}},
        {"name": "wildcard_host", "data": {"host": "0.0.0.0", "port": 9000}},
        {"name": "ipv6_wildcard", "data": {"host": "::", "port": 9000}},
        {"name": "ipv6_host", "data": {"host": "::1", "port": 9000}},
        {"name": "bracketed_ipv6_host", "data": {"host": "[::1]", "port": 9000}},
        {"name": "hostname_with_string_port", "data": {"host": "localhost", "port": "7000"}},
        {"name": "empty_host_and_zero_port", "data": {"host": "", "port": 0}},
        {"name": "max_port", "data": {"host": "10.0.0.1", "port": "65535"}},
        {"name": "port_null", "data": {"host": "example.test", "port": None}},
        {"name": "host_as_number", "data": {"host": 1234, "port": 8000}},
        {"name": "float_port", "data": {"host": "127.0.0.1", "port": 8123.0}},
        {"name": "nonzero_ipv6_full", "data": {"host": "2001:db8::1", "port": 8000}},
    ]
    cases = []
    for spec in specs:
        try:
            url = ce.service_management_base_url(spec["data"])
            error = None
        except (TypeError, ValueError) as exc:
            url = None
            error = f"{type(exc).__name__}: {exc}"
        cases.append(
            {
                "name": spec["name"],
                "data": spec["data"],
                "url": url,
                "error": error,
            }
        )
    return cases


NATIVE_STATE_VALUES: list[Any] = [
    True,
    False,
    None,
    {},
    {"supported": True},
    {"supported": False},
    {"supported": True, "reason": "probe"},
    {"supported": False, "reason": "fallback", "expires_at": FIXED_EPOCH + 30},
    {"supported": False, "expires_at": FIXED_EPOCH - 30},
    {"supported": False, "ttl_seconds": 60, "checked_at": FIXED_EPOCH - 20},
    {"supported": False, "ttl_seconds": 60},
    {"supported": False, "ttl_seconds": 0, "checked_at": FIXED_EPOCH},
    {"supported": True, "expires_at": "abc"},
    {"supported": "yes"},
    {"supported": True, "expires_in_seconds": 5},
    [1, 2],
    "yes",
    7,
]


def native_endpoint_state_cases() -> list[dict[str, Any]]:
    cases = []
    for value in NATIVE_STATE_VALUES:
        try:
            payload = ce._native_endpoint_state_payload(value)
            error = None
        except (TypeError, ValueError) as exc:
            payload = None
            error = f"{type(exc).__name__}"
        cases.append({"value": value, "payload": payload, "error": error})
    return cases


def format_visitor_status_cases() -> list[dict[str, Any]]:
    cases = []
    for allowed in (True, False):
        for installed in (True, False):
            cases.append(
                {
                    "visitor_allowed": allowed,
                    "visitor_installed": installed,
                    "text": ce.format_visitor_status_text(allowed, installed),
                }
            )
    return cases


NATIVE_STATE_TEXTS: list[Any] = [
    None,
    {},
    {"supported": True, "reason": "probe"},
    {"supported": True},
    {"supported": False, "reason": "fallback", "expires_in_seconds": 30},
    {"supported": False, "reason": "fallback", "expires_in_seconds": 0},
    {"supported": False, "expires_in_seconds": 30},
    {"supported": False, "reason": "", "expires_in_seconds": "30"},
    {"supported": False, "expires_in_seconds": 30.0},
    {"supported": False, "expires_in_seconds": -5},
    {"supported": "yes"},
    [],
]


def native_endpoint_support_text_cases() -> list[dict[str, Any]]:
    return [
        {"state": state, "text": ce.native_endpoint_support_text(state)}
        for state in NATIVE_STATE_TEXTS
    ]


# 从文件读原生端点状态：语料只带**文件内容**，路径由 Go 测试自己造临时文件。
FILE_CASES: list[dict[str, Any]] = [
    {"name": "missing_file", "path_kind": "missing", "content": None},
    {"name": "directory_instead_of_file", "path_kind": "directory", "content": None},
    {"name": "invalid_json", "path_kind": "file", "content": "{"},
    {"name": "empty_object", "path_kind": "file", "content": "{}"},
    {
        "name": "endpoint_capabilities_section",
        "path_kind": "file",
        "content": json.dumps(
            {
                "endpoint_capabilities": {
                    "https://a.example.test/v1/messages": {
                        "supported": True,
                        "reason": "probe",
                    },
                    "https://b.example.test/v1/responses": True,
                    "https://c.example.test/v1/messages": {"supported": "yes"},
                    "https://d.example.test/v1/messages": 5,
                }
            }
        ),
    },
    {
        "name": "legacy_url_native_support_section",
        "path_kind": "file",
        "content": json.dumps({"url_native_support": {"https://a.example.test": False}}),
    },
    {
        "name": "endpoint_capabilities_none_does_not_fall_back",
        "path_kind": "file",
        "content": json.dumps(
            {
                "endpoint_capabilities": None,
                "url_native_support": {"https://a.example.test": True},
            }
        ),
    },
    {
        "name": "states_not_an_object",
        "path_kind": "file",
        "content": json.dumps({"endpoint_capabilities": ["a"]}),
    },
    {
        "name": "entry_with_expiry_uses_checked_at_and_ttl",
        "path_kind": "file",
        "content": json.dumps(
            {
                "endpoint_capabilities": {
                    "https://a.example.test": {
                        "supported": False,
                        "ttl_seconds": 60,
                        "checked_at": FIXED_EPOCH - 20,
                    }
                }
            }
        ),
    },
    {
        "name": "bad_expires_at_raises",
        "path_kind": "file",
        "content": json.dumps(
            {"endpoint_capabilities": {"https://a.example.test": {"supported": False, "expires_at": "abc"}}}
        ),
    },
]


def native_endpoint_states_from_file_cases() -> list[dict[str, Any]]:
    cases = []
    directory = Path(tempfile.mkdtemp(prefix="amkr-ce-corpus-"))
    try:
        for spec in FILE_CASES:
            path = directory / "endpoint-capabilities.json"
            if path.exists():
                if path.is_dir():
                    path.rmdir()
                else:
                    path.unlink()
            if spec["path_kind"] == "file":
                path.write_text(spec["content"], encoding="utf-8")
            elif spec["path_kind"] == "directory":
                path.mkdir()
            try:
                states = ce.load_native_endpoint_states_from_file(
                    {"endpoint_capabilities_path": str(path)}
                )
                error = None
            except ValueError:
                states = None
                error = "ValueError"
            cases.append(
                {
                    "name": spec["name"],
                    "path_kind": spec["path_kind"],
                    "content": spec["content"],
                    "expected": states,
                    "error": error,
                }
            )
    finally:
        shutil.rmtree(directory, ignore_errors=True)
    return cases


# --------------------------------------------------------------------------- #
# 交互流程：脚本化菜单/输入，记录每次菜单、每次落盘结果
# --------------------------------------------------------------------------- #


class AnswerQueue:
    """按顺序消费脚本化回答；种类不匹配立即失败（否则对拍会静默错位）。"""

    def __init__(self, answers: list[dict[str, Any]]) -> None:
        self.answers = list(answers)
        self.index = 0

    def next(self, kind: str) -> Any:
        if self.index >= len(self.answers):
            raise AssertionError(f"回答已用尽，仍在请求 {kind}")
        answer = self.answers[self.index]
        self.index += 1
        if answer["kind"] != kind:
            raise AssertionError(
                f"回答种类不匹配: 请求 {kind}，脚本给的是 {answer['kind']}"
            )
        return answer["value"]

    def exhausted(self) -> bool:
        return self.index == len(self.answers)


class InteractionRecorder:
    """记录交互流程里所有可比较的观测量。"""

    def __init__(self) -> None:
        self.menus: list[dict[str, Any]] = []
        self.multiples: list[dict[str, Any]] = []
        self.prompts: list[dict[str, Any]] = []
        self.confirms: list[dict[str, Any]] = []
        self.results: list[dict[str, Any]] = []
        self.restarts: list[list[Any]] = []
        self.opened: list[str] = []


class StatusStub:
    def __enter__(self) -> "StatusStub":
        return self

    def __exit__(self, *exc: Any) -> bool:
        return False


class ConsoleStub:
    @staticmethod
    def status(*args: Any, **kwargs: Any) -> StatusStub:
        return StatusStub()


def install_ui(
    recorder: InteractionRecorder, queue: AnswerQueue, press_on_key: str | None = None
) -> dict[str, Any]:
    """把交互原语换成脚本化实现，返回需要恢复的原值。"""
    from rich.text import Text


    saved = {
        name: getattr(ce, name)
        for name in (
            "select_option",
            "select_multiple",
            "prompt_text",
            "confirm_choice",
            "show_result_page",
            "clear_terminal_history",
            "open_config_file",
            "run_submodule",
            "console",
            "restart_service_after_config_change",
            "generate_local_api_key",
            "visitor_feature_available",
        )
    }

    def select_option(title, options, selected=0, content=None, *, on_key=None):
        recorder.menus.append(
            {
                "title": title,
                "options": [[value, label] for value, label in options],
                "selected": selected,
                "on_key": on_key is not None,
            }
        )
        if press_on_key is not None and on_key is not None:
            on_key(press_on_key)
        return queue.next("select")

    def select_multiple(title, options, content=None, *, checked_values=None):
        recorder.multiples.append(
            {"title": title, "options": [[value, label] for value, label in options]}
        )
        return queue.next("multiple")

    def prompt_text(title, prompt, *, default=None, password=False, choices=None):
        recorder.prompts.append(
            {
                "title": title,
                "prompt": prompt,
                "default": default,
                "password": password,
                "choices": list(choices) if choices else None,
            }
        )
        return queue.next("prompt")

    def confirm_choice(message, default=False):
        recorder.confirms.append({"message": message, "default": default})
        return queue.next("confirm")

    def show_result_page(title, content):
        recorder.results.append(
            {
                "title": title,
                "copy_text": getattr(content, "copy_text", None),
                "copy_label": getattr(content, "copy_label", None),
            }
        )

    def open_config_file(path):
        recorder.opened.append(str(path))
        return "已打开"

    def run_submodule(action):
        # 参照实现会把 KeyboardInterrupt/异常收敛成面板；语料场景不触发这两条路径
        # （那是 tui.run_submodule 的职责，已由 tui 语料覆盖），这里直接执行，
        # 避免真实 run_submodule 在出错时弹出阻塞式结果页。
        return action()

    def restart_service_after_config_change(path, old_config, new_config):
        recorder.restarts.append(
            [old_config.host, old_config.port, new_config.host, new_config.port]
        )
        return Text("reloaded")

    ce.select_option = select_option
    ce.select_multiple = select_multiple
    ce.prompt_text = prompt_text
    ce.confirm_choice = confirm_choice
    ce.show_result_page = show_result_page
    ce.clear_terminal_history = lambda: None
    ce.open_config_file = open_config_file
    ce.run_submodule = run_submodule
    ce.console = ConsoleStub()
    ce.restart_service_after_config_change = restart_service_after_config_change
    ce.generate_local_api_key = lambda: FIXED_LOCAL_API_KEY
    ce.visitor_feature_available = lambda: True
    return saved


def restore_ui(saved: dict[str, Any]) -> None:
    for name, value in saved.items():
        setattr(ce, name, value)


FIXED_LOCAL_API_KEY = "amkr-local-fixed"

BASE_CONFIG: dict[str, Any] = {
    "config_version": 4,
    "host": "127.0.0.1",
    "port": 8000,
    "local_api_key": "amkr-local",
    "providers": {
        "vendor": {
            "base_url": "https://vendor.example.test",
            "keys": {"main": {"api_key": "sk-main", "enabled": True}},
        },
        "other": {"base_url": "https://other.example.test", "keys": {}},
    },
    "models": {
        "local-model": {
            "aliases": ["alias-a"],
            "hidden_aliases": ["hidden-a"],
            "routing_mode": "round_robin",
            "targets": [
                {"provider": "vendor", "key": "main", "upstream_model": "upstream-x"}
            ],
        }
    },
}


def empty_config() -> dict[str, Any]:
    return {"config_version": 4, "host": "127.0.0.1", "port": 8000, "providers": {}, "models": {}}


INTERACTIVE_SPECS: list[dict[str, Any]] = [
    {
        "name": "manage_providers_returns_immediately",
        "config": copy(BASE_CONFIG),
        "run": "manage_providers",
        "answers": [{"kind": "select", "value": "0"}],
    },
    {
        "name": "manage_providers_opens_add_provider_key_with_probe",
        "config": copy(BASE_CONFIG),
        "run": "manage_providers",
        "steps": [
            {"status": 200, "body": '{"data": [{"id": "gpt-b"}, {"id": "gpt-a"}]}'},
            {"status": 200, "body": "{}"},
            {"status": 200, "body": "{}"},
            {"status": 200, "body": "{}"},
        ],
        "answers": [
            {"kind": "select", "value": "n"},
            {"kind": "prompt", "value": "fresh"},
            {"kind": "prompt", "value": "https://fresh.example.test/"},
            {"kind": "prompt", "value": "k1"},
            {"kind": "prompt", "value": "sk-fresh"},
            {"kind": "multiple", "value": ["gpt-a", "gpt-b"]},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "add_provider_key_existing_provider_no_models_selected",
        "config": copy(BASE_CONFIG),
        "run": "add_provider_key_existing",
        "provider_id": "vendor",
        "steps": [
            {"status": 200, "body": '{"data": [{"id": "gpt-a"}]}'},
            {"status": 200, "body": "{}"},
            {"status": 200, "body": "{}"},
            {"status": 200, "body": "{}"},
        ],
        "answers": [
            {"kind": "prompt", "value": "second"},
            {"kind": "prompt", "value": "sk-second"},
            {"kind": "multiple", "value": []},
        ],
    },
    {
        "name": "add_provider_key_saves_without_binding_when_manual_input_empty",
        "config": copy(BASE_CONFIG),
        "run": "add_provider_key_existing",
        "provider_id": "vendor",
        "steps": [
            {"status": 200, "body": '{"data": []}'},
            {"status": 200, "body": "{}"},
            {"status": 200, "body": "{}"},
            {"status": 200, "body": "{}"},
        ],
        "answers": [
            {"kind": "prompt", "value": "third"},
            {"kind": "prompt", "value": "sk-third"},
            {"kind": "prompt", "value": "   "},
        ],
    },
    {
        "name": "add_provider_key_rejects_duplicate_key_name",
        "config": copy(BASE_CONFIG),
        "run": "add_provider_key_existing",
        "provider_id": "vendor",
        "answers": [{"kind": "prompt", "value": "main"}],
    },
    {
        "name": "add_provider_key_rejects_empty_api_key",
        "config": copy(BASE_CONFIG),
        "run": "add_provider_key_existing",
        "provider_id": "vendor",
        "answers": [
            {"kind": "prompt", "value": "second"},
            {"kind": "prompt", "value": "   "},
        ],
    },
    {
        "name": "manage_provider_keys_toggle_then_return",
        "config": copy(BASE_CONFIG),
        "run": "manage_provider_keys",
        "provider_id": "vendor",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "manage_provider_keys_rename",
        "config": copy(BASE_CONFIG),
        "run": "manage_provider_keys",
        "provider_id": "vendor",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "2"},
            {"kind": "prompt", "value": "renamed"},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "manage_provider_keys_delete_last_key_confirmed",
        "config": copy(BASE_CONFIG),
        "run": "manage_provider_keys",
        "provider_id": "vendor",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "4"},
            {"kind": "confirm", "value": True},
        ],
    },
    {
        "name": "manage_provider_routes_set_and_clear",
        "config": copy(BASE_CONFIG),
        "run": "manage_provider_routes",
        "provider_id": "vendor",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "prompt", "value": "custom/v1"},
            {"kind": "select", "value": "c"},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "manage_provider_routes_restore_default",
        "config": copy(BASE_CONFIG),
        "run": "manage_provider_routes",
        "provider_id": "vendor",
        "answers": [
            {"kind": "select", "value": "2"},
            {"kind": "prompt", "value": ""},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "refresh_provider_capability_all_keys",
        "config": copy(BASE_CONFIG),
        "run": "refresh_provider_capability",
        "provider_id": "vendor",
        "steps": [
            {"status": 200, "body": '{"data": [{"id": "m1"}, {"id": "m2"}]}'},
            {"status": 200, "body": "{}"},
            {"status": 403, "body": '{"error": "denied"}'},
            {"status": 200, "body": "{}"},
        ],
        "answers": [{"kind": "select", "value": "1"}],
    },
    {
        "name": "refresh_provider_capability_single_key_one_mode",
        "config": copy(BASE_CONFIG),
        "run": "refresh_provider_capability",
        "provider_id": "vendor",
        "steps": [
            {"status": 200, "body": '{"data": [{"id": "m1"}]}'},
            {"status": 200, "body": "{}"},
        ],
        "answers": [
            {"kind": "select", "value": "2"},
            {"kind": "select", "value": "main"},
            {"kind": "select", "value": "2"},
        ],
    },
    {
        "name": "delete_provider_confirmed",
        "config": copy(BASE_CONFIG),
        "run": "delete_provider",
        "provider_id": "vendor",
        "answers": [{"kind": "confirm", "value": True}],
    },
    {
        "name": "delete_provider_declined",
        "config": copy(BASE_CONFIG),
        "run": "delete_provider",
        "provider_id": "vendor",
        "answers": [{"kind": "confirm", "value": False}],
    },
    {
        "name": "manage_models_update_aliases_then_return",
        "config": copy(BASE_CONFIG),
        "run": "manage_v2_model_settings",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "1"},
            {"kind": "prompt", "value": " a , b ,, "},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "manage_models_update_hidden_aliases_and_routing_mode",
        "config": copy(BASE_CONFIG),
        "run": "manage_v2_model_settings",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "2"},
            {"kind": "prompt", "value": "hidden-b"},
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "3"},
            {"kind": "prompt", "value": "priority"},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "manage_models_delete_model_confirmed",
        "config": copy(BASE_CONFIG),
        "run": "manage_v2_model_settings",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "6"},
            {"kind": "confirm", "value": True},
        ],
    },
    {
        "name": "delete_model_missing",
        "config": copy(BASE_CONFIG),
        "run": "delete_model",
        "model_id": "nope",
        "answers": [],
    },
    {
        "name": "manage_model_routes_unbind_last_key_declined",
        "config": copy(BASE_CONFIG),
        "run": "manage_model_routes",
        "model_id": "local-model",
        "answers": [
            {"kind": "select", "value": "d"},
            {"kind": "select", "value": "1"},
            {"kind": "confirm", "value": False},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "manage_model_routes_unbind_last_key_confirmed",
        "config": copy(BASE_CONFIG),
        "run": "manage_model_routes",
        "model_id": "local-model",
        "answers": [
            {"kind": "select", "value": "d"},
            {"kind": "select", "value": "1"},
            {"kind": "confirm", "value": True},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "update_model_target_upstream",
        "config": copy(BASE_CONFIG),
        "run": "update_model_target_upstream",
        "model_id": "local-model",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "prompt", "value": "upstream-y"},
        ],
    },
    {
        "name": "add_model_route_new_model",
        "config": copy(BASE_CONFIG),
        "run": "add_model_route",
        "model_id": "",
        "answers": [
            {"kind": "select", "value": "__custom__"},
            {"kind": "prompt", "value": "new-model"},
            {"kind": "select", "value": "2"},
            {"kind": "select", "value": "main"},
            {"kind": "prompt", "value": "upstream-z"},
        ],
    },
    {
        "name": "add_model_route_provider_without_keys",
        "config": copy(BASE_CONFIG),
        "run": "add_model_route",
        "model_id": "local-model",
        "answers": [{"kind": "select", "value": "1"}],
    },
    {
        "name": "export_config_transfer",
        "config": copy(BASE_CONFIG),
        "run": "manage_config_transfer",
        "press_on_key": "o",
        "answers": [
            {"kind": "select", "value": "1"},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "paste_config_transfer_apply",
        "config": copy(BASE_CONFIG),
        "run": "manage_config_transfer",
        "press_on_key": "o",
        "answers": [
            {"kind": "select", "value": "2"},
            {
                "kind": "prompt",
                "value": json.dumps(
                    {
                        "providers": {
                            "imported": {
                                "base_url": "https://imported.example.test",
                                "keys": {"k": {"api_key": "sk-imported"}},
                            }
                        },
                        "models": {"imported-model": {"aliases": ["x"], "targets": []}},
                    },
                    ensure_ascii=False,
                    separators=(",", ":"),
                ),
            },
            {"kind": "confirm", "value": True},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "paste_config_transfer_invalid_json",
        "config": copy(BASE_CONFIG),
        "run": "manage_config_transfer",
        "press_on_key": "o",
        "answers": [
            {"kind": "select", "value": "2"},
            {"kind": "prompt", "value": "{ not json"},
            {"kind": "select", "value": "0"},
        ],
    },
    {
        "name": "set_local_api_key_regenerate",
        "config": copy(BASE_CONFIG),
        "run": "set_local_api_key",
        "answers": [{"kind": "confirm", "value": True}],
    },
    {
        "name": "set_local_api_key_declined",
        "config": copy(BASE_CONFIG),
        "run": "set_local_api_key",
        "answers": [{"kind": "confirm", "value": False}],
    },
    {
        "name": "set_webui_enable",
        "config": copy(BASE_CONFIG),
        "run": "set_webui",
        "answers": [{"kind": "confirm", "value": True}],
    },
    {
        "name": "set_timeouts_ok",
        "config": copy(BASE_CONFIG),
        "run": "set_timeouts",
        "answers": [
            {"kind": "prompt", "value": "75"},
            {"kind": "prompt", "value": "95"},
            {"kind": "prompt", "value": "185.5"},
        ],
    },
    {
        "name": "set_timeouts_rejects_non_numeric",
        "config": copy(BASE_CONFIG),
        "run": "set_timeouts",
        "answers": [{"kind": "prompt", "value": "abc"}],
    },
    {
        "name": "set_timeouts_rejects_zero",
        "config": copy(BASE_CONFIG),
        "run": "set_timeouts",
        "answers": [
            {"kind": "prompt", "value": "60"},
            {"kind": "prompt", "value": "0"},
        ],
    },
    {
        "name": "set_listen_updates_port",
        "config": copy(BASE_CONFIG),
        "run": "set_listen",
        "answers": [
            {"kind": "prompt", "value": "127.0.0.1"},
            {"kind": "prompt", "value": "9000"},
        ],
    },
    {
        "name": "set_listen_unchanged",
        "config": copy(BASE_CONFIG),
        "run": "set_listen",
        "answers": [
            {"kind": "prompt", "value": "127.0.0.1"},
            {"kind": "prompt", "value": "8000"},
        ],
    },
    {
        "name": "set_listen_rejects_scheme",
        "config": copy(BASE_CONFIG),
        "run": "set_listen",
        "answers": [{"kind": "prompt", "value": "http://x"}],
    },
    {
        "name": "set_listen_rejects_bad_port",
        "config": copy(BASE_CONFIG),
        "run": "set_listen",
        "answers": [
            {"kind": "prompt", "value": "127.0.0.1"},
            {"kind": "prompt", "value": "70000"},
        ],
    },
    {
        "name": "set_listen_wildcard_confirmed",
        "config": copy(BASE_CONFIG),
        "run": "set_listen",
        "answers": [
            {"kind": "prompt", "value": "0.0.0.0"},
            {"kind": "prompt", "value": "8000"},
            {"kind": "confirm", "value": True},
        ],
    },
    {
        "name": "set_listen_wildcard_declined",
        "config": copy(BASE_CONFIG),
        "run": "set_listen",
        "answers": [
            {"kind": "prompt", "value": "0.0.0.0"},
            {"kind": "prompt", "value": "8000"},
            {"kind": "confirm", "value": False},
        ],
    },
    {
        "name": "empty_config_provider_menu",
        "config": empty_config(),
        "run": "manage_providers",
        "answers": [{"kind": "select", "value": "0"}],
    },
    {
        "name": "add_model_route_without_providers",
        "config": empty_config(),
        "run": "add_model_route",
        "model_id": "m",
        "answers": [],
    },
]

INTERACTIVE_RUNNERS = {
    "manage_providers": lambda path, spec: ce.manage_providers_interactively(path),
    "add_provider_key_existing": lambda path, spec: ce.add_provider_key_interactively(
        path, spec["provider_id"]
    ),
    "manage_provider_keys": lambda path, spec: ce.manage_provider_keys_interactively(
        path, spec["provider_id"]
    ),
    "manage_provider_routes": lambda path, spec: ce.manage_provider_routes_interactively(
        path, spec["provider_id"]
    ),
    "refresh_provider_capability": lambda path, spec: ce.refresh_provider_capability_interactively(
        path, spec["provider_id"]
    ),
    "delete_provider": lambda path, spec: ce.delete_provider_interactively(
        path, spec["provider_id"]
    ),
    "manage_v2_model_settings": lambda path, spec: ce.manage_v2_model_settings_interactively(path),
    "delete_model": lambda path, spec: ce.delete_v2_model_interactively(path, spec["model_id"]),
    "manage_model_routes": lambda path, spec: ce.manage_model_routes_interactively(
        path, spec["model_id"]
    ),
    "update_model_target_upstream": lambda path, spec: ce.update_model_target_upstream_interactively(
        path, spec["model_id"]
    ),
    "add_model_route": lambda path, spec: ce.add_model_route_interactively(path, spec["model_id"] or None),
    "manage_config_transfer": lambda path, spec: ce.manage_config_transfer_interactively(path),
    "set_local_api_key": lambda path, spec: ce.set_local_api_key_interactively(path),
    "set_webui": lambda path, spec: ce.set_webui_interactively(path),
    "set_timeouts": lambda path, spec: ce.set_timeouts_interactively(path),
    "set_listen": lambda path, spec: ce.set_listen_interactively(path),
}


def interactive_cases() -> list[dict[str, Any]]:
    cases = []
    directory = Path(tempfile.mkdtemp(prefix="amkr-ce-ui-corpus-"))
    try:
        for spec in INTERACTIVE_SPECS:
            path = directory / "router-config.json"
            path.write_text(
                json.dumps(spec["config"], indent=2, ensure_ascii=False) + "\n",
                encoding="utf-8",
            )
            # 参照实现的 FORM_DRAFTS 是进程级全局，会在场景之间泄漏（上一个场景填过的
            # key 名会成为下一个场景的默认值）。Go 侧草稿挂在 Editor 上，一个流程一份，
            # 因此这里每场景清空，让两边的起点一致——这是 doc.go 记录的有意差异。
            ce.FORM_DRAFTS.clear()
            recorder = InteractionRecorder()
            queue = AnswerQueue(spec["answers"])
            placeholders = [str(path), str(path.resolve())]
            steps: list[dict[str, Any]] = list(spec.get("steps", []))
            HttpHarness.steps = steps
            saved = install_ui(recorder, queue, spec.get("press_on_key"))
            try:
                with HttpHarness():
                    INTERACTIVE_RUNNERS[spec["run"]](path, spec)
            finally:
                restore_ui(saved)
                HttpHarness.steps = None
            if steps:
                raise AssertionError(f"交互场景网络脚本未用尽: {spec['name']}")
            if not queue.exhausted():
                raise AssertionError(
                    f"交互场景回答未用尽（{len(queue.answers) - queue.index} 条剩余）: {spec['name']}"
                )
            def scrub(value: Any) -> Any:
                """把临时配置路径折成占位符：两边的临时目录必然不同。"""
                if isinstance(value, str):
                    for placeholder in placeholders:
                        value = value.replace(placeholder, "<config-path>")
                    return value
                if isinstance(value, list):
                    return [scrub(item) for item in value]
                if isinstance(value, dict):
                    return {key: scrub(item) for key, item in value.items()}
                return value

            cases.append(
                scrub(
                {
                    "name": spec["name"],
                    "provider_id": spec.get("provider_id"),
                    "press_on_key": spec.get("press_on_key"),
                    "model_id": spec.get("model_id"),
                    "config": spec["config"],
                    "steps": spec.get("steps", []),
                    "answers": spec["answers"],
                    "menus": recorder.menus,
                    "multiples": recorder.multiples,
                    "prompts": recorder.prompts,
                    "confirms": recorder.confirms,
                    "results": recorder.results,
                    "restarts": recorder.restarts,
                    "opened": recorder.opened,
                    "opened_count": len(recorder.opened),
                    "config_after": json.loads(path.read_text(encoding="utf-8")),
                }
                )
            )
    finally:
        shutil.rmtree(directory, ignore_errors=True)
    return cases


PANEL_CONFIGS: list[dict[str, Any]] = [
    {
        "name": "two_providers_and_model",
        "config": {
            "config_version": 4,
            "providers": {
                "gateway": {
                    "base_url": "https://gateway.example.test",
                    "routes": {"openai": "custom/v1", "anthropic": "custom/messages"},
                    "keys": {
                        "main": {
                            "api_key": "sk-main-1234567890",
                            "allow_visitor": True,
                        },
                        "other": {"api_key": "sk-other", "enabled": False},
                    },
                },
                "empty": {"base_url": "https://empty.example.test", "keys": {}},
            },
            "models": {
                "local-model": {
                    "aliases": ["alias-a", "alias-b"],
                    "hidden_aliases": ["hidden-a", "alias-a"],
                    "routing_mode": "priority",
                    "targets": [
                        {"provider": "gateway", "key": "main", "upstream_model": "upstream-x"},
                        {"provider": "gateway", "key": "other", "upstream_model": "upstream-y"},
                        {"provider": "gateway", "key": "other"},
                    ],
                },
                "no-targets": {"routing_mode": "only_first"},
            },
        },
    },
    {
        "name": "probed_and_unprobed_capabilities",
        "config": {
            "config_version": 4,
            "providers": {
                "gateway": {
                    "base_url": "https://gateway.example.test",
                    "keys": {
                        "probed": {
                            "api_key": "sk-probed",
                            "enabled": True,
                            "capabilities": {
                                "models": [
                                    "m1",
                                    "m2",
                                    "m3",
                                    "m4",
                                    "m5",
                                    "m6",
                                    "m7",
                                    "m8",
                                    "m9",
                                    "m10",
                                ],
                                "route_status": {"openai": "ok", "responses": "failed: 403"},
                                "errors": {},
                                "checked_at": "2026-01-02T03:04:05+00:00",
                            },
                        },
                        "unprobed": {"api_key": "sk-unprobed"},
                    },
                }
            },
            "models": {},
        },
    },
    {
        "name": "probed_with_errors_and_empty_models",
        "config": {
            "config_version": 4,
            "providers": {
                "gateway": {
                    "base_url": "https://gateway.example.test",
                    "keys": {
                        "broken": {
                            "api_key": "sk-broken",
                            "capabilities": {
                                "models": [],
                                "route_status": {},
                                "errors": {"broken": "HTTP 401"},
                                "checked_at": None,
                            },
                        }
                    },
                }
            },
            "models": {},
        },
    },
    {
        "name": "empty_config",
        "config": {"config_version": 4, "providers": {}, "models": {}},
    },
]


BOX_GLYPHS = set("─━│┃╭╮╰╯├┤┬┴┼┏┓┗┛┣┫┳┻╋╸╺╹╻━┄┅┆┇┈┉┊┋")


def content_tokens(renderable: Any) -> list[str]:
    """把节点渲染成纯文本，抽出「有内容的词」。

    版式（列宽、间距、边框）是本迁移明确不追求一致的部分，因此对拍的是**信息**：
    每个含字母/数字/汉字的连续片段必须出现在 Go 侧同样归一化后的输出里。
    """
    from io import StringIO

    from rich.console import Console

    buffer = StringIO()
    Console(file=buffer, width=200, no_color=True, legacy_windows=False).print(renderable)
    text = buffer.getvalue()
    for glyph in BOX_GLYPHS:
        text = text.replace(glyph, " ")
    tokens: list[str] = []
    for piece in text.split():
        if len(piece) < 2:
            continue
        if not any(char.isalnum() or "\u4e00" <= char <= "\u9fff" for char in piece):
            continue
        if piece not in tokens:
            tokens.append(piece)
    return tokens


def panel_cases() -> list[dict[str, Any]]:
    cases = []
    for spec in PANEL_CONFIGS:
        data = copy(spec["config"])
        providers = data.get("providers", {})
        models = data.get("models", {})
        cases.append(
            {
                "name": spec["name"] + "/v2_summary",
                "panel": "v2_summary",
                "config": spec["config"],
                "model_id": None,
                "provider_id": None,
                "tokens": content_tokens(ce.v2_summary_panel(copy(data))),
            }
        )
        if providers:
            provider_id = sorted(providers)[0]
            cases.append(
                {
                    "name": spec["name"] + "/provider_capabilities",
                    "panel": "provider_capabilities",
                    "config": spec["config"],
                    "model_id": None,
                    "provider_id": provider_id,
                    "tokens": content_tokens(
                        ce.provider_capabilities_panel(copy(providers[provider_id]))
                    ),
                }
            )
        for model_id in sorted(models):
            cases.append(
                {
                    "name": spec["name"] + "/model_key_targets_" + model_id,
                    "panel": "model_key_targets",
                    "config": spec["config"],
                    "model_id": model_id,
                    "provider_id": None,
                    "tokens": content_tokens(
                        ce.model_key_targets_panel(copy(data), model_id)
                    ),
                }
            )
    return cases

FETCH_STATE_FILE: dict[str, Any] = {
    "endpoint_capabilities": {
        "https://file.example.test": {"supported": True, "reason": "file"}
    }
}

FETCH_SPECS: list[dict[str, Any]] = [
    {
        "name": "health_merges_over_file",
        "steps": [
            {
                "status": 200,
                "body": json.dumps(
                    {
                        "native_endpoint_states": {
                            "https://service.example.test": {"supported": True, "reason": "probe"},
                            "https://file.example.test": {"supported": False, "reason": "service"},
                        }
                    }
                ),
            }
        ],
    },
    {
        "name": "health_without_states_keeps_file",
        "steps": [{"status": 200, "body": json.dumps({"status": "ok"})}],
    },
    {
        "name": "health_states_not_object_keeps_file",
        "steps": [{"status": 200, "body": json.dumps({"native_endpoint_states": ["a"]})}],
    },
    {
        "name": "health_non_object_value_skipped",
        "steps": [
            {
                "status": 200,
                "body": json.dumps(
                    {"native_endpoint_states": {"https://bool.example.test": True}}
                ),
            }
        ],
    },
    {
        "name": "health_404_keeps_file",
        "steps": [{"status": 404, "body": "not found"}],
    },
    {
        "name": "health_invalid_json_keeps_file",
        "steps": [{"status": 200, "body": "{"}],
    },
    {
        "name": "health_network_error_keeps_file",
        "steps": [{"error": "connect", "message": "脚本化连接失败"}],
    },

]


def fetch_native_endpoint_states_cases() -> list[dict[str, Any]]:
    cases = []
    directory = Path(tempfile.mkdtemp(prefix="amkr-ce-fetch-corpus-"))
    try:
        path = directory / "endpoint-capabilities.json"
        path.write_text(json.dumps(FETCH_STATE_FILE), encoding="utf-8")
        for spec in FETCH_SPECS:
            data = {
                "host": "127.0.0.1",
                "port": 8000,
                "endpoint_capabilities_path": str(path),
            }
            requests = run_http_case(
                spec,
                lambda data=data: ce.fetch_native_endpoint_states(data, timeout=0.5),
            )
            states = spec.pop("_result")
            cases.append(
                {
                    "name": spec["name"],
                    "data": {**data, "endpoint_capabilities_path": None},
                    "file_content": json.dumps(FETCH_STATE_FILE),
                    "steps": spec["steps"],
                    "requests": requests,
                    "expected": states,
                }
            )
    finally:
        shutil.rmtree(directory, ignore_errors=True)
    return cases

def build_corpus() -> dict[str, Any]:
    real_monotonic = ce.monotonic
    real_datetime = ce.datetime
    real_time = ce.time
    try:
        ce.monotonic = ScriptedClock()
        ce.datetime = FakeDateTime
        ce.time = lambda: FIXED_EPOCH
        sections = {
            "probe_payload": probe_payload_cases(),
            "probe_error_text": probe_error_text_cases(),
            "discover_models": discover_models_cases(),
            "probe_availability": availability_cases(),
            "probe_capability": capability_cases(),
            "probe_provider_key_capabilities": provider_capabilities_cases(),
            "upstream_routes_for_base_url": upstream_routes_cases(),
            "service_management_base_url": service_management_base_url_cases(),
            "native_endpoint_state_payload": native_endpoint_state_cases(),
            "format_visitor_status_text": format_visitor_status_cases(),
            "native_endpoint_support_text": native_endpoint_support_text_cases(),
            "native_endpoint_states_from_file": native_endpoint_states_from_file_cases(),
            "panels": panel_cases(),
            "fetch_native_endpoint_states": fetch_native_endpoint_states_cases(),
            "interactive": interactive_cases(),
        }
    finally:
        ce.monotonic = real_monotonic
        ce.datetime = real_datetime
        ce.time = real_time
        HttpHarness.steps = None

    counts = {name: len(cases) for name, cases in sections.items()}
    return {
        "version": 1,
        "generated_by": "scripts/gen_config_editor_corpus.py",
        "detail_sentinel": DETAIL_SENTINEL,
        "fixed_local_api_key": FIXED_LOCAL_API_KEY,
        "counts": counts,
        "total": sum(counts.values()),
        "sections": sections,
    }


def serialize(corpus: dict[str, Any]) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=1) + "\n"


def main() -> int:
    corpus = build_corpus()
    text = serialize(corpus)
    check = "--check" in sys.argv[1:]
    if check:
        if not OUTPUT.exists():
            print(f"语料缺失: {OUTPUT}", file=sys.stderr)
            return 1
        current = OUTPUT.read_text(encoding="utf-8")
        if current != text:
            print(
                f"语料已过期: {OUTPUT}\n"
                "请运行 python scripts/gen_config_editor_corpus.py 重新生成。",
                file=sys.stderr,
            )
            return 1
        print(f"语料最新: {OUTPUT.name}（{corpus['total']} 条）")
        return 0
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    # newline="\n" 保证在 Windows 上也是 LF，避免 CRLF 让对拍结果随平台漂移。
    with OUTPUT.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
    print(f"已写入 {OUTPUT}（{corpus['total']} 条）")
    for name, count in corpus["counts"].items():
        print(f"  {name}: {count}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
