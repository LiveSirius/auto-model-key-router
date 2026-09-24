package canonical

import (
	"math"
	"strings"
	"testing"
)

// TestDumpsIndentPreservesKeyOrder 锁定「不排序」这一语义差异。
//
// 若有人误把 Dumps 的排序选项用于落盘路径，本测试会失败——这正是需要防住的
// 回归，因为排序后的输出仍然是合法 JSON，不会被任何其它测试发现。
func TestDumpsIndentPreservesKeyOrder(t *testing.T) {
	value, err := ParseString(`{"z":1,"m":2,"a":3}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	indented := DumpsIndent(value, 2)
	compact := Dumps(value)

	if !strings.Contains(indented, `"z"`) || !strings.Contains(compact, `"a"`) {
		t.Fatal("测试自身构造有误")
	}
	// indent 形式保留插入顺序：z 出现在 a 之前。
	if strings.Index(indented, `"z"`) > strings.Index(indented, `"a"`) {
		t.Errorf("indent 形式不应排序键:\n%s", indented)
	}
	// canonical 形式必须排序：a 出现在 z 之前。
	if strings.Index(compact, `"a"`) > strings.Index(compact, `"z"`) {
		t.Errorf("canonical 形式应排序键: %s", compact)
	}
}

// TestDumpsIndentZeroAndNegative 覆盖 indent 边界。
//
// Python 已验证行为：indent=0 与 indent=-1 都输出换行但**不缩进**，且键值仍用
// ": " 分隔（与紧凑形式的 ":" 不同）。这里断言精确输出而非仅「可解析」。
func TestDumpsIndentZeroAndNegative(t *testing.T) {
	value, err := ParseString(`{"a":[1,2],"b":{"c":3}}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	const want = "{\n\"a\": [\n1,\n2\n],\n\"b\": {\n\"c\": 3\n}\n}"
	for _, indent := range []int{0, -1} {
		if got := DumpsIndent(value, indent); got != want {
			t.Errorf("indent=%d 输出不一致\n期望: %q\n实际: %q", indent, want, got)
		}
	}
}

// TestNewFloatFormatting 直接锁定浮点格式化规则。
//
// 这些是边界值，出错时能立刻看出是哪条规则错了。
//
// 注意：期望值必须经 runtimeFloat 构造。Go 的**无类型常量**算术在编译期以任意
// 精度求值，`0.1 + 0.2` 会得到精确的 0.3 并舍入为最接近 0.3 的 float64；
// 而 Python 在运行期用 float64 相加，得到 0.30000000000000004。若直接在表里写
// 常量表达式，测的就不是 Python 的行为。
func TestNewFloatFormatting(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{runtimeFloat(0.0), "0.0"},
		{runtimeFloat(1.0), "1.0"},
		{runtimeFloat(100.0), "100.0"},
		{runtimeFloat(math.Copysign(0, -1)), "-0.0"},
		{runtimeFloat(0.1), "0.1"},
		{runtimeFloat(1.5), "1.5"},
		{runtimeFloat(1e15), "1000000000000000.0"},
		{runtimeFloat(1e16), "1e+16"},
		{runtimeFloat(1e17), "1e+17"},
		{runtimeFloat(1e21), "1e+21"},
		{runtimeFloat(0.0001), "0.0001"},
		{runtimeFloat(1e-05), "1e-05"},
		{runtimeFloat(1e-06), "1e-06"},
		{runtimeFloat(1e-07), "1e-07"},
		{runtimeFloat(3.141592653589793), "3.141592653589793"},
		{runtimeAdd(0.1, 0.2), "0.30000000000000004"},
	}
	for _, tc := range cases {
		if got := NewFloat(tc.value).Num; got != tc.want {
			t.Errorf("NewFloat(%v) = %q，期望 %q", tc.value, got, tc.want)
		}
	}
}

// runtimeFloat 阻止编译器对浮点字面量做常量折叠，确保被测值就是 float64 值。
func runtimeFloat(f float64) float64 {
	var v float64 = f
	return v
}

// runtimeAdd 在运行期做 float64 加法，复现 Python 的运算结果。
func runtimeAdd(a, b float64) float64 {
	var x, y float64 = a, b
	return x + y
}

