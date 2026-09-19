# API 文档

本文档描述 Auto Model Key Router 当前提供的 HTTP、SSE 和 WebSocket 接口。

默认服务地址：

```text
http://127.0.0.1:8000
```

## 鉴权

除 `HEAD /` 与 `GET /health` 外，业务接口通常需要本地 API key。

```http
Authorization: Bearer your-local-api-key
```

也支持：

```http
x-api-key: your-local-api-key
```

鉴权规则：

| 调用方 | 可访问接口 | 说明 |
| --- | --- | --- |
| 本地 API key | `/v1/models`、`/v1/*`、`/metrics`、`/api/*` | 完整权限 |
| 固定 visitor key `amkr-visitor` | `/v1/models`、`/v1/*` | 需要安装 `visitor` 扩展，只能使用允许访客访问的 Key |
| 无 key | `HEAD /`、`GET /health` | 当运行时 `local_api_key` 为空时，其他接口也按本地完整权限处理 |

visitor 模型使用 `amkr-{真实模型ID}` 形式，例如 `amkr-gpt-5.5`。visitor 不能使用内部别名、真实模型 ID、`unified-model` 或没有开启 `allow_visitor` 的 Key。

## 接口总览

| 方法 | 路径 | 鉴权 | 用途 |
| --- | --- | --- | --- |
| `HEAD` | `/` | 无 | 存活探针，返回 `204` |
| `GET` | `/health` | 无 | 服务、配置和端点能力状态 |
| `GET` | `/v1/models` | 本地或 visitor | 查询当前调用方可用模型 |
| `POST` | `/v1/chat/completions` | 本地或 visitor | OpenAI Chat Completions 兼容接口 |
| `POST` | `/v1/messages` | 本地或 visitor | Anthropic Messages 兼容接口 |
| `POST` | `/v1/messages/count_tokens` | 本地或 visitor | 本地估算 Anthropic 输入 token |
| `POST` | `/v1/responses` | 本地或 visitor | OpenAI Responses 兼容接口 |
| `POST` | `/v1/embeddings` | 本地或 visitor | OpenAI Embeddings 兼容接口 |
| 多种 | `/v1/{path}` | 本地或 visitor | 其他 OpenAI-compatible 接口透传 |
| `GET` | `/metrics` | 仅本地 | 查询 SQLite 聚合调用统计 |
| `GET` | `/metrics/requests` | 仅本地 | 分页查询持久化上游调用明细 |
| `GET` | `/metrics/series` | 仅本地 | 查询补零的持久化统计时间桶 |
| `GET/POST` | `/api/models` | 仅本地 | 查询或创建模型 |
| `GET/PUT/DELETE` | `/api/models/{model_id}` | 仅本地 | 查询、更新或删除模型 |
| `GET/POST` | `/api/models/{model_id}/keys` | 仅本地 | 查询或创建模型 Key |
| `GET/PUT/DELETE` | `/api/models/{model_id}/keys/{key_name}` | 仅本地 | 查询、更新或删除 Key |
| `GET/POST` | `/api/providers` | 仅本地 | 查询或创建 Provider |
| `GET/PUT/DELETE` | `/api/providers/{provider_id}` | 仅本地 | 查询、更新或删除 Provider |
| `GET/POST` | `/api/providers/{provider_id}/keys` | 仅本地 | 查询或创建 Provider Key |
| `GET/PUT/DELETE` | `/api/providers/{provider_id}/keys/{key_name}` | 仅本地 | 查询、更新或删除 Provider Key |
| `POST` | `/api/providers/{provider_id}/probe` | 仅本地 | 同步刷新该 Provider 全部启用 Key 的能力探测（各 Key 的模型列表 + 路由可用性） |
| `POST` | `/api/providers/{provider_id}/keys/{key_name}/probe` | 仅本地 | 同步刷新指定 Key 的能力探测，可用 `modes` 限定路由检查范围 |
| `GET/POST` | `/api/routes` | 仅本地 | 查询或创建模型路由 |
| `GET/PUT/DELETE` | `/api/routes/{route_id}` | 仅本地 | 查询、更新或删除模型路由 |
| `GET/POST` | `/api/tasks` | 仅本地 | 查询或创建任务路由（`TASK_XXXXXX` → 模型 + 固定参数）；可用 `X-AMKR-Workspace` 头指定工作空间 |
| `GET/PUT/DELETE` | `/api/tasks/{task_name}` | 仅本地 | 查询、更新或删除任务路由；可用 `X-AMKR-Workspace` 头指定工作空间 |
| `GET` | `/api/workspaces` | 仅本地 | 列出**有任务**的工作空间及各自任务数 |
| `PUT/DELETE` | `/api/workspaces/{workspace}` | 仅本地 | 重命名或删除工作空间（组内任务一并搬走/删除）；默认空间 `default` 不可改删 |
| `GET/PUT` | `/api/settings` | 仅本地 | 查询或更新监听、超时和重试设置 |
| `POST` | `/api/settings/local-api-key` | 仅本地 | 重置本地鉴权 Key；新 Key 仅在本次响应返回 |
| `POST` | `/api/update/check` | 仅本地 | 复用 CLI 的 GitHub Releases 版本检查 |
| `POST` | `/api/probes/keys` | 仅本地 | 异步探测指定 Provider 下 Key 的模型列表与各端点可用性（逐 Key 兼容接口） |
| `GET` | `/api/probes/{probe_id}` | 仅本地 | 查询探测进度和结果 |
| `POST` | `/api/probes/{probe_id}/cancel` | 仅本地 | 取消探测 |
| `POST` | `/api/config/export`、`/api/config/import` | 仅本地 | 导出或导入可迁移配置 |
| `GET` | `/ui/` | 无 | 内置 WebUI（需 `webui_enabled`，未启用或资产缺失时返回 `404`） |
| `GET` | `/ui/pricing.json` | 无 | models.dev 价格目录快照，用于 WebUI 估算成本（需 `webui_enabled`） |
| `GET` | `/ui/workspace-usage.json` | 仅本地 | 按工作空间拆分的用量读数与请求流向，供 WebUI 的「工作空间」页（需 `webui_enabled`） |
| `GET` | `/ui/update/status` | 无 | 报告本构建是否具备自更新能力及当前版本，供 WebUI 决定是否显示「立即更新」 |
| `POST` | `/ui/update/apply` | 仅本地 | 执行自更新：下载并校验新版、就地替换、启动收尾助手重启服务 |
| `GET` | `/api/logs` | 仅本地 | 读取日志文件尾部（默认最后 64 KiB） |
| `GET` | `/api/tool` | 仅本地 | 查询版本、可用更新与 WebUI 状态 |
| `POST` | `/api/tool/webui` | 仅本地 | 启用或关闭 WebUI（写入配置，需重启服务生效） |
| `POST` | `/api/service/{action}` | 仅本地 | 执行服务的启动/停止/重启/注册/注销 |
| `GET` | `/api/integrations` | 仅本地 | 查询 Claude Code / Codex / Pi Agent 的集成状态 |
| `POST` | `/api/integrations/{agent}` | 仅本地 | 以 `unified-model` 或 `native` 模式接管该 Agent 配置 |
| `POST` | `/api/integrations/{agent}/rollback` | 仅本地 | 回退该 Agent 到备份配置 |

### 新增能力挂在 `/api/` 还是 `/ui/`

管理面的 47 条与运维面的 7 条路由列在 `routePatterns()` / `opsRoutePatterns()` 里，响应
由逐字节语料锁定——那是**已发布接口**的回归凭证，不能随意增减。新增能力因此分两类：

- **管理面的正式资源**（工作空间自身的读/改/删）注册在 `/api` 之下，但列在**另一份**
  清单（`workspacePatterns()`）里。它们没有历史版本可对照，塞进那 47 条会让「这 47 条
  逐字节等于已发布行为」这句话失去意义。两批注册在同一棵 mux 上，因此错方法的
  `405` / `Allow` 判定要同时看两份清单。
- **本项目自有的读数**（价格目录 `/ui/pricing.json`、自更新入口、工作空间用量
  `/ui/workspace-usage.json`）挂在 `/ui/` 前缀下：它们与被锁定的 `/api` 面在语义上不
  连续，挂 `/ui/` 既落在冻结清单之外，也让「不开 WebUI 就没有这些读数」顺理成章。

判断标准是**它是不是管理面的正式资源**，而不是「能不能挂到 `/ui/` 躲开语料」。

