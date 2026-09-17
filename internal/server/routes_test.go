package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件从**装配后的 Handler** 出发逐条发请求，证明 app 面、管理面与运维面的 URL
// 空间互不遮蔽。
//
// 为什么要在装配层再测一遍：internal/api 已经有一条同样的测试证明「47 条模式都注册
// 了」，但那是在它自己的 mux 上。装配层多了一层 —— 外层 ServeMux 用 "/" 兜住所有
// 未匹配路径，一旦前缀分支写错（例如把 "/api/" 写成 "/api"，或把 app 面的 "/" 兜底
// 注册得过于宽泛），管理路由会被**静默**吞掉，返回一个形状完全相同的 404。
// 因此这里断言的落脚点是「命中的不是兜底 404」，而不是状态码本身。

// managementRoutePatterns 是 internal/api/router.go 里 routePatterns() 的副本。
//
// 那份清单没有导出，只能复制。失真风险由两个断言兜住：这里断言数量是 47（api 侧同样
// 断言 47），以及 TestAPIPrefixIsForwarded 断言 "/api/" 整体被转发。数量变了或转发
// 断了，测试都会失败。
var managementRoutePatterns = []string{
	"GET /api/unified-model", "PUT /api/unified-model", "DELETE /api/unified-model",
	"GET /api/tasks", "POST /api/tasks",
	"GET /api/tasks/{task_name}", "PUT /api/tasks/{task_name}", "DELETE /api/tasks/{task_name}",
	"GET /api/settings", "PUT /api/settings", "POST /api/settings/local-api-key",
	"POST /api/update/check",
	"GET /api/providers", "POST /api/providers",
	"GET /api/providers/{provider_id}", "PUT /api/providers/{provider_id}",
	"DELETE /api/providers/{provider_id}",
	"GET /api/providers/{provider_id}/keys", "POST /api/providers/{provider_id}/keys",
	"GET /api/providers/{provider_id}/keys/{key_name}",
	"PUT /api/providers/{provider_id}/keys/{key_name}",
	"DELETE /api/providers/{provider_id}/keys/{key_name}",
	"GET /api/routes", "POST /api/routes",
	"GET /api/routes/{route_id}", "PUT /api/routes/{route_id}", "DELETE /api/routes/{route_id}",
	"POST /api/probes/keys",
	"POST /api/providers/{provider_id}/probe",
	"POST /api/providers/{provider_id}/keys/{key_name}/probe",
	"GET /api/providers/{provider_id}/keys/{key_name}/models",
	"PUT /api/providers/{provider_id}/keys/{key_name}/models",
	"GET /api/probes/{probe_id}", "POST /api/probes/{probe_id}/cancel",
	"POST /api/config/export", "POST /api/config/import",
	"POST /api/models", "GET /api/models", "GET /api/models/{model_id}",
	"PUT /api/models/{model_id}", "DELETE /api/models/{model_id}",
	"GET /api/models/{model_id}/keys", "POST /api/models/{model_id}/keys",
	"GET /api/models/{model_id}/keys/{key_name}",
	"GET /api/models/{model_id}/keys/{key_name}/stats",
	"PUT /api/models/{model_id}/keys/{key_name}",
	"DELETE /api/models/{model_id}/keys/{key_name}",
}

// opsRoutePatterns 是 internal/api/handlers_ops.go 里 opsRoutePatterns() 的副本（7 条）。
var opsRoutePatterns = []string{
	"GET /api/logs",
	"GET /api/tool",
	"POST /api/tool/webui",
	"POST /api/service/{action}",
	"GET /api/integrations",
	"POST /api/integrations/{agent}",
	"POST /api/integrations/{agent}/rollback",
}

// appSurfacePatterns 是装配层自己实现的路由（与 routes.go 的注册一一对应）。
var appSurfacePatterns = []string{
	"HEAD /",
	"GET /",
	"GET /health",
	"HEAD /health",
	"GET /v1/models",
	"HEAD /v1/models",
	"GET /metrics",
	"GET /metrics/requests",
	"GET /metrics/series",
	"GET /v1/{path}",
}

