package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件固化 /api/workspaces* 的语义（Go 侧新增能力，无对拍语料）。
//
// 与 workspace_test.go 的分工：那边管 /api/tasks* 的**空间隔离**（用请求头选空间），
// 这里管**空间自身**的增删改查（空间名是路径参数）。

// workspaceList 是 GET /api/workspaces 的响应形状。
//
// Models 用 *[]string：目录里**没有** models 字段表示「不限制」，有该字段（哪怕是空
// 数组）表示显式授权清单。这个区别对界面是有意义的，因此这里也保留指针语义。
type workspaceList struct {
	Workspaces []struct {
		Name            string    `json:"name"`
		TaskCount       int       `json:"task_count"`
		HasInferenceKey bool      `json:"has_inference_key"`
		Models          *[]string `json:"models"`
	} `json:"workspaces"`
}

// listWorkspaces 发一条 GET /api/workspaces 并解析。
func listWorkspaces(t *testing.T, server *Server) (workspaceList, string) {
	t.Helper()
	recorder := callTasks(t, server, http.MethodGet, "/api/workspaces", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/workspaces 状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	var payload workspaceList
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析目录失败: %v（body=%s）", err, recorder.Body.String())
	}
	return payload, recorder.Body.String()
}

// callWorkspace 发一条针对某个空间的请求（空间名走路径参数）。
func callWorkspace(t *testing.T, server *Server, method, workspace, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callTasks(t, server, method, "/api/workspaces/"+workspace, "", body)
}

// TestListWorkspacesReturnsCatalog 固化目录的内容与形状。
func TestListWorkspacesReturnsCatalog(t *testing.T) {
	server, _ := workspaceServer(t)
	payload, raw := listWorkspaces(t, server)

	if len(payload.Workspaces) != 2 {
		t.Fatalf("工作空间数量 = %d，期望 2（默认 + teamA）：%s", len(payload.Workspaces), raw)
	}
	// 默认空间固定排第一：前端直接拿它当下拉的默认选中项。
	if payload.Workspaces[0].Name != "default" || payload.Workspaces[0].TaskCount != 2 {
		t.Errorf("首项应为 default/2，实际 %+v", payload.Workspaces[0])
	}
	if payload.Workspaces[1].Name != "teamA" || payload.Workspaces[1].TaskCount != 1 {
		t.Errorf("次项应为 teamA/1，实际 %+v", payload.Workspaces[1])
	}
	// 形状与字段顺序都要稳定（前端按 name 渲染下拉项）。
	if !strings.HasPrefix(raw, `{"workspaces":[{"name":"default","task_count":2,"has_inference_key":false}`) {
		t.Errorf("响应形状与字段顺序不符: %s", raw)
	}
	// 目录里**不能**出现任何凭据：它是列表接口，把 key 随列表发出去等于让每次 GET
	// 都成了取 key 的入口。has_inference_key 只报「发过没有」。
	if strings.Contains(raw, "amkr_ws_") || strings.Contains(raw, "amkr_ik_") {
		t.Errorf("目录不应泄漏凭据: %s", raw)
	}
	// 没有配置 models 的空间**不带**这个字段（表示不限制），而不是给一个空数组。
	if payload.Workspaces[0].Models != nil {
		t.Errorf("未配置模型清单的空间不应带 models 字段: %+v", payload.Workspaces[0])
	}
	// 与其它管理端点一致：响应里带当前配置版本号。
	if !strings.Contains(raw, `"config_revision":"`) {
		t.Errorf("响应应带 config_revision: %s", raw)
	}
}

