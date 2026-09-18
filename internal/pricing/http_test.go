package pricing

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHTTPFetcherSendsConditionalHeaders 钉住条件请求的头。
//
// If-None-Match 只在该带上时才发：首次取回带空 etag，若发出 `If-None-Match: ""`
// 上游会把它当成一个不匹配的值（无害但不正确）。
func TestHTTPFetcherSendsConditionalHeaders(t *testing.T) {
	var gotETag, gotAccept, gotAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotETag = r.Header.Get("If-None-Match")
		gotAccept = r.Header.Get("Accept")
		gotAgent = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"p":{"models":{}}}`))
	}))
	defer server.Close()

	fetch := HTTPFetcher(server.Client())
	if _, err := fetch(server.URL, "", time.Second); err != nil {
		t.Fatalf("首次取回失败: %v", err)
	}
	if gotETag != "" {
		t.Errorf("无条件请求的 If-None-Match = %q，期望空", gotETag)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q，期望 application/json", gotAccept)
	}
	if gotAgent != userAgent {
		t.Errorf("User-Agent = %q，期望 %q", gotAgent, userAgent)
	}

	result, err := fetch(server.URL, `W/"abc"`, time.Second)
	if err != nil {
		t.Fatalf("条件取回失败: %v", err)
	}
	if gotETag != `W/"abc"` {
		t.Errorf("条件请求的 If-None-Match = %q，期望 W/\"abc\"", gotETag)
	}
	if result.NotModified {
		t.Error("200 响应不应标记为 NotModified")
	}
}

// TestHTTPFetcherHandles304 断言 304 折叠成 NotModified 且保留原 ETag。
//
// 304 响应可能不带 ETag 头；若不保留，下次就会退化成无条件请求（重新下载 4.7 MB）。
func TestHTTPFetcherHandles304(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	result, err := HTTPFetcher(server.Client())(server.URL, `W/"keep"`, time.Second)
	if err != nil {
		t.Fatalf("304 不应是错误: %v", err)
	}
	if !result.NotModified {
		t.Error("NotModified 应当为 true")
	}
	if len(result.Body) != 0 {
		t.Errorf("304 的 Body = %q，期望空", result.Body)
	}
	if result.ETag != `W/"keep"` {
		t.Errorf("304 后的 ETag = %q，期望保留 W/\"keep\"", result.ETag)
	}
}

// TestHTTPFetcherReturnsStatusError 断言非 2xx 是错误，并且错误里带状态码。
func TestHTTPFetcherReturnsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := HTTPFetcher(server.Client())(server.URL, "", time.Second)
	statusErr, ok := err.(*HTTPStatusError)
	if !ok {
		t.Fatalf("错误类型 = %T，期望 *HTTPStatusError", err)
	}
	if statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d，期望 503", statusErr.StatusCode)
	}
	if statusErr.URL != server.URL {
		t.Errorf("URL = %q，期望 %q", statusErr.URL, server.URL)
	}
}

// TestHTTPFetcherEnforcesTimeout 断言超时覆盖**读响应体**的过程，而不只是建连。
//
// 这条很关键：目录有 4.7 MB，实测在慢链路上取回需要 30 秒以上。若超时只作用于
// 建连/响应头，一次挂住的读取会永久占住后台刷新的 goroutine。
func TestHTTPFetcherEnforcesTimeout(t *testing.T) {
	// 先写一个字节再按住不放：响应头已发出（Do 已返回），读取体时卡住。
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"p":`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	start := time.Now()
	_, err := HTTPFetcher(server.Client())(server.URL, "", 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("读取体超时应当报错")
	}
	if elapsed > 5*time.Second {
		t.Errorf("耗时 %v，超时未被及时施加", elapsed)
	}
}

// TestHTTPFetcherHonoursContextlessTimeout 断言 timeout 为 0 时不施加截止时间。
func TestHTTPFetcherHonoursZeroTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"p":{"models":{}}}`))
	}))
	defer server.Close()

	result, err := HTTPFetcher(server.Client())(server.URL, "", 0)
	if err != nil {
		t.Fatalf("取回失败: %v", err)
	}
	if len(result.Body) == 0 {
		t.Error("应当读到响应体")
	}
}
