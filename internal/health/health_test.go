package health

import (
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// fakeKeyPool 是可控的 KeyPool 假实现，用来构造真实 pool 上难以出现的组合
// （访客功能未安装、没有模型、unified 未配置等）。
type fakeKeyPool struct {
	publicModelIDs   []string
	modelIDs         []string
	visitorKeyCounts map[string]int
	unifiedRoute     *canonical.Value
	endpointStates   *canonical.Value
}

func (f *fakeKeyPool) PublicModelIDs() []string { return f.publicModelIDs }
func (f *fakeKeyPool) ModelIDs() []string       { return f.modelIDs }
func (f *fakeKeyPool) VisitorKeyCount(modelID string) int {
	return f.visitorKeyCounts[modelID]
}
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
func TestBuildFieldOrderMatchesPython(t *testing.T) {
	got := canonical.DumpsOrdered(Build(Inputs{
		Version:     "4.1.0",
		ConfigPath:  "/tmp/c.json",
		LocalAPIKey: "amkr_abc",
		// 各模型都没有 visitor key。
		OpsEnabled: false,
		KeyPool: &fakeKeyPool{
			publicModelIDs:   []string{"gpt-4"},
			modelIDs:         []string{"gpt-4"},
			visitorKeyCounts: map[string]int{"gpt-4": 0},
			unifiedRoute:     nil,
		},
		WebUI: WebUI{Available: false, Enabled: false, Mounted: false},
	}))
	want := `{"status":"ok","version":"4.1.0","models":["gpt-4"],"config_path":"/tmp/c.json","local_auth_enabled":true,"local_api_key_fingerprint":"63c833584280","visitor_access_enabled":false,"visitor_key_count":0,"unified_model":null,"native_endpoint_states":{},"ops_enabled":false,"webui_available":false,"webui_enabled":false,"webui_mounted":false,"webui_path":null}`
	if got != want {
		t.Errorf("/health 响应不符\n实际 %s\n期望 %s", got, want)
	}
}

// TestBuildVisitorCountersMatchDecidedSemantics 锁定 visitor 的两个计数字段。
//
// 与参照实现相比，这里锁的是**已决策的新语义**：访客功能取消开关、常驻，因此
//   - visitor_key_count 不再在「功能未安装」时归零，恒为各模型之和；
//   - visitor_access_enabled 退化为单纯的「数量 > 0」。
//
// 参照实现原本是 sum(...) if installed else 0 与 installed and count > 0（app.py:174）。
func TestBuildVisitorCountersMatchDecidedSemantics(t *testing.T) {
	cases := []struct {
		name       string
		counts     map[string]int
		modelIDs   []string
		wantAccess bool
		wantCount  int
	}{
		{"多个模型求和", map[string]int{"a": 2, "b": 3}, []string{"a", "b"}, true, 5},
		{"全为零则访问不启用", map[string]int{"a": 0, "b": 0}, []string{"a", "b"}, false, 0},
		{"没有任何模型", map[string]int{}, []string{}, false, 0},
		{"单个模型有 key", map[string]int{"a": 1}, []string{"a"}, true, 1},
		// 模型 ID 未出现在计数表里时按 0 计，不应 panic。
		{"缺计数按零计", map[string]int{}, []string{"a", "b"}, false, 0},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := Build(Inputs{
				KeyPool: &fakeKeyPool{
					publicModelIDs:   item.modelIDs,
					modelIDs:         item.modelIDs,
					visitorKeyCounts: item.counts,
				},
			})
			access, _ := got.Lookup("visitor_access_enabled").AsBool()
			count, _ := got.Lookup("visitor_key_count").AsInt()
			if access != item.wantAccess || count != int64(item.wantCount) {
				t.Errorf("counts=%v\n实际 access=%v count=%d\n期望 access=%v count=%d",
					item.counts, access, count, item.wantAccess, item.wantCount)
			}
		})
	}
	// 字段本身必须**不再出现**在响应里。
	if _, exists := Build(Inputs{KeyPool: &fakeKeyPool{}}).LookupOK("visitor_feature_installed"); exists {
		t.Error("visitor_feature_installed 应已从 /health 删除")
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
