#!/usr/bin/env python3
"""生成 ``auto_model_key_router.management_api`` 的差分对拍语料。

这个脚本驱动**真实的** Python FastAPI 应用（``create_app`` + httpx ASGITransport），
对一个临时配置文件逐条发请求，把「状态码 + 响应体字节 + 选定的响应头」记进
``internal/api/testdata/management_api_corpus.json``；Go 侧用 httptest 重放同一批
请求并逐字节比对。

设计要点：

1. **固定路径**。所有用例共用同一个临时目录（``%TEMP%/amkr_management_api_corpus``），
   每轮开始前清空。配置文件路径会出现在 409 的 ``detail`` 里，用 mkdtemp 会让语料
   随运行变化，``--check`` 就永远失败。
2. **必须打补丁的外部依赖**。probe/版本检查/metrics 都会发网络请求或依赖另一个
   尚未移植的包，语料对它们做了确定性替换（见 ``patch_dependencies``）。补丁点选在
   ``management_api`` 真正查找它们的位置：
   - ``management_api.uuid``        -> 固定 uuid4（探测 id 与备份文件后缀）
   - ``management_api.generate_local_api_key`` -> 固定本地 key
   - ``update.check_latest_version`` -> 固定 VersionCheckResult
   - ``config_editor.probe_key_capability`` / ``probe_provider_key_capabilities``
     / ``probe_key_availability`` -> 固定探测结果
   - ``metrics.MetricsStore.key_stats`` -> 固定 stats（并把 hours 原样回显，
     让 Go 侧能断言 hours 的解析行为）
3. **``$REV`` 占位符**。需要当前 config_revision 的用例写 ``"$REV"``，发请求前替换成
   从磁盘算出的当前版本；``"$STALE"`` 是一个必然不匹配的版本号。
4. **变异请求额外记录 config_after**。POST/PUT/DELETE 用例会把请求后的配置文件按
   canonical（sort_keys + 紧凑分隔符）形式记录下来，Go 侧据此断言「改了什么」以及
   「版本不符时一个字节都没动」。

用法::

    python -X utf8 scripts/gen_management_api_corpus.py
    python -X utf8 scripts/gen_management_api_corpus.py --check
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import sys
import tempfile
import uuid
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import anyio  # noqa: E402
import httpx  # noqa: E402

from auto_model_key_router import config_editor, update  # noqa: E402
from auto_model_key_router import management_api as mgmt  # noqa: E402
from auto_model_key_router import metrics as metrics_module  # noqa: E402
from auto_model_key_router.app import create_app  # noqa: E402
from auto_model_key_router.config import (  # noqa: E402
    RouterConfig,
    migrate_config_data,
    save_config_data,
)
from auto_model_key_router.update import VersionCheckResult  # noqa: E402

DEFAULT_PATH = REPO_ROOT / "internal" / "api" / "testdata" / "management_api_corpus.json"
CORPUS_VERSION = 1

BASE_DIR = Path(tempfile.gettempdir()) / "amkr_management_api_corpus"
CONFIG_PATH = BASE_DIR / "router-config.json"
STALE_REVISION = "stale-revision-0000000000000000"
FIXED_UUID = uuid.UUID("00112233445566778899aabbccddeeff")
FIXED_LOCAL_API_KEY = "amkr_CORPUS0000000000000000000000000000000000000"
FULL_AUTH = {"Authorization": "Bearer local-key"}
VISITOR_AUTH = {"Authorization": "Bearer amkr-visitor"}

# 探测补丁的可变状态：单个用例可以临时翻转它以覆盖失败/屏蔽分支。
PATCH_STATE = {"fail_capabilities": False, "availability_error": None}


def canonical_dumps(obj: object) -> str:
    """sort_keys + 紧凑分隔符，与 Go 的 ``canonical.Dumps`` 逐字节对齐。"""
    return json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def fixture() -> dict:
    """所有用例共用的初始配置。"""
    return {
        "config_version": 4,
        "host": "127.0.0.1",
        "port": 8000,
        "request_timeout": 10,
        "stream_first_byte_timeout": 30,
        "stream_idle_timeout": 60,
        "max_retries": 1,
        "key_failure_threshold": 1,
        "key_cooldown_seconds": 60,
        "endpoint_capabilities_path": str(BASE_DIR / "endpoint-capabilities.json"),
        "upstream_health_check_interval": 0,
        "metrics_db_path": str(BASE_DIR / "metrics.sqlite3"),
        "log_file_path": str(BASE_DIR / "server.log"),
        "local_api_key": "local-key",
        "providers": {
            "prov-a": {
                "base_url": "https://a.example.test",
                "keys": {
                    "key-a": {"api_key": "sk-secret-a", "enabled": True},
                    "key-b": {"api_key": "sk-secret-b", "enabled": False},
                },
                # 刻意用非规范顺序：响应里的键序必须与配置一致，排序输出会暴露差异。
                "routes": {
                    "responses": "v1/responses",
                    "openai": "v1/chat/completions",
                },
            },
            "prov-b": {
                "base_url": "https://b.example.test",
                "keys": {
                    "key-c": {
                        "api_key": "sk-secret-c",
                        "enabled": True,
                        "allow_visitor": True,
                    }
                },
                "routes": {},
            },
        },
        "models": {
            "model-a": {
                "targets": [
                    {"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}
                ],
                "aliases": ["alias-a"],
                "hidden_aliases": ["hidden-a"],
                "routing_mode": "round_robin",
            },
            "model-b": {
                "targets": [
                    {"provider": "prov-b", "key": "key-c", "upstream_model": "model-b"}
                ]
            },
        },
        "tasks": {
            "task-a": {
                "model": "model-a",
                "fallback_model": "model-b",
                "params": {"temperature": 0.5, "stop": ["\n"]},
            }
        },
        "unified_model": {
            "default": {
                "primary": {"model": "model-a", "key": "key-a"},
                "fallback": {"model": "model-b", "key": "key-c"},
            },
            "image": {"primary": {"model": "model-b"}},
        },
    }


def reset_base_dir() -> None:
    shutil.rmtree(BASE_DIR, ignore_errors=True)
    BASE_DIR.mkdir(parents=True, exist_ok=True)


def current_revision() -> str:
    data = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    payload = canonical_dumps(migrate_config_data(data))
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def substitute(value: object) -> object:
    """把 $REV / $STALE 占位符换成真实版本号。"""
    if isinstance(value, str):
        if value == "$REV":
            return current_revision()
        if value == "$STALE":
            return STALE_REVISION
        return value
    if isinstance(value, dict):
        return {key: substitute(item) for key, item in value.items()}
    if isinstance(value, list):
        return [substitute(item) for item in value]
    return value


# —— 依赖补丁 —— #


class _FakeUUID:
    @staticmethod
    def uuid4() -> uuid.UUID:
        return FIXED_UUID


class _FakeAvailability:
    __slots__ = ("available", "url", "duration_ms", "error")

    def __init__(self, available: bool, url: str, duration_ms: float, error: str | None):
        self.available = available
        self.url = url
        self.duration_ms = duration_ms
        self.error = error


def patch_dependencies() -> None:
    mgmt.uuid = _FakeUUID
    mgmt.generate_local_api_key = lambda: FIXED_LOCAL_API_KEY

    def fake_capability(provider, key_name, modes=None, timeout=15.0):
        return {
            "models": ["model-a", "model-b"],
            "routes": {"openai": True, "anthropic": False},
            "modes": list(modes) if modes is not None else None,
        }

    def fake_provider_capabilities(provider, key_names, timeout=15.0):
        if PATCH_STATE["fail_capabilities"]:
            # 故意把密钥写进异常文本，用来验证探测错误的脱敏。
            raise RuntimeError("upstream rejected Authorization: Bearer sk-secret-a")
        return {"key_models": {name: ["model-a"] for name in key_names}}

    def fake_availability(routes, model, key, timeout=15.0):
        error = PATCH_STATE["availability_error"]
        return [
            _FakeAvailability(
                available=error is None,
                url="https://a.example.test/v1/models",
                duration_ms=250,
                error=error,
            )
        ]

    config_editor.probe_key_capability = fake_capability
    config_editor.probe_provider_key_capabilities = fake_provider_capabilities
    config_editor.probe_key_availability = fake_availability

    def fake_check(current_version="0.0.0", timeout=3.0):
        return VersionCheckResult(
            current_version="1.2.3",
            latest_version="1.3.0",
            latest_tag="v1.3.0",
            release_url="https://example.test/release",
            source="pypi",
            artifact_url="https://example.test/a.whl",
            artifact_sha256="deadbeef",
            fallback_error=None,
            error=None,
        )

    update.check_latest_version = fake_check

    async def fake_key_stats(self, model_id, key_name, hours=None):
        # 把 hours 原样回显：Go 侧据此断言查询参数的解析（缺省/非法 -> None）。
        return {
            "model_id": model_id,
            "key_name": key_name,
            "hours": hours,
            "stats": {"requests": 0},
        }

    metrics_module.MetricsStore.key_stats = fake_key_stats


# —— 用例定义 —— #

CASES: list[dict] = []


def case(
    name: str,
    method: str,
    path: str,
    *,
    body: object = None,
    has_body: bool = False,
    auth: str = "full",
    covers: list[str] | None = None,
    setup: list[tuple[str, str, object]] | None = None,
    delete_config: bool = False,
    patch_state: dict | None = None,
    config_replace: dict | None = None,
) -> None:
    CASES.append(
        {
            "name": name,
            "method": method,
            "path": path,
            "body": {"kind": "json", "value": body} if has_body else {"kind": "none"},
            "auth": auth,
            "covers": covers or [],
            "setup": setup or [],
            "delete_config": delete_config,
            "patch_state": patch_state or {},
            "config_replace": config_replace,
        }
    )


def build_cases() -> None:
    # —— unified-model ——
    case("unified/get", "GET", "/api/unified-model", covers=["GET /api/unified-model"])
    case(
        "unified/put-model",
        "PUT",
        "/api/unified-model",
        body={"model": "model-b"},
        has_body=True,
        covers=["PUT /api/unified-model"],
    )
    case(
        "unified/put-model-with-key-and-image",
        "PUT",
        "/api/unified-model",
        body={
            "config_revision": "$REV",
            "model": "model-b",
            "key": "key-c",
            "image_model": "model-a",
            "image_key": "key-a",
        },
        has_body=True,
    )
    case(
        "unified/put-default",
        "PUT",
        "/api/unified-model",
        body={
            "config_revision": "$REV",
            "default": {"primary": {"model": "model-a", "key": "key-a"}},
        },
        has_body=True,
    )
    case(
        "unified/put-default-with-embeddings",
        "PUT",
        "/api/unified-model",
        body={
            "config_revision": "$REV",
            "default": {"primary": {"model": "model-b", "key": "key-c"}},
            "image": {"primary": {"model": "model-a"}},
            "embeddings": {"primary": {"model": "model-a", "key": "key-a"}},
        },
        has_body=True,
    )
    case(
        "unified/put-empty-body-object",
        "PUT",
        "/api/unified-model",
        body={},
        has_body=True,
        covers=[],
    )
    case(
        "unified/put-no-model",
        "PUT",
        "/api/unified-model",
        body={"model": None},
        has_body=True,
    )
    case(
        "unified/put-blank-model",
        "PUT",
        "/api/unified-model",
        body={"model": "   "},
        has_body=True,
    )
    case(
        "unified/put-stale-revision",
        "PUT",
        "/api/unified-model",
        body={"config_revision": "$STALE", "default": {"primary": {"model": "model-b"}}},
        has_body=True,
    )
    case(
        "unified/put-no-body",
        "PUT",
        "/api/unified-model",
    )
    case(
        "unified/put-default-not-object",
        "PUT",
        "/api/unified-model",
        body={"default": [], "model": "model-a"},
        has_body=True,
    )
    case("unified/delete-no-body", "DELETE", "/api/unified-model", covers=["DELETE /api/unified-model"])
    case(
        "unified/delete-with-revision",
        "DELETE",
        "/api/unified-model",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "unified/delete-null-body",
        "DELETE",
        "/api/unified-model",
        body=None,
        has_body=True,
    )
    case(
        "unified/delete-stale-revision",
        "DELETE",
        "/api/unified-model",
        body={"config_revision": "$STALE"},
        has_body=True,
    )

    # —— 任务 ——
    case("tasks/list", "GET", "/api/tasks", covers=["GET /api/tasks"])
    case(
        "tasks/create",
        "POST",
        "/api/tasks",
        body={
            "config_revision": "$REV",
            "name": "task-new",
            "model": "model-a",
            "fallback_model": "model-b",
            "params": {
                "temperature": 0.25,
                "top_p": 0.9,
                "top_k": 40,
                "frequency_penalty": 0.1,
                "presence_penalty": 0.2,
                "seed": 7,
                "stop": ["x"],
                "reasoning_effort": "high",
            },
        },
        has_body=True,
        covers=["POST /api/tasks"],
    )
    case(
        "tasks/create-string-numbers",
        "POST",
        "/api/tasks",
        body={
            "config_revision": "$REV",
            "name": "task-str",
            "model": "model-a",
            "params": {"temperature": "0.5", "top_k": "3"},
        },
        has_body=True,
    )
    case(
        "tasks/create-empty-params",
        "POST",
        "/api/tasks",
        body={"config_revision": "$REV", "name": "task-empty", "model": "model-a", "params": {}},
        has_body=True,
    )
    case(
        "tasks/create-unknown-param",
        "POST",
        "/api/tasks",
        body={
            "config_revision": "$REV",
            "name": "task-bad",
            "model": "model-a",
            "params": {"nope": 1},
        },
        has_body=True,
    )
    case(
        "tasks/create-param-revision-quirk",
        "POST",
        "/api/tasks",
        body={
            "config_revision": "$REV",
            "name": "task-quirk",
            "model": "model-a",
            "params": {"config_revision": "inner"},
        },
        has_body=True,
    )
    case(
        "tasks/create-missing-name",
        "POST",
        "/api/tasks",
        body={"config_revision": "$REV", "model": "model-a"},
        has_body=True,
    )
    case(
        "tasks/create-missing-all",
        "POST",
        "/api/tasks",
        body={},
        has_body=True,
    )
    case(
        "tasks/create-stale-revision",
        "POST",
        "/api/tasks",
        body={"config_revision": "$STALE", "name": "task-x", "model": "model-a"},
        has_body=True,
    )
    case(
        "tasks/create-extra-field",
        "POST",
        "/api/tasks",
        body={"config_revision": "$REV", "name": "task-x", "model": "model-a", "extra": 1},
        has_body=True,
    )
    case("tasks/get", "GET", "/api/tasks/task-a", covers=["GET /api/tasks/{task_name}"])
    case("tasks/get-missing", "GET", "/api/tasks/nope")
    case(
        "tasks/update-model",
        "PUT",
        "/api/tasks/task-a",
        body={"config_revision": "$REV", "model": "model-b"},
        has_body=True,
        covers=["PUT /api/tasks/{task_name}"],
    )
    case(
        "tasks/update-params",
        "PUT",
        "/api/tasks/task-a",
        body={"config_revision": "$REV", "params": {"temperature": 0.75}},
        has_body=True,
    )
    case(
        "tasks/update-params-empty",
        "PUT",
        "/api/tasks/task-a",
        body={"config_revision": "$REV", "params": {}},
        has_body=True,
    )
    case(
        "tasks/update-fallback-null",
        "PUT",
        "/api/tasks/task-a",
        body={"config_revision": "$REV", "fallback_model": None},
        has_body=True,
    )
    case(
        "tasks/update-nothing",
        "PUT",
        "/api/tasks/task-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "tasks/update-missing-task",
        "PUT",
        "/api/tasks/nope",
        body={"config_revision": "$REV", "model": "model-b"},
        has_body=True,
    )
    case("tasks/delete-no-body", "DELETE", "/api/tasks/task-a", covers=["DELETE /api/tasks/{task_name}"])
    case(
        "tasks/delete-with-revision",
        "DELETE",
        "/api/tasks/task-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "tasks/delete-stale-revision",
        "DELETE",
        "/api/tasks/task-a",
        body={"config_revision": "$STALE"},
        has_body=True,
    )

    # —— 设置 ——
    case("settings/get", "GET", "/api/settings", covers=["GET /api/settings"])
    case(
        "settings/put",
        "PUT",
        "/api/settings",
        body={"config_revision": "$REV", "port": 9000, "max_retries": 3},
        has_body=True,
        covers=["PUT /api/settings"],
    )
    case(
        "settings/put-lax-coercion",
        "PUT",
        "/api/settings",
        body={
            "config_revision": "$REV",
            "port": "9",
            "request_timeout": "1.5",
            "max_retries": True,
        },
        has_body=True,
    )
    case(
        "settings/put-empty",
        "PUT",
        "/api/settings",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "settings/put-no-revision",
        "PUT",
        "/api/settings",
        body={"host": "0.0.0.0"},
        has_body=True,
    )
    case(
        "settings/put-stale-revision",
        "PUT",
        "/api/settings",
        body={"config_revision": "$STALE", "port": 9000},
        has_body=True,
    )
    case(
        "settings/put-bad-port",
        "PUT",
        "/api/settings",
        body={"config_revision": "$REV", "port": 0},
        has_body=True,
    )
    case(
        "settings/put-extra-and-bad-port",
        "PUT",
        "/api/settings",
        body={"port": 0, "nope": 1},
        has_body=True,
    )
    case(
        "settings/put-host-int",
        "PUT",
        "/api/settings",
        body={"config_revision": "$REV", "host": 123},
        has_body=True,
    )
    case(
        "settings/put-fractional-int",
        "PUT",
        "/api/settings",
        body={"config_revision": "$REV", "max_retries": 1.5},
        has_body=True,
    )
    case("settings/put-body-list", "PUT", "/api/settings", body=[1, 2], has_body=True)
    case("settings/put-body-string", "PUT", "/api/settings", body="x", has_body=True)
    case("settings/put-body-null", "PUT", "/api/settings", body=None, has_body=True)
    case("settings/put-no-body", "PUT", "/api/settings")
    case(
        "settings/regenerate-key",
        "POST",
        "/api/settings/local-api-key",
        body={"config_revision": "$REV"},
        has_body=True,
        covers=["POST /api/settings/local-api-key"],
    )
    case(
        "settings/regenerate-key-no-revision",
        "POST",
        "/api/settings/local-api-key",
        body={},
        has_body=True,
    )
    case(
        "settings/regenerate-key-stale",
        "POST",
        "/api/settings/local-api-key",
        body={"config_revision": "$STALE"},
        has_body=True,
    )

    # —— 版本检查 ——
    case("update/check", "POST", "/api/update/check", covers=["POST /api/update/check"])

    # —— 供应商 ——
    case("providers/list", "GET", "/api/providers", covers=["GET /api/providers"])
    case(
        "providers/create",
        "POST",
        "/api/providers",
        body={"config_revision": "$REV", "id": "prov-c", "base_url": "https://c.example.test"},
        has_body=True,
        covers=["POST /api/providers"],
    )
    case(
        "providers/create-duplicate",
        "POST",
        "/api/providers",
        body={"config_revision": "$REV", "id": "prov-a", "base_url": "https://c.example.test"},
        has_body=True,
    )
    case(
        "providers/create-blank-base-url",
        "POST",
        "/api/providers",
        body={"config_revision": "$REV", "id": "prov-c", "base_url": ""},
        has_body=True,
    )
    case(
        "providers/create-stale-revision",
        "POST",
        "/api/providers",
        body={"config_revision": "$STALE", "id": "prov-c", "base_url": "https://c.example.test"},
        has_body=True,
    )
    case(
        "providers/create-no-revision",
        "POST",
        "/api/providers",
        body={"id": "prov-c", "base_url": "https://c.example.test"},
        has_body=True,
    )
    case("providers/get", "GET", "/api/providers/prov-a", covers=["GET /api/providers/{provider_id}"])
    case("providers/get-missing", "GET", "/api/providers/nope")
    case(
        "providers/update-base-url",
        "PUT",
        "/api/providers/prov-a",
        body={"config_revision": "$REV", "base_url": "https://a2.example.test"},
        has_body=True,
        covers=["PUT /api/providers/{provider_id}"],
    )
    case(
        "providers/update-rename-and-routes",
        "PUT",
        "/api/providers/prov-a",
        body={
            "config_revision": "$REV",
            "id": "prov-a2",
            "routes": {"openai": "/v1/chat/completions", "images": None},
        },
        has_body=True,
    )
    case(
        "providers/update-id-null",
        "PUT",
        "/api/providers/prov-a",
        body={"config_revision": "$REV", "id": None},
        has_body=True,
    )
    case(
        "providers/update-stale-revision",
        "PUT",
        "/api/providers/prov-a",
        body={"config_revision": "$STALE", "base_url": "https://a2.example.test"},
        has_body=True,
    )
    case(
        "providers/update-missing-provider",
        "PUT",
        "/api/providers/nope",
        body={"config_revision": "$REV", "base_url": "https://a2.example.test"},
        has_body=True,
    )
    case(
        "providers/delete",
        "DELETE",
        "/api/providers/prov-a",
        body={"config_revision": "$REV"},
        has_body=True,
        covers=["DELETE /api/providers/{provider_id}"],
    )
    case("providers/delete-no-body", "DELETE", "/api/providers/prov-a")
    case(
        "providers/delete-empty-body",
        "DELETE",
        "/api/providers/prov-a",
        body={},
        has_body=True,
    )
    case(
        "providers/delete-stale-revision",
        "DELETE",
        "/api/providers/prov-a",
        body={"config_revision": "$STALE"},
        has_body=True,
    )
    case(
        "providers/delete-missing",
        "DELETE",
        "/api/providers/nope",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case("provider-keys/list", "GET", "/api/providers/prov-a/keys", covers=["GET /api/providers/{provider_id}/keys"])
    case("provider-keys/list-missing-provider", "GET", "/api/providers/nope/keys")
    case(
        "provider-keys/create",
        "POST",
        "/api/providers/prov-a/keys",
        body={
            "config_revision": "$REV",
            "name": "key-d",
            "api_key": "sk-secret-d",
            "enabled": False,
            "allow_visitor": True,
        },
        has_body=True,
        covers=["POST /api/providers/{provider_id}/keys"],
    )
    case(
        "provider-keys/create-duplicate",
        "POST",
        "/api/providers/prov-a/keys",
        body={"config_revision": "$REV", "name": "key-a", "api_key": "sk-secret-d"},
        has_body=True,
    )
    case(
        "provider-keys/create-blank-name",
        "POST",
        "/api/providers/prov-a/keys",
        body={"config_revision": "$REV", "name": "", "api_key": "sk-secret-d"},
        has_body=True,
    )
    case(
        "provider-keys/create-null-enabled",
        "POST",
        "/api/providers/prov-a/keys",
        body={"config_revision": "$REV", "name": "key-d", "api_key": "sk-secret-d", "enabled": None},
        has_body=True,
    )
    case(
        "provider-keys/create-bad-bool",
        "POST",
        "/api/providers/prov-a/keys",
        body={"config_revision": "$REV", "name": "key-d", "api_key": "sk-secret-d", "enabled": 2},
        has_body=True,
    )
    case(
        "provider-keys/get",
        "GET",
        "/api/providers/prov-a/keys/key-a",
        covers=["GET /api/providers/{provider_id}/keys/{key_name}"],
    )
    case("provider-keys/get-missing", "GET", "/api/providers/prov-a/keys/nope")
    case(
        "provider-keys/update",
        "PUT",
        "/api/providers/prov-a/keys/key-a",
        body={"config_revision": "$REV", "name": "key-a2", "enabled": False},
        has_body=True,
        covers=["PUT /api/providers/{provider_id}/keys/{key_name}"],
    )
    case(
        "provider-keys/update-empty-updates",
        "PUT",
        "/api/providers/prov-a/keys/key-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "provider-keys/update-name-null",
        "PUT",
        "/api/providers/prov-a/keys/key-a",
        body={"config_revision": "$REV", "name": None},
        has_body=True,
    )
    case(
        "provider-keys/update-missing-key",
        "PUT",
        "/api/providers/prov-a/keys/nope",
        body={"config_revision": "$REV", "enabled": False},
        has_body=True,
    )
    case(
        "provider-keys/delete",
        "DELETE",
        "/api/providers/prov-a/keys/key-a",
        body={"config_revision": "$REV"},
        has_body=True,
        covers=["DELETE /api/providers/{provider_id}/keys/{key_name}"],
    )
    case("provider-keys/delete-no-body", "DELETE", "/api/providers/prov-a/keys/key-a")
    case(
        "provider-keys/delete-missing-key",
        "DELETE",
        "/api/providers/prov-a/keys/nope",
        body={"config_revision": "$REV"},
        has_body=True,
    )

    # —— 路由（v3 视图）——
    case("routes/list", "GET", "/api/routes", covers=["GET /api/routes"])
    case(
        "routes/create",
        "POST",
        "/api/routes",
        body={
            "config_revision": "$REV",
            "id": "route-x",
            "targets": [
                {"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}
            ],
            "aliases": ["route-alias"],
            "hidden_aliases": ["route-hidden"],
        },
        has_body=True,
        covers=["POST /api/routes"],
    )
    case(
        "routes/create-with-mode",
        "POST",
        "/api/routes",
        body={
            "config_revision": "$REV",
            "id": "route-y",
            "targets": [{"provider": "prov-b", "key": "key-c", "upstream_model": "model-b"}],
            "routing_mode": "round_robin",
        },
        has_body=True,
    )
    case(
        "routes/create-bad-routing-mode",
        "POST",
        "/api/routes",
        body={
            "config_revision": "$REV",
            "id": "route-x",
            "targets": [{"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}],
            "routing_mode": "nope",
        },
        has_body=True,
    )
    case(
        "routes/create-empty-targets",
        "POST",
        "/api/routes",
        body={"config_revision": "$REV", "id": "route-x", "targets": []},
        has_body=True,
    )
    case(
        "routes/create-bad-target",
        "POST",
        "/api/routes",
        body={
            "config_revision": "$REV",
            "id": "route-x",
            "targets": [{"provider": "nope", "key": "key-a", "upstream_model": "model-a"}],
        },
        has_body=True,
    )
    case(
        "routes/create-target-not-object",
        "POST",
        "/api/routes",
        body={"config_revision": "$REV", "id": "route-x", "targets": ["x"]},
        has_body=True,
    )
    case(
        "routes/create-stale-revision",
        "POST",
        "/api/routes",
        body={
            "config_revision": "$STALE",
            "id": "route-x",
            "targets": [{"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}],
        },
        has_body=True,
    )
    case("routes/get", "GET", "/api/routes/model-a", covers=["GET /api/routes/{route_id}"])
    case("routes/get-missing", "GET", "/api/routes/nope")
    case(
        "routes/update-aliases",
        "PUT",
        "/api/routes/model-a",
        body={"config_revision": "$REV", "aliases": ["alias-a2"]},
        has_body=True,
        covers=["PUT /api/routes/{route_id}"],
    )
    case(
        "routes/update-targets",
        "PUT",
        "/api/routes/model-a",
        body={
            "config_revision": "$REV",
            "targets": [{"provider": "prov-b", "key": "key-c", "upstream_model": "model-a"}],
        },
        has_body=True,
    )
    case(
        "routes/update-rename",
        "PUT",
        "/api/routes/model-a",
        body={"config_revision": "$REV", "id": "model-a2"},
        has_body=True,
    )
    case(
        "routes/update-id-null",
        "PUT",
        "/api/routes/model-a",
        body={"config_revision": "$REV", "id": None},
        has_body=True,
    )
    case(
        "routes/update-missing",
        "PUT",
        "/api/routes/nope",
        body={"config_revision": "$REV", "aliases": []},
        has_body=True,
    )
    case(
        "routes/delete",
        "DELETE",
        "/api/routes/model-a",
        body={"config_revision": "$REV"},
        has_body=True,
        covers=["DELETE /api/routes/{route_id}"],
    )
    case("routes/delete-no-body", "DELETE", "/api/routes/model-a")
    case(
        "routes/delete-stale-revision",
        "DELETE",
        "/api/routes/model-a",
        body={"config_revision": "$STALE"},
        has_body=True,
    )

    # —— 探测 ——
    case(
        "probes/start",
        "POST",
        "/api/probes/keys",
        body={"provider_id": "prov-a", "keys": ["key-a"], "timeout_seconds": 5},
        has_body=True,
        covers=["POST /api/probes/keys"],
    )
    case(
        "probes/start-all-keys",
        "POST",
        "/api/probes/keys",
        body={"config_revision": "$REV", "provider_id": "prov-a", "keys": []},
        has_body=True,
    )
    case(
        "probes/start-missing-key",
        "POST",
        "/api/probes/keys",
        body={"provider_id": "prov-a", "keys": ["nope"]},
        has_body=True,
    )
    case(
        "probes/start-missing-provider",
        "POST",
        "/api/probes/keys",
        body={"provider_id": "nope", "keys": []},
        has_body=True,
    )
    case(
        "probes/start-bad-timeout",
        "POST",
        "/api/probes/keys",
        body={"config_revision": "$REV", "provider_id": "prov-a", "timeout_seconds": 0},
        has_body=True,
    )
    case(
        "probes/get-complete",
        "GET",
        f"/api/probes/{FIXED_UUID.hex}",
        covers=["GET /api/probes/{probe_id}"],
        setup=[
            (
                "POST",
                "/api/probes/keys",
                {"provider_id": "prov-a", "keys": ["key-a"], "timeout_seconds": 5},
            )
        ],
    )
    case(
        "probes/get-failed",
        "GET",
        f"/api/probes/{FIXED_UUID.hex}",
        setup=[
            (
                "POST",
                "/api/probes/keys",
                {"provider_id": "prov-a", "keys": ["key-a"], "timeout_seconds": 5},
            )
        ],
        patch_state={"fail_capabilities": True},
    )
    case(
        "probes/get-redacted-availability-error",
        "GET",
        f"/api/probes/{FIXED_UUID.hex}",
        setup=[
            (
                "POST",
                "/api/probes/keys",
                {"provider_id": "prov-a", "keys": ["key-a"], "timeout_seconds": 5},
            )
        ],
        patch_state={
            "availability_error": "Authorization: Bearer sk-secret-a rejected for key-a"
        },
    )
    case("probes/get-missing", "GET", "/api/probes/deadbeefdeadbeefdeadbeefdeadbeef")
    case(
        "probes/cancel-complete",
        "POST",
        f"/api/probes/{FIXED_UUID.hex}/cancel",
        covers=["POST /api/probes/{probe_id}/cancel"],
        setup=[
            (
                "POST",
                "/api/probes/keys",
                {"provider_id": "prov-a", "keys": ["key-a"], "timeout_seconds": 5},
            )
        ],
    )
    case("probes/cancel-missing", "POST", "/api/probes/deadbeefdeadbeefdeadbeefdeadbeef/cancel")
    case(
        "providers/probe",
        "POST",
        "/api/providers/prov-a/probe",
        body={"config_revision": "$REV"},
        has_body=True,
        covers=["POST /api/providers/{provider_id}/probe"],
    )
    case(
        "providers/probe-no-keys",
        "POST",
        "/api/providers/prov-a/probe",
        body={"config_revision": "$REV"},
        has_body=True,
        setup=[
            (
                "DELETE",
                "/api/providers/prov-a/keys/key-a",
                {"config_revision": "$REV"},
            ),
            (
                "DELETE",
                "/api/providers/prov-a/keys/key-b",
                {"config_revision": "$REV"},
            ),
        ],
    )
    case(
        "providers/probe-missing-provider",
        "POST",
        "/api/providers/nope/probe",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "provider-keys/probe",
        "POST",
        "/api/providers/prov-a/keys/key-a/probe",
        body={"config_revision": "$REV", "modes": ["openai"]},
        has_body=True,
        covers=["POST /api/providers/{provider_id}/keys/{key_name}/probe"],
    )
    case(
        "provider-keys/probe-no-modes",
        "POST",
        "/api/providers/prov-a/keys/key-a/probe",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "provider-keys/probe-modes-null",
        "POST",
        "/api/providers/prov-a/keys/key-a/probe",
        body={"config_revision": "$REV", "modes": None},
        has_body=True,
    )
    case(
        "provider-keys/probe-missing-key",
        "POST",
        "/api/providers/prov-a/keys/nope/probe",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "provider-keys/get-models",
        "GET",
        "/api/providers/prov-a/keys/key-a/models",
        covers=["GET /api/providers/{provider_id}/keys/{key_name}/models"],
    )
    case("provider-keys/get-models-missing-key", "GET", "/api/providers/prov-a/keys/nope/models")
    case(
        "provider-keys/set-models",
        "PUT",
        "/api/providers/prov-a/keys/key-a/models",
        body={"config_revision": "$REV", "models": ["model-a"]},
        has_body=True,
        covers=["PUT /api/providers/{provider_id}/keys/{key_name}/models"],
    )
    case(
        "provider-keys/set-models-missing-key",
        "PUT",
        "/api/providers/prov-a/keys/nope/models",
        body={"config_revision": "$REV", "models": ["model-a"]},
        has_body=True,
    )

    # —— 配置导入导出 ——
    case("config/export", "POST", "/api/config/export", covers=["POST /api/config/export"])
    case(
        "config/import",
        "POST",
        "/api/config/import",
        body={
            "config_revision": "$REV",
            "config": {
                "config_version": 4,
                "providers": {
                    "prov-z": {
                        "base_url": "https://z.example.test",
                        "keys": {"key-z": {"api_key": "sk-secret-z"}},
                    }
                },
                "models": {},
            },
        },
        has_body=True,
        covers=["POST /api/config/import"],
    )
    case(
        "config/import-bad-config",
        "POST",
        "/api/config/import",
        body={"config_revision": "$REV", "config": []},
        has_body=True,
    )
    case(
        "config/import-stale-revision",
        "POST",
        "/api/config/import",
        body={"config_revision": "$STALE", "config": {"config_version": 4}},
        has_body=True,
    )

    # —— 模型 ——
    case("models/list", "GET", "/api/models", covers=["GET /api/models"])
    case(
        "models/create",
        "POST",
        "/api/models",
        body={
            "config_revision": "$REV",
            "id": "model-c",
            "aliases": [" alias-c "],
            "hidden_aliases": [],
            "reasoning_effort": "high",
            "keys": [{"name": "key-e", "api_key": "sk-secret-e"}],
        },
        has_body=True,
        covers=["POST /api/models"],
    )
    case(
        "models/create-key-with-routes",
        "POST",
        "/api/models",
        body={
            "config_revision": "$REV",
            "id": "model-c",
            "keys": [
                {
                    "name": "key-e",
                    "api_key": "sk-secret-e",
                    "base_url": "https://e.example.test",
                    "upstream_routes": {"openai": "/v1/chat/completions"},
                }
            ],
        },
        has_body=True,
    )
    case(
        "models/create-key-revision-quirk",
        "POST",
        "/api/models",
        body={
            "config_revision": "$REV",
            "id": "model-c",
            "keys": [{"name": "key-e", "api_key": "sk-secret-e", "config_revision": "inner"}],
        },
        has_body=True,
    )
    case(
        "models/create-missing-keys-fields",
        "POST",
        "/api/models",
        body={"config_revision": "$REV", "id": "model-c", "keys": [{}]},
        has_body=True,
    )
    case(
        "models/create-extra-in-key",
        "POST",
        "/api/models",
        body={
            "config_revision": "$REV",
            "id": "model-c",
            "keys": [{"name": "k", "api_key": "s", "x": 1}],
        },
        has_body=True,
    )
    case(
        "models/create-null-aliases",
        "POST",
        "/api/models",
        body={"config_revision": "$REV", "id": "model-c", "aliases": None},
        has_body=True,
    )
    case(
        "models/create-aliases-string",
        "POST",
        "/api/models",
        body={"config_revision": "$REV", "id": "model-c", "aliases": "x"},
        has_body=True,
    )
    case(
        "models/create-duplicate",
        "POST",
        "/api/models",
        body={"config_revision": "$REV", "id": "model-a"},
        has_body=True,
    )
    case(
        "models/create-stale-revision",
        "POST",
        "/api/models",
        body={"config_revision": "$STALE", "id": "model-c"},
        has_body=True,
    )
    case("models/get", "GET", "/api/models/model-a", covers=["GET /api/models/{model_id}"])
    case("models/get-by-alias", "GET", "/api/models/alias-a")
    case("models/get-missing", "GET", "/api/models/nope")
    case(
        "models/update",
        "PUT",
        "/api/models/model-a",
        body={"config_revision": "$REV", "reasoning_effort": "high"},
        has_body=True,
        covers=["PUT /api/models/{model_id}"],
    )
    case(
        "models/update-empty",
        "PUT",
        "/api/models/model-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "models/update-null-id",
        "PUT",
        "/api/models/model-a",
        body={"config_revision": "$REV", "id": None},
        has_body=True,
    )
    case(
        "models/update-extra-field",
        "PUT",
        "/api/models/model-a",
        body={"api_key": None},
        has_body=True,
    )
    case(
        "models/update-rename",
        "PUT",
        "/api/models/model-a",
        body={"config_revision": "$REV", "id": "model-a2"},
        has_body=True,
    )
    case(
        "models/update-stale-revision",
        "PUT",
        "/api/models/model-a",
        body={"config_revision": "$STALE", "aliases": []},
        has_body=True,
    )
    case("models/delete-no-body", "DELETE", "/api/models/model-a", covers=["DELETE /api/models/{model_id}"])
    case(
        "models/delete-with-revision",
        "DELETE",
        "/api/models/model-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case("models/delete-empty-body", "DELETE", "/api/models/model-a", body={}, has_body=True)
    case(
        "models/delete-stale-revision",
        "DELETE",
        "/api/models/model-a",
        body={"config_revision": "$STALE"},
        has_body=True,
    )
    case("model-keys/list", "GET", "/api/models/model-a/keys", covers=["GET /api/models/{model_id}/keys"])
    case("model-keys/list-missing-model", "GET", "/api/models/nope/keys")
    case(
        "model-keys/create",
        "POST",
        "/api/models/model-a/keys",
        body={
            "config_revision": "$REV",
            "name": "key-f",
            "api_key": "sk-secret-f",
            "base_url": "https://f.example.test",
        },
        has_body=True,
        covers=["POST /api/models/{model_id}/keys"],
    )
    case(
        "model-keys/create-blank-name",
        "POST",
        "/api/models/model-a/keys",
        body={"config_revision": "$REV", "name": "  ", "api_key": "sk-secret-f"},
        has_body=True,
    )
    case(
        "model-keys/create-blank-api-key",
        "POST",
        "/api/models/model-a/keys",
        body={"config_revision": "$REV", "name": "key-f", "api_key": ""},
        has_body=True,
    )
    case(
        "model-keys/create-null-base-url-ok",
        "POST",
        "/api/models/model-a/keys",
        body={"config_revision": "$REV", "name": "key-f", "api_key": "sk-secret-f", "base_url": None},
        has_body=True,
    )
    case(
        "model-keys/get",
        "GET",
        "/api/models/model-a/keys/key-a",
        covers=["GET /api/models/{model_id}/keys/{key_name}"],
    )
    case("model-keys/get-missing", "GET", "/api/models/model-a/keys/nope")
    case(
        "model-keys/stats",
        "GET",
        "/api/models/model-a/keys/key-a/stats",
        covers=["GET /api/models/{model_id}/keys/{key_name}/stats"],
    )
    case("model-keys/stats-hours", "GET", "/api/models/model-a/keys/key-a/stats?hours=24")
    case("model-keys/stats-bad-hours", "GET", "/api/models/model-a/keys/key-a/stats?hours=abc")
    case("model-keys/stats-missing-key", "GET", "/api/models/model-a/keys/nope/stats")
    case(
        "model-keys/update",
        "PUT",
        "/api/models/model-a/keys/key-a",
        body={"config_revision": "$REV", "api_key": "sk-rotated"},
        has_body=True,
        covers=["PUT /api/models/{model_id}/keys/{key_name}"],
    )
    case(
        "model-keys/update-empty",
        "PUT",
        "/api/models/model-a/keys/key-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "model-keys/update-rename",
        "PUT",
        "/api/models/model-a/keys/key-a",
        body={"config_revision": "$REV", "name": "key-a2"},
        has_body=True,
    )
    case(
        "model-keys/update-null-name",
        "PUT",
        "/api/models/model-a/keys/key-a",
        body={"config_revision": "$REV", "name": None},
        has_body=True,
    )
    case(
        "model-keys/update-missing-key",
        "PUT",
        "/api/models/model-a/keys/nope",
        body={"config_revision": "$REV", "enabled": False},
        has_body=True,
    )
    case(
        "model-keys/delete-no-body",
        "DELETE",
        "/api/models/model-a/keys/key-a",
        covers=["DELETE /api/models/{model_id}/keys/{key_name}"],
    )
    case(
        "model-keys/delete-with-revision",
        "DELETE",
        "/api/models/model-a/keys/key-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )
    case(
        "model-keys/delete-stale-revision",
        "DELETE",
        "/api/models/model-a/keys/key-a",
        body={"config_revision": "$STALE"},
        has_body=True,
    )

    # —— 鉴权（每条管理路由都要求 full）——
    case("auth/no-header", "GET", "/api/models", auth="none")
    case("auth/visitor", "GET", "/api/models", auth="visitor")
    case("auth/visitor-unified", "GET", "/api/unified-model", auth="visitor")
    case("auth/no-header-put", "PUT", "/api/settings", body={"config_revision": "$REV", "port": 9000}, has_body=True, auth="none")

    # —— 配置文件缺失 ——
    case("config-missing/read", "GET", "/api/models", delete_config=True)
    case(
        "config-missing/write",
        "PUT",
        "/api/settings",
        body={"config_revision": "$REV", "port": 9000},
        has_body=True,
        delete_config=True,
    )

    # —— 配置文件被外部改坏 ——
    #
    # 应用启动时配置是合法的，之后磁盘上的配置被改坏——这正是管理 API 每次请求都
    # 重新读盘的原因。这里覆盖「裸 ValueError 在不同深度分别表现为 500 与 400」：
    # * `models` 不是对象时，_routes() 在路由函数里抛 ValueError -> 顶层 -> 500；
    # * 同样输入走 _update_config 时被 `except ValueError` 捕获 -> 400 配置校验失败。
    case(
        "broken/models-not-object-get-routes",
        "GET",
        "/api/routes",
        config_replace={**fixture(), "models": "not-an-object"},
    )
    case(
        "broken/models-not-object-put-routes",
        "PUT",
        "/api/routes/model-a",
        body={"config_revision": "$REV", "aliases": ["x"]},
        has_body=True,
        config_replace={**fixture(), "models": "not-an-object"},
    )
    case(
        "broken/models-not-object-get-model",
        "GET",
        "/api/models/model-a",
        config_replace={**fixture(), "models": "not-an-object"},
    )
    case(
        "broken/providers-not-object-get-providers",
        "GET",
        "/api/providers",
        config_replace={**fixture(), "providers": "not-an-object"},
    )
    case(
        "broken/providers-not-object-get-provider",
        "GET",
        "/api/providers/prov-a",
        config_replace={**fixture(), "providers": "not-an-object"},
    )
    case(
        "broken/providers-not-object-put-provider",
        "PUT",
        "/api/providers/prov-a",
        body={"config_revision": "$REV", "base_url": "https://a2.example.test"},
        has_body=True,
        config_replace={**fixture(), "providers": "not-an-object"},
    )
    case(
        "providers/update-no-fields",
        "PUT",
        "/api/providers/prov-a",
        body={"config_revision": "$REV"},
        has_body=True,
    )


# —— 执行 —— #


async def run_case(entry: dict, index: int) -> dict:
    reset_base_dir()
    data = migrate_config_data(fixture())
    save_config_data(CONFIG_PATH, data)
    PATCH_STATE["fail_capabilities"] = bool(entry["patch_state"].get("fail_capabilities"))
    PATCH_STATE["availability_error"] = entry["patch_state"].get("availability_error")

    app = create_app(RouterConfig.load(CONFIG_PATH), CONFIG_PATH)
    result: dict = {
        "name": entry["name"],
        "method": entry["method"],
        "path": entry["path"],
        "auth": entry["auth"],
        "covers": entry["covers"],
        "patch_state": entry["patch_state"],
        "delete_config": entry["delete_config"],
        "config_replace": entry["config_replace"],
        "setup": [],
    }
    async with app.router.lifespan_context(app):
        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        async with httpx.AsyncClient(transport=transport, base_url="http://testserver") as client:
            # 应用已经带合法配置启动；现在把磁盘上的配置换成坏版本，模拟外部手改。
            if entry["config_replace"] is not None:
                save_config_data(CONFIG_PATH, migrate_config_data(entry["config_replace"]))
            headers = dict(FULL_AUTH)
            if entry["auth"] == "none":
                headers = {}
            elif entry["auth"] == "visitor":
                headers = dict(VISITOR_AUTH)

            async def send_payload(
                method: str, path: str, payload: object, with_body: bool
            ) -> httpx.Response:
                if not with_body:
                    return await client.request(method, path, headers=headers)
                return await client.request(
                    method,
                    path,
                    content=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
                    headers={**headers, "content-type": "application/json"},
                )

            for method, path, body in entry["setup"]:
                # $REV 逐**步**解析：准备请求本身会改配置，所以第二步的版本号与第一步
                # 不同。语料里记的必须是真正发出去的那份载荷，否则 Go 侧回放到第二步
                # 就会因为版本过期而 409，后续状态随之发散。
                resolved = substitute(body)
                result["setup"].append(
                    {"method": method, "path": path, "body": {"kind": "json", "value": resolved}}
                )
                await send_payload(method, path, resolved, True)
                await settle_probes(app)

            # $REV 必须在删掉配置文件**之前**解析：这两个用例要覆盖「配置不见了」
            # 的 409，但用例本身仍然基于一份具体的初始版本。
            main_payload = (
                substitute(entry["body"]["value"]) if entry["body"]["kind"] == "json" else None
            )
            result["body"] = (
                {"kind": "json", "value": main_payload}
                if entry["body"]["kind"] == "json"
                else {"kind": "none"}
            )
            if entry["delete_config"]:
                CONFIG_PATH.unlink()

            response = await send_payload(
                entry["method"],
                entry["path"],
                main_payload,
                entry["body"]["kind"] == "json",
            )
            await settle_probes(app)

            result["status"] = response.status_code
            result["content_type"] = response.headers.get("content-type")
            result["content_length"] = response.headers.get("content-length")
            result["body_text"] = response.content.decode("utf-8")
            if entry["method"] in ("POST", "PUT", "DELETE"):
                result["config_after"] = config_after()
            else:
                result["config_after"] = None
    PATCH_STATE["fail_capabilities"] = False
    PATCH_STATE["availability_error"] = None
    return result


async def settle_probes(app) -> None:
    """等待后台探测任务结束，让后续的 GET /api/probes/{id} 看到终态。"""
    for task in list(app.state.management_probe_tasks.values()):
        try:
            await task
        except Exception:  # noqa: BLE001 - 任务内部已把异常写进 record
            pass


def config_after() -> str | None:
    if not CONFIG_PATH.is_file():
        return None
    return canonical_dumps(json.loads(CONFIG_PATH.read_text(encoding="utf-8")))


async def build_corpus() -> dict:
    patch_dependencies()
    build_cases()
    results = []
    for index, entry in enumerate(CASES):
        results.append(await run_case(entry, index))
    covered = {pattern for result in results for pattern in result["covers"]}
    missing = sorted(set(ROUTE_PATTERNS) - covered)
    if missing:
        raise SystemExit(f"语料未覆盖以下路由: {missing}")
    return {
        "version": CORPUS_VERSION,
        "note": (
            "由 scripts/gen_management_api_corpus.py 驱动真实 Python management_api 生成；"
            "config_after 为 canonical（sort_keys + 紧凑分隔符）形式，None 表示非变异请求。"
            "fixture_dir 是生成时的固定临时目录，Go 侧回放时应替换成自己的临时目录。"
        ),
        "fixture_dir": str(BASE_DIR),
        "fixture": fixture(),
        "case_count": len(results),
        "cases": results,
    }


# 47 条路由的「方法 + 模式」清单；与 internal/api/router.go 的 routePatterns() 手工对齐。
ROUTE_PATTERNS = [
    "GET /api/unified-model",
    "PUT /api/unified-model",
    "DELETE /api/unified-model",
    "GET /api/tasks",
    "POST /api/tasks",
    "GET /api/tasks/{task_name}",
    "PUT /api/tasks/{task_name}",
    "DELETE /api/tasks/{task_name}",
    "GET /api/settings",
    "PUT /api/settings",
    "POST /api/settings/local-api-key",
    "POST /api/update/check",
    "GET /api/providers",
    "POST /api/providers",
    "GET /api/providers/{provider_id}",
    "PUT /api/providers/{provider_id}",
    "DELETE /api/providers/{provider_id}",
    "GET /api/providers/{provider_id}/keys",
    "POST /api/providers/{provider_id}/keys",
    "GET /api/providers/{provider_id}/keys/{key_name}",
    "PUT /api/providers/{provider_id}/keys/{key_name}",
    "DELETE /api/providers/{provider_id}/keys/{key_name}",
    "GET /api/routes",
    "POST /api/routes",
    "GET /api/routes/{route_id}",
    "PUT /api/routes/{route_id}",
    "DELETE /api/routes/{route_id}",
    "POST /api/probes/keys",
    "POST /api/providers/{provider_id}/probe",
    "POST /api/providers/{provider_id}/keys/{key_name}/probe",
    "GET /api/providers/{provider_id}/keys/{key_name}/models",
    "PUT /api/providers/{provider_id}/keys/{key_name}/models",
    "GET /api/probes/{probe_id}",
    "POST /api/probes/{probe_id}/cancel",
    "POST /api/config/export",
    "POST /api/config/import",
    "POST /api/models",
    "GET /api/models/{model_id}",
    "PUT /api/models/{model_id}",
    "DELETE /api/models/{model_id}",
    "GET /api/models/{model_id}/keys",
    "POST /api/models/{model_id}/keys",
    "GET /api/models/{model_id}/keys/{key_name}",
    "GET /api/models/{model_id}/keys/{key_name}/stats",
    "PUT /api/models/{model_id}/keys/{key_name}",
    "DELETE /api/models/{model_id}/keys/{key_name}",
]


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="只校验磁盘语料是否与实现一致")
    parser.add_argument("--out", type=Path, default=DEFAULT_PATH)
    args = parser.parse_args()

    corpus = anyio.run(build_corpus)
    text = render(corpus)
    if args.check:
        if not args.out.is_file():
            print(f"语料缺失: {args.out}", file=sys.stderr)
            return 1
        existing = args.out.read_text(encoding="utf-8")
        if existing != text:
            print(f"语料已过期: {args.out}", file=sys.stderr)
            return 1
        print(f"语料一致（{corpus['case_count']} 条）")
        return 0
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(text, encoding="utf-8", newline="\n")
    print(f"已写入 {args.out}（{corpus['case_count']} 条）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
