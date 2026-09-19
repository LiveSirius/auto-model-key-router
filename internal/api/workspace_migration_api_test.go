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

// 本文件固化 /api/workspaces/export 与 /api/workspaces/import 的语义。
//
// 这是**工作空间自己的迁移通道**，与 /api/config/export 刻意分开，两条关键差别都在
// 这里钉住：
//
//  1. 这条通道**带** api_key（那条剥掉它）。面板凭据是应用侧要搬的东西。
//  2. 这条通道**不带** providers / models（那条的核心内容），任务引用的模型在目标
//     实例上解析不了时按 RepairTasks 的既有规则清掉并如实回报。

// workspaceBundle 是导出响应的形状。
//
// bundle.spaces 就是 `workspaces` 段**原样**的对象（`{空间名: {api_key?, tasks?}}`），
// 不是完整配置：迁移包不带 providers / models（那是配置导出的内容）。
type workspaceBundle struct {
	Bundle struct {
		Version    int      `json:"version"`
		Workspaces []string `json:"workspaces"`
		Spaces     map[string]struct {
			APIKey string `json:"api_key"`
			Tasks  map[string]struct {
				Model string `json:"model"`
			} `json:"tasks"`
		} `json:"spaces"`
	} `json:"bundle"`
	ConfigRevision string `json:"config_revision"`
}

// callMigration 向迁移端点发一条请求（完整权限）。
func callMigration(t *testing.T, server *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callTasks(t, server, http.MethodPost, path, "", body)
}

// exportBundle 导出全部命名工作空间并解析。
func exportBundle(t *testing.T, server *Server, body string) workspaceBundle {
	t.Helper()
	recorder := callMigration(t, server, "/api/workspaces/export", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导出状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	var payload workspaceBundle
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析导出结果失败: %v（body=%s）", err, recorder.Body.String())
	}
	return payload
}

