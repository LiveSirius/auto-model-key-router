package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/proxysupport"
	"github.com/Sparrived/auto-model-key-router/internal/runtime"
)

// 本包对 internal/runtime 的接缝断言。
//
// 参照实现把这些资源放在 RuntimeResources 里（runtime.py:14-19），Go 侧对应
// runtime.HTTPClient（manager.go:31-33）。**没有导入环**：internal/runtime 只依赖
// internal/config，不反向依赖本包，所以这里能直接引用它的接口。放在测试里既能在
// 编译期锁住接缝，又不必让生产代码依赖 runtime。runtime 一旦改接缝，本测试立刻
// 编译失败，而不是等到运行期热重载才发现。
var _ runtime.HTTPClient = (*Client)(nil)

// TestPoolLimitsMatchHttpxLimits 锁定连接池参数。
//
// 对齐 app.py:45-47 的 httpx.Limits(max_connections=100,
// max_keepalive_connections=30, keepalive_expiry=30)。这条断言的价值在于：Go 的
// MaxIdleConnsPerHost 默认是 **2**，不设就会把并发上游连接静默串行化——没有报错，
// 只是慢，所以必须有测试把它钉住。
func TestPoolLimitsMatchHttpxLimits(t *testing.T) {
	cases := []struct {
		name            string
		config          Config
		wantMaxConns    int
		wantMaxIdle     int
		wantIdleTimeout time.Duration
	}{
		{
			name:            "零值按 httpx 默认补齐",
			config:          Config{},
			wantMaxConns:    100,
			wantMaxIdle:     30,
			wantIdleTimeout: 30 * time.Second,
		},
		{
			name:            "负值同样回落到默认",
			config:          Config{MaxConnections: -1, MaxIdlePerHost: -5, IdleConnTimeout: -time.Second},
			wantMaxConns:    100,
			wantMaxIdle:     30,
			wantIdleTimeout: 30 * time.Second,
		},
		{
			name:            "显式配置透传",
			config:          Config{MaxConnections: 7, MaxIdlePerHost: 3, IdleConnTimeout: 2 * time.Second},
			wantMaxConns:    7,
			wantMaxIdle:     3,
			wantIdleTimeout: 2 * time.Second,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			transport := New(item.config).Transport()
			if transport.MaxConnsPerHost != item.wantMaxConns {
				t.Errorf("MaxConnsPerHost = %d，期望 %d", transport.MaxConnsPerHost, item.wantMaxConns)
			}
			if transport.MaxIdleConnsPerHost != item.wantMaxIdle {
				t.Errorf("MaxIdleConnsPerHost = %d，期望 %d（Go 默认 2 会静默串行化并发）",
					transport.MaxIdleConnsPerHost, item.wantMaxIdle)
			}
			if transport.IdleConnTimeout != item.wantIdleTimeout {
				t.Errorf("IdleConnTimeout = %v，期望 %v", transport.IdleConnTimeout, item.wantIdleTimeout)
			}
		})
	}
}

// TestDisableCompressionIsSet 锁定「不自动补 Accept-Encoding、也不透明解压」。
//
// 对应 httpx 的 Transport.DisableCompression 行为。Go 的默认 Transport 会自动补
// Accept-Encoding: gzip，而参照实现强制 identity（proxy_support.py:325），两者
// 冲突且不报错。
func TestDisableCompressionIsSet(t *testing.T) {
	if tr := New(Config{}).Transport(); !tr.DisableCompression {
		t.Fatal("DisableCompression 必须为 true：否则 Go 会自行补 Accept-Encoding 并透明解压")
	}
}

