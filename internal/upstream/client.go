// Package upstream 复刻参照实现（httpx.AsyncClient）访问上游时的 HTTP 客户端行为。
//
// 这个包只有一个存在理由：Go 的 net/http 默认值与 httpx 不同，而且多数不同是
// **静默**的——不报错，只是上游看到的请求或下游收到的响应悄悄变了。本包把参照
// 实现里靠 httpx 默认值成立的行为逐条显式化：
//
//  1. 重定向：httpx 默认 follow_redirects=False（httpx/_client.py:197），Go 默认
//     最多跟 10 次 → 关掉（noRedirect）。
//  2. 压缩：Go 的 Transport 会自动补 Accept-Encoding: gzip 并透明解压；参照实现
//     显式发 identity（proxy_support.py:325），必须原样到达上游且不被解压
//     → DisableCompression。请求头侧的构造见 headers.go。
//  3. 连接池：httpx Limits(100/30/30)（app.py:45-47）；Go 的
//     MaxIdleConnsPerHost 默认只有 2，不显式设置就会把上游连接串行化。
//  4. 响应头：重复头折叠成单值、逐跳头剔除，否则下游拿到的头与参照实现不一致、
//     流式响应还会被塞上 Content-Length（见 headers.go）。
//  5. query：RawQuery 逐字转发，绝不过 url.Values 重编码（见 UpstreamURL）。
//  6. 超时：整请求超时与「首字节窗口」是两套语义，分别由 Do / DoStream 建模。
//  7. 错误分类：Go 没有 httpx.RequestError 那样的统一根类型，用 Classify 折算成
//     可分支的小集合（见本文件末尾）。
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// 连接池参数，逐条对齐 app.py:45-47 的
// httpx.Limits(max_connections=100, max_keepalive_connections=30, keepalive_expiry=30)。
const (
	// MaxConnections 是每宿主并发连接上限（httpx max_connections）。
	MaxConnections = 100
	// MaxKeepaliveConnections 是每宿主空闲连接上限（httpx max_keepalive_connections）。
	//
	// 这是本包最容易被忽略的一项：Go 的 MaxIdleConnsPerHost 默认值是 2，不显式
	// 设置的话并发上游请求会被静默串行化——没有报错，只是慢。
	MaxKeepaliveConnections = 30
	// KeepaliveExpiry 是空闲连接的存活时长（httpx keepalive_expiry）。
	KeepaliveExpiry = 30 * time.Second
)

// Config 是客户端行为配置。
//
// 两个超时对应参照实现的两套语义，**不要合并成一个**：
//
//   - RequestTimeout：非流式请求的整请求上限。参照实现用 config.request_timeout
//     构造客户端级 timeout（app.py:42-48、app.py:95），且非流式路径不覆盖它
//     （proxy_support.py:338-340 在 stream=False 时返回 None，即用客户端默认值）。
//   - FirstByteTimeout：流式请求「等到响应头」的上限，对应
//     proxy_handler.py:530-535 的 first_byte_deadline（由
//     config.stream_first_byte_timeout 派生），只在 is_stream 时存在。
//
// 首字节窗口**不是**整请求超时：参照实现在流式路径把 read 设为 None
// （proxy_support.py:341 的 httpx.Timeout(request_timeout, read=None)），响应体
// 没有总时长上限，空闲超时由 internal/runtime 的流式层单独负责。
//
// 零值字段按参照实现的默认值补齐（见 New），所以 Config{} 就是「和 httpx 一样」。
type Config struct {
	// RequestTimeout 为非正数时不设整请求超时。
	RequestTimeout time.Duration
	// FirstByteTimeout 为非正数时 DoStream 不设首字节窗口（等价 read=None）。
	FirstByteTimeout time.Duration
	// MaxConnections 为 0 时取 MaxConnections。
	MaxConnections int
	// MaxIdlePerHost 为 0 时取 MaxKeepaliveConnections。
	MaxIdlePerHost int
	// IdleConnTimeout 为 0 时取 KeepaliveExpiry。
	IdleConnTimeout time.Duration
}

// Client 是访问上游的 HTTP 客户端。
//
// 它满足 internal/runtime 的 HTTPClient 接缝（manager.go:31-33，只要求
// CloseIdleConnections），对应参照实现里 RuntimeResources.http_client
// （runtime.py:19）与关停时的 aclose()（runtime.py:131-132）。该断言写在
// client_test.go 里而不是这里：本包不需要在生产代码里依赖 internal/runtime，
// 放在测试里同样能在编译期锁住接缝，又不会给未来的反向依赖埋下导入环。
type Client struct {
	config    Config
	transport *http.Transport

	// whole 用于非流式请求：http.Client.Timeout 覆盖整条请求（含响应体读取）。
	whole *http.Client
	// stream 用于流式请求：**刻意不设** Client.Timeout，否则一条正常推进的长流
	// 会被整请求超时掐断；只由 DoStream 的首字节窗口约束「等到响应头」这一段。
	stream *http.Client
}

