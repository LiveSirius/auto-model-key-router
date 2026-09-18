package configeditor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/api"
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/configops"
	"github.com/Sparrived/auto-model-key-router/internal/proxysupport"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// probeRouteModes 对应 config_editor.py:62 的 PROBE_ROUTE_MODES。
//
// 只有这三种模式会发「最小请求」探测；images/embeddings 虽然也在 UPSTREAM_ROUTE_MODES
// 里，但探测它们需要形状完全不同的请求体，参照实现因此不做。
var probeRouteModes = []string{"openai", "anthropic", "responses"}

// upstreamRouteLabels 复刻 config.py:56 的 UPSTREAM_ROUTE_LABELS。
//
// config 包里有同名表但没有导出（internal/config/normalize.go:33），且本任务不允许
// 改动那个包，所以在这里按参照实现重建。**若 config 包将来导出它，应改成引用**，
// 否则两处会各自漂移。
var upstreamRouteLabels = map[string]string{
	"openai":     "OpenAI Chat",
	"anthropic":  "Anthropic Messages",
	"responses":  "OpenAI Responses",
	"images":     "OpenAI Images",
	"embeddings": "OpenAI Embeddings",
}

// upstreamRouteLabel 等价于 Python 的 `UPSTREAM_ROUTE_LABELS.get(mode, mode)`。
func upstreamRouteLabel(mode string) string {
	if label, ok := upstreamRouteLabels[mode]; ok {
		return label
	}
	return mode
}

// KeyProbeResult 对应 config_editor.py:70-81 的 KeyProbeResult。
type KeyProbeResult struct {
	ModelID    string
	KeyName    string
	Mode       string
	Label      string
	Path       string
	URL        string
	Available  bool
	StatusCode *int
	DurationMS int
	Error      string
}

// Clock 是探测用的时间源：墙钟写 checked_at，单调钟算 duration_ms，epoch 秒用于
// 端点能力缓存的过期换算。
//
// 参照实现直接用 datetime.now / time.monotonic / time.time；把三者做成接缝是为了让
// 语料能同时锁定 checked_at、duration_ms 与 expires_in_seconds（generator 侧把
// config_editor.datetime / monotonic / time 分别打桩成确定性值）。
type Clock interface {
	// Now 返回当前墙钟时间。
	Now() time.Time
	// MonotonicSeconds 返回单调递增秒数，等价于 time.monotonic()。
	MonotonicSeconds() float64
	// EpochSeconds 返回 epoch 秒，等价于 time.time()。
	EpochSeconds() float64
}

// realClock 是生产用时钟。
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) MonotonicSeconds() float64 { return tui.MonotonicSeconds() }

func (realClock) EpochSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Prober 是探测逻辑的宿主：把 HTTP 客户端与时钟做成接缝，其余全是纯逻辑。
//
// 用法（装配层直接取方法值喂 internal/api 的接缝）：
//
//	var prober configeditor.Prober
//	srv.ProbeKeyCapability = prober.ProbeKeyCapability
//	srv.ProbeProviderKeyCapabilities = prober.ProbeProviderKeyCapabilities
//	srv.ProbeKeyAvailability = prober.ProbeKeyAvailability
//
// 探测属于「读上游」，不需要配置写锁；api 侧已经在自己的写锁里调用它。
type Prober struct {
	// Client 是探测用的 HTTP 客户端。nil 表示使用内置客户端：不跟随重定向
	// （httpx 默认 follow_redirects=False，httpx/_client.py:197），并把 timeout
	// 折算成整请求超时。
	Client *http.Client
	// Clock 是时间源。nil 表示进程时钟。
	Clock Clock
}

