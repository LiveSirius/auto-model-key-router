package canonical

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// ValueError / TypeError / OverflowError 对应 Python 的三个内建异常。
//
// 需要区分是因为它们的**契约地位不同**：int("soon") 抛 ValueError，其文本会经
// management API 直接回给用户，必须逐字复刻；而 int(None) 抛 TypeError，那是
// 解释器层面的类型误用，Go 只需失败关闭，不必复刻措辞。
type ValueError struct{ Message string }

func (e *ValueError) Error() string { return e.Message }

// TypeError 对应 Python 的 TypeError。
type TypeError struct{ Message string }

func (e *TypeError) Error() string { return e.Message }

// OverflowError 对应 Python 的 OverflowError（如 int(inf)）。
type OverflowError struct{ Message string }

func (e *OverflowError) Error() string { return e.Message }

// ToInt 对齐 Python 的 int(value)。
//
// 规则（均以 Python 3.12 实测为准）：
//   - bool 视为 0/1；
//   - 字符串按十进制整数解析，允许首尾空白、正负号、数字间的下划线、Unicode
//     十进制数字；带小数点或指数的一律拒绝；
//   - 浮点向零截断（int(-3.7) == -3）；NaN 抛 ValueError，无穷抛 OverflowError；
//   - None / 数组 / 对象抛 TypeError。
//
// 错误文本中的字符串是**原始输入**（未 strip），例如 int(" 45x ") 的文本里保留
// 了空格——照抄 Python 的写法会让这类细节成为差异源。
func ToInt(v *Value) (int64, error) {
	if v == nil || v.Kind == KindNull {
		return 0, &TypeError{Message: fmt.Sprintf(
			"int() argument must be a string, a bytes-like object or a real number, not '%s'",
			PyTypeName(v))}
	}
	switch v.Kind {
	case KindBool:
		if v.Bool {
			return 1, nil
		}
		return 0, nil
	case KindString:
		return pyIntFromString(v.Str)
	case KindNumber:
		if isIntLiteral(v.Num) {
			n, err := strconv.ParseInt(v.Num, 10, 64)
			if err != nil {
				// 超出 int64 的整数：Python 能处理任意精度，Go 侧失败关闭。
				return 0, &OverflowError{Message: "int too large to convert to Go int64"}
			}
			return n, nil
		}
		f, ok := parseFloat(v.Num)
		if !ok {
			return 0, &ValueError{Message: fmt.Sprintf(
				"invalid literal for int() with base 10: '%s'", v.Num)}
		}
		if math.IsNaN(f) {
			return 0, &ValueError{Message: "cannot convert float NaN to integer"}
		}
		if math.IsInf(f, 0) {
			return 0, &OverflowError{Message: "cannot convert float infinity to integer"}
		}
		if f >= math.MaxInt64 || f <= math.MinInt64 {
			return 0, &OverflowError{Message: "int too large to convert to Go int64"}
		}
		return int64(f), nil
	}
	return 0, &TypeError{Message: fmt.Sprintf(
		"int() argument must be a string, a bytes-like object or a real number, not '%s'",
		PyTypeName(v))}
}

// ToFloat 对齐 Python 的 float(value)。
//
// 字符串接受 Python 的全部浮点写法，含 nan / inf / infinity 及其符号变体、科学
// 计数法、下划线分隔；其余类型抛 TypeError。
func ToFloat(v *Value) (float64, error) {
	if v == nil || v.Kind == KindNull {
		return 0, &TypeError{Message: fmt.Sprintf(
			"float() argument must be a string or a real number, not '%s'", PyTypeName(v))}
	}
	switch v.Kind {
	case KindBool:
		if v.Bool {
			return 1, nil
		}
		return 0, nil
	case KindString:
		return pyFloatFromString(v.Str)
	case KindNumber:
		f, ok := parseFloat(v.Num)
		if !ok {
			return 0, &ValueError{Message: fmt.Sprintf(
				"could not convert string to float: '%s'", v.Num)}
		}
		return f, nil
	}
	return 0, &TypeError{Message: fmt.Sprintf(
		"float() argument must be a string or a real number, not '%s'", PyTypeName(v))}
}

