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
type workspaceList struct {
	Workspaces []struct {
		Name      string `json:"name"`
		TaskCount int    `json:"task_count"`
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
	if !strings.HasPrefix(raw, `{"workspaces":[{"name":"default","task_count":2}`) {
		t.Errorf("响应形状与字段顺序不符: %s", raw)
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

	// 错方法：405 + Allow，且 405 判定在鉴权之前。
	recorder := callTasks(t, server, http.MethodPost, "/api/workspaces", "", `{}`)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/workspaces 应 405，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q，期望 GET", got)
	}
	recorder = opsRequest(t, server, http.MethodPatch, "/api/workspaces/teamA", nil, "none")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH /api/workspaces/teamA 应 405（鉴权前判定），实际 %d", recorder.Code)
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
