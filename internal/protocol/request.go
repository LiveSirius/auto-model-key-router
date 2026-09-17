package protocol

import (
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 本文件移植 auto_model_key_router/protocols/request.py：把下游可能发来的三种
// 请求体方言（OpenAI chat-completions、Anthropic Messages、OpenAI Responses）
// 统一改写成上游能吃的 chat-completions 形态，并实现 /v1/messages/count_tokens
// 的本地 token 估算。
//
// 两条贯穿全文件的语义约束：
//
//  1. **键的插入顺序即输出顺序**。参照实现到处是 `dict(payload)` 浅拷贝 +
//     `pop` + 重新赋值，Python dict 的「覆盖不移动位置、新键追加到末尾」规则
//     直接决定上游收到的 JSON 字段顺序。Go 侧用 canonical.Object 的 SetKey/DeleteKey
//     复刻同一规则，不能换成 map。
//  2. **真值判断用 Truthy()**。Python 的 `x or default` 会把 0 / "" / [] / {} /
//     None 一并当作假值，而 `is not None` 只认 None；两者在同一条链上交替出现
//     （例如 `str(item.get("call_id") or item.get("id") or "call_amkr")` 与
//     `if tool.get("description") is not None`），必须逐处对齐。

// AdaptMessagePayload 把任意方言的请求体适配成 OpenAI chat-completions 形态。
//
// 移植 request.py:9。分派完全靠**键存在性嗅探**：先看 `messages`，再看 `input`，
// 都没有就当已经是 chat 请求体，只做参数规范化。这条顺序决定了 `input` 在
// chat 请求体里不会被误当成 Responses 的输入（embeddings 体因此需要调用方另行
// 绕开，见 proxy_support.py:115）。
//
// 非对象输入直接原样返回：参照实现只在 dict 上定义（`"messages" in payload`
// 对字符串会退化成子串匹配），调用方传进来的始终是解析后的 JSON 对象，
// 这里以「不做任何事」作为最保守的兜底。
func AdaptMessagePayload(payload *canonical.Value) *canonical.Value {
	if !payload.IsObject() {
		return payload
	}
	if _, ok := payload.LookupOK("messages"); ok {
		return requestNormalizeChatCompatParameters(requestAdaptAnthropicMessagesPayload(payload))
	}
	if _, ok := payload.LookupOK("input"); ok {
		return requestNormalizeChatCompatParameters(requestAdaptResponsesInputPayload(payload))
	}
	return requestNormalizeChatCompatParameters(payload)
}

// EstimateAnthropicInputTokens 用字节启发式估算输入 token 数。
//
// 移植 request.py:21，服务于 `/v1/messages/count_tokens`（proxy_handler.py:312）：
// 本地算 `max(1, (utf8_len + 3) // 4)`，不转发上游、也不写指标行。
//
// 参与计算的是**除 model / stream / max_tokens / max_output_tokens 之外的全部字段**，
// 包括未知字段；顺序即原请求体的顺序，因此这里必须用 DumpsOrdered 而不是 Dumps。
// 长度是 UTF-8 字节数，故 len(string) 即可（Go 的 string 长度就是字节数）。
func EstimateAnthropicInputTokens(payload *canonical.Value) int {
	content := canonical.NewObject()
	if payload.IsObject() {
		for _, key := range payload.Obj.Keys() {
			if requestEstimateExcludedKeys[key] {
				continue
			}
			value, _ := payload.Obj.Get(key)
			content.SetKey(key, value)
		}
	}
	// 与 Python 一致地取字节长度：非 ASCII 内容按 UTF-8 编码后的字节数计。
	encoded := len(canonical.DumpsOrdered(content))
	tokens := (encoded + 3) / 4
	if tokens < 1 {
		return 1
	}
	return tokens
}

// requestEstimateExcludedKeys 是 count_tokens 估算时被剔除的字段。
var requestEstimateExcludedKeys = map[string]bool{
	"model":             true,
	"stream":            true,
	"max_tokens":        true,
	"max_output_tokens": true,
}

// --- Anthropic Messages 请求体 ---

// requestAdaptAnthropicMessagesPayload 把 Anthropic Messages 请求体改成 chat 形态。
//
// 移植 request.py:33。`messages` 不是列表时**原样返回**（连 system / tools 都不动），
// 这是参照实现唯一的分支判断。
func requestAdaptAnthropicMessagesPayload(payload *canonical.Value) *canonical.Value {
	messages := payload.Lookup("messages")
	if !messages.IsArray() {
		return payload
	}
	adapted := payload.Clone()
	adaptedMessages := []*canonical.Value{}
	for _, message := range messages.Items() {
		if !message.IsObject() {
			continue
		}
		adaptedMessages = append(adaptedMessages, requestAdaptAnthropicMessage(message)...)
	}
	// `system` 是 Messages 方言的顶层字段，要降级成一条 system 消息插到最前面。
	// 注意两点：键**无论真值与否都会被 pop 掉**（不能留给上游），而判据是
	// `if system:`——空串 / [] / {} 都不插入消息。
	if system, _ := requestPop(adapted, "system"); system.Truthy() {
		adaptedMessages = append([]*canonical.Value{canonical.NewObjectOf(
			canonical.ObjectPair{Key: "role", Value: canonical.NewString("system")},
			canonical.ObjectPair{Key: "content", Value: requestAdaptContent(system)},
		)}, adaptedMessages...)
	}
	adapted.SetKey("messages", canonical.NewArray(adaptedMessages...))
	if tools := adapted.Lookup("tools"); tools.IsArray() {
		adapted.SetKey("tools", requestAdaptToolList(tools))
	}
	if toolChoice := adapted.Lookup("tool_choice"); toolChoice.IsObject() {
		adapted.SetKey("tool_choice", requestAdaptAnthropicToolChoice(toolChoice))
		// 键存在性判据（不是真值）：`disable_parallel_tool_use: false` 也要显式
		// 翻出 parallel_tool_calls: true，让上游知道调用方主动开了并行。
		if disable, ok := toolChoice.LookupOK("disable_parallel_tool_use"); ok {
			adapted.SetKey("parallel_tool_calls", canonical.NewBool(!disable.Truthy()))
		}
	}
	return adapted
}

// --- Responses 请求体 ---

// requestAdaptResponsesInputPayload 把 OpenAI Responses 请求体改成 chat 形态。
//
// 移植 request.py:67。与 Anthropic 分支的差别在于：`input` 一律被消费掉（无论
// 是不是列表），`instructions` 走的是同一套 `_adapt_content`，且 tools 同样过
// Anthropic 的工具适配器（Responses 的工具定义与 Anthropic 的顶层 name 形态
// 一致，故复用；已是 `{"type":"function","function":{...}}` 的则原样保留）。
func requestAdaptResponsesInputPayload(payload *canonical.Value) *canonical.Value {
	adapted := payload.Clone()
	input, _ := requestPop(adapted, "input")
	messages := requestResponsesInputToMessages(input)
	// 与参照实现一致：instructions **无论真值与否都已被 pop 掉**，只有真值时才
	// 生成 system 消息。
	if instructions, _ := requestPop(adapted, "instructions"); instructions.Truthy() {
		messages = append([]*canonical.Value{canonical.NewObjectOf(
			canonical.ObjectPair{Key: "role", Value: canonical.NewString("system")},
			canonical.ObjectPair{Key: "content", Value: requestAdaptContent(instructions)},
		)}, messages...)
	}
	// `messages` 在 Responses 体里通常不存在，因此这个键会被**追加到末尾**——
	// 上游看到的字段顺序与参照实现一致。
	adapted.SetKey("messages", canonical.NewArray(messages...))
	if tools := adapted.Lookup("tools"); tools.IsArray() {
		adapted.SetKey("tools", requestAdaptToolList(tools))
	}
	// Responses 的 tool_choice 是 `{"type":"function","name":...}`，要包一层
	// OpenAI 的 `function` 对象；其它形态（none / auto / required 等字符串）不动。
	if toolChoice := adapted.Lookup("tool_choice"); toolChoice.IsObject() {
		if choiceType := toolChoice.Lookup("type"); choiceType.IsString() && choiceType.Str == "function" {
			adapted.SetKey("tool_choice", canonical.NewObjectOf(
				canonical.ObjectPair{Key: "type", Value: canonical.NewString("function")},
				canonical.ObjectPair{Key: "function", Value: canonical.NewObjectOf(
					canonical.ObjectPair{Key: "name", Value: canonical.NewString(toolChoice.Lookup("name").StringValue())},
				)},
			))
		}
	}
	return adapted
}

// --- 参数规范化 ---

// requestNormalizeChatCompatParameters 把非 chat 方言的字段翻译成 chat 字段并剔除噪声。
//
// 移植 request.py:94。三处容易踩的顺序细节：
//
//  1. `max_output_tokens` → `max_tokens` 时用的是「先 pop 再赋值」，所以
//     `max_tokens` 会**移动到末尾**；而 `max_tokens` 已存在时只删 `max_output_tokens`，
//     已有值保持不变（任务参数优先于调用方）。
//  2. `stop_sequences` → `stop` 同理。
//  3. 最后的剔除列表里 `metadata` 会把调用方自带的 metadata 一起丢掉，这是
//     参照实现的既有行为，照搬。
func requestNormalizeChatCompatParameters(payload *canonical.Value) *canonical.Value {
	adapted := payload.Clone()
	maxOutputTokens, hasMaxOutputTokens := requestPop(adapted, "max_output_tokens")
	if hasMaxOutputTokens && !requestHasKey(adapted, "max_tokens") {
		adapted.SetKey("max_tokens", maxOutputTokens)
	}
	stopSequences, hasStopSequences := requestPop(adapted, "stop_sequences")
	if hasStopSequences && !requestHasKey(adapted, "stop") {
		adapted.SetKey("stop", stopSequences)
	}
	for _, key := range requestDroppedKeys {
		adapted.DeleteKey(key)
	}
	return adapted
}

// requestDroppedKeys 是 chat 上游不认识、必须剔除的字段。
var requestDroppedKeys = []string{
	"anthropic_version",
	"metadata",
	"reasoning",
	"text",
	"truncation",
	"previous_response_id",
	"include",
	"store",
	"safety_identifier",
}

// --- Responses input → messages ---

// requestResponsesInputToMessages 把 Responses 的 `input` 展开成 chat 的 messages。
//
// 移植 request.py:119。`input` 有四种合法形态，分支顺序不能动：
// 字符串、非列表、列表（逐项按 `type` 分派）、以及列表中非字典的项。
func requestResponsesInputToMessages(value *canonical.Value) []*canonical.Value {
	if value.IsString() {
		return []*canonical.Value{requestUserMessage(value)}
	}
	if !value.IsArray() {
		return []*canonical.Value{requestUserMessage(requestAdaptContent(value))}
	}
	messages := []*canonical.Value{}
	for _, item := range value.Items() {
		if !item.IsObject() {
			messages = append(messages, requestUserMessage(requestAdaptContent(item)))
			continue
		}
		// item_type 在 Python 里只取一次（request.py:127），后面的 switch 与
		// `item_type == "message"`（request.py:169）共用同一个值。Go 里若在
		// switch 的初始化语句中声明，出了 switch 就不可见，所以必须先声明。
		itemType := requestStringField(item, "type")
		switch itemType {
		case "function_call":
			// 历史里的助手工具调用：参数是 JSON 字符串（Responses 方言），
			// 非字符串时先补 `or {}` 再序列化——所以 `arguments: ""` 会变成 "{}"。
			messages = append(messages, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "role", Value: canonical.NewString("assistant")},
				canonical.ObjectPair{Key: "content", Value: canonical.NewNull()},
				canonical.ObjectPair{Key: "tool_calls", Value: canonical.NewArray(canonical.NewObjectOf(
					canonical.ObjectPair{Key: "id", Value: canonical.NewString(requestCallID(item))},
					canonical.ObjectPair{Key: "type", Value: canonical.NewString("function")},
					canonical.ObjectPair{Key: "function", Value: canonical.NewObjectOf(
						canonical.ObjectPair{Key: "name", Value: canonical.NewString(item.Lookup("name").StringValue())},
						canonical.ObjectPair{Key: "arguments", Value: canonical.NewString(
							requestArgumentsText(item.Lookup("arguments")),
						)},
					)},
				))},
			))
			continue
		case "function_call_output":
			// 工具结果走 ToolResultText（会把非文本部分序列化），与 MessageText
			// 不同——两者不可互换，否则参数里会多出意外的 JSON 片段。
			messages = append(messages, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "role", Value: canonical.NewString("tool")},
				canonical.ObjectPair{Key: "tool_call_id", Value: canonical.NewString(item.Lookup("call_id").StringValue())},
				canonical.ObjectPair{Key: "content", Value: canonical.NewString(ToolResultText(item.Lookup("output")))},
			))
			continue
		case "reasoning":
			// 推理项直接丢弃：chat 上游没有对应概念。
			continue
		}
		// `str(item.get("type") or "user")` 在语义上等价于先 StringValue 再兜底：
		// StringValue 就是 `str(value or "")`，因此 0 / false / [] 都会退到 "user"。
		role := item.Lookup("role").StringValue()
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}
		// `item.get("content", item.get("text", ""))`：只有 content 键**不存在**
		// 时才回退到 text，`content: None` 不会回退。
		content, ok := item.LookupOK("content")
		if !ok {
			content = item.Lookup("text")
			if content == nil {
				content = canonical.NewString("")
			}
		}
		_, hasRole := item.LookupOK("role")
		_, hasContent := item.LookupOK("content")
		if itemType == "message" || hasRole || hasContent {
			messages = append(messages, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "role", Value: canonical.NewString(role)},
				canonical.ObjectPair{Key: "content", Value: requestAdaptContent(content)},
			))
			continue
		}
		// 既不是 message 形态、也没有 role/content：把整个项当作内容塞给 user。
		messages = append(messages, requestUserMessage(requestAdaptContent(item)))
	}
	return messages
}

