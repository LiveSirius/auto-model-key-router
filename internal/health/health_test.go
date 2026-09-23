package health

import (
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// fakeKeyPool 是可控的 KeyPool 假实现，用来构造真实 pool 上难以出现的组合
// （没有模型、unified 未配置等）。
type fakeKeyPool struct {
	publicModelIDs []string
	unifiedRoute   *canonical.Value
	endpointStates *canonical.Value
}

func (f *fakeKeyPool) PublicModelIDs() []string       { return f.publicModelIDs }
func (f *fakeKeyPool) UnifiedRoute() *canonical.Value { return f.unifiedRoute }
func (f *fakeKeyPool) EndpointCapabilityStates() *canonical.Value {
	if f.endpointStates == nil {
		return canonical.NewObject()
	}
	return f.endpointStates
}

// TestBuildFieldOrderMatchesPython 逐字节锁定字段顺序与全部取值。
//
// 期望值来自对参照实现的实测：按 app.py:165-179 的字典字面量与 webui.py:59-64 的
// 构造方式，用相同输入在真实 Python 里 dump（ensure_ascii=False,
// separators=(",", ":")）。字段顺序是契约的一部分——canonical 按插入顺序输出，
// 重排会改变响应字节，而调用方按字段名解析、缓存与比对。
//
// 与参照实现相比少了三个 visitor 字段（随访客模式一并删除，见 Build 的说明）；
// 其余字段的字面值仍然逐字节一致。
func TestBuildFieldOrderMatchesPython(t *testing.T) {
	got := canonical.DumpsOrdered(Build(Inputs{
		Version:     "4.1.0",
		ConfigPath:  "/tmp/c.json",
		LocalAPIKey: "amkr_abc",
		OpsEnabled:  false,
		KeyPool: &fakeKeyPool{
			publicModelIDs: []string{"gpt-4"},
			unifiedRoute:   nil,
		},
		WebUI: WebUI{Available: false, Enabled: false, Mounted: false},
	}))
	want := `{"status":"ok","version":"4.1.0","models":["gpt-4"],"config_path":"/tmp/c.json","local_auth_enabled":true,"local_api_key_fingerprint":"63c833584280","unified_model":null,"native_endpoint_states":{},"ops_enabled":false,"webui_available":false,"webui_enabled":false,"webui_mounted":false,"webui_path":null}`
	if got != want {
		t.Errorf("/health 响应不符\n实际 %s\n期望 %s", got, want)
	}
}

// TestBuildDropsVisitorFields 断言三个 visitor 字段**不再出现**在响应里。
//
// 这是那条「删干净」的回归线：字段名是外部契约的一部分，只要有人把它们加回来，
// 这里就会红。/health 无鉴权，报出「本实例发了几把受限 key」本来就是没必要的泄漏面。
func TestBuildDropsVisitorFields(t *testing.T) {
	got := Build(Inputs{KeyPool: &fakeKeyPool{}})
	for _, field := range []string{"visitor_feature_installed", "visitor_access_enabled", "visitor_key_count"} {
		if _, exists := got.LookupOK(field); exists {
			t.Errorf("%s 应已从 /health 删除", field)
		}
	}
}

// TestBuildLocalAuthMatchesPython 锁定本地鉴权开关与指纹。
//
// local_auth_enabled 是「key 非空」；fingerprint 在 key 为空时是空串（不是 null），
// 因为 key_fingerprint 显式返回 ""（formatting.py:9-10）且被包装成字符串。
func TestBuildLocalAuthMatchesPython(t *testing.T) {
	cases := []struct {
		key         string
		wantAuth    bool
		wantFingerp string
	}{
		{"", false, ""},
		{"amkr_abc", true, "63c833584280"},
		{"x", true, "2d711642b726"},
	}
	for _, item := range cases {
		got := Build(Inputs{LocalAPIKey: item.key, KeyPool: &fakeKeyPool{}})
		auth, _ := got.Lookup("local_auth_enabled").AsBool()
		fingerprint := got.Lookup("local_api_key_fingerprint").PyStr()
		if auth != item.wantAuth || fingerprint != item.wantFingerp {
			t.Errorf("key=%q\n实际 auth=%v fp=%q\n期望 auth=%v fp=%q",
				item.key, auth, fingerprint, item.wantAuth, item.wantFingerp)
		}
	}
}

// TestBuildWebUIPathMatchesPython 锁定 webui_path 的 null 与前缀拼接。
//
// 未挂载时是 null 而不是空串——两者在 JSON 里不同，调用方用 `is None` 判断。
// 前缀先 rstrip("/")，所以 "/amkr" 与 "/amkr/" 都得到 "/amkr/ui"。
func TestBuildWebUIPathMatchesPython(t *testing.T) {
	cases := []struct {
		mounted  bool
		prefix   string
		wantJSON string
	}{
		{false, "", `null`},
		{true, "", `"/ui"`},
		{true, "/amkr", `"/amkr/ui"`},
		{true, "/amkr/", `"/amkr/ui"`},
	}
	for _, item := range cases {
		got := Build(Inputs{
			KeyPool: &fakeKeyPool{},
			WebUI:   WebUI{Available: true, Enabled: true, Mounted: item.mounted, MountPrefix: item.prefix},
		})
		path := canonical.DumpsOrdered(got.Lookup("webui_path"))
		if path != item.wantJSON {
			t.Errorf("mounted=%v prefix=%q -> webui_path=%s，期望 %s",
				item.mounted, item.prefix, path, item.wantJSON)
		}
	}
}

// TestBuildEmptyModelsIsArrayNotNull 锁定 models 空时是 [] 而不是 null。
//
// 参照实现里 models 来自 pool 的属性，永远是列表；若 Go 侧把 nil 切片直接交给
// 编码器会得到 null，调用方遍历时崩溃。
func TestBuildEmptyModelsIsArrayNotNull(t *testing.T) {
	got := canonical.DumpsOrdered(Build(Inputs{KeyPool: &fakeKeyPool{publicModelIDs: nil}}).Lookup("models"))
	if got != "[]" {
		t.Fatalf("空模型列表应渲染为 []，实际 %s", got)
	}
	// KeyPool 为 nil 时也不能 panic，并且同样是 []。
	got = canonical.DumpsOrdered(Build(Inputs{}).Lookup("models"))
	if got != "[]" {
		t.Fatalf("无 KeyPool 时 models 应为 []，实际 %s", got)
	}
}

// TestBuildOpsEnabledCoercionLivesAtCaller 记录 ops_enabled 的强制布尔由调用方完成。
//
// 参照实现写的是 bool(getattr(app.state, "ops_enabled", True))（app.py:177），因此
// 0/""/None 都会变成 false、非空字符串变成 true。Go 侧 Inputs.OpsEnabled 已经是
// bool，转换在装配处完成；本测试只锁定"传什么出什么"，避免有人误以为本包会再做
// 一次真值判断。
func TestBuildOpsEnabledCoercionLivesAtCaller(t *testing.T) {
	for _, want := range []bool{true, false} {
		got, ok := Build(Inputs{OpsEnabled: want, KeyPool: &fakeKeyPool{}}).Lookup("ops_enabled").AsBool()
		if !ok || got != want {
			t.Errorf("ops_enabled 应原样输出 %v，实际 %v", want, got)
		}
	}
}

// TestBuildUnifiedAndNativeStatesPreserveNull 锁定 unified/native 为空时的 null。
func TestBuildUnifiedAndNativeStatesPreserveNull(t *testing.T) {
	got := Build(Inputs{KeyPool: &fakeKeyPool{unifiedRoute: nil}})
	if unified := canonical.DumpsOrdered(got.Lookup("unified_model")); unified != "null" {
		t.Errorf("未配置 unified 路由应为 null，实际 %s", unified)
	}
	// 有值时原样透传（不重新序列化语义）。
	route := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "default", Value: canonical.NewString("gpt-4")},
	)
	got = Build(Inputs{KeyPool: &fakeKeyPool{unifiedRoute: route}})
	want := `{"default":"gpt-4"}`
	if unified := canonical.DumpsOrdered(got.Lookup("unified_model")); unified != want {
		t.Errorf("unified_model 应原样透传，实际 %s，期望 %s", unified, want)
	}
}
