//go:build amkr_no_visitor

package auth

import "testing"

// TestVisitorFeatureDisabledByBuildTag 验证 -tags amkr_no_visitor 真的关闭了访客功能。
//
// 这是该构建标签**唯一的证据**：没有这个用例，标签写错（拼错名字、条件写反）时会
// 静默地保持默认形态，而默认形态下访客 key 是可用的——一个本意关掉的能力其实还开着。
// 反向的用例在 visitor_test.go 里（默认编译下必须为 true）。
func TestVisitorFeatureDisabledByBuildTag(t *testing.T) {
	if VisitorFeatureAvailable() {
		t.Fatal("amkr_no_visitor 形态下访客功能应不可用")
	}
	// 功能关闭时，即便凭据完全正确也必须拒绝。
	if IsVisitorAPIKey(VisitorAPIKey) {
		t.Fatal("功能关闭时正确的访客 key 也必须被拒绝")
	}
	if mode := ModeFromAPIKey(VisitorAPIKey, pythonLocalKey); mode != nil {
		t.Fatalf("功能关闭时访客 key 不应获得任何权限，实际 mode=%q", *mode)
	}
	// 但本地 key 不受影响：关掉 visitor 不等于关掉鉴权。
	if mode := ModeFromAPIKey(pythonLocalKey, pythonLocalKey); mode == nil || *mode != ModeFull {
		t.Fatal("关闭访客功能不应影响本地 key 的完整权限")
	}
}
