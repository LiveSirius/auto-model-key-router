package api

import "net/http"

// register 注册全部管理路由。
//
// 用 Go 1.22+ ServeMux 的「方法 + 模板」模式（GET /api/providers/{provider_id}），
// 因此不需要任何第三方路由库；路径参数在 handler 里用 r.PathValue 取。
//
// 为什么不用 http.MethodXxx 常量写模式：ServeMux 的模式语法要求方法名内联在字符串
// 里，写成 "GET /api/..." 是唯一可行形式。这里保持与参照实现的装饰器顺序一致，
// 便于逐条对照 management_api.py。
//
// 路由分两批：**参照实现那批**（routePatterns）与 **Go 侧新增能力**
// （workspacePatterns）。两批注册在同一棵 mux 上，但只有前者的响应字节被对拍语料
// 锁定，因此清单也分开维护。
func (s *Server) register(mux *http.ServeMux) {
	// —— unified-model ——
	mux.HandleFunc("GET /api/unified-model", s.handleGetUnifiedModel)
	mux.HandleFunc("PUT /api/unified-model", s.handleUpdateUnifiedModel)
	mux.HandleFunc("DELETE /api/unified-model", s.handleDeleteUnifiedModel)

	// —— 任务 ——
	mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/tasks/{task_name}", s.handleGetTask)
	mux.HandleFunc("PUT /api/tasks/{task_name}", s.handleUpdateTask)
	mux.HandleFunc("DELETE /api/tasks/{task_name}", s.handleDeleteTask)

	// —— 工作空间（Go 侧新增，见 handlers_workspaces.go 与 workspacePatterns）——
	mux.HandleFunc("GET /api/workspaces", s.handleListWorkspaces)
	mux.HandleFunc("POST /api/workspaces", s.handleCreateWorkspace)
	mux.HandleFunc("PUT /api/workspaces/{workspace}", s.handleRenameWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{workspace}", s.handleDeleteWorkspace)

	// —— 设置 ——
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)
	mux.HandleFunc("POST /api/settings/local-api-key", s.handleRegenerateLocalAPIKey)
	mux.HandleFunc("POST /api/update/check", s.handleCheckUpdate)

	// —— 供应商 ——
	mux.HandleFunc("GET /api/providers", s.handleListProviders)
	mux.HandleFunc("POST /api/providers", s.handleCreateProvider)
	mux.HandleFunc("GET /api/providers/{provider_id}", s.handleGetProvider)
	mux.HandleFunc("PUT /api/providers/{provider_id}", s.handleUpdateProvider)
	mux.HandleFunc("DELETE /api/providers/{provider_id}", s.handleDeleteProvider)
	mux.HandleFunc("GET /api/providers/{provider_id}/keys", s.handleListProviderKeys)
	mux.HandleFunc("POST /api/providers/{provider_id}/keys", s.handleCreateProviderKey)
	mux.HandleFunc("GET /api/providers/{provider_id}/keys/{key_name}", s.handleGetProviderKey)
	mux.HandleFunc("PUT /api/providers/{provider_id}/keys/{key_name}", s.handleUpdateProviderKey)
	mux.HandleFunc("DELETE /api/providers/{provider_id}/keys/{key_name}", s.handleDeleteProviderKey)
	mux.HandleFunc("POST /api/providers/{provider_id}/probe", s.handleProbeProvider)
	mux.HandleFunc("POST /api/providers/{provider_id}/keys/{key_name}/probe", s.handleProbeProviderKey)
	mux.HandleFunc("GET /api/providers/{provider_id}/keys/{key_name}/models", s.handleGetProviderKeyModels)
	mux.HandleFunc("PUT /api/providers/{provider_id}/keys/{key_name}/models", s.handleSetProviderKeyModels)

	// —— 路由（v3 视图，落在配置的 models 段）——
	mux.HandleFunc("GET /api/routes", s.handleListRoutes)
	mux.HandleFunc("POST /api/routes", s.handleCreateRoute)
	mux.HandleFunc("GET /api/routes/{route_id}", s.handleGetRoute)
	mux.HandleFunc("PUT /api/routes/{route_id}", s.handleUpdateRoute)
	mux.HandleFunc("DELETE /api/routes/{route_id}", s.handleDeleteRoute)

	// —— 探测 ——
	mux.HandleFunc("POST /api/probes/keys", s.handleProbeKeys)
	mux.HandleFunc("GET /api/probes/{probe_id}", s.handleGetProbe)
	mux.HandleFunc("POST /api/probes/{probe_id}/cancel", s.handleCancelProbe)

	// —— 配置导入导出 ——
	mux.HandleFunc("POST /api/config/export", s.handleExportConfig)
	mux.HandleFunc("POST /api/config/import", s.handleImportConfig)

	// —— 模型 ——
	mux.HandleFunc("GET /api/models", s.handleListModels)
	mux.HandleFunc("POST /api/models", s.handleCreateModel)
	mux.HandleFunc("GET /api/models/{model_id}", s.handleGetModel)
	mux.HandleFunc("PUT /api/models/{model_id}", s.handleUpdateModel)
	mux.HandleFunc("DELETE /api/models/{model_id}", s.handleDeleteModel)
	mux.HandleFunc("GET /api/models/{model_id}/keys", s.handleListModelKeys)
	mux.HandleFunc("POST /api/models/{model_id}/keys", s.handleCreateModelKey)
	mux.HandleFunc("GET /api/models/{model_id}/keys/{key_name}", s.handleGetModelKey)
	mux.HandleFunc("GET /api/models/{model_id}/keys/{key_name}/stats", s.handleGetModelKeyStats)
	mux.HandleFunc("PUT /api/models/{model_id}/keys/{key_name}", s.handleUpdateModelKey)
	mux.HandleFunc("DELETE /api/models/{model_id}/keys/{key_name}", s.handleDeleteModelKey)
}

