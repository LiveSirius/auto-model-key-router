package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件固化管理 API 的工作空间语义。
//
// 工作空间是 Go 侧新增的能力（参照实现没有），因此没有对拍语料。既有的 22 条
// /api/tasks* 语料（都不带 X-AMKR-Workspace 头）继续锁定「默认工作空间」的
// 兼容性——这里只补语料无法表达的部分。

// workspaceFixture 是一份两空间配置，teamA 与默认空间各有一个同名任务 shared。
const workspaceFixture = `{
  "config_version": 4,
  "local_api_key": "local-key",
  "ops_enabled": false,
  "webui_enabled": false,
  "host": "127.0.0.1",
  "port": 8000,
  "endpoint_capabilities_path": "caps.json",
  "metrics_db_path": "metrics.sqlite3",
  "log_file_path": "server.log",
  "providers": {
    "prov-a": {"base_url": "https://a.example.test", "keys": {"key-a": {"api_key": "sk-a"}}}
  },
  "models": {
    "model-a": {"targets": [{"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}]},
    "model-b": {"targets": [{"provider": "prov-a", "key": "key-a", "upstream_model": "model-b"}]}
  },
  "tasks": {"shared": {"model": "model-a"}, "default-only": {"model": "model-b"}},
  "workspaces": {"teamA": {"tasks": {"team-a-only": {"model": "model-b"}}}}
}`

// workspaceServer 装配一个指向临时配置文件的 Server，返回它与配置文件路径。
func workspaceServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	if err := os.WriteFile(path, []byte(workspaceFixture), 0o644); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	server := &Server{ConfigPath: path}
	return server, path
}

// callTasks 向管理 API 发一条请求，可带工作空间头。
func callTasks(t *testing.T, server *Server, method, path, workspace, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+corpusLocalAuthKey)
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

