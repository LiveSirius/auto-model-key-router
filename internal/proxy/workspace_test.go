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
