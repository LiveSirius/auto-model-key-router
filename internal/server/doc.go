// Package server 把已经移植完成的各个模块装配成一个可运行的 AMKR 服务。
//
// 它是 auto_model_key_router/app.py（497 行）的对应物：**不实现任何业务语义**，
// 只做四件事：
//
//  1. 构造一代运行时资源（config / keypool / metrics / 上游客户端）并交给
//     internal/runtime 的 RuntimeManager 管理；
//  2. 把 app 面（`HEAD /`、`GET /health`、`GET /v1/models`、三条 `/metrics*`）与
//     `/v1/{path}` 代理通配路由注册到一棵 ServeMux 上，并挂载 api 的 47 条管理路由
//     与可选 WebUI；
//  3. 移植 _reload_config_if_changed 的热重载（配置 mtime 变化时换一代资源）；
//  4. 在启动/关停时管理资源生命周期。
//
// 装配关系（谁是谁的接缝）：
//
//	config ──► keypool ─┐
//	                    ├──► runtime.NewRuntimeResources ──► RuntimeManager
//	metrics.Open ───────┤                                      │
//	upstream.New ───────┘                                      ├──► proxy.New(manager, sink, opts)
//	                                                           └──► api.Server{Reload, CurrentConfig, ...}
//
// # 与参照实现的有意分歧
//
// 除下面这些点，本包的行为与 app.py 逐字节对齐（已由 testdata/server_corpus.json
// 的差分语料锁定）：
//
//   - **缺少 WebSocket 与事件总线**。参照实现用 EventBus 向 `/ws/events` 推送
//     metrics_snapshot / client_count / config_change；前端改为轮询，本任务明确
//     不含 WebSocket，因此 app.py:64-66 的指标广播任务**不启动**，
//     app.py:355 的 `_metrics_dirty.set()` 也没有对应动作。接缝留在
//     metrics.Store.SetOnRecord（写后回调）上：将来接上广播循环时在这里注册即可。
//   - **运维面已接线，但有两个接缝仍是 nil**。ops_api.py 的 7 条路由由
//     internal/api 实现，装配层只负责把 api.Server.OpsEnabled（跟配置里的
//     ops_enabled，可由 Options.OpsEnabled 覆盖）与 Version / WebUIStatus 传进去；
//     路由注册发生在 api.Server.Handler() 内部，与参照实现一样挂在同一棵 mux 上。
//     仍未接线的是：`POST /api/service/{action}`（RunServiceAction，service.py 未移植）
//     与三条 `/api/integrations*`（Integrations，internal/agentconfig 已就绪但不在本
//     任务的装配范围内），两者都**响亮失败**（500 + 明确文案），不会假装成功。
//   - **完整 CLI 与 service.py 未移植**。见 cmd/amkr 的说明。
//   - 若干 HTTP 细节差别（ServeMux 与 Starlette 的路由语义差异），逐条写在
//     routes.go 与 handlers.go 的注释里。
//
// # 方法
//
// 本包的测试分三层：
//
//   - corpus_test.go：回放 scripts/gen_server_corpus.py 用**真实 Python 应用**
//     产出的语料，逐字节比对状态码 / 响应体 / content-type / content-length；
//   - *_test.go：对无法在 HTTP 层对拍的接缝做单元测试（指标字段映射、版本检查的
//     update_available 推导、热重载、WebUI 挂载、路由不互相遮蔽）；
//   - handlers_test.go：纯 Go 的边界补充（例如未覆盖的查询参数组合）。
package server
