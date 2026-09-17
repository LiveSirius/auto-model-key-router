from __future__ import annotations

from auto_model_key_router import config_operations as operations
from auto_model_key_router.config import RouterConfig


def test_provider_operations_allow_an_empty_provider() -> None:
    data = {"config_version": 4, "providers": {}, "models": {}}

    operations.create_provider(data, "empty", "https://empty.example.test")

    assert data["providers"]["empty"] == {
        "base_url": "https://empty.example.test",
        "keys": {},
    }
    RouterConfig.from_dict(data)


def test_delete_model_key_is_local_when_a_key_is_shared() -> None:
    data = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {
                    "main": {"api_key": "sk-main"},
                    "backup": {"api_key": "sk-backup"},
                },
            }
        },
        "models": {
            "first": {
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "main",
                        "upstream_model": "upstream",
                    },
                    {
                        "provider": "gateway",
                        "key": "backup",
                        "upstream_model": "upstream",
                    },
                ]
            },
            "second": {
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "main",
                        "upstream_model": "upstream",
                    }
                ]
            },
        },
    }

    # Removing the binding from "first" must not delete the shared provider key.
    operations.delete_model_key(data, "first", "gateway-main")

    assert set(data["providers"]["gateway"]["keys"]) == {"main", "backup"}
    assert data["models"]["second"]["targets"][0]["provider"] == "gateway"
    assert data["models"]["second"]["targets"][0]["key"] == "main"
    # "first" still routes through its remaining backup key.
    assert data["models"]["first"]["targets"] == [
        {
            "provider": "gateway",
            "key": "backup",
            "upstream_model": "upstream",
        }
    ]
    RouterConfig.from_dict(data)


def test_clearing_all_model_keys_removes_model_even_when_it_has_aliases() -> None:
    data = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {"main": {"api_key": "sk-main"}},
            }
        },
        "models": {
            "local": {
                "aliases": ["friendly"],
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "main",
                        "upstream_model": "upstream",
                    }
                ],
            }
        },
    }

    operations.delete_model_key_local(data, "local", "main")

    assert data["models"] == {}
    assert data["providers"] == {}


def test_deleting_last_key_referencing_provider_key_removes_provider_key() -> None:
    data = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {"main": {"api_key": "sk-main"}},
            }
        },
        "models": {
            "local": {
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "main",
                        "upstream_model": "upstream",
                    }
                ]
            }
        },
    }

    operations.delete_model_key(data, "local", "main")

    assert data["models"] == {}
    assert data["providers"] == {}


def test_disabling_a_pinned_model_key_clears_the_unified_key() -> None:
    data = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {"main": {"api_key": "sk-main"}},
            }
        },
        "models": {
            "local": {
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "main",
                        "upstream_model": "upstream",
                    }
                ]
            }
        },
        "unified_model": {
            "default": {"primary": {"model": "local", "key": "main"}}
        },
    }

    operations.update_model_key_local(data, "local", "main", enabled=False)

    assert data["unified_model"]["default"]["primary"]["key"] is None
    RouterConfig.from_dict(data)


def test_merge_transferable_config_skips_targets_bound_to_duplicate_secret_keys() -> None:
    current = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {"existing": {"api_key": "sk-shared"}},
            }
        },
        "models": {
            "local": {
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "existing",
                        "upstream_model": "upstream",
                    }
                ]
            }
        },
    }
    transfer = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {
                    # 与现有 key secret 重复 → 跳过不导入。
                    "existing": {"api_key": "sk-shared"},
                    "fresh": {"api_key": "sk-fresh"},
                },
            }
        },
        "models": {
            "remote-a": {
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "existing",
                        "upstream_model": "remote-a",
                    },
                    {
                        "provider": "gateway",
                        "key": "fresh",
                        "upstream_model": "remote-a",
                    },
                ]
            }
        },
    }

    merged, added_models, added_keys, skipped_keys = operations.merge_transferable_config(
        current, transfer
    )

    assert added_models == 1
    assert added_keys == 1
    assert skipped_keys == 1
    # 绑定到被跳过 key 的 target 一并过滤，避免引用不存在的 key。
    assert merged["models"]["remote-a"]["targets"] == [
        {"provider": "gateway", "key": "fresh", "upstream_model": "remote-a"}
    ]
    RouterConfig.from_dict(merged)


