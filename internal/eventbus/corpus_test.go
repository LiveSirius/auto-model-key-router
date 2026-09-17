package eventbus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// testdataDir 是差分语料目录；语料由 scripts/gen_websocket_corpus.py 驱动**真实的
// Python event_bus.py / app.py** 产出。期望值不是手写的：auth.jsonl 与 e2e.jsonl
// 来自真实 EventBus 与真实 FastAPI 应用，broadcast.jsonl 是真实 broadcast 发出的
// 字节，throttle.jsonl 来自 app.py:113-124 循环体的虚拟时钟转写。
const testdataDir = "testdata"

// corpusLine 是语料行的通用外壳（四种 kind 共用一个结构，未用到的字段为零值）。
type corpusLine struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Note        string  `json:"note"`
	FrameKind   string  `json:"frame_kind"`
	Frame       *string `json:"frame"`
	Verify      string  `json:"verify"`
	AuthTimeout float64 `json:"auth_timeout"`

	EventType string `json:"event_type"`
	DataJSON  string `json:"data_json"`
	Clients   int    `json:"clients"`

	IdleInterval float64        `json:"idle_interval"`
	MinInterval  float64        `json:"min_interval"`
	Until        float64        `json:"until"`
	Steps        []throttleStep `json:"steps"`

	Send *struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		B64  string `json:"b64"`
	} `json:"send"`

	Expect string `json:"expect"`
}

// throttleStep 对应 Loop.Advance 的一次调用。
type throttleStep struct {
	Now     float64   `json:"now"`
	Count   int       `json:"count"`
	DirtyAt []float64 `json:"dirty_at"`
}

// loadCorpus 逐行读取 JSONL 语料。
func loadCorpus(t *testing.T, name string) []corpusLine {
	t.Helper()
	path := filepath.Join(testdataDir, name)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开语料 %s: %v", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []corpusLine
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var line corpusLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("%s: 解析语料行失败: %v", path, err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s: 读取语料失败: %v", path, err)
	}
	if len(lines) == 0 {
		t.Fatalf("%s: 语料为空", path)
	}
	return lines
}

// decodeExpect 把 expect（紧凑 JSON 文本）解成给定类型。
func decodeExpect(t *testing.T, expect string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(expect), target); err != nil {
		t.Fatalf("解析 expect 失败: %v (%s)", err, expect)
	}
}

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

// micros 把语料里的秒数转成微秒精度的 Duration。
//
// 不能直接 time.Duration(seconds * float64(time.Second))：1.2 这类十进制小数在
// float64 里是近似值，乘 1e9 会掉到 1199999999ns，与语料里的 1.2 不再相等。语料里
// 的时间都精确到微秒，因此按微秒取整。
func micros(seconds float64) time.Duration {
	return time.Duration(math.Round(seconds*1e6)) * time.Microsecond
}

// microsOf 把 Duration 转回微秒整数。
func microsOf(d time.Duration) int64 { return int64(d / time.Microsecond) }

// appendIfFired 追加一次唤醒时刻。
func appendIfFired(fired []int64, at time.Duration) []int64 {
	return append(fired, microsOf(at))
}

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

// newFakeConn 按语料行构造假连接。
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

// authOutcome 是 Go 侧一次鉴权握手的可观测结果。
type authOutcome struct {
	Returned      bool
	Authenticated bool
	Err           error
	Close         *closeRecord
	VerifyCalls   []string
	ClientCount   int
	Callbacks     []int
}

// authExpectation 与 auth.jsonl 的 expect 同形。
type authExpectation struct {
	Outcome struct {
		Result        string `json:"result"`
		Authenticated bool   `json:"authenticated"`
		Exception     string `json:"exception"`
		Message       string `json:"message"`
	} `json:"outcome"`
	CloseCode   *int     `json:"close_code"`
	CloseReason *string  `json:"close_reason"`
	VerifyCalls []string `json:"verify_calls"`
	ClientCount int      `json:"client_count"`
	Callbacks   []int    `json:"callbacks"`
}