工作空间对**既有**代理与管理路由的影响只体现为新增的 `X-AMKR-Workspace` 请求头：
不带该头的请求（也就是全部语料）行为逐字节不变，因此 `tasks/list` 这类被锁定的响应
体不需要（也不允许）新增 `workspace` 字段。

与 `/ui/pricing.json` 的区别是**鉴权**：价格目录是公开只读数据，自更新会替换磁盘上的
可执行文件并重启服务，因此 `/ui/update/apply` 要求完整权限（访客 key 一律 401），
只有 `/ui/update/status` 不鉴权（它只回答「这个构建有没有自更新能力」）。
`/ui/workspace-usage.json` 与自更新同类，要求完整权限。

`/api/update/check` 保持不变，仍用于「只检查、不安装」。

## 代理接口通用参数

代理型 `/v1/{path}` 接口读取 JSON 请求体，并使用其中的 `model` 选择模型和上游 Key。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 是 | 真实模型 ID、模型别名、隐藏别名（如各 target 的 `upstream_model`）、`unified-model`、任务名（`TASK_XXXXXX`）、`模型[Key名称]` 或 visitor 公共模型 ID |
| `stream` | boolean | 否 | 为 `true` 时使用流式响应，并自动向上游补充 `stream_options.include_usage=true` |
| `stream_options` | object | 否 | 流式选项；服务会保留已有字段并强制加入 `include_usage=true` |
| `reasoning_effort` | string | 否 | 推理强度；模型配置中的非空值优先级更高 |
| `reasoning.effort` | string | 否 | Responses 风格推理强度，没有顶层覆盖时转成 `reasoning_effort` |

模型选择示例：

```json
{"model": "gpt-5.5"}
```

```json
{"model": "gpt"}
```

```json
{"model": "gpt-5.5[main]"}
```

```json
{"model": "unified-model"}
```

```json
{"model": "TASK_000001"}
```

配置文件中的字段名仍为 `unified_model`；请求中的虚拟模型 ID 为 `unified-model`。

### 任务路由

任务名（`TASK_XXXXXX`）是配置里定义的任务。请求它时，真实模型和采样参数都由 AMKR 侧决定：

```json
{"model": "TASK_000001", "messages": [{"role": "user", "content": "hi"}]}
```

- 传给上游的 `model` 会是任务的 `model`；若该模型重试后仍返回可重试状态码，会再尝试任务的 `fallback_model`，此时响应带 `X-AMKR-Fallback: true`。
- 任务 `params` 里固定了的采样参数（`temperature`、`top_p`、`top_k`、`frequency_penalty`、`presence_penalty`、`seed`、`stop`、`max_tokens`）会覆盖请求体中的同名参数。
- 请求里**显式传了**这些参数会直接被拒绝（`400`），而不是被静默覆盖 —— 静默覆盖会让调用方以为自己的值生效了。`reasoning_effort` 也一样会被拒绝：任务路由是给别的 AI 服务用的，不是给 Agent 用的，调用方显式传了任务已固定的参数就该收到明确的 `400`。
- 不在任务 `params` 里的参数照常透传。`max_tokens` 与 Anthropic 的 `stop` 一样有跨方言同义字段：任务固定了 `max_tokens` 时，Responses 方言的 `max_output_tokens` 同样视为冲突（`400`），不会绕过后被静默丢掉。
- 任务不接受调用方指定 Key（`TASK_000001[main]` 返回 `400`），Key 仍由目标模型自身的路由模式决定。
- 访客 Key 不能访问任务名。

#### 工作空间

任务名只在**工作空间**内唯一：不同工作空间可以有同名任务，各自指向不同的模型与参数。工作空间由请求头选择：

| 头 | 说明 |
| --- | --- |
| `X-AMKR-Workspace` | 工作空间名。缺省或为空串即默认工作空间（配置的顶层 `tasks` 段）；超过首尾空白的部分会被裁掉 |

```bash
# 命中 workspaces.teamA.tasks 里的 TASK_000001
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer amkr_your-local-api-key" \
  -H "X-AMKR-Workspace: teamA" \
  -d '{"model": "TASK_000001", "messages": [{"role": "user", "content": "hi"}]}'
```

- 该头**不会**转发给上游：它是 AMKR 自己的路由状态，上游既看不懂也不该看到，因此与 `Authorization`、`X-Api-Key`、`Host` 等同属转发前剔除的请求头。
- 未配置的工作空间名不是错误：任务查表落空后会按普通模型名继续解析，因此最终和「模型未配置」是同一个 `404`。
- 工作空间只隔离任务：模型的 ID、别名、隐藏别名与 `unified-model` 仍然全局唯一，任务名也不能与它们撞名。
- 访客 Key 不能使用任务，带上该头也一样。

工作空间本身的管理（列出/改名/删除）走 `/api/workspaces*`，见下方「任务路由接口」一节。

成功选中路由后，服务会把传给上游的 `model` 改为真实模型 ID，并用选中 Key 的密钥替换鉴权头。其他兼容参数通常会继续传给上游。

### Chat Completions

`POST /v1/chat/completions`

常用请求参数：

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 是 | 路由模型 |
| `messages` | array | 是 | Chat Completions 消息数组 |
| `stream` | boolean | 否 | 是否返回 SSE |
| `tools` | array | 否 | Function tools |
| `tool_choice` | string/object | 否 | 工具选择策略 |
| `max_tokens` | integer | 否 | 最大输出 token |
| `max_output_tokens` | integer | 否 | 当没有 `max_tokens` 时转换为 `max_tokens` |
| `stop` | string/array | 否 | 停止序列 |
| `stop_sequences` | array | 否 | 当没有 `stop` 时转换为 `stop` |

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer your-local-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.5",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

### Anthropic Messages

`POST /v1/messages`

请求会优先以原生 Anthropic 格式发送到上游 `/v1/messages`，保留所有原生字段（包括 `cache_control`、`prompt_cache_key` 等）。如果上游不支持原生端点（返回 404/405/501），自动回退到转换为 `/v1/chat/completions` 格式。

**原生优先模式**（默认启用）：
- 首次请求时自动测试上游是否支持 `/v1/messages` 端点
- 测试结果按“上游 URL + 实际原生路径”缓存在端点能力缓存中，避免重复测试
- 支持原生端点时保留所有 Anthropic 原生字段，提高缓存命中率
- 可通过配置 `native_first: false` 禁用

如果上游的 Anthropic 入口需要额外路径前缀，可按上游 URL 配置 `upstream_routes[base_url].anthropic`，例如 `"anthropic": "anthropic/"` 会转发到 `base_url/anthropic/v1/messages`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 是 | 路由模型 |
| `messages` | array | 是 | 支持文本、`tool_use` 和 `tool_result` |
| `system` | string/array | 否 | 转换为首条 system 消息 |
| `max_tokens` | integer | 通常是 | 原样作为 Chat Completions 最大输出 token |
| `tools` | array | 否 | Anthropic tool 定义会转换为 function tool |
| `tool_choice` | object | 否 | 支持 `auto`、`none`、`any` 和指定 `tool` |
| `stop_sequences` | array | 否 | 转换为 `stop` |
| `stream` | boolean | 否 | 返回 Anthropic 风格 SSE |
| `prompt_cache_key` | string | 否 | 原生模式下保留，用于缓存路由 |
| `cache_control` | object | 否 | 原生模式下保留在 content block 中 |

以下字段仅在回退到 chat/completions 模式时移除：

`anthropic_version`、`metadata`、`reasoning`、`text`、`truncation`、`previous_response_id`、`include`、`store`、`safety_identifier`。

### Token 估算

`POST /v1/messages/count_tokens`

请求体与 Messages 接口类似，但不会请求上游。返回：

```json
{"input_tokens": 123}
```

该结果按请求内容的 UTF-8 JSON 字节长度估算，不等同于模型 tokenizer 的精确结果。

### Responses

`POST /v1/responses`

请求默认会探测上游 `/v1/responses`，不支持时回退到 `/v1/chat/completions` 并转换响应。配置 URL 级 `upstream_routes[base_url].responses` 后会改为对应原生 Responses 路径透传，不支持时仍会回退。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 是 | 路由模型 |
| `input` | string/array | 是 | 文本、message、`function_call` 或 `function_call_output` |
| `instructions` | string/array | 否 | 转换为首条 system 消息 |
| `tools` | array | 否 | Function tools |
| `tool_choice` | string/object | 否 | Responses function 选择会转换为 Chat Completions 格式 |
| `max_output_tokens` | integer | 否 | 转换为 `max_tokens` |
| `reasoning.effort` | string | 否 | 转换为 `reasoning_effort` |
| `stream` | boolean | 否 | 返回 Responses 风格 SSE |

