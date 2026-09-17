# AMKR Go 迁移进度与交接

配套文档：[迁移方案](./2026-09-17-go-migration.md)（架构决策、兼容性契约、风险登记）。

本文只记**当前状态**、**如何自行验证**、**待决策项**，不重复方案里的论证。

## 总体进度

| 指标 | 数值 |
| --- | --- |
| Python 核心 | 15,630 行 / 41 文件 |
| Go 生产代码 | 23,088 行 / 72 文件（**21 个包已提交**） |
| Go 测试代码 | 17,445 行 / 49 文件 |
| 已移植 Python 源 | ≈ 10,300 行（约 66%） |
| 服务端待移植 | ≈ 5,298 行；其中 app.py 与 ops_api.py 正在做 |
| Go 相关提交 | 34（触及 internal/、go.mod 或 cmd） |

**代理面与管理面已全部落地**：`internal/proxy`（proxy_handler.py，81 条语料含逐条上游调用
序列）与 `internal/api`（management_api.py，47 条路由，188 条语料）已提交并通过门禁。

按方案的分期，**Phase 0–2 基本完成**（基础设施、配置与 canonical 兼容、运行时与
协议转换）。剩下的体量集中在一个线性依赖链上：

```
proxy_handler、management_api 已完成并提交
   └─> app 装配（internal/server）+ 最小可运行 CLI（进行中）  ← 「能用」的最后一段
   └─> ops_api                276 行（进行中；4 条前端端点里 3 条依赖已就绪）
   └─> config_editor          1,878 行
   └─> service / 完整 24 flags CLI
          └─> TUI（Bubble Tea，重写而非直译） 912 行
```

**WebSocket 不在「能用」的关键路径上**：前端是**轮询**（`setInterval` 打 `/health`、
`/metrics*`、每 2 秒 `/api/logs`），全仓库搜 `new WebSocket` / `/ws/events` 为空。
`WS /ws/events` 与 `WS /v1/{path}` 可排到 WebUI 可用之后。

也就是说：**主要并行度已经用尽**，剩下的模块之间存在硬依赖。当前同时推进
`proxy_handler` 与 `management_api` 两条线；再往上（app 装配、CLI）必须等它们定型。
服务端可切换的乐观估计仍是 **5–7 周**（原方案 12–16 周，靠并行与决策 7/8 砍量压缩）。

## 已落地的 Go 包

全部位于 `internal/`，每个包都带从**真实 Python 生成**的对拍语料。

| 包 | 对应 Python | 备注 |
| --- | --- | --- |
| `canonical` | （json 兼容层） | 逐字节兼容，两处静默失败面的基础 |
| `config` | `config.py` | v1~v4 迁移、原子保存、CRLF 语义 |
| `keypool` | `key_pool.py` 等 | key 选择、冷却、端点能力缓存 |
| `runtime` | `runtime.py` `streaming.py` | 租约、流式超时、重试策略 |
| `auth` | `auth.py` `visitor.py` | 访客功能常驻（开关语义已取消） |
| `protocol` | `protocols/*.py` | anthropic / responses / request |
| `proxysupport` | `proxy_support.py` | 请求构造、宽容解码、头部语义 |
| `upstream` | （httpx 行为） | 重定向、压缩、连接池、错误分类 |
| `metrics` | `metrics.py` | SQLite 19 列 schema，无 `user_version` |
| `configops` | `config_operations.py` | 全部 64 个函数，管理 API 的前置 |
| `configservice` | `config_service.py` | 读-改-写 + 按路径共享锁 |
| `unifiedmodel` | `unified_model.py` | unified 目标切换入口 |
| `health` | `app.py` 的 `/health` | 冻结契约（已按决策 4 删一个字段） |
| `formatting` | `formatting.py` | 含手写的 urlsplit/urlparse 最小实现 |
| `logfiles` | `log_files.py` | 日志发现与归档 |
| `clipboard` | `clipboard.py` | 平台分发 + OSC52 回退 |
| `servicestatus` | `service_status.py` | schtasks XML / systemd 解析 |
| `proxy` | `proxy_handler.py` | 代理编排枢纽；语料含逐条上游调用序列 |
| `api` | `management_api.py` | 47 条管理路由；`ConfigPath`/`Reload`/`CheckUpdate` 等接缝 |
| `webui` | `webui.py` | 静态资源服务（`fs.FS` 注入，不做目录列表） |
| `updatecheck` | `update.py` 的版本检查 | 决策 8 只保留版本检查，砍自更新 |

