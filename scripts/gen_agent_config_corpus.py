#!/usr/bin/env python3
"""生成 internal/agentconfig 的对拍语料（Python 侧为参照实现）。

语料由**真实运行** auto_model_key_router/agent_config.py 得到，不是手写的期望值。
所有路径都被钉进一个临时目录：

* HOME / USERPROFILE 指向临时根，于是 ``Path.home()`` 落在临时目录里；
* LOCALAPPDATA 指向临时根下的 cache/，于是 ``default_cache_dir()`` 也在里面；
* CLAUDE_CONFIG_DIR / PI_CODING_AGENT_DIR / CODEX_HOME 在需要时按用例注入。

**绝不会碰到开发者真实的 ~/.claude、~/.codex、~/.pi。**

语料里的绝对路径统一换算成 ``<root>/...`` 且剩余部分用 "/" 分隔，因此
Windows 生成、Linux CI 重放时逐字节一致（Go 测试用同样的函数换算自己的临时目录）。

用法::

    python scripts/gen_agent_config_corpus.py           # 写入语料
    python scripts/gen_agent_config_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import shutil
import sys
import tempfile
from contextlib import contextmanager
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router.agent_config import (  # noqa: E402
    AGENT_MODE_NATIVE,
    AGENT_MODE_UNIFIED_MODEL,
    CLAUDE_CODE,
    CODEX,
    PI_AGENT,
    AgentConfigError,
    _resolved_path,
    agent_backup_path,
    agent_config_path,
    configure_agent,
    get_agent_config_status,
    rollback_agent,
)
from auto_model_key_router.config import RouterConfig  # noqa: E402

DEFAULT_DIR = REPO_ROOT / "internal" / "agentconfig" / "testdata"

TARGET_REL = {
    CLAUDE_CODE: ".claude/settings.json",
    CODEX: ".codex/config.toml",
    PI_AGENT: ".pi/agent/models.json",
}
EXTRA_REL = {
    CODEX: {".codex/auth.json": ".codex/auth.json"},
}

PLACEHOLDER = "<root>"


# --------------------------------------------------------------------------- #
# 路径换算
# --------------------------------------------------------------------------- #

def portable_path(value, root: Path) -> str:
    """把绝对路径的 root 前缀换成 ``<root>``，剩余部分统一成 "/" 分隔。"""
    text = str(value)
    prefix = str(root)
    if not text.startswith(prefix):
        return text
    return PLACEHOLDER + text[len(prefix):].replace("\\", "/")


def portable_text(text: str, root: Path) -> str:
    """在**原始文本**里把 root 前缀换成占位符。

    备份 JSON 里的路径带 JSON 转义（Windows 上是 ``C:\\\\Users\\\\...``），所以
    必须同时匹配原文形式与 JSON 转义形式，并把占位符之后那段路径的分隔符统一成
    "/"，否则语料会带上平台与 git 配置相关的抖动。
    """
    escaped_root = json.dumps(str(root))[1:-1]
    marker = "\x00"
    out = text.replace(escaped_root, marker).replace(str(root), marker)

    pieces: list[str] = []
    index = 0
    while True:
        found = out.find(marker, index)
        if found < 0:
            pieces.append(out[index:])
            break
        pieces.append(out[index:found])
        pieces.append(PLACEHOLDER)
        cursor = found + 1
        segment: list[str] = []
        while cursor < len(out) and out[cursor] not in "\n\r\t\"' ,}]":
            char = out[cursor]
            if char == "\\" and cursor + 1 < len(out) and out[cursor + 1] == "\\":
                segment.append("/")
                cursor += 2
                continue
            if char in "\\/":
                segment.append("/")
                cursor += 1
                continue
            segment.append(char)
            cursor += 1
        pieces.append("".join(segment))
        index = cursor
    return "".join(pieces)


def json_escape(value: str) -> str:
    """按 JSON 字符串转义，用于把路径塞进已序列化的备份模板。"""
    return json.dumps(value)[1:-1]


def bytes_field(content: bytes) -> dict:
    """按可读性选择 text / hex 表示。"""
    try:
        return {"text": content.decode("utf-8")}
    except UnicodeDecodeError:
        return {"hex": content.hex()}


# --------------------------------------------------------------------------- #
# 环境变量与配置文件读写
# --------------------------------------------------------------------------- #

@contextmanager
def pinned_env(root: Path, extra: dict | None = None):
    """把 home / 缓存目录（以及用例指定的覆盖变量）钉进临时根。"""
    values = {
        "HOME": str(root),
        "USERPROFILE": str(root),
        "HOMEDRIVE": "",
        "HOMEPATH": "",
        "LOCALAPPDATA": str(root / "cache"),
        "APPDATA": str(root / "cache-roaming"),
        "XDG_CACHE_HOME": str(root / "xdg-cache"),
        "CLAUDE_CONFIG_DIR": "",
        "PI_CODING_AGENT_DIR": "",
        "CODEX_HOME": "",
    }
    if extra:
        values.update(extra)
    saved = {key: os.environ.get(key) for key in values}
    try:
        for key, value in values.items():
            if value == "":
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        yield
    finally:
        for key, old in saved.items():
            if old is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = old


def write_initial(workdir: Path, rel: str, spec: dict | None) -> None:
    if spec is None:
        return
    path = workdir.joinpath(*rel.split("/"))
    path.parent.mkdir(parents=True, exist_ok=True)
    if "hex" in spec:
        path.write_bytes(bytes.fromhex(spec["hex"]))
    else:
        # 用 bytes 写：语料里的 CRLF / BOM 必须原样落盘，不能被换行翻译改写。
        path.write_bytes(spec["text"].encode("utf-8"))


def read_file_text(path: Path) -> dict:
    if not path.exists():
        return {"missing": True}
    return bytes_field(path.read_bytes())


# --------------------------------------------------------------------------- #
# 配置夹具
# --------------------------------------------------------------------------- #

def config_alpha(reasoning_effort: str | None = "high") -> dict:
    """一份能通过 RouterConfig.from_dict 的 v4 配置。"""
    alpha: dict = {
        "aliases": ["alpha-alias"],
        "targets": [
            {"provider": "openai", "key": "k1", "upstream_model": "gpt-4o"},
            {"provider": "openai", "key": "disabled", "upstream_model": "gpt-4o-mini"},
        ],
    }
    if reasoning_effort is not None:
        alpha["reasoning_effort"] = reasoning_effort
    return {
        "config_version": 4,
        "host": "127.0.0.1",
        "port": 8123,
        "local_api_key": "amkr_corpus_local_key",
        "providers": {
            "openai": {
                "base_url": "https://api.openai.com",
                "keys": {
                    "k1": {"api_key": "s1", "enabled": True},
                    "disabled": {"api_key": "s2", "enabled": False},
                },
            }
        },
        "models": {
            "alpha": alpha,
            "beta": {
                "aliases": ["beta-alias", "alpha-alias-dupe"],
                "targets": [{"provider": "openai", "key": "k1", "upstream_model": "gpt-5"}],
            },
            "gamma": {
                "targets": [{"provider": "openai", "key": "disabled", "upstream_model": "gpt-3.5"}],
            },
        },
        "unified_model": {"default": {"primary": {"model": "alpha", "key": "k1"}}},
    }


def config_wildcard() -> dict:
    raw = config_alpha()
    raw["host"] = "0.0.0.0"
    raw["port"] = 7000
    return raw


def config_ipv6() -> dict:
    raw = config_alpha()
    raw["host"] = "::"
    raw["port"] = 7001
    return raw


def config_bracketed_ipv6() -> dict:
    raw = config_alpha()
    raw["host"] = "[::]"
    raw["port"] = 7002
    return raw


def config_plain_ipv6() -> dict:
    raw = config_alpha()
    raw["host"] = "fe80::1"
    raw["port"] = 7003
    return raw


def config_no_unified() -> dict:
    raw = config_alpha()
    del raw["unified_model"]
    return raw


def config_empty_key() -> dict:
    raw = config_alpha()
    raw["local_api_key"] = ""
    return raw


def config_no_reasoning() -> dict:
    return config_alpha(reasoning_effort=None)


CONFIGS = {
    "alpha": config_alpha,
    "wildcard": config_wildcard,
    "ipv6": config_ipv6,
    "bracketed_ipv6": config_bracketed_ipv6,
    "plain_ipv6": config_plain_ipv6,
    "no_unified": config_no_unified,
    "empty_key": config_empty_key,
    "no_reasoning": config_no_reasoning,
}


# --------------------------------------------------------------------------- #
# Codex TOML 输入夹具
# --------------------------------------------------------------------------- #

CODEX_SIMPLE = (
    'model = "gpt-5-codex"\n'
    'model_reasoning_effort = "high"\n'
    "approval_policy = \"on-request\"\n"
)

CODEX_COMMENTED = (
    "# Codex 全局配置，手工维护。\n"
    "# 第二行注释，含中文与 emoji 🙂\n"
    "\n"
    'model_provider = "openai"   # 行尾注释\n'
    'model = "gpt-5"\n'
    'approval_policy = "on-request"\n'
    "\n"
    "[model_providers.openai]\n"
    'name = "OpenAI"\n'
    'base_url = "https://api.openai.com/v1"\n'
    'wire_api = "responses"\n'
    "\n"
    "[profiles.deep]\n"
    'model = "gpt-5-codex"\n'
    'model_reasoning_effort = "high"\n'
)

CODEX_OPENAI_TABLE = (
    "# AMKR 之前写过的 provider，本次要就地更新\n"
    "[model_providers.OpenAI]\n"
    'name = "old-name"\n'
    'base_url = "https://old.example/v1"\n'
    "requires_openai_auth = false\n"
)

CODEX_TABLES_AFTER = (
    'model = "gpt-5"\n'
    "[model_providers.OpenAI]\n"
    'name = "x"\n'
    "\n"
    "[tui]\n"
    "notifications = true\n"
    "\n"
    "[history]\n"
    'persistence = "save-all"\n'
)

CODEX_NO_TRAILING_NEWLINE = 'model = "gpt-5"'

CODEX_CRLF = (
    "# comment\r\n"
    'model = "gpt-5"\r\n'
    "\r\n"
    "[model_providers.OpenAI]\r\n"
    'name = "x"\r\n'
)

CODEX_WHITESPACE_STYLE = (
    "model   =   'gpt-5'\n"
    "\n"
    "[model_providers.OpenAI]\n"
    "name='x'\n"
)

CODEX_COMMENTS_EVERYWHERE = (
    "# a\n"
    'model = "gpt-5"\n'
    "# b\n"
    "[model_providers.OpenAI]\n"
    "# c\n"
    'name = "x"\n'
    "# d\n"
)

CODEX_BLANK_LINES = "\n\n" + 'model = "gpt-5"\n' + "\n\n\n" + "[model_providers.OpenAI]\n" + 'name = "x"\n' + "\n\n"

CODEX_DUPLICATE_PARENT = (
    'model = "gpt-5"\n'
    "\n"
    "[profiles.deep]\n"
    'model = "gpt-5-codex"\n'
    "\n"
    "[profiles.other]\n"
    'model = "gpt-5-mini"\n'
    "\n"
    "[model_providers.openai]\n"
    'name = "OpenAI"\n'
)

CODEX_MCP_SERVERS = (
    "# MCP 服务器配置\n"
    "[mcp_servers.docs]\n"
    'command = "npx"\n'
    'args = ["-y", "mcp-docs"]\n'
    "\n"
    "[mcp_servers.docs.env]\n"
    'TOKEN = "secret"  # 别删这行\n'
    "\n"
    "[shell_environment_policy]\n"
    'inherit = "core"\n'
)

CODEX_ARRAY_VALUES = (
    "# 数组与多行数组都要原样保留\n"
    'notify = ["pythonw", "notify.py"]\n'
    "trusted = [\n"
    '  "/a",   # 第一项\n'
    '  "/b",\n'
    "]\n"
    "count = 3\n"
)

CODEX_QUOTED_TABLE = (
    'project = "x"\n'
    "\n"
    '[projects."/home/user/my project"]\n'
    'trust_level = "trusted"\n'
)

CODEX_MULTILINE_STRING = (
    'developer_instructions = """\n'
    "  保持简洁。\n"
    '  """\n'
    'model = "gpt-5"\n'
)

