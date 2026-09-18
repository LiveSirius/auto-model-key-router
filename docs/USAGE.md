# Auto Model Key Router 使用教程

本文是一份从零开始的完整使用教程，覆盖安装、配置模型与 Key、启动服务、发送请求、查看统计与成本估算、访客 Key、统一模型切换、任务路由，以及 Claude Code / Codex / Pi Agent 接入。

如果只想查 CLI 参数或 HTTP API 字段，请参考 [`CLI.md`](CLI.md) 和 [`API.md`](API.md)。

---

## 1. 适用场景

Auto Model Key Router（简称 AMKR）是一个本地 OpenAI-compatible API 路由服务。它适合：

- 给同一个模型配置多个上游 API key，并自动分流。
- 在某个 key 限流、鉴权失败或上游异常时自动切换到其他 key。
- 给 Claude Code、Codex、Pi Agent 或其他 OpenAI-compatible 客户端提供一个稳定的本地入口。
- 使用固定模型名 `unified-model`，在路由器里随时切换真实模型或指定 key，避免反复改客户端配置。
- 为不同任务固化模型与采样参数：客户端传 `TASK_XXXXXX`，由路由器决定用哪个模型、用什么参数。
- 统计本地调用、访客调用、重试、状态码、token 和耗时。

---

## 2. 安装

AMKR 需要 Python `>=3.12`。

推荐用 `pipx` 或 `uv tool` 安装成独立命令行工具。两者都会把 `amkr` 安装到自己的 bin 目录；如果该目录尚未在 PATH 中，安装成功后仍可能提示找不到命令：

使用 pipx：

```bash
pipx install auto-model-key-router
pipx ensurepath
```

或使用 uv：

```bash
uv tool install auto-model-key-router
uv tool update-shell
```

执行 PATH 设置命令后，请关闭并重新打开终端，再运行：

```bash
amkr --version
auto-model-key-router --version
```

Windows PowerShell 中可用 `uv tool dir --bin` 或 `pipx environment --value PIPX_BIN_DIR` 查看需要加入 PATH 的目录；macOS/Linux 中可用 `command -v amkr` 检查命令位置。PATH 变化不会自动刷新已经打开的终端、IDE 或服务进程。

如果暂时只想确认包本身可以运行，不依赖当前 shell 的 PATH，可使用：

```bash
uvx --from auto-model-key-router amkr --version
```

若 `uvx` 成功而 `amkr` 失败，就是 PATH 激活问题，不是项目没有提供命令入口。使用 `pipx list` 或 `uv tool list` 可确认工具是否已安装。

如需启用访客 Key 功能，请安装 `visitor` extra：

```bash
pipx install "auto-model-key-router[visitor]"
# 或
uv tool install "auto-model-key-router[visitor]"
```

---

## 3. 准备配置文件

### 3.1 默认配置路径

不传 `--config` 时，AMKR 默认读写系统缓存目录中的 `router-config.json`：

| 系统 | 默认目录 |
| --- | --- |
| Windows | `%LOCALAPPDATA%\AutoModelKeyRouter\` |
| macOS | `~/Library/Caches/AutoModelKeyRouter/` |
| Linux | `${XDG_CACHE_HOME:-~/.cache}/auto-model-key-router/` |

首次启动时，如果配置文件不存在，程序会自动创建空配置，并生成本地鉴权 Key。

### 3.2 项目目录配置

如果你想把配置放在当前目录，复制示例配置即可：

```bash
# Windows PowerShell / CMD
copy router-config.example.json router-config.json

# macOS / Linux
cp router-config.example.json router-config.json
```

后续命令统一加上：

```bash
--config router-config.json
```

### 3.3 最小可用配置

一个可工作的配置至少需要：

- `host` / `port`：本地监听地址和端口。
- `local_api_key`：客户端访问本地代理时使用的鉴权 Key；留空表示不启用本地鉴权，不推荐暴露到非可信网络。
- `providers`：供应商（含 `base_url`）及至少一个 `keys`（供应商级 API Key）。
- `models`：真实模型列表；每个模型至少有一个 `targets[]`，通过 `{provider, key, upstream_model}` 绑定一个供应商 Key。可选 `aliases`（会出现在 `/v1/models`）与 `hidden_aliases`（可调用但不列出，见 [3.x 同一个模型的多个名字](#同一个模型的多个名字隐藏别名)）。

示例：

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
      "keys": {
        "main": {"api_key": "sk-your-first-upstream-key"},
        "backup": {"api_key": "sk-your-second-upstream-key"}
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
        {"provider": "openai", "key": "backup", "upstream_model": "gpt-4o-mini"}
      ]
    }
  },
  "unified_model": {
    "default": {
      "primary": {"model": "gpt-4o-mini", "key": null}
    }
  }
}
```

