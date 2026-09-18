# Auto Model Key Router

一个本地 OpenAI-compatible API 路由器：把多个模型和多个上游 API Key 统一收口到本地服务，自动分流、失败切换、统计调用，并可一键接入 Claude Code / Codex。

## 主要能力

- **按模型管理 Key**：Key 属于供应商（`providers.*.keys`），模型通过 `targets[]`（`{provider, key, upstream_model}`）绑定一个或多个 Key；同一 Key 可服务多个模型，支持 `round_robin`、`priority`、`only_first`。
- **失败切换与冷却**：遇到 `401/403/429/5xx` 等可重试错误时自动重试或切换 Key，并在进程内临时冷却异常 Key。
- **统一模型名**：客户端固定请求 `unified-model`，真实模型和固定 Key 可在路由器侧随时切换。
- **任务路由**：把任务名（`TASK_XXXXXX`）直接当模型名传，由路由器决定用哪个模型（首选 + 备选）和哪组采样参数；调用方改不了这些参数。
- **每个 Key 独立探测**：模型清单按 Key 缓存（同一供应商不同 Key 可见模型可能不同），添加 Key 时自动探测该 Key，可在 TUI 或管理 API 手动刷新。
- **OpenAI-compatible 代理**：支持 `/v1/chat/completions`、`/v1/models`，并兼容 Claude Code 的 `/v1/messages` 与 Codex 的 `/v1/responses`；可为不同协议模式配置上游额外路径。
- **WebUI 管理**：浏览器里配置供应商与 Key、模型、统一模型、服务注册和客户端接入；`amkr` 的交互式终端界面已随 Python 版一并退役。
- **访客 Key**：用固定访客 Key `amkr-visitor` 暴露受限公共模型（该功能现为常驻，不再需要额外安装步骤）。
- **统计与日志**：记录本地/访客调用、模型、Key、状态码、token、重试、延迟等指标。

## 安装

AMKR 是单个 Go 二进制：配置、WebUI 静态资产（`//go:embed`）与 SQLite 驱动的指标库都编在里面，
运行时不需要 Python、node 或额外的运行时依赖。

### 从源码构建

需要 Go 1.24+。

```bash
git clone https://github.com/sparr68/auto-model-key-router.git
cd auto-model-key-router
go build ./cmd/amkr          # 产出 amkr（Windows 上是 amkr.exe）
./amkr --version
```

### Docker

```bash
docker build -t amkr .
docker run -d --name amkr -p 8000:8000 -v amkr-data:/data amkr
```

镜像里配置、指标库与日志都落在 `/data`（`XDG_CACHE_HOME`），挂一个卷即可整体持久化。
容器内以 `--host 0.0.0.0` 启动 —— 配置默认监听 `127.0.0.1`，不改的话端口映射进不来。

### 首次运行

```bash
./amkr --show-config      # 看一眼当前配置摘要与配置文件路径
./amkr --serve-foreground # 前台启动（不带参数时也是这个行为）
```

启动后打开 `http://127.0.0.1:<port>/ui` 使用 WebUI 配置供应商、Key 与模型。

### 常用命令

```bash
./amkr --show-address          # 查询监听地址与服务地址
./amkr --show-api-key          # 获取本地授权 Key
./amkr --status                # 查看后台服务状态
./amkr --service install-user  # 注册为用户级服务（Windows 计划任务 / systemd user unit）
./amkr --stop                  # 停止后台服务
./amkr --check-update          # 检查是否有新版本
```

`./amkr --help` 可看到全部参数。
### 可选 WebUI

AMKR 自带一套可选的浏览器管理界面。**资产随软件包一起安装，没有单独的安装步骤，也没有额外依赖**，只由开关决定是否启用：

```bash
# 启动服务时启用（同时写入配置）
amkr --config router-config.json --webui

# 关闭
amkr --config router-config.json --no-webui
```

也可以不写配置文件，直接在 TUI 的「CLI 设置 → WebUI」里切换。启用后访问 `http://127.0.0.1:8000/ui/`（端口以实际配置为准）。管理接口照常要求本地鉴权 Key；`/ui/` 的挂载在服务启动时完成，通过接口改动开关需要重启服务才会生效。

