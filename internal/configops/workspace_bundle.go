package configops

import (
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件是**工作空间整包迁移**：把一个或多个工作空间连任务带面板 key 一起搬走。
//
// 为什么不能复用 /api/config/export：那份是「把整台实例的供应商与模型搬到另一台」，
// 它**刻意剥掉 api_key**（导出文件会被贴到工单与聊天记录里，见 transfer.go 的说明），
// 且合并时按供应商 base_url / key secret 去重、按模型 id 合并——对一个只想把自己的
// 空间整体搬到另一个实例、并保留面板凭据的应用来说，这两条都不对：key 是它要的东西，
// 而它的任务引用的模型在目标实例上**未必存在**（合并规则会按 id 撞车或凭空造模型）。
//
// 因此这是一条独立通道，语义刻意与 TransferableConfig 不同：
//
//   - **带 api_key**：这是有意的凭据搬迁，导入方明确知道自己在搬凭据；
//   - **不带 providers / models**：搬的是命名空间本身，不是模型库。任务里引用的模型
//     在目标实例上解析不了时，由 RepairTasks 按既有规则清掉（那正是它的职责）。
//
// 两者互不影响：/api/config/export 仍然剥 key，这里也不碰 providers。

// WorkspaceBundle 是一次工作空间迁移的内容。
//
// 用配置里 `workspaces` 段的**原样形状**（`{名字: {api_key?, tasks?}}`），而不是另立
// 一套字段：它就是配置的一部分，让用户能把导出的这段直接粘回配置文件，出问题时也能
// 一眼对照。多包一层数组只为表达"可以搬多个空间"。
type WorkspaceBundle struct {
	// version 是这份包的格式版本。工作空间的形状将来可能长出新字段，届时靠它分流。
	Version int
	// names 是包里的工作空间名（升序），用于渲染与冲突判断。
	names []string
	// payload 是 `workspaces` 段的对象（只含被选中的空间）。
	payload *canonical.Value
}

// ExportWorkspaces 抽出指定工作空间的迁移包。
//
// workspaces 为空表示导出**全部**命名工作空间（含只有 key、没有任务的空间）。默认
// 工作空间（顶层的 tasks）不在其中：它没有名字也没有 key，是"不带空间头"的落点，
// 搬到别的实例就等于把对方的默认空间覆盖掉——那是另一件事，不该混进这条通道。
//
// 带 api_key：见文件头的说明，这是与 /api/config/export 的关键差别。
func ExportWorkspaces(data *canonical.Value, workspaces []string) (*WorkspaceBundle, error) {
	available := lookup(data, "workspaces")
	selected := canonical.NewObject()
	names := make([]string, 0)
	// 显式指定时按调用方给的顺序取（重的顺序由调用方决定），并逐个校验存在性——
	// 静默跳过不存在的名字会让用户以为搬走了、实际漏了。
	if len(workspaces) > 0 {
		for _, raw := range workspaces {
			name, err := nonEmptyString(raw, "工作空间名")
			if err != nil {
				return nil, err
			}
			name = normalizeWorkspace(name)
			if name == config.DefaultWorkspace {
				return nil, opErr(400, "不能迁移默认工作空间")
			}
			entry := lookup(available, name)
			if !entry.IsObject() {
				return nil, opErrf(404, "工作空间不存在: %s", name)
			}
			// 任务为空但带 key 的空间也要能搬：它正是"应用先建空间拿 key、还没填
			// 任务"的状态，搬过去就是要接着用。
			selected.SetKey(name, entry.Clone())
			names = append(names, name)
		}
	} else {
		for _, pair := range objectItems(available) {
			if !pair.Value.IsObject() {
				continue
			}
			selected.SetKey(pair.Key, pair.Value.Clone())
			names = append(names, pair.Key)
		}
	}
	if len(names) == 0 {
		return nil, opErr(404, "没有可迁移的工作空间")
	}
	return &WorkspaceBundle{Version: 1, names: names, payload: selected}, nil
}

// BundlePayload 返回包里 `workspaces` 段的对象（原样形状，可直接粘回配置）。
func (b *WorkspaceBundle) BundlePayload() *canonical.Value {
	if b == nil || b.payload == nil {
		return canonical.NewObject()
	}
	return b.payload
}

// Names 返回包里的工作空间名。
func (b *WorkspaceBundle) Names() []string {
	if b == nil {
		return nil
	}
	return append([]string(nil), b.names...)
}

// WorkspaceBundleValue 把包渲染成响应体里的 JSON 值。
//
// 形状刻意**不是**配置形状：
//
//	{version, workspaces: [名字], spaces: {名字: {api_key?, tasks?}}}
//
// 用独立的 `spaces` 而不是把工作空间段塞进一个 `config` 包装层，是因为后者会让
// 「工作空间叫 providers」与「包里混进了配置导出的 providers 段」变成同一种输入——
// 前者是合法的空间名，后者必须被拒。分开之后两者一眼可辨。
//
// 也**不**把这段喂给 config.MigrateConfigData：工作空间是 v4 才有的概念，这份包格式
// 自带 version（当前只有 1），没有需要迁移的旧形状；而让迁移函数遍历一张以空间名为
// 键的表，等于把 `unified_model` 这种空间名当成配置段去改写。
//
// ponytail: version 目前只写 1、读时只校验是不是 1。将来形状真的变了，就在这里按
// version 分流。
func WorkspaceBundleValue(b *WorkspaceBundle) *canonical.Value {
	names := canonical.NewArray()
	for _, name := range b.Names() {
		names.Arr = append(names.Arr, canonical.NewString(name))
	}
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "version", Value: canonical.NewIntValue(int64(b.Version))},
		canonical.ObjectPair{Key: "workspaces", Value: names},
		canonical.ObjectPair{Key: "spaces", Value: b.BundlePayload()},
	)
}

