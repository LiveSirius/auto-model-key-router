#!/usr/bin/env python3
"""生成 internal/formatting 的对拍语料（Python 侧为参照实现）。

直接 import 本仓库的 ``auto_model_key_router.formatting``，用真实实现算出期望值。
覆盖五块：

1. ``key_fingerprint``    —— sha256 前 12 位，含空值与多字节 key
2. ``short_text``         —— 码点截断与省略号，含 limit<=1 的下限分支
3. ``compact_url``        —— netloc/path 压缩，含 urlparse 的 ValueError 退化路径
4. ``percent``            —— 百分比格式化，含分母<=0 与等距平局（四舍六入五成双）
5. ``abbreviate_number``  —— K/M/B 缩写，含阈值边界

另附 ``nfkc_unsafe_runes``：Python 用 NFKC 归一化拦截「会拆出 /?#@: 的域名」。
Go 标准库没有 NFKC，只能把这 19 个码点写成表；这里把 Python 全码点扫描的结果
落进语料，Go 测试再拿它校验自己的表，防止手抄出错。

用法::

    python scripts/gen_formatting_corpus.py           # 写入语料
    python scripts/gen_formatting_corpus.py --check   # 只校验语料是否最新
"""

from __future__ import annotations

import argparse
import json
import random
import sys
import unicodedata
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from auto_model_key_router.formatting import (  # noqa: E402
    abbreviate_number,
    compact_url,
    key_fingerprint,
    percent,
    short_text,
)

DEFAULT_DIR = REPO_ROOT / "internal" / "formatting" / "testdata"

# 逐字节稳定：语料的每一段都按代码里的固定顺序产出，不用集合/字典迭代顺序。
SEED = 20260117


# --------------------------------------------------------------------------- #
# 各函数的输入面
# --------------------------------------------------------------------------- #

def fingerprint_inputs() -> list[str]:
    return [
        "",
        "s",
        "sk-abc",
        "local-key",
        "sk-proj-0123456789abcdefghijklmnopqrstuvwxyz",
        "中文密钥",
        "🔑-emoji-key",
        "  leading-and-trailing  ",
        "a" * 200,
        "key with space",
        "key\twith\ttab",
    ]


def short_text_inputs() -> list[tuple[str, int]]:
    texts = [
        "",
        "a",
        "ab",
        "abc",
        "abcdef",
        "x" * 31,
        "x" * 32,
        "x" * 33,
        "x" * 40,
        "中文字符串测试内容",
        "中" * 32,
        "🔑emoji-密集-🀄🎯",
        "cafe\u0301-组合字符",
        "line1\nline2",
        "含省略号…的文本",
        "…",
        " " * 40,
    ]
    limits = [32, 33, 31, 19, 28, 36, 42, 56, 5, 4, 3, 2, 1, 0, -1, -5, 100]
    return [(text, limit) for text in texts for limit in limits]


