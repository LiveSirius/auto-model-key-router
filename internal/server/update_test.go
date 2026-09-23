package server

import (
	"io/fs"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/selfupdate"
)

// 本文件覆盖自更新入口的路由接线与鉴权。
//
// 它测的是**接缝**而不是自更新本身：真正换二进制的那套逻辑在 internal/selfupdate 里
// 单独测（那里用假 release 服务，完全离线）。

// newSelfUpdateApp 装配一个挂载了 WebUI 且注入了自更新接缝的 App。
//
// update 为 nil 表示"这个构建不带自更新能力"（生产里只有显式装配才带）。
func newSelfUpdateApp(t *testing.T, update func(string) (selfupdate.Result, error)) *App {
	t.Helper()
	return newTestAppWith(t, t.TempDir(), appFixture{
		webUIEnabled: true,
		mutate: func(options *Options) {
			// /ui 只有在 index.html 存在时才挂载（webui.Available）。
			options.WebUIAssets = fs.FS(fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte("<html>amkr</html>")},
			})
			options.SelfUpdate = update
		},
	})
}

// TestUpdateStatusReportsCapability 断言 /ui/update/status 如实回答能力与当前版本。
func TestUpdateStatusReportsCapability(t *testing.T) {
	app := newSelfUpdateApp(t, func(string) (selfupdate.Result, error) {
		return selfupdate.Result{}, nil
	})
	recorder := serve(app, http.MethodGet, "/ui/update/status", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /ui/update/status 状态码 = %d，期望 200（body=%s）",
			recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"available":true`) {
		t.Errorf("应报告 available=true，实际 %s", body)
	}
	if !strings.Contains(body, testVersion) {
		t.Errorf("应带上当前版本 %s，实际 %s", testVersion, body)
	}
}

// TestUpdateStatusWithoutSeamReportsUnavailable 断言没装接缝时报 available=false
// （旧构建/测试夹具正是这种状态）。
func TestUpdateStatusWithoutSeamReportsUnavailable(t *testing.T) {
	app := newSelfUpdateApp(t, nil)
	recorder := serve(app, http.MethodGet, "/ui/update/status", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"available":false`) {
		t.Errorf("应报告 available=false，实际 %s", recorder.Body.String())
	}
}

// TestUpdateStatusDoesNotRequireAuth 断言状态查询不鉴权。
//
// 理由：WebUI 要在用户填 key **之前**就正确决定是否显示「立即更新」按钮；而这个响应
// 只回答"这个构建有没有自更新能力"，没有任何敏感信息。
func TestUpdateStatusDoesNotRequireAuth(t *testing.T) {
	app := newSelfUpdateApp(t, func(string) (selfupdate.Result, error) {
		return selfupdate.Result{}, nil
	})
	recorder := serve(app, http.MethodGet, "/ui/update/status", "")
	if recorder.Code == http.StatusUnauthorized {
		t.Error("状态查询不该要求凭据")
	}
}

// TestUpdateApplyRequiresFullAuth 是安全边界：替换可执行文件必须要求**完整**权限，
// 访客 key 一律拒绝。
func TestUpdateApplyRequiresFullAuth(t *testing.T) {
	called := false
	app := newSelfUpdateApp(t, func(string) (selfupdate.Result, error) {
		called = true
		return selfupdate.Result{}, nil
	})

	anonymous := serve(app, http.MethodPost, "/ui/update/apply", "")
	if anonymous.Code != http.StatusUnauthorized {
		t.Errorf("匿名请求状态码 = %d，期望 401（body=%s）", anonymous.Code, anonymous.Body.String())
	}
	other := serve(app, http.MethodPost, "/ui/update/apply", otherAuthorization)
	if other.Code != http.StatusUnauthorized {
		t.Errorf("非完整权限凭据状态码 = %d，期望 401", other.Code)
	}
	if called {
		t.Error("未通过鉴权时绝不能触发更新")
	}
}

// TestUpdateApplyRunsSeamAndReportsResult 断言完整权限下真的调用接缝并回显结果。
func TestUpdateApplyRunsSeamAndReportsResult(t *testing.T) {
	var gotExecutable string
	app := newSelfUpdateApp(t, func(executable string) (selfupdate.Result, error) {
		gotExecutable = executable
		return selfupdate.Result{
			Updated:        true,
			CurrentVersion: "5.1.0",
			LatestVersion:  "5.1.1",
			RestartPending: true,
			Message:        "已更新。",
		}, nil
	})

	recorder := serve(app, http.MethodPost, "/ui/update/apply", fullAuthorization)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"updated":true`, `"latest_version":"5.1.1"`, `"restart_pending":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("响应应含 %s，实际 %s", want, body)
		}
	}
	// 接缝收到的应当是本进程的可执行文件路径（不是空串）。
	if gotExecutable == "" {
		t.Error("接缝应收到可执行文件路径")
	}
	if _, err := os.Stat(gotExecutable); err != nil {
		t.Errorf("传入的路径应当是真实存在的文件: %v", err)
	}
}

