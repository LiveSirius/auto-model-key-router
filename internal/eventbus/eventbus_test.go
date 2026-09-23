package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestAuthTimeoutIsTenSeconds 锁定冻结契约里的「首帧等待上限 10 秒」。
func TestAuthTimeoutIsTenSeconds(t *testing.T) {
	if AuthTimeout != 10*time.Second {
		t.Fatalf("AuthTimeout = %v，参照实现是 event_bus.py:37 的 timeout=10.0", AuthTimeout)
	}
}

// TestAuthHandshakeCloseCodes 锁定 4001 / 4003 与两条 reason 文本。
//
// 客户端库按这两个数字分支（超时/非法 vs 鉴权失败），照抄错一个数字就会让调用方
// 走错分支；reason 也会出现在客户端的日志里。
func TestAuthHandshakeCloseCodes(t *testing.T) {
	if CloseCodeAuthTimeoutOrInvalidMessage != 4001 {
		t.Fatalf("超时/非法消息的关闭码 = %d，参照实现是 4001", CloseCodeAuthTimeoutOrInvalidMessage)
	}
	if CloseCodeAuthFailed != 4003 {
		t.Fatalf("鉴权失败的关闭码 = %d，参照实现是 4003", CloseCodeAuthFailed)
	}
	if AuthCloseReasonTimeoutOrInvalidMessage != "auth timeout or invalid message" {
		t.Fatalf("reason 文本不一致: %q", AuthCloseReasonTimeoutOrInvalidMessage)
	}
	if AuthCloseReasonFailed != "auth failed" {
		t.Fatalf("reason 文本不一致: %q", AuthCloseReasonFailed)
	}

	// 非合法 JSON -> 4001
	conn := newFakeConn("text", "{oops")
	bus := New()
	if ok, err := bus.Authenticate(context.Background(), conn, okVerifier); ok || err != nil {
		t.Fatalf("坏 JSON 应返回 (false, nil)，实际 (%v, %v)", ok, err)
	}
	if closed := conn.lastClose(); closed == nil ||
		closed.Code != CloseCodeAuthTimeoutOrInvalidMessage ||
		closed.Reason != AuthCloseReasonTimeoutOrInvalidMessage {
		t.Fatalf("坏 JSON 的关闭不正确: %+v", closed)
	}

	// 合法首帧但 verify 不通过 -> 4003
	conn = newFakeConn("text", `{"type": "auth", "token": "nope"}`)
	bus = New()
	reject := func(string) (bool, error) { return false, nil }
	if ok, err := bus.Authenticate(context.Background(), conn, reject); ok || err != nil {
		t.Fatalf("鉴权失败应返回 (false, nil)，实际 (%v, %v)", ok, err)
	}
	if closed := conn.lastClose(); closed == nil ||
		closed.Code != CloseCodeAuthFailed || closed.Reason != AuthCloseReasonFailed {
		t.Fatalf("鉴权失败的关闭不正确: %+v", closed)
	}
}

// TestVerifyIsNotCalledForShortCircuitFrames 锁定「type/token 不合格时不调用 verify」。
//
// 参照实现的 `and` 短路（event_bus.py:44-48）意味着注入的鉴权钩子不会被无效帧调用；
// 反过来（先把 token 交给 verify）会让宿主的审计/计费多出假请求。
func TestVerifyIsNotCalledForShortCircuitFrames(t *testing.T) {
	frames := []string{
		`{"type": "hello", "token": "local-key"}`,
		`{"type": 1, "token": "local-key"}`,
		`{"type": "auth", "token": 1}`,
		`{"type": "auth", "token": null}`,
		`{"type": "auth"}`,
		`{"type": "auth", "token": true}`,
	}
	for _, frame := range frames {
		conn := newFakeConn("text", frame)
		bus := New()
		calls := 0
		verify := func(string) (bool, error) { calls++; return true, nil }
		if _, err := bus.Authenticate(context.Background(), conn, verify); err != nil {
			t.Fatalf("%s: 意外错误 %v", frame, err)
		}
		if calls != 0 {
			t.Fatalf("%s: verify 被调用了 %d 次，参照实现的短路不会调用它", frame, calls)
		}
		if closed := conn.lastClose(); closed == nil || closed.Code != CloseCodeAuthFailed {
			t.Fatalf("%s: 应关闭 4003，实际 %+v", frame, closed)
		}
	}
}