// defaultProbeClient 返回与 httpx 默认行为对齐的客户端。
func defaultProbeClient() *http.Client {
	return &http.Client{
		// 参照实现从不跟随重定向：302 在 Python 里是 raise_for_status 的失败，
		// Go 默认客户端却会跟到终点并可能拿到 200，那会把「上游把探测请求重定向到
		// 登录页」误判成「端点可用」。ErrUseLastResponse 表示返回重定向响应本身。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (p Prober) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return defaultProbeClient()
}

func (p Prober) clock() Clock {
	if p.Clock != nil {
		return p.Clock
	}
	return realClock{}
}

// ProbePayloadForMode 对应 config_editor.py:1581 的 probe_payload_for_mode。
//
// OpenAI 与 Anthropic 共用 chat 形状（历史原因，参照实现如此），只有 responses
// 用 input / max_output_tokens。
func ProbePayloadForMode(mode string, modelID string) *canonical.Value {
	if mode == "responses" {
		return canonical.NewObjectOf(
			canonical.ObjectPair{Key: "model", Value: canonical.NewString(modelID)},
			canonical.ObjectPair{Key: "input", Value: canonical.NewString(".")},
			canonical.ObjectPair{Key: "max_output_tokens", Value: canonical.NewInt("1")},
		)
	}
	message := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "role", Value: canonical.NewString("user")},
		canonical.ObjectPair{Key: "content", Value: canonical.NewString(".")},
	)
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "model", Value: canonical.NewString(modelID)},
		canonical.ObjectPair{Key: "messages", Value: canonical.NewArray(message)},
		canonical.ObjectPair{Key: "max_tokens", Value: canonical.NewInt("1")},
	)
}

// ProbeErrorText 对应 config_editor.py:1591 的 _probe_error_text。
//
// 依次尝试 error.message / error.type / error 本身 / message，全部落空时回落到
// 原始响应体；任何分支都截断到 160 个**字符**（Python 的切片按字符）。
//
// 与参照实现的差异：httpx 的 response.text 会按 content-type 的 charset 解码并用
// U+FFFD 替换非法字节，Go 侧不做 charset 嗅探，直接按 UTF-8 截断。探测响应的错误
// 文本不是契约字段，且非法编码的上游响应不在语料覆盖范围内。
func ProbeErrorText(body []byte) string {
	data, err := canonical.Parse(body)
	if err != nil {
		return truncateRunes(string(body), 160)
	}
	if data.IsObject() {
		errorValue := data.Lookup("error")
		if errorValue.IsObject() {
			message := errorValue.Lookup("message")
			if !message.Truthy() {
				message = errorValue.Lookup("type")
			}
			if !message.Truthy() {
				message = errorValue
			}
			return truncateRunes(message.PyStr(), 160)
		}
		if errorValue.Truthy() {
			return truncateRunes(errorValue.PyStr(), 160)
		}
		if message := data.Lookup("message"); message.Truthy() {
			return truncateRunes(message.PyStr(), 160)
		}
	}
	return truncateRunes(string(body), 160)
}

// httpResult 是一次探测请求的结果。
type httpResult struct {
	status int
	body   []byte
}

// doRequest 发出一次探测请求。
//
// 对应 `httpx.Client(timeout=timeout).get/post(...)`。差异有三处：
//   - 超时用 context 表达（httpx 是连接/读/写/连接池各自超时）；
//   - Go 会自动补 User-Agent / Accept-Encoding，参照实现不会；
//   - Go 的 `client.Do` 把传输层错误包成 *url.Error（`Get "https://…": …`），
//     这里剥掉外层包装——httpx 的 RequestError 文本里没有 URL，不剥就无法与
//     「脚本化错误消息」对拍，也会让日志里多一层噪音。
//
// withAuth 为假时不发 Authorization 头：只有 `GET /health` 走这条路
// （config_editor.py:122 用的是裸 httpx.get）。
func (p Prober) doRequest(ctx context.Context, method, url, apiKey string, payload *canonical.Value, timeout float64, withAuth bool) (httpResult, error) {
	var body io.Reader
	if payload != nil {
		// 用 canonical 的有序紧凑序列化而不是 encoding/json：Go 的 map 会把键排序，
		// 上游看到的字段顺序就与参照实现（json.dumps 保持插入顺序）不同。探测请求体
		// 是纯逻辑产物，顺序可以逐字锁定，就不该放任它漂移。
		body = strings.NewReader(canonical.DumpsOrdered(payload))
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
		defer cancel()
	}
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return httpResult{}, err
	}
	if withAuth {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client().Do(request)
	if err != nil {
		var urlErr *neturl.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			err = urlErr.Err
		}
		return httpResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return httpResult{}, err
	}
	return httpResult{status: response.StatusCode, body: content}, nil
}