### 其他代理路径

`/v1/{path}` 支持 `GET`、`POST`、`PUT`、`PATCH`、`DELETE`。除上述特殊转换接口外，请求路径、方法、查询参数和响应主体会尽量保持上游兼容格式。

所有通用代理请求仍需在 JSON 请求体中提供 `model`。缺少该字段会返回 `400`。

`POST /v1/embeddings` 不做协议转换：请求体本来就是 OpenAI 形状（`model`、`input`、`encoding_format` 等），AMKR 只替换 `model` 为上游真实模型名后原样转发，因此 `input` 不会被改写成 chat 的 `messages`。上游路径默认 `v1/embeddings`，可按上游 URL 配置 `upstream_routes[base_url].embeddings` 覆盖（别名 `embedding` / `embed`），例如 `"embeddings": "gateway/embed"` 会转发到 `base_url/gateway/embed/v1/embeddings`。请求 `unified-model` 时使用 `unified_model.embeddings` 计划；未配置该计划则继承 `default.primary`。

### WebSocket

可连接 `ws://HOST:PORT/v1/{path}`。服务读取客户端发送的第一帧，将其按对应路径的 HTTP `POST` 请求处理，把响应内容逐块发回，然后关闭连接。

## 基础接口

### `HEAD /`

无参数。成功时返回 `204 No Content`。

### `GET /health`

无参数、无需鉴权。主要响应字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `status` | string | 当前为 `ok` |
| `models` | array | 已配置的真实模型 ID 和别名（不含隐藏别名） |
| `config_path` | string | 当前配置文件绝对路径；嵌入式应用可能为空 |
| `local_auth_enabled` | boolean | 是否设置本地鉴权 |
| `local_api_key_fingerprint` | string | 本地 key 的 SHA-256 前 12 位 |
| `visitor_feature_installed` | boolean | 是否安装 visitor 扩展 |
| `visitor_access_enabled` | boolean | 是否存在启用且允许 visitor 的 Key |
| `visitor_key_count` | integer | visitor 可用 Key 总数 |
| `unified_model` | object/null | 当前真实模型和可选固定 Key；含 `default` 与可选的 `image` / `embeddings` 计划 |
| `native_endpoint_states` | object | 上游原生端点能力缓存 |
| `ops_enabled` | boolean | 运维接口是否注册（见 `--no-ops` / `enable_ops`） |

Key 的失败次数和冷却属于内部调度细节，不通过 `/health` 或管理 API 暴露。长期启停 Key 请更新配置中的 `enabled`。

### `GET /v1/models`

无查询参数。响应采用 OpenAI 模型列表格式：

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-5.5",
      "object": "model",
      "owned_by": "auto-model-key-router"
    }
  ]
}
```

本地调用只返回当前有可用 Key 的真实模型、别名和已配置的 `unified-model`。visitor 只返回 `amkr-*` 公共模型 ID。

隐藏别名（各 target 的 `upstream_model` 自动获得的叫法，以及模型 `hidden_aliases` 中手写的名字）可以直接调用，但不会出现在这里；见 [`docs/USAGE.md`](USAGE.md) 的「同一个模型的多个名字」。

任务名（`TASK_XXXXXX`）同样不出现在这里：它是一整组路由与参数的别名，而不是某个模型的名字。如果客户端需要从 `/v1/models` 里看到可调用的名字，请改用模型的别名或隐藏别名。

## 调用统计

### `GET /metrics`

仅接受本地完整权限，不接受 visitor key。

可选查询参数：

| 参数 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `hours` | number | `24` | 仅聚合最近若干小时；必须大于 `0` 且不超过 `8760` |
| `all_history` | boolean | `false` | 为 `true` 时忽略 `hours` 并聚合全部历史 |

`hours` 的默认值有界，是因为全量聚合要扫描整张 `request_metrics` 表（16 万行实测约 3–5 秒），而统计查询与写入共用同一把锁，全量查询会把代理请求路径一起卡住。确需全量时显式传 `all_history=true`。

顶层响应字段：

| 字段 | 说明 |
| --- | --- |
| `count_semantics` | 固定为 `upstream_attempt`，表示请求数按上游调用尝试计数 |
| `window` | 本次聚合的 `from`、`to` 和 `hours`；全量查询的 `from` 为 `null` |
| `started_at` | 当前统计存储实例启动时间 |
| `database_path` | SQLite 文件路径 |
| `rate_window_seconds` | 当前 RPM/TPM 统计窗口秒数，默认 `60` |
| `current_rpm` | 当前窗口内请求数，即近 1 分钟 RPM |
| `current_tpm` | 当前窗口内 token 总数，即近 1 分钟 TPM |
| `total` | 全局累计统计 |
| `caller_types` | 按 `local`、`visitor` 拆分 |
| `models` | 按真实模型 ID 拆分 |
| `requested_models` | 按请求中的模型名或别名拆分 |
| `model_requested_models` | 真实模型到请求模型名的嵌套统计 |
| `keys` | 真实模型到 Key 名称的嵌套统计 |
| `providers` | 按请求发生时的供应商 ID 拆分 |
| `provider_pools` | 按供应商 + 模型池拆分的嵌套统计；v4 已删除模型池概念，该维度只含 v3 及更早写入的历史行（v4 新行无 pool 归因），新部署通常为空 |
| `upstream_models` | 按实际发送给上游的模型 ID 拆分 |
| `unattributed` | 缺少供应商、上游模型归因字段，或只有历史模型池归因的调用汇总 |

每组统计包含：

```text
requests, successes, failures, retries
prompt_tokens, completion_tokens, total_tokens
cached_tokens, cache_creation_input_tokens, cache_read_input_tokens
cached_token_rate
total_duration_ms, avg_duration_ms, min_duration_ms, max_duration_ms
total_first_token_ms, avg_first_token_ms, min_first_token_ms, max_first_token_ms
status_codes
```

v4 起新写入的调用只按供应商与上游模型归因（模型池维度已随 v3 移除，pool 归因为空）。v3 及更早写入的历史行可能带有供应商/模型池归因，统一计入 `unattributed`；供应商、池或模型的实际 ID 即使为 `unknown`，也仍按字面 ID 查询和聚合，不会与未归因数据混淆。AMKR 不会根据当前配置反推旧数据，避免配置改名后改变历史含义。

### `GET /metrics/requests`

返回持久化的上游调用明细、所选范围汇总和相同筛选条件下的近 60 秒速率。仅接受本地完整权限。

查询参数：

| 参数 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `hours` | number | `24` | 最近小时数，必须大于 `0` 且不超过 `720` |
| `all_history` | boolean | `false` | 为 `true` 时忽略 `hours` 并查询全部历史 |
| `caller_type` | string | 无 | `local` 或 `visitor` |
| `model_id` | string | 无 | 真实路由模型 ID |
| `requested_model_id` | string | 无 | 客户端请求中的模型名或别名 |
| `provider_id` | string | 无 | 请求发生时的供应商 ID |
| `pool_name` | string | 无 | 请求发生时的模型池名称（v3 历史数据筛选用；v4 新调用该字段为空） |
| `upstream_model_id` | string | 无 | 实际发送给上游的模型 ID |
| `key_name` | string | 无 | 路由使用的 Key 名称 |
| `status_code` | integer | 无 | `100..599` |
| `success` | boolean | 无 | 是否成功 |
| `attributed` | boolean | 无 | `true` 仅返回三个归因字段完整的调用；`false` 返回缺少任一字段的调用 |
| `limit` | integer | `50` | 每页 `1..200` 条 |
| `before_id` | integer | 无 | 仅返回 ID 小于该值的行，用于稳定加载下一页 |

响应示例：

```json
{
  "count_semantics": "upstream_attempt",
  "window": {
    "from": "2026-07-13T12:00:00+08:00",
    "to": "2026-07-14T12:00:00+08:00",
    "hours": 24
  },
  "filters": {
    "caller_type": "local"
  },
  "rate_window_seconds": 60,
  "current_rpm": 4,
  "current_tpm": 18200,
  "summary": {},
  "latest_request_at": "2026-07-14T11:59:52+08:00",
  "total_items": 1284,
  "items": [
    {
      "id": 1284,
      "created_at": "2026-07-14T11:59:52+08:00",
      "caller_type": "local",
      "model_id": "gpt-5.5",
      "requested_model_id": "default",
      "provider_id": "openai",
      "pool_name": null,
      "upstream_model_id": "gpt-5.5-2026-05-01",
      "key_name": "main",
      "status_code": 200,
      "success": true,
      "retried": false,
      "prompt_tokens": 1200,
      "uncached_prompt_tokens": 300,
      "completion_tokens": 80,
      "total_tokens": 1280,
      "cached_tokens": 900,
      "cache_creation_input_tokens": 0,
      "cache_read_input_tokens": 900,
      "first_token_ms": 420,
      "duration_ms": 3100
    }
  ],
  "next_before_id": 1235
}
```

当 `next_before_id` 为 `null` 时没有下一页。刷新第一页时不要携带 `before_id`。

### `GET /metrics/series`

返回 SQLite 历史数据生成的非重叠时间桶，用于趋势图。服务端按北京时间自然边界对齐起点并补齐没有调用的空桶，因此响应 `window.from` 可能略早于精确的 `hours` 起点；最后一个桶可能尚未结束。仅接受本地完整权限。

查询参数：

| 参数 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `hours` | number | `1` | 最近小时数，必须大于 `0` 且不超过 `8760`（1 年） |
| `bucket_seconds` | integer | `60` | 桶宽 `15..86400` 秒；一次查询最多返回 `500` 个桶 |
| 其他筛选参数 | - | 无 | 与 `/metrics/requests` 的来源、模型、供应商、Key、状态、成功和归因筛选一致 |

`hours` 的上界是 **8760**（1 年），比 `/metrics`、`/metrics/requests` 的 720 宽：长窗口只
能配粗桶，点数上限（500）才是真正的约束——`hours=8760&bucket_seconds=86400` 是 366 个点、合法，
而 `hours=8760&bucket_seconds=60` 会因超过 500 点被拒（422）。

响应示例：

```json
{
  "count_semantics": "upstream_attempt",
  "window": {
    "from": "2026-07-14T11:00:00+08:00",
    "to": "2026-07-14T12:00:00+08:00",
    "hours": 1
  },
  "filters": {},
  "bucket_seconds": 60,
  "points": [
    {
      "started_at": "2026-07-14T11:00:00+08:00",
      "ended_at": "2026-07-14T11:01:00+08:00",
      "complete": true,
      "requests": 3,
      "successes": 3,
      "failures": 0,
      "retries": 0,
      "prompt_tokens": 12000,
      "completion_tokens": 900,
      "total_tokens": 12900,
      "cached_tokens": 8600,
      "cached_token_rate": 0.716667,
      "avg_duration_ms": 2400,
      "avg_first_token_ms": 380,
      "status_codes": {
        "200": 3
      }
    }
  ]
}
```

每个统计行代表一次上游调用尝试。发生自动重试时，同一个客户端请求会产生多行；在没有持久化请求关联 ID 前，API 不会把这些行错误合并成一个请求。

## 模型与 Key 管理 API

所有管理接口只接受本地完整权限。写操作会原子更新当前配置文件，并在完成后热重载运行时配置。资源读取响应包含 `config_revision`；写请求可携带读取到的 `config_revision`，版本冲突会在 mutation 前返回 `409`，避免覆盖其他端的修改。

查询响应不会返回上游 `api_key` 明文，只返回 SHA-256 前 12 位的 `api_key_fingerprint`。

### 数据结构

#### ModelCreate

| 字段 | 类型 | 必填 | 默认值/约束 |
| --- | --- | --- | --- |
| `id` | string | 是 | 非空；不能与其他 ID 或别名重复 |
| `aliases` | string[] | 否 | `[]`；所有模型名称必须全局唯一；会出现在 `/v1/models` |
| `hidden_aliases` | string[] | 否 | `[]`；可直接调用但不出现在 `/v1/models`；与任何模型 ID、`aliases` 或其他模型的手写隐藏别名重复时返回 `409` |
| `routing_mode` | string | 否 | `round_robin`；可选 `round_robin`、`priority`、`only_first` |
| `reasoning_effort` | string/null | 否 | 可选 `none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max` |
| `keys` | KeyCreate[] | 否 | `[]`；兼容写法：每个 Key 会在其 `base_url` 对应的供应商下创建（不存在则自动建供应商）并绑定为模型的 target。也可先创建无 Key 的模型，再通过模型 Key 接口或 `/api/routes` 补充绑定 |

#### ModelUpdate

字段与 ModelCreate 的模型字段相同，全部可省略，但请求中至少需要出现一个字段。`id`、`aliases`、`hidden_aliases`、`routing_mode` 不能为 `null`；`reasoning_effort: null` 用于清除模型级覆盖，`hidden_aliases: []` 用于清空手写隐藏别名。不能通过该接口更新 `keys` 或 `targets`（使用模型 Key 接口或 `/api/routes`）。

#### KeyCreate

| 字段 | 类型 | 必填 | 默认值/约束 |
| --- | --- | --- | --- |
| `name` | string | 是 | 非空；同一供应商内唯一（创建时若供应商已存在同名 Key 返回 `409`） |
| `api_key` | string | 是 | 非空 |
| `base_url` | string/null | 否 | 决定 Key 落在哪个供应商：匹配已存在供应商的 `base_url`，否则自动创建供应商；缺省用配置的 `default_base_url`，否则为 `https://api.openai.com` |
| `enabled` | boolean | 否 | `true` |
| `allow_visitor` | boolean | 否 | `false` |
| `upstream_routes` | object/null | 否 | 兼容字段；会写入该 Key 的 `base_url` 对应的 URL 级路由，而不是保存到 Key 上 |

