package wsproxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Sparrived/auto-model-key-router/internal/proxysupport"
)

// frameExpectation 是一帧期望值。
type frameExpectation struct {
	Kind string
	Text string
	B64  string
}

// assertSame 比较期望与实际，不一致时给出 JSON 形式的差异。
func assertSame(t *testing.T, name string, want, got any) {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	t.Errorf("%s 不一致\n期望: %s\n实际: %s", name, wantJSON, gotJSON)
}

// encodeRawPairs 把 http.Header 展成 [名, 值] 列表。
func encodeRawPairs(header http.Header) [][]string {
	pairs := [][]string{}
	for name, values := range header {
		for _, value := range values {
			pairs = append(pairs, []string{name, value})
		}
	}
	return pairs
}

// normalizePairs 归一化头部对：键转小写、去掉 host、排序后比较。
func normalizePairs(pairs [][]string) []string {
	result := []string{}
	for _, pair := range pairs {
		name := strings.ToLower(pair[0])
		if name == "host" {
			continue
		}
		result = append(result, name+": "+pair[1])
	}
	sort.Strings(result)
	return result
}

// 连接结果：帧序列与关闭码。
type connectionResult struct {
	Frames    []frameExpectation
	CloseCode int
	CloseErr  error
}

// runConnection 起一个进程内 WS 服务器，发一帧，把响应读到关闭为止。
//
// 用真实服务器 + 真实 WebSocket 客户端（coder/websocket）而不是模拟：帧的**类型**
// （文本/二进制）只有在真的写线上帧时才体现，而类型判定正是这个垫片最容易错的地方。
func runConnection(t *testing.T, proxy ProxyHandler, path string,
	headers http.Header, send []byte, binary bool) connectionResult {
	t.Helper()
	server := httptest.NewServer(&Handler{Proxy: proxy})
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dialOptions := &websocket.DialOptions{}
	if headers != nil {
		dialOptions.HTTPHeader = headers
	}
	conn, _, err := websocket.Dial(ctx, server.URL+path, dialOptions)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	messageType := websocket.MessageText
	if binary {
		messageType = websocket.MessageBinary
	}
	if err := conn.Write(ctx, messageType, send); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	result := connectionResult{}
	for {
		readType, data, readErr := conn.Read(ctx)
		if readErr != nil {
			var closeErr websocket.CloseError
			if errors.As(readErr, &closeErr) {
				result.CloseCode = int(closeErr.Code)
				result.CloseErr = closeErr
			}
			return result
		}
		if readType == websocket.MessageText {
			result.Frames = append(result.Frames, frameExpectation{Kind: "text", Text: string(data)})
		} else {
			result.Frames = append(result.Frames,
				frameExpectation{Kind: "bytes", B64: base64.StdEncoding.EncodeToString(data)})
		}
	}
}

// scripted 构造一个按「动作」脚本写响应的 ProxyHandler。
//
// 动作语义：write:数据 写一段字节；flush 发一帧（清空缓冲）；status:码 写状态码；
// header:名=值 设置响应头。
func scripted(actions ...string) ProxyHandler {
	return func(w http.ResponseWriter, request *http.Request, path string) {
		for _, action := range actions {
			switch {
			case action == "flush":
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			case strings.HasPrefix(action, "write:"):
				_, _ = w.Write([]byte(strings.TrimPrefix(action, "write:")))
			case strings.HasPrefix(action, "status:"):
				code, err := strconv.Atoi(strings.TrimPrefix(action, "status:"))
				if err != nil {
					panic(err)
				}
				w.WriteHeader(code)
			case strings.HasPrefix(action, "header:"):
				name, value, _ := strings.Cut(strings.TrimPrefix(action, "header:"), "=")
				w.Header().Set(name, value)
			}
		}
	}
}

// TestFlushIsFrameBoundary 锁定「一次 Flush = 一帧」这条映射。
//
// 参照实现拿到的是 StreamingResponse 的 body_iterator：一个 chunk 一帧
// （websocket_proxy.py:75-76）。Go 侧没有迭代器，帧边界只能由 Flush 表达，所以这条
// 映射是整份移植的关键约定。
func TestFlushIsFrameBoundary(t *testing.T) {
	result := runConnection(t, scripted(
		"header:Content-Type=text/event-stream",
		"write:one", "flush",
		"write:two", "flush",
		"write:three",
	), "/v1/chat/completions", nil, []byte("{}"), false)

	want := []frameExpectation{
		{Kind: "text", Text: "one"},
		{Kind: "text", Text: "two"},
		{Kind: "text", Text: "three"}, // 收尾时最后一段没 flush 的字节也是一帧
	}
	assertSame(t, "frames", want, result.Frames)
	if result.CloseCode != CloseCodeNormal {
		t.Fatalf("关闭码 = %d，期望 %d", result.CloseCode, CloseCodeNormal)
	}
}