CODEX_DOTTED_KEY = 'model_providers.OpenAI.name = "dotted"\n'

CODEX_INLINE_TABLE = 'model_providers = { OpenAI = { name = "inline" } }\n'

CODEX_ARRAY_OF_TABLES = '[[model_providers.OpenAI]]\nname = "aot"\n'

CODEX_MISSING_SEPARATOR = "model_providers = [1, 2]\n"

CODEX_SCALAR_MODEL_PROVIDERS = 'model_providers = "nope"\n'

CODEX_SCALAR_OPENAI = "[model_providers]\nOpenAI = 1\n"

CODEX_INVALID = 'model = "unterminated\n'

CODEX_EMPTY = ""

CODEX_ONLY_COMMENTS = "# 只有注释\n# 第二行\n"

CODEX_TRAILING_WHITESPACE = 'model = "gpt-5"   \n\n[tui]\nnotifications = true\n'

CODEX_TAB_INDENT = '\tmodel = "gpt-5"\n\n[model_providers.openai]\n\tname = "OpenAI"\n'

CODEX_BOM = "\ufeff# bom 注释\n" 'model = "gpt-5"\n'

# 各种 TOML 字面量：用来暴露「参照实现是否会重排未修改的值」这类保真问题
# （例如 tomlkit 会不会把 0x1f 规范化成 31、把 1_000 规范化成 1000）。
CODEX_LITERALS = (
    "# 各种字面量都必须原样保留\n"
    "started_at = 1979-05-27T07:32:00Z\n"
    "hex = 0x1f\n"
    "underscored = 1_000_000\n"
    "float_exp = 1e-9\n"
    "float_plain = 3.14\n"
    "enabled = true\n"
    "tags = [\"a\", \"b\"]\n"
    "point = { x = 1, y = 2 }\n"
    '"quoted_中文_key" = "值 emoji 🙂"\n'
    'note = """多行\n文本"""\n'
    "literal = 'C:\\path\\x'\n"
    "\n"
    "[model_providers.OpenAI]\n"
    'name = "x"\n'
)