// TestHTTP2IsDisabled 锁定「只走 HTTP/1.1」。
//
// httpx 默认 http2=False（httpx/_client.py 的默认 HttpTransport），Go 的
// http.Transport 则会在 TLS 上自动协商 h2。两者对上游的可观测差异包括：请求头
// 大小写被小写化、响应头形态不同、连接复用与并发行为不同。
//
// 两层断言缺一不可：
//   - 配置层：TLSNextProto 非 nil 且为空（net/http 文档里关掉自动 h2 的写法）；
//   - 行为层：对**真实的 TLS h2 服务端**发起请求，实际用的必须是 HTTP/1.1。
//
// 只写配置层会漏掉「有人改成 ForceAttemptHTTP2: true」这类回归，只写行为层则会在
// 上游是明文 HTTP 时永远测不到（明文没有 h2 协商）。
func TestHTTP2IsDisabled(t *testing.T) {
	tr := New(Config{}).Transport()
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto 为 nil：Go 会在 TLS 上自动协商 h2，与 httpx 默认的 HTTP/1.1 不一致")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto = %v，期望空表（非 nil 但无协议 = 只走 HTTP/1.1）", tr.TLSNextProto)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 必须为 false")
	}

	// 行为层：起一个**开启 h2** 的 TLS 服务端，确认客户端仍用 HTTP/1.1。
	var proto string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	// 明确允许 h2，这样「客户端选了 1.1」才是客户端的决定，而不是服务端不支持。
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	client := New(Config{})
	client.Transport().TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if proto != "HTTP/1.1" {
		t.Fatalf("上游看到的协议 = %q，期望 HTTP/1.1（httpx 默认 http2=False）", proto)
	}
}

// TestDoesNotFollowRedirect 复刻 httpx 的 follow_redirects=False。
//
// httpx 默认不跟重定向（httpx/_client.py:197），Go 默认最多跟 10 次。若回归成跟
// 随，调用方就再也看不到上游的 3xx——它会被静默替换成重定向目标的响应。
func TestDoesNotFollowRedirect(t *testing.T) {
	var targetHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			w.Header().Set("Location", "/target")
			w.WriteHeader(http.StatusFound)
		case "/target":
			targetHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("redirected"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := New(Config{})
	req, err := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d，期望 302（不跟随重定向，原样返回）", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/target" {
		t.Errorf("Location = %q，期望 %q", got, "/target")
	}
	if string(body) == "redirected" {
		t.Error("响应体是重定向目标的内容：客户端跟随了重定向")
	}
	if targetHits != 0 {
		t.Errorf("重定向目标被访问了 %d 次，期望 0 次", targetHits)
	}
}

// TestRawQueryForwardedVerbatim 锁定 query 逐字转发。
//
// 对照实测（httpx 0.28.1 + starlette 1.2.1）：`?a&b=1&c=%2F&d=x y&e=1&b=2` 经
// Starlette QueryParams → httpx params= 之后变成 `a=&b=2&c=%2F&d=x+y&e=1`——裸键
// a 被补成 a=、重复键 b 被合并丢掉一个、空格变 +。本包走 URL.RawQuery，不做任何
// 重编码，因此下游发什么字节上游就收到什么。
func TestRawQueryForwardedVerbatim(t *testing.T) {
	cases := []struct {
		name     string
		rawQuery string
		want     string
	}{
		{name: "裸键不带等号", rawQuery: "a&b=1&c=%2F", want: "a&b=1&c=%2F"},
		{name: "空 query 不产生问号", rawQuery: "", want: ""},
		{name: "重复键都保留且顺序不变", rawQuery: "b=1&b=2&a=3", want: "b=1&b=2&a=3"},
		{name: "保留原始转义大小写", rawQuery: "c=%2f&d=%2F", want: "c=%2f&d=%2F"},
		{name: "保留加号与空值", rawQuery: "q=a+b&e=", want: "q=a+b&e="},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			var received string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received = r.URL.RawQuery
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			target := UpstreamURL(server.URL, "v1/chat/completions", item.rawQuery)
			req, err := http.NewRequest(http.MethodGet, target, nil)
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			resp, err := New(Config{}).Do(req)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()

			if received != item.want {
				t.Fatalf("服务端收到的 RawQuery = %q，期望 %q", received, item.want)
			}
		})
	}
}

// TestForcedAcceptEncodingIdentityNotDecompressed 锁定压缩行为的两半。
//
// 参照实现强制 identity（proxy_support.py:325，且 tests/test_app.py:3454 直接断言
// 了上游收到的 accept-encoding 就是 identity）。配合 DisableCompression，Go 既不
// 会改写这个头，也不会在服务端仍返回 gzip 时替我们解开——解开会改变下游收到的
// 字节，而这是静默的。
func TestForcedAcceptEncodingIdentityNotDecompressed(t *testing.T) {
	var gzipped bytes.Buffer
	writer := gzip.NewWriter(&gzipped)
	if _, err := writer.Write([]byte("hello upstream")); err != nil {
		t.Fatalf("构造 gzip 失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 gzip 失败: %v", err)
	}
	gzipBytes := gzipped.Bytes()

	var seenAcceptEncoding string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipBytes)
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/x", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header = CopyRequestHeaders(http.Header{"Accept-Encoding": {"gzip"}}, "sk-test")

	resp, err := New(Config{}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}

	if seenAcceptEncoding != "identity" {
		t.Errorf("上游收到的 Accept-Encoding = %q，期望 %q", seenAcceptEncoding, "identity")
	}
	if !bytes.Equal(body, gzipBytes) {
		t.Errorf("响应体被透明解压了：收到 %d 字节，期望仍是原始 gzip 的 %d 字节",
			len(body), len(gzipBytes))
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("响应体不是 gzip 魔数开头，前 2 字节 = %x", body[:min(2, len(body))])
	}
}

