#!/usr/bin/env python3
"""生成 ``auto_model_key_router.config_operations`` 的差分对拍语料。

参照实现是纯配置操作：输入一份配置 dict，就地改它，可能抛
``ConfigOperationError``（带 status_code）。这里逐条驱动真实的 Python 实现，
把「初始配置 + 调用参数 + 结果 + 调用后的配置」全部记进语料，Go 侧重放同一
调用并逐字节比对。

三条贯穿全文件的注意点：

1. **错误也要记改动**。参照实现的失败路径几乎都不是原子的：``create_provider``
   在 base_url 非法之前就已经 setdefault 了 providers，``update_provider`` 会先
   改掉 base_url 再因为 upstream_routes 抛错。只比对异常文本会漏掉这些差异，
   所以失败用例同样记录调用后的 data。
2. **status_code 只在 ConfigOperationError 上存在**。不是 ConfigOperationError
   的异常（migrate 的版本过高、normalize_upstream_base_url 的空 URL、
   ``for x in None``、``int("abc")``）在管理 API 里是 500，Go 侧必须同样不把它
   折叠成 400，故语料分别记录 error_type 与 status_code（其余为 null）。
3. **set 顺序不可复现**。``set_key_service_models`` 的 desired 是 Python set：
   一次新增多个模型时写入 models 的顺序取决于字符串哈希（进程间随机），因此
   语料只覆盖「新增 0 个或 1 个模型」；``key_service_models`` 之类的 set 返回值
   统一排序后记录。

用法::

    python -X utf8 scripts/gen_configops_corpus.py
    python -X utf8 scripts/gen_configops_corpus.py --check
"""

from __future__ import annotations

import argparse
import json
import sys
from copy import deepcopy
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router import config_operations as ops  # noqa: E402

DEFAULT_PATH = REPO_ROOT / "internal" / "configops" / "testdata" / "configops_corpus.json"
CORPUS_VERSION = 1


def dumps(obj: object) -> str:
    """保留字典插入顺序的紧凑形式，与 Go 的 ``canonical.DumpsOrdered`` 逐字节对齐。"""
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


def stable(obj: object) -> object:
    """把结果归一成可确定性序列化的形式：set → 升序列表，tuple → 列表。"""
    if isinstance(obj, set):
        return sorted(obj)
    if isinstance(obj, tuple):
        return [stable(item) for item in obj]
    if isinstance(obj, dict):
        return {key: stable(value) for key, value in obj.items()}
    if isinstance(obj, list):
        return [stable(item) for item in obj]
    return obj


CASES: list[dict] = []


def add(call, data: object, args: dict | None = None, *, name: str, note: str = "") -> None:
    """执行一次参照实现调用并记录一条语料。

    ``args`` 的键名与参照实现的参数名一致（Go 侧按名字取值），因此这里能把
    ``**args`` 直接当关键字参数展开——``update_settings`` 的 ``**updates`` 正好
    依赖这一点。
    """
    call_args = deepcopy(args or {})
    payload = deepcopy(data)
    entry: dict = {
        "name": name,
        "call": call.__name__,
        "data": dumps(data),
        "args": dumps(call_args),
    }
    if note:
        entry["note"] = note
    try:
        result = call(payload, **call_args)
        entry["outcome"] = {
            "ok": True,
            "result": stable(result),
            "data": dumps(payload),
        }
    except Exception as exc:  # noqa: BLE001 - 语料要记录任意异常
        entry["outcome"] = {
            "ok": False,
            "error_type": type(exc).__name__,
            "error": str(exc),
            "status_code": getattr(exc, "status_code", None),
            "data": dumps(payload),
        }
    CASES.append(entry)


# --------------------------------------------------------------------------- #
# 配置夹具
# --------------------------------------------------------------------------- #

def config_basic() -> dict:
    """一份结构完整、能通过 RouterConfig.from_dict 的 v4 配置。"""
    return {
        "config_version": 4,
        "local_api_key": "sk-local",
        "providers": {
            "openai": {
                "base_url": "https://api.openai.com",
                "keys": {
                    "k1": {"api_key": "s1", "enabled": True, "allow_visitor": True},
                    "k2": {"api_key": "s2"},
                },
            },
            "anthropic": {
                "base_url": "https://api.anthropic.com/",
                "keys": {"ak": {"api_key": "s3"}},
            },
        },
        "models": {
            "m1": {
                "aliases": ["m1a"],
                "routing_mode": "round_robin",
                "targets": [
                    {"provider": "openai", "key": "k1", "upstream_model": "gpt-4o"},
                    {"provider": "anthropic", "key": "ak", "upstream_model": "claude"},
                ],
            },
            "m2": {
                "targets": [{"provider": "openai", "key": "k2", "upstream_model": "gpt-4o-mini"}],
                "hidden_aliases": ["h2"],
            },
        },
        "unified_model": {"default": {"primary": {"model": "m1", "key": "k1"}}},
        "tasks": {"T1": {"model": "m1", "params": {"temperature": 0.3}}},
    }


def config_shared_key() -> dict:
    """两个模型共用同一个 provider key，用于验证克隆/拆分行为。"""
    return {
        "config_version": 4,
        "local_api_key": "sk-local",
        "providers": {
            "p": {
                "base_url": "https://a.example",
                "keys": {
                    "shared": {"api_key": "s1", "enabled": True},
                    "other": {"api_key": "s2"},
                },
            }
        },
        "models": {
            "ma": {"targets": [{"provider": "p", "key": "shared", "upstream_model": "ua"}]},
            "mb": {"targets": [{"provider": "p", "key": "shared", "upstream_model": "ub"}]},
        },
    }


def config_unified() -> dict:
    return {
        "config_version": 4,
        "local_api_key": "sk-local",
        "providers": {
            "p": {
                "base_url": "https://a.example",
                "keys": {
                    "k1": {"api_key": "1", "enabled": True},
                    "k2": {"api_key": "2", "enabled": True},
                },
            }
        },
        "models": {
            "m": {"targets": [
                {"provider": "p", "key": "k1", "upstream_model": "u1"},
                {"provider": "p", "key": "k2", "upstream_model": "u4"},
            ], "aliases": ["m-alias"]},
            "img": {"targets": [{"provider": "p", "key": "k2", "upstream_model": "u2"}]},
            "emb": {"targets": [{"provider": "p", "key": "k2", "upstream_model": "u3"}]},
        },
        "unified_model": {
            "default": {"primary": {"model": "m", "key": "k1"}},
            "image": {"primary": {"model": "img"}},
            "embeddings": {"primary": {"model": "emb"}},
        },
    }


def config_transfer() -> dict:
    return {
        "config_version": 4,
        "local_api_key": "sk-local",
        "providers": {
            "p": {"base_url": "https://a.example", "keys": {"k1": {"api_key": "s1"}}},
            "q": {"base_url": "https://b.example", "keys": {"only": {"api_key": "s9"}}},
        },
        "models": {
            "m": {"targets": [{"provider": "p", "key": "k1", "upstream_model": "u1"}],
                  "aliases": ["m-alias"]},
        },
        "tasks": {"T": {"model": "m"}},
    }


# --------------------------------------------------------------------------- #
# 1. 访问器与 require_*
# --------------------------------------------------------------------------- #