// DiscoverUpstreamModelsResult 对应 config_editor.py:1542 的
// discover_upstream_models_result：GET {base_url}/v1/models，返回排序后的模型 id
// 列表与错误文本（错误文本为空表示成功）。
//
// 参照实现的第三个参数 existing_model_ids 从未被使用，Go 侧直接去掉。
func (p Prober) DiscoverUpstreamModelsResult(ctx context.Context, baseURL, apiKey string, timeout float64) ([]string, string) {
	// 非 ASCII 的 key 无法作为 HTTP Header 发送：Python 在编码时就会抛
	// UnicodeEncodeError，这里提前判掉，错误文案照抄（config_editor.py:1552）。
	if !isASCII("Bearer " + apiKey) {
		return nil, "API Key 仅支持 ASCII 字符"
	}
	url := proxysupport.JoinURL(baseURL, "/v1/models")
	result, err := p.doRequest(ctx, http.MethodGet, url, apiKey, nil, timeout, true)
	if err != nil {
		return nil, truncateRunes("网络错误: "+err.Error(), 160)
	}
	// httpx 的 raise_for_status 对**一切**非 2xx（含 3xx）抛 HTTPStatusError。
	if result.status < 200 || result.status >= 300 {
		return nil, fmt.Sprintf("HTTP %d", result.status)
	}
	data, parseErr := canonical.Parse(result.body)
	if parseErr != nil {
		// Python 侧 exc 是 json.JSONDecodeError 的文本，Go 侧是 canonical 的解析
		// 错误文本——只锁定前缀与 160 字符上限（见 doc.go）。
		return nil, truncateRunes("JSON 解析失败: "+parseErr.Error(), 160)
	}
	if !data.IsObject() || !data.Obj.Has("data") {
		return nil, "响应 JSON 格式无效"
	}
	items := data.Lookup("data")
	if !items.IsArray() {
		return nil, "响应 JSON 格式无效"
	}
	modelIDs := make([]string, 0, len(items.Arr))
	for _, item := range items.Arr {
		// `str(item["id"])`：缺键（KeyError）、非对象（TypeError）都算格式无效。
		if item == nil || !item.IsObject() {
			return nil, "响应 JSON 格式无效"
		}
		id, ok := item.LookupOK("id")
		if !ok {
			return nil, "响应 JSON 格式无效"
		}
		modelIDs = append(modelIDs, id.PyStr())
	}
	sort.Strings(modelIDs)
	return modelIDs, ""
}

// RawUpstreamRoutesByURL 对应 config_editor.py:164 的 raw_upstream_routes_by_url。
func RawUpstreamRoutesByURL(data *canonical.Value) *canonical.Value {
	routes := data.Lookup("upstream_routes")
	if !routes.IsObject() {
		return canonical.NewObject()
	}
	return routes
}

