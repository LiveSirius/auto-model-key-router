package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/Sparrived/auto-model-key-router/internal/auth"
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/eventbus"
	"github.com/Sparrived/auto-model-key-router/internal/metrics"
)

// 本文件是 app.py 里两个 WebSocket 路由的装配层：
//
//	@app.websocket("/ws/events")            app.py:325-349  -> handleWSEvents
//	register_websocket_proxy(app, proxy)    app.py:374      -> handleWSProxy
//	_broadcast_metrics_loop()               app.py:113-124  -> runMetricsBroadcast
//
// 三者的决策逻辑都不在这里：`/ws/events` 的握手契约（首帧、4001/4003、10 秒超时）
// 在 internal/eventbus，节流规则在 eventbus/throttle.go 的 Loop，HTTP-over-WebSocket
// 的折算与成帧在 internal/wsproxy。装配层只负责「把真实时钟、真实连接与 dirty 信号
// 接上去」——这正是 throttle.go 的 Loop 文档里说的「未来的装配任务照此写生产驱动器」。

// isWebSocketUpgrade 判断一条 HTTP 请求是不是 WebSocket 握手。
//
// 参照实现由 ASGI 协议层分流：Starlette 只在 `scope["type"] == "websocket"` 时匹配
// WebSocketRoute，而这个类型由 uvicorn 按**请求头**决定——uvicorn 的
// protocols/http/{h11,httptools}_impl.py 的 `_get_upgrade` 要求 `upgrade: websocket`
// 且 `connection` 里含 `upgrade` 这个 token。Go 侧没有协议层分流，因此按同一条规则
// 读头（而不是自己发明一套判断）。
//
// **已知分歧（有实测证据）**：Starlette 的 TestClient 只能发 HTTP scope，所以「用
// TestClient 发一个带升级头的普通 HTTP 请求」在 Python 侧落进 HTTP 路由（实测
// `GET /v1/does-not-exist` -> 401），而 Go 会真的升级（101）。在**真实** uvicorn 上
// 两者一致——实测同一个请求返回 `HTTP/1.1 101 Switching Protocols`，因为 uvicorn 已经
// 把它当成 websocket scope 了。因此这不是行为差异，而是测试载体的差异；对拍语料
// 因此**不收录**「带升级头的普通 HTTP 请求」这类用例（它锁不到真实协议行为）。
func isWebSocketUpgrade(request *http.Request) bool {
	if request == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, token := range strings.Split(request.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

// handleWSEvents 处理 `GET /ws/events`（app.py:325-349）。
//
// 分三段，与参照实现逐段对应：
//
//  1. `websocket.accept()`（app.py:327）——不是 WebSocket 握手就回 Starlette 的兜底
//     404（HTTP 请求不匹配 websocket 路由，见 isWebSocketUpgrade）；
//  2. `event_bus.authenticate(websocket, verify)`（app.py:339）——握手契约全在
//     internal/eventbus；未通过时它已经按契约关了连接（4001/4003）；
//  3. 补发 connected（app.py:342）后一直读到出错，再 `event_bus.disconnect`
//     （app.py:343-349）。
//
// 顺序上容易看漏的一点：客户端看到的**第一帧不是 connected**。`authenticate` 在返回
// 之前会 await `on_client_count_change`（app.py:135），而那个回调会立刻广播
// client_count 与 metrics_snapshot（app.py:129-132）；connected 是握手完成之后才补发
// 的。eventbus 的 e2e.jsonl 与本次新增的语料 ws_auth_valid 都钉住了
// `client_count, metrics_snapshot, connected` 这个顺序。
func (a *App) handleWSEvents(w http.ResponseWriter, request *http.Request) {
	if !isWebSocketUpgrade(request) {
		writeNotFound(w)
		return
	}
	// InsecureSkipVerify：FastAPI/Starlette 默认**不校验** Origin，coder/websocket
	// 默认校验。不关掉它，浏览器之外的客户端（脚本、测试）会因为缺 Origin 被 403 拒掉，
	// 那就与参照实现不同了。信任模型与参照实现一致：凭据来自首帧，由鉴权钩子校验。
	conn, err := websocket.Accept(w, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		// Accept 失败时它已经写过 HTTP 错误响应（不是 WS 关闭帧），这里没有可做的事。
		return
	}
	// 兜底：正常路径由 Authenticate 的 close(4001/4003) 或客户端断开收尾；这条 defer
	// 负责把底层连接一定收掉（对应参照实现里异常冒泡时 ASGI 服务器的兜底关闭）。
	defer func() { _ = conn.CloseNow() }()

	wrapped := eventbus.NewCoderConn(conn)
	// 用握手请求的 context：它在处理器返回时被 net/http 取消，而本处理器直到连接
	// 结束才返回，所以不存在「连接还活着但 context 已经没了」的窗口。**不能**给它加
	// deadline：coder/websocket 在 context 到期时会直接关闭底层连接，4001 就发不出去
	// 了（见 eventbus.CoderConn.ReadText 的说明）。10 秒上限在 Authenticate 内部。
	ctx := request.Context()
	authenticated, authErr := a.eventBus.Authenticate(ctx, wrapped, a.wsEventsVerifier(request.Header))
	if authErr != nil {
		// 参照实现会把这个异常冒到 ASGI 服务器，连接被**异常**关闭（不发 4001/4003）。
		// 实测（eventbus/testdata/e2e.jsonl 的 e2e_json_null）：客户端侧看到的是
		// AttributeError，而不是关闭帧。Go 侧没有可冒泡的异常，等价动作就是不发关闭帧
		// 直接断开（上面那条 defer 的 CloseNow）——客户端同样只看到连接异常终止。
		return
	}
	if !authenticated {
		// Authenticate 已按契约关闭（4001 或 4003），这里不能再关一次：重复的关闭帧会
		// 覆盖掉客户端真正要读的那个关闭码。
		return
	}
	defer a.eventBus.Disconnect(wrapped)

	// app.py:342 的 `send_json`：**紧凑**分隔符，与 event_bus.py:71 的 broadcast 帧
	// 不同（`{"type":"connected","data":{}}` vs `{"type": "connected", "data": {}}`）。
	// eventbus.ConnectedFrame 逐字节复刻了前者。
	if err := wrapped.SendText(ctx, eventbus.ConnectedFrame()); err != nil {
		return
	}
	for {
		if _, err := wrapped.ReadText(ctx); err != nil {
			return
		}
	}
}

// wsEventsVerifier 构造首帧 token 的校验器，对应 app.py:331-337 的 verify 闭包。
//
// 三个必须照抄的语义：
//
//   - **只对完整权限开放**：受限的推理凭据（访问密钥、工作空间推理 key）能过 HTTP 的
//     /v1/models，但过不了事件流，会被 4003 拒掉。
//   - **配置在连接建立时读一次**（app.py:329 的
//     `config = app.state.runtime_manager.current.config`）：之后即使配置文件被改写、
//     热重载换了配置，这条连接的首帧仍按旧配置里的 local_api_key 校验。这是参照实现
//     的行为，照抄而不是「顺手修正」。
//   - token 只来自首帧（WebSocket 握手带不了自定义头），因此用
//     auth.WebsocketAuthRequest 把首帧折成请求；**握手头原样保留**，宿主的 cookie
//     鉴权（替换 Options.Authorizer）因此同样能用。
func (a *App) wsEventsVerifier(handshake http.Header) eventbus.TokenVerifier {
	localAPIKey := ""
	if current := a.manager.Current(); current != nil && current.Config != nil {
		localAPIKey = current.Config.LocalAPIKey
	}
	return func(token string) (bool, error) {
		context := auth.Authenticate(
			a.options.Authorizer,
			auth.WebsocketAuthRequest(handshake, token),
			localAPIKey,
		)
		return context != nil && context.IsFull(), nil
	}
}

// handleWSProxy 把升级到 /v1/{path} 的连接交给 internal/wsproxy（app.py:374 的
// register_websocket_proxy）。
//
// 注入点上不需要适配层：`internal/proxy.Handler.Handle` 的方法值签名与
// `wsproxy.ProxyHandler` 逐字相同（见 wsproxy.ProxyHandler 的说明），New 里直接绑定
// `app.proxy.Handle`。path 由 wsproxy 自己按 `{path:path}` 的语义还原（去掉 "/v1/"
// 前缀、URL 解码后取值），与 HTTP 通配路由的 strings.TrimPrefix 结果一致。
func (a *App) handleWSProxy(w http.ResponseWriter, request *http.Request) {
	a.wsProxy.ServeHTTP(w, request)
}

// —— 节流广播循环 ——

// startMetricsBroadcast 启动 app.py:113-124 的节流广播循环。
//
// throttle.go 的 Loop 只把循环体做成状态机（虚拟时钟推进），等待与睡眠是**装配层的
// 工作**——这里就是那个驱动器。与 Loop 文档里给的骨架同形，只多两处：
//
//   - 定时器每轮新建并在退出 select 后 Stop：Loop 的 `sleepTill` 是「上一轮唤醒 +
//     1 秒」，与「新一轮 select 里那个 30 秒定时器开始计时的时刻」重合，所以每轮必须
//     是一个**全新**的 30 秒窗口，上一轮残留下来的 pending 定时不能被继承；
//   - ctx 取消：对应 lifespan 关停时 cancel 掉广播任务（app.py:69-77）。
//
// 虚拟时钟的原点（broadcastStart）在 New 里就取好了，比循环真正启动略早一点。这个方向
// 是安全的：Advance 只要求 `now >= sleepTill + IdleInterval` 才认为超时到点，原点取早
// 会让第一次超时判定**更容易**成立；取晚则会让它永远差一点点，白等一轮 30 秒。
//
// dirty 信号的语义与 Python 的 asyncio.Event 一致：**电平而不是队列**。容量 1 的非阻塞
// 发送正好表达这一点——sleep(1.0) 期间到达的写入只要还有一个信号没过期，下一轮
// wait() 就会立刻返回（对应 eventbus 语料里的
// dirty_inside_sleep_is_remembered）。多余信号被丢弃是安全的：dirty 只表示「有写入」，
// 不承载数量。
func (a *App) startMetricsBroadcast() {
	if a.eventBus == nil || a.broadcaster == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.broadcastCtx = ctx
	a.broadcastCancel = cancel
	a.broadcastDone = make(chan struct{})
	// 订阅者数变化的回调（app.py:129-132）：先广播 client_count，count > 0 时再立刻
	// 广播一次 metrics_snapshot——这一条不经过节流器，所以新客户端连上时会先看到
	// client_count + metrics_snapshot，然后才是 handleWSEvents 补发的 connected。
	a.eventBus.OnClientCountChange = func(count int) {
		a.broadcaster.OnClientCountChange(a.broadcastContext(), count)
	}
	go a.runMetricsBroadcast(ctx)
}

// runMetricsBroadcast 是 startMetricsBroadcast 起的那个 goroutine。
func (a *App) runMetricsBroadcast(ctx context.Context) {
	defer close(a.broadcastDone)
	for {
		var dirtyAts []time.Duration
		timer := time.NewTimer(eventbus.IdleBroadcastInterval)
		select {
		case at := <-a.metricsDirty:
			dirtyAts = append(dirtyAts, at)
		case <-timer.C:
			// 空闲心跳，刷新 RPM/TPM 衰减（app.py:120-121 的 `pass`）。
		case <-ctx.Done():
			timer.Stop()
			return
		}
		timer.Stop()
		// Broadcaster.Tick 在读 Bus.ClientCount() **之后**才决定要不要构建快照，
		// 因此没有订阅者时 metrics.Store.Snapshot 根本不会被调用（app.py:122）。
		a.broadcaster.Tick(ctx, time.Since(a.broadcastStart), dirtyAts)
		// app.py:124 的 `await asyncio.sleep(1.0)`：它在每轮**末尾**无条件执行，
		// 所以空闲时两次广播的间隔是 30 + 1 = 31 秒（见 eventbus.IdleBroadcastInterval）。
		select {
		case <-time.After(eventbus.BroadcastMinInterval):
		case <-ctx.Done():
			return
		}
	}
}

// stopMetricsBroadcast 取消广播循环并等它退出。
//
// 与 lifespan 的 finally 同序（app.py:69-77）：**先**取消广播任务，**再**关资源
// （App.Close 里紧接着走 manager.Close）。顺序不能反：循环里的快照构建读的是当前代
// 指标库，先关库会让它在已关闭的库上查询。
func (a *App) stopMetricsBroadcast() {
	if a.broadcastCancel == nil {
		return
	}
	a.broadcastCancel()
	if a.broadcastDone != nil {
		<-a.broadcastDone
	}
}

// broadcastContext 返回广播循环的 context；循环未启动时退化成 Background。
func (a *App) broadcastContext() context.Context {
	if a.broadcastCtx != nil {
		return a.broadcastCtx
	}
	return context.Background()
}

// markMetricsDirty 是挂在指标库上的写后回调，对应 app.py:126-127 的
// `_metrics_dirty.set()`。
//
// 非阻塞发送：Record 在写完一行之后同步调用它（metrics.Store.Record），绝不能因为广播
// 循环正忙而把写入卡住——参照实现里 `Event.set()` 也是不等待的。
func (a *App) markMetricsDirty() {
	if a.metricsDirty == nil {
		return
	}
	select {
	case a.metricsDirty <- time.Since(a.broadcastStart):
	default:
		// 已经有一个未消费的信号：电平语义下它等价于本次写入。
	}
}

// bindMetricsDirty 把写后回调挂到**一代**指标库上。
//
// 对应 app.py:134（装配时挂一次）与 app.py:468（换库时把回调带到新库上）。少了后者，
// metrics_db_path 一经热重载，写入就不再产生 dirty 信号，metrics_snapshot 会静默退化
// 成 30 秒一次的心跳。
func (a *App) bindMetricsDirty(store *metrics.Store) {
	if store == nil {
		return
	}
	store.SetOnRecord(a.markMetricsDirty)
}

// broadcastMetricsSnapshot 构建一次快照，对应 app.py:105-111 的
// `_broadcast_metrics_snapshot`。
//
// 每次广播都重新取**当前代**的指标库（app.py:107 的
// `app.state.runtime_manager.current.metrics`），所以热重载换库后立刻生效；`since`
// 取该库自己的 `_started_at`（app.py:108），而不是进程启动时刻。
//
// 这个函数是 eventbus.SnapshotFunc，只由 Broadcaster 在「已经决定要广播」时调用——
// 「没人订阅就零开销」的性质就落在这一层：Loop.Advance 只在 clientCount > 0 时才把
// 时刻放进 fired，Tick 只对 fired 调用本函数。
func (a *App) broadcastMetricsSnapshot(_ context.Context) (*canonical.Value, error) {
	adapter := a.currentMetricsAdapter()
	if adapter == nil {
		return nil, errors.New("server: 指标库未装配，无法构建 metrics_snapshot")
	}
	startedAt := adapter.store.StartedAt()
	return adapter.store.Snapshot(nil, &startedAt)
}
