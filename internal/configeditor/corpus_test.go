package configeditor

// 本文件是**对拍测试**：回放 gen_config_editor_corpus.py（已随 Python 退役移除） 用真实 Python
// 实现生成的语料（internal/configeditor/testdata/config_editor_corpus.json），
// 逐条比对请求构造与返回值。语料里没有任何 Go 侧写下的期望值。
//
// 之所以能逐字对拍：脚本化的 HTTP 桩在 Python 侧抛 `httpx.ReadError(msg)`、
// 在 Go 侧返回 `errors.New(msg)`，两边 `str(exc)` 都是 msg；其余字段（URL、方法、
// Authorization、请求体、状态码、时长、错误文本）都是纯逻辑产物。
//
// 刻意的比较方式：
//   - 对象用 canonical.DumpsOrdered 比较，因此**键顺序也被锁定**（capabilities /
//     key_models / errors / route_status 的顺序是参照实现的可观察行为）；
//   - 请求体比较的是解析后的 JSON，不是字节：Go 输出紧凑 JSON，httpx 的 `json=`
//     用 `json.dumps` 默认分隔符（见 doc.go）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

const corpusPath = "testdata/config_editor_corpus.json"

// corpusFixedNow / corpusClockStart / corpusClockStep 与生成脚本里的桩一致。
const (
	corpusClockStart = 1000.0
	corpusClockStep  = 0.25
)

// scriptedClock 复刻生成脚本里的 ScriptedClock（每次调用递增 0.25 秒）。
type scriptedClock struct{ value float64 }

func newScriptedClock() *scriptedClock { return &scriptedClock{value: corpusClockStart} }

func (c *scriptedClock) Now() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func (c *scriptedClock) EpochSeconds() float64 { return corpusFixedEpoch }

func (c *scriptedClock) MonotonicSeconds() float64 {
	current := c.value
	c.value += corpusClockStep
	return current
}

// recordedRequest 是一条被记录的请求。
type recordedRequest struct {
	method        string
	url           string
	authorization string
	body          *canonical.Value
	hasBody       bool
}

// scriptedTransport 是 Go 侧的 ScriptedTransport：按脚本应答并记录请求。
type scriptedTransport struct {
	t        *testing.T
	steps    []*canonical.Value
	pos      int
	requests []recordedRequest
}

func (s *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorded := recordedRequest{method: request.Method, url: request.URL.String()}
	recorded.authorization = request.Header.Get("Authorization")
	if request.Body != nil {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			s.t.Fatalf("读取请求体失败: %v", err)
		}
		_ = request.Body.Close()
		if len(raw) > 0 {
			value, err := canonical.Parse(raw)
			if err != nil {
				s.t.Fatalf("请求体不是合法 JSON: %v（%s）", err, raw)
			}
			recorded.body = value
			recorded.hasBody = true
		}
	}
	s.requests = append(s.requests, recorded)

	if s.pos >= len(s.steps) {
		s.t.Fatalf("脚本已用尽，但仍有请求: %s %s", request.Method, request.URL)
	}
	step := s.steps[s.pos]
	s.pos++
	if errorStep := step.Lookup("error"); errorStep.Truthy() {
		// Python 侧抛 httpx.ReadError/ConnectError(msg)，Go 侧返回等价的 error；
		// 两边 str(exc) 都是 msg，所以错误文本可以逐字比较。
		return nil, errors.New(step.Lookup("message").StringValue())
	}
	status, _ := canonical.ToInt(step.Lookup("status"))
	body := step.Lookup("body").StringValue()
	response := &http.Response{
		StatusCode: int(status),
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Request:    request,
	}
	return response, nil
}

// newCaseProber 构造注入了脚本化客户端的探测器。
func newCaseProber(t *testing.T, steps *canonical.Value) (*Prober, *scriptedTransport) {
	transport := &scriptedTransport{t: t, steps: steps.Items()}
	if transport.steps == nil {
		transport.steps = []*canonical.Value{}
	}
	return &Prober{
		Client: &http.Client{Transport: transport},
		Clock:  newScriptedClock(),
	}, transport
}

