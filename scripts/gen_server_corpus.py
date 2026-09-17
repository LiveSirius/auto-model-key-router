#!/usr/bin/env python3
"""生成 ``auto_model_key_router.app`` **app 面**路由的差分对拍语料。

这个脚本驱动**真实的** Python FastAPI 应用（``create_app`` +
``starlette.testclient.TestClient``），对一个临时配置文件逐条发请求，把「状态码 +
响应体 + content-type + content-length」记进
``internal/server/testdata/server_corpus.json``；Go 侧用 httptest 重放同一批请求并
逐字节比对。

覆盖范围是**装配层自己实现的路由**（app.py 里除了 proxy 与 /health 之外的那几条），
以及「装配层有没有把别人的路由接对」的接线用例：

* ``HEAD /``（以及其它方法打在 ``/`` 上的 405）
* ``GET /health`` 的**方法**语义（GET 之外的 405；响应体本身由 internal/health 的
  语料负责，不在这里重复）
* ``GET /v1/models``（完整权限 / 访客 / 无凭据 / 错凭据 / 未配置模型 / 外部改配置后
  热重载）
* ``GET /metrics``、``GET /metrics/requests``、``GET /metrics/series``
  （鉴权失败、hours 与其它数值边界、非数字、``all_history``、多错误顺序、
  「校验先于鉴权」、未知参数被忽略、500 点位上限）
* ``/api/*`` 打到管理 API（不是被 app 面兜底吞掉）
* ``/v1/{path}`` catch-all 打到 proxy（且**只**在 app 面没有更具体的路由时）

设计要点：

1. **固定路径**。所有用例共用同一个临时目录
   (``%TEMP%/amkr_server_corpus``)，每轮开始前清空。``/health`` 的 config_path 与
   metrics 快照的 database_path 都会出现在响应体里，用 mkdtemp 会让语料随运行变化，
   ``--check`` 就永远失败。
2. **不断言上游调用**。这些路由**一个上游请求都不会发**（打到 proxy 的两条用例在
   选 key 之前就返回了），脚本末尾用「metrics 表行数恒为 0」把这一点钉住——如果哪天
   某条用例真的打到了上游，这里会直接报错。
3. **只能归一化时间与路径**。响应体里的 ``window.from/to``、``started_at``、系列
   点位时间戳都来自**墙钟**，而两侧都没有跨 HTTP 边界的可注入时钟（Go 侧
   internal/metrics 的时钟是包内私有变量，外部测试改不了）。因此语料把这些位置替换
   成占位符（``<TIMESTAMP>`` / ``<FIXTURE_DIR>``），并在
   ``body_normalized`` 里标记。代价：被归一化的响应不再比对 content-length
   （它随占位符长度变化），Go 侧只在未归一化时断言 content-length。
   其余所有字节——键序、`hours` 的浮点写法（``24.0`` / ``1e-05``）、错误信封、
   Pydantic 错误文本——都逐字节比对。
4. **每个用例一个全新应用**。度量库、key pool 游标、配置 mtime 都是进程内状态，
   复用应用会让用例之间互相污染（尤其是 ``config_replace`` 用例会改写配置文件）。

用法::

    python -X utf8 scripts/gen_server_corpus.py
    python -X utf8 scripts/gen_server_corpus.py --check
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import sqlite3
import sys
import tempfile
import time
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from starlette.testclient import TestClient  # noqa: E402

from auto_model_key_router.app import create_app  # noqa: E402
from auto_model_key_router.config import (  # noqa: E402
    RouterConfig,
    migrate_config_data,
    save_config_data,
)

DEFAULT_PATH = REPO_ROOT / "internal" / "server" / "testdata" / "server_corpus.json"
CORPUS_VERSION = 1
BASE_DIR = Path(tempfile.gettempdir()) / "amkr_server_corpus"
CONFIG_PATH = BASE_DIR / "router-config.json"
METRICS_PATH = BASE_DIR / "metrics.sqlite3"

FIXED_LOCAL_API_KEY = "local-key"
FULL_AUTH = {"Authorization": f"Bearer {FIXED_LOCAL_API_KEY}"}
VISITOR_AUTH = {"Authorization": "Bearer amkr-visitor"}
WRONG_AUTH = {"Authorization": "Bearer not-the-key"}

# 归一化占位符。Go 侧（internal/server/corpus_test.go）用同名常量做同样的替换。
DIR_PLACEHOLDER = "<FIXTURE_DIR>"
TIMESTAMP_PLACEHOLDER = "<TIMESTAMP>"
TIMESTAMP_RE = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[+-]\d{2}:\d{2})?")


def fixture() -> dict:
    """所有用例共用的初始配置。

    刻意塞进四种模型形态，让 ``/v1/models`` 的装配差异无处可藏：

    * ``model-a``：普通模型 + 可见别名 + **隐藏别名**；
    * ``model-b``：**没有任何 key**（未配置）——不应出现在任何清单里；
    * ``model-v``：唯一带 ``allow_visitor`` key 的模型——访客清单里是
      ``amkr-model-v``；
    * ``model-nv``：有 key 但不允许访客——只出现在完整清单里。

    ``unified_model`` 同时把 ``unified-model`` 这个名字带进完整清单的**末尾**
    （app.py 的 key_pool.available_model_ids 用 append，不参与排序）。
    """
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
        "metrics_db_path": str(METRICS_PATH),
        "log_file_path": str(BASE_DIR / "server.log"),
        "local_api_key": FIXED_LOCAL_API_KEY,
        "providers": {
            "prov-a": {
                "base_url": "https://a.example.test",
                "keys": {
                    "key-a": {"api_key": "sk-secret-a", "enabled": True},
                    "key-b": {"api_key": "sk-secret-b", "enabled": True, "allow_visitor": True},
                },
                "routes": {
                    "responses": "v1/responses",
                    "openai": "v1/chat/completions",
                },
            },
            "prov-b": {
                "base_url": "https://b.example.test",
                "keys": {"key-c": {"api_key": "sk-secret-c", "enabled": True}},
                "routes": {},
            },
        },
        "models": {
            "model-a": {
                "targets": [
                    {"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}
                ],
                "aliases": ["alias-a", "alias-hidden"],
                "hidden_aliases": ["alias-hidden"],
                "routing_mode": "round_robin",
            },
            "model-b": {
                "targets": [],
                "aliases": ["alias-b"],
            },
            "model-v": {
                "targets": [
                    {"provider": "prov-a", "key": "key-b", "upstream_model": "model-v"}
                ],
                "aliases": ["alias-v"],
            },
            "model-nv": {
                "targets": [
                    {"provider": "prov-b", "key": "key-c", "upstream_model": "model-nv"}
                ],
                "aliases": ["alias-nv"],
            },
        },
        "unified_model": {
            "default": {"primary": {"model": "model-a", "key": "key-a"}},
        },
    }


def empty_fixture() -> dict:
    """没有任何 provider / model 的配置，用来覆盖「外部改配置后热重载」。"""
    data = fixture()
    data["providers"] = {}
    data["models"] = {}
    data.pop("unified_model", None)
    return data


# —— 用例定义 —— #

CASES: list[dict] = []


def case(
    name: str,
    method: str,
    path: str,
    *,
    auth: str = "full",
    config_replace: dict | None = None,
    covers: list[str] | None = None,
) -> None:
    CASES.append(
        {
            "name": name,
            "method": method,
            "path": path,
            "auth": auth,
            "config_replace": config_replace,
            "covers": covers or [],
        }
    )


def build_cases() -> None:
    # —— 根路径与方法语义 —— #
    case("head_root", "HEAD", "/", auth="none", covers=["HEAD /"])
    case("get_root_is_405", "GET", "/", auth="none", covers=["GET /"])
    case("post_root_is_405", "POST", "/", auth="none")
    # HEAD /health：Go 的 ServeMux 默认会让 GET 模式也匹配 HEAD，参照实现不会。
    case("head_health_is_405", "HEAD", "/health", auth="none", covers=["HEAD /health"])
    case("post_health_is_405", "POST", "/health", auth="none", covers=["POST /health"])
    case("options_health_is_405", "OPTIONS", "/health", auth="none")
    case("options_root_is_405", "OPTIONS", "/", auth="none")
    case("head_models_is_405", "HEAD", "/v1/models", auth="none")
    case("options_models_is_405", "OPTIONS", "/v1/models", auth="none")
    case("head_metrics_is_405", "HEAD", "/metrics", auth="full")

    # —— /v1/models —— #
    case("models_full", "GET", "/v1/models", covers=["GET /v1/models"])
    case("models_visitor", "GET", "/v1/models", auth="visitor")
    case("models_no_auth", "GET", "/v1/models", auth="none")
    case("models_wrong_key", "GET", "/v1/models", auth="wrong")
    case("models_unknown_query_ignored", "GET", "/v1/models?bogus=1")
    # 外部改配置后（mtime 变化）热重载：模型清单立刻变空，且不需要重启。
    case(
        "models_after_external_config_edit",
        "GET",
        "/v1/models",
        config_replace=empty_fixture(),
    )

    # —— /metrics —— #
    case("metrics_default", "GET", "/metrics", covers=["GET /metrics"])
    case("metrics_no_auth", "GET", "/metrics", auth="none")
    case("metrics_visitor_is_401", "GET", "/metrics", auth="visitor")
    case("metrics_hours_fraction", "GET", "/metrics?hours=24.5")
    case("metrics_hours_plus", "GET", "/metrics?hours=%2B5")
    case("metrics_hours_tiny", "GET", "/metrics?hours=1e-5")
    case("metrics_hours_at_max", "GET", "/metrics?hours=8760")
    case("metrics_hours_zero", "GET", "/metrics?hours=0")
    case("metrics_hours_negative", "GET", "/metrics?hours=-1")
    case("metrics_hours_above_max", "GET", "/metrics?hours=8761")
    case("metrics_hours_nonnumeric", "GET", "/metrics?hours=abc")
    case("metrics_hours_empty", "GET", "/metrics?hours=")
    case("metrics_hours_nan", "GET", "/metrics?hours=nan")
    case("metrics_hours_inf", "GET", "/metrics?hours=inf")
    case("metrics_hours_negative_inf", "GET", "/metrics?hours=-inf")
    case("metrics_hours_out_of_range_literal", "GET", "/metrics?hours=1e400")
    case("metrics_hours_hex_float", "GET", "/metrics?hours=0x1p-2")
    case("metrics_hours_underscores", "GET", "/metrics?hours=1_0")
    case("metrics_hours_duplicate_takes_last", "GET", "/metrics?hours=1&hours=2")
    case("metrics_hours_huge_integer", "GET", "/metrics?hours=999999999999999999999")
    case("metrics_all_history_true", "GET", "/metrics?all_history=true&hours=5")
    case("metrics_all_history_false", "GET", "/metrics?all_history=false")
    case("metrics_all_history_yes", "GET", "/metrics?all_history=yes")
    case("metrics_all_history_single_letter", "GET", "/metrics?all_history=t")
    case("metrics_all_history_invalid", "GET", "/metrics?all_history=maybe")
    case("metrics_all_history_empty", "GET", "/metrics?all_history=")
    case("metrics_multiple_errors_in_declaration_order", "GET",
         "/metrics?hours=0&all_history=maybe")
    # FastAPI 先解析参数再执行路由函数，因此非法参数在**没有凭据**时也是 422 而不是 401。
    case("metrics_validation_precedes_auth", "GET", "/metrics?hours=0", auth="none")
    case("metrics_unknown_param_ignored", "GET", "/metrics?hours=1&bogus=2")

    # —— /metrics/requests —— #
    case("requests_default", "GET", "/metrics/requests", covers=["GET /metrics/requests"])
    case("requests_no_auth", "GET", "/metrics/requests", auth="none")
    case("requests_hours_at_max", "GET", "/metrics/requests?hours=720")
    case("requests_hours_above_max", "GET", "/metrics/requests?hours=721")
    case("requests_hours_zero", "GET", "/metrics/requests?hours=0")
    case("requests_limit_fraction", "GET", "/metrics/requests?limit=1.5")
    case("requests_limit_zero", "GET", "/metrics/requests?limit=0")
    case("requests_limit_above_max", "GET", "/metrics/requests?limit=201")
    case("requests_limit_nonnumeric", "GET", "/metrics/requests?limit=abc")
    case("requests_limit_integral_float", "GET", "/metrics/requests?limit=50.0")
    case("requests_before_id_zero", "GET", "/metrics/requests?before_id=0")
    case("requests_status_code_ok", "GET", "/metrics/requests?status_code=100")
    case("requests_status_code_low", "GET", "/metrics/requests?status_code=99")
    case("requests_status_code_high", "GET", "/metrics/requests?status_code=600")
    case("requests_caller_type_visitor", "GET", "/metrics/requests?caller_type=visitor")
    case("requests_caller_type_invalid", "GET", "/metrics/requests?caller_type=LOCAL")
    case("requests_caller_type_empty", "GET", "/metrics/requests?caller_type=")
    case("requests_model_id_empty", "GET", "/metrics/requests?model_id=")
    case("requests_model_id_space", "GET", "/metrics/requests?model_id=%20")
    case("requests_model_id_too_long", "GET", "/metrics/requests?model_id=" + "x" * 513)
    case("requests_model_id_multibyte_too_long", "GET",
         "/metrics/requests?model_id=" + "%C3%A9" * 513)
    case("requests_success_true", "GET", "/metrics/requests?success=true")
    case("requests_success_invalid", "GET", "/metrics/requests?success=banana")
    case("requests_attributed_false", "GET", "/metrics/requests?attributed=off")
    case("requests_all_history_true", "GET", "/metrics/requests?all_history=1")
    case(
        "requests_all_filters",
        "GET",
        "/metrics/requests"
        "?hours=2&caller_type=local&model_id=model-a&requested_model_id=alias-a"
        "&provider_id=prov-a&pool_name=pool-x&upstream_model_id=model-a&key_name=key-a"
        "&status_code=200&success=true&attributed=false&limit=5&before_id=7",
    )

    # —— /metrics/series —— #
    case("series_default", "GET", "/metrics/series", covers=["GET /metrics/series"])
    case("series_no_auth", "GET", "/metrics/series", auth="none")
    case("series_hours_zero", "GET", "/metrics/series?hours=0")
    case("series_bucket_low", "GET", "/metrics/series?bucket_seconds=14")
    case("series_bucket_high", "GET", "/metrics/series?bucket_seconds=86401")
    case("series_bucket_nonnumeric", "GET", "/metrics/series?bucket_seconds=abc")
    case("series_bucket_integral_float", "GET", "/metrics/series?bucket_seconds=3600.0")
    case("series_bucket_daily", "GET", "/metrics/series?bucket_seconds=86400")
    # hours × bucket 超过 500 个点位：唯一把存储层 ValueError 翻成 422 的路由。
    case("series_too_many_points", "GET", "/metrics/series?hours=100&bucket_seconds=15")
    case("series_status_and_caller", "GET",
         "/metrics/series?caller_type=visitor&status_code=500&bucket_seconds=3600")
    # series 没有 all_history 参数：未知参数被完全忽略（不是 422，也不生效）。
    case("series_unknown_params_ignored", "GET",
         "/metrics/series?all_history=true&bucket_seconds=86400")

    # —— 接线：管理 API 与代理 catch-all —— #
    case("api_route_not_shadowed", "GET", "/api/providers", auth="none",
         covers=["GET /api/providers"])
    case("api_fallback_is_json_404", "GET", "/api/does-not-exist")
    case("proxy_catchall_missing_model", "GET", "/v1/does-not-exist",
         covers=["GET /v1/{path}"])
    case("proxy_catchall_post_models", "POST", "/v1/models")
    # /v1/models 的「混合路径」语义：PUT 不归 app 面，而与 POST 一样落进通配路由。
    case("proxy_catchall_put_models", "PUT", "/v1/models")


# —— 归一化 —— #


def json_escaped(path: Path) -> str:
    """返回路径在 JSON 字符串里的样子（反斜杠转义）。

    Windows 上 ``C:\\Users\\...`` 在响应体里是 ``C:\\\\Users\\\\...``，直接替换原始
    路径一个字节也匹配不到。
    """
    return json.dumps(str(path), ensure_ascii=False)[1:-1]


def normalize(body: str) -> str:
    """把墙钟时间与夹具目录替换成占位符；返回归一化后的响应体。"""
    return TIMESTAMP_RE.sub(
        TIMESTAMP_PLACEHOLDER, body.replace(json_escaped(BASE_DIR), DIR_PLACEHOLDER)
    )


# —— 执行 —— #


def reset_base_dir() -> None:
    shutil.rmtree(BASE_DIR, ignore_errors=True)
    BASE_DIR.mkdir(parents=True, exist_ok=True)


def headers_for(auth: str) -> dict:
    if auth == "none":
        return {}
    if auth == "visitor":
        return dict(VISITOR_AUTH)
    if auth == "wrong":
        return dict(WRONG_AUTH)
    return dict(FULL_AUTH)


def metrics_row_count() -> int:
    if not METRICS_PATH.is_file():
        return 0
    connection = sqlite3.connect(f"file:{METRICS_PATH}?mode=ro", uri=True)
    try:
        return int(connection.execute("SELECT COUNT(*) FROM request_metrics").fetchone()[0])
    finally:
        connection.close()


def run_case(entry: dict) -> dict:
    reset_base_dir()
    save_config_data(CONFIG_PATH, migrate_config_data(fixture()))
    app = create_app(RouterConfig.load(CONFIG_PATH), CONFIG_PATH)

    with TestClient(app) as client:
        if entry["config_replace"] is not None:
            # 睡一小会儿再写：mtime 必须真的变化，热重载才会触发（两侧都一样）。
            time.sleep(0.03)
            save_config_data(CONFIG_PATH, migrate_config_data(entry["config_replace"]))
        response = client.request(
            entry["method"], entry["path"], headers=headers_for(entry["auth"])
        )

    body = response.content.decode("utf-8")
    normalized = normalize(body)
    upsteam_rows = metrics_row_count()
    if upsteam_rows != 0:
        raise SystemExit(
            f"用例 {entry['name']} 意外产生了 {upsteam_rows} 行指标——"
            "app 面路由不应触发任何上游调用"
        )
    return {
        "name": entry["name"],
        "method": entry["method"],
        "path": entry["path"],
        "auth": entry["auth"],
        "covers": entry["covers"],
        "config_replace": entry["config_replace"],
        "status": response.status_code,
        "content_type": response.headers.get("content-type"),
        "content_length": response.headers.get("content-length"),
        "body_text": normalized,
        "body_normalized": normalized != body,
    }


def build_corpus() -> dict:
    build_cases()
    results = [run_case(entry) for entry in CASES]
    covered = {pattern for result in results for pattern in result["covers"]}
    missing = sorted(set(ROUTE_PATTERNS) - covered)
    if missing:
        raise SystemExit(f"语料未覆盖以下路由: {missing}")
    return {
        "version": CORPUS_VERSION,
        "note": (
            "由 scripts/gen_server_corpus.py 驱动真实 Python FastAPI 应用生成。"
            "body_text 已把墙钟时间戳与夹具目录替换成 <TIMESTAMP> / <FIXTURE_DIR>"
            "（见脚本头部说明），body_normalized 标记该用例是否被归一化过。"
            "fixture_dir 是生成时的固定临时目录，Go 侧回放时应替换成自己的临时目录。"
        ),
        "fixture_dir": str(BASE_DIR),
        "fixture": fixture(),
        "empty_fixture": empty_fixture(),
        "case_count": len(results),
        "cases": results,
    }


# app 面路由清单（与 internal/server/routes.go 的注册一一对应）；用于断言语料覆盖面。
ROUTE_PATTERNS = [
    "HEAD /",
    "GET /",
    "HEAD /health",
    "POST /health",
    "GET /v1/models",
    "GET /metrics",
    "GET /metrics/requests",
    "GET /metrics/series",
    "GET /v1/{path}",
    "GET /api/providers",
]


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="只校验磁盘语料是否与实现一致")
    parser.add_argument("--out", type=Path, default=DEFAULT_PATH)
    args = parser.parse_args()

    corpus = build_corpus()
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
