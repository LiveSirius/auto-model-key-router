#!/usr/bin/env python3
"""生成 metrics SQLite 层的差分对拍语料。

指标层是「静默失效」的集中区：created_at 是文本、窗口过滤靠字典序、分桶基于
SQLite 的 strftime 算术、17 个聚合里有若干看着等价其实不等价的写法。任何一处
偏差都不会报错，只会让数字慢慢不对。

因此这里直接驱动参照实现（``auto_model_key_router/metrics.py``）产出：

* ``schema.jsonl`` —— 全新建库后的 sqlite_master / table_info / 索引 / PRAGMA
  快照。用来断言建表语句与索引定义**逐字符**一致。
* ``legacy.jsonl`` —— 用旧版表结构建的库被打开升级后的快照。用来断言用户手里
  已有的 metrics.sqlite3 能被直接读写，且列补齐与两条回填与参照实现一致。
* ``record.jsonl`` —— record() 的落库结果（归一化、钳位、失败判定、字段默认）。
* ``usage.jsonl`` —— _normalize_usage / _int_value / _rate / _router_status。
* ``clock.jsonl`` —— isoformat 文本、timedelta(hours=) 的微秒舍入、分桶。
* ``filter.jsonl`` —— _metric_filter_sql / _append_filter 的 SQL 文本与参数序。
* ``query.jsonl`` —— 在真实 Python 建出的库上跑 snapshot / key_stats /
  request_history / time_series，记录**有序** JSON 作为期望响应。

``expect`` 一律是 ``json.dumps(..., separators=(",", ":"))`` 的**紧凑有序文本**：
响应体的键序是契约的一部分，不能靠外层 sort_keys 打乱。

确定性来自把 ``metrics._now_beijing`` 换成固定时钟；时间相关的入参改用
年月日时分秒分量表达，避免依赖解析器。

用法::

    python scripts/gen_metrics_corpus.py
    python scripts/gen_metrics_corpus.py --check
"""

from __future__ import annotations

import argparse
import json
import random
import shutil
import sqlite3
import sys
import tempfile
from datetime import datetime
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import auto_model_key_router.metrics as metrics_module  # noqa: E402
from auto_model_key_router.metrics import (  # noqa: E402
    BEIJING_TZ,
    MAX_SERIES_POINTS,
    MetricsStore,
    _append_filter,
    _bucket_epoch,
    _int_value,
    _metric_filter_sql,
    _normalize_usage,
    _rate,
    _router_status,
    UsageStats,
)

DEFAULT_DIR = REPO_ROOT / "internal" / "metrics" / "testdata"

# 语料时钟。用固定值而非 datetime.now()，否则语料每次生成都不同。
NOW = datetime(2026, 7, 14, 12, 0, 0, tzinfo=BEIJING_TZ)
# 用于 1970 年前后的分桶边界；1970-01-01T00:00:00+08:00 的 unix 秒是 -28800，
# 正好等于 BUCKET_ANCHOR_EPOCH。
EPOCH_NOW = datetime(1970, 1, 1, 0, 30, 0, tzinfo=BEIJING_TZ)

INSERT_COLUMNS = [
    "created_at", "caller_type", "model_id", "requested_model_id",
    "provider_id", "pool_name", "upstream_model_id", "key_name",
    "status_code", "success", "retried", "prompt_tokens",
    "completion_tokens", "total_tokens", "cached_tokens",
    "cache_creation_input_tokens", "cache_read_input_tokens",
    "first_token_ms", "duration_ms",
]


# --------------------------------------------------------------------------- #
# 基础工具
# --------------------------------------------------------------------------- #

def compact(obj: object) -> str:
    """紧凑有序 JSON 文本，作为 expect 的统一载体。"""
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


def render(cases: list[dict]) -> str:
    """一行一个 case。外层键排序，expect 是字符串所以不受影响。"""
    return "\n".join(
        json.dumps(case, ensure_ascii=False, sort_keys=True) for case in cases
    ) + "\n"


class FakeClock:
    """替换 metrics._now_beijing 的固定时钟。"""

    def __init__(self, value: datetime) -> None:
        self.value = value

    def __call__(self) -> datetime:
        return self.value


def at(**parts: int) -> datetime:
    """按分量构造北京时间。

    未给的分量取 1（月/日）或 0（时/分/秒/微秒），这样用例只需写关心的那几位。
    """
    fields = {
        "year": parts.pop("year", 2026),
        "month": parts.pop("month", 1),
        "day": parts.pop("day", 1),
        "hour": parts.pop("hour", 0),
        "minute": parts.pop("minute", 0),
        "second": parts.pop("second", 0),
        "microsecond": parts.pop("microsecond", 0),
    }
    fields.update(parts)
    return datetime(tzinfo=BEIJING_TZ, **fields)


def logical_dump(path: Path) -> dict:
    """把库文件的**逻辑内容**导出成可比对的快照。

    不比对文件字节：SQLite 的页布局会随写入顺序与版本变化，比对字节只会得到
    假失败。这里比对结构（sqlite_master 原文、列序、索引定义）、PRAGMA 状态与
    全部行数据 —— 这些才是兼容性契约。
    """
    conn = sqlite3.connect(path)
    conn.row_factory = sqlite3.Row
    try:
        master = [
            {"type": row["type"], "name": row["name"], "tbl_name": row["tbl_name"],
             "sql": row["sql"]}
            for row in conn.execute(
                "SELECT type, name, tbl_name, sql FROM sqlite_master "
                "WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name"
            )
        ]
        columns = [
            {"cid": row["cid"], "name": row["name"], "type": row["type"],
             "notnull": row["notnull"], "dflt_value": row["dflt_value"],
             "pk": row["pk"]}
            for row in conn.execute("PRAGMA table_info(request_metrics)")
        ]
        index_list = [
            {"seq": row["seq"], "name": row["name"], "unique": row["unique"],
             "origin": row["origin"], "partial": row["partial"]}
            for row in conn.execute("PRAGMA index_list(request_metrics)")
        ]
        rows = [
            list(row)
            for row in conn.execute(
                f"SELECT {', '.join(INSERT_COLUMNS)} FROM request_metrics ORDER BY id"
            )
        ]
        journal_mode = conn.execute("PRAGMA journal_mode").fetchone()[0]
        user_version = conn.execute("PRAGMA user_version").fetchone()[0]
        integrity = conn.execute("PRAGMA integrity_check").fetchone()[0]
    finally:
        conn.close()
    return {
        "master": master,
        "columns": columns,
        "index_list": index_list,
        "rows": rows,
        "journal_mode": journal_mode,
        "user_version": user_version,
        "integrity_check": integrity,
    }


