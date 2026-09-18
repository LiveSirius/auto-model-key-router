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
* ``/ws/events`` 的**普通 HTTP** 请求（websocket 路由不匹配 http scope -> 兜底 404）

除此之外还有两块 WebSocket 语料（``ws_cases`` 与 ``ws_proxy_cases``）：它们必须用
``TestClient.websocket_connect`` 驱动，因为「真的升级」在 ASGI 上是另一个 scope，
普通 ``client.request`` 无论如何都做不到：

* ``ws_cases`` —— ``/ws/events`` 的握手：帧序（client_count / metrics_snapshot /
  connected）、4001/4003、访客 key 被拒。驱动的是**真实的** ``create_app``。
* ``ws_proxy_cases`` —— 升级到 ``/v1/{path}`` 的连接（``register_websocket_proxy`` +
  app.py:374 注入的真实 proxy），记录响应帧与关闭码。

一个**测试载体**的限制必须写在这里，否则后来者会以为它是缺陷：TestClient 只能发
HTTP scope，所以「带 ``Upgrade: websocket`` 头的普通 HTTP 请求」在 Python 侧落进 HTTP
路由（``GET /v1/does-not-exist`` -> 401），而真实 uvicorn 会把它升级成 websocket scope
（实测 -> ``HTTP/1.1 101 Switching Protocols``，依据是 uvicorn
``protocols/http/{h11,httptools}_impl.py`` 的 ``_get_upgrade``：``upgrade: websocket``
且 ``connection`` 含 ``upgrade``）。Go 侧没有协议层分流，按同一组头判定，因此与**真实**
Python 一致、与 TestClient 不一致。这类用例**刻意不入语料**。

设计要点：

1. **固定路径**。所有用例共用同一个临时目录
   (``%TEMP%/amkr_server_corpus``)，每轮开始前清空。``/health`` 的 config_path 与
   metrics 快照的 database_path 都会出现在响应体里，用 mkdtemp 会让语料随运行变化，
   ``--check`` 就永远失败。
2. **不断言上游调用**。这些路由**一个上游请求都不会发**（打到 proxy 的两条用例在
   选 key 之前就返回了），脚本末尾用「metrics 表行数恒为 0」把这一点钉住——如果哪天
   某条用例真的打到了上游，这里会直接报错。
3. **钉死时钟**。响应体里的 ``window.from/to``、``started_at``、系列点位时间戳、
   **点位数**都来自 ``metrics._now_beijing()``。点位数是真的会变的：``hours=1``
   配 ``bucket_seconds=86400`` 只在跨北京零点时横跨两个自然日，于是每天只有零点后
   那一小时是 2 个点位，其余时间是 1 个——语料因此曾经随运行时刻漂移。这里把
   ``metrics._now_beijing`` 全局换成固定时刻 ``NOW_BEIJING``（**全局固定，不做
   逐用例区分**，最简单也够用），并把该时刻写进语料的 ``now_beijing`` 字段，
   Go 侧用 ``metrics.SetNowForTest`` 钉到同一个瞬间。
   时刻选在北京刚过零点的 ``00:36``：1 小时窗口必然横跨两个自然日（跨零点布局），
   同时用 ``hours=0.5`` 的用例覆盖不跨零点的布局——两种日桶布局都被钉住。
   时间戳本身仍然归一化成占位符（``<TIMESTAMP>``），夹具目录归一化成
   ``<FIXTURE_DIR>``，回归由 ``body_normalized`` 标记。代价：被归一化的响应不再
   比对 content-length（它随占位符长度变化），Go 侧只在未归一化时断言
   content-length。其余所有字节——键序、`hours` 的浮点写法（``24.0`` / ``1e-05``）、
   错误信封、Pydantic 错误文本——都逐字节比对。
4. **每个用例一个全新应用**。度量库、key pool 游标、配置 mtime 都是进程内状态，
   复用应用会让用例之间互相污染（尤其是 ``config_replace`` 用例会改写配置文件）。

用法::

    python -X utf8 scripts/gen_server_corpus.py
    python -X utf8 scripts/gen_server_corpus.py --check