// TestNonObjectAuthFramePropagatesWithoutClosing 记录一处**已知分歧**。
//
// 首帧是合法 JSON 但不是对象（null/[]/1/"x"）时，参照实现在 `message.get("token")`
// 上抛 AttributeError，而且那行在 try 之外，所以异常一路冒到 ASGI 服务器、
// **连接不会被关闭**（实测：e2e.jsonl 的 e2e_json_null）。Go 侧不关闭连接，改为返回
// ErrUnsupportedAuthFrame，把「怎么处置这条连接」留给调用方。
func TestNonObjectAuthFramePropagatesWithoutClosing(t *testing.T) {
	for _, frame := range []string{"null", "[]", "1", `"auth"`, "1.5", "true"} {
		conn := newFakeConn("text", frame)
		bus := New()
		ok, err := bus.Authenticate(context.Background(), conn, okVerifier)
		if ok {
			t.Fatalf("%s: 不该认证成功", frame)
		}
		if !errors.Is(err, ErrUnsupportedAuthFrame) {
			t.Fatalf("%s: 应返回 ErrUnsupportedAuthFrame，实际 %v", frame, err)
		}
		if conn.lastClose() != nil {
			t.Fatalf("%s: 参照实现在这一支不关连接，Go 侧却关了 %+v", frame, conn.lastClose())
		}
		if bus.ClientCount() != 0 {
			t.Fatalf("%s: 不该加入订阅列表", frame)
		}
	}
}

// TestVerifierErrorPropagatesWithoutClosing 记录第二处**已知分歧**。
//
// `await verify(token)` 也在 try 之外（event_bus.py:44-48），因此 verify 抛异常时
// 参照实现既不关 4003 也不关 4001，而是冒泡。Go 侧把该错误原样返回给调用方。
func TestVerifierErrorPropagatesWithoutClosing(t *testing.T) {
	boom := errors.New("verifier exploded")
	conn := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	bus := New()
	ok, err := bus.Authenticate(context.Background(), conn, func(string) (bool, error) {
		return false, boom
	})
	if ok {
		t.Fatal("不该认证成功")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("应把 verify 的错误冒泡出来，实际 %v", err)
	}
	if conn.lastClose() != nil {
		t.Fatalf("参照实现在这一支不关连接，Go 侧却关了 %+v", conn.lastClose())
	}
}

// TestBinaryFrameIsRejectedAsInvalidMessage 锁定二进制首帧 -> 4001。
//
// 参照实现的 receive_text() 只取 ASGI 消息的 text 键，二进制帧要么 KeyError 要么给出
// None，两者都落 4001（e2e.jsonl 的 e2e_binary_frame 由真服务器确认）。
func TestBinaryFrameIsRejectedAsInvalidMessage(t *testing.T) {
	conn := newFakeConn("binary", "")
	bus := New()
	ok, err := bus.Authenticate(context.Background(), conn, okVerifier)
	if ok || err != nil {
		t.Fatalf("二进制首帧应返回 (false, nil)，实际 (%v, %v)", ok, err)
	}
	if closed := conn.lastClose(); closed == nil ||
		closed.Code != CloseCodeAuthTimeoutOrInvalidMessage {
		t.Fatalf("二进制首帧应关 4001，实际 %+v", closed)
	}
}

