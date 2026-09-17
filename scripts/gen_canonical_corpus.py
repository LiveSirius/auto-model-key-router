#!/usr/bin/env python3
"""生成 canonical JSON 对拍语料（Python 侧为参照实现）。

Go 侧 ``internal/canonical`` 必须逐字节复现下列调用：

    json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))

这是 ``config_revision``（management_api.py:1187）与 Key 粘滞哈希
``_cache_affinity_key``（proxy_handler.py:348）共用的序列化形式，任何偏差都会
让两者静默失效。因此本脚本用 Python 的真实实现算出期望值并落盘为 JSONL 语料，
由 Go 测试逐条断言；不依赖测试时存在 Python 解释器。

语料有意保留「整数字面量」与「浮点字面量」的区别：Python 的 json.loads 把它
们解析成 int 与 float，json.dumps 分别输出 ``60`` 与 ``60.0``。Go 必须用
json.Decoder.UseNumber() 保留该区别，否则无法复现。

用法::

    python scripts/gen_canonical_corpus.py           # 写入语料
    python scripts/gen_canonical_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import json
import math
import random
import struct
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_OUTPUT = REPO_ROOT / "internal" / "canonical" / "testdata" / "corpus.jsonl"
DEFAULT_INDENT_OUTPUT = (
    REPO_ROOT / "internal" / "canonical" / "testdata" / "corpus_indent.jsonl"
)
SEED = 20260917


def canonical(obj: object) -> str:
    """Go 侧必须逐字节复现的目标实现。"""
    return json.dumps(
        obj, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    )


def canonical_indent(obj: object, indent: int = 2) -> str:
    """配置文件落盘形式（config.py:291）。

    与 canonical 有两个区别：**不排序**（字段顺序是用户可见的），且键值分隔符
    为 ``": "``，数组与对象的每一层都换行缩进。
    """
    return json.dumps(obj, indent=indent, ensure_ascii=False)


def real_config() -> dict:
    """贴近真实 router-config.json 的形状。

    重点是 tasks 里的采样参数是**浮点**（temperature/top_p），而 host/port/
    request_timeout 这类是**整数**——两者在 canonical 输出里必须保持区别。
    """
    return {
        "config_version": 4,
        "host": "127.0.0.1",
        "port": 8000,
        "default_base_url": "https://api.openai.com",
        "upstream_routes": {},
        "request_timeout": 60,
        "stream_first_byte_timeout": 60,
        "stream_idle_timeout": 60,
        "max_retries": 2,
        "key_failure_threshold": 2,
        "key_cooldown_seconds": 60,
        "endpoint_capabilities_path": "C:\\Users\\tester\\AppData\\Local\\AutoModelKeyRouter\\endpoint-capabilities.json",
        "metrics_db_path": "/home/tester/.cache/auto-model-key-router/metrics.sqlite3",
        "log_file_path": "/home/tester/.cache/auto-model-key-router/server.log",
        "local_api_key": "amkr_local_key_for_tests",
        "webui_enabled": False,
        "ops_enabled": True,
        "providers": {
            "openai": {
                "base_url": "https://api.openai.com",
                "routes": {
                    "openai": "v1/chat/completions",
                    "responses": "v1/responses",
                    "images": "v1/images/generations",
                    "embeddings": "v1/embeddings",
                },
                "keys": {
                    "main": {
                        "api_key": "sk-first",
                        "capabilities": {
                            "models": ["gpt-4o-mini", "text-embedding-3-small"],
                            "route_status": {"openai": "ok", "anthropic": "ok"},
                            "errors": {},
                            "checked_at": "2026-01-01T00:00:00+00:00",
                        },
                    },
                    "backup": {"api_key": "sk-second", "allow_visitor": True},
                },
            },
            "tokenplan": {
                "base_url": "https://example.com/tokenplan",
                "routes": {"anthropic": "anthropic/"},
                "keys": {"mimo": {"api_key": "sk-third"}},
            },
        },
        "models": {
            "gpt-4o-mini": {
                "aliases": ["fast-mini"],
                "routing_mode": "round_robin",
                "reasoning_effort": "medium",
                "targets": [
                    {"provider": "openai", "key": "main", "upstream_model": "gpt-4o-mini"},
                    {"provider": "tokenplan", "key": "mimo", "upstream_model": "gpt-4o-mini"},
                ],
            },
            "text-embedding-3-small": {
                "targets": [
                    {
                        "provider": "openai",
                        "key": "main",
                        "upstream_model": "text-embedding-3-small",
                    }
                ]
            },
        },
        "unified_model": {
            "default": {"primary": {"model": "gpt-4o-mini", "key": None}},
            "embeddings": {"primary": {"model": "text-embedding-3-small", "key": None}},
        },
        "tasks": {
            "TASK_000001": {
                "model": "gpt-4o-mini",
                "fallback_model": None,
                "params": {"temperature": 0.2, "top_p": 0.9},
            }
        },
    }


def build_cases() -> list[tuple[str, str]]:
    """返回 (名称, 原始 JSON 文本) 列表。

    原始文本而非 Python 对象：只有保留字面量才能覆盖 int/float 与转义差异。
    """
    cases: list[tuple[str, str]] = []

    def add_obj(name: str, obj: object) -> None:
        cases.append((name, json.dumps(obj, ensure_ascii=False)))

    def add_raw(name: str, raw: str) -> None:
        cases.append((name, raw))

    # ---- 真实配置形状 ----
    add_obj("real_config", real_config())
    add_obj("empty_object", {})
    add_obj("empty_array", [])
    add_obj("nested_empty", {"a": {}, "b": [], "c": None})

    # ---- 整数 / 浮点字面量区别 ----
    add_raw("int_vs_float_scalar", "60")
    add_raw("float_with_dot", "60.0")
    add_raw("float_exp_notation", "6e1")
    add_raw("negative_zero_float", "-0.0")
    add_raw("negative_zero_int", "-0")
    add_obj("int_vs_float_object", {"timeout": 60, "temperature": 60.0})
    add_obj("big_int_2_70", {"n": 2**70})
    add_obj("big_int_negative", {"n": -(2**70)})
    add_obj("big_int_10_30", {"n": 10**30})
    add_raw("big_int_literal", '{"n":1000000000000000000000000000000}')
    add_obj("int_list", {"ports": [1, 80, 8000, 65535]})

    # ---- 浮点格式：固定与科学计数法的切换边界 ----
    for exp in range(-12, 22):
        add_obj(f"pow10_{exp}", {"v": float(10**exp)} if exp >= 0 else {"v": 10.0**exp})
    add_obj(
        "float_boundaries",
        {
            "zero": 0.0,
            "neg_zero": -0.0,
            "one": 1.0,
            "hundred": 100.0,
            "e15": 1e15,
            "e16": 1e16,
            "e17": 1e17,
            "e21": 1e21,
            "e22": 1e22,
            "small_e4": 1e-4,
            "small_e5": 1e-5,
            "small_e6": 1e-6,
            "small_e7": 1e-7,
            "pi": 3.141592653589793,
            "sum_0_1_0_2": 0.1 + 0.2,
            "max": sys.float_info.max,
            "min_subnormal": 5e-324,
            "min_normal": sys.float_info.min,
        },
    )
    add_obj("float_typical_params", {"temperature": 0.2, "top_p": 0.9, "seed": 42})

    # ---- 非有限值：Python 默认允许，输出 Infinity / -Infinity / NaN ----
    add_obj("nan", {"v": float("nan")})
    add_obj("inf", {"v": float("inf")})
    add_obj("neg_inf", {"v": float("-inf")})
    add_raw("infinity_literal", '{"v":Infinity,"w":-Infinity,"x":NaN}')

    # ---- 字符串转义：ensure_ascii=False 下只转义必要字符 ----
    add_obj(
        "string_escapes",
        {
            "quote": 'he said "hi"',
            "backslash": "C:\\path\\to\\file",
            "slashes": "a/b/c",
            "newline": "line1\nline2",
            "tab": "a\tb",
            "cr": "a\rb",
            "backspace": "a\bb",
            "formfeed": "a\fb",
            "ctrl_0x00": "\x00",
            "ctrl_0x01": "\x01",
            "ctrl_0x1f": "\x1f",
            "del_0x7f": "\x7f",
            "html_chars": "<script>&\"'</script>",
        },
    )
    add_obj(
        "unicode_keep_raw",
        {
            "chinese": "中文键值",
            "accented": "café naïve",
            "emoji": "🙂🚀",
            "line_sep": "a\u2028b",
            "para_sep": "a\u2029b",
            "zero_width": "a\u200bb",
            "nbsp": "a\u00a0b",
            "bom": "\ufeff",
            "cjk_ext": "𠀀𠀁",
            "rtl": "مرحبا",
        },
    )
    add_raw("raw_unicode_escapes", '{"k":"\\u4e2d\\u6587","z":"\\ud83d\\ude00"}')
    add_raw("raw_surrogate_pair_astral", '{"emoji":"\\ud83d\\ude00"}')
    add_raw("raw_escape_sequence", '{"s":"a\\/b\\u2028c\\td"}')

    # ---- 键排序：按 Unicode 码点，与字节序一致 ----
    add_obj(
        "key_sorting",
        {"b": 1, "a": 2, "Z": 3, "_": 4, "1": 5, "": 6, "aa": 7, "aA": 8, "é": 9, "中": 10},
    )
    add_obj(
        "key_sorting_nested",
        {
            "b": {"z": 0, "y": 1, "x": {"n": None, "m": True}},
            "a": [1, 2, {"b": 1, "a": 2}],
            "c": {"n": None, "t": True, "f": False},
        },
    )
    # 长键与超 ASCII 键，验证排序不依赖 locale
    add_obj("key_sorting_long", {"k" * 50: 1, "k" * 49: 2, "z" * 60: 3})

    # ---- 深层嵌套与混合类型 ----
    add_obj(
        "deep_nesting",
        {"l1": {"l2": {"l3": {"l4": {"l5": {"l6": [1, "two", 3.0, None, True]}}}}}},
    )
    add_obj(
        "mixed_array",
        [None, True, False, 0, -1, 1.5, "", "s", [], {}, {"a": [{"b": 1.0}]}],
    )
    add_obj("array_of_objects", [{"id": i, "ratio": i / 4} for i in range(8)])
    add_obj("duplicate_after_sort", {"a": [3, 1, 2], "b": {"d": 4, "c": 3}})

    # ---- 对抗性：近似重复值需保持不同序列化 ----
    add_obj(
        "near_duplicates",
        {
            "i1": 1,
            "f1": 1.0,
            "e1": 1e0,
            "s1": "1",
            "t": True,
            "s_true": "true",
            "n": None,
            "s_null": "null",
        },
    )
    add_obj("float_precision_roundtrip", {"v": 0.30000000000000004, "w": 1.7976931348623157e308})
    add_obj("trailing_zeros", {"a": 1.50, "b": 1.500, "c": 150e-2})

    # ---- 随机浮点：覆盖最短往返表示的各类指数 ----
    rnd = random.Random(SEED)
    rand_floats: dict[str, float] = {}
    attempts = 0
    while len(rand_floats) < 500 and attempts < 5000:
        attempts += 1
        bits = rnd.getrandbits(64)
        value = struct.unpack("<d", struct.pack("<Q", bits))[0]
        if math.isnan(value) or math.isinf(value):
            continue
        rand_floats[f"f{len(rand_floats)}"] = value
    add_obj("random_float_bits", rand_floats)

    # 随机十进制小数（有限位数），更贴近手写配置
    rand_decimals: dict[str, float] = {}
    for index in range(200):
        digits = rnd.randint(1, 17)
        scale = rnd.randint(-10, 10)
        mantissa = rnd.randint(1, 10**digits - 1)
        rand_decimals[f"d{index}"] = float(f"{mantissa}e{scale}")
    add_obj("random_decimals", rand_decimals)

    # 随机整数（含超出 float64 精确范围的大整数）
    rand_ints: dict[str, int] = {}
    for index in range(120):
        width = rnd.choice([1, 3, 9, 15, 16, 17, 19, 25, 40])
        value = rnd.randint(0, 10**width - 1)
        if rnd.random() < 0.5:
            value = -value
        rand_ints[f"i{index}"] = value
    add_obj("random_ints", rand_ints)

    # 随机字符串：混合 ASCII、控制字符、多字节
    alphabet = ["a", "Z", "0", " ", "\n", "\t", '"', "\\", "\x00", "\x1f", "中", "é", "🙂", "\u2028"]
    rand_strings: dict[str, str] = {}
    for index in range(120):
        length = rnd.randint(0, 8)
        rand_strings[f"s{index}"] = "".join(rnd.choice(alphabet) for _ in range(length))
    add_obj("random_strings", rand_strings)

    # 随机键排序：多字节键与 ASCII 键混排
    rand_keys: dict[str, int] = {}
    for index in range(80):
        length = rnd.randint(0, 5)
        rand_keys["".join(rnd.choice(alphabet) for _ in range(length)) + str(index)] = index
    add_obj("random_keys", rand_keys)

    return cases


def render() -> str:
    lines = []
    for name, raw in build_cases():
        obj = json.loads(raw)
        entry = {
            "name": name,
            "input": raw,
            "expected": canonical(obj),
        }
        lines.append(json.dumps(entry, ensure_ascii=False, sort_keys=True))
    return "\n".join(lines) + "\n"


def build_indent_cases() -> list[tuple[str, str]]:
    """indent=2 形式的语料。

    重点覆盖 canonical 形式覆盖不到的两点：**键的插入顺序必须保留**，以及
    嵌套空容器（``{}`` / ``[]``）不换行展开。
    """
    cases: list[tuple[str, str]] = []

    def add(name: str, raw: str) -> None:
        cases.append((name, raw))

    add("empty_object", "{}")
    add("empty_array", "[]")
    add("scalar_int", "60")
    add("scalar_float", "60.0")
    add("key_order_preserved", '{"b": 1, "a": 2, "c": 3}')
    add("key_order_unicode", '{"中": 1, "a": 2, "é": 3}')
    add("nested_empty_containers", '{"a": {}, "b": [], "c": null}')
    add("nested_object", '{"a": [1, 2], "b": {"c": 3}}')
    add("array_of_objects", '[{"x": 1}, {"y": [2]}]')
    add("deep_nesting", '{"a": {"b": {"c": {"d": [1, {"e": "f"}]}}}}')
    add(
        "escapes_and_unicode",
        '{"q": "he said \\"hi\\"", "bs": "C:\\\\p\\\\f", "nl": "a\\nb", '
        '"tab": "a\\tb", "zh": "中文", "emoji": "🙂", "html": "<a>&b"}',
    )
    add("mixed_types", '{"i": 1, "f": 1.0, "t": true, "n": null, "s": "1"}')
    add("float_boundaries", '{"a": 1e16, "b": 1e-5, "c": 0.1, "d": -0.0, "e": 100.0}')
    add("long_array", '{"xs": [' + ", ".join(str(i) for i in range(12)) + "]}")

    # 真实配置形状：字段顺序即 canonical_indent 的输出顺序（不排序）。
    add("real_config", json.dumps(real_config(), ensure_ascii=False))
    return cases


def render_indent() -> str:
    lines = []
    for name, raw in build_indent_cases():
        obj = json.loads(raw)
        entry = {
            "name": name,
            "input": raw,
            "expected": canonical_indent(obj, 2),
        }
        lines.append(json.dumps(entry, ensure_ascii=False, sort_keys=True))
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument(
        "--indent-output", type=Path, default=DEFAULT_INDENT_OUTPUT
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help="只校验现有语料与当前实现一致，不写盘（CI 用）",
    )
    args = parser.parse_args()

    targets = [
        (args.output, render(), "canonical"),
        (args.indent_output, render_indent(), "indent=2"),
    ]

    if args.check:
        failed = False
        for path, content, label in targets:
            if not path.exists():
                print(f"[{label}] 语料缺失: {path}", file=sys.stderr)
                failed = True
                continue
            if path.read_text(encoding="utf-8") != content:
                print(
                    f"[{label}] 语料已过期: {path}\n"
                    f"请重新运行: python {Path(__file__).name}",
                    file=sys.stderr,
                )
                failed = True
                continue
            print(f"[{label}] 语料最新: {path}")
        return 1 if failed else 0

    for path, content, label in targets:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8", newline="\n")
        print(f"[{label}] 已写入 {content.count(chr(10))} 条语料: {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