# --------------------------------------------------------------------------- #
# 用例装配
# --------------------------------------------------------------------------- #

def configure_case(
    name: str,
    agent: str,
    *,
    mode: str = AGENT_MODE_UNIFIED_MODEL,
    config: str = "alpha",
    initial: dict | None = None,
    extra_initial: dict | None = None,
    note: str = "",
    rollback: bool = True,
    error_match: str = "exact",
    divergence: str = "",
) -> dict:
    case = {
        "name": name,
        "op": "configure",
        "agent": agent,
        "mode": mode,
        "config": config,
        "target_rel": TARGET_REL[agent],
        "error_match": error_match,
    }
    if initial is not None:
        case["initial"] = initial
    if extra_initial is not None:
        case["extra_initial"] = extra_initial
    if note:
        case["note"] = note
    case["rollback"] = rollback
    if divergence:
        case["divergence"] = divergence
    return case


def run_configure(case: dict, root: Path) -> dict:
    name = case["name"]
    agent = case["agent"]
    workdir = root / name
    target = workdir.joinpath(*case["target_rel"].split("/"))
    backup = workdir / "backup" / f"{agent}.json"
    write_initial(workdir, case["target_rel"], case.get("initial"))
    for rel, spec in (case.get("extra_initial") or {}).items():
        write_initial(workdir, rel, spec)

    record = dict(case)
    record.pop("rollback", None)
    cfg = CONFIGS[case["config"]]()
    try:
        result = configure_agent(
            agent,
            RouterConfig.from_dict(cfg),
            mode=case["mode"],
            target_path=target,
            backup_path=backup,
        )
    except Exception as exc:  # noqa: BLE001
        record["want_error"] = str(exc)
        record["want_error_type"] = type(exc).__name__
        return record

    record["want_target"] = read_file_text(target)
    extras = {}
    for rel in (EXTRA_REL.get(agent) or {}):
        extras[rel] = read_file_text(workdir.joinpath(*rel.split("/")))
    if extras:
        record["want_extra"] = extras
    if backup.exists():
        record["want_backup"] = portable_text(backup.read_text(encoding="utf-8"), root)
    record["want_result"] = {
        "router_url": result.router_url,
        "extra_target_paths": [portable_path(p, root) for p in result.extra_target_paths],
        "restored": result.restored,
        "mode": result.mode,
    }
    status = get_agent_config_status(agent, target_path=target, backup_path=backup)
    record["want_status"] = {
        "target_path": portable_path(status.target_path, root),
        "backup_path": portable_path(status.backup_path, root),
        "backup_available": status.backup_available,
        "current_is_applied": status.current_is_applied,
        "mode": status.mode,
    }
    if case.get("rollback", True):
        rolled = rollback_agent(agent, target_path=target, backup_path=backup)
        record["want_rollback"] = {
            "router_url": rolled.router_url,
            "extra_target_paths": [portable_path(p, root) for p in rolled.extra_target_paths],
            "restored": rolled.restored,
            "mode": rolled.mode,
        }
        after = {case["target_rel"]: read_file_text(target)}
        for rel in (EXTRA_REL.get(agent) or {}):
            after[rel] = read_file_text(workdir.joinpath(*rel.split("/")))
        record["want_after_rollback"] = after
        record["want_backup_gone"] = not backup.exists()
    return record


