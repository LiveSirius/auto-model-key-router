# AMKR Go 迁移进度与交接

配套文档：[迁移方案](./2026-09-17-go-migration.md)（架构决策、兼容性契约、风险登记）。

本文只记**当前状态**、**如何自行验证**、**待决策项**，不重复方案里的论证。

## 总体进度

| 指标 | 数值 |
| --- | --- |
| Python 核心 | 15,630 行 / 41 文件 |
| Go 生产代码 | 8,983 行 / 32 文件（9 个包） |
| 已移植 Python 源 | ≈ 5,015 行（约 1/3） |
| 服务端待移植 | 10,113 行 |
| Go 相关提交 | 15 |

按方案的分期，**Phase 0–2 基本完成**（基础设施、配置与 canonical 兼容、运行时与
协议转换）。剩下的体量集中在一个线性依赖链上：

```
metrics（进行中）
   └─> proxy_handler           1,400 行   ← 关键路径枢纽
config_operations（进行中）
   └─> management_api          1,647 行
   └─> config_editor           1,878 行
          └─> app / service / CLI / WebUI
                 └─> TUI（Bubble Tea，重写而非直译） 1,526 行
```

也就是说：**并行度已经用尽**，剩下的模块之间存在硬依赖，无法再靠加人来压缩。
乐观估计服务端可切换还需 **5–7 周**。

## 已落地的 Go 包

全部位于 `internal/`，每个包都带从**真实 Python 生成**的对拍语料。

| 包 | 对应 Python | 备注 |
| --- | --- | --- |
| `canonical` | （json 兼容层） | 逐字节兼容，两处静默失败面的基础 |
| `config` | `config.py` | v1~v4 迁移、原子保存、CRLF 语义 |
| `keypool` | `key_pool.py` 等 | key 选择、冷却、端点能力缓存 |
| `runtime` | `runtime.py` `streaming.py` | 租约、流式超时、重试策略 |
| `auth` | `auth.py` `visitor.py` | `amkr_no_visitor` 构建标签 |
| `protocol` | `protocols/*.py` | anthropic / responses / request |
| `proxysupport` | `proxy_support.py` | 请求构造、宽容解码 |
| `metrics` | `metrics.py` | （进行中，见下） |
| `upstream` | （httpx 行为） | 重定向、压缩、连接池、错误分类 |

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

## 待决策项（需要人来定，不阻塞开发）

以下都是**已确认的真实行为差异**，Go 侧按「与参照实现逐字对齐」处理，因此
Python 与 Go 在这些点上行为一致——但要明确它们是否是**期望**行为。

1. **上游调用放大**：单次下游请求最多可触发约 24 次上游调用。
2. **`images/edits` 忽略配置路由**：非 native 时走 `v1/images/edits` 兜底，不查
   `upstream_routes`；且 multipart 实质上不支持（仅 JSON）。
   已由 `TestUpstreamPathImagesEditsIgnoresConfiguredRoute` 锁定。
3. **流式分块 UTF-8 截断**：参照实现逐块 `decode("utf-8", errors="replace")`，
   多字节字符跨块时会产生 U+FFFD 乱码。**已实测确认，Go 侧已修复**（`SSESplitter`
   按字节切分），因此这是 Go 比 Python 正确的一处——需要确认接受这一偏离。
4. **`visitor_feature_installed` 语义变更**：从「能否 import itsdangerous」改为
   构建标签 `amkr_no_visitor` + 编译期常量。
5. **`protocol` 包对畸形输入不复刻异常**：Python 对非对象请求体会抛
   `ValueError`/`TypeError`/`AttributeError`，Go 侧选择保守兜底（原样返回、返回
   下限 1、退化成无 type）。理由是这些输入不构成合法协议体，而复刻异常会让
   `count_tokens` 整个接口 500。已由 `TestRequestKnownDeviations` 记录。
6. **`upstream.UpstreamURL` 的斜杠处理**：Go 侧曾用 `TrimSuffix`/`TrimPrefix`
   （只去一个斜杠），与 Python `rstrip`/`lstrip`（去全部）不一致。当前**不可达**
   （provider `base_url` 已被 `NormalizeUpstreamBaseURL` 规整），但仍应修正为
   `TrimRight`/`TrimLeft` 以消除两个实现并存的分歧。

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
4. `proxy_handler`（1,400 行，需要 metrics 定型后开工）。
5. `management_api` + `config_editor`。
6. `app` 装配、`service`、CLI、WebUI 后端。
7. TUI 重写。
8. `update.py` 与 Python 退役。

## 提交约定

按模块独立提交，Conventional Commits：`<type>(<scope>): <中文摘要>`，正文说明
**改了什么 / 为什么改 / 如何验证**。提交前用 `git diff --cached --name-only`
确认只含当前模块文件。
