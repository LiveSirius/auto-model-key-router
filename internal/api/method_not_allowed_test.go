package api

import "testing"

// methodContractCase 是一条「路径 × 方法」的对外契约用例。
//
// 期望值全部来自驱动真实 Python 应用（create_app + starlette.testclient.TestClient）
// 的实测结果，不是推断：
//
//	PUT /api/logs            -> 405 {"detail":"Method Not Allowed"} + Allow: GET
//	PUT /api/models          -> 405 + Allow: GET（该路径同时注册了 GET 与 POST）
//	POST /api/models/{id}    -> 405 + Allow: GET（GET+PUT+DELETE 里第一个是 GET）
//	GET /api/tool/webui      -> 405 + Allow: POST
//	PUT /api/service/nope    -> 405 + Allow: POST
//	DELETE /api/probes/keys  -> 405 + Allow: POST（与 /api/probes/{probe_id} 同路径）
//	GET /api/probes/keys     -> 404 {"detail":"探测不存在"}（这是**处理器**的 404）
//	PUT /api/nope            -> 404 {"detail":"Not Found"}（路由级 404，无 Allow）
//	HEAD /api/logs           -> 405 + Allow: GET（body 被客户端/服务器丢弃）
//	ops_enabled=false 时 /api/logs 整体不存在：PUT 也是 404，不是 405
//	405 判定在鉴权之前：不带凭据的 PUT /api/logs 同样是 405
type methodContractCase struct {
	name string
	// method/path 是请求本身。
	method string
	path   string
	// auth 取 "full" / "visitor" / "none"，对应 opsRequest 的第三个参数。
	auth string
	// body 为空表示不带请求体。
	body string
	// opsDisabled 为真时按 OpsEnabled=false 组装服务器（运维 URL 不存在）。
	opsDisabled bool

	wantStatus int
	// wantBody 为空表示只断言状态与 Allow（响应体依赖临时目录或 HEAD 语义）。
	wantBody string
	// wantAllow 为空表示响应里**不应**出现 Allow。
	wantAllow string
}