// UpstreamRoutesForBaseURL 对应 config_editor.py:169 的
// upstream_routes_for_base_url：把「按 URL 分组的 upstream_routes」与「每个 Key 上
// 遗留的 upstream_routes」合并成 {模式: 路径}。
//
// 返回值是 canonical 对象而不是 map[string]string：参照实现把原始值（未规范化）
// 直接搬进来，且要保留合并顺序（已有键覆盖时不改变位置）。
//
// 与参照实现的差异（输入形状不对时的崩溃折算成「无此结构」）：
//   - data 的 models 不是数组、某个 model 的 keys 不是数组时，Python 会抛
//     AttributeError/TypeError，Go 侧跳过。
//   - upstream_routes 的 URL 键规范化失败（空白键）时两者都会失败，Go 返回错误。
func UpstreamRoutesForBaseURL(data *canonical.Value, baseURL string) (*canonical.Value, error) {
	normalized, err := config.NormalizeUpstreamBaseURL(canonical.NewString(baseURL))
	if err != nil {
		return nil, err
	}
	routes := canonical.NewObject()
	rawRoutes := RawUpstreamRoutesByURL(data)
	for _, rawBaseURL := range rawRoutes.Obj.Keys() {
		candidate, err := config.NormalizeUpstreamBaseURL(canonical.NewString(rawBaseURL))
		if err != nil {
			return nil, err
		}
		rawValue := rawRoutes.Lookup(rawBaseURL)
		if candidate == normalized && rawValue.IsObject() {
			mergeObject(routes, rawValue)
		}
	}
	models := data.Lookup("models")
	if !models.IsArray() {
		return routes, nil
	}
	for _, model := range models.Arr {
		if !model.IsObject() {
			continue
		}
		keys := model.Lookup("keys")
		if !keys.IsArray() {
			continue
		}
		for _, key := range keys.Arr {
			if !key.IsObject() {
				continue
			}
			raw := key.Lookup("base_url")
			if !raw.Truthy() {
				raw = data.Lookup("default_base_url")
			}
			if !raw.Truthy() {
				raw = canonical.NewString("https://api.openai.com")
			}
			keyBaseURL, err := config.NormalizeUpstreamBaseURL(raw)
			if err != nil {
				return nil, err
			}
			legacy := key.Lookup("upstream_routes")
			if keyBaseURL == normalized && legacy.IsObject() {
				mergeObject(routes, legacy)
			}
		}
	}
	return routes, nil
}

// mergeObject 等价于 Python 的 `target.update(source)`：新键追加、已有键只改值。
func mergeObject(target, source *canonical.Value) {
	if !target.IsObject() || !source.IsObject() {
		return
	}
	for _, key := range source.Obj.Keys() {
		child, _ := source.Obj.Get(key)
		target.SetKey(key, child)
	}
}

// upstreamRoutePath 等价于 config.py:231 的 upstream_route_path：
// `routes.get(mode) or UPSTREAM_ROUTE_DEFAULT_PATHS[mode]`。
//
// 用 canonical 的 Truthy 而不是比较空串：Python 的 `or` 把 0、false、空容器都算缺省。
func upstreamRoutePath(routes *canonical.Value, mode string) string {
	if routes.IsObject() {
		if value := routes.Lookup(mode); value.Truthy() {
			return value.PyStr()
		}
	}
	return config.UpstreamRouteDefaultPath(mode)
}