`base_url` 最终必须以 `http://` 或 `https://` 开头。KeyCreate/KeyUpdate 中的 `upstream_routes` 仅用于兼容旧客户端；值只能是相对路径或路径前缀，例如 `{"anthropic": "anthropic/"}` 会规范化为 URL 级配置 `upstream_routes[base_url].anthropic = "anthropic/v1/messages"`。

> v4 中 Key 存储在 `providers.<id>.keys` 下，模型通过 `targets[]` 引用 `{provider, key, upstream_model}`。本组「模型 Key 接口」与 KeyCreate/KeyUpdate 是面向模型的操作：`POST /api/models/{model_id}/keys` 会在供应商下创建（或定位）Key 并把它绑定为该模型的一个 target；`PUT/DELETE` 只影响当前模型的绑定（详见下文）。如需直接管理供应商 Key（改名、启停、删除、访客权限），使用 `/api/providers/{provider_id}/keys` 系列接口。

#### KeyUpdate

字段与 KeyCreate 相同，全部可省略，但请求中至少需要出现一个字段。省略 `api_key` 会保留原密钥；`name`、`api_key`、`enabled`、`allow_visitor` 不能为 `null`。`base_url: null` 会恢复为配置的默认上游地址；`upstream_routes: null` 或 `{}` 会清空自定义路由。

#### ModelResponse

```json
{
  "id": "gpt-5.5",
  "aliases": ["gpt"],
  "hidden_aliases": ["gpt-latest"],
  "auto_hidden_aliases": ["gpt-5.5-2026-01-01"],
  "routing_mode": "round_robin",
  "reasoning_effort": "medium",
  "visitor_available": true,
  "keys": [
    {
      "name": "main",
      "base_url": "https://api.openai.com",
      "enabled": true,
      "allow_visitor": true,
      "api_key_fingerprint": "0123456789ab"
    }
  ]
}
```

`keys` 是该模型当前绑定的供应商 Key 展开结果（来自 `models.<id>.targets[]` 与 `providers.*.keys`）。同一供应商 Key 同时被多个模型绑定时，模型内的 `name` 可能与供应商 Key 名不同（自动加 `供应商ID-` 前缀去重），`base_url` 为该 Key 所属供应商的地址。

#### KeyResponse

```json
{
  "name": "main",
  "base_url": "https://api.openai.com",
  "enabled": true,
  "allow_visitor": true,
  "api_key_fingerprint": "0123456789ab"
}
```

模型 Key 接口与 `/api/providers/{provider_id}/keys` 系列均返回此结构；provider Key 响应额外在顶层带 `config_revision`。

#### RouteTarget

`/api/routes` 的 target 对象（即磁盘格式 `models.<id>.targets[]` 的元素）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `provider` | string | 是 | 供应商 ID，必须已存在 |
| `key` | string | 是 | 该供应商下已声明的 Key 名称 |
| `upstream_model` | string | 是 | 发送给上游的真实模型名（创建模型 Key 等交互流程默认填本地模型 ID）。该名字同时会自动成为模型的隐藏别名：可直接调用，但不出现在 `/v1/models` |

示例：

