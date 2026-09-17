from __future__ import annotations

import re
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


# —— 容器镜像 ——
# 镜像由 .github/workflows/release.yml 在发布时构建并推到 GHCR。这套断言不跑 docker，
# 只钉住那些「错了会在发布当天才发现」的点：镜像缺文件、密钥被打进镜像、容器里监听
# 127.0.0.1 导致端口映射无效、以及发布流程漏掉构建镜像这一步。

DOCKERFILE = ROOT / "Dockerfile"
DOCKERIGNORE = ROOT / ".dockerignore"
RELEASE_WORKFLOW = ROOT / ".github" / "workflows" / "release.yml"


def test_dockerfile_builds_from_a_readable_context() -> None:
    text = DOCKERFILE.read_text(encoding="utf-8")

    # setuptools 读 readme/license，源码目录要有，否则镜像构建直接失败。
    for required in ("pyproject.toml", "README.md", "LICENSE", "auto_model_key_router"):
        assert required in text, f"Dockerfile 构建上下文缺少 {required}"
    assert re.search(r"(?m)^FROM python:3\.\d+-slim$", text)


def test_dockerfile_listens_beyond_localhost() -> None:
    # 配置默认 host=127.0.0.1，容器里必须覆盖成 0.0.0.0，否则 -p 8000:8000 打不进来。
    instructions = "\n".join(
        line
        for line in DOCKERFILE.read_text(encoding="utf-8").splitlines()
        if not line.strip().startswith("#")
    )
    assert "0.0.0.0" in instructions
    assert "127.0.0.1" not in instructions


def test_dockerfile_persists_state_outside_the_image() -> None:
    text = DOCKERFILE.read_text(encoding="utf-8")
    # 配置/指标库/日志都落在 XDG_CACHE_HOME 下，卷挂在它上面才算持久化。
    assert "XDG_CACHE_HOME" in text
    assert "VOLUME /data" in text


def dockerignore_patterns() -> list[str]:
    return [
        line.strip()
        for line in DOCKERIGNORE.read_text(encoding="utf-8").splitlines()
        if line.strip() and not line.strip().startswith("#")
    ]


def test_dockerignore_keeps_secrets_out_of_the_image() -> None:
    patterns = dockerignore_patterns()

    # 这些文件里是真实上游 Key 和本地授权 Key，进镜像就是把凭据发到 registry。
    for secret in ("router-config.json", ".env", ".env.local", "metrics.sqlite3", "server.log"):
        assert any(PurePosixPath(secret).match(pattern) for pattern in patterns), secret


def test_dockerignore_excludes_heavy_build_artifacts() -> None:
    # 目录条目可以写成 "dist/" 或 "dist"，两种都算命中。
    patterns = {pattern.rstrip("/") for pattern in dockerignore_patterns()}

    for artifact in (".git", ".venv", "dist", "__pycache__"):
        assert artifact in patterns, artifact


def test_release_workflow_builds_and_pushes_container_image() -> None:
    text = RELEASE_WORKFLOW.read_text(encoding="utf-8")

    assert "docker build" in text
    assert "ghcr.io" in text
    assert "packages: write" in text, "推 GHCR 需要 packages 写权限"
    # 预发布版本不能顶掉 latest，所以 latest 标签得有条件地推。
    assert ":latest" in text
    assert "预发布版本" in text


def test_release_workflow_smoke_tests_the_image_before_pushing() -> None:
    # docker build 成功不代表容器能起来（缺文件、host 写错都是运行时才炸）。
    # 推送前必须在容器里真的起一次服务，顺序错了就等于把坏镜像发出去。
    text = RELEASE_WORKFLOW.read_text(encoding="utf-8")

    assert "/health" in text
    assert text.index("- name: Verify container image") < text.index(
        "- name: Push container image to GHCR"
    )


def test_release_workflow_pushes_image_before_creating_the_release() -> None:
    # gh release create 才会建 tag。镜像步骤必须在它之前：推送失败时 tag 和 release
    # 都不存在，重跑不会被 existing_release 的跳过条件挡住。
    text = RELEASE_WORKFLOW.read_text(encoding="utf-8")

    assert text.index("- name: Push container image to GHCR") < text.index(
        "- name: Create GitHub Release"
    )


def test_release_workflow_links_the_package_to_the_repository() -> None:
    # 没有这个标签，命名空间下若已存在同名包且未关联仓库，GITHUB_TOKEN 会推不上去；
    # 值必须由 GITHUB_REPOSITORY 推出，写死仓库地址在改名/换仓库时会失效。
    text = RELEASE_WORKFLOW.read_text(encoding="utf-8")

    assert "org.opencontainers.image.source" in text
    assert "GITHUB_SERVER_URL/$GITHUB_REPOSITORY" in text
    assert "Sparrived/auto-model-key-router" not in text


def test_readme_documents_that_ghcr_visibility_is_not_inherited() -> None:
    # 仓库公开不等于镜像可匿名拉取：GHCR 包默认私有且可见性不继承仓库，也没有 API 可改。
    # 少写这段，用户只会看到一个 403 而不知道该去包页面点一次。
    text = (ROOT / "README.md").read_text(encoding="utf-8")

    assert "不随仓库继承" in text
    assert "Change visibility" in text