def accessor_cases() -> None:
    add(ops.providers, {"models": {}}, name="providers_missing_creates_object")
    add(ops.providers, {"providers": None}, name="providers_null_rejected")
    add(ops.providers, {"providers": []}, name="providers_list_rejected")
    add(ops.providers, {"providers": {"a": 1}}, name="providers_present_returned")
    add(ops.providers, 5, name="providers_data_not_object", note="Python 抛 AttributeError")
    add(ops.models, {}, name="models_missing_creates_object")
    add(ops.models, {"models": {"m": {}}}, name="models_present_returned")
    add(ops.models, {"models": None}, name="models_null_rejected")
    add(ops.models, {"models": "x"}, name="models_string_rejected")
    add(ops.provider_keys, {"base_url": "https://a.example"}, name="provider_keys_missing_creates")
    add(ops.provider_keys, {"keys": {"k": {"api_key": "1"}}}, name="provider_keys_present_returned")
    add(ops.provider_keys, {"keys": None}, name="provider_keys_null_rejected")
    add(ops.provider_keys, {"keys": 3}, name="provider_keys_int_rejected")
    add(ops.provider_keys, 7, name="provider_keys_not_object")
    add(ops.model_targets, {"aliases": []}, name="model_targets_missing_creates")
    add(ops.model_targets, {"targets": [{"provider": "p"}]}, name="model_targets_present_returned")
    add(ops.model_targets, {"targets": None}, name="model_targets_null_rejected")
    add(ops.model_targets, {"targets": {}}, name="model_targets_object_rejected")

    add(ops.require_provider, config_basic(), {"provider_id": "openai"}, name="require_provider_ok")
    add(ops.require_provider, config_basic(), {"provider_id": "ghost"}, name="require_provider_404")
    add(ops.require_provider, {"providers": {"p": "x"}}, {"provider_id": "p"},
        name="require_provider_not_object_404")
    add(ops.require_key, {"keys": {"k": {"api_key": "1"}}}, {"key_name": "k"}, name="require_key_ok")
    add(ops.require_key, {"keys": {}}, {"key_name": "k"}, name="require_key_404")
    add(ops.require_key, {"keys": {"k": None}}, {"key_name": "k"}, name="require_key_null_404")
    add(ops.require_model, config_basic(), {"model_id": "m1"}, name="require_model_ok")
    add(ops.require_model, config_basic(), {"model_id": "ghost"}, name="require_model_404")
    add(ops.require_model, {"models": {"m": "x"}}, {"model_id": "m"}, name="require_model_not_object_404")

    add(ops.fallback_model_id, config_basic(), name="fallback_model_id_first_with_targets")
    add(ops.fallback_model_id, {"models": {"a": {}, "b": {"targets": []}, "c": {"targets": [1]}}},
        name="fallback_model_id_skips_empty", note="model_targets 会顺带补 targets: []")
    add(ops.fallback_model_id, {"models": {}}, name="fallback_model_id_none")
    add(ops.fallback_model_id, {"models": {"z": {"targets": "x"}}},
        name="fallback_model_id_targets_not_array")


# --------------------------------------------------------------------------- #
# 2. base_url 规范化
# --------------------------------------------------------------------------- #

def base_url_cases() -> None:
    for label, value in [
        ("plain", "https://api.openai.com"),
        ("trailing_slash", "https://api.openai.com/"),
        ("many_slashes", "https://api.openai.com///"),
        ("uppercase_scheme", "HTTPS://API.OpenAI.COM"),
        ("with_path", "https://api.openai.com/v1"),
        ("with_port", "http://127.0.0.1:8000"),
        ("userinfo", "https://user:pw@h.example"),
        ("bad_port_text", "http://host:bad"),
        ("space_in_host", "https://exa mple.com"),
        ("ipv6", "http://[::1]"),
        ("empty", ""),
        ("blank", "   "),
        ("slash_only", "/"),
        ("no_scheme", "api.openai.com"),
        ("scheme_only", "https://"),
        ("ftp", "ftp://x.example"),
        ("leading_control", "\u0000https://x.example"),
        ("number", 5),
        ("bool_true", True),
        ("null", None),
        ("list", ["https://a.example"]),
    ]:
        add(ops.normalize_base_url, value, name=f"normalize_base_url_{label}")
    add(ops.normalize_base_url, "http://[::1", name="normalize_base_url_unbalanced_bracket")


# --------------------------------------------------------------------------- #
# 3. provider 族
# --------------------------------------------------------------------------- #

