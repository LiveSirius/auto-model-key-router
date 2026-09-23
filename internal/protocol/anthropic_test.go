package protocol

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 本文件的**全部期望值**都由真实 Python 实现打印得到，不是自己推导的。生成方式：
//
//	cd D:\Code\auto-model-key-router
//	$env:PYTHONIOENCODING='utf-8'
//	python -X utf8 _gen_anthropic.py     # 流式 + usage + 非流式
//	python -X utf8 _gen_anthropic2.py    # stop_reason 边界、乱码实证、码点切片
//	python -X utf8 _gen_anthropic3.py    # 残留半行、CRLF
//	python -X utf8 _gen_anthropic4.py    # 直接渲染成 Go 片段
//
// 脚本调用 auto_model_key_router.protocols.anthropic 的转换函数，用
// json.dumps(..., ensure_ascii=False, separators=(",", ":")) 打印结果，再把文本
// 粘进下面的表里。这是从参照实现取期望值的可信方式（Python 源码已不在仓库里，
// 表里的文本就是留存的历史依据）。

// ev 拼出一个完整的 SSE 事件文本。
//
// 事件格式由 common.go 的 EncodeSSE 决定：`event: X\ndata: {json}\n\n`。测试断言
// 的是**逐字节**结果，所以不能只比对 data 里的 JSON。
func ev(event, payload string) string {
	return "event: " + event + "\ndata: " + payload + "\n\n"
}

// sseLine 把一个 data 行拼成上游流里的一行（含结尾空行）。
func sseLine(payload string) string {
	return "data: " + payload + "\n\n"
}

// collectStream 把若干块喂进流式转换器，返回全部事件文本。
//
// withFinish 为 true 时追加收尾路径：先处理残留半行（AnthropicStreamState.Finish，
// 对应 proxy_handler.py:1247），再收尾工具块（FinishTools，对应 anthropic.py:191）。
func collectStream(t *testing.T, chunks []string, withFinish bool) []string {
	t.Helper()
	state := NewAnthropicStreamState()
	var got []string
	for _, chunk := range chunks {
		events, _ := state.Push([]byte(chunk))
		for _, event := range events {
			got = append(got, string(event))
		}
	}
	if withFinish {
		events, _ := state.Finish()
		for _, event := range events {
			got = append(got, string(event))
		}
		for _, event := range state.FinishTools() {
			got = append(got, string(event))
		}
	}
	return got
}

// requireEvents 逐个比对事件文本。
func requireEvents(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("事件数量不符：期望 %d 个，实际 %d 个\n实际事件：\n%s",
			len(want), len(got), strings.Join(got, ""))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("第 %d 个事件不一致\n期望: %q\n实际: %q", index, want[index], got[index])
		}
	}
}

// TestAnthropicStreamTextEvents 锁定纯文本流的逐字节输出。
//
// 期望值来自 _gen_anthropic4.py 的 `--- text ---`。
//
// 首次出现文本时才发 content_block_start，后续文本复用同一个 block index——这是
// Anthropic 客户端的硬要求，多发一个 start 会被判为协议错误。
func TestAnthropicStreamTextEvents(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"content":"He"}}]}`),
		sseLine(`{"choices":[{"delta":{"content":"llo"},"finish_reason":"length"}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"He"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"llo"}}`),
	})
}

// TestAnthropicStreamMultibyteWholeLine 断言完整一行的多字节文本原样输出。
//
// 期望值来自 _gen_anthropic4.py 的 `--- multibyte ---`。这是分片场景的对照基准：
// 只要行是完整的，就不该出现任何 U+FFFD。
func TestAnthropicStreamMultibyteWholeLine(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"content":"中文内容🚀"}}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"中文内容🚀"}}`),
	})
}

// replaceDecode 复刻 Python 的 `bytes.decode("utf-8", errors="replace")`。
//
// 存在的唯一目的是**证明**下面那个分片测试不是空转：Go 的 `string([]byte)` 会把
// 非法字节原样留在字符串里（range 时才变成 U+FFFD），行为与 Python 不同，所以必须
// 手工模拟 Python 的逐块解码，才能展示参照实现的缺陷。
func replaceDecode(data []byte) string {
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(utf8.RuneError)
			data = data[1:]
			continue
		}
		b.Write(data[:size])
		data = data[size:]
	}
	return b.String()
}