// TestStreamingResponseStaysChunked 锁定流式响应不带 Content-Length。
//
// 参照实现走 ASGI StreamingResponse（app.py:362-365、proxy_handler.py:924），发头
// 时长度未知，因此不可能有 Content-Length。Go 的 http.Server 会在响应体较小时
// **自动补上**它——一旦补上，SSE 客户端会等到整个流结束才开始消费。
func TestStreamingResponseStaysChunked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("测试服务器不支持 Flush")
			return
		}
		_, _ = w.Write([]byte("data: one\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: two\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	client := New(Config{FirstByteTimeout: 5 * time.Second})
	resp, err := client.DoStream(req)
	if err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.ContentLength != -1 {
		t.Errorf("ContentLength = %d，期望 -1（长度未知即分块）", resp.ContentLength)
	}
	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Errorf("TransferEncoding = %v，期望 [chunked]", resp.TransferEncoding)
	}
	if got := resp.Header.Get("Transfer-Encoding"); got != "" {
		// Go 把 Transfer-Encoding 从 Header 摘出来放进 resp.TransferEncoding，
		// 因此 Header 里看不到它是**正确**行为——这也正是它不会污染转发给下游的
		// 响应头的原因。
		t.Errorf("Header 里不应再有 Transfer-Encoding，实际 = %q", got)
	}

	// 回给下游的头里必须**没有** Content-Length：否则下游会等整个流结束。
	out := ResponseHeaders(resp.Header)
	if got := out.Get("Content-Length"); got != "" {
		t.Errorf("转发给下游的 Content-Length = %q，期望不存在", got)
	}
	if got := out.Get("Transfer-Encoding"); got != "" {
		t.Errorf("转发给下游的 Transfer-Encoding = %q，期望不存在（由本进程的 server 决定分块）", got)
	}
	if !IsSSEMediaType(resp.Header.Get("Content-Type")) {
		t.Errorf("Content-Type = %q，应被识别为 SSE", resp.Header.Get("Content-Type"))
	}
	SetStreamingHeaders(out)
	if got := out.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q，期望 %q", got, "no-cache")
	}
	if got := out.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q，期望 %q", got, "no")
	}
}

