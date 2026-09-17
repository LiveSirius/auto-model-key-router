package auth

import (
	"net/http"
	"testing"
)

// 访客功能已按产品决策**取消开关语义**：不再有「功能是否可用」的判定，也不再提供
// VisitorFeatureAvailable()。原实现用构建标签 `amkr_no_visitor` 裁剪该功能，该机制
// 与其配套文件（visitor.go / visitor_accessor.go / visitor_disabled.go /
// visitor_disabled_test.go）已一并删除。
//
// 为什么取消：Python 侧拿「能否 import itsdangerous」当运行期标记，是打包系统的局限
// 所迫而非语义标记（visitor.py:9-17）；Go 静态编译本就不存在「可选依赖恰好缺席」。
// 而访客功能后续会持续丰富，留一个运行期/编译期开关只会让鉴权路径上多一处可被错误
// 配置影响的判断。
//
// 本文件因此不再带构建标签：访客的断言只有一种形态，不再需要两份互斥的测试文件。

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
