package formatting

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// corpusFile 是 scripts/gen_formatting_corpus.py 产出的对拍语料。
//
// 期望值全部来自真实的 Python 模块，Go 测试只读语料、不需要 Python 解释器。
type corpusFile struct {
	KeyFingerprint []struct {
		Input string `json:"input"`
		Want  string `json:"want"`
	} `json:"key_fingerprint"`
	ShortText []struct {
		Value string `json:"value"`
		Limit int    `json:"limit"`
		Want  string `json:"want"`
	} `json:"short_text"`
	CompactURL []struct {
		Value string `json:"value"`
		Limit int    `json:"limit"`
		Want  string `json:"want"`
	} `json:"compact_url"`
	Percent []struct {
		Numerator   int64  `json:"numerator"`
		Denominator int64  `json:"denominator"`
		Want        string `json:"want"`
	} `json:"percent"`
	AbbreviateNumber []struct {
		Value int64  `json:"value"`
		Want  string `json:"want"`
	} `json:"abbreviate_number"`
	// NFKCUnsafeRunes 是 Python 全码点扫描出的「NFKC 会引入 /?#@:」的字符。
	NFKCUnsafeRunes string `json:"nfkc_unsafe_runes"`
}

// loadCorpus 读取对拍语料。
func loadCorpus(t *testing.T) corpusFile {
	t.Helper()
	path := filepath.Join("testdata", "formatting_corpus.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var corpus corpusFile
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	return corpus
}

// TestKeyFingerprintMatchesPython 重放 key_fingerprint 语料。
func TestKeyFingerprintMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	if len(corpus.KeyFingerprint) == 0 {
		t.Fatal("key_fingerprint 语料为空")
	}
	for _, testCase := range corpus.KeyFingerprint {
		got := KeyFingerprint(testCase.Input)
		if got != testCase.Want {
			t.Errorf("KeyFingerprint(%q) = %q，期望 %q", testCase.Input, got, testCase.Want)
		}
	}
	t.Logf("key_fingerprint 断言 %d 条", len(corpus.KeyFingerprint))
}

// TestShortTextMatchesPython 重放 short_text 语料。
func TestShortTextMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	if len(corpus.ShortText) == 0 {
		t.Fatal("short_text 语料为空")
	}
	for _, testCase := range corpus.ShortText {
		got := ShortText(testCase.Value, testCase.Limit)
		if got != testCase.Want {
			t.Errorf("ShortText(%q, %d) = %q，期望 %q",
				testCase.Value, testCase.Limit, got, testCase.Want)
		}
	}
	t.Logf("short_text 断言 %d 条", len(corpus.ShortText))
}

// TestCompactURLMatchesPython 重放 compact_url 语料。
func TestCompactURLMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	if len(corpus.CompactURL) == 0 {
		t.Fatal("compact_url 语料为空")
	}
	for _, testCase := range corpus.CompactURL {
		got := CompactURL(testCase.Value, testCase.Limit)
		if got != testCase.Want {
			t.Errorf("CompactURL(%q, %d) = %q，期望 %q",
				testCase.Value, testCase.Limit, got, testCase.Want)
		}
	}
	t.Logf("compact_url 断言 %d 条", len(corpus.CompactURL))
}

// TestPercentMatchesPython 重放 percent 语料。
func TestPercentMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	if len(corpus.Percent) == 0 {
		t.Fatal("percent 语料为空")
	}
	for _, testCase := range corpus.Percent {
		got := Percent(testCase.Numerator, testCase.Denominator)
		if got != testCase.Want {
			t.Errorf("Percent(%d, %d) = %q，期望 %q",
				testCase.Numerator, testCase.Denominator, got, testCase.Want)
		}
	}
	t.Logf("percent 断言 %d 条", len(corpus.Percent))
}

// TestAbbreviateNumberMatchesPython 重放 abbreviate_number 语料。
func TestAbbreviateNumberMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	if len(corpus.AbbreviateNumber) == 0 {
		t.Fatal("abbreviate_number 语料为空")
	}
	for _, testCase := range corpus.AbbreviateNumber {
		got := AbbreviateNumber(testCase.Value)
		if got != testCase.Want {
			t.Errorf("AbbreviateNumber(%d) = %q，期望 %q",
				testCase.Value, got, testCase.Want)
		}
	}
	t.Logf("abbreviate_number 断言 %d 条", len(corpus.AbbreviateNumber))
}

// TestNFKCUnsafeRunesMatchPython 校验手写码点表与 Python 全码点扫描结果一致。
//
// Go 标准库没有 NFKC（x/text 不在依赖里），这张表是全码点扫描的产物，只能靠
// 语料锁住。注意它与 Python 的 unicodedata 版本绑定：Python 升级 Unicode 数据后
// 语料的 --check 会先失败，提示重新生成并同步这张表。
func TestNFKCUnsafeRunesMatchPython(t *testing.T) {
	corpus := loadCorpus(t)
	if corpus.NFKCUnsafeRunes == "" {
		t.Fatal("nfkc_unsafe_runes 语料为空")
	}
	runes := make([]rune, 0, len(nfkcUnsafeRunes))
	for char := range nfkcUnsafeRunes {
		runes = append(runes, char)
	}
	sort.Slice(runes, func(i, j int) bool { return runes[i] < runes[j] })

	var builder strings.Builder
	for _, char := range runes {
		builder.WriteRune(char)
	}
	if builder.String() != corpus.NFKCUnsafeRunes {
		t.Fatalf("NFKC 危险码点表与 Python 不一致：Go=%q，Python=%q",
			builder.String(), corpus.NFKCUnsafeRunes)
	}
	t.Logf("NFKC 危险码点 %d 个", len(runes))
}
