# Changelog

## [Unreleased]

### Added

- 新增 **embeddings（嵌入）路由**：`POST /v1/embeddings` 走与图像同构的第三种 unified 目标 —— 请求 `unified-model` 时命中 `unified_model.embeddings` 计划（`primary` + 可选 `fallback`），未配置该计划则继承 `default.primary`（不继承 `default.fallback`），其余 Key 路由、重试与熔断语义与既有目标完全一致。上游路径默认 `v1/embeddings`，可按上游 URL 配置 `upstream_routes[base_url].embeddings` 覆盖（别名 `embedding` / `embed`），TUI / WebUI / 管理 API / `--unified-target` 都已能读写该计划。
  - **关键修复**：嵌入请求体此前会被当成 chat 体做协议适配，`{"model": ..., "input": "..."}` 被改写成 `{"model": ..., "messages": [...]}` 发给上游 —— 上游收到一个没有 `input` 的 chat 请求，必然失败。现在嵌入路径只替换 `model`，`input` / `encoding_format` / `dimensions` 等字段原样转发。
  - 原因值得记一笔：路径 → 目标类型的分类原本在 `KeyPool`、`proxy_handler` 里各写了一份 `"image" if path in ("images/generations", "images/edits") else "default"`，加第三类时必然漏一处。现已收敛为 `proxy_support.request_route_kind()` 一份。
- 新增**任务路由**：把任务名（`TASK_XXXXXX`）直接当 `model` 传给 AMKR，由配置里的任务表决定真实模型（首选 + 一个备选）和一组固定采样参数（`temperature`、`top_p`、`top_k`、`frequency_penalty`、`presence_penalty`、`seed`、`stop`，以及 `reasoning_effort`）。调用方无需知道任何模型名，也拿不到改参数的余地：
  - 任务固定的采样参数由 AMKR 覆写；调用方**再传同名参数会被 400 拒绝**，而不是被静默覆盖 —— 静默覆盖会让调用方以为自己传的值生效了。`max_tokens` 等不在白名单内的参数照常透传。
  - `reasoning_effort` 是唯一的例外：它同样被任务覆写，但不拒绝调用方传入（Claude Code / Codex 这类客户端框架会自动带上它，拒绝等于让任务路由不可用）。
  - 首选模型重试后仍返回可重试状态码时，自动切到任务的备选模型，响应带 `X-AMKR-Fallback: true`，与统一模型回退语义一致。
  - 任务名不能与模型 ID、别名、隐藏别名或 `unified-model` 撞名：否则 `resolve_route` 的语义会取决于查表顺序。任务也不接受调用方指定 Key（`TASK_XXXXXX[main]` 会被 400 拒绝），Key 仍由模型自身的路由模式决定。
  - 配置写在顶层可选的 `tasks` 键下（`config_version` 仍为 4，旧配置照常加载）。参数名写错（如 `temprature`）在**保存时**就会报错，不必等到请求时才发现。
  - WebUI 新增「任务路由」页（配置组）用于增删改查；管理 API 新增 `GET/POST /api/tasks` 与 `GET/PUT/DELETE /api/tasks/{task_name}`，带与其它写接口一致的 `config_revision` 乐观并发控制。任务随 `/api/config/export`、`/api/config/import` 一起迁移（导入时引用不到模型的任务会被跳过，与「模型从未配置」一致）。
  - 删除模型时会一并清理引用它的任务：首选模型没了则整个任务删除，只有备选没了则退化为单模型任务。
  - 走原生 Anthropic（`/v1/messages`）时，任务固定的 `stop` 以 `stop_sequences` 发给上游（Anthropic 的叫法，发 `stop` 会被静默忽略）；`reasoning_effort` 在原生路径上不发，与模型级设置一致。
- 新增 `mount_app(host, "/amkr", config, config_path)`：把 AMKR 挂进已有的 FastAPI/Starlette 服务，作为子路径提供整套 OpenAI 兼容接口与管理 API，不必单独起进程。它会额外处理一件容易被忽略的事 —— Starlette **不会**为 `Mount` 的子应用运行 lifespan，直接 `host.mount()` 会静默跳过 AMKR 的启动与收尾（指标广播任务不启动、退出时 httpx client 与 SQLite 连接不关闭）；`mount_app` 把子应用 lifespan 链进宿主，同时保留宿主自己的 lifespan。详见 `docs/API.md`。
- `create_app` 新增 `enable_ops`（默认 `None` 表示跟随配置字段 `ops_enabled`，即 `True`，保持 CLI 行为不变）。运维接口（`/api/logs`、`/api/service/*`、`/api/integrations/*`、`/api/tool`）作用于「服务所在的这台机器」，嵌入到别人的进程里语义不成立，`mount_app` 默认将其关闭。
- 新增 `--no-ops` 与配置字段 `ops_enabled`（默认 `true`），用于**整体**关闭运维接口：容器与反向代理后面，这些接口会读日志文件、启停本机进程、注册系统服务、改写本机 Claude Code / Codex 配置，语义不成立；逐个路径拉黑容易漏（漏一条就等于宿主机被接管），整体关掉才是可断言的做法。关闭后这些路径返回 `404`，`/health` 新增 `ops_enabled` 字段供部署校验。与 `--webui` 一样写入配置文件 —— 后台启动与系统服务的子进程只带 `--config`，开关不落盘就会静默失效。
- `auto_model_key_router` 顶层导出 `create_app` / `mount_app` / `RouterConfig` / `KeyPool`，并附带 `py.typed` 标记。
- 新增容器镜像，发布流程（`.github/workflows/release.yml`）在打完 wheel 后构建镜像并推送到 GHCR（`ghcr.io/sparrived/auto-model-key-router`，标签为版本号，仅正式版额外更新 `latest`）；推送前先在容器里起一次服务、请求 `/health` 校验状态码与版本号 —— 本机不装 Docker 也能发布，但「镜像起不来」必须在发布时而非用户拉取时发现。
  - 镜像步骤排在 `gh release create` **之前**：tag 是那一步才创建的，推送失败时 tag 与 release 都不存在，重跑不会被 `existing_release` 的跳过条件挡住。
  - 配置、指标库、日志与 PID 文件都经 `XDG_CACHE_HOME` 落在 `/data`（已声明为卷），删容器不丢配置；构建上下文由 `.dockerignore` 排除 `router-config.json`、`.env`、`*.sqlite3`、`*.log`，避免把真实上游 Key 打进镜像推到 registry。
  - 容器内以非 root 用户运行，并固定用 `--host 0.0.0.0` 覆盖默认的 `127.0.0.1` —— 不覆盖的话 `-p 8000:8000` 映射不进容器。
  - 构建时用 `--label org.opencontainers.image.source` 把包关联到仓库（值由 `GITHUB_REPOSITORY` 推出，不在 Dockerfile 里写死，换仓库或改名时无需改动）：GHCR 页面会带上仓库信息，也避免「命名空间下已有同名包但未关联仓库」时 `GITHUB_TOKEN` 无权推送。**可见性不随仓库继承** —— GHCR 容器包默认私有，本仓库虽是公开仓库也不代表镜像能匿名拉取，而且没有 API 能改，只能在包页面手动改一次（README 已写明步骤）。