def compact_url_inputs() -> list[tuple[str, int]]:
    urls = [
        # 常规
        "https://api.example.com/v1/chat/completions",
        "https://api.example.com/",
        "https://api.example.com",
        "https://api.example.com/v1//",
        "http://user:pass@host:8080/a/b/",
        "ftp://files.example.com/pub/data/",
        "HTTPS://UPPER.EXAMPLE.COM/Path",
        "https://h/a?b=c#d",
        "https://h#frag",
        "https://h?query=only",
        # 无 netloc：整串走短文本
        "example.com/path",
        "/just/a/path",
        "not a url at all",
        "C:/Users/x/y",
        "mailto:someone@example.com",
        "urn:isbn:12345",
        "http://",
        "https:///path",
        "//host/path",
        # 空值与哨兵
        "",
        "-",
        # 空白与控制字符：lstrip 只吃开头，\t\r\n 整串删掉
        "  https://spaced.example.com/x  ",
        "\t\n https://ctrl.example.com/y",
        "http://exa\nmple.com/v1",
        "https://trail.example.com/path\r\n",
        # 百分号编码与多字节：path 保持原文不做解码
        "https://h/%E4%B8%AD%E6%96%87/path",
        "https://h/中文/路径",
        "https://h/a b",
        "https://h/p/./q/../r",
        "https://h/a;b",
        "https://h/a;b/c",
        "https://h/a;b/c;d",
        "ftp://h/x;y",
        "mailto:a;b",
        "h/a;b",
        "//h/a;b",
        "https://h/a/;b",
        "https://h/;b",
        # scheme 判定
        "1http://h/x",
        "ht tp://h/x",
        "h+ttp://h/x",
        "h.tt-p://h/x",
        "://h/x",
        # 端口异常（Python 不校验端口）
        "http://h:notaport/x",
        # IPv6 / IPvFuture 字面量
        "https://[2001:db8::1]:8080/v1",
        "https://[::1]:80/x",
        "https://[::1]/x",
        "https://[::1",
        "https://::1]/x",
        "https://[[::1]]/x",
        "https://[1.2.3.4]:80/x",
        "https://[bad-ipv6]:80/x",
        "https://[v1.foo]:80/x",
        "https://[vZ.foo]:80/x",
        "https://[v1]:80/x",
        "https://[va.b]:80/x",
        "https://[::1%eth0]:80/x",
        "https://[fe80::1%]:80/x",
        "https://[fe80::1%a%b]:80/x",
        "https://[user@::1]:80/x",
        "https://[a]b]:80/x",
        "https://[fe80::1%25eth0]:80/x",
        "https://[::ffff:1.2.3.4]/x",
        "https://[1:2:3:4:5:6:7:8:9]/x",
        "https://[0000:0000:0000:0000:0000:0000:0000:0001]/x",
        "https://[::1]:80/x?y=z#w",
        # NFKC 危险码点：Python 抛 ValueError，退化成整串截断
        "https://h\u2047x/v1",
        "https://h\u2048x/v1",
        "https://h\u2049x/v1",
        "https://h\u2100x/v1",
        "https://h\u2101x/v1",
        "https://h\u2105x/v1",
        "https://h\u2106x/v1",
        "https://h\u2a74x/v1",
        "https://h\ufe13x/v1",
        "https://h\ufe16x/v1",
        "https://h\ufe55x/v1",
        "https://h\ufe56x/v1",
        "https://h\ufe5fx/v1",
        "https://h\ufe6bx/v1",
        "https://h\uff03x/v1",
        "https://h\uff0fx/v1",
        "https://h\uff1ax/v1",
        "https://h\uff1fx/v1",
        "https://h\uff20x/v1",
        # NFKC 稳定的非 ASCII netloc：不报错，正常压缩
        "https://中文.example.com/v1",
        "https://é.example.com/v1",
        "https://Ⅻ.example.com/v1",
        "https://①.example.com/v1",
        "https://h\u0301x/v1",
        "https://h\u00a0x/v1",
        # 超长
        "https://api.example.com/" + "a" * 60,
    ]
    limits = [32]
    return [(url, limit) for url in urls for limit in limits] + [
        (url, limit)
        for url in [
            "https://api.example.com/v1/chat/completions",
            "https://api.example.com/",
            "https://h/a;b",
            "  https://spaced.example.com/x  ",
            "not a url at all",
            "",
            "https://h\u2100x/v1",
            "https://[bad-ipv6]:80/x",
            "https://中文.example.com/v1",
            "🔑🔑🔑",
        ]
        for limit in [10, 3, 1, 0, -1, 56, 100]
    ]


