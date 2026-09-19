package api

import (
	"net/http"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/configops"
)

// 任务工作空间的管理端点。
//
// 「工作空间」是任务的命名空间容器：任务名只在空间内唯一，不同空间可以有同名任务。
// 这里管的是**空间自身**（空间名是路径参数，因为它是被操作的那个资源），而
// /api/tasks* 管的是**任务**（用 X-AMKR-Workspace 头选空间）。
//
// 四个端点：GET 列目录、POST 新建（带面板 key）、PUT 改名、DELETE 删除。
//
// POST 是工作空间**自己**的创建入口：空分组本不进配置（见
// configops.writeWorkspaceTasks），但应用侧需要「先建空间拿 key、之后才填任务」，
// 因此建出来的空间带一个 api_key，任务删光了也留得住。没有 key 的空壳仍然不存在
// ——建任务依然是建空间的正常途径，POST 只多给了一条能拿到凭据的路。

// workspaceCatalog 把配置渲染成工作空间目录。
//
// 形状 [{name, task_count}, ...]：数组而非对象，因为调用方要按顺序渲染成下拉项，
// 而对象的键序在 JSON 里本就不是可靠契约。默认工作空间固定排在首位
// （WorkspaceNames 的既有语义），因此界面上永远有一个可直接选中的项。
func workspaceCatalog(cfg *config.RouterConfig) *canonical.Value {
	counts := map[string]int{}
	for i := range cfg.Tasks {
		counts[cfg.Tasks[i].Workspace]++
	}
	entries := map[string]config.WorkspaceConfig{}
	for _, workspace := range cfg.Workspaces {
		entries[workspace.Name] = workspace
	}
	items := canonical.NewArray()
	// WorkspaceNames 已经滤掉空分组并按任务出现顺序排列，这里直接照抄顺序。
	for _, name := range cfg.WorkspaceNames() {
		items.Arr = append(items.Arr, workspaceEntryOf(name, counts[name], entries[name]))
	}
	return canonical.NewArray(items.Arr...)
}

// workspaceEntry 渲染目录里的一项（只需名字与任务数时用这条）。
func workspaceEntry(name string, taskCount int) *canonical.Value {
	return workspaceEntryOf(name, taskCount, config.WorkspaceConfig{})
}

// workspaceEntryOf 渲染目录里的一项。
//
// 刻意**不含**任何 key：目录是给管理面列表用的，把每个空间的凭据随列表一起发出去，
// 等于让任何一次 GET 都成了取 key 的入口。key 只在新建那一次的响应里明文出现。
//
// 但**含 models**：可直呼模型清单不是凭据，而是运维需要看见并编辑的授权配置。列表
// 里看不到它，用户就只能靠记忆力去猜某个空间为什么调不动某个模型。
func workspaceEntryOf(name string, taskCount int, workspace config.WorkspaceConfig) *canonical.Value {
	entry := objectOf(
		canonical.ObjectPair{Key: "name", Value: canonical.NewString(name)},
		canonical.ObjectPair{Key: "task_count", Value: canonical.NewIntValue(int64(taskCount))},
		// has_inference_key 是**布尔**而不是 key 本身：界面要能提示"这个空间发过推理
		// 凭据，可以轮换"，但没有任何理由把凭据回显出来。
		canonical.ObjectPair{Key: "has_inference_key", Value: canonical.NewBool(workspace.InferenceKey != "")},
	)
	// models 只在配置里**显式写了**这个字段时才出现：省略表示「不限制」，空数组表示
	// 「一个都不许直呼」。两者对界面是有区别的状态，因此不能让空数组同时代表它们
	// ——那会把运维下的禁令在界面上显示成「未限制」。
	if workspace.Models != nil {
		list := make([]*canonical.Value, 0, len(workspace.Models))
		for _, model := range workspace.Models {
			list = append(list, canonical.NewString(model))
		}
		entry.SetKey("models", canonical.NewArray(list...))
	}
	return entry
}

// workspaceName 取本请求要操作的命名工作空间（路径参数）。
//
// 去空白后为空的段不会正常出现在 URL 里，但显式编码的 "%20" 可以，因此仍要判一次：
// 空名会被 NormalizeWorkspace 悄悄归一成默认空间，而这里的毛病恰恰是「名字不合法」，
// 静默落到默认空间上会让改/删打到一个用户没指定的空间。
func workspaceName(r *http.Request) (string, error) {
	name := trimSpace(r.PathValue("workspace"))
	if name == "" {
		return "", httpErrorf(422, "工作空间名不能为空")
	}
	return name, nil
}