// TestMethodContractMatchesPython 锁定「正确方法不变、错方法 405、未知路径 404」。
//
// 这条测试的动机是 `PUT /api/logs` 曾经回 404：无方法的兜底模式匹配任意方法，把
// ServeMux 自带的 405 分支整个吃掉了。
func TestMethodContractMatchesPython(t *testing.T) {
	cases := []methodContractCase{
		// —— 正确方法：行为不变（既不是 405，也不是兜底 404）——
		{name: "correct/GET logs", method: "GET", path: "/api/logs", auth: "full", wantStatus: 200},
		{name: "correct/GET models", method: "GET", path: "/api/models", auth: "full", wantStatus: 200},
		{
			name: "correct/GET models/{id}", method: "GET", path: "/api/models/abc", auth: "full",
			wantStatus: 404, wantBody: `{"detail":"模型不存在: abc"}`,
		},
		{name: "correct/POST tasks", method: "POST", path: "/api/tasks", auth: "full", wantStatus: 422},
		{
			name: "correct/POST service/{action}", method: "POST", path: "/api/service/nope", auth: "full",
			wantStatus: 422, wantBody: `{"detail":"不支持的服务动作: nope"}`,
		},
		{
			name: "correct/POST integrations/{agent}", method: "POST", path: "/api/integrations/nope", auth: "full",
			body: `{"mode":"native"}`, wantStatus: 404, wantBody: `{"detail":"不支持的集成: nope"}`,
		},
		// 路径同时匹配 POST /api/probes/keys 与 GET /api/probes/{probe_id}：GET 是
		// 完整匹配，走处理器（404 探测不存在），不是路由级 405/404。
		{
			name: "correct/GET probes/keys", method: "GET", path: "/api/probes/keys", auth: "full",
			wantStatus: 404, wantBody: `{"detail":"探测不存在"}`,
		},

		// —— 错方法：405 + Allow，体与 404 同形 ——
		{
			name: "wrong/PUT logs", method: "PUT", path: "/api/logs", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		{
			name: "wrong/OPTIONS logs", method: "OPTIONS", path: "/api/logs", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		// 405 在鉴权之前判定：参照实现里 Starlette 先做路由部分匹配。
		{
			name: "wrong/DELETE logs no-auth", method: "DELETE", path: "/api/logs", auth: "none",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		// ServeMux 会把 GET 模式当 HEAD 匹配，参照实现不给 GET 路由加 HEAD：
		// 必须在进 mux 之前分流，否则这里是 200。
		{name: "wrong/HEAD logs", method: "HEAD", path: "/api/logs", auth: "full", wantStatus: 405, wantAllow: "GET"},
		// 多方法路径：Allow 只报**第一个**路径匹配的路由的方法（GET），不是 "GET, POST"。
		{
			name: "wrong/PUT models", method: "PUT", path: "/api/models", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		{
			name: "wrong/PATCH unified-model", method: "PATCH", path: "/api/unified-model", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		// {param} 路由：路径参数段必须被当作单段通配。
		{
			name: "wrong/POST models/{model_id}", method: "POST", path: "/api/models/abc", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		{
			name: "wrong/PATCH tasks/{task_name}", method: "PATCH", path: "/api/tasks/x", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		{
			name: "wrong/POST providers/{id}/keys/{key}", method: "POST", path: "/api/providers/p/keys/k", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},
		// POST-only（三段精确路径）。
		{
			name: "wrong/PUT settings/local-api-key", method: "PUT", path: "/api/settings/local-api-key", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "POST",
		},
		{
			name: "wrong/GET config/import", method: "GET", path: "/api/config/import", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "POST",
		},
		// 同一路径既是精确模式又是参数模式时，Allow 取先注册的那个（POST），与
		// Starlette「只保留第一个部分匹配」一致。
		{
			name: "wrong/DELETE probes/keys", method: "DELETE", path: "/api/probes/keys", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "POST",
		},

		// —— 运维面的 7 条路由 ——
		{
			name: "wrong/GET tool/webui", method: "GET", path: "/api/tool/webui", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "POST",
		},
		{
			name: "wrong/PUT service/{action}", method: "PUT", path: "/api/service/nope", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "POST",
		},
		{
			name: "wrong/GET integrations/{agent}/rollback", method: "GET", path: "/api/integrations/nope/rollback", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "POST",
		},
		{
			name: "wrong/PUT tool", method: "PUT", path: "/api/tool", auth: "full",
			wantStatus: 405, wantBody: methodNotAllowedBody, wantAllow: "GET",
		},

		// —— 未知路径：仍是 404 {"detail":"Not Found"}，且不带 Allow ——
		{
			name: "unknown/PUT", method: "PUT", path: "/api/nope", auth: "full",
			wantStatus: 404, wantBody: notFoundBody,
		},
		{
			name: "unknown/GET", method: "GET", path: "/api/nope/deeper", auth: "full",
			wantStatus: 404, wantBody: notFoundBody,
		},
		{name: "unknown/HEAD", method: "HEAD", path: "/api/nope", auth: "full", wantStatus: 404},
		// 段数/字面量对不上就不是已知路径：{param} 只吃一段，且不吃空段。
		{
			name: "unknown/too many segments", method: "PUT", path: "/api/models/x/keys/y/models", auth: "full",
			wantStatus: 404, wantBody: notFoundBody,
		},
		// 运维面关闭时这 7 条 URL 整体不存在——错方法也是 404，不能变成 405。
		{
			name: "ops-disabled/PUT logs", method: "PUT", path: "/api/logs", auth: "full", opsDisabled: true,
			wantStatus: 404, wantBody: notFoundBody,
		},
		{
			name: "ops-disabled/GET tool", method: "GET", path: "/api/tool", auth: "full", opsDisabled: true,
			wantStatus: 404, wantBody: notFoundBody,
		},
		{
			name: "ops-disabled/POST service", method: "POST", path: "/api/service/nope", auth: "full", opsDisabled: true,
			wantStatus: 404, wantBody: notFoundBody,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newOpsTestServer(t, func(s *Server) {
				s.OpsEnabled = !testCase.opsDisabled
			})
			// 空串走 nil：opsRequest 会据此跳过 body 与 content-type。
			var body []byte
			if testCase.body != "" {
				body = []byte(testCase.body)
			}
			recorder := opsRequest(t, server, testCase.method, testCase.path, body, testCase.auth)
			result := recorder.Result()

			if result.StatusCode != testCase.wantStatus {
				t.Fatalf("%s %s: 状态码 %d，期望 %d（体 %q）",
					testCase.method, testCase.path, result.StatusCode, testCase.wantStatus, recorder.Body.String())
			}
			// Allow 必须精确：多一个方法（例如 ServeMux 自带的 "GET, HEAD"）都不算对。
			if allow := result.Header.Get("Allow"); allow != testCase.wantAllow {
				t.Errorf("%s %s: Allow %q，期望 %q", testCase.method, testCase.path, allow, testCase.wantAllow)
			}
			if testCase.wantBody == "" {
				return
			}
			if body := recorder.Body.String(); body != testCase.wantBody {
				t.Errorf("%s %s: 响应体 %q，期望 %q", testCase.method, testCase.path, body, testCase.wantBody)
			}
			if contentType := result.Header.Get("Content-Type"); contentType != "application/json" {
				t.Errorf("%s %s: Content-Type %q", testCase.method, testCase.path, contentType)
			}
		})
	}
}

// methodNotAllowedBody / notFoundBody 是两条兜底响应的逐字节形式（含字段顺序）。
const (
	methodNotAllowedBody = `{"detail":"Method Not Allowed"}`
	notFoundBody         = `{"detail":"Not Found"}`
)

// TestMatchesPathHandlesParams 单独锁定路径匹配器：{name} 是**单段非空**通配，
// 其余段按字面量比较，段数必须相等。
func TestMatchesPathHandlesParams(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/api/logs", "/api/logs", true},
		{"/api/logs", "/api/log", false},
		{"/api/logs", "/api/logs/", false}, // 尾斜杠多出一个空段
		{"/api/logs", "/api/logs/x", false},
		{"/api/models/{model_id}", "/api/models/abc", true},
		{"/api/models/{model_id}", "/api/models", false},
		{"/api/models/{model_id}", "/api/models/", false}, // 空段不匹配
		{"/api/models/{model_id}", "/api/models/a/b", false},
		{"/api/models/{model_id}/keys/{key_name}", "/api/models/m/keys/k", true},
		{"/api/models/{model_id}/keys/{key_name}", "/api/models/m/keys", false},
		{"/api/providers/{provider_id}/keys/{key_name}/models", "/api/providers/p/keys/k/models", true},
		// 参数段不能跨 "/" 吞掉多段。
		{"/api/service/{action}", "/api/service/start_amkr/x", false},
		// 字面量段必须逐字相等（不能因为参数段的存在而放宽）。
		{"/api/models/{model_id}", "/api/providers/abc", false},
	}
	for _, testCase := range cases {
		if got := matchesPath(testCase.pattern, testCase.path); got != testCase.want {
			t.Errorf("matchesPath(%q, %q) = %v，期望 %v", testCase.pattern, testCase.path, got, testCase.want)
		}
	}
}