// TestParseFloatMatchesPythonRuntimeArithmetic 用解析路径交叉验证上面的结论：
// 文本 "0.1" 与 "0.2" 相加，必须与 Python 的 0.1+0.2 一致。
func TestParseFloatMatchesPythonRuntimeArithmetic(t *testing.T) {
	value, err := ParseString(`{"sum":0.30000000000000004}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got, _ := value.Obj.Get("sum")
	if got.Num != "0.30000000000000004" {
		t.Errorf("解析 0.30000000000000004 得到 %q", got.Num)
	}
	// 反过来：Go 常量折叠出的 0.3 必须序列化为 "0.3"，与 Python 的 0.3 一致。
	if s := NewFloat(runtimeAdd(0.1, 0.2)).Num; s != "0.30000000000000004" {
		t.Errorf("运行期 0.1+0.2 = %q，期望 0.30000000000000004", s)
	}
}

// TestSpecialFloats 锁定 NaN / Infinity 字面量（Python 的 allow_nan 默认行为）。
func TestSpecialFloats(t *testing.T) {
	cases := []struct {
		name  string
		value float64
		want  string
	}{
		{"nan", math.NaN(), "NaN"},
		{"inf", math.Inf(1), "Infinity"},
		{"neg_inf", math.Inf(-1), "-Infinity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewFloat(tc.value).Num; got != tc.want {
				t.Errorf("期望 %q，实际 %q", tc.want, got)
			}
		})
	}
}

// TestIntLiteralNormalization 锁定整数规范化：-0 变 0，但不影响 -0.0。
func TestIntLiteralNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0", "0"},
		{"-0", "0"},
		{"60", "60"},
		{"-1", "-1"},
		{"007", "7"},
		{"-007", "-7"},
		{"1180591620717411303424", "1180591620717411303424"},
	}
	for _, tc := range cases {
		if got := NewInt(tc.in).Num; got != tc.want {
			t.Errorf("NewInt(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	// -0.0 是浮点，必须保留符号。
	if got := NewFloat(math.Copysign(0, -1)).Num; got != "-0.0" {
		t.Errorf("NewFloat(-0.0) = %q，期望 \"-0.0\"", got)
	}
}

// TestObjectSemantics 覆盖对象的插入顺序与覆盖行为（对齐 Python dict）。
func TestObjectSemantics(t *testing.T) {
	value, err := ParseString(`{"b":1,"a":2,"b":3}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 重复键只更新值，不移动位置：b 仍在首位。
	if keys := value.Obj.Keys(); len(keys) != 2 || keys[0] != "b" || keys[1] != "a" {
		t.Errorf("重复键应保留首次插入位置，实际键序: %v", keys)
	}
	if v, _ := value.Obj.Get("b"); v.Num != "3" {
		t.Errorf("重复键应取最后一个值，实际 %q", v.Num)
	}

	// Set 与 Delete 的顺序语义。
	obj := NewObjectMap()
	obj.Set("x", NewInt("1"))
	obj.Set("y", NewInt("2"))
	obj.Set("x", NewInt("9"))
	if keys := obj.Keys(); len(keys) != 2 || keys[0] != "x" {
		t.Errorf("Set 覆盖不应移动位置: %v", keys)
	}
	obj.Delete("x")
	if keys := obj.Keys(); len(keys) != 1 || keys[0] != "y" {
		t.Errorf("Delete 后应只剩 y: %v", keys)
	}
	if obj.Delete("missing") {
		t.Error("Delete 不存在的键应返回 false")
	}
}

// TestParseRejectsInvalid 覆盖错误路径，避免解析器静默接受非法输入。
func TestParseRejectsInvalid(t *testing.T) {
	cases := []struct{ name, input string }{
		{"trailing_content", `{"a":1} extra`},
		{"unclosed_object", `{"a":1`},
		{"unclosed_string", `{"a":"x}`},
		{"missing_colon", `{"a" 1}`},
		{"trailing_comma_object", `{"a":1,}`},
		{"trailing_comma_array", `[1,]`},
		{"bare_key", `{a:1}`},
		{"leading_zero", `01`},
		{"empty_input", ``},
		{"bad_escape", `"\q"`},
		{"unterminated_escape", `"\`},
		{"control_in_string", "\"a\x01b\""},
		{"lone_minus", `-`},
		{"dot_without_int", `.5`},
		{"bad_exponent", `1e`},
		{"nan_lowercase_wrong", `nan_extra`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseString(tc.input); err == nil {
				t.Errorf("应拒绝非法输入 %q，但解析成功了", tc.input)
			}
		})
	}
}