**更正（提交 a63e65b 的信息与实际内容不一致）**：该提交的正文写「96 条用例 /
134,097 字节 / 153 个子测试 / D1-D7」，实际落在 HEAD 里的是 **91 条用例 / 135,535 字节 /
145 个子测试 / D1-D8**。原因是 agent 在第一版报告后又做了一轮收尾加固（新增 3 条
「tomlkit 拒绝而 go-toml/v2 接受」的合法性检查，记为 D8），语料与测试计数随之变化，而
我提交时引用的是第一版报告的数字。**内容是对的、信息是旧的**；提交后已有后续提交，故不改写
历史，在此更正。教训：并发写入时提交信息里的数字应在 `git add` 之后重新采集，而不是引用
较早的报告。
**进行中**：`app.py` -> `internal/server`（+ 最小 CLI）、`ops_api.py` -> `internal/api` 的
ops 路由、`agent_config.py` -> `internal/agentconfig`。
## 组装后的完整路由表（目标，共 63 条）

用 Python AST 从 `management_api.py`(47) + `ops_api.py`(7) + `app.py`(9) 提取，作为
「Go 版是否完整」的对照清单。AST 提取的好处是跨行装饰器也能取到。

### 代理面（9 条，app.py）

| 路由 | 状态 |
| --- | --- |
| `GET /health` | ✅ 已移植（`internal/health`） |
| `ANY /v1/{path}` | ✅ 已提交（`internal/proxy`） |
| `HEAD /` | 待 app 装配 |
| `GET /v1/models` | 待 app 装配 |
| `GET /metrics` | 待 app 装配（依赖 metrics） |
| `GET /metrics/requests` | 待 app 装配（依赖 metrics） |
| `GET /metrics/series` | 待 app 装配（依赖 metrics） |
| `WS /ws/events` | 待定（**非「能用」必需**，前端是轮询） |
| `WS /v1/{path}` | 待定（同上） |

### 管理面（47 条，`internal/api`）

**已实现并提交** 47/47。核对方式是把我方路由表与 Python AST 提取结果做**双向差集**：
既无缺失、也无多余（全量比较，0 差异）。18 个测试覆盖，其中易错点各有具名测试
（config_revision 先比对再落盘、裸 ValueError 的 500/422 层级、DELETE body 的不对称、
KeyResponse 与 RawKeyResponse 的差别、明文 key 只在设置接口返回、任务参数白名单两处校验）。

**前端覆盖度交叉核对**：WebUI 调用的 15 个 `/api` 端点中 **11 个已覆盖**；缺的 4 个正是
运维面（`/api/integrations`、`/api/logs`、`/api/tool`、`/api/tool/webui`），其中 3 条依赖
已就绪（logfiles / configservice / updatecheck），第 4 条等 `agent_config`。

### 运维面（7 条，`ops_api.py`）

| 路由 | 阻塞情况 |
| --- | --- |
| `GET /api/logs` | 可做（`internal/logfiles` 已就绪） |
| `POST /api/tool/webui` | 可做（只需 ConfigService） |
| `GET /api/integrations` | 等 `agent_config`（进行中） |
| `POST /api/integrations/{agent}` | 等 `agent_config` |
| `POST /api/integrations/{agent}/rollback` | 等 `agent_config` |
| `POST /api/service/{action}` | **阻塞**：惰性 import `service.py`（720 行，未移植） |
| `GET /api/tool` | **阻塞**：惰性 import `update.py` 的 `check_latest_version` |

### 决策 8 的实际影响（比最初估计大）

`update.py` 共 713 行，其中**约 140 行是版本检查**（`version_numbers` / `comparable_version`
/ `is_newer_version` / `fetch_json` / `check_latest_pypi` / `check_latest_release` /
`check_latest_version` / `render_version_check_result`），**约 573 行是自更新机制**
（`install_latest_*`、`windows_update_helper_script` 单函数就 142 行、`uv_tool_*`、
`detected_installation_method` 等）。

决策 8 砍掉 `update.py` 会打断**两条路由 + WebUI 的「检查更新」**：
`POST /api/update/check`（management 面）与 `GET /api/tool`（ops 面）。

**建议**（待确认）：只砍自更新那 573 行；保留版本检查，因为它与分发方式无关，而前端在用。

