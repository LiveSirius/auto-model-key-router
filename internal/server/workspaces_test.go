package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// 本文件固化 /ui/workspaces.json 的语义。
//
// 它是 Go 侧新增的读数（参照实现没有工作空间），因此没有对拍语料；形状由本项目
// 自己定，这里用测试钉住。

// workspacesFixture 是一份带两个命名工作空间的配置。
//
// 顶层 tasks 有 2 个任务（默认工作空间），teamA 有 1 个，teamB 是空分组——夹具里
// 手工写出来，用来验证「空分组不该出现在清单里」。
const workspacesFixture = `{
  "config_version": 4,
  "local_api_key": "local-key",
  "ops_enabled": true,
  "webui_enabled": true,
  "host": "127.0.0.1",
  "port": 8765,
  "endpoint_capabilities_path": "<CAP>",
  "metrics_db_path": "<DB>",
  "log_file_path": "<LOG>",
  "providers": {
    "prov-a": {"base_url": "https://a.example.test", "keys": {"key-a": {"api_key": "sk-a"}}}
  },
  "models": {
    "model-a": {"targets": [{"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}]},
    "model-b": {"targets": [{"provider": "prov-a", "key": "key-a", "upstream_model": "model-b"}]}
  },
  "tasks": {
    "task-default-1": {"model": "model-a"},
    "task-default-2": {"model": "model-b"}
  },
  "workspaces": {
    "teamA": {"tasks": {"team-task": {"model": "model-a"}}},
    "teamB": {"tasks": {}}
  }
}`

// newWorkspacesApp 装配一个带工作空间配置、且 WebUI 已挂载的 App。
func newWorkspacesApp(t *testing.T, prefix string) *App {
	t.Helper()
	return newTestAppWith(t, t.TempDir(), appFixture{
		opsEnabled:   true,
		webUIEnabled: true,
		configText:   workspacesFixture,
		mutate: func(options *Options) {
			options.MountPrefix = prefix
			// /ui 只有在 index.html 存在时才挂载（webui.Available）。
			options.WebUIAssets = fs.FS(fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte("<html>amkr</html>")},
			})
		},
	})
}

// workspaceCatalogResponse 是 /ui/workspaces.json 的响应形状。
type workspaceCatalogResponse struct {
	Workspaces []struct {
		Name      string `json:"name"`
		TaskCount int    `json:"task_count"`
	} `json:"workspaces"`
}

// getWorkspaces 发一条 GET 并解析响应。
func getWorkspaces(t *testing.T, app *App, path, authorization string) (*workspaceCatalogResponse, string) {
	t.Helper()
	request := newRequestWithAuth(http.MethodGet, path, authorization)
	recorder := serveRequest(app, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d，期望 200（body=%s）", path, recorder.Code, recorder.Body.String())
	}
	var payload workspaceCatalogResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应失败: %v（body=%s）", err, recorder.Body.String())
	}
	return &payload, recorder.Body.String()
}

// TestWorkspacesEndpointListsCatalog 固化 GET /ui/workspaces.json 的内容与形状。
func TestWorkspacesEndpointListsCatalog(t *testing.T) {
	app := newWorkspacesApp(t, "")
	payload, raw := getWorkspaces(t, app, "/ui/workspaces.json", fullAuthorization)

	if len(payload.Workspaces) != 2 {
		t.Fatalf("工作空间数量 = %d，期望 2（默认空间 + teamA）：%s", len(payload.Workspaces), raw)
	}
	// 默认空间固定排第一：前端直接拿它当下拉的默认选中项。
	if payload.Workspaces[0].Name != "default" || payload.Workspaces[0].TaskCount != 2 {
		t.Errorf("首项应为 default/2，实际 %+v", payload.Workspaces[0])
	}
	if payload.Workspaces[1].Name != "teamA" || payload.Workspaces[1].TaskCount != 1 {
		t.Errorf("次项应为 teamA/1，实际 %+v", payload.Workspaces[1])
	}
	// teamB 是空分组，不该出现在清单里——它既不可观测也没有意义。
	if strings.Contains(raw, "teamB") {
		t.Errorf("空工作空间不应出现在清单里: %s", raw)
	}
	// 形状由本项目定义：字段名与顺序都要稳定（WebUI 直接按 name 渲染下拉项）。
	if !strings.HasPrefix(raw, `{"workspaces":[{"name":"default","task_count":2}`) {
		t.Errorf("响应形状与字段顺序不符: %s", raw)
	}
}