def configure_cases() -> list[dict]:
    cases: list[dict] = []

    # ---------- Claude Code ----------
    cases.append(configure_case(
        "claude_empty", CLAUDE_CODE,
        note="目标文件不存在：从零建 settings.json",
    ))
    cases.append(configure_case(
        "claude_existing_env", CLAUDE_CODE,
        initial={"text": json.dumps({
            "env": {
                "ANTHROPIC_BASE_URL": "https://old.example",
                "MY_OWN_VAR": "keep-me",
                "ANTHROPIC_MODEL": "claude-opus-4",
            },
            "permissions": {"allow": ["Bash(ls:*)"]},
            "custom_unknown_key": {"nested": [1, 2, {"中文": "值"}]},
        }, indent=2, ensure_ascii=False) + "\n"},
        note="已有 env：既有键保位置换值，新键追加；未知字段必须保留",
    ))
    cases.append(configure_case(
        "claude_native_no_removal", CLAUDE_CODE, mode=AGENT_MODE_NATIVE,
        initial={"text": '{\n  "env": {\n    "ANTHROPIC_MODEL": "claude-opus-4"\n  }\n}\n'},
        note="native 模式但没有可继承的 unified 备份：不删 ANTHROPIC_MODEL",
    ))
    cases.append(configure_case(
        "claude_native_removes_overrides", CLAUDE_CODE, mode=AGENT_MODE_NATIVE,
        initial={"text": '{\n  "env": {\n    "ANTHROPIC_MODEL": "claude-opus-4"\n  }\n}\n'},
        note="先 apply unified 再 apply native 才能观察到删除，见 special_cases",
        rollback=False,
        error_match="any",
    ))
    cases.append(configure_case(
        "claude_env_not_object", CLAUDE_CODE,
        initial={"text": '{"env": []}\n'},
        note="env 必须是对象",
    ))
    cases.append(configure_case(
        "claude_root_not_object", CLAUDE_CODE,
        initial={"text": "[1, 2, 3]\n"},
        note="根节点必须是对象",
    ))
    cases.append(configure_case(
        "claude_invalid_json", CLAUDE_CODE,
        initial={"text": "{ not json\n"},
        note="非法 JSON：只断言错误前缀",
        error_match="prefix",
    ))
    cases.append(configure_case(
        "claude_invalid_utf8", CLAUDE_CODE,
        initial={"hex": b'\xff\xfe{\n  "env": {}\n}\n'.hex()},
        note="非法 UTF-8：只断言错误前缀",
        error_match="prefix",
    ))
    cases.append(configure_case(
        "claude_bom", CLAUDE_CODE,
        initial={"text": "\ufeff" + '{"env": {"K": "v"}}\n'},
        note="带 BOM 的 JSON：utf-8-sig 会剥掉 BOM",
    ))
    cases.append(configure_case(
        "claude_non_ascii", CLAUDE_CODE,
        initial={"text": '{"env": {"NOTE": "中文值 🙂"}, "路径": "带中文的键"}\n'},
        note="ensure_ascii=False：非 ASCII 必须原样输出",
    ))
    cases.append(configure_case(
        "claude_attribution_replaced", CLAUDE_CODE,
        initial={"text": json.dumps({
            "env": {"K": "v"},
            "attribution": {"commit": "user-signed", "pr": "user-pr", "extra": "lost"},
        }, indent=2, ensure_ascii=False) + "\n"},
        note="保留的怪癖：attribution 被整体替换，用户写在里面的其它字段会丢失",
    ))
    cases.append(configure_case(
        "claude_wildcard_host", CLAUDE_CODE, config="wildcard",
        note="host=0.0.0.0 要换成 127.0.0.1",
    ))
    cases.append(configure_case(
        "claude_ipv6_host", CLAUDE_CODE, config="ipv6",
        note="host=:: 要换成 127.0.0.1",
    ))
    cases.append(configure_case(
        "claude_bracketed_ipv6_host", CLAUDE_CODE, config="bracketed_ipv6",
    ))
    cases.append(configure_case(
        "claude_plain_ipv6_host", CLAUDE_CODE, config="plain_ipv6",
        note="裸 IPv6 字面量要加方括号",
    ))
    cases.append(configure_case(
        "claude_missing_unified", CLAUDE_CODE, config="no_unified",
        note="unified-model 模式但没配 unified_model",
    ))
    cases.append(configure_case(
        "claude_empty_local_key", CLAUDE_CODE, config="empty_key",
    ))
    cases.append(configure_case(
        "claude_pi_native_rejected", PI_AGENT, mode=AGENT_MODE_NATIVE,
        note="Pi agent 只支持 unified-model",
        rollback=False,
    ))

    # ---------- Pi agent ----------
    cases.append(configure_case(
        "pi_empty", PI_AGENT,
    ))
    cases.append(configure_case(
        "pi_existing_providers", PI_AGENT,
        initial={"text": json.dumps({
            "providers": {
                "other": {"baseUrl": "https://x.example", "api": "openai-completions"},
                "amkr": {"baseUrl": "https://stale.example", "apiKey": "old"},
            },
            "theme": "dark",
        }, indent=2, ensure_ascii=False) + "\n"},
        note="providers.amkr 整体覆盖，其它 provider 与未知字段保留",
    ))
    cases.append(configure_case(
        "pi_providers_not_object", PI_AGENT,
        initial={"text": '{"providers": "x"}\n'},
    ))
    cases.append(configure_case(
        "pi_root_not_object", PI_AGENT,
        initial={"text": "[]\n"},
    ))
    cases.append(configure_case(
        "pi_disabled_models_excluded", PI_AGENT,
        note="gamma 的 key 全部禁用，不应出现在模型清单里",
    ))
    cases.append(configure_case(
        "pi_no_reasoning", PI_AGENT, config="no_reasoning",
        note="没有 reasoning_effort 时 Codex 回落 xhigh（本用例也在 Pi 上跑一遍）",
        rollback=False,
    ))

    # ---------- Codex：格式保真的核心 ----------
    codex_toml_cases = [
        ("codex_empty", CODEX_EMPTY, "空文件：从零建表"),
        ("codex_only_comments", CODEX_ONLY_COMMENTS, "只有注释：注释必须全留"),
        ("codex_simple", CODEX_SIMPLE, "无注释的最小配置"),
        ("codex_commented", CODEX_COMMENTED, "核心用例：中文注释 + 行尾注释 + 多表"),
        ("codex_openai_table", CODEX_OPENAI_TABLE, "已存在 [model_providers.OpenAI]：就地更新 + 追加 wire_api"),
        ("codex_tables_after", CODEX_TABLES_AFTER, "AMKR 的表后面还有别的表"),
        ("codex_no_trailing_newline", CODEX_NO_TRAILING_NEWLINE, "文件末尾没有换行"),
        ("codex_crlf", CODEX_CRLF, "CRLF 行尾必须原样保留"),
        ("codex_whitespace_style", CODEX_WHITESPACE_STYLE, "键与 = 两侧的空白原样保留，值重新渲染为双引号"),
        ("codex_comments_everywhere", CODEX_COMMENTS_EVERYWHERE, "注释夹在键与表头之间"),
        ("codex_blank_lines", CODEX_BLANK_LINES, "连续空行"),
        ("codex_trailing_whitespace", CODEX_TRAILING_WHITESPACE, "键值行尾有多余空格"),
        ("codex_tab_indent", CODEX_TAB_INDENT, "表体用制表符缩进"),
        ("codex_bom", CODEX_BOM, "带 BOM 的 TOML：utf-8-sig 剥掉 BOM"),
        ("codex_duplicate_parent", CODEX_DUPLICATE_PARENT, "同一父表出现多次：必须合并到同一张隐式父表"),
        ("codex_mcp_servers", CODEX_MCP_SERVERS, "真实形状：mcp_servers / shell_environment_policy"),
        ("codex_array_values", CODEX_ARRAY_VALUES, "数组值（含多行数组与数组内注释）"),
        ("codex_quoted_table", CODEX_QUOTED_TABLE, "带引号与空格的表名"),
        ("codex_multiline_string", CODEX_MULTILINE_STRING, "多行字符串值"),
        ("codex_literals", CODEX_LITERALS, "各种 TOML 字面量：未修改的值必须逐字节保留"),
    ]
    for name, text, note in codex_toml_cases:
        cases.append(configure_case(name, CODEX, initial={"text": text}, note=note))

    cases.append(configure_case(
        "codex_native_on_amkr_backup", CODEX, mode=AGENT_MODE_NATIVE,
        initial={"text": CODEX_COMMENTED},
        note="unified → native 的第二次 apply 会删掉 model/review_model/model_reasoning_effort",
        rollback=False,
    ))
    cases.append(configure_case(
        "codex_native_fresh", CODEX, mode=AGENT_MODE_NATIVE,
        initial={"text": CODEX_COMMENTED},
        note="native 但没有可继承的 unified 备份：保留用户自己的 model",
    ))
    cases.append(configure_case(
        "codex_no_reasoning_effort", CODEX, config="no_reasoning",
        initial={"text": CODEX_SIMPLE},
        note="模型没配 reasoning_effort：写 xhigh",
    ))
    cases.append(configure_case(
        "codex_existing_auth", CODEX,
        initial={"text": CODEX_SIMPLE},
        extra_initial={".codex/auth.json": {"text": '{\n  "OPENAI_API_KEY": "old",\n  "tokens": {"x": 1}\n}\n'}},
        note="auth.json 已存在：只换 OPENAI_API_KEY",
    ))
    cases.append(configure_case(
        "codex_invalid_toml", CODEX,
        initial={"text": CODEX_INVALID},
        note="非法 TOML：只断言错误前缀",
        error_match="prefix",
        rollback=False,
    ))
    # go-toml/v2 的 unstable parser 是语法解析器，不查「重复键 / 重复表 / 表撞键值」
    # 这三条 TOML 语义约束（实测：tomlkit 拒绝、它接受）。Go 侧自己拦住了它们，
    # 因此两侧都报错，只比对前缀。见 doc.go 的 D8。
    for name, text, note in (
        ("codex_duplicate_key", 'model = "a"\nmodel = "b"\n', "同一张表里重复键"),
        ("codex_duplicate_table",
         '[model_providers.OpenAI]\nname = "x"\n[model_providers.OpenAI]\nname = "y"\n',
         "同一个显式表头写两次"),
        ("codex_table_over_value",
         '[profiles]\ndebug = 1\n[profiles.debug]\nx = 1\n',
         "表头撞上已有的键值对"),
    ):
        cases.append(configure_case(
            name, CODEX, initial={"text": text}, note=note,
            rollback=False, error_match="prefix",
        ))

    # 形状不被支持 / 与 Python 行为不同：只记录期望错误
    cases.append(configure_case(
        "codex_dotted_key", CODEX, initial={"text": CODEX_DOTTED_KEY},
        note="点号键：Go 侧不支持，Python 侧成功（差异见 doc.go D6）",
        error_match="any",
        divergence="D6",
        rollback=False,
    ))
    cases.append(configure_case(
        "codex_inline_table", CODEX, initial={"text": CODEX_INLINE_TABLE},
        note="行内表：Go 侧不支持，Python 侧成功（差异见 doc.go D6）",
        error_match="any",
        divergence="D6",
        rollback=False,
    ))
    cases.append(configure_case(
        "codex_array_of_tables", CODEX, initial={"text": CODEX_ARRAY_OF_TABLES},
        note="数组表：Python 抛未包装的 TypeError，Go 抛 ConfigError",
        error_match="any",
        divergence="D6",
        rollback=False,
    ))
    cases.append(configure_case(
        "codex_array_model_providers", CODEX, initial={"text": CODEX_MISSING_SEPARATOR},
        note="model_providers 是数组：同上",
        error_match="any",
        divergence="D6",
        rollback=False,
    ))
    cases.append(configure_case(
        "codex_scalar_model_providers", CODEX, initial={"text": CODEX_SCALAR_MODEL_PROVIDERS},
        note="model_providers 是标量：两侧都报 AgentConfigError，措辞一致",
        rollback=False,
    ))
    cases.append(configure_case(
        "codex_scalar_openai", CODEX, initial={"text": CODEX_SCALAR_OPENAI},
        note="model_providers.OpenAI 是标量：两侧都报 AgentConfigError，措辞一致",
        rollback=False,
    ))

    return cases


