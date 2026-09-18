package configops

import (
	"errors"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件固化工作空间隔离在 configops 层的语义。
//
// 对拍语料（config_operations_corpus.json）由已退役的 Python 脚本生成，里面没有
// 任何工作空间用例——工作空间是 Go 侧的新增能力，因此这里手写断言。既有 API
// （CreateTask/UpdateTask/DeleteTask/RepairTasks）的行为必须与语料保持不变，那由
// 语料本身继续守着。

// workspaceData 是一份带默认空间与命名空间的最小配置。
func workspaceData(t *testing.T) *canonical.Value {
	t.Helper()
	return mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]},"model-b":{"targets":[{"provider":"p","key":"k"}]}},
		"tasks":{"shared":{"model":"model-a"}},
		"workspaces":{"teamA":{"tasks":{"shared":{"model":"model-b"}}}}}`)
}

// TestCreateTaskIsWorkspaceScoped 固化：同名任务可在不同空间共存。
func TestCreateTaskIsWorkspaceScoped(t *testing.T) {
	data := workspaceData(t)

	// 默认空间已有 shared；在默认空间再建同名 -> 409。
	_, err := CreateTask(data, "shared", CreateTaskOptions{Model: "model-a"})
	var opErr *ConfigOperationError
	if !errors.As(err, &opErr) || opErr.StatusCode != 409 || opErr.Message != "任务已存在: shared" {
		t.Fatalf("默认空间同名任务应报 409，实际 %v", err)
	}

	// `shared` 已经同时存在于默认空间与 teamA；第三个空间仍可用同名——这正是
	// 「不同空间的任务名可重复」。
	if _, err := CreateTaskIn(data, "teamB", "shared", CreateTaskOptions{Model: "model-b"}); err != nil {
		t.Fatalf("跨空间同名任务应当允许: %v", err)
	}
	// 同一空间内仍然唯一。
	if _, err := CreateTaskIn(data, "teamB", "shared", CreateTaskOptions{Model: "model-b"}); err == nil {
		t.Error("同一空间内重名应当报 409")
	}

	// 落到配置里的形状：默认空间在 tasks，命名空间在 workspaces。
	if got := WorkspaceTasks(data, "teamB").Obj.Len(); got != 1 {
		t.Errorf("teamB 应有 1 个任务，实际 %d", got)
	}
	if task, ok := WorkspaceTasks(data, config.DefaultWorkspace).LookupOK("task-a"); ok && task.IsObject() {
		t.Error("teamB 的任务不应出现在默认空间")
	}
	if !lookup(data, "workspaces").IsObject() {
		t.Error("workspaces 段应存在")
	}
}

// TestCreateTaskInBlankWorkspaceFallsBackToDefault 固化空空间名归一。
func TestCreateTaskInBlankWorkspaceFallsBackToDefault(t *testing.T) {
	data := workspaceData(t)
	if _, err := CreateTaskIn(data, "   ", "task-x", CreateTaskOptions{Model: "model-a"}); err != nil {
		t.Fatalf("空白空间名应落到默认空间: %v", err)
	}
	if task := lookup(WorkspaceTasks(data, config.DefaultWorkspace), "task-x"); !task.IsObject() {
		t.Error("空白空间名的任务应写进默认空间")
	}
	if lookup(data, "workspaces").Obj != nil && lookup(data, "workspaces").Obj.Has("") {
		t.Error("不应产生空名工作空间")
	}
}

// TestUpdateAndDeleteTaskInWorkspace 固化读写都停留在同一个空间。
func TestUpdateAndDeleteTaskInWorkspace(t *testing.T) {
	data := workspaceData(t)

	// 改 teamA 的 shared 不能碰到默认空间的 shared。
	newModel := "model-a"
	if _, err := UpdateTaskIn(data, "teamA", "shared", UpdateTaskOptions{Model: &newModel}); err != nil {
		t.Fatalf("更新 teamA 任务失败: %v", err)
	}
	defaultTask, _ := RequireTask(data, "shared")
	if got := lookup(defaultTask, "model").StringValue(); got != "model-a" {
		t.Errorf("默认空间的 shared 不应被改动，实际 model=%s", got)
	}
	teamTask, err := RequireTaskIn(data, "teamA", "shared")
	if err != nil {
		t.Fatalf("读取 teamA 任务失败: %v", err)
	}
	if got := lookup(teamTask, "model").StringValue(); got != "model-a" {
		t.Errorf("teamA 的 shared 应已更新，实际 model=%s", got)
	}

	// teamA 里没有 task-a：越空间访问报 404。
	var missing *ConfigOperationError
	if _, err := RequireTaskIn(data, "teamA", "task-a"); !errors.As(err, &missing) || missing.StatusCode != 404 {
		t.Errorf("越空间访问应报 404 ConfigOperationError，实际 %v", err)
	}

	// 删掉命名空间里唯一的任务后，这个空分组也一并消失（工作空间只是任务的
	// 分组，没有成员时既不可观测也没有意义）。
	if err := DeleteTaskIn(data, "teamA", "shared"); err != nil {
		t.Fatalf("删除 teamA 任务失败: %v", err)
	}
	if entry, ok := lookup(data, "workspaces").LookupOK("teamA"); ok && entry.IsObject() {
		t.Error("删空的命名工作空间应一并移除")
	}
	if _, err := RequireTaskIn(data, "teamA", "shared"); err == nil {
		t.Error("删掉的任务不应还能读到")
	}
	// 默认空间的 shared 仍在。
	if _, err := RequireTask(data, "shared"); err != nil {
		t.Errorf("默认空间的 shared 不应被删除: %v", err)
	}
	// 删空后配置必须仍然可解析、且不含空 workspaces 段。
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("删除后的配置应可解析: %v", err)
	}
}

// TestRepairTasksCoversNamedWorkspaces 固化修复会走进命名空间，且任务名带上空间前缀。
func TestRepairTasksCoversNamedWorkspaces(t *testing.T) {
	data := mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},
		"tasks":{"gone":{"model":"nope"},"kept":{"model":"model-a"}},
		"workspaces":{"teamA":{"tasks":{"gone":{"model":"nope"},"kept":{"model":"model-a"}}}}}`)

	removed, err := RepairTasks(data)
	if err != nil {
		t.Fatalf("修复失败: %v", err)
	}
	// 默认空间按原名返回，命名空间带前缀——两处都有 gone，不限定就分不清。
	want := []string{"gone", "teamA/gone"}
	if len(removed) != len(want) {
		t.Fatalf("被清理的任务 %v，期望 %v", removed, want)
	}
	for i := range want {
		if removed[i] != want[i] {
			t.Fatalf("被清理的任务 %v，期望 %v", removed, want)
		}
	}
	if task, ok := WorkspaceTasks(data, "teamA").LookupOK("kept"); !ok || !task.IsObject() {
		t.Error("teamA 的合法任务不应被清理")
	}
	if task, ok := WorkspaceTasks(data, config.DefaultWorkspace).LookupOK("kept"); !ok || !task.IsObject() {
		t.Error("默认空间的合法任务不应被清理")
	}

	// 修完必须还能被完整解析：否则配置写不回去。
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("修复后的配置应可解析: %v", err)
	}
}

