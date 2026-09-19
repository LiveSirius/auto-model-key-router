package configops

import (
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// ExistingTasks 返回 data["tasks"]（不是对象时返回一个**新的**空对象）。
//
// 对齐 config_operations.py:1129。刻意不 setdefault：这个访问器会被导出与配置
// 版本号计算读到，凭空往配置里塞一个空 tasks 会让版本号变化，调用方于是收到莫名
// 其妙的 409。返回新对象而非共享，也保证了「调用方改它不影响 data」。
func ExistingTasks(data *canonical.Value) *canonical.Value {
	return WorkspaceTasks(data, config.DefaultWorkspace)
}

// WorkspaceTasks 返回指定工作空间下的任务映射。
//
// 默认工作空间就是顶层的 `tasks` 段（既有配置、既有语义一字不动）；命名工作空间
// 落在 `workspaces.<名字>.tasks`。工作空间不存在时返回**新的**空对象，理由与
// ExistingTasks 相同。
func WorkspaceTasks(data *canonical.Value, workspace string) *canonical.Value {
	if workspace == "" || workspace == config.DefaultWorkspace {
		value := lookup(data, "tasks")
		if value.IsObject() {
			return value
		}
		return canonical.NewObject()
	}
	tasks := lookup(lookup(lookup(data, "workspaces"), workspace), "tasks")
	if tasks.IsObject() {
		return tasks
	}
	return canonical.NewObject()
}

// writeWorkspaceTasks 把任务映射写回它所属的位置。
//
// 与 WorkspaceTasks 互为逆操作。默认工作空间直接落在 data["tasks"]；命名工作空间
// 逐层建出 `workspaces` 与 `workspaces.<名字>`。
//
// 传入空任务映射时会把这个工作空间**整个删掉**：工作空间只是「一组任务」的分组，
// 组里没有成员时它既不可观测也没有意义。这样 `workspaces` 段始终只包含非空分组，
// 管理面也就无须发明一套「新建/删除空工作空间」的接口。
func writeWorkspaceTasks(data *canonical.Value, workspace string, tasks *canonical.Value) {
	if workspace == "" || workspace == config.DefaultWorkspace {
		if !tasks.IsObject() || tasks.Obj.Len() == 0 {
			data.DeleteKey("tasks")
			return
		}
		data.SetKey("tasks", tasks)
		return
	}
	if !tasks.IsObject() || tasks.Obj.Len() == 0 {
		deleteWorkspace(data, workspace)
		return
	}
	workspaces := lookup(data, "workspaces")
	if !workspaces.IsObject() {
		workspaces = canonical.NewObject()
	}
	entry := lookup(workspaces, workspace)
	if !entry.IsObject() {
		entry = canonical.NewObject()
	}
	entry.SetKey("tasks", tasks)
	workspaces.SetKey(workspace, entry)
	data.SetKey("workspaces", workspaces)
}

// deleteWorkspace 删掉一个命名工作空间；它是最后一个时连 workspaces 段一起清掉。
func deleteWorkspace(data *canonical.Value, workspace string) {
	workspaces := lookup(data, "workspaces")
	if !workspaces.IsObject() {
		return
	}
	workspaces.DeleteKey(workspace)
	if workspaces.Obj.Len() == 0 {
		data.DeleteKey("workspaces")
		return
	}
	data.SetKey("workspaces", workspaces)
}

// RequireTask 返回默认工作空间里的指定任务，不存在时报 404。
//
// 对齐 config_operations.py:1139。
func RequireTask(data *canonical.Value, taskName string) (*canonical.Value, error) {
	return RequireTaskIn(data, config.DefaultWorkspace, taskName)
}

// RequireTaskIn 返回指定工作空间里的任务，不存在时报 404。
func RequireTaskIn(data *canonical.Value, workspace, taskName string) (*canonical.Value, error) {
	task := lookup(WorkspaceTasks(data, workspace), taskName)
	if !task.IsObject() {
		return nil, opErrf(404, "任务不存在: %s", taskName)
	}
	return task, nil
}

// resolveTaskModel 把任务里的模型名（ID 或可见别名）解析成真实模型 ID。
//
// 对齐 config_operations.py:1146。注意别名匹配用的是 Python 的 `in`：别名列表
// 若是字符串会被当成子串匹配，这里按元素匹配处理（见 doc.go 的边界说明）。
func resolveTaskModel(data *canonical.Value, modelName *canonical.Value, field string) (string, error) {
	name, err := nonEmpty(modelName, field)
	if err != nil {
		return "", err
	}
	configured, err := Models(data)
	if err != nil {
		return "", err
	}
	for _, pair := range objectItems(configured) {
		if name == pair.Key {
			return pair.Key, nil
		}
		// Python: `name in (model.get("aliases") or [])`。`or []` 先把假值
		// （含 null）换成空列表，真值的字符串则按**子串**匹配。
		aliases := lookup(pair.Value, "aliases")
		if !aliases.Truthy() {
			continue
		}
		found, err := pyContains(aliases, name)
		if err != nil {
			return "", err
		}
		if found {
			return pair.Key, nil
		}
	}
	return "", opErrf(404, "%s 引用了未配置的模型: %s", field, name)
}

// validateTask 用完整配置解析一次候选任务，把 config 包的校验错误转成 422。
//
// 对齐 config_operations.py:1155：先在候选配置上校验，再落盘，否则校验失败会在
// 配置里留下半个任务。
func validateTask(data *canonical.Value, taskName string, task *canonical.Value) error {
	return validateTaskIn(data, config.DefaultWorkspace, taskName, task)
}

// validateTaskIn 是 validateTask 的工作空间版本。
func validateTaskIn(data *canonical.Value, workspace, taskName string, task *canonical.Value) error {
	candidate := data.Clone()
	merged := mergeTaskObject(WorkspaceTasks(data, workspace), taskName, task)
	// 写进克隆体的对应工作空间，这样 config 层的校验看到的就是落盘后的样子
	// （含「任务名在同一空间内唯一」这条）。
	writeWorkspaceTasks(candidate, workspace, merged)
	if _, err := config.FromDict(candidate); err != nil {
		return opErr(422, err.Error())
	}
	return nil
}

// CreateTaskOptions 是 CreateTask 的参数。
type CreateTaskOptions struct {
	Model         string
	FallbackModel *string
	Params        *canonical.Value
}

// CreateTask 在默认工作空间新建任务并返回它。
//
// 对齐 config_operations.py:1163。模型名先解析成模型 ID；备选与首选相同时不写
// fallback_model；params 为空对象时整段不写。
func CreateTask(data *canonical.Value, taskName string, options CreateTaskOptions) (*canonical.Value, error) {
	return CreateTaskIn(data, config.DefaultWorkspace, taskName, options)
}

// CreateTaskIn 在指定工作空间新建任务。
//
// 重名判定只看**同一个工作空间**：两个空间各有一个同名任务是合法的。
func CreateTaskIn(
	data *canonical.Value,
	workspace, taskName string,
	options CreateTaskOptions,
) (*canonical.Value, error) {
	name, err := nonEmptyString(taskName, "任务名")
	if err != nil {
		return nil, err
	}
	workspace = normalizeWorkspace(workspace)
	existing := WorkspaceTasks(data, workspace)
	if existing.Obj.Has(name) {
		return nil, opErrf(409, "任务已存在: %s", name)
	}
	primary, err := resolveTaskModel(data, canonical.NewString(options.Model), "model")
	if err != nil {
		return nil, err
	}
	task := canonical.NewObjectOf(canonical.ObjectPair{Key: "model", Value: canonical.NewString(primary)})
	if options.FallbackModel != nil && *options.FallbackModel != "" {
		fallback, err := resolveTaskModel(data, canonical.NewString(*options.FallbackModel), "fallback_model")
		if err != nil {
			return nil, err
		}
		if fallback != primary {
			task.SetKey("fallback_model", canonical.NewString(fallback))
		}
	}
	if options.Params != nil && options.Params.Truthy() {
		task.SetKey("params", options.Params.Clone())
	}
	if err := validateTaskIn(data, workspace, name, task); err != nil {
		return nil, err
	}
	writeWorkspaceTasks(data, workspace, mergeTaskObject(existing, name, task))
	return task, nil
}

// UpdateTaskOptions 是 UpdateTask 的可选参数。
type UpdateTaskOptions struct {
	Model          *string
	FallbackModel  *string
	UpdateFallback bool
	Params         *canonical.Value
	UpdateParams   bool
}

// UpdateTask 修改默认工作空间里的任务，返回任务名。
//
// 对齐 config_operations.py:1189。落盘用的是 `current.clear()` + `current.update()`
// 的原地改写：任务对象在 data 里的**身份不变**，但键顺序会变成更新后的顺序。
// Go 侧逐个 DeleteKey/SetKey 复刻这一点。
func UpdateTask(data *canonical.Value, taskName string, options UpdateTaskOptions) (string, error) {
	return UpdateTaskIn(data, config.DefaultWorkspace, taskName, options)
}

// UpdateTaskIn 修改指定工作空间里的任务。
func UpdateTaskIn(
	data *canonical.Value,
	workspace, taskName string,
	options UpdateTaskOptions,
) (string, error) {
	current, err := RequireTaskIn(data, workspace, taskName)
	if err != nil {
		return "", err
	}
	updated := current.Clone()
	if options.Model != nil {
		resolved, err := resolveTaskModel(data, canonical.NewString(*options.Model), "model")
		if err != nil {
			return "", err
		}
		updated.SetKey("model", canonical.NewString(resolved))
	}
	if options.UpdateFallback {
		if options.FallbackModel != nil && *options.FallbackModel != "" {
			resolved, err := resolveTaskModel(data, canonical.NewString(*options.FallbackModel), "fallback_model")
			if err != nil {
				return "", err
			}
			if !pyEqualsString(lookup(updated, "model"), resolved) {
				updated.SetKey("fallback_model", canonical.NewString(resolved))
			} else {
				updated.DeleteKey("fallback_model")
			}
		} else {
			updated.DeleteKey("fallback_model")
		}
	}
	if options.UpdateParams {
		if options.Params != nil && options.Params.Truthy() {
			updated.SetKey("params", options.Params.Clone())
		} else {
			updated.DeleteKey("params")
		}
	}
	if err := validateTaskIn(data, workspace, taskName, updated); err != nil {
		return "", err
	}
	for _, key := range current.Obj.Keys() {
		current.DeleteKey(key)
	}
	for _, pair := range objectItems(updated) {
		current.SetKey(pair.Key, pair.Value)
	}
	return taskName, nil
}

// DeleteTask 删除默认工作空间里的任务；删掉最后一个任务时连 tasks 键一起清掉。
//
// 对齐 config_operations.py:1223。注意现有 tasks 对象是被**原地**清空的，
// data["tasks"] 的赋值因此不会改变它在 data 里的位置。
func DeleteTask(data *canonical.Value, taskName string) error {
	return DeleteTaskIn(data, config.DefaultWorkspace, taskName)
}

// DeleteTaskIn 删除指定工作空间里的任务。
//
// 删掉空间里最后一个任务时，这个命名工作空间也一并消失（空分组没有意义）；
// 默认工作空间则是清掉 tasks 键。
func DeleteTaskIn(data *canonical.Value, workspace, taskName string) error {
	if _, err := RequireTaskIn(data, workspace, taskName); err != nil {
		return err
	}
	remaining := WorkspaceTasks(data, workspace)
	remaining.DeleteKey(taskName)
	writeWorkspaceTasks(data, workspace, remaining)
	return nil
}

// RenameWorkspace 把命名工作空间的任务整体搬到新名字下，旧名字随之消失。
//
// 工作空间只是「一组任务」的容器（见 WorkspaceTasks 的说明），因此改名就是把整组
// 任务换个键：任务本身一个字都不用动，也不需要逐条重建。
//
// 目标名已存在时报 409 而不是合并——两个空间各有一批任务时，合并会在一瞬间产生
// 重名任务，而重名任务在 config 层是非法的，等于用一个必然失败的中间态做一次
// 有损操作。
func RenameWorkspace(data *canonical.Value, workspace, newName string) (string, error) {
	source, err := requireNamedWorkspace(workspace)
	if err != nil {
		return "", err
	}
	if err := requireWorkspaceTasks(data, source); err != nil {
		return "", err
	}
	target, err := nonEmptyString(newName, "工作空间名")
	if err != nil {
		return "", err
	}
	target = normalizeWorkspace(target)
	if target == config.DefaultWorkspace {
		return "", opErrf(409, "工作空间名重复: %s", config.DefaultWorkspace)
	}
	if target == source {
		return target, nil
	}
	if WorkspaceTasks(data, target).Obj.Len() > 0 {
		return "", opErrf(409, "工作空间已存在: %s", target)
	}
	// 先写目标再清来源：两处都持有同一批任务对象，顺序反了会把刚搬过去的任务删掉。
	writeWorkspaceTasks(data, target, WorkspaceTasks(data, source))
	writeWorkspaceTasks(data, source, canonical.NewObject())
	return target, nil
}

// DeleteWorkspace 删除命名工作空间及其中的全部任务。
//
// 它等价于「把这个空间里的任务一次删光」——空分组不进配置，因此删除之后这个名字
// 就不再存在，而不是留下一个空壳。
func DeleteWorkspace(data *canonical.Value, workspace string) error {
	name, err := requireNamedWorkspace(workspace)
	if err != nil {
		return err
	}
	if err := requireWorkspaceTasks(data, name); err != nil {
		return err
	}
	writeWorkspaceTasks(data, name, canonical.NewObject())
	return nil
}

// requireNamedWorkspace 归一化并确认这是一个**命名**工作空间。
//
// 默认工作空间刻意排除在外：它是不带 X-AMKR-Workspace 头时命中的那个空间，永远
// 存在于清单里（见 RouterConfig.WorkspaceNames），因此既删不掉也改不了名——改名会
// 让全部缺省调用突然落空，删除则删不掉它、只能清空，两种结果都不是调用方要的。
func requireNamedWorkspace(workspace string) (string, error) {
	name := normalizeWorkspace(workspace)
	if name == config.DefaultWorkspace {
		return "", opErr(400, "不能修改或删除默认工作空间")
	}
	return name, nil
}

// requireWorkspaceTasks 确认工作空间里确实有任务。
//
// 工作空间由任务反推（空分组不进配置），所以「空间存在」就等于「它里面有任务」。
func requireWorkspaceTasks(data *canonical.Value, workspace string) error {
	if WorkspaceTasks(data, workspace).Obj.Len() == 0 {
		return opErrf(404, "工作空间不存在: %s", workspace)
	}
	return nil
}

// RepairTasks 删掉引用已不存在模型（或参数非法）的任务，返回被清理的任务名（升序）。
//
// 对齐 config_operations.py:1234。模型没了，指向它的任务留着只会在请求时变成
// 404，不如在写模型时一起清掉——与 unified_model 的既定行为保持一致。
//
// 工作空间全都要修：每个空间的修复规则相同，但**默认空间的任务名按原样返回**，
// 命名空间的任务返回 `空间/任务名`——两个空间可能有同名任务，不限定就无法区分。
func RepairTasks(data *canonical.Value) ([]string, error) {
	configured, err := Models(data)
	if err != nil {
		return nil, err
	}
	aliasToID := map[string]string{}
	for _, pair := range objectItems(configured) {
		// Python: `(model_id, *(model.get("aliases") or []))` —— 有 `or []`
		// 兜底，因此 null / 0 / 空串都退化成「没有别名」，不是 TypeError。
		aliases, err := iterateOrEmpty(pair.Value, "aliases")
		if err != nil {
			return nil, err
		}
		aliasToID[pair.Key] = pair.Key
		for _, alias := range aliases {
			aliasToID[alias] = pair.Key
		}
	}

	removed := map[string]bool{}
	repairTaskGroup(data, config.DefaultWorkspace, aliasToID, removed)

	rawWorkspaces := lookup(data, "workspaces")
	if rawWorkspaces != nil && !rawWorkspaces.IsObject() {
		// 形状非法的 workspaces 无法「修复」，留着会让后续每次 FromDict 都失败、
		// 配置再也写不回去，因此整个丢掉——与丢掉非对象任务条目同理。
		data.DeleteKey("workspaces")
		rawWorkspaces = nil
	}
	// rawWorkspaces 就是 data 里的那个对象（writeWorkspaceTasks 也只做原地改写），
	// 因此删掉非法成员会直接反映到配置上。
	for _, workspace := range objectKeys(rawWorkspaces) {
		if !lookup(rawWorkspaces, workspace).IsObject() {
			// 形状非法的空间整个丢掉：留着它会让后续每次 FromDict 都失败，
			// 配置再也写不回去。
			rawWorkspaces.DeleteKey(workspace)
			removed[workspace+"/"] = true
			continue
		}
		repairTaskGroup(data, workspace, aliasToID, removed)
	}
	if current := lookup(data, "workspaces"); current.IsObject() && current.Obj.Len() == 0 {
		data.DeleteKey("workspaces")
	}
	return sortedSet(removed), nil
}

// repairTaskGroup 修复一个工作空间内的任务，把被删掉的任务名记进 removed。
//
// 命名空间里的任务用 `空间/任务名` 记录：两个空间可能有同名任务，不限定就无法区分。
// 默认空间保持原名——那批名字由对拍语料锁定。
func repairTaskGroup(
	data *canonical.Value,
	workspace string,
	aliasToID map[string]string,
	removed map[string]bool,
) {
	current := WorkspaceTasks(data, workspace)
	qualify := workspace != "" && workspace != config.DefaultWorkspace
	for _, name := range objectKeys(current) {
		removeKey := name
		if qualify {
			removeKey = workspace + "/" + name
		}
		task := lookup(current, name)
		if !task.IsObject() {
			current.DeleteKey(name)
			removed[removeKey] = true
			continue
		}
		primary, known := aliasToID[lookup(task, "model").StringValue()]
		if !known {
			current.DeleteKey(name)
			removed[removeKey] = true
			continue
		}
		task.SetKey("model", canonical.NewString(primary))
		if fallbackName, present := task.LookupOK("fallback_model"); present && !fallbackName.IsNull() {
			fallback, known := aliasToID[fallbackName.StringValue()]
			// 备选没了就退化成单模型任务；与首选撞车同理。
			if !known || fallback == primary {
				task.DeleteKey("fallback_model")
			} else {
				task.SetKey("fallback_model", canonical.NewString(fallback))
			}
		}
		// Python 这里构造一次 TaskConfig，唯一会抛 ValueError 的是 params 规整。
		if _, err := config.NormalizeTaskParams(lookup(task, "params"), name); err != nil {
			current.DeleteKey(name)
			removed[removeKey] = true
		}
	}
	// 写回时顺带完成清理：空分组（含默认空间清空后的 tasks 键）都会被移除。
	writeWorkspaceTasks(data, workspace, current)
}

// normalizeWorkspace 归一化工作空间名，与运行时用同一套规则（空名落到默认空间）。
//
// 存在的空间与待建的空间都接受——命名空间由「在它里面建任务」隐式产生，与
// 「工作空间只是任务的分组」这一定位一致，也省掉一步必须先建空间的仪式。
func normalizeWorkspace(workspace string) string {
	return config.NormalizeWorkspace(workspace)
}

// mergeTaskObject 复刻 `{**existing, name: task}`：浅拷贝现有任务映射再覆盖一项。
//
// 刻意浅拷贝：参照实现也是这样，子对象与 existing 共享。
func mergeTaskObject(existing *canonical.Value, name string, task *canonical.Value) *canonical.Value {
	merged := canonical.NewObject()
	for _, pair := range objectItems(existing) {
		merged.SetKey(pair.Key, pair.Value)
	}
	merged.SetKey(name, task)
	return merged
}
