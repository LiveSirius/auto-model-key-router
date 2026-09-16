"""WebUI 本地预览服务器：用合成数据把静态前端跑起来，供人工/无头浏览器验收。

这不是产品代码，也不参与发布 —— 它的唯一用途是让 WebUI 在没有真实上游 Key
的机器上也能被打开检查：注入确定的合成指标（含失败、重试、缓存、缺口等边界），
并把 /metrics、/metrics/series、/health 以及管理接口的最小响应接上。

用法：
    python scripts/webui_preview.py [--port 8799]
然后打开 http://127.0.0.1:8799/ui/
"""

from __future__ import annotations

import argparse
import json
import math
import random
import sys
from datetime import datetime, timedelta
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

ROOT = Path(__file__).resolve().parents[1]
WEBUI = ROOT / "auto_model_key_router" / "webui"

MODELS = ["claude-sonnet-4-5", "gpt-5-codex", "gemini-2.5-pro", "deepseek-v3.2"]
KEYS = ["primary-a", "backup-b", "spare-c"]
UPSTREAMS = ["anthropic/claude-sonnet-4-5", "openai/gpt-5-codex", "google/gemini-2.5-pro"]
PROVIDERS = ["anthropic", "openai", "google"]


def stats(
    requests: int = 0,
    successes: int | None = None,
    failures: int = 0,
    retries: int = 0,
    prompt: int = 0,
    completion: int = 0,
    cached: int = 0,
    duration: int = 0,
    first_token: int = 0,
    status: dict[str, int] | None = None,
) -> dict:
    successes = requests - failures if successes is None else successes
    total = prompt + completion
    return {
        "requests": requests,
        "successes": successes,
        "failures": failures,
        "retries": retries,
        "prompt_tokens": prompt,
        "completion_tokens": completion,
        "total_tokens": total,
        "cached_tokens": cached,
        "cache_creation_input_tokens": 0,
        "cache_read_input_tokens": cached,
        "cached_token_rate": round(cached / prompt, 6) if prompt else 0.0,
        "total_duration_ms": duration,
        "avg_duration_ms": round(duration / requests) if requests else 0,
        "min_duration_ms": 320 if requests else 0,
        "max_duration_ms": 9800 if requests else 0,
        "total_first_token_ms": first_token,
        "avg_first_token_ms": round(first_token / requests) if requests else 0,
        "min_first_token_ms": 180 if requests else 0,
        "max_first_token_ms": 4200 if requests else 0,
        "status_codes": status or ({"200": successes} if successes else {}),
    }


# 固定种子：每次预览看到同一份数据，便于比对改动前后的视觉差异。
RNG = random.Random(20260101)


def jitter(epoch: float) -> float:
    """确定性噪声：同一时刻永远得到同一系数，预览逐次可比。

    刻意只用几十秒到几分钟周期的慢波，而不是"每秒乱跳"的伪随机：
    后端是按时间窗口聚合的，如果合成速率在秒级剧烈跳变，粗桶（如 15 分钟）
    采样到的值就会和细桶积分出来的总量对不上，看板上的 KPI 与曲线会互相矛盾。
    """
    slow = math.sin(epoch * 0.017) * 0.5 + math.sin(epoch * 0.043) * 0.3
    drift = math.sin(epoch * 0.0009) * 0.2
    return 1.0 + 0.17 * (slow + drift)


def rate_per_minute(epoch: float) -> float:
    """给定时刻的尝试速率（次/分）。以绝对时间为自变量。"""
    phase = (epoch % 21600) / 21600 * math.pi * 3  # 6 小时节律，一天四个峰谷
    base = 26 + 20 * math.sin(phase) + 8 * math.sin(phase * 4.1)
    return max(0.0, base * jitter(epoch))