## 自行验证

所有命令都已实测通过（`-count=1` 跳过缓存）：

```powershell
# 全仓库
gofmt -l .            # 必须无输出
go vet ./...          # 必须无输出
go test -count=1 ./...

# 语料新鲜度闸门（CI 里逐个独立执行）
python -X utf8 scripts/gen_canonical_corpus.py --check
python -X utf8 scripts/gen_config_corpus.py --check
python -X utf8 scripts/gen_config_model_corpus.py --check
python -X utf8 scripts/gen_keypool_corpus.py --check
python -X utf8 scripts/gen_proxysupport_corpus.py --check
```

每条 `--check` 都验证过**有牙齿**：篡改一条期望值后退出码 1，恢复后逐字节一致。

Python 侧基线：`python -X utf8 -m pytest -q` → **485 passed**（约 250 秒）。

### 环境注意（Windows）

- `-race` **不可用**：本机没有 cgo 工具链（`go: -race requires cgo`）。竞态相关
  断言只能靠显式同步与 `-count` 重复运行替代。
- PowerShell 控制台是 GBK，中文输出会乱码；一律用 `python -X utf8` 并设
  `$env:PYTHONIOENCODING='utf-8'`。
- 行内 `python -c` 带引号会被 PowerShell 破坏，改用临时 `.py` 文件。
- `go test` 与 `gofmt -w` 之后立刻编辑同一文件会报「file changed since it was
  read」，需重新读取。

## 已决策项（2026-09-17 产品决策，全部已落地或有明确归属）

代理层三项来自迁移方案 §4.5，另有四项在迁移过程中实测确认。**8 项已全部定案**：

| # | 事项 | 决策 | 落地状态 |
| --- | --- | --- | --- |
| 1 | 上游调用放大（3 Key 时最坏 ≈24 次上游调用/次下游请求） | **保留语义，另加显式上限与日志**，作独立 issue 跟踪 | 待 proxy_handler 阶段实施 |
| 2 | `images/edits` 不读配置路由 + multipart 不支持 | **路由与 multipart 都补齐** | 路由已修（`fc7dc99`）；**multipart 仍待做**，需 proxy_handler 的请求体处理 |
| 3 | 流式分块多字节 UTF-8 变 U+FFFD | **接受 Go 的修复**（`SSESplitter` 按字节切分） | 已实施，Go 比 Python 正确 |
| 4 | `visitor_feature_installed` 判定依据 | **取消开关语义**，访客功能常驻；该字段从 `/health` **删除** | 已实施（`ea8e2c8`、`4b36ca2`，破坏性变更） |
| 5 | `protocol` 对畸形输入不复刻异常 | **保留兜底，但在接口边界显式 400 拒绝畸形体** | 兜底已实施；**边界 400 待 API 阶段实施** |
| 6 | `upstream.UpstreamURL` 斜杠处理与 Python 不一致 | 修正为 `TrimRight`/`TrimLeft`（非决策，属缺陷修复） | 已交由 upstream 模块处理 |
| 7 | `dashboard.py`（949 行）+ `logs_tui.py`（614 行）终端仪表盘 | **砍掉，由 WebUI 覆盖** | 无需移植，直接减少约 1,563 行工作量 |
| 8 | `update.py`（713 行）自更新 | **砍掉，改由 `go install` / 包管理器分发** | 无需移植，减少 713 行工作量 |

决策 4 的连带语义（已实施）：`visitor_key_count` 不再在「功能未安装」时归零，恒为各
模型之和；`visitor_access_enabled` 退化为单纯的「数量 > 0」。

决策 7、8 使剩余服务端工作量从约 10,113 行降至约 **7,837 行**。

### 决策 2 的未完成部分（勿遗漏）

路由一半已修（`fc7dc99`），但 **multipart/form-data 仍不支持**：请求体当前按 JSON
解析并整体缓冲，真正的图片编辑请求无法工作。补齐它要动 proxy_handler 的请求体处理
路径，而该模块尚未移植，因此拆成两步。实施时需一并考虑：上传体积上限、流式转发而非
整体缓冲、以及错误路径的响应格式。

### 决策 5 的未完成部分（勿遗漏）

