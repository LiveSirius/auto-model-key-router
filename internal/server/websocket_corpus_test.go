package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/eventbus"
)

// 本文件回放语料里的两块 WebSocket 用例（由 scripts/gen_server_corpus.py 用
// TestClient.websocket_connect 驱动真实 Python 应用生成）。
//
// 为什么要单独一块语料：ASGI 的 websocket 与 http 是两个 scope，普通 client.request
// 永远匹配不到 websocket 路由，所以握手契约（帧序、4001/4003、关闭帧）只能在
// websocket 连接上对拍。Go 侧用 coder/websocket 起一个真客户端（真实 TCP，不是
// ResponseRecorder——Recorder 不支持 Hijack）。
//
// 与 HTTP 语料同一套归一化规则：帧里的时间戳替换成 <TIMESTAMP>、夹具目录替换成
// <FIXTURE_DIR>；时钟同样用语料的 now_beijing 钉死（否则快照里的窗口时间会漂移）。

// serverCorpusWSCase 是一条 WebSocket 用例。
type serverCorpusWSCase struct {
	name       string
	note       string
	divergence string
	send       *canonical.Value
	url        string
	headers    *canonical.Value
	expect     *canonical.Value
	covers     *canonical.Value
	// upstreamResponse 只有 ws_proxy 用例有：桩上游应当回的 status / content-type /
	// body（Python 侧是 httpx.MockTransport，Go 侧是 httptest 桩）。
	upstreamResponse *canonical.Value
}

// loadWebSocketCorpus 从 canonical 树里取出 ws_cases / ws_proxy_cases。
func loadWebSocketCorpus(t *testing.T, root *canonical.Value, key string) []serverCorpusWSCase {
	t.Helper()
	array := root.Lookup(key)
	if array == nil || !array.IsArray() {
		t.Fatalf("语料缺少 %s", key)
	}
	cases := make([]serverCorpusWSCase, 0, len(array.Arr))
	for index, node := range array.Arr {
		expectText := node.Lookup("expect").StringValue()
		expect, err := canonical.ParseString(expectText)
		if err != nil {
			t.Fatalf("%s[%d] 的 expect 不是合法 JSON: %v", key, index, err)
		}
		cases = append(cases, serverCorpusWSCase{
			name:             node.Lookup("name").StringValue(),
			note:             node.Lookup("note").StringValue(),
			divergence:       node.Lookup("divergence").StringValue(),
			send:             node.Lookup("send"),
			url:              node.Lookup("url").StringValue(),
			headers:          node.Lookup("headers"),
			expect:           expect,
			covers:           node.Lookup("covers"),
			upstreamResponse: node.Lookup("upstream_response"),
		})
	}
	if len(cases) == 0 {
		t.Fatalf("%s 为空", key)
	}
	return cases
}

// wsClose 是客户端观测到的关闭帧。
type wsClose struct {
	code   int
	reason string
}

// wsObservation 是一条 WebSocket 用例在 Go 侧的观测结果。
type wsObservation struct {
	frames []string
	// close 为 nil 表示「在 connected 之后主动停止读取」（与生成脚本一致，那时客户端
	// 还没看到关闭帧）。
	close *wsClose
	// readErr 是停止读取时的错误（connected 之后停止时为 nil）。
	readErr error
}

// observeWebSocket 按生成脚本同样的方式读：发一帧，然后一直读，直到关闭或收到
// connected（认证成功后服务端不会主动关闭，读下去会挂住）。
func observeWebSocket(t *testing.T, conn *websocket.Conn, send *canonical.Value) wsObservation {
	t.Helper()
	if send != nil && !send.IsNull() {
		kind := send.Lookup("kind").StringValue()
		switch kind {
		case "text":
			writeWebSocketFrame(t, conn, send.Lookup("text").StringValue())
		case "bytes":
			t.Fatal("ws 语料尚未使用二进制首帧")
		default:
			t.Fatalf("未知的 send.kind=%q", kind)
		}
	}
	var observation wsObservation
	for {
		frame, err := readWebSocketFrame(t, conn)
		if err != nil {
			code, reason := closeInfo(err)
			if code < 0 {
				observation.readErr = err
			} else {
				observation.close = &wsClose{code: code, reason: reason}
			}
			return observation
		}
		observation.frames = append(observation.frames, frame)
		if frameEventType(t, frame) == eventbus.EventConnected {
			return observation
		}
	}
}

// canonicalStringArray 把 canonical 数组读成 []string。
func canonicalStringArray(t *testing.T, value *canonical.Value) []string {
	t.Helper()
	if value == nil || value.IsNull() {
		return nil
	}
	if !value.IsArray() {
		t.Fatalf("期望数组，实际 %s", canonical.DumpsOrdered(value))
	}
	items := make([]string, 0, len(value.Arr))
	for _, item := range value.Arr {
		items = append(items, item.StringValue())
	}
	return items
}

