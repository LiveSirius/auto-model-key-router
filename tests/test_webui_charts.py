"""WebUI 图表数学的回归测试。

图表读数错了比界面难看严重得多 —— 一个把"窗口累计"当"每分钟速率"画出来的
折线图会直接误导排障方向。所以把 chart-math.js 里的纯函数（无 DOM 依赖）
用 node 跑一遍断言，锁住这些口径：

  * 每个桶的计数按 bucket_seconds 归一化成"每分钟"，绝不假设桶宽是 60 秒；
  * 比率/均值用"分子分母各自求和"再相除，不是对每桶的比率取平均；
  * 0 是真实读数（空闲），只有 null/缺失才是缺口；
  * 未完成的尾桶标记为 partial，图表用虚线画，避免被读成流量骤降；
  * 百分比轴固定 0–100，分母为 0 时显示 "-" 而不是 "0%"。

这些断言全部在 tests/webui_chart_probe.mjs 里，本文件只负责用 pytest 调度它，
以便随主测试套件一起跑。
"""

from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import pytest

PROBE = Path(__file__).with_name("webui_chart_probe.mjs")


def test_webui_chart_math_invariants() -> None:
    node = shutil.which("node")
    if node is None:
        pytest.skip("未安装 node，跳过 WebUI 图表口径校验")

    result = subprocess.run(
        [node, str(PROBE)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        cwd=str(PROBE.parent),
    )
    assert result.returncode == 0, (
        f"图表口径校验失败：\nstdout: {result.stdout}\nstderr: {result.stderr}"
    )
    payload = json.loads(result.stdout.strip().splitlines()[-1])
    assert payload["failed"] == [], f"未通过的断言: {payload['failed']}"
    # 防止探针被误删成空跑：断言数只应增加。
    assert payload["total"] >= 70, f"断言数量异常偏少: {payload['total']}"
