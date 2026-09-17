package protocol

import (
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 本文件的**全部期望值**都由真实 Python 实现打印得到，不是自己推导的。生成方式：
//
//	cd D:\Code\auto-model-key-router
//	$env:PYTHONIOENCODING='utf-8'
//	python -X utf8 <temp>\gen_request.py         # 分派、参数规范化、input 展开、内容块
//	python -X utf8 <temp>\gen_request_helpers.py # 工具列表推导与 any(...) 判定
//	python -X utf8 <temp>\gen_request_order.py   # pop 再赋值造成的键顺序位移
//
// （脚本放在系统临时目录，用完即删，不留在仓库里。）脚本调用
// auto_model_key_router.protocols.request 与 protocols.common 的转换函数，用
// json.dumps(..., ensure_ascii=False, separators=(",", ":")) 打印结果，再把文本
// 粘进下面的表里。键的插入顺序即上游看到的字段顺序，因此断言的是完整序列化
// 字符串，而不是解析后的等价性。

// requireDumps 解析输入 JSON、调用转换函数、逐个比对序列化结果。
func requireDumps(t *testing.T, input string, want string, fn func(*canonical.Value) *canonical.Value) {
	t.Helper()
	payload, err := canonical.ParseString(input)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := canonical.DumpsOrdered(fn(payload))
	if got != want {
		t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", input, want, got)
	}
}

// TestAdaptMessagePayloadDispatch 覆盖顶层分派的三条分支与顺序。
//
// 期望值来自 gen_request.py 的 `=== adapt_message_payload ===`。要点：
//   - `messages` 优先于 `input`（两个都有时走 Anthropic 分支）；
//   - `messages` 不是列表时原样返回，但**仍然过一遍参数规范化**；
//   - 两个键都没有时只做参数规范化；
//   - 非对象输入原样返回（参照实现只在 dict 上定义）。
func TestAdaptMessagePayloadDispatch(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "messages 分支",
			input: `{"messages":[{"role":"user","content":"hi"}],"max_tokens":5}`,
			want:  `{"messages":[{"role":"user","content":"hi"}],"max_tokens":5}`,
		},
		{
			name:  "messages 非列表原样返回但仍规范化",
			input: `{"messages":"x","system":"s","max_tokens":5}`,
			want:  `{"messages":"x","system":"s","max_tokens":5}`,
		},
		{
			name:  "input 分支",
			input: `{"input":"hi","instructions":"be nice"}`,
			want:  `{"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "都没有时只规范化",
			input: `{"model":"m","max_output_tokens":9}`,
			want:  `{"model":"m","max_tokens":9}`,
		},
		{
			name:  "chat 体不动内容",
			input: `{"model":"m","stream":true,"max_tokens":3,"unknown":1}`,
			want:  `{"model":"m","stream":true,"max_tokens":3,"unknown":1}`,
		},
		{
			// 非对象原样返回：Go 侧的保守兜底（Python 对字符串会做子串匹配）。
			name:  "非对象原样返回",
			input: `"just a string"`,
			want:  `"just a string"`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			requireDumps(t, item.input, item.want, AdaptMessagePayload)
		})
	}
}

// TestNormalizeChatCompatParametersMatchesPython 覆盖参数规范化。
//
// 期望值来自 gen_request.py 的 `=== normalize_chat_compat_parameters ===` 与
// gen_request_order.py 的 `=== normalize 键顺序 ===`。三处顺序细节：
//   - `max_output_tokens` → `max_tokens` 用的是「先 pop 再赋值」，所以新键**追加到
//     末尾**（`{"max_output_tokens":9,"b":2,"c":3}` → `{"b":2,"c":3,"max_tokens":9}`）；
//   - `max_tokens` 已存在时只删 `max_output_tokens`，已有值保持**原位**；
//   - 剔除列表里的 `metadata` 会把调用方自带的 metadata 一起丢掉，这是参照实现的
//     既有行为，照搬。
func TestNormalizeChatCompatParametersMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "max_output_tokens 转 max_tokens",
			input: `{"model":"m","max_output_tokens":9}`,
			want:  `{"model":"m","max_tokens":9}`,
		},
		{
			name:  "max_output_tokens 在最前时 max_tokens 移到末尾",
			input: `{"max_output_tokens":9,"b":2,"c":3}`,
			want:  `{"b":2,"c":3,"max_tokens":9}`,
		},
		{
			name:  "两者都在时保留 max_tokens 原位",
			input: `{"max_tokens":1,"max_output_tokens":9}`,
			want:  `{"max_tokens":1}`,
		},
		{
			name:  "两者都在且顺序相反",
			input: `{"max_output_tokens":9,"max_tokens":1,"z":2}`,
			want:  `{"max_tokens":1,"z":2}`,
		},
		{
			name:  "只有 max_tokens",
			input: `{"max_tokens":1}`,
			want:  `{"max_tokens":1}`,
		},
		{
			name:  "stop_sequences 转 stop",
			input: `{"stop_sequences":["a"]}`,
			want:  `{"stop":["a"]}`,
		},
		{
			name:  "stop_sequences 在最前时 stop 移到末尾",
			input: `{"stop_sequences":["s"],"b":2}`,
			want:  `{"b":2,"stop":["s"]}`,
		},
		{
			name:  "两者都在时保留 stop 原位",
			input: `{"stop":["x"],"stop_sequences":["a"]}`,
			want:  `{"stop":["x"]}`,
		},
		{
			name:  "两者都移动",
			input: `{"max_output_tokens":9,"stop_sequences":["s"],"z":1}`,
			want:  `{"z":1,"max_tokens":9,"stop":["s"]}`,
		},
		{
			name:  "剔除全部噪声键",
			input: `{"anthropic_version":"2023","metadata":{"u":1},"reasoning":{},"text":{},"truncation":"auto","previous_response_id":"r","include":[],"store":true,"safety_identifier":"s","keep":1}`,
			want:  `{"keep":1}`,
		},
		{
			name:  "剔除键在最前",
			input: `{"metadata":{"u":1},"keep":1,"store":true}`,
			want:  `{"keep":1}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			requireDumps(t, item.input, item.want, requestNormalizeChatCompatParameters)
		})
	}
}