> 注意：`local_api_key` 是调用 AMKR 本地服务的 Key；`providers.*.keys.*.api_key` 是 AMKR 转发到上游时使用的真实供应商 Key；`targets[]` 的 `key` 引用同供应商下已声明的 Key，`upstream_model` 是发送给上游的模型名（不写则默认为本地模型 ID）。v1/v2/v3 的旧格式配置会在加载时自动迁移到 v4 并写回。

### 3.4 请求与流式超时

- `request_timeout` 控制连接建立、请求写入和非流式请求。
- `stream_first_byte_timeout` 默认 60 秒，从发起流式上游请求开始，覆盖等待响应头和第一块响应体的总时间。
- `stream_idle_timeout` 默认 60 秒，控制收到第一块后相邻响应块的最大等待时间。

三个值都应大于 0。流式响应头返回前超时时，下游响应尚未建立，AMKR 会按现有重试策略切换 Key；下游流建立后发生首块或空闲超时时，只结束当前流，不会自动重放请求，以免产生重复事件、重复计费或非幂等工具调用。可在 TUI 的 **CLI 设置 → 超时配置** 中统一修改这三个值。

---

## 4. 用 Terminal UI 配置和管理

启动 Terminal UI：

```bash
amkr --config router-config.json
```

不加 `--config` 时会管理默认缓存目录中的配置。

TUI 里最常用的入口：

| 菜单 | 主要用途 |
| --- | --- |
| 一键配置 | 注册路由服务，或自动配置 Claude Code / Codex / Pi Agent 使用 AMKR |
| 供应商 | 添加供应商与 Key、管理 Key、刷新能力探测（可按 Key 与端点范围）、设置 Base URL / 路由 |
| 模型设置 | 管理模型别名、隐藏别名、路由模式，把供应商 Key 绑定/解绑到模型、修改上游模型名 |
| 统一模型 | 设置 `unified-model` 当前指向的真实模型，并选择自动路由或固定 Key |
| CLI 设置 | 管理监听地址、端口、本地鉴权、请求超时、配置迁移、版本更新等 |

> 任务路由（[第 9 节](#9-任务路由)）目前只在 WebUI 的 **配置 → 任务路由** 页和管理 API 中维护，TUI 没有对应菜单。

推荐的新手流程：

1. 进入 **供应商 → 添加供应商**，输入供应商 ID、Base URL 和第一个 API Key；AMKR 会自动探测这个新 Key 可服务的模型，多选后自动建立本地模型并完成绑定（同一供应商以后再添加 Key 也会只探测该新 Key，互不复用探测结果）。
2. 给同一模型继续添加 Key，或给模型绑定其他供应商的 Key：进入 **模型设置** 选择模型，用「管理 Key / 绑定 Key」调整。
3. 进入 **统一模型**，把 `unified-model` 指向该模型。
4. 进入 **一键配置 → 路由服务**，启动或注册本地路由服务。
5. 用客户端请求 `http://127.0.0.1:8000/v1/...`，模型名可以写真实模型、别名、`unified-model` 或任务名 `TASK_XXXXXX`。

> **能力探测与缓存（按 Key 独立）**：探测缓存按 Key 保存（磁盘为 `providers.<id>.keys.<key>.capabilities`，含该 Key 的模型清单 `models`、各路由可用性 `route_status`、`errors` 与 `checked_at`）。同一供应商的不同 Key 能访问的模型集可能不同（如免费/付费额度、不同订阅），所以每次添加 Key 时只探测这个新 Key，结果不复用、不折叠。
>
> 手动刷新：在 **供应商** 菜单选择「刷新能力探测」→ 「1 刷新全部 Key」或「2 指定 Key」（指定 Key 后可再选端点范围：「1 全部路由模式 / 2 仅 Chat (openai) / 3 仅 Messages (anthropic) / 4 仅 Responses」），「0 返回」。刷新只更新探测缓存，不会改动模型与绑定；管理 API 对应入口见 [`docs/API.md`](API.md) 的 probe 接口。

---

## 5. 启动本地代理

### 5.1 后台启动

```bash
auto-model-key-router --config router-config.json --serve
```

查看状态和停止：

```bash
auto-model-key-router --config router-config.json --status
auto-model-key-router --config router-config.json --stop
```

后台服务会写入 `server.pid`，默认和日志文件在同一个缓存目录。

### 5.2 开机自启 / 系统服务

一键注册：

```bash
auto-model-key-router --config router-config.json --install-service
```

或使用统一服务命令：

```bash
auto-model-key-router --config router-config.json --service install
auto-model-key-router --config router-config.json --service install-user
auto-model-key-router --config router-config.json --service status
auto-model-key-router --config router-config.json --service start
auto-model-key-router --config router-config.json --service stop
auto-model-key-router --config router-config.json --service restart
auto-model-key-router --config router-config.json --service uninstall
```

- Windows：默认注册为计划任务 `AutoModelKeyRouter`；管理员权限不足时会尝试弹出 UAC。
- Linux：注册为 systemd user service `auto-model-key-router.service`。

---

## 6. 发送第一个请求

启动服务后，请求 OpenAI-compatible Chat Completions：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer amkr_your-local-api-key" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [
      {"role": "user", "content": "hello"}
    ]
  }'