// newCaseProberWithoutSteps 构造只注入时钟的探测器（不需要 HTTP 的用例用它）。
func newCaseProberWithoutSteps() *Prober {
	return &Prober{Clock: newScriptedClock()}
}

// assertValue 用有序紧凑 JSON 比较两个 canonical 值（连带锁定键顺序）。
func assertValue(t *testing.T, label string, got, want *canonical.Value) {
	t.Helper()
	gotText := canonical.DumpsOrdered(got)
	wantText := canonical.DumpsOrdered(want)
	if gotText != wantText {
		t.Errorf("%s 不一致:\n got = %s\nwant = %s", label, gotText, wantText)
	}
}

// assertRequests 逐条比较被记录的请求。
func assertRequests(t *testing.T, transport *scriptedTransport, want *canonical.Value) {
	t.Helper()
	items := want.Items()
	if len(transport.requests) != len(items) {
		t.Fatalf("请求条数不一致: got %d want %d", len(transport.requests), len(items))
	}
	for index, item := range items {
		got := transport.requests[index]
		if got.method != item.Lookup("method").StringValue() {
			t.Errorf("请求 %d 方法: got %q want %q", index, got.method, item.Lookup("method").StringValue())
		}
		if got.url != item.Lookup("url").StringValue() {
			t.Errorf("请求 %d URL: got %q want %q", index, got.url, item.Lookup("url").StringValue())
		}
		wantAuth := item.Lookup("authorization")
		if wantAuth.IsNull() {
			if got.authorization != "" {
				t.Errorf("请求 %d 不应带 Authorization: %q", index, got.authorization)
			}
		} else if got.authorization != wantAuth.StringValue() {
			t.Errorf("请求 %d Authorization: got %q want %q", index, got.authorization, wantAuth.StringValue())
		}
		wantBody := item.Lookup("body")
		switch {
		case wantBody.IsNull():
			if got.hasBody {
				t.Errorf("请求 %d 不应有请求体: %s", index, canonical.DumpsOrdered(got.body))
			}
		case !got.hasBody:
			t.Errorf("请求 %d 缺少请求体: want %s", index, canonical.DumpsOrdered(wantBody))
		default:
			assertValue(t, fmt.Sprintf("请求 %d 请求体", index), got.body, wantBody)
		}
	}
}

// loadCorpus 读取语料。缺失或版本不符都直接失败：语料是契约的一部分。
func loadCorpus(t *testing.T) *canonical.Value {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	corpus, err := canonical.Parse(raw)
	if err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	version, _ := canonical.ToInt(corpus.Lookup("version"))
	if version != 1 {
		t.Fatalf("语料版本不支持: %d", version)
	}
	return corpus
}

func section(t *testing.T, corpus *canonical.Value, name string) []*canonical.Value {
	t.Helper()
	items := corpus.Lookup("sections").Lookup(name)
	if items == nil || !items.IsArray() {
		t.Fatalf("语料缺少段落 %q", name)
	}
	return items.Items()
}

// --------------------------------------------------------------------------- #
// probe_payload：请求体构造
// --------------------------------------------------------------------------- #

func TestCorpusProbePayload(t *testing.T) {
	for _, item := range section(t, loadCorpus(t), "probe_payload") {
		mode := item.Lookup("mode").StringValue()
		modelID := item.Lookup("model_id").StringValue()
		t.Run(mode+"/"+modelID, func(t *testing.T) {
			assertValue(t, "payload",
				ProbePayloadForMode(mode, modelID),
				item.Lookup("payload"))
		})
	}
}

// --------------------------------------------------------------------------- #
// probe_error_text：上游错误信封的提取与截断
// --------------------------------------------------------------------------- #

func TestCorpusProbeErrorText(t *testing.T) {
	for index, item := range section(t, loadCorpus(t), "probe_error_text") {
		body := item.Lookup("body").StringValue()
		want := item.Lookup("expected").StringValue()
		t.Run(fmt.Sprintf("case%02d", index), func(t *testing.T) {
			if got := ProbeErrorText([]byte(body)); got != want {
				t.Errorf("ProbeErrorText(%q) = %q, 期望 %q", body, got, want)
			}
		})
	}
}

