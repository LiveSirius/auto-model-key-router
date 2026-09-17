package wsproxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"github.com/Sparrived/auto-model-key-router/internal/proxysupport"
)

// Upstream 是内置的 ProxyHandler：自己完成「WS 帧 → 上游 HTTP 往返 → WS 帧」。
//
// # 什么时候用它
//
// 生产装配应当注入 internal/proxy 的处理器（`wsproxy.ProxyHandler(handler.Handle)`），
// 那条路径带 key 池、模型改写、重试、错误信封改写与指标。本类型是给**嵌入方**用的：
// 只需要把 WS 上的请求原样转给一个固定上游、不做选键与改写时，用它就能独立工作——
// 上游返回的错误体也会原样透传（不做 OpenAI/Anthropic 信封归一化）。
//
// 它同时也是本包对「WS 路径与 HTTP 路径共用同一套头部与路径规则」这条约束的
// **可测试落点**：四个 helper 全部来自 proxysupport，没有一处在本包重写——
//
//   - proxysupport.UpstreamPath：把 `chat/completions` 一类的路径映射成配置里的
//     上游路径；
//   - proxysupport.JoinURL：拼接基址，两侧斜杠数量不敏感；
//   - proxysupport.UpstreamHeaders：剔除 8 个下游/传输相关头，补
//     `Authorization: Bearer` 与 `Accept-Encoding: identity`；
//   - proxysupport.ResponseHeaders：剔除 content-encoding/content-length/
//     transfer-encoding/connection 这四个描述**上游这一跳**的头。
type Upstream struct {
	// BaseURL 是上游基址（KeyConfig.base_url 一类）。
	BaseURL string
	// APIKey 是上游凭据，会被写成 `Authorization: Bearer <APIKey>`。
	APIKey string
	// Routes 是 upstream_routes 配置，供 UpstreamPath 选择路径。
	Routes map[string]string
	// Native 表示请求体已经是目标方言的原生形态（决定 UpstreamPath 的分支）。
	Native bool
	// Client 为 nil 时用 http.DefaultClient。
	Client *http.Client
	// Logger 为 nil 时不记录。
	Logger *slog.Logger
}

// Handle 实现 ProxyHandler。
//
// 流式判定沿用 proxysupport.IsStreamRequest（`stream is True` 的严格判定）：流式
// 请求**每读一块就 Flush**，于是上游的每个数据块对应下游的一帧，与参照实现的
// StreamingResponse 逐块 yield 同形；非流式请求整体写一次，只产生一帧。
func (u *Upstream) Handle(w http.ResponseWriter, request *http.Request, path string) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	payload := proxysupport.JSONBody(body)

	upstreamPath, err := proxysupport.UpstreamPath(path, payload, u.Native, u.Routes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	target := proxysupport.JoinURL(u.BaseURL, upstreamPath)
	if request.URL != nil && request.URL.RawQuery != "" {
		// 查询串必须带上：参照实现的折算请求保留了握手时的 query_string。
		target += "?" + request.URL.RawQuery
	}

	upstreamRequest, err := http.NewRequestWithContext(
		request.Context(), http.MethodPost, target, bytes.NewReader(body),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for key, value := range proxysupport.UpstreamHeaders(map[string][]string(request.Header), u.APIKey) {
		upstreamRequest.Header.Set(key, value)
	}

	client := u.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(upstreamRequest)
	if err != nil {
		u.log().Error("websocket proxy upstream failed", "path", path, "error", err)
		http.Error(w, "上游请求失败", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	for key, value := range proxysupport.ResponseHeaders(map[string][]string(response.Header)) {
		w.Header().Set(key, value)
	}
	w.WriteHeader(response.StatusCode)

	if !proxysupport.IsStreamRequest(payload) {
		content, readErr := io.ReadAll(response.Body)
		if readErr != nil && len(content) == 0 {
			return
		}
		if len(content) > 0 {
			_, _ = w.Write(content)
		}
		return
	}

	// 流式：每读一块就 Flush，flush 即帧边界（见 capture 的说明）。
	flusher, canFlush := w.(http.Flusher)
	buffer := make([]byte, 4096)
	for {
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (u *Upstream) log() *slog.Logger {
	if u.Logger == nil {
		return slog.New(discardHandler{})
	}
	return u.Logger
}
