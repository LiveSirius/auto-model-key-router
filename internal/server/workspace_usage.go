package server

import (
	"net/http"

	"github.com/Sparrived/auto-model-key-router/internal/metrics"
	"github.com/Sparrived/auto-model-key-router/internal/runtime"
)

// workspaceUsagePath 是工作空间用量读数在 WebUI 挂载下的文件名。
//
// **为什么挂在 /ui/ 而不是新开 /api/metrics/workspaces**：与 workspaces.go、
// pricing.go、update.go 同一条理由——管理面的 47+7 条路由被逐字节语料锁定，而工作
// 空间是 Go 侧新增能力，Python 侧没有对应实现，手写一条 /api 语料等于伪造兼容性
// 证据。挂在 /ui/ 之下则落在所有冻结清单之外。
//
// 与 /metrics 系列的关系：/metrics 是**逐字节对照参照实现**的读数，不能加字段；
// 这份读数是本项目自己的形状，因此单独一条路由，两边互不牵制。
const workspaceUsagePath = "/workspace-usage.json"

// handleWorkspaceUsage 返回按工作空间拆分的用量统计与流向连边。
//
// 鉴权要求**完整权限**：内容会暴露工作空间的用量与模型流向（配置结构的投影），
// 与 /metrics 同级，访客 key 一律拒绝。
//
// 只支持 GET：与 workspaces.go 一致，其余方法回 405 + Allow 头。
func (a *App) handleWorkspaceUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	// hours 走与 /metrics 相同的取值方式（含 all_history），这样「用量统计」页
	// 上的时间选择器对两个接口是同一套语义。
	result, errs := validateQuery(r.URL.Query(), workspaceUsageParams)
	if len(errs) > 0 {
		writeQueryErrors(w, errs)
		return
	}
	a.withFullAuth(w, r, func(resources *runtime.RuntimeResources) {
		store := metricsStoreOf(resources)
		if store == nil {
			writeInternalError(w)
			return
		}
		body, err := store.WorkspaceUsage(metrics.WorkspaceUsageParams{
			Hours: result.hoursOrNil("hours", "all_history"),
		})
		if err != nil {
			// 参数已由 validateQuery 挡住，这里只可能是数据库错误；与 /metrics 一致
			// 冒到 500。
			writeInternalError(w)
			return
		}
		writeJSON(w, http.StatusOK, body)
	})
}
