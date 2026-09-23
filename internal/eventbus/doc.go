// Package eventbus 实现 /ws/events 的事件总线，移植 auto_model_key_router/event_bus.py；
// 并移植 app.py:101-135 里与它配套的 metrics_snapshot 节流策略。
//
// # 冻结契约（迁移计划 §4.6）
//
//  1. **鉴权握手**：首帧必须是 {"type":"auth","token":...}，等待上限 **10 秒**
//     （event_bus.py:37 的 timeout=10.0）。超时或消息非法关 **4001**
//     （event_bus.py:40，reason "auth timeout or invalid message"），鉴权失败关
//     **4003**（event_bus.py:50，reason "auth failed"）。客户端库会按这两个数字
//     分支，所以它们由 AuthTimeout / CloseCodeAuthTimeoutOrInvalidMessage /
//     CloseCodeAuthFailed 三个常量固定，并有具名测试。
//  2. **事件类型**：connected（app.py:342）、client_count（app.py:130）、
//     metrics_snapshot（app.py:109）、config_change（app.py:475）。
//  3. **节流**：≤1 次/秒（app.py:124 的 asyncio.sleep(1.0)）、≥1 次/30 秒
//     （app.py:103 的 _IDLE_BROADCAST_INTERVAL）、**无客户端时零开销**
//     （app.py:122 的 `client_count > 0` 门禁——没人订阅时**根本不构建**快照）。
//
// # 逐字节契约
//
// 广播帧的文本是 Python `json.dumps(obj)` 的**默认分隔符**形式（", " 与 ": "），
// 不是 AMKR 其它地方（如 SSE、JSON 响应）用的紧凑形式：event_bus.py:71 没有传
// separators。见 Encode。
//
// # 与参照实现的分歧（每一条都有具名测试）
//
//   - **首帧 JSON 不是对象**（"null" / "[]" / "1" / "\"x\""）：参照实现里
//     `message.get("token")` 抛 AttributeError，而它在 try 之外，所以异常直接冒泡、
//     **连接不会被关闭**（实测确认）。Go 侧不关闭连接，改为返回
//     ErrUnsupportedAuthFrame，把「接下来怎么处理这条连接」的决定权留给调用方。
//   - **verify 抛错**：同理冒泡（实测确认）。Go 侧返回该 error，不关闭连接。
//   - **多客户端广播顺序**：参照实现的 _clients 是 set、Go 侧是 map，两者都不定序，
//     因此不构成契约；单连接内的帧顺序才是契约。
//   - **超时的实现方式**：Go 侧用 goroutine + 定时器，而不是给读操作传带 deadline
//     的 context。原因是 coder/websocket 在 context 到期时会**直接关掉连接**
//     （read.go 的 setupReadTimeout → c.close()），客户端只会看到 1006 而不是 4001。
//   - **Conn 抽象**：参照实现直接用 starlette 的 WebSocket；Go 侧用只含三个方法的
//     Conn 接口（CoderConn 适配 github.com/coder/websocket），这样总线逻辑可以在
//     测试里用假连接驱动，也便于后续装配时替换。
//   - **回调时机**：参照实现在 async with self._lock 之后 await 回调；Go 的 sync.Mutex
//     不可重入，回调同样在**锁外**调用（回调会反过来 Broadcast，锁内调用会死锁）。
package eventbus