- `create_app` / `mount_app` 新增 `authenticator` 参数（**可插拔鉴权**）：嵌入宿主时，宿主通常已有自己的身份体系（session cookie、JWT、网关身份头），而此前的选择只有两个 —— 把 `local_api_key` 留空等于**整体关闭鉴权**，否则就得让调用方额外再持有一套 AMKR 的 key。钩子为 `async (request, config) -> AuthContext | None`，返回 `None` 即拒绝（401）。`AuthContext.mode` 沿用 `"full"` / `"visitor"` 词表：visitor 不是「权限更小的 full」，而是一套**模型级**规则（只能用 `allow_visitor` 的 Key 与 `amkr-{模型ID}`，不能用内部别名/真实模型 ID/`unified-model`，拿不到 `/metrics` 与管理接口），因此必须显式选择而不能用布尔值表达。钩子同时覆盖 WebSocket：`/ws/events` 的首帧 token 会被折算成 `Authorization` 头交给同一个钩子（宿主的 cookie 本就在握手头里，用 session 鉴权时握手即通过）。不传该参数时行为完全不变。
- WebUI 概览新增**用量热力图**（星期 × 小时矩阵，紧贴 KPI 瓦片下方整行铺开）：按「请求数量 / Token 数量」切换，一眼看出周内节律（工作日 vs 周末、白天 vs 夜间）与峰值时段。数据固定取最近 7 天、1 小时一格（不跟随页头的 1h–7d 窗口，否则切到 1h 就失去周内可比性），并给出每格读数与峰值。三种状态刻意区分：**斜纹格**是窗口未覆盖（没数据）、**最浅格**是这一小时确实是 0（没流量）、**描边虚框**是最新那个还在累加的整点桶（天然偏低，不能被读成流量骤降）。落格按 `Asia/Shanghai` 取星期与小时，浏览器时区不同也不会让矩阵平移。热力图走独立请求与 60 秒 TTL，不跟着 10 秒轮询重算 169 个桶。

### Changed

- 流式超时默认值由 `stream_first_byte_timeout=90` / `stream_idle_timeout=180` 下调为 **60 / 60**：单 Key 模型下 `max_retries=2` 意味着最坏要串行等 3 次首字节，按 90 秒计约 270 秒才返回失败，而实测上游健康请求的首字节 p95 已达 64 秒、p99 达 82.5 秒 —— 等待窗口长到既拖慢失败反馈、又让请求堆积。下调到 60 秒后失败能在约 3 分钟内给出（3 × 60），代价是个别本就慢于 60 秒的成功请求会被判超时，可按上游实际情况在 TUI 的 **CLI 设置 → 超时配置** 中调回。
- 依赖补上主版本上界（`fastapi`、`httpx`、`rich`、`tomlkit`、`uvicorn`、`websockets`）：作为别人的依赖项时，这些库的 minor 升级会改行为，不设上限迟早会把宿主一起弄坏。
- WebUI 前端的 API 基址改为按当前页面路径推导，不再写死绝对路径。此前页面能挂在 `/ui/` 只是因为独立运行时恰好同源同前缀，挂到 `/amkr/ui/` 后每个请求都会打到宿主根路径。
- WebUI 看板卡片改为**同排等高铺满**，消除卡片之间的大片空隙：栅格原先用 `align-items: start`，行高由该行最高的卡片决定，矮卡片只占自身那点高度、下方留出一整块空白（例如「响应状态分布」是空态、约 200px，与约 390px 的「请求结果」同排时，下方空出近 200px）。现在列容器撑满行高、卡片再撑满列容器，排内间距恒为间隙值、整排底边天然平齐；卡片内部的富余高度也有处可去 —— 列表/请求流/表格滚动区吸收，竖向内容块居中，页脚用 `margin-top: auto` 钉到底部。
- 概览与活动页的多段栅格合并为一个：分开写时排内间距是 16px、排间却是 `.content` 的 24px，横纵节奏对不上、整体显得松散；`.content` 的间距也改为与栅格同值，页面级纵向节奏与卡片横向间距一致。
- 请求流限高 520px 并内部滚动：它条数随流量增长，不限高会成为整排的高度上限，把旁边的「延迟趋势」一起撑到近千像素、两张图被挤到底部（这也是"间隙突然变大"的实际来源）。
- 卡片内空态不再自带背景与海拔：卡片本身已经是那个「面」，里面再套一层白底描边会像卡片里又嵌了一张卡片。

### Fixed

- 修复 `GET /metrics` 省略 `hours` 时的**无界全表聚合**：默认会聚合全部历史，在本机 16 万行 / 65 MiB 的统计库上实测 3.1–5.2 秒，而 `snapshot()` 与 `record()` 共用 `MetricsStore._lock`，于是这个只读接口会把代理请求路径（每次落库）一起卡住 —— 实测无界查询进行中，一次 `record()` 要等 4.1 秒才拿到锁。现在默认窗口改为最近 `24` 小时（与 `/metrics/requests` 一致），全量改由显式的 `all_history=true` 触发。
- 删除 `proxy_handler._broadcast_metrics()`：它从 `RuntimeResources` 上 `getattr(state, "event_bus", None)`，而 `event_bus` 只挂在 `app.state` 上，该函数因此恒为空操作；真正的节流广播在 `app.py`。留着它是个隐患 —— 一旦有人把 `event_bus` 挂到 `RuntimeResources`，每次上游失败都会在请求路径上同步跑一次无界 `snapshot()`，也就是一次 3–5 秒的全表扫描。
- 修复 `EventBus.broadcast("client_count", ...)` 漏掉 `await`：`/ws/events` 的客户端数量事件从未真正发出（只在日志里留下 `RuntimeWarning: coroutine ... was never awaited`）。
- 修复服务退出时的 sqlite **原生崩溃**（`Windows fatal exception: access violation`，进程直接挂掉）。`MetricsStore` 用 `asyncio.Lock` 串行化连接访问，再用 `asyncio.to_thread` 执行查询；但 `to_thread` 无法取消 —— 任务被 cancel 时 await 立刻抛出、锁随之释放，**工作线程仍在用同一个连接执行 SQL**，随后 `close()` 拿到刚释放的锁并关闭连接，正在查询的线程就踩到已失效的 sqlite 句柄。现在互斥下沉到工作线程（`threading.Lock`）：取消只能中断 await、中断不了线程，而 `close()` 同样要抢这把锁，于是自然排在所有在途查询之后；取消路径也会先等线程收尾再抛出。这个缺陷此前被上面那个漏掉的 `await` 掩盖着（时序恰好错开），修好 `await` 后立即暴露。
- 修复 WebUI 图表读数气泡里多出一行 `null`：原生 `replaceChildren` 会把 `null` 子项字符串化成文本节点 `"null"`，与 `dom.js` 里会过滤空值的 `append()` 行为不同，于是每个**已完结**的点都在读数下多显示一行 `null`（只有「累加中」的尾桶才看不到）。折线图与 Token 堆叠柱两处气泡、以及统一模型编辑表单（路由方式非「固定 Key」时）都改用会过滤空值的 `mount()`。同时补上 `tests/webui_tip_probe.mjs`：用忠实还原 `replaceChildren` 语义的 DOM 垫片驱动真实的 `charts.js`，此前的垫片一律复用会过滤空值的 `append()`，恰好把这个 bug 藏了过去。
- 修复发布只更新 `pyproject.toml` 而不更新 `uv.lock`：uv 把根项目也写进 `uv.lock`，于是打出的 tag 上 `pyproject.toml` 是 4.1.0、`uv.lock` 仍写着 4.0.3，`uv sync --locked` 会直接报 lock 过期；且此后任何 `uv run` 都会把它改回去，工作区永远脏一块。现在改版本号时同步 `uv.lock` 中根项目的 `version`（只改根项目那一处，不碰依赖），并且不调用 `uv lock` —— 只有根项目版本变化、依赖解析结果不变，联网跑 lock 反而可能因索引不可达而失败。
- 修复携带**非 ASCII 凭据**的请求返回 500 而不是 401：HTTP 头是字节、由 Starlette 按 latin-1 解码，因此构造一个非 ASCII 的 `Authorization` 就能把字符串送到 `hmac.compare_digest` —— 它对含非 ASCII 的 `str` 直接抛 `TypeError`（实测可复现）。现在一律比较 UTF-8 字节，非 ASCII 凭据自然地判为不匹配。原先该缺陷被 `_authorization_mode` 的 `==` 掩盖（`==` 不会抛异常），改用 `compare_digest` 时暴露。
- 修复推送失败的处理只认「错误文本里出现 `proxy` / `127.0.0.1`」：`schannel: failed to receive handshake, SSL/TLS connection failed` 这句话两者都不含，于是既不绕过代理、也不重试，一次瞬时网络抖动就把整个发布卡在最后一步，而提交和标签已经建好，留下「已提交已打标签、但没推上去」的半成品状态。现在把连接类失败（代理、TLS 握手、连接被拒/重置、超时、域名解析失败）统一识别：先临时绕过代理试一次，仍失败则退避重试，共 3 轮，并在最终报错里保留原始正文；鉴权被拒这类非连接错误仍不重试。