// TestListWorkspacesEmptyCatalogStillListsDefault 固化：一个任务都不剩时仍列出默认空间。
//
// 默认空间是调用方不带 X-AMKR-Workspace 头时命中的那个，界面上必须可见、可选，
// 哪怕它一个任务都没有。
func TestListWorkspacesEmptyCatalogStillListsDefault(t *testing.T) {
	server, path := workspaceServer(t)
	// 把两个空间的任务都删光，配置里就一个命名空间都不剩。
	for _, doomed := range []struct{ workspace, task string }{
		{"", "shared"}, {"", "default-only"}, {"teamA", "team-a-only"},
	} {
		revision := currentRevision(t, path)
		recorder := callTasks(t, server, http.MethodDelete, "/api/tasks/"+doomed.task, doomed.workspace,
			`{"config_revision":"`+revision+`"}`)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("删除 %s/%s 状态码 = %d（body=%s）", doomed.workspace, doomed.task,
				recorder.Code, recorder.Body.String())
		}
	}

	payload, raw := listWorkspaces(t, server)
	if len(payload.Workspaces) != 1 || payload.Workspaces[0].Name != config.DefaultWorkspace {
		t.Errorf("无任务时也应只列出 default，实际 %s", raw)
	}
	if payload.Workspaces[0].TaskCount != 0 {
		t.Errorf("默认空间任务数 = %d，期望 0", payload.Workspaces[0].TaskCount)
	}
}

