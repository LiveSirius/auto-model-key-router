package auth

import (
	"net/http"
	"testing"
)

// pythonLocalKey 取自参照实现实测值。
const pythonLocalKey = "amkr_local_secret_123"

// TestModeFromAPIKeyMatchesPython 逐条对齐参照实现的判定结果。
//
// 期望值全部来自对 auto_model_key_router.auth.mode_from_api_key 的实测调用，不是
// 推断出来的。这是鉴权路径，判定错一条就等于开了一个口子。
func TestModeFromAPIKeyMatchesPython(t *testing.T) {
	cases := []struct {
		name         string
		localAPIKey  string
		apiKey       string
		wantMode     Mode
		wantRejected bool
	}{
		{name: "本地 key 为空时整体关闭鉴权", localAPIKey: "", apiKey: "", wantMode: ModeFull},
		{name: "本地 key 为空时一律完整权限", localAPIKey: "", apiKey: "anything", wantMode: ModeFull},
		{name: "本地 key 匹配为完整权限", localAPIKey: pythonLocalKey, apiKey: pythonLocalKey, wantMode: ModeFull},
		{name: "mis-match 拒绝", localAPIKey: pythonLocalKey, apiKey: "wrong", wantRejected: true},
		// 前缀/后缀/大小写都不接受，防止有人把比较改成 HasPrefix 之类的"优化"。
		{name: "本地 key 的前缀被拒绝", localAPIKey: pythonLocalKey, apiKey: pythonLocalKey[:len(pythonLocalKey)-1], wantRejected: true},
		{name: "本地 key 加后缀被拒绝", localAPIKey: pythonLocalKey, apiKey: pythonLocalKey + "x", wantRejected: true},
		{name: "大小写不同被拒绝", localAPIKey: pythonLocalKey, apiKey: "AMKR_LOCAL_SECRET_123", wantRejected: true},
		{name: "首尾空白不被忽略", localAPIKey: pythonLocalKey, apiKey: " " + pythonLocalKey + " ", wantRejected: true},
		// 已取消的固定访客 key 必须被拒绝：它不是 local_api_key，也不再有任何特殊通道。
		// 留着这几条是为了防止将来有人把 `amkr-visitor` 当成"兼容旧客户端"重新接回来。
		{name: "已取消的访客 key 被拒绝", localAPIKey: pythonLocalKey, apiKey: "amkr-visitor", wantRejected: true},
		{name: "访客 key 大写被拒绝", localAPIKey: pythonLocalKey, apiKey: "AMKR-VISITOR", wantRejected: true},
		{name: "下划线代替连字符被拒绝", localAPIKey: pythonLocalKey, apiKey: "amkr_visitor", wantRejected: true},
		// 非 ASCII：参照实现不会 500，只拒绝。
		{name: "非 ASCII 凭据被拒绝而非报错", localAPIKey: pythonLocalKey, apiKey: "中文", wantRejected: true},
		{name: "非 ASCII emoji 被拒绝", localAPIKey: pythonLocalKey, apiKey: "😀", wantRejected: true},
		{name: "含 NUL 字节被拒绝", localAPIKey: pythonLocalKey, apiKey: "a\x00b", wantRejected: true},
		{name: "非 ASCII 本地 key 可精确匹配", localAPIKey: "中文key", apiKey: "中文key", wantMode: ModeFull},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := ModeFromAPIKey(item.apiKey, item.localAPIKey)
			if item.wantRejected {
				if got != nil {
					t.Fatalf("应被拒绝，实际得到 mode=%q", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("不应被拒绝，期望 mode=%q", item.wantMode)
			}
			if *got != item.wantMode {
				t.Fatalf("mode 不符: 期望 %q，实际 %q", item.wantMode, *got)
			}
		})
	}
}

// 访客模式已整体移除：固定 key `amkr-visitor`、ModeVisitor 与 VisitorModelPrefix 都
// 不再存在，取而代之的是配置里的访问密钥资源（见 config.AccessKeyConfig）。原先的
// visitor_test.go 随之删除。上面那条「已取消的访客 key 被拒绝」是留给这次删除的回归
// 断言：它必须和任何其它错误凭据一样被拒绝。

// TestRequestAPIKeyMatchesPython 逐条对齐凭据提取规则。
//
// 期望值来自对 request_api_key 的实测调用（用真实的 Starlette Request 构造）。
// 规则里的细节都是有意为之：只认 "bearer "（带空格）、大小写不敏感、取到后 strip、
// 非 Bearer 形态回落到 x-api-key。
func TestRequestAPIKeyMatchesPython(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{name: "标准 Bearer", headers: map[string]string{"Authorization": "Bearer sk-abc"}, want: "sk-abc"},
		{name: "Bearer 大写", headers: map[string]string{"Authorization": "BEARER sk-abc"}, want: "sk-abc"},
		{name: "Bearer 混合大小写", headers: map[string]string{"Authorization": "BeArEr sk-abc"}, want: "sk-abc"},
		{name: "多余空白被 strip", headers: map[string]string{"Authorization": "Bearer   sk-abc  "}, want: "sk-abc"},
		// "Bearersk-abc" 不以 "bearer " 开头（缺空格），因此不当作 Bearer，回落 x-api-key。
		{name: "缺空格的 Bearer 不识别", headers: map[string]string{"Authorization": "Bearersk-abc"}, want: ""},
		{name: "光秃 Bearer 不识别", headers: map[string]string{"Authorization": "Bearer"}, want: ""},
		{name: "Bearer 后为空", headers: map[string]string{"Authorization": "Bearer "}, want: ""},
		// 分隔符必须是空格，Tab 不算。
		{name: "Bearer 后跟 Tab 不识别", headers: map[string]string{"Authorization": "Bearer\tx"}, want: ""},
		{name: "Basic 形态回落到 x-api-key", headers: map[string]string{"Authorization": "Basic dXNlcg=="}, want: ""},
		{name: "仅 x-api-key", headers: map[string]string{"x-api-key": "sk-xyz"}, want: "sk-xyz"},
		{name: "x-api-key 大小写不敏感", headers: map[string]string{"X-API-Key": "sk-upper"}, want: "sk-upper"},
		{name: "无凭据", headers: map[string]string{}, want: ""},
		{name: "两者同时存在时 Bearer 优先", headers: map[string]string{"Authorization": "Bearer sk-a", "x-api-key": "sk-b"}, want: "sk-a"},
		{name: "Basic 时才用 x-api-key", headers: map[string]string{"Authorization": "Basic z", "x-api-key": "sk-b"}, want: "sk-b"},
		// latin-1 范围内的非 ASCII 凭据原样返回。
		{name: "latin1 非 ASCII 原样返回", headers: map[string]string{"x-api-key": "éè"}, want: "éè"},
		{name: "Bearer 后 latin1 非 ASCII", headers: map[string]string{"Authorization": "Bearer éè"}, want: "éè"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			headers := http.Header{}
			for key, value := range item.headers {
				headers.Set(key, value)
			}
			if got := RequestAPIKey(headers); got != item.want {
				t.Fatalf("凭据提取不符: 期望 %q，实际 %q", item.want, got)
			}
		})
	}
}

