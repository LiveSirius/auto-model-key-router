#!/usr/bin/env python3
"""生成 config 迁移与规范化的对拍语料（Python 侧为参照实现）。

直接 import 本仓库的 ``auto_model_key_router.config``，用真实实现算出期望值，
因此语料反映的是「代码到底怎么做」，而不是「文档说应该怎么做」。

覆盖三块纯函数：

1. ``migrate_config_data``     —— v0/v1/v2/v3 → v4，以及 v4 的幂等清理
2. ``normalize_upstream_routes`` —— 路由模式别名、路径补全
3. ``normalize_task_params``   —— 任务固定参数白名单与类型规整

错误信息也逐条记录：这些文本会经 management API 直接回给用户，属于契约的一部分。

用法::

    python scripts/gen_config_corpus.py           # 写入语料
    python scripts/gen_config_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router.config import (  # noqa: E402
    migrate_config_data,
    normalize_task_params,
    normalize_upstream_routes,
)

DEFAULT_DIR = REPO_ROOT / "internal" / "config" / "testdata"


def canonical(obj: object) -> str:
    """Go 的 canonical.Dumps 必须逐字节复现的形式（键排序）。"""
    return json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def dumps_ordered(obj: object) -> str:
    """保留字典插入顺序的紧凑形式，用于**输入**。

    输入必须保留插入顺序：Python 的 dict 有序，而若干行为依赖它——例如
    ``{"image": "a", "img": "b"}`` 两个别名都折叠到 images，后者胜出；若输入
    被排序，Go 侧解析后的顺序会改变，测出来的就不是真实语义。
    """
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


def record(call, *args, **kwargs) -> dict:
    """执行参照实现：成功记录 canonical 输出，失败记录异常类型与文本。"""
    try:
        result = call(*args, **kwargs)
    except Exception as exc:  # noqa: BLE001 - 语料要记录任意异常
        return {"ok": False, "error_type": type(exc).__name__, "error": str(exc)}
    return {"ok": True, "output": canonical(result)}


def wrap(name: str, call, raw, **kwargs) -> dict:
    """构造一条语料。

    ``input`` 保留原始插入顺序；``output`` 用 canonical 形式（Go 侧比对时也
    用 canonical，因此输出顺序不参与判定）。
    """
    entry: dict = {"name": name, "input": dumps_ordered(raw)}
    if kwargs:
        entry["kwargs"] = {k: dumps_ordered(v) for k, v in kwargs.items()}
    entry.update(record(call, raw, **kwargs))
    return entry


# --------------------------------------------------------------------------- #
# 1. migrate_config_data
# --------------------------------------------------------------------------- #

def migrate_cases() -> list[dict]:
    cases: list[dict] = []

    def add(name: str, raw: dict) -> None:
        cases.append(wrap(name, migrate_config_data, raw))

    # ---- v4 幂等与清理 ----
    add("v4_minimal", {"config_version": 4, "models": {}})
    add("v4_empty_providers", {"config_version": 4, "models": {}, "providers": {}})
    add(
        "v4_drops_pools",
        {
            "config_version": 4,
            "models": {"m": {"targets": [{"provider": "p", "key": "k"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "pools": {"x": {}}}},
        },
    )
    add(
        "v4_promotes_provider_capabilities",
        {
            "config_version": 4,
            "models": {},
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": {"k1": {"api_key": "x"}, "k2": {"api_key": "y", "capabilities": {"models": ["own"]}}},
                    "capabilities": {
                        "models": ["gpt-4o", "claude-3"],
                        "checked_at": "2026-01-01T00:00:00+00:00",
                        "errors": {"probe": "timeout"},
                        "route_status": {"openai": "ok"},
                    },
                }
            },
        },
    )
    add(
        "v4_capabilities_without_models_is_dropped",
        {
            "config_version": 4,
            "models": {},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "capabilities": {}}},
        },
    )
    add("v4_no_models_key", {"config_version": 4})
    add("v4_models_not_dict", {"config_version": 4, "models": []})
    add("v4_drops_available_models", {"config_version": 4, "models": {}, "providers": {"p": {"available_models": ["a"], "key_models": {}, "routes_by_key": {}, "_probe_cache": {}}}})

    # ---- 旧版 unified_model 折叠 ----
    add(
        "unified_legacy_default_only",
        {"config_version": 4, "models": {}, "unified_model": {"model": "gpt-4o-mini", "key": "main"}},
    )
    add(
        "unified_legacy_with_image",
        {
            "config_version": 4,
            "models": {},
            "unified_model": {"model": "gpt-4o-mini", "key": "main", "image_model": "dall-e-3", "image_key": "img"},
        },
    )
    add(
        "unified_legacy_empty_model_kept",
        {"config_version": 4, "models": {}, "unified_model": {"model": "", "key": "main"}},
    )
    add(
        "unified_already_v4_shape",
        {"config_version": 4, "models": {}, "unified_model": {"default": {"primary": {"model": "m", "key": None}}}},
    )
    add("unified_not_dict", {"config_version": 4, "models": {}, "unified_model": ["x"]})

    # ---- key_state_path → endpoint_capabilities_path ----
    add(
        "key_state_path_migrated",
        {"config_version": 4, "models": {}, "key_state_path": "/tmp/keys.json"},
    )
    add(
        "key_state_path_does_not_override",
        {"config_version": 4, "models": {}, "key_state_path": "/tmp/old.json", "endpoint_capabilities_path": "/tmp/new.json"},
    )

    # ---- 版本高于当前 ----
    add("version_5_rejected", {"config_version": 5, "models": {}})
    add("version_99_rejected", {"config_version": 99, "models": {}})

    # ---- v3 → v4：pool 展平 ----
    v3_pool = {
        "config_version": 3,
        "models": {
            "gpt-4o-mini": {
                "targets": [
                    {"provider": "openai", "pool": "primary", "upstream_model": "gpt-4o-mini"}
                ]
            }
        },
        "providers": {
            "openai": {
                "base_url": "https://api.openai.com",
                "keys": {"k1": {"api_key": "a"}, "k2": {"api_key": "b"}},
                "pools": {"primary": {"keys": ["k1", "k2"]}},
            }
        },
    }
    add("v3_pool_two_keys", v3_pool)
    add(
        "v3_pool_whitelist_filters_target",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "not-listed"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "pools": {"pl": {"keys": ["k"], "models": ["other"]}}}},
        },
    )
    add(
        "v3_pool_whitelist_allows_target",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "listed"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "pools": {"pl": {"keys": ["k"], "models": ["listed"]}}}},
        },
    )
    add(
        "v3_pool_empty_whitelist_filters_all",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "anything"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "pools": {"pl": {"keys": ["k"], "models": []}}}},
        },
    )
    add(
        "v3_pool_missing_models_key_no_filter",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "anything"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "pools": {"pl": {"keys": ["k"]}}}},
        },
    )
    add(
        "v3_empty_pool_yields_empty_targets",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "x"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {}, "pools": {"pl": {"keys": []}}}},
        },
    )
    add(
        "v3_unknown_pool_rejected",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "nope", "upstream_model": "x"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "pools": {}}},
        },
    )
    add(
        "v3_unknown_provider_rejected",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "ghost", "pool": "pl"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {}, "pools": {}}},
        },
    )
    add(
        "v3_legacy_pool_probe_folded_to_keys",
        {
            "config_version": 3,
            "models": {},
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": {"k1": {"api_key": "x"}, "k2": {"api_key": "y", "capabilities": {"models": ["own"]}}},
                    "pools": {"pl": {"keys": ["k1", "k2"], "available_models": ["m1"], "all_available_models": ["m2"], "models": ["m3"], "checked_at": "2026-01-01T00:00:00+00:00"}},
                }
            },
        },
    )
    add(
        "v3_provider_capabilities_promoted",
        {
            "config_version": 3,
            "models": {},
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": {"k": {"api_key": "x"}},
                    "capabilities": {"models": ["zz", "aa"], "checked_at": "2026-01-01T00:00:00+00:00"},
                }
            },
        },
    )
    add(
        "v3_no_targets_list",
        {"config_version": 3, "models": {"m": {}}, "providers": {}},
    )
    add(
        "v3_providers_not_dict",
        {"config_version": 3, "models": {"m": {"targets": []}}, "providers": []},
    )
    add(
        "v3_targets_not_list",
        {
            "config_version": 3,
            "models": {"m": {"targets": "bad"}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {}}},
        },
    )
    add(
        "v3_pool_name_empty_uses_all_keys",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "", "upstream_model": "x"}]}},
            "providers": {"p": {"base_url": "https://a.example", "pools": {}}},
        },
    )

    # ---- v1/v2 list layout ----
    add(
        "v1_list_layout",
        {
            "config_version": 1,
            "default_base_url": "https://api.openai.com",
            "models": [
                {
                    "id": "gpt-4o-mini",
                    "aliases": ["fast"],
                    "routing_mode": "priority",
                    "reasoning_effort": "low",
                    "native_first": False,
                    "keys": [
                        {"name": "main", "api_key": "sk-a"},
                        {"name": "backup", "api_key": "sk-b", "base_url": "https://other.example/v1", "upstream_model": "other-model", "enabled": False, "allow_visitor": True},
                    ],
                },
                {"id": "second", "keys": [{"api_key": "sk-c"}]},
            ],
        },
    )
    add(
        "v1_key_without_name_gets_generated",
        {"config_version": 1, "models": [{"id": "m", "keys": [{"api_key": "sk-a"}]}]},
    )
    add(
        "v1_duplicate_key_name_rejected",
        {
            "config_version": 1,
            "models": [{"id": "m", "keys": [{"name": "k", "api_key": "a"}, {"name": "k", "api_key": "b"}]}],
        },
    )
    add(
        "v1_missing_api_key_skipped",
        {"config_version": 1, "models": [{"id": "m", "keys": [{"name": "k", "api_key": "  "}, {"name": "j", "api_key": "x"}]}]},
    )
    add("v1_model_without_id_skipped", {"config_version": 1, "models": [{"keys": [{"api_key": "x"}]}]})
    add("v1_non_dict_model_skipped", {"config_version": 1, "models": ["bad", {"id": "m", "keys": []}]})
    add("v1_models_not_list", {"config_version": 1, "models": {"a": 1}})
    add("version_0_treated_as_v1", {"config_version": 0, "models": [{"id": "m", "keys": [{"api_key": "x"}]}]})
    add("no_version_with_list", {"models": [{"id": "m", "keys": [{"api_key": "x"}]}]})
    add(
        "v1_upstream_routes_merged",
        {
            "config_version": 1,
            "default_base_url": "https://shared.example",
            "upstream_routes": {"https://shared.example": {"anthropic": "anthropic"}},
            "models": [
                {
                    "id": "m",
                    "keys": [
                        {"name": "k1", "api_key": "a"},
                        {"name": "k2", "api_key": "b", "upstream_routes": {"anthropic": "anthropic"}},
                    ],
                }
            ],
        },
    )
    add("version_3_missing_models", {"config_version": 3})
    add("version_2_list", {"config_version": 2, "models": [{"id": "m", "keys": [{"api_key": "x"}]}]})

    # ---- config_version 的类型强转：int(x or 0) 对各种类型的反应 ----
    # int("4") 成功；int(4.0) 成功；int(True)=1；int([...]) 抛 TypeError。
    add("version_as_string", {"config_version": "4", "models": {}})
    add("version_as_string_padded", {"config_version": " 4 ", "models": {}})
    add("version_as_float", {"config_version": 4.0, "models": {}})
    add("version_as_float_fractional", {"config_version": 4.7, "models": {}})
    add("version_as_bool_true", {"config_version": True, "models": {}})
    add("version_as_none", {"config_version": None, "models": {}})
    add("version_as_empty_string", {"config_version": "", "models": {}})
    add("version_as_list", {"config_version": [4], "models": {}})
    add("version_as_dict", {"config_version": {"v": 4}, "models": {}})
    add("version_as_bad_string", {"config_version": "four", "models": {}})
    add("version_missing", {"models": {}})

    # ---- 迁移过程中的异常路径 ----
    add(
        "v3_legacy_probe_with_non_dict_keys",
        {
            "config_version": 3,
            "models": {},
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": ["not-a-dict"],
                    "capabilities": {"models": ["m1"]},
                }
            },
        },
    )
    add(
        "v3_legacy_probe_missing_keys",
        {
            "config_version": 3,
            "models": {},
            "providers": {
                "p": {"base_url": "https://a.example", "capabilities": {"models": ["m1"]}}
            },
        },
    )
    add(
        "v3_capability_models_not_list",
        {
            "config_version": 3,
            "models": {},
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": {"k": {"api_key": "x"}},
                    "capabilities": {"models": 5},
                }
            },
        },
    )
    add(
        "v3_pool_models_string_iterates_chars",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "a"}]}},
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": {"k": {"api_key": "x"}},
                    "pools": {"pl": {"keys": ["k"], "models": "ab"}},
                }
            },
        },
    )
    add(
        "v3_pool_keys_not_list",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "x"}]}},
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": {"k": {"api_key": "x"}},
                    "pools": {"pl": {"keys": "not-a-list"}},
                }
            },
        },
    )
    add(
        "v3_target_non_dict_skipped",
        {
            "config_version": 3,
            "models": {"m": {"targets": ["bad", {"provider": "p", "pool": "pl", "upstream_model": "x"}]}},
            "providers": {"p": {"base_url": "https://a.example", "pools": {"pl": {"keys": []}}}},
        },
    )
    add(
        "v3_provider_none",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl"}]}},
            "providers": {"p": None},
        },
    )
    add(
        "v3_pools_not_dict",
        {
            "config_version": 3,
            "models": {"m": {"targets": [{"provider": "p", "pool": "pl", "upstream_model": "x"}]}},
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "x"}}, "pools": "bad"}},
        },
    )

    # ---- provider key 的 enabled/allow_visitor 强转 ----
    add(
        "v1_enabled_truthy_values",
        {
            "config_version": 1,
            "models": [
                {
                    "id": "m",
                    "keys": [
                        {"name": "a", "api_key": "1", "enabled": 0},
                        {"name": "b", "api_key": "2", "enabled": "yes"},
                        {"name": "c", "api_key": "3", "allow_visitor": 1},
                        {"name": "d", "api_key": "4", "allow_visitor": []},
                    ],
                }
            ],
        },
    )
    add(
        "v1_key_not_dict_skipped",
        {"config_version": 1, "models": [{"id": "m", "keys": ["bad", {"api_key": "x"}]}]},
    )
    add(
        "v1_keys_not_list",
        {"config_version": 1, "models": [{"id": "m", "keys": {"a": 1}}]},
    )

    return cases


# --------------------------------------------------------------------------- #
# 2. normalize_upstream_routes
# --------------------------------------------------------------------------- #

def route_cases() -> list[dict]:
    cases: list[dict] = []

    def add(name: str, raw) -> None:
        cases.append(wrap(name, normalize_upstream_routes, raw))

    add("none", None)
    add("empty", {})
    add("not_dict", ["x"])
    add("openai_default_path", {"openai": "v1/chat/completions"})
    add("openai_prefix", {"openai": "my-prefix"})
    add("openai_v1_prefix", {"openai": "v1"})
    add("openai_v1_slash", {"openai": "v1/"})
    add("openai_full_prefix", {"openai": "some/path/v1/chat/completions"})
    add("anthropic_alias_messages", {"messages": "anthropic"})
    add("anthropic_alias_underscore", {"anthropic_messages": "x"})
    add("anthropic_alias_dash", {"anthropic-messages": "x"})
    add("codex_alias_responses", {"codex": "responses"})
    add("image_aliases", {"image": "img", "img": "img2", "dall-e": "d", "dalle": "d2"})
    add("images_generations_key", {"images/generations": "ig"})
    add("embeddings_alias", {"embedding": "e1", "embed": "e2"})
    add("chat_aliases", {"chat": "c", "chat_completions": "cc", "chat-completions": "cd", "openai_chat": "oc", "openai-chat": "od"})
    add("uppercase_mode", {"OPENAI": "x"})
    add("mode_with_spaces", {"  openai  ": "  x  "})
    add("unknown_mode_rejected", {"bogus": "x"})
    add("null_route_skipped", {"openai": None})
    add("blank_route_skipped", {"openai": "   "})
    add("empty_route_rejected", {"openai": ""})
    add("absolute_url_rejected", {"openai": "https://evil.example/v1"})
    add("double_slash_collapsed", {"openai": "a//b///c"})
    add("backslash_to_slash", {"openai": "a\\b"})
    add("leading_trailing_slashes", {"openai": "/prefix/"})
    add("query_rejected", {"openai": "p?x=1"})
    add("fragment_rejected", {"openai": "p#f"})
    add("only_slashes_rejected", {"openai": "///"})
    add("multi_modes", {"openai": "p1", "anthropic": "p2", "responses": "p3", "images": "p4", "embeddings": "p5"})
    add("non_string_value", {"openai": 123})
    add("trim_chars", {"openai": "  //p//  "})
    # 假值（falsy）边界：跳过判断用 str(x).strip()，取值用 x or ""，两者不一致。
    # 例如 0 不被跳过（str(0)="0" 非空），但取值时 0 or "" 得到 "" 而报错。
    add("route_zero", {"openai": 0})
    add("route_false", {"openai": False})
    add("route_empty_list", {"openai": []})
    add("route_empty_dict", {"openai": {}})
    add("route_true", {"openai": True})
    add("route_number_float", {"openai": 1.5})
    return cases


# --------------------------------------------------------------------------- #
# 3. normalize_task_params
# --------------------------------------------------------------------------- #

def task_param_cases() -> list[dict]:
    cases: list[dict] = []

    def add(name: str, raw, *, task_name: str = "TASK_1") -> None:
        cases.append(wrap(name, normalize_task_params, raw, task_name=task_name))

    add("none", None)
    add("empty", {})
    add("not_dict", ["x"])
    add("unknown_key_rejected", {"temprature": 0.5})
    add("all_sampling_params", {"temperature": 0.2, "top_p": 0.9, "top_k": 40, "frequency_penalty": 0.1, "presence_penalty": -0.1, "seed": 7})
    add("null_values_skipped", {"temperature": None, "seed": None})
    add("stop_as_list", {"stop": ["a", "b"]})
    add("stop_as_scalar", {"stop": "a"})
    add("stop_with_empty", {"stop": ["a", "", "b"]})
    add("stop_empty_list_omitted", {"stop": []})
    add("stop_non_string", {"stop": [1, 2.5]})
    add("top_k_float_integral", {"top_k": 40.0})
    add("top_k_float_fractional_rejected", {"top_k": 1.5})
    add("seed_string_integral", {"seed": "42"})
    add("seed_string_fractional_rejected", {"seed": "42.5"})
    add("seed_rejected_text", {"seed": "abc"})
    add("top_k_bool", {"top_k": True})
    add("temperature_string", {"temperature": "0.5"})
    add("temperature_bad", {"temperature": "hot"})
    add("reasoning_effort_valid", {"reasoning_effort": "high"})
    add("reasoning_effort_default_skipped", {"reasoning_effort": "default"})
    add("reasoning_effort_downstream_skipped", {"reasoning_effort": "downstream"})
    add("reasoning_effort_blank_skipped", {"reasoning_effort": "  "})
    add("reasoning_effort_invalid", {"reasoning_effort": "insane"})
    add("reasoning_effort_all_valid", {"reasoning_effort": "xhigh"})
    add("mixed", {"temperature": 1, "seed": 3, "stop": ["x"], "reasoning_effort": "minimal"})
    add("key_with_spaces_trimmed", {"  temperature  ": 0.3})
    add("key_with_spaces_unknown", {"  bogus  ": 0.3})
    add("task_name_in_error", {"bogus": 1}, task_name="TASK_ERR")
    return cases


def render(cases: list[dict]) -> str:
    return "\n".join(json.dumps(c, ensure_ascii=False, sort_keys=True) for c in cases) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    targets = [
        (args.output_dir / "migrate.jsonl", render(migrate_cases()), "migrate_config_data"),
        (args.output_dir / "routes.jsonl", render(route_cases()), "normalize_upstream_routes"),
        (args.output_dir / "taskparams.jsonl", render(task_param_cases()), "normalize_task_params"),
    ]

    if args.check:
        failed = False
        for path, content, label in targets:
            if not path.exists():
                print(f"[{label}] 语料缺失: {path}", file=sys.stderr)
                failed = True
            elif path.read_text(encoding="utf-8") != content:
                print(f"[{label}] 语料已过期: {path}", file=sys.stderr)
                failed = True
            else:
                print(f"[{label}] 语料最新: {path}")
        return 1 if failed else 0

    for path, content, label in targets:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8", newline="\n")
        print(f"[{label}] 已写入 {content.count(chr(10))} 条语料: {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