// runAuthCase 跑一次握手，只收集可观测结果，不做断言。
func runAuthCase(line corpusLine) (*fakeConn, authOutcome) {
	conn := newFakeConn(line.FrameKind, derefString(line.Frame))
	bus := New()
	callbacks := []int{}
	bus.OnClientCountChange = func(count int) { callbacks = append(callbacks, count) }
	verifyCalls := []string{}
	verify := func(token string) (bool, error) {
		verifyCalls = append(verifyCalls, token)
		if line.Verify == "raise" {
			return false, errors.New("verifier exploded")
		}
		return line.Verify == "ok", nil
	}

	authenticated, err := bus.Authenticate(context.Background(), conn, verify)
	return conn, authOutcome{
		Returned:      err == nil,
		Authenticated: authenticated,
		Err:           err,
		Close:         conn.lastClose(),
		VerifyCalls:   verifyCalls,
		ClientCount:   bus.ClientCount(),
		Callbacks:     callbacks,
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// TestAuthCorpus 重放鉴权握手语料。
//
// 覆盖冻结契约里的两条：首帧必须是 {"type":"auth","token":...} 且有 10 秒上限，
// 以及失败时的关闭码 4001 / 4003。语料还锁住两个容易漏的细节：type/token 不合格时
// verify **不被调用**（短路），首帧不是 JSON 对象时参照实现**不关连接**而是抛异常。
func TestAuthCorpus(t *testing.T) {
	for _, line := range loadCorpus(t, "auth.jsonl") {
		line := line
		t.Run(line.Name, func(t *testing.T) {
			if line.FrameKind == "timeout" {
				if testing.Short() {
					t.Skip("-short：10 秒超时用例跳过")
				}
				if line.AuthTimeout != AuthTimeout.Seconds() {
					t.Fatalf("语料里的 timeout 字面量 %v 与 AuthTimeout %v 不符",
						line.AuthTimeout, AuthTimeout)
				}
			}
			var want authExpectation
			decodeExpect(t, line.Expect, &want)

			started := time.Now()
			conn, got := runAuthCase(line)
			elapsed := time.Since(started)
			if line.FrameKind == "timeout" && elapsed < AuthTimeout {
				t.Fatalf("超时路径只等了 %v，应至少等满 AuthTimeout=%v", elapsed, AuthTimeout)
			}

			// 「抛异常」这一类只比较**是否抛出**与后续可观测行为：Python 的
			// AttributeError 与 Go 的 ErrUnsupportedAuthFrame 在类型名上不可能一致
			// ——那正是迁移记录下来的分歧。能对齐的是「不关连接、不加入订阅列表、
			// verify 是否被调用」。
			if want.Outcome.Result == "raised" {
				if got.Returned {
					t.Fatalf("期望抛出错误（%s: %s），实际正常返回",
						want.Outcome.Exception, want.Outcome.Message)
				}
				if got.Close != nil {
					t.Fatalf("参照实现不关连接，Go 侧却关闭了 %+v", *got.Close)
				}
				if want.Outcome.Exception == "AttributeError" && !errors.Is(got.Err, ErrUnsupportedAuthFrame) {
					t.Fatalf("首帧不是 JSON 对象应返回 ErrUnsupportedAuthFrame，实际 %v", got.Err)
				}
			} else {
				if !got.Returned {
					t.Fatalf("期望正常返回，实际抛出 %v", got.Err)
				}
				if got.Authenticated != want.Outcome.Authenticated {
					t.Fatalf("authenticated 不一致：期望 %v，实际 %v",
						want.Outcome.Authenticated, got.Authenticated)
				}
				var closeCode *int
				var closeReason *string
				if got.Close != nil {
					code, reason := got.Close.Code, got.Close.Reason
					closeCode, closeReason = &code, &reason
				}
				assertSame(t, line.Name+"/close_code", want.CloseCode, closeCode)
				assertSame(t, line.Name+"/close_reason", want.CloseReason, closeReason)
			}
			assertSame(t, line.Name+"/verify_calls", want.VerifyCalls, got.VerifyCalls)
			assertSame(t, line.Name+"/callbacks", want.Callbacks, got.Callbacks)
			if got.ClientCount != want.ClientCount {
				t.Fatalf("client_count 不一致：期望 %d，实际 %d", want.ClientCount, got.ClientCount)
			}
			_ = conn
		})
	}
}

// TestBroadcastCorpus 逐字节重放广播帧语料。
//
// 这里锁的是「广播帧用 json.dumps 的默认分隔符（带空格）」这一条：差一个字节不会让
// 任何 JSON 解析器报错，但会让逐字节比对的下游（以及缓存键）失败。
func TestBroadcastCorpus(t *testing.T) {
	for _, line := range loadCorpus(t, "broadcast.jsonl") {
		line := line
		t.Run(line.Name, func(t *testing.T) {
			data, err := canonical.ParseString(line.DataJSON)
			if err != nil {
				t.Fatalf("解析 data_json 失败: %v (%s)", err, line.DataJSON)
			}
			var want []string
			decodeExpect(t, line.Expect, &want)

			bus := New()
			conns := make([]*fakeConn, line.Clients)
			for index := range conns {
				conns[index] = newFakeConn("text", `{"type": "auth", "token": "local-key"}`)
				if _, err := bus.Authenticate(context.Background(), conns[index], okVerifier); err != nil {
					t.Fatalf("认证失败: %v", err)
				}
				conns[index].sent = nil
			}

			bus.Broadcast(context.Background(), line.EventType, data)

			got := []string{}
			for _, conn := range conns {
				got = append(got, conn.sent...)
			}
			if want == nil {
				want = []string{}
			}
			assertSame(t, line.Name, want, got)
		})
	}
}

// TestThrottleCorpus 重放节流语料。
//
// 期望来自 app.py:113-124 循环体的虚拟时钟转写（生成器里的 broadcast_metrics_loop），
// 覆盖三条冻结规则：≤1 次/秒、≥1 次/30 秒、无客户端零开销（那些时刻不出现在 fired 里）。
func TestThrottleCorpus(t *testing.T) {
	for _, line := range loadCorpus(t, "throttle.jsonl") {
		line := line
		t.Run(line.Name, func(t *testing.T) {
			var want struct {
				Fired  []float64 `json:"fired"`
				Builds int       `json:"builds"`
			}
			decodeExpect(t, line.Expect, &want)

			loop := Loop{
				IdleInterval: micros(line.IdleInterval),
				MinInterval:  micros(line.MinInterval),
			}
			var fired []int64
			for _, step := range line.Steps {
				dirty := make([]time.Duration, 0, len(step.DirtyAt))
				for _, at := range step.DirtyAt {
					dirty = append(dirty, micros(at))
				}
				advanced := loop.Advance(micros(step.Now), dirty, step.Count)
				if advanced == nil {
					continue
				}
				for _, at := range advanced {
					fired = appendIfFired(fired, at)
				}
			}
			if fired == nil {
				fired = []int64{}
			}

			expected := make([]int64, 0, len(want.Fired))
			for _, at := range want.Fired {
				expected = append(expected, microsOf(micros(at)))
			}
			assertSame(t, line.Name+"/fired", expected, fired)
			if len(fired) != want.Builds {
				t.Fatalf("builds 不一致：期望 %d，实际 %d", want.Builds, len(fired))
			}
		})
	}
}

// e2eExpectation 与 e2e.jsonl 的 expect 同形。
type e2eExpectation struct {
	FrameTypes     []string `json:"frame_types"`
	ConnectedFrame *string  `json:"connected_frame"`
	Close          *struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	} `json:"close"`
	Raised *struct {
		Exception string `json:"exception"`
		Message   string `json:"message"`
	} `json:"raised"`
	WaitedAtLeastAuthTimeout *bool `json:"waited_at_least_auth_timeout"`
}

// TestE2ECorpusMatchesFakeConnModel 用假连接重放真服务器观测到的输入并比较结果。
//
// e2e.jsonl 由**真实 FastAPI 应用 + TestClient** 产出，作用就是验证「假连接模型 ==
// 真服务器」：同一个首帧，两边必须给出同一个关闭码；认证成功时事件顺序（client_count
// → metrics_snapshot → connected）也必须一致。
func TestE2ECorpusMatchesFakeConnModel(t *testing.T) {
	for _, line := range loadCorpus(t, "e2e.jsonl") {
		line := line
		t.Run(line.Name, func(t *testing.T) {
			var want e2eExpectation
			decodeExpect(t, line.Expect, &want)

			frameKind, frame := "text", ""
			if line.Send != nil {
				frame = line.Send.Text
				if line.Send.Kind == "bytes" {
					frameKind, frame = "binary", ""
				}
			}
			switch {
			case line.Send == nil:
				if testing.Short() {
					t.Skip("-short：真服务器 10 秒超时用例跳过")
				}
				if line.AuthTimeout != AuthTimeout.Seconds() {
					t.Fatalf("真服务器观测到的 timeout 字面量 %v 与 AuthTimeout %v 不符",
						line.AuthTimeout, AuthTimeout)
				}
				replayRealTimeout(t, want)
			case want.Raised != nil || want.Close != nil:
				replayAcceptCase(t, frameKind, frame, want)
			default:
				replayAcceptedCase(t, frame, want)
			}
		})
	}
}

// replayAcceptCase 重放「会走到关闭」的输入。
func replayAcceptCase(t *testing.T, frameKind, frame string, want e2eExpectation) {
	t.Helper()
	conn := newFakeConn(frameKind, frame)
	bus := New()
	verify := func(token string) (bool, error) { return token == "local-key", nil }
	authenticated, err := bus.Authenticate(context.Background(), conn, verify)

	if want.Raised != nil {
		if err == nil {
			t.Fatalf("真服务器抛了 %s，Go 侧却正常返回", want.Raised.Exception)
		}
		if conn.lastClose() != nil {
			t.Fatalf("真服务器没有关闭连接，Go 侧却关了 %+v", conn.lastClose())
		}
		return
	}
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if authenticated {
		t.Fatal("这一支不该认证成功")
	}
	closed := conn.lastClose()
	if closed == nil {
		t.Fatalf("期望关闭码 %d，实际没有关闭", want.Close.Code)
	}
	if closed.Code != want.Close.Code || closed.Reason != want.Close.Reason {
		t.Fatalf("关闭不一致：期望 (%d, %q)，实际 (%d, %q)",
			want.Close.Code, want.Close.Reason, closed.Code, closed.Reason)
	}
}

// replayAcceptedCase 重放认证成功的那一支：比较三帧的事件类型与 connected 帧文本。
func replayAcceptedCase(t *testing.T, frame string, want e2eExpectation) {
	t.Helper()
	bus := New()
	broadcaster := &Broadcaster{Bus: bus, Snapshot: emptySnapshot}
	bus.OnClientCountChange = func(count int) {
		broadcaster.OnClientCountChange(context.Background(), count)
	}
	conn := newFakeConn("text", frame)
	verify := func(token string) (bool, error) { return token == "local-key", nil }
	authenticated, err := bus.Authenticate(context.Background(), conn, verify)
	if err != nil || !authenticated {
		t.Fatalf("应认证成功: err=%v authenticated=%v", err, authenticated)
	}
	// app.py:342 由路由补发 connected。
	if err := conn.SendText(context.Background(), ConnectedFrame()); err != nil {
		t.Fatalf("发送 connected 失败: %v", err)
	}

	assertSame(t, "frame_types", want.FrameTypes, frameTypesOf(conn.sent))
	if want.ConnectedFrame != nil {
		last := conn.sent[len(conn.sent)-1]
		if last != *want.ConnectedFrame {
			t.Fatalf("connected 帧文本不一致：期望 %q，实际 %q", *want.ConnectedFrame, last)
		}
	}
}

// replayRealTimeout 断言真服务器等满了 10 秒才关 4001，并在 Go 侧复现同一结果。
func replayRealTimeout(t *testing.T, want e2eExpectation) {
	t.Helper()
	if want.WaitedAtLeastAuthTimeout == nil || !*want.WaitedAtLeastAuthTimeout {
		t.Fatal("真服务器等满了 10 秒，语料却标记为未等满")
	}
	if AuthTimeout != 10*time.Second {
		t.Fatalf("AuthTimeout 应为 10 秒，实际 %v", AuthTimeout)
	}
	conn := newFakeConn("timeout", "")
	bus := New()
	started := time.Now()
	authenticated, err := bus.Authenticate(context.Background(), conn, okVerifier)
	if err != nil {
		t.Fatalf("超时不应返回错误: %v", err)
	}
	if authenticated {
		t.Fatal("超时不应认证成功")
	}
	if elapsed := time.Since(started); elapsed < AuthTimeout {
		t.Fatalf("只等了 %v，应等满 %v", elapsed, AuthTimeout)
	}
	closed := conn.lastClose()
	if closed == nil || closed.Code != want.Close.Code || closed.Reason != want.Close.Reason {
		t.Fatalf("关闭不一致：期望 (%d, %q)，实际 %+v", want.Close.Code, want.Close.Reason, closed)
	}
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
