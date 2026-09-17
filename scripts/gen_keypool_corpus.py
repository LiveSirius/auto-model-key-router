#!/usr/bin/env python3
"""生成 key_pool 选择逻辑与健康状态的随机对拍语料。

key 选择是「静默失效」的高风险区：游标推进、并发数最低者的筛选、粘滞命中/失效、
priority 与 only_first 的差异，都不会在结果错误时报警，只会表现得「偶尔不均」。

这里直接驱动参照实现（``KeyPool.next_key`` 等）在给定配置与操作序列下产出结果，
Go 侧重放同一序列并逐条比对。操作序列用**确定性随机**（固定种子）生成，因此
语料可复现。

用法::

    python scripts/gen_keypool_corpus.py
    python scripts/gen_keypool_corpus.py --check
"""

from __future__ import annotations

import argparse
import asyncio
import json
import random
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router.config import RouterConfig  # noqa: E402
from auto_model_key_router.key_pool import KeyPool  # noqa: E402

DEFAULT_DIR = REPO_ROOT / "internal" / "keypool" / "testdata"


def canonical(obj: object) -> str:
    return json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def dumps_ordered(obj: object) -> str:
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


# --------------------------------------------------------------------------- #
# 配置夹具
# --------------------------------------------------------------------------- #

def make_config(keys: list[str], *, mode: str = "round_robin",
                enabled: dict[str, bool] | None = None,
                visitor: dict[str, bool] | None = None,
                extra: dict | None = None) -> dict:
    enabled = enabled or {}
    visitor = visitor or {}
    key_block = {}
    for name in keys:
        entry: dict = {"api_key": f"sk-{name}"}
        if name in enabled:
            entry["enabled"] = enabled[name]
        if name in visitor:
            entry["allow_visitor"] = visitor[name]
        key_block[name] = entry
    raw: dict = {
        "config_version": 4,
        "local_api_key": "local",
        "providers": {"p": {"base_url": "https://a.example", "keys": key_block}},
        "models": {
            "m": {
                "targets": [
                    {"provider": "p", "key": name, "upstream_model": f"u-{name}"}
                    for name in keys
                ],
                "routing_mode": mode,
            }
        },
    }
    if extra:
        raw.update(extra)
    return raw


def multi_model_config() -> dict:
    """两个模型共用同一组 provider key，用于验证游标是按模型独立的。"""
    return {
        "config_version": 4,
        "local_api_key": "local",
        "providers": {
            "p": {"base_url": "https://a.example", "keys": {
                "k1": {"api_key": "1"}, "k2": {"api_key": "2"}, "k3": {"api_key": "3"},
            }}
        },
        "models": {
            "m1": {"targets": [
                {"provider": "p", "key": "k1", "upstream_model": "u1"},
                {"provider": "p", "key": "k2", "upstream_model": "u2"},
                {"provider": "p", "key": "k3", "upstream_model": "u3"},
            ], "routing_mode": "round_robin"},
            "m2": {"targets": [
                {"provider": "p", "key": "k1", "upstream_model": "v1"},
                {"provider": "p", "key": "k2", "upstream_model": "v2"},
            ], "routing_mode": "round_robin"},
        },
    }


def alias_config() -> dict:
    return {
        "config_version": 4,
        "local_api_key": "local",
        "providers": {"p": {"base_url": "https://a.example", "keys": {
            "k1": {"api_key": "1"}, "k2": {"api_key": "2"},
        }}},
        "models": {
            "real": {
                "targets": [
                    {"provider": "p", "key": "k1", "upstream_model": "u1"},
                    {"provider": "p", "key": "k2", "upstream_model": "u2"},
                ],
                "aliases": ["alias1", "alias2"],
                "hidden_aliases": ["hiddenX"],
                "routing_mode": "round_robin",
            }
        },
    }


