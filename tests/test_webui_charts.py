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
TIP_PROBE = Path(__file__).with_name("webui_tip_probe.mjs")


def _run_probe(probe: Path) -> dict:
    node = shutil.which("node")
    if node is None:
        pytest.skip("未安装 node，跳过 WebUI 图表口径校验")

    result = subprocess.run(
        [node, str(probe)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        cwd=str(probe.parent),
    )
    assert result.returncode == 0, (
        f"图表校验失败：\nstdout: {result.stdout}\nstderr: {result.stderr}"
    )
    return json.loads(result.stdout.strip().splitlines()[-1])


def test_webui_chart_math_invariants() -> None:
    payload = _run_probe(PROBE)
    assert payload["failed"] == [], f"未通过的断言: {payload['failed']}"
    # 防止探针被误删成空跑：断言数只应增加。
    assert payload["total"] >= 70, f"断言数量异常偏少: {payload['total']}"


def test_webui_chart_tooltips_have_no_null_row() -> None:
    """气泡读数里不能出现 null 行。

    回归：原生 replaceChildren 会把 null 子项字符串化成文本节点 "null"，
    于是每个已完结的点都在读数下多出一行 null。这个断言依赖探针里的 DOM
    垫片忠实还原该语义，所以不能用 append 那种会过滤空值的实现代替。
    """
    payload = _run_probe(TIP_PROBE)
    assert payload["failed"] == [], f"未通过的断言: {payload['failed']}"
    assert payload["total"] >= 12, f"断言数量异常偏少: {payload['total']}"