## [4.1.0] - 2026-09-16

### Changed

- WebUI 从「能看的配置页」重做定位为**监控看板 + 工作台**：新增「概览」与「实时活动」两个监控页，并在应用栏加入实时读数（服务状态、60 秒窗口 RPM/TPM、进行中请求数）。界面仍遵循 `ui.md` 的材料设计 token 与交互物理，但不再沿用它的落地页骨架（Hero、三等分卡片、页脚），改为 12 列栅格 + 卡片构成的密集看板布局；窄屏逐级塌缩为单列。
  - 「概览」：8 张 KPI 瓦片（当前 RPM/TPM、窗口请求、成功率、Token 用量、缓存命中率、平均耗时、平均首字，带迷你趋势与环比）、主流量折线图（可切请求速率/Token 速率/Token 用量/耗时/成功率）、统一模型入口、Token 构成环形图、响应状态分布、请求结果、模型/调用方/上游排行、Token 构成随时间堆叠柱、运行状态。
  - 「实时活动」：实时速率、延迟趋势（耗时与首字两段各自成图，量级差一个数量级时不互相压平）、逐条请求流（真实请求记录，可按成功/失败/重试过滤）、模型与模型/Key 维度的用量分解表（可排序）、上游分布与服务日志。
  - 统计窗口可选 1h/6h/24h/3d/7d，桶宽自动选取（保证不超过后端 500 点上限）。
- 前端引入 `chart-math.js`（纯函数，无 DOM 依赖）承载全部读数口径，使图表数字可以被单元测试锁住：桶内计数一律按 `bucket_seconds` 归一化成「每分钟」（不再假设桶宽是 60 秒）；比率与均值用分子分母分别求和再相除（不是对每桶比率取平均）；`0` 视为真实读数（空闲）而只有 `null` 视为缺口；未完成的尾桶标记 `partial` 并用虚线绘制，避免被读成流量骤降；百分比轴固定 0–100；分母为 0 显示 `-` 而非 `0%`；所有时间戳固定按 `Asia/Shanghai` 渲染，与后端统计口径一致。
- 新增 `icons.js`（内联 SVG 图标）替换此前混用的 `◎ ≣ ⛁` 等字符：这些字符在不同系统上字形差异极大，且无法随文字颜色继承。卡片、表格、徽标、表单控件统一为同一套类词汇（`stat-grid`、`segmented`、`table`、`notice`、`skeleton`、`empty`、`page-head` 等），并提供加载骨架、空态、Toast、对话框与 `prefers-reduced-motion` 兜底。
- 指标轮询按页面分层（监控页 10s/5s，配置页 30s）且不再整块重建页面 DOM：重建会丢掉展开的详情、输入框焦点与滚动位置，对供应商/设置这类工作台页面尤其致命；改由 `onTick` 通知当前页自行重绘。

### Fixed

- 修复发布脚本针对 Windows 文件占用的重试从未生效：`run_command` 靠 `result.stdout/stderr` 里的 `[WinError 5]` 判断是否该重试，但只有 `capture=True` 时这两项才有值，而真正会撞上该错误的两步（`pip install -e .`、`python -m build`）用的都是默认的 `capture=False` —— 于是 stdout 恒为 `None`，标记永远匹配不到，首次失败就直接放弃，`transient_retries=3` 形同虚设，发布卡在「构建分发产物」并留下「版本号已改、但未构建未提交未打标签」的半成品状态（4.0.3 记录的那次修复因此实际从未生效）。现 `capture=False` 且有重试次数时改走 teed 执行：把 stderr 并入 stdout 逐行读取，一边实时回显一边留存输出，标记检测与重试判定都能正常工作；并设置 `PYTHONUNBUFFERED=1` 保持输出顺序（否则 pip 的 stdout 块缓冲、stderr 无缓冲，错误会跑到逻辑上在它之前的那几行前面）。三个既有重试测试全都传了 `capture=True`，恰好只覆盖了唯一能工作的配置，故补上 `capture=False` 路径的回归测试。
- 修复发布脚本构建/可编辑安装前不清理旧 `*.egg-info`：setuptools 用 `NamedTemporaryFile` + `os.replace` 写 `PKG-INFO`，**目标文件已存在**且被杀软或索引器扫到时会抛 `WinError 5`。现在构建前先删掉 `*.egg-info`（构建产物，随时重建），目标不存在时 `os.replace` 只做创建，从源头消除该竞态，而不是仅依赖失败后重试。
- 修复 WebUI 图表的 X 轴时间标签全部渲染为 `-`：`timeTicks()` 只读取原始数据点的 `started_at`，而 `lineChart` 传入的是 `series()` 的产物（时间字段名为 `at`），导致整条时间轴丢失。
- 修复 WebUI 整页白屏：`svg()` 为 SVG 元素赋值 `className` 会抛 `TypeError`（`SVGElement.className` 是只读的 `SVGAnimatedString`，ES 模块处于严格模式），异常在渲染首屏时中断，页面只剩空壳。现 SVG 一律走 `setAttribute`。
- 修复 SVG 轴标签字体未生效：`font-family="var(--font)"` 作为 SVG 表现属性不会解析 CSS 变量，会被当成字面字族名而静默失效；改为由 CSS 统一设置。
- 修复 WebUI 模型路由页「添加目标」时已绑定的 Key 仍出现在候选列表里：模板字符串里的 `${target.key}` 被转义成字面量，去重集合的键与候选键永远不相等，导致重复项可被反复添加。
- 修复 KPI 环比颜色语义：吞吐量类指标（窗口请求）的涨跌本身不分好坏，原先一律「涨=红」会把一次正常高峰标成告警。现由 `trendPolarity` 明确区分「越高越好」「越低越好」「中性」。
- 修复看板同屏数字口径不一致：概览与活动页的 KPI、Token 构成、请求结果、分解表原先一律读全局的「最近 1 小时」快照，而折线图跟随用户选择的统计窗口（可选 1h–7d）。把窗口切到 7 天时，同一屏会出现 7 天的曲线配 1 小时的总量；即使不切窗口，同一屏内也有两个来源的「1 小时总量」并排显示（快照与序列求和），小数位对不上会被当成读数不准。现在两个页面的窗口相关数字统一取自同一次请求取回的窗口快照，序列只负责画曲线与时序图。
- 修复 uv tool 安装方式的自动更新永远失败：`uv tool install "auto-model-key-router==<版本>"` 会把版本锁写进 `uv-receipt.toml`，此后 `uv tool upgrade` 认为当前环境已满足该锁，只更新依赖、不动本体，并**以退出码 0 报「Nothing to upgrade」**。更新器因此把命令误判为成功，却在版本校验中发现仍是旧版本，重试 6 次后报「更新命令在 6 次尝试后仍失败」。现更新前读取 `uv-receipt.toml`：带版本锁时改用 `uv tool install --force`（保留 `[visitor]` 等 extras）重装以清掉锁，未锁版本时仍走 `uv tool upgrade`。
- 修复 WebUI 输入本地鉴权 Key 后仍停在「读取设置失败: AMKR 请求失败（HTTP 401）: 本地 API key 验证失败」：进入页面时只检查 localStorage 里「有没有 Key」就当作已授权，Key 失效（被重置或来自旧版本）时各页面仍带着错误 Key 加载，并把 401 当作业务错误缓存进模块级 `state`，此后每次重绘都重复显示同一条旧报错，且没有任何回到登录卡的入口。现在进入页面会先用一次真实请求校验 Key（只把 401 视为 Key 无效并清除，网络不通则保留 Key 并提示服务未运行），未授权时不加载页面模块；提交的 Key 会先校验成功才保存并整页重载，校验失败会就地提示且不写入 localStorage；会话中途 Key 失效（如重置本地鉴权 Key）时会回到验证页并说明原因。健康轮询不再重建输入框节点，避免清空已粘贴一半的 Key。
- 本地鉴权改为独立的整页验证页：未通过鉴权时不再渲染应用栏、导航与任何页面，整页只有验证页。此前验证卡是渲染在主界面内容区里的，「带着无效 Key 进入主界面」仍有入口——地址栏深链（如 `#/providers`）或浏览器前进/后退可直接落到内页。现在 `authorized` 只在确认服务可访问后置真，任何未通过的路径（含深链、切页、会话中途 401、服务连不上而无法判断是否需要鉴权）都整页回到验证页；连通性也归到这一页处理——连不上时保留已填的 Key 并给出「重试连接」，不再与「Key 无效」混为一谈。验证页页脚显示版本与入口地址，便于确认连的是哪个实例。