// —— GET /api/workspaces ——

// handleListWorkspaces 列出全部**有任务**的工作空间及各自任务数。
//
// 为什么要一个目录接口而不是让前端从任务列表里猜：GET /api/tasks 是**按空间过滤**
// 的，因此从它推不出「还有哪些空间存在」。没有这份清单，用户就只能靠记住名字来切换，
// 新建的空间也永远发现不了。
//
// 默认工作空间总在列表里（即使一个任务都没有）：它是调用方不带 X-AMKR-Workspace 头
// 时命中的那个空间，界面上必须是可见、可选的。
func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusOK, func() (*canonical.Value, error) {
		cfg, err := s.authorizedConfig(r)
		if err != nil {
			return nil, err
		}
		data, err := s.managementConfigData()
		if err != nil {
			return nil, err
		}
		return withRevision(data, objectOf(
			canonical.ObjectPair{Key: "workspaces", Value: workspaceCatalog(cfg)},
		))
	})
}

// —— POST /api/workspaces ——

// handleCreateWorkspace 显式建出一个命名工作空间并返回它的两把 key。
//
// key 在响应里**明文出现一次**，之后目录与任何 GET 都不再回它们——配置里存的是明文，
// 但界面没有「查看已有 key」的入口（要看只能翻配置文件或轮换）。这样偶然的列表请求
// 不会把凭据洒得到处都是。
//
// 两把 key 同时给：这是唯一能拿到明文的时刻（AMKR 没有任何端点会再回已有 key）。
// api_key 嵌面板，inference_key 配到项目环境变量里做 /v1 调用。
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusCreated, func() (*canonical.Value, error) {
		payload, _, err := decodePayload(r, specWorkspaceCreate, false)
		if err != nil {
			return nil, err
		}
		name := *optString(payload, "name")
		apiKey := ""
		if given := optString(payload, "api_key"); given != nil {
			apiKey = *given
		}

		var created configops.CreatedWorkspace
		if _, err := s.updateConfig(r, func(data *canonical.Value) error {
			var err error
			created, err = configops.CreateWorkspace(data, name, apiKey)
			return err
		}, optString(payload, "config_revision")); err != nil {
			return nil, err
		}
		data, err := s.managementConfigData()
		if err != nil {
			return nil, err
		}
		return withRevision(data, objectOf(
			canonical.ObjectPair{Key: "name", Value: canonical.NewString(created.Name)},
			canonical.ObjectPair{Key: "task_count", Value: canonical.NewIntValue(0)},
			canonical.ObjectPair{Key: "api_key", Value: canonical.NewString(created.APIKey)},
			canonical.ObjectPair{Key: "inference_key", Value: canonical.NewString(created.InferenceKey)},
		))
	})
}

// —— POST /api/workspaces/{workspace}/inference-key ——

// handleRotateInferenceKey 给工作空间**换一把**推理 key 并返回明文。
//
// 只换推理 key，不动面板 key：两者的轮换理由不同（推理 key 进了各项目的环境变量，
// 泄漏面更宽、轮换更频繁；面板 key 换掉会让已嵌入的页面立刻失效）。合成一个「轮换
// 全部凭据」的端点会逼调用方在只想换一把时承担另一把失效的代价。
//
// 明文只在这一次响应里出现，与建空间一致。旧 key 立即失效（配置里只存新值）。
func (s *Server) handleRotateInferenceKey(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusOK, func() (*canonical.Value, error) {
		name, err := workspaceName(r)
		if err != nil {
			return nil, err
		}
		payload, present, err := decodePayload(r, specRevisionPayload, true)
		if err != nil {
			return nil, err
		}
		var revision *string
		if present {
			revision = optString(payload, "config_revision")
		}
		var key string
		if _, err := s.updateConfig(r, func(data *canonical.Value) error {
			var err error
			key, err = configops.RotateInferenceKey(data, name)
			return err
		}, revision); err != nil {
			return nil, err
		}
		data, err := s.managementConfigData()
		if err != nil {
			return nil, err
		}
		return withRevision(data, objectOf(
			canonical.ObjectPair{Key: "name", Value: canonical.NewString(name)},
			canonical.ObjectPair{Key: "inference_key", Value: canonical.NewString(key)},
		))
	})
}