### Docker

发布时会把镜像推到 GHCR（`ghcr.io/sparrived/auto-model-key-router`，标签为版本号，正式版额外带 `latest`）。注意 GHCR 的容器包默认是**私有**，且可见性**不随仓库继承**（包只继承仓库的访问权限，不含可见性 —— 本仓库公开不代表镜像能匿名拉取），目前也没有 API 能改，只能在包页面手动改一次：

> 包页面 → 右下角 **Danger Zone** → **Change visibility** → **Public**（按提示输入包名确认；改公开后不能改回私有）

改之前 `docker pull` 会要求登录（`403 Forbidden`）。发布工作流来自仓库自身、包会自动关联到仓库，通常无需手动操作即可匿名拉取，但以包页面显示的实际可见性为准。

也可以自己从仓库构建：

```bash
docker build -t amkr .

docker run -d --name amkr \
  -p 8000:8000 \
  -v amkr-data:/data \
  amkr
```

首次启动会在卷里自动生成配置和本地授权 Key。之后照常进入 TUI 配置（`-it` 分配终端，必须带）：

```bash
docker exec -it amkr amkr --config /data/auto-model-key-router/router-config.json
```

只取本地授权 Key 时用非交互命令：

```bash
docker exec amkr amkr --config /data/auto-model-key-router/router-config.json --get-key
```

配置、指标库、日志和 PID 文件都在 `/data/auto-model-key-router` 下，删容器不丢数据。容器内固定监听 `0.0.0.0`（否则端口映射进不去），**因此务必保留 `local_api_key`，不要把端口暴露到公网**；需要改端口时改配置里的 `port`，再同步调整 `-p`。

容器里建议关掉运维接口：`/api/logs`、`/api/tool`、`/api/service/*`、`/api/integrations/*` 作用于「服务所在的这台机器」（读日志文件、启停进程、注册系统服务、改写本机 Claude Code / Codex 配置），在容器或反向代理后面语义不成立，逐个路径拉黑又容易漏：

```bash
amkr --config /data/auto-model-key-router/router-config.json --no-ops
```

开关写入配置字段 `ops_enabled`，重启后生效；关闭后这些路径返回 `404`，`/health` 的 `ops_enabled` 字段也会变成 `false`，便于部署时断言。代理、`/health`、`/metrics`、WebSocket 与 `/api/settings` 等配置管理接口不受影响。

## 快速开始

### 1. 启动 Terminal UI

```bash
amkr
```

首次启动会在系统缓存目录自动创建配置文件和本地鉴权 Key。你也可以复制示例配置到当前目录：

```bash
cp router-config.example.json router-config.json
amkr --config router-config.json
```

Windows PowerShell 可使用：

```powershell
copy router-config.example.json router-config.json
amkr --config router-config.json
```

### 2. 配置供应商、Key 与模型

在 TUI 中进入：

1. **供应商 → 添加供应商**：输入供应商 ID、Base URL 与第一个 Key 的 API Key。添加时自动探测这个新 Key（可用模型列表 + 各路由可用性）并据此建立可服务模型；以后每添加一个 Key 都会只探测该新 Key（不同 Key 可见模型可能不同）。
2. **模型设置**：管理模型别名、隐藏别名、路由模式、绑定/解绑 Key（绑定 Key 时可指定上游模型名）。
3. **统一模型**：把 `unified-model` 指向一个真实模型，必要时固定到某个 Key。
4. **任务路由**（WebUI）：为 `TASK_XXXXXX` 指定模型与固定采样参数。
5. **一键配置 → 路由服务**：启动或注册本地代理服务。
6. **一键配置 → Claude Code / Codex / Pi Agent**：按需自动写入客户端配置。

### 3. 调用本地代理

默认服务地址是：

```text
http://127.0.0.1:8000
```

请求示例：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer amkr_your-local-api-key" \
  -d '{
    "model": "unified-model",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

也可以把 `model` 写成真实模型 ID、模型 alias，或 `模型ID[key name]` 来显式指定某个 Key。

