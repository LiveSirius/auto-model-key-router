#!/usr/bin/env python3
"""生成 ``auto_model_key_router.ops_api``（运维面 7 条路由）的差分对拍语料。

这个脚本驱动**真实的** Python FastAPI 应用（``create_app(..., enable_ops=True)`` +
``starlette.testclient.TestClient``），对一个临时配置文件逐条发请求，把「状态码 +
响应体字节 + content-type / content-length + 请求后的配置文件」记进
``internal/api/testdata/ops_api_corpus.json``；Go 侧用 httptest 重放同一批请求并逐
字节比对。

设计要点：

1. **固定路径**。所有用例共用同一个临时目录
   （``%TEMP%/amkr_ops_api_corpus``），每个用例开始前只重置受影响的文件。配置文件
   路径会出现在 409 的 ``detail`` 里，用 mkdtemp 会让语料随运行变化，``--check``
   就永远失败。夹具里的所有路径都落在该目录下，Go 侧回放时整体替换成自己的临时
   目录（与运维面无关的差异只有目录名）。
2. **必须打补丁的外部依赖**。版本检查会发网络请求，语料把它换成确定性桩
   （``update.check_latest_version``）；``ops_api._tail`` 与 ``webui.webui_available``
   也在个别用例里被替换，用来确定性地覆盖 OSError 分支与「静态资产缺失」分支：
   - ``update.check_latest_version``      -> 固定 VersionCheckResult
   - ``ops_api._tail``                    -> 抛 OSError（只读失败分支）
   - ``webui.webui_available``            -> 强制 False（资产未随包发布）
3. **三条运维路由不进入语料**：``GET /api/integrations``（读真实 ~/.claude 等）、
   ``POST /api/integrations/{agent}`` 与 ``.../rollback`` 的成功路径（会改写开发者
   机器上的真实 Agent 配置，且结果依赖本机状态）。这些只覆盖「路由级校验」——
   未知 Agent 的 404、非法 mode 的 422、无鉴权的 401——它们都不触碰文件系统。
   Go 侧未接线的接缝由 handlers_ops_test.go 的具名用例断言「响亮失败」。
4. **``$REV`` 占位符**。``WebUIRequest`` 继承 ``APIModel``，因此也接受
   ``config_revision``（运维面并不做乐观并发校验）。需要它的用例写 ``"$REV"``，
   发请求前替换成从磁盘算出的当前版本。
5. **``log_file`` 规格化**。日志文件内容用 ``prefix_hex`` / ``pad_byte`` /
   ``pad_count`` / ``suffix_hex`` 描述，两侧各自展开——否则 64 KiB 截断边界的用例
   要在语料里存两份 64 KiB 字节。``body_text`` 仍然逐字节记录，那才是被对拍的语义。

用法::

    python -X utf8 scripts/gen_ops_api_corpus.py
    python -X utf8 scripts/gen_ops_api_corpus.py --check
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import sys
import tempfile
import warnings
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

# TestClient 在 **import 时**就会为 httpx/httpx2 的迁移打印弃用警告（类型是
# StarletteDeprecationWarning，它继承 UserWarning 而不是 DeprecationWarning），
# 因此过滤器必须排在星标导入之前并按消息匹配；语料生成只需要干净的 stdout。
warnings.filterwarnings(
    "ignore",
    category=UserWarning,
    message=r"Using `httpx` with `starlette\.testclient` is deprecated",
)

from starlette.testclient import TestClient  # noqa: E402

from auto_model_key_router import __version__, ops_api, update, webui  # noqa: E402
from auto_model_key_router.app import create_app  # noqa: E402
from auto_model_key_router.config import (  # noqa: E402
    RouterConfig,
    migrate_config_data,
    save_config_data,
)
from auto_model_key_router.update import VersionCheckResult  # noqa: E402

DEFAULT_PATH = REPO_ROOT / "internal" / "api" / "testdata" / "ops_api_corpus.json"
CORPUS_VERSION = 1

BASE_DIR = Path(tempfile.gettempdir()) / "amkr_ops_api_corpus"
CONFIG_PATH = BASE_DIR / "router-config.json"
LOG_PATH = BASE_DIR / "server.log"
LOG_DIR_PATH = BASE_DIR / "logs-dir"
STALE_REVISION = "stale-revision-0000000000000000"

FULL_AUTH = {"Authorization": "Bearer local-key"}
VISITOR_AUTH = {"Authorization": "Bearer amkr-visitor"}

# 版本检查桩的可变状态：单个用例可以覆盖它。
UPDATE_STATE: dict[str, Any] = {"latest": "1.3.0", "error": None}

# 原实现引用，用于每个用例开始前恢复补丁。
_ORIGINAL_TAIL = ops_api._tail
_ORIGINAL_WEBUI_AVAILABLE = webui.webui_available


def fixture() -> dict:
    """所有用例共用的初始配置。"""
    return {
        "config_version": 4,
        "host": "127.0.0.1",
        "port": 8000,
        "request_timeout": 10,
        "stream_first_byte_timeout": 30,
        "stream_idle_timeout": 60,
        "max_retries": 1,
        "key_failure_threshold": 1,
        "key_cooldown_seconds": 60,
        "endpoint_capabilities_path": str(BASE_DIR / "endpoint-capabilities.json"),
        "upstream_health_check_interval": 0,
        "metrics_db_path": str(BASE_DIR / "metrics.sqlite3"),
        "log_file_path": str(LOG_PATH),
        "local_api_key": "local-key",
        # 默认关闭：这样 POST /api/tool/webui {"enabled": true} 能同时观察到
        # webui_enabled 变化而 webui_mounted 保持进程启动时的取值。
        "webui_enabled": False,
        "providers": {},
        "models": {},
    }


def canonical_dumps(obj: object) -> str:
    """sort_keys + 紧凑分隔符，与 Go 的 ``canonical.Dumps`` 逐字节对齐。"""
    return json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def current_revision() -> str:
    data = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    payload = canonical_dumps(migrate_config_data(data))
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def substitute(value: object) -> object:
    """把 $REV / $STALE 占位符换成真实版本号。"""
    if isinstance(value, str):
        if value == "$REV":
            return current_revision()
        if value == "$STALE":
            return STALE_REVISION
        return value
    if isinstance(value, dict):
        return {key: substitute(item) for key, item in value.items()}
    if isinstance(value, list):
        return [substitute(item) for item in value]
    return value


# —— 日志文件规格 —— #


def log_bytes(data: bytes) -> dict:
    """整段字面字节。"""
    return {"prefix_hex": data.hex(), "pad_byte": None, "pad_count": 0, "suffix_hex": ""}


def log_pad(prefix: bytes, pad_byte: int, pad_count: int, suffix: bytes = b"") -> dict:
    """prefix + pad_byte * pad_count + suffix，用来紧凑表达 64 KiB 级用例。"""
    return {
        "prefix_hex": prefix.hex(),
        "pad_byte": pad_byte,
        "pad_count": pad_count,
        "suffix_hex": suffix.hex(),
    }


def materialize(spec: dict | None) -> bytes:
    if spec is None:
        return b""
    out = bytes.fromhex(spec.get("prefix_hex") or "")
    if spec.get("pad_byte") is not None:
        out += bytes([spec["pad_byte"]]) * int(spec.get("pad_count") or 0)
    out += bytes.fromhex(spec.get("suffix_hex") or "")
    return out


# —— 依赖补丁 —— #


def patch_dependencies() -> None:
    def fake_check(current_version: str = "0.0.0", timeout: float = 3.0) -> VersionCheckResult:
        if UPDATE_STATE["error"]:
            return VersionCheckResult(current_version="1.2.3", error=UPDATE_STATE["error"])
        return VersionCheckResult(
            current_version="1.2.3",
            latest_version=UPDATE_STATE["latest"],
            release_url="https://example.test/release",
            source="pypi",
        )

    update.check_latest_version = fake_check


def patch_case(entry: dict) -> None:
    """按用例恢复/替换可变补丁。"""
    UPDATE_STATE["latest"] = entry["update_state"].get("latest", "1.3.0")
    UPDATE_STATE["error"] = entry["update_state"].get("error")

    if entry["webui_assets"]:
        webui.webui_available = _ORIGINAL_WEBUI_AVAILABLE
    else:
        webui.webui_available = lambda: False  # noqa: E731 - 语料桩

    if entry["tail_error"]:
        message = entry["tail_error"]

        def raising(path: Path, limit: int) -> tuple[str, bool]:
            raise OSError(message)

        ops_api._tail = raising
    else:
        ops_api._tail = _ORIGINAL_TAIL


# —— 用例定义 —— #

CASES: list[dict] = []


def case(
    name: str,
    method: str,
    path: str,
    *,
    body: object = None,
    has_body: bool = False,
    auth: str = "full",
    covers: list[str] | None = None,
    log_file: dict | None = None,
    log_path_dir: bool = False,
    fixture_patch: dict | None = None,
    delete_config: bool = False,
    ops_enabled: bool = True,
    update_state: dict | None = None,
    webui_assets: bool = True,
    tail_error: str | None = None,
) -> None:
    CASES.append(
        {
            "name": name,
            "method": method,
            "path": path,
            "body": {"kind": "json", "value": body} if has_body else {"kind": "none"},
            "auth": auth,
            "covers": covers or [],
            "log_file": log_file,
            "log_path_dir": log_path_dir,
            "fixture_patch": fixture_patch or {},
            "delete_config": delete_config,
            "ops_enabled": ops_enabled,
            "update_state": update_state or {},
            "webui_assets": webui_assets,
            "tail_error": tail_error,
        }
    )


def build_cases() -> None:
    # —— GET /api/logs ——
    case("logs/missing-file", "GET", "/api/logs", covers=["GET /api/logs"])
    case("logs/empty-file", "GET", "/api/logs", log_file=log_bytes(b""))
    case("logs/short-file", "GET", "/api/logs", log_file=log_bytes(b"hello\nworld\n"))
    # 非法字节：Python 的 errors="replace" 每个最大子部分各产一个 U+FFFD。
    case("logs/non-utf8", "GET", "/api/logs", log_file=log_bytes(b"caf\xc3\xa9 \xff\xfe ok\n"))
    # 恰好等于窗口：size > limit 为假，不截断（边界是 > 而不是 >=）。
    case("logs/exactly-window", "GET", "/api/logs", log_file=log_pad(b"", 0x61, 65536))
    case("logs/over-window", "GET", "/api/logs", log_file=log_pad(b"", 0x61, 65537))
    # 截断边界切在多字节字符中间：从 offset=4 起读，首个最大子部分是截断的 中
    # （e4 b8 + 非续字节 X），按 Unicode 的最大子部分规则只产**一个** U+FFFD，
    # 其后是字面量 X。
    case(
        "logs/boundary-truncated-multibyte",
        "GET",
        "/api/logs",
        log_file=log_pad(b"abcd\xe4\xb8X", 0x61, 65533),
    )
    # 日志路径是目录：is_file() 为假，走「空内容」分支而不是报错。
    case(
        "logs/path-is-dir",
        "GET",
        "/api/logs",
        log_file=None,
        log_path_dir=True,
        fixture_patch={"log_file_path": str(LOG_DIR_PATH)},
    )
    # 配置文件不在磁盘上时仍然用 runtime 快照服务（_authorized_config 不读盘）。
    case("logs/config-missing", "GET", "/api/logs", log_file=log_bytes(b"x\n"), delete_config=True)
    # 读取失败（OSError）必须是 200 + error 字符串，而不是 500。
    case("logs/read-error", "GET", "/api/logs", log_file=log_bytes(b"x\n"), tail_error="模拟读取失败")
    case("logs/no-auth", "GET", "/api/logs", auth="none")
    case("logs/visitor", "GET", "/api/logs", auth="visitor")

    # —— GET /api/tool ——
    case("tool/get", "GET", "/api/tool", covers=["GET /api/tool"])
    case("tool/get-update-not-available", "GET", "/api/tool", update_state={"latest": "1.0.0"})
    case(
        "tool/get-update-error",
        "GET",
        "/api/tool",
        update_state={"error": "PyPI 检查失败: 网络不可达；GitHub 检查失败: 网络不可达"},
    )
    case(
        "tool/get-webui-enabled",
        "GET",
        "/api/tool",
        fixture_patch={"webui_enabled": True},
    )
    case("tool/get-webui-unavailable", "GET", "/api/tool", webui_assets=False)
    case("tool/no-auth", "GET", "/api/tool", auth="none")
    case("tool/visitor", "GET", "/api/tool", auth="visitor")

    # —— POST /api/tool/webui ——
    case(
        "tool/webui/enable",
        "POST",
        "/api/tool/webui",
        body={"enabled": True},
        has_body=True,
        covers=["POST /api/tool/webui"],
    )
    case(
        "tool/webui/disable",
        "POST",
        "/api/tool/webui",
        body={"enabled": False},
        has_body=True,
        fixture_patch={"webui_enabled": True},
    )
    case(
        "tool/webui/enable-with-revision",
        "POST",
        "/api/tool/webui",
        body={"config_revision": "$REV", "enabled": True},
        has_body=True,
    )
    # Pydantic 的宽松解析：字符串 "true" 也是合法 bool。
    case(
        "tool/webui/enable-string",
        "POST",
        "/api/tool/webui",
        body={"enabled": "true"},
        has_body=True,
    )
    case(
        "tool/webui/enable-with-assets-missing",
        "POST",
        "/api/tool/webui",
        body={"enabled": True},
        has_body=True,
        webui_assets=False,
    )
    case("tool/webui/missing-enabled", "POST", "/api/tool/webui", body={}, has_body=True)
    case("tool/webui/enabled-null", "POST", "/api/tool/webui", body={"enabled": None}, has_body=True)
    case("tool/webui/enabled-int", "POST", "/api/tool/webui", body={"enabled": 2}, has_body=True)
    case(
        "tool/webui/extra-field",
        "POST",
        "/api/tool/webui",
        body={"enabled": True, "x": 1},
        has_body=True,
    )
    case("tool/webui/no-body", "POST", "/api/tool/webui")
    case("tool/webui/body-list", "POST", "/api/tool/webui", body=[1], has_body=True)
    # 鉴权先于配置文件存在性检查：配置不在磁盘上时也必须是 401 而不是 409。
    case(
        "tool/webui/config-missing",
        "POST",
        "/api/tool/webui",
        body={"enabled": True},
        has_body=True,
        delete_config=True,
    )
    case(
        "tool/webui/config-missing-no-auth",
        "POST",
        "/api/tool/webui",
        body={"enabled": True},
        has_body=True,
        delete_config=True,
        auth="none",
    )
    case(
        "tool/webui/no-auth",
        "POST",
        "/api/tool/webui",
        body={"enabled": True},
        has_body=True,
        auth="none",
    )
    case(
        "tool/webui/visitor",
        "POST",
        "/api/tool/webui",
        body={"enabled": True},
        has_body=True,
        auth="visitor",
    )

    # —— POST /api/service/{action} ——
    # 动作名校验在鉴权**之前**，所以无凭据也返回 422。
    case(
        "service/unsupported-action",
        "POST",
        "/api/service/nope",
        covers=["POST /api/service/{action}"],
    )
    case("service/unsupported-action-no-auth", "POST", "/api/service/nope", auth="none")
    case("service/unsupported-action-visitor", "POST", "/api/service/nope", auth="visitor")
    case("service/empty-action", "POST", "/api/service/")
    case("service/extra-segment", "POST", "/api/service/start_amkr/x")

    # —— Agent 集成（只覆盖路由级校验，不碰真实 Agent 配置）——
    case(
        "integrations/apply-unknown-agent",
        "POST",
        "/api/integrations/nope",
        body={"mode": "native"},
        has_body=True,
        covers=["POST /api/integrations/{agent}"],
    )
    case(
        "integrations/apply-unknown-agent-no-auth",
        "POST",
        "/api/integrations/nope",
        body={"mode": "native"},
        has_body=True,
        auth="none",
    )
    case(
        "integrations/apply-bad-mode",
        "POST",
        "/api/integrations/claude-code",
        body={"mode": "bad"},
        has_body=True,
    )
    # 三个合法 Agent 各来一条非法 mode：只要白名单漏掉任何一个，这里就会从 422 变成
    # 404（Agent 检查在 mode 检查之前），从而把 SUPPORTED_AGENTS 的正反两面都钉死。
    case(
        "integrations/apply-bad-mode-codex",
        "POST",
        "/api/integrations/codex",
        body={"mode": "bad"},
        has_body=True,
    )
    case(
        "integrations/apply-bad-mode-pi-agent",
        "POST",
        "/api/integrations/pi-agent",
        body={"mode": "bad"},
        has_body=True,
    )
    case(
        "integrations/apply-bad-mode-no-auth",
        "POST",
        "/api/integrations/claude-code",
        body={"mode": "bad"},
        has_body=True,
        auth="none",
    )
    # 请求体校验先于 Agent 白名单：未知 Agent + 多余字段是 422 而不是 404。
    case(
        "integrations/apply-extra-field-first",
        "POST",
        "/api/integrations/nope",
        body={"mode": "native", "x": 1},
        has_body=True,
    )
    case("integrations/apply-missing-body", "POST", "/api/integrations/claude-code")
    case("integrations/apply-body-list", "POST", "/api/integrations/claude-code", body=[1], has_body=True)
    case(
        "integrations/rollback-unknown-agent",
        "POST",
        "/api/integrations/nope/rollback",
        covers=["POST /api/integrations/{agent}/rollback"],
    )
    case(
        "integrations/rollback-unknown-agent-no-auth",
        "POST",
        "/api/integrations/nope/rollback",
        auth="none",
    )
    # GET /api/integrations 的完整响应依赖开发者机器上的真实 Agent 配置，无法逐字节
    # 固化；这里只锁定鉴权门（未接入的接缝由 Go 用例断言）。
    case(
        "integrations/list-no-auth",
        "GET",
        "/api/integrations",
        auth="none",
        covers=["GET /api/integrations"],
    )
    case("integrations/list-visitor", "GET", "/api/integrations", auth="visitor")

    # —— ops 关闭：整条 URL 空间不存在 ——
    case("ops-disabled/logs", "GET", "/api/logs", ops_enabled=False)
    case("ops-disabled/tool", "GET", "/api/tool", ops_enabled=False)
    case(
        "ops-disabled/service",
        "POST",
        "/api/service/nope",
        ops_enabled=False,
    )
    case(
        "ops-disabled/webui",
        "POST",
        "/api/tool/webui",
        body={"enabled": True},
        has_body=True,
        ops_enabled=False,
    )
    case("ops-disabled/integrations", "GET", "/api/integrations", ops_enabled=False)


# —— 执行 —— #


def reset_case_files() -> None:
    """只重置受影响的文件。

    Windows 上对上一轮的 metrics.sqlite3 句柄会让整目录 rmtree 失败，而那个文件不
    参与任何运维响应，因此按文件清理而不是整目录重建。
    """
    BASE_DIR.mkdir(parents=True, exist_ok=True)
    for path in (CONFIG_PATH, LOG_PATH):
        if path.exists():
            path.unlink()
    if LOG_DIR_PATH.exists():
        shutil.rmtree(LOG_DIR_PATH, ignore_errors=True)


def effective_fixture(entry: dict) -> dict:
    data = fixture()
    data.update(entry["fixture_patch"])
    return data


def prepare_log(entry: dict) -> None:
    if entry["log_path_dir"]:
        LOG_DIR_PATH.mkdir(parents=True, exist_ok=True)
        return
    if entry["log_file"] is not None:
        LOG_PATH.write_bytes(materialize(entry["log_file"]))


def config_after() -> str | None:
    if not CONFIG_PATH.is_file():
        return None
    return canonical_dumps(json.loads(CONFIG_PATH.read_text(encoding="utf-8")))


def run_case(entry: dict) -> dict:
    reset_case_files()
    patch_case(entry)
    data = effective_fixture(entry)
    save_config_data(CONFIG_PATH, migrate_config_data(data))
    prepare_log(entry)

    app = create_app(RouterConfig.load(CONFIG_PATH), CONFIG_PATH, enable_ops=entry["ops_enabled"])
    result: dict[str, Any] = {
        "name": entry["name"],
        "method": entry["method"],
        "path": entry["path"],
        "auth": entry["auth"],
        "covers": entry["covers"],
        "fixture_patch": entry["fixture_patch"],
        "log_file": entry["log_file"],
        "log_path_dir": entry["log_path_dir"],
        "delete_config": entry["delete_config"],
        "ops_enabled": entry["ops_enabled"],
        "update_state": entry["update_state"],
        "webui_assets": entry["webui_assets"],
        "tail_error": entry["tail_error"],
    }
    with TestClient(app, raise_server_exceptions=False) as client:
        headers = dict(FULL_AUTH)
        if entry["auth"] == "none":
            headers = {}
        elif entry["auth"] == "visitor":
            headers = dict(VISITOR_AUTH)

        payload = substitute(entry["body"]["value"]) if entry["body"]["kind"] == "json" else None
        result["body"] = (
            {"kind": "json", "value": payload}
            if entry["body"]["kind"] == "json"
            else {"kind": "none"}
        )
        if entry["delete_config"]:
            CONFIG_PATH.unlink()

        if entry["body"]["kind"] == "json":
            response = client.request(
                entry["method"],
                entry["path"],
                content=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
                headers={**headers, "content-type": "application/json"},
            )
        else:
            response = client.request(entry["method"], entry["path"], headers=headers)

        result["status"] = response.status_code
        result["content_type"] = response.headers.get("content-type")
        result["content_length"] = response.headers.get("content-length")
        result["body_text"] = response.content.decode("utf-8")
        result["config_after"] = config_after()
    return result


# 7 条运维路由的「方法 + 模式」清单；与 internal/api/handlers_ops.go 的
# opsRoutePatterns() 手工对齐。
ROUTE_PATTERNS = [
    "GET /api/logs",
    "GET /api/tool",
    "POST /api/tool/webui",
    "POST /api/service/{action}",
    "GET /api/integrations",
    "POST /api/integrations/{agent}",
    "POST /api/integrations/{agent}/rollback",
]


def build_corpus() -> dict:
    patch_dependencies()
    build_cases()
    results = [run_case(entry) for entry in CASES]
    covered = {pattern for result in results for pattern in result["covers"]}
    missing = sorted(set(ROUTE_PATTERNS) - covered)
    if missing:
        raise SystemExit(f"语料未覆盖以下路由: {missing}")
    return {
        "version": CORPUS_VERSION,
        "note": (
            "由 scripts/gen_ops_api_corpus.py 驱动真实 Python ops_api 生成；"
            "fixture_dir 是生成时的固定临时目录，Go 侧回放时应整体替换成自己的临时目录。"
            "log_file 用 prefix_hex/pad_byte/pad_count/suffix_hex 规格化，"
            "body_text 仍逐字节记录。"
        ),
        "fixture_dir": str(BASE_DIR),
        "app_version": __version__,
        "fixture": fixture(),
        "case_count": len(results),
        "cases": results,
    }


def render(corpus: dict) -> str:
    return json.dumps(corpus, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="只校验磁盘语料是否与实现一致")
    parser.add_argument("--out", type=Path, default=DEFAULT_PATH)
    args = parser.parse_args()

    corpus = build_corpus()
    text = render(corpus)
    if args.check:
        if not args.out.is_file():
            print(f"语料缺失: {args.out}", file=sys.stderr)
            return 1
        existing = args.out.read_text(encoding="utf-8")
        if existing != text:
            print(f"语料已过期: {args.out}", file=sys.stderr)
            return 1
        print(f"语料一致（{corpus['case_count']} 条）")
        return 0
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(text, encoding="utf-8", newline="\n")
    print(f"已写入 {args.out}（{corpus['case_count']} 条）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