// pathParamReplacer 把模式里的路径参数换成具体值。
var pathParamReplacer = strings.NewReplacer(
	"{task_name}", "x", "{provider_id}", "x", "{key_name}", "x",
	"{route_id}", "x", "{model_id}", "x", "{probe_id}", "x",
	"{agent}", "x", "{action}", "status_amkr", "{path}", "does-not-exist",
)

// splitPattern 把「方法 + 路径」拆开。
func splitPattern(pattern string) (string, string) {
	method, path, _ := strings.Cut(pattern, " ")
	return method, pathParamReplacer.Replace(path)
}

// assertNotFallback404 断言请求命中了某条真实路由，而不是兜底 404。
func assertNotFallback404(t *testing.T, app *App, pattern string) {
	t.Helper()
	method, path := splitPattern(pattern)
	// 不带凭据：管理/运维面回 401、带必填体的 POST 回 422、app 面的 HEAD/GET 回它们
	// 各自的响应；这些都说明路由存在。唯一不能出现的是兜底 404。
	recorder := serve(app, method, path, "")
	if recorder.Code == http.StatusNotFound && recorder.Body.String() == notFoundBody {
		t.Errorf("路由被遮蔽（命中兜底 404）: %s", pattern)
	}
}

// TestManagementRoutesNotShadowed 逐条实例化 47 条管理路由，确认没有一条被 app 面吞掉。
func TestManagementRoutesNotShadowed(t *testing.T) {
	if len(managementRoutePatterns) != 47 {
		t.Fatalf("管理路由数量不对: %d", len(managementRoutePatterns))
	}
	app := newTestApp(t, t.TempDir(), func(options *Options) {
		options.CheckUpdate = stubCheckUpdate
	})
	for _, pattern := range managementRoutePatterns {
		assertNotFallback404(t, app, pattern)
	}
}

// TestOpsRoutesNotShadowed 逐条实例化 7 条运维路由（ops 已开启）。
//
// 三条 /api/integrations* 目前是「已注册、未接线」，返回 500 而不是 404——这正是本
// 测试要区分的状态（接线与否是后续工作，不可达才是缺陷）。
func TestOpsRoutesNotShadowed(t *testing.T) {
	if len(opsRoutePatterns) != 7 {
		t.Fatalf("运维路由数量不对: %d", len(opsRoutePatterns))
	}
	app := newTestApp(t, t.TempDir(), func(options *Options) {
		options.CheckUpdate = stubCheckUpdate
	})
	for _, pattern := range opsRoutePatterns {
		assertNotFallback404(t, app, pattern)
	}
}

// TestAppSurfaceRoutesNotShadowed 逐条实例化 app 面路由。
func TestAppSurfaceRoutesNotShadowed(t *testing.T) {
	app := newTestApp(t, t.TempDir(), nil)
	for _, pattern := range appSurfacePatterns {
		assertNotFallback404(t, app, pattern)
	}
}

// TestAPIPrefixIsForwarded 断言 "/api/" 整段被转发给管理 API。
//
// 判据选「无凭据的 GET /api/providers -> 401」：外层兜底对任何非 "/" 路径都回 404，
// 因此 401 只可能来自管理 API 的处理器。这条断言与逐条模式检查互补——它不依赖那份
// 复制的 47 条清单。
func TestAPIPrefixIsForwarded(t *testing.T) {
	app := newTestApp(t, t.TempDir(), nil)
	recorder := serve(app, http.MethodGet, "/api/providers", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/providers（无凭据）状态码 = %d，期望 401（body=%s）",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "本地 API key 验证失败") {
		t.Errorf("管理 API 的错误信封不对: %s", recorder.Body.String())
	}
	// 未注册的 /api 路径落进 api 自己的兜底（也是这条 JSON），与 Python 一致。
	unknown := serve(app, http.MethodGet, "/api/does-not-exist", "")
	if unknown.Code != http.StatusNotFound || unknown.Body.String() != notFoundBody {
		t.Errorf("未知 /api 路径 = %d %s，期望 404 %s",
			unknown.Code, unknown.Body.String(), notFoundBody)
	}
}