// TestNoSnapshotBuiltWithoutClients 锁定「无客户端零开销」。
//
// 这一条是性能要求而不是行为要求：没订阅者时**根本不调用** SnapshotFunc（app.py:122
// 的 `if app.state.event_bus.client_count > 0`）。循环本身照旧转、照旧睡 1 秒。
func TestNoSnapshotBuiltWithoutClients(t *testing.T) {
	builds := 0
	bus := New()
	broadcaster := &Broadcaster{
		Bus: bus,
		Snapshot: func(context.Context) (*canonical.Value, error) {
			builds++
			return canonical.NewObject(), nil
		},
	}
	ctx := context.Background()

	// 走满 100 秒虚拟时间，并在 5 秒处放一次「指标写入」：空闲心跳与 dirty 唤醒
	// 都会发生，但一次都不该构建快照。
	broadcaster.Tick(ctx, 5*time.Second, []time.Duration{5 * time.Second})
	broadcaster.Tick(ctx, 100*time.Second, nil)
	if builds != 0 {
		t.Fatalf("没有订阅者却构建了 %d 次快照", builds)
	}
	if bus.ClientCount() != 0 {
		t.Fatalf("订阅者数应为 0，实际 %d", bus.ClientCount())
	}

	// 有订阅者之后必须真的构建。
	conn := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	if _, err := bus.Authenticate(ctx, conn, okVerifier); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	broadcaster.Tick(ctx, 200*time.Second, nil)
	if builds == 0 {
		t.Fatal("有订阅者却一次都没构建快照")
	}
}

// TestSnapshotThrottleAtMostOncePerSecond 锁定「≤1 次/秒」（app.py:124 的 sleep(1.0)）。
//
// 这条规则与写入频率无关：**连续**写入也只能一秒一次（sleep 在每轮循环末尾无条件执行）。
func TestSnapshotThrottleAtMostOncePerSecond(t *testing.T) {
	loop := Loop{}
	// 同一瞬间灌入 50 次写入：只会唤醒一次，然后必须进入 1 秒的 sleep。
	var dirty []time.Duration
	for index := 0; index < 50; index++ {
		dirty = append(dirty, 0)
	}
	fired := loop.Advance(0, dirty, 1)
	if len(fired) != 1 || fired[0] != 0 {
		t.Fatalf("50 次同一瞬间的写入应只广播 1 次（t=0），实际 %v", fired)
	}

	// 持续每秒写入：广播时刻必须两两间隔 ≥ MinInterval。
	loop = Loop{}
	var all []time.Duration
	for second := 1; second <= 20; second++ {
		at := time.Duration(second) * time.Second
		all = append(all, loop.Advance(at, []time.Duration{at}, 1)...)
	}
	if len(all) < 2 {
		t.Fatalf("20 秒的持续写入应产生多次广播，实际 %v", all)
	}
	for index := 1; index < len(all); index++ {
		if gap := all[index] - all[index-1]; gap < BroadcastMinInterval {
			t.Fatalf("第 %d 次广播间隔 %v 小于 %v（时刻表 %v）",
				index, gap, BroadcastMinInterval, all)
		}
	}
}

// TestIdleHeartbeatIntervalIncludesTrailingSleep 锁定空闲心跳的实际节奏。
//
// 冻结契约说的是「≥1 次/30 秒」，而参照实现的 **timeout 是 30 秒**
// （app.py:103），循环末尾的 sleep(1.0)（app.py:124）无条件执行，所以两次空闲广播
// 之间实际是 31 秒。这条测试把这个「看起来多出来的 1 秒」固定下来，免得有人按
// 「30 秒一次」去改代码。
func TestIdleHeartbeatIntervalIncludesTrailingSleep(t *testing.T) {
	if IdleBroadcastInterval != 30*time.Second {
		t.Fatalf("IdleBroadcastInterval = %v，参照实现是 30.0", IdleBroadcastInterval)
	}
	if BroadcastMinInterval != time.Second {
		t.Fatalf("BroadcastMinInterval = %v，参照实现是 1.0", BroadcastMinInterval)
	}
	loop := Loop{}
	fired := loop.Advance(100*time.Second, nil, 1)
	want := []time.Duration{30 * time.Second, 61 * time.Second, 92 * time.Second}
	if len(fired) != len(want) {
		t.Fatalf("空闲 100 秒的广播时刻 = %v，期望 %v", fired, want)
	}
	for index := range want {
		if fired[index] != want[index] {
			t.Fatalf("空闲 100 秒的广播时刻 = %v，期望 %v", fired, want)
		}
	}
}