def unified_config() -> dict:
    return {
        "config_version": 4,
        "local_api_key": "local",
        "providers": {"p": {"base_url": "https://a.example", "keys": {
            "k1": {"api_key": "1"}, "k2": {"api_key": "2"}, "k3": {"api_key": "3"},
        }}},
        "models": {
            "m": {"targets": [
                {"provider": "p", "key": "k1", "upstream_model": "u1"},
                {"provider": "p", "key": "k2", "upstream_model": "u2"},
            ], "aliases": ["m-alias"]},
            "img": {"targets": [{"provider": "p", "key": "k3", "upstream_model": "ui"}]},
            "emb": {"targets": [{"provider": "p", "key": "k3", "upstream_model": "ue"}]},
        },
        "unified_model": {
            "default": {"primary": {"model": "m"}, "fallback": {"model": "img"}},
            "image": {"primary": {"model": "img"}},
            "embeddings": {"primary": {"model": "emb"}},
        },
        "tasks": {"TASK_1": {"model": "m", "params": {"temperature": 0.3}}},
    }


# --------------------------------------------------------------------------- #
# 操作序列
# --------------------------------------------------------------------------- #

async def run_sequence(raw: dict, ops: list[dict]) -> list[dict]:
    """按操作序列驱动参照实现，返回每步的可观察结果。"""
    config = RouterConfig.from_dict(raw)
    pool = KeyPool(config)
    results: list[dict] = []
    for op in ops:
        kind = op["op"]
        try:
            if kind == "next":
                key = await pool.next_key(
                    op["model"],
                    set(op.get("excluded") or []),
                    visitor_only=op.get("visitor_only", False),
                    affinity_key=op.get("affinity_key"),
                )
                results.append({"op": kind, "ok": True, "key": key.name})
            elif kind == "acquire":
                await pool.acquire_key(op["model"], op["key"])
                results.append({"op": kind, "ok": True})
            elif kind == "release":
                await pool.release_key(op["model"], op["key"])
                results.append({"op": kind, "ok": True})
            elif kind == "success":
                await pool.mark_success(op["model"], op["key"])
                results.append({"op": kind, "ok": True})
            elif kind == "failure":
                await pool.mark_failure(
                    op["model"], op["key"],
                    status_code=op.get("status_code"),
                    retry_after=op.get("retry_after"),
                )
                results.append({"op": kind, "ok": True})
            elif kind == "cooling":
                value = pool._is_cooling_down(op["model"], op["key"])
                results.append({"op": kind, "ok": True, "value": value})
            elif kind == "active_count":
                value = pool._active_requests.get((op["model"], op["key"]), 0)
                results.append({"op": kind, "ok": True, "value": value})
            elif kind == "model_ids":
                results.append({"op": kind, "ok": True, "value": pool.model_ids})
            elif kind == "public_ids":
                results.append({"op": kind, "ok": True, "value": pool.public_model_ids})
            elif kind == "hidden_ids":
                results.append({"op": kind, "ok": True, "value": pool.hidden_model_ids})
            elif kind == "available_ids":
                results.append({"op": kind, "ok": True,
                                "value": pool.available_model_ids(
                                    visitor_only=op.get("visitor_only", False))})
            elif kind == "resolve_model":
                results.append({"op": kind, "ok": True, "value": pool.resolve_model_id(op["model"])})
            elif kind == "resolve_visitor":
                results.append({"op": kind, "ok": True, "value": pool.resolve_visitor_model_id(op["model"])})
            elif kind == "routing_mode":
                results.append({"op": kind, "ok": True, "value": pool.routing_mode(op["model"])})
            elif kind == "key_count":
                results.append({"op": kind, "ok": True, "value": pool.key_count(op["model"])})
            elif kind == "visitor_key_count":
                results.append({"op": kind, "ok": True, "value": pool.visitor_key_count(op["model"])})
            elif kind == "resolve_route":
                # 区分「未指定 key」（None）与「显式空 key」（""）：参照实现里
                # 前者保留计划原值、后者会替换成空串。
                key_arg = op["key"] if "key" in op else None
                value = pool.resolve_route(op["model"], key_arg, path=op.get("path"))
                results.append({"op": kind, "ok": True, "value": list(value)})
            elif kind == "task_plan":
                plan = pool.task_plan(op["model"])
                if plan is None:
                    results.append({"op": kind, "ok": True, "value": None})
                else:
                    results.append({"op": kind, "ok": True, "value": {
                        "primary": {"model": plan.primary.model, "key": plan.primary.key},
                        "fallback": (
                            {"model": plan.fallback.model, "key": plan.fallback.key}
                            if plan.fallback else None
                        ),
                    }})
            elif kind == "unified_route":
                results.append({"op": kind, "ok": True, "value": pool.unified_route})
            elif kind == "task_params":
                results.append({"op": kind, "ok": True, "value": pool.task_params(op["model"])})
            else:
                raise AssertionError(f"未知操作: {kind}")
        except Exception as exc:  # noqa: BLE001 - 语料要记录任意异常
            results.append({
                "op": kind, "ok": False,
                "error_type": type(exc).__name__, "error": str(exc),
            })
    return results