// New 构造客户端；Config 里的零值按参照实现的默认值补齐。
func New(config Config) *Client {
	if config.MaxConnections <= 0 {
		config.MaxConnections = MaxConnections
	}
	if config.MaxIdlePerHost <= 0 {
		config.MaxIdlePerHost = MaxKeepaliveConnections
	}
	if config.IdleConnTimeout <= 0 {
		config.IdleConnTimeout = KeepaliveExpiry
	}

	transport := &http.Transport{
		// httpx 的 trust_env 默认为 True（httpx/_client.py:201），即默认尊重
		// HTTP_PROXY / HTTPS_PROXY / NO_PROXY。Go 只有 http.DefaultTransport 自带
		// ProxyFromEnvironment，自己 new 出来的 Transport 不设就完全不走代理。
		Proxy: http.ProxyFromEnvironment,

		// 连接池（对齐 app.py:45-47）。Go 的默认 MaxIdleConnsPerHost=2 是反向的，
		// 不显式设置就会退化。
		//
		// ponytail: httpx 的 30 是**全局** keepalive 上限，Go 只有每宿主口径，
		// MaxIdleConns（全局）留 0=不限。路由器只面对少数几个上游宿主，每宿主 30
		// 已是这里最接近的对应物；若日后配了上百个上游宿主需要真正的全局上限，
		// 再把 MaxIdleConns 设成 30。
		MaxConnsPerHost:     config.MaxConnections,
		MaxIdleConnsPerHost: config.MaxIdlePerHost,
		IdleConnTimeout:     config.IdleConnTimeout,

		// 对应 httpx Transport 的 DisableCompression：既不自动补
		// Accept-Encoding，也不透明解压。Go 若插一层 gzip，上游看到的请求头和
		// 下游拿到的字节都会与参照实现不同，而且**不报错**。
		DisableCompression: true,

		// httpx 默认 http2=False（只走 HTTP/1.1），Go 的零值 Transport 会自动经
		// ALPN 协商 h2——上游看到的协议版本、以及响应是否 chunked 都会变。按
		// net/http 文档，给 TLSNextProto 一个非 nil 空表即可关掉自动 h2。
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}

	return &Client{
		config:    config,
		transport: transport,
		whole: &http.Client{
			Transport:     transport,
			CheckRedirect: noRedirect,
			// 整请求超时，对应 httpx 客户端级 timeout（app.py:42-48）。
			Timeout: config.RequestTimeout,
		},
		stream: &http.Client{
			Transport:     transport,
			CheckRedirect: noRedirect,
		},
	}
}

// noRedirect 复刻 httpx 的 follow_redirects=False。
//
// 返回 ErrUseLastResponse 让 http.Client 把 3xx **原样**交回调用方，而不是跟随。
// 这一点必须是显式的：Go 默认最多跟 10 次，跟完之后调用方只会看到最终响应，上游
// 的 3xx 被静默吞掉（连带把 Location 指向的第三方也访问了）。
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Do 发送非流式上游请求，整条请求（含响应体读取）受 Config.RequestTimeout 约束。
func (c *Client) Do(req *http.Request) (*http.Response, error) { return c.whole.Do(req) }