// TestCatchAllOnlyMatchesV1 断言 /v1/ 通配路由不会吞掉别的路径。
//
// 这是一条「负面」断言：通配路由写成 "/" 或 "/v1" 时，/health、/metrics 甚至 /ui/
// 都会被送进 proxy，而这些路径的响应形状与 proxy 的错误完全不同。
func TestCatchAllOnlyMatchesV1(t *testing.T) {
	app := newTestApp(t, t.TempDir(), nil)
	cases := []struct {
		method        string
		path          string
		authorization string
		status        int
	}{
		// /health 归 app 面：proxy 不会返回 200 + status:"ok"。
		{http.MethodGet, "/health", "", http.StatusOK},
		// /metrics 无凭据：app 面的 401，而不是 proxy 的 400「请求体中缺少 model 字段」。
		{http.MethodGet, "/metrics", "", http.StatusUnauthorized},
		// /v1/ 之下归 proxy：带凭据但无 body 时是 400「请求体中缺少 model 字段」
		// （proxy 的 prepare 先鉴权、再解析 body），确认请求真的进了 proxy。
		{http.MethodGet, "/v1/anything", fullAuthorization, http.StatusBadRequest},
	}
	for _, testCase := range cases {
		recorder := serve(app, testCase.method, testCase.path, testCase.authorization)
		if recorder.Code != testCase.status {
			t.Errorf("%s %s 状态码 = %d，期望 %d（body=%s）",
				testCase.method, testCase.path, recorder.Code, testCase.status,
				recorder.Body.String())
		}
	}
	// 反向：/v1/models 的 GET 走 app 面（同一条路径的其它方法走 proxy，见语料）。
	models := serve(app, http.MethodGet, "/v1/models", fullAuthorization)
	if !strings.Contains(models.Body.String(), `"object":"list"`) {
		t.Errorf("GET /v1/models 未走 app 面: %s", models.Body.String())
	}
}

// TestStaticAndWebSocketAbsentWhenDisabled 断言「未启用/未实现的东西不注册任何路由」。
//
//   - WebUI：资产为 nil（或配置关闭）时 /ui/ 必须 404，而不是 200 或 500。
//   - WebSocket：/ws/events 与 WS 升级的 /v1/{path} 都**不在**本任务范围内（前端改
//     为轮询），留接缝但不注册——普通 GET /ws/events 必须落进 404，而不是被 app 面
//     的某个兜底当成普通请求处理。
func TestStaticAndWebSocketAbsentWhenDisabled(t *testing.T) {
	app := newTestApp(t, t.TempDir(), nil)
	for _, path := range []string{"/ui/", "/ui/index.html", "/ws/events"} {
		recorder := serve(app, http.MethodGet, path, "")
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d，期望 404（未注册）", path, recorder.Code)
		}
		if recorder.Body.String() != notFoundBody {
			t.Errorf("GET %s 响应体 = %s，期望 %s", path, recorder.Body.String(), notFoundBody)
		}
	}
	// 升级请求（WebSocket 握手）同样不被接受：没有路由会回 101。
	upgrade := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, upgrade)
	if recorder.Code == http.StatusSwitchingProtocols {
		t.Error("/v1/anything 不应接受 WebSocket 升级（本任务不含 WebSocket）")
	}
}

// TestUpdateCheckRouteWired 断言 /api/update/check 用的是注入的 CheckUpdate 接缝
// （而不是 api 侧「更新检查未接入」的 500）。
func TestUpdateCheckRouteWired(t *testing.T) {
	app := newTestApp(t, t.TempDir(), func(options *Options) {
		options.CheckUpdate = stubCheckUpdate
	})
	recorder := serve(app, http.MethodPost, "/api/update/check", fullAuthorization)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /api/update/check 状态码 = %d，期望 200（body=%s）",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"latest_version":"99.0.0"`) {
		t.Errorf("响应体未包含接缝给的版本号: %s", recorder.Body.String())
	}
}