# --------------------------------------------------------------------------- #
# 用例
# --------------------------------------------------------------------------- #

def build_cases() -> list[dict]:
    cases: list[dict] = []

    async def add(name: str, raw: dict, ops: list[dict], *, note: str = "") -> None:
        # 钉住能力缓存路径：否则 KeyPool 构造时会去读本机真实的
        # endpoint_capabilities 文件，语料就依赖运行环境了。指向一个不存在的
        # 路径即可（Load 对缺失文件返回空状态）。
        raw = {**raw, "endpoint_capabilities_path": "/nonexistent/amkr-corpus-caps.json"}
        results = await run_sequence(raw, ops)
        entry = {"name": name, "input": dumps_ordered(raw),
                 "ops": dumps_ordered(ops), "results": canonical(results)}
        if note:
            entry["note"] = note
        cases.append(entry)

    async def build() -> None:
        three = ["k1", "k2", "k3"]

        # ---- round_robin 基本轮转 ----
        await add("rr_round_robin_cycle", make_config(three),
                  [{"op": "next", "model": "m"} for _ in range(9)])
        await add("rr_round_robin_cycle_release_each", make_config(three),
                  sum([[{"op": "next", "model": "m"},
                        {"op": "release", "model": "m", "key": "k1"},
                        {"op": "release", "model": "m", "key": "k2"},
                        {"op": "release", "model": "m", "key": "k3"}] for _ in range(3)], []))
        await add("rr_single_key", make_config(["only"]),
                  [{"op": "next", "model": "m"} for _ in range(4)])
        await add("rr_two_keys", make_config(["a", "b"]),
                  [{"op": "next", "model": "m"} for _ in range(6)])

        # ---- 并发数最低者优先：不释放时反复选同一个 ----
        await add("rr_stays_on_lowest_when_not_released", make_config(three),
                  [{"op": "next", "model": "m"} for _ in range(6)],
                  note="不释放时并发数递增，游标应持续选中并发数最低者")
        await add("rr_prefers_released_key", make_config(three),
                  [{"op": "next", "model": "m"},
                   {"op": "next", "model": "m"},
                   {"op": "next", "model": "m"},
                   {"op": "release", "model": "m", "key": "k1"},
                   {"op": "next", "model": "m"},
                   {"op": "next", "model": "m"}])

        # ---- excluded ----
        await add("rr_excluded_one", make_config(three),
                  [{"op": "next", "model": "m", "excluded": ["k2"]} for _ in range(4)])
        await add("rr_excluded_all", make_config(three),
                  [{"op": "next", "model": "m", "excluded": three}])
        await add("rr_excluded_two", make_config(three),
                  [{"op": "next", "model": "m", "excluded": ["k2", "k3"]} for _ in range(3)])

        # ---- priority ----
        await add("priority_always_first", make_config(three, mode="priority"),
                  [{"op": "next", "model": "m"} for _ in range(4)],
                  note="priority 忽略并发数与游标，恒取配置顺序第一个可用项")
        await add("priority_skips_excluded", make_config(three, mode="priority"),
                  [{"op": "next", "model": "m", "excluded": ["k1"]} for _ in range(3)])
        await add("priority_all_excluded", make_config(three, mode="priority"),
                  [{"op": "next", "model": "m", "excluded": three}])
        await add("priority_does_not_advance_cursor", make_config(three, mode="priority"),
                  [{"op": "next", "model": "m"},
                   {"op": "next", "model": "m", "excluded": ["k1"]},
                   {"op": "next", "model": "m"}])

        # ---- only_first ----
        await add("only_first_always_first", make_config(three, mode="only_first"),
                  [{"op": "next", "model": "m"} for _ in range(4)])
        await add("only_first_excluded_fails", make_config(three, mode="only_first"),
                  [{"op": "next", "model": "m", "excluded": ["k1"]}],
                  note="only_first 只用第一个；被排除即失败，不尝试其余 key")
        await add("only_first_ignores_cooldown", make_config(three, mode="only_first"),
                  [{"op": "failure", "model": "m", "key": "k1", "status_code": 429},
                   {"op": "cooling", "model": "m", "key": "k1"},
                   {"op": "next", "model": "m"}],
                  note="only_first 不参与冷却排除，冷却中仍返回首个 key")

        # ---- 禁用与访客 ----
        await add("disabled_keys_skipped",
                  make_config(three, enabled={"k2": False}),
                  [{"op": "next", "model": "m"} for _ in range(4)]
                  + [{"op": "key_count", "model": "m"}])
        await add("all_disabled",
                  make_config(three, enabled={"k1": False, "k2": False, "k3": False}),
                  [{"op": "next", "model": "m"}, {"op": "key_count", "model": "m"}])
        await add("visitor_only",
                  make_config(three, visitor={"k1": True, "k3": True}),
                  [{"op": "next", "model": "m", "visitor_only": True} for _ in range(4)]
                  + [{"op": "visitor_key_count", "model": "m"},
                     {"op": "key_count", "model": "m"}])
        await add("visitor_only_none_allowed", make_config(three),
                  [{"op": "next", "model": "m", "visitor_only": True}])

        # ---- 粘滞 ----
        await add("sticky_same_affinity_keeps_key", make_config(three),
                  [{"op": "next", "model": "m", "affinity_key": "aaa"} for _ in range(4)],
                  note="同一 affinity 重复请求应稳定命中同一 key")
        await add("sticky_different_affinity_distributes", make_config(three),
                  [{"op": "next", "model": "m", "affinity_key": key}
                   for key in ["a", "b", "c", "d"]])
        await add("sticky_released_key_reenables", make_config(three),
                  [{"op": "next", "model": "m", "affinity_key": "z"},
                   {"op": "next", "model": "m", "affinity_key": "z"},
                   {"op": "release", "model": "m", "key": "k1"},
                   {"op": "release", "model": "m", "key": "k2"},
                   {"op": "release", "model": "m", "key": "k3"},
                   {"op": "next", "model": "m", "affinity_key": "z"}])
        await add("sticky_ignored_for_priority", make_config(three, mode="priority"),
                  [{"op": "next", "model": "m", "affinity_key": "aaa"} for _ in range(3)],
                  note="priority/only_first 不受粘滞影响")

        # ---- 冷却 ----
        await add("cooldown_429_immediate", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1", "status_code": 429},
                   {"op": "cooling", "model": "m", "key": "k1"},
                   {"op": "cooling", "model": "m", "key": "k2"},
                   {"op": "next", "model": "m"}],
                  note="429 不累计阈值，立即冷却")
        await add("cooldown_threshold_accumulates", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1", "status_code": 500},
                   {"op": "cooling", "model": "m", "key": "k1"},
                   {"op": "failure", "model": "m", "key": "k1", "status_code": 500},
                   {"op": "cooling", "model": "m", "key": "k1"}],
                  note="非 429 需累计到 key_failure_threshold(2) 才冷却")
        await add("cooldown_success_clears", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1", "status_code": 429},
                   {"op": "success", "model": "m", "key": "k1"},
                   {"op": "cooling", "model": "m", "key": "k1"}])
        await add("cooldown_soft_fallback_all_cooling", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1", "status_code": 429},
                   {"op": "failure", "model": "m", "key": "k2", "status_code": 429},
                   {"op": "failure", "model": "m", "key": "k3", "status_code": 429},
                   {"op": "next", "model": "m"}],
                  note="全部冷却时回退到未排除集合，冷却中的 key 仍会被选中")
        await add("cooldown_retry_after_used", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1",
                    "status_code": 429, "retry_after": 12.5},
                   {"op": "cooling", "model": "m", "key": "k1"}])
        await add("cooldown_retry_after_capped", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1",
                    "status_code": 429, "retry_after": 99999.0},
                   {"op": "cooling", "model": "m", "key": "k1"}])
        await add("cooldown_skips_cooled_key", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1", "status_code": 429}]
                  + [{"op": "next", "model": "m"} for _ in range(3)],
                  note="冷却中的 key 在有可用替代时被排除")
        await add("cooldown_none_status", make_config(three),
                  [{"op": "failure", "model": "m", "key": "k1"},
                   {"op": "cooling", "model": "m", "key": "k1"},
                   {"op": "failure", "model": "m", "key": "k1"},
                   {"op": "cooling", "model": "m", "key": "k1"}])

        # ---- 并发计数 ----
        await add("acquire_release_counts", make_config(three),
                  [{"op": "acquire", "model": "m", "key": "k1"},
                   {"op": "active_count", "model": "m", "key": "k1"},
                   {"op": "acquire", "model": "m", "key": "k1"},
                   {"op": "active_count", "model": "m", "key": "k1"},
                   {"op": "release", "model": "m", "key": "k1"},
                   {"op": "active_count", "model": "m", "key": "k1"},
                   {"op": "release", "model": "m", "key": "k1"},
                   {"op": "active_count", "model": "m", "key": "k1"}])
        await add("release_below_zero_ignored", make_config(three),
                  [{"op": "release", "model": "m", "key": "k1"},
                   {"op": "active_count", "model": "m", "key": "k1"}])
        await add("rr_observes_acquired_counts", make_config(three),
                  [{"op": "acquire", "model": "m", "key": "k1"},
                   {"op": "acquire", "model": "m", "key": "k1"},
                   {"op": "next", "model": "m"}],
                  note="已被占用的 key 并发数高，轮转应避开")

        # ---- 多模型 ----
        await add("multi_model_independent_cursors", multi_model_config(),
                  [{"op": "next", "model": "m1"},
                   {"op": "next", "model": "m2"},
                   {"op": "next", "model": "m1"},
                   {"op": "next", "model": "m2"},
                   {"op": "next", "model": "m1"},
                   {"op": "next", "model": "m2"}],
                  note="游标按模型独立，互不干扰")
        await add("multi_model_active_counts_isolated", multi_model_config(),
                  [{"op": "acquire", "model": "m1", "key": "k1"},
                   {"op": "active_count", "model": "m1", "key": "k1"},
                   {"op": "active_count", "model": "m2", "key": "k1"}],
                  note="在途计数按 (模型, key) 隔离")

        # ---- 别名 / 隐藏名 / 列表 ----
        await add("aliases_lists", alias_config(),
                  [{"op": "model_ids"}, {"op": "public_ids"}, {"op": "hidden_ids"},
                   {"op": "available_ids"},
                   {"op": "available_ids", "visitor_only": True},
                   {"op": "resolve_model", "model": "alias1"},
                   {"op": "resolve_model", "model": "alias2"},
                   {"op": "resolve_model", "model": "hiddenX"},
                   {"op": "resolve_model", "model": "unknown"},
                   {"op": "key_count", "model": "alias1"},
                   {"op": "routing_mode", "model": "alias1"}])
        await add("alias_routing_shares_cursor", alias_config(),
                  [{"op": "next", "model": "real"},
                   {"op": "next", "model": "alias1"},
                   {"op": "next", "model": "alias2"},
                   {"op": "next", "model": "real"}],
                  note="别名与真实 ID 共用同一模型，游标与并发计数共享")

        # ---- unified / 任务 ----
        await add("unified_route_and_resolve", unified_config(),
                  [{"op": "unified_route"},
                   {"op": "resolve_route", "model": "unified-model", "path": "chat/completions"},
                   {"op": "resolve_route", "model": "unified-model", "path": "chat/completions", "key": "k2"},
                   {"op": "resolve_route", "model": "unified-model", "path": "chat/completions", "key": ""},
                   {"op": "resolve_route", "model": "unified-model", "path": "images/generations"},
                   {"op": "resolve_route", "model": "unified-model", "path": "embeddings"},
                   {"op": "resolve_route", "model": "unified-model", "path": ""},
                   {"op": "resolve_route", "model": "m-alias"},
                   {"op": "resolve_route", "model": "m-alias", "key": ""},
                   {"op": "resolve_route", "model": "TASK_1"},
                   {"op": "task_params", "model": "TASK_1"}],
                  note="key=None 保留计划原值，key='' 会替换成空串——两者行为不同")
        await add("unified_absent", make_config(three),
                  [{"op": "unified_route"},
                   {"op": "resolve_route", "model": "unified-model", "path": "chat/completions"},
                   {"op": "resolve_route", "model": "TASK_MISSING"}])
        await add("task_plan_lookup", unified_config(),
                  [{"op": "task_plan", "model": "TASK_1"},
                   {"op": "task_plan", "model": "m"},
                   {"op": "task_plan", "model": "TASK_MISSING"},
                   {"op": "task_params", "model": "TASK_MISSING"},
                   {"op": "task_params", "model": "m"}])
        await add("available_ids_includes_unified", unified_config(),
                  [{"op": "available_ids"}, {"op": "model_ids"}, {"op": "public_ids"}])

        # ---- 端点能力归类 ----
        await add("route_kind_classification", make_config(three),
                  [{"op": "resolve_route", "model": "m", "path": "images/edits"},
                   {"op": "resolve_route", "model": "m", "path": "images/generations"},
                   {"op": "resolve_route", "model": "m", "path": "embeddings"},
                   {"op": "resolve_route", "model": "m", "path": "chat/completions"},
                   {"op": "resolve_route", "model": "m", "path": "messages"}])

        # ---- 未配置模型 ----
        await add("unknown_model_next", make_config(three),
                  [{"op": "next", "model": "ghost"}])
        await add("unknown_model_resolve", make_config(three),
                  [{"op": "resolve_model", "model": "ghost"},
                   {"op": "routing_mode", "model": "ghost"},
                   {"op": "key_count", "model": "ghost"}])

        # ---- 随机序列（固定种子，覆盖组合） ----
        await random_sequences(add)

    asyncio.run(build())
    return cases