// probeKeyAvailability 对应 config_editor.py:1608 的 probe_key_availability，返回
// 完整字段（含 mode / status_code），供 probe_key_capability 组装 route_status。
func (p Prober) probeKeyAvailability(ctx context.Context, data *canonical.Value, modelID string, key *canonical.Value, timeout float64, modes []string) ([]KeyProbeResult, error) {
	keyName := key.Lookup("name").StringValue()
	if keyName == "" {
		keyName = modelID + "-key"
	}
	apiKey := key.Lookup("api_key").StringValue()
	rawBaseURL := key.Lookup("base_url")
	if !rawBaseURL.Truthy() {
		rawBaseURL = data.Lookup("default_base_url")
	}
	if !rawBaseURL.Truthy() {
		rawBaseURL = canonical.NewString("https://api.openai.com")
	}
	baseURL := rawBaseURL.PyStr()
	routes, err := UpstreamRoutesForBaseURL(data, baseURL)
	if err != nil {
		return nil, err
	}
	probeModes := filterModes(modes, probeRouteModes, probeRouteModes)

	// 非 ASCII 的 key 连请求都发不出去：所有模式都直接记一条失败结果。
	if !isASCII("Bearer " + apiKey) {
		const message = "API Key 包含非 ASCII 字符，无法作为 HTTP Header 发送"
		results := make([]KeyProbeResult, 0, len(probeModes))
		for _, mode := range probeModes {
			path := upstreamRoutePath(routes, mode)
			results = append(results, KeyProbeResult{
				ModelID:   modelID,
				KeyName:   keyName,
				Mode:      mode,
				Label:     upstreamRouteLabels[mode],
				Path:      path,
				URL:       proxysupport.JoinURL(baseURL, path),
				Available: false,
				Error:     message,
			})
		}
		return results, nil
	}

	results := make([]KeyProbeResult, 0, len(probeModes))
	for _, mode := range probeModes {
		path := upstreamRoutePath(routes, mode)
		url := proxysupport.JoinURL(baseURL, path)
		started := p.clock().MonotonicSeconds()
		var statusCode *int
		errorText := ""
		available := false
		result, requestErr := p.doRequest(ctx, http.MethodPost, url, apiKey, ProbePayloadForMode(mode, modelID), timeout, true)
		if requestErr != nil {
			errorText = truncateRunes(requestErr.Error(), 160)
		} else {
			status := result.status
			statusCode = &status
			available = status >= 200 && status < 300
			if !available {
				errorText = ProbeErrorText(result.body)
			}
		}
		durationMS := int((p.clock().MonotonicSeconds() - started) * 1000)
		results = append(results, KeyProbeResult{
			ModelID:    modelID,
			KeyName:    keyName,
			Mode:       mode,
			Label:      upstreamRouteLabels[mode],
			Path:       path,
			URL:        url,
			Available:  available,
			StatusCode: statusCode,
			DurationMS: durationMS,
			Error:      errorText,
		})
	}
	return results, nil
}

// ProbeKeyAvailabilityForData 是 probe_key_availability 的完整入参版本：data 是整份
// 配置字典（会用到 upstream_routes 与 models 里的遗留路由，见
// UpstreamRoutesForBaseURL），返回带 mode/status_code 的完整结果。
//
// 对拍与交互界面用它；喂 internal/api 接缝的是 ProbeKeyAvailability。
func (p Prober) ProbeKeyAvailabilityForData(ctx context.Context, data *canonical.Value, modelID string, key *canonical.Value, timeout float64, modes []string) ([]KeyProbeResult, error) {
	return p.probeKeyAvailability(ctx, data, modelID, key, timeout, modes)
}

// ProbeKeyAvailability 是喂给 api.Server.ProbeKeyAvailability 的导出入口。
//
// 参数 routes 是 **upstream_routes 的内容**（{base_url: {mode: path}}），因为
// api 侧的 runKeyProbe 就是这样构造的（internal/api/probes.go:180-182：
// `routesValue.SetKey(baseURL, routes)`）；参照实现传的是外面还包了一层
// `{"upstream_routes": ...}` 的 data，这里补回那一层再交给内部实现。
//
// 与参照实现的差别只有一处：`probe_key_availability` 的 modes 参数在这里固定为 nil
// （三种模式全测），因为对应的接缝没有这个参数——api 的两条路由也从来只做全测。
func (p Prober) ProbeKeyAvailability(ctx context.Context, routes *canonical.Value, model string, key *canonical.Value, timeout float64) ([]api.ProbeAvailability, error) {
	data := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "upstream_routes", Value: routes},
	)
	results, err := p.probeKeyAvailability(ctx, data, model, key, timeout, nil)
	if err != nil {
		return nil, err
	}
	out := make([]api.ProbeAvailability, 0, len(results))
	for _, result := range results {
		out = append(out, api.ProbeAvailability{
			Available:  result.Available,
			URL:        result.URL,
			DurationMS: int64(result.DurationMS),
			Error:      result.Error,
		})
	}
	return out, nil
}