"""

from __future__ import annotations

import argparse
import base64
import json
import re
import shutil
import sqlite3
import sys
import tempfile
import time
from datetime import datetime
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from starlette.testclient import TestClient  # noqa: E402

from auto_model_key_router import metrics as metrics_module  # noqa: E402
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

# NOW_BEIJING 是语料钉死的「当前时刻」（见文件头第 3 点）。
#
# 故意选在**北京刚过零点**的尴尬时刻（00:36，且不在整点/整分上）：
#   * hours=1 + bucket_seconds=86400 的窗口 [前一天 23:36, 当天 00:36] 横跨两个
#     自然日 → 2 个日桶点位（第一个 complete=true、第二个 false），把跨零点布局钉住；
#   * hours=0.5 的窗口 [00:06, 00:36] 落在同一天 → 1 个日桶点位，把不跨零点布局钉住；
#   * 3600 / 60 秒桶也都不落在桶边界上，第一个点位是部分桶。
# 日期本身不承载语义（上海无夏令时，1970 年后恒为 +08:00），只要求是个合法日期。
NOW_BEIJING = datetime(2026, 3, 15, 0, 36, 0, tzinfo=metrics_module.BEIJING_TZ)

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
    # 同一固定时刻下**不跨零点**的日桶窗口（0.5 小时 = 30 分钟，窗口 [00:06, 00:36]
    # 落在同一天）：只有 1 个点位。与上面两条 hours=1 的跨零点用例配成一对，把两种
    # 日桶布局都钉死（这正是语料曾经每天只有零点后一小时能通过的根因）。
    case("series_bucket_daily_within_one_day", "GET",
         "/metrics/series?hours=0.5&bucket_seconds=86400")

    # —— 接线：管理 API 与代理 catch-all —— #
    case("api_route_not_shadowed", "GET", "/api/providers", auth="none",
         covers=["GET /api/providers"])
    case("api_fallback_is_json_404", "GET", "/api/does-not-exist")
    case("proxy_catchall_missing_model", "GET", "/v1/does-not-exist",
         covers=["GET /v1/{path}"])
    case("proxy_catchall_post_models", "POST", "/v1/models")
    # /v1/models 的「混合路径」语义：PUT 不归 app 面，而与 POST 一样落进通配路由。
    case("proxy_catchall_put_models", "PUT", "/v1/models")

    # —— /ws/events 上的普通 HTTP 请求 —— #
    #
    # websocket 路由**不匹配** http scope（Starlette 的 WebSocketRoute.matches 只在
    # scope["type"] == "websocket" 时返回匹配），因此这四条都落到 Starlette 的兜底 404。
    # 它们钉住的是「HTTP 请求不会被 WebSocket 路由吞掉」，以及 Go 侧
    # handleWSEvents 的非升级分支必须给出同一个 404。
    case("ws_events_http_get", "GET", "/ws/events", auth="none",
         covers=["GET /ws/events"])
    case("ws_events_http_post", "POST", "/ws/events", auth="none")
    case("ws_events_http_head", "HEAD", "/ws/events", auth="none")
    case("ws_events_http_query", "GET", "/ws/events?x=1", auth="none")
    # "/ws/events/" 没有尾斜杠路由，也不在 "/ws/events" 的子树里。
    case("ws_events_http_trailing_slash", "GET", "/ws/events/", auth="none")


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


def compact(value: object) -> str:
    """紧凑 JSON 文本，作为 WebSocket 语料 expect 的统一载体。

    它是**数据**而不是响应体，所以用紧凑分隔符；真正要逐字节比的是其中的
    ``frames`` 字段（那些帧文本本身已经是线上原样）。
    """
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


# —— 执行 —— #


def reset_base_dir() -> None:
    shutil.rmtree(BASE_DIR, ignore_errors=True)
    BASE_DIR.mkdir(parents=True, exist_ok=True)


def pin_clock(value: datetime = NOW_BEIJING) -> None:
    """把参照实现的时钟钉死在 ``value`` 上（对应测试里的 monkeypatch）。

    只替换模块级函数对象；``metrics.py`` 内部按全局名查找 ``_now_beijing``，
    所以之后构造的应用与查询都只看这个固定时刻，与真实墙钟彻底无关。
    """
    metrics_module._now_beijing = lambda: value


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


# —— WebSocket 语料 —— #
#
# 为什么不能沿用上面的 client.request：ASGI 的 websocket 与 http 是两个 scope，
# TestClient 的 ``websocket_connect`` 才会走 Starlette 的 websocket 路由。因此这些
# 用例单独驱动，并把「帧文本」当作逐字节契约（唯一的例外是 ws_json_null，见下）。

# ws_cases：/ws/events 的握手（app.py:325-349）。
WS_CASES: list[dict] = [
    {
        "name": "ws_auth_valid",
        "send": {"kind": "text", "text": '{"type": "auth", "token": "local-key"}'},
        "note": "合法首帧 + 完整权限：client_count / metrics_snapshot 是 authenticate() "
                "内部的回调广播的，connected 是路由补发的（app.py:339-342）",
        "covers": ["WS /ws/events"],
    },
    {
        "name": "ws_auth_wrong_token",
        "send": {"kind": "text", "text": '{"type": "auth", "token": "nope"}'},
        "note": "token 不对 -> 4003 auth failed（event_bus.py:50）",
    },
    {
        "name": "ws_auth_visitor_token",
        "send": {"kind": "text", "text": '{"type": "auth", "token": "amkr-visitor"}'},
        "note": "访客 key 能过 HTTP 的 /v1/models，但事件流只对 is_full 开放 -> 4003",
    },
    {
        "name": "ws_auth_bad_json",
        "send": {"kind": "text", "text": "{oops"},
        "note": "坏 JSON -> 4001（与超时同一个 except 分支）",
    },
    {
        "name": "ws_auth_type_mismatch",
        "send": {"kind": "text", "text": '{"type": "hello", "token": "local-key"}'},
        "note": "type 不是 auth -> 4003，且 verify 不会被调用（短路）",
    },
    {
        "name": "ws_auth_json_null",
        "send": {"kind": "text", "text": "null"},
        "divergence": (
            "首帧是合法 JSON 但不是对象：参照实现抛 AttributeError 冒泡，连接被 ASGI "
            "服务器异常关闭（不发关闭帧）；Go 侧在 eventbus.Authenticate 返回 "
            "ErrUnsupportedAuthFrame 后直接 CloseNow，同样不发关闭帧。客户端观测一致"
            "（无帧 + 异常终止），但 Python 的 TestClient 会把这个异常**抛回调用线程**，"
            "Go 侧只能观察到一个读错误，因此这一条的期望值不逐字节比对。"
        ),
        "note": "合法 JSON 但不是对象：AttributeError 冒泡，不关 4001/4003",
    },
]


def run_ws_case(entry: dict) -> dict:
    reset_base_dir()
    save_config_data(CONFIG_PATH, migrate_config_data(fixture()))
    app = create_app(RouterConfig.load(CONFIG_PATH), CONFIG_PATH)

    frames: list[str] = []
    close: dict | None = None
    raised: dict | None = None
    try:
        with TestClient(app) as client:
            with client.websocket_connect("/ws/events") as websocket:
                send = entry["send"]
                if send["kind"] == "text":
                    websocket.send_text(send["text"])
                else:
                    websocket.send_bytes(base64.b64decode(send["b64"]))
                while True:
                    try:
                        message = websocket.receive()
                    except WebSocketDisconnect as error:
                        close = {"code": error.code, "reason": error.reason}
                        break
                    if message["type"] == "websocket.close":
                        close = {"code": message.get("code"),
                                 "reason": message.get("reason")}
                        break
                    frames.append(message.get("text") or "")
                    # 认证成功后服务端不会主动关闭，看到 connected 就停（同
                    # eventbus/testdata/e2e.jsonl 的观测方式）。
                    if json.loads(frames[-1]).get("type") == "connected":
                        break
    except BaseException as error:  # noqa: BLE001
        raised = {"exception": type(error).__name__, "message": str(error)}

    # 帧文本里的 database_path/时间戳与夹具目录相关，必须归一化（与 HTTP 语料同一套）。
    normalized = [normalize(frame) for frame in frames]
    return {
        "name": entry["name"],
        "kind": "ws_auth",
        "note": entry["note"],
        "send": entry["send"],
        "covers": entry.get("covers", []),
        "divergence": entry.get("divergence", ""),
        "expect": compact({
            "frame_types": [json.loads(frame).get("type") for frame in normalized],
            "frames": normalized,
            "close": close,
            "raised": raised,
        }),
    }


def build_ws_cases() -> list[dict]:
    return [run_ws_case(entry) for entry in WS_CASES]


# ws_proxy_cases：升级到 /v1/{path} 的连接（websocket_proxy.py + app.py:374）。
#
# 上游用 httpx.MockTransport 替换掉当前代的 http_client（与 tests/test_app.py:479 同法），
# 因此不需要真的监听端口，也不会联网。记录的是「客户端看到的帧 + 关闭码」以及
# 「桩上游收到的请求」——后者是给 Go 侧对照用的，Go 那边用 httptest 桩上游，host:port
# 必然不同，所以只比 method/path/query/头部/body。
WS_PROXY_CASES: list[dict] = [
    {
        "name": "ws_proxy_json_ok",
        "url": "/v1/chat/completions?trace=1",
        "headers": {"Authorization": "Bearer local-key"},
        "send": {"kind": "text", "text": '{"model": "model-a", "messages": []}'},
        "upstream_status": 200,
        "upstream_body": '{"id": "ok"}',
        "note": "升级请求里的查询串会带到上游（websocket_proxy.py:51-67 复制整个 scope），"
                "非流式响应一帧文本 + 1000",
        "covers": ["WS /v1/{path}"],
    },
    {
        "name": "ws_proxy_auth_failure",
        "url": "/v1/chat/completions",
        "headers": {},
        "send": {"kind": "text", "text": '{"model": "model-a", "messages": []}'},
        "upstream_status": None,
        "upstream_body": None,
        "note": "没有凭据：proxy 回 401，帧里是错误信封，关闭码 1008（4xx -> 1008），"
                "且一次上游请求都不发",
    },
]


def run_ws_proxy_case(entry: dict) -> dict:
    import httpx

    reset_base_dir()
    save_config_data(CONFIG_PATH, migrate_config_data(fixture()))
    app = create_app(RouterConfig.load(CONFIG_PATH), CONFIG_PATH)

    upstream: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        raw_body = request.content.decode("utf-8", "replace")
        # 注意 request 是 **httpx** 的 Request（不是 Starlette 的），它的 url.query 是
        # bytes 而不是 str——gen_websocket_corpus.py 那边用的是 Starlette Request，
        # 所以那里可以直接入 JSON，这里必须先解码。
        query = request.url.query
        if isinstance(query, bytes):
            query = query.decode("utf-8", "replace")
        try:
            # 对象的**键序**在两条实现里可能不同（各自按自己的写入顺序序列化），而它
            # 不是契约；值才是。因此这里按 sort_keys=True 存，Go 侧用
            # canonical.Dumps（紧凑 + 键排序）对照。
            body = json.dumps(json.loads(raw_body), ensure_ascii=False,
                              sort_keys=True, separators=(",", ":"))
        except ValueError:
            body = raw_body
        upstream.append({
            "method": request.method,
            "path": request.url.path,
            "query": query,
            "authorization": request.headers.get("authorization"),
            "content_type": request.headers.get("content-type"),
            # 六个握手头必须被剔除（websocket_proxy.py:13-22）；记成 null 便于 Go 侧
            # 对照「上游收不到它们」。
            "upgrade": request.headers.get("upgrade"),
            "sec_websocket_key": request.headers.get("sec-websocket-key"),
            "body": body,
        })
        return httpx.Response(
            entry["upstream_status"],
            headers={"content-type": "application/json"},
            content=entry["upstream_body"].encode("utf-8"),
        )

    if entry["upstream_status"] is not None:
        app.state.runtime_manager.current.http_client = httpx.AsyncClient(
            transport=httpx.MockTransport(handler)
        )

    frames: list[str] = []
    close: dict | None = None
    raised: dict | None = None
    try:
        with TestClient(app) as client:
            with client.websocket_connect(entry["url"], headers=entry["headers"]) as websocket:
                send = entry["send"]
                if send["kind"] == "text":
                    websocket.send_text(send["text"])
                else:
                    websocket.send_bytes(base64.b64decode(send["b64"]))
                while True:
                    try:
                        message = websocket.receive()
                    except WebSocketDisconnect as error:
                        close = {"code": error.code, "reason": error.reason}
                        break
                    if message["type"] == "websocket.close":
                        close = {"code": message.get("code"),
                                 "reason": message.get("reason")}
                        break
                    frames.append(message.get("text") or "")
    except BaseException as error:  # noqa: BLE001
        raised = {"exception": type(error).__name__, "message": str(error)}

    return {
        "name": entry["name"],
        "kind": "ws_proxy",
        "note": entry["note"],
        "url": entry["url"],
        "headers": entry["headers"],
        "send": entry["send"],
        "covers": entry.get("covers", []),
        "divergence": entry.get("divergence", ""),
        # 桩上游的回应也记进语料：Go 侧的 httptest 桩必须回同一个东西，否则两边比
        # 的就不是同一个场景。httpx 的 json= 用 json.dumps 的默认分隔符，所以
        # body 里会带空格（`{"id": "ok"}`），Go 侧要照抄。
        "upstream_response": {
            "status": entry["upstream_status"],
            "content_type": "application/json" if entry["upstream_status"] is not None else None,
            "body": entry["upstream_body"],
        },
        "expect": compact({
            "frames": [normalize(frame) for frame in frames],
            "close": close,
            "raised": raised,
            "upstream": upstream,
        }),
    }


def build_ws_proxy_cases() -> list[dict]:
    return [run_ws_proxy_case(entry) for entry in WS_PROXY_CASES]


def build_corpus() -> dict:
    # 全局钉死时钟：语料里的每一个时间都来自 NOW_BEIJING，不再随运行时刻漂移。
    pin_clock()
    build_cases()
    results = [run_case(entry) for entry in CASES]
    ws_cases = build_ws_cases()
    ws_proxy_cases = build_ws_proxy_cases()
    covered = {
        pattern
        for result in results + ws_cases + ws_proxy_cases
        for pattern in result.get("covers", [])
    }
    missing = sorted(set(ROUTE_PATTERNS) - covered)
    if missing:
        raise SystemExit(f"语料未覆盖以下路由: {missing}")
    return {
        "version": CORPUS_VERSION,
        "note": (
            "由 scripts/gen_server_corpus.py 驱动真实 Python FastAPI 应用生成。"
            "now_beijing 是生成时钉死的固定时钟（两侧必须一致，见脚本头部第 3 点），"
            "Go 侧回放时用 metrics.SetNowForTest 钉到同一瞬间。"
            "body_text 已把时间戳与夹具目录替换成 <TIMESTAMP> / <FIXTURE_DIR>，"
            "body_normalized 标记该用例是否被归一化过。"
            "fixture_dir 是生成时的固定临时目录，Go 侧回放时应替换成自己的临时目录。"
            "ws_cases / ws_proxy_cases 是 WebSocket 语料（用 websocket_connect 驱动），"
            "同样已归一化；Go 侧用 coder/websocket 客户端回放。"
        ),
        "now_beijing": NOW_BEIJING.isoformat(),
        "fixture_dir": str(BASE_DIR),
        "fixture": fixture(),
        "empty_fixture": empty_fixture(),
        "case_count": len(results),
        "cases": results,
        "ws_cases": ws_cases,
        "ws_proxy_cases": ws_proxy_cases,
    }


# app 面路由清单（与 internal/server/routes.go 的注册一一对应）；用于断言语料覆盖面。
# 后三条是 WebSocket 路由：它们的语料在 ws_cases / ws_proxy_cases 里（HTTP 请求永远
# 匹配不到它们，只能靠 websocket_connect 驱动）。
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
    "GET /ws/events",
    "WS /ws/events",
    "WS /v1/{path}",
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
