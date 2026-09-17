package proxy

import (
	"net/http"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/protocol"
)

// 本文件是三个流式转发状态机。它们共同的结构是：pumpUpstream 负责超时、观察
// 首字节、收尾；每个 writer 只负责「一块上游数据 → 若干下游字节」。
//
// 三处与参照实现一致的可观测行为，改掉任何一条都会改变下游看到的字节：
//
//  1. **每个事件后都 Flush**（参照实现是 `await asyncio.sleep(0)`，
//     proxy_handler.py:1130）。Go 里对应 http.Flusher。
//  2. **未完成的事件不外发**：SSE 路径只在下游攒够一个完整事件（`\n\n` 或
//     `\r\n\r\n`）时才写；流结束时残余的**未完成事件被丢弃**，但非 SSE 的残余会
//     原样吐出（proxy_handler.py:1133）。
//  3. `first_token_ms` 记录的是**上游首字节**时间，由 pumpUpstream 在转发前
//     ObserveChunk 决定，不在这里。

// streamWriter 是三种流式转发的统一接口。
type streamWriter interface {
	// begin 在下游开始读取上游之前产出「开场事件」。
	//
	// 只有 Anthropic 重建流需要它：message_start 必须在**任何**上游字节之前发出
	// （proxy_handler.py:1217），否则首块超时这类失败路径留下的下游字节会与参照
	// 实现不同。其余两种是空操作。
	begin(w http.ResponseWriter)
	// push 处理一块上游数据并转发；返回 false 表示应终止（下游写失败或转换失败）。
	push(w http.ResponseWriter, chunk []byte) bool
	// flush 处理流**正常结束**时的残余缓冲与收尾事件。
	flush(w http.ResponseWriter)
	// abort 处理流因错误终止时的残余。
	//
	// 两者的区别是参照实现里一个有、一个没有的分支：_stream_upstream 在 except
	// 里也把 SSE 残余吐出去（proxy_handler.py:1138），而两个转换流的收尾事件写在
	// try 内、async for 之后（proxy_handler.py:1247、1359），一旦循环抛错就被整段
	// 跳过——不会补 content_block_stop / message_delta / message_stop。
	abort(w http.ResponseWriter)
}

// rawStream 是「字节级原样转发（SSE 时按事件边界切分）」的流式处理器。
//
// 移植 proxy_handler.py:1081 的 _stream_upstream。
type rawStream struct {
	isSSE    bool
	splitter *sseEventSplitter
}

// newRawStream 构造原样转发流。
func newRawStream(_ *streamLifecycle, mediaType string, _ func() time.Time) *rawStream {
	stream := &rawStream{isSSE: isSSEMediaType(mediaType)}
	if stream.isSSE {
		stream.splitter = &sseEventSplitter{}
	}
	return stream
}

func (s *rawStream) begin(_ http.ResponseWriter) {}

// abort 在错误路径上同样外发残余缓冲（对齐 proxy_handler.py:1138）。
func (s *rawStream) abort(w http.ResponseWriter) { s.flush(w) }

// push 转发一块：SSE 路径按事件切分，其余原样吐出。
func (s *rawStream) push(w http.ResponseWriter, chunk []byte) bool {
	if s.isSSE {
		for _, event := range s.splitter.push(chunk) {
			if !writeBytes(w, event) {
				return false
			}
		}
		return true
	}
	return writeBytes(w, chunk)
}

// flush 处理流结束时的残余。
//
// 参照实现只对 SSE 路径保留残余并外发（proxy_handler.py:1133）；非 SSE 路径的
// 数据在 push 里已经全部吐出，没有残余。注意这里外发的是**未完成事件**（可能不含
// `\n\n`），与「一个完整 SSE 事件」不同——参照实现就是这么写的。
func (s *rawStream) flush(w http.ResponseWriter) {
	if s.splitter == nil || len(s.splitter.pending()) == 0 {
		return
	}
	_ = writeBytes(w, s.splitter.pending())
	s.splitter.clear()
}