// routePatterns 列出参照实现那 47 条「方法 + 模式」，供测试断言它们齐全。
//
// 单独维护一份而不是从 mux 反射取出：ServeMux 不暴露已注册模式，而「47 条都注册
// 了」是验收条件之一，需要一条明确的、可断言的清单。
//
// **顺序是行为的一部分**：Handler 的兜底 405 用它算 Allow，而参照实现只报「第一个
// 路径匹配的路由」的方法（见 allowedMethod）。因此同一路径的多个模式必须按注册顺序
// 排列（`GET /api/models` 在 `POST /api/models` 之前，与 management_api.py 的装饰器
// 顺序一致，也决定了 `PUT /api/models` 的 Allow 是 "GET"）。
//
// **这份清单是冻结的**：每条都被 management_api_corpus.json 逐字节覆盖，加一条就会
// 让 TestCorpusCoversEveryRoute 失败（新增能力没有可比对的 oracle，手写语料等于伪造
// 兼容性证据）。Go 侧新增的路由因此另立 workspacePatterns。
func routePatterns() []string {
	return []string{
		"GET /api/unified-model", "PUT /api/unified-model", "DELETE /api/unified-model",
		"GET /api/tasks", "POST /api/tasks",
		"GET /api/tasks/{task_name}", "PUT /api/tasks/{task_name}", "DELETE /api/tasks/{task_name}",
		"GET /api/settings", "PUT /api/settings", "POST /api/settings/local-api-key",
		"POST /api/update/check",
		"GET /api/providers", "POST /api/providers",
		"GET /api/providers/{provider_id}", "PUT /api/providers/{provider_id}", "DELETE /api/providers/{provider_id}",
		"GET /api/providers/{provider_id}/keys", "POST /api/providers/{provider_id}/keys",
		"GET /api/providers/{provider_id}/keys/{key_name}",
		"PUT /api/providers/{provider_id}/keys/{key_name}",
		"DELETE /api/providers/{provider_id}/keys/{key_name}",
		"GET /api/routes", "POST /api/routes",
		"GET /api/routes/{route_id}", "PUT /api/routes/{route_id}", "DELETE /api/routes/{route_id}",
		"POST /api/probes/keys",
		"POST /api/providers/{provider_id}/probe",
		"POST /api/providers/{provider_id}/keys/{key_name}/probe",
		"GET /api/providers/{provider_id}/keys/{key_name}/models",
		"PUT /api/providers/{provider_id}/keys/{key_name}/models",
		"GET /api/probes/{probe_id}", "POST /api/probes/{probe_id}/cancel",
		"POST /api/config/export", "POST /api/config/import",
		"GET /api/models", "POST /api/models", "GET /api/models/{model_id}",
		"PUT /api/models/{model_id}", "DELETE /api/models/{model_id}",
		"GET /api/models/{model_id}/keys", "POST /api/models/{model_id}/keys",
		"GET /api/models/{model_id}/keys/{key_name}",
		"GET /api/models/{model_id}/keys/{key_name}/stats",
		"PUT /api/models/{model_id}/keys/{key_name}",
		"DELETE /api/models/{model_id}/keys/{key_name}",
	}
}

// workspacePatterns 列出 Go 侧新增的工作空间路由。
//
// 与 routePatterns 分开的**唯一理由是语料**：那 47 条由已退役的参照实现产出，是
// 本项目兼容性的凭证；工作空间在 Python 侧没有对应实现，因此没有可比对的 oracle，
// 给它补一条 /api 语料等于伪造兼容性证据。分开之后 47 条那份仍然逐字节受锁，新增
// 能力则由 workspace_test.go 自己钉形状。
//
// 仍然注册在同一棵 mux 上（而不是像 /ui/pricing.json 那样挂到别处）：工作空间已经是
// 管理面的正式资源，与 /api/tasks 同级；挂在 /ui/ 下会让「管理面必须开 WebUI 才能
// 用」——那是当初为了绕开语料冻结而付的代价，现在没有理由继续付。
//
// 有 POST：应用侧需要「先建空间拿 key、之后才填任务」，因此工作空间有了自己的显式
// 创建入口（空分组本不进配置，见 configops.writeWorkspaceTasks）。POST 建出的空间
// 带 api_key，任务删光了也留得住；不带 key 的空壳依然不存在。
//
// 命名空间的面板 key 能调用的路由**不在这里**：它们是 assets 那一批 /ui/ 端点
// （见 internal/server 的 panel 处理器），刻意不在这份 /api 清单里。
func workspacePatterns() []string {
	return []string{
		"GET /api/workspaces",
		"POST /api/workspaces",
		"PUT /api/workspaces/{workspace}",
		"DELETE /api/workspaces/{workspace}",
	}
}
