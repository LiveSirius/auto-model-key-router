from __future__ import annotations

from importlib.metadata import PackageNotFoundError, version
from pathlib import Path


def _resolve_version() -> str:
    # 已安装的包以安装元数据为准。源码树里的 pyproject.toml 可能是发布中断遗留的
    # 「已改版本、未提交未发布」状态（见 scripts/release.py 的写入顺序），
    # 直接采信会报出一个 PyPI 上并不存在的假版本号。
    try:
        return version("auto-model-key-router")
    except PackageNotFoundError:
        pass

    # 未安装（例如直接从源码运行）时，才退回读取 pyproject.toml。
    pyproject = Path(__file__).resolve().parents[1] / "pyproject.toml"
    if pyproject.exists():
        import tomllib

        return tomllib.loads(pyproject.read_text(encoding="utf-8"))["project"]["version"]
    return "0.0.0"


__version__ = _resolve_version()