// anthropicStream 把上游 OpenAI SSE 重建为 Anthropic Messages SSE。
//
// 移植 proxy_handler.py:1175 的 _stream_anthropic_messages。事件切分与 usage 提取
// 都交给 protocol.AnthropicStreamState（它内部用字节级 SSESplitter，顺带修掉了
// 参照实现跨分片多字节 UTF-8 变 U+FFFD 的缺陷）。
type anthropicStream struct {
	lifecycle *streamLifecycle
	state     *protocol.AnthropicStreamState
}

// newAnthropicStream 构造 Anthropic 重建流。
func newAnthropicStream(lifecycle *streamLifecycle, _ func() time.Time) *anthropicStream {
	return &anthropicStream{
		lifecycle: lifecycle,
		state:     protocol.NewAnthropicStreamState(),
	}
}

// push 处理一块上游数据。
// begin 发出 message_start。
//
// 参照实现在读取任何上游字节**之前**就 yield 了 message_start
// （proxy_handler.py:1217），因此「首块超时」这类失败也已经在下游留下了一个
// message_start——这个顺序是可观测的，不能挪到读到首块之后再发。
// abort 不做任何事：转换流的收尾事件在参照实现里位于 try 内，异常会整段跳过。
func (s *anthropicStream) abort(_ http.ResponseWriter) {}

func (s *anthropicStream) begin(w http.ResponseWriter) {
	_ = writeSSE(w, "message_start", anthropicMessageStart(s.lifecycle.requestedModelID))
}

// push 处理一块上游数据。
func (s *anthropicStream) push(w http.ResponseWriter, chunk []byte) bool {
	events, usage := s.state.Push(chunk)
	s.absorbUsage(usage)
	for _, event := range events {
		if !writeBytes(w, event) {
			return false
		}
	}
	return true
}

// absorbUsage 合并本块解析出的 usage。
func (s *anthropicStream) absorbUsage(usage *canonical.Value) {
	if usage == nil {
		return
	}
	s.lifecycle.SetUsage(mergeUsage(s.lifecycle.Usage(), usage))
}