// ParseWorkspaceBundle 解析一份工作空间迁移包。
//
// 只认 `spaces`（见 WorkspaceBundleValue 的形状说明）。把 /api/config/export 的整包
// 粘到这里时会因为没有 spaces 而明确报错，而不是静默丢掉 providers / models 让人以为
// 模型也搬过来了。
func ParseWorkspaceBundle(raw *canonical.Value) (*WorkspaceBundle, error) {
	if raw == nil || !raw.IsObject() {
		return nil, opErr(422, "工作空间迁移包必须是对象")
	}
	if raw.Lookup("providers").IsObject() || raw.Lookup("models").IsObject() {
		return nil, opErr(422, "这看起来是配置迁移包，请使用配置导入导出接口")
	}
	version := raw.Lookup("version")
	if version.IsObject() || version.IsArray() {
		return nil, opErr(422, "工作空间迁移包的 version 非法")
	}
	available := lookup(raw, "spaces")
	if !available.IsObject() || available.Obj.Len() == 0 {
		return nil, opErr(422, "工作空间迁移包里没有任何工作空间（缺少 spaces）")
	}
	payload := canonical.NewObject()
	names := make([]string, 0)
	for _, pair := range objectItems(available) {
		name := normalizeWorkspace(pair.Key)
		if name == config.DefaultWorkspace {
			return nil, opErr(422, "工作空间迁移包不能包含默认工作空间")
		}
		if !pair.Value.IsObject() {
			return nil, opErrf(422, "工作空间 %s 的内容必须是对象", pair.Key)
		}
		payload.SetKey(name, pair.Value.Clone())
		names = append(names, name)
	}
	return &WorkspaceBundle{Version: 1, names: names, payload: payload}, nil
}

