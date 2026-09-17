# AMKR Go 迁移方案

> **Goal:** 在不停机、不破坏既有用户数据的前提下，把 AMKR 从 Python 逐步迁移到 Go；最终由单个静态二进制同时承担代理、管理 API、WebUI 托管、TUI 与运维职责。

**基准版本：** `4.1.0`（`pyproject.toml:7`，2026-09-16）
**基线规模：** 核心 Python 15,630 行 / 40 模块；测试 ~12,300 行 / 485 用例；WebUI 5,885 行 / 19 文件
**Go 工具链：** 本机已装 `go1.26.5 windows/amd64`

---

## 1. 已确认的四项决策

| 决策点 | 结论 | 影响 |
| --- | --- | --- |
| 迁移策略 | **并行共存、逐步替换** | Go 服务与 Python 版共用同一份 `router-config.json`，按能力灰度切换，任一阶段可回退 |
| TUI | **用 Go 重写（Bubble Tea）** | 需要重建 `tui.py + dashboard.py + logs_tui.py + config_editor.py`（≈4,357 行）的能力，是最大的单块工作量 |
| 嵌入能力 | **改为 Go 库 + 独立进程** | 对外提供 Go `http.Handler` 挂载点与独立进程两种集成；Python `mount_app` 随 Python 版退役 |
| 兼容性 | **完全兼容** | 现有 `config_version: 4` 配置与 `metrics.sqlite3` 必须被 Go 版直接读写，老用户换二进制即可 |

---

## 2. 现状要点（决定迁移顺序的事实）

**架构分层清晰但异步语义重。** `app.py` 是唯一的 FastAPI 装配点：`create_app()` 构建 `RuntimeManager`，把「配置 / KeyPool / MetricsStore / httpx client」打包成一个 `RuntimeResources` generation；`/v1/{path:path}` 通配路由把所有代理请求交给 `proxy_handler.handle_proxy_request`。

**行为契约集中在测试而非文档。** `tests/test_app.py`（4,953 行、111 用例）是事实上的协议规范：覆盖 SSE 分帧、重试与冷却、首字节/空闲超时、请求头净化、Anthropic/Responses/embeddings 转换、visitor 规则、unified-model 与任务降级。**迁移应以它为验收基准，而不是以现有代码为基准。**

**三处高风险兼容面：**

1. **`config_revision` 是 sha256 契约。** `management_api.py:1187` 用 `json.dumps(migrate_config_data(data), ensure_ascii=False, sort_keys=True, separators=(",", ":"))` 再 sha256。Go 必须逐字节复现该序列化，否则所有管理 API 的乐观并发校验（409）会在混跑时互相误判。
2. **指标库无 schema 版本。** `metrics.py:885` 只有 `CREATE TABLE IF NOT EXISTS` + `_ensure_column()`（`PRAGMA table_info` → `ALTER TABLE ADD COLUMN`）+ 2 条无条件回填 `UPDATE`。没有 `user_version`，因此**无法与新老二进制协商格式**，混跑期任何一方写入都必须保持旧格式可读。
3. **`created_at` 是字典序比较的字符串。** 写入格式是 `datetime.now(ZoneInfo("Asia/Shanghai")).isoformat()`，**微秒为 0 时省略小数部分**；所有时间窗过滤都是 `created_at >= ?` 的字符串比较；分桶用 `strftime('%s', ...)` 且锚点 `BUCKET_ANCHOR_EPOCH = -28800`（`metrics.py:20`）。格式偏差不会报错，只会静默算错。

**canonical JSON 有两个消费者，不止一个。** 除 `config_revision` 外，`_cache_affinity_key`（`proxy_handler.py:348`）也对 `{path, model, system, tools, tool_choice, messages[{role,content}]}` 做 `sort_keys=True` + 紧凑分隔符 + `ensure_ascii=False` 的 sha256，用于 round-robin 粘滞。**任何序列化偏差都会同时破坏配置乐观并发和 Key 粘滞行为**，且两者都是静默失效。这使 canonical 序列化器成为 Phase 1 的最高优先级交付物。

**Runtime 租约语义是 Go 侧最容易写错的地方。** `RuntimeLease.wrap_stream`（`runtime.py:41`）在 async generator 的 `finally` 里释放租约，因此租约覆盖整条流式响应；热重载只能把旧 generation 标 `retired`，无法关闭仍被流式请求持有的 httpx client。`RuntimeManager.close()` 会 `await` 所有 generation 的 `drained`，**一个泄漏的租约就会让关停永久阻塞**。

**运维面不是 TUI 专属。** `--install-service`、`--service`、`/api/service/*`、`/api/integrations/*` 都会改本机状态（`schtasks` + UAC 提权、systemd user unit + `loginctl enable-linger`、改写 `~/.claude/settings.json` / `~/.codex/config.toml` / `~/.pi/agent/models.json`），必须与 TUI 决策解耦、独立排期。