// TestWorkspaceExportCarriesPanelKey 固化导出**带** key，与配置导出相反。
//
// 这是整条通道存在的理由：应用侧搬空间时要保住嵌入方的凭据。
func TestWorkspaceExportCarriesPanelKey(t *testing.T) {
	server, path := workspaceServer(t)
	// 先建一个带 key 的空间：夹具里的 teamA 只有任务，没有 api_key。
	revision := currentRevision(t, path)
	created := callMigration(t, server, "/api/workspaces",
		`{"config_revision":"`+revision+`","name":"panel","api_key":"amkr_ws_panel_key"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("建空间状态码 = %d（body=%s）", created.Code, created.Body.String())
	}

	payload := exportBundle(t, server, "")
	// 两个空间：teamA（只有任务）与 panel（只有 key）。
	if len(payload.Bundle.Workspaces) != 2 {
		t.Fatalf("导出空间数 = %d，期望 2（%v）", len(payload.Bundle.Workspaces), payload.Bundle.Workspaces)
	}
	exported := payload.Bundle.Spaces
	if exported["panel"].APIKey != "amkr_ws_panel_key" {
		t.Errorf("导出应带上面板 key，实际 %+v", exported["panel"])
	}
	if exported["teamA"].Tasks["team-a-only"].Model != "model-b" {
		t.Errorf("导出应带上任务内容，实际 %+v", exported["teamA"])
	}
	// 迁移包不带 providers / models：搬的是命名空间本身，不是模型库。
	raw := canonical.Dumps(readConfigData(t, path))
	if !strings.Contains(raw, `"providers"`) {
		t.Fatalf("夹具本身应当有 providers（否则下面的断言没意义）: %s", raw)
	}
	recorder := callMigration(t, server, "/api/workspaces/export", "")
	for _, forbidden := range []string{`"providers"`, `"models"`} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Errorf("迁移包不应包含 %s: %s", forbidden, recorder.Body.String())
		}
	}
	// 默认工作空间不在包里：它没有名字也没有 key。
	if strings.Contains(recorder.Body.String(), `"default"`) {
		t.Errorf("迁移包不应包含默认工作空间: %s", recorder.Body.String())
	}
}

// TestWorkspaceExportCanSelectNames 固化按名字导出与「名字不存在就报错」。
func TestWorkspaceExportCanSelectNames(t *testing.T) {
	server, _ := workspaceServer(t)

	payload := exportBundle(t, server, `{"workspaces":["teamA"]}`)
	if len(payload.Bundle.Workspaces) != 1 || payload.Bundle.Workspaces[0] != "teamA" {
		t.Errorf("按名导出应只含 teamA，实际 %v", payload.Bundle.Workspaces)
	}

	// 名字不存在必须 404：静默跳过会让人以为搬走了、实际漏了。
	recorder := callMigration(t, server, "/api/workspaces/export", `{"workspaces":["nope"]}`)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("导出不存在的空间应 404，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 默认空间不能迁移（搬过去等于覆盖对方的默认空间）。
	recorder = callMigration(t, server, "/api/workspaces/export", `{"workspaces":["default"]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("导出默认空间应 400，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 一个空间都没有时明确报错，而不是回一个空包。
	recorder = callMigration(t, server, "/api/workspaces/export", `{"workspaces":[]}`)
	if recorder.Code != http.StatusOK {
		t.Errorf("空列表等于导出全部，应 200，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
}

// TestWorkspaceImportReplacesAndRenames 固化两种冲突策略。
func TestWorkspaceImportReplacesAndRenames(t *testing.T) {
	server, path := workspaceServer(t)
	bundleJSON := `{"spaces":{"teamA":{"api_key":"amkr_ws_imported","tasks":{"imported-task":{"model":"model-a"}}}}}`

	// 1) 不加前缀：同名空间被整包替换（含 key），并如实回报 replaced。
	revision := currentRevision(t, path)
	recorder := callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","bundle":`+bundleJSON+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导入状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Imported     bool              `json:"imported"`
		Added        []string          `json:"added"`
		Replaced     []string          `json:"replaced"`
		Renamed      map[string]string `json:"renamed"`
		RemovedTasks []string          `json:"removed_tasks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析导入结果失败: %v（body=%s）", err, recorder.Body.String())
	}
	if !result.Imported {
		t.Error("导入响应应带 imported=true")
	}
	// replaced 而不是 added：覆盖会换掉目标实例上那个空间的面板 key，调用方必须
	// 能把这件事告诉用户（旧 key 立刻失效）。
	if len(result.Replaced) != 1 || result.Replaced[0] != "teamA" {
		t.Errorf("replaced = %v，期望 [teamA]", result.Replaced)
	}
	if len(result.Added) != 0 {
		t.Errorf("added = %v，期望空", result.Added)
	}
	dumped := canonical.Dumps(readConfigData(t, path))
	if !strings.Contains(dumped, `"amkr_ws_imported"`) {
		t.Errorf("导入的 key 应写进配置: %s", dumped)
	}
	// 原来的任务 team-a-only 被整包替换掉了（覆盖语义）。
	if strings.Contains(dumped, "team-a-only") {
		t.Errorf("同名空间应被整包替换: %s", dumped)
	}
	if !strings.Contains(dumped, "imported-task") {
		t.Errorf("导入的任务应写进配置: %s", dumped)
	}

	// 2) 带前缀：同名空间改名为 teamB-teamA，一个都不覆盖。
	revision = currentRevision(t, path)
	recorder = callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","prefix":"teamB-","bundle":`+bundleJSON+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("带前缀导入状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析导入结果失败: %v", err)
	}
	if result.Renamed["teamA"] != "teamB-teamA" {
		t.Errorf("renamed = %v，期望 teamA -> teamB-teamA", result.Renamed)
	}
	if len(result.Replaced) != 0 {
		t.Errorf("带前缀时不应覆盖任何空间，replaced = %v", result.Replaced)
	}
	dumped = canonical.Dumps(readConfigData(t, path))
	// 原来那个 teamA 及其导入的 key 都还在（没被覆盖）。
	if !strings.Contains(dumped, `"teamA"`) || !strings.Contains(dumped, `"amkr_ws_imported"`) {
		t.Errorf("带前缀时原空间应保留: %s", dumped)
	}
	if !strings.Contains(dumped, `"teamB-teamA"`) {
		t.Errorf("带前缀时应新建改名后的空间: %s", dumped)
	}
}

// TestWorkspaceImportRemovesTasksWithMissingModels 固化：解析不了的任务被清掉并回报。
//
// 这是「只搬命名空间、不搬模型库」的直接后果：包里引用的模型在目标实例上未必存在。
// 留着它们只会变成请求时必然 404 的僵尸任务。
func TestWorkspaceImportRemovesTasksWithMissingModels(t *testing.T) {
	server, path := workspaceServer(t)
	bundleJSON := `{"spaces":{"newSpace":{"tasks":{` +
		`"ok-task":{"model":"model-a"},` +
		`"broken-task":{"model":"not-configured-here"}}}}}`

	revision := currentRevision(t, path)
	recorder := callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","bundle":`+bundleJSON+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导入状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Added        []string `json:"added"`
		RemovedTasks []string `json:"removed_tasks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析导入结果失败: %v", err)
	}
	if len(result.Added) != 1 || result.Added[0] != "newSpace" {
		t.Errorf("added = %v，期望 [newSpace]", result.Added)
	}
	// 命名空间里的任务用 `空间/任务名` 记录（RepairTasks 的既有口径）。
	if len(result.RemovedTasks) != 1 || result.RemovedTasks[0] != "newSpace/broken-task" {
		t.Errorf("removed_tasks = %v，期望 [newSpace/broken-task]", result.RemovedTasks)
	}
	dumped := canonical.Dumps(readConfigData(t, path))
	if strings.Contains(dumped, "broken-task") {
		t.Errorf("引用失效的任务应被清掉: %s", dumped)
	}
	if !strings.Contains(dumped, "ok-task") {
		t.Errorf("能解析的任务应保留: %s", dumped)
	}
}

// TestWorkspaceImportRejectsConfigBundle 固化：把 /api/config/export 的整包粘过来会被拒。
//
// 静默丢掉 providers / models 会让人以为模型也搬过来了。
func TestWorkspaceImportRejectsConfigBundle(t *testing.T) {
	server, path := workspaceServer(t)

	for _, body := range []string{
		// 配置导出的整包（外层就有 providers / models）。
		`{"config_revision":"x","bundle":{"config":{"providers":{},"models":{},"workspaces":{"a":{}}}}}`,
		// 没有 spaces 段：包括空的包与只带 version 的包。
		`{"config_revision":"x","bundle":{"version":1}}`,
		`{"config_revision":"x","bundle":{"spaces":{}}}`,
		// spaces 不是对象。
		`{"config_revision":"x","bundle":{"spaces":[]}}`,
		// 空间内容是字符串。
		`{"config_revision":"x","bundle":{"spaces":{"a":"nope"}}}`,
		// 含默认工作空间：它没有名字也没有 key，不该出现在迁移包里。
		`{"config_revision":"x","bundle":{"spaces":{"default":{"tasks":{}}}}}`,
	} {
		revision := currentRevision(t, path)
		body = strings.Replace(body, `"x"`, `"`+revision+`"`, 1)
		recorder := callMigration(t, server, "/api/workspaces/import", body)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("畸形包应 422，实际 %d（body=%s，请求=%s）",
				recorder.Code, recorder.Body.String(), body)
		}
	}
	// 畸形请求一律不改配置。
	assertConfigUnchanged(t, path, workspaceFixture)
}

// TestWorkspaceMigrationAcceptsWorkspaceNamedLikeConfigSection 固化：形状不含歧义。
//
// `providers` / `models` 是完全合法的**工作空间名**。如果迁移包把空间表塞进一个
// `config` 包装层，这两个名字就会与"包里混进了配置导出的段"变成同一种输入，只能二选
// 一地误判。spaces 这个独立键名让两者一眼可辨。
func TestWorkspaceMigrationAcceptsWorkspaceNamedLikeConfigSection(t *testing.T) {
	server, path := workspaceServer(t)
	bundleJSON := `{"spaces":{"providers":{"tasks":{"t":{"model":"model-a"}}},` +
		`"models":{"tasks":{"t2":{"model":"model-b"}}}}}`

	revision := currentRevision(t, path)
	recorder := callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","bundle":`+bundleJSON+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导入状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	dumped := canonical.Dumps(readConfigData(t, path))
	if !strings.Contains(dumped, `"providers":{"tasks"`) || !strings.Contains(dumped, `"models":{"tasks"`) {
		t.Errorf("以配置段名命名的工作空间应当被接受: %s", dumped)
	}

	// 导出同样要能带它们出去，并且再导入回来时还是工作空间而不是配置段。
	payload := exportBundle(t, server, "")
	if _, ok := payload.Bundle.Spaces["providers"]; !ok {
		t.Errorf("导出结果里应有名为 providers 的空间，实际 %v", payload.Bundle.Workspaces)
	}
}

// TestWorkspaceImportRewritesDuplicateKey 固化：包里 key 与目标实例撞车时换一个新的。
//
// 不换会撞 config.Validate 的重复检查，而那条错误的措辞（"两个空间用了同一个
// api_key"）对一个正在导入的人毫无指向性。
func TestWorkspaceImportRewritesDuplicateKey(t *testing.T) {
	server, path := workspaceServer(t)
	// 先建一个占用了 amkr_ws_taken 的空间。
	revision := currentRevision(t, path)
	created := callMigration(t, server, "/api/workspaces",
		`{"config_revision":"`+revision+`","name":"holder","api_key":"amkr_ws_taken"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("建空间状态码 = %d（body=%s）", created.Code, created.Body.String())
	}

	bundleJSON := `{"spaces":{"incoming":{"api_key":"amkr_ws_taken","tasks":{"t":{"model":"model-a"}}}}}`
	revision = currentRevision(t, path)
	recorder := callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","bundle":`+bundleJSON+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导入状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	dumped := canonical.Dumps(readConfigData(t, path))
	// 旧 key 仍只属于 holder 一个空间，导入方拿到的是新生成的。
	if strings.Count(dumped, `"amkr_ws_taken"`) != 1 {
		t.Errorf("重复的 key 应被换掉，实际配置: %s", dumped)
	}
	if !strings.Contains(dumped, `"amkr_ws_`) {
		t.Errorf("应为导入方生成一个新 key: %s", dumped)
	}
	// 配置本身仍然合法（config.Validate 的重复检查会在这里拦住）。
	if _, err := config.FromDict(readConfigData(t, path)); err != nil {
		t.Errorf("导入后的配置应当合法: %v", err)
	}
}

// TestWorkspaceMigrationRequiresFullAuth 固化：这条通道只认完整权限。
//
// 响应里有明文面板 key，比配置导出更敏感；面板 key 自己也不能用（它就是被搬的东西）。
//
// 请求体必须是**语法与形状都合法**的：参数校验排在鉴权之前（与 /metrics 同序，见
// handlers.go 的说明），畸形体会先拿到 422——那是另一条被测行为，会把这条测糊。
func TestWorkspaceMigrationRequiresFullAuth(t *testing.T) {
	server, _ := workspaceServer(t)

	cases := []struct{ path, body string }{
		{"/api/workspaces/export", `{"workspaces":["teamA"]}`},
		{"/api/workspaces/import", `{"config_revision":"rev","bundle":{"spaces":{"a":{"tasks":{}}}}}`},
	}
	for _, item := range cases {
		for _, authorization := range []string{"", "Bearer amkr-visitor", "Bearer wrong"} {
			request := httptest.NewRequest(http.MethodPost, item.path, strings.NewReader(item.body))
			request.Header.Set("Content-Type", "application/json")
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("%s 凭据 %q 状态码 = %d，期望 401（body=%s）",
					item.path, authorization, recorder.Code, recorder.Body.String())
			}
		}
	}
}