def rollback_cases() -> list[dict]:
    """独立回退用例（无备份 / 路径不符 / 损坏 / 旧版备份）。"""
    return [
        {"name": "rollback_no_backup", "op": "rollback", "agent": CODEX,
         "target_rel": TARGET_REL[CODEX], "note": "没有备份文件"},
        {"name": "rollback_agent_mismatch", "op": "rollback", "agent": CLAUDE_CODE,
         "target_rel": TARGET_REL[CLAUDE_CODE],
         "backup_text": json.dumps({"version": 2, "agent": CODEX}, indent=2) + "\n",
         "note": "备份属于另一个 Agent"},
        {"name": "rollback_missing_target_path", "op": "rollback", "agent": CODEX,
         "target_rel": TARGET_REL[CODEX],
         "backup_text": json.dumps({"version": 2, "agent": CODEX}, indent=2) + "\n",
         "note": "备份缺 target_path"},
        {"name": "rollback_corrupt_base64", "op": "rollback", "agent": CODEX,
         "target_rel": TARGET_REL[CODEX],
         "backup_text": json.dumps({
             "version": 2, "agent": CODEX, "mode": AGENT_MODE_UNIFIED_MODEL,
             "target_path": "@TARGET@", "original_exists": True,
             "original_content": "!!!not base64!!!",
             "applied_sha256": "0" * 64,
         }, indent=2) + "\n",
         "note": "原内容 base64 损坏"},
        {"name": "rollback_missing_extra_target_path", "op": "rollback", "agent": CODEX,
         "target_rel": TARGET_REL[CODEX],
         "backup_text": json.dumps({
             "version": 2, "agent": CODEX, "mode": AGENT_MODE_UNIFIED_MODEL,
             "target_path": "@TARGET@", "original_exists": True,
             "original_content": base64.b64encode(b'x = 1\n').decode("ascii"),
             "applied_sha256": "0" * 64,
             "extra_targets": [{"applied_sha256": "0" * 64}],
         }, indent=2) + "\n",
         "note": "附加目标缺 target_path"},
        {"name": "rollback_target_mismatch", "op": "rollback", "agent": CODEX,
         "target_rel": TARGET_REL[CODEX],
         "backup_text": json.dumps({
             "version": 2, "agent": CODEX, "mode": AGENT_MODE_NATIVE,
             "target_path": "@OTHER@", "original_exists": True,
             "original_content": base64.b64encode(b'x = 1\n').decode("ascii"),
             "applied_sha256": "0" * 64,
         }, indent=2) + "\n",
         "note": "备份记录的路径与当前目标不一致"},
        {"name": "rollback_v1_backup", "op": "rollback", "agent": CODEX,
         "target_rel": TARGET_REL[CODEX],
         "backup_text": json.dumps({
             "version": 1, "agent": CODEX,
             "target_path": "@TARGET@", "original_exists": False,
             "original_content": "",
             "applied_sha256": "0" * 64,
         }, indent=2) + "\n",
         "note": "v1 备份没有 mode：按 unified-model 处理，且 original_exists=false 时删文件"},
        {"name": "rollback_not_json", "op": "rollback", "agent": CODEX,
         "target_rel": TARGET_REL[CODEX], "backup_text": "{ 坏掉的 json\n",
         "note": "备份不是合法 JSON：等同于没有备份"},
    ]