// TestRenameWorkspaceMovesTasksAcrossConfig 固化改名：组内任务整体跟着走。
func TestRenameWorkspaceMovesTasksAcrossConfig(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	recorder := callWorkspace(t, server, http.MethodPut, "teamA",
		`{"config_revision":"`+revision+`","name":"teamB"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("改名状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 响应回的是新名字与新任务数（调用方据此刷新下拉，不必再查一次）。
	if !strings.Contains(recorder.Body.String(), `"name":"teamB"`) ||
		!strings.Contains(recorder.Body.String(), `"task_count":1`) {
		t.Errorf("改名响应应回新名字与任务数: %s", recorder.Body.String())
	}

	// 配置里只剩 teamB，teamA 消失，任务内容一字未改。
	dumped := canonical.Dumps(readConfigData(t, path))
	if strings.Contains(dumped, `"teamA"`) {
		t.Errorf("旧名字应从配置里消失: %s", dumped)
	}
	if !strings.Contains(dumped, `"teamB"`) {
		t.Errorf("新名字应写进配置: %s", dumped)
	}
	data := readConfigData(t, path)
	task, err := config.FromDict(data)
	if err != nil {
		t.Fatalf("改名后的配置应可解析: %v", err)
	}
	moved, found := task.TaskForWorkspace("teamB", "team-a-only")
	if !found || moved.Model != "model-b" {
		t.Errorf("任务应原样搬到 teamB，实际 %+v（found=%v）", moved, found)
	}
	// 默认空间的两个任务不受影响。
	if got := len(defaultTaskNames(t, server)); got != 2 {
		t.Errorf("默认空间任务数 = %d，期望 2", got)
	}
}

// TestRenameWorkspaceConflictsAndMissing 固化改名的拒绝路径。
func TestRenameWorkspaceConflictsAndMissing(t *testing.T) {
	server, path := workspaceServer(t)

	// 目标名已被占用 -> 409，且两边都不动。
	revision := currentRevision(t, path)
	recorder := callWorkspace(t, server, http.MethodPut, "teamA",
		`{"config_revision":"`+revision+`","name":"default"}`)
	if recorder.Code != http.StatusConflict {
		t.Errorf("改名为默认空间应 409，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}

	// 源空间不存在（没有任务）-> 404。
	recorder = callWorkspace(t, server, http.MethodPut, "nope",
		`{"config_revision":"`+revision+`","name":"whatever"}`)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("改名不存在的空间应 404，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}

	// 默认空间改不了 -> 400。
	recorder = callWorkspace(t, server, http.MethodPut, "default",
		`{"config_revision":"`+revision+`","name":"moved"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("改名默认空间应 400，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	assertConfigUnchanged(t, path, workspaceFixture)
}

// TestRenameWorkspaceValidatesPayload 固化请求体校验与版本号。
func TestRenameWorkspaceValidatesPayload(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	// 缺 name -> 422（Pydantic 风格）。
	recorder := callWorkspace(t, server, http.MethodPut, "teamA",
		`{"config_revision":"`+revision+`"}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("缺 name 应 422，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 过期版本号 -> 409。
	recorder = callWorkspace(t, server, http.MethodPut, "teamA",
		`{"config_revision":"stale","name":"teamB"}`)
	if recorder.Code != http.StatusConflict {
		t.Errorf("过期版本号应 409，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 名字全空白 -> 422。刻意先判非空再归一化：归一化会把空白悄悄变成默认空间名，
	// 让「改名为一个没写出来的名字」表现成「改名为 default」。
	recorder = callWorkspace(t, server, http.MethodPut, "teamA",
		`{"config_revision":"`+revision+`","name":"   "}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("改名为空白应 422，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}

	// 路径里的空间名为空白（URL 编码）-> 422，而不是静默落到默认空间上。
	recorder = callTasks(t, server, http.MethodPut, "/api/workspaces/%20%20", "",
		`{"config_revision":"`+revision+`","name":"teamB"}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("空白的路径空间名应 422，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	assertConfigUnchanged(t, path, workspaceFixture)
}

// TestDeleteWorkspaceRemovesTasks 固化删除：整组任务与分组一起消失。
func TestDeleteWorkspaceRemovesTasks(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	recorder := callWorkspace(t, server, http.MethodDelete, "teamA",
		`{"config_revision":"`+revision+`"}`)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("删除状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "" {
		t.Errorf("204 不应带响应体，实际 %q", body)
	}

	dumped := canonical.Dumps(readConfigData(t, path))
	if strings.Contains(dumped, "teamA") || strings.Contains(dumped, "workspaces") {
		t.Errorf("删空的命名空间与 workspaces 段都应消失: %s", dumped)
	}
	// 默认空间的两个任务一个都不能少。
	if names := defaultTaskNames(t, server); len(names) != 2 {
		t.Errorf("默认空间任务不应受影响，实际 %v", names)
	}
	// 目录里只剩默认空间。
	payload, _ := listWorkspaces(t, server)
	if len(payload.Workspaces) != 1 || payload.Workspaces[0].Name != config.DefaultWorkspace {
		t.Errorf("删除后目录应只剩 default，实际 %+v", payload.Workspaces)
	}
}

// TestDeleteWorkspaceRejectsDefaultAndMissing 固化删除的拒绝路径。
func TestDeleteWorkspaceRejectsDefaultAndMissing(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	recorder := callWorkspace(t, server, http.MethodDelete, "default",
		`{"config_revision":"`+revision+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("删除默认空间应 400，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	recorder = callWorkspace(t, server, http.MethodDelete, "nope",
		`{"config_revision":"`+revision+`"}`)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("删除不存在的空间应 404，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	assertConfigUnchanged(t, path, workspaceFixture)
}

// TestWorkspacesRequireAuthAndRejectMethods 固化鉴权与错方法契约。
func TestWorkspacesRequireAuthAndRejectMethods(t *testing.T) {
	server, _ := workspaceServer(t)

	// 访客与无凭据都拿不到（工作空间会暴露配置结构）。
	for _, auth := range []string{"none", "visitor"} {
		recorder := opsRequest(t, server, http.MethodGet, "/api/workspaces", nil, auth)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("凭据 %q 的状态码 = %d，期望 401", auth, recorder.Code)
		}
	}

	// 错方法：405 + Allow，且 405 判定在鉴权之前。GET 与 POST 都已注册，因此这里
	// 拿一个两条都没注册的方法来探（PATCH 落在集合上）。
	recorder := callTasks(t, server, http.MethodPatch, "/api/workspaces", "", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH /api/workspaces 应 405，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// Allow 只报**第一个**路径匹配的模式（allowedMethod 的既有语义，与参照实现一致：
	// PUT /api/models 在多方法路径上也只回 "GET"）。GET 注册在 POST 之前，因此这里
	// 是 "GET"，而不是 "GET, POST"。
	if got := recorder.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q，期望 GET", got)
	}
	recorder = opsRequest(t, server, http.MethodPatch, "/api/workspaces/teamA", nil, "none")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH /api/workspaces/teamA 应 405（鉴权前判定），实际 %d", recorder.Code)
	}
}

// panelCall 用某个空间的面板 key 发一条任务面请求，可带伪造的工作空间头。
func panelCall(t *testing.T, server *Server, method, path, apiKey, workspace, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if workspace != "" {
		request.Header.Set(config.WorkspaceHeader, workspace)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// TestCreateWorkspaceReturnsKey 固化显式建空间：拿到 key，且空间立刻可见。
func TestCreateWorkspaceReturnsKey(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	recorder := callTasks(t, server, http.MethodPost, "/api/workspaces", "",
		`{"config_revision":"`+revision+`","name":"panel"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("建空间状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Name         string `json:"name"`
		TaskCount    int    `json:"task_count"`
		APIKey       string `json:"api_key"`
		InferenceKey string `json:"inference_key"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析建空间响应失败: %v（body=%s）", err, recorder.Body.String())
	}
	if created.Name != "panel" || created.TaskCount != 0 {
		t.Errorf("响应应为 panel/0，实际 %+v", created)
	}
	// key 必须是服务端生成的 amkr_ws_ 前缀（WebUI 没传 api_key）。
	if !strings.HasPrefix(created.APIKey, "amkr_ws_") || len(created.APIKey) != len("amkr_ws_")+43 {
		t.Errorf("生成的 key 形状不符: %q", created.APIKey)
	}
	// 推理 key 与面板 key **同时**发出来：建空间是唯一能拿到明文 key 的时刻，
	// 少了它应用侧就还得再找一条路要推理凭据。
	if !strings.HasPrefix(created.InferenceKey, "amkr_ik_") || len(created.InferenceKey) != len("amkr_ik_")+43 {
		t.Errorf("生成的推理 key 形状不符: %q", created.InferenceKey)
	}
	if created.InferenceKey == created.APIKey {
		t.Errorf("两把 key 不能是同一个字符串: %q", created.InferenceKey)
	}

	// 空空间必须留住：任务数为 0 但仍在目录里（这是对「空分组不进配置」的有意放宽）。
	payload, raw := listWorkspaces(t, server)
	found := false
	for _, item := range payload.Workspaces {
		if item.Name == "panel" {
			found = true
			if item.TaskCount != 0 {
				t.Errorf("panel 的任务数 = %d，期望 0", item.TaskCount)
			}
			if !item.HasInferenceKey {
				t.Error("目录应报告这个空间已发过推理 key")
			}
		}
	}
	if !found {
		t.Errorf("新建的空空间应出现在目录里: %s", raw)
	}
	// 目录里绝不能回 key（否则任何一次 GET 都成了取凭据的入口）。
	if strings.Contains(raw, created.APIKey) || strings.Contains(raw, created.InferenceKey) {
		t.Errorf("目录响应不应包含任何 key: %s", raw)
	}

	// 配置里落的是**两把** key，且任务确实一个都没有。
	dumped := canonical.Dumps(readConfigData(t, path))
	if !strings.Contains(dumped, `"api_key":"`+created.APIKey+`"`) {
		t.Errorf("面板 key 应写进配置: %s", dumped)
	}
	if !strings.Contains(dumped, `"inference_key":"`+created.InferenceKey+`"`) {
		t.Errorf("推理 key 应写进配置: %s", dumped)
	}
}

// TestCreateWorkspaceAcceptsCallerKey 固化应用侧自带 key：原样采用，不改写。
func TestCreateWorkspaceAcceptsCallerKey(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	recorder := callTasks(t, server, http.MethodPost, "/api/workspaces", "",
		`{"config_revision":"`+revision+`","name":"app","api_key":"app-supplied-key"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("建空间状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"api_key":"app-supplied-key"`) {
		t.Errorf("应采用调用方给的 key: %s", recorder.Body.String())
	}

	// 该 key 立刻能用：读自己的空间。
	response := panelCall(t, server, http.MethodGet, "/api/tasks", "app-supplied-key", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("面板 key 读任务状态码 = %d（body=%s）", response.Code, response.Body.String())
	}
}

// TestCreateWorkspaceRejectsBadInput 固化建空间的拒绝路径。
func TestCreateWorkspaceRejectsBadInput(t *testing.T) {
	server, path := workspaceServer(t)

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantText string
	}{
		{"与默认空间重名", `"name":"default"`, http.StatusConflict, "工作空间名重复: default"},
		{"已存在", `"name":"teamA"`, http.StatusConflict, "工作空间已存在: teamA"},
		{"占用访客 key", `"name":"x","api_key":"amkr-visitor"`, http.StatusUnprocessableEntity, "不能使用保留的访客 key"},
		{"与本地 key 相同", `"name":"x","api_key":"local-key"`, http.StatusUnprocessableEntity, "不能与 local_api_key 相同"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			revision := currentRevision(t, path)
			recorder := callTasks(t, server, http.MethodPost, "/api/workspaces", "",
				`{"config_revision":"`+revision+`",`+testCase.body+`}`)
			if recorder.Code != testCase.wantCode {
				t.Fatalf("状态码 = %d，期望 %d（body=%s）", recorder.Code, testCase.wantCode, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.wantText) {
				t.Errorf("错误文本应含 %q: %s", testCase.wantText, recorder.Body.String())
			}
		})
	}
	// 全部被拒绝后配置一字未改。
	assertConfigUnchanged(t, path, workspaceFixture)
}

// TestWorkspaceKeyIsPinnedToItsWorkspace 固化面板 key 的核心语义：钉死在那个空间。
//
// 三条必须同时成立：
//  1. 能读写自己空间的任务；
//  2. **伪造的 X-AMKR-Workspace 头无效**——空间由 key 决定，否则这个模式形同虚设；
//  3. 拿不到别的管理面（配置、供应商、设置），也进不了 /v1 代理。
func TestWorkspaceKeyIsPinnedToItsWorkspace(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)
	recorder := callTasks(t, server, http.MethodPost, "/api/workspaces", "",
		`{"config_revision":"`+revision+`","name":"panel","api_key":"panel-key"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("建空间失败: %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	const key = "panel-key"

	// 1+2. 建任务时带上伪造的头，任务必须落在 panel 而不是请求头说的 teamA。
	revision = currentRevision(t, path)
	created := panelCall(t, server, http.MethodPost, "/api/tasks", key, "teamA",
		`{"config_revision":"`+revision+`","name":"from-panel","model":"model-b"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("面板建任务状态码 = %d（body=%s）", created.Code, created.Body.String())
	}
	cfg, err := config.FromDict(readConfigData(t, path))
	if err != nil {
		t.Fatalf("配置应可解析: %v", err)
	}
	if _, found := cfg.TaskForWorkspace("panel", "from-panel"); !found {
		t.Error("任务应落在 panel，而不是请求头伪造的 teamA")
	}
	if _, found := cfg.TaskForWorkspace("teamA", "from-panel"); found {
		t.Error("伪造的 X-AMKR-Workspace 头不该生效")
	}

	// 读列表：panel 有 1 个任务（自己建的）。
	listed := panelCall(t, server, http.MethodGet, "/api/tasks", key, "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("面板读列表状态码 = %d", listed.Code)
	}
	names := taskNames(t, listed.Body.String())
	if len(names) != 1 || names[0] != "from-panel" {
		t.Errorf("面板只应看到自己的任务，实际 %v", names)
	}

	// 3. 其余管理面一律拒绝。
	for _, blocked := range []struct{ method, path string }{
		{http.MethodGet, "/api/workspaces"},
		{http.MethodGet, "/api/settings"},
		{http.MethodGet, "/api/providers"},
		{http.MethodGet, "/api/models"},
		{http.MethodGet, "/api/unified-model"},
		{http.MethodPost, "/api/config/export"},
	} {
		response := panelCall(t, server, blocked.method, blocked.path, key, "", "")
		if response.Code != http.StatusUnauthorized {
			t.Errorf("面板 key %s %s 应 401，实际 %d（body=%s）",
				blocked.method, blocked.path, response.Code, response.Body.String())
		}
	}
	// /api/workspaces 的写路径同样拒绝（不能自己建/删空间）。
	revision = currentRevision(t, path)
	post := panelCall(t, server, http.MethodPost, "/api/workspaces", key, "",
		`{"config_revision":"`+revision+`","name":"sneaky"}`)
	if post.Code != http.StatusUnauthorized {
		t.Errorf("面板 key 建空间应 401，实际 %d", post.Code)
	}

	// /v1 代理不接受面板 key（面板不是调用方凭据）。
	proxied := panelCall(t, server, http.MethodPost, "/v1/chat/completions", key, "",
		`{"model":"from-panel"}`)
	if proxied.Code == http.StatusOK {
		t.Errorf("/v1 不应接受面板 key（实际 %d）", proxied.Code)
	}

	// 别的空间的面板 key 看不到这个空间的任务：再建一个空间交叉验证。
	revision = currentRevision(t, path)
	callTasks(t, server, http.MethodPost, "/api/workspaces", "",
		`{"config_revision":"`+revision+`","name":"other","api_key":"other-key"}`)
	otherList := panelCall(t, server, http.MethodGet, "/api/tasks", "other-key", "", "")
	if otherList.Code != http.StatusOK {
		t.Fatalf("other 面板读列表状态码 = %d", otherList.Code)
	}
	if names := taskNames(t, otherList.Body.String()); len(names) != 0 {
		t.Errorf("other 空间不该看到 panel 的任务，实际 %v", names)
	}
}

// TestDeletedWorkspaceKeyStopsWorking 固化：空间删了，key 立刻失效。
func TestDeletedWorkspaceKeyStopsWorking(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)
	callTasks(t, server, http.MethodPost, "/api/workspaces", "",
		`{"config_revision":"`+revision+`","name":"panel","api_key":"panel-key"}`)

	if response := panelCall(t, server, http.MethodGet, "/api/tasks", "panel-key", "", ""); response.Code != http.StatusOK {
		t.Fatalf("删除前面板应可用，实际 %d", response.Code)
	}

	revision = currentRevision(t, path)
	deleted := callWorkspace(t, server, http.MethodDelete, "panel",
		`{"config_revision":"`+revision+`"}`)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("删空间状态码 = %d（body=%s）", deleted.Code, deleted.Body.String())
	}
	if response := panelCall(t, server, http.MethodGet, "/api/tasks", "panel-key", "", ""); response.Code != http.StatusUnauthorized {
		t.Errorf("删除后 key 应失效（401），实际 %d", response.Code)
	}
}

// defaultTaskNames 取默认空间（不带请求头）的任务名，用于断言别的空间没被连累。
func defaultTaskNames(t *testing.T, server *Server) []string {
	t.Helper()
	recorder := callTasks(t, server, http.MethodGet, "/api/tasks", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks 状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	return taskNames(t, recorder.Body.String())
}

// TestSetWorkspaceModels 固化模型授权清单的三态与持久化。
//
// 三种取值必须可区分：数组（允许这些）、空数组（一个都不许）、null（清除限制）。
// 尤其 null 与 [] 都是「长度为零」，若实现里用长度判断，运维写下的禁令会被当成放开。
func TestSetWorkspaceModels(t *testing.T) {
	server, path := workspaceServer(t)
	// teamA 是 fixture 里已有的命名空间（有任务、无凭据）。
	set := func(body string) *httptest.ResponseRecorder {
		revision := currentRevision(t, path)
		return callWorkspace(t, server, http.MethodPut, "teamA/models",
			`{"config_revision":"`+revision+`",`+body+`}`)
	}

	if got := set(`"models":["model-a"]`); got.Code != http.StatusOK {
		t.Fatalf("设定清单状态码 = %d（body=%s）", got.Code, got.Body.String())
	}
	dumped := canonical.Dumps(readConfigData(t, path))
	if !strings.Contains(dumped, `"models":["model-a"]`) {
		t.Errorf("清单应写进配置: %s", dumped)
	}

	// 空数组：一条都不许直呼，是**已配置**状态（界面上必须区别于「不限制」）。
	if got := set(`"models":[]`); got.Code != http.StatusOK {
		t.Fatalf("空清单状态码 = %d（body=%s）", got.Code, got.Body.String())
	}
	payload, raw := listWorkspaces(t, server)
	for _, item := range payload.Workspaces {
		if item.Name == "teamA" {
			if item.Models == nil {
				t.Fatalf("空清单应是已配置状态（目录里带 models 字段）: %s", raw)
			}
			if len(*item.Models) != 0 {
				t.Errorf("空清单应为 0 项，实得 %v", *item.Models)
			}
		}
	}

	// null：清除限制，目录里**不再有** models 字段。
	if got := set(`"models":null`); got.Code != http.StatusOK {
		t.Fatalf("清除清单状态码 = %d（body=%s）", got.Code, got.Body.String())
	}
	payload, raw = listWorkspaces(t, server)
	for _, item := range payload.Workspaces {
		if item.Name == "teamA" && item.Models != nil {
			t.Errorf("清除后不应再带 models 字段: %s", raw)
		}
	}
	// 清除之后这个空间既没有凭据也没有清单，但仍有任务，所以还在目录里。
	if !strings.Contains(raw, `"name":"teamA"`) {
		t.Errorf("有任务的空间清除清单后仍应在目录里: %s", raw)
	}
}

// TestSetWorkspaceModelsRejectsUnknown 固化：引用未配置的模型当场 422。
//
// 写错一个名字会让该空间静默少一个可用模型，排查要翻两边配置。
func TestSetWorkspaceModelsRejectsUnknown(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)
	got := callWorkspace(t, server, http.MethodPut, "teamA/models",
		`{"config_revision":"`+revision+`","models":["nope"]}`)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("未知模型应 422，实得 %d（body=%s）", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "nope") {
		t.Errorf("错误应点名那个模型: %s", got.Body.String())
	}
	// 失败的请求不改配置。
	if strings.Contains(canonical.Dumps(readConfigData(t, path)), "nope") {
		t.Error("失败的请求不应写入配置")
	}
}

// TestSetWorkspaceModelsRejectsDefaultWorkspace 固化：默认空间不能配置模型清单。
//
// 默认空间没有作用域凭据（它靠不带 X-AMKR-Workspace 头命中），因此不存在「它的
// inference key」这回事——清单配了也没有对象去生效。
func TestSetWorkspaceModelsRejectsDefaultWorkspace(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)
	got := callWorkspace(t, server, http.MethodPut, "default/models",
		`{"config_revision":"`+revision+`","models":["model-a"]}`)
	if got.Code == http.StatusOK {
		t.Fatalf("默认空间不应能配置模型清单（body=%s）", got.Body.String())
	}
}

// TestRotateInferenceKey 固化推理 key 轮换：拿到新明文，旧值立即失效。
//
// 只换推理 key、**不动**面板 key：后者换掉会让已嵌入的页面立刻失效，两者轮换节奏不同。
func TestRotateInferenceKey(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)
	created := callTasks(t, server, http.MethodPost, "/api/workspaces", "",
		`{"config_revision":"`+revision+`","name":"rot","api_key":"panel-key"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("建空间状态码 = %d（body=%s）", created.Code, created.Body.String())
	}
	var before struct {
		InferenceKey string `json:"inference_key"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &before); err != nil {
		t.Fatalf("解析建空间响应失败: %v", err)
	}

	revision = currentRevision(t, path)
	rotated := callWorkspace(t, server, http.MethodPost, "rot/inference-key",
		`{"config_revision":"`+revision+`"}`)
	if rotated.Code != http.StatusOK {
		t.Fatalf("轮换状态码 = %d（body=%s）", rotated.Code, rotated.Body.String())
	}
	var after struct {
		Name         string `json:"name"`
		InferenceKey string `json:"inference_key"`
	}
	if err := json.Unmarshal(rotated.Body.Bytes(), &after); err != nil {
		t.Fatalf("解析轮换响应失败: %v（body=%s）", err, rotated.Body.String())
	}
	if after.Name != "rot" {
		t.Errorf("响应空间名 = %q，期望 rot", after.Name)
	}
	if !strings.HasPrefix(after.InferenceKey, "amkr_ik_") {
		t.Errorf("新推理 key 前缀不符: %q", after.InferenceKey)
	}
	if after.InferenceKey == before.InferenceKey {
		t.Errorf("轮换后 key 应变化: %q", after.InferenceKey)
	}

	// 配置里只剩新 key，旧 key 不再出现——旧凭据立即失效。
	dumped := canonical.Dumps(readConfigData(t, path))
	if !strings.Contains(dumped, after.InferenceKey) {
		t.Errorf("新 key 应写进配置: %s", dumped)
	}
	if strings.Contains(dumped, before.InferenceKey) {
		t.Errorf("旧 key 不应留在配置里: %s", dumped)
	}
	// 面板 key 不受影响。
	if !strings.Contains(dumped, `"api_key":"panel-key"`) {
		t.Errorf("轮换推理 key 不应动面板 key: %s", dumped)
	}
	// 面板 key 依然可用。
	if response := panelCall(t, server, http.MethodGet, "/api/tasks", "panel-key", "", ""); response.Code != http.StatusOK {
		t.Errorf("轮换后面板 key 应仍可用，实际 %d", response.Code)
	}
}

// TestRotateInferenceKeyUnknownWorkspace 固化：不存在的空间轮换报 404。
func TestRotateInferenceKeyUnknownWorkspace(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)
	got := callWorkspace(t, server, http.MethodPost, "nope/inference-key",
		`{"config_revision":"`+revision+`"}`)
	if got.Code != http.StatusNotFound {
		t.Fatalf("未知空间应 404，实得 %d（body=%s）", got.Code, got.Body.String())
	}
}

// TestScopedKeyEndpointsRejectPanelKey 固化：这两条端点只认**完整权限**。
//
// 空间面板 key 不能给自己加模型授权或换推理 key——那等于让被嵌入的页面自行扩权，
// 面板「只能读写这一个空间的任务与用量」这句承诺就没了。
func TestScopedKeyEndpointsRejectPanelKey(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)
	if got := callTasks(t, server, http.MethodPost, "/api/workspaces", "",
		`{"config_revision":"`+revision+`","name":"rot","api_key":"panel-key"}`); got.Code != http.StatusCreated {
		t.Fatalf("建空间失败: %d（%s）", got.Code, got.Body.String())
	}

	cases := []struct{ method, path, body string }{
		{http.MethodPut, "/api/workspaces/rot/models", `{"config_revision":"x","models":[]}`},
		{http.MethodPost, "/api/workspaces/rot/inference-key", `{"config_revision":"x"}`},
	}
	for _, item := range cases {
		response := panelCall(t, server, item.method, item.path, "panel-key", "", item.body)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 用面板 key 应 401，实得 %d（body=%s）",
				item.method, item.path, response.Code, response.Body.String())
		}
	}
}
