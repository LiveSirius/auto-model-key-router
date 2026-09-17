package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
)

// Dumps 按 canonical 形式序列化：键排序 + 紧凑分隔符 + 不转义非 ASCII。
//
// 等价于 Python 的
//
//	json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
//
// 这是 config_revision 与 Key 粘滞哈希使用的形式。
func Dumps(v *Value) string {
	var b strings.Builder
	encodeValue(&b, v, encodeOptions{sorted: true}, 0)
	return b.String()
}

// DumpsIndent 按 Python 的 indent 形式序列化：**保留键插入顺序** + 每层缩进。
//
// 等价于 Python 的
//
//	json.dumps(obj, indent=n, ensure_ascii=False)
//
// 注意它与 Dumps 有两个区别：不排序（配置文件的字段顺序是用户可见的），且
// 键值分隔符为 ": "。router-config.json 的落盘（config.py:291）走这条路径。
func DumpsIndent(v *Value, indent int) string {
	if indent < 0 {
		indent = 0
	}
	var b strings.Builder
	encodeValue(&b, v, encodeOptions{indent: indent, hasIndent: true}, 0)
	return b.String()
}

// DumpsOrdered 按 canonical 紧凑形式序列化，但**保留键插入顺序**。
//
// 等价于 Python 的
//
//	json.dumps(obj, ensure_ascii=False, separators=(",", ":"))
//
// 与 Dumps 的唯一区别是不排序。这是协议转换层的 SSE 编码形式
// （protocols/anthropic.py:227、protocols/responses.py 的 _anthropic_sse /
// _responses_sse）：那里用紧凑分隔符但**不**排序，键顺序即 Python dict 的插入
// 顺序，客户端可依赖它。
//
// 注意不能拿 Dumps 代替：排序会改变事件字段顺序，虽然 JSON 语义等价，但逐字节
// 对比的测试与依赖顺序的下游都会失败。
func DumpsOrdered(v *Value) string {
	var b strings.Builder
	encodeValue(&b, v, encodeOptions{}, 0)
	return b.String()
}

// RevisionHash 返回 canonical 形式的 sha256 十六进制摘要。
//
// 对应 management_api.py:1187 的 _config_revision 与 proxy_handler.py:368 的
// Key 粘滞哈希：两者都是 sha256(canonical_json.encode("utf-8")).hexdigest()。
func RevisionHash(v *Value) string {
	sum := sha256.Sum256([]byte(Dumps(v)))
	return hex.EncodeToString(sum[:])
}

type encodeOptions struct {
	sorted    bool
	indent    int
	hasIndent bool
}

func encodeValue(b *strings.Builder, v *Value, opts encodeOptions, depth int) {
	if v == nil {
		b.WriteString("null")
		return
	}
	switch v.Kind {
	case KindNull:
		b.WriteString("null")
	case KindBool:
		if v.Bool {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case KindNumber:
		b.WriteString(v.Num)
	case KindString:
		encodeString(b, v.Str)
	case KindArray:
		encodeArray(b, v, opts, depth)
	case KindObject:
		encodeObject(b, v, opts, depth)
	default:
		// 未初始化的 Value 视作 null，避免产出非法 JSON。
		b.WriteString("null")
	}
}

func encodeArray(b *strings.Builder, v *Value, opts encodeOptions, depth int) {
	if len(v.Arr) == 0 {
		b.WriteString("[]")
		return
	}
	b.WriteByte('[')
	for i, item := range v.Arr {
		if i > 0 {
			b.WriteByte(',')
		}
		writeIndentNewline(b, opts, depth+1)
		encodeValue(b, item, opts, depth+1)
	}
	writeIndentNewline(b, opts, depth)
	b.WriteByte(']')
}

func encodeObject(b *strings.Builder, v *Value, opts encodeOptions, depth int) {
	if v.Obj == nil || v.Obj.Len() == 0 {
		b.WriteString("{}")
		return
	}
	keys := v.Obj.keys
	if opts.sorted {
		keys = v.Obj.sortKeys()
	}
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		writeIndentNewline(b, opts, depth+1)
		encodeString(b, key)
		// 紧凑形式用 ":"，indent 形式用 ": "（Python 的默认 key_separator）。
		if opts.hasIndent {
			b.WriteString(": ")
		} else {
			b.WriteByte(':')
		}
		value, _ := v.Obj.Get(key)
		encodeValue(b, value, opts, depth+1)
	}
	writeIndentNewline(b, opts, depth)
	b.WriteByte('}')
}

