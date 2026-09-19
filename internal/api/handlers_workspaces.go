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
// 三个端点：GET 列目录、PUT 改名、DELETE 删除。**没有 POST**：空分组不进配置
// （见 configops.writeWorkspaceTasks），空间由「在里面建第一个任务」隐式产生，
// 因此「新建空工作空间」这个动作在本项目里不存在——建任务就是建空间，多一条 POST
// 只会造出配置层随后要清掉的空壳。

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
	items := canonical.NewArray()
	// WorkspaceNames 已经滤掉空分组并按任务出现顺序排列，这里直接照抄顺序。
	for _, name := range cfg.WorkspaceNames() {
		items.Arr = append(items.Arr, workspaceEntry(name, counts[name]))
	}
	return canonical.NewArray(items.Arr...)
}

// workspaceEntry 渲染目录里的一项。
func workspaceEntry(name string, taskCount int) *canonical.Value {
	return objectOf(
		canonical.ObjectPair{Key: "name", Value: canonical.NewString(name)},
		canonical.ObjectPair{Key: "task_count", Value: canonical.NewIntValue(int64(taskCount))},
	)
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
