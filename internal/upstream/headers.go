package upstream

import (
	"net/http"
	"net/textproto"
	"strings"
)

// 本文件负责请求头与响应头的构造/折损。两个方向的折叠规则**不同**，而且很容易
// 被想当然地写错，所以先说清楚（两者都用真实的 httpx 0.28.1 实测过）：
//
//   - **请求头**取自 Starlette 的 `request.headers.items()`（proxy_support.py:319-323）。
//     Starlette 的 Headers 是「原样保留重复项」的列表，配 Python dict 推导式，
//     结果就是**后者覆盖前者**（last-wins），且键全小写。
//   - **响应头**取自 httpx 的 `response.headers.items()`（proxy_support.py:331-335）。
//     httpx 的 items() 在遍历时就已经把同名头用 ", " **拼接**成一个值
//     （httpx/_models.py 的 `values_dict[str_key] += f", {str_value}"`），所以字典
//     推导式里根本不存在重复键，折叠结果是 "first, second" 而不是 "second"。
//
// 结论：last-wins 只适用于请求方向；响应方向必须复刻 httpx 的逗号拼接。若把响应
// 方向也做成 last-wins，下游拿到的头会与参照实现不同（丢掉前一个值），而这是静默
// 的——没有报错，只是少了一个值。

// blockedRequestHeaders 是转发上游时要剔除的请求头（proxy_support.py:309-318）。
//
// Authorization / x-api-key 剔除后换成路由器的 key；host 让 Go 按目标地址重算；
// content-length 由 net/http 依据实际 body 重算；accept-encoding 剔除后强制 identity；
// anthropic-version / anthropic-beta 由原生 messages 路径按需回填
// （proxy_handler.py:522-527）；destination-addr 是 Cloudflare 的私有头。
var blockedRequestHeaders = map[string]bool{
	"authorization":     true,
	"host":              true,
	"content-length":    true,
	"destination-addr":  true,
	"accept-encoding":   true,
	"x-api-key":         true,
	"anthropic-version": true,
	"anthropic-beta":    true,
}

// blockedResponseHeaders 是回给下游时要剔除的响应头（proxy_support.py:330）。
//
// content-encoding 必须剔除：body 已按 identity 取回，若把上游的 content-encoding
// 透传下去，下游会尝试解开一层并不存在的压缩。
//
// content-length 与 transfer-encoding 也剔除——这是「流式响应保持 chunked」的关键。
// 参照实现走 ASGI StreamingResponse，响应体长度在发头时未知，因此不可能有
// Content-Length。Go 的 http.Server 会在 handler 写完且总字节数较小时**自动补上**
// Content-Length，一旦补上，下游（尤其浏览器与 SSE 客户端）就会等到整个响应结束
// 才开始消费。剔除它并配合 Flush，响应才会保持分块传输。
var blockedResponseHeaders = map[string]bool{
	"content-encoding":  true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
}

// CopyRequestHeaders 复刻 _upstream_headers（proxy_support.py:308-326）。
//
// 重复头按 Starlette 语义折叠为**后者覆盖前者**；结果里必定带有
// `Authorization: Bearer {apiKey}` 与 `Accept-Encoding: identity`。
//
// Accept-Encoding: identity 是刻意强制的，不是可选优化：参照实现永远发 identity
// （tests/test_app.py:3454 直接断言了这一点），配合客户端侧的 DisableCompression，
// 上游返回什么字节、下游就收到什么字节，中间不会多出一层压缩。
//
// 范围说明：本函数只复刻 _upstream_headers，即**代理转发**路径。参照实现里还有一
// 组原生端点能力探测（proxy_support.py:474-529 的 test_native_messages_support 与
// proxy_support.py:534-576 的 responses 版本），它们自建 headers、不带
// accept-encoding，因此 httpx 会补上默认的 "gzip, deflate"。实测语料
// internal/upstream/testdata/python_upstream_fixtures.jsonl 正是这个分布：103 条
// 记录里 93 条 identity（转发路径）、10 条 gzip, deflate（探测路径）。探测属于
// 「能力缓存」职责、自带 10 秒超时（proxy_support.py:504），不在本包范围内；若日后
// 要移植，给探测请求显式设一个 Accept-Encoding 默认值即可。
func CopyRequestHeaders(src http.Header, apiKey string) http.Header {
	if src == nil {
		src = http.Header{}
	}
	out := make(http.Header, len(src)+2)
	// 以 key 为单位遍历，但 Python 的 dict 推导式是**按原始出现顺序**逐条覆盖，
	// 因此这里逐条遍历所有值，last-wins 才与参照实现一致。
	for key, values := range src {
		lower := strings.ToLower(key)
		if blockedRequestHeaders[lower] {
			continue
		}
		for _, value := range values {
			out.Set(key, value)
		}
	}
	out.Set("Authorization", "Bearer "+apiKey)
	out.Set("Accept-Encoding", "identity")
	return out
}

// ResponseHeaders 复刻 _response_headers（proxy_support.py:329-335）。
//
// 重复头按 httpx 语义用 ", " 拼接成单值（见文件头说明），并剔除逐跳头。返回的每个
// 键都只有**一个**值：下游按 dict 语义取值，多值会破坏 last-wins/拼接的一致性。
func ResponseHeaders(src http.Header) http.Header {
	out := make(http.Header, len(src))
	for key, values := range src {
		if blockedResponseHeaders[strings.ToLower(key)] {
			continue
		}
		// 跳过上游给的空占位值（Python 侧不存在这种形态，保留确定性行为）。
		kept := make([]string, 0, len(values))
		for _, value := range values {
			if value != "" {
				kept = append(kept, value)
			}
		}
		if len(kept) == 0 {
			continue
		}
		out[textproto.CanonicalMIMEHeaderKey(key)] = []string{strings.Join(kept, ", ")}
	}
	return out
}

// SetStreamingHeaders 复刻 _set_streaming_headers（proxy_handler.py:1025-1027）。
//
// cache-control: no-cache 阻止中间层缓存流；x-accel-buffering: no 是给 nginx 的
// 显式指令，否则它会缓冲整个 SSE 流后再一次性下发。
func SetStreamingHeaders(headers http.Header) {
	headers.Set("Cache-Control", "no-cache")
	headers.Set("X-Accel-Buffering", "no")
}

// IsSSEMediaType 复刻 _is_sse_media_type（proxy_handler.py:1151-1155）。
//
// 只看分号前的 media type，且大小写不敏感、两侧空白忽略；带参数
// （`text/event-stream; charset=utf-8`）也算。
func IsSSEMediaType(mediaType string) bool {
	base, _, _ := strings.Cut(mediaType, ";")
	return strings.EqualFold(strings.TrimSpace(base), "text/event-stream")
}
