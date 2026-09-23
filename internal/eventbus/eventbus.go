package eventbus

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 事件类型。四个取值都来自参照实现的**调用点**，而不是 event_bus.py 本身：
// connected 由 app.py:342 发出、client_count 由 app.py:130、metrics_snapshot 由
// app.py:109、config_change 由 app.py:475。
const (
	// EventConnected 是鉴权通过后由调用方补发的首帧（app.py:342）。
	EventConnected = "connected"
	// EventClientCount 携带 {"count": n}，在订阅者数变化时广播（app.py:130）。
	EventClientCount = "client_count"
	// EventMetricsSnapshot 携带指标快照，受节流约束（app.py:109）。
	EventMetricsSnapshot = "metrics_snapshot"
	// EventConfigChange 携带 {"reloaded": true}，只在有订阅者时广播（app.py:475）。
	EventConfigChange = "config_change"
)

// AuthTimeout 是等首帧的上限，对应 event_bus.py:37 的 timeout=10.0。
const AuthTimeout = 10 * time.Second

// 关闭码。客户端库按这两个数字分支，所以必须逐字对齐 event_bus.py:40 / :50。
const (
	// CloseCodeAuthTimeoutOrInvalidMessage 用于首帧超时或消息非法（event_bus.py:40）。
	CloseCodeAuthTimeoutOrInvalidMessage = 4001
	// CloseCodeAuthFailed 用于首帧合法但鉴权不通过（event_bus.py:50）。
	CloseCodeAuthFailed = 4003
)

// 关闭原因文本，逐字对齐 event_bus.py:40 / :50。
const (
	// AuthCloseReasonTimeoutOrInvalidMessage 对应 event_bus.py:40 的 reason。
	AuthCloseReasonTimeoutOrInvalidMessage = "auth timeout or invalid message"
	// AuthCloseReasonFailed 对应 event_bus.py:50 的 reason。
	AuthCloseReasonFailed = "auth failed"
)

// ErrUnsupportedAuthFrame 表示首帧是合法 JSON 但不是对象。
//
// 参照实现在这种情况下抛 AttributeError（`message.get` 不存在于 list/str/int/None），
// 且因为没有关闭连接，异常会一路冒到 ASGI 服务器。Go 侧把它变成可判定的 error，
// 调用方若要与参照实现完全对齐，应当直接关连接（不要回 4001/4003）。
var ErrUnsupportedAuthFrame = errors.New("eventbus: 首帧 JSON 不是对象")

// Conn 是事件总线需要的最小 WebSocket 能力面。
//
// 只暴露三个方法（而不是直接用 *websocket.Conn）有两个理由：一是总线逻辑
// （鉴权握手、成帧、剔除失效连接）可以在测试里脱离真实连接驱动；二是后续装配时
// 替换实现不需要改这个包。CoderConn 是 github.com/coder/websocket 的适配器。
type Conn interface {
	// ReadText 读取下一帧的**文本**内容，对应 starlette 的 receive_text()。
	//
	// 语义要点：二进制帧、对端断开、超时都必须以 error 返回，而不是返回空串——
	// 参照实现在这三种情况下分别抛 KeyError / WebSocketDisconnect / TimeoutError，
	// 而它们统统落进同一个 except，最终都是 4001。
	//
	// 另一个隐含要求：Close 必须让挂起的 ReadText 返回（CoderConn 由
	// coder/websocket 保证），否则超时路径上那个读 goroutine 会一直挂着。
	ReadText(ctx context.Context) (string, error)
	// SendText 发送一帧文本，对应 starlette 的 send_text()。
	SendText(ctx context.Context, text string) error
	// Close 以指定关闭码关闭连接，对应 starlette 的 close(code=..., reason=...)。
	//
	// 允许是阻塞的：CoderConn 会做完 coder/websocket 的关闭握手（对端不应答时
	// 最长 5 秒）。参照实现的 starlette close 只是投递一条 ASGI 消息，因此这里
	// 是**已知分歧**：关闭帧的送达与关闭码不受影响，只是本侧返回得更晚。
	Close(code int, reason string)
}

// TokenVerifier 校验首帧里的 token。由调用方注入，以便复用统一的鉴权钩子。
//
// 对应 event_bus.py:15 的 TokenVerifier = Callable[[str], Awaitable[bool]]。返回
// error 对应参照实现里 verify 抛异常——那种情况下参照实现**不会**关 4003，而是让
// 异常冒泡，所以这里也把 error 交给调用方。
type TokenVerifier func(token string) (bool, error)