// flush 处理残余行并发出收尾事件。
//
// 移植 proxy_handler.py:1247-1298，顺序不可交换：
//  1. 残余行（补一个换行再解析）；
//  2. 文本块未收尾则补 content_block_stop；
//  3. 工具块收尾（FinishTools）；
//  4. **一个 content 块都没有**时补一对空文本块的 start+stop——否则 Anthropic
//     客户端拿到的是一个没有任何 content 的消息；
//  5. message_delta（stop_reason 与 usage）+ message_stop。
func (s *anthropicStream) flush(w http.ResponseWriter) {
	events, usage := s.state.Finish()
	s.absorbUsage(usage)
	for _, event := range events {
		if !writeBytes(w, event) {
			return
		}
	}

	state := s.state
	if state.TextIndex != nil && !state.TextStopped {
		if !writeSSE(w, "content_block_stop", anthropicContentBlockStop(*state.TextIndex)) {
			return
		}
		state.TextStopped = true
	}
	for _, event := range state.FinishTools() {
		if !writeBytes(w, event) {
			return
		}
	}
	if state.NextContentIndex == 0 {
		if !writeSSE(w, "content_block_start", anthropicEmptyTextBlock()) {
			return
		}
		if !writeSSE(w, "content_block_stop", anthropicContentBlockStop(0)) {
			return
		}
	}
	stopReason := state.StopReason
	if stopReason == "" {
		if len(state.ToolCalls) > 0 {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	_ = writeSSE(w, "message_delta",
		anthropicMessageDelta(stopReason, s.lifecycle.Usage()))
	_ = writeSSE(w, "message_stop", anthropicMessageStop())
}

// responsesStream 把上游 OpenAI SSE 重建为 Responses SSE。
//
// 移植 proxy_handler.py:1310 的 _stream_responses。
type responsesStream struct {
	lifecycle *streamLifecycle
	state     *protocol.ResponsesStreamState
}

// newResponsesStream 构造 Responses 重建流。
func newResponsesStream(lifecycle *streamLifecycle, _ func() time.Time) *responsesStream {
	return &responsesStream{
		lifecycle: lifecycle,
		state:     protocol.NewResponsesStreamState(),
	}
}

// push 处理一块上游数据。
// abort 不做任何事：见 anthropicStream.abort。
func (s *responsesStream) abort(_ http.ResponseWriter) {}

func (s *responsesStream) begin(_ http.ResponseWriter) {}

// push 处理一块上游数据。
func (s *responsesStream) push(w http.ResponseWriter, chunk []byte) bool {
	events, usage, err := s.state.Push(chunk)
	if err != nil {
		// ResponsesUsage 的转换错误（无法转成数字的 usage 字段）在参照实现里会从
		// generator 冒泡，被 except 捕获后**静默截断整条流**。这里用 push 返回
		// false 表达同一结果（pump 会记 failed 并写日志）。
		return false
	}
	s.absorbUsage(usage)
	for _, event := range events {
		if !writeBytes(w, event) {
			return false
		}
	}
	return true
}

// absorbUsage 合并本块解析出的 usage。
func (s *responsesStream) absorbUsage(usage *canonical.Value) {
	if usage == nil {
		return
	}
	s.lifecycle.SetUsage(mergeUsage(s.lifecycle.Usage(), usage))
}

// flush 处理残余行并发出收尾事件。
//
// 移植 proxy_handler.py:1359-1391：
//  1. 残余行（补一个换行再解析）；
//  2. 每个累积的 output item 一个 response.output_item.done；
//  3. response.completed（带 id / model / usage）。
func (s *responsesStream) flush(w http.ResponseWriter) {
	events, usage, err := s.state.Flush()
	if err != nil {
		// 与 push 里的转换错误同样处理：静默截断，不再产出收尾事件。
		return
	}
	s.absorbUsage(usage)
	for _, event := range events {
		if !writeBytes(w, event) {
			return
		}
	}
	outputItems := s.state.OutputItems()
	if outputItems.IsArray() {
		for index, item := range outputItems.Items() {
			if !writeSSE(w, "response.output_item.done",
				responsesOutputItemDone(int64(index), item)) {
				return
			}
		}
	}
	completed, usageErr := responsesCompleted(s.lifecycle.requestedModelID, s.lifecycle.Usage())
	if usageErr != nil {
		return
	}
	_ = writeSSE(w, "response.completed", completed)
}

// --- SSE 载荷构造 ---
//
// 每个构造函数都严格按参照实现里 dict 字面量的**键顺序**插入：canonical 的
// DumpsOrdered 按插入顺序输出，而事件字节是下游可解析的契约。

// anthropicMessageStart 构造 message_start 载荷。
//
// 移植 proxy_handler.py:1217。id 硬编码为 msg_amkr、content 为空数组、usage 全 0；
// model 用**请求的**模型 ID（而非解析后的真实模型），这是客户端看到的模型名。
func anthropicMessageStart(requestedModelID string) *canonical.Value {
	message := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "id", Value: canonical.NewString("msg_amkr")},
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("message")},
		canonical.ObjectPair{Key: "role", Value: canonical.NewString("assistant")},
		canonical.ObjectPair{Key: "model", Value: canonical.NewString(requestedModelID)},
		canonical.ObjectPair{Key: "content", Value: canonical.NewArray()},
		canonical.ObjectPair{Key: "stop_reason", Value: canonical.NewNull()},
		canonical.ObjectPair{Key: "stop_sequence", Value: canonical.NewNull()},
		canonical.ObjectPair{Key: "usage", Value: canonical.NewObjectOf(
			canonical.ObjectPair{Key: "input_tokens", Value: canonical.NewIntValue(0)},
			canonical.ObjectPair{Key: "output_tokens", Value: canonical.NewIntValue(0)},
		)},
	)
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("message_start")},
		canonical.ObjectPair{Key: "message", Value: message},
	)
}

// anthropicContentBlockStop 构造 content_block_stop 载荷。'
//
// 移植 proxy_handler.py:1259、1281。
func anthropicContentBlockStop(index int64) *canonical.Value {
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("content_block_stop")},
		canonical.ObjectPair{Key: "index", Value: canonical.NewIntValue(index)},
	)
}

