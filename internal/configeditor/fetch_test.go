package configeditor

// fetch_native_endpoint_states 的对拍与相关刻意差异。
//
// 参照实现用模块级 `httpx.get(...)`（无 Authorization），生成脚本把 httpx.get 也换成
// 脚本化实现，因此请求 URL 与合并结果都能逐字比较。文件部分由语料给出内容、测试自己
// 写临时文件（两边的临时路径必然不同）。

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

func TestCorpusFetchNativeEndpointStates(t *testing.T) {
	corpus := loadCorpus(t)
	for _, item := range section(t, corpus, "fetch_native_endpoint_states") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "endpoint-capabilities.json")
			if err := os.WriteFile(path, []byte(item.Lookup("file_content").StringValue()), 0o600); err != nil {
				t.Fatalf("写能力缓存失败: %v", err)
			}
			data := item.Lookup("data").Clone()
			data.SetKey("endpoint_capabilities_path", canonical.NewString(path))
			transport := &scriptedTransport{t: t, steps: item.Lookup("steps").Items()}
			prober := Prober{Client: &http.Client{Transport: transport}, Clock: newScriptedClock()}
			states, err := prober.FetchNativeEndpointStates(context.Background(), data, 0.5)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			assertRequests(t, transport, item.Lookup("requests"))
			if transport.pos != len(transport.steps) {
				t.Fatalf("网络脚本未用尽：已用 %d / 共 %d", transport.pos, len(transport.steps))
			}
			assertValue(t, "states", states, item.Lookup("expected"))
		})
	}
}

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