// TestAnthropicStreamSplitMultibyteIsRepaired 是本次移植最重要的一个测试。
//
// 参照实现把每块 TCP 数据独立 `decode("utf-8", errors="replace")` 后再拼成 buffer。
// 当一个多字节字符横跨两块时，两个分片各自解码都会失败并产出 U+FFFD，于是
// "中文内容" 会静默变成 "\uFFFD\uFFFD\uFFFD文内容"（_gen_anthropic2.py 的
// python_mojibake_demo 已实证：PY_SPLIT_HAS_REPLACEMENT True）。
//
// Go 侧改用 SSESplitter，在**字节层面**找换行，拿到完整一行才解码，因此切分点的
// 多字节序列能正确接上。本测试同时做两件事：
//
//  1. 证明朴素做法（每块各自解码）确实会产出 U+FFFD —— 避免这个测试变成永远通过
//     的废测试；
//  2. 断言切分器产出的事件与「整行一次喂入」**逐字节相同**。
func TestAnthropicStreamSplitMultibyteIsRepaired(t *testing.T) {
	line := sseLine(`{"choices":[{"delta":{"content":"中文内容"}}]}`)

	// 切在「中」的三个 UTF-8 字节中间，与 _gen_anthropic2.py 的切点一致。
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
	whole := collectStream(t, []string{line}, true)
	split := collectStream(t, []string{first, second}, true)
	requireEvents(t, split, whole)

	joined := strings.Join(split, "")
	if strings.Contains(joined, "\uFFFD") {
		t.Fatalf("分片输入产出了 U+FFFD：%q", joined)
	}
	if !strings.Contains(joined, `"text":"中文内容"`) {
		t.Fatalf("分片输入未还原出完整中文：%q", joined)
	}
}

// TestAnthropicStreamSplitMultibyteToolArguments 断言工具参数里的多字节字符同样
// 能跨越 TCP 分片。
//
// 参数只有在**整行**解析出来后才知道，因此切在「北」的字节中间时，第一块不应产出
// 任何事件，第二块才补上完整的 input_json_delta。
func TestAnthropicStreamSplitMultibyteToolArguments(t *testing.T) {
	line := sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"北京"}}]}}]}`)
	cut := strings.Index(line, "北") + 1
	if cut <= 0 || cut >= len(line) {
		t.Fatalf("切点计算异常: %d（行长度 %d）", cut, len(line))
	}

	state := NewAnthropicStreamState()
	early, _ := state.Push([]byte(line[:cut]))
	if len(early) != 0 {
		t.Fatalf("半个多字节字符不该产出事件，实际 %d 个", len(early))
	}
	late, _ := state.Push([]byte(line[cut:]))

	var got []string
	for _, event := range append(early, late...) {
		got = append(got, string(event))
	}
	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"n","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"北京"}}`),
	})
}