def provider_cases() -> None:
    add(ops.create_provider, {}, {"provider_id": "newp", "base_url": "https://new.example"},
        name="create_provider_ok")
    add(ops.create_provider, {"providers": {}}, {"provider_id": "  spaced  ", "base_url": "https://n.example/"},
        name="create_provider_trims_id_and_url")
    add(ops.create_provider, config_basic(), {"provider_id": "openai", "base_url": "https://x.example"},
        name="create_provider_duplicate_409")
    add(ops.create_provider, {}, {"provider_id": "  ", "base_url": "https://x.example"},
        name="create_provider_empty_id_422")
    add(ops.create_provider, {}, {"provider_id": "p", "base_url": "not-a-url"},
        name="create_provider_bad_url_still_creates_providers_key",
        note="失败路径已经 setdefault 了 providers")

    add(ops.update_provider, config_basic(), {"provider_id": "openai", "new_id": "openai2"},
        name="update_provider_rename_moves_to_end")
    add(ops.update_provider, config_basic(),
        {"provider_id": "openai", "base_url": "https://api.openai.com/v2"},
        name="update_provider_base_url")
    add(ops.update_provider, config_basic(), {"provider_id": "openai", "base_url": "https://api.openai.com"},
        name="update_provider_base_url_unchanged")
    add(ops.update_provider, config_basic(), {"provider_id": "anthropic", "base_url": "https://api.anthropic.com"},
        name="update_provider_url_strips_slash_and_matches")
    add(ops.update_provider, config_basic(), {"provider_id": "anthropic", "new_id": "openai"},
        name="update_provider_rename_conflict_409")
    add(ops.update_provider, config_basic(), {"provider_id": "openai", "new_id": "  "},
        name="update_provider_empty_new_id_422")
    add(ops.update_provider, config_basic(),
        {"provider_id": "openai", "routes": {"openai": "v1/chat/completions", "anthropic": None}},
        name="update_provider_routes_without_flag_ignored")
    add(ops.update_provider, config_basic(),
        {"provider_id": "openai", "update_routes": True, "routes": {"openai": "v1/chat/completions"}},
        name="update_provider_routes_set")
    add(ops.update_provider, config_basic(),
        {"provider_id": "openai", "update_routes": True, "routes": {"openai": None}},
        name="update_provider_routes_emptied_removes_key")
    add(ops.update_provider, config_basic(),
        {"provider_id": "openai", "update_routes": True, "routes": []},
        name="update_provider_routes_not_object_422")
    add(ops.update_provider, config_basic(),
        {"provider_id": "openai", "update_routes": True, "routes": {"bogus": "x"}},
        name="update_provider_routes_bad_mode_422")
    add(ops.update_provider, {"providers": {"p": {"keys": {}}}},
        {"provider_id": "p", "base_url": "https://x.example"},
        name="update_provider_missing_old_base_url_valueerror",
        note="normalize_upstream_base_url 抛裸 ValueError，不是 ConfigOperationError")
    routes_config = config_basic()
    routes_config["upstream_routes"] = {
        "https://api.openai.com": {"openai": "v1/chat/completions"},
        "https://other.example": {"openai": "v1/chat/completions"},
    }
    add(ops.update_provider, routes_config,
        {"provider_id": "openai", "base_url": "https://api.openai.com/v2"},
        name="update_provider_moves_upstream_routes_entry")
    add(ops.update_provider, routes_config,
        {"provider_id": "openai", "base_url": "https://other.example"},
        name="update_provider_drops_colliding_routes_entry")

    add(ops.create_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k3", "api_key": "s4"},
        name="create_provider_key_ok")
    add(ops.create_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k3", "api_key": "s4", "enabled": False,
         "allow_visitor": True},
        name="create_provider_key_flags")
    add(ops.create_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "api_key": "s4"},
        name="create_provider_key_duplicate_409")
    add(ops.create_provider_key, config_basic(),
        {"provider_id": "ghost", "key_name": "k3", "api_key": "s4"},
        name="create_provider_key_provider_404")
    add(ops.create_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": " ", "api_key": "s4"},
        name="create_provider_key_empty_name_422")
    add(ops.create_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k3", "api_key": "  "},
        name="create_provider_key_empty_secret_422")

    add(ops.update_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "new_name": "k1r"},
        name="update_provider_key_rename")
    add(ops.update_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "api_key": "brand-new"},
        name="update_provider_key_secret")
    add(ops.update_provider_key, {"providers": {"p": {"keys": {"k": {"api_key": "s", "capabilities": {"a": 1}}}}}},
        {"provider_id": "p", "key_name": "k", "api_key": "s2"},
        name="update_provider_key_secret_drops_capabilities")
    add(ops.update_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "enabled": False},
        name="update_provider_key_disable")
    add(ops.update_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "allow_visitor": False},
        name="update_provider_key_allow_visitor")
    add(ops.update_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "new_name": "k2"},
        name="update_provider_key_rename_conflict_409")
    add(ops.update_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "ghost", "new_name": "x"},
        name="update_provider_key_missing_404")
    add(ops.update_provider_key, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "api_key": " "},
        name="update_provider_key_empty_secret_422")
    add(ops.update_provider_key, config_shared_key(),
        {"provider_id": "p", "key_name": "shared", "new_name": "renamed"},
        name="update_provider_key_rename_rewrites_targets")

    add(ops.delete_provider_key, config_basic(), {"provider_id": "openai", "key_name": "k2"},
        name="delete_provider_key_removes_model")
    add(ops.delete_provider_key, config_basic(), {"provider_id": "openai", "key_name": "k1"},
        name="delete_provider_key_keeps_model")
    add(ops.delete_provider_key, {"providers": {"p": {"base_url": "https://a.example", "keys": {"only": {"api_key": "1"}}}}},
        {"provider_id": "p", "key_name": "only"},
        name="delete_provider_key_drops_empty_provider")
    add(ops.delete_provider_key, config_basic(), {"provider_id": "openai", "key_name": "ghost"},
        name="delete_provider_key_missing_404")
    add(ops.delete_provider_key, config_basic(), {"provider_id": "ghost", "key_name": "k1"},
        name="delete_provider_key_provider_404")

    add(ops.delete_provider, config_basic(), {"provider_id": "anthropic"},
        name="delete_provider_removes_dependent_target")
    add(ops.delete_provider, config_basic(), {"provider_id": "openai"},
        name="delete_provider_removes_models")
    add(ops.delete_provider, config_basic(), {"provider_id": "ghost"},
        name="delete_provider_404")

    add(ops.provider_id_for_base_url, config_basic(), {"base_url": "https://api.openai.com"},
        name="provider_id_for_base_url_existing")
    add(ops.provider_id_for_base_url, config_basic(), {"base_url": "https://api.openai.com/"},
        name="provider_id_for_base_url_existing_slash")
    add(ops.provider_id_for_base_url, {"providers": {}}, {"base_url": "https://new.example/v1"},
        name="provider_id_for_base_url_creates")
    add(ops.provider_id_for_base_url, {"providers": {"new.example-v1": {"base_url": "https://x"}}},
        {"base_url": "https://new.example/v1"},
        name="provider_id_for_base_url_suffix")
    add(ops.provider_id_for_base_url, {}, {"base_url": "https://a:1/v1"},
        name="provider_id_for_base_url_replaces_colon")
    add(ops.provider_id_for_base_url, {}, {"base_url": "not-a-url"},
        name="provider_id_for_base_url_invalid_422")

    add(ops.set_upstream_routes_for_base_url, {}, {"base_url": "https://a.example",
                                                   "routes": {"openai": "v1/chat/completions"}},
        name="set_upstream_routes_ok")
    add(ops.set_upstream_routes_for_base_url,
        {"upstream_routes": {"https://a.example": {"openai": "old"}}},
        {"base_url": "https://a.example", "routes": {"openai": "v1/chat/completions"}},
        name="set_upstream_routes_replaces")
    add(ops.set_upstream_routes_for_base_url,
        {"upstream_routes": {"https://a.example": {"openai": "old"}}},
        {"base_url": "https://a.example", "routes": {}},
        name="set_upstream_routes_empty_drops_key")
    add(ops.set_upstream_routes_for_base_url,
        {"upstream_routes": "junk"},
        {"base_url": "https://a.example", "routes": {"openai": "v1/chat/completions"}},
        name="set_upstream_routes_replaces_non_object")
    add(ops.set_upstream_routes_for_base_url, {}, {"base_url": "  ", "routes": {"openai": "x"}},
        name="set_upstream_routes_blank_url_422")
    add(ops.set_upstream_routes_for_base_url, {}, {"base_url": "https://a.example", "routes": 5},
        name="set_upstream_routes_not_object_422")
    add(ops.set_upstream_routes_for_base_url, {}, {"base_url": "https://a.example", "routes": None},
        name="set_upstream_routes_none_is_empty")


# --------------------------------------------------------------------------- #
# 4. model 族与 target 族
# --------------------------------------------------------------------------- #