// pyIntFromString 对齐 int(str)。
func pyIntFromString(original string) (int64, error) {
	fail := func() (int64, error) {
		return 0, &ValueError{Message: fmt.Sprintf(
			"invalid literal for int() with base 10: '%s'", original)}
	}
	// Python 先做空白剥离，但错误文本里用的是原始字符串。
	body := strings.TrimSpace(original)
	if body == "" {
		return fail()
	}
	// Unicode 十进制数字（如全角 "４５"）等价于对应 ASCII 数字。
	normalized, ok := normalizeDecimalDigits(body)
	if !ok {
		return fail()
	}
	// 下划线只允许夹在数字之间；先去掉再交给 ParseInt，非法位置直接判错。
	stripped, ok := stripDigitUnderscores(normalized)
	if !ok {
		return fail()
	}
	n, err := strconv.ParseInt(stripped, 10, 64)
	if err != nil {
		return fail()
	}
	return n, nil
}

// pyFloatFromString 对齐 float(str)。
func pyFloatFromString(original string) (float64, error) {
	fail := func() (float64, error) {
		return 0, &ValueError{Message: fmt.Sprintf(
			"could not convert string to float: '%s'", original)}
	}
	body := strings.TrimSpace(original)
	if body == "" {
		return fail()
	}
	// Python 接受大小写不敏感的 inf/infinity/nan 前缀写法，也接受 "infinity"。
	lowered := strings.ToLower(body)
	sign := 1.0
	unsigned := lowered
	switch {
	case strings.HasPrefix(unsigned, "+"):
		unsigned = unsigned[1:]
	case strings.HasPrefix(unsigned, "-"):
		sign = -1
		unsigned = unsigned[1:]
	}
	switch unsigned {
	case "inf", "infinity":
		return math.Inf(int(sign)), nil
	case "nan":
		return math.NaN(), nil
	}
	normalized, ok := normalizeDecimalDigits(body)
	if !ok {
		return fail()
	}
	stripped, ok := stripDigitUnderscores(normalized)
	if !ok {
		return fail()
	}
	f, err := strconv.ParseFloat(stripped, 64)
	if err != nil {
		return fail()
	}
	return f, nil
}

// normalizeDecimalDigits 把 Unicode 十进制数字映射为 ASCII 数字。
//
// 非十进制数字字符原样保留（后续解析会拒绝它们）。
func normalizeDecimalDigits(value string) (string, bool) {
	needsMapping := false
	for _, r := range value {
		if r > unicode.MaxASCII && unicode.IsDigit(r) {
			needsMapping = true
			break
		}
	}
	if !needsMapping {
		return value, true
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if r <= unicode.MaxASCII {
			b.WriteRune(r)
			continue
		}
		if !unicode.IsDigit(r) {
			return "", false
		}
		// 以 '0' 为基准取数字值：Unicode 的 Nd 区段是连续编码的。
		b.WriteByte(byte('0' + (r - unicodeDigitZero(r))))
	}
	return b.String(), true
}

// unicodeDigitZero 返回某个 Unicode 十进制数字所属区段的 '0'。
func unicodeDigitZero(r rune) rune {
	// Nd 区段的数字是连续的，逐个回退到该区段的起点即可。
	zero := r
	for unicode.IsDigit(zero - 1) {
		zero--
	}
	return zero
}

// stripDigitUnderscores 去掉数字间的下划线，并对非法位置报告失败。
//
// Python 只允许下划线夹在两个数字之间：`1_000` 可以，`_1`、`1_`、`1__0` 不可以。
func stripDigitUnderscores(value string) (string, bool) {
	if !strings.ContainsRune(value, '_') {
		return value, true
	}
	var b strings.Builder
	b.Grow(len(value))
	runes := []rune(value)
	for i, r := range runes {
		if r != '_' {
			b.WriteRune(r)
			continue
		}
		if i == 0 || i == len(runes)-1 {
			return "", false
		}
		if !isASCIIDigit(runes[i-1]) || !isASCIIDigit(runes[i+1]) {
			return "", false
		}
	}
	return b.String(), true
}

// isASCIIDigit 报告字符是否为 0-9。
func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }
