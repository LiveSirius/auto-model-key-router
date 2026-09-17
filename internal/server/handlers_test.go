package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// TestModelsExcludesUnconfiguredModelAndAppendsUnified 断言 /v1/models 的语义。
//
// 与语料的逐字节比对互补：语料锁定的是**整体字节**（键序、owned_by 等），这里把
// 「哪些模型该出现」显式写出来，读者不必去读语料 JSON：
//
//   - model-b 没有任何 key（未配置）⇒ 它自己与它的别名都不出现；
//   - model-v 有 allow_visitor 的 key ⇒ 完整清单里出现，访客清单里是 amkr- 前缀名；
//   - model-nv 有 key 但不允许访客 ⇒ 只在完整清单里；
//   - unified-model 是**末尾追加**的（key_pool.available_model_ids 的 append 语义）。
func TestModelsExcludesUnconfiguredModelAndAppendsUnified(t *testing.T) {
	app := newTestApp(t, t.TempDir(), nil)

	full := modelIDs(t, serve(app, http.MethodGet, "/v1/models", fullAuthorization))
	for _, want := range []string{"alias-a", "alias-v", "alias-nv", "model-a", "model-v", "model-nv"} {
		if !contains(full, want) {
			t.Errorf("完整清单缺少 %q: %v", want, full)
		}
	}
	for _, unwanted := range []string{"model-b", "alias-b"} {
		if contains(full, unwanted) {
			t.Errorf("未配置的模型不应出现: %q（%v）", unwanted, full)
		}
	}
	if len(full) == 0 || full[len(full)-1] != "unified-model" {
		t.Errorf("unified-model 必须是最后一项: %v", full)
	}

	visitor := modelIDs(t, serve(app, http.MethodGet, "/v1/models", visitorAuthorization))
	if len(visitor) != 1 || visitor[0] != "amkr-model-v" {
		t.Errorf("访客清单 = %v，期望只有 amkr-model-v", visitor)
	}
	// 访客看不到 unified-model（它没有 allow_visitor 的 key）。
	for _, unwanted := range []string{"unified-model", "model-a", "model-nv"} {
		if contains(visitor, unwanted) {
			t.Errorf("访客清单不应包含 %q: %v", unwanted, visitor)
		}
	}
}

// modelIDs 从 /v1/models 响应里取出 id 列表。
func modelIDs(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	parsed, err := canonical.ParseString(recorder.Body.String())
	if err != nil {
		t.Fatalf("解析 /v1/models 响应失败: %v（body=%s）", err, recorder.Body.String())
	}
	items := parsed.Lookup("data")
	if items == nil || !items.IsArray() {
		t.Fatalf("响应里没有 data 数组: %s", recorder.Body.String())
	}
	ids := make([]string, 0, len(items.Arr))
	for _, item := range items.Arr {
		ids = append(ids, item.Lookup("id").StringValue())
	}
	return ids
}

// contains 报告字符串切片是否包含目标。
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestUnauthorizedBodiesMatchReference 锁定两处 401 的信封差异。
//
// /v1/models 与三条 metrics 路由用 proxy 风格的 {"error":{"message":...}}，而管理 API
// 用 FastAPI 风格的 {"detail":...}；两者都在参照实现里出现过，不能统一。
func TestUnauthorizedBodiesMatchReference(t *testing.T) {
	app := newTestApp(t, t.TempDir(), nil)
	const appSurface = `{"error":{"message":"本地 API key 验证失败"}}`
	for _, path := range []string{
		"/v1/models", "/metrics", "/metrics/requests", "/metrics/series",
	} {
		recorder := serve(app, http.MethodGet, path, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("GET %s 状态码 = %d，期望 401", path, recorder.Code)
		}
		if recorder.Body.String() != appSurface {
			t.Errorf("GET %s 响应体 = %s，期望 %s", path, recorder.Body.String(), appSurface)
		}
	}
	management := serve(app, http.MethodGet, "/api/providers", "")
	if management.Body.String() != `{"detail":"本地 API key 验证失败"}` {
		t.Errorf("管理 API 的 401 应用 detail 信封: %s", management.Body.String())
	}
}

// TestHealthReportsAssembledInputs 断言 /health 的取数来自装配层。
//
// 响应体的字段顺序与语义由 internal/health 的语料锁定；这里只钉住「装配层喂进去的
// 是哪些值」——尤其是 local_api_key 的指纹（它证明真的把运行时配置里的 key 传下去了）
// 与 config_path（绝对路径），这两项一旦漏传会静默变成空串。
func TestHealthReportsAssembledInputs(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t, dir, nil)
	recorder := serve(app, http.MethodGet, "/health", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /health 状态码 = %d，期望 200（body=%s）",
			recorder.Code, recorder.Body.String())
	}
	parsed, err := canonical.ParseString(recorder.Body.String())
	if err != nil {
		t.Fatalf("解析 /health 失败: %v", err)
	}

	if got := parsed.Lookup("status").StringValue(); got != "ok" {
		t.Errorf("status = %q", got)
	}
	if got := parsed.Lookup("version").StringValue(); got != testVersion {
		t.Errorf("version = %q，期望 %q", got, testVersion)
	}
	// "local-key" 的 sha256 前 12 位（与真实 Python /health 的实测值一致）。
	if got := parsed.Lookup("local_api_key_fingerprint").StringValue(); got != "dba73d3fcb67" {
		t.Errorf("local_api_key_fingerprint = %q，期望 dba73d3fcb67", got)
	}
	if got := parsed.Lookup("local_auth_enabled"); got == nil || !got.Bool {
		t.Errorf("local_auth_enabled = %v，期望 true", got)
	}
	wantPath := filepath.Join(dir, "router-config.json")
	if got := parsed.Lookup("config_path").StringValue(); got != wantPath {
		t.Errorf("config_path = %q，期望绝对路径 %q", got, wantPath)
	}
	if got := parsed.Lookup("models"); got == nil || !got.IsArray() || len(got.Arr) == 0 {
		t.Errorf("models 应非空: %v", got)
	}
	if got := parsed.Lookup("ops_enabled"); got == nil || !got.Bool {
		t.Errorf("ops_enabled = %v，期望 true（配置里是 true）", got)
	}
	if got := parsed.Lookup("webui_path"); got == nil || !got.IsNull() {
		t.Errorf("未挂载时 webui_path 必须是 null: %v", got)
	}
}

// TestHealthOpsFlagFollowsOverride 断言 ops_enabled 的覆盖语义（app.py:142-146）。
func TestHealthOpsFlagFollowsOverride(t *testing.T) {
	// 配置里 ops_enabled=true，但显式覆盖为 false。
	off := false
	app := newTestAppWith(t, t.TempDir(), appFixture{
		opsEnabled: true,
		mutate:     func(options *Options) { options.OpsEnabled = &off },
	})
	parsed, err := canonical.ParseString(serve(app, http.MethodGet, "/health", "").Body.String())
	if err != nil {
		t.Fatalf("解析 /health 失败: %v", err)
	}
	if got := parsed.Lookup("ops_enabled"); got == nil || got.Bool {
		t.Errorf("ops_enabled = %v，期望 false（被 Options.OpsEnabled 覆盖）", got)
	}
	// 覆盖只影响应用层开关：api 侧同样不注册运维路由。
	if recorder := serve(app, http.MethodGet, "/api/logs", ""); recorder.Code != http.StatusNotFound {
		t.Errorf("覆盖为 false 时 GET /api/logs 状态码 = %d，期望 404", recorder.Code)
	}
}

// TestActiveRequestsReflectsInflightProxyRequest 是 /v1/ 包装层的端到端证据。
//
// 参照实现里 app.py:354 在进入 proxy 前 acquire_active()、在流式响应结束或非流式响应
// 写完后 release_active()，因此 /metrics 的 active_requests 在一次真实代理请求期间是 1。
// Go 侧同样在 handleProxy 里成对调用（defer 覆盖整条同步流），这里用「上游慢响应」
// 把那个窗口拉到可观测：
//
//	发起代理请求 → 等它进入上游等待 → GET /metrics 必须看到 active_requests=1
//	                → 等响应返回     → 再查必须是 0
//
// 用真实 httptest 上游（而不是桩）是为了同时验证装配层的上游客户端接缝真的通了。
func TestActiveRequestsReflectsInflightProxyRequest(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	// 把夹具里的第一个 provider 指向上游测试服务器：POST 探针会走到它。
	path := writeFixture(t, dir, true, false)
	rewriteBaseURL(t, path, upstream.URL)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	app, err := New(Options{ConfigPath: path, Config: loaded, Version: testVersion})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = app.Close() }()

	activeRequests := func() int {
		recorder := serve(app, http.MethodGet, "/metrics?hours=1", fullAuthorization)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /metrics 状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
		}
		parsed, parseErr := canonical.ParseString(recorder.Body.String())
		if parseErr != nil {
			t.Fatalf("解析 /metrics 失败: %v", parseErr)
		}
		value, _ := parsed.Lookup("active_requests").AsFloat()
		return int(value)
	}

	body := `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`
	done := make(chan int, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", fullAuthorization)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		done <- recorder.Code
	}()

	// 轮询等待请求进入上游等待窗口（最多 2 秒），此时活跃数必须是 1。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if activeRequests() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("上游等待期间 active_requests 始终不是 1")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(release)
	if status := <-done; status != http.StatusOK {
		t.Fatalf("代理请求状态码 = %d，期望 200", status)
	}
	// 响应写完后必须归零（否则计数会随请求数单调增长）。
	if got := activeRequests(); got != 0 {
		t.Errorf("请求结束后 active_requests = %d，期望 0", got)
	}
}

// rewriteBaseURL 把配置里第一个 provider 的 base_url 换成本地测试上游。
func rewriteBaseURL(t *testing.T, path, baseURL string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	text := strings.Replace(string(raw), "https://a.example.test", baseURL, 1)
	text = strings.Replace(text, "https://b.example.test", baseURL, 1)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
}

// TestMetricsSnapshotSeesKeyStatsWiring 断言管理 API 的 key_stats 接缝接到了当前代
// 的指标库上（否则 /api/models/{id}/keys/{name}/stats 会回 500「未接入」）。
func TestMetricsSnapshotSeesKeyStatsWiring(t *testing.T) {
	app := newTestApp(t, t.TempDir(), func(options *Options) {
		options.CheckUpdate = stubCheckUpdate
	})
	recorder := serve(app, http.MethodGet,
		"/api/models/model-a/keys/key-a/stats", fullAuthorization)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET key stats 状态码 = %d，期望 200（body=%s）",
			recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 key stats 响应失败: %v", err)
	}
	if payload["model_id"] != "model-a" || payload["key_name"] != "key-a" {
		t.Errorf("key stats 的 model/key 不对: %v", payload)
	}
	if _, ok := payload["stats"]; !ok {
		t.Errorf("key stats 缺少 stats 字段: %v", payload)
	}
}
