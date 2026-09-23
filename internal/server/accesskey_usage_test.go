package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/metrics"
)

// 本文件固化访客看板的读数端点 /ui/access-key-usage.json。
//
// 访客 = 访问密钥的持有者。与 workspace_panel_test.go 的分工：那条按**工作空间**
// 收窄（嵌入方面板），这条按**访问密钥**收窄。两条都是「一条路由一个范围」，因此
// 这里最关键的断言与那边同类：不能看到别人的数字。

// guestDashboardFixture 是一份带两把访问密钥的最小配置。
//
// 两把 key 刻意**都不带工作空间**，且被授了不同的清单：这正是访问密钥的常态，也是
// 「按工作空间收窄」替代不了「按 key 收窄」的原因——它们的流量在 request_workspace
// 里会落进同一个 default。
const guestDashboardFixture = `{
  "config_version": 4,
  "local_api_key": "local-key",
  "ops_enabled": true,
  "webui_enabled": true,
  "host": "127.0.0.1",
  "port": 8000,
  "endpoint_capabilities_path": "<CAP>",
  "metrics_db_path": "<DB>",
  "log_file_path": "<LOG>",
  "providers": {
    "prov-a": {"base_url": "https://a.example.test", "keys": {"key-a": {"api_key": "sk-a"}}},
    "prov-b": {"base_url": "https://b.example.test", "keys": {"key-b": {"api_key": "sk-b"}}}
  },
  "models": {
    "model-a": {"targets": [{"provider": "prov-a", "key": "key-a", "upstream_model": "model-a"}]},
    "model-b": {"targets": [{"provider": "prov-b", "key": "key-b", "upstream_model": "model-b"}]}
  },
  "access_keys": {
    "trial": {"name": "试用", "key": "amkr_ak_trial", "enabled": true, "providers": ["prov-a"], "models": ["model-a"]},
    "other": {"name": "另一位", "key": "amkr_ak_other", "enabled": true},
    "off":   {"name": "已停用", "key": "amkr_ak_off", "enabled": false}
  }
}`

// newAccessKeyUsageApp 装配一个 /ui 真的挂载、且带访问密钥的 App。
func newAccessKeyUsageApp(t *testing.T) *App {
	t.Helper()
	return newTestAppWith(t, t.TempDir(), appFixture{
		configText: guestDashboardFixture,
		mutate: func(options *Options) {
			options.WebUIAssets = fs.FS(fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte("<html>amkr</html>")},
			})
		},
	})
}

// seedAccessKeyUsage 往指标库写几行归属不同的记录。
//
// 每把 key 各写各的 AccessKeyID，且**共用同一个工作空间**（访问密钥不带工作空间头，
// 归一化后都是 default）——若归属写错成按 workspace 收窄，下面的断言会立刻发现两把
// key 看到同一个数字。
func seedAccessKeyUsage(t *testing.T, app *App) {
	t.Helper()
	adapter := app.currentMetricsAdapter()
	if adapter == nil || adapter.store == nil {
		t.Fatal("指标不可用，无法准备测试数据")
	}
	provA, provB := "prov-a", "prov-b"
	record := func(accessKeyID, modelID string, provider *string, prompt int64) {
		t.Helper()
		if err := adapter.store.Record(metrics.RecordParams{
			ModelID:     modelID,
			KeyName:     "key-a",
			Usage:       canonical.NewObjectOf(canonical.ObjectPair{Key: "prompt_tokens", Value: canonical.NewIntValue(prompt)}),
			CallerType:  "access_key",
			ProviderID:  provider,
			Workspace:   "default",
			AccessKeyID: accessKeyID,
		}); err != nil {
			t.Fatalf("写入指标失败: %v", err)
		}
	}
	record("trial", "model-a", &provA, 100)
	record("trial", "model-a", &provA, 50)
	record("other", "model-b", &provB, 30)
}