// WorkspaceImportResult 是 ImportWorkspaces 的结果。
type WorkspaceImportResult struct {
	// Added 是**新建**的工作空间名。
	Added []string
	// Replaced 是被导入内容**覆盖**的已存在工作空间名。
	//
	// 与 Added 分开报，是因为两者的后果不同：覆盖会换掉目标实例上那个空间的面板
	// key（旧 key 立刻失效）。调用方必须能把这件事告诉用户，否则嵌入方的面板会
	// 毫无征兆地开始 401。
	Replaced []string
	// Renamed 是改过名的空间（原名已在目标实例上存在且选择保留）：`原名 -> 新名`。
	Renamed map[string]string
	// Rekeyed 是**换了 key** 的空间：`最终空间名 -> 新 key`。
	//
	// 只在加前缀克隆时可能出现：那个空间在原实例上仍然存在并占着原 key，克隆体不可能
	// 也用它。调用方必须把新 key 交给嵌入方，否则克隆出来的空间没有可用的面板。
	Rekeyed map[string]string
	// RemovedTasks 是导入后因引用不到模型而被清掉的任务（`空间/任务名`）。
	RemovedTasks []string
}

// ImportWorkspaces 把一份工作空间迁移包并入当前配置。
//
// 冲突策略由 prefix 决定，与"合并"和"覆盖"两种直觉各自对应：
//
//   - prefix 为空：**同空间覆盖**。导入方的同名空间被整包替换（含 key）。这是"恢复
//     备份"的语义——用户要的就是把那个空间变回包里的样子。
//   - prefix 非空（如 "teamA-"）：同名时把导入的空间**改名**成 `prefix + 原名`，一个
//     都不覆盖。这是"把别人的空间搬到我这"的语义——用户不想丢自己已有的东西。
//
// 两种都跑 RepairTasks：包里的任务引用的模型在目标实例上未必存在，解析不了的任务按
// 既有规则清掉并**如实回报**，而不是带进来一批请求时必然 404 的僵尸任务。
func ImportWorkspaces(
	data *canonical.Value,
	bundle *WorkspaceBundle,
	prefix string,
) (WorkspaceImportResult, error) {
	result := WorkspaceImportResult{Renamed: map[string]string{}, Rekeyed: map[string]string{}}
	if bundle == nil {
		return result, opErr(422, "工作空间迁移包为空")
	}
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix != "" {
		trimmedPrefix = normalizeWorkspace(trimmedPrefix)
		if trimmedPrefix == config.DefaultWorkspace {
			return result, opErr(422, "空间名前缀非法")
		}
	}

	for _, name := range bundle.Names() {
		source := lookup(bundle.payload, name)
		if !source.IsObject() {
			continue
		}
		clone := source.Clone()
		target := name
		cloned := false
		if workspaceExists(data, target) {
			if trimmedPrefix == "" {
				result.Replaced = append(result.Replaced, target)
			} else {
				target = availableWorkspaceName(data, trimmedPrefix+name)
				result.Renamed[name] = target
				// 克隆体与原空间**并存**，因此原 key 必然还在被原空间占着，克隆体
				// 不可能也用它。这里换一把新的并如实回报——不换的话加前缀克隆对
				// 「克隆一个本地已存在的空间」这个主要用法永远失败。
				cloned = true
			}
		} else {
			result.Added = append(result.Added, target)
		}
		changed, err := resolveImportKey(data, target, clone, cloned)
		if err != nil {
			return result, err
		}
		if changed {
			result.Rekeyed[target] = strings.TrimSpace(clone.Lookup("api_key").StringValue())
		}
		workspaces := lookup(data, "workspaces")
		if !workspaces.IsObject() {
			workspaces = canonical.NewObject()
		}
		workspaces.SetKey(target, clone)
		data.SetKey("workspaces", workspaces)
	}

	removed, err := RepairTasks(data)
	if err != nil {
		return result, err
	}
	result.RemovedTasks = removed
	if _, err := config.FromDict(data); err != nil {
		return result, opErr(422, err.Error())
	}
	return result, nil
}

