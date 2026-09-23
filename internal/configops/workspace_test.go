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
	exported, err := TransferableConfig(data)
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

// TestRenameWorkspaceMovesTasks 固化改名：整组任务跟着新名字走，旧键消失。
func TestRenameWorkspaceMovesTasks(t *testing.T) {
	data := workspaceData(t)
	// 先给 teamA 加上第二个任务，确认改名搬的是整组而不是一个。
	if _, err := CreateTaskIn(data, "teamA", "extra", CreateTaskOptions{Model: "model-a"}); err != nil {
		t.Fatalf("准备第二个任务失败: %v", err)
	}

	renamed, err := RenameWorkspace(data, "teamA", "teamB")
	if err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if renamed != "teamB" {
		t.Errorf("返回的新名字 = %q，期望 teamB", renamed)
	}
	if got := WorkspaceTasks(data, "teamB").Obj.Len(); got != 2 {
		t.Errorf("teamB 应有 2 个任务，实际 %d（%s）", got, canonical.Dumps(data))
	}
	if entry, ok := lookup(data, "workspaces").LookupOK("teamA"); ok && entry.IsObject() {
		t.Error("旧名字应从配置里消失")
	}
	// 任务内容本身要完好：改名不该顺手改掉模型或参数。
	task, err := RequireTaskIn(data, "teamB", "shared")
	if err != nil {
		t.Fatalf("改名后应能按新名字读到任务: %v", err)
	}
	if got := lookup(task, "model").StringValue(); got != "model-b" {
		t.Errorf("改名不应改动任务内容，实际 model=%s", got)
	}
	// 默认空间一个字节都不该动。
	if got := WorkspaceTasks(data, config.DefaultWorkspace).Obj.Len(); got != 1 {
		t.Errorf("默认空间任务数 = %d，期望 1", got)
	}
}

// TestRenameWorkspaceRejectsTargets 固化改名的两条拒绝路径。
func TestRenameWorkspaceRejectsTargets(t *testing.T) {
	// 目标已存在时不合并：两个空间各有一批任务时合并会出现重名任务，而那是非法配置。
	data := workspaceData(t)
	if _, err := CreateTaskIn(data, "teamB", "only-b", CreateTaskOptions{Model: "model-a"}); err != nil {
		t.Fatalf("准备 teamB 失败: %v", err)
	}
	var conflict *ConfigOperationError
	err := func() error { _, err := RenameWorkspace(data, "teamB", "teamA"); return err }()
	if !errors.As(err, &conflict) || conflict.StatusCode != 409 {
		t.Errorf("改名到已存在的空间应报 409，实际 %v", err)
	}
	// 两边都原样保留。
	if WorkspaceTasks(data, "teamA").Obj.Len() != 1 || WorkspaceTasks(data, "teamB").Obj.Len() != 1 {
		t.Errorf("失败的改名不应改动任何一边: %s", canonical.Dumps(data))
	}

	// 源空间没有任务 -> 404（空间由任务反推，空分组不算存在）。
	data = workspaceData(t)
	if _, err := RenameWorkspace(data, "nope", "other"); !errors.As(err, &conflict) || conflict.StatusCode != 404 {
		t.Errorf("改名不存在的空间应报 404，实际 %v", err)
	}

	// 默认空间改不动：全组任务会跟着换名字，缺省调用方全部落空。
	data = workspaceData(t)
	if _, err := RenameWorkspace(data, config.DefaultWorkspace, "moved"); !errors.As(err, &conflict) || conflict.StatusCode != 400 {
		t.Errorf("默认空间不可改名，实际 %v", err)
	}
	if _, err := RenameWorkspace(data, "teamA", config.DefaultWorkspace); !errors.As(err, &conflict) || conflict.StatusCode != 409 {
		t.Errorf("不能改名成默认空间名，实际 %v", err)
	}
}

// TestDeleteWorkspaceRemovesTasks 固化删除：整组任务与分组一起消失。
func TestDeleteWorkspaceRemovesTasks(t *testing.T) {
	data := workspaceData(t)

	if err := DeleteWorkspace(data, "teamA"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := RequireTaskIn(data, "teamA", "shared"); err == nil {
		t.Error("删掉的空间不该还能读到任务")
	}
	// 这是团队 A 唯一的空间，删完 workspaces 段应整个消失（空段无意义）。
	if lookup(data, "workspaces").IsObject() {
		t.Errorf("最后一个命名空间删掉后 workspaces 段应消失: %s", canonical.Dumps(data))
	}
	if WorkspaceTasks(data, config.DefaultWorkspace).Obj.Len() != 1 {
		t.Errorf("默认空间不应受影响: %s", canonical.Dumps(data))
	}
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("删除后的配置应可解析: %v", err)
	}

	// 删不存在的空间 -> 404；默认空间删不得 -> 400。
	var opErr *ConfigOperationError
	if err := DeleteWorkspace(data, "teamA"); !errors.As(err, &opErr) || opErr.StatusCode != 404 {
		t.Errorf("重复删除应报 404，实际 %v", err)
	}
	if err := DeleteWorkspace(data, config.DefaultWorkspace); !errors.As(err, &opErr) || opErr.StatusCode != 400 {
		t.Errorf("默认空间不可删除，实际 %v", err)
	}
}