`protocol` 侧的保守兜底保留不动，但需要在 **HTTP 接口边界**加显式校验：非法请求体
（非对象、字段类型错误）应返回 400，而不是被内部函数静默兜底成「看似合理」的结果。
这一层属于 `app` / `management_api` 装配阶段的工作。
## 刻意保留的参照实现缺陷

为「同一份配置在两种实现下行为一致」，以下缺陷**不在 Go 侧修复**，各由具名测试锁定：

- 顶层 `upstream_routes` 被 `from_dict` 完全忽略，但 `migrate_config_data` 会写它
  → v1/v2 的路由被静默丢弃。`config.py:877`
  → `TestParseDroppedRoutesDocumented`
- `KeyConfig` 构造时未传 `upstream_routes=`，使合并循环成为死代码。
  `config.py:963`、`config.py:840-844`
- 实际**只有** `providers[].routes` 生效。

## 下一批工作（按依赖顺序）

1. 收尾并提交 `internal/metrics`（SQLite 19 列 schema、7 索引、无 `user_version`、
   `created_at` 为 Asia/Shanghai 的 isoformat 字符串）。
2. 收尾并提交 `internal/upstream` 与协议测试。
3. `internal/configops`（`config_operations.py`，1,282 行）→ 解锁管理 API。
4. `proxy_handler`（1,400 行，需要 metrics 定型后开工）。此步必须一并落地：
   - 决策 1 的**上游调用上限与日志**；
   - 决策 2 剩余的 **multipart/form-data 支持**；
   - 决策 5 的**接口边界 400 校验**（也可放在第 6 步的装配层）。
5. `management_api` + `config_editor`。
6. `app` 装配、`service`、CLI、WebUI 后端。
7. TUI 重写（`tui.py` 912 行）。注意 `dashboard.py` 与 `logs_tui.py` 已按决策 7
   砍掉，不要顺手一并移植。
8. Python 退役。`update.py` 已按决策 8 砍掉（改由 `go install` / 包管理器分发），
   不再移植。

## 阶段 3：Python 退役顺序（已实测计算，非估计）

用 AST 解析全部 Python 模块的真实 import 边，算出「反向依赖数」与「还被哪些语料生成器
（传递地）需要」，据此定序。**踩过的坑**：`from . import X` 这种形式（`config_editor.py:33`
与 `management_api.py:33` 都用它导入 `config_operations`）在第一版脚本里被漏掉了，导致
`config_operations` 被误判为「没有生成器需要」。修正后它是 4。**删除顺序必须建立在这张
修正过的表上**，否则会删掉仍被 app 驱动型生成器需要的模块。

### 解锁前提（必须先做）

`auto_model_key_router/__init__.py:29` 的 `from .app import create_app, mount_app` 会让
**导入任意子模块都执行 `__init__`**，从而拉进整个 app。实测后果：8 个生成器的传递闭包
覆盖全部 35 个模块，于是删掉任何一个 app 依赖的模块都会让**所有**生成器失效。

因此第一步是给 `__init__.py` 瘦身（只留 `__version__` 与 `_resolve_version`）。这与决策 3
一致——`mount_app` 嵌入 API 已决定退役；其唯一使用者是 `tests/test_embedding.py`，该测试
随之一并退役。

### 第一批（自包含簇，删除不影响任何门禁）

| 模块 | 行数 | 反向依赖 | 生成器需求 | 删除依据 |
| --- | --- | --- | --- | --- |
| `dashboard.py` | 949 | 1（main） | **0** | 决策 7：由 WebUI 覆盖，Go 永不实现 |
| `logs_tui.py` | 614 | 2（main, dashboard） | **0** | 决策 7 同上 |
| `unified_model.py` | 65 | 2（dashboard, main） | **0** | Go 侧 `internal/unifiedmodel` 已提交并测试 |

三者的反向依赖**全部落在簇内**，所以可以一起删。注意 `main.py` 引用了 dashboard/logs_tui，
需要同步去掉对应子命令（而完整 24 flags CLI 尚未移植，故 `main.py` 本身**暂不删**）。

`protocols/__init__.py` 反向依赖为 0，但它属于 protocols 簇，随簇一起删。

### 后续批次（按反向依赖数升序，每批删完必须复测）

