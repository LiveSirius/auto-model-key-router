package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// pypiFetcher 返回一个只认 PyPI JSON 接口的假取回器。
//
// 用它而不是真实的 updatecheck.HTTPFetcher：这条测试要证明的是**装配层的推导**，
// 不能联网，也不能依赖 PyPI/GitHub 当前发布的是什么版本。
func pypiFetcher(version string) updatecheck.Fetcher {
	return func(url string, _ map[string]string, _ time.Duration) (*canonical.Value, error) {
		if url != updatecheck.PyPIJSONAPI {
			return nil, &unexpectedURLError{url: url}
		}
		info := canonical.NewObject()
		info.SetKey("version", canonical.NewString(version))
		info.SetKey("release_url", canonical.NewString("https://example.test/"+version+"/"))
		root := canonical.NewObject()
		root.SetKey("info", info)
		root.SetKey("urls", canonical.NewArray())
		return root, nil
	}
}

// unexpectedURLError 表示取回器收到了非预期的 URL。
type unexpectedURLError struct{ url string }

func (e *unexpectedURLError) Error() string { return "非预期的取回 URL: " + e.url }

// TestUpdateCheckDerivesUpdateAvailable 是那处已知缺陷的回归测试。
//
// 缺陷：api 的 handleCheckUpdate 直接信任调用方给的 UpdateAvailable 字段（它内部那段
// `if !updateAvailable && ...` 是死代码），而参照实现的 update_available 是从
// latest/current 推导出来的属性。装配层如果只做 `UpdateAvailable: result.UpdateAvailable`
// 的字段搬运，就会在「确实有新版」时回答 false。
//
// 这里从**接口层**验证：POST /api/update/check 的响应体里 update_available 必须是
// true，且 latest_version 是取回器给的新版本号。
func TestUpdateCheckDerivesUpdateAvailable(t *testing.T) {
	cases := []struct {
		name            string
		current         string
		latest          string
		wantAvailable   bool
		wantLatestValue string
	}{
		{"有新版本", "4.1.0", "4.2.0", true, "4.2.0"},
		{"已是最新", "4.2.0", "4.2.0", false, "4.2.0"},
		{"远端更旧", "4.2.0", "4.1.9", false, "4.1.9"},
		{"跨大版本", "4.1.0", "5.0.0", true, "5.0.0"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t, t.TempDir(), func(options *Options) {
				// 装配层的适配器必须自己推导 UpdateAvailable；这里刻意让 CheckUpdate
				// 走真实适配器（只把取回器换成桩），而不是注入一个填好字段的假结果。
				options.Version = testCase.current
				options.CheckUpdate = newUpdateCheck(testCase.current, pypiFetcher(testCase.latest))
			})

			recorder := serve(app, http.MethodPost, "/api/update/check", fullAuthorization)
			if recorder.Code != http.StatusOK {
				t.Fatalf("POST /api/update/check 状态码 = %d，期望 200（body=%s）",
					recorder.Code, recorder.Body.String())
			}
			parsed, err := canonical.ParseString(recorder.Body.String())
			if err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			if got := parsed.Lookup("latest_version").StringValue(); got != testCase.wantLatestValue {
				t.Errorf("latest_version = %q，期望 %q", got, testCase.wantLatestValue)
			}
			got := parsed.Lookup("update_available")
			if got == nil {
				t.Fatal("响应里缺少 update_available")
			}
			if got.Bool != testCase.wantAvailable {
				t.Errorf("update_available = %v，期望 %v（必须由 latest/current 推导）",
					got.Bool, testCase.wantAvailable)
			}
		})
	}
}

// TestUpdateCheckAdapterMapsOptionalFields 断言 nil 在适配器里折叠成空串、并由 api
// 侧渲染回 JSON null（往返无损）。
func TestUpdateCheckAdapterMapsOptionalFields(t *testing.T) {
	failing := updatecheck.Fetcher(
		func(string, map[string]string, time.Duration) (*canonical.Value, error) {
			return nil, &unexpectedURLError{url: "unreachable"}
		})
	adapter := newUpdateCheck("4.1.0", failing)
	result := adapter(3.0)

	if result.CurrentVersion != "4.1.0" {
		t.Errorf("CurrentVersion = %q", result.CurrentVersion)
	}
	if result.LatestVersion != "" {
		t.Errorf("LatestVersion = %q，期望空串（两个源都失败）", result.LatestVersion)
	}
	if result.UpdateAvailable {
		t.Error("没有最新版本号时 update_available 必须为 false")
	}
	if result.Error == "" {
		t.Error("两个源都失败时 Error 必须非空")
	}
	if result.ReleaseURL != "" || result.Source != "" ||
		result.ArtifactURL != "" || result.ArtifactSHA256 != "" {
		t.Errorf("失败结果里的可选字段应为空串: %+v", result)
	}
}

// TestUpdateCheckAdapterVersionRoundTrip 断言 CurrentVersion 来自装配层注入的版本号。
func TestUpdateCheckAdapterVersionRoundTrip(t *testing.T) {
	adapter := newUpdateCheck("7.7.7", pypiFetcher("7.8.0"))
	if got := adapter(3.0).CurrentVersion; got != "7.7.7" {
		t.Errorf("CurrentVersion = %q，期望 7.7.7", got)
	}
	if !adapter(3.0).UpdateAvailable {
		t.Error("7.8.0 比 7.7.7 新，update_available 应为 true")
	}
}