def model_cases() -> None:
    add(ops.validate_targets, config_basic(), {"targets": []}, name="validate_targets_empty_422")
    add(ops.validate_targets, config_basic(),
        {"targets": [{"provider": "openai", "key": "k1", "upstream_model": "u"}]},
        name="validate_targets_ok")
    add(ops.validate_targets, config_basic(),
        {"targets": [{"provider": "openai", "key": "k1", "upstream_model": " "}]},
        name="validate_targets_blank_upstream_422")
    add(ops.validate_targets, config_basic(),
        {"targets": [{"provider": "ghost", "key": "k1", "upstream_model": "u"}]},
        name="validate_targets_provider_404")
    add(ops.validate_targets, config_basic(),
        {"targets": [{"provider": "openai", "key": "ghost", "upstream_model": "u"}]},
        name="validate_targets_key_404")
    add(ops.validate_targets, config_basic(), {"targets": ["x"]},
        name="validate_targets_not_object_422")
    add(ops.validate_targets, config_basic(), {"targets": [{"provider": 5, "key": "k1", "upstream_model": "u"}]},
        name="validate_targets_number_provider")

    add(ops.add_model_target, config_basic(),
        {"model_id": "m1", "target": {"provider": "openai", "key": "k2", "upstream_model": "gpt-5"}},
        name="add_model_target_ok")
    add(ops.add_model_target, config_basic(),
        {"model_id": "m1", "target": {"provider": "openai", "key": "k1", "upstream_model": "gpt-4o"}},
        name="add_model_target_duplicate_409")
    add(ops.add_model_target, config_basic(),
        {"model_id": "ghost", "target": {"provider": "openai", "key": "k1", "upstream_model": "u"}},
        name="add_model_target_model_404")
    add(ops.add_model_target, config_basic(),
        {"model_id": "m1", "target": {"provider": "openai", "key": "k1", "upstream_model": "u", "extra": 1}},
        name="add_model_target_copies_extra_fields")

    add(ops.update_model_target, config_basic(), {"model_id": "m1", "target_index": 0, "upstream_model": "gpt-4o-2024"},
        name="update_model_target_ok")
    add(ops.update_model_target, config_basic(), {"model_id": "m1", "target_index": 9, "upstream_model": "u"},
        name="update_model_target_index_404")
    add(ops.update_model_target, config_basic(), {"model_id": "m1", "target_index": -1, "upstream_model": "u"},
        name="update_model_target_negative_index_404")
    add(ops.update_model_target, config_basic(), {"model_id": "m1", "target_index": 0, "upstream_model": " "},
        name="update_model_target_blank_422")
    add(ops.update_model_target, {"models": {"m": {"targets": ["junk"]}}},
        {"model_id": "m", "target_index": 0, "upstream_model": "u"},
        name="update_model_target_not_object")
    add(ops.update_model_target, {"models": {"m": {"targets": ["junk"]}}},
        {"model_id": "m", "target_index": 0, "upstream_model": " "},
        name="update_model_target_blank_beats_type_error",
        note="Python 先算 RHS，因此空 upstream_model 的 422 排在 TypeError 之前")

    add(ops.delete_model_target, config_basic(), {"model_id": "m1", "target_index": 0},
        name="delete_model_target_ok")
    add(ops.delete_model_target, config_basic(), {"model_id": "m2", "target_index": 0},
        name="delete_model_target_last_removes_model")
    add(ops.delete_model_target, config_basic(), {"model_id": "m1", "target_index": 5},
        name="delete_model_target_index_404")

    add(ops.create_model, config_basic(), {"model_id": "m3"}, name="create_model_minimal")
    add(ops.create_model, {"models": {"m": {"aliases": None, "targets": []}}}, {"model_id": "n"},
        name="create_model_other_model_aliases_null_typeerror",
        note="for alias in None 是 TypeError，不是空迭代")
    add(ops.create_model, {"models": {"m": {"aliases": "abc", "targets": []}}}, {"model_id": "a"},
        name="create_model_other_model_aliases_string_iterated_per_char")
    add(ops.update_model, {"models": {"m": {"aliases": None, "targets": []}}},
        {"model_id": "m", "aliases": ["x"]},
        name="update_model_own_aliases_null_skipped_by_exclude")
    add(ops.update_model, {"models": {"m": {"hidden_aliases": None, "targets": []}}},
        {"model_id": "m", "aliases": ["x"]},
        name="update_model_own_hidden_aliases_null_typeerror")
    add(ops.update_model, {"models": {"m": {"targets": []}, "n": {"targets": [], "aliases": "ab"}}},
        {"model_id": "m", "new_id": "zz"},
        name="update_model_rename_scans_string_aliases_per_char")
    add(ops.create_model, config_basic(),
        {"model_id": "m3", "aliases": ["a", " b "], "hidden_aliases": ["h"],
         "routing_mode": "priority", "reasoning_effort": "high"},
        name="create_model_full")
    add(ops.create_model, config_basic(),
        {"model_id": "m3", "reasoning_effort": "default"},
        name="create_model_reasoning_default_skipped")
    add(ops.create_model, config_basic(), {"model_id": "m3", "routing_mode": "bogus"},
        name="create_model_bad_routing_mode_422")
    add(ops.create_model, config_basic(), {"model_id": "m1"}, name="create_model_duplicate_409")
    add(ops.create_model, config_basic(), {"model_id": "  "}, name="create_model_empty_id_422")
    add(ops.create_model, config_basic(), {"model_id": "m3", "aliases": ["x", "x"]},
        name="create_model_duplicate_alias_422")
    add(ops.create_model, config_basic(), {"model_id": "m3", "hidden_aliases": ["h", "h"]},
        name="create_model_duplicate_hidden_422")
    add(ops.create_model, config_basic(), {"model_id": "m3", "aliases": ["m2"]},
        name="create_model_alias_conflict_409")
    add(ops.create_model, config_basic(), {"model_id": "m3", "aliases": ["h2"]},
        name="create_model_alias_conflicts_hidden_409")
    add(ops.create_model, config_basic(), {"model_id": "m3", "hidden_aliases": ["m1a"]},
        name="create_model_hidden_conflicts_alias_409")
    add(ops.create_model, config_basic(), {"model_id": "m3", "hidden_aliases": ["h2"]},
        name="create_model_hidden_conflicts_hidden_409")
    add(ops.create_model, config_basic(),
        {"model_id": "m3", "targets": [{"provider": "openai", "key": "k1", "upstream_model": "u"}]},
        name="create_model_with_targets")
    add(ops.create_model, config_basic(), {"model_id": "m3", "targets": []},
        name="create_model_empty_targets_422")
    add(ops.create_model, config_basic(),
        {"model_id": "m3", "targets": [{"provider": "ghost", "key": "k", "upstream_model": "u"}]},
        name="create_model_bad_target_404")

    add(ops.update_model, config_basic(), {"model_id": "m1", "new_id": "renamed"},
        name="update_model_rename")
    add(ops.update_model, config_basic(), {"model_id": "m1", "new_id": "m2"},
        name="update_model_rename_conflict_409")
    add(ops.update_model, config_basic(), {"model_id": "m1", "new_id": "m1a"},
        name="update_model_rename_hits_own_alias_409")
    add(ops.update_model, config_basic(), {"model_id": "m2", "new_id": "m1a"},
        name="update_model_rename_to_other_alias_409")
    add(ops.update_model, config_basic(), {"model_id": "m1", "aliases": ["new-a", "new-b"]},
        name="update_model_set_aliases")
    add(ops.update_model, config_basic(), {"model_id": "m1", "aliases": []},
        name="update_model_clear_aliases")
    add(ops.update_model, config_basic(), {"model_id": "m1", "aliases": ["h2"]},
        name="update_model_alias_conflicts_hidden_409")
    add(ops.update_model, config_basic(), {"model_id": "m2", "aliases": ["h2"]},
        name="update_model_alias_conflicts_own_hidden_409")
    add(ops.update_model, config_basic(), {"model_id": "m2", "hidden_aliases": []},
        name="update_model_clear_hidden")
    add(ops.update_model, config_basic(), {"model_id": "m1", "hidden_aliases": ["h1"]},
        name="update_model_set_hidden")
    add(ops.update_model, config_basic(), {"model_id": "m1", "hidden_aliases": ["m2"]},
        name="update_model_hidden_conflicts_model_409")
    add(ops.update_model, config_basic(), {"model_id": "m1", "routing_mode": "only_first"},
        name="update_model_routing_mode")
    add(ops.update_model, config_basic(), {"model_id": "m1", "routing_mode": "bogus"},
        name="update_model_bad_routing_422")
    add(ops.update_model, config_basic(),
        {"model_id": "m1", "update_reasoning_effort": True, "reasoning_effort": "high"},
        name="update_model_set_reasoning")
    add(ops.update_model, config_basic(),
        {"model_id": "m1", "update_reasoning_effort": True, "reasoning_effort": "downstream"},
        name="update_model_clear_reasoning")
    add(ops.update_model, config_basic(), {"model_id": "m1", "reasoning_effort": "high"},
        name="update_model_reasoning_without_flag_ignored")
    add(ops.update_model, config_basic(),
        {"model_id": "m1", "targets": [{"provider": "openai", "key": "k2", "upstream_model": "u"}]},
        name="update_model_replace_targets")
    add(ops.update_model, config_basic(), {"model_id": "m1", "targets": []},
        name="update_model_empty_targets_422")
    add(ops.update_model, config_basic(), {"model_id": "ghost", "aliases": ["a"]},
        name="update_model_missing_404")

    add(ops.delete_model, config_basic(), {"model_id": "m1"}, name="delete_model_ok")
    add(ops.delete_model, config_basic(), {"model_id": "m2"}, name="delete_model_hidden_alias")
    add(ops.delete_model, config_basic(), {"model_id": "ghost"}, name="delete_model_404")