```

也可以使用别名：

```json
{
  "model": "fast-mini",
  "messages": [{"role": "user", "content": "hello"}]
}
```

如果已经配置 `unified_model`，推荐客户端固定使用：

```json
{
  "model": "unified-model",
  "messages": [{"role": "user", "content": "hello"}]
}
```

如果配置了任务路由，`model` 也可以直接写任务名（见 [第 9 节](#9-任务路由)）：

```json
{
  "model": "TASK_000001",
  "messages": [{"role": "user", "content": "hello"}]
}
```

AMKR 会在转发给上游前把请求体里的模型名改写为真实模型 ID，并把 `Authorization` 替换成选中的上游 `api_key`。

### 同一个模型的多个名字（隐藏别名）

同一模型在不同地方经常叫不同名字（例如本地叫 `deepseek-flash`，别处叫 `deepseek-v4.1-flash`）。除了会出现在 `/v1/models` 里的 `aliases`，还有一类「隐藏别名」：**可以直接调用，但不会出现在 `/v1/models` 与 `/health` 中**。两种来源：

1. **自动（通常不需要任何配置）**：每个 target 的 `upstream_model` 会自动成为隐藏别名。上面 `gpt-4o-mini` 的 target 上游名是 `gpt-4o-mini-2024-07-18`，于是客户端可以直接用这个名字调用，但 `/v1/models` 只列出 `gpt-4o-mini` 及其 `aliases`。绑定 Key 时填的上游模型名即是这个用途，不需要再手工登记一遍。
2. **手动**：模型级 `hidden_aliases` 列表，用于上游名之外还想额外接受的叫法。

```json
{
  "models": {
    "gpt-4o-mini": {
      "aliases": ["fast-mini"],
      "hidden_aliases": ["mini-latest"],
      "targets": [
        {
          "provider": "openai",
          "key": "main",
          "upstream_model": "gpt-4o-mini-2024-07-18"
        }
      ]
    }
  }
}
```

上例中 `fast-mini`（别名）会出现在 `/v1/models`；`mini-latest`（手写隐藏别名）与 `gpt-4o-mini-2024-07-18`（上游名，自动获得隐藏别名待遇）都只可调用、不列出。

规则：

- 隐藏别名只在**本地调用**时生效。visitor 只能用 `amkr-{真实模型ID}`，看不到也用不了隐藏别名。
- 名字冲突时真实模型 ID 与 `aliases` 优先；多个模型指向同一上游名时按 `models` 顺序取第一个匹配。
- 手写的 `hidden_aliases` 参与重名校验：与任何模型 ID、`aliases` 或其他模型的手写隐藏别名冲突都会报错。
- 在 TUI「模型设置 → 隐藏别名」或 WebUI 的模型路由页可以编辑手写隐藏别名；自动推导的部分无需维护（同步调整绑定 Key 时的上游模型名即可）。

---

## 7. 路由模式与失败切换

每个模型可以设置 `routing_mode`：

| 模式 | 适合场景 | 行为 |
| --- | --- | --- |
| `round_robin` | 多 Key 均衡分流 | 按配置顺序轮询可用 Key |
| `priority` | 主备、成本优先 | 优先使用靠前 Key，失败后再尝试后面的 Key |
| `only_first` | 只允许第一个 Key | 只使用第一个 Key；可重试错误按 `max_retries` 重试 |

以下状态码会触发重试或切换：

```text
401, 403, 429, 500, 502, 503, 504
```

冷却规则：

- `429` 会立即让当前 Key 进入冷却。
- 其他可重试错误达到 `key_failure_threshold` 后进入冷却。
- 上游返回 `Retry-After` 时严格使用该冷却时间；否则按连续失败次数递增 `key_cooldown_seconds`，最长 300 秒。
- 自动失败只产生临时冷却，不会自动永久禁用 Key。
- 冷却和失败计数仅保存在内存中；冷却到期后请求会自然恢复，任意成功请求会立即清空失败状态。
- Key 健康状态不对外暴露，也不接受人工清除；长期启停请修改配置中的 `enabled`。
- 旧配置字段 `upstream_health_check_interval` 仍可读取，但已弃用且不再启动后台健康探测。

### 原生 Anthropic 端点优先

对于 Anthropic Messages 请求（`/v1/messages`），可设置 `native_first` 控制是否优先使用原生格式：

```json
{
  "id": "claude-3-opus",
  "native_first": true,
  "targets": [
    {"provider": "anthropic", "key": "main", "upstream_model": "claude-3-opus"}
  ]
}
```

| 设置 | 行为 |
| --- | --- |
| `true`（默认） | 优先以原生格式发送到上游 `/v1/messages`，保留所有 Anthropic 字段（`cache_control`、`prompt_cache_key` 等），提高缓存命中率 |
| `false` | 直接转换为 `/v1/chat/completions` 格式 |

原生优先模式工作流程：
1. 首次请求时自动测试上游是否支持 `/v1/messages` 端点
2. 测试结果按“上游 URL + 实际原生路径”记录在 `endpoint-capabilities.json` 的 `endpoint_capabilities` 中
3. 如果上游返回 404/405/501，自动回退到 `chat/completions` 格式并记录结果
4. 如需重新测试，可删除 `endpoint-capabilities.json` 中对应的 `endpoint_capabilities` 条目

如果某个上游的 Anthropic 入口不是 `base_url/v1/messages`，可以按上游 URL 配置额外路由：

```json
{
  "upstream_routes": {
    "https://example.com/tokenplan": {
      "anthropic": "anthropic/"
    }
  }
}
```

`anthropic/` 会被规范化为 `anthropic/v1/messages`，请求会发到 `https://example.com/tokenplan/anthropic/v1/messages`。也可以直接写完整相对路径，例如 `anthropic/v1/messages`。