def run_rollback(case: dict, root: Path) -> dict:
    name = case["name"]
    agent = case["agent"]
    workdir = root / name
    target = workdir.joinpath(*case["target_rel"].split("/"))
    backup = workdir / "backup" / f"{agent}.json"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes('# 当前内容\nmodel = "current"\n'.encode("utf-8"))
    backup.parent.mkdir(parents=True, exist_ok=True)
    text = case.get("backup_text")
    if text is not None:
        # 占位符替换必须按 JSON 字符串规则转义：Windows 路径里的反斜杠直接塞进
        # 已序列化的 JSON 会产出非法转义（"C:\Users..." 的 \U），json.loads 会
        # 失败，用例就变成在测「坏 JSON」而不是原本想测的分支。
        text = text.replace("@TARGET@", json_escape(str(target)))
        text = text.replace("@OTHER@", json_escape(str(workdir / "elsewhere.toml")))
        backup.write_text(text, encoding="utf-8", newline="\n")

    record = {key: value for key, value in case.items() if key != "backup_text"}
    record.setdefault("error_match", "exact")
    # 记下**模板**（@TARGET@ / @OTHER@ 占位符未替换）：Go 测试要用自己的临时
    # 路径做替换，写死绝对路径会让语料与平台绑定。
    if case.get("backup_text") is not None:
        record["backup_text"] = case["backup_text"]
    try:
        result = rollback_agent(agent, target_path=target, backup_path=backup)
    except Exception as exc:  # noqa: BLE001
        record["want_error"] = str(exc)
        record["want_error_type"] = type(exc).__name__
        return record
    record["want_result"] = {
        "router_url": result.router_url,
        "restored": result.restored,
        "mode": result.mode,
    }
    record["want_target"] = read_file_text(target)
    record["want_backup_gone"] = not backup.exists()
    return record


