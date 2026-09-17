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
	value := lookup(data, "tasks")
	if value.IsObject() {
		return value
	}
	return canonical.NewObject()
}

// RequireTask 返回指定任务，不存在时报 404。
//
// 对齐 config_operations.py:1139。
func RequireTask(data *canonical.Value, taskName string) (*canonical.Value, error) {
	task := lookup(ExistingTasks(data), taskName)
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
	candidate := data.Clone()
	candidate.SetKey("tasks", mergeTaskObject(ExistingTasks(data), taskName, task))
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

// CreateTask 新建任务并返回它。
//
// 对齐 config_operations.py:1163。模型名先解析成模型 ID；备选与首选相同时不写
// fallback_model；params 为空对象时整段不写。
func CreateTask(data *canonical.Value, taskName string, options CreateTaskOptions) (*canonical.Value, error) {
	name, err := nonEmptyString(taskName, "任务名")
	if err != nil {
		return nil, err
	}
	existing := ExistingTasks(data)
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
	if err := validateTask(data, name, task); err != nil {
		return nil, err
	}
	data.SetKey("tasks", mergeTaskObject(existing, name, task))
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

// UpdateTask 修改任务，返回任务名。
//
// 对齐 config_operations.py:1189。落盘用的是 `current.clear()` + `current.update()`
// 的原地改写：任务对象在 data 里的**身份不变**，但键顺序会变成更新后的顺序。
// Go 侧逐个 DeleteKey/SetKey 复刻这一点。
func UpdateTask(data *canonical.Value, taskName string, options UpdateTaskOptions) (string, error) {
	current, err := RequireTask(data, taskName)
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
	if err := validateTask(data, taskName, updated); err != nil {
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

// DeleteTask 删除任务；删掉最后一个任务时连 tasks 键一起清掉。
//
// 对齐 config_operations.py:1223。注意现有 tasks 对象是被**原地**清空的，
// data["tasks"] 的赋值因此不会改变它在 data 里的位置。
func DeleteTask(data *canonical.Value, taskName string) error {
	if _, err := RequireTask(data, taskName); err != nil {
		return err
	}
	remaining := ExistingTasks(data)
	remaining.DeleteKey(taskName)
	if remaining.Obj.Len() > 0 {
		data.SetKey("tasks", remaining)
	} else {
		data.DeleteKey("tasks")
	}
	return nil
}

// RepairTasks 删掉引用已不存在模型（或参数非法）的任务，返回被清理的任务名（升序）。
//
// 对齐 config_operations.py:1234。模型没了，指向它的任务留着只会在请求时变成
// 404，不如在写模型时一起清掉——与 unified_model 的既定行为保持一致。
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
	current := ExistingTasks(data)
	for _, name := range objectKeys(current) {
		task := lookup(current, name)
		if !task.IsObject() {
			current.DeleteKey(name)
			removed[name] = true
			continue
		}
		primary, known := aliasToID[lookup(task, "model").StringValue()]
		if !known {
			current.DeleteKey(name)
			removed[name] = true
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
			removed[name] = true
		}
	}
	// 全部清空后连 tasks 键一起移除，避免配置里留个空对象影响版本号与导出。
	if current.Obj.Len() > 0 {
		data.SetKey("tasks", current)
	} else {
		data.DeleteKey("tasks")
	}
	return sortedSet(removed), nil
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
