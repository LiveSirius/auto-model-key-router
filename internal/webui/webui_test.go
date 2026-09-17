package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// assetsWithIndex 构造一个最小资产树：根有 index.html，另有 js/ 与 docs/。
//
// docs/ 刻意做成**没有** index.html 的目录，用来验证「不列目录」。
func assetsWithIndex() fstest.MapFS {
	return fstest.MapFS{
		"index.html":     {Data: []byte("<html>amkr</html>")},
		"styles.css":     {Data: []byte("body{}")},
		"js/app.js":      {Data: []byte("console.log(1)")},
		"docs/readme.md": {Data: []byte("hi")},
	}
}

// assetsWithoutIndex 是没有 index.html 的资产树。
func assetsWithoutIndex() fstest.MapFS {
	return fstest.MapFS{"styles.css": {Data: []byte("body{}")}}
}

// TestMountPathMatchesPython 锁定挂载常量。
func TestMountPathMatchesPython(t *testing.T) {
	if MountPath != "/ui" {
		t.Errorf("MountPath = %q，期望 /ui", MountPath)
	}
	if IndexFile != "index.html" {
		t.Errorf("IndexFile = %q，期望 index.html", IndexFile)
	}
}

// TestAvailableMatchesPython 锁定可用性判据为「index.html 是否存在」。
func TestAvailableMatchesPython(t *testing.T) {
	if !Available(assetsWithIndex()) {
		t.Error("有 index.html 时应可用")
	}
	if Available(assetsWithoutIndex()) {
		t.Error("无 index.html 时应不可用")
	}
	if Available(nil) {
		t.Error("nil 资产应不可用")
	}
	// 目录不能当作 index.html。
	dirOnly := fstest.MapFS{"index.html/x": {Data: []byte("x")}}
	if Available(dirOnly) {
		t.Error("index.html 是目录时不应算可用")
	}
}

// TestPathMatchesPython 锁定挂载路径拼接（期望值来自真实 Python webui_path）。
//
// 实测：前缀 "" -> "/ui"；"/amkr" -> "/amkr/ui"；"/amkr/" -> "/amkr/ui"。
func TestPathMatchesPython(t *testing.T) {
	cases := map[string]string{
		"":       "/ui",
		"/":      "/ui",
		"/amkr":  "/amkr/ui",
		"/amkr/": "/amkr/ui",
		"//a//":  "//a/ui",
	}
	for prefix, want := range cases {
		if got := Path(prefix); got != want {
			t.Errorf("Path(%q) = %q，期望 %q", prefix, got, want)
		}
	}
}

// TestHandlerNotMountedWhenDisabledOrUnavailable 锁定「未启用/不可用时不注册路由」。
//
// 对应 webui.py:37-39：enabled 为假或资产缺失时返回 False，调用方因此不挂载任何
// 路由，对外完全不暴露 /ui。
func TestHandlerNotMountedWhenDisabledOrUnavailable(t *testing.T) {
	if handler, mounted := Handler(assetsWithIndex(), false); mounted || handler != nil {
		t.Error("enabled=false 时不应挂载")
	}
	if handler, mounted := Handler(assetsWithoutIndex(), true); mounted || handler != nil {
		t.Error("资产不可用（无 index.html）时不应挂载")
	}
	if handler, mounted := Handler(nil, true); mounted || handler != nil {
		t.Error("nil 资产时不应挂载")
	}
	handler, mounted := Handler(assetsWithIndex(), true)
	if !mounted || handler == nil {
		t.Fatal("启用且可用时应挂载")
	}
}

// serveRequest 直接调用处理器（调用方已用 StripPrefix 去掉挂载前缀）。
func serveRequest(t *testing.T, assets fs.FS, method, urlPath string) *httptest.ResponseRecorder {
	t.Helper()
	handler, mounted := Handler(assets, true)
	if !mounted {
		t.Fatal("应挂载")
	}
	request := httptest.NewRequest(method, urlPath, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestHandlerServesFilesMatchesStaticFiles 锁定文件发送行为。
func TestHandlerServesFilesMatchesStaticFiles(t *testing.T) {
	cases := []struct {
		name       string
		urlPath    string
		wantStatus int
		wantBody   string
	}{
		{"根路径落到 index.html", "/", http.StatusOK, "<html>amkr</html>"},
		{"普通文件", "/styles.css", http.StatusOK, "body{}"},
		{"子目录文件", "/js/app.js", http.StatusOK, "console.log(1)"},
		{"目录下有 index.html 时取其 index", "/js/", http.StatusNotFound, ""},
		{"文件不存在", "/nope.js", http.StatusNotFound, ""},
		{"空目录", "/docs/", http.StatusNotFound, ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := serveRequest(t, assetsWithIndex(), http.MethodGet, item.urlPath)
			if recorder.Code != item.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d（body=%q）", recorder.Code, item.wantStatus, recorder.Body.String())
			}
			if item.wantBody != "" && recorder.Body.String() != item.wantBody {
				t.Fatalf("响应体 = %q，期望 %q", recorder.Body.String(), item.wantBody)
			}
		})
	}
}