反向依赖越多越靠后：`clipboard`/`endpoint_capabilities`/`key_health`/`ops_api`/
`proxy_handler`/`routing`/`streaming`/`websocket_proxy` 等为 1；`app`/`agent_config`/
`config_editor`/`log_files`/`management_api`/`runtime`/`webui` 为 2；`proxy_support`/
`formatting`/`update` 为 3–4；`auth`/`key_pool`/`service` 为 5；`config_service`/`tui` 为
6；`visitor` 为 8；`metrics` 为 9；**`config` 为 18（最后）**。

**被生成器需要 = 4–8 的那些模块不能先删**，因为它们被 app 驱动型生成器
（management_api / proxy_handler / server / ops_api）传递依赖；那些生成器本身要先退役。

### 每个模块的删除清单

1. 确认 Go 侧已验证：差分语料回放全绿 + 真实运行冒烟覆盖到该模块。
2. 退役该模块的语料生成器（`scripts/gen_*.py`）与 CI 里的 `--check` step——**语料保留在
   仓库里作为冻结夹具**，Go 测试继续回放它，回归价值不丢；丢掉的只是「Python 是否漂移」
   的检测能力，而 Python 正在退役，这可以接受。
3. 删除 Python 模块与其 Python 测试。
4. 清理引用：`pyproject.toml` 的打包清单、`__init__.py` 的再导出、其它 Python 模块的
   import、`scripts/` 下仅为其服务的脚本。
5. 复测：`gofmt -l .`、`go vet ./...`、`go test ./...`、以及**剩余未退役门禁**全部仍为绿。
6. 独立提交，提交信息写明删除依据（哪些测试/冒烟证明它已被 Go 取代）。

### `scripts/` 清理清单（实测清单，Python 退役后）

| 文件 | 行数 | 处置 |
| --- | --- | --- |
| `gen_*_corpus.py`（**18 个**） | 14,196 | 全部删除——它们 import 真实 Python 参照实现；语料已冻结在 `internal/*/testdata/` |
| `release.py` | 639 | 删除；改为 Go 的发布流程（`release.yml` 的 Python 分支一并退役） |
| `webui_preview.py` | 502 | 删除；其职责（托管前端静态资源）由 Go 二进制自身的 `/ui` 承担 |
| `webui_preview_check.py` | 39 | 删除（依赖上面的预览服务） |
| `webui_smoke.ps1` | 83 | 删除或改写为针对 Go 二进制 /ui 的冒烟（依赖预览服务与浏览器） |
| **`webui_module_check.mjs`** | 66 | **保留**——纯 JS 检查前端 ES 模块 import 图，与后端语言无关，前端资产仍随包发布 |

合计待清：**22 个脚本、约 15,400 行**。

### 发布与 CI 面的退役（需替换而非仅删除）

| 文件 | 行数 | 处置 |
| --- | --- | --- |
| `.github/workflows/release.yml` | 181 | **整条替换**：读版本、`python -m build`、`twine check`、wheel 冒烟、容器镜像、推 GHCR —— 全部围绕 Python 打包 |
| `.github/workflows/publish-pypi.yml` | 34 | 删除（PyPI 发布不再需要） |
| `.github/workflows/ci.yml` 的 `python` 作业 | 163（整个文件） | 最终只留 `go` 作业；16 条 `--check` 门禁随生成器逐一退役 |
| `pyproject.toml` 的打包段 | — | 只剩前端资产（若仍随 Go 二进制发布则也可一并去掉） |

替换后的 Go 发布面应产出**交叉编译的二进制**（`GOOS/GOARCH` 矩阵）+ GHCR 镜像，其中前端
资产由 `webui_assets.go` 的 `//go:embed` 编进二进制——这正是当初把 `go.mod` 放在仓库根的原因。

注意 `release.yml` 的 wheel 冒烟里有一条断言
`assert not visitor_feature_available()` / `assert visitor_feature_available()`（靠装不装
`[visitor]` extra 区分）。该语义**已按决策 4 取消**（访客功能常驻、去掉开关），所以这条断言
随 Python 一起消失，不需要在 Go 侧找对应物。
`webui_module_check.mjs` 是唯一与后端语言无关的，
CI 里应保留它的 `node` 步骤。
最终：`python` CI 作业整体删除，只剩 `go` 作业；`pyproject.toml` 与 `release.yml` 的 Python
分支一并清理。
## 提交约定

按模块独立提交，Conventional Commits：`<type>(<scope>): <中文摘要>`，正文说明
**改了什么 / 为什么改 / 如何验证**。提交前用 `git diff --cached --name-only`
确认只含当前模块文件。