def path_cases(root: Path) -> list[dict]:
    cases: list[dict] = []

    def add(name: str, func, env: dict | None = None, inputs: list | None = None,
            note: str = "") -> None:
        record: dict = {"name": name}
        if env:
            record["env"] = {key: portable_path(value, root) for key, value in env.items()}
        if inputs:
            record["inputs"] = [portable_path(value, root) for value in inputs]
        if note:
            record["note"] = note
        try:
            with pinned_env(root, env):
                value = func()
        except Exception as exc:  # noqa: BLE001
            record["want_error"] = str(exc)
            record["want_error_type"] = type(exc).__name__
        else:
            record["want"] = portable_path(value, root)
        cases.append(record)

    add("path_claude_default", lambda: agent_config_path(CLAUDE_CODE))
    add("path_claude_env", lambda: agent_config_path(CLAUDE_CODE),
        env={"CLAUDE_CONFIG_DIR": str(root / "claude-home")})
    add("path_claude_env_tilde", lambda: agent_config_path(CLAUDE_CODE),
        env={"CLAUDE_CONFIG_DIR": "~/claude-home"})
    add("path_pi_default", lambda: agent_config_path(PI_AGENT))
    add("path_pi_env", lambda: agent_config_path(PI_AGENT),
        env={"PI_CODING_AGENT_DIR": str(root / "pi-home")})
    add("path_codex_default", lambda: agent_config_path(CODEX))
    add("path_codex_env", lambda: agent_config_path(CODEX),
        env={"CODEX_HOME": str(root / "codex-home")})
    add("path_backup_default", lambda: agent_backup_path(CLAUDE_CODE))
    add("path_backup_dir", lambda: agent_backup_path(CODEX, root / "explicit-backups"))
    add("path_unknown_agent", lambda: agent_config_path("nope"))
    add("path_unknown_agent_backup", lambda: agent_backup_path("nope"))

    dotdot = str(Path(root) / "a" / ".." / "b" / "c")
    nested = str(Path(root) / "x" / "y" / ".." / ".." / "z.toml")
    add("path_resolve_dotdot", lambda: str(_resolved_path(Path(dotdot))), inputs=[dotdot])
    add("path_resolve_dotdot_nested", lambda: str(_resolved_path(Path(nested))), inputs=[nested])
    add("path_resolve_tilde", lambda: str(_resolved_path(Path("~/nested/./file.toml"))),
        inputs=["~/nested/./file.toml"])
    return cases


def special_cases(root: Path) -> list[dict]:
    """需要连续两次调用才能观察到的行为（native 模式删除 unified 覆盖）。"""
    results: list[dict] = []
    for agent, name, target_rel in (
        (CLAUDE_CODE, "claude_unified_then_native", TARGET_REL[CLAUDE_CODE]),
        (CODEX, "codex_unified_then_native", TARGET_REL[CODEX]),
    ):
        workdir = root / name
        target = workdir.joinpath(*target_rel.split("/"))
        backup = workdir / "backup" / f"{agent}.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        if agent == CODEX:
            initial = CODEX_COMMENTED
        else:
            initial = json.dumps({
                "env": {"ANTHROPIC_MODEL": "claude-opus-4", "MY_VAR": "keep"},
                "permissions": {"allow": ["Bash(ls:*)"]},
            }, indent=2, ensure_ascii=False) + "\n"
        target.write_bytes(initial.encode("utf-8"))
        record = {
            "name": name,
            "op": "configure_twice",
            "agent": agent,
            "mode": AGENT_MODE_UNIFIED_MODEL,
            "config": "alpha",
            "target_rel": target_rel,
            "second_mode": AGENT_MODE_NATIVE,
            "initial": {"text": initial},
        }
        cfg = RouterConfig.from_dict(CONFIGS["alpha"]())
        configure_agent(agent, cfg, mode=AGENT_MODE_UNIFIED_MODEL,
                        target_path=target, backup_path=backup)
        record["after_first"] = read_file_text(target)
        result = configure_agent(agent, cfg, mode=AGENT_MODE_NATIVE,
                                 target_path=target, backup_path=backup)
        record["want_target"] = read_file_text(target)
        record["want_result"] = {
            "router_url": result.router_url,
            "extra_target_paths": [portable_path(p, root) for p in result.extra_target_paths],
            "restored": result.restored,
            "mode": result.mode,
        }
        status = get_agent_config_status(agent, target_path=target, backup_path=backup)
        record["want_status"] = {
            "target_path": portable_path(status.target_path, root),
            "backup_path": portable_path(status.backup_path, root),
            "backup_available": status.backup_available,
            "current_is_applied": status.current_is_applied,
            "mode": status.mode,
        }
        # 备份里的原始快照必须仍是「第一次 apply 之前」的内容
        record["want_backup"] = portable_text(backup.read_text(encoding="utf-8"), root)
        results.append(record)
    return results