---

## 8. 统一模型 `unified-model`

`unified-model` 是 AMKR 的固定虚拟模型名。客户端一直请求它，真实模型和 Key 在 AMKR 侧切换。

查看当前指向：

```bash
auto-model-key-router --config router-config.json --show-unified-model
```

切换目标模型：

```bash
auto-model-key-router --config router-config.json --switch-model gpt-4o-mini
```

切换到某个模型并固定 Key：

```bash
auto-model-key-router --config router-config.json --switch-model gpt-4o-mini --switch-key openai-backup
```

只切换当前目标模型使用的 Key：

```bash
auto-model-key-router --config router-config.json --switch-key openai-main
```

恢复自动路由：

```bash
auto-model-key-router --config router-config.json --switch-key auto
```

说明：

- `--switch-model` 接受真实模型 ID 或 alias，写回配置时会规范化为真实模型 ID。
- 如果切换到另一个模型且未传 `--switch-key`，旧的固定 Key 会自动清空，避免误用。
- `unified_model` 只引用现有模型和 Key，不会复制或新增上游 Key。
- 配置中不能把真实模型 ID 或 alias 命名为保留名 `unified-model`。
- `unified_model` 下可选的 `image` 与 `embeddings` 计划把图像、嵌入请求指向各自的模型：`/v1/images/*` 用 `image`，`/v1/embeddings` 用 `embeddings`，其余请求用 `default`。未配置对应计划时该路径继承 `default.primary`（不继承 `default.fallback`）。用 `--unified-target` 指定要改的计划，例如把嵌入切到另一个模型：

```bash
auto-model-key-router --config router-config.json --switch-model text-embedding-3-small --unified-target embeddings.primary
```

---

## 9. 任务路由

`unified-model` 适合「客户端固定一个入口、模型在 AMKR 侧切换」；任务路由解决的是另一个问题：**不同任务该用不同模型和不同的采样参数**。把模型名与参数一起固化在服务端，客户端只要传任务名，就不必（也不能）关心这些细节。

在 WebUI 的 **配置 → 任务路由** 页新建一个任务：

| 字段 | 说明 |
| --- | --- |
| 任务名 | 客户端要传的 `model`，如 `TASK_000001`；不能与模型 ID、别名、隐藏别名或 `unified-model` 撞名 |
| 首选模型 | 任务实际调用的模型 |
| 备选模型 | 可选；首选模型重试失败后自动切换，响应带 `X-AMKR-Fallback: true` |
| 推理强度 | 可选，`none`…`max` |
| 固定采样参数 | 可选，`temperature`、`top_p`、`top_k`、`frequency_penalty`、`presence_penalty`、`seed`、`stop` |

调用时把 `model` 写成任务名：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer amkr_your-local-api-key" \
  -d '{
    "model": "TASK_000001",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

说明：

