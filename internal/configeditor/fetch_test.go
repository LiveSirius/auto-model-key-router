package configeditor

// fetch_native_endpoint_states 的刻意差异。
//
// 参照实现用模块级 `httpx.get(...)`（无 Authorization）。

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestFetchNativeEndpointStatesToleratesNonObjectHealth 记录刻意差异：/health 返回的
// 顶层不是对象时，参照实现会在 `response.json().get(...)` 上抛 AttributeError（把整个
// dashboard 打挂）；Go 侧按「只用文件缓存」处理。
func TestFetchNativeEndpointStatesToleratesNonObjectHealth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoint-capabilities.json")
	if err := os.WriteFile(path, []byte(`{"endpoint_capabilities": {"u": true}}`), 0o600); err != nil {
		t.Fatalf("写能力缓存失败: %v", err)
	}
	data := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "endpoint_capabilities_path", Value: canonical.NewString(path)},
	)
	transport := &scriptedTransport{t: t, steps: []*canonical.Value{
		canonicalFromRaw(`{"status": 200, "body": "[]"}`),
	}}
	prober := Prober{Client: &http.Client{Transport: transport}, Clock: newScriptedClock()}
	states, err := prober.FetchNativeEndpointStates(context.Background(), data, 0.5)
	if err != nil {
		t.Fatalf("不应失败: %v", err)
	}
	// 顶层不是对象 => 只用文件缓存（文件里 u=true 折算成 legacy 状态）。
	assertValue(t, "states", states, canonicalFromRaw(
		`{"u": {"supported": true, "reason": "legacy", "expires_in_seconds": null}}`,
	))
}
