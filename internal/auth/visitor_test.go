//go:build !amkr_no_visitor

package auth

import (
	"net/http"
	"testing"
)

// 本文件承载「访客功能可用」形态下的正向断言。
//
// 必须带构建标签：关闭访客功能的形态下这些结论全部相反（正确的访客 key 也会被
// 拒绝），两类结论互斥，不能共存于同一文件。对应文件是 visitor_disabled_test.go。

// TestVisitorFeatureAvailableDefault 锁定默认编译形态下访客功能可用。
//
// 这是 /health 的 visitor_installed 字段来源，属于对外契约。
func TestVisitorFeatureAvailableDefault(t *testing.T) {
	if !VisitorFeatureAvailable() {
		t.Fatal("默认编译下访客功能应可用")
	}
}

// TestIsVisitorAPIKeyMatchesPython 锁定访客 key 的精确匹配。
//
// 期望值来自对 auto_model_key_router.visitor.is_visitor_api_key 的实测调用。
func TestIsVisitorAPIKeyMatchesPython(t *testing.T) {
	cases := []struct {
		apiKey string
		want   bool
	}{
		{VisitorAPIKey, true},
		{"AMKR-VISITOR", false},
		{" " + VisitorAPIKey, false},
		{"", false},
		{"amkr_visitor", false},
		{VisitorAPIKey + "x", false},
		{"中文", false},
	}
	for _, item := range cases {
		if got := IsVisitorAPIKey(item.apiKey); got != item.want {
			t.Errorf("IsVisitorAPIKey(%q) = %v，期望 %v", item.apiKey, got, item.want)
		}
	}
}

// TestVisitorKeyGrantsVisitorMode 验证正确的访客 key 折算成受限权限。
func TestVisitorKeyGrantsVisitorMode(t *testing.T) {
	mode := ModeFromAPIKey(VisitorAPIKey, pythonLocalKey)
	if mode == nil {
		t.Fatal("正确的访客 key 不应被拒绝")
	}
	if *mode != ModeVisitor {
		t.Fatalf("访客 key 应折算成 visitor，实际 %q", *mode)
	}
	context := Context{Mode: *mode}
	if context.IsFull() || !context.VisitorOnly() {
		t.Fatalf("访客上下文不应被当作完整权限: %+v", context)
	}
}

// TestDefaultAuthorizerGrantsVisitorForVisitorKey 验证默认实现的访客分支。
func TestDefaultAuthorizerGrantsVisitorForVisitorKey(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://amkr.local/v1/models", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("x-api-key", VisitorAPIKey)

	context := DefaultAuthorizer(request, pythonLocalKey)
	if context == nil {
		t.Fatal("访客 key 应通过鉴权")
	}
	if !context.VisitorOnly() || context.IsFull() {
		t.Fatalf("应为受限权限，实际 %+v", context)
	}
}