// TestDefaultAuthorizerWiring 验证默认实现把提取与判定接在一起。
func TestDefaultAuthorizerWiring(t *testing.T) {
	build := func(authorization, apiKey string) *http.Request {
		request, err := http.NewRequest(http.MethodGet, "http://amkr.local/v1/models", nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		if apiKey != "" {
			request.Header.Set("x-api-key", apiKey)
		}
		return request
	}

	if got := DefaultAuthorizer(build("Bearer "+pythonLocalKey, ""), pythonLocalKey); got == nil || !got.IsFull() {
		t.Fatalf("本地 key 应为完整权限，实际 %+v", got)
	}
	if got := DefaultAuthorizer(build("Bearer wrong", ""), pythonLocalKey); got != nil {
		t.Fatalf("错误凭据应被拒绝，实际 %+v", got)
	}
	if got := DefaultAuthorizer(build("", ""), pythonLocalKey); got != nil {
		t.Fatalf("无凭据应被拒绝，实际 %+v", got)
	}
	// 本地 key 为空 = 鉴权整体关闭，无凭据也放行。
	if got := DefaultAuthorizer(build("", ""), ""); got == nil || !got.IsFull() {
		t.Fatalf("鉴权关闭时应放行并给完整权限，实际 %+v", got)
	}
}

// TestAuthenticateUsesCustomAuthorizer 验证宿主替换鉴权钩子的路径。
//
// 这是宿主嵌入时的关键接缝：宿主通常已有自己的身份体系，需要整体替换判定。
func TestAuthenticateUsesCustomAuthorizer(t *testing.T) {
	called := false
	custom := func(r *http.Request, localAPIKey string) *Context {
		called = true
		if got := r.Header.Get("X-Host-Identity"); got != "trusted" {
			t.Fatalf("自定义钩子应看到宿主的头，实际 %q", got)
		}
		return &Context{Mode: ModeFull}
	}
	request, err := http.NewRequest(http.MethodGet, "http://amkr.local/", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("X-Host-Identity", "trusted")

	if got := Authenticate(custom, request, pythonLocalKey); got == nil || !got.IsFull() {
		t.Fatalf("自定义钩子应生效，实际 %+v", got)
	}
	if !called {
		t.Fatal("自定义钩子未被调用")
	}
	// nil 钩子回落到默认实现。
	if got := Authenticate(nil, request, pythonLocalKey); got != nil {
		t.Fatalf("nil 钩子应回落到默认实现并拒绝无凭据请求，实际 %+v", got)
	}
}

// TestWebsocketAuthRequestPreservesHandshakeHeaders 验证 WebSocket 首帧鉴权路径。
//
// 为什么保留原始握手头很重要：AMKR 的凭据只能走首帧（握手不能带自定义头），但宿主
// 的 session cookie 是**在**握手头里的。丢掉它们会让只用 cookie 的宿主在 WebSocket
// 上鉴权失败。
func TestWebsocketAuthRequestPreservesHandshakeHeaders(t *testing.T) {
	handshake := http.Header{}
	handshake.Set("Cookie", "session=host-cookie-value")
	handshake.Set("X-Host-Identity", "trusted")

	token := "amkr_ws_token"
	request := WebsocketAuthRequest(handshake, token)

	// 补上的 Authorization 必须能被常规提取逻辑读到。
	if got := RequestAPIKey(request.Header); got != token {
		t.Fatalf("首帧 token 应可被提取，实际 %q", got)
	}
	// 宿主自己的头必须原样保留。
	if got := request.Header.Get("Cookie"); got != "session=host-cookie-value" {
		t.Fatalf("握手 cookie 应保留，实际 %q", got)
	}
	if got := request.Header.Get("X-Host-Identity"); got != "trusted" {
		t.Fatalf("宿主身份头应保留，实际 %q", got)
	}
	if request.Method != http.MethodGet {
		t.Fatalf("折算后的请求应为 GET，实际 %q", request.Method)
	}
	if request.Body != nil {
		t.Fatal("鉴权只读 header，折算请求不应带 body")
	}
	// 不修改调用方传入的头，避免握手头被就地污染。
	if got := handshake.Get("Authorization"); got != "" {
		t.Fatalf("不应修改调用方传入的握手头，实际 Authorization=%q", got)
	}
}
