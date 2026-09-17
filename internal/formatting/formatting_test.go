package formatting

import (
	"math"
	"strings"
	"testing"
)

// TestShortTextLimitQuirks 锁定 ShortText 的两个「看着像 bug」的 Python 行为
// （formatting.py:14）：
//
//   - 截断结果长度恰好是 limit（先取 limit-1 个字符再补省略号）；
//   - limit<=1 时下限为 1，返回「首字符 + 省略号」而不是空串。
//
// 下游（TUI 表格列宽）依赖第一条：按列宽算出来的字符串不会超宽。
func TestShortTextLimitQuirks(t *testing.T) {
	if got := ShortText("abcdef", 5); got != "abcd…" {
		t.Errorf("ShortText(\"abcdef\", 5) = %q，期望 %q", got, "abcd…")
	}
	if got := ShortText("abcdef", 1); got != "a…" {
		t.Errorf("ShortText(\"abcdef\", 1) = %q，期望 %q", got, "a…")
	}
	if got := ShortText("abcdef", 0); got != "a…" {
		t.Errorf("ShortText(\"abcdef\", 0) = %q，期望 %q", got, "a…")
	}
	if got := ShortText("abcdef", -5); got != "a…" {
		t.Errorf("ShortText(\"abcdef\", -5) = %q，期望 %q", got, "a…")
	}
	// 空串在 limit<0 时同样走「截断」分支，得到只有一个省略号的结果。
	if got := ShortText("", -1); got != "…" {
		t.Errorf("ShortText(\"\", -1) = %q，期望 %q", got, "…")
	}
	if got := ShortText("", 0); got != "" {
		t.Errorf("ShortText(\"\", 0) = %q，期望空串", got)
	}
}

// TestShortTextCountsCodePoints 锁定长度按码点而非字节计算。
//
// 中文与 emoji 在 UTF-8 里占 3~4 字节，若按字节截断会砍出半个字符。
func TestShortTextCountsCodePoints(t *testing.T) {
	if got := ShortText("中文字符串测试内容", 5); got != "中文字符…" {
		t.Errorf("ShortText 中文 limit=5 = %q，期望 %q", got, "中文字符…")
	}
	if got := ShortText("🔑🔑🔑", 2); got != "🔑…" {
		t.Errorf("ShortText emoji limit=2 = %q，期望 %q", got, "🔑…")
	}
	// 长度刚好等于 limit 时不做任何处理（Python 是 `len(text) <= limit`）。
	if got := ShortText("中", 1); got != "中" {
		t.Errorf("ShortText(\"中\", 1) = %q，期望 %q", got, "中")
	}
}

// TestShortTextAcceptsStringOnly 记录一处刻意收窄：Python 的 value 是 Any，
// Go 只接受 string。
//
// 所有调用点传的都是 str；完整复刻 Python 的 str() 还要区分 bool/None/float
// 的表示（"True"/"None"/"1.5"），收益不抵复杂度。收窄后 CompactURL 的
// `value or "-"` 也只剩「空串」一种触发条件，而 Python 里 0/None/[] 同样会
// 变成 "-"——这是已知且刻意的差异。
func TestShortTextAcceptsStringOnly(t *testing.T) {
	if got := CompactURL("", 32); got != "-" {
		t.Errorf("CompactURL(\"\") = %q，期望 %q", got, "-")
	}
	// Python 的 short_text(None) 走的是 str(None)="None"；Go 侧必须由调用方
	// 显式转换，函数本身只认字符串。
	if got := ShortText("None", 32); got != "None" {
		t.Errorf("ShortText(\"None\") = %q，期望 %q", got, "None")
	}
}

// TestPercentNonPositiveDenominator 锁定分母<=0 时的 "0%" 分支（formatting.py:35）。
//
// 返回 "0%" 而不是 "0.0%"：调用方靠这个区分「没有样本」与「比例为零」。
func TestPercentNonPositiveDenominator(t *testing.T) {
	for _, denominator := range []int64{0, -1, math.MinInt64} {
		if got := Percent(7, denominator); got != "0%" {
			t.Errorf("Percent(7, %d) = %q，期望 %q", denominator, got, "0%")
		}
	}
	if got := Percent(0, 5); got != "0.0%" {
		t.Errorf("Percent(0, 5) = %q，期望 %q", got, "0.0%")
	}
}

