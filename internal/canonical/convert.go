package canonical

import (
	"math"
	"strconv"
	"strings"
)

// parseInt 对齐 Python 的 int() 语义。
//
// 与 parseFloat 分开的原因：Python 的 int("60.0") 会抛 ValueError（字符串只接受
// 整数写法），而 int(60.0) 会截断成 60（浮点可以转）。两者必须区分，否则
// config 里把 port 写成字符串 "8000.5" 时会被静默接受，而 Python 会直接报错。
func parseInt(literal string) (int64, bool) {
	s := strings.TrimSpace(literal)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return n, true
	}
	if !isIntLiteral(s) {
		// 含小数点或指数的字面量：Python 只会经 float 转换才接受，
		// 字符串来源不接受，这里同样拒绝。
		return 0, false
	}
	return 0, false
}

// parseFloat 对齐 Python 的 float() 语义，并接受 NaN / Infinity 字面量。
func parseFloat(literal string) (float64, bool) {
	s := strings.TrimSpace(literal)
	switch s {
	case "NaN", "nan":
		return math.NaN(), true
	case "Infinity", "inf", "infinity":
		return math.Inf(1), true
	case "-Infinity", "-inf", "-infinity":
		return math.Inf(-1), true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// isIntLiteral 报告字面量是否为整数写法（无小数点、无指数）。
func isIntLiteral(literal string) bool {
	if literal == "" {
		return false
	}
	return !strings.ContainsAny(literal, ".eE") &&
		!strings.Contains(literal, "Infinity") &&
		!strings.Contains(literal, "NaN")
}

// intFromNumber 把数字 Value 转成 int64，对齐 Python 的 int(number)。
//
// 整数走精确解析（大整数超出 int64 时失败）；浮点向零截断，与 Python 一致。
func intFromNumber(v *Value) (int64, bool) {
	if v == nil || v.Kind != KindNumber {
		return 0, false
	}
	if isIntLiteral(v.Num) {
		n, err := strconv.ParseInt(v.Num, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	f, ok := parseFloat(v.Num)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	// 越界时 Go 的转换结果未定义，先挡掉。
	if f >= math.MaxInt64 || f <= math.MinInt64 {
		return 0, false
	}
	return int64(f), true
}

// NewObjectOf 按键值对构造对象，保持传入顺序。
func NewObjectOf(pairs ...ObjectPair) *Value {
	obj := NewObjectMap()
	for _, pair := range pairs {
		obj.Set(pair.Key, pair.Value)
	}
	return &Value{Kind: KindObject, Obj: obj}
}

// ObjectPair 是 NewObjectOf 的键值对。
type ObjectPair struct {
	Key   string
	Value *Value
}

// NewIntValue 返回整数 Value。
func NewIntValue(n int64) *Value { return &Value{Kind: KindNumber, Num: strconv.FormatInt(n, 10)} }

// NewStringArray 构造字符串数组。
func NewStringArray(items []string) *Value {
	arr := make([]*Value, len(items))
	for i, item := range items {
		arr[i] = NewString(item)
	}
	return &Value{Kind: KindArray, Arr: arr}
}

// StringList 返回字符串数组的 Go 切片；非数组时返回 nil。
//
// 对齐 Python 里 “tuple(str(x) for x in value)“ 的写法：非数组会被调用方先挡掉，
// 这里只做元素渲染。
func (v *Value) StringList() []string {
	if v == nil || v.Kind != KindArray {
		return nil
	}
	out := make([]string, 0, len(v.Arr))
	for _, item := range v.Arr {
		out = append(out, item.StringValue())
	}
	return out
}