// TestEstimateAnthropicInputTokensMatchesPython 锁定 /v1/messages/count_tokens 的
// 本地估算（proxy_handler.py:312）。
//
// 期望值来自 gen_request.py 的 `=== estimate_anthropic_input_tokens ===`。这是
// 用户可见的整数，必须逐字一致。要点：
//   - `max(1, (utf8_len + 3) // 4)`，因此空内容也给 1；
//   - 参与计算的字段是**除 model / stream / max_tokens / max_output_tokens 之外的
//     全部字段**，包括未知字段；
//   - 长度按 **UTF-8 字节数** 计，所以中文与 emoji 比字符数更"重"；
//   - 序列化顺序即原请求体的键顺序（`{"messages":[],"unknown":1}` 与
//     `{"unknown":1,"messages":[]}` 长度相同，但更复杂的嵌套会不同）。
func TestEstimateAnthropicInputTokensMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{name: "空对象", input: `{}`, want: 1},
		{name: "只有 model", input: `{"model":"claude"}`, want: 1},
		{name: "只有 stream", input: `{"stream":true}`, want: 1},
		{name: "只有 max_tokens", input: `{"max_tokens":100}`, want: 1},
		{name: "只有 max_output_tokens", input: `{"max_output_tokens":100}`, want: 1},
		{name: "全部被剔除", input: `{"model":"m","stream":false,"max_tokens":1,"max_output_tokens":2}`, want: 1},
		{name: "单字符内容", input: `{"messages":[{"role":"user","content":"a"}]}`, want: 11},
		{name: "空内容", input: `{"messages":[]}`, want: 4},
		{name: "中文内容", input: `{"messages":[{"role":"user","content":"你好"}]}`, want: 13},
		{name: "emoji", input: `{"messages":[{"role":"user","content":"🚀"}]}`, want: 12},
		{name: "未知字段参与", input: `{"model":"m","unknown":[1,2,3],"messages":[]}`, want: 9},
		{name: "键顺序不同", input: `{"messages":[],"unknown":1}`, want: 7},
		{name: "键顺序反过来", input: `{"unknown":1,"messages":[]}`, want: 7},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			payload, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := EstimateAnthropicInputTokens(payload); got != item.want {
				t.Errorf("token 估算不一致\n输入: %s\n期望: %d\n实际: %d",
					item.input, item.want, got)
			}
		})
	}
}