// anthropicEmptyTextBlock 构造「一个块都没有」时补的空文本块 start 载荷。
//
// 移植 proxy_handler.py:1272。
func anthropicEmptyTextBlock() *canonical.Value {
	block := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("text")},
		canonical.ObjectPair{Key: "text", Value: canonical.NewString("")},
	)
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("content_block_start")},
		canonical.ObjectPair{Key: "index", Value: canonical.NewIntValue(0)},
		canonical.ObjectPair{Key: "content_block", Value: block},
	)
}

// anthropicMessageDelta 构造 message_delta 载荷。
//
// 移植 proxy_handler.py:1289。delta 在前、usage 在后。
func anthropicMessageDelta(stopReason string, usage *canonical.Value) *canonical.Value {
	delta := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "stop_reason", Value: canonical.NewString(stopReason)},
		canonical.ObjectPair{Key: "stop_sequence", Value: canonical.NewNull()},
	)
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("message_delta")},
		canonical.ObjectPair{Key: "delta", Value: delta},
		canonical.ObjectPair{Key: "usage", Value: protocol.AnthropicStreamUsage(usage)},
	)
}

// anthropicMessageStop 构造 message_stop 载荷。
func anthropicMessageStop() *canonical.Value {
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("message_stop")},
	)
}

// responsesOutputItemDone 构造 response.output_item.done 载荷。
//
// 移植 proxy_handler.py:1372。item 直接复用 OutputItems 产出的对象（不复制），
// 与参照实现一致。
func responsesOutputItemDone(index int64, item *canonical.Value) *canonical.Value {
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("response.output_item.done")},
		canonical.ObjectPair{Key: "output_index", Value: canonical.NewIntValue(index)},
		canonical.ObjectPair{Key: "item", Value: item},
	)
}

// responsesCompleted 构造 response.completed 载荷。
//
// 移植 proxy_handler.py:1381。id 与 model 是硬编码 / 请求模型名。
func responsesCompleted(requestedModelID string, usage *canonical.Value) (*canonical.Value, error) {
	normalized, err := protocol.ResponsesUsage(usage)
	if err != nil {
		return nil, err
	}
	response := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "id", Value: canonical.NewString("resp_amkr")},
		canonical.ObjectPair{Key: "model", Value: canonical.NewString(requestedModelID)},
		canonical.ObjectPair{Key: "usage", Value: normalized},
	)
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "type", Value: canonical.NewString("response.completed")},
		canonical.ObjectPair{Key: "response", Value: response},
	), nil
}

// writeBytes 写一块字节并刷出。
//
// 写失败返回 false：这意味着下游已经断开，继续读上游只是浪费（参照实现里
// generator 被关闭会抛 GeneratorExit/CancelledError）。
func writeBytes(w http.ResponseWriter, chunk []byte) bool {
	if _, err := w.Write(chunk); err != nil {
		return false
	}
	flush(w)
	return true
}

// writeSSE 编码一个 SSE 事件并写出。
func writeSSE(w http.ResponseWriter, event string, payload *canonical.Value) bool {
	return writeBytes(w, protocol.EncodeSSE(event, payload))
}

// flush 在每一块之后显式刷出。
//
// 参照实现是 `await asyncio.sleep(0)`（proxy_handler.py:1130），让出事件循环以便
// ASGI 把已产出的字节发出去；Go 没有对应物，等价动作是 http.Flusher.Flush
// （迁移方案 §Phase 3 明确要求「每事件显式 Flush」）。
func flush(w http.ResponseWriter) {
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

// mergeUsage 把新解析到的 usage 合并进已有值。
//
// 移植 proxy_handler.py:1030：`merged = dict(current); merged.update(update)`。
// 用 canonical 的 SetKey 复刻 dict 的「覆盖不移动位置、新键追加到末尾」规则——
// 键顺序会体现在最终 message_delta / response.completed 的 usage 里，属可观测行为。
func mergeUsage(current, update *canonical.Value) *canonical.Value {
	if update == nil {
		return current
	}
	if current == nil || !current.IsObject() {
		return update.Clone()
	}
	merged := current.Clone()
	if update.IsObject() {
		for _, key := range update.Obj.Keys() {
			value, _ := update.Obj.Get(key)
			merged.Obj.Set(key, value)
		}
	}
	return merged
}