async def random_sequences(add) -> None:
    """固定种子的随机操作序列，覆盖人工用例没想到的组合。"""
    for seed in (1, 2, 3):
        rng = random.Random(seed)
        keys = ["k1", "k2", "k3", "k4"]
        mode = rng.choice(["round_robin", "priority", "only_first"])
        raw = make_config(keys, mode=mode,
                          enabled={"k3": rng.random() < 0.5},
                          visitor={"k2": True, "k4": rng.random() < 0.5})
        ops: list[dict] = []
        for _ in range(60):
            choice = rng.random()
            if choice < 0.45:
                op: dict = {"op": "next", "model": "m"}
                if rng.random() < 0.25:
                    op["excluded"] = rng.sample(keys, rng.randint(1, 2))
                if rng.random() < 0.3:
                    op["affinity_key"] = rng.choice(["a", "b"])
                if rng.random() < 0.2:
                    op["visitor_only"] = True
                ops.append(op)
            elif choice < 0.6:
                ops.append({"op": "failure", "model": "m",
                            "key": rng.choice(keys),
                            "status_code": rng.choice([429, 500, 503, None]),
                            "retry_after": rng.choice([None, 5.0, 999.0])})
            elif choice < 0.7:
                ops.append({"op": "success", "model": "m", "key": rng.choice(keys)})
            elif choice < 0.8:
                ops.append({"op": "release", "model": "m", "key": rng.choice(keys)})
            elif choice < 0.9:
                ops.append({"op": "acquire", "model": "m", "key": rng.choice(keys)})
            else:
                ops.append({"op": "cooling", "model": "m", "key": rng.choice(keys)})
        await add(f"random_seed{seed}", raw, ops,
                  note=f"固定种子随机序列，mode={mode}")


def render(cases: list[dict]) -> str:
    return "\n".join(json.dumps(c, ensure_ascii=False, sort_keys=True) for c in cases) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    path = args.output_dir / "selection.jsonl"
    content = render(build_cases())

    if args.check:
        if not path.exists():
            print(f"语料缺失: {path}", file=sys.stderr)
            return 1
        if path.read_text(encoding="utf-8") != content:
            print(f"语料已过期: {path}", file=sys.stderr)
            return 1
        print(f"语料最新: {path}")
        return 0

    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8", newline="\n")
    print(f"已写入 {content.count(chr(10))} 条语料: {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