// requestCallID 复刻 `str(item.get("call_id") or item.get("id") or "call_amkr")`。
func requestCallID(item *canonical.Value) string {
	if callID := item.Lookup("call_id").StringValue(); callID != "" {
		return callID
	}
	if id := item.Lookup("id").StringValue(); id != "" {
		return id
	}
	return "call_amkr"
}

// requestArgumentsText 复刻 `arguments if isinstance(arguments, str) else json.dumps(arguments or {})`。
func requestArgumentsText(arguments *canonical.Value) string {
	if arguments.IsString() {
		return arguments.Str
	}
	if !arguments.Truthy() {
		// 注意默认值是空对象而不是空串：`arguments: 0` 会变成 "{}"。
		return "{}"
	}
	return canonical.DumpsOrdered(arguments)
}

// requestUserMessage 构造一条 user 消息。
func requestUserMessage(content *canonical.Value) *canonical.Value {
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "role", Value: canonical.NewString("user")},
		canonical.ObjectPair{Key: "content", Value: content},
	)
}

// --- Anthropic message → OpenAI messages ---

// requestAdaptAnthropicMessage 把一条 Anthropic 消息拆成零到多条 chat 消息。
//
// 移植 request.py:178。返回值可能是**多条**，因为 user 消息里的 tool_result
// 块会被提升成独立的 role=tool 消息，并把同一 content 数组里其余块按原有分组
// 变回若干条 user 消息（保持块之间的相对顺序）。
func requestAdaptAnthropicMessage(message *canonical.Value) []*canonical.Value {
	// `str(message.get("role") or "user")`：StringValue 即 `str(value or "")`，
	// 因此 0 / false / [] / {} 一并退到 "user"。
	role := message.Lookup("role").StringValue()
	if role == "" {
		role = "user"
	}
	content := message.Lookup("content")
	if content == nil {
		content = canonical.NewString("")
	}
	// content 不是数组时整体套用 `{**message, "role":…, "content":…}`：
	// 保留 message 的其余字段（tool_call_id 等），只覆盖 role/content。
	if !content.IsArray() {
		adapted := message.Clone()
		adapted.SetKey("role", canonical.NewString(role))
		adapted.SetKey("content", requestAdaptContent(content))
		return []*canonical.Value{adapted}
	}

	if role == "assistant" {
		textParts := []*canonical.Value{}
		toolCalls := []*canonical.Value{}
		for index, part := range content.Items() {
			if part.IsObject() && requestStringField(part, "type") == "tool_use" {
				callID := part.Lookup("id").StringValue()
				if callID == "" {
					callID = "toolu_amkr_" + intString(int64(index))
				}
				// `input` 缺失或为 null 都补 `{}`；其余一律 JSON 序列化
				// （与 responses.py 不同，这里对已是非字符串的 0 / "" 也补 {}）。
				toolInput := part.Lookup("input")
				arguments := "{}"
				if !toolInput.IsNull() {
					arguments = canonical.DumpsOrdered(toolInput)
				}
				toolCalls = append(toolCalls, canonical.NewObjectOf(
					canonical.ObjectPair{Key: "id", Value: canonical.NewString(callID)},
					canonical.ObjectPair{Key: "type", Value: canonical.NewString("function")},
					canonical.ObjectPair{Key: "function", Value: canonical.NewObjectOf(
						canonical.ObjectPair{Key: "name", Value: canonical.NewString(part.Lookup("name").StringValue())},
						canonical.ObjectPair{Key: "arguments", Value: canonical.NewString(arguments)},
					)},
				))
				continue
			}
			// 非 tool_use 的块（含 text / image）留给文本侧统一适配。
			textParts = append(textParts, part)
		}
		adapted := message.Clone()
		adapted.DeleteKey("content")
		adapted.SetKey("role", canonical.NewString(role))
		if len(textParts) > 0 {
			adapted.SetKey("content", requestAdaptContent(canonical.NewArray(textParts...)))
		} else {
			// 纯工具调用：content 显式为 null（不能省，上游据此判断是工具回合）。
			adapted.SetKey("content", canonical.NewNull())
		}
		if len(toolCalls) > 0 {
			adapted.SetKey("tool_calls", canonical.NewArray(toolCalls...))
		}
		return []*canonical.Value{adapted}
	}

	if role == "user" && requestHasToolResultPart(content) {
		adaptedMessages := []*canonical.Value{}
		pendingParts := []*canonical.Value{}
		flushPending := func() {
			if len(pendingParts) == 0 {
				return
			}
			adaptedMessages = append(adaptedMessages, requestUserMessage(
				requestAdaptContent(canonical.NewArray(pendingParts...)),
			))
			pendingParts = []*canonical.Value{}
		}
		for _, part := range content.Items() {
			if !part.IsObject() || requestStringField(part, "type") != "tool_result" {
				pendingParts = append(pendingParts, part)
				continue
			}
			flushPending()
			adaptedMessages = append(adaptedMessages, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "role", Value: canonical.NewString("tool")},
				canonical.ObjectPair{Key: "tool_call_id", Value: canonical.NewString(part.Lookup("tool_use_id").StringValue())},
				canonical.ObjectPair{Key: "content", Value: canonical.NewString(ToolResultText(part.Lookup("content")))},
			))
		}
		flushPending()
		return adaptedMessages
	}

	adapted := message.Clone()
	adapted.SetKey("role", canonical.NewString(role))
	adapted.SetKey("content", requestAdaptContent(content))
	return []*canonical.Value{adapted}
}