// Bus 是事件总线本体，对应 event_bus.py:18 的 EventBus。
//
// 零值可用（clients 懒初始化），但推荐用 New。
type Bus struct {
	mu      sync.Mutex
	clients map[Conn]struct{}

	// OnClientCountChange 在订阅者数变化后调用，对应 app.py:135 的注入点。
	//
	// 参照实现的回调是 `async def`，会先广播 client_count、再（count>0 时）广播一次
	// metrics_snapshot。因为它在 authenticate() 返回**之前**被 await，客户端看到的
	// 第一帧是 client_count 而不是 connected（app.py:339-342，实测与
	// tests/test_embedding.py:271-287 一致）。Go 侧保持同步调用以维持同样的顺序。
	OnClientCountChange func(count int)

	// Logger 为 nil 时不记录日志。参照实现在连接/断开处写 debug 日志
	// （event_bus.py:56、:65），属可观测性而非契约。
	Logger *slog.Logger
}

// New 构造一个空总线。
func New() *Bus {
	return &Bus{clients: make(map[Conn]struct{})}
}

// ClientCount 返回当前已认证的订阅者数，对应 event_bus.py:89-91 的 client_count。
//
// 这是「无客户端零开销」的判据：调用方在构建快照**之前**先看它（app.py:122）。
func (b *Bus) ClientCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

// Authenticate 等客户端发首帧并校验，对应 event_bus.py:31-59。
//
// 返回值 (true, nil) 表示认证成功且已加入广播列表；返回 (false, nil) 表示已经按
// 契约关闭了连接（4001 或 4003）；返回 (false, err) 表示参照实现会**冒泡异常、
// 不关连接**的两种情形（首帧不是 JSON 对象、verify 抛错），关闭策略由调用方决定。
func (b *Bus) Authenticate(ctx context.Context, conn Conn, verify TokenVerifier) (bool, error) {
	raw, err := receiveFirstFrame(ctx, conn)
	if err != nil {
		// 对应 event_bus.py:39-41：TimeoutError、JSONDecodeError 以及**任何**其它
		// 异常（含 WebSocketDisconnect、二进制帧的 KeyError）都落这里。
		//
		// 注意 wait_for 取消的是 receive_text() 这个任务，连接本身在 starlette 里
		// 还没被关，所以 close(4001) 是**唯一**的关闭动作：客户端必须看到 4001，
		// 而不是 1006。
		conn.Close(CloseCodeAuthTimeoutOrInvalidMessage, AuthCloseReasonTimeoutOrInvalidMessage)
		return false, nil
	}

	message, parseErr := canonical.ParseString(raw)
	if parseErr != nil {
		// json.JSONDecodeError 与超时是同一个 4001 分支（event_bus.py:39-41）。
		conn.Close(CloseCodeAuthTimeoutOrInvalidMessage, AuthCloseReasonTimeoutOrInvalidMessage)
		return false, nil
	}
	if !message.IsObject() {
		return false, ErrUnsupportedAuthFrame
	}

	// 短路顺序是契约的一部分：`type != "auth"` 或 token 不是字符串时**不会**调用
	// verify（现场记录：wrong_type / token_int / token_missing 三种输入的
	// verify_calls 都是空）。把 verify 提前执行会让注入的鉴权钩子被无效帧调用，
	// 对宿主可能是计费或审计上的差异。
	token := message.Lookup("token")
	valid := message.Lookup("type").StringValue() == "auth" && token.IsString()
	if valid {
		// await verify(token) 在 try **之外**（event_bus.py:44-48）：verify 抛异常
		// 不会关 4003，而是冒泡。
		verified, verifyErr := verify(token.Str)
		if verifyErr != nil {
			return false, verifyErr
		}
		valid = verified
	}
	if !valid {
		conn.Close(CloseCodeAuthFailed, AuthCloseReasonFailed)
		return false, nil
	}

	b.mu.Lock()
	if b.clients == nil {
		b.clients = make(map[Conn]struct{})
	}
	b.clients[conn] = struct{}{}
	total := len(b.clients)
	b.mu.Unlock()

	b.log().Debug("event bus client connected", "total", total)
	// 锁外调用：回调会 Broadcast，而 Broadcast 要再取同一把锁。
	b.notifyClientCount(total)
	return true, nil
}

// receiveFirstFrame 是等首帧的 10 秒上限，对应 event_bus.py:37 的
// wait_for(receive_text(), timeout=10.0)。
//
// 为什么不直接把 AuthTimeout 做成读操作的 deadline：coder/websocket 在 context
// 到期时会**直接关闭连接**（read.go 的 setupReadTimeout 里注册的 AfterFunc 调用
// c.close()），那样客户端收到的是 1006 异常关闭，而参照实现的 4001 永远发不出去。
// 换成「读 goroutine + 定时器」后，超时路径仍由我们自己的 close(4001) 收尾。
//
// goroutine 不会泄漏：无论哪条分支，连接随后都会被关闭（超时分支由调用方关，
// 正常分支读完即返回），被阻塞的读会立刻出错并退出；结果通道有缓冲，写回不阻塞。
func receiveFirstFrame(ctx context.Context, conn Conn) (string, error) {
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := conn.ReadText(ctx)
		done <- result{text: text, err: err}
	}()

	timer := time.NewTimer(AuthTimeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.text, res.err
	case <-timer.C:
		return "", context.DeadlineExceeded
	}
}