如果配置了任务路由，还可以直接传任务名：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer amkr_your-local-api-key" \
  -d '{
    "model": "TASK_000001",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

任务名对应的模型、备选模型和采样参数都在 AMKR 侧固定，调用方不需要知道真实模型名。详见 [`docs/USAGE.md`](docs/USAGE.md#9-任务路由)。

## 常用命令

```bash
# 打开 TUI
amkr

# 使用指定配置文件打开 TUI
amkr --config router-config.json

# 后台启动 / 查看状态 / 停止
auto-model-key-router --config router-config.json --serve
auto-model-key-router --config router-config.json --status
auto-model-key-router --config router-config.json --stop

# 注册、管理系统服务
auto-model-key-router --config router-config.json --install-service
auto-model-key-router --config router-config.json --service status
auto-model-key-router --config router-config.json --service restart

# 查询 AMKR 监听 IP 和端口
auto-model-key-router --config router-config.json --show-address

# 获取当前 AMKR 的本地授权 Key（也可使用 --show-api-key）
auto-model-key-router --config router-config.json --get-key

# 查看配置摘要、日志与统计
auto-model-key-router --config router-config.json --show-config
auto-model-key-router --config router-config.json --show-logs 50

# 启用 / 关闭内置 WebUI（访问 http://127.0.0.1:8000/ui/）
auto-model-key-router --config router-config.json --webui
auto-model-key-router --config router-config.json --no-webui

# 管理 unified-model
auto-model-key-router --config router-config.json --show-unified-model
auto-model-key-router --config router-config.json --switch-model gpt-4o-mini
auto-model-key-router --config router-config.json --switch-key auto
```

## 配置示例

```json
{
  "config_version": 4,
  "host": "127.0.0.1",
  "port": 8000,
  "request_timeout": 60,
  "stream_first_byte_timeout": 60,
  "stream_idle_timeout": 60,
  "max_retries": 2,
  "key_failure_threshold": 2,
  "key_cooldown_seconds": 60,
  "local_api_key": "amkr_your-local-api-key",
  "providers": {
    "openai": {
      "base_url": "https://api.openai.com",
      "routes": {
        "openai": "v1/chat/completions",
        "responses": "v1/responses",
        "images": "v1/images/generations",
        "embeddings": "v1/embeddings"
      },
      "keys": {
        "main": {
          "api_key": "sk-your-first-upstream-key",
          "capabilities": {
            "models": ["gpt-4o-mini"],
            "route_status": {"openai": "ok", "anthropic": "ok", "responses": "ok"},
            "errors": {},
            "checked_at": "2026-01-01T00:00:00+00:00"
          }
        },
        "backup": {
          "api_key": "sk-your-second-upstream-key",
          "allow_visitor": true,
          "capabilities": {
            "models": ["gpt-4o-mini"],
            "route_status": {"openai": "ok", "anthropic": "ok", "responses": "ok"},
            "errors": {},
            "checked_at": "2026-01-01T00:00:00+00:00"
          }
        }
      }
    },
    "tokenplan": {
      "base_url": "https://example.com/tokenplan",
      "routes": {"anthropic": "anthropic/"},
      "keys": {
        "mimo": {"api_key": "sk-your-third-upstream-key"}
      }
    }
  },
  "models": {
    "gpt-4o-mini": {
      "aliases": ["fast-mini"],
      "routing_mode": "round_robin",
      "reasoning_effort": "medium",
      "targets": [
        {"provider": "openai", "key": "main", "upstream_model": "gpt-4o-mini"},
        {"provider": "tokenplan", "key": "mimo", "upstream_model": "gpt-4o-mini"}
      ]
    },
    "text-embedding-3-small": {
      "targets": [
        {"provider": "openai", "key": "main", "upstream_model": "text-embedding-3-small"}
      ]
    }
  },
  "unified_model": {
    "default": {
      "primary": {"model": "gpt-4o-mini", "key": null}
    },
    "embeddings": {
      "primary": {"model": "text-embedding-3-small", "key": null}
    }
  },
  "tasks": {
    "TASK_000001": {
      "model": "gpt-4o-mini",
      "fallback_model": null,
      "params": {"temperature": 0.2, "top_p": 0.9}
    }
  }
}
```

> `local_api_key` 是客户端访问本地 AMKR 的 Key；`providers.*.keys.*.api_key` 是真实供应商 Key；模型通过 `models.*.targets[]` 按 `{provider, key, upstream_model}` 粒度绑定供应商 Key，`upstream_model` 是发给上游的真实模型名（默认同本地模型 ID）。`tasks` 是可选的任务路由表，键即客户端传的 `model` 名（如 `TASK_000001`），值为 `{model, fallback_model?, params?}`；`params` 支持的键为 `temperature`、`top_p`、`top_k`、`frequency_penalty`、`presence_penalty`、`seed`、`stop`、`reasoning_effort`，写错键名会在保存时报错。探测缓存按 Key 存放在 `providers.*.keys.<key>.capabilities`（`models` 为该 Key 探测到的可服务模型清单，`route_status` 为各协议路由的可用性，`errors` / `checked_at` 记录探测错误与时间）；同一供应商的不同 Key 可见模型可能不同，因此每个 Key 独立探测、缓存互不复用。添加 Key 时自动探测该新 Key（探测失败仍会保存 Key，可稍后手动刷新），之后可在 TUI「供应商 → 刷新能力探测」（全部 Key 或指定 Key、可限端点范围）或管理 API 的 probe 接口手动刷新。探测缓存是机器本地信息，配置导出/粘贴（transferable_config）不会携带。旧版 v1/v2/v3 配置会在加载时自动迁移为 v4 并写回，无需手工修改；v3 池级探测元数据与旧 v4 供应商级缓存会折进各 Key 的 capabilities。

流式请求使用分段超时：`stream_first_byte_timeout`（默认 60 秒）覆盖等待上游响应头和第一块响应体的总时间，`stream_idle_timeout`（默认 60 秒）限制首块之后相邻响应块的等待时间，两者都必须大于 0。响应头返回前超时会按现有重试策略切换 Key；下游流建立后超时只结束当前流，不会自动重放请求。可在 TUI 的 **CLI 设置 → 超时配置** 中统一调整普通请求和两个流式超时。

## 文档

- [完整使用教程](docs/USAGE.md)：从安装、配置、启动、请求到 Claude Code / Codex 接入的完整流程。
- [CLI 参考](docs/CLI.md)：所有命令行参数与示例。
- [HTTP API 参考](docs/API.md)：代理、健康检查、统计和管理接口。
- [更新日志](CHANGELOG.md)：版本变更记录。
- [配置示例](router-config.example.json)：可复制修改的完整 JSON 示例。

## 访客 Key 简介

可以用固定 Key `amkr-visitor` 暴露受限公共模型（该功能现为常驻）。只有设置了 `allow_visitor: true` 的上游 Key 才能被访客使用，访客看到的模型名格式为 `amkr-{真实模型ID}`。

详细限制和示例见 [完整使用教程：使用访客 Key](docs/USAGE.md#14-使用访客-key)。

## 开发

需要 Go 1.24+（前端资产用 `//go:embed` 编进二进制，无需 node/python 即可构建）。

```bash
git clone https://github.com/sparr68/auto-model-key-router.git
cd auto-model-key-router
go build ./cmd/amkr     # 产出 amkr（Windows 上是 amkr.exe）
go test ./...           # 全部包
```

前端资产的检查（可选，只在改动 `webui/` 时需要）：

```bash
node scripts/webui_module_check.mjs      # 16 个 ES 模块的加载检查
node webui/probes/webui_chart_probe.mjs  # 图表口径
node webui/probes/webui_tip_probe.mjs
node webui/probes/webui_auth_probe.mjs
```

## 安全提示

- 不要把真实上游 API Key 提交到 Git。
- `local_api_key` 为空会关闭本地鉴权；仅建议在可信本机环境使用。
- `amkr --get-key` / `--show-api-key` 会直接输出本地授权 Key，请勿在共享终端、日志或 CI 输出中执行。
- 如果监听 `0.0.0.0` 或暴露到局域网/公网，请务必启用本地鉴权并配置防火墙。

## License

MIT