// requestHasToolResultPart 复刻 `any(... for part in content)`。
func requestHasToolResultPart(content *canonical.Value) bool {
	for _, part := range content.Items() {
		if part.IsObject() && requestStringField(part, "type") == "tool_result" {
			return true
		}
	}
	return false
}

// --- 工具与 tool_choice ---

// requestAdaptToolList 过滤并改写工具定义列表。
//
// 对应 `[x for tool in tools if isinstance(tool, dict) and (x := _adapt_anthropic_tool(tool)) is not None]`：
// 非字典项被静默丢弃，`_adapt_anthropic_tool` 返回 None 的也被丢弃。结果即便为空
// 也会写回成 `[]`（不是删键）。
func requestAdaptToolList(tools *canonical.Value) *canonical.Value {
	filtered := []*canonical.Value{}
	for _, tool := range tools.Items() {
		if !tool.IsObject() {
			continue
		}
		adaptedTool, ok := requestAdaptAnthropicTool(tool)
		if !ok {
			continue
		}
		filtered = append(filtered, adaptedTool)
	}
	return canonical.NewArray(filtered...)
}

// requestAdaptAnthropicTool 把一条工具定义改成 OpenAI function calling 形态。
//
// 移植 request.py:246。第二个返回值对应 Python 的「返回 None」，表示该工具不支持、
// 应被过滤掉。
//
// 三处判据的分寸要拿准：
//   - `tool_type is not None and tool_type != "function"`：只把**字面 null** 当作
//     「没写 type」，所以 `"type": ""` 会被过滤掉（`"" != "function"`）；
//   - 已是 OpenAI 形态且 function.name 非空时**原样返回同一个对象**（参照实现
//     也是 `return tool`，不做拷贝）；
//   - function 存在但缺 name 时补上顶层的 name，补进去的键追加在 func 末尾。
func requestAdaptAnthropicTool(tool *canonical.Value) (*canonical.Value, bool) {
	toolType, hasToolType := tool.LookupOK("type")
	if hasToolType && !toolType.IsNull() && !requestIsFunctionType(toolType) {
		return nil, false
	}
	if requestIsFunctionType(toolType) {
		if function := tool.Lookup("function"); function.IsObject() {
			if name := function.Lookup("name"); name.Truthy() {
				return tool, true
			}
			adaptedFunction := function.Clone()
			adaptedFunction.SetKey("name", canonical.NewString(tool.Lookup("name").StringValue()))
			return requestFunctionTool(adaptedFunction), true
		}
	}
	function := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "name", Value: canonical.NewString(tool.Lookup("name").StringValue())},
	)
	// `is not None`：description 为空串时**照样**写成空串，不能真值判断。
	if description, ok := tool.LookupOK("description"); ok && !description.IsNull() {
		function.SetKey("description", canonical.NewString(description.PyStr()))
	}
	// `tool.get("input_schema", tool.get("parameters"))`：只按 input_schema 键
	// 是否存在回退，`input_schema: null` 不会回退到 parameters。
	parameters, hasSchema := tool.LookupOK("input_schema")
	if !hasSchema {
		parameters = tool.Lookup("parameters")
	}
	if parameters.IsObject() {
		function.SetKey("parameters", parameters)
	} else {
		function.SetKey("parameters", canonical.NewObjectOf(
			canonical.ObjectPair{Key: "type", Value: canonical.NewString("object")},
			canonical.ObjectPair{Key: "properties", Value: canonical.NewObject()},
		))
	}
	return requestFunctionTool(function), true
}

