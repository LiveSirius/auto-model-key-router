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
- 因此工作空间不是「多租户」：它不隔离凭据、用量统计或配额。它只是任务集合的
  命名空间。

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

## 5. 未知工作空间名不是错误

调用方写了一个没建过的空间名时，**不报错**：那里没有任务，于是任务查表落空，接着按
普通模型名继续解析，最终和「模型未配置」是同一个 `404`。

`NormalizeWorkspace` 因此只做去空白，**不校验存在性**。单加一条「空间不存在」的错误
既没有信息量（调用方真正需要知道的是任务或模型名不对），也会多一种需要维护的状态。

## 6. 兼容性契约

工作空间是 Go 侧新增的能力，参照实现没有对应实现，因此它**没有**可对拍的语料。它的
兼容性契约是另一条：**不影响既有配置与既有调用方**。

由此推出必须守住的性质：

1. **`config_version` 仍是 4**，`workspaces` 是可选新增字段，顶层 `tasks` 就是
   `DefaultWorkspace`。
2. **不带 `X-AMKR-Workspace` 头的请求行为逐字节不变。** 正因如此，被语料锁定的响应体
   （`tasks/list` 锁定了 `{"tasks":[{name,model,fallback_model,params}],"config_revision"}`）
   **没有、也不允许**新增 `workspace` 字段——当前空间由请求头决定，不体现在响应里。
3. **不新增 `/api` 路由。** 管理面 47 条与运维面 7 条路由被逐字节语料锁定，那些语料由
   已退役的 Python 实现产出，是本项目兼容性的唯一凭证。工作空间是 Python 侧没有的
   新能力，给它手写 `/api` 语料等于伪造兼容性证据。因此工作空间目录挂在
   `/ui/workspaces.json`（与 `pricing.json`、自更新入口同类），那里不在任何冻结清单
   之内。
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
| 管理端点感知空间 | `internal/api/handlers_meta.go` |
| 工作空间目录 `/ui/workspaces.json` | `internal/server/workspaces.go` |
| 界面切换 | `webui/pages/tasks.js`、`webui/api.js` |

`WorkspaceNames()` 由任务反推，默认空间固定排首位——界面上的下拉顺序因此与配置文件
里的书写顺序无关，且默认空间永远可直接选中。

## 8. 改动时的检查清单

- [ ] 新代码是否让「不带 `X-AMKR-Workspace` 头」的行为发生了变化？语料会立刻发现。
- [ ] 是否给被语料锁定的响应体新增了字段？（`tasks/list` 等）
- [ ] 是否新增了 `/api` 路由？应挂 `/ui/` 并说明理由。
- [ ] 冲突检查是否被意外地收窄到空间内？模型名冲突必须保持**全局**。
- [ ] 空分组的两处口径是否仍然一致？（`configops.writeWorkspaceTasks` 与
      `RouterConfig.WorkspaceNames`）
- [ ] `go test ./...` 与 `node webui/probes/webui_auth_probe.mjs` 是否全绿？
