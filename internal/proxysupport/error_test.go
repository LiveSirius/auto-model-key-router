package proxysupport

import (
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestStructuredUpstreamErrorMatchesPython 锁定 521 结构化错误模板。
//
// 上游返回非 JSON 体（Cloudflare 的 HTML 错误页）时用它生成可读错误。两个方言的
// 字段顺序不同：OpenAI 是 message/type/code/status_code/reason，Anthropic 的
// type 在 error 内部且值为 api_error。实测确认。
func TestStructuredUpstreamErrorMatchesPython(t *testing.T) {
	openai := canonical.DumpsOrdered(StructuredUpstreamError(521, false))
	wantOpenAI := `{"error":{"message":"上游服务不可用：Cloudflare 521 Web Server Is Down：Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。","type":"upstream_cloudflare_error","code":"cloudflare_521","status_code":521,"reason":"Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。"}}`
	if openai != wantOpenAI {
		t.Errorf("OpenAI 结构化错误不符\n 实际 %s\n 期望 %s", openai, wantOpenAI)
	}
	anthropic := canonical.DumpsOrdered(StructuredUpstreamError(521, true))
	wantAnthropic := `{"type":"error","error":{"type":"api_error","message":"上游服务不可用：Cloudflare 521 Web Server Is Down：Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。","code":"cloudflare_521","status_code":521,"reason":"Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。"}}`
	if anthropic != wantAnthropic {
		t.Errorf("Anthropic 结构化错误不符\n 实际 %s\n 期望 %s", anthropic, wantAnthropic)
	}
	// 没有模板的状态码返回 nil，交由调用方走通用兜底。
	for _, code := range []int{400, 404, 429, 500, 502} {
		if StructuredUpstreamError(code, false) != nil {
			t.Errorf("状态码 %d 不应有结构化模板", code)
		}
	}
}

// TestAnthropicErrorResponseMatchesPython 锁定 Anthropic 错误信封改写。
//
// 消息取值有三条来源，优先级为 error.message（**真值**才取）→ error 字符串 →
// 顶层 message，全取不到用兜底文案。注意 error.message 为空串时**不会**退到顶层
// message——参照实现用的是 `or`，实测确认。
func TestAnthropicErrorResponseMatchesPython(t *testing.T) {
	cases := []struct{ input, want string }{
		// 已是 Anthropic 形态：幂等，原样返回。
		{`{"type":"error","error":{"type":"api_error","message":"m"}}`,
			`{"type":"error","error":{"type":"api_error","message":"m"}}`},
		// error 是字符串。
		{`{"type":"error","error":"str-err"}`,
			`{"type":"error","error":{"type":"api_error","message":"str-err"}}`},
		// OpenAI 形态：取出 error.message。
		{`{"error":{"message":"inner"}}`,
			`{"type":"error","error":{"type":"api_error","message":"inner"}}`},
		{`{"error":"plainstr"}`,
			`{"type":"error","error":{"type":"api_error","message":"plainstr"}}`},
		// 只有顶层 message。
		{`{"message":"top"}`,
			`{"type":"error","error":{"type":"api_error","message":"top"}}`},
		// 空串是假值 -> 兜底，**不**退到顶层 message。
		{`{"error":{"message":""}}`,
			`{"type":"error","error":{"type":"api_error","message":"上游请求失败"}}`},
		{`{"error":{"message":null},"message":"fallback"}`,
			`{"type":"error","error":{"type":"api_error","message":"上游请求失败"}}`},
		{`{}`, `{"type":"error","error":{"type":"api_error","message":"上游请求失败"}}`},
		{`[1,2]`, `{"type":"error","error":{"type":"api_error","message":"上游请求失败"}}`},
		{`"juststring"`, `{"type":"error","error":{"type":"api_error","message":"上游请求失败"}}`},
	}
	for _, item := range cases {
		got := canonical.DumpsOrdered(AnthropicErrorResponse(mustValue(t, item.input)))
		if got != item.want {
			t.Errorf("AnthropicErrorResponse(%s)\n 实际 %s\n 期望 %s", item.input, got, item.want)
		}
	}
}

// TestJSONErrorResponseFromContentMatchesPython 锁定错误响应规范化的三条路径。
func TestJSONErrorResponseFromContentMatchesPython(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		content   string
		anthropic bool
		want      string
	}{
		{
			// 1. 可解析 JSON 且非 anthropic：原样透传。
			name: "合法 JSON 透传", status: 404, content: `{"error":{"message":"nf"}}`,
			want: `{"error":{"message":"nf"}}`,
		},
		{
			// 可解析 JSON 且 anthropic：改写信封。
			name: "合法 JSON 改写为 Anthropic 信封", status: 404,
			content: `{"error":{"message":"nf"}}`, anthropic: true,
			want: `{"type":"error","error":{"type":"api_error","message":"nf"}}`,
		},
		{
			// 2. 坏 JSON 且有 521 模板：用模板（优先于把 HTML 塞进 message）。
			name: "521 使用结构化模板", status: 521, content: `notjson`,
			want: `{"error":{"message":"上游服务不可用：Cloudflare 521 Web Server Is Down：Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。","type":"upstream_cloudflare_error","code":"cloudflare_521","status_code":521,"reason":"Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。"}}`,
		},
		{
			name: "521 anthropic 使用结构化模板", status: 521, content: `notjson`, anthropic: true,
			want: `{"type":"error","error":{"type":"api_error","message":"上游服务不可用：Cloudflare 521 Web Server Is Down：Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。","code":"cloudflare_521","status_code":521,"reason":"Cloudflare 无法连接到上游源站，通常表示源站服务离线、端口未监听或防火墙拒绝 Cloudflare 连接。"}}`,
		},
		{
			// 3. 坏 JSON 且无模板：用响应体文本。
			name: "无模板时用响应体文本", status: 400, content: `boom`,
			want: `{"error":{"message":"boom"}}`,
		},
		{
			// 空体 -> 兜底文案（含状态码）。
			name: "空体用兜底文案", status: 400, content: ``,
			want: `{"error":{"message":"上游返回 HTTP 400，且响应体为空"}}`,
		},
		{
			name: "空体 anthropic 用兜底文案", status: 400, content: ``, anthropic: true,
			want: `{"type":"error","error":{"type":"api_error","message":"上游返回 HTTP 400，且响应体为空"}}`,
		},
		{
			// 合法 JSON 但 anthropic 且非法形状 -> Anthropic 兜底文案。
			name: "anthropic 下无消息字段", status: 500, content: `{"a":1}`, anthropic: true,
			want: `{"type":"error","error":{"type":"api_error","message":"上游请求失败"}}`,
		},
		{
			name: "anthropic 下数组体", status: 500, content: `[1,2]`, anthropic: true,
			want: `{"type":"error","error":{"type":"api_error","message":"上游请求失败"}}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := canonical.DumpsOrdered(JSONErrorResponseFromContent(item.status, []byte(item.content), item.anthropic))
			if got != item.want {
				t.Errorf("不符\n 实际 %s\n 期望 %s", got, item.want)
			}
		})
	}
}

// TestJSONErrorResponseFromContentReplacesInvalidUTF8 锁定非法字节的宽容解码。
//
// 参照实现用 content.decode("utf-8", errors="replace")：非法字节变成 U+FFFD 而不是
// 抛异常。上游返回 gzip 残留或二进制错误页时，若不宽容就会让代理 500——明明是要
// 把上游错误转达给调用方，却变成自己的错误。
func TestJSONErrorResponseFromContentReplacesInvalidUTF8(t *testing.T) {
	got := canonical.DumpsOrdered(JSONErrorResponseFromContent(429, []byte{0xff, 0xfe}, false))
	// canonical 编码器用 ensure_ascii=False，U+FFFD 以**原始字节**输出而非 \u 转义，
	// 因此这里必须用解释型字符串（反引号是原始字符串，`\uFFFD` 会变成字面反斜杠）。
	want := `{"error":{"message":"` + "\uFFFD\uFFFD" + `"}}`
	if got != want {
		t.Fatalf("非法字节应解码为两个 U+FFFD（每个非法字节各一个）\n 实际 %s\n 期望 %s", got, want)
	}
}

// TestStructuredErrorTakesPrecedenceOverBodyText 顺序锁。
//
// 521 分支必须排在「用响应体文本」之前，否则 Cloudflare 的 HTML 错误页会被整段塞
// 进 message，调用方看到一坨标签页而拿不到有用信息。
func TestStructuredErrorTakesPrecedenceOverBodyText(t *testing.T) {
	html := []byte(`<html><body>521 Web Server Is Down</body></html>`)
	got := canonical.DumpsOrdered(JSONErrorResponseFromContent(521, html, false))
	if strings.Contains(got, "<html>") {
		t.Fatalf("521 应使用结构化模板而非透传 HTML: %s", got)
	}
	if !strings.Contains(got, "cloudflare_521") {
		t.Fatalf("521 应带结构化 code 字段: %s", got)
	}
}
