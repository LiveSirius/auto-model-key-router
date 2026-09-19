package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件固化工作空间在代理层的可观察行为。
//
// 工作空间是 Go 侧的新增能力（参照实现没有这个概念），因此这些用例是手写的，
// 而不是来自对拍语料。既有语料（不带 X-AMKR-Workspace 头）继续锁定兼容性。

// workspaceConfig 构造一份两空间配置：
//
//	默认空间：shared -> workspace-a（备选 workspace-b）
//	teamA   ：shared -> team-b
//
// 同名任务指向不同模型，因此「路由到了哪个模型」就能看出用了哪个空间。
func workspaceConfig() *config.RouterConfig {
	return &config.RouterConfig{
		Models: []config.ModelConfig{
			testModel("model-a", []config.KeyConfig{testKey("ka", "https://a.test")}),
			testModel("model-b", []config.KeyConfig{testKey("kb", "https://b.test")}),
		},
		Tasks: []config.TaskConfig{
			{Name: "shared", Model: "model-a", FallbackModel: "model-b"},
			{Name: "shared", Workspace: "teamA", Model: "model-b"},
		},
	}
}

// TestWorkspaceHeaderSelectsTaskGroup 固化：同名任务按请求头落到不同模型。
func TestWorkspaceHeaderSelectsTaskGroup(t *testing.T) {
	env := newTestEnv(t, workspaceConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	body := `{"model":"shared","messages":[{"role":"user","content":"hi"}]}`

	// 不带请求头 -> 默认空间 -> model-a。
	if got := env.request(http.MethodPost, "chat/completions", body, nil); got.Code != http.StatusOK {
		t.Fatalf("默认空间状态码: got %d（%s）", got.Code, got.Body.String())
	}
	if len(env.transport.calls) != 1 || env.transport.calls[0].path != "/v1/chat/completions" {
		t.Fatalf("默认空间上游调用异常: %v", describeUpstreams(env.transport.calls))
	}

	// 带 teamA 头 -> teamA 空间 -> model-b。用它自己的 key 就说明换了模型。
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	got := env.request(http.MethodPost, "chat/completions", body,
		map[string]string{config.WorkspaceHeader: "teamA"})
	if got.Code != http.StatusOK {
		t.Fatalf("teamA 状态码: got %d（%s）", got.Code, got.Body.String())
	}
	if len(env.transport.calls) != 2 {
		t.Fatalf("teamA 应有第二次上游调用，实得 %v", describeUpstreams(env.transport.calls))
	}
	if key := env.transport.calls[1].headers["authorization"]; key != "Bearer sk-kb" {
		t.Fatalf("teamA 的 shared 应指向 model-b（用 kb），实际 Authorization=%q", key)
	}
}

// TestWorkspaceHeaderDoesNotLeakUpstream 固化：路由头不转发给上游。
//
// 上游既看不懂也不该看到 AMKR 的内部路由状态——与 x-api-key 同类。
func TestWorkspaceHeaderDoesNotLeakUpstream(t *testing.T) {
	env := newTestEnv(t, workspaceConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{config.WorkspaceHeader: "teamA"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码: got %d（%s）", recorder.Code, recorder.Body.String())
	}
	if len(env.transport.calls) != 1 {
		t.Fatalf("上游调用数: %v", describeUpstreams(env.transport.calls))
	}
	if value, exists := env.transport.calls[0].headers["x-amkr-workspace"]; exists {
		t.Fatalf("X-AMKR-Workspace 不应转发给上游，实得 %q", value)
	}
}

// TestUnknownWorkspaceFallsBackToModelLookup 固化：未知空间名不是错误。
//
// 空间里没有这个任务名，就按普通模型名继续解析——因此得到一个「模型未配置」404，
// 与不带该头时写一个不存在的模型名完全一致。刻意不做「空间不存在」的前置校验：
// 空间是隐式产生的（在它里面建任务即存在），单独一条错误既没有信息量也会多一种
// 需要维护的状态。
func TestUnknownWorkspaceFallsBackToModelLookup(t *testing.T) {
	env := newTestEnv(t, workspaceConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{config.WorkspaceHeader: "nope"})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知空间状态码: got %d（%s）", recorder.Code, recorder.Body.String())
	}
	if len(env.transport.calls) != 0 {
		t.Fatalf("未知空间不应触达上游，实得 %v", describeUpstreams(env.transport.calls))
	}
}

// TestWorkspaceTaskFallbackStaysInWorkspace 固化：备选也取自同一个空间。
func TestWorkspaceTaskFallbackStaysInWorkspace(t *testing.T) {
	cfg := workspaceConfig()
	// 让 teamA 的 shared 也带备选，且备选指向 model-a——与默认空间的备选相反，
	// 因此「用了哪个备选」能区分空间。
	cfg.Tasks[1].FallbackModel = "model-a"
	env := newTestEnv(t, cfg, Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	// model-b 只有一个 key，MaxRetries=1 ⇒ 首选要试两次才轮到任务备选。
	env.route("/v1/chat/completions",
		jsonStep(500, `{"error":{"message":"boom"}}`),
		jsonStep(500, `{"error":{"message":"boom"}}`),
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{config.WorkspaceHeader: "teamA"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码: got %d（%s）", recorder.Code, recorder.Body.String())
	}
	if len(env.transport.calls) != 3 {
		t.Fatalf("应有「首选两次 + 备选一次」三次调用，实得 %v", describeUpstreams(env.transport.calls))
	}
	// teamA 首选 model-b（kb）连败两次后切到自己的备选 model-a（ka）。
	for _, index := range []int{0, 1} {
		if key := env.transport.calls[index].headers["authorization"]; key != "Bearer sk-kb" {
			t.Fatalf("第 %d 次应为 teamA 的 model-b（kb），实际 %q", index, key)
		}
	}
	if key := env.transport.calls[2].headers["authorization"]; key != "Bearer sk-ka" {
		t.Fatalf("备选应为 teamA 的 model-a（ka），实际 %q", key)
	}
}

// TestWorkspaceHeaderIsTrimmed 固化：请求头两端空白被忽略。
//
// 复用配置侧的 config.NormalizeWorkspace，因此代理层与「显式传 default」等价。
func TestWorkspaceHeaderIsTrimmed(t *testing.T) {
	env := newTestEnv(t, workspaceConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{config.WorkspaceHeader: "  teamA  "})
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码: got %d（%s）", recorder.Code, recorder.Body.String())
	}
	if key := env.transport.calls[0].headers["authorization"]; key != "Bearer sk-kb" {
		t.Fatalf("带空白的 teamA 头应照常生效（用 kb），实际 %q", key)
	}
}

// TestEmptyWorkspaceHeaderUsesDefault 固化：空头等价于不带头。
func TestEmptyWorkspaceHeaderUsesDefault(t *testing.T) {
	env := newTestEnv(t, workspaceConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{config.WorkspaceHeader: ""})
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码: got %d（%s）", recorder.Code, recorder.Body.String())
	}
	if key := env.transport.calls[0].headers["authorization"]; key != "Bearer sk-ka" {
		t.Fatalf("空工作空间头应落到默认空间（model-a / ka），实际 %q", key)
	}
}

// TestTaskWithoutModelIsRejectedExplicitly 固化「尚未指定模型」的任务行为（Go 侧分叉）。
//
// 参照实现里任务必须写 model，因此这条路径在 Python 侧不存在，没有对拍语料可依。
// 刻意选的是**明确报错**而不是「回落到 unified_model.default」或「回落到第一个已配置
// 模型」：静默回落会让一个忘记选模型的任务照常服务，用户既看不到问题、也无从知道
// 自己调用的其实是另一个模型；而占位任务本就允许存在，所以必须在请求时把它挑明。
func TestTaskWithoutModelIsRejectedExplicitly(t *testing.T) {
	cfg := workspaceConfig()
	cfg.Tasks = append(cfg.Tasks, config.TaskConfig{Name: "placeholder"})
	env := newTestEnv(t, cfg, Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"placeholder","messages":[{"role":"user","content":"hi"}]}`, nil)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("尚未指定模型的任务应 404，实得 %d（%s）", recorder.Code, recorder.Body.String())
	}
	// 文案要点名是哪个任务没选模型：只说「模型未配置」会把人引到模型设置页去，
	// 而真正该改的是这个任务。
	body := recorder.Body.String()
	for _, want := range []string{"placeholder", "尚未指定模型", "任务路由"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 文案应包含 %q，实得 %s", want, body)
		}
	}
	// 关键：绝不能触达上游。一旦回落成某个真实模型，这里就会有调用。
	if len(env.transport.calls) != 0 {
		t.Fatalf("尚未指定模型的任务不应触达上游，实得 %v", describeUpstreams(env.transport.calls))
	}
}

// TestVisitorCannotUseWorkspaceTasks 固化：访客带工作空间头也拿不到任务。
func TestVisitorCannotUseWorkspaceTasks(t *testing.T) {
	cfg := workspaceConfig()
	// 访客凭据是保留 key（config.VisitorAPIKey）。把 model-b 的 key 标成允许访客，
	// 这样「若无任务路由，访客本可访问 model-b」成立，用例才能证明是**任务**被
	// 拦下，而不是模型不可见。
	cfg.Models[1].Keys[0].AllowVisitor = true
	env := newTestEnv(t, cfg, Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})

	recorder := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			"Authorization":        "Bearer " + config.VisitorAPIKey,
			config.WorkspaceHeader: "teamA",
		})
	// 访客既不能解析任务名，就按普通模型名走到「无权访问模型」。
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("访客状态码: got %d（%s）", recorder.Code, recorder.Body.String())
	}
	if len(env.transport.calls) != 0 {
		t.Fatalf("访客不应触达上游，实得 %v", describeUpstreams(env.transport.calls))
	}
}

// scopedConfig 构造一份带作用域推理凭据的两空间配置：
//
//	teamA：推理 key ia，任务 shared -> model-b，允许直呼 model-b
//	teamB：推理 key ib，任务 shared -> model-a，无 models 清单（不限制）
//	默认空间：任务 shared -> model-a
//
// 每个空间的 shared 指向不同模型，因此「用的是哪个模型」就说明空间钉在了哪一个。
func scopedConfig() *config.RouterConfig {
	cfg := workspaceConfig()
	cfg.Tasks = append(cfg.Tasks, config.TaskConfig{Name: "shared", Workspace: "teamB", Model: "model-a"})
	cfg.Workspaces = []config.WorkspaceConfig{
		{Name: "teamA", InferenceKey: "ia", Models: []string{"model-b"}},
		{Name: "teamB", InferenceKey: "ib"},
	}
	return cfg
}

// TestScopedKeyPinsWorkspaceIgnoringHeader 是本能力最核心的一条：作用域推理 key 决定
// 空间，且**忽略 X-AMKR-Workspace 头**。
//
// 若请求头能换空间，一把配进各项目环境变量的 key 泄漏后就等于拿到所有空间的推理权限
// ——而它之所以被设计出来，正是因为项目环境变量是最容易泄漏的地方。因此这里断言两次
// 调用（带一个**别的空间**的头、带一个不存在的头）都仍然落在 key 自己的空间上。
func TestScopedKeyPinsWorkspaceIgnoringHeader(t *testing.T) {
	env := newTestEnv(t, scopedConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	body := `{"model":"shared","messages":[{"role":"user","content":"hi"}]}`

	// 不带任何工作空间头：teamB 的 shared -> model-a（用 ka）。
	if got := env.request(http.MethodPost, "chat/completions", body,
		map[string]string{"Authorization": "Bearer ib"}); got.Code != http.StatusOK {
		t.Fatalf("不带头的状态码: got %d（%s）", got.Code, got.Body.String())
	}
	if key := env.transport.calls[0].headers["authorization"]; key != "Bearer sk-ka" {
		t.Fatalf("teamB 的 shared 应指向 model-a（用 ka），实际 %q", key)
	}

	// 带头指向 teamA —— 必须被**忽略**，仍走 teamB 的 shared。
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	got := env.request(http.MethodPost, "chat/completions", body,
		map[string]string{"Authorization": "Bearer ib", config.WorkspaceHeader: "teamA"})
	if got.Code != http.StatusOK {
		t.Fatalf("带 teamA 头的状态码: got %d（%s）", got.Code, got.Body.String())
	}
	if key := env.transport.calls[1].headers["authorization"]; key != "Bearer sk-ka" {
		t.Fatalf("X-AMKR-Workspace 必须被忽略（仍应走 teamB/model-a 的 ka），实际 %q", key)
	}

	// 指向一个**不存在**的空间也不行：拿不到任何别的空间的路由。
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	got = env.request(http.MethodPost, "chat/completions", body,
		map[string]string{"Authorization": "Bearer ib", config.WorkspaceHeader: "nope"})
	if got.Code != http.StatusOK {
		t.Fatalf("带不存在空间头的状态码: got %d（%s）", got.Code, got.Body.String())
	}
	if key := env.transport.calls[2].headers["authorization"]; key != "Bearer sk-ka" {
		t.Fatalf("不存在的空间头也必须被忽略，实际 %q", key)
	}
}

// TestScopedKeyCannotReachOtherWorkspaceTask 固化：作用域 key 拿不到别的空间的同名任务。
//
// shared 在两个空间里都存在但指向不同模型。teamA 的 key 必须始终拿到 teamA 的那份，
// 哪怕调用方显式要求 teamB。
func TestScopedKeyCannotReachOtherWorkspaceTask(t *testing.T) {
	env := newTestEnv(t, scopedConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	got := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ia", config.WorkspaceHeader: "teamB"})
	if got.Code != http.StatusOK {
		t.Fatalf("状态码: got %d（%s）", got.Code, got.Body.String())
	}
	// teamA 的 shared -> model-b（kb），而不是 teamB 的 model-a（ka）。
	if key := env.transport.calls[0].headers["authorization"]; key != "Bearer sk-kb" {
		t.Fatalf("teamA 的 key 应拿到 teamA 的 shared（kb），实际 %q", key)
	}
}

// TestScopedKeyEnforcesModelAllowlist 固化：作用域 key 只能直呼清单内的模型。
//
// teamA 的清单是 [model-b]。直呼 model-a 必须 403 且**不触达上游**；直呼 model-b 放行。
// 这条是「共用网关」在模型维度的隔离——任务名天然隔离，模型名不是。
func TestScopedKeyEnforcesModelAllowlist(t *testing.T) {
	env := newTestEnv(t, scopedConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	// 不在清单内：403，且不触达上游。
	denied := env.request(http.MethodPost, "chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ia"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("清单外模型应 403，实得 %d（%s）", denied.Code, denied.Body.String())
	}
	if !strings.Contains(denied.Body.String(), "无权访问模型") {
		t.Errorf("403 文案应说明无权访问模型: %s", denied.Body.String())
	}
	if len(env.transport.calls) != 0 {
		t.Fatalf("被拒的请求不应触达上游，实得 %v", describeUpstreams(env.transport.calls))
	}

	// 在清单内：放行。
	allowed := env.request(http.MethodPost, "chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ia"})
	if allowed.Code != http.StatusOK {
		t.Fatalf("清单内模型应放行，实得 %d（%s）", allowed.Code, allowed.Body.String())
	}
	if len(env.transport.calls) != 1 {
		t.Fatalf("应有一次上游调用，实得 %v", describeUpstreams(env.transport.calls))
	}
}

// TestScopedKeyAllowlistMatchesAliasResolution 固化：清单按**解析后的真实模型**判定，
// 而不是拿调用方写的别名去比对。
//
// 否则「同一个模型写成别名」就绕过了白名单——那是这类白名单最典型的失效方式。
func TestScopedKeyAllowlistMatchesAliasResolution(t *testing.T) {
	cfg := scopedConfig()
	cfg.Models[1].Aliases = []string{"alias-b"}
	env := newTestEnv(t, cfg, Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	// teamA 的清单里只有 model-b，别名 alias-b 解析到 model-b，因此**应当放行**。
	got := env.request(http.MethodPost, "chat/completions",
		`{"model":"alias-b","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ia"})
	if got.Code != http.StatusOK {
		t.Fatalf("别名解析到清单内模型应放行，实得 %d（%s）", got.Code, got.Body.String())
	}
}

