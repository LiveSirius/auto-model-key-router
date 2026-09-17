package api

import "net/http"

// register 注册全部 47 条管理路由。
//
// 用 Go 1.22+ ServeMux 的「方法 + 模板」模式（GET /api/providers/{provider_id}），
// 因此不需要任何第三方路由库；路径参数在 handler 里用 r.PathValue 取。
//
// 为什么不用 http.MethodXxx 常量写模式：ServeMux 的模式语法要求方法名内联在字符串
// 里，写成 "GET /api/..." 是唯一可行形式。这里保持与参照实现的装饰器顺序一致，
// 便于逐条对照 management_api.py。
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

// routePatterns 列出全部已注册的「方法 + 模式」，供测试断言 47 条路由齐全。
//
// 单独维护一份而不是从 mux 反射取出：ServeMux 不暴露已注册模式，而「47 条都注册
// 了」是验收条件之一，需要一条明确的、可断言的清单。
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
		"POST /api/models", "GET /api/models", "GET /api/models/{model_id}",
		"PUT /api/models/{model_id}", "DELETE /api/models/{model_id}",
		"GET /api/models/{model_id}/keys", "POST /api/models/{model_id}/keys",
		"GET /api/models/{model_id}/keys/{key_name}",
		"GET /api/models/{model_id}/keys/{key_name}/stats",
		"PUT /api/models/{model_id}/keys/{key_name}",
		"DELETE /api/models/{model_id}/keys/{key_name}",
	}
}
