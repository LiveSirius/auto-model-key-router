package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// breakingReader 先产出给定的字节，然后返回一个非 EOF 的错误。
type breakingReader struct {
	data []byte
	done bool
}

func (r *breakingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("上游连接中断")
	}
	r.done = true
	return copy(p, r.data), nil
}

func (r *breakingReader) Close() error { return nil }

// TestMidStreamErrorTerminatesSilently 固化「下游已开始收字节后永不重试，中途错误
// 静默终止 SSE、不发错误帧」（proxy_handler.py:1137、1299、1392）。
//
// 三件事一起断言，缺一条都会让行为看起来「变好了」但实际与参照实现不同：
//  1. 已经转发的完整事件仍然到达下游；
//  2. **没有**任何错误帧（`event: error` / 上游错误 JSON）被写下去；
//  3. 收尾照样执行：指标记 failed=true、key 在途计数归零、租约释放。
func TestMidStreamErrorTerminatesSilently(t *testing.T) {
	env := newTestEnv(t, simpleChatConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions", upstreamStep{
		Status:  200,
		Headers: map[string]string{"content-type": "text/event-stream"},
		Reader:  &breakingReader{data: []byte("data: {\"i\":1}\n\n")},
	})

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"vendor-model","messages":[],"stream":true}`, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码: got %d want 200（响应头已写出，不能再改）", recorder.Code)
	}
	body := recorder.Body.String()
	if body != "data: {\"i\":1}\n\n" {
		t.Fatalf("应转发已收到的完整事件且不发错误帧，实得 %q", body)
	}
	if strings.Contains(body, "error") {
		t.Fatalf("中途错误不得写错误帧，实得 %q", body)
	}

	records := env.metrics.take()
	if len(records) != 1 {
		t.Fatalf("指标行数: got %d want 1", len(records))
	}
	if !records[0].Failed {
		t.Fatalf("中途错误应记 failed=true，实得 %+v", records[0])
	}
	// 注意不断言 first_token_ms != 0：参照实现的哨兵就是 `if first_token_ms == 0`，
	// 因此一次亚毫秒就结束的流**合法地**记 0（下一个块才会真正写入）。断言
	// duration_ms 非负即可，不把「毫秒精度以下的流」误判成缺陷。
	if records[0].DurationMS < 0 {
		t.Fatalf("duration_ms 不应为负，实得 %d", records[0].DurationMS)
	}
	if got := env.pool.ActiveCount("vendor-model", "k1"); got != 0 {
		t.Fatalf("key 在途计数应归零，实得 %d", got)
	}
	if got := env.resources.ActiveLeases(); got != 0 {
		t.Fatalf("租约应释放，实得 %d", got)
	}
}

// TestMidStreamErrorDoesNotRetryOnAnotherKey 固化「一旦开始写下游就不再换 key 重试」。
//
// 多 key 配置下最容易被误实现的一点：上游中途断了，直觉上「还有别的 key，换一个
// 重试」——但那会把已经发给下游的半条 SSE 流接上另一条流，客户端拿到的是两条流
// 拼接后的垃圾。参照实现不重试。
func TestMidStreamErrorDoesNotRetryOnAnotherKey(t *testing.T) {
	cfg := simpleChatConfig()
	cfg.Models[0].Keys = append(cfg.Models[0].Keys, testKey("k2", "https://upstream.test"))
	cfg.MaxRetries = 1
	env := newTestEnv(t, cfg, Options{BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions", upstreamStep{
		Status:  200,
		Headers: map[string]string{"content-type": "text/event-stream"},
		Reader:  &breakingReader{data: []byte("data: {\"i\":1}\n\n")},
	})

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"vendor-model","messages":[],"stream":true}`, nil)

	if recorder.Body.String() != "data: {\"i\":1}\n\n" {
		t.Fatalf("下游字节: %q", recorder.Body.String())
	}
	if len(env.transport.calls) != 1 {
		t.Fatalf("下游已收到字节后不得重试，上游调用数应为 1，实得 %v",
			describeUpstreams(env.transport.calls))
	}
}

// TestAnthropicStreamMidStreamErrorEmitsNoErrorFrame 覆盖转换流的同一语义。
//
// Anthropic 重建流在失败前已经发出 message_start，中途错误同样只静默终止：
// 不补 content_block_stop、不发 message_delta / message_stop。
func TestAnthropicStreamMidStreamErrorEmitsNoErrorFrame(t *testing.T) {
	env := newTestEnv(t, simpleChatConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions", upstreamStep{
		Status:  200,
		Headers: map[string]string{"content-type": "text/event-stream"},
		Reader: &breakingReader{data: []byte(
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")},
	})

	recorder := env.request(http.MethodPost, "messages",
		`{"model":"vendor-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		nil)

	body := recorder.Body.String()
	if !strings.HasPrefix(body, "event: message_start\n") {
		t.Fatalf("应先发出 message_start，实得 %q", body)
	}
	if !strings.Contains(body, "text_delta") {
		t.Fatalf("应转发已收到的文本增量，实得 %q", body)
	}
	for _, forbidden := range []string{"message_stop", "message_delta", "content_block_stop"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("中途错误后不得补 %s，实得 %q", forbidden, body)
		}
	}
	if got := env.resources.ActiveLeases(); got != 0 {
		t.Fatalf("租约应释放，实得 %d", got)
	}
}

// 编译期哨兵：确保 io 的引用不被误删（Reader 字段的类型）。
var _ io.Reader = (*breakingReader)(nil)

// 编译期哨兵：确保 httptest 的引用不被误删（本文件只用它的请求构造器）。
var _ = httptest.NewRequest