// ProbeKeyCapability 对应 config_editor.py:465 的 probe_key_capability。
//
// 一个 Key 的探测包含两件事：/v1/models 发现可用模型，以及每个文本路由模式发一次
// 最小请求。结果按 Key 单独缓存（providers.<id>.keys.<key>.capabilities），
// **绝不跨 Key 合并**——同一供应商的免费 Key 与付费 Key 能看到的模型集常常不同
// （config_editor.py:472-481）。
//
// 返回对象的键顺序与参照实现一致：models / route_status / errors / checked_at。
func (p Prober) ProbeKeyCapability(provider *canonical.Value, keyName string, modes []string, timeout float64) (*canonical.Value, error) {
	// 接缝没有 ctx（与 internal/api 的字段签名一致），这里用 Background：探测本身
	// 由 timeout 参数限时。
	ctx := context.Background()
	baseURL := strings.TrimSpace(provider.Lookup("base_url").StringValue())
	keys, err := configops.ProviderKeys(provider)
	if err != nil {
		return nil, err
	}
	rawKey := keys.Lookup(keyName)
	errorsValue := canonical.NewObject()
	discovered := []string{}
	apiKey := ""
	if rawKey.IsObject() {
		apiKey = rawKey.Lookup("api_key").StringValue()
	}
	switch {
	case baseURL == "":
		errorsValue.SetKey("provider", canonical.NewString("缺少 Base URL"))
	case !rawKey.IsObject():
		errorsValue.SetKey(keyName, canonical.NewString("Key 不存在"))
	case apiKey == "":
		errorsValue.SetKey(keyName, canonical.NewString("API Key 为空"))
	default:
		models, discoverErr := p.DiscoverUpstreamModelsResult(ctx, baseURL, apiKey, timeout)
		if discoverErr != "" {
			errorsValue.SetKey(keyName, canonical.NewString(discoverErr))
		} else {
			discovered = models
		}
	}

	routeStatus := canonical.NewObject()
	probeModes := filterModes(modes, config.UpstreamRouteModes(), probeRouteModes)
	if apiKey != "" && baseURL != "" && errorsValue.Obj.Len() == 0 {
		probeKey := rawKey.Clone()
		probeKey.SetKey("name", canonical.NewString(keyName))
		probeKey.SetKey("base_url", canonical.NewString(baseURL))
		routes := provider.Lookup("routes")
		if !routes.IsObject() {
			routes = canonical.NewObject()
		}
		probeData := canonical.NewObjectOf(
			canonical.ObjectPair{Key: "upstream_routes", Value: canonical.NewObjectOf(
				canonical.ObjectPair{Key: baseURL, Value: routes},
			)},
		)
		modelForProbe := "probe-model"
		if len(discovered) > 0 {
			modelForProbe = discovered[0]
		}
		results, err := p.probeKeyAvailability(ctx, probeData, modelForProbe, probeKey, timeout, probeModes)
		if err != nil {
			return nil, err
		}
		allowed := map[string]bool{}
		for _, mode := range probeModes {
			allowed[mode] = true
		}
		for _, result := range results {
			if !allowed[result.Mode] {
				continue
			}
			// `f"failed: {result.error or result.status_code}"`：错误为空时退到
			// 状态码，状态码也没有（网络错误已在 error 里）时是 None。
			status := "ok"
			if !result.Available {
				if result.Error != "" {
					status = "failed: " + result.Error
				} else if result.StatusCode != nil {
					status = fmt.Sprintf("failed: %d", *result.StatusCode)
				} else {
					status = "failed: None"
				}
			}
			routeStatus.SetKey(result.Mode, canonical.NewString(status))
		}
	}

	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "models", Value: canonical.NewStringArray(discovered)},
		canonical.ObjectPair{Key: "route_status", Value: routeStatus},
		canonical.ObjectPair{Key: "errors", Value: errorsValue},
		canonical.ObjectPair{Key: "checked_at", Value: canonical.NewString(checkedAt(p.clock()))},
	), nil
}

