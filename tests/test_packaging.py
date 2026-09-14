from __future__ import annotations

import tomllib
from pathlib import Path, PurePosixPath


ROOT = Path(__file__).resolve().parents[1]


def project_metadata() -> dict:
    return tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))


def test_console_scripts_expose_both_supported_command_names() -> None:
    scripts = project_metadata()["project"]["scripts"]

    assert scripts["amkr"] == "auto_model_key_router.main:main"
    assert scripts["auto-model-key-router"] == "auto_model_key_router.main:main"


def test_package_discovery_includes_runtime_package() -> None:
    package_find = project_metadata()["tool"]["setuptools"]["packages"]["find"]

    assert "auto_model_key_router*" in package_find["include"]
    assert "build*" in package_find["exclude"]
    assert ".venv*" in package_find["exclude"]


def test_webui_assets_are_declared_as_package_data() -> None:
    # WebUI 采用「资产随包发布」方案：没有构建步骤，也没有 [webui] extra，
    # 因此 package-data 必须覆盖实际存在的每个资产文件，漏一个就 404。
    patterns = project_metadata()["tool"]["setuptools"]["package-data"][
        "auto_model_key_router"
    ]

    package_dir = ROOT / "auto_model_key_router"
    asset_dir = package_dir / "webui"
    # package-data 的 glob 相对包目录，因此这里同样带上 "webui/" 前缀。
    files = sorted(
        path.relative_to(package_dir).as_posix()
        for path in asset_dir.rglob("*")
        if path.is_file()
    )
    assert files, "WebUI 资产缺失，/ui 将返回 404"

    # PurePosixPath.match 的 "*" 不跨 "/"，正好等价于 setuptools 的目录层级语义：
    # "webui/*" 只覆盖顶层文件，不会误判 webui/pages/ 下的文件。
    unmatched = [
        name
        for name in files
        if not any(PurePosixPath(name).match(pattern) for pattern in patterns)
    ]
    assert not unmatched, f"以下 WebUI 资产没有被 package-data 覆盖: {unmatched}"

    assert "webui/index.html" in files
    assert any(name.startswith("webui/pages/") for name in files)