```json
[
  {"provider": "openai", "key": "main", "upstream_model": "gpt-5.5"},
  {"provider": "openai", "key": "backup", "upstream_model": "gpt-5.5"}
]
```

#### ProviderResponse

`GET /api/providers`、`GET/PUT /api/providers/{provider_id}` 等接口返回的 provider 对象：

```json
{
  "id": "openai",
  "base_url": "https://api.openai.com",
  "keys": [
    {
      "name": "main",
      "enabled": true,
      "allow_visitor": true,
      "api_key_fingerprint": "0123456789ab",
      "capabilities": {
        "models": ["gpt-5.5"],
        "route_status": {"openai": "ok", "anthropic": "ok", "responses": "ok"},
        "errors": {},
        "checked_at": "2026-09-01T00:00:00+00:00"
      }
    }
  ],
  "routes": {"openai": "v1/chat/completions", "responses": "v1/responses"}
}
```

provider 对象不再含顶层 `capabilities`；探测缓存按 Key 存于 `keys[]` 中每个元素的 `capabilities`（该 Key 通过 `GET /v1/models` 看到的可服务模型清单与 openai/anthropic/responses 路由的可用性，`errors` / `checked_at` 记录探测错误与时间），未探测时为 `null`。同一供应商的不同 Key 能访问的模型集可能不同（如免费/付费额度、不同订阅），因此各 Key 独立探测、独立缓存，互不复用。v3 的 `pools` 字段已移除。

#### TaskCreate

| 字段 | 类型 | 必填 | 默认值/约束 |
| --- | --- | --- | --- |
| `name` | string | 是 | 非空；即客户端传的 `model`。不能与已有任务名、任何模型 ID、`aliases`、`hidden_aliases` 或 `unified-model` 重复 |
| `model` | string | 是 | 首选模型，可写模型 ID 或别名；写回时规范化为模型 ID |
| `fallback_model` | string/null | 否 | `null`；备选模型，与首选引用同一模型时忽略 |
| `params` | object | 否 | `{}`；固定采样参数，见下节。留空表示全部透传 |
| `config_revision` | string | 是 | 并发校验版本号 |

`params` 只接受白名单内的键，写错键名返回 `422`（不会静默忽略）：

| 键 | 类型 | 约束 |
| --- | --- | --- |
| `temperature`、`top_p`、`frequency_penalty`、`presence_penalty` | number | 任意数值 |
| `top_k`、`seed`、`max_tokens` | integer | 必须是整数，`1.5` 这类值返回 `422` |
| `stop` | string[] | 非空字符串数组；传字符串返回 `422`（配置文件路径会包成单元素数组，管理 API 不会） |
| `reasoning_effort` | string | `none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`；传 `""`、`default`、`downstream` 视为不固定 |

#### TaskUpdate

字段与 TaskCreate 的模型字段相同（`model`、`fallback_model`、`params`），全部可省略，但请求中至少需要出现一个，否则返回 `422`。`fallback_model: null` 清除备选，`params: {}` 清空全部固定参数。`name` 不可改 —— 它是调用方使用的 `model` 名，改名等于换了个任务。

#### TaskResponse

```json
{
  "name": "TASK_000001",
  "model": "gpt-4o-mini",
  "fallback_model": "claude-sonnet",
  "params": {"temperature": 0.2, "reasoning_effort": "high"}
}
```

### 模型接口

#### `GET /api/models`

返回：

```json
{"models": [ModelResponse]}
```

#### `POST /api/models`

请求体为 ModelCreate，成功返回 `201` 和 ModelResponse。

```bash
curl -X POST http://127.0.0.1:8000/api/models \
  -H "Authorization: Bearer your-local-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "gpt-5.5",
    "aliases": ["gpt"],
    "routing_mode": "round_robin",
    "keys": [{
      "name": "main",
      "api_key": "sk-upstream",
      "base_url": "https://api.openai.com"
    }]
  }'
```

上例的 `keys` 会在供应商 `openai`（按 `base_url` 匹配或自动创建）下写入 `main`，并把它绑定为该模型的一个 target。

#### `GET /api/models/{model_id}`

路径参数 `model_id` 为真实模型 ID。成功返回 ModelResponse，并在顶层附带当前 `config_revision`。

#### `PUT /api/models/{model_id}`

请求体为 ModelUpdate，成功返回更新后的 ModelResponse。

#### `DELETE /api/models/{model_id}`

成功返回 `204 No Content`；请求体可带 `config_revision` 进行并发校验。只删除模型本身及其绑定关系，被删除模型绑定的供应商 Key 与供应商会保留（供应商 Key 被模型引用不算配置错误）。若引用该模型的 Key 不再被任何模型使用，可另行通过 `/api/providers/{provider_id}/keys/{key_name}` 删除。

### Key 接口

Key 接口操作的是「模型绑定的 Key」（v4 中即该模型 `targets[]` 对应的供应商 Key）。路径参数 `model_id` 为真实模型 ID；`key_name` 是该模型内的 Key 名称（解析时兼容供应商原始 Key 名与 `供应商ID-Key名` 限定名）。这些操作与 WebUI 模型路由页的 Key 管理等价：供应商 Key 被其他模型绑定时，写操作会先把该模型解耦到独立的供应商 Key 克隆，再应用修改，避免影响其他模型。

#### `GET /api/models/{model_id}/keys`

返回：

```json
{"keys": [KeyResponse]}
```

#### `POST /api/models/{model_id}/keys`

请求体为 KeyCreate，成功返回 `201` 和 KeyResponse。该 Key 会写入对应供应商（`base_url` 匹配或自动创建），并为当前模型追加一条 `target` 绑定。

#### `GET /api/models/{model_id}/keys/{key_name}`

成功返回 KeyResponse。

#### `PUT /api/models/{model_id}/keys/{key_name}`

请求体为 KeyUpdate，成功返回更新后的 KeyResponse。该 Key 同时被其他模型绑定时，会先为当前模型克隆一个独立的供应商 Key 再应用修改，因此不会影响其他模型对该供应商 Key 的使用。

```bash
curl -X PUT http://127.0.0.1:8000/api/models/gpt-5.5/keys/main \
  -H "Authorization: Bearer your-local-api-key" \
  -H "Content-Type: application/json" \
  -d '{"allow_visitor": true}'
```

#### `DELETE /api/models/{model_id}/keys/{key_name}`

成功返回 `204 No Content`。该接口与 WebUI 模型路由页的 Key 操作等价：解绑当前模型的这条 Key 绑定。若该 Key 不再被任何模型绑定，会连带删除供应商下的这个 Key；供应商随后没有 Key 时也会一并删除。若这是模型的最后一条绑定，模型会被自动删除。

### 任务路由接口

任务路由把「模型 + 固定采样参数」打包成一个可直接当 `model` 传的名字。任务名不能与模型 ID、别名、隐藏别名或 `unified-model` 撞名（否则路由语义会取决于查表顺序），也不能指定 Key。

这五个端点都认 `X-AMKR-Workspace` 头，语义与代理面完全一致：缺省即默认工作空间（顶层 `tasks`），带 `X-AMKR-Workspace: teamA` 即读写 `workspaces.teamA.tasks`。任务名只在工作空间内唯一，因此**同一空间内**重名报 `409`、跨空间同名合法；`GET/PUT/DELETE /api/tasks/{task_name}` 取的是该空间里的那个任务，别的空间的同名任务不会被误改。工作空间名不需要事先声明：在它下面建第一个任务即存在，删掉最后一个任务即消失。

响应体没有变化：`TaskResponse` 不含 `workspace` 字段（`tasks/list` 的字节被对拍语料锁定），当前空间由请求头决定。

#### `GET /api/tasks`

返回 `{"tasks": [TaskResponse]}`（只含该空间的任务），另含 `config_revision`。

#### `POST /api/tasks`

请求体为 TaskCreate，成功返回 `201` 与 TaskResponse（含 `config_revision`）。该空间已有同名任务时返回 `409`（`{"detail": "任务已存在: <name>"}`）。

#### `GET/PUT/DELETE /api/tasks/{task_name}`

查询、更新或删除单个任务。`PUT` 请求体为 TaskUpdate + `config_revision`，成功返回 `200` 与更新后的 TaskResponse；`GET` 返回 TaskResponse；`DELETE` 请求体只需 `config_revision`，成功返回 `204`。该空间里任务不存在时返回 `404`（`{"detail": "任务不存在: <name>"}`）——**不会**回落到别的空间的同名任务。

