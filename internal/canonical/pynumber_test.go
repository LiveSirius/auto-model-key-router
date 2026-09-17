package canonical

import (
	"errors"
	"math"
	"testing"
)

// TestPyIntMatchesPython 断言 ToInt 复刻 Python 的 int()。
//
// 期望值全部由 Python 3.12 实测得到（本机运行 eval 记录），不是推断的。
// wantErr 是 str(exc)（Python 的异常文本不含类名），wantKind 是异常类名——
// 两者分开是因为调用方靠类名决定是上报契约错误（ValueError）还是内部错误。
func TestPyIntMatchesPython(t *testing.T) {
	cases := []struct {
		name     string
		value    *Value
		want     int64
		wantKind string // 空表示期望成功
		wantErr  string
	}{
		{name: "int_number", value: NewFloat(8080.0), want: 8080},
		{name: "int_negative_float", value: NewFloat(-3.7), want: -3},
		{name: "int_positive_float", value: NewFloat(3.7), want: 3},
		{name: "int_negative_half", value: NewFloat(-0.5), want: 0},
		{name: "int_true", value: NewBool(true), want: 1},
		{name: "int_false", value: NewBool(false), want: 0},
		{name: "int_string", value: NewString("45"), want: 45},
		{name: "int_string_padded", value: NewString(" 45 "), want: 45},
		{name: "int_string_plus", value: NewString("+45"), want: 45},
		{name: "int_string_underscore", value: NewString("1_000"), want: 1000},
		{name: "int_string_negative", value: NewString("-45"), want: -45},
		{name: "int_string_fullwidth", value: NewString("４５"), want: 45},

		{
			name: "nan", value: NewFloat(math.NaN()),
			wantKind: "ValueError", wantErr: "cannot convert float NaN to integer",
		},
		{
			name: "inf", value: NewFloat(math.Inf(1)),
			wantKind: "OverflowError", wantErr: "cannot convert float infinity to integer",
		},
		{
			name: "neg_inf", value: NewFloat(math.Inf(-1)),
			wantKind: "OverflowError", wantErr: "cannot convert float infinity to integer",
		},
		{
			name: "string_float_literal", value: NewString("1.5"),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: '1.5'",
		},
		{
			name: "string_bad", value: NewString("soon"),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: 'soon'",
		},
		{
			// 错误文本保留原始输入的空格，不做 strip。
			name: "string_padded_bad", value: NewString(" 45x "),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: ' 45x '",
		},
		{
			name: "string_hex", value: NewString("0x10"),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: '0x10'",
		},
		{
			name: "string_empty", value: NewString(""),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: ''",
		},
		{
			name: "string_leading_underscore", value: NewString("_1"),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: '_1'",
		},
		{
			name: "string_trailing_underscore", value: NewString("1_"),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: '1_'",
		},
		{
			name: "string_inner_spaces", value: NewString(" 4 5 "),
			wantKind: "ValueError", wantErr: "invalid literal for int() with base 10: ' 4 5 '",
		},
		{
			name: "none", value: NewNull(),
			wantKind: "TypeError",
			wantErr:  "int() argument must be a string, a bytes-like object or a real number, not 'NoneType'",
		},
		{
			name: "list", value: NewArray(NewIntValue(1)),
			wantKind: "TypeError",
			wantErr:  "int() argument must be a string, a bytes-like object or a real number, not 'list'",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ToInt(testCase.value)
			if testCase.wantKind == "" {
				if err != nil {
					t.Fatalf("期望成功，实际报错: %v", err)
				}
				if got != testCase.want {
					t.Errorf("期望 %d，实际 %d", testCase.want, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望 %s，实际成功并返回 %d", testCase.wantKind, got)
			}
			if err.Error() != testCase.wantErr {
				t.Errorf("错误文本不一致\n期望: %s\n实际: %s", testCase.wantErr, err.Error())
			}
			assertErrorKind(t, err, testCase.wantKind)
		})
	}
}

// assertErrorKind 断言错误的具体类型。
func assertErrorKind(t *testing.T, err error, wantKind string) {
	t.Helper()
	switch wantKind {
	case "ValueError":
		var target *ValueError
		if !errors.As(err, &target) {
			t.Errorf("期望 ValueError，实际 %T", err)
		}
	case "TypeError":
		var target *TypeError
		if !errors.As(err, &target) {
			t.Errorf("期望 TypeError，实际 %T", err)
		}
	case "OverflowError":
		var target *OverflowError
		if !errors.As(err, &target) {
			t.Errorf("期望 OverflowError，实际 %T", err)
		}
	default:
		t.Fatalf("未知的期望异常类型: %s", wantKind)
	}
}

// TestPyFloatMatchesPython 断言 ToFloat 复刻 Python 的 float()。
func TestPyFloatMatchesPython(t *testing.T) {
	cases := []struct {
		name     string
		value    *Value
		want     float64
		wantNaN  bool
		wantKind string // 空表示期望成功
		wantErr  string
	}{
		{name: "float_number", value: NewFloat(12.5), want: 12.5},
		{name: "int_number", value: NewIntValue(3), want: 3.0},
		{name: "true", value: NewBool(true), want: 1.0},
		{name: "false", value: NewBool(false), want: 0.0},
		{name: "string_plain", value: NewString("45"), want: 45.0},
		{name: "string_decimal", value: NewString("1.5"), want: 1.5},
		{name: "string_padded", value: NewString("  1.5 "), want: 1.5},
		{name: "string_exponent", value: NewString("1e3"), want: 1000.0},
		{name: "string_underscore", value: NewString("1_0.5"), want: 10.5},
		{name: "string_nan", value: NewString("nan"), wantNaN: true},
		{name: "string_inf", value: NewString("inf"), want: math.Inf(1)},
		{name: "string_infinity", value: NewString("Infinity"), want: math.Inf(1)},
		{name: "string_neg_infinity", value: NewString("-infinity"), want: math.Inf(-1)},
		{name: "number_infinity_literal", value: NewFloat(math.Inf(-1)), want: math.Inf(-1)},

		{
			name: "string_bad", value: NewString("soon"),
			wantKind: "ValueError", wantErr: "could not convert string to float: 'soon'",
		},
		{
			name: "string_padded_bad", value: NewString(" 1.5x "),
			wantKind: "ValueError", wantErr: "could not convert string to float: ' 1.5x '",
		},
		{
			name: "string_nanx", value: NewString("nanx"),
			wantKind: "ValueError", wantErr: "could not convert string to float: 'nanx'",
		},
		{
			name: "string_bare_exponent", value: NewString("1e"),
			wantKind: "ValueError", wantErr: "could not convert string to float: '1e'",
		},
		{
			name: "string_empty", value: NewString(""),
			wantKind: "ValueError", wantErr: "could not convert string to float: ''",
		},
		{
			name: "none", value: NewNull(),
			wantKind: "TypeError",
			wantErr:  "float() argument must be a string or a real number, not 'NoneType'",
		},
		{
			name: "list", value: NewArray(),
			wantKind: "TypeError",
			wantErr:  "float() argument must be a string or a real number, not 'list'",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ToFloat(testCase.value)
			if testCase.wantKind != "" {
				if err == nil {
					t.Fatalf("期望 %s，实际成功并返回 %v", testCase.wantKind, got)
				}
				if err.Error() != testCase.wantErr {
					t.Errorf("错误文本不一致\n期望: %s\n实际: %s", testCase.wantErr, err.Error())
				}
				assertErrorKind(t, err, testCase.wantKind)
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际报错: %v", err)
			}
			if testCase.wantNaN {
				if !math.IsNaN(got) {
					t.Errorf("期望 NaN，实际 %v", got)
				}
				return
			}
			if got != testCase.want {
				t.Errorf("期望 %v，实际 %v", testCase.want, got)
			}
		})
	}
}

// TestParseIntTruncatesFloats 锁住 parseInt 对浮点字面量的截断行为。
//
// 这是本轮修复的回归点：旧实现让 parseInt 拒绝一切含小数点/指数的字面量，导致
// 配置里 port 写成 8080.0 时 Go 报错而 Python 静默接受为 8080。对拍语料
// port_as_float 正是捕获该差异的用例。
func TestParseIntTruncatesFloats(t *testing.T) {
	cases := []struct {
		literal string
		want    int64
		ok      bool
	}{
		{"8080", 8080, true},
		{"8080.0", 8080, true},
		{"-3.7", -3, true},
		{"3.7", 3, true},
		{"-0.5", 0, true},
		{"4.7e2", 470, true},
		{"1e2", 100, true},
		{"NaN", 0, false},
		{"Infinity", 0, false},
		{"-Infinity", 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.literal, func(t *testing.T) {
			got, ok := parseInt(testCase.literal)
			if ok != testCase.ok {
				t.Fatalf("ok 期望 %v，实际 %v", testCase.ok, ok)
			}
			if ok && got != testCase.want {
				t.Errorf("期望 %d，实际 %d", testCase.want, got)
			}
		})
	}
}