// TestRequestResponsesInputToMessagesMatchesPython 覆盖 input 的四种形态。
//
// 期望值来自 gen_request.py 的 `=== responses_input_to_messages ===`。几个容易错的点：
//   - 裸字符串 → 一条 user 消息；非列表非字符串 → 整个值过 `_adapt_content` 再包成
//     user 消息（数字变 "42"、null 变 "None"、对象变 `[{type:text,...}]`）；
//   - `function_call` 的 id 取 `call_id or id or "call_amkr"`，arguments 非字符串时
//     先 `or {}` 再序列化（空串是字符串，**原样保留**）；
//   - `function_call_output` 走 ToolResultText（会把非文本部分序列化）；
//   - `reasoning` 直接丢弃；
//   - `developer` role 改写成 `system`；
//   - 既没有 type/role/content 的项**整个当作内容**塞给 user（`{}` 会变成文本 "{}"）。
func TestRequestResponsesInputToMessagesMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "裸字符串", input: `"hello"`, want: `[{"role":"user","content":"hello"}]`},
		{name: "非列表数字", input: `42`, want: `[{"role":"user","content":"42"}]`},
		{name: "非列表 null", input: `null`, want: `[{"role":"user","content":"None"}]`},
		{name: "非列表布尔", input: `true`, want: `[{"role":"user","content":"True"}]`},
		{name: "非列表对象", input: `{"a":1}`, want: `[{"role":"user","content":[{"type":"text","text":"{\"a\":1}"}]}]`},
		{name: "空列表", input: `[]`, want: `[]`},
		{name: "列表含裸字符串", input: `["a","b"]`, want: `[{"role":"user","content":"a"},{"role":"user","content":"b"}]`},
		{name: "列表含数字", input: `[1,2]`, want: `[{"role":"user","content":"1"},{"role":"user","content":"2"}]`},
		{name: "列表含 null", input: `[null]`, want: `[{"role":"user","content":"None"}]`},
		{
			name:  "function_call 字符串参数",
			input: `[{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"}]`,
			want:  `[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}]`,
		},
		{
			name:  "function_call 对象参数",
			input: `[{"type":"function_call","id":"c2","name":"f","arguments":{"b":2}}]`,
			want:  `[{"role":"assistant","content":null,"tool_calls":[{"id":"c2","type":"function","function":{"name":"f","arguments":"{\"b\":2}"}}]}]`,
		},
		{
			name:  "function_call 无 id 与参数",
			input: `[{"type":"function_call","name":"f"}]`,
			want:  `[{"role":"assistant","content":null,"tool_calls":[{"id":"call_amkr","type":"function","function":{"name":"f","arguments":"{}"}}]}]`,
		},
		{
			// 空字符串是**字符串**，isinstance(arguments, str) 命中，原样保留。
			name:  "function_call 空字符串参数不补 {}",
			input: `[{"type":"function_call","name":"f","arguments":""}]`,
			want:  `[{"role":"assistant","content":null,"tool_calls":[{"id":"call_amkr","type":"function","function":{"name":"f","arguments":""}}]}]`,
		},
		{
			name:  "function_call_output",
			input: `[{"type":"function_call_output","call_id":"c1","output":"done"}]`,
			want:  `[{"role":"tool","tool_call_id":"c1","content":"done"}]`,
		},
		{
			name:  "function_call_output 数组输出走 ToolResultText",
			input: `[{"type":"function_call_output","call_id":"c1","output":[{"type":"output_text","text":"a"},{"x":1}]}]`,
			want:  `[{"role":"tool","tool_call_id":"c1","content":"a{\"x\":1}"}]`,
		},
		{
			name:  "function_call_output 缺 call_id",
			input: `[{"type":"function_call_output","output":"d"}]`,
			want:  `[{"role":"tool","tool_call_id":"","content":"d"}]`,
		},
		{name: "reasoning 丢弃", input: `[{"type":"reasoning","summary":[]}]`, want: `[]`},
		{
			name:  "message 类型",
			input: `[{"type":"message","role":"user","content":"hi"}]`,
			want:  `[{"role":"user","content":"hi"}]`,
		},
		{
			name:  "developer 改写成 system",
			input: `[{"type":"message","role":"developer","content":"rules"}]`,
			want:  `[{"role":"system","content":"rules"}]`,
		},
		{
			name:  "role 无 type",
			input: `[{"role":"user","content":"hi"}]`,
			want:  `[{"role":"user","content":"hi"}]`,
		},
		{
			name:  "只有 content 无 role",
			input: `[{"content":"hi"}]`,
			want:  `[{"role":"user","content":"hi"}]`,
		},
		{
			// content 与 role 键都不在，`item.get("type") == "message" or "role" in item
			// or "content" in item` 全为假，于是走「整项当内容」的分支：整个项
			// `{"text":"fallback"}` 过 `_adapt_content`，恰好因为带 text 字段而变成
			// 一个 text 块。回退读到的 text 值在此路径里**不影响输出**。
			name:  "只有 text 无 content 走整项分支",
			input: `[{"text":"fallback"}]`,
			want:  `[{"role":"user","content":[{"type":"text","text":"fallback"}]}]`,
		},
		{
			// content 键存在但值是 null：**不**回退到 text（Python 的
			// item.get("content", item.get("text", ""))），于是 content=None 进
			// _adapt_content 变成字符串 "None"。
			name:  "content 为 null 时不回退 text",
			input: `[{"type":"message","role":"user","content":null,"text":"not used"}]`,
			want:  `[{"role":"user","content":"None"}]`,
		},
		{
			name:  "content 缺失时回退 text",
			input: `[{"type":"message","role":"user","text":"used"}]`,
			want:  `[{"role":"user","content":"used"}]`,
		},
		{
			name:  "既无 role 也无 content 时整项作内容",
			input: `[{"foo":"bar"}]`,
			want:  `[{"role":"user","content":[{"type":"text","text":"{\"foo\":\"bar\"}"}]}]`,
		},
		{
			name:  "空对象项",
			input: `[{}]`,
			want:  `[{"role":"user","content":[{"type":"text","text":"{}"}]}]`,
		},
		{
			name:  "role 非字符串回落 user",
			input: `[{"role":0,"content":"x"}]`,
			want:  `[{"role":"user","content":"x"}]`,
		},
		{
			name:  "message 类型配数组内容",
			input: `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}]}]`,
			want:  `[{"role":"assistant","content":[{"type":"text","text":"yo"}]}]`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			value, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			got := canonical.DumpsOrdered(canonical.NewArray(requestResponsesInputToMessages(value)...))
			if got != item.want {
				t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", item.input, item.want, got)
			}
		})
	}
}