// TestDownstreamStreamKeepsChunkedEndToEnd 端到端验证：模仿路由器自己的转发写法，
// 下游看到的响应**没有** Content-Length 且是分块传输。
//
// 上一个测试断言的是上游响应的形状；这一条才覆盖真正的风险点——响应头的折叠与
// 转发。若把上游的 Content-Length（或 Go 自动补的那个）带下去，下游的 SSE 客户端
// 会一直等到流结束，表现为「流式接口不流式」，而且不报错。
func TestDownstreamStreamKeepsChunkedEndToEnd(t *testing.T) {
	// 上游刻意带上**正确**的 Content-Length：这样上游是长度分帧，而下游仍必须
	// 是分块——正是本测试要证明的那条差异。
	const streamBody = "data: one\n\ndata: two\n\n"

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 上游这里刻意**带** Content-Length（且长度正确），用来证明本包转发时会把它
		// 剔掉：上游是长度分帧，下游仍必须是分块。
		w.Header().Set("Content-Length", strconv.Itoa(len(streamBody)))
		// 必须用 Add 而不是 Set：Set 会覆盖，服务端就只会发一个值，重复头场景
		// 根本不会出现。
		w.Header().Add("X-Dup", "first")
		w.Header().Add("X-Dup", "second")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody[:10]))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte(streamBody[10:]))
	}))
	defer upstreamServer.Close()

	// 模拟路由器的转发 handler：用本包的头处理 + Flush。
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest(http.MethodPost, upstreamServer.URL+"/v1/messages", nil)
		if err != nil {
			t.Errorf("构造上游请求失败: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp, err := New(Config{FirstByteTimeout: 5 * time.Second}).DoStream(req)
		if err != nil {
			t.Errorf("上游请求失败: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		headers := ResponseHeaders(resp.Header)
		if IsSSEMediaType(headers.Get("Content-Type")) {
			SetStreamingHeaders(headers)
		}
		for key, values := range headers {
			for _, value := range values {
				// ResponseHeaders 保证每键只有一个值，Set 即可。
				w.Header().Set(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)

		flusher := w.(http.Flusher)
		buf := make([]byte, 32)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					t.Errorf("写下游失败: %v", werr)
					return
				}
				// 每块都 Flush：这是响应保持分块的关键（Go 只有在 handler 返回且
				// 总字节数较小时才会补 Content-Length）。
				flusher.Flush()
			}
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Errorf("读上游失败: %v", err)
				return
			}
		}
	}))
	defer router.Close()

	resp, err := router.Client().Get(router.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Errorf("下游收到 Content-Length = %q，期望不存在（流式必须保持分块）", got)
	}
	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Errorf("下游 TransferEncoding = %v，期望 [chunked]", resp.TransferEncoding)
	}
	if resp.ContentLength != -1 {
		t.Errorf("下游 ContentLength = %d，期望 -1", resp.ContentLength)
	}
	if string(body) != "data: one\n\ndata: two\n\n" {
		t.Errorf("下游响应体 = %q，期望完整的两条 SSE 事件", string(body))
	}
	// 响应头折叠：httpx 语义是逗号拼接，且逐跳头与长度头被剔除。
	if got := resp.Header.Get("X-Dup"); got != "first, second" {
		t.Errorf("下游 X-Dup = %q，期望 %q", got, "first, second")
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("下游 Cache-Control = %q，期望 %q", got, "no-cache")
	}
}

// TestStreamSurvivesWellPastFirstByteWindow 锁定「首字节窗口只管到响应头」。
//
// 参照实现的流式语义是 read=None（proxy_support.py:341）：响应头之后没有任何总时长
// 上限。首字节窗口若被误当成整请求超时，一条正常但缓慢的流会在窗口到点的瞬间被掐
// 断——而这时调用方已经拿到 200 了，只能表现为「流中途断掉」，很难追。
//
// 这里让响应头立刻到达、响应体拖到远超窗口之后才发完，断言能读全。
func TestStreamSurvivesWellPastFirstByteWindow(t *testing.T) {
	const chunks = 4
	// 窗口设得远小于整个响应体所需时间。
	firstByteWindow := 40 * time.Millisecond
	perChunkDelay := 60 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// 关键：WriteHeader 只是把响应头写进缓冲区，必须 Flush 才会真正发出去。
		// 不 Flush 的话响应头也要等到第一块 body 才走，这个测试就失去了意义。
		flusher.Flush()
		// 响应头已发出，响应体慢慢来。
		for i := 0; i < chunks; i++ {
			time.Sleep(perChunkDelay)
			if _, err := w.Write([]byte("data: x\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	client := New(Config{FirstByteTimeout: firstByteWindow})

	resp, err := client.DoStream(req)
	if err != nil {
		t.Fatalf("响应头应立刻到达、不该超时，实际: %v", err)
	}
	defer resp.Body.Close()

	// 整个响应体耗时约 chunks*perChunkDelay，远超 firstByteWindow。
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v（首字节窗口可能被误当成整请求超时）", err)
	}
	want := strings.Repeat("data: x\n\n", chunks)
	if string(body) != want {
		t.Fatalf("响应体 = %q，期望 %q", string(body), want)
	}
	if elapsed := chunks * perChunkDelay; elapsed <= firstByteWindow {
		t.Fatalf("测试前提不成立：响应体耗时 %v 未超过首字节窗口 %v", elapsed, firstByteWindow)
	}
}

// TestCancelBodyReleasesContextOnClose 直接锁住 cancelBody 的契约。
//
// 注意这是**白盒**测试，形态上依赖具体类型：从外部其实观察不到「子 context 有没有
// 被释放」——Close 无论有没有取消 context 都会立即返回（net/http 自己就会解开阻塞
// 中的读），所以黑盒断言（比如「Close 不阻塞」）根本抓不到这条回归。这里改成直接
// 断言 stop 被调用，并配合 TestDoStreamWrapsBodyToReleaseContext 断言 DoStream 确实
// 交出了这个包装体。
func TestCancelBodyReleasesContextOnClose(t *testing.T) {
	var calls int
	body := &cancelBody{
		ReadCloser: io.NopCloser(strings.NewReader("payload")),
		stop:       func() { calls++ },
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("读取内容 = %q，期望 %q", string(data), "payload")
	}
	if calls != 0 {
		t.Fatalf("读取期间 stop 被调用了 %d 次，期望 0 次（只有 Close 才该释放）", calls)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Close 后 stop 被调用 %d 次，期望恰好 1 次", calls)
	}
}

// TestDoStreamWrapsBodyToReleaseContext 断言 DoStream 交出的响应体带释放钩子。
//
// 回归形态：DoStream 直接返回裸的 resp.Body（漏掉 cancelBody 包装）。此时每条流式
// 响应的子 context 都要等到父 context 结束才释放——请求量一大就是持续的 context
// 与连接泄漏，而所有功能测试都照样通过。
func TestDoStreamWrapsBodyToReleaseContext(t *testing.T) {
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: one\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-serverDone:
		}
	}))
	defer func() {
		close(serverDone)
		server.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := New(Config{FirstByteTimeout: 5 * time.Second}).DoStream(req)
	if err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}
	defer resp.Body.Close()

	wrapped, ok := resp.Body.(*cancelBody)
	if !ok {
		t.Fatalf("DoStream 返回的响应体类型 = %T，期望 *cancelBody（否则子 context 不会随 Body.Close 释放）", resp.Body)
	}
	if wrapped.stop == nil {
		t.Fatal("cancelBody.stop 为 nil：关闭响应体不会释放子 context")
	}
}