// TestWorkspacesEndpointRequiresAuth 固化：这个读数不能匿名读。
//
// 与 pricing.json 不同：价格目录是 models.dev 的公开数据，而工作空间清单会暴露
// 配置结构（有哪些分组、各有多少任务）。
func TestWorkspacesEndpointRequiresAuth(t *testing.T) {
	app := newWorkspacesApp(t, "")

	for _, authorization := range []string{"", visitorAuthorization, "Bearer wrong-key"} {
		recorder := serveRequest(app, newRequestWithAuth(http.MethodGet, "/ui/workspaces.json", authorization))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("凭据 %q 的状态码 = %d，期望 401", authorization, recorder.Code)
		}
	}
}

// TestWorkspacesEndpointRejectsNonGet 固化只接受 GET。
func TestWorkspacesEndpointRejectsNonGet(t *testing.T) {
	app := newWorkspacesApp(t, "")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		recorder := serveRequest(app, newRequestWithAuth(method, "/ui/workspaces.json", fullAuthorization))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 状态码 = %d，期望 405", method, recorder.Code)
		}
		if got := recorder.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("%s 的 Allow = %q，期望 GET", method, got)
		}
	}
}

// TestWorkspacesEndpointAbsentWithoutWebUI 固化它与 WebUI 同生共死。
//
// 挂在 /ui/ 之下是本项目刻意的取舍（见 workspaces.go）：这条测试把它钉住，
// 免得日后有人误以为它是一条独立路由。
func TestWorkspacesEndpointAbsentWithoutWebUI(t *testing.T) {
	app := newTestApp(t, t.TempDir(), nil)

	recorder := serveRequest(app, newRequestWithAuth(http.MethodGet, "/ui/workspaces.json", fullAuthorization))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("未挂载 WebUI 时状态码 = %d，期望 404", recorder.Code)
	}
	if recorder.Body.String() != notFoundBody {
		t.Errorf("响应体 = %s，期望兜底 404 %s", recorder.Body.String(), notFoundBody)
	}
}

// TestWorkspacesEndpointFollowsMountPrefix 固化嵌入宿主时的前缀跟随。
func TestWorkspacesEndpointFollowsMountPrefix(t *testing.T) {
	app := newWorkspacesApp(t, "/amkr")

	if _, raw := getWorkspaces(t, app, "/amkr/ui/workspaces.json", fullAuthorization); raw == "" {
		t.Error("带前缀时应返回清单")
	}
	plain := serveRequest(app, newRequestWithAuth(http.MethodGet, "/ui/workspaces.json", fullAuthorization))
	if plain.Code != http.StatusNotFound {
		t.Errorf("无前缀时状态码 = %d，期望 404", plain.Code)
	}
}

// TestWorkspacesEndpointAlwaysListsDefault 固化：没有命名空间时仍列出默认空间。
//
// 默认空间是调用方不带 X-AMKR-Workspace 头时命中的那个，界面上必须可见、可选，
// 哪怕它一个任务都没有。
func TestWorkspacesEndpointAlwaysListsDefault(t *testing.T) {
	app := newTestAppWith(t, t.TempDir(), appFixture{
		opsEnabled:   true,
		webUIEnabled: true,
		mutate: func(options *Options) {
			options.WebUIAssets = fs.FS(fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte("<html>amkr</html>")},
			})
		},
	})

	payload, raw := getWorkspaces(t, app, "/ui/workspaces.json", fullAuthorization)
	if len(payload.Workspaces) != 1 || payload.Workspaces[0].Name != "default" {
		t.Errorf("无命名空间时应只列出 default，实际 %s", raw)
	}
}

// —— 测试辅助 ——

// newRequestWithAuth 构造一条带 Authorization 的请求（空串表示不带）。
func newRequestWithAuth(method, path, authorization string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	return request
}

// serveRequest 用 App 的处理器回放一条请求。
func serveRequest(app *App, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}
