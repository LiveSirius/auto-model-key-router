package protocol

import (
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 本文件的**全部期望值**都由真实 Python 实现打印得到，不是自己推导的。生成方式：
//
//	cd D:\Code\auto-model-key-router
//	$env:PYTHONIOENCODING='utf-8'
//	python -X utf8 <temp>\gen_responses.py          # 非流式 response / output items / usage
//	python -X utf8 <temp>\gen_responses_stream.py   # 流式事件序列与 output items
//	python -X utf8 <temp>\gen_responses_edges.py    # index 边界、残留半行
//	python -X utf8 <temp>\gen_responses_mojibake.py # 分片乱码实证
//
// （脚本放在系统临时目录，用完即删，不留在仓库里。）脚本调用
// auto_model_key_router.protocols.responses 的转换函数，用
// json.dumps(..., ensure_ascii=False, separators=(",", ":")) 打印结果，再把文本
// 粘进下面的表里。这与 anthropic_test.go 的做法一致，是与参照实现做差分测试的
// 唯一可信方式。

// collectResponsesStream 把若干块喂进 Responses 流式转换器，返回全部事件文本。
//
// withFlush 为 true 时追加收尾路径：对应 proxy_handler.py:1359 的
// `if buffer.strip(): _responses_stream_events(buffer, b"\n", ...)`，本模块用 Flush
// 提供等价入口。
func collectResponsesStream(t *testing.T, chunks []string, withFlush bool) []string {
	t.Helper()
	state := NewResponsesStreamState()
	var got []string
	for _, chunk := range chunks {
		events, _, err := state.Push([]byte(chunk))
		if err != nil {
			t.Fatalf("Push 失败: %v", err)
		}
		for _, event := range events {
			got = append(got, string(event))
		}
	}
	if withFlush {
		events, _, err := state.Flush()
		if err != nil {
			t.Fatalf("Flush 失败: %v", err)
		}
		for _, event := range events {
			got = append(got, string(event))
		}
	}
	return got
}

// TestResponsesResponseMatchesPython 锁定非流式重建的逐字节输出。
//
// 期望值来自 gen_responses.py 的 `=== responses_response ===`。要点：
//   - 上游本来就是 Responses 形态时**原样返回**（不重建）；
//   - 没有 choices 时返回 `output: []` 的空壳，而不是 nil；
//   - message 不是字典 / 缺失时走「无内容」分支，补一条空 output_text；
//   - `id` 用 `or` 兜底，因此 0 / false 都退到 "resp_amkr"，而数字 12345 被 str()；
//   - usage 不是字典时按空处理。
func TestResponsesResponseMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "没有 choices",
			input: `{"id":"x"}`,
			want:  `{"id":"x","object":"response","status":"completed","model":"req-model","output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "choices 不是数组",
			input: `{"id":"x","choices":"y","usage":{"prompt_tokens":1}}`,
			want:  `{"id":"x","object":"response","status":"completed","model":"req-model","output":[],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":1}}`,
		},
		{
			name:  "choices 为空",
			input: `{"id":"x","choices":[]}`,
			want:  `{"id":"x","object":"response","status":"completed","model":"req-model","output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "choices 里只有非字典",
			input: `{"id":"x","choices":[1,"a"]}`,
			want:  `{"id":"x","object":"response","status":"completed","model":"req-model","output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			// object == "response" 但 output 不是数组：**不**走早退，照常重建。
			name:  "object=response 但 output 不是数组",
			input: `{"object":"response","output":{}}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "纯文本",
			input: `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"你好，世界"},"finish_reason":"stop"}]}`,
			want:  `{"id":"chatcmpl-1","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"你好，世界"}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "message 不是字典",
			input: `{"id":"x","choices":[{"message":"nope"}]}`,
			want:  `{"id":"x","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "message 缺失",
			input: `{"id":"x","choices":[{"finish_reason":"stop"}]}`,
			want:  `{"id":"x","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "工具调用",
			input: `{"id":"x","choices":[{"message":{"content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
			want:  `{"id":"x","object":"response","status":"completed","model":"req-model","output":[{"type":"function_call","id":"fc_amkr_0","call_id":"call_9","name":"f","arguments":"{\"a\":1}"}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "旧式 function_call",
			input: `{"choices":[{"message":{"function_call":{"name":"g","arguments":"{\"z\":0}"}}}]}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[{"type":"function_call","id":"fc_amkr_0","call_id":"call_amkr_0","name":"g","arguments":"{\"z\":0}"}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			// content 数组里的数字 3 被**丢弃**（MessageText 不序列化非字典非字符串）。
			name:  "content 数组",
			input: `{"choices":[{"message":{"content":[{"type":"text","text":"a"},"b",3]}}]}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"ab"}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "数字 id 被渲染成字符串",
			input: `{"id":12345,"choices":[{"message":{"content":"y"}}]}`,
			want:  `{"id":"12345","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"y"}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			// `str(data.get("id") or "resp_amkr")`：0 是假值，退到默认 id。
			name:  "id 为 0 退到默认",
			input: `{"id":0,"choices":[{"message":{"content":"y"}}]}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"y"}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "id 为 false 退到默认",
			input: `{"id":false,"choices":[{"message":{"content":"y"}}]}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"y"}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "空 content 补空文本块",
			input: `{"choices":[{"message":{"content":""}}]}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "usage 不是字典时当作空",
			input: `{"choices":[{"message":{"content":"y"}}],"usage":[]}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"y"}]}],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`,
		},
		{
			name:  "usage 归一进输出",
			input: `{"choices":[{"message":{"content":"y"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"cached_tokens":9}}`,
			want:  `{"id":"resp_amkr","object":"response","status":"completed","model":"req-model","output":[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"y"}]}],"usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":9},"output_tokens":4,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":7}}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			data, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			result, err := ResponsesResponse(data, "req-model")
			if err != nil {
				t.Fatalf("不应返回错误: %v", err)
			}
			if result == nil {
				t.Fatal("不应返回 nil")
			}
			if got := canonical.DumpsOrdered(result); got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}
}

// TestResponsesResponseReturnsNil 覆盖非对象输入。
//
// 期望值来自 gen_responses.py 的 `非对象-数组` / `非对象-字符串` 两行（都输出 None）；
// 调用方据此回 502（proxy_handler.py:985 的 `if converted is None`）。
func TestResponsesResponseReturnsNil(t *testing.T) {
	for _, item := range []struct {
		name string
		json string
	}{
		{name: "数组", json: `[1,2]`},
		{name: "字符串", json: `"hi"`},
		{name: "数字", json: `5`},
		{name: "null", json: `null`},
	} {
		t.Run(item.name, func(t *testing.T) {
			data, err := canonical.ParseString(item.json)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			result, err := ResponsesResponse(data, "m")
			if err != nil {
				t.Fatalf("不应返回错误: %v", err)
			}
			if result != nil {
				t.Errorf("应返回 nil，实际 %s", canonical.DumpsOrdered(result))
			}
		})
	}
}

// TestResponsesResponsePassthrough 断言已是 Responses 形态的响应体**原样返回同一个
// 对象**（不复制、不规范化）。
//
// 期望值来自 gen_responses.py 的 `已是 response 形态`：输出与输入逐字节相同，顺序
// 仍是上游给的 `type, object, output, id`，说明没有被重建过。
func TestResponsesResponsePassthrough(t *testing.T) {
	const input = `{"type":"x","object":"response","output":[{"type":"message"}],"id":"r1"}`
	data, err := canonical.ParseString(input)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	result, err := ResponsesResponse(data, "req-model")
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if result != data {
		t.Fatal("应返回同一个对象（参照实现直接 return data）")
	}
	if got := canonical.DumpsOrdered(result); got != input {
		t.Errorf("输出应逐字节等于输入\n期望: %s\n实际: %s", input, got)
	}
}

// TestResponsesMessageOutputItemsMatchesPython 锁定 output 数组的构造。
//
// 期望值来自 gen_responses.py 的 `=== responses_message_output_items ===`。几个
// 非显然的点：
//   - 非字典工具项被跳过，但**枚举下标照样前进**，所以第三个元素的 id 是 fc_amkr_2；
//   - tool_calls 存在时即使为空数组也**不会**回落到 function_call（判的是
//     isinstance(list)）；
//   - 缺 function 的项被跳过，于是落到「无内容」分支补一条空 message；
//   - arguments 非字符串时先 `or {}` 再序列化，故 0 / [] 都变 "{}"，而真值数组变 "[1,2]"；
//   - arguments 是字符串时**原样保留**（空串也保留），不补 {}。
func TestResponsesMessageOutputItemsMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "空对象补空文本块",
			input: `{}`,
			want:  `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}]`,
		},
		{
			name:  "content 为 null",
			input: `{"content":null}`,
			want:  `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}]`,
		},
		{
			name:  "content 数组丢弃裸数字",
			input: `{"content":[{"type":"text","text":"a"},"b",3]}`,
			want:  `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"ab"}]}]`,
		},
		{
			name:  "跳过非字典但下标前进",
			input: `{"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}},"skip",{"function":{"name":"g","arguments":""}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"c1","name":"f","arguments":"{\"a\":1}"},{"type":"function_call","id":"fc_amkr_2","call_id":"call_amkr_2","name":"g","arguments":""}]`,
		},
		{
			name:  "空的 tool_calls 不回落 function_call",
			input: `{"tool_calls":[],"function_call":{"name":"h","arguments":"{}"}}`,
			want:  `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}]`,
		},
		{
			name:  "只有 function_call 时补 call_amkr_0",
			input: `{"function_call":{"name":"h","arguments":"{\"k\":\"中\"}"}}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"call_amkr_0","name":"h","arguments":"{\"k\":\"中\"}"}]`,
		},
		{
			name:  "tool_calls 不是列表时回落",
			input: `{"tool_calls":"x","function_call":{"name":"h"}}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"call_amkr_0","name":"h","arguments":"{}"}]`,
		},
		{
			name:  "工具缺 function 被跳过",
			input: `{"tool_calls":[{"id":"c"}]}`,
			want:  `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}]`,
		},
		{
			// 对象参数原样保留：**不**做 canonical 排序，键顺序即上游顺序（b 在 a 前）。
			name:  "arguments 是对象",
			input: `{"tool_calls":[{"id":"c","function":{"name":"f","arguments":{"b":2,"a":"中"}}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"f","arguments":"{\"b\":2,\"a\":\"中\"}"}]`,
		},
		{
			name:  "arguments 是数字 0",
			input: `{"tool_calls":[{"id":"c","function":{"name":"f","arguments":0}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"f","arguments":"{}"}]`,
		},
		{
			name:  "arguments 是空数组",
			input: `{"tool_calls":[{"id":"c","function":{"name":"f","arguments":[]}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"f","arguments":"{}"}]`,
		},
		{
			name:  "arguments 是真值数组",
			input: `{"tool_calls":[{"id":"c","function":{"name":"f","arguments":[1,2]}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"f","arguments":"[1,2]"}]`,
		},
		{
			name:  "工具 id 是数字",
			input: `{"tool_calls":[{"id":7,"function":{"name":"f","arguments":"{}"}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"7","name":"f","arguments":"{}"}]`,
		},
		{
			name:  "工具 id 为空串",
			input: `{"tool_calls":[{"id":"","function":{"name":"f","arguments":"{}"}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"call_amkr_0","name":"f","arguments":"{}"}]`,
		},
		{
			name:  "工具 name 是数字",
			input: `{"tool_calls":[{"id":"c","function":{"name":5,"arguments":"{}"}}]}`,
			want:  `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"5","arguments":"{}"}]`,
		},
		{
			name:  "文本加工具",
			input: `{"content":"先说","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]}`,
			want:  `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"先说"}]},{"type":"function_call","id":"fc_amkr_0","call_id":"c1","name":"f","arguments":"{}"}]`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			message, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			got := canonical.DumpsOrdered(ResponsesMessageOutputItems(message))
			if got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}
}

// TestResponsesUsageMatchesPython 锁定 usage 归一。
//
// 期望值来自 gen_responses.py 的 `=== responses_usage ===`。字段探测链一律是
// Python 的 `or`：
//   - `prompt_tokens: 0` 退到 `input_tokens`；
//   - `total_tokens: 0`（数字零）退到 input+output 求和，而字符串 "0" 是真值、
//     直接 int("0") == 0；
//   - 顶层 `cached_tokens` 优先于 `input_tokens_details` 里的同名键；
//   - 字符串计数被 int() 解析（"12" → 12），布尔 true 变 1、false 变 0；
//   - 负数是真值不回落，浮点向零截断。
func TestResponsesUsageMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "空对象",
			input: `{}`,
			want:  `{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}`,
		},
		{
			name:  "标准字段",
			input: `{"prompt_tokens":11,"completion_tokens":22}`,
			want:  `{"input_tokens":11,"input_tokens_details":{"cached_tokens":0},"output_tokens":22,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":33}`,
		},
		{
			name:  "input/output 字段",
			input: `{"input_tokens":5,"output_tokens":6}`,
			want:  `{"input_tokens":5,"input_tokens_details":{"cached_tokens":0},"output_tokens":6,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":11}`,
		},
		{
			name:  "零值退到第二字段",
			input: `{"prompt_tokens":0,"input_tokens":9,"completion_tokens":0,"output_tokens":4}`,
			want:  `{"input_tokens":9,"input_tokens_details":{"cached_tokens":0},"output_tokens":4,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":13}`,
		},
		{
			name:  "input_tokens_details 缓存",
			input: `{"prompt_tokens":1,"completion_tokens":2,"input_tokens_details":{"cached_tokens":7}}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":7},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "input_tokens_details 旧键 cache_read",
			input: `{"prompt_tokens":1,"completion_tokens":2,"input_tokens_details":{"cache_read_input_tokens":7}}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":7},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "顶层 cached 优先于 details",
			input: `{"prompt_tokens":1,"completion_tokens":2,"cached_tokens":8,"input_tokens_details":{"cached_tokens":7}}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":8},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "顶层 cache_read 优先级低于 cached",
			input: `{"prompt_tokens":1,"completion_tokens":2,"cache_read_input_tokens":4,"cached_tokens":8}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":8},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "input_tokens_details 非字典时被忽略",
			input: `{"prompt_tokens":1,"completion_tokens":2,"input_tokens_details":[1],"cached_tokens":3}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":3},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "reasoning_tokens",
			input: `{"prompt_tokens":1,"completion_tokens":2,"reasoning_tokens":5}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":3}`,
		},
		{
			name:  "total 显式覆盖求和",
			input: `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":99}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":99}`,
		},
		{
			name:  "total 字符串零是真值",
			input: `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":"0"}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}`,
		},
		{
			name:  "total 数字零回落到求和",
			input: `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":0}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "浮点向零截断",
			input: `{"prompt_tokens":1.9,"completion_tokens":2.0}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "字符串计数被解析",
			input: `{"prompt_tokens":"12","completion_tokens":"3"}`,
			want:  `{"input_tokens":12,"input_tokens_details":{"cached_tokens":0},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":15}`,
		},
		{
			name:  "布尔计数",
			input: `{"prompt_tokens":true,"completion_tokens":false}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":1}`,
		},
		{
			name:  "负数不回落",
			input: `{"prompt_tokens":-3,"completion_tokens":-4}`,
			want:  `{"input_tokens":-3,"input_tokens_details":{"cached_tokens":0},"output_tokens":-4,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":-7}`,
		},
		{
			// 空容器是**假值**：`or` 链继续往后找（或落到默认 0），不会去 int({})。
			// 期望值来自 gen_usage_falsy.py 的三行 `{}` 用例。
			name:  "缓存字段为空容器时按假值处理",
			input: `{"prompt_tokens":1,"completion_tokens":2,"cached_tokens":{}}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "total 为空容器时回落到求和",
			input: `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":{}}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
		{
			name:  "reasoning 为空容器时按假值处理",
			input: `{"prompt_tokens":1,"completion_tokens":2,"reasoning_tokens":{}}`,
			want:  `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			usage, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			result, err := ResponsesUsage(usage)
			if err != nil {
				t.Fatalf("不应返回错误: %v", err)
			}
			if got := canonical.DumpsOrdered(result); got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}

	// usage 缺失（nil）与 `usage or {}` 等价。
	result, err := ResponsesUsage(nil)
	if err != nil {
		t.Fatalf("nil usage 不应返回错误: %v", err)
	}
	const wantNil = `{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}`
	if got := canonical.DumpsOrdered(result); got != wantNil {
		t.Errorf("nil usage 不一致\n期望: %s\n实际: %s", wantNil, got)
	}
}

// TestResponsesUsageNonConvertibleIsError 记录**已知且有意保留**的与参照实现的差异。
//
// 参照实现在 responses.py:231 直接写 `int(...)`：遇到无法转换的计数（字典/数组/
// NaN/Infinity）会抛 TypeError / ValueError / OverflowError。非流式路径因此变成
// 500，而流式路径的异常会穿透生成器被外层捕获，结果是**静默截断整条响应流**。
//
// Go 侧如实返回 error，让调用方决定记 500 还是带 0 继续（见 ResponsesUsage 的
// 注释）。期望的“Python 异常类型”来自 gen_responses.py 的 `容器计数` / `NaN` / `inf`
// 三行，它们分别打印 EXC:TypeError / EXC:ValueError / EXC:OverflowError。
//
// 这是刻意的护栏：若日后把这里的 error 改成静默 0，本测试必须一起改。
func TestResponsesUsageNonConvertibleIsError(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		python string
	}{
		{name: "容器计数", input: `{"prompt_tokens":{"x":1},"completion_tokens":[1]}`, python: "TypeError"},
		{name: "NaN", input: `{"prompt_tokens":NaN,"completion_tokens":1}`, python: "ValueError"},
		{name: "Infinity", input: `{"prompt_tokens":Infinity,"completion_tokens":1}`, python: "OverflowError"},
		{name: "output 侧容器", input: `{"prompt_tokens":1,"completion_tokens":[1]}`, python: "TypeError"},
		{name: "缓存字段容器", input: `{"prompt_tokens":1,"completion_tokens":2,"cached_tokens":{"a":1}}`, python: "TypeError"},
		{name: "total 容器", input: `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":{"a":1}}`, python: "TypeError"},
		{name: "reasoning 容器", input: `{"prompt_tokens":1,"completion_tokens":2,"reasoning_tokens":{"a":1}}`, python: "TypeError"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			usage, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			result, err := ResponsesUsage(usage)
			if err == nil {
				t.Fatalf("参照实现抛 %s；Go 应上报 error，实际返回 %s",
					item.python, canonical.DumpsOrdered(result))
			}
			if result != nil {
				t.Errorf("出错时应返回 nil，实际 %s", canonical.DumpsOrdered(result))
			}
		})
	}
}

// TestResponsesStreamTextEventsBytes 锁定纯文本流的**逐字节** SSE 输出。
//
// 期望值来自 gen_responses_stream.py 的 `--- text ---`。首次出现文本时才发
// output_item.added + content_part.added，后续文本复用同一组 id/index。
func TestResponsesStreamTextEventsBytes(t *testing.T) {
	got := collectResponsesStream(t, []string{
		sseLine(`{"choices":[{"delta":{"content":"He"}}]}`),
		sseLine(`{"choices":[{"delta":{"content":"llo"},"finish_reason":"length"}]}`),
	}, false)

	requireEvents(t, got, []string{
		ev("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_amkr","role":"assistant","status":"in_progress","content":[]}}`),
		ev("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_amkr","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`),
		ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_amkr","output_index":0,"content_index":0,"delta":"He"}`),
		ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_amkr","output_index":0,"content_index":0,"delta":"llo"}`),
	})
}