// TestConnectedFrameIsCompactWhileBroadcastIsSpaced 记录发送 connected 时的分隔符差异。
//
// app.py:342 用 `websocket.send_json(...)`（Starlette 的 JSONResponse，紧凑分隔符），
// 而 event_bus.py:71 用 `json.dumps(...)` 的**默认**分隔符（带空格）。
func TestConnectedFrameIsCompactWhileBroadcastIsSpaced(t *testing.T) {
	if got := ConnectedFrame(); got != `{"type":"connected","data":{}}` {
		t.Fatalf("connected 帧文本 = %q", got)
	}
	if got := Encode(EventConnected, ConnectedData()); got != `{"type": "connected", "data": {}}` {
		t.Fatalf("广播形式的 connected 帧文本 = %q", got)
	}
}

// TestBroadcastFrameUsesPythonDefaultSeparators 逐字节锁定成帧形式。
func TestBroadcastFrameUsesPythonDefaultSeparators(t *testing.T) {
	cases := []struct {
		eventType string
		data      *canonical.Value
		want      string
	}{
		{EventClientCount, ClientCountData(7), `{"type": "client_count", "data": {"count": 7}}`},
		{EventConfigChange, ConfigChangeData(), `{"type": "config_change", "data": {"reloaded": true}}`},
		{EventMetricsSnapshot, nil, `{"type": "metrics_snapshot", "data": null}`},
		{EventConnected, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "列表", Value: canonical.NewArray(
				canonical.NewString("a"), canonical.NewNull())},
		), `{"type": "connected", "data": {"列表": ["a", null]}}`},
	}
	for _, item := range cases {
		if got := Encode(item.eventType, item.data); got != item.want {
			t.Errorf("Encode(%s) = %q，期望 %q", item.eventType, got, item.want)
		}
	}
}

// TestEventTypeConstants 锁定四个事件类型的字面量。
//
// 它们不是 event_bus.py 里的常量，而是 app.py 的调用点字面量（342/130/109/475），
// 因此没有别的机制保证它们不被改错。
func TestEventTypeConstants(t *testing.T) {
	want := map[string]string{
		EventConnected:       "connected",
		EventClientCount:     "client_count",
		EventMetricsSnapshot: "metrics_snapshot",
		EventConfigChange:    "config_change",
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("事件类型常量 = %q，期望 %q", got, expected)
		}
	}
}

