package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
)

// 本文件是测试用的假上游与假指标接收器。
//
// 它们原先住在对拍语料文件里，语料退役后只保留被测用例真正需要的那部分能力。

// upstreamStep 是一条脚本化的上游响应。
type upstreamStep struct {
	Status  int
	Headers map[string]string
	Body    string
	Chunks  []string
	Fail    bool
	// Reader 用来表达「块间有延迟」这类时序场景。
	Reader io.Reader
}

// recordedUpstream 是一次已发生的上游请求。
type recordedUpstream struct {
	method  string
	path    string
	query   string
	headers map[string]string
	body    string
}

// describeUpstreams 把调用记录渲染成诊断文本。
func describeUpstreams(calls []recordedUpstream) []string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, call.method+" "+call.path)
	}
	return parts
}

// ignoredRequestHeader 是不参与断言的请求头（由传输层生成）。
var ignoredRequestHeader = map[string]bool{
	"user-agent":     true,
	"content-length": true,
}

// scriptedTransport 按路径消费脚本化响应。
//
// 未脚本化的路径返回 500 —— 这样「路由到错误的端点」会在状态码上暴露，而不是被
// 当成一次正常的上游成功。
type scriptedTransport struct {
	mu     sync.Mutex
	routes map[string][]upstreamStep
	calls  []recordedUpstream
}

// newScriptedTransport 从路由表构造假上游。
func newScriptedTransport(routes map[string][]upstreamStep) *scriptedTransport {
	copied := map[string][]upstreamStep{}
	for path, steps := range routes {
		copied[path] = append([]upstreamStep(nil), steps...)
	}
	return &scriptedTransport{routes: copied}
}

// stubUpstreamClient 把 scriptedTransport 适配成本包的 UpstreamClient。
//
// 它直接构造 *http.Response，绕过 net/http 的传输层：这让我们能精确控制上游的
// 响应头、状态码与 body（包括「连接直接失败」）。
type stubUpstreamClient struct {
	transport *scriptedTransport
}

// Do 处理非流式上游请求。
func (c *stubUpstreamClient) Do(req *http.Request) (*http.Response, error) {
	return c.transport.roundTrip(req)
}

// DoStream 处理流式上游请求。
//
// 与 Do 走同一条路径：脚本里已经用 Reader 表达了「分块到达」，而分块之间的空闲
// 超时由 runtime 在读取侧计时，不需要传输层参与。
func (c *stubUpstreamClient) DoStream(req *http.Request) (*http.Response, error) {
	return c.transport.roundTrip(req)
}

// CloseIdleConnections 满足 runtime 的 HTTPClient 接缝。
func (c *stubUpstreamClient) CloseIdleConnections() {}

// roundTrip 记录请求并返回脚本化的响应。
func (s *scriptedTransport) roundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	headers := map[string]string{}
	for key, values := range req.Header {
		lower := strings.ToLower(key)
		if ignoredRequestHeader[lower] {
			continue
		}
		headers[lower] = strings.Join(values, ", ")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedUpstream{
		method:  req.Method,
		path:    req.URL.Path,
		query:   req.URL.RawQuery,
		headers: headers,
		body:    string(body),
	})
	queue := s.routes[req.URL.Path]
	if len(queue) == 0 {
		return jsonUpstreamResponse(http.StatusInternalServerError,
			fmt.Sprintf(`{"error":{"message":"未脚本化的上游路径: %s"}}`,
				req.URL.Path)), nil
	}
	step := queue[0]
	if len(queue) > 1 {
		s.routes[req.URL.Path] = queue[1:]
	}
	if step.Fail {
		// 用真实的拨号失败（ECONNREFUSED）而不是裸 errors.New：Go 侧的错误名由
		// upstream.Classify 折算（它把所有连接类失败归为 CauseConnection），用裸
		// 错误会落到**另一条**分支，让「连接失败」的用例失去意义。
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	payload := step.Body
	if step.Chunks != nil {
		payload = strings.Join(step.Chunks, "")
	}
	var reader io.Reader = strings.NewReader(payload)
	if step.Reader != nil {
		reader = step.Reader
	}
	return &http.Response{
		StatusCode: step.Status,
		Header:     headerPairs(step.Headers),
		Body:       io.NopCloser(reader),
	}, nil
}

// jsonUpstreamResponse 构造一个 JSON 上游响应。
func jsonUpstreamResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// headerPairs 把响应头表折成 http.Header。
func headerPairs(headers map[string]string) http.Header {
	result := http.Header{}
	for key, value := range headers {
		result.Set(key, value)
	}
	return result
}

// recordingMetrics 是本包 MetricsSink 的假实现。
//
// 它把「指标行数与失败标记」变成可断言的记录。
type recordingMetrics struct {
	mu      sync.Mutex
	records []MetricRecord
}

// Record 记录一行指标。
func (m *recordingMetrics) Record(_ context.Context, record MetricRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, record)
	return nil
}

// Count 返回已记录的行数。
func (m *recordingMetrics) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}

// take 返回并清空已记录的行。
func (m *recordingMetrics) take() []MetricRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := m.records
	m.records = nil
	return result
}

// discardLogger 返回一个丢弃全部输出的 logger，避免测试噪音。
//
// 阈值设在 Error 之上：大量用例故意触发 Warn / Error 分支。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
}
