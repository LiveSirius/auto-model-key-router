package configops

import (
	"strings"

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
// 传入空任务映射时，**没有 api_key** 的工作空间会被整个删掉：工作空间只是「一组
// 任务」的分组，组里没有成员时它既不可观测也没有意义。这样 `workspaces` 段始终只
// 包含非空分组，管理面也就无须发明一套「新建/删除空工作空间」的接口。
//
// 带 api_key 的工作空间是**例外**，要留住：它是应用侧先建空间拿 key、之后才慢慢填
// 任务的落脚点，任务删光了不代表这个空间该消失——那样面板会指向一个不存在的空间，
// 而嵌入方手上的 key 却还在。默认工作空间无条件留住（它恒在清单里）。
func writeWorkspaceTasks(data *canonical.Value, workspace string, tasks *canonical.Value) {
	if workspace == "" || workspace == config.DefaultWorkspace {
		if !tasks.IsObject() || tasks.Obj.Len() == 0 {
			data.DeleteKey("tasks")
			return
		}
		data.SetKey("tasks", tasks)
		return
	}
	workspaces := lookup(data, "workspaces")
	entry := lookup(workspaces, workspace)
	entryIsObject := entry.IsObject()
	if !tasks.IsObject() || tasks.Obj.Len() == 0 {
		if entryIsObject && strings.TrimSpace(entry.Lookup("api_key").StringValue()) != "" {
			// 有 key：只清任务，留壳（壳里的 api_key 原样保留）。
			entry.DeleteKey("tasks")
			workspaces.SetKey(workspace, entry)
			data.SetKey("workspaces", workspaces)
			return
		}
		deleteWorkspace(data, workspace)
		return
	}
	if !workspaces.IsObject() {
		workspaces = canonical.NewObject()
	}
	if !entryIsObject {
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
	// Model 为首选模型；空串表示**尚未指定**——任务可以先建出来占位，请求它时由
	// proxy 明确报 404，而不是静默落到别的模型上。
	Model         string
	DisplayName   string
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
	// model 为空即「尚未指定」：不写这个键，也不做存在性解析（解析空串只会报
	// 「不能为空」，那不是这里要表达的语义）。备选在没有首选时由 config 层拒绝。
	primary := ""
	if strings.TrimSpace(options.Model) != "" {
		if primary, err = resolveTaskModel(data, canonical.NewString(options.Model), "model"); err != nil {
			return nil, err
		}
	}
	task := canonical.NewObject()
	if primary != "" {
		task.SetKey("model", canonical.NewString(primary))
	}
	if options.FallbackModel != nil && strings.TrimSpace(*options.FallbackModel) != "" {
		// 没有首选的备选无处可退：config 层不查这一条（它的校验顺序是对外契约，
		// 不能插新分支），因此在写盘前于此处拒掉。
		if primary == "" {
			return nil, opErrf(422, "任务 %s 指定了备选但没有首选模型", name)
		}
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
	// display_name 落在最后：既有任务的键序因此一个字节都不变，只有真正取了名的
	// 任务才多出一个键。
	if displayName := strings.TrimSpace(options.DisplayName); displayName != "" {
		task.SetKey("display_name", canonical.NewString(displayName))
	}
	if err := validateTaskIn(data, workspace, name, task); err != nil {
		return nil, err
	}
	writeWorkspaceTasks(data, workspace, mergeTaskObject(existing, name, task))
	return task, nil
}

// UpdateTaskOptions 是 UpdateTask 的可选参数。
//
// nil 表示「不动这个字段」；非 nil 表示「要更新」。要清空某个字符串字段时传指向
// 空串的指针，而不是 nil——nil 已经被「不改」占用了。这与既有 Model 的语义一致
// （fallback_model 用了独立的 UpdateFallback 开关，那是语料锁定的历史写法）。
type UpdateTaskOptions struct {
	Model          *string
	DisplayName    *string
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
		// 传空串（或经 API 传来的显式 null）就是「清掉模型」：把任务退回占位状态。
		// 备选随首选一起清掉——没有首选的备选是非法配置。
		if strings.TrimSpace(*options.Model) == "" {
			updated.DeleteKey("model")
			updated.DeleteKey("fallback_model")
		} else {
			resolved, err := resolveTaskModel(data, canonical.NewString(*options.Model), "model")
			if err != nil {
				return "", err
			}
			updated.SetKey("model", canonical.NewString(resolved))
		}
	}
	if options.DisplayName != nil {
		if displayName := strings.TrimSpace(*options.DisplayName); displayName != "" {
			updated.SetKey("display_name", canonical.NewString(displayName))
		} else {
			updated.DeleteKey("display_name")
		}
	}
	if options.UpdateFallback {
		if options.FallbackModel != nil && strings.TrimSpace(*options.FallbackModel) != "" {
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

// RenameWorkspace 把命名工作空间整体搬到新名字下，旧名字随之消失。
//
// 工作空间只是「一组任务」的容器（见 WorkspaceTasks 的说明），因此改名就是把整个
// 分组换个键：任务本身一个字都不用动，也不需要逐条重建。
//
// 搬的是**整个分组**而不是只有 tasks：分组里还可能有 api_key，它属于这个空间，
// 改名时把它落下会让嵌入方的面板突然失效（key 指向的名字已经不存在了）。
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
	if workspaceExists(data, target) {
		return "", opErrf(409, "工作空间已存在: %s", target)
	}
	// 同一个对象换到新键下，再删掉旧键——把它想成「移动」而不是「复制后清空」，
	// 这样 api_key 与 tasks 一起走，也不会出现中间态。
	workspaces := lookup(data, "workspaces")
	workspaces.SetKey(target, lookup(workspaces, source))
	data.SetKey("workspaces", workspaces)
	deleteWorkspace(data, source)
	return target, nil
}

// DeleteWorkspace 删除命名工作空间及其中的全部任务。
//
// 连同 api_key 一起删：key 是这个空间的凭据，空间没了它就不该再能开门（否则删掉
// 再建个同名空间，旧 key 会凭空复活）。
func DeleteWorkspace(data *canonical.Value, workspace string) error {
	name, err := requireNamedWorkspace(workspace)
	if err != nil {
		return err
	}
	if err := requireWorkspaceTasks(data, name); err != nil {
		return err
	}
	deleteWorkspace(data, name)
	return nil
}

// CreateWorkspace 显式建出一个命名工作空间，并给它一个面板 key。
//
// 这是「空分组不进配置」的**唯一例外入口**：写任务时删空即删分组（见
// writeWorkspaceTasks），但这里建出来的空间带着 api_key，因此任务删光了也留得住。
//
// apiKey 为空表示由服务端生成一个（WebUI 走这条）；应用侧传入自己的 key 时原样采用
// ——调用方可能已经有既定的凭据命名规范，替它改名只会让对接方多一层映射。
//
// 返回真正落盘的空间名与 key。key 的合法性在这一层判（而不是只靠 config.Validate）：
// _update_config 不跑 FromDict，非法配置会被直接写盘，下一次热重载才炸——那时调用方
// 已经拿到 200 了。
func CreateWorkspace(data *canonical.Value, name, apiKey string) (string, string, error) {
	target, err := nonEmptyString(name, "工作空间名")
	if err != nil {
		return "", "", err
	}
	target = normalizeWorkspace(target)
	if target == config.DefaultWorkspace {
		return "", "", opErrf(409, "工作空间名重复: %s", config.DefaultWorkspace)
	}
	if workspaceExists(data, target) {
		return "", "", opErrf(409, "工作空间已存在: %s", target)
	}

	key := strings.TrimSpace(apiKey)
	if key == "" {
		if key, err = config.GenerateWorkspaceKey(); err != nil {
			return "", "", err
		}
	}
	// 与 config.Validate 同一组底线，理由见那里的说明。这里判是为了让请求当场拿到
	// 4xx，而不是写进一个下次重载才失败的配置。
	if key == config.VISITOR_API_KEY {
		return "", "", opErrf(422, "工作空间 %s 的 api_key 不能使用保留的访客 key: %s", target, config.VISITOR_API_KEY)
	}
	if localKey := strings.TrimSpace(lookup(data, "local_api_key").StringValue()); localKey != "" && key == localKey {
		return "", "", opErrf(422, "工作空间 %s 的 api_key 不能与 local_api_key 相同", target)
	}
	for _, workspace := range objectItems(lookup(data, "workspaces")) {
		if existing := strings.TrimSpace(lookup(workspace.Value, "api_key").StringValue()); existing != "" && existing == key {
			return "", "", opErrf(422, "工作空间 %s 与 %s 的 api_key 重复", workspace.Key, target)
		}
	}

	workspaces := lookup(data, "workspaces")
	if !workspaces.IsObject() {
		workspaces = canonical.NewObject()
	}
	entry := lookup(workspaces, target)
	if !entry.IsObject() {
		entry = canonical.NewObject()
	}
	entry.SetKey("api_key", canonical.NewString(key))
	workspaces.SetKey(target, entry)
	data.SetKey("workspaces", workspaces)
	return target, key, nil
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

// requireWorkspaceTasks 确认工作空间存在。
//
// 工作空间通常由任务反推（空分组不进配置），所以「空间存在」大多等于「它里面有
// 任务」；但带 api_key 的工作空间可以没有任务仍然存在（见 writeWorkspaceTasks），
// 因此这里必须把 key 也算作存在依据，否则改名/删除会把刚建好的空面板空间判成 404。
func requireWorkspaceTasks(data *canonical.Value, workspace string) error {
	if !workspaceExists(data, workspace) {
		return opErrf(404, "工作空间不存在: %s", workspace)
	}
	return nil
}

// workspaceExists 报告命名工作空间是否存在于配置里（有任务或有 api_key）。
func workspaceExists(data *canonical.Value, workspace string) bool {
	if WorkspaceTasks(data, workspace).Obj.Len() > 0 {
		return true
	}
	entry := lookup(lookup(data, "workspaces"), workspace)
	return entry.IsObject() && strings.TrimSpace(entry.Lookup("api_key").StringValue()) != ""
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
		// model 为空是「尚未指定模型」这个合法状态，不是引用失效：它没有可解析的
		// 模型名，因此不能按「引用了不存在的模型」丢掉——那样用户刚建好的占位任务
		// 会在下一次导入/修复时凭空消失。repair 只有这一个分支不沿用「解析不了就删」。
		primary := ""
		rawPrimary := lookup(task, "model")
		if strings.TrimSpace(rawPrimary.StringValue()) != "" {
			resolved, known := aliasToID[rawPrimary.StringValue()]
			if !known {
				current.DeleteKey(name)
				removed[removeKey] = true
				continue
			}
			primary = resolved
			task.SetKey("model", canonical.NewString(primary))
		}
		if fallbackName, present := task.LookupOK("fallback_model"); present && !fallbackName.IsNull() {
			fallback, known := aliasToID[fallbackName.StringValue()]
			// 备选没了就退化成单模型任务；与首选撞车同理；没有首选时备选也无处可退
			// （config 层会因此判整个配置非法），一并删掉。
			if !known || fallback == primary || primary == "" {
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
