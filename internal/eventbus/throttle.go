package eventbus

import (
	"context"
	"log/slog"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// IdleBroadcastInterval 是空闲心跳间隔，对应 app.py:103 的
// `_IDLE_BROADCAST_INTERVAL = 30.0`。
//
// 它约束的是 `wait_for(_metrics_dirty.wait(), timeout=...)` 的**超时值**。注意
// 广播之间的实际间隔不止 30 秒：app.py:124 的 `sleep(1.0)` 在**每轮循环末尾**
// 无条件执行，因此「无人写入指标」时的广播间隔是 30 + 1 = **31 秒**。这是参照
// 实现的真实行为（迁移计划里「≥1 次/30 秒」的近似说法），由
// TestIdleHeartbeatIntervalIncludesTrailingSleep 锁定。
const IdleBroadcastInterval = 30 * time.Second

// BroadcastMinInterval 是两次广播之间的最小间隔，对应 app.py:124 的
// `await asyncio.sleep(1.0)`，即「≤1 次/秒」。
const BroadcastMinInterval = time.Second

// Loop 是 app.py:113-124 那个循环体的**确定性**版本。
//
// 参照实现的循环体是：
//
//	await asyncio.wait_for(_metrics_dirty.wait(), timeout=30.0)   # 等写入 或 30s 超时
//	_metrics_dirty.clear()
//	if client_count > 0: await _broadcast_metrics_snapshot()      # 零开销门禁
//	await asyncio.sleep(1.0)                                      # ≤1 次/秒
//
// 这三条正好是迁移计划 §4.6 frozen 的节流规则。为了能对拍，这里把它写成状态机：
// 时间由调用方以「虚拟时钟」推进（Advance），等待与睡眠变成可判定的唤醒时刻。
//
// 使用方式（未来的装配任务照此写生产驱动器）：
//
//	start := time.Now()
//	dirty := make(chan time.Duration, 1)        // 指标写入回调里非阻塞发送 time.Since(start)
//	loop := eventbus.Loop{}
//	for {
//	    var wake []time.Duration
//	    select {
//	    case at := <-dirty: wake = append(wake, at)
//	    case <-time.After(eventbus.IdleBroadcastInterval):
//	    }
//	    broadcaster.Tick(ctx, time.Since(start), wake)
//	    time.Sleep(eventbus.BroadcastMinInterval)
//	}
//
// 虚拟时钟的原点必须与「循环启动时刻」一致（上面的 start），否则第一轮的 30 秒
// 超时会落在错误的时刻。
type Loop struct {
	// IdleInterval 为 0 时取 IdleBroadcastInterval。
	IdleInterval time.Duration
	// MinInterval 为 0 时取 BroadcastMinInterval。
	MinInterval time.Duration

	started   bool
	sleepTill time.Duration
	pending   []time.Duration
	lastNow   time.Duration
}

// Advance 把虚拟时钟推进到 now，执行所有已经到期的循环迭代，返回**真正广播了
// metrics_snapshot** 的时刻。
//
// dirtyAts 是 (上次调用, now] 区间内「指标写入」的时刻（升序；对应 app.py:126-127
// 的 `_metrics_dirty.set()`）。允许重复，也允许早于上次调用（会被夹到上次调用，
// 因为时钟必须单调）。
//
// 返回值只包含 count > 0 的唤醒：没人订阅时循环照样转、照样睡 1 秒，但**不构建
// 快照**（app.py:122），所以对调用方而言那些时刻没有事件发生。
func (l *Loop) Advance(now time.Duration, dirtyAts []time.Duration, clientCount int) []time.Duration {
	if l.IdleInterval <= 0 {
		l.IdleInterval = IdleBroadcastInterval
	}
	if l.MinInterval <= 0 {
		l.MinInterval = BroadcastMinInterval
	}
	if !l.started {
		l.started = true
		// 虚拟时钟原点 = 循环启动时刻。参照实现刚进循环就进入 wait()，所以
		// 第一个超时点就是 IdleInterval。
		l.sleepTill = 0
		l.lastNow = 0
	}
	if now < l.lastNow {
		now = l.lastNow
	}
	for _, at := range dirtyAts {
		if at < l.lastNow {
			at = l.lastNow
		}
		l.pending = append(l.pending, at)
	}

	var fired []time.Duration
	for {
		if l.sleepTill > now {
			// 还在 sleep(1.0) 里——asyncio 的循环体不会在中途被唤醒。
			break
		}
		timeout := l.sleepTill + l.IdleInterval
		wake := timeout
		hadDirty := false
		if len(l.pending) > 0 {
			// 已经置位的 Event 让 wait() 立刻返回（即使信号是在 sleep 期间到达的，
			// Event 的置位状态会保留到下一次 wait）；否则等到最早的那次写入。
			candidate := l.pending[0]
			if candidate < l.sleepTill {
				candidate = l.sleepTill
			}
			if candidate < timeout {
				wake = candidate
				hadDirty = true
			}
		}
		if wake > now {
			break
		}
		if hadDirty {
			// 对应 event_bus 循环里的 _metrics_dirty.clear()：只清掉已经消费的信号，
			// 唤醒之后才到达的信号留给下一轮（那时 wait() 会立刻返回）。
			for len(l.pending) > 0 && l.pending[0] <= wake {
				l.pending = l.pending[1:]
			}
		}
		l.sleepTill = wake + l.MinInterval
		if clientCount > 0 {
			fired = append(fired, wake)
		}
	}
	l.lastNow = now
	return fired
}

// SnapshotFunc 构建一次指标快照，对应 app.py:107-108 的
// `metrics.snapshot(since=metrics._started_at)`。
//
// 它被刻意放在调用方：**没人订阅时根本不会被调用**，这才是「零开销」的落点。
type SnapshotFunc func(ctx context.Context) (*canonical.Value, error)

// Broadcaster 把节流器、总线与快照构建接起来，对应 app.py:105-135 的三个回调。
type Broadcaster struct {
	// Bus 是事件总线；Tick 会先读它的 ClientCount 作为零开销门禁。
	Bus *Bus
	// Loop 是节流状态机；零值即取默认的 30 秒 / 1 秒。
	Loop Loop
	// Snapshot 构建快照。为 nil 时 Tick 什么都不广播（便于只测时序）。
	Snapshot SnapshotFunc
	// Logger 为 nil 时不记录快照构建失败。
	Logger *slog.Logger
}

// Tick 推进节流器并广播到期的 metrics_snapshot，返回真正广播的次数。
//
// 与 app.py:122 一致：订阅者数为 0 时**不构建快照**。快照构建失败只记日志
// （app.py:110-111 的 try/except），不影响循环继续。
func (b *Broadcaster) Tick(ctx context.Context, now time.Duration, dirtyAts []time.Duration) int {
	fired := b.Loop.Advance(now, dirtyAts, b.count())
	for range fired {
		b.broadcastSnapshot(ctx)
	}
	return len(fired)
}

// OnClientCountChange 是订阅者数变化时的回调，对应 app.py:129-132。
//
// 两件事，顺序即契约：先广播 client_count；count > 0 时**立刻**再广播一次
// metrics_snapshot——这一条**不经过节流器**（它不在循环体里，是回调里直接
// await 的）。参照实现如此，所以新客户端连上时会先收到 client_count +
// metrics_snapshot，然后才是路由补发的 connected（app.py:339-342）。
//
// 用法（装配时绑定 ctx）：
//
//	bus.OnClientCountChange = func(count int) { broadcaster.OnClientCountChange(ctx, count) }
func (b *Broadcaster) OnClientCountChange(ctx context.Context, count int) {
	b.Bus.Broadcast(ctx, EventClientCount, ClientCountData(count))
	if count > 0 {
		b.broadcastSnapshot(ctx)
	}
}

func (b *Broadcaster) broadcastSnapshot(ctx context.Context) {
	if b.Snapshot == nil {
		return
	}
	snapshot, err := b.Snapshot(ctx)
	if err != nil {
		b.log().Debug("metrics_snapshot broadcast failed", "error", err)
		return
	}
	b.Bus.Broadcast(ctx, EventMetricsSnapshot, snapshot)
}

func (b *Broadcaster) count() int {
	if b.Bus == nil {
		return 0
	}
	return b.Bus.ClientCount()
}

func (b *Broadcaster) log() *slog.Logger {
	if b.Logger == nil {
		return slog.New(discardHandler{})
	}
	return b.Logger
}

// ConnectedData 构造 connected 事件的 data，对应 app.py:342 的空对象。
func ConnectedData() *canonical.Value { return canonical.NewObject() }

// ConnectedFrame 是鉴权通过后由调用方补发的首帧文本，对应 app.py:342 的
// `websocket.send_json({"type": "connected", "data": {}})`。
//
// **分隔符与 Broadcast 的帧不同**，这一点由真实服务器观测确认
// （testdata/e2e.jsonl 的 connected_frame）：`send_json` 走 Starlette 的
// JSONResponse，用紧凑分隔符 `{"type":"connected","data":{}}`；而 event_bus.py:71 的
// `json.dumps` 用默认分隔符（`{"type": "connected", "data": {}}`，带空格）。
// 客户端如果按字符串前缀匹配事件名，两种形式都能解析；但逐字节比对时必须区分。
func ConnectedFrame() string {
	return canonical.DumpsOrdered(canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString(EventConnected)},
		canonical.ObjectPair{Key: "data", Value: ConnectedData()},
	))
}

// ClientCountData 构造 client_count 事件的 data，对应 app.py:130 的 {"count": count}。
func ClientCountData(count int) *canonical.Value {
	return canonical.NewObjectOf(canonical.ObjectPair{
		Key:   "count",
		Value: canonical.NewIntValue(int64(count)),
	})
}

// ConfigChangeData 构造 config_change 事件的 data，对应 app.py:475 的
// {"reloaded": True}。
func ConfigChangeData() *canonical.Value {
	return canonical.NewObjectOf(canonical.ObjectPair{
		Key:   "reloaded",
		Value: canonical.NewBool(true),
	})
}

// BroadcastConfigChange 广播配置热重载事件，对应 app.py:473-475。
//
// 参照实现在调用点自己判了 `if event_bus.client_count > 0`——广播前先看有没有人，
// 没有就直接返回（连帧都不构造）。这里把这个门禁收进方法里，避免第二个调用点
// 忘了判。
func (b *Bus) BroadcastConfigChange(ctx context.Context) {
	if b.ClientCount() == 0 {
		return
	}
	b.Broadcast(ctx, EventConfigChange, ConfigChangeData())
}
