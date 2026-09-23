package canonical

import (
	"strings"
	"testing"
)

// TestRevisionHashStable 锁定 sha256 摘要的十六进制形式（64 位小写）。
func TestRevisionHashStable(t *testing.T) {
	value, err := ParseString(`{"b":1,"a":2}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := RevisionHash(value)
	if len(got) != 64 {
		t.Fatalf("摘要长度应为 64，实际 %d: %s", len(got), got)
	}
	if strings.ToLower(got) != got {
		t.Errorf("摘要应为小写十六进制: %s", got)
	}
	// 键顺序不同的等价对象必须得到同一摘要（sort_keys 的语义）。
	other, err := ParseString(`{"a":2,"b":1}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if otherHash := RevisionHash(other); otherHash != got {
		t.Errorf("键顺序不应影响摘要: %s != %s", otherHash, got)
	}
}

// reportFirstDiff 指出首个不同的字节位置，便于定位浮点或转义偏差。
func reportFirstDiff(t *testing.T, want, got string) {
	t.Helper()
	limit := len(want)
	if len(got) < limit {
		limit = len(got)
	}
	for i := 0; i < limit; i++ {
		if want[i] != got[i] {
			start := i - 20
			if start < 0 {
				start = 0
			}
			t.Errorf("首个差异在字节 %d：期望 %q，实际 %q",
				i, snippet(want, start), snippet(got, start))
			return
		}
	}
	t.Errorf("前缀相同但长度不同：期望 %d 字节，实际 %d 字节", len(want), len(got))
}

func snippet(s string, start int) string {
	end := start + 40
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}