// TestStreamBodyCloseReleasesContext 锁定响应体关闭后不再泄漏 context。
//
// DoStream 的 cancel 挂在响应体上（client.go 的 cancelBody），必须读到 Body.Close
// 才触发。忘了挂或提前挂都会出问题：提前挂会掐断长流，忘了挂则每次流式请求都泄漏
// 一个 context 直到进程退出。
func TestStreamBodyCloseReleasesContext(t *testing.T) {
	// 服务端在客户端断开前一直挂住；用 Done 保证测试结束时 handler 一定会退出，
	// 否则 server.Close() 会等它，测试以自己的超时收场。
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: one\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-serverDone:
		}
	}))
	defer func() {
		close(serverDone)
		server.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := New(Config{FirstByteTimeout: 5 * time.Second}).DoStream(req)
	if err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}
	// 只读到第一块为止。缓冲区长度必须与写入的块等长：ReadFull 会一直等到读满，
	// 给多了就会挂在这里，看起来像被测代码的问题。
	firstChunk := "data: one\n\n"
	if _, err := io.ReadFull(resp.Body, make([]byte, len(firstChunk))); err != nil {
		t.Fatalf("读取首块失败: %v", err)
	}
	// Close 必须立即返回，不能挂在这里等超时。
	done := make(chan error, 1)
	go func() { done <- resp.Body.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Body.Close 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Body.Close 未在 3 秒内返回：cancel 可能没挂在响应体上")
	}
}

// TestFirstByteTimeoutClassifiedAsTimeout 锁定首字节窗口到点被归类为超时。
//
// 这是重试策略的分支点：参照实现专门定义了 UpstreamFirstByteTimeout
// （proxy_support.py:21-26、375-379），并在 proxy_handler.py:569 用它决定
// 「不要用同一个 key 重试」——原地重试只会把等待时间乘以重试次数。若这里退化成
// 通用错误，重试策略就会打错分支。
func TestFirstByteTimeoutClassifiedAsTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先不发响应头；客户端断开（context 取消）或测试放行后才返回，避免
		// server.Close() 被这个 handler 拖住。
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	client := New(Config{FirstByteTimeout: 150 * time.Millisecond})

	started := time.Now()
	_, err = client.DoStream(req)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("期望首字节超时，实际成功")
	}
	if !errors.Is(err, ErrFirstByteTimeout) {
		t.Fatalf("错误 = %v，期望 errors.Is(err, ErrFirstByteTimeout) 成立", err)
	}
	var timeoutErr *FirstByteTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Errorf("错误类型 = %T，期望 *FirstByteTimeoutError", err)
	}
	if cause := Classify(err); cause != CauseTimeout {
		t.Fatalf("Classify = %v，期望 %v（重试策略依赖这个分支）", cause, CauseTimeout)
	}
	// proxy_handler.py:569 的 retryable_same_key=not isinstance(exc,
	// UpstreamFirstByteTimeout)：首字节超时**不能**原地重试。
	if cause := Classify(err); cause.RetryableSameKey() {
		t.Error("首字节超时不应原地用同一个 key 重试")
	}
	// 窗口是 150ms，不该被别的超时误伤成「整请求超时」之外的东西。
	if elapsed > 2*time.Second {
		t.Errorf("耗时 %v，远超 150ms 的首字节窗口", elapsed)
	}

	// 整请求超时（Do 路径）同样归到 CauseTimeout。
	req2, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if _, err := New(Config{RequestTimeout: 100 * time.Millisecond}).Do(req2); err == nil {
		t.Fatal("期望整请求超时，实际成功")
	} else if cause := Classify(err); cause != CauseTimeout {
		t.Errorf("整请求超时 Classify = %v，期望 %v", cause, CauseTimeout)
	}
}

