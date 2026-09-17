package canonical

import (
	"math"
	"strconv"
	"strings"
)

// parseInt 把**数字字面量**转成 int64，对齐 Python 的 int(number)。
//
// 两种情况必须区分（Python 亦如此）：
//   - 整数字面量精确解析，超出 int64 视为失败（Python 支持任意精度，Go 侧失败关闭）；
//   - 浮点字面量向零截断：int(8080.0) == 8080、int(-3.7) == -3。
//
// 字符串来源不能走这里——Python 的 int("60.0") 抛 ValueError，而 int(60.0) 成功。
// 字符串请用 ToInt / pyIntFromString。
func parseInt(literal string) (int64, bool) {
	if isIntLiteral(literal) {
		n, err := strconv.ParseInt(strings.TrimSpace(literal), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	f, ok := parseFloat(literal)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	// 越界时 Go 的浮点转整数结果未定义，先挡掉。
	if f >= math.MaxInt64 || f <= math.MinInt64 {
		return 0, false
	}
	return int64(f), true
}

// parseFloat 对齐 Python 对数字字面量的 float() 语义，并接受 NaN / Infinity。
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