# --------------------------------------------------------------------------- #
# 5. model key 族（含克隆拆分）
# --------------------------------------------------------------------------- #

def model_key_cases() -> None:
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5"},
        name="create_model_key_existing_provider")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5", "base_url": "https://api.openai.com/"},
        name="create_model_key_normalizes_url")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5", "base_url": "https://brand.example",
         "upstream_model": "custom"},
        name="create_model_key_new_provider")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5", "enabled": False,
         "allow_visitor": True},
        name="create_model_key_flags")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5",
         "update_upstream_routes": True, "upstream_routes": {"openai": "v1/chat/completions"}},
        name="create_model_key_with_routes")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5", "update_upstream_routes": True},
        name="create_model_key_clears_routes")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m1", "key_name": "k1", "api_key": "s5"},
        name="create_model_key_duplicate_model_key_409")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k2", "api_key": "s5"},
        name="create_model_key_duplicate_provider_key_409")
    add(ops.create_model_key, config_basic(),
        {"model_id": "ghost", "key_name": "k3", "api_key": "s5"},
        name="create_model_key_model_404")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": " ", "api_key": "s5"},
        name="create_model_key_empty_name_422")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5", "base_url": "nope"},
        name="create_model_key_bad_url_422")
    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5", "upstream_model": " "},
        name="create_model_key_blank_upstream_422")
    add(ops.create_model_key, {"config_version": 4, "default_base_url": "https://def.example"},
        {"model_id": "m", "key_name": "k", "api_key": "s"},
        name="create_model_key_default_base_url_model_missing_404",
        note="model 不存在会先 404；这条固定 default_base_url 的回落逻辑")

    add(ops.create_model_key, config_basic(),
        {"model_id": "m2", "key_name": "k3", "api_key": "s5", "base_url": "https://oops.example"},
        name="create_model_key_existing_provider_url")

    add(ops.create_model_with_keys, config_basic(), {"model_id": "m3"},
        name="create_model_with_keys_no_keys")
    add(ops.create_model_with_keys, config_basic(),
        {"model_id": "m3", "aliases": ["a3"], "keys": [
            {"name": "k9", "api_key": "s9"},
            {"name": "k10", "api_key": "s10", "base_url": "https://new.example", "enabled": False,
             "allow_visitor": True, "upstream_model": "up9"},
        ]},
        name="create_model_with_keys_two_keys")
    add(ops.create_model_with_keys, config_basic(),
        {"model_id": "m3", "keys": [{"api_key": "s9"}]},
        name="create_model_with_keys_missing_name_422")
    add(ops.create_model_with_keys, config_basic(),
        {"model_id": "m3", "keys": [{"name": "k9"}]},
        name="create_model_with_keys_missing_secret_422")
    add(ops.create_model_with_keys, config_basic(),
        {"model_id": "m1", "keys": []},
        name="create_model_with_keys_duplicate_model_409")

    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "new_name": "k2r"},
        name="update_model_key_rename_same_provider")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "api_key": "rotated"},
        name="update_model_key_secret")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "enabled": False},
        name="update_model_key_disable")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "allow_visitor": True},
        name="update_model_key_allow_visitor")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "base_url": "https://moved.example"},
        name="update_model_key_moves_base_url_clones_provider")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "update_base_url": True},
        name="update_model_key_update_base_url_without_value")
    add(ops.update_model_key_local, {"config_version": 4, "default_base_url": "https://def.example",
                                     "providers": {"p": {"base_url": "https://a.example",
                                                         "keys": {"k": {"api_key": "1"}}}},
                                     "models": {"m": {"targets": [{"provider": "p", "key": "k",
                                                                   "upstream_model": "u"}]}}},
        {"model_id": "m", "key_name": "k", "update_base_url": True},
        name="update_model_key_uses_default_base_url")
    add(ops.update_model_key_local, config_shared_key(),
        {"model_id": "ma", "key_name": "shared", "api_key": "rotated"},
        name="update_model_key_shared_key_clones_provider")
    add(ops.update_model_key_local, config_shared_key(),
        {"model_id": "ma", "key_name": "shared", "enabled": False},
        name="update_model_key_shared_key_disable")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m1", "key_name": "openai-k1", "api_key": "via-qualified-name"},
        name="update_model_key_qualified_name")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m1", "key_name": "openai-k1"},
        name="update_model_key_qualified_name_renames",
        note="new_name 为 None 时用调用方传入的名字，于是 key 真的被改名")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "new_name": "other"},
        name="update_model_key_rename_conflict_409")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "ghost", "new_name": "x"},
        name="update_model_key_missing_404")
    add(ops.update_model_key_local, config_basic(),
        {"model_id": "m2", "key_name": "k2", "api_key": " "},
        name="update_model_key_empty_secret_422")
    add(ops.update_model_key_local, {"models": {"m": {"targets": [{"provider": "ghost", "key": "k"}]}}},
        {"model_id": "m", "key_name": "k", "new_name": "x"},
        name="update_model_key_locates_provider_first_404")

    add(ops.delete_model_key_local, config_basic(), {"model_id": "m2", "key_name": "k2"},
        name="delete_model_key_last_target_removes_model_and_key")
    add(ops.delete_model_key_local, config_basic(), {"model_id": "m1", "key_name": "k1"},
        name="delete_model_key_keeps_other_target")
    add(ops.delete_model_key_local, config_shared_key(), {"model_id": "ma", "key_name": "shared"},
        name="delete_model_key_shared_key_kept")
    add(ops.delete_model_key_local, config_basic(), {"model_id": "m1", "key_name": "ghost"},
        name="delete_model_key_missing_404")
    add(ops.delete_model_key, config_basic(), {"model_id": "m2", "key_name": "k2"},
        name="delete_model_key_alias_call")

    add(ops.key_service_models, config_basic(), {"provider_id": "openai", "key_name": "k1"},
        name="key_service_models_multiple")
    add(ops.key_service_models, config_basic(), {"provider_id": "openai", "key_name": "k2"},
        name="key_service_models_one")
    add(ops.key_service_models, config_basic(), {"provider_id": "openai", "key_name": "ghost"},
        name="key_service_models_missing_key_404")

    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "k2", "model_ids": ["m1", "m2"]},
        name="set_key_service_models_add_existing")
    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "k2", "model_ids": ["m1", "m2", "m3"]},
        name="set_key_service_models_adds_one_model",
        note="一次只新增 1 个模型，避免 Python set 顺序影响 models 键序")
    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "model_ids": []},
        name="set_key_service_models_detach_all")
    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "model_ids": None},
        name="set_key_service_models_none_detaches")
    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "k1", "model_ids": ["m2"]},
        name="set_key_service_models_moves_between_models")
    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "k2", "model_ids": ["m2"]},
        name="set_key_service_models_noop")
    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "k2", "model_ids": [" "]},
        name="set_key_service_models_blank_422")
    add(ops.set_key_service_models, config_basic(),
        {"provider_id": "openai", "key_name": "ghost", "model_ids": []},
        name="set_key_service_models_missing_key_404")