// resolveImportKey 决定待导入空间的 key 能不能原样落地，返回是否换了 key。
//
// 分两种情况，差别在于**冲突是不是克隆造成的**：
//
//   - cloned 为真（加前缀克隆）：原 key 必定被原空间占着（它还在），换一把新的。这不是
//     意外，是克隆的必然结果，因此由 Rekeyed 明确回报给调用方——嵌入方需要新 key。
//   - 否则：key 撞上目标实例上**别处**的凭据是**错误**，报出而不是悄悄改写。改写看似
//     "让导入成功"，实际藏起了用户必须知道的事：那把 key 通常已经嵌在别人的页面里，
//     换掉之后旧 key 会指向别人**别的**空间，而响应里没有任何字段能说明这件事。
//
// 保留 key（`amkr-visitor`）与本地主凭据（`local_api_key`）在任何情况下都是错误：它们
// 是配置层明令禁止的入站凭据，静默改写等于把坏包藏起来。措辞与 config.Validate 一致
// （那边是最终防线，这里是为了在**写盘前**给出同一个结论）。
//
// `target` 是本次要写入的位置：prefix 为空时它是被覆盖的那个空间（连同旧 key 一起消失），
// 因此它与自己重复不算冲突。
func resolveImportKey(data *canonical.Value, target string, incoming *canonical.Value, cloned bool) (bool, error) {
	key := strings.TrimSpace(incoming.Lookup("api_key").StringValue())
	if key == "" {
		return false, nil
	}
	if key == config.VISITOR_API_KEY {
		return false, opErrf(422, "工作空间 %s 的 api_key 不能使用保留的访客 key: %s", target, config.VISITOR_API_KEY)
	}
	if local := strings.TrimSpace(lookup(data, "local_api_key").StringValue()); local != "" && key == local {
		return false, opErrf(422, "工作空间 %s 的 api_key 不能与 local_api_key 相同", target)
	}
	if owner := keyOwnerOutside(data, key, target); owner != "" {
		if !cloned {
			return false, opErrf(422, "工作空间 %s 的 api_key 与 %s 重复", target, owner)
		}
		// 克隆：生成一把新的。本次导入中更早写入的空间已经落在 data 里，因此同一个
		// 扫描就能覆盖它们，无需另记一份。
		generated, err := config.GenerateWorkspaceKey()
		if err != nil {
			return false, err
		}
		incoming.SetKey("api_key", canonical.NewString(generated))
		return true, nil
	}
	return false, nil
}

// keyOwnerOutside 返回占用该**面板** key 的那个空间名（`except` 自身除外），没有则空串。
func keyOwnerOutside(data *canonical.Value, key, except string) string {
	return secretOwnerOutside(data, key, except)
}

// secretOwnerOutside 返回占用该凭据的空间名（`except` 自身除外），没有则空串。
//
// 面板 key 与推理 key **一起扫**：两者都不允许在实例内重复出现，且跨类型重复同样
// 有害（同一个字符串会在面板面与 /v1 面各命中一个空间）。判定与 config.Validate
// 的占用表一致。
func secretOwnerOutside(data *canonical.Value, secret, except string) string {
	if secret == "" {
		return ""
	}
	for _, pair := range objectItems(lookup(data, "workspaces")) {
		if pair.Key == except || !pair.Value.IsObject() {
			continue
		}
		for _, field := range []string{"api_key", "inference_key"} {
			if existing := strings.TrimSpace(pair.Value.Lookup(field).StringValue()); existing == secret {
				return pair.Key
			}
		}
	}
	return ""
}

// availableWorkspaceName 在 base 已被占用时依次尝试 `base-2`、`base-3`……。
func availableWorkspaceName(data *canonical.Value, base string) string {
	if !workspaceExists(data, base) {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if !workspaceExists(data, candidate) {
			return candidate
		}
	}
}