// TestRequestAdaptResponsesInputPayloadMatchesPython 覆盖 Responses 请求体适配。
//
// 期望值来自 gen_request.py 的 `=== adapt_responses_input_payload ===` 与
// gen_request_order.py 的 `=== responses payload 键顺序 ===`。要点：
//   - `input` 无论真假都被消费，`instructions` 同样被 pop，只有真值才生成 system 消息；
//   - `messages` 通常追加到**末尾**（键顺序可见）；
//   - tools 复用 Anthropic 的工具适配器（Responses 的工具是顶层 name 形态）；
//   - tool_choice 只有 `{"type":"function"}` 才包一层，`"auto"` 字符串与其它对象不动。
func TestRequestAdaptResponsesInputPayloadMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "最小",
			input: `{"input":"hi"}`,
			want:  `{"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "instructions 生成 system 消息",
			input: `{"input":"hi","instructions":"be nice","max_output_tokens":5}`,
			want:  `{"max_output_tokens":5,"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "instructions 为空串时不插入",
			input: `{"input":"hi","instructions":""}`,
			want:  `{"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "input 是列表",
			input: `{"model":"m","input":[{"type":"message","role":"user","content":"a"}],"temperature":0.5}`,
			want:  `{"model":"m","temperature":0.5,"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			// input 在最前：pop 掉后其余键前移，messages 追加到末尾。
			name:  "input 在最前",
			input: `{"input":"a","model":"m"}`,
			want:  `{"model":"m","messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "instructions 在最前",
			input: `{"instructions":"s","input":"a","model":"m"}`,
			want:  `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"a"}]}`,
		},
		{
			name:  "input 非列表数字",
			input: `{"input":7}`,
			want:  `{"messages":[{"role":"user","content":"7"}]}`,
		},
		{
			name:  "input 为 null",
			input: `{"input":null}`,
			want:  `{"messages":[{"role":"user","content":"None"}]}`,
		},
		{
			name:  "tools 顶层 name 形态",
			input: `{"input":"a","tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}]}`,
			want:  `{"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tools 已是 function 形态原样保留",
			input: `{"input":"a","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
			want:  `{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tools 非 function 被过滤但仍写回空数组",
			input: `{"input":"a","tools":[{"type":"code_interpreter"}]}`,
			want:  `{"tools":[],"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tools 里非字典被丢弃",
			input: `{"input":"a","tools":[1,{"name":"f"}]}`,
			want:  `{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}],"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tools 在最前时键位置不变",
			input: `{"tools":[{"name":"f"}],"input":"a"}`,
			want:  `{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}],"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tool_choice function 包一层",
			input: `{"input":"a","tool_choice":{"type":"function","name":"f"}}`,
			want:  `{"tool_choice":{"type":"function","function":{"name":"f"}},"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tool_choice 字符串不动",
			input: `{"input":"a","tool_choice":"auto"}`,
			want:  `{"tool_choice":"auto","messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tool_choice 其它对象不动",
			input: `{"input":"a","tool_choice":{"type":"required"}}`,
			want:  `{"tool_choice":{"type":"required"},"messages":[{"role":"user","content":"a"}]}`,
		},
		{
			name:  "tool_choice type 非字符串不动",
			input: `{"input":"a","tool_choice":{"type":5,"name":"f"}}`,
			want:  `{"tool_choice":{"type":5,"name":"f"},"messages":[{"role":"user","content":"a"}]}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			requireDumps(t, item.input, item.want, requestAdaptResponsesInputPayload)
		})
	}
}

// TestRequestAdaptAnthropicMessagesPayloadMatchesPython 覆盖 Anthropic 请求体适配。
//
// 期望值来自 gen_request.py 的 `=== adapt_anthropic_messages_payload ===` 与
// gen_request_order.py 的 `=== anthropic payload 键顺序 ===`。要点：
//   - `messages` 不是列表时**原样返回**（连 system / tools 都不动）；
//   - `system` 无论真值与否都被 pop；只有真值才在**最前**插入 system 消息；
//   - 非字典消息项被静默丢弃；
//   - `disable_parallel_tool_use` 用**键存在性**判断，`false` 也要显式翻出
//     `parallel_tool_calls: true`。
func TestRequestAdaptAnthropicMessagesPayloadMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "messages 非列表原样返回",
			input: `{"messages":"x","system":"s","max_tokens":5}`,
			want:  `{"messages":"x","system":"s","max_tokens":5}`,
		},
		{
			name:  "messages 为 null 原样返回",
			input: `{"messages":null,"system":"s"}`,
			want:  `{"messages":null,"system":"s"}`,
		},
		{
			name:  "messages 为对象原样返回",
			input: `{"messages":{},"system":"s"}`,
			want:  `{"messages":{},"system":"s"}`,
		},
		{
			name:  "system 字符串",
			input: `{"messages":[{"role":"user","content":"hi"}],"system":"be nice","max_tokens":5}`,
			want:  `{"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}],"max_tokens":5}`,
		},
		{
			name:  "system 数组",
			input: `{"messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"s1"}],"max_tokens":5}`,
			want:  `{"messages":[{"role":"system","content":[{"type":"text","text":"s1"}]},{"role":"user","content":"hi"}],"max_tokens":5}`,
		},
		{
			name:  "system 为空串时被剔除",
			input: `{"messages":[{"role":"user","content":"hi"}],"system":""}`,
			want:  `{"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "system 在最前也移到 messages 位置",
			input: `{"system":"s","messages":[{"role":"user","content":"a"}],"model":"m"}`,
			want:  `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"a"}],"model":"m"}`,
		},
		{
			name:  "messages 里非字典项被丢弃",
			input: `{"messages":[1,"x",{"role":"user","content":"hi"}]}`,
			want:  `{"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "其它参数保留原位",
			input: `{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.9}`,
			want:  `{"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.9}`,
		},
		{
			name:  "disable_parallel_tool_use 为 true",
			input: `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`,
			want:  `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"auto","parallel_tool_calls":false}`,
		},
		{
			// 键存在性判据：false 也要显式翻出 true。
			name:  "disable_parallel_tool_use 为 false 仍写出 parallel_tool_calls",
			input: `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":false}}`,
			want:  `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"auto","parallel_tool_calls":true}`,
		},
		{
			name:  "disable_parallel_tool_use 缺省时不写出",
			input: `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto"}}`,
			want:  `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`,
		},
		{
			name:  "tool_choice 非字典不动",
			input: `{"messages":[],"tool_choice":"auto"}`,
			want:  `{"messages":[],"tool_choice":"auto"}`,
		},
		{
			name:  "tool_choice 非字典且在最前",
			input: `{"tool_choice":{"type":"any"},"messages":[]}`,
			want:  `{"tool_choice":"required","messages":[]}`,
		},
		{
			name:  "tools 过滤并改写",
			input: `{"messages":[],"tools":[{"type":"code_interpreter"},{"name":"f","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}]}`,
			want:  `{"messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			requireDumps(t, item.input, item.want, requestAdaptAnthropicMessagesPayload)
		})
	}
}

// TestRequestAdaptAnthropicMessageMatchesPython 覆盖 Anthropic 单条消息的拆分。
//
// 期望值来自 gen_request.py 的 `=== adapt_anthropic_message ===`。要点：
//   - content 非数组时整体套 `{**message, "role":…, "content":…}`，保留其余字段
//     （`content: null` → `_adapt_content(None)` → 字符串 "None"）；
//   - assistant：tool_use 块抽成 tool_calls，其余块进 content；没有文本块时 content
//     显式为 **null**（不是省略）；非 tool_use 的块连 image 一起保留；
//   - user 且含 tool_result：tool_result 提升为独立 role=tool 消息，其余块按原有
//     分组变回 user 消息，相对顺序不变；
//   - tool_use 的 id 缺失回落到 `toolu_amkr_{枚举下标}`，input 缺失/null 补 {}；
//   - content 数组里 role 缺失时 `{**message, "role":"user"}` 会把 role 追加到**末尾**。
func TestRequestAdaptAnthropicMessageMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "content 非列表",
			input: `{"role":"user","content":"hi"}`,
			want:  `[{"role":"user","content":"hi"}]`,
		},
		{
			name:  "content 缺失",
			input: `{"role":"user"}`,
			want:  `[{"role":"user","content":""}]`,
		},
		{
			name:  "content 为 null",
			input: `{"role":"user","content":null}`,
			want:  `[{"role":"user","content":"None"}]`,
		},
		{
			name:  "content 是字典",
			input: `{"role":"user","content":{"type":"text","text":"a"}}`,
			want:  `[{"role":"user","content":[{"type":"text","text":"a"}]}]`,
		},
		{
			name:  "assistant 纯文本数组",
			input: `{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`,
			want:  `[{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]`,
		},
		{
			name:  "assistant 文本加工具",
			input: `{"role":"assistant","content":[{"type":"text","text":"thinking"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]}`,
			want:  `[{"role":"assistant","content":[{"type":"text","text":"thinking"}],"tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}]`,
		},
		{
			name:  "assistant 纯工具时 content 为 null",
			input: `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]}`,
			want:  `[{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}]`,
		},
		{
			name:  "assistant 工具缺 id 与 input",
			input: `{"role":"assistant","content":[{"type":"tool_use","name":"f","input":null}]}`,
			want:  `[{"role":"assistant","content":null,"tool_calls":[{"id":"toolu_amkr_0","type":"function","function":{"name":"f","arguments":"{}"}}]}]`,
		},
		{
			name:  "assistant 工具 input 为空对象",
			input: `{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]}`,
			want:  `[{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]}]`,
		},
		{
			// `{key: value for key, value in message.items() if key != "content"}` 保留
			// 额外字段，content 被重新追加到**末尾**。
			name:  "assistant 保留额外字段",
			input: `{"role":"assistant","content":[{"type":"text","text":"a"}],"custom":"x"}`,
			want:  `[{"role":"assistant","custom":"x","content":[{"type":"text","text":"a"}]}]`,
		},
		{
			name:  "user 单个 tool_result",
			input: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"res"}]}`,
			want:  `[{"role":"tool","tool_call_id":"t1","content":"res"}]`,
		},
		{
			name:  "user tool_result 夹在文本之间",
			input: `{"role":"user","content":[{"type":"text","text":"before"},{"type":"tool_result","tool_use_id":"t1","content":"res"},{"type":"text","text":"after"}]}`,
			want:  `[{"role":"user","content":[{"type":"text","text":"before"}]},{"role":"tool","tool_call_id":"t1","content":"res"},{"role":"user","content":[{"type":"text","text":"after"}]}]`,
		},
		{
			name:  "user 两个 tool_result",
			input: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"},{"type":"tool_result","tool_use_id":"t2","content":"b"}]}`,
			want:  `[{"role":"tool","tool_call_id":"t1","content":"a"},{"role":"tool","tool_call_id":"t2","content":"b"}]`,
		},
		{
			// tool_result 的 content 走 ToolResultText：没有 text 字段的块被序列化，
			// 因此 image 块变成 JSON 文本片段（与 MessageText 的行为不同）。
			name:  "user tool_result 数组内容序列化非文本块",
			input: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QQ=="}}]}]}`,
			want:  `[{"role":"tool","tool_call_id":"t1","content":"a{\"type\":\"image\",\"source\":{\"type\":\"base64\",\"media_type\":\"image/jpeg\",\"data\":\"QQ==\"}}"}]`,
		},
		{
			name:  "user 普通数组",
			input: `{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"u"}}]}`,
			want:  `[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"u"}}]}]`,
		},
		{
			name:  "user 非字典块被 str()",
			input: `{"role":"user","content":[1,"x"]}`,
			want:  `[{"role":"user","content":[{"type":"text","text":"1"},{"type":"text","text":"x"}]}]`,
		},
		{
			name:  "user 空数组",
			input: `{"role":"user","content":[]}`,
			want:  `[{"role":"user","content":[]}]`,
		},
		{
			name:  "其它 role",
			input: `{"role":"system","content":[{"type":"text","text":"s"}]}`,
			want:  `[{"role":"system","content":[{"type":"text","text":"s"}]}]`,
		},
		{
			// role 缺失：`dict(message)` 后 setdefault 式的赋值把 role 追加到末尾。
			name:  "role 缺失时追加到末尾",
			input: `{"content":[{"type":"text","text":"s"}]}`,
			want:  `[{"content":[{"type":"text","text":"s"}],"role":"user"}]`,
		},
		{
			name:  "tool_result 的 tool_use_id 是数字",
			input: `{"role":"user","content":[{"type":"tool_result","tool_use_id":7,"content":"c"}]}`,
			want:  `[{"role":"tool","tool_call_id":"7","content":"c"}]`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			message, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			got := canonical.DumpsOrdered(canonical.NewArray(requestAdaptAnthropicMessage(message)...))
			if got != item.want {
				t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", item.input, item.want, got)
			}
		})
	}
}

