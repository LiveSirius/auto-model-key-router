package server

import (
	"net/http"
	"strconv"
	"strings"
)

// 本文件补上参照实现里由 uvicorn 负责、迁移时漏掉的那一半日志：**HTTP 访问日志**。
//
// 参照实现的日志全部由 uvicorn 的 log_config 落到 log_file_path（service.py 的
// uvicorn_log_config）：应用侧日志走 `auto_model_key_router` logger，每个请求的访问
// 日志走 uvicorn 的 `uvicorn.access` logger（uvicorn/protocols/http/httptools_impl.py
// :483-491）：
//
//	self.access_logger.info(
//	    '%s - "%s %s HTTP/%s" %d',
//	    get_client_addr(self.scope), self.scope["method"],
//	    get_path_with_query_string(self.scope), self.scope["http_version"], status_code,
//	)
//
// 落盘后形如：
//
//	2026-09-18 09:22:44,361 INFO uvicorn.access 127.0.0.1:50874 - "GET /metrics?hours=1 HTTP/1.1" 200
//
// 访问日志不是可有可无的装饰：历史日志里 1,618,926 行 uvicorn.access 占了全部
// 1,630,000 余行的 99% 以上。没有它，log_file_path 只在出错时才有内容，WebUI 的
// 「服务日志」面板在正常运行时几乎永远是空的——这正是本次要修的现象。

// accessLog 包装整棵路由树，为每个 HTTP 请求写一行 uvicorn.access 形态的访问日志。
//
// AccessLogger 为 nil 时**完全不包装**（直接返回原 handler）：嵌入方与测试不需要访问
// 日志时，不引入任何多余的包装层与类型断言变化。
func (a *App) accessLog(next http.Handler) http.Handler {
	logger := a.options.AccessLogger
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebSocket 升级**没有** uvicorn.access 行：uvicorn 的访问日志只写在
		// http.response.start 上，而升级请求走的是 websocket 协议实现，它用的是
		// uvicorn.error logger（websockets_impl.py:262 的 `[accepted]`、:280 的 403）。
		// 实测历史日志：`uvicorn.access` 里带 "WebSocket" 的行数为 0。这里照实跳过，
		// 免得凭空多出一行参照实现不会有的日志。
		if isWebSocketUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		recorder := &accessRecorder{ResponseWriter: w}
		// 记录时机与 uvicorn 对齐：它在 http.response.start（响应头写出）时就记，
		// 而不是等响应结束。对**流式响应**这不是细节——一条跑几分钟的 SSE 请求，
		// 参照实现里访问日志在开头就出现了，等结束再记会让日志里的顺序与真实请求
		// 发生顺序完全错位。
		recorder.onStart = func(status int) {
			logger.Info(accessLine(r, status))
		}
		next.ServeHTTP(recorder, r)
		// 兜底：处理器一个字都没写就返回时，net/http 会隐式回 200，此时回调还没
		// 触发过。补记一次，保证每个非升级请求都恰好有一行访问日志。
		recorder.report(http.StatusOK)
	})
}

// accessLine 复刻 uvicorn 的 `'%s - "%s %s HTTP/%s" %d'`（httptools_impl.py:485）：
//
//	127.0.0.1:50874 - "GET /metrics?hours=1 HTTP/1.1" 200
//
// 三段分别对应 get_client_addr / get_path_with_query_string / scope["http_version"]。
// 拼接用显式引号而不是 %q：Python 只是把值原样插进双引号之间，不做转义，%q 会额外
// 转义反斜杠与非 ASCII 字符，破坏逐字对齐。
func accessLine(r *http.Request, status int) string {
	return r.RemoteAddr + ` - "` + r.Method + " " + pathWithQuery(r) +
		" HTTP/" + strings.TrimPrefix(r.Proto, "HTTP/") + `" ` + strconv.Itoa(status)
}

// pathWithQuery 对应 get_path_with_query_string：
// `urllib.parse.quote(scope["path"])` 再按需接上 `?` + query_string。
//
// r.URL.RequestURI() 正是 EscapedPath + "?" + RawQuery，语义相同。唯一差别是
// Python 对**已经解码**的 path 再 quote 一次，而 Go 用原始转义形式；两者只在
// 路径本身含 %XX 时不同（那种路径本仓库没有）。
func pathWithQuery(r *http.Request) string { return r.URL.RequestURI() }

// 注意：客户端的 host:port 直接取 r.RemoteAddr，对应 get_client_addr 的
// `"%s:%d" % client`。net/http 的 RemoteAddr 本来就是 host:port 形态（IPv6 是
// `[::1]:port`），不需要再拆一次。

// accessRecorder 在状态码第一次确定时回调 onStart，其余行为全部透传给底层
// ResponseWriter。
//
//   - WriteHeader/Write：拿到状态码并只上报一次。net/http 没有回读已写状态码的接口。
//   - Flush：**必须保留**，否则流式响应会退化成缓冲——proxy 与 wsproxy 都靠
//     `w.(http.Flusher)` 把已产出的字节推给客户端（internal/proxy/stream_writers.go
//     :404、internal/wsproxy/upstream.go:110）。net/http 自己的 ResponseWriter 恒
//     实现 Flusher，因此这里无条件提供它是安全的。
//   - Unwrap：让 http.ResponseController 与 http.Hijacker 能穿透包装层。
//
// 关于 Unwrap 有一点值得说清，免得后人高估它：**升级请求根本不经过这个包装层**
// （accessLog 已经用 isWebSocketUpgrade 短路），所以 Unwrap 不在 WebSocket 握手的
// 路径上，去掉它握手照样成功（实测：注释掉 Unwrap 后
// TestAccessLogWrapperKeepsWebSocketUpgradeWorking 仍然通过）。它在这里是「包装层
// 保持透明」的标准做法——一旦将来有处理器用 http.ResponseController 或 Hijack，
// 少了它就只能在运行时才发现。
type accessRecorder struct {
	http.ResponseWriter
	// onStart 在状态码第一次确定时调用一次，对应 uvicorn 在 http.response.start
	// 写访问日志的时机。上报后置 nil，保证只记一行。
	onStart func(status int)
	logged  bool
}

// report 上报状态码，只会生效一次。
func (r *accessRecorder) report(status int) {
	if r.logged {
		return
	}
	r.logged = true
	if r.onStart != nil {
		r.onStart(status)
	}
}

func (r *accessRecorder) WriteHeader(status int) {
	r.report(status)
	r.ResponseWriter.WriteHeader(status)
}

func (r *accessRecorder) Write(b []byte) (int, error) {
	// net/http 在首次 Write 时隐式写 200，因此这里补记一次，免得「只 Write 不
	// WriteHeader」的处理器漏掉访问日志。
	r.report(http.StatusOK)
	return r.ResponseWriter.Write(b)
}

// Flush 透传给底层；底层不支持时静默跳过（与 http.Flusher 的可选语义一致）。
func (r *accessRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap 暴露底层 ResponseWriter，见类型说明。
func (r *accessRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// 编译期断言：包装层必须同时满足 ResponseWriter、Flusher 与 Unwrap 约定。
var (
	_ http.ResponseWriter = (*accessRecorder)(nil)
	_ http.Flusher        = (*accessRecorder)(nil)
	_ interface {
		Unwrap() http.ResponseWriter
	} = (*accessRecorder)(nil)
)
