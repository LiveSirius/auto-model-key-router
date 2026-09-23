package upstream

import (
	"net/http"
	"testing"
)

// TestResponseHeadersCollapseLikeHttpx 锁定响应头的折叠规则。
//
// 这里对委托说明做了一处**实测修正**：说明写的是「Python 折叠成 dict，last-wins」，
// 但那只适用于**请求**方向。响应方向的折叠发生在 httpx 内部——`Headers.items()`
// 遍历时就把同名头用 ", " 拼成一个值（httpx 0.28.1 的 _models.py：
// `values_dict[str_key] += f", {str_value}"`），于是 proxy_support.py:331-335 的
// 字典推导式里根本没有重复键。实测：X-Dup: first / X-Dup: second →
// items() 得到 [('x-dup', 'first, second')]。
//
// 若按 last-wins 实现，下游会拿到 "second"、静默丢掉 "first"——与参照实现不一致
// 且不报错。所以这里断言逗号拼接。
func TestResponseHeadersCollapseLikeHttpx(t *testing.T) {
	cases := []struct {
		name string
		src  http.Header
		want map[string]string
		gone []string
	}{
		{
			name: "重复头逗号拼接（httpx items() 语义）",
			src:  http.Header{"X-Dup": {"first", "second"}},
			want: map[string]string{"X-Dup": "first, second"},
		},
		{
			name: "三值依次拼接",
			src:  http.Header{"X-Dup": {"a", "b", "c"}},
			want: map[string]string{"X-Dup": "a, b, c"},
		},
		{
			name: "单值原样保留",
			src:  http.Header{"X-One": {"only"}},
			want: map[string]string{"X-One": "only"},
		},
		{
			name: "逐跳头与长度头全部剔除",
			src: http.Header{
				"X-Keep":            {"yes"},
				"Content-Encoding":  {"gzip"},
				"Content-Length":    {"123"},
				"Transfer-Encoding": {"chunked"},
				"Connection":        {"keep-alive"},
			},
			want: map[string]string{"X-Keep": "yes"},
			gone: []string{"Content-Encoding", "Content-Length", "Transfer-Encoding", "Connection"},
		},
		{
			name: "头名大小写不敏感地剔除",
			src:  http.Header{"content-length": {"9"}, "CONTENT-ENCODING": {"br"}},
			want: map[string]string{},
			gone: []string{"Content-Length", "Content-Encoding"},
		},
		{
			name: "空占位值被丢弃",
			src:  http.Header{"X-Empty": {""}},
			want: map[string]string{},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := ResponseHeaders(item.src)
			for key, want := range item.want {
				if got.Get(key) != want {
					t.Errorf("%s = %q，期望 %q", key, got.Get(key), want)
				}
				if len(got.Values(key)) != 1 {
					t.Errorf("%s 有 %d 个值，期望恰好 1 个（下游按 dict 取值）",
						key, len(got.Values(key)))
				}
			}
			for _, key := range item.gone {
				if value := got.Get(key); value != "" {
					t.Errorf("%s 应被剔除，实际 = %q", key, value)
				}
			}
			if len(got) != len(item.want) {
				t.Errorf("头数量 = %d，期望 %d（got=%v）", len(got), len(item.want), got)
			}
		})
	}
}

// TestResponseHeadersPreservesAllDuplicateValues 防止有人把它"优化"成 last-wins。
//
// 单独一条是因为这正是委托说明里写错的那条：折叠掉前一个值不会报错，只会让下游
// 少看到一个头值。
func TestResponseHeadersPreservesAllDuplicateValues(t *testing.T) {
	got := ResponseHeaders(http.Header{"Set-Cookie": {"a=1", "b=2"}})
	value := got.Get("Set-Cookie")
	if value != "a=1, b=2" {
		t.Fatalf("Set-Cookie = %q，期望 %q（httpx items() 逗号拼接，不是 last-wins）", value, "a=1, b=2")
	}
}