## [4.0.3] - 2026-09-15

### Added

- 模型隐藏别名：同一个模型可以在多个名字下调用，但只有本地模型 ID 和 `aliases` 会出现在 `/v1/models` 与 `/health` 中。两种来源：① 自动——每个 target 的 `upstream_model`（上游叫法）自动成为可直接调用的名字，无需逐个登记；② 手动——模型可配置 `hidden_aliases` 列表。隐藏名与真实 ID/别名冲突时以真实名优先；手写的隐藏别名参与重名校验（与模型 ID、`aliases` 及其他模型的隐藏别名冲突都会报错）。管理 API（`/api/models`、`/api/routes`）新增 `hidden_aliases` 字段并在模型响应中额外返回 `auto_hidden_aliases`（自动推导的名字，便于排查）；TUI 模型设置新增「隐藏别名」菜单项，WebUI 模型路由页新增隐藏别名输入框。
- 可选启用的内置 WebUI：资产随软件包一起发布（无构建步骤、无额外依赖），通过配置字段 `webui_enabled` 或 `--webui` / `--no-webui` 启用，也可在 TUI「CLI 设置 → WebUI」中切换；启用后访问 `http://<host>:<port>/ui/`。未启用或资产缺失时 `/ui` 返回 `404`，不影响其他接口。
- 运维 API（均需本地鉴权）：`GET /api/logs`（读取日志尾部）、`GET /api/tool` 与 `POST /api/tool/webui`（版本检查与 WebUI 开关）、`POST /api/service/{action}`（服务启停与自启注册）、`GET /api/integrations`、`POST /api/integrations/{agent}` 与 `POST /api/integrations/{agent}/rollback`（Claude Code / Codex / Pi Agent 接管与回退）。`/health` 新增 `webui_available`、`webui_enabled`、`webui_mounted`、`webui_path` 四个字段。
- WebUI 是预构建的静态资产（原生 ES 模块 + 手写 Material Design 样式），随 wheel 通过 `package-data` 发布，运行时不需要 Node.js。

### Fixed

- 修复发布脚本在「准备发布环境」一步偶发失败：`pip install -e .` / `python -m build` 走到 setuptools 写 `egg-info/PKG-INFO` 时，`NamedTemporaryFile` + `os.replace` 可能被 Windows 上短暂占用的句柄或杀软扫描拒绝，抛 `PermissionError: [WinError 5]` 并以 `subprocess-exited-with-error` 中止发布（与 Agent 配置写入那条同属 Windows 文件占用噪声，区别是这次发生在 pip / build 子进程内部，脚本自身无法重试那次 `os.replace`）。现由发布脚本检测该错误码后小退避重试整条命令（`[WinError 5]` / `[WinError 32]`，用带方括号的 ASCII 片段匹配以适配本地化错误正文，并避免误命中 `WinError 50` 等其它码）。
- 修复发布脚本在中文 Windows 控制台（GBK）下中断：Rich 打印 `✓` / `ℹ` / `⚠` 会抛 `UnicodeEncodeError`，而此时版本号已写入、提交与标签尚未创建，会留下「已改版本但未发布」的半成品状态。现放开发布脚本输出流的编码错误处理（仅 `errors="replace"`，不改变终端实际编码）。
- 修复 Agent 配置写入（Claude Code / Codex / Pi Agent）在 Windows 上偶发 `PermissionError: [WinError 5]` 失败：`os.replace` 可能被杀软扫描或未释放的句柄短暂拒绝，现与配置写入一致地做小退避重试。
- 修复源码树内运行时报出「假版本号」：`__version__` 原先优先读取 `parents[1]/pyproject.toml`，会把发布中断遗留的「已改版本但未发布」状态当成真实版本（并在升级检查中误判为已是最新）。现在已安装的包一律以安装元数据为准，仅未安装（直接从源码运行）时才回退读取 `pyproject.toml`。

## [4.0.2] - 2026-09-05

### Added

- 管理 API 新增按 Key 设置服务模型集：`GET/PUT /api/providers/{provider_id}/keys/{key_name}/models`。PUT 原子同步该 Key 在所有模型上的绑定：按需创建模型并绑定（upstream_model = 模型 ID），取消绑定的模型会移除引用该 Key 的全部 target，空模型级联删除。桌面端可据此复用旧的「勾选模型卡片」交互。

## [4.0.1] - 2026-09-05

### Fixed

- 修复 v3 配置迁移：存在但没有 Key 的空模型池不应被误判为不存在，现会按 v3 白名单语义过滤对应目标并正常完成 v4 迁移。

## [4.0.0] - 2026-09-05

### v4（config_version=4）

- 彻底移除「模型池 (Pool)」抽象：Key 归 `providers.<id>.keys` 管理，模型 `targets[]` 改为 `{provider, key, upstream_model}` 直接绑定供应商 Key（同一 Key 可被多个模型引用，一个模型可绑定多个 Key）。
- 磁盘格式升级到 v4，`unified_model` 改为嵌套结构（`default` / 可选 `image`，各含 `primary` 与可选 `fallback`）；v3/v2/v1 配置在加载时自动、幂等迁移到 v4 并写回；v3 池级探测元数据与旧 v4 供应商级 `capabilities` 会折进该供应商每个 Key 的 `capabilities`（迁移期保守共享同一份探测快照，随后逐 Key 刷新会各自更新）。
- 探测改为每个 Key 独立进行并缓存：添加 Key（无论是否该供应商第一个 Key）都会只探测这个新 Key（`GET /v1/models` 模型清单 + 对 openai/anthropic/responses 路由模式各做一次最小请求），结果写入 `providers.<id>.keys.<key>.capabilities`，同一供应商的不同 Key 可见模型可能不同，探测结果互不复用；手动刷新可在 TUI「供应商 → 刷新能力探测」选择全部 Key 或指定 Key（指定 Key 还可限定端点范围），或调用 `POST /api/providers/{provider_id}/probe` 刷新全部启用 Key。
- 管理 API：删除 pools 系列端点（`/api/providers/{id}/pools/*`）与 `/api/probes/pools`；新增同步的 `POST /api/providers/{provider_id}/probe` 与单 Key 的 `POST /api/providers/{provider_id}/keys/{key_name}/probe`（请求体可带 `modes` 限定路由检查）；provider 响应不再含顶层 `capabilities`，探测缓存移至其 `keys[]` 各项的 `capabilities`（可为 `null`），单 Key probe 响应额外返回 `key` 对象。
- 删除语义：删除 Key 只清理引用它的模型绑定（模型无 target 自动删除；供应商无 Key 自动删除）。
- TUI 改版：供应商菜单（添加 Key / 管理 Key / 刷新能力探测 / Base URL 与路由 / 删除供应商）与模型设置菜单（别名 / 路由模式 / 管理 Key / 绑定 Key / 删除模型）取代「模型池」入口。
- 统计：v4 新调用不再写入模型池归因（`pool_name` 为空），历史池归因仅作为 v3 及更早数据保留。

