# 工作空间

任务集合的隔离单位。本文说明**为什么这样设计**、边界在哪，以及改动时要守住哪些
不变量。使用方式见 [`USAGE.md` 9.1](USAGE.md#91-工作空间把任务集合分开)，接口细节
见 [`API.md` 的「工作空间」](API.md#工作空间)。

## 1. 解决什么问题

任务路由把「模型 + 一组固定采样参数」打包成一个可直接当 `model` 传的名字。原来的
问题出在**唯一性范围**：任务名全局唯一，于是多个人或多个团队共用一个配置时，每个人
都得为避让别人的名字而加前缀（`ALICE_SUMMARIZE`、`TEAM_A_SUMMARIZE`……）。前缀解决
的是命名冲突，却把一个团队的概念（「摘要任务」）污染成了带人员信息的字符串。

工作空间把唯一性的范围从「整份配置」缩到「一个空间」：

```
默认工作空间（顶层 tasks）        团队 A（workspaces.teamA）
  summarize  -> gpt-4o-mini        summarize  -> claude-sonnet-4
```

同名任务在两个空间里各指各的模型与参数，互不可见。

## 2. 三个已确认的设计决策

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 工作空间是什么 | 任务的命名空间容器：装一组任务，不同空间任务名可重复 | 它需要表达的是「谁的这一组」，而不是「另一套配置」 |
| 调用方怎么选 | HTTP 头 `X-AMKR-Workspace` | 见下节 |
| 兼容策略 | 保持 `config_version` 4，`workspaces` 为可选新增字段，顶层 `tasks` 视为默认空间 | 既有配置与既有调用方零改动 |

### 为什么用请求头

任务名是请求体 `model` 字段的**值**，因此只有三个候选位置：

- **模型名前缀**（`teamA/summarize`）：会让工作空间与真实模型名挤进同一个命名空间，
  于是还得处理「前缀」与「模型名」的冲突，等于把问题挪了个地方。
- **URL 路径**（`/v1/teamA/chat/completions`）：与参照实现固定的 `/v1/...` 形状冲突。
  代理路径的分类（`RequestRouteKind`）是按固定形状匹配的，动它会波及所有路由判断。
- **请求头**：唯一既不动请求体、也不动路径的位置。因此选它。

首尾空白会被裁掉；空串等价于默认工作空间。这个头**不会**转发给上游（见
`proxysupport.UpstreamHeaders`）：它是 AMKR 自己的路由状态，上游既看不懂也不该看到。

## 3. 边界：它只隔离任务

这是最容易误推的一点。工作空间**只**让任务名免于全局唯一，此外一律不变：

- 模型 ID、别名、隐藏别名与 `unified-model` 仍然**全局唯一**，任务名也不能与它们
  撞名。否则 `resolve_route` 的语义会取决于查表顺序——那是全局唯一的判断，工作空间
  不能用来遮蔽模型名。
- 供应商、Key、unified_model、设置项都**没有**工作空间维度。
- 因此工作空间不是「多租户」：它不隔离凭据、不隔离配额，任何人带上这个头就能用任
  何空间的任务。它只是任务集合的命名空间。

用量**可以**按工作空间分开看（见第 8 节），但那只是**观测**，不是**隔离**：归属被
记录下来用于统计与图表，却不用来拒绝任何请求。两者的区别很关键——把统计误当成配额
会得出「工作空间 A 用超了会影响 B」这种并不存在的结论。

## 4. 空工作空间不存在

工作空间由「在它里面建任务」**隐式产生**：

- 没有独立的「创建工作空间」接口。
- 删掉某个空间的最后一个任务，这个空间就消失了（`configops.writeWorkspaceTasks`
  删空即删分组）。
- 配置里手写的空分组也会被清掉（`config.RepairTasks` 同样会清理）。
- `RouterConfig.WorkspaceNames()` 因此由**任务反推**，而不是回放配置里的
  `workspaces` 键。

为什么不允许空分组：工作空间只是「一组任务」的容器，一个没有成员的分组既不可观测
（按名字取不到任何东西）也没有意义。允许它存在会立刻带来三处口径分叉——配置里能写、
删空后留不留、界面上列不列——而每一处都要单独决定。由任务反推后只剩一个口径。

**反过来说**，界面上的「新建工作空间」不能是一个独立按钮：新空间必须先被选中、再在
里面建第一个任务才真正落地。这就是 WebUI 把它挂在下拉项里、并在空态明说「建了第一个
任务才会写进配置」的原因。

> 手写的配置是唯一能短暂出现空分组的地方（例如 `"teamB": {}`），它会被解析接受、
> 但不会出现在工作空间清单里，并在下一次写回时消失。

### 改与删

「空分组不存在」这条决定了两个既有动作的形状（`internal/configops/tasks.go`）：

- **改名**（`RenameWorkspace`）就是**把整组任务搬到新键下**：旧键清空后由
  `writeWorkspaceTasks` 自动抹掉，因此不需要（也不能）单独「新建一个空间再搬」。
  先写新键再清旧键，顺序不能反——两条路径共用同一批任务对象，先清旧键会把要搬的
  内容一起丢掉。
- **删除**（`DeleteWorkspace`）就是**清空该组的任务**，分组随后自动消失。它因此
  没有独立的删除逻辑，也不会留下空壳。

两条动作的拒绝路径都由「默认空间不可动」与「空间由任务反推」推出：

| 情形 | 结果 | 理由 |
| --- | --- | --- |
| 改名/删除默认空间 | `400` | 默认空间是不带 `X-AMKR-Workspace` 头的调用方命中的那个，动它会让所有缺省调用方落空 |
| 改名/删除不存在的空间 | `404` | 空间由任务反推，没有任务就是不存在，没有可改可删的东西 |
| 改成另一个已存在的空间名 | `409` | **不合并**：两个空间各有一批任务时，合并会瞬间造出重名任务，而任务名在同一空间内唯一是配置层的硬校验 |
| 改成默认空间名 | `409` | 同「与既有空间重名」，默认空间只是那个必然存在的特例 |

## 5. 未知工作空间名不是错误

调用方写了一个没建过的空间名时，**不报错**：那里没有任务，于是任务查表落空，接着按
普通模型名继续解析，最终和「模型未配置」是同一个 `404`。

`NormalizeWorkspace` 因此只做去空白，**不校验存在性**。单加一条「空间不存在」的错误
既没有信息量（调用方真正需要知道的是任务或模型名不对），也会多一种需要维护的状态。

这条只适用于**调用方按请求头选空间**的读路径。管理面的改/删（`PUT`/`DELETE
/api/workspaces/{workspace}`）是另一回事：那两个动作有副作用，悄悄成功比报错危险得多，
因此对不存在的空间报 `404`（见上节）。

## 6. 兼容性契约

工作空间是 Go 侧新增的能力，参照实现没有对应实现，因此它**没有**可对拍的语料。它的
兼容性契约是另一条：**不影响既有配置与既有调用方**。

由此推出必须守住的性质：

1. **`config_version` 仍是 4**，`workspaces` 是可选新增字段，顶层 `tasks` 就是
   `DefaultWorkspace`。
2. **不带 `X-AMKR-Workspace` 头的请求行为逐字节不变。** 正因如此，被语料锁定的响应体
   （`tasks/list` 锁定了 `{"tasks":[{name,model,fallback_model,params}],"config_revision"}`）
   **没有、也不允许**新增 `workspace` 字段——当前空间由请求头决定，不体现在响应里。
3. **新增的 `/api` 路由与冻结清单分开维护。** 管理面 47 条与运维面 7 条路由的响应被
   逐字节语料锁定（`routePatterns()` / `opsRoutePatterns()`），那些语料是本项目**已
   发布接口**的回归凭证。工作空间自身的操作（`GET /api/workspaces`、
   `PUT|DELETE /api/workspaces/{workspace}`）是管理面的正式资源，因此注册在
   `/api` 之下，但列在**另一份**清单（`workspacePatterns()`）里：它们没有历史版本
   可对照，塞进那 47 条会让「这 47 条逐字节等于已发布行为」这句话失去意义。两批都
   注册在同一棵 mux 上，因此错方法的 `405` / `Allow` 判定必须同时看两份清单
   （`internal/api/server.go` 的 `patterns`）。

   反过来说，**其余新增能力**（价格目录 `/ui/pricing.json`、自更新入口、工作空间用量
   `/ui/workspace-usage.json`）仍然挂 `/ui/`：那些是本项目自有的读数，与被锁定的
   `/api` 面在语义上不连续，挂 `/ui/` 既落在冻结清单之外，也让「不开 WebUI 就没有
   这些读数」这件事顺理成章。判断标准是**它是不是管理面的正式资源**，而不是「能不
   能挂到 /ui 躲开语料」。
4. **访客 Key 不能使用任务**，带上该头也一样。
5. **错误文本**：工作空间引入的新错误（`workspaces 必须是对象`、`工作空间名不能为空`、
   `工作空间名重复: %s`、`工作空间 %s 必须是对象`、`workspaces.<空间>.tasks...`）没有
   Python 先例，是自由措辞；但既有的 `任务…` / `tasks.<名字>…` 文本被语料逐字节锁定，
   不得改写。
6. **`Validate` 的错误顺序也是契约**。新增检查请追加在末尾或紧邻同类检查，不要插到
   既有检查之前——那会改变既有非法配置报出的第一条错误。

## 7. 数据形状与关键实现

配置（`config` 包）：

```json
{
  "config_version": 4,
  "tasks": {"summarize": {"model": "gpt-4o-mini"}},
  "workspaces": {
    "teamA": {"tasks": {"summarize": {"model": "claude-sonnet-4"}}}
  }
}
```

解析后是**扁平**的一份 `[]TaskConfig`，每项带自己的 `Workspace`（顶层 `tasks` 的
任务归属 `default`）。扁平化让运行时查表退化成一次线性扫描或一次二元组查表，不必在
每个调用点先选分组。

各层的落点：

| 关注点 | 位置 |
| --- | --- |
| 常量与归一化（`DefaultWorkspace` / `WorkspaceHeader` / `NormalizeWorkspace`） | `internal/config/model.go` |
| 解析 `workspaces`、按 `(空间, 任务名)` 校验唯一性 | `internal/config/model.go` 的 `parseTasks` / `Validate` |
| 任务 CRUD（`*TaskIn` 系列）、`RepairTasks`、导入导出 | `internal/configops/tasks.go`、`transfer.go` |
| 运行时查表（`taskPlans` / `taskParams` 的键是 `[2]string`） | `internal/keypool/pool.go` |
| 读头、选空间（`RequestContext.Workspace`） | `internal/proxy/handler.go` |
| 阻止该头外泄 | `internal/proxysupport/support.go` |
| 管理端点感知空间（`X-AMKR-Workspace` 头） | `internal/api/handlers_meta.go` |
| 空间自身的读/改/删（`/api/workspaces*`、`workspacePatterns()`） | `internal/api/handlers_workspaces.go`、`internal/api/router.go` |
| 归属落库（`RecordParams.Workspace`、`request_workspace` 旁挂表） | `internal/metrics/store.go`、`internal/metrics/schema.go` |
| 归属贯穿（`MetricRecord.Workspace` ← `RequestContext.Workspace`） | `internal/proxy/retry.go`、`internal/server/metricsadapter.go` |
| 空间用量与流向查询（`WorkspaceUsage`） | `internal/metrics/workspace.go` |
| 读数端点（`/ui/workspace-usage.json`） | `internal/server/workspace_usage.go` |
| 流向图基元与页面 | `webui/charts.js`、`webui/chart-math.js`、`webui/pages/workspaces.js` |
| 界面切换与改名/删除 | `webui/pages/tasks.js`、`webui/api.js` |

`WorkspaceNames()` 由任务反推，默认空间固定排首位——界面上的下拉顺序因此与配置文件
里的书写顺序无关，且默认空间永远可直接选中。

## 8. 用量统计与流向图

每个请求在工作空间维度上的归属会被记下来，用于出「各空间用量」与「请求流向」两张
读数（WebUI 的**工作空间**页）。这一节说明存储形状与**为什么不能事后反推**。

### 为什么必须落库：工作空间反推不出来

一个自然的想法是「反正指标里已经存了请求模型名（`requested_model_id`），查的时候
拿它回配置里查一下不就是工作空间了」。这条路走不通：

同一个任务名**可以合法地同时存在于多个工作空间**（这正是工作空间要解决的问题，
见第 1 节）。`router-config.example.json` 里 `TASK_000001` 就同时出现在顶层 `tasks`
与 `workspaces.teamA.tasks` 下。历史指标只记了「调用方传的模型名是 `TASK_000001`」，
单凭它无从判断当时那个 `X-AMKR-Workspace` 头写的是什么。**归属只在请求处理时就近
可得**，因此必须在写入指标时一并记下。

### 存储：旁挂表而不是新列

归属存在 `request_workspace` 旁挂表里，**不是** `request_metrics` 的新列：

```sql
CREATE TABLE IF NOT EXISTS request_workspace (
    request_id INTEGER PRIMARY KEY,   -- 就是 request_metrics.id
    workspace  TEXT NOT NULL
)
```

两个理由，第二个是决定性的：

1. `request_id` 是 `INTEGER PRIMARY KEY`，即 rowid 别名。一对一约束与 JOIN 索引
   同时到手，且**不会**在 `sqlite_master` 里多出一条索引条目。
2. `request_metrics` 的建表原文、列序与索引定义被 `internal/metrics/testdata/schema.jsonl`
   **逐字节锁定**（`TestSchemaMatchesPython`），那是整条兼容链的根。而
   `ALTER TABLE ... ADD COLUMN` **会重写 `sqlite_master.sql`**（实测：即便在全新库
   上也会把新列以追加形式写进原文），加列必然打破那份语料——**而语料生成器已随
   Python 退役，无法重生成**。旁挂表让 `request_metrics` 自身逐字节不变，差异面从
   「表定义被改写」缩小到「多了一张表」。

因此 `TestSchemaMatchesPython` / `TestLegacyDatabaseUpgrade` 里有一份显式白名单
（`extraMasterEntries`）：测试的职责从「完全一致」变成「除白名单外完全一致」。往库里
再加第二个对象会让它立刻失败——兼容分歧必须是白名单式的、被逐条审视的。

> 旧二进制打开新库时只是看不到这张表，仍能正常读写指标。加列则会遇到它不认识的列序
> （`SELECT *` 与 `table_info` 的输出都会变）。

### 没有归属的行：`unattributed`，不兜底成 default

`workspace` 为空时不写旁挂表。查询端用 `NOT EXISTS` 把这类行单独统计成
`unattributed`（未归属），**不会**并进 `default`：

升级前写入的历史行都在这里。把它们算到默认工作空间头上会凭空造出一段并不存在的
用量，而且看起来像真的。界面因此把未归属提示放在流向图**之前**——否则用户先看到一张
少了一块的图，往下才知道原因。

同理，流向图里某一端为空的请求（`provider_id` / `upstream_model_id` 可空）**不补占位
节点**，而是在那个位置留出缺口。补一个占位符会让它与真实取值混在一起，看图的人分不出
哪条是数据、哪条是兜底。

### 流向图的五层

```
工作空间 → 请求模型 → 实际模型 → 供应商 → 上游模型
```

粒度选在这里是因为它恰好是请求在系统里的完整流转，且每一层都已存在于指标里
（`requested_model_id` / `model_id` / `provider_id` / `upstream_model_id`），无需额外
埋点。宽度可切请求数或 Token：前者看调用次数，后者看实际消耗。

布局上有一条容易写错、写错了却"看起来对"的地方：**纵向必须只用一把尺子**（全局
scale），不能让每层各自缩放到满高。后者会让同一节点的入边与出边拿到不同厚度，流带
溢出节点、读数失真。代价是层总量不齐时留白，而那段留白本身就是"在此处丢失的流量"。
节点自身的量取 `max(流入合计, 流出合计)`——取单条最大边会让多条出边依次排开时溢出。

### 边界

- **统计不是配额**：归属只用于观测，不用来拒绝任何请求（见第 3 节）。
- **归属从本版本才开始记录**：升级前的历史行永远是 `unattributed`，不会追溯回填
  （回填需要当时的工作空间名，而它没有被记下来）。
- 该读数挂在 `/ui/workspace-usage.json`：它是本项目自有的响应形状，没有可比对的
  oracle，因此不混进被逐字节语料锁定的 `/metrics` 系列（理由与 `/ui/` 的选择一致，
  见第 6 节）。

## 9. 改动时的检查清单

- [ ] 新代码是否让「不带 `X-AMKR-Workspace` 头」的行为发生了变化？语料会立刻发现。
- [ ] 是否给被语料锁定的响应体新增了字段？（`tasks/list` 等）
- [ ] 新增的 `/api` 路由是否放在了 `workspacePatterns()` 这类**独立清单**里，并同步
      了 `internal/api/server.go` 的 `patterns`（错方法的 405 判定）？
- [ ] 挂 `/ui/` 的新能力是否真的是「非管理面读数」？管理面的正式资源不该借 `/ui/`
      躲开语料冻结。
- [ ] 冲突检查是否被意外地收窄到空间内？模型名冲突必须保持**全局**。
- [ ] 空分组的两处口径是否仍然一致？（`configops.writeWorkspaceTasks` 与
      `RouterConfig.WorkspaceNames`）
- [ ] 是否往指标库里加了**第二个**新对象？`extraMasterEntries` 白名单会拦住——兼容
      分歧必须逐条列出来，不能无声增长。
- [ ] 归属是否仍只在写入时确定？（查询期反推不成立，见第 8 节）
- [ ] `go test ./...`、`node webui/probes/webui_auth_probe.mjs` 与
      `node webui/probes/webui_chart_probe.mjs` 是否全绿？