// TestRequestHasToolResultPartMatchesPython 覆盖 `any(...)` 判定。
//
// 期望值来自 gen_request_helpers.py 的 `=== has_tool_result_part ===`：非字典元素
// 与 type 非字符串的项都不算命中。
func TestRequestHasToolResultPartMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "空数组", input: `[]`, want: false},
		{name: "无 tool_result", input: `[{"type":"text","text":"a"},"b",3]`, want: false},
		{name: "有 tool_result", input: `[{"type":"text"},{"type":"tool_result","content":"c"}]`, want: true},
		{name: "首项即 tool_result", input: `[{"type":"tool_result"}]`, want: true},
		{name: "非字典元素", input: `[1,"tool_result",null]`, want: false},
		{name: "type 是数字", input: `[{"type":5}]`, want: false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			content, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := requestHasToolResultPart(content); got != item.want {
				t.Errorf("期望 %v，实际 %v", item.want, got)
			}
		})
	}
}

// TestRequestAdaptAnthropicToolMatchesPython 锁定单条工具定义的适配。
//
// 期望值来自 gen_request.py 的 `=== adapt_anthropic_tool ===`。三处判据的分寸：
//   - `type is not None and type != "function"`：只有**字面 null** 当作「没写 type」，
//     `"type": ""` 与数字都被过滤掉（返回 None）；
//   - 已是 OpenAI 形态且 function.name 非空时**原样返回同一个对象**（不做拷贝）；
//   - function 存在但缺 name 时补上顶层 name，补进去的键追加在 func 末尾；
//   - `tool.get("input_schema", tool.get("parameters"))` 只按 input_schema 键是否
//     存在回退，`input_schema: null` **不**回退（退化到默认 schema）。
func TestRequestAdaptAnthropicToolMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Anthropic 形态",
			input: `{"name":"f","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}`,
			want:  `{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}`,
		},
		{
			name:  "parameters 回退",
			input: `{"name":"f","parameters":{"type":"object"}}`,
			want:  `{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}`,
		},
		{
			name:  "input_schema 为 null 不回退",
			input: `{"name":"f","input_schema":null,"parameters":{"type":"object"}}`,
			want:  `{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}`,
		},
		{
			name:  "input_schema 非字典用默认 schema",
			input: `{"name":"f","input_schema":"x"}`,
			want:  `{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}`,
		},
		{
			// `is not None`：空串 description 照样写出空串。
			name:  "description 为空串照样写出",
			input: `{"name":"f","description":""}`,
			want:  `{"type":"function","function":{"name":"f","description":"","parameters":{"type":"object","properties":{}}}}`,
		},
		{
			name:  "description 缺失不写出",
			input: `{"name":"f"}`,
			want:  `{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}`,
		},
		{
			name:  "已是 function 形态原样返回",
			input: `{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}`,
			want:  `{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}`,
		},
		{
			name:  "function 缺 name 时补顶层 name",
			input: `{"type":"function","function":{"parameters":{"type":"object"}},"name":"top"}`,
			want:  `{"type":"function","function":{"parameters":{"type":"object"},"name":"top"}}`,
		},
		{
			name:  "type function 但 function 非字典走 Anthropic 路径",
			input: `{"type":"function","name":"f","input_schema":{"type":"object"}}`,
			want:  `{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}`,
		},
		{
			name:  "type 为空串被过滤",
			input: `{"type":"","name":"f"}`,
			want:  "None",
		},
		{
			name:  "type 为 null 当作没写",
			input: `{"type":null,"name":"f"}`,
			want:  `{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}`,
		},
		{
			name:  "type code_interpreter 被过滤",
			input: `{"type":"code_interpreter"}`,
			want:  "None",
		},
		{
			name:  "type 数字被过滤",
			input: `{"type":5,"name":"f"}`,
			want:  "None",
		},
		{
			name:  "name 缺失得到空串",
			input: `{"input_schema":{"type":"object"}}`,
			want:  `{"type":"function","function":{"name":"","parameters":{"type":"object"}}}`,
		},
		{
			name:  "name 数字被 str()",
			input: `{"name":5,"input_schema":{"type":"object"}}`,
			want:  `{"type":"function","function":{"name":"5","parameters":{"type":"object"}}}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			tool, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			result, ok := requestAdaptAnthropicTool(tool)
			if item.want == "None" {
				if ok {
					t.Fatalf("应被过滤（返回 None），实际 %s", canonical.DumpsOrdered(result))
				}
				return
			}
			if !ok {
				t.Fatal("不应被过滤")
			}
			if got := canonical.DumpsOrdered(result); got != item.want {
				t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", item.input, item.want, got)
			}
		})
	}
}

// TestRequestAdaptAnthropicToolPassthrough 断言已是 OpenAI 形态的工具**原样返回同一
// 个对象**（参照实现是 `return tool`，不做拷贝）。
func TestRequestAdaptAnthropicToolPassthrough(t *testing.T) {
	tool, err := canonical.ParseString(`{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	result, ok := requestAdaptAnthropicTool(tool)
	if !ok {
		t.Fatal("不应被过滤")
	}
	if result != tool {
		t.Fatal("应返回同一个对象（参照实现直接 return tool）")
	}
}

// TestRequestAdaptToolListMatchesPython 覆盖工具列表的过滤与写回。
//
// 期望值来自 gen_request_helpers.py 的 `=== adapt_tool_list ===`：非字典项与
// 返回 None 的项都被丢弃，结果即便为空也写回 `[]`（不是删键）。
func TestRequestAdaptToolListMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "空数组", input: `[]`, want: `[]`},
		{
			name:  "全是字典",
			input: `[{"name":"a"},{"name":"b","input_schema":{"type":"object"}}]`,
			want:  `[{"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"b","parameters":{"type":"object"}}}]`,
		},
		{
			name:  "含非字典",
			input: `[1,"x",{"name":"a"}]`,
			want:  `[{"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{}}}}]`,
		},
		{
			name:  "含被过滤的类型",
			input: `[{"type":"code_interpreter"},{"name":"a"}]`,
			want:  `[{"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{}}}}]`,
		},
		{name: "全部被过滤", input: `[{"type":"code_interpreter"}]`, want: `[]`},
		{
			name:  "已是 function 形态原样返回",
			input: `[{"type":"function","function":{"name":"a"}}]`,
			want:  `[{"type":"function","function":{"name":"a"}}]`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			tools, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := canonical.DumpsOrdered(requestAdaptToolList(tools)); got != item.want {
				t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", item.input, item.want, got)
			}
		})
	}
}

