package server

import (
	"net/http"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/webui"
)

// buildHandler 组装整棵路由树。
//
// 与参照实现的对应关系：
//
//	register_management_api(app, ...)   app.py:138  -> "/api/" 交给 api.Server
//	register_ops_api(app, ...)          app.py:145  -> api.Server.OpsEnabled（同一棵 mux）
//	register_webui(app, ...)            app.py:147  -> "/ui/" 交给 webui.Handler
//	@app.head("/")                      app.py:149  -> "/"（只接受 HEAD）
//	@app.get("/health")                 app.py:153  -> "/health"
//	@app.get("/v1/models")              app.py:183  -> "/v1/models"
//	@app.get("/metrics")                app.py:212  -> "/metrics"
//	@app.get("/metrics/requests")       app.py:235  -> "/metrics/requests"
//	@app.get("/metrics/series")         app.py:279  -> "/metrics/series"
//	app.add_api_route("/v1/{path:path}") app.py:371 -> "/v1/" 交给 proxy
//
// 运维面**没有**独立的前缀分支：与参照实现一样，7 条 /api/* 运维路由注册在管理 API
// 的同一棵 mux 上（api.Server.Handler 在 OpsEnabled 为真时调用 RegisterOps），所以
// 下面只有一条 "/api/" 分支。同一批模式注册两遍会互相遮蔽（外层优先），必须避免。
//
// # 为什么都注册成「方法无关」的模式
//
// Go 1.22 的 ServeMux 有两处与 FastAPI 不同的默认行为，都会改变对外响应：
//
//   - `GET /health` 这个模式**也**匹配 HEAD 请求（实测），而 FastAPI 只注册 GET；
//     参照实现里 `HEAD /health` 是 405 + allow: GET。若用 "GET /health"，Go 会返回
//     200 空体，health 探针会得到相反的结论。
//   - 方法不匹配时 ServeMux 自动回 405 纯文本 "Method Not Allowed"（并带上它自己
//     算出的 Allow），而 FastAPI 回 {"detail":"Method Not Allowed"}。
//
// 因此这里注册路径模式（无方法），在处理器里自行判定方法：两种差异同时消失，
// 而且 405 的 Allow 头由我们显式给出，与参照实现一致（GET 路由 -> "GET",
// `HEAD /` -> "HEAD"）。
func (a *App) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// 管理 API 的 47 条路由（OpsEnabled 为真时 api 会在同一棵 mux 上再注册 7 条运维
	// 路由）：内部 mux 自带 "/" 兜底（404 {"detail":"Not Found"}），与参照实现的
	// FastAPI 兜底一致。
	apiHandler := a.api.Handler()
	mux.Handle("/api/", apiHandler)
	// "/api"（无尾斜杠）：FastAPI 里这是 404（路由是逐个注册的，没有挂载前缀），而
	// ServeMux 会把子树模式的 "/api" 请求 301 重定向到 "/api/"。显式注册精确模式
	// 挡掉重定向，让内层兜底给出同样的 404。
	mux.Handle("/api", apiHandler)

	if handler, mounted := webui.Handler(a.options.WebUIAssets, a.webuiEnabled); mounted {
		path := webui.Path(a.options.MountPrefix)
		// webui.Handler 期望收到**已去掉挂载前缀**的路径（见 internal/webui 的测试
		// 说明），所以这里显式 StripPrefix。
		mux.Handle(path+"/", http.StripPrefix(path, handler))
		a.webuiMounted = true
	}

	a.getRoute(mux, "/health", a.handleHealth)
	// /v1/models 是**混合路径**：GET 归 app 面，POST/PUT/PATCH/DELETE 落进
	// /v1/{path:path} 通配路由，HEAD/OPTIONS 等才是 405。参照实现里
	// `@app.get("/v1/models")` 比通配路由更精确，只吃掉 GET；通配路由的方法集合是
	// ["GET","POST","PUT","PATCH","DELETE"]（app.py:371-373），因此其余四个方法会走到
	// proxy 并得到 proxy 的错误（实测：POST /v1/models -> 404「模型  未配置」）。
	// 若这里简单写成「非 GET 一律 405」，POST /v1/models 就会从 404 变成 405——
	// 语料里的 proxy_catchall_post_models 正是钉住这一点的。
	mux.HandleFunc("/v1/models", a.handleModelsRoute)
	a.getRoute(mux, "/metrics", a.handleMetrics)
	a.getRoute(mux, "/metrics/requests", a.handleMetricsRequests)
	a.getRoute(mux, "/metrics/series", a.handleMetricsSeries)

	// 代理通配：只认 /v1/ 之下，path 由处理器去掉前缀后传给 proxy。
	mux.HandleFunc("/v1/", a.handleProxyRoute)
	// "/v1"（无尾斜杠）：参照实现里 404（Starlette 的 "/v1/{path:path}" 不匹配
	// "/v1"），而 ServeMux 会 307 重定向到 "/v1/"。显式注册精确模式挡掉重定向。
	mux.HandleFunc("/v1", a.handleRoot)

	mux.HandleFunc("/", a.handleRoot)
	return mux
}

