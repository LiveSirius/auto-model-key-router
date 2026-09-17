#!/usr/bin/env python3
"""生成 internal/proxysupport 的对拍语料。

语料来自**真实 Python 实现**，Go 测试只读它、不需要 Python 解释器。`--check` 是 CI
的新鲜度闸门：语料与当前实现不一致时退出码 1，防止有人改了 Python 却忘了重新生成。

用法：
    python scripts/gen_proxysupport_corpus.py            # 生成
    python scripts/gen_proxysupport_corpus.py --check    # 只校验
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
OUTPUT = REPO_ROOT / "internal" / "proxysupport" / "testdata" / "utf8_replace_corpus.json"


def build_corpus() -> list[dict[str, str]]:
    """构造穷举的 UTF-8 宽容解码语料。

    覆盖面（去重后约 1476 条）：
      * 全部 256 个单字节；
      * 首字节 0xC0-0xFF 与代表性后继字节的两字节组合；
      * E0/E1/E4/EC/ED/EE/EF 的三字节边界组合（收窄区间、代理对）；
      * F0/F1/F4/F5 的四字节边界组合（过长、超 U+10FFFF）；
      * 手工挑选的混合与长序列。
    """
    cases: list[bytes] = []
    for b in range(0x100):
        cases.append(bytes([b]))

    second_bytes = [0x00, 0x41, 0x7F, 0x80, 0x8F, 0x90, 0x9F, 0xA0, 0xBF, 0xC0, 0xEF, 0xF0, 0xFF]
    for b0 in range(0xC0, 0x100):
        for b1 in second_bytes:
            cases.append(bytes([b0, b1]))

    third_bytes = [0x00, 0x7F, 0x80, 0x8F, 0x90, 0x9F, 0xA0, 0xBF, 0xC0, 0xFF]
    for b0 in (0xE0, 0xE1, 0xE4, 0xEC, 0xED, 0xEE, 0xEF):
        for b1 in third_bytes:
            for b2 in (0x80, 0xA0, 0xBF, 0xC0):
                cases.append(bytes([b0, b1, b2]))

    for b0 in (0xF0, 0xF1, 0xF4, 0xF5):
        for b1 in (0x7F, 0x80, 0x8F, 0x90, 0xBF, 0xC0):
            for b2 in (0x80, 0xBF):
                for b3 in (0x80, 0xBF):
                    cases.append(bytes([b0, b1, b2, b3]))

    cases += [
        b"\xff\xfe", b"a\xffb", b"\xff\xff\xff", b"\xc0\x80", b"\xed\xa0\x80",
        b"\xf0\x9f", b"\xf0\x9f\x98\x80", b"\x80", b"\xe4\xb8\xad\xff",
        b"\xf4\x90\x80\x80", b"\xe4\xb8\xad", b"\xe4\xb8",
        b"\xe4\xb8\xad\xe6\x96\x87\xe5\x86\x85\xe5\xae\xb9",
        b"\xed\xa0\x80\xed\xb0\x80", b"\xf4\x8f\xbf\xbf",
        b"\xc2\x80", b"\xdf\xbf", b"\xe0\xa0\x80", b"\xef\xbf\xbf",
        b"\xf0\x90\x80\x80", b"", b"hello", b"\x00\x01\x02",
    ]

    seen: set[bytes] = set()
    corpus: list[dict[str, str]] = []
    for raw in cases:
        if raw in seen:
            continue
        seen.add(raw)
        corpus.append(
            {
                "bytes_hex": raw.hex(),
                # 这就是被对拍的语义：Python 的 errors="replace"。
                "replaced": raw.decode("utf-8", errors="replace"),
            }
        )
    return corpus


def serialize(corpus: list[dict[str, str]]) -> str:
    """与 Go 侧读取方式一致的稳定序列化。"""
    return json.dumps(corpus, ensure_ascii=False, indent=1) + "\n"


def main() -> int:
    text = serialize(build_corpus())
    check = "--check" in sys.argv[1:]
    if check:
        if not OUTPUT.exists():
            print(f"语料缺失: {OUTPUT}", file=sys.stderr)
            return 1
        current = OUTPUT.read_text(encoding="utf-8")
        if current != text:
            print(
                f"语料已过期: {OUTPUT}\n"
                "请运行 python scripts/gen_proxysupport_corpus.py 重新生成。",
                file=sys.stderr,
            )
            return 1
        print(f"语料最新: {OUTPUT.name}")
        return 0
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    # newline="\n" 保证在 Windows 上也是 LF，避免 CRLF 让对拍结果随平台漂移。
    with OUTPUT.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
    print(f"已写入 {OUTPUT}（{len(build_corpus())} 条）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
