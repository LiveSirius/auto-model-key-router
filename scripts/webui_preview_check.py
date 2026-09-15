"""开发用：核对预览服务器的合成数据是否口径自洽。

图表按 15 秒桶切片、快照按 30 秒桶聚合，两者的窗口总量必须一致；
否则预览时 KPI 与曲线会显示两个对不上的"1 小时总量"，让视觉验收失去意义。

用法：python scripts/webui_preview_check.py [--port 8813]
"""

from __future__ import annotations

import argparse
import json
import urllib.request


def get(url: str) -> dict:
    with urllib.request.urlopen(url, timeout=5) as response:  # noqa: S310 - 本地预览
        return json.load(response)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8813)
    args = parser.parse_args()
    base = f"http://127.0.0.1:{args.port}"

    snapshot = get(f"{base}/metrics?hours=1")
    snap_requests = snapshot["total"]["requests"]
    snap_tokens = snapshot["total"]["total_tokens"]
    print(f"snapshot           requests={snap_requests:>6} tokens={snap_tokens:>10}")

    failures = []
    for bucket in (15, 60, 300, 900):
        series = get(f"{base}/metrics/series?hours=1&bucket_seconds={bucket}")
        points = series["points"]
        requests = sum(p["requests"] for p in points)
        tokens = sum(p["total_tokens"] for p in points)
        drift = abs(requests - snap_requests) / max(1, snap_requests)
        flag = "OK  " if drift < 0.05 else "FAIL"
        if drift >= 0.05:
            failures.append(bucket)
        print(f"{flag} series {bucket:>3}s buckets={len(points):>3} requests={requests:>6} tokens={tokens:>10} drift={drift:.2%}")

    if failures:
        print(f"\n桶宽 {failures} 的窗口总量与快照偏差超过 5%")
        return 1
    print("\n各桶宽的窗口总量与快照一致")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