- 任务名是**虚拟模型名**，不会出现在 `/v1/models` 里。这与「隐藏别名」不同：隐藏别名仍是一个模型的名字，任务名则是一整组路由与参数的别名；如果需要客户端直接调用的名字能在 `/v1/models` 里看到，请改用模型的别名或隐藏别名。
- 任务 `params` 里固定了的采样参数会覆盖请求体中的同名参数；请求里**显式传了**这些参数会直接返回 `400`，而不是被静默忽略。`max_tokens` 等不在列表里的参数照常透传。
- `reasoning_effort` 是唯一例外：任务同样会覆盖它，但不拒绝调用方传入 —— Claude Code、Codex 这类客户端框架会自动带上这个字段，拒绝等于让任务路由不可用。
- 任务不接受调用方指定 Key（`TASK_000001[main]` 返回 `400`）。Key 仍由目标模型自身的路由模式（`round_robin` / `priority` / `only_first`）决定，与第 7 节一致。
- 参数名写错（如 `temprature`）会在**保存时**就报错，不必等到请求时才发现。
- 走原生 Anthropic（`/v1/messages`）时，固定的 `stop` 会以 `stop_sequences` 发给上游 —— 那是 Anthropic 的叫法，发 `stop` 会被静默忽略。`reasoning_effort` 在原生路径上不发（与模型级设置一致）。
- 访客 Key 不能访问任务名。
- 任务写在配置文件的顶层 `tasks` 键下，可选；删掉某个模型时，引用它的任务会被自动清理（首选模型没了删整个任务，只有备选没了则退化为单模型任务）。

---

## 10. 显式指定某个 Key

如果只想让单次请求使用某个 Key，可以把 `model` 写成：

```text
模型ID[key name]
别名[key name]
unified-model[key name]
```

示例：

```json
{
  "model": "fast-mini[openai-backup]",
  "messages": [{"role": "user", "content": "hello"}]
}
```

这次请求会：

1. 先把 `fast-mini` 解析到真实模型 `gpt-4o-mini`。
2. 在该模型绑定的 Key（`targets[]` 展开后的 Key）中找到 `name` 为 `openai-backup` 的那个，并转发给对应的供应商。
3. 转发给上游时仍使用真实模型 ID `gpt-4o-mini`（`upstream_model` 与本地模型 ID 不同时使用前者）。

同一模型下 Key 名称必须非空且唯一；Key 由供应商管理，多个模型可以共用同一个供应商 Key（解析后可能显示为 `供应商ID-Key名` 的限定名称）。

---

## 11. 本地鉴权

配置里有 `local_api_key` 时，下列接口需要鉴权：

- `/v1/models`
- `/v1/{path}` 代理接口
- `/metrics`
- `/api/*` 管理接口

支持两种传法：

```http
Authorization: Bearer amkr_your-local-api-key
```

或：

```http
x-api-key: amkr_your-local-api-key
```

`/health` 不需要鉴权。

如果 `local_api_key` 为空，则本地接口不启用鉴权。只有在完全可信的本机环境中才建议这样做；如果监听地址改成 `0.0.0.0`，请务必启用鉴权并配置防火墙。

---

## 12. 查看健康状态和模型列表

健康检查：

```bash
curl http://127.0.0.1:8000/health
```

模型列表：

```bash
curl http://127.0.0.1:8000/v1/models \
  -H "Authorization: Bearer amkr_your-local-api-key"
```

`/health` 会返回服务状态、配置路径、本地鉴权状态、公开模型、Key 指纹、冷却状态等信息。

---

## 13. 查看统计和日志

命令行查看配置摘要：

```bash
auto-model-key-router --config router-config.json --show-config
```

查看最近运行日志和调用统计：

```bash
auto-model-key-router --config router-config.json --show-logs
# 指定最近 50 行日志
auto-model-key-router --config router-config.json --show-logs 50
```

HTTP 查看聚合统计：

```bash
curl http://127.0.0.1:8000/metrics \
  -H "Authorization: Bearer amkr_your-local-api-key"
```

查看最近一小时的分钟时间桶和最近 24 小时调用明细：

```bash
curl "http://127.0.0.1:8000/metrics/series?hours=1&bucket_seconds=60" \
  -H "Authorization: Bearer amkr_your-local-api-key"

curl "http://127.0.0.1:8000/metrics/requests?hours=24&limit=50" \
  -H "Authorization: Bearer amkr_your-local-api-key"
```

调用明细传入 `all_history=true` 可与 CLI 的“全部”时间范围保持一致。

统计会持久化写入 SQLite，默认文件为缓存目录下的 `metrics.sqlite3`。返回数据包含：

- 总请求数、成功、失败、重试。
- prompt / completion / total tokens。
- 缓存命中和缓存 token 统计。
- 总耗时、平均耗时、首 token 耗时。
- 状态码分布。
- 按真实模型、请求模型名、Key、本地调用和访客调用拆分的聚合。
- 按供应商、上游模型和 Key 拆分的调用明细与聚合。
- 补零的服务端时间桶，以及带稳定游标的逐次上游调用明细。