// TestClosedPortClassifiedAsConnectionRefused 锁定「连接被拒 ≠ 超时」。
//
// 参照实现靠 httpx.RequestError 的实例判断来区分两者（proxy_handler.py:547-570）：
// 连接被拒值得换 key 立刻重试，首字节超时则不值得。若这里错判成超时，路由器会在
// 上游端口没监听时白白不重试。
func TestClosedPortClassifiedAsConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("关闭监听失败: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/models", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	_, err = New(Config{RequestTimeout: 5 * time.Second}).Do(req)
	if err == nil {
		t.Fatal("期望连接被拒，实际成功")
	}

	cause := Classify(err)
	if cause == CauseTimeout {
		t.Fatalf("Classify = %v，连接被拒绝不能算超时（错误: %v）", cause, err)
	}
	if cause != CauseConnection {
		t.Fatalf("Classify = %v，期望 %v（错误: %v）", cause, CauseConnection, err)
	}
	if !cause.RetryableSameKey() {
		t.Error("连接被拒应当允许原地重试同一个 key")
	}
}

// TestClassifyTable 逐条锁定错误分类。
//
// 期望值不是推断的：每条都对应参照实现会走到的分支（proxy_handler.py:547-570 的
// httpx.RequestError 分支）。分类错一条，重试策略就打错一次。
func TestClassifyTable(t *testing.T) {
	timeoutErr := &net.DNSError{Err: "i/o timeout", Name: "up.example", IsTimeout: true}
	dnsErr := &net.DNSError{Err: "no such host", Name: "up.example", IsNotFound: true}
	refusedErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	cases := []struct {
		name string
		err  error
		want Cause
	}{
		{name: "nil 归为其它", err: nil, want: CauseOther},
		{name: "首字节超时哨兵", err: ErrFirstByteTimeout, want: CauseTimeout},
		{name: "包装后的首字节超时", err: &FirstByteTimeoutError{cause: context.Canceled}, want: CauseTimeout},
		{name: "context 到点", err: context.DeadlineExceeded, want: CauseTimeout},
		{name: "context 取消不算超时", err: context.Canceled, want: CauseOther},
		{name: "DNS 超时优先归为超时", err: timeoutErr, want: CauseTimeout},
		{name: "DNS 解析失败", err: dnsErr, want: CauseDNS},
		{name: "dial 失败", err: refusedErr, want: CauseConnection},
		{name: "无法归类", err: errors.New("something else"), want: CauseOther},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := Classify(item.err); got != item.want {
				t.Fatalf("Classify = %v，期望 %v", got, item.want)
			}
		})
	}

	// 只有超时不原地重试。
	if CauseTimeout.RetryableSameKey() {
		t.Error("超时不应原地重试同一个 key")
	}
	for _, cause := range []Cause{CauseConnection, CauseDNS, CauseTLS, CauseOther} {
		if !cause.RetryableSameKey() {
			t.Errorf("%v 应允许原地重试", cause)
		}
	}
}

