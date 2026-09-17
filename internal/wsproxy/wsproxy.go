package wsproxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// 关闭码。websocket_proxy.py:97-102 的三分支映射，客户端按它判定成败。
const (
	// CloseCodeNormal 对应状态码 < 400。
	CloseCodeNormal = 1000
	// CloseCodePolicy 对应 400 ≤ 状态码 < 500（含 401/403/404）。
	CloseCodePolicy = 1008
	// CloseCodeInternal 对应状态码 ≥ 500，也用于本侧的兜底失败。
	CloseCodeInternal = 1011
)

// HandshakeHeaders 是折算请求时必须剔除的 WebSocket 握手头。
//
// 移植 websocket_proxy.py:13-22 的 WEBSOCKET_HANDSHAKE_HEADERS。这六个头描述的是
// **下游这一跳**的协议协商，把它们带给上游会撒谎（`upgrade: websocket` 会让上游
// 以为自己在处理升级请求）。注意 authorization / x-api-key **不在这里**：它们是
// 下游凭据，参照实现刻意保留（websocket_proxy.py:57 之后由代理层决定怎么用）。
//
// 派生自 GET /v1/{path} 的 Upgrade 请求**可能**带 `content-length: 0`；它不在剔除
// 名单里，但会被 proxysupport.UpstreamHeaders 拦住（那一层负责 content-length）。
var HandshakeHeaders = map[string]struct{}{
	"connection":               {},
	"sec-websocket-extensions": {},
	"sec-websocket-key":        {},
	"sec-websocket-protocol":   {},
	"sec-websocket-version":    {},
	"upgrade":                  {},
}

// ProxyHandler 与参照实现的 ProxyHandler（websocket_proxy.py:12）对应：给定路由
// 参数与折算出的请求，把响应写进 ResponseWriter。
//
// internal/proxy.Handler 的方法值正好是这个签名（
// `func (h *Handler) Handle(w http.ResponseWriter, request *http.Request, path string)`），
// 所以装配时不需要适配层：`wsproxy.ProxyHandler(handler.Handle)`。
//
// 与参照实现一致，本包不关心它内部怎么选 key、怎么改写模型；上游的路径与头部过滤
// 发生在它内部（internal/proxy 走的就是 proxysupport）。
type ProxyHandler func(w http.ResponseWriter, request *http.Request, path string)

// FilterHandshakeHeaders 复制一份握手头并剔除 WebSocket 握手头。
//
// 移植 websocket_proxy.py:57。参照实现是「整个 scope 复制一份、只替换 headers」，
// 所以这里也返回**新** map：调用方（以及 ProxyHandler）改它不应污染握手请求。
// 头部名按 Go 的规范化键处理，比较时统一转小写。
func FilterHandshakeHeaders(handshake http.Header) http.Header {
	filtered := make(http.Header, len(handshake))
	for name, values := range handshake {
		if _, drop := HandshakeHeaders[strings.ToLower(name)]; drop {
			continue
		}
		filtered[name] = append([]string(nil), values...)
	}
	return filtered
}

// Path 还原 FastAPI 路由参数 `{path:path}` 的取值，即去掉前导 "/v1/"。
//
// 参照实现的路由是 @app.websocket("/v1/{path:path}")，Starlette 的 path 转换器
// 匹配 `.*`，于是：
//
//	/v1/chat/completions -> "chat/completions"
//	/v1/                  -> ""            （实测）
//	/v1/a/b/c             -> "a/b/c"
//	/v1/a%2Fb             -> "a/b"         （URL 是**解码后**的，实测）
//
// 用 u.Path（解码后）而不是 u.EscapedPath()，正是最后一条的原因。
func Path(u *url.URL) string {
	if u == nil {
		return ""
	}
	path := u.Path
	if strings.HasPrefix(path, "/v1/") {
		return path[len("/v1/"):]
	}
	// 路由不会匹配到 /v1 本身；这里只是不让调用方拿到带前缀的路径。
	return strings.TrimPrefix(strings.TrimPrefix(path, "/v1"), "/")
}

// SchemeFromWebSocket 复刻 websocket_proxy.py:56 的映射：wss → https，其余 → http。
//
// 参照实现读的是 Starlette 的 `websocket.url.scheme`（取值 "ws"/"wss"）。Go 侧没有
// 这个字段，等价信息是握手请求是否走 TLS，因此 Scheme 把它折算成 "ws"/"wss" 之后
// 仍走这一个函数——映射规则只此一份。
func SchemeFromWebSocket(scheme string) string {
	if scheme == "wss" {
		return "https"
	}
	return "http"
}

// Scheme 从握手请求推断折算请求的 scheme。
//
// Go 侧没有 Starlette 的 scope["scheme"]，等价信息是请求是否走 TLS。反向代理场景下
// 真实协议可能写在 X-Forwarded-Proto 里；那属于装配方的判断，所以 SynthesizeRequest
// 单独收 scheme 参数，本函数只是默认值来源。
func Scheme(request *http.Request) string {
	if request != nil && request.TLS != nil {
		return SchemeFromWebSocket("wss")
	}
	return SchemeFromWebSocket("ws")
}