// TestHandlerNeverListsDirectories 锁定「不做目录列表」。
//
// 这是与 Go 默认 FileServer 的**有意差异**：Starlette 的 html=True 在目录无
// index.html 时返回 404，而 http.FileServer 会列出目录内容。列出资产目录不算漏洞，
// 但那是与参照实现不同的对外行为，故这里自己实现并钉住。
func TestHandlerNeverListsDirectories(t *testing.T) {
	recorder := serveRequest(t, assetsWithIndex(), http.MethodGet, "/docs/")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("无 index.html 的目录应 404，实际 %d", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, "readme.md") {
		t.Fatalf("不应列出目录内容，实际响应体 %q", body)
	}
}

// TestHandlerRejectsTraversal 锁定越界路径不会逃出资产根。
func TestHandlerRejectsTraversal(t *testing.T) {
	for _, urlPath := range []string{"/../secret", "/..", "/../../etc/passwd", "/js/../../secret"} {
		recorder := serveRequest(t, assetsWithIndex(), http.MethodGet, urlPath)
		if recorder.Code == http.StatusOK {
			t.Errorf("越界路径 %q 不应返回 200（body=%q）", urlPath, recorder.Body.String())
		}
	}
}

// TestHandlerRejectsWriteMethods 锁定只接受 GET/HEAD。
func TestHandlerRejectsWriteMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		recorder := serveRequest(t, assetsWithIndex(), method, "/")
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 应返回 405，实际 %d", method, recorder.Code)
		}
	}
	// HEAD 允许，且不应有响应体。
	recorder := serveRequest(t, assetsWithIndex(), http.MethodHead, "/")
	if recorder.Code != http.StatusOK {
		t.Errorf("HEAD 应返回 200，实际 %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("HEAD 不应有响应体，实际 %q", recorder.Body.String())
	}
}

// TestResolveNameMapsRootAndBlocksEscape 直接锁定路径解析。
//
// 用直接调用而非 HTTP 请求：httptest.NewRequest 不接受空 URL，而真实 http.Server
// 也永远不会给出空路径。
//
// 关于越界：path.Clean 对**绝对**路径的 ".." 会就地折叠（"/../secret" -> "/secret"），
// 因此结果始终落在资产根**之内**——安全属性是"逃不出去"，而不是"一律拒绝"。
// 真正无法映射到根内任何名字的只有 "/.." 这类（折叠后为空），返回空串由调用方 404。
func TestResolveNameMapsRootAndBlocksEscape(t *testing.T) {
	cases := map[string]string{
		"":                  IndexFile,
		"/":                 IndexFile,
		".":                 IndexFile,
		"/styles.css":       "styles.css",
		"/js/app.js":        "js/app.js",
		"/js/./app.js":      "js/app.js",
		"/js/../styles.css": "styles.css",
		// 折叠后仍在根内（安全），调用方会因文件不存在而 404。
		"/../secret":   "secret",
		"/../../etc/x": "etc/x",
		// 折叠后为空：无法映射到根内任何名字。
		"/..": "",
	}
	for input, want := range cases {
		got := resolveName(input)
		if got != want {
			t.Errorf("resolveName(%q) = %q，期望 %q", input, got, want)
		}
		// 兜底安全断言：无论输入如何，结果都不得以 .. 开头（即不许逃出资产根）。
		if strings.HasPrefix(got, "..") {
			t.Errorf("resolveName(%q) = %q，逃出了资产根", input, got)
		}
	}
}

// TestHandlerSetsContentType 验证按扩展名设置 Content-Type（ServeFileFS 的行为）。
func TestHandlerSetsContentType(t *testing.T) {
	cases := map[string]string{
		"/styles.css": "text/css; charset=utf-8",
		"/js/app.js":  "text/javascript; charset=utf-8",
		"/":           "text/html; charset=utf-8",
	}
	for urlPath, want := range cases {
		recorder := serveRequest(t, assetsWithIndex(), http.MethodGet, urlPath)
		if got := recorder.Header().Get("Content-Type"); got != want {
			t.Errorf("%s Content-Type = %q，期望 %q", urlPath, got, want)
		}
	}
}
