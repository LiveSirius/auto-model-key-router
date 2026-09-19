package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
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
		Rekeyed      map[string]string `json:"rekeyed"`
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
	// 克隆体与原空间并存，原 key 被原空间占着，因此克隆体**必然**拿到一把新 key，
	// 且必须回报出来——嵌入方手里那把对应的是原空间。
	newKey, ok := result.Rekeyed["teamB-teamA"]
	if !ok || !strings.HasPrefix(newKey, "amkr_ws_") {
		t.Errorf("rekeyed = %v，期望 teamB-teamA 拿到一把新 key", result.Rekeyed)
	}
	dumped = canonical.Dumps(readConfigData(t, path))
	// 原来那个 teamA 及其导入的 key 都还在（没被覆盖）。
	if !strings.Contains(dumped, `"teamA"`) || !strings.Contains(dumped, `"amkr_ws_imported"`) {
		t.Errorf("带前缀时原空间应保留: %s", dumped)
	}
	if !strings.Contains(dumped, `"teamB-teamA"`) {
		t.Errorf("带前缀时应新建改名后的空间: %s", dumped)
	}
	// 两个空间没有共用同一把 key（配置层的重复校验会拒）。
	if strings.Count(dumped, `"amkr_ws_imported"`) != 1 {
		t.Errorf("原 key 只应留在原空间上: %s", dumped)
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

// TestWorkspaceImportCloneKeepsBothSpacesUsable 固化加前缀克隆的主要用法：把一个本地
// **已存在**的空间克隆一份出来，两个都还能用。
//
// 这是最容易写坏的一条：克隆体与原空间并存，原 key 必然还被原空间占着，因此克隆体必须
// 换一把新 key（否则要么配置校验失败、要么两个空间共用同一把 key）。新 key 必须在响应里
// 回报，否则克隆出来的空间没有可用的面板。
func TestWorkspaceImportCloneKeepsBothSpacesUsable(t *testing.T) {
	server, path := workspaceServer(t)
	// 先建一个带 key 的空间，再把它自己导出、加前缀导入回去。
	revision := currentRevision(t, path)
	created := callMigration(t, server, "/api/workspaces",
		`{"config_revision":"`+revision+`","name":"orig","api_key":"amkr_ws_orig"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("建空间状态码 = %d（body=%s）", created.Code, created.Body.String())
	}
	// 给它一个任务，确认内容真的被复制。
	revision = currentRevision(t, path)
	task := callTasks(t, server, http.MethodPost, "/api/tasks", "orig",
		`{"config_revision":"`+revision+`","name":"copy-me","model":"model-a"}`)
	if task.Code != http.StatusCreated {
		t.Fatalf("建任务状态码 = %d（body=%s）", task.Code, task.Body.String())
	}

	bundle := exportBundle(t, server, `{"workspaces":["orig"]}`)
	bundleJSON := canonical.Dumps(workspaceBundleRaw(bundle))
	revision = currentRevision(t, path)
	recorder := callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","prefix":"copy-","bundle":`+bundleJSON+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("克隆导入状态码 = %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Renamed map[string]string `json:"renamed"`
		Rekeyed map[string]string `json:"rekeyed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析导入结果失败: %v（body=%s）", err, recorder.Body.String())
	}
	if result.Renamed["orig"] != "copy-orig" {
		t.Errorf("renamed = %v，期望 orig -> copy-orig", result.Renamed)
	}
	cloneKey := result.Rekeyed["copy-orig"]
	if cloneKey == "" || cloneKey == "amkr_ws_orig" {
		t.Fatalf("克隆体应拿到一把不同的新 key，实际 %q（rekeyed=%v）", cloneKey, result.Rekeyed)
	}

	// 两个空间都还能用，且各自只看得到自己的任务。
	for _, item := range []struct{ key, workspace, task string }{
		{"amkr_ws_orig", "orig", "copy-me"},
		{cloneKey, "copy-orig", "copy-me"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/tasks", strings.NewReader(""))
		request.Header.Set("Authorization", "Bearer "+item.key)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s 的任务列表状态码 = %d（body=%s）", item.workspace, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), item.task) {
			t.Errorf("%s 应看得到复制过来的任务: %s", item.workspace, response.Body.String())
		}
	}
}

// workspaceBundleRaw 把导出结果还原成可直接喂给导入接口的 JSON。
//
// 导出响应里 bundle 与 config_revision 同级，而导入只吃 bundle，因此要拆出来重编一遍；
// 用它而不是手写字符串，是为了让「导出 → 导入」这条闭环真的被走通。
func workspaceBundleRaw(bundle workspaceBundle) *canonical.Value {
	spaces := canonical.NewObject()
	for name, entry := range bundle.Bundle.Spaces {
		tasks := canonical.NewObject()
		for taskName, task := range entry.Tasks {
			tasks.SetKey(taskName, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "model", Value: canonical.NewString(task.Model)},
			))
		}
		entryValue := canonical.NewObject()
		if entry.APIKey != "" {
			entryValue.SetKey("api_key", canonical.NewString(entry.APIKey))
		}
		entryValue.SetKey("tasks", tasks)
		spaces.SetKey(name, entryValue)
	}
	return canonical.NewObjectOf(canonical.ObjectPair{Key: "spaces", Value: spaces})
}

// TestWorkspaceImportRejectsReservedVisitorKey 固化：包里带保留的访客 key 或本地主凭据时
// **报错**，而不是悄悄改写。
//
// 这两把 key 与「撞上别的空间」是同一种结论（都报错），但理由不同：撞车可以靠加前缀克隆
// 绕开（那种情况由 rekeyed 回报），而包里写着 amkr-visitor 或 local_api_key 说明这份包
// 本身坏了——那是配置层明令禁止的入站凭据，静默改写会把它藏起来。
func TestWorkspaceImportRejectsReservedVisitorKey(t *testing.T) {
	server, path := workspaceServer(t)

	for _, bundleJSON := range []string{
		`{"spaces":{"a":{"api_key":"amkr-visitor","tasks":{"t":{"model":"model-a"}}}}}`,
		// 与目标实例的 local_api_key 相同同理：那是全量权限的主凭据。
		`{"spaces":{"a":{"api_key":"local-key","tasks":{"t":{"model":"model-a"}}}}}`,
	} {
		revision := currentRevision(t, path)
		recorder := callMigration(t, server, "/api/workspaces/import",
			`{"config_revision":"`+revision+`","bundle":`+bundleJSON+`}`)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("保留 key 应 422，实际 %d（body=%s，请求=%s）",
				recorder.Code, recorder.Body.String(), bundleJSON)
		}
	}
	// 一律不改配置（否则会留下一个 config.Validate 会拒的配置）。
	assertConfigUnchanged(t, path, workspaceFixture)
}

// TestWorkspaceImportRejectsDuplicateKey 固化：包里 key 与目标实例上的**别处**凭据撞车
// 时报错。
//
// 不悄悄换一个新 key：换掉看似"让导入成功"，实际是把一件用户必须知道的事藏了起来——那把
// key 通常已经嵌在别人的页面里，换掉之后旧 key 会指向别人**别的**空间，而导入响应里没有
// 任何字段能说明这件事。报错则用户一眼知道该改哪一个。
func TestWorkspaceImportRejectsDuplicateKey(t *testing.T) {
	server, path := workspaceServer(t)
	// 先建一个占用了 amkr_ws_taken 的空间。
	revision := currentRevision(t, path)
	created := callMigration(t, server, "/api/workspaces",
		`{"config_revision":"`+revision+`","name":"holder","api_key":"amkr_ws_taken"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("建空间状态码 = %d（body=%s）", created.Code, created.Body.String())
	}
	// 建空间同时发一把推理 key（服务端生成，明文只在这次响应里）。它同样落在配置里，
	// 因此下面的「配置未变」断言要带上它——否则会因为一个**预期内**的字段而失败。
	var createBody struct {
		InferenceKey string `json:"inference_key"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("解析建空间响应失败: %v（body=%s）", err, created.Body.String())
	}
	if !strings.HasPrefix(createBody.InferenceKey, "amkr_ik_") {
		t.Fatalf("建空间应返回 amkr_ik_ 前缀的推理 key，实际 %q", createBody.InferenceKey)
	}

	bundleJSON := `{"spaces":{"incoming":{"api_key":"amkr_ws_taken","tasks":{"t":{"model":"model-a"}}}}}`
	revision = currentRevision(t, path)
	recorder := callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","bundle":`+bundleJSON+`}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("重复 key 应 422，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	// 错误信息要指名道姓（与哪个空间撞了），否则用户不知道该改哪一个。
	if !strings.Contains(recorder.Body.String(), "holder") {
		t.Errorf("错误信息应指出冲突的空间名: %s", recorder.Body.String())
	}
	// 失败的导入不改配置。
	withHolder := strings.Replace(workspaceFixture,
		`"workspaces": {"teamA": {"tasks": {"team-a-only": {"model": "model-b"}}}}`,
		`"workspaces": {"holder": {"api_key": "amkr_ws_taken", "inference_key": "`+createBody.InferenceKey+`"}, `+
			`"teamA": {"tasks": {"team-a-only": {"model": "model-b"}}}}`,
		1)
	assertConfigUnchanged(t, path, withHolder)

	// 但**覆盖同一个空间**不算冲突：那个位置连同旧 key 一起被替换，没有第二个持有者。
	revision = currentRevision(t, path)
	replaced := callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","bundle":{"spaces":{"holder":{"api_key":"amkr_ws_replaced"}}}}`)
	if replaced.Code != http.StatusOK {
		t.Fatalf("覆盖同名空间应成功，实际 %d（body=%s）", replaced.Code, replaced.Body.String())
	}
	// 而把那个 key 换到**另一个**空间上就要被拒（否则两个空间同 key，判定取决于遍历顺序）。
	revision = currentRevision(t, path)
	recorder = callMigration(t, server, "/api/workspaces/import",
		`{"config_revision":"`+revision+`","bundle":{"spaces":{"other":{"api_key":"amkr_ws_replaced"}}}}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("把已有 key 挪给另一个空间应 422，实际 %d（body=%s）",
			recorder.Code, recorder.Body.String())
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
