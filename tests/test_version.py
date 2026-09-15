from __future__ import annotations

from importlib.metadata import PackageNotFoundError
from pathlib import Path
import tomllib

import auto_model_key_router
from auto_model_key_router import _resolve_version


ROOT = Path(__file__).resolve().parents[1]


def test_installed_metadata_wins_over_source_tree_pyproject(monkeypatch) -> None:
    # 回归：曾优先读取 parents[1]/pyproject.toml，导致「发布中断遗留的版本号」
    # 被当成真实版本报出去。已安装时必须采信安装元数据。
    monkeypatch.setattr(auto_model_key_router, "version", lambda _name: "9.9.9")

    assert _resolve_version() == "9.9.9"


def test_falls_back_to_pyproject_when_package_is_not_installed(monkeypatch) -> None:
    def raise_not_found(_name: str) -> str:
        raise PackageNotFoundError(_name)

    monkeypatch.setattr(auto_model_key_router, "version", raise_not_found)

    expected = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))["project"]["version"]

    assert _resolve_version() == expected