// —— PUT /api/workspaces/{workspace}/models ——

// handleSetWorkspaceModels 设定本空间**允许直呼**的模型清单。
//
// 三种取值（见 specWorkspaceModels）：数组=允许这些，空数组=一个都不许，null=清除
// 限制回到不限制。把整份清单一次交上来而不是做成增删接口，是因为它天然是一份完整
// 集合：调用方手上已有全量视图，逐条增删只会多出「删到一半」的中间态。
//
// 运维侧按空间收窄可直呼的模型集合，正是「共用网关」在模型维度上的隔离手段——
// 只有任务名是天然隔离的，模型名在配置里是全局的。
func (s *Server) handleSetWorkspaceModels(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusOK, func() (*canonical.Value, error) {
		name, err := workspaceName(r)
		if err != nil {
			return nil, err
		}
		payload, _, err := decodePayload(r, specWorkspaceModels, false)
		if err != nil {
			return nil, err
		}
		// null 与 [] 都是「长度为零」，必须靠 Kind 区分：null 是清除限制，[] 是禁止
		// 一切直呼。optStringSlice 把两者都返回 nil，所以这里自己判 Kind。
		var models []string
		if value, present := payload.LookupOK("models"); present && !value.IsNull() {
			models = optStringSlice(payload, "models")
			if models == nil {
				// 校验层已经挡住非数组；走到这里说明是空数组，给一个非 nil 空切片，
				// 否则 configops 会把它当成「清除限制」。
				models = []string{}
			}
		}
		updated, err := s.updateConfig(r, func(data *canonical.Value) error {
			return configops.SetWorkspaceModels(data, name, models)
		}, optString(payload, "config_revision"))
		if err != nil {
			return nil, err
		}
		data, err := s.managementConfigData()
		if err != nil {
			return nil, err
		}
		count := 0
		for i := range updated.Tasks {
			if updated.Tasks[i].Workspace == name {
				count++
			}
		}
		for _, workspace := range updated.Workspaces {
			if workspace.Name == name {
				return withRevision(data, workspaceEntryOf(name, count, workspace))
			}
		}
		return withRevision(data, workspaceEntry(name, count))
	})
}

// —— PUT /api/workspaces/{workspace} ——

// handleRenameWorkspace 把工作空间改名为请求体里的 name，组内任务整体跟着走。
func (s *Server) handleRenameWorkspace(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusOK, func() (*canonical.Value, error) {
		payload, _, err := decodePayload(r, specWorkspaceRename, false)
		if err != nil {
			return nil, err
		}
		name, err := workspaceName(r)
		if err != nil {
			return nil, err
		}
		newName := *optString(payload, "name")
		// 目标名由 configops 归一化并回传，响应据此渲染（调用方拿到的是真正落盘的
		// 那个名字，而不是自己写的原始字符串）。
		var renamed string
		updated, err := s.updateConfig(r, func(data *canonical.Value) error {
			var err error
			renamed, err = configops.RenameWorkspace(data, name, newName)
			return err
		}, optString(payload, "config_revision"))
		if err != nil {
			return nil, err
		}
		data, err := s.managementConfigData()
		if err != nil {
			return nil, err
		}
		count := 0
		for i := range updated.Tasks {
			if updated.Tasks[i].Workspace == renamed {
				count++
			}
		}
		return withRevision(data, workspaceEntry(renamed, count))
	})
}

// —— DELETE /api/workspaces/{workspace} ——

// handleDeleteWorkspace 删除工作空间连同其中的全部任务。
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusNoContent, func() (*canonical.Value, error) {
		name, err := workspaceName(r)
		if err != nil {
			return nil, err
		}
		payload, present, err := decodePayload(r, specRevisionPayload, true)
		if err != nil {
			return nil, err
		}
		var revision *string
		if present {
			revision = optString(payload, "config_revision")
		}
		_, err = s.updateConfig(r, func(data *canonical.Value) error {
			return configops.DeleteWorkspace(data, name)
		}, revision)
		return nil, err
	})
}