// TestAccessKeyUsageIsScopedToItsKey 是访客看板的**核心断言**：只看到自己那把 key。
func TestAccessKeyUsageIsScopedToItsKey(t *testing.T) {
	app := newAccessKeyUsageApp(t)
	seedAccessKeyUsage(t, app)

	recorder := serve(app, http.MethodGet, "/ui/access-key-usage.json", "Bearer amkr_ak_trial")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	var body struct {
		AccessKeyID   string `json:"access_key_id"`
		AccessKeyName string `json:"access_key_name"`
		Stats         struct {
			Requests    int64 `json:"requests"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"stats"`
		Dimensions struct {
			ModelID    map[string]struct{ Requests int64 } `json:"model_id"`
			ProviderID map[string]struct{ Requests int64 } `json:"provider_id"`
		} `json:"dimensions"`
		RecentRequests []struct {
			ModelID string `json:"model_id"`
		} `json:"recent_requests"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v（body=%s）", err, recorder.Body.String())
	}

	if body.AccessKeyID != "trial" {
		t.Errorf("access_key_id = %q，期望 trial", body.AccessKeyID)
	}
	// 名字来自配置，供看板做标题；只给名字，**绝不给明文 key**。
	if body.AccessKeyName != "试用" {
		t.Errorf("access_key_name = %q，期望 试用", body.AccessKeyName)
	}
	if body.Stats.Requests != 2 {
		t.Errorf("trial 请求数 = %d，期望 2（不得把 other 的算进来）", body.Stats.Requests)
	}
	if body.Stats.TotalTokens != 150 {
		t.Errorf("trial total_tokens = %d，期望 150", body.Stats.TotalTokens)
	}
	// 关键断言：拆分里只有自己的模型与供应商。
	if len(body.Dimensions.ModelID) != 1 {
		t.Fatalf("模型拆分 = %+v，期望只有 model-a", body.Dimensions.ModelID)
	}
	if _, ok := body.Dimensions.ModelID["model-a"]; !ok {
		t.Errorf("模型拆分缺少 model-a: %+v", body.Dimensions.ModelID)
	}
	if _, leaked := body.Dimensions.ModelID["model-b"]; leaked {
		t.Errorf("模型拆分泄漏了别的 key 的模型: %+v", body.Dimensions.ModelID)
	}
	if _, leaked := body.Dimensions.ProviderID["prov-b"]; leaked {
		t.Errorf("供应商拆分泄漏了别的 key 的供应商: %+v", body.Dimensions.ProviderID)
	}
	// 最近明细同样只含自己的两条，且不含别的模型。
	if len(body.RecentRequests) != 2 {
		t.Errorf("最近明细 = %d 条，期望 2", len(body.RecentRequests))
	}
	for _, item := range body.RecentRequests {
		if item.ModelID != "model-a" {
			t.Errorf("最近明细泄漏了别的 key 的记录: %+v", item)
		}
	}

	// 反向确认：另一把 key 只看到自己的那一条。
	other := serve(app, http.MethodGet, "/ui/access-key-usage.json", "Bearer amkr_ak_other")
	if other.Code != http.StatusOK {
		t.Fatalf("other 状态码 = %d，期望 200", other.Code)
	}
	var otherBody struct {
		AccessKeyID string `json:"access_key_id"`
		Stats       struct {
			Requests int64 `json:"requests"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(other.Body.Bytes(), &otherBody); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if otherBody.AccessKeyID != "other" || otherBody.Stats.Requests != 1 {
		t.Errorf("other 的读数 = %+v，期望 id=other requests=1", otherBody)
	}
}

// TestAccessKeyUsageNeverLeaksPlaintext 固化响应里不出现明文密钥。
//
// 明文只出现在创建与轮换的响应里（见 internal/api/handlers_accesskeys.go）。看板是
// 一个可能被投屏、被截图、被随手分享的页面，回显明文等于把凭据散播出去。
func TestAccessKeyUsageNeverLeaksPlaintext(t *testing.T) {
	app := newAccessKeyUsageApp(t)
	seedAccessKeyUsage(t, app)

	for _, key := range []string{"amkr_ak_trial", "amkr_ak_other", "amkr_ak_off"} {
		recorder := serve(app, http.MethodGet, "/ui/access-key-usage.json", "Bearer "+key)
		if recorder.Code == http.StatusOK && strings.Contains(recorder.Body.String(), key) {
			t.Errorf("响应里回显了明文 key %s: %s", key, recorder.Body.String())
		}
	}
}

// TestAccessKeyUsageRejectsOtherCredentials 固化：另外三种凭据都被拒。
//
// 完整权限走 /metrics（能看全部流量）；工作空间面板 key 与推理 key 各有自己的读数。
// 让它们在这条路由上拿到一份「全是 0」的合法响应会让人以为自己的流量丢了。
func TestAccessKeyUsageRejectsOtherCredentials(t *testing.T) {
	app := newAccessKeyUsageApp(t)

	for _, authorization := range []string{"", fullAuthorization, otherAuthorization, "Bearer nope"} {
		recorder := serve(app, http.MethodGet, "/ui/access-key-usage.json", authorization)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("凭据 %q 状态码 = %d，期望 401（body=%s）",
				authorization, recorder.Code, recorder.Body.String())
		}
	}
}

// TestAccessKeyUsageDisabledKeyIsForbidden 固化：停用的 key 回 403 而不是 401。
//
// 停用是可恢复的已知身份（去管理面把开关打开即可），错误凭据不是——混成 401 会让
// 持有者以为 key 打错了而重新索要一把。
func TestAccessKeyUsageDisabledKeyIsForbidden(t *testing.T) {
	app := newAccessKeyUsageApp(t)

	recorder := serve(app, http.MethodGet, "/ui/access-key-usage.json", "Bearer amkr_ak_off")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403（body=%s）", recorder.Code, recorder.Body.String())
	}
}