// TestDeleteWorkspaceKeepsSibling 固化删除一个空间不连累兄弟空间。
func TestDeleteWorkspaceKeepsSibling(t *testing.T) {
	data := workspaceData(t)
	if _, err := CreateTaskIn(data, "teamB", "kept", CreateTaskOptions{Model: "model-a"}); err != nil {
		t.Fatalf("准备 teamB 失败: %v", err)
	}
	if err := DeleteWorkspace(data, "teamA"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if task, ok := WorkspaceTasks(data, "teamB").LookupOK("kept"); !ok || !task.IsObject() {
		t.Errorf("兄弟空间应原样保留: %s", canonical.Dumps(data))
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

// TestCreateTaskWithoutModel 固化：不传模型也能建任务，且不写 model 键。
//
// 关键是**不写这个键**而不是写 null / 空串：config 层把缺失、null、空白串都当作
// 「尚未指定」，但配置里多一个 "model": null 会让 `router-config.json` 的 diff
// 变得难读，也会让「用户从没选过模型」和「用户选过又清掉了」这两种历史无法区分。
func TestCreateTaskWithoutModel(t *testing.T) {
	data := workspaceData(t)

	task, err := CreateTask(data, "placeholder", CreateTaskOptions{})
	if err != nil {
		t.Fatalf("不传模型应能建任务: %v", err)
	}
	if _, present := task.LookupOK("model"); present {
		t.Errorf("未指定模型时不应写入 model 键，实际 %s", canonical.Dumps(task))
	}
	// 配置仍须整体可解析：放宽的是「可以没有模型」，不是「配置可以不自洽」。
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("未指定模型的任务应可解析: %v", err)
	}

	// 之后再补上模型：这是占位任务的主要用法，必须能走通。
	chosen := "model-b"
	if _, err := UpdateTask(data, "placeholder", UpdateTaskOptions{Model: &chosen}); err != nil {
		t.Fatalf("给占位任务补模型失败: %v", err)
	}
	stored, err := RequireTask(data, "placeholder")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got := lookup(stored, "model").StringValue(); got != "model-b" {
		t.Errorf("补上的模型应为 model-b，实际 %q", got)
	}
}

// TestCreateTaskFallbackWithoutModelRejected 固化：没有首选的备选必须被拒。
//
// 备选是「首选用不了时的退路」，没有首选就无处可退。config.Validate 不查这一条
// （它的校验顺序与文案是对外契约，不能插入新分支），因此这道闸在这里。
func TestCreateTaskFallbackWithoutModelRejected(t *testing.T) {
	data := workspaceData(t)
	fallback := "model-b"
	_, err := CreateTask(data, "placeholder", CreateTaskOptions{FallbackModel: &fallback})
	var opErr *ConfigOperationError
	if !errors.As(err, &opErr) || opErr.StatusCode != 422 {
		t.Fatalf("没有首选时的备选应报 422，实际 %v", err)
	}
	if opErr.Message != "任务 placeholder 指定了备选但没有首选模型" {
		t.Errorf("错误文本不一致: %s", opErr.Message)
	}
	if task, ok := WorkspaceTasks(data, config.DefaultWorkspace).LookupOK("placeholder"); ok && task.IsObject() {
		t.Error("被拒的任务不应写进配置")
	}
}

// TestUpdateTaskDisplayName 固化显示名的增删改。
func TestUpdateTaskDisplayName(t *testing.T) {
	data := workspaceData(t)

	// 取名字。
	name := "  长文摘要  "
	if _, err := UpdateTask(data, "shared", UpdateTaskOptions{DisplayName: &name}); err != nil {
		t.Fatalf("设置显示名失败: %v", err)
	}
	stored, err := RequireTask(data, "shared")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 两端空白要清掉，否则界面上就是「 长文摘要 」。
	if got := lookup(stored, "display_name").StringValue(); got != "长文摘要" {
		t.Errorf("显示名应去空白，实际 %q", got)
	}

	// 空串表示清掉这个名字（而不是写一个空字符串进配置）。
	empty := ""
	if _, err := UpdateTask(data, "shared", UpdateTaskOptions{DisplayName: &empty}); err != nil {
		t.Fatalf("清空显示名失败: %v", err)
	}
	stored, err = RequireTask(data, "shared")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if _, present := stored.LookupOK("display_name"); present {
		t.Errorf("清空后不应残留 display_name 键，实际 %s", canonical.Dumps(stored))
	}

	// 不传 DisplayName 时不能动它：nil 是「不改」，不是「清空」。
	named := "摘要"
	if _, err := UpdateTask(data, "shared", UpdateTaskOptions{DisplayName: &named}); err != nil {
		t.Fatalf("设置显示名失败: %v", err)
	}
	other := "model-b"
	if _, err := UpdateTask(data, "shared", UpdateTaskOptions{Model: &other}); err != nil {
		t.Fatalf("只改模型失败: %v", err)
	}
	stored, err = RequireTask(data, "shared")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got := lookup(stored, "display_name").StringValue(); got != "摘要" {
		t.Errorf("只改模型时显示名不应受影响，实际 %q", got)
	}
}

// TestUpdateTaskCanClearModel 固化：清空首选会连带清掉备选。
//
// 否则配置里会留下「只有备选没有首选」的非法组合，下一次写盘时整个配置都解析不了。
func TestUpdateTaskCanClearModel(t *testing.T) {
	data := workspaceData(t)
	fallback := "model-b"
	if _, err := UpdateTask(data, "shared", UpdateTaskOptions{
		FallbackModel: &fallback, UpdateFallback: true}); err != nil {
		t.Fatalf("设置备选失败: %v", err)
	}

	empty := ""
	if _, err := UpdateTask(data, "shared", UpdateTaskOptions{Model: &empty}); err != nil {
		t.Fatalf("清空模型失败: %v", err)
	}
	stored, err := RequireTask(data, "shared")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if _, present := stored.LookupOK("model"); present {
		t.Errorf("清空后不应残留 model 键，实际 %s", canonical.Dumps(stored))
	}
	if _, present := stored.LookupOK("fallback_model"); present {
		t.Errorf("清空首选时应一并清掉备选，实际 %s", canonical.Dumps(stored))
	}
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("清空模型后的配置应仍可解析: %v", err)
	}
}

// TestRepairTasksKeepsTaskWithoutModel 固化：repair 不会删掉未指定模型的任务。
//
// 这是 repair 唯一不沿用「解析不了就删」的分支。若沿用，用户刚建好的占位任务会在
// 任何一次导入/合并（都会走 RepairTasks）时凭空消失。
func TestRepairTasksKeepsTaskWithoutModel(t *testing.T) {
	data := mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},
		"tasks":{"placeholder":{"display_name":"长文摘要","params":{"temperature":0.5}},"gone":{"model":"model-typo"}}}`)

	removed, err := RepairTasks(data)
	if err != nil {
		t.Fatalf("修复失败: %v", err)
	}
	// 引用了不存在模型的任务照旧删掉。
	if len(removed) != 1 || removed[0] != "gone" {
		t.Errorf("应只删掉 gone，实际 %v", removed)
	}
	placeholder := lookup(WorkspaceTasks(data, config.DefaultWorkspace), "placeholder")
	if !placeholder.IsObject() {
		t.Fatalf("未指定模型的任务不应被删掉: %s", canonical.Dumps(data))
	}
	// 固定参数要原样保留——占位任务的价值就在于先把参数定下来。
	if got, ok := lookup(lookup(placeholder, "params"), "temperature").AsFloat(); !ok || got != 0.5 {
		t.Errorf("占位任务的固定参数应保留，实际 %v（ok=%v）", got, ok)
	}
	// 显示名同样要保留：它不是能靠模型名反推出来的信息，丢了就真丢了。
	if got := lookup(placeholder, "display_name").StringValue(); got != "长文摘要" {
		t.Errorf("占位任务的显示名应保留，实际 %q", got)
	}
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("修复后的配置应可解析: %v", err)
	}
}

// TestRepairTasksDropsFallbackWithoutModel 固化：repair 清掉无首选的备选。
//
// 这种组合是非法配置，留着会让后续每次 FromDict 都失败、配置再也写不回去。
func TestRepairTasksDropsFallbackWithoutModel(t *testing.T) {
	data := mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},
		"tasks":{"placeholder":{"fallback_model":"model-a"}}}`)

	if _, err := RepairTasks(data); err != nil {
		t.Fatalf("修复失败: %v", err)
	}
	task := lookup(WorkspaceTasks(data, config.DefaultWorkspace), "placeholder")
	if !task.IsObject() {
		t.Fatal("任务本身应保留")
	}
	if _, present := task.LookupOK("fallback_model"); present {
		t.Errorf("无首选的备选应被清掉，实际 %s", canonical.Dumps(task))
	}
	if _, err := config.FromDict(data); err != nil {
		t.Fatalf("修复后的配置应可解析: %v", err)
	}
}