升级前（v3 及更早）写入的历史统计行可能带有模型池（pool）归因，v4 不再产生该字段；这类历史数据会与无归因数据一起按 `unattributed` 汇总，不会根据当前配置反推，也不会参与当前配置的模型池概念。调用明细和时间桶支持 `attributed=true|false` 筛选。

---

## 14. 成本估算（基于 models.dev）

AMKR 本身**只记 token 用量，不记账**。成本是 WebUI 读出来的派生数字：拿本机的 token 用量乘以 models.dev 的公开单价。金额单位是 **USD**，单价单位是 **USD / 100 万 token**。

### 在哪里看

| 位置 | 内容 |
| --- | --- |
| 「成本」页 | 估算成本、模型成本排行、成本构成（输入 / 缓存读 / 缓存写 / 输出）、供应商成本、最近请求成本，以及可核对的单价明细表 |
| 「概览」页 | 「估算成本」KPI 与「模型成本排行」卡 |
| 「实时活动」页 | 请求流里每条请求的估算成本（悬停可见匹配到的单价） |

### 价格目录怎么来的

服务进程启动时从 `https://models.dev/api.json`（约 4.7 MB）取回一次，编译成「模型 id → 单价」的索引后**只驻内存**，此后每 6 小时复验一次。复验带 `If-None-Match`，目录没变时上游回 `304`，不会重复下载。

WebUI 通过 `GET /ui/pricing.json` 读取这份快照（与 `/ui/` 同级、不需要鉴权）。想让界面拿到价格，`webui_enabled` 必须为真。

目录取回失败**不会清空**已有价格：界面继续显示上一次成功的价格，并提示"当前显示的是上次成功获取的价格"。服务刚启动、尚未取到时，成本一律显示 `—`。

### 匹配规则

按 `upstream_model`（真正发给上游的模型名）匹配，**大小写不敏感**，依次尝试：

1. 原名，例如 `gpt-4o-mini`；
2. 去掉供应商前缀，例如 `openai/gpt-4o` → `gpt-4o`；
3. 去掉日期后缀，例如 `gpt-4o-mini-2024-07-18` → `gpt-4o-mini`。

用 `upstream_model` 而不是 `model_id`：后者是你自取的本地路由名，价格表里不可能有。因此**没有 upstream 归因的历史请求无法计价**，显示 `—`。

同一模型 id 在 models.dev 上常被多家供应商列出，其中不少把它标成 `input`/`output` 全为 `0`。服务端会先排除这类挂名条目，再取最便宜的一条；只有所有供应商都报 `0` 时才承认免费。不做这一步，`claude-sonnet-4-6`、`qwen3-max`、`glm-4.6` 等会被算成 `$0`，成本页会显示一张看起来很真、实则全零的假账。

### 计费口径

按 token **类别**分别计价：

| 类别 | 取哪个字段 | 用哪个单价 |
| --- | --- | --- |
| 普通输入 | `prompt_tokens` 扣除下面两类缓存量 | `input` |
| 缓存读 | `cache_read_input_tokens`，为 `0` 时退回 `cached_tokens` | `cache_read` |
| 缓存写 | `cache_creation_input_tokens` | `cache_write` |
| 输出 | `completion_tokens` | `output` |

缓存价缺失时**回退到输入价**，绝不当成 `0`。

举例：某次请求 prompt 1,000,000 token（其中缓存读 400,000、缓存写 100,000）、输出 200,000 token，模型单价 `input=3 / output=15 / cache_read=0.3 / cache_write=3.75`：

```
(500000×3 + 400000×0.3 + 100000×3.75 + 200000×15) ÷ 1,000,000 = $4.995
```

### 必须知道的偏差

- **这是估算，不是账单。** 单价来自 models.dev 的公开目录，与上游实际计费可能有出入（区域价、合约价、促销）。
- **金额会变。** 成本不落库，是按当前目录现算的；目录更新后，历史请求的估算金额会跟着变。
- **未匹配的部分不计入，也不当成 0。** 界面上会标出「x/y 项有定价」与「计价覆盖率」。覆盖率明显不足时，合计金额低于真实开销是正常的——**不要把它当成完整账单**。
- **忽略阶梯定价。** 当前只取目录里的基础单价，不看 `cost.tiers`（如超长上下文加价，约 460 个模型带此字段）。
- **不做汇率换算。** 金额固定是 USD。

---

## 15. 使用访客 Key

访客功能需要安装：

```bash
pipx install "auto-model-key-router[visitor]"
```

访客固定 Key 是：

```text
amkr-visitor
```

它不能在配置里修改，也不能作为 `local_api_key` 使用。

给某个上游 Key 开放访客权限（在 `providers.*.keys` 中给对应 Key 加 `allow_visitor: true`，例如供应商 `openai` 的 Key `openai-backup`）：