**CI 目前不跑测试。** `.github/workflows/release.yml` 只在 `pyproject.toml` 变更时触发，流程是 build → twine check → wheel 冒烟 → docker build → 容器 `/health` 校验 → 推 GHCR → 建 Release；485 个测试只在本地或 `scripts/release.py` 里跑。**迁移的第一步就是补上这个缺失的红绿信号。**

**没有可复用的 golden 数据。** `tests/` 下无 fixture / cassette / 录制报文，上游交互全是内联 `httpx.MockTransport`（73 处）。唯一语言中立、可直接复用的是 3 个 `.mjs` 探针，它们驱动真实的 `webui/` ES 模块并输出 JSON。

**开工前需先收敛当前工作区。** 迁移启动时工作区有大量未提交改动（**36 个已修改文件、12 个未跟踪文件**，含 `app.py`/`config.py`/`management_api.py`/`proxy_handler.py`/`key_pool.py` 等核心模块，以及未跟踪的 `auth.py`、`tests/test_embedding.py`、`webui/pages/tasks.js`）。这些正是迁移的目标模块，且部分（如 `auth.py` 的可插拔鉴权、`test_embedding.py` 的嵌入契约）与第 1 节的决策直接相关。**应先按模块拆分提交、形成干净的基线，再开始 Phase 0**，否则无法区分"迁移引入的偏差"与"未提交改动带来的差异"。

---

## 3. 目标架构

单个 Go module，`cmd/` 双入口，`internal/` 按现有 Python 模块边界一一对应：

```text
go/
├── go.mod                      # module github.com/Sparrived/auto-model-key-router/go
├── cmd/
│   ├── amkr/main.go            # 入口一（与 auto-model-key-router 同二进制）
│   └── auto-model-key-router/main.go
├── amkr/                       # 公开可嵌入 API（替代 mount_app / create_app）
│   ├── app.go                  # New(cfg) (http.Handler, error); Mount(mux, prefix, cfg, opts)
│   └── auth.go                 # Authenticator 钩子、AuthContext{mode: full|visitor}
├── internal/
│   ├── canonical/              # 与 Python json.dumps 逐字节对齐的 canonical JSON
│   ├── config/                 # RouterConfig、v1–v4 迁移、原子保存、revision hash
│   ├── keypool/                # 选 Key、冷却、sticky、visitor、route plan
│   ├── runtime/                # generation 租约引用计数 + 热重载
│   ├── metrics/                # SQLite schema / 查询 / snapshot / series / history
│   ├── proxy/                  # 代理主流程、重试与降级、流式编排
│   ├── protocol/               # Anthropic / Responses 请求与 SSE 转换
│   ├── httpapi/                # 代理路由、/health、/v1/models、/metrics*、WS
│   ├── mgmt/                   # 47 条管理 API
│   ├── ops/                    # 7 条运维 API
│   ├── webui/                  # go:embed 静态资产（复用现有文件）
│   ├── service/                # PID、schtasks、systemd、UAC
│   ├── agentcfg/               # claude-code / codex / pi-agent 配置改写
│   ├── update/                 # 版本检查 + 原子二进制替换
│   └── tui/                    # Bubble Tea 界面
└── webui/                      # 直接搬移 auto_model_key_router/webui/（零改动）
```

**依赖选型（均为可静态链接的纯 Go 实现，避免 CGO 以保留交叉编译能力）：**

| 用途 | 选型 | 理由 |
| --- | --- | --- |
| 路由 | 标准库 `net/http` + Go 1.22+ `ServeMux` 模板 | `GET /api/providers/{id}/keys/{name}` 这类模式原生支持，无需框架 |
| SQLite | `modernc.org/sqlite` | 纯 Go、无 CGO、可交叉编译；WAL 文件格式与 Python 侧完全互通 |
| WebSocket | `github.com/coder/websocket` | 上下文感知、无 CGO；覆盖 `/ws/events` 与 `/v1/{path}` WS 代理 |
| TOML | `github.com/pelletier/go-toml/v2` | Codex `config.toml` 往返，替代 `tomlkit` |
| TUI | `bubbletea` + `lipgloss` + `bubbles` | 社区标准，覆盖原 `tui.py` 自研的按键/鼠标/视口能力 |
| 时区 | 内嵌 `time/tzdata` | `Asia/Shanghai` 不依赖系统 tzdata，与 Python `tzdata` extra 对齐 |

---

## 4. 兼容性契约（必须逐条断言，不可"差不多"）

### 4.1 配置文件

