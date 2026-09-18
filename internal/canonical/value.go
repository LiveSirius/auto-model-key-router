// Package canonical 提供与 Python json 模块逐字节兼容的 JSON 解析与序列化。
//
// 存在的唯一理由：AMKR 有两处 sha256 契约依赖 Python json.dumps 的精确输出——
// config_revision（management_api.py:1187）与 Key 粘滞哈希 _cache_affinity_key
// （proxy_handler.py:348）。两者都用
//
//	json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
//
// Go 的 encoding/json 有三处与 Python 不同，任何一处都会让哈希静默失效：
//
//   - map[string]any 把数字统一成 float64：大整数丢精度，且无法区分 60 与 60.0
//     （Python 的 int 与 float 分别输出 "60" 与 "60.0"）；
//   - Encoder 默认把 < > & 与 U+2028/U+2029 转义成 \uXXXX，Python 不转义；
//   - 不接受 NaN / Infinity / -Infinity 字面量，Python 接受。
//
// 因此这里自带解析器与序列化器。逐字节正确性由
// gen_canonical_corpus.py（已随 Python 退役移除） 生成、testdata/corpus.jsonl 承载的语料断言，
// 该语料以 Python 真实实现为参照，不依赖测试时存在 Python 解释器。
package canonical

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Kind 是 Value 的类型标签。
type Kind uint8

const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	case KindObject:
		return "object"
	}
	return "unknown"
}

// Value 是一个 JSON 值。
//
// 刻意不复用 encoding/json 的类型：数字保存为 Python 语义下的规范化字面量
// （Num），对象保留键的插入顺序（Obj），并允许 NaN / Infinity。
type Value struct {
	Kind Kind
	Bool bool
	// Num 是已规范化的数字字面量。整数保持十进制原样（任意精度），浮点数已按
	// Python repr 规则格式化，因此序列化时直接输出即可。
	Num string
	Str string
	Arr []*Value
	Obj *Object
}

// NewNull 返回 null。
func NewNull() *Value { return &Value{Kind: KindNull} }

// NewBool 返回布尔值。
func NewBool(b bool) *Value { return &Value{Kind: KindBool, Bool: b} }

// NewString 返回字符串。
func NewString(s string) *Value { return &Value{Kind: KindString, Str: s} }

// NewInt 返回以十进制字面量表示的整数（保留任意精度）。
func NewInt(literal string) *Value {
	return &Value{Kind: KindNumber, Num: normalizeIntLiteral(literal)}
}

// NewFloat 返回按 Python repr 规则格式化的浮点数。
func NewFloat(f float64) *Value {
	if v, ok := specialFloat(f); ok {
		return &Value{Kind: KindNumber, Num: v}
	}
	return &Value{Kind: KindNumber, Num: formatFloat(f)}
}

// NewArray 返回数组。
func NewArray(items ...*Value) *Value {
	if items == nil {
		items = []*Value{}
	}
	return &Value{Kind: KindArray, Arr: items}
}

// NewObject 返回空对象。
func NewObject() *Value { return &Value{Kind: KindObject, Obj: NewObjectMap()} }

// IsNull 报告是否为 JSON null。
func (v *Value) IsNull() bool { return v == nil || v.Kind == KindNull }

// Object 是一个保留键插入顺序的 JSON 对象。
//
// Python 的 dict 在覆盖已有键时保留首次插入的位置，这里与之保持一致：重复键
// 只更新值，不移动位置。
type Object struct {
	keys []string
	vals map[string]*Value
}

// NewObjectMap 返回空对象。
func NewObjectMap() *Object {
	return &Object{vals: map[string]*Value{}}
}

// Len 返回成员数量。
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Keys 按插入顺序返回键的副本。
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}

// Get 按键取值。
func (o *Object) Get(key string) (*Value, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[key]
	return v, ok
}

// Has 报告键是否存在。
func (o *Object) Has(key string) bool {
	_, ok := o.Get(key)
	return ok
}

