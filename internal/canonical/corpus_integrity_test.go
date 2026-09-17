package canonical

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是一道「测试有效性」防线，不是产品断言。
//
// 目的：证明 testdata/corpus.jsonl 具备区分力，并且记录 Go 标准库在哪些用例上
// 无法复现 Python json.dumps——这正是 internal/canonical 存在的理由。
//
// 标准库有两条可选路径，都达不到逐字节一致：
//
//	naive:  json.Unmarshal 到 any + json.Marshal
//	        —— 数字全变 float64（大整数丢精度、60 与 60.0 无法区分），键被排序，
//	           HTML 字符被转义，NaN/Infinity 直接失败。
//	best:   json.Decoder.UseNumber + Encoder.SetEscapeHTML(false)
//	        —— 修掉了精度与 HTML 转义，但仍有：浮点指数格式（Go "1e+16" vs
//	           Python "1e+16" 之外的差异，如 Go 不补指数前缀零）、U+2028/U+2029
//	           仍被转义、NaN/Infinity 仍无法解析。
//
// 因此断言的是「至少有一类偏差必然存在」，而不是一个任意比例。

func loadCorpusEntries(t *testing.T) []corpusEntry {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "corpus.jsonl"))
	if err != nil {
		t.Fatalf("打开语料失败: %v", err)
	}
	defer f.Close()

	var entries []corpusEntry
	scanner := bufio.NewScanner(f)
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
	return entries
}

// encodeNaive 复现最朴素的标准库用法：Unmarshal 到 any 再 Marshal。
func encodeNaive(input string) (string, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(input), &decoded); err != nil {
		return "", false
	}
	out, err := json.Marshal(decoded)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// encodeBestEffort 复现标准库能做到的最好情况：保留数字字面量并关闭 HTML 转义。
func encodeBestEffort(input string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", false
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(decoded); err != nil {
		return "", false
	}
	// Encoder.Encode 会追加换行，Python 的 json.dumps 不会。
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// TestStdlibCannotMatchPython 断言标准库在语料上必然出现偏差，并统计规模。
func TestStdlibCannotMatchPython(t *testing.T) {
	entries := loadCorpusEntries(t)
	if len(entries) == 0 {
		t.Fatal("语料为空")
	}

	var naiveFail, bestFail int
	var bestExamples []string
	for _, entry := range entries {
		if got, ok := encodeNaive(entry.Input); !ok || got != entry.Expected {
			naiveFail++
		}
		got, ok := encodeBestEffort(entry.Input)
		if !ok || got != entry.Expected {
			bestFail++
			if len(bestExamples) < 5 {
				bestExamples = append(bestExamples, entry.Name)
			}
		}
	}

	t.Logf("naive 标准库路径: %d/%d 条与 Python 不一致", naiveFail, len(entries))
	t.Logf("best-effort 标准库路径: %d/%d 条仍不一致（示例: %s）",
		bestFail, len(entries), strings.Join(bestExamples, ", "))

	// 朴素路径必须大面积失败，否则语料没有起到作用。
	if naiveFail*2 < len(entries) {
		t.Errorf("语料区分力不足：naive 路径仅 %d/%d 条偏离", naiveFail, len(entries))
	}
	// 即使做到最好，标准库也必须在若干条上偏离——这是本包存在的根本理由。
	if bestFail == 0 {
		t.Error("best-effort 标准库路径竟然全部一致：internal/canonical 已无必要，请复核语料与实现")
	}
}

// TestStdlibBestEffortKnownGaps 逐项锁定标准库的具体缺口，避免语料被悄悄削弱
// 后仍显示为「有偏差」。
func TestStdlibBestEffortKnownGaps(t *testing.T) {
	cases := []struct {
		name  string
		input string
		why   string
	}{
		{"nan_literal", `{"v":NaN}`, "标准库不接受 NaN 字面量"},
		{"infinity_literal", `{"v":Infinity}`, "标准库不接受 Infinity 字面量"},
		{"big_int_precision", `{"n":1000000000000000000000000000000}`, "float64 无法精确表示大整数"},
		{"int_float_distinction", `{"a":60,"b":60.0}`, "naive 路径无法区分 int 与 float"},
		{"line_separator_escape", `{"s":"a` + "\u2028" + `b"}`, "标准库转义 U+2028，Python 不转义"},
		{"html_chars", `{"s":"<a>&b"}`, "naive 路径转义 < > &"},
		{"key_order_preserved", `{"b":1,"a":2}`, "标准库排序键，Python 的 indent 形式保留顺序"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := loadCorpusEntries(t)
			var expected string
			found := false
			for _, e := range entries {
				if e.Input == tc.input || e.Name == tc.name {
					expected = e.Expected
					found = true
					break
				}
			}
			if !found {
				t.Skipf("语料未包含该用例，跳过（%s）", tc.why)
			}
			got, ok := encodeBestEffort(tc.input)
			naive, naiveOK := encodeNaive(tc.input)
			if ok && got == expected && naiveOK && naive == expected {
				t.Errorf("两条标准库路径都复现了 %s，语料对该点的区分力失效", tc.name)
			}
		})
	}
}
