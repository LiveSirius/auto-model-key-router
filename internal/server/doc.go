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
// 除下面这些点，本包的行为与 app.py 对齐（当年由 testdata/server_corpus.json 的
// 差分语料逐字节核对过，该语料已随 Python 参照实现退役删除；现在的保护是下面「方法」
// 一节列出的那些本包用例，它们只覆盖被逐条写出来的地方）：
//
//   - **WebSocket 已接线，但升级判定读的是请求头**。参照实现由 ASGI 协议层分流
//     （`scope["type"]`），而 Go 的 net/http 没有这一层，因此 websocket.go 的
//     isWebSocketUpgrade 复刻 uvicorn 的 `_get_upgrade`：`upgrade: websocket` 且
//     `connection` 含 `upgrade`。真实 uvicorn 上两者一致（当年实测带这两个头的
//     `GET /v1/does-not-exist` 返回 101）；而 Starlette 的 TestClient 只能发 HTTP
//     scope，「用 TestClient 发带升级头的普通 HTTP 请求」会落进 Python 的 HTTP 路由，
//     因此这条差异锁不到真实协议行为（详见 websocket.go 的说明）。
//   - **异常冒泡的落点不同**。首帧是合法 JSON 但不是对象时，参照实现抛
//     AttributeError 冒到 ASGI 服务器（连接的异常关闭）；Go 侧在
//     `eventbus.Authenticate` 返回 ErrUnsupportedAuthFrame 后直接关闭底层连接。
//     客户端观测一致（零帧 + 异常终止），但没有可比的异常对象。
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
// 本包的测试是纯 Go 的：全部用 httptest 装配真实的 App，不再有回放外部语料的层。
//
//   - routes_test.go：从装配后的 Handler 出发逐条实例化管理/运维/app 面路由，证明
//     三块 URL 空间互不遮蔽（外层 mux 的 "/" 兜底一旦写宽就会静默吞掉它们）；
//   - websocket_test.go：两个 WebSocket 路由的装配——握手帧序、4001/4003 关闭码、
//     dirty 驱动的 metrics_snapshot、config_change，以及 /v1/{path} 升级到 wsproxy
//     并真的打到上游，全部用真实 socket（ResponseRecorder 不支持 Hijack）；
//   - handlers_test.go：app 面读数的语义（/v1/models 的收窄、/health 的取数来自
//     装配层、401 的两套错误信封）；
//   - ops_test.go / accesslog_test.go：运维面的可达性与访问日志接进 /api/logs 的闭环；
//   - reload_test.go：热重载的四条可观测语义（换代、换库、改坏配置保留旧一代、mtime）；
//   - metricsadapter_test.go / updatecheck_test.go / pricing_test.go /
//     workspace_usage_test.go / workspace_panel_test.go / accesskey_usage_test.go /
//     update_test.go：各条接缝与新增读数的形状、鉴权与 Allow 头。
//
// 纯决策（节流状态机、成帧、字段映射）分别由 internal/eventbus、internal/wsproxy、
// internal/metrics 自己的用例覆盖，本包只验证「接缝接对了」。
package server