// TestResponsesStreamMultibyteWholeLine 断言完整一行的多字节文本原样输出。
//
// 期望值来自 gen_responses_stream.py 的 `--- multibyte ---`。这是分片场景的对照
// 基准：只要行是完整的，就不该出现任何 U+FFFD。
func TestResponsesStreamMultibyteWholeLine(t *testing.T) {
	got := collectResponsesStream(t, []string{
		sseLine(`{"choices":[{"delta":{"content":"中文内容🚀"}}]}`),
	}, false)

	requireEvents(t, got, []string{
		ev("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_amkr","role":"assistant","status":"in_progress","content":[]}}`),
		ev("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_amkr","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`),
		ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_amkr","output_index":0,"content_index":0,"delta":"中文内容🚀"}`),
	})
}

// TestResponsesStreamSplitMultibyteIsRepaired 证明分片多字节字符被修复。
//
// 参照实现把每块 TCP 数据独立 `decode("utf-8", errors="replace")` 后再拼成 buffer
// （responses.py:15）。当一个多字节字符横跨两块时，两个分片各自解码都会失败并产出
// U+FFFD，于是 "中文内容" 会静默变成 "\uFFFD\uFFFD\uFFFD文内容"——gen_responses_mojibake.py
// 已按**字节级切点**实证（NAIVE_HAS_REPLACEMENT True、TEXT_HAS_REPLACEMENT True）。
//
// Go 侧改用 SSESplitter，在字节层面找换行，拿到完整一行才解码，因此切分点的多字节
// 序列能正确接上。本测试同时做两件事：
//
//  1. 证明朴素做法（每块各自解码）确实会产出 U+FFFD —— 避免这个测试变成永远通过
//     的废测试；
//  2. 断言切分器产出的事件与「整行一次喂入」**逐字节相同**。
func TestResponsesStreamSplitMultibyteIsRepaired(t *testing.T) {
	line := sseLine(`{"choices":[{"delta":{"content":"中文内容"}}]}`)

	// 切在「中」的三个 UTF-8 字节中间（截断成 1 字节 + 2 字节），与
	// gen_responses_mojibake.py 的切点一致。
	cut := strings.Index(line, "中") + 1
	if cut <= 0 || cut >= len(line) {
		t.Fatalf("切点计算异常: %d（行长度 %d）", cut, len(line))
	}
	first, second := line[:cut], line[cut:]

	// 1. 朴素做法必须真的坏掉，否则这个测试没有意义。
	naive := replaceDecode([]byte(first)) + replaceDecode([]byte(second))
	if !strings.Contains(naive, "\uFFFD") {
		t.Fatalf("朴素逐块解码未复现乱码，测试前提不成立：%q", naive)
	}

	// 2. 切分器必须修复它，且与整行喂入逐字节一致。
	whole := collectResponsesStream(t, []string{line}, false)
	split := collectResponsesStream(t, []string{first, second}, false)
	requireEvents(t, split, whole)

	joined := strings.Join(split, "")
	if strings.Contains(joined, "\uFFFD") {
		t.Fatalf("分片输入产出了 U+FFFD：%q", joined)
	}
	if !strings.Contains(joined, `"delta":"中文内容"`) {
		t.Fatalf("分片输入未还原出完整中文：%q", joined)
	}
}

