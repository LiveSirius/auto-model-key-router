package canonical

import (
	"strings"
	"testing"
)

// TestDumpsOrderedPreservesKeyOrder 锁定「紧凑 + 不排序」的语义。
//
// 这是协议转换层 SSE 事件的编码形式（protocols/anthropic.py:227）。它与 Dumps
// 只差排序一件事，但排序后的输出仍是合法 JSON，任何只校验「能解析」的测试都发现
// 不了这个回归——所以必须逐字节断言。
func TestDumpsOrderedPreservesKeyOrder(t *testing.T) {
	value, err := ParseString(`{"z":1,"m":2,"a":3}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 键顺序必须与输入一致。
	if got, want := DumpsOrdered(value), `{"z":1,"m":2,"a":3}`; got != want {
		t.Fatalf("顺序未保留: 期望 %s，实际 %s", want, got)
	}
	// 对照：Dumps 会排序，两者必须不同。
	if Dumps(value) == DumpsOrdered(value) {
		t.Fatal("Dumps 与 DumpsOrdered 不应产生相同结果（前者应排序）")
	}
}

// TestDumpsOrderedMatchesPython 断言与 Python 的紧凑不排序形式逐字节一致。
//
// 期望值取自参照实现：
//
//	json.dumps(obj, ensure_ascii=False, separators=(",", ":"))
func TestDumpsOrderedMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "嵌套对象保留插入顺序",
			input: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
			want:  `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		},
		{
			name:  "键非字典序时不被重排",
			input: `{"z":1,"m":2,"a":3}`,
			want:  `{"z":1,"m":2,"a":3}`,
		},
		{
			name:  "分隔符紧凑且带空格",
			input: `{"a": 1, "b": [1, 2, 3]}`,
			want:  `{"a":1,"b":[1,2,3]}`,
		},
		{
			name:  "空容器保持紧凑",
			input: `{"a":{},"b":[]}`,
			want:  `{"a":{},"b":[]}`,
		},
		{
			name:  "ASCII 不转义非 ASCII 字符",
			input: `{"text":"中文🙂","html":"<b>&</b>"}`,
			want:  `{"text":"中文🙂","html":"<b>&</b>"}`,
		},
		{
			name:  "控制字符仍按 JSON 规则转义",
			input: `{"text":"line1\nline2\ttab"}`,
			want:  `{"text":"line1\nline2\ttab"}`,
		},
		{
			name:  "整数浮点与布尔 null",
			input: `{"i":1,"f":1.5,"g":8080.0,"t":true,"n":null}`,
			want:  `{"i":1,"f":1.5,"g":8080.0,"t":true,"n":null}`,
		},
		{
			name:  "数组内对象保留各自顺序",
			input: `{"items":[{"z":1,"a":2},{"y":3,"b":4}]}`,
			want:  `{"items":[{"z":1,"a":2},{"y":3,"b":4}]}`,
		},
		{
			// 与 Dumps 一致的浮点形态：1e16 起用指数形式。
			name:  "大整数与指数浮点",
			input: `{"big":1180591620717411303424,"exp":1e16}`,
			want:  `{"big":1180591620717411303424,"exp":1e+16}`,
		},
		{
			// U+2028/U+2029 在 Python 的 ensure_ascii=False 下不转义。
			name:  "行分隔符不转义",
			input: `{"s":"a\u2028b\u2029c"}`,
			want:  "{\"s\":\"a\u2028b\u2029c\"}",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			value, err := ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := DumpsOrdered(value); got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
				reportFirstDiff(t, item.want, got)
			}
		})
	}
}

// TestDumpsOrderedAgreesWithIndentZero 交叉验证 encodeOptions 的配置正确。
//
// Python 的 indent=0 会插换行但不缩进，因此把 DumpsIndent(v, 0) 里每行左侧的空白
// 去掉、再把换行删除，只保留对象/数组层级差异——这不足以直接比较。
// 更直接的自检是：两者对**键顺序**必须给出相同结论（都不排序）。
func TestDumpsOrderedAgreesWithIndentZero(t *testing.T) {
	value, err := ParseString(`{"z":{"y":1},"a":[{"n":2}]}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 两者都不排序：各自输出里第一个出现的键名都应是 z。
	ordered := DumpsOrdered(value)
	indented := DumpsIndent(value, 0)
	if !strings.HasPrefix(ordered, `{"z"`) {
		t.Fatalf("DumpsOrdered 首个键应为 z，实际输出 %s", ordered)
	}
	// indent=0 会在 { 之后插入换行（Python 的行为），所以不能按下标取字符。
	if !strings.HasPrefix(indented, "{\n\"z\"") {
		t.Fatalf("DumpsIndent 首个键应为 z，实际输出 %s", indented)
	}
	// 且两者都不应等于排序后的形式（Dumps 把 a 排到最前）。
	if sortedForm := Dumps(value); !strings.HasPrefix(sortedForm, `{"a"`) {
		t.Fatalf("Dumps 首个键应为 a（排序），实际输出 %s", sortedForm)
	}
}

// TestDumpsOrderedNestedDepthIsFlat 验证紧凑形式不插入任何换行或空格。
func TestDumpsOrderedNestedDepthIsFlat(t *testing.T) {
	value, err := ParseString(`{"a":{"b":{"c":[1,{"d":2}]}}}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := DumpsOrdered(value)
	if got != `{"a":{"b":{"c":[1,{"d":2}]}}}` {
		t.Fatalf("紧凑形式不符: %s", got)
	}
	for _, ch := range got {
		if ch == '\n' || ch == ' ' || ch == '\t' {
			t.Fatalf("紧凑形式不应含空白字符: %q", got)
		}
	}
}