### Fixed
- v3 → v4 迁移对池白名单的语义与 v3 解析器保持一致：`pool.models` 键存在（含空数组）即按白名单过滤 target 引用，避免 v3 中被静默丢弃的死引用在升级后意外复活（线上 v3 配置实测逐模型等价）。

## [3.3.0] - 2026-08-30

### Added
- align management mutations with TUI

### Changed
- use shared config operations
- centralize mutation operations

## [3.2.9] - 2026-08-21

### Fixed
- link to version-specific PyPI release

## [3.2.8] - 2026-08-20

### Added
- add command to print local authorization key

## [3.2.7] - 2026-08-20

### Changed
- 补充 pipx 与 uv tool 安装后的 PATH 配置、命令定位和临时验证说明，明确“找不到 amkr”通常是终端未刷新或工具 bin 目录未加入 PATH。
- 增加打包元数据回归测试，确保 `amkr` 与 `auto-model-key-router` 两个 console script 始终指向同一个入口。

## [3.2.6] - 2026-08-20

### Added
- 新增 `--show-address` CLI 指令，用于查询 AMKR 的监听 IP、端口和服务地址。
- 推理强度设置新增 `max` 选项，并支持通过配置、管理 API 和 TUI 传递。

## [3.2.5] - 2026-08-09

### Changed
- 支持 Pi Agent unified-model 一键配置，并将模型上下文上限设置为 256k。

## [3.2.4] - 2026-07-19

### Fixed
- disable upstream response compression

## [3.2.3] - 2026-07-19

### Fixed
- 同步模型池与模型路由

## [3.2.2] - 2026-07-18

### Fixed
- 明确模型池 Key 归属错误提示
- 修复模型名模型池迁移冲突

## [3.2.1] - 2026-07-16

### Fixed
- 调整非成功响应日志等级
- 保留 v3 供应商模型池

## [3.2.0] - 2026-07-14

### Added
- 提供持久化统计时间序列和调用明细 API
- 持久化供应商、模型池和上游模型统计归因

### Changed
- 为统计响应补充明确时间窗口并严格校验查询参数
- 内部端点回退和工具过滤重试按实际上游调用分别记账

## [3.1.1] - 2026-07-11

### Added
- 交互修复重复模型池归属
- 添加 Key 时指定唯一模型池
- 保留模型池启用状态并准确回显
- 支持多选项初始勾选状态
- 支持配置流式分段超时
- 在协议流中应用分段超时
- 限制流式首字节与空闲等待
- 添加流式分段超时配置

### Changed
- 更新模型池严格路由夹具
- 添加模型池路由实施计划
- 补充流式超时配置说明
- 补充模型池归属与选择交互
- 明确模型池模型约束路由设计
- 添加流式分段超时实施计划
- 添加流式分段超时设计
- 修正 Key 冷却状态说明

### Fixed
- 完善模型池唯一归属约束
- 按模型池启用模型筛选 Key

## [3.1.0] - 2026-07-11

### Changed
- 更新 Key 内部状态与端点缓存说明
- 内收 Key 健康状态并简化运行时资源

### Fixed
- 仅更新模型调用相关配置

## [3.0.4] - 2026-07-10

### Changed
- y

## [3.0.3] - 2026-07-04

### Fixed
- 校验Windows更新后的版本
- 同步模型池路由并管理Key运行态

## [3.0.2] - 2026-07-04

### Added
- 优化配置项选择交互

### Changed
- 移除旧配置运行时兼容迁移
- 收敛模型配置职责展示

### Fixed
- 优化原生端点探测缓存与旧配置迁移
- 未安装访客扩展时隐藏访客内容

## [3.0.1] - 2026-07-04

### Fixed
- 统一返回语义并保留添加草稿
- 完善模型池启用与删除清理
- 捕获子模块异常并返回主页

## [3.0.0] - 2026-07-03

### Added
- 支持模型池探测与手动模型
- 支持模型池配置与迁移
- 重构供应商模型管理界面
- 支持供应商 Key 配置自动迁移

## [2.2.6] - 2026-07-03

### Changed
- Add key availability probes to TUI

### Fixed
- fix some problems

## [2.2.5.post1] - 2026-06-28

### Changed
- 简化Codex鉴权处理，移除现有令牌保留逻辑

## [2.2.5] - 2026-06-28

### Added
- 重构Codex配置，拆分鉴权到独立auth.json文件
- 完善usage提取逻辑并新增统一模型ID支持

## [2.2.4] - 2026-06-27

### Added
- 完善 Codex 配置支持
- 添加OpenAI图像生成支持

### Changed
- 调整指标快照逻辑，以服务启动时间为起始点

### Fixed
- 正确过滤工具适配中的非函数类型无效工具

## [2.2.3.post1] - 2026-06-21

### Added
- 新增工具错误自动重试，过滤非function工具

## [2.2.3] - 2026-06-21

### Added
- 为Windows更新助手添加可配置的初始等待和重试基础时长
- 为metrics快照添加24小时时间范围参数

## [2.2.2.post3] - 2026-06-21

### Added
- 新增实时指标广播并优化工具适配逻辑

## [2.2.2.post2] - 2026-06-21

### Fixed
- 处理function字典缺失name的情况

## [2.2.2.post1] - 2026-06-21

### Added
- 新增活跃请求数统计并完善流式响应处理

## [2.2.2] - 2026-06-20

### Added
- 新增实时监控与WebSocket事件推送功能

### Fixed
- 为subprocess调用添加显式编码与错误处理

## [2.2.1] - 2026-06-19

### Added
- 新增 `GET/PUT/DELETE /api/unified-model` REST API 端点，支持通过 API 查询、设置和移除 unified-model 配置，`PUT` 支持按模型 ID 或别名指定目标模型及可选 key。

## [2.2.0] - 2026-06-19

### Added
- 新增单个 Key 统计页面，TUI 管理 Key 菜单中可查看指定 Key 的请求量、成功率、Token 用量、延迟等指标，支持时间范围切换和请求明细翻页。
- 新增 `GET /api/models/{model_id}/keys/{key_name}/stats` REST API 端点，返回指定 Key 的统计数据，支持 `hours` 参数过滤时间范围。

### Changed
- 移除缓存命中次数统计（`cache_hits`、`cache_misses`、`cache_hit_rate`），仅保留 token 维度的缓存统计（`cached_tokens`、`cached_token_rate`）；TUI 总览面板「缓存命中」改为「缓存 Tok 比例」。

## [2.1.6] - 2026-06-19

### Added
- `/metrics` 接口新增 `hours` 参数，支持获取指定时间段的监控指标。

### Changed
- Token 数量显示改用 K/M/B 缩写，优化大数值可读性。

## [2.1.5.post1] - 2026-06-19

### Changed
- 修复一些问题。

## [2.1.5] - 2026-06-19

### Added
- 首页新增运行统计面板，显示总请求数、成功率、总 Token、RPM 和 TPM 等运营指标。

### Changed
- 优化首页布局：运行概览与运行统计合并为紧凑两行显示，统一模型信息合并到概览面板，上游原生支持合并到模型路由表格。
- 请求明细表格列顺序调整，缓存列移至输入列后面。
- 请求总览输入 Token 改为显示总量（含缓存），移除总 Tok 行，合并 RPM 和 TPM 为一行。
- 未安装 visitor 时不显示 visitor 相关内容。

## [2.1.4] - 2026-06-19

### Changed
- 将上游路由管理页面的英文文本翻译为中文，统一界面语言。

### Fixed
- 修复 Anthropic 格式输入 token 统计为负数的问题，`prompt_tokens` 现正确包含缓存 token。

## [2.1.3] - 2026-06-19

### Changed
- 根据 `9b129a0`，将 `upstream_routes` 从单个 Key 级配置重构为按上游 `base_url` 分组的全局配置；旧版 Key 级配置仍会兼容读取并提升到对应上游 URL。

