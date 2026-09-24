// Package formatting 移植 auto_model_key_router/formatting.py：给 TUI / 管理接口
// 做展示用的短文本工具（截断、URL 压缩、百分比、数量缩写、key 指纹）。
//
// 这些函数看着简单，但都是「显示不对也没人报警」的地方，且 Python 的格式化语义
// 相当细（截断后长度仍等于 limit、int/int 是精确有理数除法后一次舍入、
// 百分号格式是十进制四舍六入五成双）。因此每个函数都由本包用例逐个边界钉住。
package formatting

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// KeyFingerprint 返回 API key 的 sha256 十六进制前 12 位（formatting.py:8）。
//
// 空串返回空串而不是 sha256("")：管理接口用「空指纹」表示这个 key 不存在，
// 与「存在但为空」区分开（management_api.py:1470 同样先判断空值）。
func KeyFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])[:12]
}

// ShortText 把 value 截断到最多 limit 个字符，超长时用省略号收尾（formatting.py:14）。
//
// 两个必须原样保留的 Python 细节：
//
//   - 截断只取 limit-1 个字符再补 "…"，补完仍是 limit 个字符；
//   - 保留数下限是 1（`max(limit - 1, 1)`），所以 limit<=1 时不会返回空串，
//     而是第 1 个字符加省略号。
//
// 长度按**码点**算（Python 的 str 索引即码点），不能用 len() 按字节算，
// 否则中文与 emoji 会提前截断。
//
// Python 里 value 是 Any，这里收窄为 string：仓库内所有调用点传入的都是 str
// （config_editor.py:332、logs_tui.py:404、dashboard.py:211 等）。要完整复刻
// Python 的 str() 还得区分 bool/None/float 的表示形式，收益不抵复杂度。
func ShortText(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	keep := limit - 1
	if keep < 1 {
		keep = 1
	}
	return truncateRunes(value, keep) + "…"
}

// truncateRunes 返回 s 的前 n 个码点（n<=0 时返回空串）。
func truncateRunes(s string, n int) string {
	count := 0
	for index := range s {
		if count == n {
			return s[:index]
		}
		count++
	}
	return s
}

// CompactURL 把 URL 压成「主机 + 路径」，超长时按 limit 截断（formatting.py:21）。
//
// 压缩形态只保留 netloc 与 path，**丢掉 scheme**（"https://a/b" → "a/b"）；
// 这是给窄栏 TUI 用的，scheme 由上下文可知。path 为 "" 或 "/" 时不拼路径，
// 否则先 rstrip("/") 再拼。
//
// value 为空时按 Python 的 `value or "-"` 变成 "-"。同理收窄为 string，
// 见 ShortText 的说明。
func CompactURL(value string, limit int) string {
	text := value
	if text == "" {
		text = "-"
	}
	parsed, ok := parseURL(text)
	if !ok {
		// urlparse 抛 ValueError（IPv6 括号不配对、netloc 经 NFKC 会拆出分隔符等）
		// 时，Python 直接退化成纯截断。
		return ShortText(text, limit)
	}
	if parsed.netloc == "" {
		return ShortText(text, limit)
	}
	compact := parsed.netloc
	path := parsed.path
	if usesParams[parsed.scheme] {
		path = splitParams(path)
	}
	if path != "" && path != "/" {
		compact = parsed.netloc + strings.TrimRight(path, "/")
	}
	return ShortText(compact, limit)
}

// Percent 返回 numerator/denominator 的百分比文本，保留 1 位小数（formatting.py:35）。
//
// 分母 <=0（含 0）时直接返回 "0%"，不返回 "0.0%"——调用方靠这一点区分
// 「没有样本」与「比例是 0」，这是对外契约（由 TestPercentNonPositiveDenominator 锁定）。
//
// 用 big.Rat 而不是 float64(n)/float64(d)：Python 的 int/int 是对**精确有理数**
// 做一次正确舍入，而先转 float64 再除会多一次舍入，大整数上两者结果不同
// （percent(9007199254740993, 7) 在 Python 是 128674275067728480.0%，两次舍入
// 会得到 128674275067728448.0%）。math/big 是标准库，且这条路径只在 TUI 里跑，
// 没有性能顾虑。
func Percent(numerator, denominator int64) string {
	if denominator <= 0 {
		return "0%"
	}
	ratio, _ := new(big.Rat).SetFrac64(numerator, denominator).Float64()
	return formatFloat1(ratio*100) + "%"
}

// AbbreviateNumber 把大数缩写成 1.0K / 1.0M / 1.0B（formatting.py:41）。
//
// 与 Percent 同理用 big.Rat 保证「精确有理数除法 + 一次舍入」，而不是
// float64(value)/1e3。阈值判断用整数，负值一律原样输出十进制
// （-1500 → "-1500"，不带单位）。
//
// Python 的 int 无上限，超出 int64 时 `value / 1e9` 会抛 OverflowError；
// Go 侧签名收窄为 int64，不做该边界。调用方是统计计数，量级远达不到。
func AbbreviateNumber(value int64) string {
	switch {
	case value >= 1_000_000_000:
		return abbreviate(value, 1_000_000_000, "B")
	case value >= 1_000_000:
		return abbreviate(value, 1_000_000, "M")
	case value >= 1_000:
		return abbreviate(value, 1_000, "K")
	}
	return strconv.FormatInt(value, 10)
}

// abbreviate 用精确有理数除法把 value 缩到 divisor 的量级，保留 1 位小数。
func abbreviate(value, divisor int64, unit string) string {
	scaled, _ := new(big.Rat).SetFrac64(value, divisor).Float64()
	return formatFloat1(scaled) + unit
}

// formatFloat1 等价于 Python 的 `f"{value:.1f}"`。
//
// Go 的 FormatFloat 与 CPython 的 PyOS_double_to_string 都是对二进制精确值做
// 正确舍入、平局取偶（"6.25" → 6.2、"18.75" → 18.8），由 TestPercentRoundsHalfToEven
// 用等距平局逐条钉住。
func formatFloat1(value float64) string {
	return strconv.FormatFloat(value, 'f', 1, 64)
}