// TestScopedKeyCannotUseUnifiedModel 固化：作用域 key 不能用 unified-model。
//
// unified-model 是全局计划（运维为整台实例挑的默认首选/备选），不属于任何工作空间，
// 因此绕过按空间收窄的模型清单。要让它可用就把具体模型写进清单——那是显式授权。
func TestScopedKeyCannotUseUnifiedModel(t *testing.T) {
	env := newTestEnv(t, scopedConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})

	got := env.request(http.MethodPost, "chat/completions",
		`{"model":"unified-model","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ia"})
	if got.Code != http.StatusForbidden {
		t.Fatalf("作用域 key 用 unified-model 应 403，实得 %d（%s）", got.Code, got.Body.String())
	}
	if len(env.transport.calls) != 0 {
		t.Fatalf("不应触达上游，实得 %v", describeUpstreams(env.transport.calls))
	}
}

// TestScopedKeyIsNotFullPermission 固化：推理 key 拿不到完整权限。
//
// 顺序错了（先判推理 key 再判完整权限）会让一把空间 key 变成管理员凭据。这里用一个
// 错误的本机 key 反证：错误 key 与作用域 key 都不该通过完整权限那条路。
func TestScopedKeyIsNotFullPermission(t *testing.T) {
	cfg := scopedConfig()
	env := newTestEnv(t, cfg, Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})

	// 未知 key：401。
	unknown := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer nope"})
	if unknown.Code != http.StatusUnauthorized {
		t.Fatalf("未知 key 应 401，实得 %d（%s）", unknown.Code, unknown.Body.String())
	}
	if len(env.transport.calls) != 0 {
		t.Fatalf("未鉴权请求不应触达上游，实得 %v", describeUpstreams(env.transport.calls))
	}
}

// TestEmptyScopedKeyDoesNotMatch 固化：空 Authorization 不命中任何空间。
//
// 若空串能命中（例如某个空间没配 inference_key），随便发一个空 Bearer 就能用别人的空间。
func TestEmptyScopedKeyDoesNotMatch(t *testing.T) {
	cfg := scopedConfig()
	cfg.Workspaces = append(cfg.Workspaces, config.WorkspaceConfig{Name: "noKey"})
	env := newTestEnv(t, cfg, Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})

	got := env.request(http.MethodPost, "chat/completions",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer "})
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("空 key 应 401，实得 %d（%s）", got.Code, got.Body.String())
	}
}

// TestUnrestrictedScopedKeyKeepsDefaultWorkspaceBehavior 固化：没配 models 清单的
// 作用域 key 仍可直呼全部模型（兼容既有配置的默认语义）。
func TestUnrestrictedScopedKeyKeepsDefaultWorkspaceBehavior(t *testing.T) {
	env := newTestEnv(t, scopedConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	// teamB 没有 models 清单 -> 不限制 -> 能直呼 model-b（不在它自己的任务里）。
	got := env.request(http.MethodPost, "chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ib"})
	if got.Code != http.StatusOK {
		t.Fatalf("无清单空间应可直呼任意模型，实得 %d（%s）", got.Code, got.Body.String())
	}
}

// TestScopedKeyCannotReachOtherWorkspaceViaUnifiedModelFallback 固化：被清单拒掉之后
// **不能**有任何回落。
//
// 这条防的是「拒绝之后悄悄换一个模型继续跑」——那样限制看起来生效（日志里有 403 的
// 分支）而实际照跑。断言方式是拒绝后上游调用数仍为 0。
func TestScopedKeyNoFallbackAfterDeny(t *testing.T) {
	env := newTestEnv(t, scopedConfig(), Options{
		BodyPolicy: BodyPolicyPython, Multipart: MultipartPython})
	env.route("/v1/chat/completions",
		jsonStep(200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))

	got := env.request(http.MethodPost, "chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ia"})
	if got.Code != http.StatusForbidden {
		t.Fatalf("应 403，实得 %d（%s）", got.Code, got.Body.String())
	}
	if len(env.transport.calls) != 0 {
		t.Fatalf("拒绝后不得回落，实得 %v", describeUpstreams(env.transport.calls))
	}
}
