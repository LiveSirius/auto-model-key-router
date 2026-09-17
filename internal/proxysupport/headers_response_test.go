package proxysupport

import (
	"testing"
)

// TestResponseHeadersMatchesPython 锁定响应头过滤、键小写化与同名头拼接。
//
// 期望值来自对参照实现的实测（用真实 httpx.Response 调 _response_headers）：
//
//	httpx.Response(200, headers=[("X-Multi","first"),("X-Multi","second"),
//	                             ("X-Multi","third"),("Content-Type","application/json")])
//	  .headers.items()      -> [('x-multi','first, second, third'), ('content-type','application/json')]
//	  _response_headers(...) -> {'x-multi': 'first, second, third', 'content-type': 'application/json'}
//
// 两个要点都反直觉，且都曾是本包的错误实现：
//
//  1. 同名头是**用 ", " 按序拼接**，不是后者覆盖。若按 last-wins 实现，下游会静默丢掉
//     前几个值（例如多个 Set-Cookie 只剩最后一个）。
//  2. 键被 httpx **统一成小写**（X-Multi -> x-multi），不是保留原始大小写。
//
// 请求侧方向相反（见 TestUpstreamHeadersMatchesPython）：那边保留原始大小写、同大小写下
// 后者胜。两处刻意分成两个测试，因为把它们当成同一种行为正是最初的错误来源。
func TestResponseHeadersMatchesPython(t *testing.T) {
	got := ResponseHeaders(map[string][]string{
		"Content-Encoding":  {"gzip"},
		"Content-Length":    {"5"},
		"Transfer-Encoding": {"chunked"},
		"Connection":        {"keep-alive"},
		"Content-Type":      {"application/json"},
		"X-Req-Id":          {"r1"},
	})
	want := map[string]string{"content-type": "application/json", "x-req-id": "r1"}
	if len(got) != len(want) {
		t.Fatalf("响应头数量不符: 期望 %d，实际 %d (%v)", len(want), len(got), got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("响应头 %q = %q，期望 %q", key, got[key], value)
		}
	}
	// 键必须是小写：原始大小写不应出现。
	if _, exists := got["Content-Type"]; exists {
		t.Error("响应头键应统一小写，不应保留原始大小写")
	}
	// 同名头按序拼接，不是后者覆盖。
	dup := ResponseHeaders(map[string][]string{"X-Multi": {"first", "second", "third"}})
	if dup["x-multi"] != "first, second, third" {
		t.Fatalf("同名响应头应逗号拼接，实际 %q", dup["x-multi"])
	}
	// 被屏蔽的头即使出现多次也必须整个消失（大小写不敏感）。
	blocked := ResponseHeaders(map[string][]string{
		"Content-Length": {"5"},
		"content-length": {"9"},
		"X-K":            {"v"},
	})
	if len(blocked) != 1 || blocked["x-k"] != "v" {
		t.Fatalf("被屏蔽头应全部消失，实际 %v", blocked)
	}
	if len(ResponseHeaders(nil)) != 0 {
		t.Fatal("空输入应返回空")
	}
}
