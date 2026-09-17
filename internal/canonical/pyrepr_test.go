package canonical

import "testing"

// TestPyReprMatchesPython 锁定 Python repr 的引用符与转义规则。
//
// 期望值由 Python 3.12 实测得到（scripts 无覆盖，故直接写死）。这条路径只在
// 配置写错类型时触发——例如 upstream_routes 的路径填成列表，会得到
// "['a']/v1/chat/completions" 并回给用户。属于对外错误文本，必须逐字一致。
func TestPyReprMatchesPython(t *testing.T) {
	cases := []struct {
		input string // canonical JSON 文本
		want  string
	}{
		{`"a"`, `'a'`},
		{`""`, `''`},
		// 含单引号且不含双引号 -> 改用双引号包裹（不转义）。
		{`"it's"`, `"it's"`},
		{`"'"`, `"'"`},
		{`"''"`, `"''"`},
		// 不含单引号 -> 保持单引号，双引号不转义。
		{`"say \"hi\""`, `'say "hi"'`},
		{`"\"\""`, `'""'`},
		// 两种引号都有 -> 保持单引号并转义单引号。
		{`"both ' and \""`, `'both \' and "'`},
		{`"a'b\"c"`, `'a\'b"c'`},
		// 反斜杠与具名转义。
		{`"back\\slash"`, `'back\\slash'`},
		{`"nl\nx"`, `'nl\nx'`},
		{`"tab\tx"`, `'tab\tx'`},
		{`"cr\rx"`, `'cr\rx'`},
		// 控制字符用 \xNN（含 DEL）。
		{`"\u0001"`, `'\x01'`},
		{`"\u007f"`, `'\x7f'`},
		// 非 ASCII 原样输出，不转义。
		{`"中文"`, `'中文'`},
		{`"🙂"`, `'🙂'`},
	}
	for _, tc := range cases {
		value, err := ParseString(tc.input)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", tc.input, err)
		}
		if got := PyRepr(value); got != tc.want {
			t.Errorf("PyRepr(%s) = %s，期望 %s", tc.input, got, tc.want)
		}
	}
}

// TestPyReprContainers 锁定容器的 repr 形式与键顺序。
func TestPyReprContainers(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`[]`, `[]`},
		{`{}`, `{}`},
		{`[1,"a"]`, `[1, 'a']`},
		{`{"k":"v"}`, `{'k': 'v'}`},
		// 键顺序按插入顺序保留，不排序。
		{`{"k":1,"a":2}`, `{'k': 1, 'a': 2}`},
		{`[null,true,1.5]`, `[None, True, 1.5]`},
		{`{"a":[1,{"b":null}]}`, `{'a': [1, {'b': None}]}`},
	}
	for _, tc := range cases {
		value, err := ParseString(tc.input)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", tc.input, err)
		}
		if got := PyRepr(value); got != tc.want {
			t.Errorf("PyRepr(%s) = %s，期望 %s", tc.input, got, tc.want)
		}
	}
}

// TestPyStrVsStringValue 锁定两个转换函数的语义差异。
//
// 这个差异是真实缺陷的来源：config.py 里同时存在 `str(x)`（保留 None/False）
// 与 `str(x or "")`（把它们当空串），用错会静默改变配置。
func TestPyStrVsStringValue(t *testing.T) {
	cases := []struct {
		input       string
		wantPyStr   string
		wantStrOrEl string
	}{
		{`null`, "None", ""},
		{`false`, "False", ""},
		{`true`, "True", "True"},
		{`0`, "0", ""},
		{`0.0`, "0.0", ""},
		{`1.5`, "1.5", "1.5"},
		{`""`, "", ""},
		{`"x"`, "x", "x"},
		// 空容器是假值，得到空串；非空容器渲染成 repr。
		{`[]`, "[]", ""},
		{`{}`, "{}", ""},
		{`[1]`, "[1]", "[1]"},
		{`{"a":1}`, "{'a': 1}", "{'a': 1}"},
	}
	for _, tc := range cases {
		value, err := ParseString(tc.input)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", tc.input, err)
		}
		if got := value.PyStr(); got != tc.wantPyStr {
			t.Errorf("PyStr(%s) = %q，期望 %q", tc.input, got, tc.wantPyStr)
		}
		if got := value.StringValue(); got != tc.wantStrOrEl {
			t.Errorf("StringValue(%s) = %q，期望 %q", tc.input, got, tc.wantStrOrEl)
		}
	}
}

// TestPyIterate 锁定 Python 的迭代语义。
func TestPyIterate(t *testing.T) {
	cases := []struct {
		input string
		want  string // 以 "|" 连接，便于比较
	}{
		{`[]`, ""},
		{`[1,2]`, "1|2"},
		{`["a","b"]`, "a|b"},
		// str 迭代字符：池白名单写成 "ab" 会被展开成两个模型名。
		{`"ab"`, "a|b"},
		{`""`, ""},
		// dict 迭代键。
		{`{"x":1,"y":2}`, "x|y"},
	}
	for _, tc := range cases {
		value, err := ParseString(tc.input)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", tc.input, err)
		}
		items, err := PyIterate(value)
		if err != nil {
			t.Fatalf("PyIterate(%s) 报错: %v", tc.input, err)
		}
		got := ""
		for i, item := range items {
			if i > 0 {
				got += "|"
			}
			got += item
		}
		if got != tc.want {
			t.Errorf("PyIterate(%s) = %q，期望 %q", tc.input, got, tc.want)
		}
	}

	// 不可迭代类型必须报错，而不是静默返回空。
	for _, input := range []string{`1`, `1.5`, `true`, `null`} {
		value, err := ParseString(input)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", input, err)
		}
		if _, err := PyIterate(value); err == nil {
			t.Errorf("PyIterate(%s) 应报错", input)
		}
	}
}