// SynthesizeRequest 把 WS 握手折算成一条等价的 HTTP 请求，移植
// websocket_proxy.py:51-67。
//
// 参照实现复制整个 ASGI scope 后只改四处（type/http_version/method/scheme），因此
// path、query_string 都沿用握手那一份——这解释了一个容易看漏的行为：**查询串会被
// 带进上游 URL**（tests/test_app.py:485 的 `?trace=1` 就依赖它）。
//
// Go 侧对应地保留 URL 原样（含 RawQuery），只固定 method=POST 与 scheme。请求体
// 由 WS 帧提供，ContentLength 设为长度（参照实现走 ASGI 的 receive 通道、scope 里
// 没有 content-length，但这一层在 Go 里必须显式给，且它本来就会被上游头部过滤剔除）。
func SynthesizeRequest(scheme string, target *url.URL, handshake http.Header, body []byte) *http.Request {
	requestURL := &url.URL{Scheme: scheme, Path: "/"}
	if target != nil {
		copied := *target
		requestURL = &copied
	}
	// scheme 落在 URL.Scheme 上：它是 Go 侧对 scope["scheme"] 的直接对应物，下游
	// 处理器能像读 request.url.scheme 一样读到它。
	requestURL.Scheme = scheme
	request := &http.Request{
		Method:        http.MethodPost,
		URL:           requestURL,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        FilterHandshakeHeaders(handshake),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	if target != nil {
		// Go 把 Host 单独存放；参照实现是把它留在 headers 里（scope["headers"]）。
		// 两者对上游都不可观测：proxysupport.UpstreamHeaders 会剔除 host。
		request.Host = target.Host
	}
	return request
}

// RequestFromHandshake 是 SynthesizeRequest 的便捷包装：scheme 取自握手请求。
func RequestFromHandshake(handshake *http.Request, body []byte) *http.Request {
	if handshake == nil {
		return SynthesizeRequest("http", nil, nil, body)
	}
	return SynthesizeRequest(Scheme(handshake), handshake.URL, handshake.Header, body)
}

// CloseCode 把响应状态码映射成关闭码，移植 websocket_proxy.py:97-102。
//
// 三段分界正好是 400 与 500，与 HTTP 的错误语义一致：客户端拿到 1008 就知道是
// 自己的请求有问题（401/403/404/429），拿到 1011 则知道是上游或本侧的故障。
func CloseCode(statusCode int) int {
	if statusCode < 400 {
		return CloseCodeNormal
	}
	if statusCode < 500 {
		return CloseCodePolicy
	}
	return CloseCodeInternal
}

// Handler 处理一条升级到 /v1/{path} 的 WebSocket 连接。
type Handler struct {
	// Proxy 是执行上游往返的一方。为 nil 时保留连接但立即以 1011 关闭（配置错误）。
	Proxy ProxyHandler
	// Logger 为 nil 时不记录。
	Logger *slog.Logger
}

// ServeHTTP 接受升级并处理这一条请求。
//
// 与参照实现的两处实现细节：
//
//   - **InsecureSkipVerify**：FastAPI 默认不校验 Origin，coder/websocket 默认校验。
//     不关掉它，浏览器之外的客户端（脚本、测试）会因为缺 Origin 或跨源被 403 拒掉，
//     那就与参照实现不同了。这里的信任模型与参照实现一致：凭据来自首帧/握手头，
//     由代理层校验。
//   - **一次连接一条请求**：处理完即关闭，不循环（websocket_proxy.py:29-40）。
func (h *Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	conn, err := websocket.Accept(w, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		// Accept 失败时它已经写过 HTTP 错误响应（不是 WS 关闭帧），这里只记日志。
		h.log().Debug("websocket proxy accept failed", "error", err)
		return
	}
	// 兜底：正常路径会在 Serve 里 Close；这条 defer 只负责「读失败/panic 之后」
	// 把底层连接收掉（对应参照实现的 except 分支）。
	defer func() { _ = conn.CloseNow() }()
	h.Serve(request.Context(), conn, request)
}

// Serve 在已升级的连接上处理一条请求。handshake 是升级前的 HTTP 请求（提供 URL、
// 查询串与头部），也是 scheme 的来源。
//
// 返回时不保证连接已关闭——调用方负责兜底关闭（ServeHTTP 用 CloseNow）。
func (h *Handler) Serve(ctx context.Context, conn *websocket.Conn, handshake *http.Request) {
	if h.Proxy == nil || handshake == nil {
		_ = conn.Close(websocket.StatusCode(CloseCodeInternal), "")
		return
	}
	// 参照实现的 except Exception 分支（websocket_proxy.py:43-48）：任何异常都记
	// 日志后关 1011。Go 里对应 panic（Python 的 Exception 覆盖不到 BaseException，
	// 所以这是**有意**比参照实现更宽一点）。
	defer func() {
		if recovered := recover(); recovered != nil {
			h.log().Error("websocket proxy failed", "panic", recovered)
			_ = conn.Close(websocket.StatusCode(CloseCodeInternal), "")
		}
	}()

	// 参照实现先收一帧；收到 disconnect 时直接 return（websocket_proxy.py:30-32）。
	// coder/websocket 把「对端关闭」表现为 Read 的 error，因此这里同样直接返回：
	// 对端已经走了，再关一次没有意义，而且客户端也看不到。
	_, frame, err := conn.Read(ctx)
	if err != nil {
		return
	}

	request := RequestFromHandshake(handshake, frame)
	captured := newCapture(ctx, conn)
	h.Proxy(captured, request, Path(handshake.URL))

	if err := captured.finish(); err != nil {
		// 与参照实现一致：发送阶段失败也算「代理失败」，关 1011。
		h.log().Error("websocket proxy failed", "path", Path(handshake.URL), "error", err)
		_ = conn.Close(websocket.StatusCode(CloseCodeInternal), "")
		return
	}
	_ = conn.Close(websocket.StatusCode(CloseCode(captured.statusCode())), "")
}

func (h *Handler) log() *slog.Logger {
	if h.Logger == nil {
		return slog.New(discardHandler{})
	}
	return h.Logger
}

// discardHandler 是 Logger 为 nil 时的空处理器。
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
