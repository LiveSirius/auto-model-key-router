package server

import (
	"net/http"
	"os"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/runtime"
)

// updateApplyPath 是「立即更新」端点的路径（挂在 WebUI 前缀下）。
//
// **为什么挂在 /ui/ 而不是新开一条 /api/ 路由**：管理面的 47 条路由与运维面的 7 条都被
// 逐字节语料锁定，那些语料由已退役的 Python 脚本生成，是本项目兼容性的唯一凭证。
// 自更新是 Go 版**新增**的能力，Python 侧没有对应实现，因此没有可比对的 oracle——
// 给它手写一条 /api 语料等于伪造兼容性证据。这与 pricing.go 选择的出路相同：挂在
// /ui/ 这个不在任何冻结清单里的前缀下。
//
// 与 pricing.json 的**关键区别**：价格目录是公开只读数据，故不鉴权；而替换可执行文件
// 是本服务能提供的最特权操作，因此这里要求**完整权限**（受限的推理凭据一律拒绝）。
const updateApplyPath = "/update/apply"

// updateStatusPath 报告自更新是否可用（WebUI 据此决定是否显示「立即更新」按钮）。
//
// 它同样不鉴权：内容只是「这个构建是否带自更新能力」，没有任何敏感信息，而按钮要能在
// 用户填入 key 之前就正确地隐藏或显示。
const updateStatusPath = "/update/status"

// handleUpdateStatus 返回自更新能力状态。
func (a *App) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, canonical.NewObjectOf(
		canonical.ObjectPair{Key: "available", Value: canonical.NewBool(a.options.SelfUpdate != nil)},
		canonical.ObjectPair{Key: "current_version", Value: canonical.NewString(a.options.Version)},
	))
}

// handleUpdateApply 执行自更新：下载新版 → 校验 → 就地替换 → 启动收尾助手。
//
// 换完文件后本进程会**优雅关停自己**，由收尾助手在端口释放后按注册形态把服务重新拉起。
// 关停放在响应写出之后，因此调用方（WebUI）能拿到「已更新」的结论，而不是一个连接被
// 掐断的错误。
//
// 助手为什么不由本进程去「停服务」：本进程就是被替换的那个服务，它自己退出即可；
// 若让助手去停，后台服务形态下走的是 `taskkill /T /F`，实测会把助手自己也杀掉。
func (a *App) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if a.options.SelfUpdate == nil {
		// 响亮失败：不假装成功，也不暗示「已经是新版」。这与 api 侧未接线接缝的处理
		// 一致（500 + 点名接缝）。用 501 更贴合语义：服务器不支持该能力。
		writeErrorEnvelope(w, http.StatusNotImplemented, "自更新未接入：该构建未注册更新执行器")
		return
	}
	a.withFullAuth(w, r, func(resources *runtime.RuntimeResources) {
		executable, err := os.Executable()
		if err != nil {
			writeErrorEnvelope(w, http.StatusInternalServerError, "取可执行文件路径失败: "+err.Error())
			return
		}
		result, err := a.options.SelfUpdate(executable)
		if err != nil {
			writeErrorEnvelope(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "updated", Value: canonical.NewBool(result.Updated)},
			canonical.ObjectPair{Key: "current_version", Value: canonical.NewString(result.CurrentVersion)},
			canonical.ObjectPair{Key: "latest_version", Value: canonical.NewString(result.LatestVersion)},
			canonical.ObjectPair{Key: "restart_pending", Value: canonical.NewBool(result.RestartPending)},
			canonical.ObjectPair{Key: "message", Value: canonical.NewString(result.Message)},
		))
		// 只有真的换了文件才关停：没更新（已是最新）或收尾助手没起来时关停服务毫无
		// 意义，只会白白中断用户正在用的界面。
		if result.Updated && result.RestartPending && a.options.RequestShutdown != nil {
			// 放到响应写出之后：此刻 writeJSON 已经完成，关停不会截断这个响应。
			go a.options.RequestShutdown()
		}
	})
}