// DoStream 发送流式上游请求：只把「等到响应头」这一段限制在 Config.FirstByteTimeout
// 内，拿到响应头之后不再受本包任何超时约束。
//
// 为什么不用 http.Client.Timeout 或 context.WithTimeout：这两者一旦生效就会连
// 响应体一起掐断，而参照实现的流式语义恰恰是「响应体没有总时长上限」
// （proxy_support.py:341 的 read=None）。所以这里用可撤销的 time.AfterFunc——
// net/http 内部实现自己那个 Client.Timeout 用的是同一套写法——并在拿到响应头后
// **立刻**停表。
//
// 超时后返回 *FirstByteTimeoutError，对应参照实现的 UpstreamFirstByteTimeout
// （proxy_support.py:375-379）。调用方靠它决定「不要用同一个 key 重试」。
func (c *Client) DoStream(req *http.Request) (*http.Response, error) {
	if c.config.FirstByteTimeout <= 0 {
		return c.stream.Do(req)
	}

	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(c.config.FirstByteTimeout, func() { cancel(ErrFirstByteTimeout) })

	resp, err := c.stream.Do(req.WithContext(ctx))
	if err != nil {
		// 判定必须在 cancel(nil) 之前做：context.Cause 记的是第一次取消的原因，
		// 而 CancelCauseFunc(nil) 之后它仍是 ErrFirstByteTimeout——所以只能靠
		// 「现在是不是已经是它」来区分「窗口到点」和「别的传输错误」。
		timedOut := errors.Is(context.Cause(ctx), ErrFirstByteTimeout)
		cancel(nil)
		if timedOut {
			return nil, &FirstByteTimeoutError{cause: err}
		}
		return nil, err
	}

	// 响应头已到：立即停表，否则定时器会在 FirstByteTimeout 之后掐断一条仍在
	// 正常推进的流。cancel 则必须活到响应体读完，交给 body 包装器。
	if !timer.Stop() {
		// Stop 返回 false 表示定时器已经触发（或正在触发）。这是「响应头刚好在
		// 窗口边缘到达」的窄竞态：Do 已经成功返回，但 context 已经被取消，拿到的
		// 响应体是死的。
		//
		// 必须在这里挡住，而不是把死 body 交出去：调用方读到的是
		// context.Canceled，Classify 会归成 CauseOther，于是重试策略判定「可以
		// 原地重试同一个 key」——正好把首字节超时该有的分支走反，而且第一次读到
		// 的只是一条读不动的流，不报错。
		if errors.Is(context.Cause(ctx), ErrFirstByteTimeout) {
			_ = resp.Body.Close()
			cancel(nil)
			return nil, &FirstByteTimeoutError{cause: context.Cause(ctx)}
		}
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, stop: func() { cancel(nil) }}
	return resp, nil
}

// ErrFirstByteTimeout 是首字节窗口到点的哨兵错误。
//
// 单独一个哨兵（而不是复用 context.DeadlineExceeded）是为了让 Classify 能把
// 「上游慢到首字节超时」和「调用方自己的 context 到点」分开报——重试策略对前者
// 有专门分支（proxy_handler.py:569）。
var ErrFirstByteTimeout = errors.New("timed out waiting for upstream response headers")

// FirstByteTimeoutError 是首字节窗口到点时 DoStream 返回的错误。
//
// 保留底层错误（通常是 context.Canceled 包装的网络错误）便于诊断，但 Is 只认
// ErrFirstByteTimeout，这样调用方用 errors.Is 一条判断就能分支。
type FirstByteTimeoutError struct{ cause error }

// Error 实现 error。
func (e *FirstByteTimeoutError) Error() string {
	return ErrFirstByteTimeout.Error() + ": " + e.cause.Error()
}

// Unwrap 暴露底层错误。
func (e *FirstByteTimeoutError) Unwrap() error { return e.cause }

// Is 让 errors.Is(err, ErrFirstByteTimeout) 成立。
func (e *FirstByteTimeoutError) Is(target error) bool { return target == ErrFirstByteTimeout }

// Timeout 让 *FirstByteTimeoutError 满足 net.Error，与 *url.Error 的行为一致。
func (e *FirstByteTimeoutError) Timeout() bool { return true }

// Transport 返回底层 *http.Transport，供测试与观测断言连接池参数。
//
// 暴露它而不是加一堆 getter：连接的语义就在 Transport 上，测试直接断言真实字段
// 比断言一层转发更有意义。
func (c *Client) Transport() *http.Transport { return c.transport }

// CloseIdleConnections 关闭空闲连接，满足 internal/runtime 的 HTTPClient 接缝。
//
// 对应参照实现关停时的 http_client.aclose()（runtime.py:131-132）。Go 的
// http.Client 没有等价的「关掉整个连接池」方法，关闭空闲连接是最接近的语义：
// 在途请求不受影响，而这正是租约要保护的东西（manager.go:28-33）。
func (c *Client) CloseIdleConnections() { c.transport.CloseIdleConnections() }