// requestIsFunctionType 报告 type 字段是否字面等于 "function"。
func requestIsFunctionType(value *canonical.Value) bool {
	return value.IsString() && value.Str == "function"
}

// requestFunctionTool 包装成 OpenAI 的 `{"type":"function","function":…}`。
func requestFunctionTool(function *canonical.Value) *canonical.Value {
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("function")},
		canonical.ObjectPair{Key: "function", Value: function},
	)
}

// requestAdaptAnthropicToolChoice 把 Anthropic 的 tool_choice 翻译成 OpenAI 形态。
//
// 移植 request.py:278。`any` → 字符串 `"required"`，`auto` / `none` → 同名字符串，
// `tool` → `{"type":"function","function":{"name":…}}`，其余原样返回。
func requestAdaptAnthropicToolChoice(toolChoice *canonical.Value) *canonical.Value {
	switch requestStringField(toolChoice, "type") {
	case "any":
		return canonical.NewString("required")
	case "auto", "none":
		return canonical.NewString(requestStringField(toolChoice, "type"))
	case "tool":
		return requestFunctionTool(canonical.NewObjectOf(
			canonical.ObjectPair{Key: "name", Value: canonical.NewString(toolChoice.Lookup("name").StringValue())},
		))
	}
	return toolChoice
}