// TestResponsesStreamSplitMultibyteToolArguments 断言工具参数里的多字节字符同样能
// 跨越 TCP 分片。
//
// 期望值来自 gen_responses_mojibake.py 的 TOOL 段：参数只有在**整行**解析出来后
// 才知道，因此切在「北」的字节中间时，第一块不应产出任何事件。
func TestResponsesStreamSplitMultibyteToolArguments(t *testing.T) {
	line := sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"北京"}}]}}]}`)
	cut := strings.Index(line, "北") + 1
	if cut <= 0 || cut >= len(line) {
		t.Fatalf("切点计算异常: %d（行长度 %d）", cut, len(line))
	}

	state := NewResponsesStreamState()
	early, _, err := state.Push([]byte(line[:cut]))
	if err != nil {
		t.Fatalf("Push 失败: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("半个多字节字符不该产出事件，实际 %d 个", len(early))
	}
	if _, _, err := state.Push([]byte(line[cut:])); err != nil {
		t.Fatalf("Push 失败: %v", err)
	}

	// 工具调用不产生 SSE 事件，只在收尾的 output items 里体现（TOOL EVENTS 0）。
	want := `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"n","arguments":"北京"}]`
	if got := canonical.DumpsOrdered(state.OutputItems()); got != want {
		t.Errorf("工具项不一致\n期望: %s\n实际: %s", want, got)
	}
}

// TestResponsesStreamToolOutputItems 锁定工具调用的累积与收尾输出。
//
// 期望值来自 gen_responses_stream.py 的 `--- tool ---`、`--- name_chunks ---`、
// `--- null_arguments ---`、`--- skip_non_dict_tool_call ---`、`--- empty_fields ---`、
// `--- negative_index ---` 各段。要点：
//   - 工具增量**不产出 SSE 事件**（与 anthropic.py 不同），只在 OutputItems 里体现；
//   - name 与 arguments 都是**累加**的（上游把名字拆成多块时必须续接）；
//   - arguments 为 null 时不追加任何内容，收尾时空串兜底成 "{}"；
//   - 缺 index 时回落到**枚举下标**，非字典元素被跳过但枚举照样前进；
//   - call_id 空串兜底成 call_amkr_{index}。
func TestResponsesStreamToolOutputItems(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name: "两片拼出参数",
			chunks: []string{
				sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`),
				sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"北京\"}"}}]}}]}`),
			},
			want: `[{"type":"function_call","id":"fc_amkr_0","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"}]`,
		},
		{
			name: "名字分块累加",
			chunks: []string{
				sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"ge"}}]}}]}`),
				sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"t_x"}}]}}]}`),
			},
			want: `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"get_x","arguments":"{}"}]`,
		},
		{
			name:   "arguments 为 null 不追加",
			chunks: []string{sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":null}}]}}]}`)},
			want:   `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"n","arguments":"{}"}]`,
		},
		{
			name:   "非字典元素被跳过但下标前进",
			chunks: []string{sseLine(`{"choices":[{"delta":{"tool_calls":["skip",{"index":2,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`)},
			want:   `[{"type":"function_call","id":"fc_amkr_2","call_id":"c","name":"n","arguments":"{}"}]`,
		},
		{
			name:   "全空字段兜底",
			chunks: []string{sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"","arguments":""}}]}}]}`)},
			want:   `[{"type":"function_call","id":"fc_amkr_0","call_id":"call_amkr_0","name":"","arguments":"{}"}]`,
		},
		{
			name:   "缺 index 回落到枚举下标",
			chunks: []string{sseLine(`{"choices":[{"delta":{"tool_calls":[{"id":"c0","function":{"name":"b","arguments":"["}}]}}]}`)},
			want:   `[{"type":"function_call","id":"fc_amkr_0","call_id":"c0","name":"b","arguments":"["}]`,
		},
		{
			name:   "负 index",
			chunks: []string{sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":-2,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`)},
			want:   `[{"type":"function_call","id":"fc_amkr_-2","call_id":"c","name":"n","arguments":"{}"}]`,
		},
		{
			name:   "参数是 JSON 值",
			chunks: []string{sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":{"a":[1,2],"b":"中"}}}]}}]}`)},
			want:   `[{"type":"function_call","id":"fc_amkr_0","call_id":"call_amkr_0","name":"","arguments":"{\"a\":[1,2],\"b\":\"中\"}"}]`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			state := NewResponsesStreamState()
			for _, chunk := range item.chunks {
				events, _, err := state.Push([]byte(chunk))
				if err != nil {
					t.Fatalf("Push 失败: %v", err)
				}
				if len(events) != 0 {
					t.Fatalf("工具增量不应产出 SSE 事件，实际 %d 个", len(events))
				}
			}
			if got := canonical.DumpsOrdered(state.OutputItems()); got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}
}

