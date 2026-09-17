package protocol

import (
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestRequestKnownDeviations 记录**已知且有意保留**的与参照实现的差异：非对象输入，
// 以及 `_adapt_content_part` 里不可哈希的 part_type。
//
// Python 的真实行为来自 gen_deviations.py（与 gen_request.py）：
//
//	adapt_message_payload("just a string")  -> ValueError（dict("...") 失败）
//	adapt_message_payload([["messages",[]]]) -> {"messages":[]}（dict() 接受键值对序列）
//	adapt_message_payload(5)                 -> TypeError（"messages" in 5 不可迭代）
//	adapt_message_payload(None)              -> TypeError（同上）
//	estimate_anthropic_input_tokens(<非 dict>) -> AttributeError（payload.items() 不存在）
//	adapt_content_part({"type":["text"],...}) -> TypeError（unhashable type: 'list'）
//	adapt_content_part({"type":{"a":1},...}) -> TypeError（unhashable type: 'dict'）
//
// 这些输入都不构成合法的协议请求体（调用方传进来的始终是解析后的 JSON 对象），
// 参照实现遇到它们一律抛异常。Go 侧选择**最保守的兜底**而不是复刻异常：
//
//   - AdaptMessagePayload 对非对象原样返回（见 request.go 的注释）；
//   - EstimateAnthropicInputTokens 把非对象当成空请求体，返回下限 1
//     （Python 会 AttributeError，而该函数服务于 count_tokens，抛异常会让整个接口 500）；
//   - requestAdaptContentPart 把字符串比较之外的 type 退化成「无 type」，
//     继续走 text 字段 / JSON 兜底（见 request.go 的注释，Python 会 TypeError）。
//
// 这是刻意的护栏：若日后有人把这些兜底改成复刻异常，本测试必须一起改。
func TestRequestKnownDeviations(t *testing.T) {
	// 1. 顶层分派对非对象的兜底。
	for _, item := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "字符串", input: `"just a string"`, want: `"just a string"`},
		{name: "键值对序列（Python 会解析成对象）", input: `[["messages",[]]]`, want: `[["messages",[]]]`},
		{name: "数组", input: `[1,2]`, want: `[1,2]`},
		{name: "数字", input: `5`, want: `5`},
		{name: "null", input: `null`, want: `null`},
	} {
		t.Run("分派-"+item.name, func(t *testing.T) {
			requireDumps(t, item.input, item.want, AdaptMessagePayload)
		})
	}

	// 2. token 估算对非对象返回下限 1。
	for _, item := range []struct {
		name  string
		input string
	}{
		{name: "字符串", input: `"abc"`},
		{name: "数组", input: `[1,2]`},
		{name: "数字", input: `5`},
		{name: "null", input: `null`},
	} {
		t.Run("估算-"+item.name, func(t *testing.T) {
			payload, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := EstimateAnthropicInputTokens(payload); got != 1 {
				t.Errorf("非对象输入应返回下限 1，实际 %d", got)
			}
		})
	}

	// 3. 不可哈希的 part_type 退化到 text 字段（Python 抛 TypeError）。
	for _, item := range []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "type 是数组",
			input: `{"type":["text"],"text":"t"}`,
			want:  `{"type":"text","text":"t"}`,
		},
		{
			name:  "type 是对象",
			input: `{"type":{"a":1},"text":"t"}`,
			want:  `{"type":"text","text":"t"}`,
		},
		{
			name:  "type 是布尔",
			input: `{"type":true,"text":"t"}`,
			want:  `{"type":"text","text":"t"}`,
		},
		{
			name:  "type 是数字且无 text",
			input: `{"type":5}`,
			want:  `{"type":"text","text":"{\"type\":5}"}`,
		},
	} {
		t.Run("内容块-"+item.name, func(t *testing.T) {
			part, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := canonical.DumpsOrdered(requestAdaptContentPart(part)); got != item.want {
				t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", item.input, item.want, got)
			}
		})
	}
}

// TestRequestAdaptAnthropicToolNamePosition 覆盖 `function` 已带 name 键时的**键位置**。
//
// 期望值来自 gen_deviations.py 的 `function.name 为假值时的键位置`。Python 的写法是
// `adapted_func = dict(func)` 后 `adapted_func["name"] = ...`：name 键**已存在**时
// dict 赋值只更新值、**保留原位置**，因此 `{"name":"","parameters":{…}}` 补上 name 后
// 仍输出 `{"name":"top","parameters":{…}}`——不是把 name 挪到末尾。
//
// 这条差异只在「function 里已有 name 键但值为假」时可见；缺键时才是追加到末尾
// （见 TestRequestAdaptAnthropicToolMatchesPython 的 `function 缺 name 时补顶层 name`）。
func TestRequestAdaptAnthropicToolNamePosition(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "function.name 为空串时原位更新",
			input: `{"type":"function","function":{"name":"","parameters":{"z":1}},"name":"top"}`,
			want:  `{"type":"function","function":{"name":"top","parameters":{"z":1}}}`,
		},
		{
			name:  "function.name 为 null 时原位更新",
			input: `{"type":"function","function":{"name":null,"parameters":{"z":1}},"name":"top"}`,
			want:  `{"type":"function","function":{"name":"top","parameters":{"z":1}}}`,
		},
		{
			name:  "function 无 name 键时追加到末尾",
			input: `{"type":"function","function":{"parameters":{"z":1}},"name":"top"}`,
			want:  `{"type":"function","function":{"parameters":{"z":1},"name":"top"}}`,
		},
		{
			name:  "function 多键时追加到末尾",
			input: `{"type":"function","function":{"description":"d","parameters":{"z":1}},"name":"top"}`,
			want:  `{"type":"function","function":{"description":"d","parameters":{"z":1},"name":"top"}}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			tool, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			result, ok := requestAdaptAnthropicTool(tool)
			if !ok {
				t.Fatal("不应被过滤")
			}
			if got := canonical.DumpsOrdered(result); got != item.want {
				t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", item.input, item.want, got)
			}
		})
	}
}