def bucket_requests(epoch: int, bucket_seconds: int) -> int:
    """桶内请求数 = 速率在桶上的积分。

    按不超过 15 秒的步长取样求平均，而不是只取中点 ——
    粗桶（15 分钟）只用中点采样会丢掉桶内的起伏，与细桶求和的结果越差越远。
    """
    step = min(15, bucket_seconds)
    samples = max(1, bucket_seconds // step)
    total = 0.0
    for index in range(samples):
        total += rate_per_minute(epoch + (index + 0.5) * step)
    return max(0, round(total / samples * bucket_seconds / 60))


def build_series(hours: float, bucket_seconds: int) -> dict:
    now = datetime.now().astimezone()
    now_epoch = int(now.timestamp())
    start_epoch = now_epoch - int(hours * 3600)
    # 起点不对齐桶边界：从窗口起点开始平铺，严格覆盖 [start, now]。
    # 若把起点向下取整到桶边界，粗桶会多覆盖最多一整个桶（15 分钟桶多算 25%），
    # 于是"15 分钟桶求和"与快照总量对不上。
    anchor = start_epoch

    points = []
    epoch = anchor
    while epoch < now_epoch:
        started = datetime.fromtimestamp(epoch).astimezone()
        ended = started + timedelta(seconds=bucket_seconds)
        requests = bucket_requests(epoch, bucket_seconds)
        # 速率下限为 0，正弦谷底自然会出现接近空闲的时段，不再额外人为清零：
        # "按时间槽清零"会在粗桶下抹掉整段，让不同桶宽的窗口总量对不上。
        failures = 1 if requests and jitter(epoch * 1.7) > 1.08 else 0
        retries = min(requests, round(requests * 0.08 * jitter(epoch * 2.3)))
        prompt = requests * 2400
        completion = requests * 480
        cached = round(prompt * 0.26 * jitter(epoch * 3.1))
        # 延迟随负载走：请求越多越慢。写成常数会让延迟曲线是一条直线，
        # 视觉验收就看不出坐标轴、图例与实际形状对不对。
        load = min(2.0, requests / max(1.0, 26.0))
        duration = round(requests * 2600 * (0.7 + 0.6 * load) * jitter(epoch * 4.7))
        first_token = round(requests * 620 * (0.7 + 0.5 * load) * jitter(epoch * 5.9))
        point = {
            "started_at": started.isoformat(),
            "ended_at": ended.isoformat(),
            # 末尾那个桶还没走完：图表会把它画成虚线并标"累加中"。
            "complete": int(ended.timestamp()) <= now_epoch,
            **stats(
                requests=requests,
                failures=failures,
                retries=retries,
                prompt=prompt,
                completion=completion,
                cached=cached,
                duration=duration,
                first_token=first_token,
                status={"200": requests - failures, "429": failures},
            ),
        }
        points.append(point)
        epoch += bucket_seconds

    return {
        "count_semantics": "upstream_attempt",
        "window": {
            "from": datetime.fromtimestamp(start_epoch).astimezone().isoformat(),
            "to": now.isoformat(),
            "hours": hours,
        },
        "filters": {},
        "bucket_seconds": bucket_seconds,
        "points": points,
    }


def build_snapshot(hours: float = 1) -> dict:
    # 用 30 秒细桶聚合快照：结果与图表所用桶宽无关，两边读数才对得上。
    series = build_series(hours, 30)
    points = series["points"]
    agg = {
        "requests": sum(p["requests"] for p in points),
        "failures": sum(p["failures"] for p in points),
        "retries": sum(p["retries"] for p in points),
        "prompt": sum(p["prompt_tokens"] for p in points),
        "completion": sum(p["completion_tokens"] for p in points),
        "cached": sum(p["cached_tokens"] for p in points),
        "duration": sum(p["total_duration_ms"] for p in points),
        "first_token": sum(p["total_first_token_ms"] for p in points),
    }
    now = datetime.now().astimezone()
    # 实时速率取"此刻"的速率（60 秒滚动窗口），不是窗口平均值 —— 与后端语义一致。
    current_rpm = round(rate_per_minute(now.timestamp()))
    recent = stats(
        requests=current_rpm,
        prompt=current_rpm * 2400,
        completion=current_rpm * 480,
        cached=round(current_rpm * 2400 * 0.26),
    )
    return {
        "count_semantics": "upstream_attempt",
        "window": {"from": (now - timedelta(hours=hours)).isoformat(), "to": now.isoformat(), "hours": hours},
        "started_at": (now - timedelta(hours=6)).isoformat(),
        "database_path": "/tmp/metrics.sqlite3",
        "rate_window_seconds": 60,
        "current_rpm": recent["requests"],
        "current_tpm": recent["total_tokens"],
        "router_status": "green",
        "active_requests": 3,
        "total": stats(
            requests=agg["requests"],
            failures=agg["failures"],
            retries=agg["retries"],
            prompt=agg["prompt"],
            completion=agg["completion"],
            cached=agg["cached"],
            duration=agg["duration"],
            first_token=agg["first_token"],
            status={"200": agg["requests"] - agg["failures"], "429": agg["failures"] // 2, "500": agg["failures"] - agg["failures"] // 2},
        ),
        "caller_types": {
            # 每个维度都要带上 duration / cached / first_token：漏掉就会让
            # 分解表里的"平均耗时"算成 0ms，看起来像前端算错了。
            "local": stats(
                requests=int(agg["requests"] * 0.72),
                prompt=agg["prompt"] // 2,
                completion=agg["completion"] // 2,
                cached=agg["cached"] // 2,
                duration=agg["duration"] * 3 // 4,
                first_token=agg["first_token"] * 3 // 4,
            ),
            "visitor": stats(
                requests=agg["requests"] - int(agg["requests"] * 0.72),
                failures=agg["failures"],
                prompt=agg["prompt"] // 3,
                completion=agg["completion"] // 3,
                cached=agg["cached"] // 3,
                duration=agg["duration"] // 4,
                first_token=agg["first_token"] // 4,
            ),
        },
        "models": {
            model: stats(
                requests=max(1, agg["requests"] // len(MODELS)),
                prompt=agg["prompt"] // len(MODELS),
                completion=agg["completion"] // len(MODELS),
                cached=agg["cached"] // len(MODELS),
                duration=agg["duration"] // len(MODELS),
                first_token=agg["first_token"] // len(MODELS),
            )
            for model in MODELS
        },
        "requested_models": {},
        "model_requested_models": {},
        "keys": {
            model: {
                key: stats(
                    requests=max(1, agg["requests"] // (len(MODELS) * len(KEYS))),
                    prompt=agg["prompt"] // (len(MODELS) * len(KEYS)),
                    completion=agg["completion"] // (len(MODELS) * len(KEYS)),
                    cached=agg["cached"] // (len(MODELS) * len(KEYS)),
                    duration=agg["duration"] // (len(MODELS) * len(KEYS)),
                    first_token=agg["first_token"] // (len(MODELS) * len(KEYS)),
                )
                for key in KEYS
            }
            for model in MODELS
        },
        "providers": {
            provider: stats(
                requests=max(1, agg["requests"] // len(PROVIDERS)),
                prompt=agg["prompt"] // len(PROVIDERS),
                completion=agg["completion"] // len(PROVIDERS),
                cached=agg["cached"] // len(PROVIDERS),
                duration=agg["duration"] // len(PROVIDERS),
                first_token=agg["first_token"] // len(PROVIDERS),
            )
            for provider in PROVIDERS
        },
        "provider_pools": {},
        "upstream_models": {
            upstream: stats(
                requests=max(1, agg["requests"] // len(UPSTREAMS)),
                prompt=agg["prompt"] // len(UPSTREAMS),
                completion=agg["completion"] // len(UPSTREAMS),
                cached=agg["cached"] // len(UPSTREAMS),
                duration=agg["duration"] // len(UPSTREAMS),
                first_token=agg["first_token"] // len(UPSTREAMS),
            )
            for upstream in UPSTREAMS
        },
        "unattributed": stats(),
    }


def build_requests(hours: float, limit: int) -> dict:
    now = datetime.now().astimezone()
    items = []
    for index in range(limit):
        created = now - timedelta(seconds=index * RNG.randint(6, 40))
        failed = RNG.random() < 0.12
        model = RNG.choice(MODELS)
        prompt = RNG.randint(400, 9000)
        completion = RNG.randint(0, 1800)
        items.append({
            "id": 100000 - index,
            "created_at": created.isoformat(),
            "caller_type": RNG.choice(["local", "local", "local", "visitor"]),
            "model_id": model,
            "requested_model_id": model,
            "provider_id": RNG.choice(PROVIDERS),
            "pool_name": None,
            "upstream_model_id": RNG.choice(UPSTREAMS),
            "key_name": RNG.choice(KEYS),
            "status_code": RNG.choice([500, 429]) if failed else 200,
            "success": not failed,
            "retried": (not failed) and RNG.random() < 0.16,
            "prompt_tokens": prompt,
            "uncached_prompt_tokens": prompt // 2,
            "completion_tokens": completion,
            "total_tokens": prompt + completion,
            "cached_tokens": prompt // 3,
            "cache_creation_input_tokens": 0,
            "cache_read_input_tokens": prompt // 3,
            "first_token_ms": RNG.randint(180, 4200),
            "duration_ms": RNG.randint(420, 18000),
        })
    return {
        "count_semantics": "upstream_attempt",
        "window": {"from": (now - timedelta(hours=hours)).isoformat(), "to": now.isoformat(), "hours": hours},
        "filters": {},
        "rate_window_seconds": 60,
        "current_rpm": 28,
        "current_tpm": 41000,
        "summary": stats(requests=len(items)),
        "latest_request_at": now.isoformat(),
        "total_items": len(items),
        "items": items,
        "next_before_id": None,
    }


HEALTH = {
    "status": "ok",
    "version": "4.0.3",
    "models": MODELS,
    "config_path": "D:/Code/auto-model-key-router/router-config.json",
    "local_auth_enabled": False,
    "local_api_key_fingerprint": "000000000000",
    "visitor_feature_installed": True,
    "visitor_access_enabled": True,
    "visitor_key_count": 2,
    "unified_model": {
        "default": {"primary": {"model": "claude-sonnet-4-5", "key": None}, "fallback": {"model": "deepseek-v3.2", "key": "backup-b"}},
        "image": {"primary": {"model": "gemini-2.5-pro", "key": None}},
    },
    "native_endpoint_states": {
        "anthropic": {"supported": True, "reason": None, "path": "/v1/messages"},
        "openai": {"supported": True, "reason": None, "path": "/v1/chat/completions"},
        "responses": {"supported": False, "reason": "unsupported", "path": None},
    },
    "webui_available": True,
    "webui_enabled": True,
    "webui_mounted": True,
    "webui_path": "/ui",
}
HEALTH["base_url"] = "http://127.0.0.1:8799"


PROVIDERS_PAYLOAD = [
    {
        "id": "anthropic",
        "base_url": "https://api.anthropic.com",
        "routes": {"openai": None, "anthropic": "/v1/messages", "responses": None, "images": None},
        "keys": [
            {
                "name": "primary-a",
                "enabled": True,
                "allow_visitor": True,
                "capabilities": {
                    "models": ["claude-sonnet-4-5", "claude-haiku-4-5"],
                    "errors": {"responses": "upstream 404"},
                    "checked_at": datetime.now().astimezone().isoformat(),
                },
            },
            {
                "name": "backup-b",
                "enabled": True,
                "allow_visitor": False,
                "capabilities": None,
            },
        ],
    },
    {
        "id": "openai",
        "base_url": "https://api.openai.com",
        "routes": {"openai": None, "anthropic": None, "responses": "/v1/responses", "images": "/v1/images/generations"},
        "keys": [
            {
                "name": "spare-c",
                "enabled": True,
                "allow_visitor": False,
                "capabilities": {"models": ["gpt-5-codex"], "errors": {}, "checked_at": datetime.now().astimezone().isoformat()},
            },
        ],
    },
]


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(WEBUI), **kwargs)

    def log_message(self, fmt, *args):  # 静默：预览时刷屏没意义
        pass

    def _json(self, payload: dict, status: int = 200) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler 接口
        parsed = urlparse(self.path)
        path = parsed.path
        query = parse_qs(parsed.query)

        if path == "/ui":
            self.send_response(302)
            self.send_header("Location", "/ui/")
            self.end_headers()
            return
        if path.startswith("/ui/"):
            # 静态资源直接映射到 webui/ 目录，并去掉前缀。
            self.path = path[len("/ui"):] + (f"?{parsed.query}" if parsed.query else "")
            if self.path in ("/", ""):
                self.path = "/index.html"
            return super().do_GET()

        if path == "/health":
            return self._json(HEALTH)
        # 鉴权模式下，只有带正确 Bearer 的请求才放行受保护接口 —— 否则前端拿到的
        # 永远是 200，整页验证页根本没有机会出现。
        if HEALTH["local_auth_enabled"] and (path.startswith("/api/") or path.startswith("/metrics")):
            if self.headers.get("Authorization") != "Bearer preview-key":
                return self._json({"detail": "本地 API key 验证失败"}, 401)
        if path == "/metrics":
            hours = float(query.get("hours", ["1"])[0])
            return self._json(build_snapshot(hours))
        if path == "/metrics/series":
            hours = float(query.get("hours", ["1"])[0])
            bucket = int(query.get("bucket_seconds", ["60"])[0])
            return self._json(build_series(hours, bucket))
        if path == "/metrics/requests":
            hours = float(query.get("hours", ["24"])[0])
            limit = int(query.get("limit", ["50"])[0])
            return self._json(build_requests(hours, limit))
        if path == "/api/providers":
            return self._json({"config_revision": "preview-rev", "providers": PROVIDERS_PAYLOAD})
        if path == "/api/settings":
            return self._json({
                "config_revision": "preview-rev",
                "settings": {
                    "host": "127.0.0.1", "port": 8799, "max_retries": 2,
                    "request_timeout": 120, "stream_first_byte_timeout": 60,
                    "stream_idle_timeout": 60, "local_auth_enabled": False,
                },
            })
        if path == "/api/logs":
            lines = [
                f"{datetime.now().astimezone().isoformat()} INFO  proxy  upstream={RNG.choice(UPSTREAMS)} status=200 duration={RNG.randint(400, 4000)}ms",
                f"{datetime.now().astimezone().isoformat()} WARN  key_pool key={RNG.choice(KEYS)} 429 rate limited, cooling down 12s",
                f"{datetime.now().astimezone().isoformat()} ERROR proxy  upstream responded 500, retrying with next key",
                f"{datetime.now().astimezone().isoformat()} DEBUG metrics recorded attempt model={RNG.choice(MODELS)}",
            ] * 12
            return self._json({"text": "\n".join(lines), "error": None, "truncated": False})
        if path == "/api/tool":
            return self._json({"version": "4.0.3", "webui_enabled": True, "webui_mounted": True, "latest_version": "4.0.3", "update_available": False})
        if path == "/api/routes":
            return self._json({
                "config_revision": "preview-rev",
                "routes": [
                    {
                        "id": "claude-sonnet-4-5",
                        "aliases": ["sonnet", "claude-sonnet"],
                        "hidden_aliases": ["anthropic/claude-sonnet-4-5"],
                        "routing_mode": "round_robin",
                        "targets": [
                            {"provider": "anthropic", "key": "primary-a", "upstream_model": "claude-sonnet-4-5"},
                            {"provider": "anthropic", "key": "backup-b", "upstream_model": "claude-sonnet-4-5-20250929"},
                        ],
                    },
                    {"id": "gpt-5-codex", "aliases": [], "hidden_aliases": [], "routing_mode": None, "targets": [{"provider": "openai", "key": "spare-c", "upstream_model": "gpt-5-codex"}]},
                ],
            })
        if path == "/api/models":
            return self._json({
                "config_revision": "preview-rev",
                "models": [
                    {
                        "id": model,
                        "aliases": [],
                        "hidden_aliases": [],
                        "routing_mode": None,
                        "reasoning_effort": None,
                        "auto_hidden_aliases": [],
                        "keys": [{"name": key, "enabled": True, "allow_visitor": key == "primary-a"} for key in KEYS],
                    }
                    for model in MODELS
                ],
            })
        if path == "/api/unified-model":
            return self._json({"config_revision": "preview-rev", "unified_model": HEALTH["unified_model"]})
        if path == "/api/integrations":
            return self._json({"integrations": [
                {"agent": "claude-code", "display_name": "Claude Code", "target_path": "~/.claude/settings.json", "target_exists": True, "backup_available": True, "current_is_applied": True, "mode": "unified-model", "error": None},
                {"agent": "codex", "display_name": "Codex", "target_path": "~/.codex/config.toml", "target_exists": True, "backup_available": False, "current_is_applied": False, "mode": None, "error": None},
                {"agent": "pi-agent", "display_name": "Pi Agent", "target_path": "~/.pi/config.json", "target_exists": False, "backup_available": False, "current_is_applied": False, "mode": None, "error": None},
            ]})
        return self._json({"detail": f"预览服务器未实现该接口: {path}"}, 404)


def main() -> int:
    parser = argparse.ArgumentParser(description="WebUI 本地预览服务器")
    parser.add_argument("--port", type=int, default=8799)
    parser.add_argument("--host", default="127.0.0.1")
    # 打开鉴权才能看到整页验证页；默认关闭是为了直接落到看板，省一次粘贴 Key。
    parser.add_argument("--auth", action="store_true", help="让 /health 报告已启用本地鉴权（用于预览验证页）")
    args = parser.parse_args()

    if args.auth:
        HEALTH["local_auth_enabled"] = True

    if not (WEBUI / "index.html").is_file():
        print(f"未找到 WebUI 资产: {WEBUI}", file=sys.stderr)
        return 1

    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"WebUI 预览已启动: http://{args.host}:{args.port}/ui/")
    print("按 Ctrl+C 停止。")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n已停止。")
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