// --------------------------------------------------------------------------- #
// discover_models：GET /v1/models
// --------------------------------------------------------------------------- #

func TestCorpusDiscoverUpstreamModels(t *testing.T) {
	corpus := loadCorpus(t)
	detailSentinel := corpus.Lookup("detail_sentinel").StringValue()
	for _, item := range section(t, corpus, "discover_models") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			prober, transport := newCaseProber(t, item.Lookup("steps"))
			models, errorText := prober.DiscoverUpstreamModelsResult(
				context.Background(),
				item.Lookup("base_url").StringValue(),
				item.Lookup("api_key").StringValue(),
				15.0,
			)
			assertRequests(t, transport, item.Lookup("requests"))
			assertValue(t, "models", canonical.NewStringArray(models), item.Lookup("models"))
			wantError := item.Lookup("error")
			if wantError.IsNull() {
				if errorText != "" {
					t.Errorf("不应有错误，得到 %q", errorText)
				}
				return
			}
			want := wantError.StringValue()
			// JSON 解析失败的 detail 是解释器解析器的文本，Go 与 Python 不可能一致：
			// 只锁定前缀、detail 非空、总长不超过 160 个字符（Python 的切片）。
			if strings.HasSuffix(want, detailSentinel) {
				prefix := strings.TrimSuffix(want, detailSentinel)
				if !strings.HasPrefix(errorText, prefix) {
					t.Fatalf("错误文本前缀: got %q want 前缀 %q", errorText, prefix)
				}
				detail := strings.TrimPrefix(errorText, prefix)
				if detail == "" {
					t.Fatalf("错误文本缺少 detail: %q", errorText)
				}
				if length := len([]rune(errorText)); length > 160 {
					t.Fatalf("错误文本长度 %d 超过 160: %q", length, errorText)
				}
				return
			}
			if errorText != want {
				t.Errorf("错误文本: got %q want %q", errorText, want)
			}
		})
	}
}

// --------------------------------------------------------------------------- #
// probe_availability：每个文本路由发一次最小请求
// --------------------------------------------------------------------------- #

func TestCorpusProbeKeyAvailability(t *testing.T) {
	for _, item := range section(t, loadCorpus(t), "probe_availability") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			prober, transport := newCaseProber(t, item.Lookup("steps"))
			results, err := prober.ProbeKeyAvailabilityForData(
				context.Background(),
				item.Lookup("data"),
				item.Lookup("model_id").StringValue(),
				item.Lookup("key"),
				15.0,
				stringList(item.Lookup("modes")),
			)
			if err != nil {
				t.Fatalf("探测失败: %v", err)
			}
			assertRequests(t, transport, item.Lookup("requests"))
			assertValue(t, "results", keyProbeResultsValue(results), item.Lookup("results"))
		})
	}
}

// keyProbeResultsValue 把 []KeyProbeResult 折成与语料同构的 canonical 值。
func keyProbeResultsValue(results []KeyProbeResult) *canonical.Value {
	items := make([]*canonical.Value, 0, len(results))
	for _, result := range results {
		status := canonical.NewNull()
		if result.StatusCode != nil {
			status = canonical.NewIntValue(int64(*result.StatusCode))
		}
		items = append(items, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "model_id", Value: canonical.NewString(result.ModelID)},
			canonical.ObjectPair{Key: "key_name", Value: canonical.NewString(result.KeyName)},
			canonical.ObjectPair{Key: "mode", Value: canonical.NewString(result.Mode)},
			canonical.ObjectPair{Key: "label", Value: canonical.NewString(result.Label)},
			canonical.ObjectPair{Key: "path", Value: canonical.NewString(result.Path)},
			canonical.ObjectPair{Key: "url", Value: canonical.NewString(result.URL)},
			canonical.ObjectPair{Key: "available", Value: canonical.NewBool(result.Available)},
			canonical.ObjectPair{Key: "status_code", Value: status},
			canonical.ObjectPair{Key: "duration_ms", Value: canonical.NewIntValue(int64(result.DurationMS))},
			canonical.ObjectPair{Key: "error", Value: canonical.NewString(result.Error)},
		))
	}
	return canonical.NewArray(items...)
}

