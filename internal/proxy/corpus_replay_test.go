package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/keypool"
	"github.com/Sparrived/auto-model-key-router/internal/runtime"
)

// corpusCase 是 scripts/gen_proxy_handler_corpus.py 产出的一条对拍用例。
type corpusCase struct {
	Name          string                    `json:"name"`
	Note          string                    `json:"note"`
	Status        int                       `json:"status"`
	Headers       map[string]string         `json:"headers"`
	Body          string                    `json:"body"`
	Upstream      map[string][]upstreamStep `json:"upstream"`
	UpstreamCalls []corpusUpstreamCall      `json:"upstream_calls"`
	Config        corpusConfig              `json:"config"`
	Request       corpusRequest             `json:"request"`
}

// corpusUpstreamCall 是一次**实际发生**的上游调用。
type corpusUpstreamCall struct {
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Query          string            `json:"query"`
	Authorization  string            `json:"authorization"`
	AcceptEncoding string            `json:"accept_encoding"`
	ContentType    string            `json:"content_type"`
	Headers        map[string]string `json:"headers"`
	Body           string            `json:"body"`
}

// upstreamStep 是一条脚本化的上游响应。
type upstreamStep struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Chunks  []string          `json:"chunks"`
	Fail    bool              `json:"error"`
	// BreakAfter 非 nil 时表示「先产出前 N 块、然后上游中途断开」。
	BreakAfter *int `json:"break_after"`
	// Reader 只在测试里设置：用来表达「块间有延迟」这类时序场景（语料无法表达）。
	Reader io.Reader `json:"-"`
}

// corpusRequest 是下游请求。
type corpusRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	NoAuth  bool              `json:"no_auth"`
}

// corpusConfig 是紧凑配置描述（字段与 Python RouterConfig 同名）。
type corpusConfig struct {
	Models                 []corpusModel                `json:"models"`
	UnifiedModel           *corpusUnified               `json:"unified_model"`
	Tasks                  []corpusTask                 `json:"tasks"`
	UpstreamRoutes         map[string]map[string]string `json:"upstream_routes"`
	MaxRetries             *int                         `json:"max_retries"`
	KeyFailureThreshold    *int                         `json:"key_failure_threshold"`
	KeyCooldownSeconds     *float64                     `json:"key_cooldown_seconds"`
	StreamFirstByteTimeout *float64                     `json:"stream_first_byte_timeout"`
	StreamIdleTimeout      *float64                     `json:"stream_idle_timeout"`
	LocalAPIKey            *string                      `json:"local_api_key"`
}

type corpusModel struct {
	ID              string      `json:"id"`
	Keys            []corpusKey `json:"keys"`
	Aliases         []string    `json:"aliases"`
	RoutingMode     string      `json:"routing_mode"`
	ReasoningEffort *string     `json:"reasoning_effort"`
	NativeFirst     bool        `json:"native_first"`
	HiddenAliases   []string    `json:"hidden_aliases"`
}

type corpusKey struct {
	Name          string  `json:"name"`
	APIKey        string  `json:"api_key"`
	BaseURL       string  `json:"base_url"`
	Enabled       *bool   `json:"enabled"`
	AllowVisitor  bool    `json:"allow_visitor"`
	UpstreamModel string  `json:"upstream_model"`
	Provider      *string `json:"provider"`
}

type corpusUnified struct {
	Default    corpusPlan  `json:"default"`
	Image      *corpusPlan `json:"image"`
	Embeddings *corpusPlan `json:"embeddings"`
}

type corpusPlan struct {
	Primary  corpusTarget  `json:"primary"`
	Fallback *corpusTarget `json:"fallback"`
}

type corpusTarget struct {
	Model string  `json:"model"`
	Key   *string `json:"key"`
}

type corpusTask struct {
	Name          string          `json:"name"`
	Model         string          `json:"model"`
	FallbackModel *string         `json:"fallback_model"`
	Params        json.RawMessage `json:"params"`
}

// loadCorpus 读取对拍语料。
func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()
	path := filepath.Join("testdata", "handler.jsonl")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开语料失败: %v", err)
	}
	defer func() { _ = file.Close() }()

	var cases []corpusCase
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item corpusCase
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatalf("解析语料失败: %v", err)
		}
		cases = append(cases, item)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("语料为空")
	}
	return cases
}