// TestEmptyStreamChunkEmitsFrameButEmptyBodyDoesNot 锁定两条相反的边界。
//
//   - 流式里的空 chunk **会**发一个空帧（websocket_proxy.py:75-76 无条件 send）；
//   - 非流式空 body **一帧都不发**（websocket_proxy.py:82 的 `if response.body:`）。
func TestEmptyStreamChunkEmitsFrameButEmptyBodyDoesNot(t *testing.T) {
	stream := runConnection(t, scripted(
		"header:Content-Type=text/event-stream", "flush", "write:x", "flush",
	), "/v1/chat/completions", nil, []byte("{}"), false)
	assertSame(t, "空 chunk 仍成帧", []frameExpectation{
		{Kind: "text", Text: ""},
		{Kind: "text", Text: "x"},
	}, stream.Frames)

	empty := runConnection(t, scripted(
		"header:Content-Type=application/json",
	), "/v1/chat/completions", nil, []byte("{}"), false)
	if len(empty.Frames) != 0 {
		t.Fatalf("空响应体不该发帧，实际 %v", empty.Frames)
	}
	if empty.CloseCode != CloseCodeNormal {
		t.Fatalf("关闭码 = %d，期望 1000", empty.CloseCode)
	}
}

// TestTextFrameDecisionIsCaseSensitive 锁定帧类型判定的**大小写敏感**。
//
// websocket_proxy.py:91 是
//
//	content_type.startswith("text/") or "json" in content_type
//
// 两个比较都区分大小写，所以 `Application/JSON` 会走**二进制**帧。头部**名**的查找
// 则是大小写不敏感的（Starlette 的 Headers 如此），这一条也一起钉住。
func TestTextFrameDecisionIsCaseSensitive(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		wantBinary  bool
	}{
		{"json_lower", "application/json", false},
		{"json_with_charset", "application/json; charset=utf-8", false},
		{"text_prefix", "text/plain; charset=utf-8", false},
		{"json_uppercase_value", "Application/JSON", true},
		{"text_uppercase_prefix", "Text/Plain", true},
		{"no_content_type", "", true},
		{"octet_stream", "application/octet-stream", true},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			// 头部名故意用全大写，验证查找不区分大小写。
			actions := []string{"write:payload"}
			if item.contentType != "" {
				actions = append([]string{"header:CONTENT-TYPE=" + item.contentType}, actions...)
			}
			result := runConnection(t, scripted(actions...),
				"/v1/chat/completions", nil, []byte("{}"), false)
			if len(result.Frames) != 1 {
				t.Fatalf("应有 1 帧，实际 %v", result.Frames)
			}
			got := result.Frames[0]
			if item.wantBinary {
				if got.Kind != "bytes" || got.B64 != base64.StdEncoding.EncodeToString([]byte("payload")) {
					t.Fatalf("应为二进制帧，实际 %+v", got)
				}
				return
			}
			if got.Kind != "text" || got.Text != "payload" {
				t.Fatalf("应为文本帧，实际 %+v", got)
			}
		})
	}
}

// TestInvalidUTF8ReplacedInTextFrame 与 TestInvalidUTF8PreservedInBinaryFrame
// 一起锁定 websocket_proxy.py:92 的 decode("utf-8", errors="replace")。
//
// 文本帧必须是合法 UTF-8，参照实现会先把字节宽容解码成 str 再 send_text，也就是
// 把非法字节按「最大子部分」替换成 U+FFFD 后重新编码；二进制帧则原样透传。
func TestInvalidUTF8ReplacedInTextFrame(t *testing.T) {
	raw := []byte("a\xe4\xb8Xc") // 截断的「中」：最大子部分只产一个 U+FFFD
	result := runConnection(t, scripted(
		"header:Content-Type=text/plain", "write:"+string(raw),
	), "/v1/chat/completions", nil, []byte("{}"), false)
	assertSame(t, "text frames", []frameExpectation{
		{Kind: "text", Text: "a\uFFFDXc"},
	}, result.Frames)

	binary := runConnection(t, scripted(
		"header:Content-Type=application/octet-stream", "write:"+string(raw),
	), "/v1/chat/completions", nil, []byte("{}"), false)
	assertSame(t, "binary frames", []frameExpectation{
		{Kind: "bytes", B64: base64.StdEncoding.EncodeToString(raw)},
	}, binary.Frames)
}