// TestResponsesStreamInterleavedToolsOrdering 覆盖多个工具交错到达时的排序。
//
// 期望值来自 gen_responses_stream.py 的 `--- interleaved ---`：收尾按 index
// **升序**（Python 的 `sorted(dict)`），因此 index 0 排在 index 1 之前，与到达顺序
// 无关。这与 anthropic.py 的「活跃工具最先」规则不同。
func TestResponsesStreamInterleavedToolsOrdering(t *testing.T) {
	state := NewResponsesStreamState()
	for _, chunk := range []string{
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"a","arguments":"{"}}]}}]}`),
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"id":"c0","function":{"name":"b","arguments":"["}}]}}]}`),
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}},{"function":{"arguments":"]"}}]}}]}`),
	} {
		if _, _, err := state.Push([]byte(chunk)); err != nil {
			t.Fatalf("Push 失败: %v", err)
		}
	}
	const want = `[{"type":"function_call","id":"fc_amkr_0","call_id":"c0","name":"b","arguments":"["},{"type":"function_call","id":"fc_amkr_1","call_id":"c1","name":"a","arguments":"{}]"}]`
	if got := canonical.DumpsOrdered(state.OutputItems()); got != want {
		t.Errorf("输出不一致\n期望: %s\n实际: %s", want, got)
	}
}

// TestResponsesStreamOutputItemsMessageRule 覆盖 `if text or not tool_calls` 的分支。
//
// 期望值来自 gen_responses_stream.py：
//   - 纯文本 → 一条 message；
//   - 空 delta / 空文本 / 无 delta → 一条**空** message（`not tool_calls` 为真）；
//   - 只有工具 → **不**多出空 message（这是与 anthropic 的一致性要求）。
func TestResponsesStreamOutputItemsMessageRule(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "纯文本",
			chunks: []string{sseLine(`{"choices":[{"delta":{"content":"hi"}}]}`)},
			want:   `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]`,
		},
		{
			name:   "空 delta",
			chunks: []string{sseLine(`{"choices":[{"delta":{}}]}`)},
			want:   `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}]`,
		},
		{
			name:   "空文本",
			chunks: []string{sseLine(`{"choices":[{"delta":{"content":""}}]}`)},
			want:   `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":""}]}]`,
		},
		{
			name:   "只有工具不补空 message",
			chunks: []string{sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`)},
			want:   `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"n","arguments":"{}"}]`,
		},
		{
			name: "文本加工具",
			chunks: []string{
				sseLine(`{"choices":[{"delta":{"content":"先说","tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`),
			},
			want: `[{"type":"message","id":"msg_amkr","role":"assistant","content":[{"type":"output_text","text":"先说"}]},{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"n","arguments":"{}"}]`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			state := NewResponsesStreamState()
			for _, chunk := range item.chunks {
				if _, _, err := state.Push([]byte(chunk)); err != nil {
					t.Fatalf("Push 失败: %v", err)
				}
			}
			if got := canonical.DumpsOrdered(state.OutputItems()); got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}
}

// TestResponsesStreamSkipsMalformedLines 断言坏载荷被**静默跳过**。
//
// 期望值来自 gen_responses_stream.py 的 `--- bad_lines ---`：坏 JSON、非对象 JSON、
// 空 data、注释行、其它事件名、[DONE] 都不产出事件也不报错——上游流里出现一行垃圾
// 不该中断整条响应。
//
// 同一行为在 anthropic_test.go 的 TestAnthropicStreamSkipsMalformedLines 里也有
// 覆盖；这里保留一份是因为 responses.py 与 anthropic.py 的坏行处理是**各自独立**
// 实现的（前者用 _json_text，后者用 jsonText），任一条回归都要能被发现。
func TestResponsesStreamSkipsMalformedLines(t *testing.T) {
	got := collectResponsesStream(t, []string{
		"data: {oops\n\n",
		"event: ping\n\n",
		"data: [1,2]\n\n",
		"data:\n\n",
		": comment\n\n",
		"data: [DONE]\n\n",
		sseLine(`{"choices":[{"delta":{"content":"ok"}}]}`),
	}, false)

	requireEvents(t, got, []string{
		ev("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_amkr","role":"assistant","status":"in_progress","content":[]}}`),
		ev("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_amkr","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`),
		ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_amkr","output_index":0,"content_index":0,"delta":"ok"}`),
	})
}