// TestUpdateApplyWithoutSeamReturns501 断言没装接缝时**响亮失败**，
// 而不是假装成功——那会让用户以为已经升级。
func TestUpdateApplyWithoutSeamReturns501(t *testing.T) {
	app := newSelfUpdateApp(t, nil)
	recorder := serve(app, http.MethodPost, "/ui/update/apply", fullAuthorization)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "自更新未接入") {
		t.Errorf("应点明未接入，实际 %s", recorder.Body.String())
	}
}

// TestUpdateApplyPropagatesSeamError 断言更新失败如实回 500 与原因。
func TestUpdateApplyPropagatesSeamError(t *testing.T) {
	app := newSelfUpdateApp(t, func(string) (selfupdate.Result, error) {
		return selfupdate.Result{}, os.ErrPermission
	})
	recorder := serve(app, http.MethodPost, "/ui/update/apply", fullAuthorization)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（body=%s）", recorder.Code, recorder.Body.String())
	}
}

// TestUpdateApplyRequestsShutdownOnlyWhenRestartPending 锁定关停条件。
//
// 只有「确实换了文件」且「助手已起来接管重启」时才关停自己：其余情况关停只会白白中断
// 用户正在用的界面。
func TestUpdateApplyRequestsShutdownOnlyWhenRestartPending(t *testing.T) {
	cases := []struct {
		name    string
		result  selfupdate.Result
		stopped bool
	}{
		{"已是最新", selfupdate.Result{Updated: false, RestartPending: false}, false},
		{"换了文件但助手没起来", selfupdate.Result{Updated: true, RestartPending: false}, false},
		{"换了文件且助手已接管", selfupdate.Result{Updated: true, RestartPending: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shutdown := make(chan struct{}, 1)
			app := newTestAppWith(t, t.TempDir(), appFixture{
				webUIEnabled: true,
				mutate: func(options *Options) {
					options.WebUIAssets = fs.FS(fstest.MapFS{
						"index.html": &fstest.MapFile{Data: []byte("<html>amkr</html>")},
					})
					options.SelfUpdate = func(string) (selfupdate.Result, error) {
						return tc.result, nil
					}
					options.RequestShutdown = func() { shutdown <- struct{}{} }
				},
			})

			recorder := serve(app, http.MethodPost, "/ui/update/apply", fullAuthorization)
			if recorder.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200", recorder.Code)
			}
			// 关停被放进 goroutine（必须在响应写出之后），因此这里要**真的等**一小会儿。
			select {
			case <-shutdown:
				if !tc.stopped {
					t.Error("不该请求关停")
				}
			case <-time.After(500 * time.Millisecond):
				if tc.stopped {
					t.Error("应当请求关停")
				}
			}
		})
	}
}

// TestUpdateRoutesAreMountedUnderUIPrefix 锁定两条路由都挂在 /ui/ 下，
// 且不占用被语料锁定的 /api 前缀（理由见 update.go）。
func TestUpdateRoutesAreMountedUnderUIPrefix(t *testing.T) {
	app := newSelfUpdateApp(t, func(string) (selfupdate.Result, error) {
		return selfupdate.Result{}, nil
	})
	for _, path := range []string{"/ui/update/status", "/ui/update/apply"} {
		recorder := serve(app, http.MethodGet, path, "")
		if recorder.Code == http.StatusNotFound && recorder.Body.String() == notFoundBody {
			t.Errorf("%s 被兜底 404 吞掉了", path)
		}
	}
	// 带挂载前缀时同样可达（嵌入宿主的情形）。
	prefixed := newTestAppWith(t, t.TempDir(), appFixture{
		webUIEnabled: true,
		mutate: func(options *Options) {
			options.WebUIAssets = fs.FS(fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte("<html>amkr</html>")},
			})
			options.MountPrefix = "/amkr"
			options.SelfUpdate = func(string) (selfupdate.Result, error) {
				return selfupdate.Result{}, nil
			}
		},
	})
	recorder := serve(prefixed, http.MethodGet, "/amkr/ui/update/status", "")
	if recorder.Code != http.StatusOK {
		t.Errorf("带前缀的 GET /amkr/ui/update/status 状态码 = %d，期望 200", recorder.Code)
	}
}

// TestUpdateApplyRejectsGet 锁定方法约束。
func TestUpdateApplyRejectsGet(t *testing.T) {
	app := newSelfUpdateApp(t, func(string) (selfupdate.Result, error) {
		return selfupdate.Result{}, nil
	})
	recorder := serve(app, http.MethodGet, "/ui/update/apply", fullAuthorization)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /ui/update/apply 状态码 = %d，期望 405", recorder.Code)
	}
}