// stringList 把语料里的数组（或 null）折成 []string。
func stringList(value *canonical.Value) []string {
	if !value.IsArray() {
		return nil
	}
	out := make([]string, 0, len(value.Arr))
	for _, item := range value.Arr {
		out = append(out, item.StringValue())
	}
	return out
}

// --------------------------------------------------------------------------- #
// probe_capability：单个 Key 的能力缓存
// --------------------------------------------------------------------------- #

func TestCorpusProbeKeyCapability(t *testing.T) {
	for _, item := range section(t, loadCorpus(t), "probe_capability") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			prober, transport := newCaseProber(t, item.Lookup("steps"))
			capabilities, err := prober.ProbeKeyCapability(
				cloneValue(item.Lookup("provider")),
				item.Lookup("key_name").StringValue(),
				stringList(item.Lookup("modes")),
				15.0,
			)
			if err != nil {
				t.Fatalf("探测失败: %v", err)
			}
			assertRequests(t, transport, item.Lookup("requests"))
			assertValue(t, "capabilities", capabilities, item.Lookup("capabilities"))
		})
	}
}

// --------------------------------------------------------------------------- #
// probe_provider_key_capabilities：多 Key 的交集 / 并集
// --------------------------------------------------------------------------- #

func TestCorpusProbeProviderKeyCapabilities(t *testing.T) {
	for _, item := range section(t, loadCorpus(t), "probe_provider_key_capabilities") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			prober, transport := newCaseProber(t, item.Lookup("steps"))
			result, err := prober.ProbeProviderKeyCapabilities(
				cloneValue(item.Lookup("provider")),
				stringList(item.Lookup("key_names")),
				15.0,
			)
			if err != nil {
				t.Fatalf("探测失败: %v", err)
			}
			assertRequests(t, transport, item.Lookup("requests"))
			assertValue(t, "result", result, item.Lookup("result"))
		})
	}
}

// cloneValue 深拷贝语料里的输入，避免被测函数就地改写后影响后续断言。
func cloneValue(value *canonical.Value) *canonical.Value {
	if value == nil {
		return canonical.NewNull()
	}
	return value.Clone()
}

// --------------------------------------------------------------------------- #
// upstream_routes_for_base_url：路由合并
// --------------------------------------------------------------------------- #

func TestCorpusUpstreamRoutesForBaseURL(t *testing.T) {
	for _, item := range section(t, loadCorpus(t), "upstream_routes_for_base_url") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			routes, err := UpstreamRoutesForBaseURL(
				cloneValue(item.Lookup("data")),
				item.Lookup("base_url").StringValue(),
			)
			if item.Lookup("error").Truthy() {
				if err == nil {
					t.Fatalf("应当失败，却返回了 %s", canonical.DumpsOrdered(routes))
				}
				if got, want := err.Error(), item.Lookup("message").StringValue(); got != want {
					t.Errorf("错误文本: got %q want %q", got, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应失败: %v", err)
			}
			pairs := canonical.NewArray()
			for _, key := range routes.Obj.Keys() {
				child, _ := routes.Obj.Get(key)
				// 值为数字时不走 PyStr（语料里保留了原始的 42），直接放进数组。
				pairs.Arr = append(pairs.Arr, canonical.NewArray(canonical.NewString(key), child))
			}
			assertValue(t, "pairs", pairs, item.Lookup("pairs"))
		})
	}
}

// --------------------------------------------------------------------------- #
// service_management_base_url / 原生端点状态
// --------------------------------------------------------------------------- #

func TestCorpusServiceManagementBaseURL(t *testing.T) {
	for _, item := range section(t, loadCorpus(t), "service_management_base_url") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			url, err := ServiceManagementBaseURL(cloneValue(item.Lookup("data")))
			if !item.Lookup("error").IsNull() {
				if err == nil {
					t.Fatalf("应当失败，却得到 %q", url)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应失败: %v", err)
			}
			if want := item.Lookup("url").StringValue(); url != want {
				t.Errorf("URL: got %q want %q", url, want)
			}
		})
	}
}

