package proxysupport

import (
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestDecodeUTF8ReplacingIsValidUTF8 验证输出始终是合法 UTF-8。
//
// 这是函数的存在意义：非法输入不能带着非法字节继续往 JSON 编码器里走，否则
// encoding/json 会替换、而 canonical 编码器会原样输出，两边行为分叉。
func TestDecodeUTF8ReplacingIsValidUTF8(t *testing.T) {
	for _, raw := range []string{"fffe", "ffffff", "f4908080", "eda080", "e4b8", "c080", "80ff"} {
		got := decodeUTF8Replacing(hexToBytes(t, raw))
		if !utf8.ValidString(got) {
			t.Errorf("输入 %s 的输出不是合法 UTF-8: %q", raw, got)
		}
	}
}

// TestDecodeUTF8ReplacingCountsMaximalSubparts 锁定几个有代表性的计数。
//
// 直接写明数字，让回归时不至于只剩一个"与参照实现一致"的模糊结论。
func TestDecodeUTF8ReplacingCountsMaximalSubparts(t *testing.T) {
	cases := []struct {
		hex       string
		wantCount int
	}{
		{"fffe", 2},     // 两个非法首字节
		{"ffffff", 3},   // 同上
		{"e4b8", 1},     // 截断的中：一个最大子部分
		{"e4", 1},       // 孤立首字节
		{"80", 1},       // 孤立续字节
		{"c080", 2},     // 过长编码：C0 非法，80 又是孤立续字节
		{"eda080", 3},   // UTF-16 代理：ED 收窄使 A0 非法
		{"f4908080", 4}, // 超 U+10FFFF：F4 收窄使 90 非法
		{"f09f", 1},     // 截断的 emoji
		{"e4b8ad", 0},   // 合法的中
		{"f09f9880", 0}, // 合法的 emoji
		{"61ff62", 1},   // a<非法>b
		{"e4b8adff", 1}, // 中 + 非法
	}
	for _, item := range cases {
		got := decodeUTF8Replacing(hexToBytes(t, item.hex))
		if count := strings.Count(got, "\uFFFD"); count != item.wantCount {
			t.Errorf("输入 %s：U+FFFD 个数 %d，期望 %d（结果 %q）",
				item.hex, count, item.wantCount, got)
		}
	}
}

// hexToBytes 把十六进制字符串转成字节。
func hexToBytes(t *testing.T, raw string) []byte {
	t.Helper()
	out, err := hex.DecodeString(raw)
	if err != nil {
		t.Fatalf("解析十六进制 %q 失败: %v", raw, err)
	}
	return out
}