# --------------------------------------------------------------------------- #
# 6. unified / 设置 / 本地 key
# --------------------------------------------------------------------------- #

def unified_cases() -> None:
    add(ops.replace_unified_model_name, config_basic(), {"old_name": "m1", "new_name": "m9"},
        name="replace_unified_model_name_hit")
    add(ops.replace_unified_model_name, config_unified(), {"old_name": "m", "new_name": "m9"},
        name="replace_unified_model_name_multiple")
    add(ops.replace_unified_model_name, {"models": {}}, {"old_name": "m", "new_name": "m9"},
        name="replace_unified_model_name_absent")
    add(ops.replace_unified_model_name, {"unified_model": "junk"}, {"old_name": "m", "new_name": "m9"},
        name="replace_unified_model_name_not_object")
    add(ops.replace_unified_model_name,
        {"config_version": 3, "unified_model": {"model": "m", "key": "k"}},
        {"old_name": "m", "new_name": "m9"},
        name="replace_unified_model_name_migrates_legacy")

    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "m2"}}}},
        name="set_unified_model_simple")
    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "m1a", "key": "k1"},
                                 "fallback": {"model": "m2", "key": "k2"}},
                     "image": {"primary": {"model": "m2"}}}},
        name="set_unified_model_resolves_alias_and_drops_unknown_keys")
    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "m1", "key": ""}}}},
        name="set_unified_model_blank_key_dropped")
    add(ops.set_unified_model, config_basic(), {"unified": None},
        name="set_unified_model_none_removes")
    add(ops.set_unified_model, config_basic(), {"unified": "junk"},
        name="set_unified_model_not_object_422")
    add(ops.set_unified_model, config_basic(), {"unified": {}},
        name="set_unified_model_empty_422")
    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "ghost"}}}},
        name="set_unified_model_unknown_model_422")
    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "m1", "key": "ghost"}}}},
        name="set_unified_model_unknown_key_422")
    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "m1", "key": "k1"},
                                 "fallback": {"model": "m1", "key": "k2"}}}},
        name="set_unified_model_same_model_422")
    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "m1"}}, "bogus": {"primary": {"model": "m2"}}}},
        name="set_unified_model_drops_unknown_plan")
    add(ops.set_unified_model, config_basic(),
        {"unified": {"default": {"primary": {"model": "m1"}}, "image": None}},
        name="set_unified_model_null_plan")

    add(ops.switch_unified_target, config_unified(), {"target": "default.fallback", "model_name": "img"},
        name="switch_unified_target_set_model")
    add(ops.switch_unified_target, config_unified(),
        {"target": "default.primary", "model_name": "m-alias"},
        name="switch_unified_target_resolves_alias")
    add(ops.switch_unified_target, config_unified(), {"target": "default.primary"},
        name="switch_unified_target_keeps_current")
    add(ops.switch_unified_target, config_unified(),
        {"target": "default.primary", "model_name": "img"},
        name="switch_unified_target_clears_key_on_model_change")
    add(ops.switch_unified_target, config_unified(),
        {"target": "default.primary", "update_key": True, "key_name": "k2"},
        name="switch_unified_target_update_key")
    add(ops.switch_unified_target, config_unified(),
        {"target": "default.primary", "update_key": True},
        name="switch_unified_target_update_key_none_clears")
    add(ops.switch_unified_target, config_unified(),
        {"target": "image.fallback", "model_name": "m"},
        name="switch_unified_target_image_fallback")
    add(ops.switch_unified_target, config_unified(),
        {"target": "embeddings.fallback", "model_name": "m"},
        name="switch_unified_target_embeddings_fallback")
    add(ops.switch_unified_target, {"config_version": 4}, {"target": "default.primary", "model_name": "m"},
        name="switch_unified_target_no_models_404")
    add(ops.switch_unified_target, {"config_version": 4, "models": {}},
        {"target": "default.primary"},
        name="switch_unified_target_unconfigured_422")
    add(ops.switch_unified_target, config_unified(), {"target": "default.bogus", "model_name": "m"},
        name="switch_unified_target_invalid_target_422")
    add(ops.switch_unified_target, config_unified(), {"target": "default.primary", "model_name": "ghost"},
        name="switch_unified_target_unknown_model_404")
    add(ops.switch_unified_target, config_unified(),
        {"target": "default.primary", "update_key": True, "key_name": "ghost"},
        name="switch_unified_target_unknown_key_404")
    add(ops.switch_unified_target, config_unified(),
        {"target": "image.primary", "update_key": True, "key_name": "k2"},
        name="switch_unified_target_validate_key_enabled")
    add(ops.switch_unified_target, config_unified(), {"target": "default.primary", "update_key": True,
                                                      "key_name": "   "},
        name="switch_unified_target_blank_key_treated_as_none")
    add(ops.switch_unified_target, {"config_version": 4, "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "1"}}}},
                                    "models": {"m": {"targets": [{"provider": "p", "key": "k", "upstream_model": "u"}]}}},
        {"target": "image.primary", "model_name": "m"},
        name="switch_unified_target_image_without_default_422")

    add(ops.repair_unified_model, config_basic(), name="repair_unified_model_intact")
    add(ops.repair_unified_model,
        {"config_version": 4, "providers": {}, "models": {"m": {"targets": [
            {"provider": "p", "key": "k", "upstream_model": "u"}]},
            "other": {"targets": [{"provider": "p", "key": "k", "upstream_model": "u"}]}},
         "unified_model": {"default": {"primary": {"model": "missing", "key": "k"}}}},
        name="repair_unified_model_replaces_missing_primary",
        note="providers/keys 不存在时解析失败，函数会原样返回")
    add(ops.repair_unified_model,
        {"config_version": 4, "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "1"}}}},
         "models": {"m": {"targets": [{"provider": "p", "key": "k", "upstream_model": "u"}]},
                    "n": {"targets": [{"provider": "p", "key": "k", "upstream_model": "v"}]}},
         "unified_model": {"default": {"primary": {"model": "gone", "key": "k"}},
                           "image": {"primary": {"model": "m"}},
                           "embeddings": {"fallback": {"model": "n"}}}},
        name="repair_unified_model_full")
    add(ops.repair_unified_model,
        {"config_version": 4, "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "1", "enabled": False}}}},
         "models": {"m": {"targets": [{"provider": "p", "key": "k", "upstream_model": "u"}]}},
         "unified_model": {"default": {"primary": {"model": "m", "key": "k"}}}},
        name="repair_unified_model_disabled_key_cleared")
    add(ops.repair_unified_model,
        {"config_version": 4, "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "1"}}}},
         "models": {"m": {"targets": [{"provider": "p", "key": "k", "upstream_model": "u"}]}},
         "unified_model": {"default": {"primary": {"model": "m"}, "fallback": {"model": "m"}}}},
        name="repair_unified_model_drops_duplicate_fallback")
    add(ops.repair_unified_model,
        {"config_version": 4, "models": {}, "unified_model": {"default": {"primary": {"model": "m"}}}},
        name="repair_unified_model_removes_when_no_models")
    add(ops.repair_unified_model, {"config_version": 4, "unified_model": "junk"},
        name="repair_unified_model_drops_non_object")

    add(ops.repair_model_references, config_basic(), name="repair_model_references_intact")
    add(ops.repair_model_references,
        {"config_version": 4, "models": {}, "tasks": {"T": {"model": "gone"}},
         "unified_model": {"default": {"primary": {"model": "gone"}}}},
        name="repair_model_references_clears_tasks_first")
    add(ops.repair_model_references,
        {"config_version": 4, "providers": {"p": {"base_url": "https://a.example",
                                                  "keys": {"k": {"api_key": "1"}}}},
         "models": {"m": {"targets": [{"provider": "p", "key": "k", "upstream_model": "u"}]}},
         "unified_model": {"default": {"primary": {"model": "m", "key": "k"}}},
         "tasks": {"T": {"model": "m"}}},
        name="repair_model_references_keeps_valid_config")

    add(ops.regenerate_local_api_key, config_basic(), {"api_key": "sk-new"},
        name="regenerate_local_api_key_ok")
    add(ops.regenerate_local_api_key, config_basic(), {"api_key": "  sk-new  "},
        name="regenerate_local_api_key_trims")
    add(ops.regenerate_local_api_key, config_basic(), {"api_key": " "},
        name="regenerate_local_api_key_blank_422")

    add(ops.update_settings, config_basic(), {"host": "0.0.0.0", "port": 9000},
        name="update_settings_host_port")
    add(ops.update_settings, config_basic(), {"host": "  127.0.0.1  "},
        name="update_settings_host_trimmed")
    add(ops.update_settings, config_basic(), {"host": "http://x"},
        name="update_settings_host_with_scheme_422")
    add(ops.update_settings, config_basic(), {"host": "a/b"},
        name="update_settings_host_with_slash_422")
    add(ops.update_settings, config_basic(), {"host": ""},
        name="update_settings_host_blank_422")
    add(ops.update_settings, config_basic(), {"port": 0},
        name="update_settings_port_zero_422")
    add(ops.update_settings, config_basic(), {"port": 65536},
        name="update_settings_port_high_422")
    add(ops.update_settings, config_basic(), {"port": "8080"},
        name="update_settings_port_string")
    add(ops.update_settings, config_basic(), {"port": "abc"},
        name="update_settings_port_not_number")
    add(ops.update_settings, config_basic(), {"request_timeout": 0},
        name="update_settings_timeout_zero_422")
    add(ops.update_settings, config_basic(), {"request_timeout": -1, "max_retries": -1},
        name="update_settings_timeout_negative_first_422")
    add(ops.update_settings, config_basic(), {"max_retries": -1},
        name="update_settings_max_retries_negative_422")
    add(ops.update_settings, config_basic(), {"max_retries": 0},
        name="update_settings_max_retries_zero_ok")
    add(ops.update_settings, config_basic(), {"stream_idle_timeout": 12.5, "webui_enabled": False},
        name="update_settings_arbitrary_keys")
    add(ops.update_settings, config_basic(), {"host": None, "port": None},
        name="update_settings_nulls_written_through")
    add(ops.update_settings, config_basic(), {},
        name="update_settings_no_updates")