// TestRepairTasksDropsMalformedWorkspaces 固化：形状非法的 workspaces 整个丢掉。
//
// 留着它会让后续每次 FromDict 都失败，配置再也写不回去——比丢一段坏数据更糟。
func TestRepairTasksDropsMalformedWorkspaces(t *testing.T) {
	for _, payload := range []string{`"workspaces":[]`, `"workspaces":{"teamA":[]}`} {
		data := mustParse(t, `{"config_version":4,"local_api_key":"k",
			"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
			"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},
			"tasks":{"kept":{"model":"model-a"}},`+payload+`}`)
		if _, err := RepairTasks(data); err != nil {
			t.Fatalf("修复失败（%s）: %v", payload, err)
		}
		if lookup(data, "workspaces").IsObject() {
			t.Errorf("非法的 workspaces 应被丢掉（%s），实际 %s",
				payload, canonical.Dumps(lookup(data, "workspaces")))
		}
		if _, err := config.FromDict(data); err != nil {
			t.Errorf("丢掉坏 workspaces 后应可解析（%s）: %v", payload, err)
		}
	}
}

// TestRepairTasksKeepsValidSiblingWorkspace 固化：丢弃非法空间不影响合法兄弟。
//
// 整段丢掉是「只能整段丢」时的退路；一个坏成员不该连累旁边的好成员——那会把用户
// 完好的任务路由一起抹掉。
func TestRepairTasksKeepsValidSiblingWorkspace(t *testing.T) {
	data := mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},
		"tasks":{},
		"workspaces":{"broken":[],"teamA":{"tasks":{"kept":{"model":"model-a"}}}}}`)

	removed, err := RepairTasks(data)
	if err != nil {
		t.Fatalf("修复失败: %v", err)
	}
	if len(removed) != 1 || removed[0] != "broken/" {
		t.Errorf("被清理项 %v，期望 [broken/]", removed)
	}
	if task, ok := WorkspaceTasks(data, "teamA").LookupOK("kept"); !ok || !task.IsObject() {
		t.Error("合法兄弟工作空间的任务不应被连累")
	}
	if entry, ok := lookup(data, "workspaces").LookupOK("broken"); ok && entry.IsObject() {
		t.Error("非法工作空间应被丢弃")
	}
	// 默认空间空掉后 tasks 键也应消失（既有语义，不能因为多了 workspaces 而变）。
	if lookup(data, "tasks").IsObject() {
		t.Errorf("空掉的默认空间不应留下 tasks 键，实际 %s", canonical.Dumps(data))
	}
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("修复后的配置应可解析: %v", err)
	}
}

// TestTransferCarriesWorkspaces 固化导出/导入带着命名空间一起走。
func TestTransferCarriesWorkspaces(t *testing.T) {
	data := workspaceData(t)
	exported, err := TransferableConfig(data, true)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !lookup(exported, "workspaces").IsObject() {
		t.Fatalf("导出应包含 workspaces，实际 %s", canonical.Dumps(exported))
	}

	// 导入到一个只有默认空间的实例：命名空间与其中的任务都要过来。
	target := mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{},"tasks":{}}`)
	merged, err := MergeTransferableConfig(target, exported)
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	task, found := lookup(lookup(lookup(merged.Config, "workspaces"), "teamA"), "tasks").LookupOK("shared")
	if !found || !task.IsObject() {
		t.Fatalf("导入后 teamA 的 shared 应存在，实际 %s", canonical.Dumps(merged.Config))
	}
	if _, err := config.FromDict(merged.Config); err != nil {
		t.Fatalf("合并结果应可解析: %v", err)
	}
}

// TestDefaultWorkspaceAliasesLegacyAPI 固化：既有 API 一律作用于默认空间。
//
// 这是兼容性的核心——语料锁定的那批调用路径（不带工作空间）必须一字不变。
func TestDefaultWorkspaceAliasesLegacyAPI(t *testing.T) {
	data := workspaceData(t)
	if _, err := CreateTask(data, "legacy", CreateTaskOptions{Model: "model-b"}); err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	if _, err := RequireTask(data, "legacy"); err != nil {
		t.Fatalf("既有 API 应能读到刚建的任务: %v", err)
	}
	if task, ok := WorkspaceTasks(data, config.DefaultWorkspace).LookupOK("legacy"); !ok || !task.IsObject() {
		t.Error("既有 API 建的任务应落在默认空间")
	}
	if err := DeleteTask(data, "legacy"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if task, ok := WorkspaceTasks(data, "teamA").LookupOK("legacy"); ok && task.IsObject() {
		t.Error("既有 API 不应碰命名空间")
	}
}