def insert_row(conn: sqlite3.Connection, **values: object) -> None:
    columns = ", ".join(INSERT_COLUMNS)
    placeholders = ", ".join("?" for _ in INSERT_COLUMNS)
    conn.execute(
        f"INSERT INTO request_metrics ({columns}) VALUES ({placeholders})",
        [values.get(name) for name in INSERT_COLUMNS],
    )


def patch_clock(value: datetime) -> None:
    metrics_module._now_beijing = FakeClock(value)


# --------------------------------------------------------------------------- #
# schema.jsonl
# --------------------------------------------------------------------------- #

def build_schema_cases() -> list[dict]:
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "fresh.sqlite3"
        store = MetricsStore(path)
        store._connection.close()
        dump = logical_dump(path)
    return [{
        "name": "fresh_database",
        "kind": "logical_dump",
        "note": "全新库：断言建表语句、列序、7 条索引与 PRAGMA 状态",
        "expect": compact(dump),
    }]


# --------------------------------------------------------------------------- #
# legacy.jsonl —— 旧版表结构升级
# --------------------------------------------------------------------------- #

LEGACY_DDL = """
CREATE TABLE request_metrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    caller_type TEXT,
    model_id TEXT NOT NULL,
    requested_model_id TEXT NOT NULL DEFAULT '',
    key_name TEXT NOT NULL,
    status_code INTEGER,
    success INTEGER NOT NULL,
    retried INTEGER NOT NULL,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0
)
"""

# caller_type 故意混入 NULL / '' / 'weird'：旧库可能是在该列还是可空时建的，
# 或者被早期版本写坏过。两条回填必须把非法值收敛到 'local'。
LEGACY_ROWS = [
    ("2026-07-01T08:00:00+08:00", None, "m-alpha", "", "k1", 200, 1, 0, 100, 50, 150),
    ("2026-07-01T09:00:00+08:00", "local", "m-alpha", "", "k1", 500, 0, 0, 10, 0, 10),
    ("2026-07-01T10:00:00+08:00", "visitor", "m-beta", "", "k2", 200, 1, 1, 20, 30, 50),
    ("2026-07-01T11:00:00+08:00", "weird", "m-beta", "", "k2", None, 0, 0, 0, 0, 0),
    ("2026-07-01T12:00:00+08:00", "", "m-gamma", "", "k3", 429, 0, 1, 5, 0, 5),
]