// TestClientCountCallbackRunsOutsideLock 验证回调里可以反过来广播。
//
// 参照实现在 `async with self._lock` **之后**才 await 回调（event_bus.py:53-58），
// 而回调（app.py:129-132）会立刻 broadcast 一次。Go 的 sync.Mutex 不可重入，
// 锁内调用回调会直接死锁——所以这条测试用「回调里再 Broadcast/Disconnect」来钉住
// 「回调在锁外执行」。
func TestClientCountCallbackRunsOutsideLock(t *testing.T) {
	bus := New()
	conn := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	bus.OnClientCountChange = func(count int) {
		bus.Broadcast(context.Background(), EventClientCount, ClientCountData(count))
		if count == 1 {
			// 断开也必须能重入（它会再次触发回调）。
			bus.Disconnect(conn)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := bus.Authenticate(context.Background(), conn, okVerifier); err != nil {
			t.Errorf("认证失败: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("回调里重入 Broadcast/Disconnect 死锁了")
	}

	// 只应收到一条：Disconnect 会先把连接移出集合，回调里的广播自然轮不到它
	// （参照实现同理：discard 之后才 await 回调）。
	if len(conn.sent) != 1 || !strings.Contains(conn.sent[0], `{"count": 1}`) {
		t.Fatalf("应收到一条 count=1 的计数事件，实际 %v", conn.sent)
	}
}

// TestBroadcastPrunesStaleClients 验证发送失败的连接被剔除。
//
// 参照实现只在**整轮之后**统一 discard（event_bus.py:84-87），并且不调用
// on_client_count_change——回调只在 authenticate/disconnect 里触发。
func TestBroadcastPrunesStaleClients(t *testing.T) {
	bus := New()
	callbacks := []int{}
	bus.OnClientCountChange = func(count int) { callbacks = append(callbacks, count) }

	live := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	dead := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	dead.failSend = true
	for _, conn := range []*fakeConn{live, dead} {
		if _, err := bus.Authenticate(context.Background(), conn, okVerifier); err != nil {
			t.Fatalf("认证失败: %v", err)
		}
	}
	callbacksBefore := len(callbacks)

	bus.Broadcast(context.Background(), EventClientCount, ClientCountData(2))
	if bus.ClientCount() != 1 {
		t.Fatalf("失效连接应被剔除，剩余 %d", bus.ClientCount())
	}
	if len(callbacks) != callbacksBefore {
		t.Fatalf("剔除失效连接不该触发计数回调: %v", callbacks)
	}

	// 剔除之后，失效连接不会再收到帧。
	dead.sent = nil
	bus.Broadcast(context.Background(), EventClientCount, ClientCountData(1))
	if len(dead.sent) != 0 {
		t.Fatal("失效连接不该继续收到帧")
	}
}

// TestConfigChangeOnlyBroadcastWhenSubscribed 锁定 config_change 的门禁。
//
// app.py:473-475 在调用点判 `client_count > 0`：没人订阅时连帧都不构造。
func TestConfigChangeOnlyBroadcastWhenSubscribed(t *testing.T) {
	bus := New()
	bus.BroadcastConfigChange(context.Background())

	conn := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	if _, err := bus.Authenticate(context.Background(), conn, okVerifier); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	conn.sent = nil
	bus.BroadcastConfigChange(context.Background())
	want := []string{`{"type": "config_change", "data": {"reloaded": true}}`}
	assertSame(t, "config_change", want, conn.sent)
}

// TestDisconnectNotifiesEvenWhenNeverAuthenticated 验证 disconnect 的幂等通知。
//
// 参照实现的 disconnect 不检查连接是否在集合里（discard 幂等），但计数回调**每次
// 都会调用**（event_bus.py:61-67）。
func TestDisconnectNotifiesEvenWhenNeverAuthenticated(t *testing.T) {
	bus := New()
	callbacks := []int{}
	bus.OnClientCountChange = func(count int) { callbacks = append(callbacks, count) }
	bus.Disconnect(newFakeConn("text", ""))
	if len(callbacks) != 1 || callbacks[0] != 0 {
		t.Fatalf("从未认证的连接断开也应通知一次 count=0，实际 %v", callbacks)
	}
}

// TestOnClientCountChangeBroadcastsSnapshotImmediately 锁定回调里的第二条广播。
//
// app.py:129-132：先广播 client_count，count>0 时**立刻**再广播一次 metrics_snapshot。
// 这一条**不经过节流器**，因此新连接的客户端会先看到 client_count + metrics_snapshot，
// 然后才是路由补发的 connected（e2e.jsonl 的 e2e_valid_token 观测到的顺序）。
func TestOnClientCountChangeBroadcastsSnapshotImmediately(t *testing.T) {
	bus := New()
	broadcaster := &Broadcaster{Bus: bus, Snapshot: emptySnapshot}
	bus.OnClientCountChange = func(count int) {
		broadcaster.OnClientCountChange(context.Background(), count)
	}
	observer := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	if _, err := bus.Authenticate(context.Background(), observer, okVerifier); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	assertSame(t, "回调广播顺序", []string{EventClientCount, EventMetricsSnapshot},
		frameTypesOf(observer.sent))

	// 第二个连接断开：留下的那个应当仍收到 client_count + metrics_snapshot。
	// 注意 `count > 0` 是**订阅者数**而不是「有人离开」：只要还有订阅者，每次计数
	// 变化都会补一次快照；真正降到 0 的那一次虽然也广播，但已经没人收得到。
	other := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	if _, err := bus.Authenticate(context.Background(), other, okVerifier); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	observer.sent = nil
	bus.Disconnect(other)
	assertSame(t, "断开后剩余订阅者的广播", []string{EventClientCount, EventMetricsSnapshot},
		frameTypesOf(observer.sent))
}

// TestBroadcasterDropsSnapshotOnError 验证快照构建失败只记日志，不影响循环。
//
// 对应 app.py:105-111 的 try/except：快照失败时那一轮什么都不广播。
func TestBroadcasterDropsSnapshotOnError(t *testing.T) {
	bus := New()
	conn := newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
	if _, err := bus.Authenticate(context.Background(), conn, okVerifier); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	conn.sent = nil
	broadcaster := &Broadcaster{
		Bus: bus,
		Snapshot: func(context.Context) (*canonical.Value, error) {
			return nil, errors.New("metrics 挂了")
		},
	}
	if builds := broadcaster.Tick(context.Background(), 30*time.Second, nil); builds != 1 {
		t.Fatalf("应尝试构建一次快照，实际 %d", builds)
	}
	if len(conn.sent) != 0 {
		t.Fatalf("快照失败时不该广播任何帧，实际 %v", conn.sent)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试替身
// ─────────────────────────────────────────────────────────────────────────────

// closeRecord 是假连接观测到的一次关闭。
type closeRecord struct {
	Code   int
	Reason string
}

// fakeConn 是脚本化的 Conn，覆盖 ReadText 的四类结局。
type fakeConn struct {
	frameKind string
	frame     string
	failSend  bool
	closed    []closeRecord
	sent      []string
	unblock   chan struct{}
	once      sync.Once
}

// newFakeConn 按帧类型与帧文本构造假连接。
func newFakeConn(frameKind, frame string) *fakeConn {
	return &fakeConn{frameKind: frameKind, frame: frame, unblock: make(chan struct{})}
}

func (c *fakeConn) ReadText(ctx context.Context) (string, error) {
	switch c.frameKind {
	case "binary":
		// 参照实现在二进制帧上取不到 text（KeyError 或 None），两条路都是 4001。
		return "", ErrBinaryFrame
	case "disconnect":
		return "", errors.New("对端已断开")
	case "timeout":
		select {
		case <-c.unblock:
			return "", errors.New("连接已关闭")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	default:
		return c.frame, nil
	}
}

func (c *fakeConn) SendText(_ context.Context, text string) error {
	if c.failSend {
		return errors.New("发送失败")
	}
	c.sent = append(c.sent, text)
	return nil
}

func (c *fakeConn) Close(code int, reason string) {
	c.closed = append(c.closed, closeRecord{Code: code, Reason: reason})
	if c.unblock != nil {
		c.once.Do(func() { close(c.unblock) })
	}
}

func (c *fakeConn) lastClose() *closeRecord {
	if len(c.closed) == 0 {
		return nil
	}
	return &c.closed[len(c.closed)-1]
}

// okVerifier 是恒真的 token 校验器。
func okVerifier(string) (bool, error) { return true, nil }

// assertSame 比较期望与实际，失败时把两边都打成 JSON 便于定位。
func assertSame(t *testing.T, name string, want, got any) {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	t.Errorf("%s 不一致\n期望: %s\n实际: %s", name, wantJSON, gotJSON)
}

// emptySnapshot 是拨测用的空快照。
func emptySnapshot(context.Context) (*canonical.Value, error) {
	return canonical.NewObject(), nil
}

// frameTypesOf 从广播帧文本里取出 type 字段序列。
func frameTypesOf(frames []string) []string {
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		value, err := canonical.ParseString(frame)
		if err != nil {
			types = append(types, "<not-json>")
			continue
		}
		types = append(types, value.Lookup("type").StringValue())
	}
	return types
}