### Fixed
- 修复 `upstream_routes` 上游 URL 格式校验错误信息缺少具体无效 `base_url` 的问题，便于定位配置错误。

## [2.1.2] - 2026-06-19

### Added
- 新增上游路由自定义配置 `upstream_routes`，支持分别配置 Anthropic Messages、OpenAI Chat Completions 和 OpenAI Responses 的上游请求路径，并在管理 API、Terminal UI 与 Dashboard 中查看和维护。
- 新增请求缓存亲和路由，轮询 Key 模式可基于 `prompt_cache_key` 或请求内容哈希将同一缓存会话绑定到同一上游 Key，提升 prompt cache 命中稳定性。
- 新增 OpenAI Responses 原生接口探测与失败回退处理，支持按自定义路由缓存原生支持状态并在不支持时回退到兼容转发。

### Changed
- 上游路由配置会自动规范化并补全标准路径前缀；Key 的原生支持状态缓存改为按“上游 URL + 路由路径”维度存储，避免不同自定义路由状态互相污染。
- 优化令牌使用统计，兼容 Anthropic 缓存读取和缓存创建 token 的多种返回格式，并调整日志 TUI 统计表布局。

### Fixed
- 修复 Anthropic 请求转发头处理，改为保留客户端传入的 `anthropic-version`，并透传 `anthropic-beta`。
- 修复请求统计中输入 token 未扣除缓存 token 导致统计偏差的问题。

## [2.1.1] - 2026-06-17

### Added
- 新增 Anthropic 原生 `/v1/messages` 端点自动探测与回退功能，首次请求自动测试上游支持情况，不支持则自动回退到 `/v1/chat/completions` 格式。
- 新增模型配置项 `native_first`，控制是否启用原生优先模式，默认开启；支持持久化存储上游端点支持状态，减少重复探测开销。
- 保留 Anthropic 原生请求字段（如 `prompt_cache_key`、`cache_control`）转发至上游，提升缓存命中率。
- Terminal UI 模型管理新增 `O` 快捷键快速打开配置文件。

### Changed
- Claude Code 配置生成改为在 `env` 中自动添加 `CLAUDE_CODE_ATTRIBUTION_HEADER: false`，禁用 CCH 以避免第三方 API 缓存失效。
- 上游模型探测结果不再自动过滤已存在的模型，批量添加菜单新增跳过选项，避免误覆盖已有配置。
- 更新 API 与使用文档，补充原生优先模式的配置说明。

## [2.1.0] - 2026-06-17

### Added
- 新增上游模型自动探测功能，通过调用兼容 OpenAI 格式的 `/v1/models` 接口获取可用模型列表，支持批量多选添加探测到的新模型。
- 新增 TUI 多选菜单组件，支持带复选框的表格展示、完整的快捷键操作（空格切换选中、A 键全选/取消、上下/翻页导航等）。
- 新增近 1 分钟 RPM 和 TPM 实时统计功能，在 TUI 总览界面展示当前 RPM 和 TPM 数据，默认统计窗口为 60 秒。

### Changed
- Claude Code 配置生成自动添加 `anthropic_attribution_header: false`，禁用 CCH（Claude Code Attribution Header）以避免第三方 API 服务的缓存失效问题。

## [2.0.2] - 2026-06-15

### Added
- 新增模型与上游 key 的 REST 管理 API，支持增删改查、配置 `allow_visitor` 访客可用性、原子持久化和运行时热重载；查询结果仅返回 key 指纹，不暴露上游密钥明文。
- 新增 Key 连续失败自动禁用机制：同一上游 Key 连续 5 次请求失败后会自动标记为禁用并持久化状态，后续请求分发会排除已禁用 Key。
- 新增 Cloudflare 521 上游错误识别，将 521 纳入可重试状态码，并为 OpenAI/Anthropic 兼容错误响应返回结构化错误信息。
- 新增官方 CLI 使用文档、API 接口文档和完整使用指南，覆盖命令行参数、管理接口、安装配置、路由、访客访问、WebSocket、统计与维护流程。

### Changed
- 配置迁移的“粘贴并应用”改为追加模型 Key，不再覆盖目标端已有模型；重复 Key 会跳过，同名的新 Key 会自动生成唯一名称。
- Key 失败冷却时间会随连续失败次数放大，多 Key 路由会优先避开冷却或已禁用的 Key，提升上游故障时的自动切换能力。
- Terminal UI 的模型 Key 列表、管理、复制和排序界面会高亮展示允许访客访问的 Key，并统一访客访问状态展示。
- 官方文档迁移到 `docs/` 目录，README 改为项目概览与文档入口，避免在首页重复维护完整使用说明。

## [2.0.0.post1] - 2026-06-14

### Fixed
- 修复 visitor `/v1/models` 返回 `amkr-{真实模型ID}` 后，代理请求无法将该公共 ID 映射回真实模型而返回 `404` 的问题；visitor 公共路由现在直接基于真实模型 ID 构建，不经过内部别名索引。

## [2.0.0] - 2026-06-14

### Changed
- `/v1/models` 现在要求提供本地或 visitor API key，并按该 Key 的访问权限返回实际可用模型；visitor 列表只包含有权限的 `amkr-` 原始模型 ID，不再暴露内部别名或支持调用 `unified-model`。
- 重构代理请求处理，将请求准备、Key 选择、重试策略、上游调用、流式响应生命周期和错误转换拆分为独立模块，降低 `app.py` 的职责和复杂度。
- 按 Anthropic Messages、OpenAI Responses 和通用请求转换拆分协议兼容层，同时保留原有 `protocol_compat.py` 兼容入口。
- 重构配置写入流程，统一执行校验和原子提交；将系统服务状态采集与 Terminal UI 渲染解耦。
- 将调用指标和 Key 状态持久化移出异步锁与事件循环，减少磁盘和 SQLite 操作对并发请求的阻塞。

### Fixed
- 修复配置热重载期间旧 HTTP 客户端、指标存储和 KeyPool 可能在进行中的请求结束前被关闭的问题；运行时资源现在按代际管理，并在最后一个使用者释放后关闭。
- 修复流式请求在重试、异常或客户端提前断开时可能未统一释放上游响应和所占用 Key 的问题。

## [1.7.0] - 2026-06-14

### Added
- 新增 `/v1/{path}` WebSocket 入口，支持 Trae 等客户端通过 WebSocket 提交 OpenAI-compatible 请求；复用现有鉴权、模型与 Key 路由、失败重试、协议转换及调用统计，并支持流式 SSE 事件和非流式 JSON 响应。
- 增加 `websockets` 运行时依赖，确保 Uvicorn 可以处理 WebSocket 协议升级。

### Changed
- 重构 FastAPI 应用模块，将 Anthropic Messages、OpenAI Responses 请求/响应及 SSE 事件转换迁移到 `protocol_compat.py`，将 WebSocket 握手和帧适配迁移到 `websocket_proxy.py`，精简 `app.py` 并保持原有代理行为不变。

## [1.6.1.post2] - 2026-06-14

### Added
- Terminal UI 的“模型 Key”中新增“模型别称”管理，可查看并添加、编辑、删除模型别称。

## [1.6.1.post1] - 2026-06-14

### Fixed
- 修复跨机器配置迁移时“粘贴并应用”读取运行端系统剪贴板、无法获取本机复制内容的问题；现在导出单行 JSON，并在目标终端中手动粘贴后解析应用。

## [1.6.1] - 2026-06-14

### Added
- 调用统计新增 `local`（本地鉴权）与 `visitor`（访客鉴权）来源分类；`/metrics` 新增 `caller_types` 聚合结果，Terminal UI 调用日志新增“全部调用”“本地调用”和“访客调用”统计页面。旧版 SQLite 统计库会自动补充来源字段，已有记录按本地调用处理。