def test_merge_transferable_config_renames_target_key_when_source_key_is_renamed() -> None:
    current = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {"main": {"api_key": "sk-main"}},
            }
        },
        "models": {},
    }
    transfer = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {
                    # 重名但 secret 不同 → 追加为 main-2。
                    "main": {"api_key": "sk-other"},
                },
            }
        },
        "models": {
            "remote-a": {
                "targets": [
                    {
                        "provider": "gateway",
                        "key": "main",
                        "upstream_model": "remote-a",
                    }
                ]
            }
        },
    }

    merged, added_models, added_keys, skipped_keys = operations.merge_transferable_config(
        current, transfer
    )

    assert added_models == 1
    assert added_keys == 1
    assert skipped_keys == 0
    assert merged["providers"]["gateway"]["keys"]["main-2"]["api_key"] == "sk-other"
    assert merged["models"]["remote-a"]["targets"] == [
        {"provider": "gateway", "key": "main-2", "upstream_model": "remote-a"}
    ]
    RouterConfig.from_dict(merged)


def test_hidden_aliases_round_trip_and_reject_collisions() -> None:
    data = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {"main": {"api_key": "sk-main"}},
            }
        },
        "models": {},
    }
    operations.create_model(
        data,
        "local",
        aliases=["friendly"],
        hidden_aliases=["deepseek-v4.1-flash"],
        targets=[{"provider": "gateway", "key": "main", "upstream_model": "remote"}],
    )
    assert data["models"]["local"]["hidden_aliases"] == ["deepseek-v4.1-flash"]
    config = RouterConfig.from_dict(data)
    assert config.hidden_model_names() == {
        "deepseek-v4.1-flash": "local",
        "remote": "local",
    }

    # 隐藏别名与既有别名/模型 ID/其他模型的隐藏别名都不能撞名。
    for bad in (["friendly"], ["local"], ["deepseek-v4.1-flash"]):
        try:
            operations.create_model(data, "another", hidden_aliases=bad)
        except operations.ConfigOperationError:
            continue
        raise AssertionError(f"应当拒绝重复的隐藏别名: {bad}")

    # 单改别名也不能撞上本模型已有的隐藏别名，反之亦然。
    for kwargs in (
        {"aliases": ["deepseek-v4.1-flash"]},
        {"hidden_aliases": ["friendly"]},
    ):
        try:
            operations.update_model(data, "local", **kwargs)
        except operations.ConfigOperationError:
            continue
        raise AssertionError(f"应当拒绝自家名字撞名: {kwargs}")

    # 改名同样不能撞上隐藏别名。
    try:
        operations.update_model(data, "local", new_id="deepseek-v4.1-flash")
    except operations.ConfigOperationError:
        pass
    else:
        raise AssertionError("应当拒绝把模型 ID 改成隐藏别名")

    # 清空隐藏别名会连字段一起移除。
    operations.update_model(data, "local", hidden_aliases=[])
    assert "hidden_aliases" not in data["models"]["local"]
    RouterConfig.from_dict(data)


def test_updating_provider_key_api_key_drops_stale_capabilities() -> None:
    data = {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {
                    "main": {
                        "api_key": "sk-old",
                        "capabilities": {
                            "models": ["gpt-a"],
                            "route_status": {"openai": "ok"},
                            "errors": {},
                            "checked_at": "2026-09-01T00:00:00+00:00",
                        },
                    }
                },
            }
        },
        "models": {},
    }

    # 换 API Key：探测缓存属于旧凭据，必须失效。
    operations.update_provider_key(data, "gateway", "main", api_key="sk-new")
    assert data["providers"]["gateway"]["keys"]["main"]["api_key"] == "sk-new"
    assert "capabilities" not in data["providers"]["gateway"]["keys"]["main"]

    # 重新探测后，改名/禁用不应丢新缓存。
    key = data["providers"]["gateway"]["keys"]["main"]
    key["capabilities"] = {"models": ["gpt-b"], "checked_at": "2026-09-02T00:00:00+00:00"}
    operations.update_provider_key(data, "gateway", "main", new_name="renamed", enabled=False)
    renamed = data["providers"]["gateway"]["keys"]["renamed"]
    assert renamed["capabilities"]["models"] == ["gpt-b"]
    assert renamed["enabled"] is False
    RouterConfig.from_dict(data)