// TestCorpusReplay 用真实 keypool + 假上游回放全部对拍用例。
//
// 断言四层：下游状态码、下游响应体**逐字节**、下游响应头、以及上游请求的完整
// 序列（方法 / 路径 / query / 请求头 / 请求体）。最后一层是对拍价值最高的部分：
// 它把「路由到哪个上游端点、请求体怎么改写、头部怎么净化、回退发生了几次」都变成
// 可断言的事实，而不只是最终响应凑巧一致。
func TestCorpusReplay(t *testing.T) {
	cases := loadCorpus(t)
	for _, item := range cases {
		item := item
		t.Run(item.Name, func(t *testing.T) {
			t.Parallel()
			runCorpusCase(t, item)
		})
	}
}

func runCorpusCase(t *testing.T, item corpusCase) {
	t.Helper()
	cfg := routerConfigFromCorpus(item.Config)
	pool := keypool.New(cfg, nil, nil)
	transport := newScriptedTransport(item.Upstream)
	client := &stubUpstreamClient{transport: transport}

	resources := runtime.NewRuntimeResources(cfg, pool, nil, nil)
	resources.HTTPClient = client
	manager := runtime.NewRuntimeManager(resources)
	handler := New(manager, &recordingMetrics{}, Options{
		// 语料是参照实现的产物，因此对拍必须走**参照档**：畸形体静默变 {}、
		// multipart 被当 JSON 解析。产品决策的默认档由
		// TestMalformedBodyIsRejected / TestMultipartPolicy 单独覆盖。
		BodyPolicy:                 BodyPolicyPython,
		Multipart:                  MultipartPython,
		MaxUpstreamCallsPerRequest: 10_000,
		Logger:                     discardLogger(),
	})

	// 语料的 path 是完整的下游路径（含 /v1/ 前缀），而 Handle 收的是路由参数
	// `{path}`（不含前缀）——这与 Starlette 的 /v1/{path:path} 一致。
	routePath := strings.TrimPrefix(item.Request.Path, "/v1/")
	target := "http://testserver" + item.Request.Path
	if item.Request.Query != "" {
		target += "?" + item.Request.Query
	}
	req := httptest.NewRequest(item.Request.Method,
		target, strings.NewReader(item.Request.Body))
	for header, value := range item.Request.Headers {
		req.Header.Set(header, value)
	}
	if !item.Request.NoAuth && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+cfg.LocalAPIKey)
	}
	recorder := httptest.NewRecorder()
	handler.Handle(recorder, req, routePath)

	if recorder.Code != item.Status {
		t.Fatalf("状态码不一致: got %d want %d\nbody=%s\nnote=%s",
			recorder.Code, item.Status, recorder.Body.String(), item.Note)
	}
	if got := recorder.Body.String(); got != item.Body {
		t.Fatalf("响应体不一致:\n got=%q\nwant=%q\nnote=%s", got, item.Body, item.Note)
	}
	assertHeaders(t, recorder.Result().Header, item.Headers)
	assertUpstreams(t, transport.calls, item.UpstreamCalls)
}

// assertHeaders 比较下游响应头。
//
// 一处规范化，且是**已记录的差异**而不是掩盖：media type 的 `charset` 参数。
// Starlette 1.2 只在 Response（非 StreamingResponse）上按 `text/*` 补 charset，
// 因此 SSE 响应是裸的 `text/event-stream`；Go 侧对所有 text/* 都补
// `; charset=utf-8`。SSE 规范本就要求 UTF-8，这个参数不影响任何客户端。
func assertHeaders(t *testing.T, got http.Header, want map[string]string) {
	t.Helper()
	normalized := map[string]string{}
	for key, values := range got {
		lower := strings.ToLower(key)
		if ignoredHeader[lower] || len(values) == 0 {
			continue
		}
		normalized[lower] = strings.TrimSpace(strings.Join(values, ", "))
	}
	for key, value := range want {
		if ignoredHeader[key] {
			continue
		}
		gotValue, ok := normalized[key]
		if !ok {
			t.Fatalf("缺少响应头 %s（实得 %v）", key, normalized)
		}
		if gotValue != value && !equalMediaType(gotValue, value) {
			t.Fatalf("响应头 %s 不一致: got %q want %q", key, gotValue, value)
		}
	}
	for key := range normalized {
		if _, ok := want[key]; !ok {
			t.Fatalf("多出响应头 %s=%q", key, normalized[key])
		}
	}
}