// assertWSObservation 把 Go 的观测与语料的 expect 对齐。
//
// 逐字节比的是 frames（帧文本就是线上字节）；close 比关闭码与原因；raised 只在
// Python 侧可观测（Go 没有可冒泡的异常），因此只在 divergence 用例里用形状断言。
func assertWSObservation(t *testing.T, dir string, entry serverCorpusWSCase, observation wsObservation) {
	t.Helper()
	if entry.divergence != "" {
		// 分歧用例（见语料的 divergence 字段）：两边客户端观测到的**形状**必须一致
		// ——零帧、且不是干净的 4001/4003 关闭。
		if len(observation.frames) != 0 {
			t.Errorf("分歧用例应当零帧，实际 %v", observation.frames)
		}
		if code, _ := closeInfo(observation.readErr); code == eventbus.CloseCodeAuthTimeoutOrInvalidMessage ||
			code == eventbus.CloseCodeAuthFailed {
			t.Errorf("分歧用例不应以干净关闭码收尾，实际 %d", code)
		}
		if observation.close != nil {
			t.Errorf("分歧用例不应有关闭帧，实际 %+v", observation.close)
		}
		return
	}

	wantFrames := canonicalStringArray(t, entry.expect.Lookup("frames"))
	gotFrames := make([]string, 0, len(observation.frames))
	for _, frame := range observation.frames {
		gotFrames = append(gotFrames, normalizeCorpusBody(frame, dir))
	}
	if len(gotFrames) != len(wantFrames) {
		t.Fatalf("帧数不一致: got %d %v want %d %v", len(gotFrames), gotFrames, len(wantFrames), wantFrames)
	}
	for index := range wantFrames {
		if gotFrames[index] != wantFrames[index] {
			t.Errorf("第 %d 帧不一致:\n got=%s\nwant=%s", index, gotFrames[index], wantFrames[index])
		}
	}

	wantTypes := canonicalStringArray(t, entry.expect.Lookup("frame_types"))
	for index := range wantTypes {
		if index >= len(gotFrames) {
			break
		}
		if got := frameEventType(t, gotFrames[index]); got != wantTypes[index] {
			t.Errorf("第 %d 帧的事件类型 = %s，期望 %s", index, got, wantTypes[index])
		}
	}

	wantClose := entry.expect.Lookup("close")
	if wantClose == nil || wantClose.IsNull() {
		if observation.close != nil {
			t.Errorf("不应有关闭帧，实际 %+v", observation.close)
		}
		return
	}
	if observation.close == nil {
		t.Fatalf("应有关闭帧 %s，实际读了 %d 帧（err=%v）",
			canonical.DumpsOrdered(wantClose), len(observation.frames), observation.readErr)
	}
	wantCode, _ := wantClose.Lookup("code").AsInt()
	if int64(observation.close.code) != wantCode {
		t.Errorf("关闭码 = %d，期望 %d", observation.close.code, wantCode)
	}
	if wantReason := wantClose.Lookup("reason").StringValue(); observation.close.reason != wantReason {
		t.Errorf("关闭原因 = %q，期望 %q", observation.close.reason, wantReason)
	}
}

// TestServerWebSocketMatchesPython 回放 /ws/events 的握手语料。
func TestServerWebSocketMatchesPython(t *testing.T) {
	corpus := loadServerCorpus(t)
	pinCorpusClock(t, corpus)
	for _, entry := range corpus.wsCases {
		t.Run(entry.name, func(t *testing.T) {
			dir := t.TempDir()
			app := newCorpusApp(t, substituteCorpusPaths(corpus.fixture, corpus.fixtureDir, dir), dir)
			server := httptest.NewServer(app.Handler())
			defer server.Close()

			conn := dialWebSocket(t, server, "/ws/events", nil)
			observation := observeWebSocket(t, conn, entry.send)
			assertWSObservation(t, dir, entry, observation)
		})
	}
}

