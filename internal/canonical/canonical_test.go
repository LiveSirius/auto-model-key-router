package canonical

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusEntry 是一条对拍语料：input 为原始 JSON 文本，expected 为 Python
// json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",",":"))
// 的精确输出。
type corpusEntry struct {
	Name     string `json:"name"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

func loadCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	path := filepath.Join("testdata", "corpus.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开语料失败（语料已冻结并随仓库提交，生成器已随 Python 退役移除）: %v", err)
	}
	defer f.Close()

	var entries []corpusEntry
	scanner := bufio.NewScanner(f)
	// 单行可能很长（随机浮点与随机字符串用例），放宽缓冲上限。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry corpusEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("语料行解析失败: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("语料为空")
	}
	return entries
}

// TestDumpsMatchesPython 是本次迁移的核心对拍断言：Go 的 canonical 序列化
// 必须与 Python 的 json.dumps 逐字节一致。
func TestDumpsMatchesPython(t *testing.T) {
	for _, entry := range loadCorpus(t) {
		t.Run(entry.Name, func(t *testing.T) {
			value, err := ParseString(entry.Input)
			if err != nil {
				t.Fatalf("解析 %s 失败: %v", entry.Input, err)
			}
			got := Dumps(value)
			if got != entry.Expected {
				t.Errorf("canonical 输出不一致\n输入: %s\n期望: %s\n实际: %s",
					entry.Input, entry.Expected, got)
				reportFirstDiff(t, entry.Expected, got)
			}
		})
	}
}

// TestParseRoundTrip 确保解析后重新输出仍与 Python 期望一致，覆盖
// 「解析规范化 + 序列化」的组合路径。
func TestParseRoundTrip(t *testing.T) {
	for _, entry := range loadCorpus(t) {
		t.Run(entry.Name, func(t *testing.T) {
			first, err := ParseString(entry.Input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			second, err := ParseString(Dumps(first))
			if err != nil {
				t.Fatalf("二次解析失败: %v", err)
			}
			if got := Dumps(second); got != entry.Expected {
				t.Errorf("二次序列化不一致\n期望: %s\n实际: %s", entry.Expected, got)
			}
		})
	}
}

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