// TestPercentUsesExactRationalDivision 锁定「精确有理数除法 + 一次舍入」。
//
// Python 的 int/int 拿精确有理数做一次正确舍入；若改成 float64(n)/float64(d)
// 会多一次舍入，下面这组大整数就会算出 128674275067728448.0%（差在末两位）。
// 这条用例是防止有人为了「简单」把 big.Rat 换掉。
func TestPercentUsesExactRationalDivision(t *testing.T) {
	const want = "128674275067728480.0%"
	if got := Percent(9007199254740993, 7); got != want {
		t.Fatalf("Percent(9007199254740993, 7) = %q，期望 %q"+
			"（float64 两次舍入会得到 128674275067728448.0%%）", got, want)
	}
}

// TestPercentRoundsHalfToEven 锁定等距平局按「取偶」处理（CPython 与 strconv
// 都是对二进制精确值做正确舍入）。
func TestPercentRoundsHalfToEven(t *testing.T) {
	cases := map[[2]int64]string{
		{1, 16}:  "6.2%",  // 6.25  → 6.2（2 是偶数）
		{3, 16}:  "18.8%", // 18.75 → 18.8（8 是偶数）
		{5, 16}:  "31.2%",
		{7, 16}:  "43.8%",
		{1, 800}: "0.1%",
	}
	for pair, want := range cases {
		if got := Percent(pair[0], pair[1]); got != want {
			t.Errorf("Percent(%d, %d) = %q，期望 %q", pair[0], pair[1], got, want)
		}
	}
}

// TestAbbreviateNumberDoesNotPromoteUnit 锁定「只在阈值处换单位」。
//
// 999999 会输出 "1000.0K" 而不是进位成 "1.0M"：Python 先按 value 判阈值，
// 再对商做一位小数格式化，所以商跨过 1000 也不会换单位。TUI 上这是可见的
// 「奇怪但正确」的输出，语料已逐条锁定。
func TestAbbreviateNumberDoesNotPromoteUnit(t *testing.T) {
	cases := map[int64]string{
		999:           "999",
		1000:          "1.0K",
		999949:        "999.9K",
		999950:        "1000.0K",
		999999:        "1000.0K",
		1000000:       "1.0M",
		999999999:     "1000.0M",
		1000000000:    "1.0B",
		1000000000000: "1000.0B",
	}
	for value, want := range cases {
		if got := AbbreviateNumber(value); got != want {
			t.Errorf("AbbreviateNumber(%d) = %q，期望 %q", value, got, want)
		}
	}
}

// TestAbbreviateNumberNegativeStaysDecimal 锁定负值不缩写。
func TestAbbreviateNumberNegativeStaysDecimal(t *testing.T) {
	for value, want := range map[int64]string{
		-1:          "-1",
		-1500:       "-1500",
		-1500000000: "-1500000000",
	} {
		if got := AbbreviateNumber(value); got != want {
			t.Errorf("AbbreviateNumber(%d) = %q，期望 %q", value, got, want)
		}
	}
}

// TestAbbreviateNumberNarrowedToInt64 记录一处刻意收窄：Python 的 int 无上限，
// Go 侧是 int64。
//
// 超出 int64 的值在 Python 里能表示（10**19 会输出 "10000000000.0B"），
// Go 侧连实参都构造不出来。调用方是 token / 请求计数，量级远达不到。
func TestAbbreviateNumberNarrowedToInt64(t *testing.T) {
	if got := AbbreviateNumber(math.MaxInt64); got != "9223372036.9B" {
		t.Errorf("AbbreviateNumber(MaxInt64) = %q，期望 %q", got, "9223372036.9B")
	}
	if got := AbbreviateNumber(math.MinInt64); got != "-9223372036854775808" {
		t.Errorf("AbbreviateNumber(MinInt64) = %q，期望十进制原样输出", got)
	}
}