def task_data() -> dict:
    return {
        "config_version": 4,
        "providers": {
            "gateway": {
                "base_url": "https://gateway.example.test",
                "keys": {"main": {"api_key": "sk-main"}},
            }
        },
        "models": {
            "primary": {
                "aliases": ["fast"],
                "targets": [
                    {"provider": "gateway", "key": "main", "upstream_model": "up-main"}
                ],
            },
            "backup": {
                "targets": [
                    {"provider": "gateway", "key": "main", "upstream_model": "up-backup"}
                ],
            },
        },
    }


def test_task_operations_round_trip_through_validation() -> None:
    data = task_data()

    operations.create_task(
        data,
        "TASK_000001",
        model="fast",
        fallback_model="backup",
        params={"temperature": 0.2, "stop": ["\n\n"]},
    )

    # 别名要落成真实模型 ID，后面删模型时才能对上号。
    assert data["tasks"]["TASK_000001"] == {
        "model": "primary",
        "fallback_model": "backup",
        "params": {"temperature": 0.2, "stop": ["\n\n"]},
    }
    config = RouterConfig.from_dict(data)
    task = config.task_for("TASK_000001")
    assert task is not None
    assert task.plan.primary.model == "primary"
    assert task.plan.fallback.model == "backup"

    operations.update_task(
        data, "TASK_000001", params={"top_k": 5}, update_params=True
    )
    assert data["tasks"]["TASK_000001"]["params"] == {"top_k": 5}

    # 清空备选与参数要把字段一起移除，而不是留个空壳。
    operations.update_task(
        data, "TASK_000001", fallback_model=None, update_fallback=True
    )
    operations.update_task(data, "TASK_000001", params=None, update_params=True)
    assert data["tasks"]["TASK_000001"] == {"model": "primary"}
    RouterConfig.from_dict(data)

    operations.delete_task(data, "TASK_000001")
    # 最后一个任务删掉后连 tasks 键一起清理，避免导出里出现空对象。
    assert "tasks" not in data


def test_task_operations_reject_bad_input() -> None:
    data = task_data()

    for kwargs, reason in (
        ({"model": "nope"}, "未配置的模型"),
        ({"model": "primary", "params": {"temprature": 0.2}}, "不支持的参数"),
        ({"model": "primary", "params": {"reasoning_effort": "extreme"}}, "reasoning_effort"),
    ):
        try:
            operations.create_task(data, "TASK_000002", **kwargs)
        except operations.ConfigOperationError as exc:
            assert reason in str(exc), (reason, str(exc))
        else:
            raise AssertionError(f"应当拒绝: {kwargs}")

    assert "tasks" not in data

    # 任务名不能和模型 ID 或别名撞名，否则路由要靠查找顺序决定。
    operations.create_task(data, "TASK_000003", model="primary")
    for name in ("TASK_000003", "primary", "fast"):
        try:
            operations.create_task(data, name, model="primary")
        except operations.ConfigOperationError:
            continue
        raise AssertionError(f"应当拒绝任务名: {name}")


def test_deleting_a_model_cleans_up_tasks_that_reference_it() -> None:
    data = task_data()
    operations.create_task(
        data, "TASK_000001", model="primary", fallback_model="backup"
    )
    operations.create_task(data, "TASK_000002", model="backup")

    operations.delete_model(data, "primary")

    # 首选没了 → 整个任务删除；备选没了 → 退化成单模型任务。
    assert "TASK_000001" not in data["tasks"]
    assert data["tasks"]["TASK_000002"] == {"model": "backup"}
    RouterConfig.from_dict(data)

    operations.delete_model(data, "backup")
    assert "tasks" not in data


def test_deleting_a_model_repairs_unified_model_even_with_stale_tasks() -> None:
    # 回归：repair_unified_model 要先解析候选配置才敢改，残留的失效任务会让那次
    # 解析失败，于是它什么都不修 —— 统一模型仍指向已删模型，配置直接不可加载。
    data = task_data()
    data["unified_model"] = {"default": {"primary": {"model": "primary", "key": None}}}
    operations.create_task(data, "TASK_000001", model="primary")

    operations.delete_model(data, "primary")

    assert data["unified_model"]["default"]["primary"]["model"] == "backup"
    assert "tasks" not in data
    RouterConfig.from_dict(data)