// TestAnthropicStreamToolEvents 锁定工具调用分片的输出。
//
// 期望值来自 _gen_anthropic4.py 的 `--- tool_stream ---`。要点：
//   - id 与 name 都到手后才发 content_block_start（否则下游会看到没名字的工具）；
//   - 参数增量按**码点**切片下发，海量拼接由客户端负责；
//   - finish_reason=tool_calls 映射成 stop_reason=tool_use。
func TestAnthropicStreamToolEvents(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`),
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"北京\"}"}}]},"finish_reason":"tool_calls"}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"get_weather","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ty\":\"北京\"}"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	})
}

// TestAnthropicStreamLegacyFunctionCall 覆盖旧式 function_call 形态。
//
// 期望值来自 _gen_anthropic4.py 的 `--- function_call ---`。上游既没给 id 也没给
// index，所以**整个事件序列都推迟到收尾时**才产出，id 回落成 call_amkr_0。
func TestAnthropicStreamLegacyFunctionCall(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"function_call":{"name":"lookup","arguments":"{\"q\":1}"}}}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_amkr_0","name":"lookup","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":1}"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	})
}

// TestAnthropicStreamInterleavedTools 覆盖多个工具交错到达、且其中一个**缺 index**。
//
// 期望值来自 _gen_anthropic4.py 的 `--- interleaved ---`。这里有两个容易看错的点：
//
//   - 缺 `index` 的工具回落到**枚举下标**（第三块的第二个元素枚举下标是 1），于是
//     它被并进已存在的 tool index 1，而不是新建一个；
//   - 收尾顺序是「当前活跃的最先，其余按 index 升序」，所以 index 0 的 stop 事件
//     反而落在 index 1 的 stop 之后。
func TestAnthropicStreamInterleavedTools(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"a","arguments":"{"}}]}}]}`),
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"id":"c0","function":{"name":"b","arguments":"["}}]}}]}`),
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}},{"function":{"arguments":"]"}}]}}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"a","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"}"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"]"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c0","name":"b","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"["}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
	})
}

// TestAnthropicStreamArgumentIsJSONValue 覆盖上游直接给 JSON 值（而非字符串）的参数。
//
// 期望值来自 _gen_anthropic4.py 的 `--- json_args ---`。参照实现会把它**紧凑序列化**
// 后当作参数文本累积，因此 input_json_delta 里看到的是重排后的紧凑 JSON，而不是
// 上游原始写法。
func TestAnthropicStreamArgumentIsJSONValue(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":{"a":[1,2],"b":"中"}}}]}}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"n","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":[1,2],\"b\":\"中\"}"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	})
}

// TestAnthropicStreamTextThenToolInSameDelta 覆盖同一 delta 里既有文本又有工具调用。
//
// 期望值来自 _gen_anthropic4.py 的 `--- text_and_tool ---`。文本块必须先被 stop，
// 工具块才能占用下一个 content index——顺序错了客户端会拒绝。
func TestAnthropicStreamTextThenToolInSameDelta(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"content":"先说话","tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"先说话"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c","name":"n","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
	})
}

// TestAnthropicStreamReopensTextBlockAfterTool 覆盖「文本 → 工具 → 文本」。
//
// 期望值来自 _gen_anthropic4.py 的 `--- text_after_tool ---`：工具块关掉文本块之后，
// 新文本必须**另开**一个块（index 2），不能复用 index 0。
func TestAnthropicStreamReopensTextBlockAfterTool(t *testing.T) {
	got := collectStream(t, []string{
		sseLine(`{"choices":[{"delta":{"content":"hi"}}]}`),
		sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}`),
		sseLine(`{"choices":[{"delta":{"content":" bye"}}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"f","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":" bye"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
	})
}

// TestAnthropicStreamSkipsMalformedLines 断言坏载荷被**静默跳过**。
//
// 期望值来自 _gen_anthropic.py 的 bad_lines：坏 JSON、非对象 JSON、空 data、
// 注释行、其它事件名都不产出事件也不报错——上游流里出现一行垃圾不该中断整条响应。
func TestAnthropicStreamSkipsMalformedLines(t *testing.T) {
	got := collectStream(t, []string{
		"data: {oops\n\n",
		"event: ping\n\n",
		"data: [1,2]\n\n",
		"data:\n\n",
		": comment\n\n",
		sseLine(`{"choices":[{"delta":{"content":"ok"}}]}`),
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
	})
}

// TestAnthropicStreamCRLFLineEnding 断言 \r\n 行尾同样被接受。
//
// 期望值来自 _gen_anthropic3.py 的 CRLF-EVENTS：参照实现靠 line.strip() 吃掉 \r，
// 切分器在字节层面去掉两端空白，效果一致。
func TestAnthropicStreamCRLFLineEnding(t *testing.T) {
	got := collectStream(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"crlf\"}}]}\r\n",
	}, true)

	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"crlf"}}`),
	})
}

// TestAnthropicStreamTrailingPartialLine 覆盖流结束时没有换行结尾的半行。
//
// 期望值来自 _gen_anthropic3.py：半行在普通 Push 时**不产出任何事件**（缓冲区留
// 着），收尾补一个换行后才产出。参照实现把这一步放在 proxy_handler.py:1247，本模块
// 用 Finish 提供等价入口。
func TestAnthropicStreamTrailingPartialLine(t *testing.T) {
	state := NewAnthropicStreamState()
	early, _ := state.Push([]byte(`data: {"choices":[{"delta":{"content":"tail"}}]}`))
	if len(early) != 0 {
		t.Fatalf("没有换行结尾时不该产出事件，实际 %d 个", len(early))
	}
	if state.PendingBytes() == 0 {
		t.Fatal("半行应留在缓冲区里")
	}

	events, _ := state.Finish()
	var got []string
	for _, event := range events {
		got = append(got, string(event))
	}
	requireEvents(t, got, []string{
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tail"}}`),
	})
	// 再次 Finish 不应重复产出（缓冲已清空）。
	again, _ := state.Finish()
	if len(again) != 0 {
		t.Fatalf("重复 Finish 应无事件，实际 %d 个", len(again))
	}
}