// proxyMethods 是参照实现给 /v1/{path:path} 注册的方法集合（app.py:371-373）。
var proxyMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// proxyMethodList 是通配路由 405 的 Allow 取值。
//
// **已知分歧（有实测证据）**：参照实现的 Allow 由 Starlette 用一个集合拼出来，顺序随
// PYTHONHASHSEED 变化——同一个 HEAD /v1/chat/completions 连续三次运行分别是
// "PATCH, DELETE, GET, PUT, POST"、"PUT, PATCH, GET, POST, DELETE"、
// "PUT, POST, GET, DELETE, PATCH"。那串字节因此不是稳定契约，不能进语料（会让
// --check 随机失败）。这里固定成一个有序列表。
//
// 多方法 405 只出现在通配路由上；/v1/models、/health、/metrics* 与 "/" 的 Allow
// 都是单元素（Python 侧同样稳定），它们已被语料逐字节锁定。
const proxyMethodList = "GET, POST, PUT, PATCH, DELETE"

// handleProxyRoute 处理 /v1/{path}：只有参照实现注册的那五个方法才进 proxy，
// 其余方法（HEAD/OPTIONS/TRACE）是 405。
func (a *App) handleProxyRoute(w http.ResponseWriter, r *http.Request) {
	if !proxyMethods[r.Method] {
		writeMethodNotAllowed(w, proxyMethodList)
		return
	}
	a.handleProxy(w, r)
}

// handleModelsRoute 按方法把 /v1/models 分派到 app 面或 proxy（见 buildHandler 的说明）。
func (a *App) handleModelsRoute(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet:
		a.handleModels(w, r)
	case proxyMethods[r.Method]:
		a.handleProxy(w, r)
	default:
		// 参照实现在这里报的 Allow 只有 "GET"：/v1/models 比通配路由更精确，
		// Starlette 的部分匹配只列它自己的方法。
		writeMethodNotAllowed(w, http.MethodGet)
	}
}

// getRoute 注册一条只接受 GET 的路由，并复刻 FastAPI 对其它方法的 405。
func (a *App) getRoute(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		handler(w, r)
	})
}

// handleRoot 处理没有匹配到具体路由的请求。
//
// 对应 FastAPI/Starlette 的两条兜底：
//
//   - "/" 只注册了 HEAD（app.py:149），其它方法是 405 + allow: HEAD；
//   - 其余路径是 404 {"detail":"Not Found"}（Starlette 的默认 404，与 api 包内部的
//     兜底同形）。
//
// "/v1" 也走这里（见 buildHandler），从而与参照实现的 404 对齐。
func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeNotFound(w)
		return
	}
	if r.Method == http.MethodHead {
		// app.py:149-151 的 `Response(status_code=204)`：204 不带 content-type 与
		// content-length。
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeMethodNotAllowed(w, http.MethodHead)
}

// handleProxy 把 /v1/{path} 交给 internal/proxy，path 不含 "/v1/" 前缀
// （对应 app.py:371-373 的 `"/v1/{path:path}"` 与 handle_proxy_request(path, ...)）。
func (a *App) handleProxy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	// app.py:351-369 的包装层：进入时记一次活跃请求，退出时释放。
	//
	// Go 侧比 Python 简单：proxy.Handle **同步**写完整个响应（流式也写完才返回），
	// 所以 defer 释放天然覆盖整条流，不需要 _wrap_active_stream 那样给流式响应换
	// 迭代器。_metrics_dirty.set()（app.py:355）没有对应动作——它只用来唤醒
	// WebSocket 广播，见包文档。
	if adapter := a.currentMetricsAdapter(); adapter != nil {
		adapter.AcquireActive()
		defer adapter.ReleaseActive()
	}
	a.proxy.Handle(w, r, path)
}