# --------------------------------------------------------------------------- #
# 7. 任务族
# --------------------------------------------------------------------------- #

def task_cases() -> None:
    add(ops.existing_tasks, config_basic(), name="existing_tasks_present")
    add(ops.existing_tasks, {"tasks": []}, name="existing_tasks_not_object")
    add(ops.existing_tasks, {}, name="existing_tasks_missing")
    add(ops.require_task, config_basic(), {"task_name": "T1"}, name="require_task_ok")
    add(ops.require_task, config_basic(), {"task_name": "ghost"}, name="require_task_404")
    add(ops.require_task, {"tasks": {"T": "junk"}}, {"task_name": "T"}, name="require_task_not_object_404")

    add(ops.create_task, config_basic(), {"task_name": "T2", "model": "m1"}, name="create_task_ok")
    add(ops.create_task, config_basic(), {"task_name": "T2", "model": "m1a"},
        name="create_task_resolves_alias")
    add(ops.create_task, config_basic(),
        {"task_name": "T2", "model": "m1", "fallback_model": "m2"},
        name="create_task_with_fallback")
    add(ops.create_task, config_basic(),
        {"task_name": "T2", "model": "m1", "fallback_model": "m1a"},
        name="create_task_fallback_same_model_omitted")
    add(ops.create_task, config_basic(),
        {"task_name": "T2", "model": "m1", "params": {"temperature": 0.5, "stop": ["x"]}},
        name="create_task_with_params")
    add(ops.create_task, config_basic(),
        {"task_name": "T2", "model": "m1", "params": {}},
        name="create_task_empty_params_omitted")
    add(ops.create_task, config_basic(), {"task_name": "T1", "model": "m1"},
        name="create_task_duplicate_409")
    add(ops.create_task, config_basic(), {"task_name": " ", "model": "m1"},
        name="create_task_blank_name_422")
    add(ops.create_task, config_basic(), {"task_name": "T2", "model": "ghost"},
        name="create_task_unknown_model_404")
    add(ops.create_task, config_basic(), {"task_name": "T2", "model": "m1", "fallback_model": "ghost"},
        name="create_task_unknown_fallback_404")
    add(ops.create_task, config_basic(), {"task_name": "m1", "model": "m1"},
        name="create_task_name_conflicts_model_422")
    add(ops.create_task, config_basic(),
        {"task_name": "T2", "model": "m1", "params": {"bogus": 1}},
        name="create_task_bad_params_422")

    add(ops.update_task, config_basic(), {"task_name": "T1", "model": "m2"},
        name="update_task_model")
    add(ops.update_task, config_basic(),
        {"task_name": "T1", "update_fallback": True, "fallback_model": "m2"},
        name="update_task_set_fallback")
    add(ops.update_task, config_basic(), {"task_name": "T1", "update_fallback": True},
        name="update_task_clear_fallback")
    add(ops.update_task, config_basic(),
        {"task_name": "T1", "update_params": True, "params": {"temperature": 0.9}},
        name="update_task_set_params")
    add(ops.update_task, config_basic(), {"task_name": "T1", "update_params": True},
        name="update_task_clear_params")
    add(ops.update_task, config_basic(), {"task_name": "T1", "params": {"temperature": 0.9}},
        name="update_task_params_without_flag_ignored")
    add(ops.update_task, config_basic(), {"task_name": "T1", "model": "ghost"},
        name="update_task_unknown_model_404")
    add(ops.update_task, config_basic(), {"task_name": "ghost", "model": "m1"},
        name="update_task_missing_404")
    add(ops.update_task, config_basic(),
        {"task_name": "T1", "update_params": True, "params": {"bogus": 1}},
        name="update_task_bad_params_422")
    add(ops.update_task, config_basic(),
        {"task_name": "T1", "update_fallback": True, "fallback_model": "m1"},
        name="update_task_fallback_same_as_model")

    add(ops.delete_task, config_basic(), {"task_name": "T1"},
        name="delete_task_last_removes_key")
    add(ops.delete_task, {"config_version": 4, "tasks": {"a": {"model": "m"}, "b": {"model": "m"}}},
        {"task_name": "a"}, name="delete_task_keeps_others")
    add(ops.delete_task, config_basic(), {"task_name": "ghost"}, name="delete_task_404")

    add(ops.repair_tasks, config_basic(), name="repair_tasks_intact")
    add(ops.repair_tasks, {"config_version": 4, "models": {}, "tasks": {"T": {"model": "gone"}}},
        name="repair_tasks_drops_unknown_model")
    add(ops.repair_tasks, {"config_version": 4, "models": {"m": {}},
                           "tasks": {"T": {"model": "m"}, "U": {"model": "m", "fallback_model": "m"}}},
        name="repair_tasks_clears_duplicate_fallback")
    add(ops.repair_tasks, {"config_version": 4, "models": {"m": {}},
                           "tasks": {"T": {"model": "m", "params": {"bogus": 1}}}},
        name="repair_tasks_drops_bad_params")
    add(ops.repair_tasks, {"config_version": 4, "models": {"m": {}}, "tasks": {"T": "junk"}},
        name="repair_tasks_drops_non_object")
    add(ops.repair_tasks, {"config_version": 4, "models": {"m": {"aliases": ["alias"]}},
                           "tasks": {"T": {"model": "alias", "fallback_model": "m"}}},
        name="repair_tasks_resolves_alias")
    add(ops.repair_tasks, {"config_version": 4, "models": {"m": {}}, "tasks": {}},
        name="repair_tasks_empty_tasks_removed")
    add(ops.repair_tasks, {"config_version": 4, "models": {"m": {}}, "tasks": []},
        name="repair_tasks_non_object_tasks_removed")