// Disconnect 把连接移出广播列表并通知订阅者数变化，对应 event_bus.py:61-67。
//
// 与参照实现一致：即使连接从未成功认证（不在集合里），回调也会被调用一次——
// discard 是幂等的，而计数回调不是「只在变化时」才触发的。
func (b *Bus) Disconnect(conn Conn) {
	b.mu.Lock()
	delete(b.clients, conn)
	total := len(b.clients)
	b.mu.Unlock()

	b.log().Debug("event bus client disconnected", "total", total)
	b.notifyClientCount(total)
}

// Broadcast 向所有已认证客户端推送事件，对应 event_bus.py:69-87。
//
// 语义与参照实现一致的三处：
//
//   - 先成帧、再看有没有客户端（event_bus.py:71-77）。成帧是纯内存操作，"零开销"
//     指的是**没人订阅时不构建快照**，那由调用方用 ClientCount() 门禁，而不是这里。
//   - 先在锁内拷一份客户端列表，再在锁外逐个发送（event_bus.py:74-83）：一个慢
//     客户端不能拖住 Authenticate/Disconnect。
//   - 发送失败的连接在**整轮之后**统一剔除，且不会触发 OnClientCountChange
//     （参照实现只 discard，不调回调）。
func (b *Bus) Broadcast(ctx context.Context, eventType string, data *canonical.Value) {
	message := Encode(eventType, data)

	b.mu.Lock()
	clients := make([]Conn, 0, len(b.clients))
	for conn := range b.clients {
		clients = append(clients, conn)
	}
	b.mu.Unlock()

	if len(clients) == 0 {
		return
	}
	var stale []Conn
	for _, conn := range clients {
		if err := conn.SendText(ctx, message); err != nil {
			stale = append(stale, conn)
		}
	}
	if len(stale) == 0 {
		return
	}
	b.mu.Lock()
	for _, conn := range stale {
		delete(b.clients, conn)
	}
	b.mu.Unlock()
}

// notifyClientCount 在锁外调用回调。
func (b *Bus) notifyClientCount(count int) {
	if b.OnClientCountChange != nil {
		b.OnClientCountChange(count)
	}
}

func (b *Bus) log() *slog.Logger {
	if b.Logger == nil {
		return slog.New(discardHandler{})
	}
	return b.Logger
}

// discardHandler 是 Logger 为 nil 时的空处理器。
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

// Encode 把 (type, data) 成帧，对应 event_bus.py:71-73 的 json.dumps 调用。
//
// 分隔符是 Python 的**默认值**（", " 与 ": "），这是本包最容易看错的一处：
// event_bus.py:71 没有传 separators，所以线上文本是
//
//	{"type": "connected", "data": {}}
//
// 而不是全仓库其它 JSON 输出用的 `{"type":"connected","data":{}}`。键顺序是
// 构造顺序（type 在前、data 在后），与参照实现一致。
//
// data 为 nil 时按 JSON 的 null 处理（Python 的 `data=None`）。
func Encode(eventType string, data *canonical.Value) string {
	var b strings.Builder
	b.WriteString(`{"type": `)
	b.WriteString(canonical.DumpsOrdered(canonical.NewString(eventType)))
	b.WriteString(`, "data": `)
	writeSpaced(&b, data)
	b.WriteByte('}')
	return b.String()
}

// writeSpaced 用 Python json.dumps 的默认分隔符写出一个 canonical 值。
//
// 标量直接借用 canonical.DumpsOrdered：字符串转义、浮点格式（含 NaN/Infinity 的
// `NaN` / `Infinity` 写法）与整数归一化都已在那边逐字节对齐过 Python，没有必要
// 重写一份。容器才需要自己插分隔符，因为 canonical 只提供紧凑与缩进两种形式。
func writeSpaced(b *strings.Builder, value *canonical.Value) {
	if value == nil {
		b.WriteString("null")
		return
	}
	switch {
	case value.IsObject():
		b.WriteByte('{')
		for index, key := range value.Obj.Keys() {
			if index > 0 {
				b.WriteString(", ")
			}
			// 键同样走 canonical 的字符串转义（Python 的 json 会转义非 ASCII 之外的
			// 控制字符与引号，ensure_ascii=False 只是不把非 ASCII 变成 \uXXXX）。
			b.WriteString(canonical.DumpsOrdered(canonical.NewString(key)))
			b.WriteString(": ")
			child, _ := value.Obj.Get(key)
			writeSpaced(b, child)
		}
		b.WriteByte('}')
	case value.IsArray():
		b.WriteByte('[')
		for index, item := range value.Items() {
			if index > 0 {
				b.WriteString(", ")
			}
			writeSpaced(b, item)
		}
		b.WriteByte(']')
	default:
		b.WriteString(canonical.DumpsOrdered(value))
	}
}