// Set 设置键值。新键追加到末尾，已有键只更新值。
func (o *Object) Set(key string, value *Value) {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

// Delete 删除键，报告是否删除成功。
func (o *Object) Delete(key string) bool {
	if _, exists := o.vals[key]; !exists {
		return false
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

// sortKeys 返回按键的 Unicode 码点升序排列的键副本。
//
// Python 的 sort_keys=True 按码点排序；对合法 UTF-8 而言与字节序一致，因此
// 直接比较字符串即可。
func (o *Object) sortKeys() []string {
	keys := o.Keys()
	slices.Sort(keys)
	return keys
}

// Parse 解析 JSON 文本。
//
// 语义对齐 Python 的 json.loads：接受 NaN / Infinity / -Infinity，整数保持
// 任意精度，浮点数按 repr 规则规范化，对象保留键插入顺序。
func Parse(data []byte) (*Value, error) {
	p := &parser{data: data}
	p.skipSpace()
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, p.errf("存在多余内容")
	}
	return v, nil
}

// ParseString 是 Parse 的字符串版本。
func ParseString(s string) (*Value, error) { return Parse([]byte(s)) }

type parser struct {
	data []byte
	pos  int
}

func (p *parser) errf(format string, args ...any) error {
	return fmt.Errorf("JSON 解析失败（偏移 %d）：%s", p.pos, fmt.Sprintf(format, args...))
}

func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) peek() byte {
	if p.pos < len(p.data) {
		return p.data[p.pos]
	}
	return 0
}

func (p *parser) hasPrefix(lit string) bool {
	return strings.HasPrefix(string(p.data[p.pos:]), lit)
}

func (p *parser) parseValue() (*Value, error) {
	if p.pos >= len(p.data) {
		return nil, p.errf("内容意外结束")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return NewString(s), nil
	case c == 't':
		if p.hasPrefix("true") {
			p.pos += 4
			return NewBool(true), nil
		}
		return nil, p.errf("非法字面量")
	case c == 'f':
		if p.hasPrefix("false") {
			p.pos += 5
			return NewBool(false), nil
		}
		return nil, p.errf("非法字面量")
	case c == 'n':
		if p.hasPrefix("null") {
			p.pos += 4
			return NewNull(), nil
		}
		if p.hasPrefix("nan") {
			p.pos += 3
			return &Value{Kind: KindNumber, Num: "NaN"}, nil
		}
		return nil, p.errf("非法字面量")
	case c == 'N':
		if p.hasPrefix("NaN") {
			p.pos += 3
			return &Value{Kind: KindNumber, Num: "NaN"}, nil
		}
		return nil, p.errf("非法字面量")
	case c == 'I':
		if p.hasPrefix("Infinity") {
			p.pos += len("Infinity")
			return &Value{Kind: KindNumber, Num: "Infinity"}, nil
		}
		return nil, p.errf("非法字面量")
	case c == '-' || (c >= '0' && c <= '9'):
		if p.hasPrefix("-Infinity") {
			p.pos += len("-Infinity")
			return &Value{Kind: KindNumber, Num: "-Infinity"}, nil
		}
		return p.parseNumber()
	}
	return nil, p.errf("非法字符 %q", string(rune(p.data[p.pos])))
}

func (p *parser) parseObject() (*Value, error) {
	p.pos++ // '{'
	obj := NewObjectMap()
	p.skipSpace()
	if p.peek() == '}' {
		p.pos++
		return &Value{Kind: KindObject, Obj: obj}, nil
	}
	for {
		p.skipSpace()
		if p.peek() != '"' {
			return nil, p.errf("对象键必须是字符串")
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.peek() != ':' {
			return nil, p.errf("对象键后缺少 ':'")
		}
		p.pos++
		p.skipSpace()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		obj.Set(key, value)
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return &Value{Kind: KindObject, Obj: obj}, nil
		default:
			return nil, p.errf("对象成员后缺少 ',' 或 '}'")
		}
	}
}

func (p *parser) parseArray() (*Value, error) {
	p.pos++ // '['
	arr := []*Value{}
	p.skipSpace()
	if p.peek() == ']' {
		p.pos++
		return &Value{Kind: KindArray, Arr: arr}, nil
	}
	for {
		p.skipSpace()
		item, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arr = append(arr, item)
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return &Value{Kind: KindArray, Arr: arr}, nil
		default:
			return nil, p.errf("数组元素后缺少 ',' 或 ']'")
		}
	}
}