### Changed
- 配置迁移改为仅复制和应用模型 Key 配置，保留目标端的本地鉴权、监听地址、端口、超时、重试、文件路径及其他 CLI 设置；安装 `visitor` 扩展时会同时迁移各 Key 的访客访问权限，未安装时则忽略该权限。

## [1.6.0] - 2026-06-13

### Added
- 主页“一键配置”新增路由服务、Claude Code 和 Codex 子菜单；可增量写入 Agent 配置，使其通过本项目的 `unified-model` 路由，并缓存应用前的完整配置用于精确回退。
- 新增 Codex Responses 协议兼容，将 Responses 消息、function call、function output 和 tools 转换为 Chat Completions，并把普通及流式文本、工具调用和 usage 转回 Responses 风格。
- 新增 Claude Code `/v1/messages/count_tokens` 本地兼容响应，避免 OpenAI-compatible 上游不支持 Anthropic token 计数接口时中断。

### Fixed
- 修复 Windows 独立更新器将 `uv` 写入标准错误流的成功摘要误判为 `NativeCommandError`，导致升级实际完成却显示失败的问题；现在通过独立进程重定向输出，并以真实进程退出码判断更新结果。
- 修复 Windows 独立更新器接管后父进程已提前退出时，`Wait-Process` 抛出异常并在执行升级命令前中止的问题；现在仅在父进程仍存在时等待其退出。

## [1.5.0] - 2026-06-13

### Changed
- 重构 Terminal UI 为固定窗体式布局，主菜单、选项菜单、Key 排序、运行日志和调用统计统一在备用屏幕中重绘，不再通过追加输出展示交互内容。
- TUI 内容区域支持根据终端尺寸自动调整和滚动，长菜单会自动保持当前选中项可见，并可使用 PgUp/PgDn、Home/End 或 Windows 鼠标滚轮查看被折叠内容。
- 配置编辑中的文本和密码输入改为窗体内输入控件，避免连续操作时终端历史不断累积；终端窗口缩放后会自动重新计算布局。

### Fixed
- 修复终端高度或宽度不足时，TUI 内容被直接截断、选中项移出可视区域以及窄窗口横向超界的问题。

## [1.4.3] - 2026-06-13

### Fixed
- 修复 Linux/POSIX 下调用日志页的单键快捷键和方向键可能需要按 Enter 才生效，以及 raw 模式关闭终端输出处理后可能引发的 TUI 重绘异常；现在首页、选项菜单、Key 排序和日志页会在交互期间统一使用 cbreak 模式，并在退出时恢复终端设置。
- 修复 Windows 独立更新器直接调用更新命令时可能无法稳定记录退出码、错误输出和后续重试的问题；现在通过独立进程等待更新命令完成并读取实际退出码，同时保留标准输出和错误日志。

## [1.4.2] - 2026-06-13

### Fixed
- 修复 `/v1/chat/completions` 等非 Anthropic 转换路径直接按上游网络 chunk 转发 SSE，导致一个 chunk 内多个 `data:` 事件在客户端一次性显示的问题；现在所有 `text/event-stream` 响应都会按完整 SSE event 拆分并逐事件刷新。

## [1.4.1] - 2026-06-13

### Fixed
- 修复 `/v1/messages` 将 OpenAI 流式 `tool_calls` 缓存到消息结束后才转换为 Anthropic `tool_use`，导致 Claude Code 延迟显示工具调用的问题；现在会在首个工具 delta 到达时关闭文本块、立即开始工具块，并逐段转发 JSON 参数。
- 修复同一个上游网络块包含多个 SSE 事件时，下游可能合并发送连续事件、导致 Claude Code 长时间无输出后一次性显示整段内容的问题；现在会在每个转换后的 Anthropic SSE 事件之间主动让出执行权。

## [1.4.0] - 2026-06-13

### Added
- 新增固定虚拟模型 `unified-model`，可引用已有模型和可选 key；调用端无需修改请求模型名，即可通过 `--switch-model`、`--switch-key` 和 `--show-unified-model` 快速切换或查看当前路由。
- `unified_model` 配置变更支持原子写入和服务热加载，并可在 TUI 首页的“统一模型”中选择模型、自动路由或指定已启用 key，同时在 `/health`、`/v1/models` 和配置摘要中展示。
- 新增根路径 `HEAD /` 探活接口，返回 `204 No Content`，便于负载均衡器和托管平台执行轻量健康检查。

### Changed
- 优化 TUI 添加 Key 流程：可直接选择已有模型，并从当前模型、其他模型及默认配置中复用已有上游 URL，仍可按需新建模型或输入自定义 URL。

### Fixed
- 修复 Windows PowerShell 5.1 按本地代码页读取无 BOM UTF-8 更新脚本，导致包含中文提示的脚本可能解析失败、延后更新实际未执行的问题。
- 重构 Windows 自更新流程：不再依赖无确认的隐藏延迟脚本，改为由独立更新器窗口握手接管；更新器会等待文件锁释放、自动重试失败命令、持续写入日志，并在失败时保留窗口显示错误。

## [1.3.7] - 2026-06-13

### Fixed
- 修复 Claude Code 通过 `/v1/messages` 使用工具时，Anthropic `tools`、`tool_use`、`tool_result` 未转换为 OpenAI tool calling，且上游 `tool_calls` 未转换回 Anthropic `tool_use`，导致工具调用被当成文本一次性打印、实际文件未修改的问题。
- 修复 Linux/POSIX 终端下方向键无法用于菜单选择的问题，原因是 Python `BufferedReader` 预读了 ESC 序列的后续字节，导致 `select.select` 检查底层 fd 时超时，将方向键误判为 `ignore`；改为使用 `os.read(fd, 1)` 直接从文件描述符读取，绕过 Python 缓冲层。

### Changed
- Linux/POSIX 平台禁用鼠标滚轮支持，避免部分终端因鼠标模式与键盘输入冲突导致交互异常；相应移除 Linux 下 UI 中的滚轮操作提示。

## [1.3.6] - 2026-06-12

### Added
- 新增 MIT License 文件，并补充 PyPI 包元数据、项目链接、分类器和 README 许可证入口。
- 新增更新后服务重启和 Windows 延迟更新后置命令相关回归测试。

### Changed
- 优化手动更新流程，更新成功后会按当前运行状态自动重启后台/系统服务；从 TUI 发起更新时会退出当前界面，并在 Windows 延迟更新完成后自动重新打开 Terminal UI。
- 调整 Linux/POSIX TUI 返回提示，不再把单独 Esc 作为返回键，改为提示使用 Ctrl+C、q 或 0 等明确按键返回或退出。

## [1.3.5] - 2026-06-12

### Added
- 新增远程终端剪贴板复制支持，检测 SSH 等远程会话时优先通过 OSC 52 向终端发送复制请求，改善无本地图形剪贴板命令的环境体验。
- 新增远程终端剪贴板复制和 POSIX 不完整转义序列相关回归测试。

### Fixed
- 修复 Linux/POSIX 终端下滚轮、方向键、翻页键等 Esc 开头序列在慢终端或不完整输入时可能被误判为返回/退出的问题；Linux TUI 改为使用 Ctrl+C/q/0 等明确按键返回或退出。

## [1.3.4] - 2026-06-12

### Added
- 新增配置迁移 TUI 功能，可一键复制当前配置文件到剪贴板，并在另一个 TUI 中从剪贴板粘贴校验后应用。

### Fixed
- 修复 Windows 下从正在运行的 `amkr.exe` 内执行 `uv tool upgrade` 时，因入口文件被当前进程锁定导致更新失败的问题；现在会等待当前进程退出后继续执行更新。

## [1.3.3] - 2026-06-12

### Fixed

- 修复上游流处理异常时错误被重复抛出的问题，移除 `_stream_upstream` 和 `_stream_anthropic_messages` 中记录错误日志后多余的 `raise`。

## [1.3.3a1] - 2026-06-12