// ProbeProviderKeyCapabilities 对应 config_editor.py:248 的
// probe_provider_key_capabilities：逐个 Key 发现模型，再求交集与并集。
//
// models 是**所有成功 Key 的模型交集**（表示「每个 Key 都能服务」的模型），
// all_models 是并集；失败的 Key 只记错误，不参与集合运算。
// 返回对象的键顺序与参照实现一致：models / all_models / key_models / errors。
func (p Prober) ProbeProviderKeyCapabilities(provider *canonical.Value, keyNames []string, timeout float64) (*canonical.Value, error) {
	ctx := context.Background()
	baseURL := strings.TrimSpace(provider.Lookup("base_url").StringValue())
	keys, err := configops.ProviderKeys(provider)
	if err != nil {
		return nil, err
	}
	keyModels := canonical.NewObject()
	errorsValue := canonical.NewObject()
	var successful []map[string]bool
	for _, keyName := range keyNames {
		key := keys.Lookup(keyName)
		apiKey := ""
		if key.IsObject() {
			apiKey = key.Lookup("api_key").StringValue()
		}
		var models []string
		var errorText string
		switch {
		case baseURL == "":
			errorText = "缺少 Base URL"
		case !key.IsObject():
			errorText = "Key 不存在"
		case apiKey == "":
			errorText = "API Key 为空"
		default:
			models, errorText = p.DiscoverUpstreamModelsResult(ctx, baseURL, apiKey, timeout)
		}
		keyModels.SetKey(keyName, canonical.NewStringArray(models))
		if errorText != "" {
			errorsValue.SetKey(keyName, canonical.NewString(errorText))
			continue
		}
		set := map[string]bool{}
		for _, model := range models {
			set[model] = true
		}
		successful = append(successful, set)
	}

	intersection := []string{}
	union := []string{}
	if len(successful) > 0 {
		var intersectionSet, unionSet map[string]bool
		for index, set := range successful {
			if index == 0 {
				intersectionSet = map[string]bool{}
				unionSet = map[string]bool{}
				for model := range set {
					intersectionSet[model] = true
					unionSet[model] = true
				}
				continue
			}
			for model := range intersectionSet {
				if !set[model] {
					delete(intersectionSet, model)
				}
			}
			for model := range set {
				unionSet[model] = true
			}
		}
		intersection = sortedKeys(intersectionSet)
		union = sortedKeys(unionSet)
	}
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "models", Value: canonical.NewStringArray(intersection)},
		canonical.ObjectPair{Key: "all_models", Value: canonical.NewStringArray(union)},
		canonical.ObjectPair{Key: "key_models", Value: keyModels},
		canonical.ObjectPair{Key: "errors", Value: errorsValue},
	), nil
}

// filterModes 复刻 Python 的 `[mode for mode in (modes or default) if mode in allowed]`。
//
// `modes or default` 是「空列表也取默认值」——显式传空切片等于不限制，这一点
// 与 internal/api 的 start_probe（`names = key_names or list(available)`）同源。
func filterModes(modes []string, allowed []string, fallback []string) []string {
	source := modes
	if len(source) == 0 {
		source = fallback
	}
	allowedSet := map[string]bool{}
	for _, mode := range allowed {
		allowedSet[mode] = true
	}
	out := make([]string, 0, len(source))
	for _, mode := range source {
		if allowedSet[mode] {
			out = append(out, mode)
		}
	}
	return out
}

// checkedAt 复刻 `datetime.now(timezone.utc).isoformat(timespec="seconds")`。
func checkedAt(clock Clock) string {
	return clock.Now().UTC().Format("2006-01-02T15:04:05") + "+00:00"
}

// sortedKeys 返回集合的码点序切片（等价于 Python 的 sorted(set))。
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// isASCII 报告字符串是否只含 ASCII（Python 的 `value.encode("ascii")` 是否会抛）。
func isASCII(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] > 0x7f {
			return false
		}
	}
	return true
}

// truncateRunes 按字符截断，与 Python 的 `value[:160]` 一致。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// errNotWired 是「接缝未接线」的统一错误。
//
// 借用 api 包对未接线接缝的措辞（internal/api/probes.go:151 的「探测功能未接入」），
// 保证装配层漏接线时得到的是明确失败，而不是一个看起来成功的空结果。
func errNotWired(feature string) error {
	return errors.New(feature + "未接入")
}