// TestUpstreamURLJoinsLikePython 锁定路径拼接与 query 追加。
//
// 路径拼接对齐 proxy_support.py:304-305：
//
//	return f"{base_url.rstrip('/')}/{path.lstrip('/')}"
//
// 期望值全部由**真实 Python _join_url 跑出来**，不是照着实现推的（rstrip/lstrip
// 与 Go 的 TrimSuffix/TrimPrefix 差别就藏在「多个斜杠」这一档里，靠推理极易写错）。
func TestUpstreamURLJoinsLikePython(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		path  string
		query string
		want  string
	}{
		{name: "常规拼接", base: "https://up.example", path: "v1/chat/completions", want: "https://up.example/v1/chat/completions"},
		{name: "单侧各一个斜杠", base: "https://up.example/", path: "/v1/models", want: "https://up.example/v1/models"},
		{name: "两侧都是多个斜杠", base: "https://api.openai.com///", path: "///v1/x", want: "https://api.openai.com/v1/x"},
		{name: "base 多个斜杠、path 无斜杠", base: "https://api.openai.com///", path: "v1/x", want: "https://api.openai.com/v1/x"},
		{name: "base 无斜杠、path 多个斜杠", base: "https://api.openai.com", path: "///v1/x", want: "https://api.openai.com/v1/x"},
		{name: "四个斜杠", base: "https://up.example////", path: "////v1/models", want: "https://up.example/v1/models"},
		// Python 只 lstrip 路径，不 rstrip：路径末尾斜杠保留。
		{name: "路径末尾斜杠保留", base: "https://api.openai.com/", path: "v1/x/", want: "https://api.openai.com/v1/x/"},
		{name: "带前缀的 base", base: "https://up.example/api/", path: "v1/models", want: "https://up.example/api/v1/models"},
		{name: "附加 query", base: "https://up.example", path: "v1/models", query: "a&b=1", want: "https://up.example/v1/models?a&b=1"},
		{name: "空 query 不加问号", base: "https://up.example", path: "v1/models", query: "", want: "https://up.example/v1/models"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := UpstreamURL(item.base, item.path, item.query); got != item.want {
				t.Fatalf("UpstreamURL = %q，期望 %q", got, item.want)
			}
		})
	}
}

// TestUpstreamURLAcceptsHostileInput 记录本函数对畸形输入的**实际**行为，不做纠正。
//
// 参照实现的 _join_url 是纯字符串拼接，同样不校验、不纠正。这里只把行为钉住，
// 免得日后有人「顺手」加上规范化，反而与参照实现分叉。
func TestUpstreamURLAcceptsHostileInput(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		// 全是斜杠：rstrip 之后 base 为空，结果退化成绝对路径。
		{name: "base 全是斜杠", base: "///", path: "v1/x", want: "/v1/x"},
		{name: "path 全是斜杠", base: "https://up.example", path: "///", want: "https://up.example/"},
		// 空 path：拼接结果的路径部分退化成单个 "/"。
		{name: "空 path", base: "https://up.example", path: "", want: "https://up.example/"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := UpstreamURL(item.base, item.path, ""); got != item.want {
				t.Fatalf("UpstreamURL = %q，期望 %q", got, item.want)
			}
		})
	}
}

// TestUpstreamURLAgressWithProxysupportJoinURL 防止两份实现漂移。
//
// 仓库里已有一份等价的 JoinURL（internal/proxysupport/support.go:119-121），用
// TrimRight/TrimLeft。两份实现各自演进迟早会分叉，而分叉的表现是「某些上游的
// URL 多个斜杠」，不会报错、只会 404。
//
// internal/proxysupport 不依赖本包（它只依赖 canonical/config/keypool/protocol），
// 因此这里引用它不会形成导入环。
func TestUpstreamURLAgressWithProxysupportJoinURL(t *testing.T) {
	cases := []struct{ base, path string }{
		{"https://up.example", "v1/x"},
		{"https://up.example/", "/v1/x"},
		{"https://up.example///", "///v1/x"},
		{"https://up.example////", "////v1/x"},
		{"https://up.example/api/", "v1/x"},
		{"https://up.example", "v1/x/"},
		{"https://up.example/", "/"},
		{"https://up.example", ""},
		{"///", "v1/x"},
	}
	for _, item := range cases {
		t.Run(item.base+" + "+item.path, func(t *testing.T) {
			mine := UpstreamURL(item.base, item.path, "")
			theirs := proxysupport.JoinURL(item.base, item.path)
			if mine != theirs {
				t.Fatalf("两份拼接实现分叉：upstream.UpstreamURL = %q，proxysupport.JoinURL = %q",
					mine, theirs)
			}
		})
	}
}
