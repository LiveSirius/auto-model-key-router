// Package protocol 实现 OpenAI / Anthropic Messages / OpenAI Responses 三种协议
// 之间的请求体与响应流转换。
//
// 移植 auto_model_key_router/protocols/ 下的四个模块。三条通用约束：
//
//  1. SSE 的 JSON 用**紧凑且不排序**的形式（canonical.DumpsOrdered），键顺序即
//     Python dict 的插入顺序，客户端可依赖它；事件格式是
//     "event: {name}\ndata: {json}\n\n"。
//  2. JSON 解析必须用 internal/canonical，不能用 encoding/json——后者把所有数字
//     变成 float64，会丢大整数精度（token 计数与 id 都可能很大）。
//  3. 从上游流里取 usage 时，后者覆盖前者（同一流里可能多次出现 usage 块）。
package protocol

import (
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// ExtractUsage 从响应体里取出 usage 对象。
//
// 移植 metrics.py:1054。三个位置按优先级探测：顶层 `usage`、`response.usage`
// （Responses API 形态）、`message.usage`（Messages 形态）。都不是字典则返回 nil。
//
// ponytail: 这里对 metrics 包的 extract_usage 做了**有意的重复实现**（共 10 行），
// 而不是 import internal/metrics。理由是转换层不该依赖存储层——两个包由不同批次
// 交付，而真正的跨供应商归一化（_normalize_usage，涉及 OpenAI 的 prompt_tokens vs
// Anthropic 的 input_tokens 与缓存字段）属于 metrics 的职责。升级路径：若日后再有
// 第三处需要它，就把这个函数提到一个不依赖任何存储细节的公共包。
func ExtractUsage(data *canonical.Value) *canonical.Value {
	if !data.IsObject() {
		return nil
	}
	if usage := data.Lookup("usage"); usage.IsObject() {
		return usage
	}
	if response := data.Lookup("response"); response.IsObject() {
		if usage := response.Lookup("usage"); usage.IsObject() {
			return usage
		}
	}
	if message := data.Lookup("message"); message.IsObject() {
		if usage := message.Lookup("usage"); usage.IsObject() {
			return usage
		}
	}
	return nil
}

// OpenaiChoices 返回 `choices` 里的字典元素。
//
// 移植 common.py:77：非列表返回空；列表里非字典的元素被丢弃（不是报错）。
func OpenaiChoices(data *canonical.Value) []*canonical.Value {
	if !data.IsObject() {
		return nil
	}
	choices := data.Lookup("choices")
	if !choices.IsArray() {
		return nil
	}
	result := make([]*canonical.Value, 0, choices.Len())
	for _, choice := range choices.Items() {
		if choice.IsObject() {
			result = append(result, choice)
		}
	}
	return result
}

// DeltaText 取 delta 里的文本。
func DeltaText(delta *canonical.Value) string {
	if !delta.IsObject() {
		return ""
	}
	return MessageText(delta.Lookup("content"))
}

// MessageText 把 content 归一成字符串。
//
// 移植 common.py:89。与 ToolResultText 的差别很关键：这里**丢弃**既非字典也非
// 字符串的元素（不做 JSON 序列化兜底），而 ToolResultText 会把它序列化成 JSON。
// 两者不能互换，否则工具调用参数会多出意外的 JSON 文本片段。
func MessageText(content *canonical.Value) string {
	switch {
	case content == nil || content.IsNull():
		return ""
	case content.Kind == canonical.KindString:
		return content.Str
	case content.IsArray():
		var parts strings.Builder
		for _, part := range content.Items() {
			if part.IsObject() {
				if text := part.Lookup("text"); !text.IsNull() {
					parts.WriteString(text.PyStr())
				}
				continue
			}
			if part.Kind == canonical.KindString {
				parts.WriteString(part.Str)
			}
		}
		return parts.String()
	default:
		return content.PyStr()
	}
}

// ToolResultText 把工具结果归一成字符串。
//
// 移植 common.py:10。与 MessageText 的差别：数组里既非字典也非字符串的元素会被
// **JSON 序列化**（保持信息不丢），字典元素若没有 text 字段也会被序列化。
func ToolResultText(content *canonical.Value) string {
	switch {
	case content == nil || content.IsNull():
		return ""
	case content.Kind == canonical.KindString:
		return content.Str
	case content.IsArray():
		var parts strings.Builder
		for _, part := range content.Items() {
			if part.IsObject() {
				if text := part.Lookup("text"); !text.IsNull() {
					parts.WriteString(text.PyStr())
					continue
				}
			}
			if part.Kind == canonical.KindString {
				parts.WriteString(part.Str)
				continue
			}
			parts.WriteString(canonical.DumpsOrdered(part))
		}
		return parts.String()
	case content.IsObject():
		if text := content.Lookup("text"); !text.IsNull() {
			return text.PyStr()
		}
	}
	return canonical.DumpsOrdered(content)
}

// jsonText 解析 JSON 文本，失败返回 nil。
//
// 移植 common.py:122：解析失败**静默**返回 nil（流里出现坏行不该中断整个响应）。
func jsonText(content string) *canonical.Value {
	value, err := canonical.ParseString(content)
	if err != nil {
		return nil
	}
	return value
}

// --- SSE 行切分 ---

// SSELine 是解析出的一行 data 载荷。
type SSELine struct {
	// Data 是 `data:` 之后的去空白内容。
	Data string
	// Payload 是解析后的 JSON；解析失败或非对象时为 nil。
	Payload *canonical.Value
}

// SSESplitter 在字节流上按行切分 SSE data 行。
//
// **这是对本项目一个既有缺陷的修复。** 参照实现的三个流式转换函数都写
// `buffer += chunk.decode("utf-8", errors="replace")`，再按 "\n" 切分。当多字节
// UTF-8 字符被 TCP 分片切开时，两个分片各自 decode 都会产生 U+FFFD，导致中文与
// emoji 内容静默变成乱码。
//
// 这里改为**只在字节层面寻找换行**，拿到完整一行后才 decode，因此跨分片的多字节
// 序列能正确拼接。切分规则同时接受 "\n" 与 "\r\n"（参照实现靠 line.strip() 顺带
// 吃掉 \r）。
//
// ponytail: 未做的是「首块依赖绝对 deadline」以外的背压控制——上游产得比下游写得
// 快时缓冲区会增长。当前上游块大小有界（32KB）且下游是同速转发，暂不需要；若将来
// 出现超大单行（例如 base64 内联图片在一个 data 行里），应改为带上限的流式扫描并在
// 超限时报错，而不是无界累积。
type SSESplitter struct {
	buffer []byte
}

// NewSSESplitter 构造切分器。
func NewSSESplitter() *SSESplitter { return &SSESplitter{} }

// Push 追加一块数据并返回其中已完整的 SSE 行。
//
// 返回的行按出现顺序排列；未以换行结尾的尾部留在内部缓冲，等下一块到来。
func (s *SSESplitter) Push(chunk []byte) []SSELine {
	s.buffer = append(s.buffer, chunk...)
	var lines []SSELine
	for {
		index := indexByte(s.buffer, '\n')
		if index < 0 {
			break
		}
		raw := s.buffer[:index]
		// 复制出剩余部分再重置缓冲：append 可能复用底层数组，直接切片会互相踩。
		rest := make([]byte, len(s.buffer)-index-1)
		copy(rest, s.buffer[index+1:])
		s.buffer = rest

		line := trimSpaceBytes(raw)
		// 只在拿到完整一行后才 decode，这是修复的核心。
		text := string(line)
		if !strings.HasPrefix(text, "data:") {
			continue
		}
		data := strings.TrimSpace(text[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		payload := jsonText(data)
		if payload == nil || !payload.IsObject() {
			// 参照实现跳过非对象的载荷（含解析失败）。
			continue
		}
		lines = append(lines, SSELine{Data: data, Payload: payload})
	}
	return lines
}

// Flush 返回并清空未以换行结尾的残余缓冲。
//
// 参照实现从不处理残余（流结束时若有半行会被丢弃），这里保留同样语义，只在需要
// 诊断时读取内容。
func (s *SSESplitter) Flush() string {
	text := string(s.buffer)
	s.buffer = nil
	return text
}

// Pending 返回当前缓冲的字节数（供测试观测）。
func (s *SSESplitter) Pending() int { return len(s.buffer) }

// indexByte 查找字节位置。
//
// 直接用 strings.IndexByte 的字节版本，避免为此引入 bytes 包。
func indexByte(data []byte, target byte) int {
	for index, value := range data {
		if value == target {
			return index
		}
	}
	return -1
}

// trimSpaceBytes 去掉字节切片两端的 ASCII 空白。
//
// 对应 Python 的 str.strip() 在常见输入下的行为。注意 Python 还会去掉 Unicode
// 空白，但 SSE 行首尾只会出现 ASCII 空白，因此这里足够。
func trimSpaceBytes(data []byte) []byte {
	start := 0
	for start < len(data) && isSpaceByte(data[start]) {
		start++
	}
	end := len(data)
	for end > start && isSpaceByte(data[end-1]) {
		end--
	}
	return data[start:end]
}

// isSpaceByte 报告是否为 ASCII 空白。
func isSpaceByte(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// --- SSE 编码 ---

// EncodeSSE 按参照实现的格式编码一个 SSE 事件。
//
// 格式为 "event: {event}\ndata: {紧凑JSON}\n\n"（anthropic.py:226）。JSON 用
// **不排序**的紧凑形式，键顺序即构造顺序。
func EncodeSSE(event string, payload *canonical.Value) []byte {
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\ndata: ")
	b.WriteString(canonical.DumpsOrdered(payload))
	b.WriteString("\n\n")
	return []byte(b.String())
}

// --- 数值与文本辅助 ---

// usageInt 把 usage 字段转成整数，无法转换时为 0。
//
// 移植 anthropic.py:258。Python 侧用 isinstance(value, int) 判断并排除 bool，
// 这里保持同样语义（bool 不当作数值）。
func usageInt(value *canonical.Value) int64 {
	if value == nil || value.Kind != canonical.KindNumber {
		return 0
	}
	parsed, err := canonical.ToInt(value)
	if err != nil {
		return 0
	}
	return parsed
}

// usageIntOr 取第一个真值字段并转成整数。
//
// 对应 Python 的 `source.get("prompt_tokens") or source.get("input_tokens")`：
// 注意用的是 `or`，所以 0 会退到第二个字段。
func usageIntOr(source *canonical.Value, keys ...string) int64 {
	for _, key := range keys {
		value := source.Lookup(key)
		if value.Truthy() {
			if parsed := usageInt(value); parsed != 0 {
				return parsed
			}
		}
	}
	return 0
}

// intString 把整数渲染成文本。
func intString(value int64) string { return strconv.FormatInt(value, 10) }