// equalMediaType 比较 media type，忽略 charset 参数。
func equalMediaType(left, right string) bool {
	return stripCharset(left) == stripCharset(right)
}

// stripCharset 去掉 media type 的 charset 参数。
func stripCharset(value string) string {
	parts := strings.Split(value, ";")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "charset=") {
			continue
		}
		kept = append(kept, strings.TrimSpace(part))
	}
	return strings.Join(kept, ";")
}

// ignoredHeader 是不参与对拍的响应头。
var ignoredHeader = map[string]bool{
	"date":              true,
	"server":            true,
	"transfer-encoding": true,
	"connection":        true,
	"keep-alive":        true,
}

// assertUpstreams 逐条比较上游请求。
//
// 头部比较忽略 user-agent 与 content-length：Go 的 http.Client 会自动补
// `Go-http-client/1.1` 与长度，httpx 补的是 `python-httpx/x`；参照实现从不设置
// user-agent，这是两条实现**无法**对齐的一处（除非其中一方写死对方的 UA）。
func assertUpstreams(t *testing.T, got []recordedUpstream, want []corpusUpstreamCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("上游调用次数不一致: got %d want %d\n got=%v\nwant=%v",
			len(got), len(want), describeUpstreams(got), describeCorpusUpstreams(want))
	}
	for index := range want {
		expected := want[index]
		actual := got[index]
		if actual.method != expected.Method {
			t.Fatalf("上游 #%d 方法不一致: got %s want %s", index, actual.method, expected.Method)
		}
		if actual.path != expected.Path {
			t.Fatalf("上游 #%d 路径不一致: got %s want %s", index, actual.path, expected.Path)
		}
		if actual.query != expected.Query {
			t.Fatalf("上游 #%d query 不一致: got %q want %q", index, actual.query, expected.Query)
		}
		if actual.body != expected.Body {
			t.Fatalf("上游 #%d 请求体不一致:\n got=%q\nwant=%q", index, actual.body, expected.Body)
		}
		assertUpstreamHeaders(t, index, actual.headers, expected.Headers)
	}
}

// assertUpstreamHeaders 比较上游请求头。
func assertUpstreamHeaders(t *testing.T, index int, got, want map[string]string) {
	t.Helper()
	if want == nil {
		return
	}
	for key, value := range want {
		if ignoredRequestHeader[key] {
			continue
		}
		gotValue, ok := got[key]
		if !ok {
			t.Fatalf("上游 #%d 缺少请求头 %s（实得 %v）", index, key, got)
		}
		if gotValue != value {
			t.Fatalf("上游 #%d 请求头 %s 不一致: got %q want %q", index, key, gotValue, value)
		}
	}
	for key := range got {
		if ignoredRequestHeader[key] {
			continue
		}
		if _, ok := want[key]; !ok {
			t.Fatalf("上游 #%d 多出请求头 %s=%q", index, key, got[key])
		}
	}
}

// ignoredRequestHeader 是不参与对拍的上游请求头。
var ignoredRequestHeader = map[string]bool{
	"user-agent":     true,
	"content-length": true,
}

func describeUpstreams(calls []recordedUpstream) []string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, call.method+" "+call.path)
	}
	return parts
}

func describeCorpusUpstreams(calls []corpusUpstreamCall) []string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, call.Method+" "+call.Path)
	}
	return parts
}

// --- 假上游 ------------------------------------------------------------------

// recordedUpstream 是一次已发生的上游请求。
type recordedUpstream struct {
	method  string
	path    string
	query   string
	headers map[string]string
	body    string
}

// scriptedTransport 按路径消费脚本化响应。
//
// 未脚本化的路径返回 500 —— 与 Python 侧的行为一致，这样「路由到错误的端点」会在
// 状态码上暴露，而不是被当成一次正常的上游成功。
type scriptedTransport struct {
	mu     sync.Mutex
	routes map[string][]upstreamStep
	calls  []recordedUpstream
}