- 读写 `config_version: 4`；**迁移链 v1/v2 → v3 → v4 必须完整复刻**（`config.py:315` `migrate_config_data`、`config.py:434` `_migrate_v3_to_v4`）。重点是 v3 pool 白名单语义：`models` 键存在即过滤（含空数组），键缺失才不设限（`config.py:515`）——漏掉这条会让 v3 里被静默丢弃的死引用在升级后"复活"。
- 保存格式：`json.dumps(data, indent=2, ensure_ascii=False) + "\n"`，写临时文件后 `os.replace`，带 4 次指数退避重试（`config.py:300`，应对 Windows 杀软占用）。Go 的 `os.Rename` 在 Windows 上有同样失败模式，**必须保留重试循环**。
- **保存不排序**：与 4.2 的 canonical 路径相反，落盘保留键的插入顺序。Go 侧必须走顺序保留的缩进序列化（`canonical.DumpsIndent`），不能复用 `Dumps`。
- 空容器在 indent 模式下仍写成紧凑的 `{}` / `[]`，不是多行。
- **换行必须复刻文本模式翻译**：`Path.write_text` 以文本模式打开文件，会把 `"\n"` 翻译成 `os.linesep`，因此 Windows 上真实磁盘字节是 **CRLF**。实测 `save_config_data` 写出的字节为 `b'{\r\n  "config_version": 4,\r\n...\r\n}\r\n'`。Go 的 `os.WriteFile` 不做翻译，照搬会每个换行少一个 `\r`——文件功能等价，但字节级兼容不成立，且用户在两版之间切换时 git 会持续显示改动。字符串**内部**的真实换行在 JSON 里是转义的两字节（`\` 与 `n`），不受翻译影响。
- 加载时若迁移产生变化会**写回磁盘**（`config.py:860`）。

### 4.2 canonical JSON（两处消费者）

需自建 canonical 序列化器，**逐字节**对齐 Python `json.dumps`。消费者有二：`config_revision`（`management_api.py:1187`）与 Key 粘滞哈希 `_cache_affinity_key`（`proxy_handler.py:348`）。两者都用 `sort_keys=True` + 紧凑分隔符 + `ensure_ascii=False`。

- 键排序 → Go 的 `encoding/json` 对 map 默认按字典序输出，方向一致；
- 紧凑分隔符 → 默认输出即为 `,` / `:`；
- `ensure_ascii=False` → 用 `Encoder.SetEscapeHTML(false)`；
- **三处已知陷阱**：Go 无条件转义 `U+2028` / `U+2029`（Python 不转义）；浮点渲染 `1.0`（Python）vs `1`（Go）——配置里出现 `"request_timeout": 60.0` 这类手写浮点会直接导致哈希不一致；`map[string]any` 会把所有数字统一成 `float64`，大整数会丢精度。
- 因此 Phase 1 必须建立**穷举对拍测试**：同一输入两侧算哈希并断言相等，覆盖非 ASCII、控制字符、浮点、大整数、`U+2028`、深层嵌套。

### 4.3 指标库

- 表结构与 7 个索引完全一致（`metrics.py:888-948`）；迁移仍走 `CREATE TABLE IF NOT EXISTS` + `ADD COLUMN`，**不引入 `user_version`**（Python 版不认它，混跑期会误判）。
- `created_at` 格式化为 `YYYY-MM-DDTHH:MM:SS[.ffffff]+08:00`，**微秒为 0 时省略小数部分**；不得使用 `time.RFC3339`（定宽秒）或 `RFC3339Nano`（尾随零裁剪规则不同）。
- 分桶算术必须与 SQL 表达式等价，含负数锚点 `-28800` 与 SQLite 的整数除法截断方向。
- 并发模型**不要照搬**"单连接 + 全局锁"：Python 侧那样写是因为单句柄 + 取消安全的关停顺序（`metrics.py:118` 记录了 `to_thread` 曾导致原生访问违例）。Go 改用 **WAL 下的一写多读连接池**，避免 `all_history` 全表扫描（16 万行实测 3–5 秒）把写路径一起卡住。
- `NULL` 维度在 Python 快照里被字符串化成 `"None"`；Go 若映射成 `""` 或省略键，就改变了 `/metrics` 的 JSON 契约。
- `_normalize_usage`（`metrics.py:1108`）的跨供应商 token 归一化（OpenAI `prompt_tokens` vs Anthropic `input_tokens` + 缓存字段）必须精确移植，否则历史 token 汇总会漂移。

### 4.4 配置解析（实测确认，均已在对拍语料中固化）

以下行为由阅读代码无法推断，且差异不会报错，只会在两种实现下产生不同的路由/配置。Go 侧**刻意保持与 Python 一致**，相关断言见 `internal/config/model_test.go`、`internal/config/interop_test.go`：

| 现象 | 位置 | 处理 |
| --- | --- | --- |
| 配置文件**顶层** `upstream_routes` 被 `from_dict` 完全忽略（从空字典起步，其后只从 providers 汇总）。v1/v2 迁移会写出该字段，即那些版本升级上来的路由配置**实际不生效** | `config.py:877`、`config.py:367` | 保持丢弃；`TestParseDroppedRoutesDocumented` 固化 |
| **provider key 级** `upstream_routes` 恒为空：构造 `KeyConfig` 时未传该参数，令其后的合并循环成为死代码 | `config.py:963`、`config.py:840` | 保持丢弃；同上用例固化 |
| 只有 **provider 级 `routes`** 真正生效 | `config.py:912` | 作为对照组断言，防止把"都没生效"误判为正确 |
| `int()` 的 ValueError 文本会经管理 API **原样回给用户**（如 `invalid literal for int() with base 10: 'soon'`），属对外契约；而 `int(None)` 的 TypeError 只是解释器细节 | `config.py:873` | 两类异常分开建模：契约文本逐字复刻，内部错误仅失败关闭 |
| `int(8080.0) == 8080`（浮点向零截断）但 `int("8080.5")` 抛错；字符串接受首尾空白、`+45`、数字间下划线、Unicode 十进制数字（全角 `４５`） | Python 内建 | 移植为 `canonical.ToInt` / `ToFloat`，40 个实测用例覆盖 |

以上两项"丢弃"是既存缺陷，修复应作为**独立变更并在两种实现上同步进行**，不能在迁移中单方面修好——否则同一份配置文件会在两种实现下产生不同的上游请求路径。

### 4.5 代理行为中必须显式决策的点

代理层有三处**当前实现与直觉不符**的地方。它们在 Go 重写时要么被当作契约保留、要么作为 bug 修掉，但**必须在动手前明确决策**，否则会成为重写过程中的意外行为变更：

| 现象 | 位置 | 建议 |
| --- | --- | --- |
| 上游调用放大：`attempts × 2`（native→chat 回退）× `2`（400 触发 tool 重试），再叠加 unified/task 备选；3 个 Key 时最坏约 **24 次上游调用/次下游请求** | `proxy_handler.py:574-832` | 保留语义但**加显式上限与日志**，作为独立 issue 跟踪 |
| `images/edits` 不读配置路由（落到 `v1/{path}`），且请求体被整体缓冲，**multipart 实际不被支持**（现有测试发的是 JSON） | `proxy_support.py:279-301` | 明确"JSON-only"为契约或补齐 multipart；二选一 |
| 转换流按 chunk 做 `decode("utf-8", errors="replace")` 再按 `\n` 切分，**跨 TCP 分片的多字节 UTF-8 会变成 U+FFFD** | `protocols/anthropic.py:15`、`protocols/responses.py:15` | Go 侧用**单一字节切分器**（同时处理 `\n\n` 与 `\r\n\r\n`），顺带修掉该 bug |

其余需要精确复刻的行为：

- 请求体转换：`if not payload or "model" not in payload: return body`——**字节级透传**，不做任何适配、不注入 `stream_options`。转换分派靠**键存在性嗅探**（先 `messages`，再 `input`，否则透传）。
- 请求头：丢弃 `authorization/host/content-length/destination-addr/accept-encoding/x-api-key/anthropic-version/anthropic-beta`，强制 `Authorization: Bearer` + `Accept-Encoding: identity`，其余（含 `cookie`）转发；query string 原样转发。响应头丢弃 `content-encoding/content-length/transfer-encoding/connection`，且**重复头折叠为后者胜**。
- `/v1/messages/count_tokens` **本地计算**（`max(1, (utf8_len+3)//4)` 字节启发式），不转发、不写指标行；它位于鉴权与模型校验之后，因此未配置的模型仍返回 404。
- 非法 JSON 静默变成 `{}` → 400 "缺少 model 字段"（**不是**解析错误）。
- 冷却：`429` 立即冷却；其他状态码需累计 `key_failure_threshold`（默认 2）次；`cooldown = min(300, cooldown_seconds × failures)`，`mark_success` **删除**状态。冷却过滤是**软**的——可用集合为空时会回退到"未排除"集合，冷却中的 Key 仍可能被再次选中。
- 能力探测在**请求路径上惰性执行**：新 base_url 的首个请求会额外付出 1–2 次（计费的）上游调用；只有 404/405/501 判为不支持，401/403/429/5xx 都判为支持；正结果**永久缓存**，负结果 600s / 60s 过期。
- 流式超时的两个阶段语义不同：**响应头阶段**用 `request_timeout`（`read=None`），**响应体阶段**首块沿用同一绝对 deadline、其后每块重置 `idle_timeout` 且**无总时长上限**。一旦下游已开始收到字节，**任何失败都不再重试**，中途错误会静默终止 SSE（不发错误帧）。
- `first_token_ms` 记录的是**上游首字节**时间，而非下游首个事件时间。

### 4.6 HTTP 与 CLI

- 代理面：`/v1/chat/completions`、`/v1/messages`（含 `count_tokens`）、`/v1/responses`、`/v1/embeddings`、`/v1/images/*`、`/v1/models`、`/metrics`、`/metrics/requests`、`/metrics/series`、`HEAD /`、`/health`、`WS /ws/events`、`WS /v1/{path}`、通配 `ANY /v1/{path}`。
- 管理面 **47 条** + 运维面 **7 条**，全部要求 full 权限；`config_revision` 语义是"变更前先比对，不一致 409"。
- 已知不对称细节需保留：`DELETE` 接受可选 JSON body，但 provider/route 家族要求必填；`KeyResponse` 含 `base_url` 而 `RawKeyResponse` 不含；`api_key` 只在 `POST /api/settings/local-api-key` 返回明文，其余一律 `sha256[:12]` 指纹。
- CLI **24 个 flag**，两个等价入口名（`amkr`、`auto-model-key-router`），首匹配即返回、无互斥组；退出码 0 / 1 / **130**（Ctrl+C）。
- `/health` 的 JSON 字段被测试与发布流水线双重断言（`status`、`version`、`models`、`visitor_*`、`ops_enabled`、`webui_*`），属冻结契约。
- 鉴权用 `hmac.compare_digest` 对 UTF-8 **字节**比较（`auth.py:70`），目的是避免非 ASCII 凭据触发 500；Go 用 `hmac.Equal` 保持同等语义。
- **visitor 功能的"是否可用"是 Python 打包产物**：`visitor.py:17` 用 `itsdangerous` 是否可导入作为 runtime marker。Go 没有 extras 概念，需改为**编译期构建标签 + 运行期配置开关**，并让 `/health.visitor_feature_installed` 反映新语义（该字段被测试与发布流水线断言）。
- WebSocket 事件总线（`/ws/events`）的鉴权契约要保留：首帧 `{"type":"auth","token":...}` + 10 秒超时，失败关闭码 `4001`（超时/非法消息）/ `4003`（鉴权失败）。事件类型为 `connected` / `client_count` / `metrics_snapshot` / `config_change`，节流为"≤1 次/秒、≥1 次/30 秒、无客户端时零开销"。

### 4.7 混跑期的写冲突

两个进程都会按 mtime 热重载同一份配置，且都可能写盘。**过渡期必须指定单一写入方**：通过锁文件 + `--read-only-config` 开关，让 Go 侧先只读；待切换后再反转。`config_revision` 的不一致会让混跑的 WebUI/管理客户端互相报 409，这是设计约束而非缺陷。

---

## 5. 分阶段方案

每个阶段独立可发布、可回退；退出标准未达成不进入下一阶段。

### Phase 0：契约冻结与对拍基建（1–1.5 周）

**交付物**
- 给 CI 补上缺失的测试门禁：`pytest -q` 成为 `release.yml` / 新 `ci.yml` 的必过步骤（当前 CI 完全不跑测试）。
- 录制**差分测试语料**：把 `tests/test_app.py` 里 73 处 `MockTransport` 交互导出为可重放的请求/响应 JSON 语料（上游请求 → 期望下游响应），供 Go 与 Python 双向回放。
- 冻结契约清单文档：`/health` 字段、`config_revision` 算法、`created_at` 格式、`request_metrics` schema。
- 把 3 个 `.mjs` 前端探针接入 CI（它们已在仓库里，只是没被任何 workflow 调用）。

**退出标准**：CI 上 485 用例全绿；差分语料可被一个独立回放器重放并复现 Python 行为。

**为什么先做**：当前没有 CI 测试门禁、也没有 golden 数据，等于没有回归基线。带着这个缺口开始重写，任何行为偏差都无法被发现。

### Phase 1：Go 骨架 + 配置与领域模型（2–3 周）

**范围**：`canonical/`、`config/`（含 v1–v4 迁移、原子保存）、`keypool/`（选 Key、`only_first`/`priority`/`round_robin`、sticky、冷却、visitor 路由、unified/task plan 解析）。

**退出标准**
- Go 与 Python 对同一份配置算出**完全相同**的 `config_revision`，且对同一组 `messages` 请求算出**完全相同**的 Key 粘滞 sha256（对拍测试覆盖 4.2 节全部陷阱用例）。
- 对 `tests/test_config_service.py`、`test_config_operations.py`、`test_key_pool.py` 的行为对等移植测试全绿。
- v1/v2/v3 历史配置样例迁移到 v4 后与 Python 输出逐字节一致。
- 此时 Go 二进制可 `--show-config` / `--show-unified-model`，**只读**，不监听端口。

**风险**：canonical 序列化器是本阶段唯一的硬骨头，且它有**两个消费者**（`config_revision` 与 Key 粘滞哈希），任一偏差都是静默失效。务必先用对拍测试锁死再往下走。

### Phase 2：指标与持久化（2 周）

**范围**：`metrics/` 全量——schema 初始化与 `ADD COLUMN` 迁移、`record`、`snapshot`、`request_history`、`time_series`、`key_stats`、`_normalize_usage`。

**退出标准**
- Go 直接打开**现有的** `data/metrics.sqlite3`，一侧写入后另一侧读到相同聚合结果（双向互操作测试）。
- `/metrics`、`/metrics/requests`、`/metrics/series` 的 JSON 与 Python 逐字段一致（含 `"None"` 维度、零填充桶、`complete` 标记）。
- 北京时区跨零点分桶、`MAX_SERIES_POINTS=500` 的 422 行为一致。
- 新增 Go 侧并发压测：`all_history` 全表扫描期间写路径不被阻塞。

**风险**：时间戳格式与分桶算术；这两处出错是静默算错，必须用固定时间戳数据集断言。

### Phase 3：代理与协议（3–4 周，核心）

**范围**：`proxy/`（`_prepare_proxy_request` → `_handle_single_target` → `_execute_attempt` 全链路）、重试与降级状态机、`protocol/`（Anthropic 与 Responses 的请求体转换 + SSE 事件重建）、流式超时、native endpoint 探测与缓存、`runtime/` 租约。

**退出标准**
- 差分语料全部回放通过——优先复用 `test_app.py` 的 111 个用例语义。
- 明确断言的重试语义：`RETRYABLE_STATUS_CODES = {401,403,429,500,502,503,504,521}`（注意 **401/403 是可重试的**，会换 Key）；三级状态机（Level A 备选模型 / Level B Key 轮换 / Level C 单次尝试）各自的尝试预算；`RetryPolicy` 的三档规则（指定 Key、`only_first`、单 Key → `max_retries+1`；否则 → `key_count`，即**一轮且无额外重试**）；首字节超时且不换 Key 时立即返回而非重复等待。
- 流式：SSE 分帧、首块与块间超时、下游流建立后不重放；转换流使用**单一字节切分器**。
- 请求头净化与响应头折叠（后者胜）一致。
- `X-AMKR-Fallback` 响应头在备选成功时出现。
- 每事件显式 `http.Flusher.Flush()`（原实现是 `await asyncio.sleep(0)`，Go 无对应物）。
- **租约用 `defer` 显式释放**，并新增"客户端中途断开"与"关停时仍有活跃流"两个测试，证明不会死锁。`finish()` 必须**恰好执行一次**，它是流式尝试唯一的 Key 释放路径。

**风险**：这是全项目语义最密的部分。三处特别脆弱：(1) httpx 的超时/异常模型——`except httpx.RequestError` 是**兜底捕获**（含 Timeout/Network/Protocol/DecodingError/TooManyRedirects），Go 没有单一根类型可对应；响应头 deadline 与逐块 idle deadline 是两套机制，而 Go 取消 `context` 会终止整个请求；(2) 流式生成器的 `finally` 语义是唯一的 Key 释放路径，Go 必须改成显式 `defer` 且覆盖全部退出路径；(3) WebSocket 代理不是 WS↔WS 桥接，而是 HTTP-over-WS 垫片，需要自建实现 `http.Flusher` 的捕获式 `ResponseWriter`，并按同一规则过滤握手头（注意 Go 会规范化头名，而 Python 比较的是原始小写字节）。

### Phase 4：管理 API + 运维 API（2–3 周）

**范围**：`mgmt/`（47 条）、`ops/`（7 条）、配置热重载、探针（异步 + 同步两种）、配置导出/导入。

**退出标准**
- 逐条对齐 47 + 7 条路由的方法、路径、请求体校验（`extra="forbid"` → 未知字段 422）、状态码、响应体形状。
- `config_revision` 冲突返回 409 且**先于任何变更**。
- 运维接口在 `ops_enabled=false` 时整体 404，且 `/health.ops_enabled=false`。
- 运维接口返回的富文本降级为等价的纯文本（原实现用 Rich 渲染后转纯文本，属不透明字符串）。

**注意**：`/api/service/*` 与 `/api/integrations/*` 的真实副作用本阶段先接桩，实际实现放 Phase 5。

### Phase 5：WebUI + CLI + 服务注册与集成（2–3 周）

**范围**
- `webui/`：用 `go:embed` 原样托管现有 19 个静态文件，**零改动**；`/ui/` 在任意挂载前缀下都能工作（前端已从 `location.pathname` 自动推导 API 基址）。
- 非交互 CLI：`--config/--host/--port/--version/--show-*/--switch-*/--status/--stop/--serve/--check-update/--service/--get-key` 等脚本化能力。
- `service/`：PID 文件与进程存活判断、detached 启动、Windows `schtasks` + UAC 提权、systemd user unit + `loginctl enable-linger`。
- `agentcfg/`：Claude Code / Codex / Pi Agent 配置改写与回滚（原子写 + 备份）。

**退出标准**
- WebUI 全部页面在 Go 服务下功能正常；3 个 `.mjs` 探针在 CI 中通过。
- `--get-key`、`--show-config`、`--status`、`--serve` 的 stdout 与退出码与 Python 一致。
- Windows 与 Linux 的服务注册/卸载在真实环境验证（当前 CI 完全没有覆盖这些平台行为，需新增 windows-latest / ubuntu-latest 矩阵）。
- 集成改写不会破坏用户已有的 Claude Code / Codex 配置（备份可回滚）。

### Phase 6：Go TUI（4–8 周，可与 Phase 4/5 并行）

**范围**：`tui/` 用 Bubble Tea 重建 `tui.py + dashboard.py + logs_tui.py + config_editor.py` 的能力：供应商/Key/模型/统一模型/任务管理、日志与统计浏览、CLI 设置、服务注册向导、更新。

**退出标准**：与 Python TUI 的功能对等清单逐项打勾（**不要求逐像素一致**，但键盘流、菜单层级、危险操作二次确认需一致）。

**说明**：原 `tui.py` 是自研全屏框架（`ctypes`/`msvcrt`/`termios` 按键、SGR 鼠标滚轮、Rich 视口），没有 1:1 的 Go 对应物，**这是重写而非移植**。建议先用一个"最小可用 TUI"（只做配置 CRUD + 服务控制）打通，再逐步补齐，避免一次性投入 6 周却无法交付。

### Phase 7：发布、更新与 Python 退役（2 周）

**范围**
- 发布：GoReleaser 风格的交叉编译矩阵（linux/darwin/windows × amd64/arm64）——注意现有流水线**没有任何跨平台构建步骤**，这是净新增工作。
- Docker：`python:3.12-slim` → distroless/静态镜像；保持可观测不变量（非 root、`VOLUME /data`、`EXPOSE 8000`、`XDG_CACHE_HOME=/data`、CMD 用 `0.0.0.0`）。
- 更新：保留 `/api/update/check` 与 `--check-update` 的**响应契约**，但指向 Go release 产物；自更新改为"下载 → 校验 sha256 → 原子替换二进制"。现有实现抓取了 `artifact_sha256` 却**从不校验**，Go 版必须补上。
- 版本单一来源：`pyproject.toml:7` 目前被 5 处消费（两个 workflow、`scripts/release.py`、`test_version.py`、`/health` 断言），Go 侧改用 ldflags 注入并保证 `/health.version` 一致。
- 保留双命令名 `amkr` 与 `auto-model-key-router`。
- 退役 Python：删除/归档 `auto_model_key_router/`，保留 `tests/test_app.py` 作为历史规范参考。

---

## 6. 并行共存与灰度切换

```
Phase 0-2   Python 提供服务        Go 只读（--show-config / 离线对拍）
Phase 3-4   Python 提供服务        Go 在备用端口跑同一份配置，差分回放比对
Phase 5     Go 提供服务（主）      Python 保留为回退 + TUI
Phase 6     Go 提供服务 + TUI      Python 冻结
Phase 7     Go 唯一实现            Python 退役
```

**切换闸门**：每个 Phase 结束都满足「回放语料全绿 + 关键端点逐字节一致 + 至少一个真实客户端（Claude Code 或 Codex）端到端跑通」，才允许把默认服务切成 Go。

**回退**：任一阶段保留上一版二进制，切换点是配置与端口而非数据格式，因此回退只需换回二进制。

---

## 7. 一致性验证策略

| 层级 | 手段 | 覆盖 |
| --- | --- | --- |
| 单元 | Go 原生测试，语义对齐 Python 同名测试 | config 迁移、keypool 选择、metrics 聚合 |
| 对拍 | Python 与 Go 对同一输入算哈希/输出，断言逐字节相等 | `config_revision`、`created_at`、配置文件序列化 |
| 差分回放 | Phase 0 导出的 73 组上游交互语料 | 代理与协议转换 |
| 契约 | 直接移植 `test_app.py` / `test_management_api.py` 的断言 | HTTP 面 |
| 互操作 | Go 与 Python 交替读写同一 SQLite / 配置文件 | 混跑期数据安全 |
| 前端 | 复用 3 个 `.mjs` 探针（驱动真实 `webui/` 模块） | WebUI 未回归 |
| 端到端 | 真实 Claude Code / Codex 接入 | 用户可见行为 |

**建议**：Go 侧不追求测试数量对等（Python 有 119 个 TUI 单测本质是测 Rich 渲染，无移植价值），而是追求**契约覆盖对等**——预计保留 ~300/485 用例的语义，其余（`test_tui.py` 119、`test_release_script.py` 25、`test_packaging.py` 13、`test_update.py` 大部分、`test_version.py`）随对应模块重写而重做。

---

## 8. 风险登记表

| # | 风险 | 影响 | 缓解 |
| --- | --- | --- | --- |
| 1 | 租约依赖 async generator finalizer | 关停永久阻塞 | 改显式 `defer`；新增断流/关停用例 |
| 2 | canonical JSON 不一致（浮点、`U+2028`） | 同时破坏 `config_revision` 与 Key 粘滞，均静默失效 | Phase 1 穷举对拍两个消费者，先锁死再推进 |
| 3 | `created_at` 格式偏差 | 静默算错统计 | 固定时间戳数据集断言；禁用手写格式化外的 API |
| 4 | 指标库无 schema 版本 | 混跑期互相写坏 | 不引入 `user_version`；互操作测试作为门禁 |
| 5 | 双进程写配置 | 配置丢失 | 过渡期单一写入方 + 锁文件 |
| 6 | httpx 超时/异常模型无 Go 对应物 | 重试与超时行为偏差 | 明确拆分 header deadline 与 idle deadline；自建错误分类 |
| 7 | 上游调用放大最坏 ≈24× | 计费与限流风险 | 保留语义但加显式上限与日志，单独跟踪 |
| 8 | TUI 重写工作量被低估 | 拖期 | 拆成"最小可用 TUI"增量交付 |
| 9 | Windows/systemd 行为无 CI 覆盖 | 平台回归 | 新增 windows/linux/macos 测试矩阵 |
| 10 | 自更新无校验和 | 供应链风险 | Go 版强制 sha256 校验 |
| 11 | 时间与既有 305 commits / 72 releases 的高频发版节奏冲突 | 合并冲突 | 迁移期间 Python 侧冻结大重构；Go 进 `go/` 子目录减少冲突面 |

---

## 9. 提交与发布

沿用仓库既有规范：每完成一个模块立即独立提交，`<type>(<scope>): <中文摘要>`，正文说明"改了什么 / 为什么改 / 如何验证"。

建议的提交粒度（示例）：
- `docs(go-migration): 增加 Go 迁移方案`
- `build(ci): 为发布流水线补上测试门禁`
- `feat(go/config): 实现 v1–v4 配置迁移与原子保存`
- `test(go/canonical): 对拍 config_revision 序列化`

**版本策略**：建议 Go 版以 `5.0.0` 发布（数据格式兼容但运行时替换，属破坏性变更），并在 `CHANGELOG.md` 明确"配置与指标库可直接沿用，无需手工迁移"。

---

## 10. 工作量与排期估算

| Phase | 内容 | 估时 |
| --- | --- | --- |
| 0 | 契约冻结与对拍基建 | 1–1.5 周 |
| 1 | 骨架 + 配置/领域模型 | 2–3 周 |
| 2 | 指标与持久化 | 2 周 |
| 3 | 代理与协议 | 3–4 周 |
| 4 | 管理/运维 API | 2–3 周 |
| 5 | WebUI + CLI + 服务与集成 | 2–3 周 |
| 6 | Go TUI | 4–8 周（可并行） |
| 7 | 发布/更新/退役 | 2 周 |
| | **合计（TUI 并行）** | **14–19 周** |

按 Phase 0–5 顺序推进（约 12–16 周）即可达到「服务端可切换为 Go」的里程碑；TUI 是唯一可独立延后的部分。

---

## 11. 明确不做的事

- **不改数据格式。** 不引入 `user_version`、不重命名列、不改时间戳格式——即便某些设计（单连接 + 全局锁、字符串时间戳、无保留策略导致表无限增长）在 Go 里本可以更好。
- **不顺手重构 Python 侧。** 迁移期间 Python 只接受 bugfix，不接大重构，避免把迁移变成两线作战。
- **不在本阶段清理文档漂移。** 已发现 `docs/API.md` 记录了 `/api/logs` 的 `tail` 参数但代码未实现、且遗漏了 `/api/models/{id}/keys/{name}/stats`；以**代码为准**建立 Go 契约，文档修正单独提 issue。
- **不追求 TUI 像素级一致。**
- **不保留 Python 嵌入 API**（`mount_app`）——按决策改为 Go 库 + 独立进程两种集成方式。