// --- 内容块适配（common.py 的 _adapt_content / _adapt_content_part）---

// 这两个函数在参照实现里位于 common.py:32/42，但只有 request.py 用到，因此随
// 本模块一起移植。命名加 `request` 前缀是为了与同包内并发移植的 anthropic.go
// 隔离，避免同名符号冲突。

// requestAdaptContent 把任意 content 归一成 chat 上游能吃的形态。
//
// 字符串原样返回；数组逐项过 requestAdaptContentPart；字典包成单元素数组；
// 其余（数字、布尔、None）一律 `str()` 成字符串。注意 `str(None)` 是 "None"
// 而不是空串，这与 ToolResultText / MessageText 的 None 处理**不同**。
func requestAdaptContent(content *canonical.Value) *canonical.Value {
	switch {
	case content.IsString():
		return content
	case content.IsArray():
		parts := make([]*canonical.Value, 0, content.Len())
		for _, part := range content.Items() {
			parts = append(parts, requestAdaptContentPart(part))
		}
		return canonical.NewArray(parts...)
	case content.IsObject():
		return canonical.NewArray(requestAdaptContentPart(content))
	default:
		return canonical.NewString(content.PyStr())
	}
}

// requestAdaptContentPart 适配单个内容块。
//
// 移植 common.py:42。分支顺序即优先级，最容易被忽略的是最后两条兜底：
//   - `part_type` 恰好是 `text` / `image_url` 时**原样返回该块**（连多余字段都保留）；
//   - 都不匹配时先看 text 字段（`is not None`，空串也算命中），仍然没有才把整个
//     块 JSON 序列化成 text——这条兜底保证未知块不会被静默丢掉。
//
// 已知与参照实现的一处不可达差异：Python 用 `part_type in {"text","image_url"}`
// 做集合成员判断，若 type 是数组/对象这类不可哈希值会抛 TypeError；这里退化为
// 字符串比较后落到兜底分支。该输入不构成合法 content，故不做复刻。
func requestAdaptContentPart(part *canonical.Value) *canonical.Value {
	if !part.IsObject() {
		return requestTextPart(part.PyStr())
	}
	partType := requestStringField(part, "type")
	switch partType {
	case "text", "image_url":
		return part
	case "input_text", "output_text":
		return requestTextPart(part.Lookup("text").StringValue())
	case "image":
		if source := part.Lookup("source"); source.IsObject() && requestStringField(source, "type") == "base64" {
			mediaType := source.Lookup("media_type").StringValue()
			if mediaType == "" {
				mediaType = "image/png"
			}
			// f-string 拼接：data 为空时也要留下 `data:…;base64,` 前缀。
			url := "data:" + mediaType + ";base64," + source.Lookup("data").StringValue()
			return requestImageURLPart(url)
		}
	case "input_image":
		// `part.get("image_url") or part.get("file_id") or ""`，再 str()。
		imageURL := part.Lookup("image_url").StringValue()
		if imageURL == "" {
			imageURL = part.Lookup("file_id").StringValue()
		}
		return requestImageURLPart(imageURL)
	}
	if text, ok := part.LookupOK("text"); ok && !text.IsNull() {
		return requestTextPart(text.PyStr())
	}
	return requestTextPart(canonical.DumpsOrdered(part))
}

