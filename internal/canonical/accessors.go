package canonical

import "strings"

// 本文件提供操作 Value 的便捷访问器。config 包以 *Value 作为动态 JSON 数据的
// 表示，而不是另建一套动态类型：Value 已经保留键的插入顺序与 int/float 区别，
// 这两点分别是「保存配置文件」与「config_revision」的硬要求。

// Clone 返回深拷贝。
//
// 迁移逻辑会就地改写嵌套结构，必须像 Python 的 deepcopy 一样与调用方隔离，
// 否则调用者传入的原始 payload 会被悄悄改掉。
func (v *Value) Clone() *Value {
	if v == nil {
		return NewNull()
	}
	switch v.Kind {
	case KindArray:
		items := make([]*Value, len(v.Arr))
		for i, item := range v.Arr {
			items[i] = item.Clone()
		}
		return &Value{Kind: KindArray, Arr: items}
	case KindObject:
		obj := NewObjectMap()
		if v.Obj != nil {
			for _, key := range v.Obj.keys {
				child, _ := v.Obj.Get(key)
				obj.Set(key, child.Clone())
			}
		}
		return &Value{Kind: KindObject, Obj: obj}
	default:
		copied := *v
		return &copied
	}
}

// IsObject 报告是否为 JSON 对象。
func (v *Value) IsObject() bool { return v != nil && v.Kind == KindObject }

// IsArray 报告是否为 JSON 数组。
func (v *Value) IsArray() bool { return v != nil && v.Kind == KindArray }

// IsString 报告是否为 JSON 字符串。
func (v *Value) IsString() bool { return v != nil && v.Kind == KindString }

// IsNumber 报告是否为 JSON 数字。
func (v *Value) IsNumber() bool { return v != nil && v.Kind == KindNumber }

// IsBool 报告是否为 JSON 布尔值。
func (v *Value) IsBool() bool { return v != nil && v.Kind == KindBool }

// Len 返回数组长度或对象成员数；其它类型返回 0。
func (v *Value) Len() int {
	if v == nil {
		return 0
	}
	switch v.Kind {
	case KindArray:
		return len(v.Arr)
	case KindObject:
		return v.Obj.Len()
	}
	return 0
}

// Index 返回数组元素；越界或非数组返回 nil。
func (v *Value) Index(i int) *Value {
	if v == nil || v.Kind != KindArray || i < 0 || i >= len(v.Arr) {
		return nil
	}
	return v.Arr[i]
}

// Items 返回数组元素；非数组返回 nil。
func (v *Value) Items() []*Value {
	if v == nil || v.Kind != KindArray {
		return nil
	}
	return v.Arr
}

// Lookup 按键取对象成员；非对象或键不存在返回 nil。
func (v *Value) Lookup(key string) *Value {
	if v == nil || v.Kind != KindObject || v.Obj == nil {
		return nil
	}
	child, _ := v.Obj.Get(key)
	return child
}

// LookupOK 按键取对象成员，并报告键是否存在。
func (v *Value) LookupOK(key string) (*Value, bool) {
	if v == nil || v.Kind != KindObject || v.Obj == nil {
		return nil, false
	}
	return v.Obj.Get(key)
}

// SetKey 在对象上设置成员；非对象时静默无操作。
func (v *Value) SetKey(key string, value *Value) {
	if v == nil || v.Kind != KindObject || v.Obj == nil {
		return
	}
	v.Obj.Set(key, value)
}

// DeleteKey 删除对象成员；非对象时静默无操作。
func (v *Value) DeleteKey(key string) {
	if v == nil || v.Kind != KindObject || v.Obj == nil {
		return
	}
	v.Obj.Delete(key)
}

// AsString 返回字符串值；非字符串返回零值与 false。
func (v *Value) AsString() (string, bool) {
	if v == nil || v.Kind != KindString {
		return "", false
	}
	return v.Str, true
}

// AsBool 返回布尔值；非布尔返回零值与 false。
func (v *Value) AsBool() (bool, bool) {
	if v == nil || v.Kind != KindBool {
		return false, false
	}
	return v.Bool, true
}

