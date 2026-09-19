package amkr

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/webui"
)

// TestEmbeddedProbesAreNotServed 是本模块的安全回归：开发/CI 探针虽然被编进二进制，
// 但**不得**随 /ui/ 对外可取。
//
// 为什么必须用**真实嵌入资产**而不是 fstest 假树：internal/webui 的用例只能证明
// resolveName 的判定逻辑，证明不了「webui/ 下真的有一个 probes/ 目录、并且真的被
// 挡住了」。前者在没有探针时同样通过（空断言），后者才是这次修复要锁住的事实。
//
// 三条断言缺一不可：
//
//  1. 嵌入的资产里**确实存在** probes/ —— 否则 2、3 是因为「本来就没有」而通过，
//     测试会随着资产被移走而悄悄失去意义；
//  2. probes 下的 .mjs 取出 404；
//  3. 普通资产仍然 200 —— 否则把整个 /ui/ 关掉也能让 2 通过。
func TestEmbeddedProbesAreNotServed(t *testing.T) {
	probeAssets := collectProbeAssets(t)
	if len(probeAssets) == 0 {
		t.Fatal("嵌入资产里没有 probes/ 下的文件：本用例已失去意义，" +
			"若探针目录真的被移走了，应连同本用例一起删除")
	}

	handler, mounted := webui.Handler(WebUIAssets, true)
	if !mounted {
		t.Fatal("/ui 未挂载（index.html 不在嵌入资产里？）")
	}

	for _, name := range probeAssets {
		recorder := serveAsset(t, handler, "/"+name)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("探针资产 %s 应 404，实际 %d（body=%q）",
				name, recorder.Code, recorder.Body.String())
		}
	}

	// 反向断言：同一次请求下普通资产仍然可取。少了这一条，把处理器改成「一律 404」
	// 也能让上面的循环通过。
	recorder := serveAsset(t, handler, "/main.js")
	if recorder.Code != http.StatusOK {
		t.Fatalf("普通资产 /main.js 应 200，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "boot") {
		t.Errorf("/main.js 内容异常: %q", recorder.Body.String())
	}
}

// TestEmbeddedIndexIsAvailable 锁定可用性判据仍然成立（探针目录的存在不影响它）。
func TestEmbeddedIndexIsAvailable(t *testing.T) {
	if !webui.Available(WebUIAssets) {
		t.Fatal("嵌入资产应可用（index.html 存在）")
	}
}

// collectProbeAssets 列出嵌入资产里 probes/ 下的全部文件（相对资产根的斜杠路径）。
func collectProbeAssets(t *testing.T) []string {
	t.Helper()
	var names []string
	err := fs.WalkDir(WebUIAssets, "probes", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		// probes 目录不存在时 WalkDir 返回错误：这正是「探针被移走」的信号，
		// 由上层的空列表断言报出来，这里不当作失败。
		return nil
	}
	return names
}

// serveAsset 直接调用处理器（调用方已用 StripPrefix 去掉挂载前缀，见 routes.go）。
func serveAsset(t *testing.T, handler http.Handler, urlPath string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, urlPath, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