// requestTextPart 构造 `{"type":"text","text":…}`。
func requestTextPart(text string) *canonical.Value {
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("text")},
		canonical.ObjectPair{Key: "text", Value: canonical.NewString(text)},
	)
}

// requestImageURLPart 构造 `{"type":"image_url","image_url":{"url":…}}`。
func requestImageURLPart(url string) *canonical.Value {
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("image_url")},
		canonical.ObjectPair{Key: "image_url", Value: canonical.NewObjectOf(
			canonical.ObjectPair{Key: "url", Value: canonical.NewString(url)},
		)},
	)
}

// --- 小工具 ---

// requestPop 取出并删除一个键，同时报告键是否存在。
func requestPop(obj *canonical.Value, key string) (*canonical.Value, bool) {
	value, ok := obj.LookupOK(key)
	if !ok {
		return nil, false
	}
	obj.DeleteKey(key)
	return value, true
}

// requestHasKey 报告对象里是否存在该键（与值是否为真无关）。
func requestHasKey(obj *canonical.Value, key string) bool {
	_, ok := obj.LookupOK(key)
	return ok
}

// requestStringField 取字符串字段；非字符串（含 None）返回空串。
//
// 对应参照实现里那些只与字符串字面量比较的判据，例如
// `choice_type in {"auto","none"}`：非字符串一律不匹配，而不是转成文本再比。
func requestStringField(obj *canonical.Value, key string) string {
	if value := obj.Lookup(key); value.IsString() {
		return value.Str
	}
	return ""
}