// newScriptedTransport 从语料的上游脚本构造假上游。
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
// 响应头、状态码与 body（包括「连接直接失败」与「声明了压缩但不是压缩流」）。
type stubUpstreamClient struct {
	transport *scriptedTransport
}

// Do 处理非流式上游请求。
func (c *stubUpstreamClient) Do(req *http.Request) (*http.Response, error) {
	return c.transport.roundTrip(req)
}

// DoStream 处理流式上游请求。
//
// 与 Do 走同一条路径：脚本里已经用 chunks 表达了「分块到达」，而分块之间的空闲
// 超时由 runtime.IterStreamBytes 在读取侧计时，不需要传输层参与。
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
		// 用真实的拨号失败（ECONNREFUSED）而不是裸 errors.New：参照实现抛的是
		// httpx.ConnectError，而 Go 侧的错误名由 upstream.Classify 折算（它把所有
		// 连接类失败归为 CauseConnection ⇒ "ConnectError"）。用裸错误会得到
		// "RequestError"，那是**另一条**分支，会让这条用例失去意义。
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	// 声明了 content-encoding 的响应体在真实 httpx 客户端侧会被透明解压；Go 侧
	// 刻意关闭了透明解压（internal/upstream 的 DisableCompression），因此这一类
	// 响应无法逐字节对拍——语料里不出现它，差异由
	// TestContentEncodingIsNotDecoded 显式记录。
	payload := step.Body
	if step.Chunks != nil {
		payload = strings.Join(step.Chunks, "")
	}
	var reader io.Reader = strings.NewReader(payload)
	if step.Reader != nil {
		reader = step.Reader
	}
	if step.BreakAfter != nil && step.Chunks != nil {
		// 复刻 Python 侧 `break_after`：先产出前 N 块，然后抛错（对应 httpx.ReadError）。
		reader = &breakingChunksReader{chunks: step.Chunks, breakAfter: *step.BreakAfter}
	}
	return &http.Response{
		StatusCode: step.Status,
		Header:     headerPairs(step.Headers),
		Body:       io.NopCloser(reader),
	}, nil
}

// breakingChunksReader 逐块产出，到 breakAfter 块之后返回一个错误。
type breakingChunksReader struct {
	chunks     []string
	breakAfter int
	index      int
}

func (r *breakingChunksReader) Read(p []byte) (int, error) {
	if r.index >= r.breakAfter {
		return 0, errors.New("上游流中途断开")
	}
	chunk := r.chunks[r.index]
	r.index++
	return copy(p, chunk), nil
}

// jsonUpstreamResponse 构造一个 JSON 上游响应。
func jsonUpstreamResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// headerPairs 把语料里的响应头表折成 http.Header。
func headerPairs(headers map[string]string) http.Header {
	result := http.Header{}
	for key, value := range headers {
		result.Set(key, value)
	}
	return result
}

// recordingMetrics 是本包 MetricsSink 的假实现。
//
// 它把「语料无法表达」的那一层（指标行数、失败标记）变成可断言的记录；具体断言在
// metrics_test.go 里。
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
// 阈值设在 Error 之上：语料里大量用例故意触发 Warn / Error 分支。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// --- 配置构造 -----------------------------------------------------------------

