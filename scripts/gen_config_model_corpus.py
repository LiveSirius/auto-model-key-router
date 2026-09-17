#!/usr/bin/env python3
"""生成 RouterConfig 解析与校验的对拍语料（Python 侧为参照实现）。

覆盖 ``RouterConfig.from_dict`` 与 ``RouterConfig.validate`` 的完整行为，包括：

- providers / models / targets 三层解析与 key 名字去重规则；
- unified_model 与 tasks 的模型名解析（别名 → 真实 id）；
- 全部校验分支的报错文本（这些文本会经 management API 回给用户，属对外契约）；
- 顶层 ``upstream_routes`` 是否真正参与解析（见下方 NOTE）。

``RouterConfig`` 是 frozen dataclass，不能直接 json 序列化，因此这里写一个显式的
序列化器把结果摊平成稳定的 JSON 结构，Go 侧产出同样的结构后逐字节比对。
表示形式是本脚本定义的**测试契约**，不是产品输出。

用法::

    python scripts/gen_config_model_corpus.py
    python scripts/gen_config_model_corpus.py --check
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router.config import RouterConfig  # noqa: E402

DEFAULT_DIR = REPO_ROOT / "internal" / "config" / "testdata"


def canonical(obj: object) -> str:
    return json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def dumps_ordered(obj: object) -> str:
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


# --------------------------------------------------------------------------- #
# 序列化器：把 RouterConfig 摊平成稳定结构
# --------------------------------------------------------------------------- #

def serialize_route_target(target) -> dict:
    return {"model": target.model, "key": target.key}


def serialize_plan(plan) -> dict | None:
    if plan is None:
        return None
    return {
        "primary": serialize_route_target(plan.primary),
        "fallback": serialize_route_target(plan.fallback) if plan.fallback else None,
    }


def serialize_key(key) -> dict:
    return {
        "name": key.name,
        "api_key": key.api_key,
        "base_url": key.base_url,
        "enabled": key.enabled,
        "allow_visitor": key.allow_visitor,
        "upstream_routes": key.upstream_routes,
        "provider": key.provider,
        "upstream_model": key.upstream_model,
    }


def serialize_model(model) -> dict:
    return {
        "id": model.id,
        "keys": [serialize_key(key) for key in model.keys],
        "aliases": list(model.aliases),
        "routing_mode": model.routing_mode,
        "reasoning_effort": model.reasoning_effort,
        "native_first": model.native_first,
        "hidden_aliases": list(model.hidden_aliases),
    }


def serialize_provider(provider) -> dict:
    return {
        "id": provider.id,
        "base_url": provider.base_url,
        "keys": [
            {
                "name": key.name,
                "api_key": key.api_key,
                "enabled": key.enabled,
                "allow_visitor": key.allow_visitor,
                "capabilities": key.capabilities,
            }
            for key in provider.keys
        ],
        "routes": provider.routes,
        "capabilities": provider.capabilities,
    }


def serialize_task(task) -> dict:
    return {
        "name": task.name,
        "model": task.model,
        "fallback_model": task.fallback_model,
        "params": task.params,
    }


def serialize_config(cfg: RouterConfig) -> dict:
    """把 RouterConfig 摊平成 JSON 结构。

    ``hidden_model_names`` 是派生结果（手写隐藏别名 + targets 的 upstream_model），
    它决定哪些名字不出现在 /v1/models，属于对外可见行为，故一并固化。
    """
    return {
        "host": cfg.host,
        "port": cfg.port,
        "request_timeout": cfg.request_timeout,
        "stream_first_byte_timeout": cfg.stream_first_byte_timeout,
        "stream_idle_timeout": cfg.stream_idle_timeout,
        "max_retries": cfg.max_retries,
        "key_failure_threshold": cfg.key_failure_threshold,
        "key_cooldown_seconds": cfg.key_cooldown_seconds,
        "endpoint_capabilities_path": cfg.endpoint_capabilities_path,
        "metrics_db_path": cfg.metrics_db_path,
        "log_file_path": cfg.log_file_path,
        "local_api_key": cfg.local_api_key,
        "webui_enabled": cfg.webui_enabled,
        "ops_enabled": cfg.ops_enabled,
        "models": [serialize_model(model) for model in cfg.models],
        "providers": [serialize_provider(provider) for provider in cfg.providers],
        "upstream_routes": cfg.upstream_routes,
        "unified_model": None
        if cfg.unified_model is None
        else {
            "default": serialize_plan(cfg.unified_model.default),
            "image": serialize_plan(cfg.unified_model.image),
            "embeddings": serialize_plan(cfg.unified_model.embeddings),
        },
        "tasks": [serialize_task(task) for task in cfg.tasks],
        "reasoning_effort_by_model": cfg.reasoning_effort_by_model,
        "hidden_model_names": cfg.hidden_model_names(),
    }


def make_entry(name: str, raw: dict) -> dict:
    entry: dict = {"name": name, "input": dumps_ordered(raw)}
    try:
        cfg = RouterConfig.from_dict(raw)
    except Exception as exc:  # noqa: BLE001 - 语料要记录任意异常
        entry.update(ok=False, error_type=type(exc).__name__, error=str(exc))
        return entry
    entry.update(ok=True, output=canonical(serialize_config(cfg)))
    return entry


# --------------------------------------------------------------------------- #
# 基础夹具
# --------------------------------------------------------------------------- #

def base(**overrides) -> dict:
    """最小可用的 v4 配置，用于逐项叠加被测字段。"""
    raw = {
        "config_version": 4,
        "host": "127.0.0.1",
        "port": 8000,
        "default_base_url": "https://api.openai.com",
        "local_api_key": "local-key",
        "providers": {
            "openai": {
                "base_url": "https://api.openai.com",
                "keys": {"main": {"api_key": "sk-a"}},
            }
        },
        "models": {
            "gpt-4o-mini": {
                "targets": [
                    {"provider": "openai", "key": "main", "upstream_model": "gpt-4o-mini"}
                ]
            }
        },
    }
    raw.update(overrides)
    return raw


def model_cases() -> list[dict]:
    cases: list[dict] = []

    def add(name: str, raw: dict) -> None:
        cases.append(make_entry(name, raw))

    # ---- 最小与边界 ----
    add("minimal", base())
    add("empty_everything", {"config_version": 4, "local_api_key": "k"})
    add("no_local_api_key", {"config_version": 4})
    add("wrong_version", {**base(), "config_version": 3, "models": {"m": {"targets": []}}})

    # ---- 标量默认值与强转 ----
    add("scalar_defaults_omitted", {"config_version": 4, "local_api_key": "k", "models": {}, "providers": {}})
    add(
        "scalar_explicit",
        base(
            host="0.0.0.0",
            port=9999,
            request_timeout=12.5,
            stream_first_byte_timeout=7,
            stream_idle_timeout=8,
            max_retries=5,
            key_failure_threshold=9,
            key_cooldown_seconds=1.5,
            webui_enabled=True,
            ops_enabled=False,
        ),
    )
    add("key_failure_threshold_clamped", base(key_failure_threshold=0))
    add("key_failure_threshold_negative", base(key_failure_threshold=-5))
    add("key_cooldown_clamped", base(key_cooldown_seconds=-10))
    add("port_as_string", base(port="8080"))
    add("port_as_float", base(port=8080.0))
    add("timeout_as_int", base(request_timeout=30))
    add("timeout_as_string", base(request_timeout="45"))
    add("timeout_bad_string", base(request_timeout="soon"))
    add("stream_first_byte_zero", base(stream_first_byte_timeout=0))
    add("stream_first_byte_negative", base(stream_first_byte_timeout=-1))
    add("stream_idle_zero", base(stream_idle_timeout=0))
    add("local_api_key_reserved", base(local_api_key="amkr-visitor"))
    add("local_api_key_numeric", base(local_api_key=12345))
    add("host_numeric", base(host=1234))
    add("webui_enabled_truthy", base(webui_enabled="yes"))
    add("ops_enabled_falsy", base(ops_enabled=0))

    # ---- 路径字段 ----
    add(
        "paths_from_fields",
        base(
            endpoint_capabilities_path="/tmp/caps.json",
            metrics_db_path="/tmp/metrics.sqlite3",
            log_file_path="/tmp/server.log",
        ),
    )
    add("paths_blank_fall_back", base(endpoint_capabilities_path="", metrics_db_path="", log_file_path=""))
    add(
        "key_state_path_legacy_fallback",
        {**base(), "endpoint_capabilities_path": None, "key_state_path": "/tmp/legacy.json"},
    )

    # ---- NOTE: 顶层 upstream_routes 是否参与解析 ----
    add(
        "top_level_upstream_routes_only",
        {**base(), "upstream_routes": {"https://api.openai.com": {"anthropic": "anthropic-prefix"}}},
    )
    add(
        "provider_routes_merged",
        {
            **base(),
            "providers": {
                "openai": {
                    "base_url": "https://api.openai.com",
                    "keys": {"main": {"api_key": "sk-a"}},
                    "routes": {"anthropic": "anthropic-via-provider"},
                }
            },
        },
    )
    add(
        "key_upstream_routes_merged",
        {
            **base(),
            "providers": {
                "openai": {
                    "base_url": "https://api.openai.com",
                    "keys": {
                        "main": {"api_key": "sk-a", "upstream_routes": {"anthropic": "anthropic-via-key"}}
                    },
                }
            },
        },
    )
    add(
        "provider_and_key_routes_conflict",
        {
            **base(),
            "providers": {
                "openai": {
                    "base_url": "https://api.openai.com",
                    "keys": {
                        "main": {"api_key": "sk-a", "upstream_routes": {"anthropic": "from-key"}}
                    },
                    "routes": {"anthropic": "from-provider"},
                }
            },
        },
    )
    add(
        "top_level_routes_conflict_with_provider",
        {
            **base(),
            "upstream_routes": {"https://api.openai.com": {"anthropic": "from-top"}},
            "providers": {
                "openai": {
                    "base_url": "https://api.openai.com",
                    "keys": {"main": {"api_key": "sk-a"}},
                    "routes": {"anthropic": "from-provider"},
                }
            },
        },
    )
    add(
        "routes_across_two_providers",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {
                "a": {"base_url": "https://a.example/", "keys": {"k": {"api_key": "1"}}, "routes": {"openai": "pa"}},
                "b": {"base_url": "https://b.example", "keys": {"k": {"api_key": "2"}}, "routes": {"openai": "pb"}},
            },
            "models": {
                "m": {
                    "targets": [
                        {"provider": "a", "key": "k", "upstream_model": "m"},
                        {"provider": "b", "key": "k", "upstream_model": "m"},
                    ]
                }
            },
        },
    )
    add("provider_routes_invalid_mode", {
        **base(),
        "providers": {
            "openai": {
                "base_url": "https://api.openai.com",
                "keys": {"main": {"api_key": "sk-a"}},
                "routes": {"bogus": "x"},
            }
        },
    })
    add("base_url_trailing_slash", {
        **base(),
        "providers": {"openai": {"base_url": "https://api.openai.com///", "keys": {"main": {"api_key": "a"}}}},
    })
    add("base_url_blank_uses_default", {
        **base(),
        "providers": {"openai": {"base_url": "", "keys": {"main": {"api_key": "a"}}}},
    })
    add("base_url_missing_uses_default", {
        "config_version": 4,
        "local_api_key": "k",
        "providers": {"openai": {"keys": {"main": {"api_key": "a"}}}},
        "models": {},
    })
    add("base_url_no_scheme", {
        **base(),
        "providers": {"openai": {"base_url": "api.openai.com", "keys": {"main": {"api_key": "a"}}}},
    })

    # ---- providers 解析 ----
    add("providers_not_dict", {**base(), "providers": []})
    add("provider_not_dict", {**base(), "providers": {"openai": "bad"}})
    add("provider_missing_keys", {"config_version": 4, "local_api_key": "k", "providers": {"p": {"base_url": "https://a.example"}}, "models": {}})
    add("provider_keys_not_dict", {"config_version": 4, "local_api_key": "k", "providers": {"p": {"base_url": "https://a.example", "keys": []}}, "models": {}})
    add(
        "provider_key_not_dict",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": "bad"}}},
            "models": {},
        },
    )
    add(
        "provider_key_missing_api_key",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"enabled": True}}}},
            "models": {},
        },
    )
    add(
        "provider_key_flags",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {
                "p": {
                    "base_url": "https://a.example",
                    "keys": {
                        "off": {"api_key": "1", "enabled": False},
                        "visitor": {"api_key": "2", "allow_visitor": True},
                        "caps": {"api_key": "3", "capabilities": {"models": ["m"]}},
                        "caps_not_dict": {"api_key": "4", "capabilities": "bad"},
                    },
                }
            },
            "models": {},
        },
    )
    add(
        "provider_capabilities",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {"p": {"base_url": "https://a.example", "keys": {}, "capabilities": {"models": ["m"], "checked_at": "t"}}},
            "models": {},
        },
    )

    # ---- models / targets 解析 ----
    add("models_not_dict", {**base(), "models": []})
    add(
        "target_unknown_provider_key",
        {
            **base(),
            "models": {"m": {"targets": [{"provider": "openai", "key": "ghost"}]}},
        },
    )
    add("target_non_dict_skipped", {**base(), "models": {"m": {"targets": ["bad"]}}})
    add("targets_not_list", {**base(), "models": {"m": {"targets": "bad"}}})
    add("target_missing_upstream_model_uses_model_id", {
        **base(),
        "models": {"m": {"targets": [{"provider": "openai", "key": "main"}]}},
    })
    add(
        "target_enabled_flag_disables_key",
        {
            **base(),
            "models": {"m": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u", "enabled": False}]}},
        },
    )
    add(
        "target_name_override",
        {
            **base(),
            "models": {"m": {"targets": [{"provider": "openai", "key": "main", "name": "custom"}]}},
        },
    )
    add(
        "target_name_collision_qualified",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {
                "pa": {"base_url": "https://a.example", "keys": {"main": {"api_key": "1"}}},
                "pb": {"base_url": "https://b.example", "keys": {"main": {"api_key": "2"}}},
            },
            "models": {
                "m": {
                    "targets": [
                        {"provider": "pa", "key": "main", "upstream_model": "u"},
                        {"provider": "pb", "key": "main", "upstream_model": "u"},
                        {"provider": "pa", "key": "main", "upstream_model": "u"},
                    ]
                }
            },
        },
    )
    add(
        "same_key_name_across_models",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {"p": {"base_url": "https://a.example", "keys": {"main": {"api_key": "1"}}}},
            "models": {
                "m1": {"targets": [{"provider": "p", "key": "main", "upstream_model": "u"}]},
                "m2": {"targets": [{"provider": "p", "key": "main", "upstream_model": "u"}]},
            },
        },
    )
    add("model_field_defaults", {
        **base(),
        "models": {"m": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}]}},
    })
    add(
        "model_explicit_fields",
        {
            **base(),
            "models": {
                "m": {
                    "targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}],
                    "aliases": ["a1", "a2"],
                    "hidden_aliases": [" h1 ", "", "h2"],
                    "routing_mode": "priority",
                    "reasoning_effort": "high",
                    "native_first": False,
                }
            },
        },
    )
    add(
        "model_reasoning_effort_neutralized",
        {
            **base(),
            "models": {
                "m": {
                    "targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}],
                    "reasoning_effort": "downstream",
                }
            },
        },
    )
    add(
        "model_reasoning_effort_default_word",
        {
            **base(),
            "models": {
                "m": {
                    "targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}],
                    "reasoning_effort": "default",
                }
            },
        },
    )
    add("model_routing_mode_from_top_level_default", {
        **base(),
        "routing_mode": "priority",
        "models": {"m": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}]}},
    })
    add("model_id_override", {
        **base(),
        "models": {"keyname": {"id": "realid", "targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}]}},
    })
    add("aliases_blank_filtered", {
        **base(),
        "models": {"m": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}], "aliases": ["ok", "", "  "]}},
    })

    # ---- 校验分支 ----
    add("routing_mode_invalid", {
        **base(),
        "models": {"m": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}], "routing_mode": "bogus"}},
    })
    add("reasoning_effort_invalid", {
        **base(),
        "models": {"m": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}], "reasoning_effort": "insane"}},
    })
    add("duplicate_model_name", {
        **base(),
        "models": {
            "m1": {"targets": [], "aliases": ["shared"]},
            "m2": {"targets": [], "aliases": ["shared"]},
        },
    })
    add("alias_collides_with_model_id", {
        **base(),
        "models": {"m1": {"targets": []}, "m2": {"targets": [], "aliases": ["m1"]}},
    })
    add("empty_api_key_in_target", {
        **base(),
        "providers": {"openai": {"base_url": "https://api.openai.com", "keys": {"main": {"api_key": ""}}}},
    })
    add("invalid_base_url_rejected", {
        **base(),
        "providers": {"openai": {"base_url": "ftp://a.example", "keys": {"main": {"api_key": "a"}}}},
    })
    add("hidden_alias_empty", {
        **base(),
        "models": {"m": {"targets": [], "hidden_aliases": [""]}},
    })
    add("hidden_alias_reserved_unified", {
        **base(),
        "models": {"m": {"targets": [], "hidden_aliases": ["unified-model"]}},
    })
    add("hidden_alias_collides_other_model", {
        **base(),
        "models": {"m1": {"targets": []}, "m2": {"targets": [], "hidden_aliases": ["m1"]}},
    })
    add("hidden_alias_same_model_ok", {
        **base(),
        "models": {"m": {"targets": [], "hidden_aliases": ["m"]}},
    })
    add("hidden_alias_duplicate_across_models", {
        **base(),
        "models": {"m1": {"targets": [], "hidden_aliases": ["h"]}, "m2": {"targets": [], "hidden_aliases": ["h"]}},
    })
    add("upstream_route_invalid_path", {
        **base(),
        "providers": {
            "openai": {
                "base_url": "https://api.openai.com",
                "keys": {"main": {"api_key": "a"}},
                "routes": {"openai": "https://evil.example"},
            }
        },
    })

    # ---- 派生：hidden_model_names ----
    add(
        "hidden_names_from_upstream_model",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "1"}}}},
            "models": {
                "local-a": {"targets": [{"provider": "p", "key": "k", "upstream_model": "up-a"}]},
                "local-b": {"targets": [{"provider": "p", "key": "k", "upstream_model": "up-b"}]},
            },
        },
    )
    add(
        "hidden_names_prefer_real_id",
        {
            "config_version": 4,
            "local_api_key": "k",
            "providers": {"p": {"base_url": "https://a.example", "keys": {"k": {"api_key": "1"}}}},
            "models": {
                "real": {"targets": [{"provider": "p", "key": "k", "upstream_model": "real"}]},
                "other": {"targets": [{"provider": "p", "key": "k", "upstream_model": "real"}]},
            },
        },
    )

    # ---- unified_model ----
    add("unified_missing", base())
    add("unified_not_dict", {**base(), "unified_model": []})
    add("unified_missing_default", {**base(), "unified_model": {}})
    add("unified_references_unknown_model", {
        **base(),
        "unified_model": {"default": {"primary": {"model": "nope"}}},
    })
    add("unified_primary_not_dict", {
        **base(),
        "unified_model": {"default": {"primary": "bad"}},
    })
    add("unified_default_not_dict", {
        **base(),
        "unified_model": {"default": "bad"},
    })
    add("unified_by_alias", {
        **base(),
        "models": {"gpt-4o-mini": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}], "aliases": ["fast"]}},
        "unified_model": {"default": {"primary": {"model": "fast", "key": "main"}}},
    })
    add("unified_with_fallback", {
        **base(),
        "providers": {"openai": {"base_url": "https://api.openai.com", "keys": {"main": {"api_key": "a"}, "alt": {"api_key": "b"}}}},
        "models": {
            "m1": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u1"}]},
            "m2": {"targets": [{"provider": "openai", "key": "alt", "upstream_model": "u2"}]},
        },
        "unified_model": {
            "default": {"primary": {"model": "m1", "key": "main"}, "fallback": {"model": "m2", "key": "alt"}}
        },
    })
    add("unified_image_and_embeddings", {
        **base(),
        "providers": {"openai": {"base_url": "https://api.openai.com", "keys": {"main": {"api_key": "a"}}}},
        "models": {
            "chat": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}]},
            "img": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "i"}]},
            "emb": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "e"}]},
        },
        "unified_model": {
            "default": {"primary": {"model": "chat"}},
            "image": {"primary": {"model": "img"}},
            "embeddings": {"primary": {"model": "emb"}},
        },
    })
    add("unified_same_primary_fallback", {
        **base(),
        "unified_model": {
            "default": {"primary": {"model": "gpt-4o-mini"}, "fallback": {"model": "gpt-4o-mini"}}
        },
    })
    add("unified_key_not_available", {
        **base(),
        "unified_model": {"default": {"primary": {"model": "gpt-4o-mini", "key": "ghost"}}},
    })
    add("unified_key_disabled", {
        **base(),
        "providers": {"openai": {"base_url": "https://api.openai.com", "keys": {"main": {"api_key": "a", "enabled": False}}}},
        "unified_model": {"default": {"primary": {"model": "gpt-4o-mini", "key": "main"}}},
    })
    add("unified_model_id_reserved_collision", {
        **base(),
        "models": {"unified-model": {"targets": []}},
        "unified_model": {"default": {"primary": {"model": "unified-model"}}},
    })
    add("unified_key_blank_becomes_none", {
        **base(),
        "unified_model": {"default": {"primary": {"model": "gpt-4o-mini", "key": "   "}}},
    })
    add("unified_fallback_null", {
        **base(),
        "unified_model": {"default": {"primary": {"model": "gpt-4o-mini"}, "fallback": None}},
    })
    add("unified_image_null", {
        **base(),
        "unified_model": {"default": {"primary": {"model": "gpt-4o-mini"}}, "image": None},
    })

    # ---- tasks ----
    add("tasks_missing", base())
    add("tasks_not_dict", {**base(), "tasks": []})
    add("tasks_empty", {**base(), "tasks": {}})
    add("task_references_unknown_model", {
        **base(),
        "tasks": {"TASK_1": {"model": "nope"}},
    })
    add("task_model_not_dict", {**base(), "tasks": {"TASK_1": "bad"}})
    add("task_blank_name", {**base(), "tasks": {"  ": {"model": "gpt-4o-mini"}}})
    add("task_with_params", {
        **base(),
        "tasks": {"TASK_1": {"model": "gpt-4o-mini", "params": {"temperature": 0.2, "seed": 9, "stop": ["x"]}}},
    })
    add("task_params_invalid", {
        **base(),
        "tasks": {"TASK_1": {"model": "gpt-4o-mini", "params": {"bogus": 1}}},
    })
    add("task_with_fallback", {
        **base(),
        "providers": {"openai": {"base_url": "https://api.openai.com", "keys": {"main": {"api_key": "a"}, "alt": {"api_key": "b"}}}},
        "models": {
            "m1": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u1"}]},
            "m2": {"targets": [{"provider": "openai", "key": "alt", "upstream_model": "u2"}]},
        },
        "tasks": {"TASK_1": {"model": "m1", "fallback_model": "m2"}},
    })
    add("task_fallback_same_as_model", {
        **base(),
        "tasks": {"TASK_1": {"model": "gpt-4o-mini", "fallback_model": "gpt-4o-mini"}},
    })
    add("task_name_collides_with_model", {
        **base(),
        "tasks": {"gpt-4o-mini": {"model": "gpt-4o-mini"}},
    })
    add("task_name_collides_with_hidden_name", {
        **base(),
        "tasks": {"gpt-4o-mini": {"model": "gpt-4o-mini"}},
    })
    add("task_name_reserved_unified", {
        **base(),
        "tasks": {"unified-model": {"model": "gpt-4o-mini"}},
    })
    add("task_duplicate_names", {
        **base(),
        "tasks": {"TASK_1": {"model": "gpt-4o-mini"}},
    })
    add("task_by_alias", {
        **base(),
        "models": {"gpt-4o-mini": {"targets": [{"provider": "openai", "key": "main", "upstream_model": "u"}], "aliases": ["fast"]}},
        "tasks": {"TASK_1": {"model": "fast"}},
    })
    add("task_unknown_fallback", {
        **base(),
        "tasks": {"TASK_1": {"model": "gpt-4o-mini", "fallback_model": "nope"}},
    })

    # ---- v3 配置端到端解析（迁移 + from_dict） ----
    add(
        "v3_end_to_end",
        {
            "config_version": 3,
            "local_api_key": "k",
            "providers": {
                "openai": {
                    "base_url": "https://api.openai.com",
                    "keys": {"k1": {"api_key": "1"}, "k2": {"api_key": "2"}},
                    "pools": {"primary": {"keys": ["k1", "k2"], "models": ["m"]}},
                }
            },
            "models": {"m": {"targets": [{"provider": "openai", "pool": "primary", "upstream_model": "m"}]}},
        },
    )
    add(
        "v1_end_to_end",
        {
            "config_version": 1,
            "local_api_key": "k",
            "default_base_url": "https://api.openai.com",
            "models": [{"id": "m", "aliases": ["fast"], "keys": [{"name": "k1", "api_key": "1"}]}],
        },
    )
    add(
        "v3_end_to_end_multi_target",
        {
            "config_version": 3,
            "local_api_key": "k",
            "providers": {
                "a": {"base_url": "https://a.example", "keys": {"ka": {"api_key": "1"}}, "pools": {"p": {"keys": ["ka"]}}},
                "b": {"base_url": "https://b.example", "keys": {"kb": {"api_key": "2"}}, "pools": {"p": {"keys": ["kb"]}}},
            },
            "models": {
                "m": {
                    "targets": [
                        {"provider": "a", "pool": "p", "upstream_model": "ua"},
                        {"provider": "b", "pool": "p", "upstream_model": "ub"},
                    ]
                }
            },
        },
    )

    return cases


def render(cases: list[dict]) -> str:
    return "\n".join(json.dumps(c, ensure_ascii=False, sort_keys=True) for c in cases) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    path = args.output_dir / "model.jsonl"
    content = render(model_cases())

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