// TestRequestAdaptAnthropicToolChoiceMatchesPython 覆盖 tool_choice 的翻译。
//
// 期望值来自 gen_request.py 的 `=== adapt_anthropic_tool_choice ===`：
// `any` → 字符串 `"required"`，`auto` / `none` → 同名字符串，`tool` →
// `{"type":"function","function":{"name":…}}`，其余（含缺 type / 数字 type）原样返回。
func TestRequestAdaptAnthropicToolChoiceMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "any", input: `{"type":"any"}`, want: `"required"`},
		{name: "auto", input: `{"type":"auto"}`, want: `"auto"`},
		{name: "none", input: `{"type":"none"}`, want: `"none"`},
		{name: "tool", input: `{"type":"tool","name":"f"}`, want: `{"type":"function","function":{"name":"f"}}`},
		{name: "tool 缺 name", input: `{"type":"tool"}`, want: `{"type":"function","function":{"name":""}}`},
		{name: "tool 的 name 是数字", input: `{"type":"tool","name":5}`, want: `{"type":"function","function":{"name":"5"}}`},
		{name: "未知 type", input: `{"type":"weird"}`, want: `{"type":"weird"}`},
		{name: "type 缺失", input: `{}`, want: `{}`},
		{name: "type 是数字", input: `{"type":3}`, want: `{"type":3}`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			choice, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := canonical.DumpsOrdered(requestAdaptAnthropicToolChoice(choice)); got != item.want {
				t.Errorf("输出不一致\n输入: %s\n期望: %s\n实际: %s", item.input, item.want, got)
			}
		})
	}
}