// routerConfigFromCorpus 把语料里的紧凑配置展开成 RouterConfig。
//
// 刻意不走 config.FromDict：语料要能精确控制 KeyConfig 的每个字段（尤其是
// upstream_model 与 provider），而 from_dict 的 providers 展开会引入与代理层无关
// 的复杂度。这与 Python 侧生成器的做法一致。
func routerConfigFromCorpus(raw corpusConfig) *config.RouterConfig {
	cfg := &config.RouterConfig{
		Host:                   "127.0.0.1",
		Port:                   8000,
		RequestTimeout:         10,
		StreamFirstByteTimeout: 60,
		StreamIdleTimeout:      60,
		MaxRetries:             1,
		KeyFailureThreshold:    2,
		KeyCooldownSeconds:     60,
		LocalAPIKey:            "local-key",
		UpstreamRoutes:         raw.UpstreamRoutes,
		ReasoningEffortByModel: map[string]string{},
	}
	if raw.MaxRetries != nil {
		cfg.MaxRetries = *raw.MaxRetries
	}
	if raw.KeyFailureThreshold != nil {
		cfg.KeyFailureThreshold = *raw.KeyFailureThreshold
	}
	if raw.KeyCooldownSeconds != nil {
		cfg.KeyCooldownSeconds = *raw.KeyCooldownSeconds
	}
	if raw.StreamFirstByteTimeout != nil {
		cfg.StreamFirstByteTimeout = *raw.StreamFirstByteTimeout
	}
	if raw.StreamIdleTimeout != nil {
		cfg.StreamIdleTimeout = *raw.StreamIdleTimeout
	}
	if raw.LocalAPIKey != nil {
		cfg.LocalAPIKey = *raw.LocalAPIKey
	}
	if cfg.UpstreamRoutes == nil {
		cfg.UpstreamRoutes = map[string]map[string]string{}
	}
	for _, model := range raw.Models {
		parsed := config.ModelConfig{
			ID:            model.ID,
			Aliases:       model.Aliases,
			RoutingMode:   model.RoutingMode,
			NativeFirst:   model.NativeFirst,
			HiddenAliases: model.HiddenAliases,
		}
		if parsed.RoutingMode == "" {
			parsed.RoutingMode = "round_robin"
		}
		if model.ReasoningEffort != nil {
			parsed.ReasoningEffort = *model.ReasoningEffort
			if *model.ReasoningEffort != "" {
				cfg.ReasoningEffortByModel[model.ID] = *model.ReasoningEffort
			}
		}
		for _, key := range model.Keys {
			enabled := true
			if key.Enabled != nil {
				enabled = *key.Enabled
			}
			provider := ""
			if key.Provider != nil {
				provider = *key.Provider
			}
			parsed.Keys = append(parsed.Keys, config.KeyConfig{
				Name:          key.Name,
				APIKey:        key.APIKey,
				BaseURL:       key.BaseURL,
				Enabled:       enabled,
				AllowVisitor:  key.AllowVisitor,
				Provider:      provider,
				UpstreamModel: key.UpstreamModel,
			})
		}
		cfg.Models = append(cfg.Models, parsed)
	}
	if raw.UnifiedModel != nil {
		cfg.UnifiedModel = &config.UnifiedModelConfig{
			Default:    routePlanFromCorpus(raw.UnifiedModel.Default),
			Image:      corpusPlanPtr(raw.UnifiedModel.Image),
			Embeddings: corpusPlanPtr(raw.UnifiedModel.Embeddings),
		}
	}
	for _, task := range raw.Tasks {
		parsed := config.TaskConfig{Name: task.Name, Model: task.Model}
		if task.FallbackModel != nil {
			parsed.FallbackModel = *task.FallbackModel
		}
		if len(task.Params) > 0 {
			if value, err := canonical.Parse(task.Params); err == nil {
				parsed.Params = value
			}
		} else {
			parsed.Params = canonical.NewObject()
		}
		cfg.Tasks = append(cfg.Tasks, parsed)
	}
	return cfg
}

func corpusPlanPtr(plan *corpusPlan) *config.RoutePlan {
	if plan == nil {
		return nil
	}
	return &config.RoutePlan{
		Primary:  routeTargetFromCorpus(plan.Primary),
		Fallback: corpusTargetPtr(plan.Fallback),
	}
}

func corpusTargetPtr(target *corpusTarget) *config.RouteTarget {
	if target == nil {
		return nil
	}
	value := routeTargetFromCorpus(*target)
	return &value
}

func routePlanFromCorpus(plan corpusPlan) config.RoutePlan {
	return config.RoutePlan{
		Primary:  routeTargetFromCorpus(plan.Primary),
		Fallback: corpusTargetPtr(plan.Fallback),
	}
}

func routeTargetFromCorpus(target corpusTarget) config.RouteTarget {
	result := config.RouteTarget{Model: target.Model}
	if target.Key != nil {
		result.Key = *target.Key
	}
	return result
}