任务随 `/api/config/export`、`/api/config/import` 一同迁移（命名工作空间也在其中）；导入时引用不到模型的任务会被跳过（与「该模型从未配置」一致）。删除模型时会一并清理引用它的任务：首选模型没了则删除整个任务，只有备选没了则退化为单模型任务。

#### `GET /api/workspaces`

列出**有任务**的工作空间及各自任务数，供 WebUI 填充切换下拉：

```json
{"workspaces": [{"name": "default", "task_count": 2}, {"name": "teamA", "task_count": 1}]}
```

默认工作空间 `default` 固定排在首位。没有任何任务的工作空间不会出现——空间由「在它里面建任务」隐式产生，空分组既不可观测也没有意义（删空的工作空间也会从配置里消失）。

这个目录是必要的：`GET /api/tasks` 是**按空间过滤**的，因此从它推不出「还有哪些空间存在」。

#### `PUT /api/workspaces/{workspace}`

把工作空间改名为请求体里的 `name`，组内任务整体跟着搬到新键下。请求体为 `{"config_revision": "...", "name": "..."}`，成功返回 `200` 与 `{"name": "<新名>", "task_count": <任务数>, "config_revision": "..."}`。

- `400`：目标是默认工作空间（`default` 不可改名）。
- `404`：源空间没有任务（空间由任务反推，空的不存在）。
- `409`：目标名已被占用，或目标名就是 `default`。**不会合并**两个空间——合并会瞬间造出重名任务，而任务名在同一空间内唯一是配置层的硬校验。

#### `DELETE /api/workspaces/{workspace}`

删除工作空间**连同其中的全部任务**。请求体只需 `config_revision`（可省略），成功返回 `204`。`400` 删除默认空间，`404` 空间不存在。

#### 没有 `POST /api/workspaces`

空分组不进配置（见上），空间由「在里面建第一个任务」隐式产生——建任务就是建空间。多一条 `POST` 只会造出配置层随后要清掉的空壳。

### 供应商接口与能力探测

v4 中 Key 与探测都以 Key 为单元：`providers.<id>` 保存 `base_url`、各协议 `routes` 与 `keys`（Key 集合），探测缓存按 Key 存放在 `providers.<id>.keys.<key>.capabilities`。同一供应商的不同 Key 能访问的模型集可能不同，因此每个 Key 独立探测、独立缓存，互不复用。删除供应商或其 Key 时会清理所有引用它们的模型绑定。

#### `GET /api/providers`

返回：

```json
{"providers": [ProviderResponse]}
```

#### `POST /api/providers`

请求体：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `id` | string | 是 | 供应商 ID，非空且不能与已有 ID 重复 |
| `base_url` | string | 是 | `http://` 或 `https://` 开头的上游地址 |
| `config_revision` | string | 是 | 并发校验版本号 |

成功返回 `201` 和 ProviderResponse。新供应商没有 Key，添加 Key 请用 `/api/providers/{provider_id}/keys` 或模型 Key 接口（按 `base_url` 自动归并）。

#### `GET/PUT/DELETE /api/providers/{provider_id}`

查询、更新（`id`、`base_url`、`routes`）或删除供应商。`PUT` 请求体为 `ProviderUpdate`（`id`/`base_url`/`routes` 可省略）+ `config_revision`。删除供应商会移除其所有 Key，并删除所有引用它的模型 target；因此失去全部 target 的模型会被一并删除（响应不含被删模型列表，删除前请自行确认）。

#### `GET/POST /api/providers/{provider_id}/keys`

列出该供应商的 Key（`{"keys": [KeyResponse]}`）或创建新 Key。创建请求体为 ProviderKeyCreate（`name`、`api_key`、`enabled`、`allow_visitor`、`config_revision`），成功返回 `201` 和 KeyResponse。新 Key 默认不绑定任何模型；需要按模型绑定请使用模型 Key 接口或 `/api/routes`。

#### `GET/PUT/DELETE /api/providers/{provider_id}/keys/{key_name}`

查询、更新或删除单个供应商 Key。删除会从所有模型的 `targets[]` 中移除对该 Key 的引用，因而失去全部 target 的模型会被自动删除；若这是供应商最后一个 Key，供应商也会被删除。被模型 Key 接口解绑到只剩本模型时，同样会走到这里（删除模型 Key 的最后引用）。

#### `POST /api/providers/{provider_id}/probe`

同步刷新该供应商**全部启用 Key** 的能力探测：每个 Key 分别执行 `GET /v1/models` 拉取该 Key 的可服务模型清单，并对该 Key 在 `openai` / `anthropic` / `responses` 三个路由模式下各做一次最小请求，结果分别写入 `providers.<id>.keys.<key>.capabilities` 后随响应返回。请求体只需携带 `config_revision`。供应商尚无 Key 时返回 `422`。探测结果按 Key 缓存，不跨 Key 共享或折叠。

```bash
curl -X POST http://127.0.0.1:8000/api/providers/openai/probe \
  -H "Authorization: Bearer your-local-api-key" \
  -H "Content-Type: application/json" \
  -d '{"config_revision": "..."}'
```

#### `POST /api/providers/{provider_id}/keys/{key_name}/probe`

同步刷新**指定单个 Key** 的能力探测：同样先执行 `GET /v1/models` 拉取该 Key 的可服务模型清单，再按 `modes` 做路由最小请求，结果写入 `providers.<id>.keys.<key>.capabilities` 后随响应返回。请求体为：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `config_revision` | string | 是 | 并发校验版本号 |
| `modes` | string[] | 否 | 限定做最小请求的路由模式，可选 `openai`、`anthropic`、`responses`；省略时检查全部路由模式。模型清单探测总是执行 |

```bash
curl -X POST http://127.0.0.1:8000/api/providers/openai/keys/main/probe \
  -H "Authorization: Bearer your-local-api-key" \
  -H "Content-Type: application/json" \
  -d '{"config_revision": "...", "modes": ["openai", "responses"]}'
```

响应额外包含 `key` 对象（含 `capabilities`，未探测时为 `null`）。

> 说明：探测缓存是机器本地信息（不同机器、不同 Key 权限看到的模型可能不同），只由以上手动刷新入口更新；WebUI 供应商页的「刷新能力探测」语义与此一致（刷新全部 Key，或指定 Key 并可限定端点范围）。管理 API 的 Key 创建、模型绑定等写操作不会自动发起探测。

#### `POST /api/probes/keys`（兼容接口）

按 Key 粒度的异步探测，返回 `202` 与 `probe_id`。请求体为 `ProbeKeysRequest`（`provider_id`、`keys`、`timeout_seconds`），随后的 `GET /api/probes/{probe_id}` 轮询结果、`POST /api/probes/{probe_id}/cancel` 取消。该接口的模型列表结果同样按 Key 独立探测；日常手动刷新请优先使用上面的 `probe` 接口（同步写回并随响应返回结果）。

### 路由接口

`/api/routes` 系列是模型路由（targets）的管理入口，与 `/api/models` 操作同一份模型数据：`POST /api/routes` 创建模型并写入 targets，`GET/PUT/DELETE /api/routes/{route_id}` 读取、整体替换或删除某模型的 targets。请求体中的 `targets` 为 RouteTarget 数组（`{provider, key, upstream_model}`），target 引用的供应商与 Key 必须已存在。v3 的 `pool` 引用已不存在于 target 中。请求体同样支持 `aliases` 和 `hidden_aliases`（后者可直接调用但不出现在 `/v1/models`）。

```bash
curl -X POST http://127.0.0.1:8000/api/routes \
  -H "Authorization: Bearer your-local-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "gpt-5.5",
    "aliases": ["gpt"],
    "hidden_aliases": ["gpt-latest"],
    "routing_mode": "round_robin",
    "targets": [
      {"provider": "openai", "key": "main", "upstream_model": "gpt-5.5"},
      {"provider": "tokenplan", "key": "mimo", "upstream_model": "gpt-5.5"}
    ]
  }'
```

## 状态码与错误格式

常见状态码：

| 状态码 | 场景 |
| --- | --- |
| `200/201` | 管理接口读写成功（创建类返回 `201`，同步探测 `POST /api/providers/{id}/probe` 与 `POST /api/providers/{id}/keys/{key_name}/probe` 返回 `200`） |
| `202` | 异步探测任务已接受（`/api/probes/keys`） |
| `204` | 删除成功 |
| `400` | 缺少 `model`、更新体为空或配置校验失败；请求任务名时显式传了该任务已固定的采样参数，或指定了 Key（`TASK_XXXXXX[key]`） |
| `401` | 本地 API key 验证失败 |
| `403` | visitor 无权访问模型或 Key（含任务名） |
| `404` | 模型、Key、供应商或探测不存在；任务指向的模型没有启用的 Key |
| `409` | 名称冲突、删除最后一个 Key、无法持久化嵌入式配置 |
| `422` | 管理 API 请求字段类型错误、缺少必填字段或包含未知字段；供应商暂无 Key 时探测 |
| `500` | 配置保存失败 |
| `502` | 上游连接或响应转换失败 |
| `503` | 没有可用 Key |