# --------------------------------------------------------------------------- #
# 8. 迁移
# --------------------------------------------------------------------------- #

def transfer_cases() -> None:
    add(ops.transferable_config, config_basic(), {"include_visitor": True},
        name="transferable_config_with_visitor")
    add(ops.transferable_config, config_basic(), {"include_visitor": False},
        name="transferable_config_without_visitor")
    add(ops.transferable_config,
        {"config_version": 4,
         "providers": {"p": {"base_url": "https://a.example", "_amkr_model_key_clone": True,
                             "keys": {"k": {"api_key": "1", "capabilities": {"models": ["a"]},
                                            "allow_visitor": True}}}},
         "models": {"m": {"targets": [{"provider": "p", "key": "k", "upstream_model": "u"}]}},
         "local_api_key": "sk-local"},
        {"include_visitor": True},
        name="transferable_config_strips_local_fields")
    add(ops.transferable_config, {"providers": {}, "models": {}}, {"include_visitor": True},
        name="transferable_config_empty")
    add(ops.transferable_config, {"providers": {}, "models": {}, "tasks": {}}, {"include_visitor": True},
        name="transferable_config_empty_tasks_omitted")

    add(ops.merge_transferable_config, config_basic(), {"transfer_data": config_transfer()},
        name="merge_transferable_config_into_existing")
    add(ops.merge_transferable_config, {"config_version": 4, "providers": {}, "models": {}},
        {"transfer_data": config_transfer()},
        name="merge_transferable_config_into_empty")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"config_version": 4,
                           "providers": {"p": {"base_url": "https://other.example",
                                               "keys": {"k1": {"api_key": "s1"},
                                                        "k2": {"api_key": "new"}}}},
                           "models": {"m": {"targets": [
                               {"provider": "p", "key": "k1", "upstream_model": "u"},
                               {"provider": "p", "key": "k2", "upstream_model": "v"}]}}}},
        name="merge_transferable_config_renames_provider_and_skips_secret")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"config_version": 4,
                           "providers": {"openai": {"base_url": "https://api.openai.com",
                                                    "keys": {"k1": {"api_key": "s1"},
                                                             "k9": {"api_key": "s9"}}}},
                           "models": {"m1": {"targets": [
                               {"provider": "openai", "key": "k9", "upstream_model": "u9"}]}}}},
        name="merge_transferable_config_same_url_merges_keys")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {"p": {"base_url": "https://a.example",
                                               "keys": {"k": {"api_key": "1"}}}},
                           "models": {"m": {"targets": [{"provider": "p", "key": "k",
                                                         "upstream_model": "u"}]}},
                           "tasks": {"T": {"model": "m"}, "NEW": {"model": "m"}}}},
        name="merge_transferable_config_tasks")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {}, "models": {}, "tasks": {"T": {"model": "gone"}}}},
        name="merge_transferable_config_unresolvable_task_dropped")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {"p": {"base_url": "https://a.example",
                                               "keys": {"k": {"api_key": "1"}}}},
                           "models": {"m": {"targets": [{"provider": "ghost", "key": "k",
                                                         "upstream_model": "u"}]}}}},
        name="merge_transferable_config_skips_unmapped_provider")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {}, "models": {"m2": {"targets": "junk"}}}},
        name="merge_transferable_config_bad_source_targets_400")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {"openai": {"keys": {}}}, "models": {}}},
        name="merge_transferable_config_source_provider_without_base_url",
        note="归一化源供应商 base_url 时 normalize_upstream_base_url 抛裸 ValueError")
    add(ops.merge_transferable_config,
        {"config_version": 4, "providers": {"p": {"keys": {}}}, "models": {}},
        {"transfer_data": {"providers": {"p": {"base_url": "https://a.example", "keys": {}}},
                           "models": {}}},
        name="merge_transferable_config_current_provider_without_base_url",
        note="归一化当前供应商 base_url 时抛裸 ValueError")
    add(ops.merge_transferable_config, {"config_version": 4, "models": []},
        {"transfer_data": {"providers": {}, "models": {}}},
        name="merge_transferable_config_current_models_not_object_400")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {}, "models": {"unified-model": {"targets": []}}}},
        name="merge_transferable_config_reserved_model_name_422",
        note="合并后的配置仍要能通过 RouterConfig.from_dict，这里故意让它失败")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {}, "models": {}}},
        name="merge_transferable_config_noop")
    add(ops.merge_transferable_config, config_basic(),
        {"transfer_data": {"providers": {"p": {"base_url": "https://a.example",
                                               "keys": {"k": {"api_key": "1"}}}},
                           "models": {"broken": {"targets": [{"provider": "p", "key": "k",
                                                              "upstream_model": "u"}]}}}},
        name="merge_transferable_config_secret_skipped_leaves_bare_model",
        note="合并出的配置仍要能通过 RouterConfig.from_dict")


# --------------------------------------------------------------------------- #
# 渲染与入口
# --------------------------------------------------------------------------- #

def build_cases() -> list[dict]:
    accessor_cases()
    base_url_cases()
    provider_cases()
    model_cases()
    model_key_cases()
    unified_cases()
    task_cases()
    transfer_cases()
    names = [case["name"] for case in CASES]
    duplicates = {name for name in names if names.count(name) > 1}
    if duplicates:
        raise AssertionError(f"语料用例名重复: {sorted(duplicates)}")
    return CASES


def render(cases: list[dict]) -> str:
    payload = {"version": CORPUS_VERSION, "cases": cases}
    return json.dumps(payload, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_PATH)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    content = render(build_cases())
    if args.check:
        if not args.output.exists():
            print(f"语料缺失: {args.output}", file=sys.stderr)
            return 1
        if args.output.read_text(encoding="utf-8") != content:
            print(f"语料已过期: {args.output}", file=sys.stderr)
            return 1
        print(f"语料最新: {args.output}")
        return 0

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(content, encoding="utf-8", newline="\n")
    print(f"已写入 {len(CASES)} 条语料: {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