// TestCloseCodeMapping 锁定 `_websocket_close_code`（websocket_proxy.py:97-102）。
func TestCloseCodeMapping(t *testing.T) {
	cases := map[int]int{
		200: CloseCodeNormal, 204: CloseCodeNormal, 302: CloseCodeNormal,
		399: CloseCodeNormal, 400: CloseCodePolicy, 401: CloseCodePolicy,
		404: CloseCodePolicy, 429: CloseCodePolicy, 499: CloseCodePolicy,
		500: CloseCodeInternal, 502: CloseCodeInternal, 521: CloseCodeInternal,
	}
	for status, want := range cases {
		if got := CloseCode(status); got != want {
			t.Errorf("CloseCode(%d) = %d，期望 %d", status, got, want)
		}
	}
	// 端到端也要看得到：脚本化 401 与 500 各关一次。
	for _, item := range []struct {
		status int
		want   int
	}{{401, CloseCodePolicy}, {500, CloseCodeInternal}} {
		result := runConnection(t, scripted("status:"+strconv.Itoa(item.status), "write:x"),
			"/v1/chat/completions", nil, []byte("{}"), false)
		if result.CloseCode != item.want {
			t.Fatalf("状态码 %d 的关闭码 = %d，期望 %d", item.status, result.CloseCode, item.want)
		}
	}
}

// TestHandshakeHeadersAreFiltered 锁定被剔除的六个握手头（websocket_proxy.py:13-22）。
//
// 注意 authorization / x-api-key **不在**剔除名单里：它们是下游凭据，要留给代理层。
func TestHandshakeHeadersAreFiltered(t *testing.T) {
	handshake := http.Header{
		"Connection":               {"Upgrade"},
		"Upgrade":                  {"websocket"},
		"Sec-Websocket-Key":        {"key"},
		"Sec-Websocket-Version":    {"13"},
		"Sec-Websocket-Protocol":   {"chat"},
		"Sec-Websocket-Extensions": {"permessage-deflate"},
		// 下面这些必须留下（大小写混写，验证过滤是按小写比较的）。
		"Authorization": {"Bearer local-key"},
		"X-Api-Key":     {"visitor"},
		"Cookie":        {"session=1"},
	}
	filtered := FilterHandshakeHeaders(handshake)
	want := []string{"authorization: Bearer local-key", "cookie: session=1", "x-api-key: visitor"}
	got := normalizePairs(encodeRawPairs(filtered))
	assertSame(t, "filtered", want, got)

	// 原 map 不能被改动（参照实现复制 scope 之后再替换 headers）。
	if len(handshake) != 9 {
		t.Fatalf("过滤不应改动入参，实际剩 %d 个键", len(handshake))
	}
	if len(HandshakeHeaders) != 6 {
		t.Fatalf("握手头集合应有 6 个成员，实际 %d", len(HandshakeHeaders))
	}
}