// TestRequestAdaptContentMatchesPython 覆盖 content 归一。
//
// 期望值来自 gen_request.py 的 `=== adapt_content ===`：字符串原样返回；数组逐项
// 适配；字典包成单元素数组；其余（数字、布尔、null）一律 `str()`——注意
// `str(None)` 是 **"None"** 而不是空串，这与 ToolResultText / MessageText 对 None
// 的处理不同。
func TestRequestAdaptContentMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "字符串", input: `"hi"`, want: `"hi"`},
		{name: "空串", input: `""`, want: `""`},
		{name: "数组", input: `[{"type":"text","text":"a"},"b"]`, want: `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`},
		{name: "字典", input: `{"type":"text","text":"a"}`, want: `[{"type":"text","text":"a"}]`},
		{name: "整数", input: `5`, want: `"5"`},
		{name: "浮点", input: `3.5`, want: `"3.5"`},
		{name: "布尔", input: `true`, want: `"True"`},
		{name: "null", input: `null`, want: `"None"`},
		{name: "空数组", input: `[]`, want: `[]`},
		{name: "空字典", input: `{}`, want: `[{"type":"text","text":"{}"}]`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			requireDumps(t, item.input, item.want, requestAdaptContent)
		})
	}
}

// TestRequestAdaptContentPartMatchesPython 覆盖单个内容块的适配。
//
// 期望值来自 gen_request.py 的 `=== adapt_content_part ===`。分支顺序即优先级：
//   - `text` / `image_url` 原样返回（连多余字段都保留）；
//   - `input_text` / `output_text` 取 `str(part.get("text",""))`；
//   - `image` 且 source 是 base64 → `data:{media_type};base64,{data}`，media_type
//     缺失时默认 **image/png**；source 非 base64 时**不返回**，继续往下走；
//   - `input_image` → `image_url or file_id or ""` 再 str()；
//   - 都不匹配时先看 text 字段（`is not None`，**空串也算命中**），仍然没有才把
//     整个块 JSON 序列化成 text——这条兜底保证未知块不会被静默丢掉；
//   - 非字典元素一律 `{"type":"text","text":str(part)}`（数组用 Python repr 形式）。
func TestRequestAdaptContentPartMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "text 原样保留多余字段",
			input: `{"type":"text","text":"a","extra":1}`,
			want:  `{"type":"text","text":"a","extra":1}`,
		},
		{
			name:  "image_url 原样保留多余字段",
			input: `{"type":"image_url","image_url":{"url":"u","detail":"high"}}`,
			want:  `{"type":"image_url","image_url":{"url":"u","detail":"high"}}`,
		},
		{name: "input_text", input: `{"type":"input_text","text":"a"}`, want: `{"type":"text","text":"a"}`},
		{name: "output_text 数字 text", input: `{"type":"output_text","text":5}`, want: `{"type":"text","text":"5"}`},
		{
			name:  "image base64",
			input: `{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QQ=="}}`,
			want:  `{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QQ=="}}`,
		},
		{
			// media_type 缺失时的默认值是 image/png（common.py:53）。
			name:  "image base64 无 media_type 默认 png",
			input: `{"type":"image","source":{"type":"base64","data":"QQ=="}}`,
			want:  `{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}`,
		},
		{
			name:  "image base64 无 data 保留前缀",
			input: `{"type":"image","source":{"type":"base64","media_type":"image/png"}}`,
			want:  `{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}`,
		},
		{
			name:  "image 非 base64 落到 JSON 兜底",
			input: `{"type":"image","source":{"type":"url","url":"u"}}`,
			want:  `{"type":"text","text":"{\"type\":\"image\",\"source\":{\"type\":\"url\",\"url\":\"u\"}}"}`,
		},
		{
			name:  "image source 非字典时用 text 字段",
			input: `{"type":"image","source":"x","text":"fallback"}`,
			want:  `{"type":"text","text":"fallback"}`,
		},
		{
			name:  "input_image image_url 优先",
			input: `{"type":"input_image","image_url":"http://x","file_id":"f"}`,
			want:  `{"type":"image_url","image_url":{"url":"http://x"}}`,
		},
		{
			name:  "input_image file_id 回退",
			input: `{"type":"input_image","file_id":"f"}`,
			want:  `{"type":"image_url","image_url":{"url":"f"}}`,
		},
		{
			name:  "input_image 两者都无",
			input: `{"type":"input_image"}`,
			want:  `{"type":"image_url","image_url":{"url":""}}`,
		},
		{
			// `image_url or file_id or ""` 再 str()：数字 5 是真值。
			name:  "input_image image_url 是数字",
			input: `{"type":"input_image","image_url":5}`,
			want:  `{"type":"image_url","image_url":{"url":"5"}}`,
		},
		{name: "未知类型带 text", input: `{"type":"weird","text":"t"}`, want: `{"type":"text","text":"t"}`},
		{
			name:  "未知类型 text 为 null 走 JSON 兜底",
			input: `{"type":"weird","text":null}`,
			want:  `{"type":"text","text":"{\"type\":\"weird\",\"text\":null}"}`,
		},
		{
			// 键顺序即输入顺序，兜底用的是紧凑序列化（不是 canonical 排序）。
			name:  "未知类型无 text",
			input: `{"type":"weird","b":2,"a":1}`,
			want:  `{"type":"text","text":"{\"type\":\"weird\",\"b\":2,\"a\":1}"}`,
		},
		{name: "无 type 有 text", input: `{"text":"t"}`, want: `{"type":"text","text":"t"}`},
		{name: "非字典整数", input: `7`, want: `{"type":"text","text":"7"}`},
		{name: "非字典字符串", input: `"s"`, want: `{"type":"text","text":"s"}`},
		{name: "非字典 null", input: `null`, want: `{"type":"text","text":"None"}`},
		{name: "非字典数组用 repr", input: `[1,2]`, want: `{"type":"text","text":"[1, 2]"}`},
		{name: "空对象走 JSON 兜底", input: `{}`, want: `{"type":"text","text":"{}"}`},
		{
			// `is not None`：空串也是命中，因此不落到 JSON 兜底。
			name:  "text 为空串照样命中",
			input: `{"type":"unknown","text":""}`,
			want:  `{"type":"text","text":""}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
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