func (p *parser) parseString() (string, error) {
	p.pos++ // 起始引号
	var b strings.Builder
	for {
		if p.pos >= len(p.data) {
			return "", p.errf("字符串未闭合")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c == '\\':
			p.pos++
			if p.pos >= len(p.data) {
				return "", p.errf("转义序列未完成")
			}
			switch e := p.data[p.pos]; e {
			case '"':
				b.WriteByte('"')
				p.pos++
			case '\\':
				b.WriteByte('\\')
				p.pos++
			case '/':
				b.WriteByte('/')
				p.pos++
			case 'b':
				b.WriteByte('\b')
				p.pos++
			case 'f':
				b.WriteByte('\f')
				p.pos++
			case 'n':
				b.WriteByte('\n')
				p.pos++
			case 'r':
				b.WriteByte('\r')
				p.pos++
			case 't':
				b.WriteByte('\t')
				p.pos++
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				b.WriteRune(r)
			default:
				return "", p.errf("非法转义 \\%s", string(rune(e)))
			}
		case c < 0x20:
			return "", p.errf("字符串中出现未转义的控制字符 0x%02x", c)
		default:
			r, size := utf8.DecodeRune(p.data[p.pos:])
			if r == utf8.RuneError && size == 1 {
				return "", p.errf("字符串包含非法 UTF-8 字节")
			}
			b.Write(p.data[p.pos : p.pos+size])
			p.pos += size
		}
	}
}

// parseUnicodeEscape 解析 \uXXXX，并在遇到代理对时合并为单个码点。
//
// Python 的 json 会把 \ud83d\ude00 解码成 U+1F600；孤立的代理项在 Python 里
// 会得到一个无法编码为 UTF-8 的字符，Go 的 string 无法表示，这里退化为
// U+FFFD（该情况不构成有效配置，语料也未覆盖）。
func (p *parser) parseUnicodeEscape() (rune, error) {
	p.pos++ // 'u'
	code, err := p.readHex4()
	if err != nil {
		return 0, err
	}
	r := rune(code)
	if !utf16.IsSurrogate(r) {
		return r, nil
	}
	// 尝试读取紧随的低/高代理项组成代理对。
	if p.pos+1 < len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
		save := p.pos
		p.pos += 2
		low, err := p.readHex4()
		if err == nil {
			if combined := utf16.DecodeRune(r, rune(low)); combined != utf8.RuneError {
				return combined, nil
			}
		}
		p.pos = save
	}
	return utf8.RuneError, nil
}

func (p *parser) readHex4() (uint32, error) {
	if p.pos+4 > len(p.data) {
		return 0, p.errf("\\u 转义需要 4 位十六进制")
	}
	var code uint32
	for i := 0; i < 4; i++ {
		c := p.data[p.pos+i]
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		default:
			return 0, p.errf("\\u 转义含非十六进制字符")
		}
		code = code<<4 | d
	}
	p.pos += 4
	return code, nil
}

// parseNumber 按 Python json 的严格文法解析数字。
//
// 整数保持十进制字面量（任意精度，超出 float64 也不丢精度）；浮点数解析为
// float64 后立即按 Python repr 规则格式化，因此 Value.Num 始终可直接输出。
func (p *parser) parseNumber() (*Value, error) {
	start := p.pos
	if p.peek() == '-' {
		p.pos++
	}
	switch {
	case p.peek() == '0':
		p.pos++
	case p.peek() >= '1' && p.peek() <= '9':
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	default:
		return nil, p.errf("数字缺少整数部分")
	}

	isFloat := false
	if p.peek() == '.' {
		if p.pos+1 >= len(p.data) || !isDigit(p.data[p.pos+1]) {
			return nil, p.errf("小数点后缺少数字")
		}
		p.pos++
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
		isFloat = true
	}
	if c := p.peek(); c == 'e' || c == 'E' {
		expStart := p.pos
		p.pos++
		if c := p.peek(); c == '+' || c == '-' {
			p.pos++
		}
		if !isDigit(p.peek()) {
			p.pos = expStart
			return nil, p.errf("指数部分缺少数字")
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
		isFloat = true
	}

	literal := string(p.data[start:p.pos])
	if !isFloat {
		return &Value{Kind: KindNumber, Num: normalizeIntLiteral(literal)}, nil
	}
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		// 溢出到 ±Inf：Python 的 json.loads("1e400") 同样得到 inf。
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return nil, p.errf("非法数字 %q", literal)
		}
	}
	return NewFloat(f), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// normalizeIntLiteral 规范整数十进制字面量。
//
// Python 把 "-0" 解析为 int 0 并输出 "0"，而把 "-0.0" 保留为浮点 -0.0；两者
// 必须区分，因此仅对整数调用本函数。
func normalizeIntLiteral(literal string) string {
	neg := strings.HasPrefix(literal, "-")
	digits := strings.TrimPrefix(literal, "-")
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0"
	}
	if neg {
		return "-" + digits
	}
	return digits
}