def status_cases(root: Path) -> list[dict]:
    """状态判定：文件被第三方改过之后 current_is_applied 必须变 false。"""
    results: list[dict] = []

    def build(name: str, mutate) -> None:
        workdir = root / name
        target = workdir.joinpath(*TARGET_REL[CODEX].split("/"))
        backup = workdir / "backup" / f"{CODEX}.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(CODEX_COMMENTED.encode("utf-8"))
        before = get_agent_config_status(CODEX, target_path=target, backup_path=backup)
        configure_agent(CODEX, RouterConfig.from_dict(CONFIGS["alpha"]()),
                        mode=AGENT_MODE_UNIFIED_MODEL, target_path=target, backup_path=backup)
        mutate(workdir, target)
        status = get_agent_config_status(CODEX, target_path=target, backup_path=backup)
        results.append({
            "name": name,
            "op": "status",
            "agent": CODEX,
            "mode": AGENT_MODE_UNIFIED_MODEL,
            "config": "alpha",
            "target_rel": TARGET_REL[CODEX],
            "initial": {"text": CODEX_COMMENTED},
            "before_backup_available": before.backup_available,
            "before_current_is_applied": before.current_is_applied,
            "want_status": {
                "backup_available": status.backup_available,
                "current_is_applied": status.current_is_applied,
                "mode": status.mode,
            },
        })

    build("status_fresh", lambda workdir, target: None)
    build("status_target_edited", lambda workdir, target: target.write_bytes(
        target.read_bytes() + "# 用户又改了一行\n".encode("utf-8")))
    build("status_target_deleted", lambda workdir, target: target.unlink())
    build("status_auth_edited", lambda workdir, target: workdir.joinpath(
        ".codex", "auth.json").write_bytes(b'{"OPENAI_API_KEY":"tampered"}\n'))
    build("status_auth_deleted", lambda workdir, target: workdir.joinpath(
        ".codex", "auth.json").unlink())
    build("status_backup_deleted", lambda workdir, target: workdir.joinpath(
        "backup", f"{CODEX}.json").unlink())
    return results


# --------------------------------------------------------------------------- #
# 组装
# --------------------------------------------------------------------------- #

def build_corpus() -> dict:
    root = Path(tempfile.mkdtemp(prefix="amkr-agentconfig-corpus-")).resolve()
    try:
        with pinned_env(root):
            # 防线：路径推导必须完全落在临时目录里，否则立刻失败而不是悄悄
            # 写进开发者真实的用户配置目录。
            assert Path.home() == root, Path.home()
            for agent in (CLAUDE_CODE, CODEX, PI_AGENT):
                assert str(agent_config_path(agent)).startswith(str(root)), agent_config_path(agent)
            assert str(agent_backup_path(CLAUDE_CODE)).startswith(str(root)), agent_backup_path(CLAUDE_CODE)
            paths = path_cases(root)
            configure = [run_configure(case, root) for case in configure_cases()]
            configure.extend(special_cases(root))
            configure.extend(status_cases(root))
            rollbacks = [run_rollback(case, root) for case in rollback_cases()]
            env_variants = env_path_rechecks(root)
        return {
            "configs": {name: factory() for name, factory in CONFIGS.items()},
            "paths": paths,
            "configure": configure,
            "rollback": rollbacks,
            "env": env_variants,
        }
    finally:
        shutil.rmtree(root, ignore_errors=True)


def env_path_rechecks(root: Path) -> list[dict]:
    """环境变量覆盖下 configure_agent 的落点（验证 Go 侧同样读这些变量）。"""
    results: list[dict] = []
    for name, agent, env_name, rel in (
        ("env_claude_dir", CLAUDE_CODE, "CLAUDE_CONFIG_DIR", ".claude-env/settings.json"),
        ("env_codex_home", CODEX, "CODEX_HOME", ".codex-env/config.toml"),
        ("env_pi_dir", PI_AGENT, "PI_CODING_AGENT_DIR", ".pi-env/models.json"),
    ):
        env_dir = root / rel.split("/")[0]
        shutil.rmtree(env_dir, ignore_errors=True)
        workdir = root / name
        backup = workdir / "backup" / f"{agent}.json"
        target = root / rel
        cfg = RouterConfig.from_dict(CONFIGS["alpha"]())
        with pinned_env(root, {env_name: str(env_dir)}):
            result = configure_agent(agent, cfg, mode=AGENT_MODE_UNIFIED_MODEL,
                                     backup_path=backup)
        record = {
            "name": name,
            "op": "env",
            "agent": agent,
            "env": {env_name: portable_path(str(env_dir), root)},
            "want_target_path": portable_path(result.target_path, root),
            "want_target": read_file_text(target),
        }
        status = get_agent_config_status(agent, target_path=target, backup_path=backup)
        record["want_status_available"] = status.backup_available
        results.append(record)
    return results


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=1) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    path = args.output_dir / "agentconfig_corpus.json"
    content = render(build_corpus())

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
    print(f"已写入语料: {path} ({len(content.splitlines())} 行)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