// UpstreamURL 拼出上游 URL，query 逐字转发、绝不重编码。
//
// 路径拼接对齐 proxy_support.py:304-305 的 _join_url。query 部分刻意不做任何
// 处理：参照实现把 Starlette 的 query_params 交给 httpx 的 params=
// （proxy_support.py:355）；而在 Go 侧若走 url.Values.Encode() 会重排键序、把
// %20 变成 +、改写转义大小写，并丢掉没有 = 的裸键。这里按 URL.RawQuery 原样透传
// ——下游发什么字节，上游就收到什么字节。调用方直接把这个字符串交给
// http.NewRequest 即可（url.Parse 不重写 RawQuery 中的转义）。
func UpstreamURL(baseURL, path, rawQuery string) string {
	// TrimRight/TrimLeft 而不是 TrimSuffix/TrimPrefix：后者只去掉**一个**斜杠，而
	// 参照实现的 rstrip/lstrip 去掉**全部**。实测
	// `_join_url("https://api.openai.com///", "///v1/x")` = ".../v1/x"，用
	// TrimSuffix/TrimPrefix 会得到 ".../v1/x" 前面多三个斜杠的 URL。
	// 注意只 lstrip 路径、不 rstrip：路径末尾的斜杠是保留下来的
	// （`_join_url("https://a/", "v1/x/")` = "https://a/v1/x/"）。
	target := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	return target
}

// cancelBody 在响应体关闭时释放 DoStream 派生的 context。
//
// 不这么做的话，每条流式响应的子 context 都要等到父 context 结束才释放。
type cancelBody struct {
	io.ReadCloser
	stop func()
}

// Close 关闭响应体并释放子 context。
func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.stop()
	return err
}

// Cause 是 Go 错误折算出的上游失败类别。
//
// 存在理由是参照实现用 httpx.RequestError 这个统一根类型做 catch-all
// （proxy_handler.py:547、proxy_handler.py:638），Go 没有对应物：超时、连接被拒、
// DNS 失败散落在 *url.Error / *net.OpError / *net.DNSError 里。调用方的重试循环
// 需要的是「能不能原地重试同一个 key」这个判断，所以这里只提供它真正需要区分的
// 几个类别，而不是复刻一套错误树。
type Cause int

const (
	// CauseOther 表示无法归类（含 err 为 nil）。
	CauseOther Cause = iota
	// CauseTimeout 表示等上游等超时：首字节窗口、整请求超时或 context 到点。
	//
	// **不可用同一个 key 原地重试**（proxy_handler.py:569）：重试只会把等待时间
	// 乘以重试次数。
	CauseTimeout
	// CauseConnection 表示连接被拒或连接被重置/中断。
	CauseConnection
	// CauseDNS 表示域名解析失败。
	CauseDNS
	// CauseTLS 表示 TLS 握手或证书校验失败。
	CauseTLS
)

// String 返回类别的可读名，用于日志与错误消息。
func (c Cause) String() string {
	switch c {
	case CauseTimeout:
		return "timeout"
	case CauseConnection:
		return "connection"
	case CauseDNS:
		return "dns"
	case CauseTLS:
		return "tls"
	default:
		return "other"
	}
}

// RetryableSameKey 报告该类别是否值得用同一个 key 立刻重试。
//
// 只有这一条判断需要区分类别，其余（换 key、计冷却）由调用方按状态码处理。参照
// 实现的判断是「不是 UpstreamFirstByteTimeout 就原地重试」
// （proxy_handler.py:569、proxy_handler.py:660）。
func (c Cause) RetryableSameKey() bool { return c != CauseTimeout }

// Classify 把一次上游请求的错误折算成 Cause。
//
// 顺序有意义：超时排在最前（*url.Error 会同时满足 Timeout() 与其它特征），TLS 与
// DNS 都必须先于「连接」判断——证书错误在 Windows 上会带一层 ECONNRESET 外衣，
// 归到 CauseConnection 会让调用方误以为是上游拒连。
func Classify(err error) Cause {
	if err == nil {
		return CauseOther
	}
	if errors.Is(err, ErrFirstByteTimeout) || isTimeout(err) {
		return CauseTimeout
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return CauseDNS
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return CauseTLS
	}
	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &verifyErr) {
		return CauseTLS
	}
	var authorityErr x509.UnknownAuthorityError
	if errors.As(err, &authorityErr) {
		return CauseTLS
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return CauseTLS
	}
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &invalidErr) {
		return CauseTLS
	}

	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return CauseConnection
	}
	// 连接建立失败但底层 errno 已被 net 包抹平时（Windows 上常见），靠
	// *net.OpError 的 op 字段兜底：dial 失败就是「连不上」。
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return CauseConnection
	}
	return CauseOther
}

// isTimeout 报告错误是否为超时。
//
// 依次看：调用方 context 到点、net.Error.Timeout()（*url.Error 与 *net.OpError 都
// 实现了它）、以及 os.ErrDeadlineExceeded（SetReadDeadline 风格的超时不会带
// Timeout() 标记）。
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}