// TestCompactURLDropsSchemeAndKeepsRawPath 锁定压缩形态：
// 丢掉 scheme、path 保留原文（不做百分号解码、不做 ./.. 归一化），尾随 '/' 去掉。
func TestCompactURLDropsSchemeAndKeepsRawPath(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"https://api.example.com/v1/", "api.example.com/v1"},
		{"https://api.example.com/", "api.example.com"},
		{"https://api.example.com", "api.example.com"},
		{"HTTPS://UPPER.EXAMPLE.COM/Path", "UPPER.EXAMPLE.COM/Path"},
		{"https://h/%E4%B8%AD%E6%96%87/path", "h/%E4%B8%AD%E6%96%87/path"},
		{"https://h/p/./q/../r", "h/p/./q/../r"},
		{"example.com/path", "example.com/path"},
	}
	for _, testCase := range cases {
		if got := CompactURL(testCase.value, 56); got != testCase.want {
			t.Errorf("CompactURL(%q) = %q，期望 %q", testCase.value, got, testCase.want)
		}
	}
}

// TestCompactURLStripsControlCharacters 锁定 urlsplit 的空白/控制字符处理：
// 只 lstrip 开头的 C0 控制字符与空格（尾随空格保留），而 \t\r\n 是整串删除。
func TestCompactURLStripsControlCharacters(t *testing.T) {
	if got := CompactURL("  https://spaced.example.com/x  ", 56); got != "spaced.example.com/x  " {
		t.Errorf("前导空白 lstrip / 尾随空格保留失败: %q", got)
	}
	if got := CompactURL("http://exa\nmple.com/v1", 56); got != "example.com/v1" {
		t.Errorf("\\n 应被整串删除: %q", got)
	}
}

// TestCompactURLFallsBackOnUnsafeNetloc 锁定 _checknetloc 的退化路径。
//
// 19 个「NFKC 归一化后会拆出 /?#@:」的码点会让 urlparse 抛 ValueError，
// compact_url 随即退化成整串截断；而 NFKC 稳定的非 ASCII 域名（含 IDN 与
// 组合字符）照常压缩。Go 标准库没有 NFKC，只能靠码点表复现该判定。
func TestCompactURLFallsBackOnUnsafeNetloc(t *testing.T) {
	unsafe := []string{
		"\u2047", "\u2100", "\uff0f", "\uff20", "\u2a74", "\ufe5f",
	}
	for _, char := range unsafe {
		value := "https://h" + char + "x/v1"
		if got := CompactURL(value, 56); got != value {
			t.Errorf("CompactURL(%q) = %q，期望退化成整串", value, got)
		}
	}
	for _, value := range []string{
		"https://中文.example.com/v1",
		"https://é.example.com/v1",
		"https://Ⅻ.example.com/v1",
	} {
		netloc := strings.SplitN(strings.TrimPrefix(value, "https://"), "/", 2)[0]
		if got := CompactURL(value, 56); got != netloc+"/v1" {
			t.Errorf("CompactURL(%q) = %q，期望 %q", value, got, netloc+"/v1")
		}
	}
}

// TestCompactURLValidatesBracketedHost 锁定方括号主机的校验规则
// （CPython ipaddress._check_bracketed_host）。
func TestCompactURLValidatesBracketedHost(t *testing.T) {
	valid := []string{
		"https://[::1]:80/x",
		"https://[2001:db8::1]/x",
		"https://[::ffff:1.2.3.4]/x",
		"https://[::1%eth0]:80/x",
		"https://[v1.foo]:80/x",
	}
	for _, value := range valid {
		if got := CompactURL(value, 56); strings.HasPrefix(got, "https://") {
			t.Errorf("CompactURL(%q) = %q，应当压缩而不是退化", value, got)
		}
	}
	invalid := []string{
		"https://[::1/x",         // 少右括号
		"https://::1]/x",         // 少左括号
		"https://[1.2.3.4]:80/x", // IPv4 不允许带括号
		"https://[bad-ipv6]:80/x",
		"https://[v1]:80/x",       // IPvFuture 缺 "."
		"https://[fe80::1%]:80/x", // scope id 为空
		"https://[user@::1]:80/x", // 方括号前不允许有内容
		"https://[a]b]:80/x",      // 右括号后不允许有内容（除非是端口）
	}
	for _, value := range invalid {
		if got := CompactURL(value, 56); got != value {
			t.Errorf("CompactURL(%q) = %q，期望退化成整串", value, got)
		}
	}
}