```json
{
  "providers": {
    "openai": {
      "base_url": "https://api.openai.com",
      "keys": {
        "openai-backup": {
          "api_key": "sk-your-second-upstream-key",
          "allow_visitor": true
        }
      }
    }
  }
}
```

访客请求示例：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer amkr-visitor" \
  -d '{
    "model": "amkr-gpt-4o-mini",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

访客限制：

- 只能使用 `allow_visitor: true` 且启用的上游 Key。
- `/v1/models` 中只会看到公共模型 ID，格式为 `amkr-{真实模型ID}`。
- 不能访问内部 alias、真实模型 ID、`unified-model` 或显式 `模型[key]`。
- 不能访问 `/metrics` 和 `/api/*` 管理接口。
- 未安装 `visitor` extra 时，`amkr-visitor` 不会被接受；配置里的 `allow_visitor` 可以保留但不会生效。

---

## 16. 接入 Claude Code

AMKR 支持 Anthropic Messages 风格入口 `/v1/messages`，可供 Claude Code 使用。

在 **一键配置 → Claude Code** 中可选择三种状态：

- **AMKR unified-model 模式**：先设置 `unified-model`，再由 AMKR 写入路由、本地鉴权和 `unified-model` 的 Claude 模型环境变量。
- **AMKR 原生模式**：只写入路由和本地鉴权；首次接管会保留已有的 Claude 模型配置。从 unified-model 模式切换时会移除 AMKR 写入的模型变量，由用户手动配置 Claude Code 默认模型。
- **未接管**：选择回退原配置，AMKR 会恢复首次应用前的完整文件内容。

两种 AMKR 接管模式都要求 `local_api_key` 不为空。原生模式不要求设置 `unified-model`，但 Claude Code 使用的每个模型名必须先在 AMKR 的**模型设置**中配置；否则请求会明确提示缺失的模型名。

AMKR 会更新：

```text
~/.claude/settings.json
# 或 CLAUDE_CONFIG_DIR/settings.json
```

unified-model 模式写入的核心环境变量包括：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8000",
    "ANTHROPIC_AUTH_TOKEN": "amkr_your-local-api-key",
    "ANTHROPIC_MODEL": "unified-model",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "unified-model",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "unified-model",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "unified-model"
  }
}
```

原生模式保留 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN` 和 AMKR 流量控制变量，但不会注入模型名。应用前的原始配置会备份到 AMKR 缓存目录，可在 TUI 中回退。

---

## 17. 接入 Codex

AMKR 支持 OpenAI Responses 风格入口 `/v1/responses`，可供 Codex 使用。

在 **一键配置 → Codex** 中可选择 unified-model 模式、原生模式或回退原配置。两种接管模式都写入 AMKR OpenAI Provider 和 `auth.json` 的本地鉴权 key；只有 unified-model 模式需要预先设置 `unified-model`。

原生模式不会注入模型：首次接管会保留用户已有的 `model`、`review_model` 和 `model_reasoning_effort`；从 unified-model 模式切换时会移除这些 AMKR 写入字段。用户需要手动配置 Codex 的 `model`、`review_model` 等模型名，并先将每个名称添加到 AMKR 的**模型设置**中；模型不存在时路由器会返回包含该名称的配置提示。

AMKR 会更新：

```text
~/.codex/config.toml
~/.codex/auth.json
# 或 CODEX_HOME/config.toml 与 CODEX_HOME/auth.json
```

unified-model 模式写入的核心配置类似：

```toml
model_provider = "OpenAI"
model = "unified-model"
review_model = "unified-model"
model_reasoning_effort = "max"

[model_providers.OpenAI]
name = "OpenAI"
base_url = "http://127.0.0.1:8000/v1"
wire_api = "responses"
requires_openai_auth = true
```

`auth.json` 会更新本地鉴权 key：

```json
{
  "OPENAI_API_KEY": "amkr_your-local-api-key"
}
```

一键配置只会更新上述模型调用字段，以及 `auth.json` 中的 `OPENAI_API_KEY`。原生模式始终保留 Provider 与本地鉴权配置。现有的其他 Codex 设置、注释、OpenAI Provider 自定义字段和其他鉴权字段都会保留；旧版本已经写入的非模型字段也不会被主动删除。

应用前的原始配置同样会备份，可在 TUI 中回退。

---

## 18. 接入 Pi Agent

在 **一键配置 → Pi Agent** 中，AMKR 只提供 `unified-model` 模式，不提供原生模型模式。应用前需要先设置 `unified-model` 和本地鉴权 key；回退会恢复首次应用前的完整配置文件。

AMKR 会更新：

