package server

import (
	"net/http"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/runtime"
)

// workspacesPath 是工作空间目录在 WebUI 挂载下的文件名。
//
// **为什么挂在 /ui/ 而不是新开一条 /api/ 路由**：管理面的 47 条路由被逐字节语料
// 锁定，那些语料由已退役的 Python 脚本生成，是本项目兼容性的唯一凭证。工作空间是
// Go 侧新增的能力，Python 侧没有对应实现，因此没有可比对的 oracle——给它手写一条
// /api 语料等于伪造兼容性证据。这与 pricing.go、update.go 选择的出路相同：挂在
// /ui/ 这个不在任何冻结清单里的前缀下。
//
// 代价同样是与 WebUI 同生共死：webui_enabled 为假时 /ui/ 整体不注册，这个目录也
// 不可达。工作空间清单本来就是给任务页用的读数，没有界面时无人消费。
const workspacesPath = "/workspaces.json"

// handleWorkspaces 列出全部工作空间及其任务数。
//
// 为什么要一个目录接口而不是让前端从任务列表里猜：任务列表**按空间过滤**
// （GET /api/tasks 只回当前空间的任务），因此从它推不出「还有哪些空间存在」。
// 没有这份清单，用户就只能靠记住名字来切换，新建的空间也永远发现不了。
//
// 鉴权要求**完整权限**：内容会暴露配置结构（有哪些分组、各有多少任务），不是
// pricing.json 那样的公开数据；访客 key 一律拒绝。
//
// 默认工作空间总在列表里（即使一个任务都没有）：它是调用方不带
// X-AMKR-Workspace 头时命中的那个空间，界面上必须是可见、可选的。
func (a *App) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	a.withFullAuth(w, r, func(resources *runtime.RuntimeResources) {
		writeJSON(w, http.StatusOK, workspaceCatalog(resources.Config))
	})
}

// workspaceCatalog 把配置渲染成工作空间目录。
//
// **有意增补**的接口（参照实现没有工作空间），因此形状由本项目自己定：无需对齐
// 任何 Python 响应，也不用担心与既有客户端的兼容性。这里用
// {workspaces: [{name, task_count}, ...]}：数组而非对象，因为前端要按顺序渲染成
// 下拉项，而对象的键序在 JSON 里本就不是可靠契约。
func workspaceCatalog(cfg *config.RouterConfig) *canonical.Value {
	counts := map[string]int{}
	for i := range cfg.Tasks {
		counts[cfg.Tasks[i].Workspace]++
	}

	items := canonical.NewArray()
	// WorkspaceNames 已经把默认空间排在首位、并滤掉空分组，这里直接照抄顺序：
	// 界面上的下拉项顺序因此与「谁真的有任务」一致，不会出现选中后空无一物的项。
	for _, name := range cfg.WorkspaceNames() {
		items.Arr = append(items.Arr, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "name", Value: canonical.NewString(name)},
			canonical.ObjectPair{Key: "task_count", Value: canonical.NewIntValue(int64(counts[name]))},
		))
	}
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "workspaces", Value: items},
	)
}