// writeIndentNewline 在 indent 模式下换行并写入 depth 层缩进。
func writeIndentNewline(b *strings.Builder, opts encodeOptions, depth int) {
	if !opts.hasIndent {
		return
	}
	b.WriteByte('\n')
	for i := 0; i < opts.indent*depth; i++ {
		b.WriteByte(' ')
	}
}

// encodeString 按 Python 的 ensure_ascii=False 规则转义字符串。
//
// Python 只转义: 反斜杠、双引号、\b \f \n \r \t，以及其余 < 0x20 的控制字符
// （写作 \u00XX）。DEL(0x7f)、U+2028/U+2029、< > & 均原样输出——Go 的
// encoding/json 会转义后三者，因此不能复用。
func encodeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hexDigits = "0123456789abcdef"
				b.WriteByte(hexDigits[byte(r)>>4])
				b.WriteByte(hexDigits[byte(r)&0x0f])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// specialFloat 返回 NaN / Infinity / -Infinity 的 JSON 字面量。
//
// Python 的 json.dumps 默认 allow_nan=True，输出 NaN / Infinity / -Infinity；
// Go 的 encoding/json 会直接报错，因此必须自行处理。
func specialFloat(f float64) (string, bool) {
	switch {
	case math.IsNaN(f):
		return "NaN", true
	case math.IsInf(f, 1):
		return "Infinity", true
	case math.IsInf(f, -1):
		return "-Infinity", true
	}
	return "", false
}

// formatFloat 复现 CPython 的 float.__repr__（json.dumps 对 float 使用它）。
//
// 规则（Python/pystrtod.c 的 format_float_short，mode='r'）：
//
//  1. 取最短且能往返的十进制有效数字（Go 的 FormatFloat 'e' 精度 -1 即此语义）；
//  2. 令 decpt 为小数点在有效数字中的位置（value = 0.d1d2... × 10^decpt）；
//  3. decpt <= -4 或 decpt > 16 时用科学计数法，否则用定点表示；
//  4. 定点表示始终带小数点（整数补 ".0"）；科学计数法的指数至少两位且有符号。
//
// 逐字节正确性由 testdata/corpus.jsonl 的浮点语料断言。
func formatFloat(f float64) string {
	if f == 0 {
		// 区分 0.0 与 -0.0：Python 的 repr(-0.0) 是 "-0.0"。
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}

	// 'e' 形式形如 "1.2345e-07" / "6e+01"，取其有效数字与十进制指数。
	scientific := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expText, ok := strings.Cut(scientific, "e")
	if !ok {
		// 理论上不可达；退回 Go 的默认格式以免产出非法 JSON。
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	exp, err := strconv.Atoi(expText)
	if err != nil {
		return scientific
	}

	negative := strings.HasPrefix(mantissa, "-")
	mantissa = strings.TrimPrefix(mantissa, "-")
	digits := strings.Replace(mantissa, ".", "", 1)
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		digits = "0"
	}

	// decpt: value = 0.<digits> × 10^decpt
	decpt := exp + 1

	var out string
	if decpt <= -4 || decpt > 16 {
		out = formatScientific(digits, decpt)
	} else {
		out = formatFixed(digits, decpt)
	}
	if negative {
		return "-" + out
	}
	return out
}

// formatScientific 输出 d1[.d2...]e±XX，指数至少两位。
func formatScientific(digits string, decpt int) string {
	var b strings.Builder
	b.WriteByte(digits[0])
	if len(digits) > 1 {
		b.WriteByte('.')
		b.WriteString(digits[1:])
	}
	b.WriteByte('e')
	exp := decpt - 1
	if exp < 0 {
		b.WriteByte('-')
		exp = -exp
	} else {
		b.WriteByte('+')
	}
	expText := strconv.Itoa(exp)
	if len(expText) < 2 {
		b.WriteByte('0')
	}
	b.WriteString(expText)
	return b.String()
}

// formatFixed 输出定点表示，整数部分不为空且始终带小数点。
func formatFixed(digits string, decpt int) string {
	switch {
	case decpt <= 0:
		return "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		return digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	default:
		return digits[:decpt] + "." + digits[decpt:]
	}
}