// TestAccessKeyUsageEmptyKeyStillAnswers 固化：没有流量时照样 200 + 零值。
//
// 刚创建的访问密钥就是这个状态（有 key、还没人用），回 404 会让持有者以为接口坏了；
// 回一份零值读数是「还没有数据」，语义正确。
func TestAccessKeyUsageEmptyKeyStillAnswers(t *testing.T) {
	// 刻意**不写任何指标**：这才是「刚创建」的真实状态。
	app := newAccessKeyUsageApp(t)

	recorder := serve(app, http.MethodGet, "/ui/access-key-usage.json", "Bearer amkr_ak_trial")
	if recorder.Code != http.StatusOK {
		t.Fatalf("无流量状态码 = %d，期望 200（body=%s）", recorder.Code, recorder.Body.String())
	}
	var body struct {
		AccessKeyName string `json:"access_key_name"`
		Stats         struct {
			Requests    int64 `json:"requests"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"stats"`
		Dimensions struct {
			ModelID map[string]any `json:"model_id"`
		} `json:"dimensions"`
		RecentRequests []any `json:"recent_requests"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v（body=%s）", err, recorder.Body.String())
	}
	if body.Stats.Requests != 0 || body.Stats.TotalTokens != 0 {
		t.Errorf("无流量时 stats = %+v，期望全 0", body.Stats)
	}
	if len(body.Dimensions.ModelID) != 0 {
		t.Errorf("无流量时模型拆分 = %+v，期望空", body.Dimensions.ModelID)
	}
	if len(body.RecentRequests) != 0 {
		t.Errorf("无流量时最近明细 = %d 条，期望 0", len(body.RecentRequests))
	}
	// 名字仍然要给：界面靠它渲染标题，即使是空看板。
	if body.AccessKeyName != "试用" {
		t.Errorf("access_key_name = %q，期望 试用", body.AccessKeyName)
	}
}

// TestAccessKeyUsageRejectsNonGet 固化非 GET 回 405 并带 Allow 头。
func TestAccessKeyUsageRejectsNonGet(t *testing.T) {
	app := newAccessKeyUsageApp(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		recorder := serve(app, method, "/ui/access-key-usage.json", "Bearer amkr_ak_trial")
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 状态码 = %d，期望 405", method, recorder.Code)
		}
		if allow := recorder.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s 的 Allow = %q，期望 GET", method, allow)
		}
	}
}

// TestAccessKeyUsageValidatesHours 固化参数校验先于鉴权（与 /metrics 同序）。
func TestAccessKeyUsageValidatesHours(t *testing.T) {
	app := newAccessKeyUsageApp(t)

	recorder := serve(app, http.MethodGet, "/ui/access-key-usage.json?hours=0", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("hours=0 状态码 = %d，期望 422（body=%s）", recorder.Code, recorder.Body.String())
	}
}