// TestResponsesStreamCRLFLineEnding 断言 \r\n 行尾同样被接受。
//
// 期望值来自 gen_responses_stream.py 的 `--- crlf ---`：参照实现靠 line.strip() 吃掉
// \r，切分器在字节层面去掉两端空白，效果一致。
func TestResponsesStreamCRLFLineEnding(t *testing.T) {
	got := collectResponsesStream(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"crlf\"}}]}\r\n",
	}, false)

	requireEvents(t, got, []string{
		ev("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_amkr","role":"assistant","status":"in_progress","content":[]}}`),
		ev("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_amkr","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`),
		ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_amkr","output_index":0,"content_index":0,"delta":"crlf"}`),
	})
}

// TestResponsesStreamTrailingPartialLine 覆盖流结束时没有换行结尾的半行。
//
// 期望值来自 gen_responses_stream.py 的 `--- partial_no_newline ---`（0 事件，缓冲区
// 留着）与 `--- partial_then_newline ---`（3 事件）。参照实现把这一步放在
// proxy_handler.py:1359，本模块用 Flush 提供等价入口。
func TestResponsesStreamTrailingPartialLine(t *testing.T) {
	state := NewResponsesStreamState()
	early, _, err := state.Push([]byte(`data: {"choices":[{"delta":{"content":"tail"}}]}`))
	if err != nil {
		t.Fatalf("Push 失败: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("没有换行结尾时不该产出事件，实际 %d 个", len(early))
	}
	if state.Pending() == 0 {
		t.Fatal("半行应留在缓冲区里")
	}

	events, _, err := state.Flush()
	if err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	var got []string
	for _, event := range events {
		got = append(got, string(event))
	}
	requireEvents(t, got, []string{
		ev("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_amkr","role":"assistant","status":"in_progress","content":[]}}`),
		ev("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_amkr","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`),
		ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_amkr","output_index":0,"content_index":0,"delta":"tail"}`),
	})
	// 再次 Flush 不应重复产出（缓冲已清空）。
	again, _, err := state.Flush()
	if err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("重复 Flush 应无事件，实际 %d 个", len(again))
	}

	// 纯空白残留：对应 gen_responses_stream.py 的 `--- only_spaces ---` 与
	// `--- spaces_then_newline ---`，两者都是 0 事件。
	blank := NewResponsesStreamState()
	if _, _, err := blank.Push([]byte("   ")); err != nil {
		t.Fatalf("Push 失败: %v", err)
	}
	blankEvents, _, err := blank.Flush()
	if err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	if len(blankEvents) != 0 {
		t.Fatalf("空白残留不该产出事件，实际 %d 个", len(blankEvents))
	}
}