### Added
- 新增 `/v1/messages` 响应适配，将常见 OpenAI Chat Completions 文本响应转换为 Anthropic Messages 风格 JSON/SSE，提升 Claude Code 兼容性。
- 新增 Claude Code 兼容相关测试，覆盖非流式响应转换、流式 SSE 转换、非 JSON 错误包装和 Anthropic 请求头过滤。

### Changed
- 更新请求兼容说明，明确 `/v1/messages` 已支持 Anthropic Messages 风格响应转换，`/v1/responses` 仍为输入兼容。

### Fixed
- 修复 Claude Code 访问 `/v1/messages` 时因收到 OpenAI SSE、`data: [DONE]` 或非 JSON 上游错误页而触发 `API Error: Failed to parse JSON` 的问题。
- 修复转发上游时 `x-api-key`、`anthropic-version`、`anthropic-beta` 等 Anthropic/本地鉴权请求头污染 OpenAI-compatible 上游的问题。

## [1.3.2] - 2026-06-12

### Added
- 新增 POSIX 终端非阻塞字符读取辅助逻辑，并补充终端按键读取与 systemd 服务命令生成相关测试。

### Changed
- 优化 Linux systemd user service 启动命令，优先使用已安装的 `amkr` 控制台脚本，并通过 shell 安全拼接支持包含空格的路径。
- 重构 Terminal UI 按键读取流程，简化 POSIX 终端输入读取与解析逻辑。

### Fixed
- 修复 Terminal UI 对转义序列、鼠标事件和未知输入的处理，避免无效输入被误判为有效按键。

## [1.3.1] - 2026-06-11

### FIXED

- 修复 uv tool 默认安装目录未设置 `UV_TOOL_DIR` 时被误判为普通 pip 环境，导致手动更新调用缺失 pip 的工具环境失败的问题。
- 修复鼠标点击被作为Esc按键处理的问题。

## [1.3.0] - 2026-06-11

### Added
- 新增跨平台剪贴板复制模块，支持自动检测 Windows、macOS、Linux 可用复制命令。
- 新增 Terminal UI 结果页复制能力，可一键复制本地鉴权 key、模型 API key 等指定内容。
- 新增调用日志主菜单入口，便于从 Terminal UI 首页直接查看调用日志。
- 新增系统服务注册状态检测能力，并在自启动管理中展示服务注册与配置状态。
- 新增基于活跃请求数的 key 负载均衡调度，降低多 key 并发请求集中到同一 key 的概率。
- 新增剪贴板、Esc 按键、key 调度、更新命令与服务状态相关测试覆盖。

### Changed
- 优化 Windows 自启动计划任务设置，允许电池模式启动、不因切换电池停止、错过启动后尽快补启，并取消后台服务执行时限。
- 优化安装与更新说明，补充 pipx、uv tool 和 uvx 用法，并在手动更新时按 pipx/uv tool 环境选择对应更新命令。
- 优化手动更新命令生成逻辑，根据当前 pipx 或 uv tool 安装环境自动选择对应更新指令。
- 拆分系统自启管理菜单，优化服务管理交互流程。
- 优化 Terminal UI 主菜单、设置菜单和调用日志页面布局，并完善 Esc 退出提示。
- 优化配置交互流程，仅在新建模型时询问别名、路由模式等初始化配置项。

### Fixed
- 修复 Windows 开机自启动可能受计划任务默认电源策略或执行时限影响而未启动的问题。
- 修复 Windows 终端下 Esc 按键处理逻辑，单独按 Esc 可返回或取消，同时正确处理方向键与翻页键序列。
- 修复 Terminal UI 选项小写快捷键匹配问题。
- 修复 key 资源未正确释放导致负载统计不准确的问题。

## [1.2.4] - 2026-06-10

### Added
- 新增服务日志归档与历史日志列表，启动服务前自动归档非空旧日志，调用日志界面可切换查看历史日志并用默认文本编辑器打开日志文件。
- 新增 Windows 计划任务和 Linux systemd user service 状态详情展示，覆盖注册状态、启动状态、启动命令、原始状态与服务文件。
- 新增 Windows 当前用户登录自启入口，支持非管理员场景下注册 LIMITED 计划任务。

### Changed
- 重构 Terminal UI 菜单，将模型服务、本地鉴权、监听配置、调用日志和版本更新统一收敛到 CLI 设置。
- 优化一键配置与服务管理流程，自动注册系统服务、生成本地鉴权 key，并在结果页展示访问方式和服务地址。
- 优化调用日志界面，支持运行日志/调用统计分页、时间范围切换、日志级别与 HTTP 状态码高亮。

### Fixed
- 调整 Terminal UI 菜单结构、默认选中项与快捷键逻辑，并同步更新相关测试断言。

## [1.2.3rc3] - 2026-06-10

### Added
- 新增 `only_first` 路由模式，仅使用首个 key 并按 `max_retries` 对可重试错误进行重试。
- 新增通过 `模型ID[key name]` 或 `别名[key name]` 显式指定 key 的调用方式。
- 新增交互式维护者发布脚本，支持版本计算、CHANGELOG 归档、敏感文件检查、构建、上传与 GitHub Release 发布流程。
- 新增路由模式、显式 key、发布脚本、请求头过滤和超时策略相关测试。

### Changed
- 增强 Terminal UI 鼠标滚轮支持，并优化菜单、长内容视窗与调用日志滚动体验。
- 优化 README 配置、路由模式、显式指定 key、服务管理和维护者发布流程说明。
- 优化发布脚本对预览版本、稳定版、自定义版本、敏感文件和 Git 代理配置的处理。
- 流式请求超时策略调整为不限制读取阶段，避免长时间流式响应被读超时中断。

### Fixed
- 修复转发上游时 `destination-addr` 请求头导致部分上游拒绝的问题。
- 修复请求兼容转换、超时处理和 Terminal UI 布局相关问题。

## [1.2.2] - 2026-06-09

### Added
- 新增 Terminal UI 长内容滚动视窗，支持 PgUp/PgDn、Home/End 和鼠标滚轮翻阅。
- 新增调用日志鼠标滚轮滚动支持。
- 新增 Terminal UI 滚轮解析与内容滚动测试。

### Changed
- 优化 Terminal UI 标题展示与 README 使用说明。

## [1.2.1] - 2026-06-09

### Added
- 新增关闭 reasoning 选项，并将未设置状态展示为“由下游决定”。

### Changed
- 模型级推理强度配置在非“由下游决定”时会覆盖下游请求中的 reasoning 设置。
- 版本检查调整为优先查询 PyPI JSON API，失败时回退到 GitHub Release。

## [1.2.0] - 2026-06-09

### Added
- 新增 GitHub Release 版本检查、Terminal UI 更新提示和手动更新入口。
- 推理强度配置补充支持 `xhigh`。

## [1.1.1] - 2026-06-09

### Changed
- 优化模型推理强度查找逻辑，避免每次请求遍历模型配置。

### Fixed
- 修复 Linux 发布环境中 Terminal UI 顶层导入 Windows-only `msvcrt` 导致构建失败的问题。

## [1.1.0] - 2026-06-09

### Added
- 新增模型级推理强度配置，支持 `minimal`、`low`、`medium`、`high`、`xhigh`。
- 新增请求级推理强度透传与 Responses 风格 `reasoning.effort` 兼容转换。
- 新增 Terminal UI 推理强度设置入口，并在配置概览中展示推理强度。
- 新增 key 冷却状态持久化与上游健康探测恢复机制。
- 新增监听地址与端口的 Terminal UI 配置能力。
- 新增发布工作流 wheel 烟测与 PyPI 发布联动。
- 新增路由、key 冷却、健康探测和推理强度转发测试。

### Changed
- 多 key 请求失败时优先切换其他 key，单 key 模型才按重试次数重复尝试同一 key。
- 完善 Windows 时区依赖、测试依赖与打包文件查找配置。

### Fixed
- 修复命令行覆盖 host/port 时 RouterConfig 参数不完整的问题。
- 修复后台服务 PID 文件残留时无法重新启动的问题。