```text
~/.pi/agent/models.json
# 或 PI_CODING_AGENT_DIR/models.json
```

它会保留其他自定义提供商，并写入 `amkr` 提供商，其中包含 `unified-model` 以及当前有启用 Key 的 AMKR 模型和别名：

```json
{
  "providers": {
    "amkr": {
      "baseUrl": "http://127.0.0.1:8000/v1",
      "api": "openai-completions",
      "apiKey": "amkr_your-local-api-key",
      "authHeader": true,
      "models": [
        { "id": "unified-model", "contextWindow": 262144 },
        { "id": "gpt-5.5", "contextWindow": 262144 },
        { "id": "my-gpt", "contextWindow": 262144 }
      ]
    }
  }
}
```

Pi 的 `/model` 会继续显示其内置及其他自定义提供商的模型；选择 `amkr/unified-model` 会经由 AMKR 的统一路由，选择其他 `amkr/<模型或别名>` 则直接请求对应的 AMKR 模型。重新执行一键配置即可同步新增或删除的模型。

---

## 19. 请求兼容说明

AMKR 的代理入口是 `/v1/{path}`，主要兼容：

| 客户端入口 | 实际转发 | 说明 |
| --- | --- | --- |
| `/v1/chat/completions` | `/v1/chat/completions` | OpenAI-compatible 主路径 |
| `/v1/messages` | 默认原生 `/v1/messages`，不支持时回退 `/v1/chat/completions` | Anthropic Messages 原生优先；可用 URL 级 `upstream_routes[base_url].anthropic` 改原生路径 |
| `/v1/messages/count_tokens` | 本地处理 | 返回 token 估算，不访问上游 |
| `/v1/responses` | 默认探测 `/v1/responses`，不支持时回退 `/v1/chat/completions`；配置 URL 级 `upstream_routes[base_url].responses` 时改原生 Responses 路径 | Responses 原生透传或转 Chat Completions |

兼容转换包括：

- `max_output_tokens` → `max_tokens`
- `stop_sequences` → `stop`
- Responses 的 `instructions`、function call、function output 和 tools 转换
- Anthropic 的 `system`、`tools`、`tool_use`、`tool_result` 转换
- `stream: true` 时自动补充 `stream_options.include_usage=true`，并从 SSE chunk 中提取 usage 用于统计

高级多模态、托管工具等能力仍取决于上游 OpenAI-compatible 服务的兼容程度。

---

## 20. 常见问题

### 请求返回 401 / 403

检查两层 Key：

1. 请求 AMKR 时传的本地 Key 是否等于 `local_api_key`。
2. 配置里模型绑定的供应商 Key（`providers.*.keys.*.api_key`）是否有效。

### 请求返回 404 模型不存在

检查请求体里的 `model` 是否是：

- 真实模型 ID；或
- 该模型的 `aliases[]`；或
- 已配置的 `unified-model`；或
- 已配置的任务名（`TASK_XXXXXX`，见 [第 9 节](#9-任务路由)）；或
- 访客模式下 `/v1/models` 返回的 `amkr-{真实模型ID}`。

### 请求任务名返回 400

任务路由会拒绝两类请求（见 [第 9 节](#9-任务路由)）：

- **显式传了任务已固定的采样参数**：错误信息会列出冲突的参数名。任务固定了什么就不能再传什么，去掉这些字段即可（`max_tokens` 这类不在此列，照常透传）。
- **写了 `TASK_XXXXXX[key]`**：任务不接受调用方指定 Key，Key 由目标模型自身的路由模式决定。

`reasoning_effort` 是唯一的例外：任务会覆盖它，但不会因为你传了就报错。

### 请求任务名返回 404

任务指向的模型当前没有**启用**的 Key（一条都没绑定，或绑定的都被禁用了）。先在**模型设置**里给该模型绑定或启用 Key，或在**任务路由**页把任务改指向一个可用模型。删掉模型时引用它的任务会被自动清理，所以这个错误通常出现在「模型被解绑或禁用了所有 Key」的情况下。

### 请求返回 503 没有可用 Key

可能原因：

- 该模型没有启用的 Key。
- Key 都在冷却中。
- 访客请求的模型没有任何 `allow_visitor: true` 的 Key。

### 修改配置后是否需要重启？

配置文件和管理 API 写入后，运行中的服务会热加载。系统服务、监听地址/端口等运行参数变化时，建议重启服务。

### 如何迁移配置到另一台机器？

使用 TUI：

1. 源机器进入 **CLI 设置 → 配置迁移 → 复制 Key 配置**。
2. 目标机器进入 **CLI 设置 → 配置迁移 → 粘贴并应用**。

迁移内容包含模型与上游 Key；安装 `visitor` extra 时还会包含访客权限。目标端已有监听、本地鉴权、路径等设置会保留。