// TestServerWebSocketProxyMatchesPython 回放 WS /v1/{path} 的语料。
//
// 上游用 httptest 桩（Python 侧用的是 httpx.MockTransport），因此 host:port 必然不同：
// 语料只记录 method / path / query / 头部 / body，这五项与 host 无关。
func TestServerWebSocketProxyMatchesPython(t *testing.T) {
	corpus := loadServerCorpus(t)
	pinCorpusClock(t, corpus)
	for _, entry := range corpus.wsProxyCases {
		t.Run(entry.name, func(t *testing.T) {
			dir := t.TempDir()
			capture := &upstreamCapture{}
			upstream := httptest.NewServer(http.HandlerFunc(capture.handler))
			defer upstream.Close()
			if response := entry.upstreamResponse; response != nil && !response.IsNull() {
				if status, ok := response.Lookup("status").AsInt(); ok {
					capture.status = int(status)
				}
				if contentType := response.Lookup("content_type"); contentType != nil && !contentType.IsNull() {
					capture.contentType = contentType.StringValue()
				}
				if body := response.Lookup("body"); body != nil && !body.IsNull() {
					capture.response = body.StringValue()
				}
			}

			fixture := substituteCorpusPaths(corpus.fixture, corpus.fixtureDir, dir)
			// 把 prov-a 的上游指向桩：fixture 里写死的 https://a.example.test 没有监听者。
			provider, _ := fixture.Lookup("providers").LookupOK("prov-a")
			provider.SetKey("base_url", canonical.NewString(upstream.URL))

			app := newCorpusApp(t, fixture, dir)
			server := httptest.NewServer(app.Handler())
			defer server.Close()

			conn := dialWebSocket(t, server, entry.url, corpusHeaders(entry.headers))
			observation := observeWebSocket(t, conn, entry.send)
			assertWSObservation(t, dir, entry, observation)

			assertCorpusUpstream(t, entry.expect.Lookup("upstream"), capture)
		})
	}
}

// corpusHeaders 把语料里的 {name: value} 折成请求头。
func corpusHeaders(value *canonical.Value) http.Header {
	header := http.Header{}
	if value == nil || value.IsNull() {
		return header
	}
	for _, key := range value.Obj.Keys() {
		child, _ := value.Obj.Get(key)
		header.Set(key, child.StringValue())
	}
	return header
}

// assertCorpusUpstream 比对桩上游收到的请求与语料记录。
//
// body 走 canonical.Dumps（紧凑 + 键排序）：对象键序不是契约，值才是（生成脚本同样
// 按 sort_keys=True 记录）。
func assertCorpusUpstream(t *testing.T, want *canonical.Value, capture *upstreamCapture) {
	t.Helper()
	got := capture.snapshot()
	if want == nil || !want.IsArray() {
		t.Fatalf("语料缺少 upstream 数组")
	}
	if len(want.Arr) != got.request {
		t.Fatalf("上游请求数 = %d，期望 %d", got.request, len(want.Arr))
	}
	if len(want.Arr) == 0 {
		return
	}
	expected := want.Arr[0]
	compare := func(key, actual string) {
		expectedValue := expected.Lookup(key)
		if expectedValue == nil || expectedValue.IsNull() {
			if actual != "" {
				t.Errorf("上游 %s = %q，期望不存在", key, actual)
			}
			return
		}
		if expectedValue.StringValue() != actual {
			t.Errorf("上游 %s = %q，期望 %q", key, actual, expectedValue.StringValue())
		}
	}
	compare("method", got.method)
	compare("path", got.path)
	compare("query", got.query)
	compare("authorization", got.header.Get("Authorization"))
	compare("content_type", got.header.Get("Content-Type"))
	compare("upgrade", got.header.Get("Upgrade"))
	compare("sec_websocket_key", got.header.Get("Sec-Websocket-Key"))

	wantBody := expected.Lookup("body").StringValue()
	gotBody := got.body
	if parsed, err := canonical.ParseString(gotBody); err == nil {
		gotBody = canonical.Dumps(parsed)
	}
	wantParsed, err := canonical.ParseString(wantBody)
	if err != nil {
		t.Fatalf("语料里的 body 不是合法 JSON: %q", wantBody)
	}
	if canonical.Dumps(wantParsed) != gotBody {
		t.Errorf("上游 body 不一致:\n got=%s\nwant=%s", gotBody, canonical.Dumps(wantParsed))
	}
}

// TestServerWebSocketCorpusCoversRoutes 断言语料确实覆盖了两条 WebSocket 路由。
//
// 与 HTTP 侧的 TestServerCorpusCoversEveryRoute 互补：那边的 covers 只来自 HTTP 用例，
// 这里补上握手与升级。清单必须手工与 routes.go 对齐（新增 WebSocket 路由时同步）。
func TestServerWebSocketCorpusCoversRoutes(t *testing.T) {
	corpus := loadServerCorpus(t)
	covered := map[string]bool{}
	for _, entry := range corpus.wsCases {
		for _, pattern := range canonicalStringArray(t, entry.covers) {
			covered[pattern] = true
		}
	}
	for _, entry := range corpus.wsProxyCases {
		for _, pattern := range canonicalStringArray(t, entry.covers) {
			covered[pattern] = true
		}
	}
	expected := []string{"WS /ws/events", "WS /v1/{path}"}
	for _, pattern := range expected {
		if !covered[pattern] {
			t.Errorf("WebSocket 路由未被语料覆盖: %s", pattern)
		}
	}
	for pattern := range covered {
		found := false
		for _, candidate := range expected {
			if candidate == pattern {
				found = true
			}
		}
		if !found {
			t.Errorf("语料声明了清单以外的 WebSocket 路由: %s", pattern)
		}
	}
}
