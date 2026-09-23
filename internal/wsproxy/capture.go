package wsproxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// capture 是捕获式 http.ResponseWriter，并实现 http.Flusher。
//
// # 为什么需要它
//
// 参照实现拿到的是一条已经构造好的 FastAPI Response：要么整体一个 body，要么是
// StreamingResponse 带一个 body_iterator，逐块 yield（websocket_proxy.py:70-83）。
// Go 侧的产出方是 http.Handler（例如 internal/proxy.Handler），它只认
// ResponseWriter，于是「响应」只能通过写操作现出来。这里的映射规则是：
//
//   - **一次 Flush = 一帧**（对应 StreamingResponse 的一个 chunk）；
//   - 收尾时若还有没 flush 的字节，作为**最后一帧**发出（对应普通 Response 的 body）；
//   - 从头到尾没写过任何字节 = 一帧都不发（对应 websocket_proxy.py:82 的
//     `if response.body:`）；
//   - Flush 出空缓冲**仍然发一个空帧**（对应流里的空 chunk，websocket_proxy.py:75
//     的 `send_text("")`，实测会真的发出去）。
//
// # 为什么自己实现而不是用 httptest.ResponseRecorder
//
// ResponseRecorder 把全部输出堆在内存里，即不知道 Flush，也无法在流式场景下逐块
// 转发；而本包的整个意义就是「边到边发」。这里只保留必要状态：状态码、头部、以及
// 自上次 Flush 以来累积的字节。
//
// 与 net/http 的约定保持一致的两处：重复 WriteHeader 被忽略；未 WriteHeader 直接
// Write 时状态码取 200。
type capture struct {
	ctx       context.Context
	conn      *websocket.Conn
	header    http.Header
	status    int
	wroteHead bool
	buffer    []byte
	err       error
}

// newCapture 构造一个绑定到给定连接的捕获器。
func newCapture(ctx context.Context, conn *websocket.Conn) *capture {
	return &capture{ctx: ctx, conn: conn, header: make(http.Header)}
}

// Header 返回响应头。头部**不会被下发**（参照实现只发 body 帧），它的唯一用途是
// 决定文本帧还是二进制帧，见 send。
func (c *capture) Header() http.Header { return c.header }

// WriteHeader 记录状态码。重复调用被忽略（net/http 的约定，也是参照实现里
// Response.status_code 只能被构造一次的性质）。
func (c *capture) WriteHeader(statusCode int) {
	if c.wroteHead {
		return
	}
	c.wroteHead = true
	c.status = statusCode
}

// Write 把字节追加进当前帧的缓冲。
//
// 出错后一律返回错误：Flush 没有 error 返回值，只能把首次失败记在 c.err 上，随后
// 的 Write 必须继续失败，否则处理器会以为数据已经发出去而继续干活。
func (c *capture) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if !c.wroteHead {
		c.WriteHeader(http.StatusOK)
	}
	c.buffer = append(c.buffer, p...)
	return len(p), nil
}

// Flush 发送当前缓冲作为一帧，并清空缓冲。
//
// 实现 http.Flusher（websocket_proxy.py 的 StreamingResponse 语义在 Go 侧的落点）。
func (c *capture) Flush() {
	if c.err != nil {
		return
	}
	c.err = c.send(c.buffer)
	c.buffer = c.buffer[:0]
}

// finish 在 ProxyHandler 返回后调用，发出最后一帧（若有未 flush 的字节）。
func (c *capture) finish() error {
	if c.err != nil {
		return c.err
	}
	if len(c.buffer) == 0 {
		return nil
	}
	return c.send(c.buffer)
}

// statusCode 返回响应状态码，未写入时为 200（net/http 的默认值）。
//
// 参照实现的 Response.status_code 恒有值（默认 200），因此这里也必须给 200——
// CloseCode(0) 会落进 <400 分支得到 1000，恰好也对，但不能依赖巧合。
func (c *capture) statusCode() int {
	if !c.wroteHead {
		return http.StatusOK
	}
	return c.status
}