// TestAnthropicStreamUsageExtraction 覆盖流式 usage 的提取与归一。
//
// 期望值来自 _gen_anthropic.py 的 usage_shapes 与 _gen_anthropic2.py 的
// stream_usage_shapes。注意两件事：
//   - 同一块里多次出现 usage 时**后者覆盖前者**（Push 的返回值即最终值）；
//   - "prompt_tokens": 0 会退到 input_tokens，因为参照实现用的是 `or`。
func TestAnthropicStreamUsageExtraction(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  string
	}{
		{name: "两字段都有", usage: `{"prompt_tokens":11,"completion_tokens":22}`,
			want: `{"input_tokens":11,"output_tokens":22}`},
		{name: "Anthropic 原生字段名", usage: `{"input_tokens":5,"output_tokens":6}`,
			want: `{"input_tokens":5,"output_tokens":6}`},
		{name: "零值退到第二个字段", usage: `{"prompt_tokens":0,"input_tokens":9,"completion_tokens":0,"output_tokens":4}`,
			want: `{"input_tokens":9,"output_tokens":4}`},
		{name: "负数是真值不回落", usage: `{"prompt_tokens":-3,"input_tokens":5}`,
			want: `{"input_tokens":-3,"output_tokens":0}`},
		{name: "两个缓存字段都保留", usage: `{"prompt_tokens":1,"completion_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}`,
			want: `{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}`},
		{name: "cached_tokens 顶成 cache_read", usage: `{"prompt_tokens":1,"completion_tokens":2,"cached_tokens":8}`,
			want: `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":8}`},
		{name: "显式 cache_read 优先于 cached_tokens", usage: `{"prompt_tokens":1,"completion_tokens":2,"cached_tokens":8,"cache_read_input_tokens":4}`,
			want: `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":4}`},
		{name: "浮点向零截断", usage: `{"prompt_tokens":1.9,"completion_tokens":2.0}`,
			want: `{"input_tokens":1,"output_tokens":2}`},
		{name: "容器值算 0", usage: `{"prompt_tokens":{"x":1},"completion_tokens":[1]}`,
			want: `{"input_tokens":0,"output_tokens":0}`},
		{name: "只有 input_tokens", usage: `{"input_tokens":1}`,
			want: `{"input_tokens":1,"output_tokens":0}`},
		{name: "只有 output_tokens", usage: `{"output_tokens":1}`,
			want: `{"input_tokens":0,"output_tokens":1}`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			state := NewAnthropicStreamState()
			chunk := sseLine(`{"choices":[{"delta":{"content":"x"}}],"usage":` + item.usage + `}`)
			_, usage := state.Push([]byte(chunk))
			if usage == nil {
				t.Fatal("未提取到 usage")
			}
			if got := canonical.DumpsOrdered(AnthropicStreamUsage(usage)); got != item.want {
				t.Errorf("usage 不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}
}

// TestAnthropicStreamUsageNilAndOverwrite 覆盖 usage 缺失与后块覆盖。
func TestAnthropicStreamUsageNilAndOverwrite(t *testing.T) {
	state := NewAnthropicStreamState()
	if _, usage := state.Push([]byte(sseLine(`{"choices":[{"delta":{"content":"a"}}]}`))); usage != nil {
		t.Fatalf("无 usage 字段时应返回 nil，实际 %s", canonical.DumpsOrdered(usage))
	}

	_, first := state.Push([]byte(sseLine(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)))
	if got, want := canonical.DumpsOrdered(AnthropicUsage(first, true)), `{"input_tokens":1,"output_tokens":2}`; got != want {
		t.Fatalf("首次提取不符：%s", got)
	}

	_, second := state.Push([]byte(sseLine(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":8}}`)))
	if got, want := canonical.DumpsOrdered(AnthropicUsage(second, true)), `{"input_tokens":7,"output_tokens":8}`; got != want {
		t.Fatalf("后块应覆盖：%s", got)
	}
}

// TestAnthropicUsageEmptyShapes 覆盖 usage 为空的各种形态。
//
// 期望值来自 _gen_anthropic.py 的 usage_shapes 头两行（null 与 {}）以及
// _gen_anthropic2.py 的 usage_non_dict 里的 `0` 一行。注意 **0 与 true 的区别**：
// 参照实现写的是 `usage or {}`，所以 0（假值）退化成空字典、true（真值）会去调用
// .get() 并抛 AttributeError；本实现把非字典一律当作空处理（见 AnthropicUsage 的
// 注释与 TestAnthropicUsageKnownDeviations）。
func TestAnthropicUsageEmptyShapes(t *testing.T) {
	for _, item := range []struct {
		name string
		json string
	}{
		{name: "null", json: `null`},
		{name: "空对象", json: `{}`},
		{name: "数字零（假值）", json: `0`},
		{name: "空数组", json: `[]`},
		{name: "不在字典上的键", json: ``},
	} {
		t.Run(item.name, func(t *testing.T) {
			var usage *canonical.Value
			if item.json != "" {
				parsed, err := canonical.ParseString(item.json)
				if err != nil {
					t.Fatalf("解析失败: %v", err)
				}
				usage = parsed
			}
			got := canonical.DumpsOrdered(AnthropicUsage(usage, true))
			if want := `{"input_tokens":0,"output_tokens":0}`; got != want {
				t.Errorf("期望 %s，实际 %s", want, got)
			}
		})
	}
}

// TestAnthropicUsageKnownDeviations 记录**已知且有意保留**的与参照实现的差异。
//
// 差异来自 common.go 的 usageInt / usageIntOr，本模块被要求复用它们而不重复实现：
//
//  1. anthropic.py 的 `_usage_int` 有 `isinstance(value, str) and value.isdigit()`
//     分支，字符串计数会被解析成整数；usageInt 只认 JSON 数字，字符串给 0。
//  2. Python 的 `_usage_int(True)` 返回 True（bool 是 int 的子类），渲染成 JSON 的
//     `true`；usageInt 明确排除 bool，给 0。
//  3. Python 的 `a or b` 在 a 为「真值但转不出数字」时会**停在 a**；usageIntOr 在
//     转换结果为 0 时会继续看下一个键。因此 `{"prompt_tokens":{...},"input_tokens":5}`
//     Python 给 0、Go 给 5。
//
// 这里的期望值就是 Python 的真实输出（_gen_anthropic2.py 的 usage_non_dict 与
// usage_fallback），注释里同时写明 Go 的当前行为。若日后把 usageInt 补成完整的
// Python 语义，这个测试必须**一起改**——它是刻意的护栏，不是遗漏。
func TestAnthropicUsageKnownDeviations(t *testing.T) {
	cases := []struct {
		name     string
		json     string
		python   string
		goActual string
	}{
		{
			name:     "字符串计数：Python 解析，Go 给 0",
			json:     `{"prompt_tokens":"12","completion_tokens":true}`,
			python:   `{"input_tokens":12,"output_tokens":true}`,
			goActual: `{"input_tokens":0,"output_tokens":0}`,
		},
		{
			name:     "真值但不可转数字：Python 停在第一个键，Go 回落到第二个",
			json:     `{"prompt_tokens":{"x":1},"input_tokens":5,"completion_tokens":[1],"output_tokens":6}`,
			python:   `{"input_tokens":0,"output_tokens":0}`,
			goActual: `{"input_tokens":5,"output_tokens":6}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			usage, err := canonical.ParseString(item.json)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			got := canonical.DumpsOrdered(AnthropicUsage(usage, true))
			if got != item.goActual {
				t.Errorf("Go 行为已变化，请同步更新注释与文档\nPython 期望: %s\nGo 当前: %s\nGo 实际: %s",
					item.python, item.goActual, got)
			}
		})
	}
}

// TestAnthropicMessageResponseMatchesPython 锁定非流式转换的逐字节输出。
//
// 期望值来自 _gen_anthropic4.py 的 `=== responses ===`（由 _anthropic_message_response
// 直接打印）。几个非显然的点：
//
//   - content 为空时补一个 `{"type":"text","text":""}`，而不是给空数组；
//   - `stop_sequence` 是 **null**（Python 的 None），不是空字符串；
//   - 参数是坏 JSON / 字面量 null / 缺键 / 显式 null 时，input 都退化成 {}；
//   - 参数是 JSON 对象时**原样保留**（键顺序按上游给的顺序，"b" 在前）；
//   - `id` 缺失回落到 "msg_amkr"；数字 id 被 str() 成字符串。
func TestAnthropicMessageResponseMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "纯文本",
			input: `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"你好，世界"},"finish_reason":"stop"}]}`,
			want:  `{"id":"chatcmpl-1","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":"你好，世界"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "多字节文本与 usage",
			input: `{"id":"chatcmpl-2","choices":[{"message":{"content":"中文内容🚀"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"cached_tokens":9}}`,
			want:  `{"id":"chatcmpl-2","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":"中文内容🚀"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":9}}`,
		},
		{
			// content 数组里的数字 3 被 **丢弃**（MessageText 不序列化非字典非字符串
			// 元素），所以只剩 "a" + "b"。
			name:  "content 数组拼接并丢弃裸数字",
			input: `{"choices":[{"message":{"content":[{"type":"text","text":"a"},"b",3]},"finish_reason":"stop"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":"ab"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "工具调用",
			input: `{"id":"x","choices":[{"message":{"content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`,
			want:  `{"id":"x","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"tool_use","id":"call_9","name":"f","input":{"a":1}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "参数不是合法 JSON",
			input: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"f","arguments":"not json"}}]},"finish_reason":"function_call"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"tool_use","id":"call_amkr_0","name":"f","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "参数是显式 null",
			input: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"f","arguments":null}}]},"finish_reason":"tool_calls"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"tool_use","id":"call_amkr_0","name":"f","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			// 字面量 "null" 解析成功但结果是 None，同样退化成 {}。
			name:  "参数是 JSON 字面量 null",
			input: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"f","arguments":"null"}}]},"finish_reason":"stop"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"tool_use","id":"call_amkr_0","name":"f","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			// 对象参数原样保留：**不**做 canonical 排序，键顺序即上游顺序（b 在 a 前）。
			name:  "参数是 JSON 对象",
			input: `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":{"b":2,"a":"中"}}}]},"finish_reason":"stop"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"tool_use","id":"c","name":"f","input":{"b":2,"a":"中"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "旧式 function_call",
			input: `{"choices":[{"message":{"function_call":{"name":"g","arguments":"{\"z\":0}"}},"finish_reason":"function_call"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"tool_use","id":"call_amkr_0","name":"g","input":{"z":0}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "空 content 补空文本块",
			input: `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":""}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "finish_reason length 映射 max_tokens",
			input: `{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":"x"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "usage 不是字典时当作空",
			input: `{"choices":[{"message":{"content":"y"},"finish_reason":"stop"}],"usage":[]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":"y"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "数字 id 被渲染成字符串",
			input: `{"id":12345,"choices":[{"message":{"content":"y"},"finish_reason":"stop"}]}`,
			want:  `{"id":"12345","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":"y"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name:  "content 为 null",
			input: `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`,
			want:  `{"id":"msg_amkr","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":""}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			data, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			result := AnthropicMessageResponse(data, "claude-3-5-sonnet")
			if result == nil {
				t.Fatal("不应返回 nil")
			}
			if got := canonical.DumpsOrdered(result); got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}
}

// TestAnthropicMessageResponseReturnsNil 覆盖无法转换的输入。
//
// 期望值来自 _gen_anthropic.py 的 message_response（no_choices 一行输出 null），
// 以及 anthropic.py:271 对非字典直接 return None。
func TestAnthropicMessageResponseReturnsNil(t *testing.T) {
	for _, item := range []struct {
		name string
		json string
	}{
		{name: "非对象", json: `[1,2]`},
		{name: "没有 choices", json: `{"id":"x"}`},
		{name: "choices 不是数组", json: `{"choices":"x"}`},
		{name: "choices 里没有字典", json: `{"choices":[1,"a"]}`},
		{name: "choices 为空数组", json: `{"choices":[]}`},
	} {
		t.Run(item.name, func(t *testing.T) {
			data, err := canonical.ParseString(item.json)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if result := AnthropicMessageResponse(data, "m"); result != nil {
				t.Errorf("应返回 nil，实际 %s", canonical.DumpsOrdered(result))
			}
		})
	}
}

// TestAnthropicMessageResponsePassthrough 断言已是 Anthropic message 的响应体
// **原样返回同一个对象**（不复制、不规范化）。
//
// 期望值来自 _gen_anthropic.py 的 passthrough_message：输出与输入逐字节相同，
// 且顺序是上游给的 `type, content, id`（说明没有被重建过）。
func TestAnthropicMessageResponsePassthrough(t *testing.T) {
	const input = `{"type":"message","content":[{"type":"text","text":"already"}],"id":"msg_1"}`
	data, err := canonical.ParseString(input)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	result := AnthropicMessageResponse(data, "claude-3-5-sonnet")
	if result != data {
		t.Fatal("应返回同一个对象（参照实现直接 return data）")
	}
	if got := canonical.DumpsOrdered(result); got != input {
		t.Errorf("输出应逐字节等于输入\n期望: %s\n实际: %s", input, got)
	}
}

// TestAnthropicToolUseBlocksMatchesPython 锁定 tool_use 块的构造。
//
// 期望值来自 _gen_anthropic.py 的 tool_use_blocks。要点：
//   - 非字典元素被跳过，但**枚举下标照样前进**，所以第三个元素的 id 是 call_amkr_2；
//   - tool_calls 存在时即使为空数组也**不会**回落到 function_call（Python 判的是
//     isinstance(list)），所以第二例输出 []；
//   - 缺 function 的项被跳过；缺 name 得到空字符串（不是 None）。
func TestAnthropicToolUseBlocksMatchesPython(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "跳过非字典但下标前进",
			input: `{"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}},"skip",{"function":{"name":"g","arguments":""}}]}`,
			want:  `[{"type":"tool_use","id":"c1","name":"f","input":{"a":1}},{"type":"tool_use","id":"call_amkr_2","name":"g","input":{}}]`,
		},
		{
			name:  "空的 tool_calls 不回落 function_call",
			input: `{"tool_calls":[],"function_call":{"name":"h","arguments":"{}"}}`,
			want:  `[]`,
		},
		{
			name:  "只有 function_call 时补 call_amkr_0",
			input: `{"function_call":{"name":"h","arguments":"{\"k\":\"中\"}"}}`,
			want:  `[{"type":"tool_use","id":"call_amkr_0","name":"h","input":{"k":"中"}}]`,
		},
		{
			name:  "tool_calls 不是列表时回落",
			input: `{"tool_calls":"not a list","function_call":{"name":"h"}}`,
			want:  `[{"type":"tool_use","id":"call_amkr_0","name":"h","input":{}}]`,
		},
		{
			name:  "什么都没有",
			input: `{}`,
			want:  `[]`,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			data, err := canonical.ParseString(item.input)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			blocks := AnthropicToolUseBlocks(data)
			got := "[]"
			if len(blocks) > 0 {
				got = canonical.DumpsOrdered(canonical.NewArray(blocks...))
			}
			if got != item.want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", item.want, got)
			}
		})
	}
}

// TestAnthropicStopReasonMatchesPython 锁定 finish_reason 的映射。
//
// 期望值来自 _gen_anthropic2.py 的 stop_reason 段。三个非显然点：
//   - length **优先于** has_tool_use（有工具调用时也返回 max_tokens）；
//   - has_tool_use 为真时任何 reason 都返回 tool_use（连 content_filter 也是）；
//   - 数字 3 与 true 都落到 end_turn（它们不等于任何字符串）。
//
// 参照实现对**数组/对象** reason 会抛 TypeError（`reason in {...}` 需要可哈希），
// 本实现返回 end_turn——见注释里的说明。
func TestAnthropicStopReasonMatchesPython(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		hasTool   bool
		want      string
		deviation bool
	}{
		{name: "length", reason: `"length"`, want: "max_tokens"},
		{name: "length 且带工具", reason: `"length"`, hasTool: true, want: "max_tokens"},
		{name: "tool_calls", reason: `"tool_calls"`, want: "tool_use"},
		{name: "function_call", reason: `"function_call"`, want: "tool_use"},
		{name: "stop", reason: `"stop"`, want: "end_turn"},
		{name: "stop 且带工具", reason: `"stop"`, hasTool: true, want: "tool_use"},
		{name: "content_filter", reason: `"content_filter"`, want: "end_turn"},
		{name: "content_filter 且带工具", reason: `"content_filter"`, hasTool: true, want: "tool_use"},
		{name: "null", reason: `null`, want: "end_turn"},
		{name: "null 且带工具", reason: `null`, hasTool: true, want: "tool_use"},
		{name: "未知字符串", reason: `"other"`, want: "end_turn"},
		{name: "数字", reason: `3`, want: "end_turn"},
		{name: "布尔", reason: `true`, want: "end_turn"},
		{name: "数组（Python 抛 TypeError）", reason: `["x"]`, want: "end_turn", deviation: true},
		{name: "对象（Python 抛 TypeError）", reason: `{"a":1}`, want: "end_turn", deviation: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			reason, err := canonical.ParseString(item.reason)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			got := AnthropicStopReason(reason, item.hasTool)
			if got != item.want {
				t.Errorf("期望 %s，实际 %s", item.want, got)
			}
			if item.deviation {
				// 记下这是有意的稳健性提高，避免有人当成 bug 去"修"。
				t.Logf("注意：参照实现对 %s 抛 TypeError，Go 侧返回 %s", item.reason, got)
			}
		})
	}

	// 键缺失（Python 的 dict.get 返回 None）与显式 null 同路。
	if got := AnthropicStopReason(nil, false); got != "end_turn" {
		t.Errorf("缺失 finish_reason 应为 end_turn，实际 %s", got)
	}
	if got := AnthropicStopReason(nil, true); got != "tool_use" {
		t.Errorf("缺失 finish_reason 且带工具应为 tool_use，实际 %s", got)
	}
}

// TestAnthropicStreamStopReasonRecorded 断言 finish_reason 被记录，且显式 null
// **不覆盖**已记录的值。
//
// 期望值来自 _gen_anthropic2.py 的 finish_reason_null_keeps（输出 max_tokens）。
// 参照实现判的是 `is not None`，所以显式 null 会被忽略，而缺失键也返回 None。
func TestAnthropicStreamStopReasonRecorded(t *testing.T) {
	state := NewAnthropicStreamState()
	state.Push([]byte(sseLine(`{"choices":[{"delta":{},"finish_reason":"length"}]}`)))
	if state.StopReason != "max_tokens" {
		t.Fatalf("期望 max_tokens，实际 %q", state.StopReason)
	}
	state.Push([]byte(sseLine(`{"choices":[{"delta":{},"finish_reason":null}]}`)))
	if state.StopReason != "max_tokens" {
		t.Fatalf("显式 null 不应覆盖，实际 %q", state.StopReason)
	}
	state.Push([]byte(sseLine(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)))
	if state.StopReason != "end_turn" {
		t.Fatalf("期望 end_turn，实际 %q", state.StopReason)
	}
}

// TestAnthropicStreamContentIndexes 断言 content index 的分配与收尾状态。
//
// 期望值来自 _gen_anthropic4.py 各段末尾的注释（// buffer=... stop=...）以及
// _gen_anthropic.py 里打印的 NEXT_INDEX / TEXT_INDEX。
func TestAnthropicStreamContentIndexes(t *testing.T) {
	state := NewAnthropicStreamState()
	state.Push([]byte(sseLine(`{"choices":[{"delta":{"content":"a"}}]}`)))
	if state.NextContentIndex != 1 || state.TextIndex == nil || *state.TextIndex != 0 {
		t.Fatalf("文本块状态异常：next=%d text=%v", state.NextContentIndex, state.TextIndex)
	}
	if state.TextStopped {
		t.Fatal("文本块未收到工具调用时不该被标记为已停止")
	}
	state.Push([]byte(sseLine(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`)))
	if !state.TextStopped {
		t.Fatal("工具调用出现后文本块应被标记为已停止")
	}
	if state.ActiveToolIndex == nil || *state.ActiveToolIndex != 0 {
		t.Fatalf("活跃工具下标异常：%v", state.ActiveToolIndex)
	}
	if state.NextContentIndex != 2 {
		t.Fatalf("工具块应占用 index 1，next 期望 2 实际 %d", state.NextContentIndex)
	}
	// 收尾后活跃工具被清空，且再次收尾不再产出事件。
	first := state.FinishTools()
	if state.ActiveToolIndex != nil {
		t.Fatal("收尾后活跃工具应被清空")
	}
	if len(first) != 1 {
		t.Fatalf("收尾应产出 1 个 content_block_stop，实际 %d 个", len(first))
	}
	if second := state.FinishTools(); len(second) != 0 {
		t.Fatalf("重复收尾应无事件，实际 %d 个", len(second))
	}
}