def percent_inputs() -> list[tuple[int, int]]:
    pairs: list[tuple[int, int]] = [
        (0, 1), (1, 1), (1, 2), (1, 3), (2, 3), (1, 7), (2, 7), (1, 8), (3, 8),
        (0, 0), (5, 0), (0, -1), (1, -3), (-1, -3), (-1, 3), (-5, 7),
        (1, 1000000), (999999, 1000000), (1, 100), (1, 1000), (1, 10000),
        # 等距平局：6.25 / 18.75 / 31.25 / 43.75 都要按「平局取偶」落到偶数末位
        (1, 16), (3, 16), (5, 16), (7, 16), (9, 16), (11, 16), (13, 16), (15, 16),
        (1, 32), (3, 32), (1, 64), (1, 800), (1, 1600), (123, 16),
        (1, 4), (3, 4), (1, 20), (1, 40), (1, 200), (1, 400),
        # 精确有理数除法：先转 float64 会多一次舍入，结果不同
        (9007199254740993, 7), (9223372036854775807, 3),
        (9007199254740993, 9007199254740995),
        (9007199254740993, 9007199254740992),
        (9223372036854775807, 9223372036854775806),
        (1000000000000000001, 3), (999999999999999999, 9),
        (4503599627370497, 4503599627370499),
        (9223372036854775807, 9223372036854775807),
        (-9223372036854775808, 9223372036854775807),
        (9223372036854775807, 1),
    ]
    rng = random.Random(SEED)
    for _ in range(400):
        pairs.append((rng.randint(0, 10**6), rng.randint(1, 10**6)))
    for _ in range(200):
        pairs.append((rng.randint(-10**9, 10**9), rng.randint(-10**9, 10**9)))
    for _ in range(200):
        pairs.append((rng.randint(-(2**63), 2**63 - 1), rng.randint(-(2**63), 2**63 - 1)))
    return pairs


def abbreviate_inputs() -> list[int]:
    values = [
        0, 1, 9, 10, 99, 999, 1000, 1001, 1049, 1050, 1051, 1499, 1500, 1501,
        9949, 9950, 9999, 10000, 10499, 10500, 999949, 999950, 999999,
        1000000, 1049999, 1050000, 999999999, 1000000000, 1500000000,
        999999999999, 1000000000000,
        -1, -999, -1000, -1500, -1000000, -1500000000,
        10**15, 10**18, 2**53, 2**53 + 1, 2**53 + 2, 2**62, 2**63 - 1,
        -(2**63), 1000000000000000001, 999999999999999999, 1049999999999999999,
    ]
    rng = random.Random(SEED + 1)
    for _ in range(300):
        values.append(rng.randint(-(2**63), 2**63 - 1))
    for _ in range(300):
        values.append(rng.randint(0, 10**12))
    return values


def nfkc_unsafe_runes() -> str:
    """扫描全部码点，返回 NFKC 归一化后会引入 /?#@: 的字符（按码点升序）。

    对应 CPython urllib.parse 的 _checknetloc：它在 netloc 含非 ASCII 时做
    NFKC 检查，用来拦截「看起来是域名、归一化后却被 IDNA 拆出分隔符」的地址。
    """
    found: list[str] = []
    for codepoint in range(0x110000):
        if 0xD800 <= codepoint <= 0xDFFF:
            continue
        char = chr(codepoint)
        if char.isascii():
            continue
        normalized = unicodedata.normalize("NFKC", char)
        if normalized != char and any(mark in normalized for mark in "/?#@:"):
            found.append(char)
    return "".join(found)


# --------------------------------------------------------------------------- #
# 语料组装
# --------------------------------------------------------------------------- #

def build_corpus() -> dict:
    return {
        "key_fingerprint": [
            {"input": value, "want": key_fingerprint(value)}
            for value in fingerprint_inputs()
        ],
        "short_text": [
            {"value": text, "limit": limit, "want": short_text(text, limit)}
            for text, limit in short_text_inputs()
        ],
        "compact_url": [
            {"value": url, "limit": limit, "want": compact_url(url, limit)}
            for url, limit in compact_url_inputs()
        ],
        "percent": [
            {"numerator": numerator, "denominator": denominator,
             "want": percent(numerator, denominator)}
            for numerator, denominator in percent_inputs()
        ],
        "abbreviate_number": [
            {"value": value, "want": abbreviate_number(value)}
            for value in abbreviate_inputs()
        ],
        "nfkc_unsafe_runes": nfkc_unsafe_runes(),
    }


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()

    path = args.output_dir / "formatting_corpus.json"
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