// send 按 content-type 决定帧类型并发送。
//
// 判定表达式逐字移植 websocket_proxy.py:91：
//
//	content_type.startswith("text/") or "json" in content_type
//
// **两个比较都大小写敏感**，所以 `Content-Type: Application/JSON` 会走二进制帧
// （实测确认）。头部**名**的查找是大小写不敏感的（Starlette 的 Headers 如此），
// 由 headerValue 负责。
func (c *capture) send(data []byte) error {
	contentType := headerValue(c.header, "content-type")
	if strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") {
		// websocket_proxy.py:92：文本帧先 `decode("utf-8", errors="replace")` 再
		// send_text，也就是非法字节按最大子部分替换成 U+FFFD 后再编码回 UTF-8。
		// 不能直接发原始字节：文本帧按 RFC 6455 必须是合法 UTF-8，Python 也改了字节。
		return c.conn.Write(c.ctx, websocket.MessageText, []byte(decodeUTF8Replacing(data)))
	}
	return c.conn.Write(c.ctx, websocket.MessageBinary, data)
}

// headerValue 做大小写不敏感的头部查找。
//
// 先用 Header.Get（覆盖正常路径），失败后再自己扫一遍：http.Header 是裸 map，
// 直接赋值非规范键（`h["content-type"] = ...`）的处理器存在，而 Header.Get 只查
// 规范键，会静默查不到。Starlette 的 Headers 没有这个问题，所以这里补齐。
func headerValue(header http.Header, name string) string {
	if value := header.Get(name); value != "" {
		return value
	}
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// decodeUTF8Replacing 复刻 Python 的 `bytes.decode("utf-8", errors="replace")`。
//
// 与本仓库已有的两份逐字相同：internal/proxysupport/support.go:642 与
// internal/api/handlers_ops.go:672。两处都是私有实现，而本次迁移的约定是
// 「只新增自己的包」，不值得为了一个 helper 去导出别人的内部件——本任务更是明确
// 禁止改动那两个包。三份实现必须保持逐字一致，改动其中一份就要同步改另两份。
//
// 为什么不能图省事：
//   - strings.ToValidUTF8 会把**连续**非法字节折叠成一个 U+FFFD；
//   - 逐字节用 utf8.DecodeRune 会把 `e4 b8`（截断的 中）拆成两个 U+FFFD。
//
// Python 按 Unicode 的「最大子部分」（maximal subpart, Unicode 3.9 D93）每段各产
// 一个 U+FFFD。这里会真的出现在 WS 帧上：上游返回的 text/* 响应体带非法字节时，
// 客户端收到的文本与参照实现必须逐字一致。
func decodeUTF8Replacing(content []byte) string {
	const replacement = "\uFFFD"
	var sb strings.Builder
	sb.Grow(len(content))
	for i := 0; i < len(content); {
		b0 := content[i]
		if b0 < 0x80 {
			sb.WriteByte(b0)
			i++
			continue
		}
		// 首字节决定续字节个数，以及**第二个字节**的合法区间。区间约束不可省：它同时
		// 排除过长编码（C0/C1、E0 8x、F0 8x）、代理对（ED A0-BF）与超出 U+10FFFF
		//（F4 90-BF）。这些情形下最大子部分只有首字节本身，于是每个字节各算一个 U+FFFD。
		extra := 0
		var lo, hi byte = 0x80, 0xBF
		switch {
		case b0 >= 0xC2 && b0 <= 0xDF:
			extra = 1
		case b0 == 0xE0:
			extra, lo = 2, 0xA0
		case b0 >= 0xE1 && b0 <= 0xEC:
			extra = 2
		case b0 == 0xED:
			extra, hi = 2, 0x9F
		case b0 == 0xEE || b0 == 0xEF:
			extra = 2
		case b0 == 0xF0:
			extra, lo = 3, 0x90
		case b0 >= 0xF1 && b0 <= 0xF3:
			extra = 3
		case b0 == 0xF4:
			extra, hi = 3, 0x8F
		default:
			// 非法首字节：80-BF（续字节误作首字节）、C0/C1（过长）、F5-FF。
			sb.WriteString(replacement)
			i++
			continue
		}
		// 贪心吃掉最长的合法前缀：先校验第二个字节（区间收窄过），再校验其余续字节。
		j := i + 1
		if j < len(content) && content[j] >= lo && content[j] <= hi {
			j++
			for j < i+1+extra && j < len(content) && content[j] >= 0x80 && content[j] <= 0xBF {
				j++
			}
		}
		if j == i+1+extra {
			sb.Write(content[i:j])
		} else {
			// 一个最大子部分只产生一个 U+FFFD，然后从 j 继续（不吞掉后续子部分）。
			sb.WriteString(replacement)
		}
		i = j
	}
	return sb.String()
}