代理接口错误通常使用：

```json
{"error": {"message": "错误信息"}}
```

管理 API 的错误使用：

```json
{"detail": "错误信息"}
```

Anthropic Messages 错误可能使用 Anthropic 风格的 `type` 和 `error` 对象。
## 内置 WebUI 与运维 API

AMKR 自带一套可选的 WebUI，用于在浏览器中完成日常管理。**资产随二进制一起发布（`//go:embed`），没有单独的安装步骤，也没有额外依赖**；是否启用只由配置字段 `webui_enabled`（或启动参数 `--webui` / `--no-webui`）决定。

### 启用方式

```bash
# 启动时临时启用（同时写入配置文件）
amkr --config router-config.json --webui
# 显式关闭
amkr --config router-config.json --no-webui
```

也可以在 WebUI 的设置页切换，或调用 `POST /api/tool/webui`。

启用后访问：

```
http://127.0.0.1:8000/ui/
```

> WebUI 的挂载发生在服务进程启动时。通过 `POST /api/tool/webui` 修改开关会立即写入配置并让服务热重载配置，但 `/ui` 的挂载状态**要重启服务才会改变**。因此 `/health` 与 `/api/tool` 同时给出两个字段：`webui_enabled`（配置意图）与 `webui_mounted`（本进程实际状态）。

### 鉴权

`/ui/` 本身是静态资产，不需要鉴权；**它调用的管理接口都会照常校验本地鉴权 Key**。页面会把 Key 保存在浏览器 `localStorage`（键名 `amkr.apiKey`），并在未授权时提示输入。本地鉴权未启用时，管理接口对本机开放。

### `GET /api/workspaces`、`PUT|DELETE /api/workspaces/{workspace}`

工作空间自身的读/改/删。这些是管理面的正式资源，因此挂在 `/api` 之下（详见「任务路由接口」一节），而不是像价格目录那样挂 `/ui/`。

### `GET /ui/workspace-usage.json`

按工作空间拆分的用量读数与请求流向，供 WebUI 的「工作空间」页使用。**需要鉴权**（内容反映各空间的用量与模型流向，不是公开数据）。

| 参数 | 类型 | 默认 | 约束 | 说明 |
| --- | --- | --- | --- | --- |
| `hours` | number | `24` | `> 0` 且 `<= 8760` | 统计窗口 |
| `all_history` | boolean | `false` | — | 为真时忽略 `hours`，统计全部历史（此时 `window.from` 为 `null`） |

参数校验与 `/metrics` 同序：**先校验参数、后校验凭据**，因此 `hours=0` 不带凭据返回 `422` 而不是 `401`。

响应（`Content-Type: application/json`）：

```json
{
  "count_semantics": "upstream_attempt",
  "window": {"from": "2026-01-01T00:00:00+08:00", "to": "2026-01-02T00:00:00+08:00", "hours": 24},
  "workspaces": [
    {"name": "teamA", "stats": {"requests": 12, "successes": 12, "total_tokens": 3400, "...": "..."}}
  ],
  "unattributed": {"requests": 3, "total_tokens": 800, "...": "..."},
  "layers": ["workspace", "requested_model_id", "model_id", "provider_id", "upstream_model_id"],
  "links": [
    {"source_layer": 0, "target_layer": 1, "source": "teamA", "target": "TASK_000001", "requests": 12, "total_tokens": 3400}
  ]
}
```

| 字段 | 说明 |
| --- | --- |
| `workspaces[].stats` | 与 `/metrics` 的 `total` 同形（同一套聚合口径） |
| `unattributed` | **没有**工作空间归属的请求（升级前的历史行、以及不走代理的写入路径） |
| `layers` | 流向图的层顺序，与 `links` 的 `source_layer` / `target_layer` 对应 |
| `links[].source` / `target` | 相邻两层之间的连边；`requests` 与 `total_tokens` 都给出，供前端切换宽度口径 |

关于 `unattributed`：**不会**被并进 `default`。把它算到默认工作空间头上会凭空造出一段并不存在的用量。工作空间归属从记录该字段的版本起才开始写入，升级前的历史行永远落在这里，不会追溯回填。

关于 `links`：某一端为空的请求（`provider_id` / `upstream_model_id` 可空）**不成边**，在图上留出缺口，而不是补一个占位节点——否则无法区分哪条是数据、哪条是兜底。

> 该端点挂在 `/ui/` 之下而非新增 `/api/metrics/*`：它是本项目自有的响应形状（参照实现没有工作空间，没有可比对的 oracle），不混进被逐字节语料锁定的 `/metrics` 系列。

### `GET /ui/pricing.json`

返回 models.dev 价格目录的服务端缓存快照，供 WebUI 估算成本。**不需要鉴权**：内容是 models.dev 的公开数据，与 `/ui/` 下的静态资产同级。

价格目录由服务进程在启动时取回一次、此后每 6 小时复验一次（条件请求，命中时是 `304`，不重复传 230 KB）。它**只驻内存、不落盘**；取回失败不会清空已有目录，而是在载荷的 `error` 字段里说明，让界面显示"旧价格 + 告警"。

| 参数 | 说明 |
| --- | --- |
| 无 | 不接受任何查询参数 |

响应（`Content-Type: application/json`，并带 `ETag` 与 `Cache-Control: public, must-revalidate, max-age=0`）：