// TestResponsesStreamUsageExtractionAndOverwrite 覆盖流式 usage 的提取与覆盖语义。
//
// 期望值来自 gen_responses_stream.py 的 `--- usage_overwrite ---`：usage 是**后者
// 覆盖前者**（Push 的返回值即本块的值），未出现过时为 nil。归一化由调用方走
// ResponsesUsage，因此这里断言原始引用再经 ResponsesUsage 的结果。
func TestResponsesStreamUsageExtractionAndOverwrite(t *testing.T) {
	state := NewResponsesStreamState()
	if _, usage, err := state.Push([]byte(sseLine(`{"choices":[{"delta":{"content":"a"}}]}`))); err != nil {
		t.Fatalf("Push 失败: %v", err)
	} else if usage != nil {
		t.Fatalf("无 usage 字段时应返回 nil，实际 %s", canonical.DumpsOrdered(usage))
	}

	_, first, err := state.Push([]byte(sseLine(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)))
	if err != nil {
		t.Fatalf("Push 失败: %v", err)
	}
	if got, want := canonical.DumpsOrdered(first), `{"prompt_tokens":1,"completion_tokens":2}`; got != want {
		t.Fatalf("usage 原始引用不一致\n期望: %s\n实际: %s", want, got)
	}
	normalized, err := ResponsesUsage(first)
	if err != nil {
		t.Fatalf("ResponsesUsage 失败: %v", err)
	}
	if got, want := canonical.DumpsOrdered(normalized), `{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}`; got != want {
		t.Fatalf("首次归一不符\n期望: %s\n实际: %s", want, got)
	}

	_, second, err := state.Push([]byte(sseLine(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":8}}`)))
	if err != nil {
		t.Fatalf("Push 失败: %v", err)
	}
	if got, want := canonical.DumpsOrdered(second), `{"prompt_tokens":7,"completion_tokens":8}`; got != want {
		t.Fatalf("后块应覆盖前块\n期望: %s\n实际: %s", want, got)
	}
}

// TestResponsesStreamIndexConversionMatchesPython 覆盖工具 index 的整数转换。
//
// 期望值来自 gen_responses_edges.py 的索引边界段：
//   - `"0"`（字符串）/ `1.9`（浮点）/ `true`（布尔）都能被 int() 接受，分别得到
//     0 / 1 / 1，与 Go 的 canonical.ToInt 一致；
//   - `null` / `""` / 对象无法转换：Python 抛 TypeError / ValueError，
//     **异常穿透生成器后被外层捕获，结果是静默截断整条响应流**。Go 侧如实返回
//     error（见 Push 的注释），让调用方能记录与参照实现同等的日志。
func TestResponsesStreamIndexConversionMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		index string
		want  string
	}{
		{name: "字符串 0", index: `"0"`, want: `[{"type":"function_call","id":"fc_amkr_0","call_id":"c","name":"n","arguments":"{}"}]`},
		{name: "浮点 1.9 截断", index: `1.9`, want: `[{"type":"function_call","id":"fc_amkr_1","call_id":"c","name":"n","arguments":"{}"}]`},
		{name: "布尔 true", index: `true`, want: `[{"type":"function_call","id":"fc_amkr_1","call_id":"c","name":"n","arguments":"{}"}]`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			state := NewResponsesStreamState()
			chunk := sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":` + item.index + `,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`)
			if _, _, err := state.Push([]byte(chunk)); err != nil {
				t.Fatalf("Push 失败: %v", err)
			}
			if got := canonical.DumpsOrdered(state.OutputItems()); got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}

	for _, item := range []struct {
		name   string
		index  string
		python string
	}{
		{name: "null", index: `null`, python: "TypeError"},
		{name: "空串", index: `""`, python: "ValueError"},
		{name: "对象", index: `{"a":1}`, python: "TypeError"},
	} {
		t.Run("无法转换-"+item.name, func(t *testing.T) {
			state := NewResponsesStreamState()
			chunk := sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":` + item.index + `,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`)
			events, usage, err := state.Push([]byte(chunk))
			if err == nil {
				t.Fatalf("参照实现抛 %s；Go 应上报 error，实际 events=%d usage=%v",
					item.python, len(events), usage)
			}
		})
	}
}