// taskNames 从任务列表响应里取出任务名。
func taskNames(t *testing.T, body string) []string {
	t.Helper()
	var payload struct {
		Tasks []struct {
			Name string `json:"name"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析任务列表失败: %v（body=%s）", err, body)
	}
	names := make([]string, 0, len(payload.Tasks))
	for _, task := range payload.Tasks {
		names = append(names, task.Name)
	}
	return names
}

// TestListTasksIsWorkspaceScoped 固化列表只回本空间的任务，且响应体形状不变。
func TestListTasksIsWorkspaceScoped(t *testing.T) {
	server, _ := workspaceServer(t)

	// 不带请求头 -> 默认空间。响应体形状必须与语料锁定的一致（没有 workspace 字段）。
	recorder := callTasks(t, server, http.MethodGet, "/api/tasks", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("默认空间状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	defaultNames := taskNames(t, recorder.Body.String())
	if len(defaultNames) != 2 || defaultNames[0] != "shared" || defaultNames[1] != "default-only" {
		t.Errorf("默认空间任务 %v，期望 [shared default-only]", defaultNames)
	}
	if strings.Contains(recorder.Body.String(), "team-a-only") {
		t.Errorf("默认空间的列表不应包含 teamA 的任务: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "workspace") {
		t.Errorf("响应体不应新增 workspace 字段（语料逐字节锁定）: %s", recorder.Body.String())
	}

	// 带 teamA 头 -> 只看到 teamA 的任务。
	recorder = callTasks(t, server, http.MethodGet, "/api/tasks", "teamA", "")
	teamNames := taskNames(t, recorder.Body.String())
	if len(teamNames) != 1 || teamNames[0] != "team-a-only" {
		t.Errorf("teamA 任务 %v，期望 [team-a-only]", teamNames)
	}
}

// TestGetTaskIsWorkspaceScoped 固化同名任务按空间取到不同的模型。
func TestGetTaskIsWorkspaceScoped(t *testing.T) {
	server, path := workspaceServer(t)

	// 默认空间的 shared -> model-a。
	recorder := callTasks(t, server, http.MethodGet, "/api/tasks/shared", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("默认空间状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"model":"model-a"`) {
		t.Errorf("默认空间的 shared 应指向 model-a: %s", recorder.Body.String())
	}

	// teamA 里没有 shared，因此 404——不能回落到别的空间。
	recorder = callTasks(t, server, http.MethodGet, "/api/tasks/shared", "teamA", "")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("teamA 的 shared 应为 404，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "任务不存在: shared") {
		t.Errorf("错误文本应为「任务不存在: shared」: %s", recorder.Body.String())
	}
	// 404 不应改动配置。
	assertConfigUnchanged(t, path, workspaceFixture)
}

// TestCreateTaskIsWorkspaceScoped 固化同名任务可在不同空间共存。
func TestCreateTaskIsWorkspaceScoped(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	// 默认空间已有 shared：再建同名 -> 409。
	recorder := callTasks(t, server, http.MethodPost, "/api/tasks", "",
		`{"config_revision":"`+revision+`","name":"shared","model":"model-a"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("同空间重名应为 409，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "任务已存在: shared") {
		t.Errorf("错误文本应为「任务已存在: shared」: %s", recorder.Body.String())
	}

	// teamA 里再建一个默认空间已有的名字 shared -> 成功（跨空间同名合法）。
	recorder = callTasks(t, server, http.MethodPost, "/api/tasks", "teamA",
		`{"config_revision":"`+revision+`","name":"shared","model":"model-b"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("跨空间同名应为 201，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 新任务落在 workspaces.teamA.tasks 下，默认空间的 shared 不受影响。
	data := readConfigData(t, path)
	if !strings.Contains(canonical.Dumps(data), `"teamA"`) {
		t.Errorf("新任务应写进 workspaces.teamA: %s", canonical.Dumps(data))
	}
	recorder = callTasks(t, server, http.MethodGet, "/api/tasks", "teamA", "")
	names := taskNames(t, recorder.Body.String())
	if len(names) != 2 {
		t.Errorf("teamA 应有 2 个任务，实际 %v", names)
	}
}

// TestDeleteTaskInWorkspaceLeavesOtherWorkspaceAlone 固化删除只影响本空间。
func TestDeleteTaskInWorkspaceLeavesOtherWorkspaceAlone(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	recorder := callTasks(t, server, http.MethodDelete, "/api/tasks/team-a-only", "teamA",
		`{"config_revision":"`+revision+`"}`)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("删除 teamA 任务状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 默认空间的任务一个都不能少。
	recorder = callTasks(t, server, http.MethodGet, "/api/tasks", "", "")
	names := taskNames(t, recorder.Body.String())
	if len(names) != 2 {
		t.Errorf("默认空间任务不应受影响，实际 %v", names)
	}
	// 删空的 teamA 分组也应从配置里消失。
	data := readConfigData(t, path)
	if strings.Contains(canonical.Dumps(data), "teamA") {
		t.Errorf("删空的命名工作空间应从配置里移除: %s", canonical.Dumps(data))
	}
}

// TestUpdateTaskIsWorkspaceScoped 固化更新只落在本空间。
func TestUpdateTaskIsWorkspaceScoped(t *testing.T) {
	server, path := workspaceServer(t)
	revision := currentRevision(t, path)

	recorder := callTasks(t, server, http.MethodPut, "/api/tasks/shared", "teamA",
		`{"config_revision":"`+revision+`","model":"model-a"}`)
	// teamA 里没有 shared：应 404，而不是悄悄改了默认空间的同名任务。
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("teamA 的 shared 应为 404，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	recorder = callTasks(t, server, http.MethodGet, "/api/tasks/shared", "", "")
	if !strings.Contains(recorder.Body.String(), `"model":"model-a"`) {
		t.Errorf("默认空间的 shared 不应被改动: %s", recorder.Body.String())
	}
}

// TestUnknownWorkspaceHeaderIsEmptyScope 固化未知空间名不是错误，只是一个空视图。
//
// 空间是隐式产生的（在它里面建任务即存在），因此刻意不做「空间不存在」的前置校验：
// 单独一条错误既没有信息量，也会多一种需要维护的状态。
func TestUnknownWorkspaceHeaderIsEmptyScope(t *testing.T) {
	server, _ := workspaceServer(t)

	recorder := callTasks(t, server, http.MethodGet, "/api/tasks", "nope", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("未知空间状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	if names := taskNames(t, recorder.Body.String()); len(names) != 0 {
		t.Errorf("未知空间应为空视图，实际 %v", names)
	}
}

// —— 测试辅助 ——

// currentRevision 读出磁盘配置的版本号。
func currentRevision(t *testing.T, path string) string {
	t.Helper()
	data := readConfigData(t, path)
	revision, err := configRevision(data)
	if err != nil {
		t.Fatalf("计算版本号失败: %v", err)
	}
	return revision
}

// readConfigData 读取并迁移磁盘上的配置。
func readConfigData(t *testing.T, path string) *canonical.Value {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	data, err := canonical.Parse(raw)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	migrated, err := config.MigrateConfigData(data)
	if err != nil {
		t.Fatalf("迁移配置失败: %v", err)
	}
	return migrated
}

// assertConfigUnchanged 断言磁盘配置仍是给定的文本（做了迁移归一化后比较）。
func assertConfigUnchanged(t *testing.T, path, want string) {
	t.Helper()
	wantData, err := canonical.ParseString(want)
	if err != nil {
		t.Fatalf("解析期望配置失败: %v", err)
	}
	wantMigrated, err := config.MigrateConfigData(wantData)
	if err != nil {
		t.Fatalf("迁移期望配置失败: %v", err)
	}
	got := canonical.Dumps(readConfigData(t, path))
	if expected := canonical.Dumps(wantMigrated); got != expected {
		t.Errorf("配置被改动了\n实得: %s\n期望: %s", got, expected)
	}
}