// TestRequestHeadersCollapseLastWins 锁定请求头的折叠规则。
//
// 请求头取自 Starlette 的 request.headers.items()（proxy_support.py:319-323）：
// Starlette 的 Headers 是保留重复项的列表，配字典推导式即**后者覆盖前者**。实测
// starlette 1.2.1：{'x-dup': 'second'}。这与响应方向相反，是本文件两个测试并存
// 的原因。
func TestRequestHeadersCollapseLastWins(t *testing.T) {
	cases := []struct {
		name   string
		src    http.Header
		apiKey string
		want   map[string]string
		gone   []string
	}{
		{
			name:   "重复头后者覆盖前者",
			src:    http.Header{"X-Dup": {"first", "second"}},
			apiKey: "sk-test",
			want:   map[string]string{"X-Dup": "second"},
		},
		{
			name: "被屏蔽的请求头不会转发",
			src: http.Header{
				"Authorization":     {"Bearer downstream-key"},
				"X-Api-Key":         {"downstream"},
				"Host":              {"evil.example"},
				"Content-Length":    {"999"},
				"Destination-Addr":  {"cf"},
				"Anthropic-Version": {"2023-06-01"},
				"Anthropic-Beta":    {"prompt-caching"},
				"Accept-Encoding":   {"br"},
				"X-Custom":          {"keep"},
			},
			apiKey: "sk-upstream",
			want: map[string]string{
				"X-Custom":          "keep",
				"Authorization":     "Bearer sk-upstream",
				"Accept-Encoding":   "identity",
				"Host":              "",
				"X-Api-Key":         "",
				"Anthropic-Version": "",
				"Anthropic-Beta":    "",
				"Destination-Addr":  "",
			},
		},
		{
			name:   "空 api key 也照常发 Bearer",
			src:    http.Header{},
			apiKey: "",
			want:   map[string]string{"Authorization": "Bearer ", "Accept-Encoding": "identity"},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := CopyRequestHeaders(item.src, item.apiKey)
			for key, want := range item.want {
				if got.Get(key) != want {
					t.Errorf("%s = %q，期望 %q", key, got.Get(key), want)
				}
			}
			// 请求方向必须**恰好一个**值：Python 的 dict 推导式不会留下多值。
			for key := range got {
				if len(got.Values(key)) != 1 {
					t.Errorf("%s 有 %d 个值，期望 1 个（Python dict 语义）",
						key, len(got.Values(key)))
				}
			}
			if got.Get("Accept-Encoding") != "identity" {
				t.Error("Accept-Encoding 必须被强制为 identity（proxy_support.py:325）")
			}
			if got.Get("Authorization") != "Bearer "+item.apiKey {
				t.Errorf("Authorization = %q，期望 %q", got.Get("Authorization"), "Bearer "+item.apiKey)
			}
		})
	}
}

// TestCopyRequestHeadersScopeIsForwardingPathOnly 锁定本函数的适用范围。
//
// 参照实现的分布：代理**转发**路径恒为 accept-encoding=identity（即本函数），而原生
// 端点能力探测路径自建 headers（proxy_support.py:494-505），httpx 会补上默认的
// "gzip, deflate"，不经 _upstream_headers。
//
// 这条测试的作用是防止有人把本函数改成「跟随调用方传入的 accept-encoding」——那会让
// 转发路径丢掉强制 identity 的既定契约。
func TestCopyRequestHeadersScopeIsForwardingPathOnly(t *testing.T) {
	// 即便调用方传入 gzip，转发路径也必须强制成 identity。
	for _, incoming := range []string{"gzip, deflate", "br", "identity", ""} {
		got := CopyRequestHeaders(http.Header{"Accept-Encoding": {incoming}}, "sk-x")
		if value := got.Get("Accept-Encoding"); value != "identity" {
			t.Errorf("传入 %q 时 Accept-Encoding = %q，转发路径必须恒为 identity",
				incoming, value)
		}
	}
}

// TestIsSSEMediaType 复刻 _is_sse_media_type（proxy_handler.py:1151-1155）。
func TestIsSSEMediaType(t *testing.T) {
	cases := []struct {
		mediaType string
		want      bool
	}{
		{mediaType: "text/event-stream", want: true},
		{mediaType: "text/event-stream; charset=utf-8", want: true},
		{mediaType: "TEXT/EVENT-STREAM", want: true},
		{mediaType: "  text/event-stream  ", want: true},
		{mediaType: "text/event-stream;charset=utf-8", want: true},
		{mediaType: "application/json", want: false},
		{mediaType: "", want: false},
		{mediaType: "text/plain; text/event-stream", want: false},
	}
	for _, item := range cases {
		if got := IsSSEMediaType(item.mediaType); got != item.want {
			t.Errorf("IsSSEMediaType(%q) = %v，期望 %v", item.mediaType, got, item.want)
		}
	}
}