func TestCorpusNativeEndpointStatePayload(t *testing.T) {
	for index, item := range section(t, loadCorpus(t), "native_endpoint_state_payload") {
		item := item
		t.Run(fmt.Sprintf("case%02d", index), func(t *testing.T) {
			payload, err := NativeEndpointStatePayload(cloneValue(item.Lookup("value")), corpusFixedEpoch)
			if !item.Lookup("error").IsNull() {
				if err == nil {
					t.Fatalf("应当失败，却得到 %s", canonical.DumpsOrdered(payload))
				}
				return
			}
			if err != nil {
				t.Fatalf("不应失败: %v", err)
			}
			want := item.Lookup("payload")
			if want.IsNull() {
				if payload != nil {
					t.Fatalf("应当返回 nil，得到 %s", canonical.DumpsOrdered(payload))
				}
				return
			}
			if payload == nil {
				t.Fatalf("不应返回 nil，期望 %s", canonical.DumpsOrdered(want))
			}
			assertValue(t, "payload", payload, want)
		})
	}
}

// corpusFixedEpoch 与生成脚本的 FIXED_EPOCH 一致（time.time() 的桩）。
const corpusFixedEpoch = 1_800_000_000.0

func TestCorpusFormatVisitorStatusText(t *testing.T) {
	for index, item := range section(t, loadCorpus(t), "format_visitor_status_text") {
		item := item
		t.Run(fmt.Sprintf("case%02d", index), func(t *testing.T) {
			got := FormatVisitorStatusText(
				item.Lookup("visitor_allowed").Truthy(),
				item.Lookup("visitor_installed").Truthy(),
			)
			if want := item.Lookup("text").StringValue(); got != want {
				t.Errorf("文本: got %q want %q", got, want)
			}
		})
	}
}

func TestCorpusNativeEndpointSupportText(t *testing.T) {
	for index, item := range section(t, loadCorpus(t), "native_endpoint_support_text") {
		item := item
		t.Run(fmt.Sprintf("case%02d", index), func(t *testing.T) {
			got := NativeEndpointSupportText(cloneValue(item.Lookup("state")))
			if want := item.Lookup("text").StringValue(); got != want {
				t.Errorf("文本: got %q want %q", got, want)
			}
		})
	}
}

// --------------------------------------------------------------------------- #
// 从文件读原生端点状态：语料只带文件内容，路径由测试自己造临时文件
// --------------------------------------------------------------------------- #

func TestCorpusLoadNativeEndpointStatesFromFile(t *testing.T) {
	dir := t.TempDir()
	for _, item := range section(t, loadCorpus(t), "native_endpoint_states_from_file") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			path := filepath.Join(dir, "endpoint-capabilities.json")
			_ = os.Remove(path)
			kind := item.Lookup("path_kind").StringValue()
			switch kind {
			case "file":
				if err := os.WriteFile(path, []byte(item.Lookup("content").StringValue()), 0o600); err != nil {
					t.Fatalf("写临时文件失败: %v", err)
				}
			case "directory":
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatalf("建临时目录失败: %v", err)
				}
			case "missing":
				// 路径不存在即为所需状态。
			default:
				t.Fatalf("未知的 path_kind: %q", kind)
			}
			data := canonical.NewObjectOf(canonical.ObjectPair{
				Key:   "endpoint_capabilities_path",
				Value: canonical.NewString(path),
			})
			states, err := newCaseProberWithoutSteps().LoadNativeEndpointStatesFromFile(data)
			if !item.Lookup("error").IsNull() {
				if err == nil {
					t.Fatalf("应当失败，却得到 %s", canonical.DumpsOrdered(states))
				}
				return
			}
			if err != nil {
				t.Fatalf("不应失败: %v", err)
			}
			assertValue(t, "states", states, item.Lookup("expected"))
		})
	}
}
