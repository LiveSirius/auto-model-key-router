#!/usr/bin/env python3
"""生成 internal/updatecheck 的对拍语料。

覆盖 update.py 中**与分发方式无关**的版本检查部分（约 140 行）：

  * version_numbers / comparable_version / is_newer_version —— 纯函数；
  * check_latest_pypi / check_latest_release / check_latest_version —— 需要 HTTP，
    这里通过替换 ``update.urlopen`` 注入固定响应，从而**不联网**也能对拍响应解析与
    结果整形（含缺字段、非 JSON 对象、网络异常等分支）。

语料同时记录**输入**（payload 描述符）与**输出**，这样 Go 测试可以自己构造注入的
fetcher 重放，不需要在 Go 侧重复写一份 fixture。

自更新机制（install_latest_* / windows_update_helper_script / uv_tool_* 等约 573 行）
按产品决策不移植，故不在范围内。

用法：
    python scripts/gen_updatecheck_corpus.py            # 生成
    python scripts/gen_updatecheck_corpus.py --check    # 只校验
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO_ROOT))

OUTPUT = REPO_ROOT / "internal" / "updatecheck" / "testdata" / "updatecheck_corpus.json"

# payload 描述符：
#   {"kind": "none"}               -> 抛异常（模拟网络故障）
#   {"kind": "text", "value": ...} -> 原样返回该文本（非 JSON）
#   {"kind": "json", "value": ...} -> 返回该 JSON
NONE = {"kind": "none"}


def desc(payload) -> dict:
    if payload is None:
        return NONE
    if isinstance(payload, str):
        return {"kind": "text", "value": payload}
    return {"kind": "json", "value": payload}


def version_cases() -> list[str]:
    """版本字符串语料：覆盖补零、多段、非数字前缀、build 元数据、大小写 v 等。"""
    return [
        "0", "1", "1.0", "1.0.0", "1.2", "1.2.3", "1.2.3.4", "1.2.3.4.5",
        "v1.2.3", "V1.2.3", "1.2.3+build.7", "1.2.3+local", "1.2.3-alpha",
        "1.2.3-rc.1", "2.0.0", "10.0.0", "9.9.9", "0.0.0", "4.1.0", "5.0.0",
        "1.10.0", "1.9.0", "abc", "", "+++", "1..2", " 1.2.3 ", "1.2.3.0",
        "01.02.03", "1e3", "3.0", "3", "2.99.99", "100", "1.0.0.0.0.1",
    ]


GOOD_PYPI = {
    "info": {
        "version": "5.0.0",
        "release_url": "https://pypi.org/project/auto-model-key-router/5.0.0/",
    },
    "urls": [
        {"packagetype": "sdist", "filename": "auto_model_key_router-5.0.0.tar.gz"},
        {
            "packagetype": "bdist_wheel",
            "filename": "auto_model_key_router-5.0.0-py3-none-any.whl",
            "url": "https://files.pythonhosted.org/x/auto_model_key_router-5.0.0-py3-none-any.whl",
            "digests": {"sha256": "a" * 64, "md5": "b" * 32},
        },
    ],
}
GOOD_GITHUB = {
    "tag_name": "v5.0.0",
    "html_url": "https://github.com/Sparrived/auto-model-key-router/releases/tag/v5.0.0",
}


def http_cases() -> list[dict]:
    """(名称, 路由, pypi 载荷, github 载荷)。未用到的端点填 NONE 作为绊线：

    若 Go 侧错误地调用了本不该调用的端点，会拿到"网络故障"从而产出不同的结果，
    比对立刻失败——这比只记录结果更能锁住"调用顺序"。
    """
    return [
        ("pypi/ok", "pypi", GOOD_PYPI, None),
        ("pypi/无 release_url 时回退 package_url", "pypi",
         {"info": {"version": "5.0.0", "package_url": "https://pypi.org/project/amkr"}, "urls": []}, None),
        ("pypi/无 release_url 也无 package_url", "pypi",
         {"info": {"version": "5.0.0"}, "urls": []}, None),
        ("pypi/无匹配 wheel", "pypi",
         {"info": {"version": "5.0.0"}, "urls": [
             {"packagetype": "bdist_wheel", "filename": "x-py2.py3-none-any.whl"}]}, None),
        ("pypi/wheel 缺 url 与 digests", "pypi",
         {"info": {"version": "5.0.0"}, "urls": [
             {"packagetype": "bdist_wheel", "filename": "y-py3-none-any.whl"}]}, None),
        ("pypi/缺 info", "pypi", {"urls": []}, None),
        ("pypi/info 不是对象", "pypi", {"info": "nope"}, None),
        ("pypi/缺 version", "pypi", {"info": {}}, None),
        ("pypi/version 为空白", "pypi", {"info": {"version": "   "}}, None),
        ("pypi/version 为数字", "pypi", {"info": {"version": 5000}}, None),
        ("pypi/version 为零（假值）", "pypi", {"info": {"version": 0}}, None),
        ("pypi/非 JSON 文本", "pypi", "not json", None),
        ("pypi/JSON 数组而非对象", "pypi", [1, 2], None),
        ("pypi/网络异常", "pypi", None, None),
        ("github/ok", "github", None, GOOD_GITHUB),
        ("github/tag 无 v 前缀", "github", None, {"tag_name": "5.1.0"}),
        ("github/tag 大写 V", "github", None, {"tag_name": "V5.2.0"}),
        ("github/tag 只有 v", "github", None, {"tag_name": "v"}),
        ("github/缺 tag_name", "github", None, {"html_url": "https://x"}),
        ("github/tag 为空白", "github", None, {"tag_name": "  "}),
        ("github/tag 为数字", "github", None, {"tag_name": 5000}),
        ("github/缺 html_url", "github", None, {"tag_name": "v5.0.0"}),
        ("github/网络异常", "github", None, None),
        ("combined/pypi 失败回退 github", "combined", None, GOOD_GITHUB),
        ("combined/两者都失败", "combined", None, None),
        ("combined/pypi 成功则不查 github", "combined", GOOD_PYPI, None),
        ("combined/pypi 非 JSON 后回退 github", "combined", "not json", GOOD_GITHUB),
    ]


def serialize_result(result) -> dict:
    """把 VersionCheckResult 转成可比较的 JSON 形态（None 保留为 null）。"""
    return {
        "current_version": result.current_version,
        "latest_version": result.latest_version,
        "latest_tag": result.latest_tag,
        "release_url": result.release_url,
        "source": result.source,
        "artifact_url": result.artifact_url,
        "artifact_sha256": result.artifact_sha256,
        "fallback_error": result.fallback_error,
        "error": result.error,
        "update_available": bool(result.update_available),
    }


class FakeResponse:
    def __init__(self, body: bytes) -> None:
        self._body = body

    def read(self) -> bytes:
        return self._body

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *exc: object) -> bool:
        return False


def fake_urlopen_factory(pypi_desc: dict, github_desc: dict):
    """按 URL 分派：pypi.org 用 pypi 载荷，其余用 github 载荷。"""

    def fake_urlopen(request, timeout=None):  # noqa: ANN001, ARG001
        url = request.full_url if hasattr(request, "full_url") else str(request)
        chosen = pypi_desc if "pypi.org" in url else github_desc
        kind = chosen["kind"]
        if kind == "none":
            raise OSError("模拟网络故障")
        body = chosen["value"]
        text = body if isinstance(body, str) else json.dumps(body)
        return FakeResponse(text.encode("utf-8"))

    return fake_urlopen


def build_corpus() -> dict:
    from auto_model_key_router import update

    versions = version_cases()
    version_rows = []
    for value in versions:
        try:
            numbers = list(update.version_numbers(value))
        except Exception as exc:  # noqa: BLE001
            numbers = {"error": f"{type(exc).__name__}: {exc}"}
        try:
            comparable = list(update.comparable_version(value))
        except Exception as exc:  # noqa: BLE001
            comparable = {"error": f"{type(exc).__name__}: {exc}"}
        version_rows.append({"version": value, "numbers": numbers, "comparable": comparable})

    comparisons = []
    for latest in versions:
        for current in ("4.1.0", "5.0.0", "0.0.0"):
            try:
                value = update.is_newer_version(latest, current)
            except Exception as exc:  # noqa: BLE001
                value = {"error": f"{type(exc).__name__}: {exc}"}
            comparisons.append({"latest": latest, "current": current, "newer": value})

    http_rows = []
    real_urlopen = update.urlopen
    try:
        for name, route, pypi_payload, github_payload in http_cases():
            pypi_desc, github_desc = desc(pypi_payload), desc(github_payload)
            update.urlopen = fake_urlopen_factory(pypi_desc, github_desc)
            if route == "pypi":
                result = update.check_latest_pypi("4.1.0", 3.0)
            elif route == "github":
                result = update.check_latest_release("4.1.0", 3.0)
            else:
                result = update.check_latest_version("4.1.0", 3.0)
            # 标记「错误文本属语言相关」：载荷是坏 JSON 时，错误来自解析器，
            # Python 的 json.JSONDecodeError 文案与 Go 的 encoding/json 必然不同。
            # 这类用例 Go 侧只断言「有错/有 fallback_error」，不比文本。
            language_specific = any(
                item["kind"] == "text" for item in (pypi_desc, github_desc)
            ) and bool(result.error or result.fallback_error)
            http_rows.append(
                {
                    "name": name,
                    "route": route,
                    "pypi": pypi_desc,
                    "github": github_desc,
                    "language_specific_error": language_specific,
                    "result": serialize_result(result),
                }
            )
    finally:
        update.urlopen = real_urlopen

    return {
        "version": 1,
        "note": "由 scripts/gen_updatecheck_corpus.py 驱动真实 Python update.py 生成；HTTP 分支通过替换 update.urlopen 注入固定响应，故生成时不联网。",
        "source_constants": {
            "package_name": update.PACKAGE_NAME,
            "pypi_project_url": update.PYPI_PROJECT_URL,
            "pypi_json_api": update.PYPI_JSON_API,
            "github_repository": update.GITHUB_REPOSITORY,
            "github_releases_url": update.GITHUB_RELEASES_URL,
            "github_latest_release_api": update.GITHUB_LATEST_RELEASE_API,
        },
        "versions": version_rows,
        "comparisons": comparisons,
        "http": http_rows,
    }


def serialize(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=1) + "\n"


def main() -> int:
    corpus = build_corpus()
    text = serialize(corpus)
    if "--check" in sys.argv[1:]:
        if not OUTPUT.exists():
            print(f"语料缺失: {OUTPUT}", file=sys.stderr)
            return 1
        if OUTPUT.read_text(encoding="utf-8") != text:
            print(
                f"语料已过期: {OUTPUT}\n请运行 python scripts/gen_updatecheck_corpus.py 重新生成。",
                file=sys.stderr,
            )
            return 1
        print(f"语料最新: {OUTPUT}")
        return 0
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    with OUTPUT.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
    print(
        f"已写入 {OUTPUT}（版本 {len(corpus['versions'])} 条、比较 {len(corpus['comparisons'])} 条、"
        f"HTTP {len(corpus['http'])} 条）"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