// TestCanonicalisedHeaderNamesDivergeFromPythonRawBytes 记录一处**已知分歧**。
//
// Python 在**字节层**比较头部名、并保留原始大小写，所以 `X-Multi: first` 与
// `x-multi: second` 是两个不同的键、两个都会带给上游（实测 upstream_headers =
// {"X-Multi": "first", "x-multi": "second"}）。Go 的 http.Header 以规范化键为 map 键，
// 同名头会被合并成 `X-Multi: [first, second]`，再经 proxysupport.UpstreamHeaders 的
// 「后者胜」得到**一个** `X-Multi: second`。
//
// 影响面：上游收到的同名头数量不同（HTTP 层大小写不敏感，值也不会丢给「没有重复」
// 的普通请求），只有「同一请求里带仅大小写不同的同名头」才会被观测到。这是 Go
// 标准库的既定行为，不是实现缺陷；记录下来是为了让这处与参照实现的差异有出处。
func TestCanonicalisedHeaderNamesDivergeFromPythonRawBytes(t *testing.T) {
	handshake := http.Header{}
	handshake.Add("X-Multi", "first")
	handshake.Add("x-multi", "second")
	handshake.Add("Content-Type", "application/json")

	request := SynthesizeRequest("http", mustURL(t, "/v1/chat/completions"), handshake, []byte("{}"))
	got := proxysupport.UpstreamHeaders(map[string][]string(request.Header), "sk-up")

	// Python 侧的同名输入产出两个键；迁移期录下的 full_handshake 记录了这个
	// 事实（upstream_headers 里同时有 X-Multi 与 x-multi）。Go 侧只剩一个。
	// 键数 = X-Multi + Content-Type + 补上的 Authorization / Accept-Encoding。
	if len(got) != 4 {
		t.Fatalf("Go 侧应把同名头合并成一个键，实际 %v", got)
	}
	if got["X-Multi"] != "second" {
		t.Fatalf("合并后应保留最后一个值（后者胜），实际 %q", got["X-Multi"])
	}
	if got["Content-Type"] != "application/json" {
		t.Fatalf("普通头应原样保留，实际 %q", got["Content-Type"])
	}
	if _, ok := got["x-multi"]; ok {
		t.Fatal("Go 侧不应再有小写形式的同名键")
	}
}

// TestPathResolution 锁定路由参数 `{path:path}` 的还原规则。
func TestPathResolution(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": "chat/completions",
		"/v1/a/b/c":            "a/b/c",
		"/v1/":                 "",
		"/v1/a%2Fb":            "a/b", // URL 是解码后的
		"/v1/%E4%B8%AD":        "中",
	}
	for raw, want := range cases {
		parsed := mustURL(t, raw)
		if got := Path(parsed); got != want {
			t.Errorf("Path(%s) = %q，期望 %q", raw, got, want)
		}
	}
	if got := Path(nil); got != "" {
		t.Errorf("Path(nil) = %q", got)
	}

	// 端到端也要看得到：ProxyHandler 拿到的 path 是去掉 /v1/ 的那一段。
	var seen string
	result := runConnection(t, func(w http.ResponseWriter, request *http.Request, path string) {
		seen = path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}, "/v1/a/b/c", nil, []byte("{}"), false)
	if seen != "a/b/c" {
		t.Fatalf("ProxyHandler 收到的 path = %q", seen)
	}
	if len(result.Frames) != 1 {
		t.Fatalf("应有 1 帧，实际 %v", result.Frames)
	}
}