def make_legacy_database(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    for suffix in ("", "-wal", "-shm"):
        candidate = Path(str(path) + suffix)
        if candidate.exists():
            candidate.unlink()
    conn = sqlite3.connect(path)
    try:
        conn.execute(LEGACY_DDL)
        conn.executemany(
            "INSERT INTO request_metrics (created_at, caller_type, model_id, "
            "requested_model_id, key_name, status_code, success, retried, "
            "prompt_tokens, completion_tokens, total_tokens) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            LEGACY_ROWS,
        )
        conn.commit()
    finally:
        conn.close()


def build_legacy_cases() -> list[dict]:
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "legacy.sqlite3"
        make_legacy_database(path)
        # 仅仅「打开」就该完成升级：这正是用户换二进制时的路径。
        store = MetricsStore(path)
        store._connection.close()
        dump = logical_dump(path)
    return [{
        "name": "legacy_database_upgrade",
        "kind": "logical_dump",
        "note": "旧表结构 + 脏 caller_type 打开后：补列、回填、建索引",
        "expect": compact(dump),
    }]


# --------------------------------------------------------------------------- #
# record.jsonl
# --------------------------------------------------------------------------- #

def build_record_cases() -> list[dict]:
    cases: list[dict] = []

    def add(name: str, note: str, **params: object) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "record.sqlite3"
            patch_clock(NOW)
            store = MetricsStore(path)
            store.on_record = None
            import asyncio
            asyncio.run(store.record(**params))
            row = store._connection.execute(
                f"SELECT {', '.join(INSERT_COLUMNS)} FROM request_metrics"
            ).fetchone()
            stored = {name_: row[name_] for name_ in INSERT_COLUMNS}
            store._connection.close()
        cases.append({
            "name": name,
            "kind": "record",
            "note": note,
            "now": compact(NOW.isoformat()),
            "params": params,
            "expect": compact(stored),
        })

    add("openai_basic", "OpenAI 形态：prompt/completion/total 直接给",
        model_id="m-alpha", key_name="k1", status_code=200,
        usage={"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
        duration_ms=1200, first_token_ms=300,
        requested_model_id="m-alpha-alias", provider_id="p1", pool_name="pool-a",
        upstream_model_id="gpt-4o", caller_type="visitor")

    add("anthropic_cache_addback",
        "Anthropic 形态：input_tokens 不含缓存量，按参照实现加回",
        model_id="m-beta", key_name="k2", status_code=200,
        usage={"input_tokens": 1000, "output_tokens": 200,
               "cache_read_input_tokens": 400,
               "cache_creation_input_tokens": 100},
        duration_ms=900)

    add("openai_cached_details",
        "缓存量只在 prompt_tokens_details 里",
        model_id="m-beta", key_name="k2", status_code=200,
        usage={"prompt_tokens": 500, "completion_tokens": 10,
               "prompt_tokens_details": {"cached_tokens": 128}},
        duration_ms=100)

    add("cached_falls_back_to_cache_read",
        "cached_tokens 全缺时回退到 cache_read_input_tokens",
        model_id="m-beta", key_name="k2", status_code=200,
        usage={"prompt_tokens": 300, "completion_tokens": 5,
               "cache_read_input_tokens": 64},
        duration_ms=50)

    add("explicit_zero_falls_through",
        "顶层 cached_tokens=0 会继续向后回退（or 语义，不是取默认值）",
        model_id="m-beta", key_name="k2", status_code=200,
        usage={"prompt_tokens": 300, "completion_tokens": 5, "cached_tokens": 0,
               "input_tokens_details": {"cached_tokens": 32}},
        duration_ms=50)

    add("string_and_float_numbers",
        "字符串数字与浮点会被 _int_value 收下，字符串是数字就解析",
        model_id="m-gamma", key_name="k3", status_code=200,
        usage={"prompt_tokens": "120", "completion_tokens": 3.9,
               "total_tokens": "0"},
        duration_ms=10)

    add("failure_from_status",
        "status_code >= 400 判为失败，success 落 0",
        model_id="m-alpha", key_name="k1", status_code=500, failed=False,
        usage={"prompt_tokens": 10, "completion_tokens": 0}, duration_ms=80)

    add("failure_from_flag",
        "failed=True 即使 200 也落 success=0",
        model_id="m-alpha", key_name="k1", status_code=200, failed=True,
        usage={"prompt_tokens": 10, "completion_tokens": 1}, duration_ms=80)

    add("failure_when_status_missing",
        "status_code=None 视为失败",
        model_id="m-alpha", key_name="k1", status_code=None,
        usage={"prompt_tokens": 10}, duration_ms=80)

    add("negative_durations_clamped",
        "负耗时被钳到 0，retried=True 落 1",
        model_id="m-alpha", key_name="k1", status_code=200, retried=True,
        usage={"prompt_tokens": 1}, duration_ms=-5, first_token_ms=-1)

    add("invalid_caller_type_defaults_local",
        "非法 caller_type 收敛为 local",
        model_id="m-alpha", key_name="k1", status_code=200,
        caller_type="robot", usage={"prompt_tokens": 1})

    add("empty_requested_model_falls_back",
        "requested_model_id 为空串时回退到 model_id",
        model_id="m-alpha", key_name="k1", status_code=200,
        requested_model_id="", usage={"prompt_tokens": 1})

    add("null_attribution_dimensions",
        "归属维度缺省落 NULL，供 unattributed 统计",
        model_id="m-alpha", key_name="k1", status_code=200,
        usage={"prompt_tokens": 1}, duration_ms=1)

    add("usage_none",
        "usage=None 按空字典处理，六个计数全 0",
        model_id="m-alpha", key_name="k1", status_code=200, usage=None)

    return cases


# --------------------------------------------------------------------------- #
# usage.jsonl
# --------------------------------------------------------------------------- #

NORMALIZE_CASES: list[tuple[str, dict, str]] = [
    ("openai_full", {"prompt_tokens": 100, "completion_tokens": 40,
                     "total_tokens": 140}, "OpenAI 三件套原样"),
    ("openai_total_missing", {"prompt_tokens": 100, "completion_tokens": 40},
     "total_tokens 缺失时按 prompt+completion 推导"),
    ("openai_total_zero", {"prompt_tokens": 100, "completion_tokens": 40,
                           "total_tokens": 0},
     "total_tokens=0 是假值，同样触发推导"),
    ("anthropic_basic", {"input_tokens": 900, "output_tokens": 120},
     "Anthropic 无缓存字段：prompt=input"),
    ("anthropic_with_cache", {"input_tokens": 900, "output_tokens": 120,
                              "cache_read_input_tokens": 400,
                              "cache_creation_input_tokens": 50},
     "input_tokens 不含缓存量，必须加回"),
    ("anthropic_prompt_wins", {"prompt_tokens": 10, "input_tokens": 900,
                               "output_tokens": 1},
     "prompt_tokens 非 0 时不加回，且 prompt 优先于 input"),
    ("completion_prefers_completion_field",
     {"prompt_tokens": 10, "completion_tokens": 7, "output_tokens": 99},
     "completion_tokens 优先于 output_tokens"),
    ("completion_falls_back_to_output",
     {"prompt_tokens": 10, "output_tokens": 99},
     "completion_tokens=0 时回退 output_tokens"),
    ("cache_read_top_level", {"prompt_tokens": 100,
                              "cache_read_input_tokens": 33}, "顶层缓存读"),
    ("cache_read_input_details",
     {"prompt_tokens": 100,
      "input_tokens_details": {"cache_read_input_tokens": 33}},
     "缓存读只在 input_tokens_details 里"),
    ("cache_read_top_wins_over_details",
     {"prompt_tokens": 100, "cache_read_input_tokens": 7,
      "input_tokens_details": {"cache_read_input_tokens": 33}},
     "顶层优先，哪怕更小"),
    ("cache_creation_input_details",
     {"prompt_tokens": 100,
      "input_tokens_details": {"cache_creation_input_tokens": 12}},
     "缓存写只在 details 里"),
    ("cached_from_details", {"prompt_tokens": 100,
                             "prompt_tokens_details": {"cached_tokens": 20}},
     "cached_tokens 取自 prompt_tokens_details"),
    ("cached_details_priority",
     {"prompt_tokens": 100, "prompt_tokens_details": {"cached_tokens": 20},
      "input_tokens_details": {"cached_tokens": 77}},
     "prompt_tokens_details 优先于 input_tokens_details"),
    ("cached_zero_falls_through",
     {"prompt_tokens": 100, "cached_tokens": 0,
      "prompt_tokens_details": {"cached_tokens": 20}},
     "显式 0 继续回退（or 链）"),
    ("cached_falls_back_to_cache_read",
     {"prompt_tokens": 100, "cache_read_input_tokens": 55},
     "cached_tokens 全缺时取缓存读量"),
    ("details_not_dict_ignored",
     {"prompt_tokens": 100, "prompt_tokens_details": "oops",
      "input_tokens_details": [1, 2]},
     "details 非字典时当空字典，不报错"),
    ("string_numbers", {"prompt_tokens": "100", "completion_tokens": "40"},
     "字符串数字被解析"),
    ("non_digit_string_is_zero",
     {"prompt_tokens": "-5", "completion_tokens": " 5"},
     "带符号/空白的字符串不是数字，算 0"),
    ("float_truncates", {"prompt_tokens": 10.9, "completion_tokens": -1.9},
     "浮点向零截断"),
    ("bool_is_int_subclass", {"prompt_tokens": True, "completion_tokens": False},
     "bool 是 int 子类（此处按 SQLite 落库后的 1/0 记录）"),
    ("missing_everything", {}, "空 usage"),
    ("input_tokens_zero_no_addback",
     {"input_tokens": 0, "output_tokens": 5}, "input_tokens=0 是假值，不触发加回"),
]


def build_usage_cases() -> list[dict]:
    cases: list[dict] = []
    for name, usage, note in NORMALIZE_CASES:
        normalized = _normalize_usage(usage)
        # bool 在 Python 里是 int 子类，会被原样保留；但它唯一的去向是 SQLite，
        # 而 SQLite 落库后就是 1/0。这里记录**可观测**值，避免把不可观测的
        # 类型差异当失败。
        observable = {key: int(value) for key, value in normalized.items()}
        cases.append({
            "name": name, "kind": "normalize_usage", "note": note,
            "input": usage, "expect": compact(observable),
        })

    for name, value in [
        ("int", 7), ("int_zero", 0), ("negative_int", -3),
        ("float", 2.9), ("negative_float", -2.9), ("bool_true", True),
        ("bool_false", False), ("digits", "42"), ("padded_digits", "007"),
        ("signed_string", "-5"), ("spaced_string", " 5"), ("float_string", "5.0"),
        ("empty_string", ""), ("none", None), ("list", [1]), ("dict", {"a": 1}),
    ]:
        # Python 里 bool 是 int 的子类，_int_value(True) 返回 True；但它唯一的
        # 去向是 SQLite，而 sqlite3 会把 True 存成 1。记录**可观测**的整数值，
        # 避免把不可观测的类型差异（bool vs int）当成失败。
        cases.append({
            "name": f"int_value_{name}", "kind": "int_value",
            "input": value, "expect": compact(int(_int_value(value))),
        })

    for name, numerator, denominator in [
        ("third", 1, 3), ("two_thirds", 2, 3), ("negative_third", -1, 3),
        ("exact_half", 1, 2), ("exact_half_large", 5, 2), ("exact_2_5", 3, 2),
        ("zero_numerator", 0, 5), ("zero_denominator", 7, 0),
        ("negative_denominator", 7, -1), ("tiny_negative", -1, 10**9),
        ("1_over_128_tie", 1, 128), ("3_over_128_tie", 3, 128),
        ("full_cache", 128, 128), ("large", 123456, 1000),
    ]:
        cases.append({
            "name": f"rate_{name}", "kind": "rate",
            "input": [numerator, denominator],
            "expect": compact(_rate(numerator, denominator)),
        })

    for name, stats in [
        ("empty", UsageStats()),
        ("healthy", UsageStats(requests=100, successes=99, retries=1)),
        ("retry_heavy", UsageStats(requests=10, successes=10, retries=6)),
        ("degraded", UsageStats(requests=100, successes=90)),
        ("broken", UsageStats(requests=100, successes=79)),
    ]:
        cases.append({
            "name": f"router_status_{name}", "kind": "router_status",
            "input": {"requests": stats.requests, "successes": stats.successes,
                      "retries": stats.retries},
            "expect": compact(_router_status(stats)),
        })

    # to_dict 的键序与派生值是响应体契约，单独固定一份。
    cases.append({
        "name": "to_dict_order", "kind": "to_dict",
        "note": "键序、派生均值与 cached_token_rate",
        "input": {"requests": 3, "successes": 2, "failures": 1, "retries": 1,
                  "prompt_tokens": 300, "completion_tokens": 60,
                  "total_tokens": 360, "cached_tokens": 100,
                  "cache_creation_input_tokens": 5,
                  "cache_read_input_tokens": 100,
                  "total_duration_ms": 1000, "min_duration_ms": 100,
                  "max_duration_ms": 500, "total_first_token_ms": 300,
                  "min_first_token_ms": 50, "max_first_token_ms": 150,
                  "status_codes": {"200": 2, "500": 1}},
        "expect": compact(UsageStats(
            requests=3, successes=2, failures=1, retries=1, prompt_tokens=300,
            completion_tokens=60, total_tokens=360, cached_tokens=100,
            cache_creation_input_tokens=5, cache_read_input_tokens=100,
            total_duration_ms=1000, min_duration_ms=100, max_duration_ms=500,
            total_first_token_ms=300, min_first_token_ms=50,
            max_first_token_ms=150,
            status_codes=__import__("collections").Counter({"200": 2, "500": 1}),
        ).to_dict()),
    })
    cases.append({
        "name": "to_dict_empty",
        "kind": "to_dict",
        "note": "空集：min_* 是 None，渲染为 0；均值为 0；cached_token_rate 为 0.0",
        "input": {}, "expect": compact(UsageStats().to_dict()),
    })
    cases.append({
        "name": "to_dict_banker_rounding",
        "kind": "to_dict",
        "note": "均值走 Python 的银行家舍入：2.5 -> 2",
        "input": {"requests": 2, "total_duration_ms": 5,
                  "total_first_token_ms": 7},
        "expect": compact(UsageStats(
            requests=2, total_duration_ms=5, total_first_token_ms=7).to_dict()),
    })
    return cases


# --------------------------------------------------------------------------- #
# clock.jsonl
# --------------------------------------------------------------------------- #

ISO_CASES = [
    ("whole_second", dict(year=2026, month=7, day=14, hour=12, minute=0, second=0),
     "整秒：微秒段必须整段省略，不能出现 .000000"),
    ("microsecond_1", dict(year=2026, month=7, day=14, hour=12, minute=0,
                           second=0, microsecond=1), "1 微秒补足 6 位"),
    ("microsecond_123456", dict(year=2026, month=7, day=14, hour=12, minute=0,
                                second=0, microsecond=123456), "6 位微秒"),
    ("microsecond_999999", dict(year=2026, month=7, day=14, hour=12, minute=0,
                                second=0, microsecond=999999), "最大微秒"),
    ("end_of_day", dict(year=2026, month=7, day=14, hour=23, minute=59,
                        second=59), "日界"),
    ("max_datetime", dict(year=9999, month=12, day=31, hour=23, minute=59,
                          second=59, microsecond=999999), "datetime.max"),
    ("pre_1901_lmt", dict(year=1900, month=1, day=1, hour=0, minute=0, second=0),
     "1900 年上海还是 LMT +08:05:43"),
    ("year_1901", dict(year=1901, month=1, day=1, hour=0, minute=0, second=0),
     "1901 年起固定 +08:00"),
    ("epoch_anchor", dict(year=1970, month=1, day=1, hour=0, minute=0, second=0),
     "锚点日：unix -28800"),
    ("micro_epoch_boundary",
     dict(year=1969, month=12, day=31, hour=23, minute=59, second=59,
          microsecond=500000), "负 unix 秒 + 微秒"),
]

ADD_HOURS_CASES = [
    ("whole_hour", dict(hour=12), 24.0, "整数小时精确"),
    ("one_third", dict(hour=12), 1 / 3, "1/3 小时：小数部分需最近偶数舍入"),
    ("tenth", dict(hour=12), 0.1, "0.1 小时"),
    ("negative", dict(hour=12), -0.1, "负小数小时"),
    ("sub_microsecond", dict(hour=12), 1e-9,
     "小于 1 微秒：应舍成 0，不产生偏移"),
    ("half_microsecond", dict(hour=12), 1 / 7.2e9,
     "恰好半微秒：最近偶数"),
    ("three_half_microsecond", dict(hour=12), 3 / 7.2e9,
     "1.5 微秒：最近偶数向偶数侧"),
    ("large_fractional", dict(hour=12), 869432.5734188609,
     "实测直接乘 3.6e9 会差 1 微秒的输入"),
    ("week", dict(hour=12), 168.0, "一周"),
    ("month", dict(hour=12), 720.0, "30 天"),
    ("tiny_positive", dict(hour=12), 2.5e-7, "0.9 微秒"),
]

BUCKET_CASES = [
    ("aligned", dict(year=2026, month=7, day=14, hour=12, minute=0, second=0),
     3600, "整点对齐"),
    ("mid_hour", dict(year=2026, month=7, day=14, hour=12, minute=34,
                      second=56), 3600, "向下取整到小时"),
    ("micro_second_dropped",
     dict(year=2026, month=7, day=14, hour=12, minute=0, second=0, microsecond=1),
     60, "微秒被 int() 截断，不改变桶"),
    ("pre_epoch_anchor",
     dict(year=1969, month=12, day=31, hour=23, minute=59, second=59,
          microsecond=500000), 3600,
     "负 unix 秒 + 微秒：int() 向零截断，桶落在锚点之前"),
    ("pre_epoch_exact_hour",
     dict(year=1969, month=12, day=31, hour=23, minute=0, second=0), 3600,
     "锚点前整小时"),
    ("anchor_itself", dict(year=1970, month=1, day=1, hour=0, minute=0,
                           second=0), 60, "恰好锚点"),
    ("after_anchor_utc_midnight",
     dict(year=1970, month=1, day=1, hour=8, minute=0, second=0), 900,
     "unix 0"),
    ("bucket_7s", dict(year=2026, month=7, day=14, hour=12, minute=0,
                       second=30), 7, "非整除桶长"),
    ("bucket_1s", dict(year=2026, month=7, day=14, hour=12, minute=0,
                       second=30), 1, "秒级桶"),
    ("big_bucket", dict(year=2026, month=7, day=14, hour=12, minute=0,
                        second=0), 86400, "天级桶"),
]


def build_clock_cases() -> list[dict]:
    cases: list[dict] = []
    for name, parts, note in ISO_CASES:
        moment = at(**parts)
        cases.append({
            "name": name, "kind": "isoformat", "note": note,
            "parts": parts, "expect": compact(moment.isoformat()),
        })
    for name, parts, hours, note in ADD_HOURS_CASES:
        moment = at(**parts)
        result = (moment - __import__("datetime").timedelta(hours=hours))
        cases.append({
            "name": name, "kind": "add_hours", "note": note,
            "parts": parts, "hours_literal": repr(hours),
            "expect": compact(result.isoformat()),
        })
    for name, parts, bucket_seconds, note in BUCKET_CASES:
        moment = at(**parts)
        cases.append({
            "name": name, "kind": "bucket_epoch", "note": note,
            "parts": parts, "bucket_seconds": bucket_seconds,
            "expect": compact(_bucket_epoch(moment, bucket_seconds)),
        })
    return cases


# --------------------------------------------------------------------------- #
# filter.jsonl
# --------------------------------------------------------------------------- #

FILTER_CASES = [
    ("empty", {}),
    ("since_only", {"since_created_at": "2026-07-14T11:00:00+08:00"}),
    ("until_only", {"until_created_at": "2026-07-14T12:00:00+08:00"}),
    ("both_bounds", {"since_created_at": "2026-07-14T11:00:00+08:00",
                     "until_created_at": "2026-07-14T12:00:00+08:00"}),
    ("all_columns", {"caller_type": "visitor", "model_id": "m", "requested_model_id": "r",
                     "provider_id": "p", "pool_name": "pool",
                     "upstream_model_id": "u", "key_name": "k", "status_code": 429}),
    ("status_code_zero", {"status_code": 0}),
    ("success_true", {"success": True}),
    ("success_false", {"success": False}),
    ("attributed_true", {"attributed": True}),
    ("attributed_false", {"attributed": False}),
    ("order_check", {"until_created_at": "2026-07-14T12:00:00+08:00",
                     "since_created_at": "2026-07-14T11:00:00+08:00",
                     "status_code": 500, "model_id": "m", "success": True,
                     "attributed": True, "caller_type": "local"}),
    ("empty_string_values", {"model_id": "", "key_name": ""}),
]

APPEND_CASES = [
    ("empty_where", "", "status_code IS NOT NULL"),
    ("existing_where", " WHERE model_id = ?", "id < ?"),
    ("multi", " WHERE a = ? AND b = ?", "status_code IS NOT NULL"),
]


def build_filter_cases() -> list[dict]:
    cases: list[dict] = []
    for name, kwargs in FILTER_CASES:
        where_sql, parameters = _metric_filter_sql(**kwargs)
        cases.append({
            "name": name, "kind": "metric_filter", "note": "",
            "input": kwargs,
            "expect": compact([where_sql, list(parameters)]),
        })
    for name, where_sql, condition in APPEND_CASES:
        cases.append({
            "name": name, "kind": "append_filter", "note": "",
            "input": [where_sql, condition],
            "expect": compact(_append_filter(where_sql, condition)),
        })
    return cases


# --------------------------------------------------------------------------- #
# 查询夹具库
# --------------------------------------------------------------------------- #

MODELS = ["m-alpha", "m-beta", "m-gamma"]
KEYS = ["k1", "k2", "k3", "k4"]
PROVIDERS = ["p-openai", "p-anthropic", None]
POOLS = ["pool-a", "pool-b", None]
UPSTREAMS = ["gpt-4o", "claude-3-5-sonnet", None]
STATUSES = [200, 200, 200, 201, 400, 429, 500, 503, None]


def build_metrics_database(path: Path) -> None:
    """建一个现代结构的库，喂进覆盖各分支的确定性数据。"""
    for suffix in ("", "-wal", "-shm"):
        candidate = Path(str(path) + suffix)
        if candidate.exists():
            candidate.unlink()
    patch_clock(NOW)
    store = MetricsStore(path)
    rng = random.Random(20260714)

    rows: list[dict] = []
    # 窗口内（最近 60 秒）几条，让 current_rpm / current_tpm 非平凡。
    for index in range(6):
        rows.append({
            "created_at": f"2026-07-14T11:59:{50 + index}+08:00",
            "caller_type": "local" if index % 2 == 0 else "visitor",
            "model_id": "m-alpha", "requested_model_id": "m-alpha",
            "provider_id": "p-openai", "pool_name": "pool-a",
            "upstream_model_id": "gpt-4o", "key_name": "k1",
            "status_code": 200, "success": 1, "retried": 0,
            "prompt_tokens": 100 + index, "completion_tokens": 10,
            "total_tokens": 110 + index, "cached_tokens": index,
            "cache_creation_input_tokens": 0, "cache_read_input_tokens": index,
            "first_token_ms": 100, "duration_ms": 500 + index,
        })
    # 24 小时窗口内的随机数据。
    for index in range(120):
        status = rng.choice(STATUSES)
        provider = rng.choice(PROVIDERS)
        pool = rng.choice(POOLS) if provider else None
        upstream = rng.choice(UPSTREAMS) if pool else None
        caller = rng.choice(["local", "visitor"])
        model = rng.choice(MODELS)
        prompt_tokens = rng.choice([0, 1, 7, 100, 999, 12345])
        # 有意让缓存量偶尔超过 prompt：uncached_prompt_tokens 必须钳到 0。
        cached_tokens = rng.choice([0, 0, 3, prompt_tokens + 50])
        success = 0 if (status is None or status >= 400) else 1
        # 注入少量「不自洽」的行：success 与 status 矛盾。这样 failures 的
        # SUM(CASE WHEN success = 0) 写法才与 COUNT - SUM(success) 等价的
        # 错觉被打破，17 个聚合的差异才会暴露。
        if index % 17 == 0:
            success = 1 - success
        minute = rng.randint(0, 59)
        hour = rng.randint(0, 11)
        rows.append({
            "created_at": f"2026-07-14T{hour:02d}:{minute:02d}:{index % 60:02d}+08:00",
            "caller_type": caller, "model_id": model,
            "requested_model_id": rng.choice([model, model + "-alias"]),
            "provider_id": provider, "pool_name": pool,
            "upstream_model_id": upstream, "key_name": rng.choice(KEYS),
            "status_code": status, "success": success,
            "retried": rng.choice([0, 0, 1]),
            "prompt_tokens": prompt_tokens,
            "completion_tokens": rng.choice([0, 5, 250]),
            "total_tokens": prompt_tokens + 5, "cached_tokens": cached_tokens,
            "cache_creation_input_tokens": rng.choice([0, 12]),
            "cache_read_input_tokens": cached_tokens,
            "first_token_ms": rng.choice([0, 42, 300, 9000]),
            "duration_ms": rng.choice([0, 13, 800, 60000]),
        })
    # 窗口外的历史数据与微秒/负纪元边界。
    rows.append({
        "created_at": "2026-07-10T00:00:00.000001+08:00", "caller_type": "local",
        "model_id": "m-alpha", "requested_model_id": "m-alpha", "provider_id": None,
        "pool_name": None, "upstream_model_id": None, "key_name": "k1",
        "status_code": 200, "success": 1, "retried": 0, "prompt_tokens": 1,
        "completion_tokens": 1, "total_tokens": 2, "cached_tokens": 0,
        "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
        "first_token_ms": 0, "duration_ms": 0,
    })
    rows.append({
        "created_at": "1969-12-31T23:59:59.500000+08:00", "caller_type": "local",
        "model_id": "m-legacy", "requested_model_id": "m-legacy",
        "provider_id": None, "pool_name": None, "upstream_model_id": None,
        "key_name": "k1", "status_code": 200, "success": 1, "retried": 0,
        "prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11,
        "cached_tokens": 0, "cache_creation_input_tokens": 0,
        "cache_read_input_tokens": 0, "first_token_ms": 0, "duration_ms": 0,
    })
    # created_at 不是合法日期：strftime 返回 NULL，分桶时必须整行丢弃。
    rows.append({
        "created_at": "not-a-date", "caller_type": "local", "model_id": "m-broken",
        "requested_model_id": "m-broken", "provider_id": None, "pool_name": None,
        "upstream_model_id": None, "key_name": "k1", "status_code": 200,
        "success": 1, "retried": 0, "prompt_tokens": 1, "completion_tokens": 0,
        "total_tokens": 1, "cached_tokens": 0, "cache_creation_input_tokens": 0,
        "cache_read_input_tokens": 0, "first_token_ms": 0, "duration_ms": 0,
    })

    for row in rows:
        insert_row(store._connection, **row)
    store._connection.commit()
    store._connection.close()


def build_epoch_database(path: Path) -> None:
    """围绕分桶锚点（unix -28800）的库，专门压负数 strftime 与整数除法方向。"""
    for suffix in ("", "-wal", "-shm"):
        candidate = Path(str(path) + suffix)
        if candidate.exists():
            candidate.unlink()
    patch_clock(EPOCH_NOW)
    store = MetricsStore(path)
    for index, (created_at, status) in enumerate([
        ("1969-12-31T23:00:00+08:00", 200),
        ("1969-12-31T23:30:00+08:00", 500),
        ("1969-12-31T23:59:59.500000+08:00", 200),
        ("1970-01-01T00:00:00+08:00", None),
        ("1970-01-01T00:00:00.000001+08:00", 200),
        ("1970-01-01T00:15:00+08:00", 429),
        ("1970-01-01T00:30:00+08:00", 200),
        ("1970-01-01T08:00:00+08:00", 200),
    ]):
        insert_row(
            store._connection,
            created_at=created_at, caller_type="local", model_id="m-epoch",
            requested_model_id="m-epoch", provider_id="p1", pool_name="pool-a",
            upstream_model_id="up-1", key_name="k1", status_code=status,
            success=0 if (status is None or status >= 400) else 1,
            retried=index % 2, prompt_tokens=10 * index,
            completion_tokens=index, total_tokens=10 * index + index,
            cached_tokens=index, cache_creation_input_tokens=0,
            cache_read_input_tokens=index, first_token_ms=index,
            duration_ms=100 * index,
        )
    store._connection.commit()
    store._connection.close()


# --------------------------------------------------------------------------- #
# query.jsonl
# --------------------------------------------------------------------------- #

def run_query_case(store: MetricsStore, call: dict) -> object:
    import asyncio

    method = call["method"]
    params = dict(call.get("params", {}))
    # corpus 是 JSON，datetime 不能直接入语料；约定 * _iso 后缀是 ISO 文本。
    if "since_iso" in params:
        params["since"] = datetime.fromisoformat(params.pop("since_iso"))
    if method == "snapshot":
        return asyncio.run(store.snapshot(**params))
    if method == "key_stats":
        return asyncio.run(store.key_stats(**params))
    if method == "request_history":
        return asyncio.run(store.request_history(**params))
    if method == "time_series":
        return asyncio.run(store.time_series(**params))
    raise AssertionError(f"unknown method {method}")


QUERY_CALLS: list[tuple[str, str, dict]] = [
    ("snapshot_default_24h", "metrics",
     {"method": "snapshot", "params": {"hours": 24.0}}),
    ("snapshot_all_history", "metrics",
     {"method": "snapshot", "params": {"hours": None}}),
    ("snapshot_with_since", "metrics",
     {"method": "snapshot", "params": {
         "since_iso": "2026-07-14T06:00:00+08:00"}}),
    ("snapshot_one_hour", "metrics",
     {"method": "snapshot", "params": {"hours": 1.0}}),
    ("key_stats_24h", "metrics",
     {"method": "key_stats",
      "params": {"model_id": "m-alpha", "key_name": "k1", "hours": 24.0}}),
    ("key_stats_all_time", "metrics",
     {"method": "key_stats",
      "params": {"model_id": "m-alpha", "key_name": "k1", "hours": None}}),
    ("key_stats_zero_hours_is_empty", "metrics",
     {"method": "key_stats",
      "params": {"model_id": "m-alpha", "key_name": "k1", "hours": 0.0}}),
    ("key_stats_missing_key", "metrics",
     {"method": "key_stats",
      "params": {"model_id": "nope", "key_name": "nope", "hours": 24.0}}),
    ("history_default", "metrics",
     {"method": "request_history", "params": {"hours": 24.0}}),
    ("history_all", "metrics",
     {"method": "request_history", "params": {"hours": None, "limit": 200}}),
    ("history_limit_3", "metrics",
     {"method": "request_history", "params": {"hours": 24.0, "limit": 3}}),
    ("history_before_id", "metrics",
     {"method": "request_history",
      "params": {"hours": None, "limit": 5, "before_id": 20}}),
    ("history_status_filter", "metrics",
     {"method": "request_history",
      "params": {"hours": None, "status_code": 500, "limit": 200}}),
    ("history_success_true", "metrics",
     {"method": "request_history",
      "params": {"hours": None, "success": True, "limit": 200}}),
    ("history_success_false", "metrics",
     {"method": "request_history",
      "params": {"hours": None, "success": False, "limit": 200}}),
    ("history_attributed_true", "metrics",
     {"method": "request_history",
      "params": {"hours": None, "attributed": True, "limit": 200}}),
    ("history_attributed_false", "metrics",
     {"method": "request_history",
      "params": {"hours": None, "attributed": False, "limit": 200}}),
    ("history_all_filters", "metrics",
     {"method": "request_history",
      "params": {"hours": None, "caller_type": "local", "model_id": "m-alpha",
                 "requested_model_id": "m-alpha", "provider_id": "p-openai",
                 "pool_name": "pool-a", "upstream_model_id": "gpt-4o",
                 "key_name": "k1", "status_code": 200, "success": True,
                 "attributed": True, "limit": 10}}),
    ("series_1h_60s", "metrics",
     {"method": "time_series",
      "params": {"hours": 1.0, "bucket_seconds": 60}}),
    ("series_24h_3600s", "metrics",
     {"method": "time_series",
      "params": {"hours": 24.0, "bucket_seconds": 3600}}),
    ("series_filtered", "metrics",
     {"method": "time_series",
      "params": {"hours": 24.0, "bucket_seconds": 3600, "model_id": "m-alpha",
                 "success": True}}),
    ("series_attributed", "metrics",
     {"method": "time_series",
      "params": {"hours": 24.0, "bucket_seconds": 3600, "attributed": True}}),
    # 桶长 7 秒不整除 3600，但 0.5 小时只有 258 个点，不会撞上 500 点上限。
    ("series_odd_bucket", "metrics",
     {"method": "time_series",
      "params": {"hours": 0.5, "bucket_seconds": 7}}),
    ("series_epoch_anchor", "epoch",
     {"method": "time_series",
      "params": {"hours": 1.0, "bucket_seconds": 900}}),
    ("snapshot_epoch", "epoch",
     {"method": "snapshot", "params": {"hours": None}}),
    ("history_epoch", "epoch",
     {"method": "request_history", "params": {"hours": None, "limit": 200}}),
]

ERROR_CALLS: list[tuple[str, str, dict, str]] = [
    ("history_hours_zero", "metrics",
     {"method": "request_history", "params": {"hours": 0.0}},
     "hours must be greater than zero or null"),
    ("history_hours_negative", "metrics",
     {"method": "request_history", "params": {"hours": -1.0}},
     "hours must be greater than zero or null"),
    ("history_limit_zero", "metrics",
     {"method": "request_history", "params": {"hours": 24.0, "limit": 0}},
     "limit must be between 1 and 200"),
    ("history_limit_too_large", "metrics",
     {"method": "request_history", "params": {"hours": 24.0, "limit": 201}},
     "limit must be between 1 and 200"),
    ("history_before_id_zero", "metrics",
     {"method": "request_history",
      "params": {"hours": 24.0, "before_id": 0}},
     "before_id must be greater than zero"),
    ("series_hours_zero", "metrics",
     {"method": "time_series", "params": {"hours": 0.0, "bucket_seconds": 60}},
     "hours must be greater than zero"),
    ("series_bucket_zero", "metrics",
     {"method": "time_series", "params": {"hours": 1.0, "bucket_seconds": 0}},
     "bucket_seconds must be greater than zero"),
    ("series_too_many_points", "metrics",
     {"method": "time_series", "params": {"hours": 24.0, "bucket_seconds": 60}},
     f"time series cannot exceed {MAX_SERIES_POINTS} points"),
]


def build_query_cases(metrics_db: Path, epoch_db: Path) -> list[dict]:
    import asyncio

    cases: list[dict] = []
    databases = {"metrics": (metrics_db, NOW), "epoch": (epoch_db, EPOCH_NOW)}

    for name, db_name, call in QUERY_CALLS:
        path, clock = databases[db_name]
        patch_clock(clock)
        store = MetricsStore(path)
        # 固定活跃请求数，让 active_requests 可复现。
        store.acquire_active()
        store.acquire_active()
        result = run_query_case(store, call)
        store.release_active()
        store.release_active()
        store._connection.close()
        # database_path 是机器相关的绝对路径；两边都替换成占位符再比对。
        if isinstance(result, dict) and "database_path" in result:
            result["database_path"] = "<db>"
        cases.append({
            "name": name, "kind": "query", "db": db_name,
            "now": compact(clock.isoformat()),
            "call": call, "note": "", "expect": compact(result),
        })

    for name, db_name, call, message in ERROR_CALLS:
        path, clock = databases[db_name]
        patch_clock(clock)
        store = MetricsStore(path)
        try:
            run_query_case(store, call)
        except ValueError as exc:
            actual = str(exc)
            assert actual == message, f"{name}: {actual!r} != {message!r}"
        else:
            raise AssertionError(f"{name}: 期望 ValueError 但没有抛出")
        finally:
            store._connection.close()
        cases.append({
            "name": name, "kind": "error", "db": db_name,
            "now": compact(clock.isoformat()), "call": call,
            "note": "校验失败的错误文本必须逐字一致",
            "expect": compact(message),
        })
    return cases


# --------------------------------------------------------------------------- #
# 主流程
# --------------------------------------------------------------------------- #

def build_corpus_files(output_dir: Path) -> dict[str, str]:
    """生成全部语料文本（不落盘）。"""
    metrics_db = output_dir / "metrics.sqlite3"
    epoch_db = output_dir / "epoch.sqlite3"
    build_metrics_database(metrics_db)
    build_epoch_database(epoch_db)
    return {
        "schema.jsonl": render(build_schema_cases()),
        "legacy.jsonl": render(build_legacy_cases()),
        "record.jsonl": render(build_record_cases()),
        "usage.jsonl": render(build_usage_cases()),
        "clock.jsonl": render(build_clock_cases()),
        "filter.jsonl": render(build_filter_cases()),
        "query.jsonl": render(build_query_cases(metrics_db, epoch_db)),
    }


DATABASE_FILES = ["metrics.sqlite3", "epoch.sqlite3", "legacy.sqlite3"]


def regenerate_databases(output_dir: Path) -> None:
    """重写三个夹具库（legacy 也要重建，否则 --check 无从比对）。"""
    build_metrics_database(output_dir / "metrics.sqlite3")
    build_epoch_database(output_dir / "epoch.sqlite3")
    make_legacy_database(output_dir / "legacy.sqlite3")


def legacy_snapshot(path: Path) -> dict:
    """把 legacy 夹具库「打开升级」后再导出逻辑快照。

    提交的 legacy.sqlite3 故意保持**升级前**的旧结构（它就是用户手里那个文件），
    列不全，没法直接按新列 SELECT。因此复制一份、让参照实现打开它完成升级，再
    比对升级结果 —— 这恰好就是用户换二进制时走的路径。
    """
    with tempfile.TemporaryDirectory() as tmp:
        copy = Path(tmp) / "legacy.sqlite3"
        shutil.copyfile(path, copy)
        store = MetricsStore(copy)
        store._connection.close()
        return logical_dump(copy)


def check(output_dir: Path) -> int:
    status = 0
    with tempfile.TemporaryDirectory() as tmp:
        fresh_dir = Path(tmp)
        fresh_files = build_corpus_files(fresh_dir)
        # legacy.sqlite3 由 build_legacy_cases 在临时目录里造，这里补一份正式的。
        make_legacy_database(fresh_dir / "legacy.sqlite3")

        for name, content in fresh_files.items():
            path = output_dir / name
            if not path.exists():
                print(f"语料缺失: {path}", file=sys.stderr)
                status = 1
                continue
            if path.read_bytes().decode("utf-8") != content:
                print(f"语料已过期: {path}", file=sys.stderr)
                status = 1

        for name in DATABASE_FILES:
            committed = output_dir / name
            if not committed.exists():
                print(f"语料缺失: {committed}", file=sys.stderr)
                status = 1
                continue
            # 库文件比对逻辑内容：页布局会随版本变化，比字节只会假失败。
            snapshot = legacy_snapshot if name == "legacy.sqlite3" else logical_dump
            if snapshot(fresh_dir / name) != snapshot(committed):
                print(f"夹具库已过期: {committed}", file=sys.stderr)
                status = 1

    if status == 0:
        print(f"语料最新: {output_dir}")
    return status


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    args.output_dir.mkdir(parents=True, exist_ok=True)

    if args.check:
        return check(args.output_dir)

    files = build_corpus_files(args.output_dir)
    # legacy 夹具库：先造出旧结构，再由参照实现打开升级，保证提交的是"用户
    # 手里那个还没升级的库"。
    legacy_path = args.output_dir / "legacy.sqlite3"
    make_legacy_database(legacy_path)

    total_lines = 0
    for name, content in files.items():
        path = args.output_dir / name
        path.write_text(content, encoding="utf-8", newline="\n")
        lines = content.count("\n")
        total_lines += lines
        print(f"已写入 {lines} 条语料: {path}")

    # WAL 内容合并回主库，避免提交 -wal / -shm 残file。
    for name in DATABASE_FILES:
        path = args.output_dir / name
        if not path.exists():
            continue
        conn = sqlite3.connect(path)
        try:
            conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            conn.commit()
        finally:
            conn.close()
        for suffix in ("-wal", "-shm"):
            leftover = Path(str(path) + suffix)
            if leftover.exists():
                leftover.unlink()
        print(f"已写入夹具库: {path}")

    print(f"共 {total_lines} 条语料")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