```json
{
  "version": 1,
  "source": "https://models.dev/api.json",
  "updated_at": "2026-01-02T03:04:05Z",
  "error": null,
  "models": {
    "gpt-4o": { "input": 2.5, "output": 10, "cache_read": 1.25 },
    "claude-sonnet-4-5": { "input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75 }
  }
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `version` | 整数 | 载荷版本，字段形状有破坏性变化时递增 |
| `source` | 字符串 | 价格来源，固定为 models.dev 的 `api.json` |
| `updated_at` | 字符串 \| null | 目录取回时间（RFC3339 UTC）；从未成功取回时为 `null` |
| `error` | 字符串 \| null | 最近一次刷新失败的原因；非 `null` 表示当前价格是旧的 |
| `models` | 对象 | 小写模型 id → 单价，单位是 **USD / 100 万 token** |

`models` 里的条目只保证有 `input` 与 `output`；`cache_read` / `cache_write` **缺失即不存在**（不会补 `0`），客户端应回退到 `input` 价而不是当成免费。

同一模型 id 在 models.dev 上可能被多家供应商列出，服务端已按"排除 `input`/`output` 全为 `0` 的挂名条目后取最便宜"收敛成一条。这一步不能省：实测有数百个模型被部分供应商标成全 `0`，不做排除会让成本页显示一张全零的假账。

状态码：

| 状态码 | 场景 |
| --- | --- |
| `200` | 返回目录快照 |
| `304` | 请求带 `If-None-Match` 且与当前 `ETag` 一致 |
| `405` | 非 `GET` / `HEAD` |
| `503` | 目录尚不可用（服务刚启动、或一直取不到）。**不返回空目录**——那会让客户端把每个模型当成免费 |
| `404` | `webui_enabled` 未启用时，该路由与 `/ui/` 一起不存在 |

> 价格目录挂在 `/ui/` 前缀下，因此与 WebUI 同生共死：`webui_enabled` 为假时它也不可达。成本估算是 WebUI 的读数，没有界面时无人消费。

### 成本估算口径

金额是**派生读数，不落库**：指标库只存 token 用量，成本由 WebUI 按当前目录现算。因此目录更新后，历史请求的估算金额会跟着变——单价是外部事实，不是本项目的记账结果。

按 token **类别**分别计价（单位 USD / 100 万 token）：

| 类别 | token 字段 | 单价 |
| --- | --- | --- |
| 普通输入 | `prompt_tokens` 减去下面两类缓存量 | `input` |
| 缓存读 | `cache_read_input_tokens`，为 `0` 时退回 `cached_tokens` | `cache_read`，缺失时回退 `input` |
| 缓存写 | `cache_creation_input_tokens` | `cache_write`，缺失时回退 `input` |
| 输出 | `completion_tokens` | `output` |

不能直接用 `uncached_prompt_tokens` 乘 `input`：对 Anthropic，`prompt_tokens = input_tokens + cache_read + cache_creation`，而 `uncached_prompt_tokens` 等于 `input + cache_creation`，拿它乘输入价会把缓存写计费两次——一次按输入价、一次按 `cache_write`。

匹配按 `upstream_model` 名称（大小写不敏感），依次尝试：原名 → 去掉 `vendor/` 前缀 → 去掉 `-YYYY-MM-DD` / `-YYYYMMDD` 日期后缀。匹配不到时成本显示 `—`，**不显示 `$0`**；有部分条目未匹配时，界面会明确标出"x/y 项有定价"，避免局部金额被读成完整账单。

> 当前实现使用目录里的**基础单价**，忽略 models.dev 的阶梯定价（`cost.tiers`，如超长上下文加价，约 460 个模型带此字段）；也不做汇率换算，金额固定是 USD。

### `GET /api/logs`

读取日志文件尾部，用于 WebUI 的日志面板。

日志文件是 `log_file_path`（默认 `<配置目录>/server.log`），由服务进程自己写入：应用侧日志、每个 HTTP 请求的访问日志，以及启动/关停等服务器日志都会追加到这里，因此面板在服务正常运行、没有任何报错时也会有内容。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `tail` | 整数 | 否 | 读取的字节数，默认 65536（64 KiB），上限 1048576 |

响应：

```json
{
  "text": "最后若干行日志……",
  "truncated": true,
  "path": "C:/Users/me/AppData/Local/.../server.log",
  "error": null
}
```

日志文件不存在或不可读时返回 `200`，`text` 为空、`error` 为具体原因，便于前端区分「没有日志」与「读取失败」。

### `GET /api/tool`

汇总 CLI 的版本检查与 WebUI 状态，等价于 WebUI 的「设置 → 版本」卡片。

```json
{
  "version": "4.0.2",
  "latest_version": "4.0.2",
  "update_available": false,
  "release_url": null,
  "source": "pypi",
  "error": null,
  "webui_available": true,
  "webui_enabled": true,
  "webui_mounted": true,
  "webui_path": "/ui"
}
```

版本检查使用短超时并复用 CLI 的实现；网络失败不会让接口报错，而是把原因放进 `error`。

### `POST /api/tool/webui`

```json
{"enabled": true}
```

写入 `webui_enabled` 并热重载配置，响应为更新后的 WebUI 状态（字段同 `GET /api/tool` 的 `webui_*` 部分）。

### `POST /api/service/{action}`

在本机执行服务生命周期动作，响应体包含人类可读的执行输出：

```json
{"action": "restart_amkr", "text": "……命令输出……"}
```

可用动作：

| 动作 | 说明 |
| --- | --- |
| `start_amkr` / `stop_amkr` / `restart_amkr` / `status_amkr` | 后台服务控制 |
| `install_user_amkr` / `uninstall_amkr` | 当前用户登录自启 |
| `install_system_amkr` / `uninstall_system_amkr` | 系统级服务（需要管理员授权） |
| `start_system_amkr` / `stop_system_amkr` / `restart_system_amkr` | 系统级服务控制 |

未知动作返回 `422`。

### `GET /api/integrations`

查询三个受支持客户端的接管状态。每一项都是独立的，**单个 Agent 读取失败只会让该项带 `error`，不影响其他两项**：

```json
{
  "integrations": [
    {
      "agent": "claude-code",
      "display_name": "Claude Code",
      "target_path": "C:/Users/me/.claude/settings.json",
      "target_exists": true,
      "backup_available": false,
      "current_is_applied": true,
      "mode": "unified-model",
      "error": null
    }
  ]
}
```

### `POST /api/integrations/{agent}`

```json
{"mode": "unified-model"}
```

`agent` 取 `claude-code`、`codex`、`pi-agent`；`mode` 取 `unified-model` 或 `native`（Pi Agent 固定使用 `unified-model`）。写入前会先备份原配置，未启用本地鉴权时返回 `409`。

### `POST /api/integrations/{agent}/rollback`

从备份恢复该 Agent 的原配置；没有备份时返回 `409`。

## 嵌入宿主

AMKR 的装配入口是 `internal/server` 的 `Options` 与 `New`（没有单独的 `mount_app`
层）。`internal/server` 是 Go 的内部包，因此嵌入只在**本模块内**可用（例如
`cmd/amkr`）；外部程序请把 AMKR 当成独立进程用。

```go
app, err := server.New(server.Options{
    ConfigPath: "router-config.json",
    Config:     cfg,
    // 嵌入到宿主的某个前缀下时给出；独立运行时留空。
    MountPrefix: "/amkr",
})
if err != nil { /* ... */ }
hostMux.Handle("/amkr/", http.StripPrefix("/amkr", app.Handler()))
```

挂载后的路径：

| 独立运行 | 挂载到 `/amkr` 后 |
| --- | --- |
| `POST /v1/chat/completions` | `POST /amkr/v1/chat/completions` |
| `GET /health` | `GET /amkr/health` |
| `GET /ui/` | `GET /amkr/ui/` |
| `GET /api/settings` | `GET /amkr/api/settings` |
| `WS /ws/events` | `WS /amkr/ws/events` |

`MountPrefix` 只影响报出的路径（`/health` 的 `webui_path`）与 WebUI、价格目录、自更新
入口这些挂在 `/ui/` 之下的路由；它**不会**改写管理 API 的 `/api` 前缀。

### 嵌入时的行为差异

配置里的 `ops_enabled` 决定运维接口（`/api/logs`、`/api/service/*`、
`/api/integrations/*`、`/api/tool`）是否注册。这些接口作用于「服务所在的这台机器」——
启停后台进程、注册系统服务、改写 Claude Code / Codex 的本地配置——嵌入到别人的进程里
语义不成立，因此独立部署想整体关掉运维面时用 `amkr --no-ops`，比逐个路径拉黑可靠；
关闭后 `/health` 的 `ops_enabled` 为 `false`。

其余接口（代理、`/health`、`/metrics`、WebSocket、`/api/settings` 等配置管理接口）
在挂载下与独立运行完全一致。

### 配置持久化

管理接口写配置要求已知配置文件路径。`Options.ConfigPath` 为空时，`GET /api/settings`
等读写接口会返回 `409`：

> 传入 `ConfigPath` 即可获得完整的管理能力。

WebUI 的前端会根据当前页面路径（`.../ui/`）自动推导 API 基址，因此挂载到任意前缀下
都不需要额外配置。

### 复用宿主的身份体系（可插拔鉴权）

嵌入时宿主通常已经有自己的身份认证（session cookie、JWT、网关注入的身份头）。默认的
「本地 API key」在这种场景下很别扭：把 `local_api_key` 留空等于**整体关闭鉴权**，否则
就得让调用方额外再持有一套 AMKR 的 key。

`Options.Authorizer` 可以整体替换鉴权判定：

```go
app, err := server.New(server.Options{
    ConfigPath: "router-config.json",
    Config:     cfg,
    Authorizer: func(r *http.Request, localAPIKey string) *auth.Context {
        user := hostUser(r)              // 宿主的身份体系
        if user == nil {
            return nil                   // nil = 拒绝，返回 401
        }
        if user.IsAdmin {
            return auth.Full()
        }
        return auth.Visitor()            // 受限，见下
    },
})
```

`auth.Context` 只有两种模式，语义与默认鉴权完全一致：

| 模式 | 权限 |
| --- | --- |
| full | 全部接口，全部模型 |
| visitor | 仅 `/v1/models` 与 `/v1/*`，且只能用标记了 `allow_visitor` 的 Key 与 `amkr-{模型ID}` 形式；拿不到 `/metrics` 与配置管理接口 |

`visitor` **不是「权限更小的 full」而是一套模型级规则**（visitor 不能用内部别名、真实
模型 ID 或 `unified-model`），因此钩子必须显式选择它，不能用布尔值表达。

同时覆盖 WebSocket：`/ws/events` 只对 full 开放。WebSocket 握手无法携带自定义头，所以
AMKR 的凭据只能走首帧 `{"type":"auth","token":"..."}`；该 token 会被折算成
`Authorization: Bearer <token>` 后交给同一个钩子。宿主的 cookie 在握手头中，因此用
cookie / session 鉴权时握手即可通过，无需处理首帧。钩子拒绝时连接以 `4003` 关闭。

未传 `Authorizer` 时行为不变（本地 key 或固定 visitor key）。