// TestSynthesizedRequestKeepsHandshakeShape 锁定折算请求的骨架。
//
// 参照实现复制整个 ASGI scope，只改 type/http_version/method/scheme 并替换 headers，
// 因此 method 恒为 POST、查询串与 URL 都沿用握手那一份（tests/test_app.py:485 的
// `?trace=1` 就依赖这一点）。
func TestSynthesizedRequestKeepsHandshakeShape(t *testing.T) {
	handshake := http.Header{"Authorization": {"Bearer local-key"}}
	target := mustURL(t, "/v1/messages")
	target.RawQuery = "trace=1"

	request := SynthesizeRequest(SchemeFromWebSocket("wss"), target, handshake, []byte("body"))
	if request.Method != http.MethodPost {
		t.Errorf("method = %s，期望 POST", request.Method)
	}
	if request.URL.Scheme != "https" {
		t.Errorf("scheme = %s，期望 https（wss）", request.URL.Scheme)
	}
	if request.URL.RawQuery != "trace=1" {
		t.Errorf("查询串 = %q，期望 trace=1", request.URL.RawQuery)
	}
	if request.Proto != "HTTP/1.1" || request.ProtoMajor != 1 || request.ProtoMinor != 1 {
		t.Errorf("http_version = %s，期望 HTTP/1.1", request.Proto)
	}
	if request.ContentLength != 4 {
		t.Errorf("ContentLength = %d，期望 4", request.ContentLength)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil || string(body) != "body" {
		t.Errorf("body = %q, err=%v", body, err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer local-key" {
		t.Errorf("Authorization = %q", got)
	}
}

// TestSchemeFromHandshake 锁定 TLS → https。
func TestSchemeFromHandshake(t *testing.T) {
	if got := SchemeFromWebSocket("wss"); got != "https" {
		t.Errorf("wss -> %q", got)
	}
	for _, scheme := range []string{"ws", "", "http"} {
		if got := SchemeFromWebSocket(scheme); got != "http" {
			t.Errorf("%s -> %q", scheme, got)
		}
	}
	if got := Scheme(&http.Request{}); got != "http" {
		t.Errorf("无 TLS -> %q", got)
	}
	tlsState := tls.ConnectionState{}
	if got := Scheme(&http.Request{TLS: &tlsState}); got != "https" {
		t.Errorf("有 TLS -> %q", got)
	}
}

// TestHeaderValueHandlesNonCanonicalKeys 验证帧类型判定用的头部查找不区分大小写。
//
// http.Header 是裸 map，直接写非规范键（`h["content-type"] = ...`）的处理器是存在的，
// 而 Header.Get 只查规范键、会静默查不到——那会让本该是文本帧的响应变成二进制帧。
// Starlette 的 Headers 没有这个问题。
func TestHeaderValueHandlesNonCanonicalKeys(t *testing.T) {
	header := http.Header{"content-type": {"text/event-stream"}}
	if got := headerValue(header, "content-type"); got != "text/event-stream" {
		t.Fatalf("非规范键查找失败: %q", got)
	}
	if got := headerValue(header, "Content-Type"); got != "text/event-stream" {
		t.Fatalf("大小写不敏感查找失败: %q", got)
	}
	if got := headerValue(http.Header{}, "content-type"); got != "" {
		t.Fatalf("缺失时应有空串，实际 %q", got)
	}
}

// TestSingleRequestPerConnection 锁定「一条连接只服务一条请求」。
//
// 参照实现读完第一帧就处理、发完响应直接 close（websocket_proxy.py:29-40），没有
// 循环。第二条消息只会撞上已关闭的连接，绝不会再触发一次代理。
func TestSingleRequestPerConnection(t *testing.T) {
	calls := 0
	result := runConnection(t, func(w http.ResponseWriter, request *http.Request, path string) {
		calls++
		_, _ = w.Write([]byte("{}"))
	}, "/v1/chat/completions", nil, []byte("{}"), false)

	if calls != 1 {
		t.Fatalf("ProxyHandler 被调用 %d 次，期望 1 次", calls)
	}
	if result.CloseCode != CloseCodeNormal {
		t.Fatalf("关闭码 = %d", result.CloseCode)
	}
	if len(result.Frames) != 1 {
		t.Fatalf("帧数 = %d，期望 1", len(result.Frames))
	}
}

// TestOriginHeaderIsAccepted 锁定 AcceptOptions.InsecureSkipVerify 的必要性。
//
// FastAPI 默认不校验 Origin；coder/websocket 默认会拒掉跨源握手。不关掉它，浏览器
// 之外的客户端（脚本、本仓库的测试）会因为缺 Origin 或跨源直接拿到 403，那就与
// 参照实现不同了。
func TestOriginHeaderIsAccepted(t *testing.T) {
	result := runConnection(t, func(w http.ResponseWriter, request *http.Request, path string) {
		_, _ = w.Write([]byte("{}"))
	}, "/v1/chat/completions", http.Header{
		"Origin": {"http://example.com"},
	}, []byte("{}"), false)
	if len(result.Frames) != 1 {
		t.Fatalf("带跨源 Origin 也应正常处理，实际 %+v", result)
	}
}

// TestPanicInProxyHandlerCloses1011 锁定异常兜底。
//
// websocket_proxy.py:43-48 的 `except Exception` 会记日志并关 1011。Go 侧用 recover
// 对齐（Python 的 Exception 覆盖不到 BaseException，所以这是有意更宽一点）。
func TestPanicInProxyHandlerCloses1011(t *testing.T) {
	result := runConnection(t, func(w http.ResponseWriter, request *http.Request, path string) {
		panic("boom")
	}, "/v1/chat/completions", nil, []byte("{}"), false)
	if result.CloseCode != CloseCodeInternal {
		t.Fatalf("关闭码 = %d，期望 1011", result.CloseCode)
	}
	if len(result.Frames) != 0 {
		t.Fatalf("异常路径不该发帧，实际 %v", result.Frames)
	}
}

// TestUpstreamHandlerReusesProxySupportRules 用真实上游验证内置 Upstream。
//
// 这一条同时覆盖「WS 路径与 HTTP 路径共用同一套规则」：路径经 UpstreamPath、URL 经
// JoinURL、上游头经 UpstreamHeaders（丢掉下游凭据、补 Bearer 与 identity）、响应头经
// ResponseHeaders（丢掉描述上游那一跳的四个头）。
func TestUpstreamHandlerReusesProxySupportRules(t *testing.T) {
	type upstreamCall struct {
		method string
		target string
		header http.Header
		body   string
	}
	var calls []upstreamCall
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, upstreamCall{
			method: r.Method, target: r.URL.String(), header: r.Header.Clone(), body: string(body),
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip") // 必须被 ResponseHeaders 剔除
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler := &Upstream{
		BaseURL: upstream.URL,
		APIKey:  "sk-up",
		Routes:  map[string]string{"openai": "v1/chat/completions"},
	}
	result := runConnection(t, handler.Handle, "/v1/chat/completions?trace=1",
		http.Header{
			"Authorization": {"Bearer local-key"},
			"X-Api-Key":     {"visitor"},
			"Content-Type":  {"application/json"},
		}, []byte(`{"model": "m"}`), false)

	if len(calls) != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1", len(calls))
	}
	call := calls[0]
	if call.method != http.MethodPost {
		t.Errorf("上游 method = %s", call.method)
	}
	if !strings.HasSuffix(call.target, "/v1/chat/completions?trace=1") {
		t.Errorf("上游 URL = %s，期望带上查询串", call.target)
	}
	if got := call.header.Get("Authorization"); got != "Bearer sk-up" {
		t.Errorf("上游 Authorization = %q，期望被换成 Bearer sk-up", got)
	}
	if got := call.header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("上游 Accept-Encoding = %q，期望 identity", got)
	}
	if _, ok := call.header["X-Api-Key"]; ok {
		t.Error("x-api-key 不该带到上游")
	}
	if call.body != `{"model": "m"}` {
		t.Errorf("上游收到的请求体 = %q", call.body)
	}
	if len(result.Frames) != 1 || result.Frames[0].Text != `{"ok":true}` {
		t.Fatalf("响应帧 = %+v", result.Frames)
	}
	if result.CloseCode != CloseCodeNormal {
		t.Fatalf("关闭码 = %d", result.CloseCode)
	}
}

// TestUpstreamHandlerStreamsWhenStreamRequested 验证流式分支真的逐块 Flush。
func TestUpstreamHandlerStreamsWhenStreamRequested(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("上游测试服务器没有 Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{"data: one\n\n", "data: two\n\n", "data: done\n\n"} {
			_, _ = io.WriteString(w, chunk)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	handler := &Upstream{BaseURL: upstream.URL, APIKey: "sk-up",
		Routes: map[string]string{"openai": "v1/chat/completions"}}
	result := runConnection(t, handler.Handle, "/v1/chat/completions", nil,
		[]byte(`{"model": "m", "stream": true}`), false)

	if len(result.Frames) == 0 {
		t.Fatal("流式响应应有帧")
	}
	var joined strings.Builder
	for _, frame := range result.Frames {
		if frame.Kind != "text" {
			t.Fatalf("SSE 应是文本帧，实际 %+v", frame)
		}
		joined.WriteString(frame.Text)
	}
	if joined.String() != "data: one\n\ndata: two\n\ndata: done\n\n" {
		t.Fatalf("拼接后的响应体 = %q", joined.String())
	}
}

// TestUpstreamHandlerNonStreamWritesOneFrame 验证非流式只写一帧。
func TestUpstreamHandlerNonStreamWritesOneFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id": "ok", "padding": "`))
		_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer upstream.Close()

	handler := &Upstream{BaseURL: upstream.URL, APIKey: "sk-up",
		Routes: map[string]string{"openai": "v1/chat/completions"}}
	result := runConnection(t, handler.Handle, "/v1/chat/completions", nil,
		[]byte(`{"model": "m"}`), false)

	if len(result.Frames) != 1 {
		t.Fatalf("非流式响应应只有 1 帧（即使超过一次 Read），实际 %d 帧", len(result.Frames))
	}
	if result.Frames[0].Kind != "text" {
		t.Fatalf("application/json 应是文本帧，实际 %+v", result.Frames[0])
	}
}

// mustURL 解析测试用的 URL。
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 URL 失败: %v", err)
	}
	return parsed
}