// AsInt 返回整数解析结果，等价于 Python 的 int(value) 成功路径。
//
// 第二个返回值报告是否成功；需要区分 ValueError 与 TypeError、或需要 Python 的
// 原始错误文本时，请用 ToInt。
func (v *Value) AsInt() (int64, bool) {
	parsed, err := ToInt(v)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// AsFloat 返回浮点解析结果，等价于 Python 的 float(value) 成功路径。
//
// 与 AsInt 同理，需要错误分类时请用 ToFloat。
func (v *Value) AsFloat() (float64, bool) {
	parsed, err := ToFloat(v)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// StringValue 返回 Python 的 str(value or "") 语义结果。
//
// 这是配置解析里出现最多的表达式，故单独提供：None/空串/0/false/**空容器**都
// 得到 ""，其余交给 PyStr。
//
// 空容器必须算假值：Python 的 `[] or ""` 是 ""，而 `[1] or ""` 是 [1]（进而
// 被 str() 渲染成 "[1]"）。config.py 的路径规范化正依赖这一点区分「未配置」
// 与「配错成列表」。
func (v *Value) StringValue() string {
	if v == nil {
		return ""
	}
	if !v.Truthy() {
		return ""
	}
	return v.PyStr()
}

// PyStr 返回 Python 的 str(value) 结果。
//
// 与 StringValue 的区别在 None 与布尔：str(None) 是 "None"、str(False) 是
// "False"，两者都是**非空**字符串。任务参数的 stop 列表用的正是 str(item)
// （config.py:172），若误用 StringValue，[null] 会变成 []，静默丢参数。
func (v *Value) PyStr() string {
	if v == nil {
		return "None"
	}
	switch v.Kind {
	case KindNull:
		return "None"
	case KindString:
		return v.Str
	case KindBool:
		if v.Bool {
			return "True"
		}
		return "False"
	case KindNumber:
		return v.Num
	case KindArray, KindObject:
		// Python 的 str() 与 repr() 对容器是同一形式（单引号、", " 分隔）。
		return PyRepr(v)
	}
	return ""
}

// PyRepr 返回 Python 的 repr(value) 结果。
//
// 需要它的原因是 Python 会把容器渲染成带单引号的字面量，并作为配置值使用
// （例如 upstream_routes 的路径若误填成列表，会得到 "['a']/v1/chat/completions"）。
// 这类输入是配置错误，但错误文本会回给用户，故仍需逐字一致。
func PyRepr(v *Value) string {
	if v == nil {
		return "None"
	}
	switch v.Kind {
	case KindString:
		return pyReprString(v.Str)
	case KindArray:
		parts := make([]string, 0, len(v.Arr))
		for _, item := range v.Arr {
			parts = append(parts, PyRepr(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case KindObject:
		if v.Obj == nil {
			return "{}"
		}
		parts := make([]string, 0, v.Obj.Len())
		for _, key := range v.Obj.keys {
			child, _ := v.Obj.Get(key)
			parts = append(parts, pyReprString(key)+": "+PyRepr(child))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return v.PyStr()
	}
}

// pyReprString 复刻 Python 的字符串 repr。
//
// 引用符选择与转义规则：默认单引号，若串内含单引号而不含双引号则改用双引号；
// 反斜杠与引用符转义，\n \r \t 用具名转义，其余控制字符用 \xNN。
func pyReprString(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case rune(quote):
			b.WriteByte('\\')
			b.WriteByte(quote)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(`\x`)
				const hexDigits = "0123456789abcdef"
				b.WriteByte(hexDigits[byte(r)>>4])
				b.WriteByte(hexDigits[byte(r)&0x0f])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// NotIterableError 表示按 Python 语义迭代了一个不可迭代的值。
//
// canonical 包不依赖 config 包，因此这里只报告事实，由调用方决定映射成哪种
// 对外错误类型。
type NotIterableError struct {
	// TypeName 是 Python 侧的类型名（int、float、bool 等）。
	TypeName string
}

func (e *NotIterableError) Error() string {
	return "'" + e.TypeName + "' object is not iterable"
}

// errNotIterable 构造 NotIterableError。
func errNotIterable(v *Value) error {
	return &NotIterableError{TypeName: PyTypeName(v)}
}

// PyTypeName 返回 Python 侧的类型名。
func PyTypeName(v *Value) string {
	if v == nil {
		return "NoneType"
	}
	switch v.Kind {
	case KindNull:
		return "NoneType"
	case KindBool:
		return "bool"
	case KindNumber:
		if isFloatLiteral(v.Num) {
			return "float"
		}
		return "int"
	case KindString:
		return "str"
	case KindArray:
		return "list"
	case KindObject:
		return "dict"
	}
	return "object"
}

// isFloatLiteral 报告数字字面量是否为浮点写法。
func isFloatLiteral(literal string) bool {
	return strings.ContainsAny(literal, ".eE") || literal == "NaN" ||
		strings.Contains(literal, "Infinity")
}

// Python 里 list 迭代元素、str 迭代字符、dict 迭代键，其余（None、int、float、
// bool）抛 TypeError。后三点容易被忽略：池白名单写成字符串 "ab" 时，Python 会
// 把它展开成 'a'、'b' 两个模型名；而 `for x in None` 同样是 TypeError，不是
// 「空迭代」。
//
// 若调用方要实现 “x or []“ 语义（None/空值先替换成空列表），应先做真值判断，
// 见 config 包的 iterateOrEmpty。
func PyIterate(v *Value) ([]string, error) {
	if v == nil || v.Kind == KindNull {
		return nil, errNotIterable(v)
	}
	switch v.Kind {
	case KindArray:
		out := make([]string, 0, len(v.Arr))
		for _, item := range v.Arr {
			out = append(out, item.PyStr())
		}
		return out, nil
	case KindString:
		out := make([]string, 0, len(v.Str))
		for _, r := range v.Str {
			out = append(out, string(r))
		}
		return out, nil
	case KindObject:
		if v.Obj == nil {
			return nil, nil
		}
		out := make([]string, 0, v.Obj.Len())
		for _, key := range v.Obj.keys {
			out = append(out, key)
		}
		return out, nil
	}
	return nil, errNotIterable(v)
}

// Truthy 返回 Python 真值判断结果（bool(value)）。
func (v *Value) Truthy() bool {
	if v == nil {
		return false
	}
	switch v.Kind {
	case KindNull:
		return false
	case KindBool:
		return v.Bool
	case KindNumber:
		return v.Num != "0" && v.Num != "0.0" && v.Num != "-0.0"
	case KindString:
		return v.Str != ""
	case KindArray:
		return len(v.Arr) > 0
	case KindObject:
		return v.Obj.Len() > 0
	}
	return false
}
