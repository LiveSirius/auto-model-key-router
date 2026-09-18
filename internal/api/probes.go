package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 本文件移植 management_api.py:1211-1267 的探测（probe）相关逻辑，以及两条探测
// 路由共享的启动流程。
//
// Python 侧探测是「立即返回 202 + 后台 asyncio 任务」，进度放在
// app.state.management_probes 里。Go 侧用 goroutine + 同一张表复刻：POST 只登记
// 记录并启动任务，GET 读记录时看到的是当时的状态，因此状态推进的**时序**与 Python
// 一致（同样是异步竞态，调用方靠轮询收敛）。

// probeRecord 对应 app.state.management_probes[probe_id] 里的 record 字典。
type probeRecord struct {
	probeID         string
	status          string
	provider        string
	results         []*canonical.Value
	err             string
	cancelRequested bool
	cancel          context.CancelFunc
	done            chan struct{}
}

// probeIDHex 生成 32 位小写十六进制 id，等价于 `uuid.uuid4().hex`。
//
// 真实服务用密码学随机；语料回放时通过 Server.ProbeID 注入固定值，因为 Python 侧
// 的 uuid4 在生成语料时也被 monkeypatch 成同一个常量。
func probeIDHex() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// 取不到随机数属于系统级故障；退化成时间无关的常量会撞车，故直接失败关闭。
		panic("api: 无法生成探测 id: " + err.Error())
	}
	return hex.EncodeToString(buffer)
}

// startProbe 对应 management_api.py:652 的 start_probe。
//
// 注意 `names = key_names or list(available)` 的「or」语义：显式传空数组时探测该
// 供应商的**全部** Key，而不是一个都不探测。这是参照实现的原样行为，语料已固化。
func (s *Server) startProbe(r *http.Request, providerID string, keyNames []string, timeout float64) (*canonical.Value, error) {
	if _, err := s.authorizedConfig(r); err != nil {
		return nil, err
	}
	data, err := s.managementConfigData()
	if err != nil {
		return nil, err
	}
	provider, err := rawRequireProvider(data, providerID)
	if err != nil {
		return nil, err
	}
	available := provider.Lookup("keys")
	names := keyNames
	if len(names) == 0 {
		names = objectKeyOrder(available)
	}
	for _, name := range names {
		if !objectHasKey(available, name) {
			return nil, httpErrorf(404, "Key 不存在: %s", name)
		}
	}

	probeID := s.uuidHex()
	record := &probeRecord{
		probeID:  probeID,
		status:   "pending",
		provider: providerID,
		results:  []*canonical.Value{},
		done:     make(chan struct{}),
	}
	s.probesMu.Lock()
	if s.probes == nil {
		s.probes = map[string]*probeRecord{}
	}
	s.probes[probeID] = record
	s.probesMu.Unlock()

	secrets := make([]string, 0, len(names))
	for _, name := range names {
		key := available.Lookup(name)
		apiKey := ""
		if key != nil {
			apiKey = key.Lookup("api_key").StringValue()
		}
		secrets = append(secrets, apiKey)
	}
	providerSnapshot := provider.Clone()
	namesSnapshot := append([]string{}, names...)

	ctx, cancel := context.WithCancel(context.Background())
	record.cancel = cancel
	go s.runProbe(ctx, record, providerID, providerSnapshot, namesSnapshot, secrets, timeout)

	return objectOf(
		canonical.ObjectPair{Key: "probe_id", Value: canonical.NewString(probeID)},
		canonical.ObjectPair{Key: "status", Value: canonical.NewString("pending")},
	), nil
}

// runProbe 对应 start_probe 内部的 run() 协程。
func (s *Server) runProbe(ctx context.Context, record *probeRecord, providerID string, provider *canonical.Value, names, secrets []string, timeout float64) {
	defer close(record.done)
	s.probesMu.Lock()
	record.status = "running"
	s.probesMu.Unlock()

	rows, err := s.runKeyProbe(ctx, providerID, provider, names, timeout)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			s.probesMu.Lock()
			record.status = "cancelled"
			s.probesMu.Unlock()
			return
		}
		s.probesMu.Lock()
		record.status = "failed"
		record.err = redactProbeText(err.Error(), secrets)
		s.probesMu.Unlock()
		return
	}
	redacted := redactProbeData(rows, secrets)
	s.probesMu.Lock()
	record.results = redacted
	if record.cancelRequested {
		record.status = "cancelled"
	} else {
		record.status = "complete"
	}
	s.probesMu.Unlock()
}

// runKeyProbe 对应 management_api.py:1211 的 _run_key_probe。
//
// 依赖 config_editor 的两个探测函数，通过 Server 的接缝注入；未注入时返回错误，
// 由 runProbe 折叠成 status=failed + error 文本（这正是 Python 里探测抛异常时的
// 表现）。
func (s *Server) runKeyProbe(ctx context.Context, providerID string, provider *canonical.Value, keyNames []string, timeout float64) ([]*canonical.Value, error) {
	if s.ProbeProviderKeyCapabilities == nil || s.ProbeKeyAvailability == nil {
		return nil, errors.New("探测功能未接入")
	}
	capabilities, err := s.ProbeProviderKeyCapabilities(provider, keyNames, timeout)
	if err != nil {
		return nil, err
	}
	modelsByKey := capabilities.Lookup("key_models")
	baseURL := provider.Lookup("base_url").StringValue()
	routes := provider.Lookup("routes")
	if routes == nil || !routes.IsObject() {
		routes = canonical.NewObject()
	}

	rows := []*canonical.Value{}
	for _, keyName := range keyNames {
		key := provider.Lookup("keys").Lookup(keyName)
		if key == nil || !key.IsObject() {
			continue
		}
		// probe_key = {**key, "name": key_name, "base_url": base_url}
		probeKey := key.Clone()
		probeKey.SetKey("name", canonical.NewString(keyName))
		probeKey.SetKey("base_url", canonical.NewString(baseURL))

		models := modelList(modelsByKey, keyName)
		firstModel := ""
		if len(models) > 0 {
			firstModel = models[0]
		}
		routesValue := canonical.NewObject()
		routesValue.SetKey(baseURL, routes)
		results, err := s.ProbeKeyAvailability(ctx, routesValue, firstModel, probeKey, timeout)
		if err != nil {
			return nil, err
		}
		modelValues := make([]*canonical.Value, 0, len(models))
		for _, model := range models {
			modelValues = append(modelValues, canonical.NewString(model))
		}
		for _, result := range results {
			status := "failed"
			if result.Available {
				status = "ok"
			}
			rows = append(rows, objectOf(
				canonical.ObjectPair{Key: "status", Value: canonical.NewString(status)},
				canonical.ObjectPair{Key: "provider", Value: canonical.NewString(providerID)},
				canonical.ObjectPair{Key: "key", Value: canonical.NewString(keyName)},
				canonical.ObjectPair{Key: "endpoint", Value: canonical.NewString(result.URL)},
				canonical.ObjectPair{Key: "models", Value: canonical.NewArray(modelValues...)},
				canonical.ObjectPair{Key: "latency_ms", Value: canonical.NewIntValue(result.DurationMS)},
				canonical.ObjectPair{Key: "error", Value: nullableString(result.Error)},
			))
		}
	}
	return rows, nil
}

// modelList 返回某 Key 的模型列表，等价于 `list(models_by_key.get(name, []))`。
func modelList(modelsByKey *canonical.Value, keyName string) []string {
	value := modelsByKey.Lookup(keyName)
	if value == nil || !value.IsArray() {
		return nil
	}
	out := make([]string, 0, len(value.Arr))
	for _, item := range value.Arr {
		out = append(out, item.PyStr())
	}
	return out
}

// authorizationPattern 对应 _redact_probe_text 里的正则。
//
// Python 用的是 `(?i)authorization\s*[:=]\s*[^\s,;]+`。Go 的 regexp 支持
// `(?i)`，`\s`/`[^\s,;]` 的语义与 Python 一致（都是 Unicode 感知）。
var authorizationPattern = regexp.MustCompile(`(?i)authorization\s*[:=]\s*[^\s,;]+`)

// redactProbeText 对应 management_api.py:1248 的 _redact_probe_text。
//
// 先屏蔽 Authorization 头，再把已知密钥字面替换成 [redacted]，最后截断到 160 个
// **字符**（Python 的切片按字符，不是字节）。
func redactProbeText(value string, secrets []string) string {
	clean := authorizationPattern.ReplaceAllString(value, "Authorization: [redacted]")
	for _, secret := range secrets {
		if secret != "" {
			clean = strings.ReplaceAll(clean, secret, "[redacted]")
		}
	}
	return truncateRunes(clean, 160)
}

// redactProbeData 对应 management_api.py:1256 的 _redact_probe_data。
func redactProbeData(rows []*canonical.Value, secrets []string) []*canonical.Value {
	out := make([]*canonical.Value, 0, len(rows))
	for _, row := range rows {
		redacted := redactProbeText(row.Lookup("error").StringValue(), secrets)
		copied := canonical.NewObject()
		for _, key := range row.Obj.Keys() {
			child, _ := row.Obj.Get(key)
			if key == "error" {
				copied.SetKey(key, nullableString(redacted))
				continue
			}
			copied.SetKey(key, child)
		}
		out = append(out, copied)
	}
	return out
}

// truncateRunes 按字符截断。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// objectKeyOrder 返回对象成员的键顺序；非对象返回空切片。
func objectKeyOrder(value *canonical.Value) []string {
	if value == nil || !value.IsObject() {
		return nil
	}
	return value.Obj.Keys()
}

// objectHasKey 报告对象是否含某键。
func objectHasKey(value *canonical.Value, key string) bool {
	if value == nil || !value.IsObject() {
		return false
	}
	return value.Obj.Has(key)
}

// rawRequireProvider 对应 management_api.py:1509 的 _require_provider。
//
// 它抛的是 ManagementAPIError（顶层 -> 500），不是 HTTPException。这一点与
// operations.require_provider（ConfigOperationError -> 404）**不同**，不能合并。
func rawRequireProvider(data *canonical.Value, providerID string) (*canonical.Value, error) {
	providers := data.Lookup("providers")
	if providers == nil || !providers.IsObject() {
		return nil, &pyValueError{message: "providers 必须是对象"}
	}
	provider := providers.Lookup(providerID)
	if provider == nil || !provider.IsObject() {
		return nil, &managementAPIError{status: 404, message: "供应商不存在: " + providerID}
	}
	return provider, nil
}

// rawRequireKey 对应 management_api.py:1515 的 _require_key。
func rawRequireKey(provider *canonical.Value, name string) (*canonical.Value, error) {
	key := provider.Lookup("keys").Lookup(name)
	if key == nil || !key.IsObject() {
		return nil, &managementAPIError{status: 404, message: "Key 不存在: " + name}
	}
	return key, nil
}

// rawRequireRoute 对应 management_api.py:1521 的 _require_route。
func rawRequireRoute(routes *canonical.Value, name string) (*canonical.Value, error) {
	route := routes.Lookup(name)
	if route == nil || !route.IsObject() {
		return nil, &managementAPIError{status: 404, message: "路由不存在: " + name}
	}
	return route, nil
}

// rawRoutes 对应 management_api.py:1503 的 _routes（注意它取的是 models 段）。
//
// v4 配置里 models 是「id -> 路由对象」的字典，所以 /api/routes 家族操作的是
// data["models"]，而不是 RouterConfig.models 列表。
func rawRoutes(data *canonical.Value) (*canonical.Value, error) {
	routes := data.Lookup("models")
	if routes == nil || !routes.IsObject() {
		return nil, &pyValueError{message: "models 必须是对象"}
	}
	return routes, nil
}

// rawProviders 对应 management_api.py:1497 的 _providers。
func rawProviders(data *canonical.Value) (*canonical.Value, error) {
	providers := data.Lookup("providers")
	if providers == nil || !providers.IsObject() {
		return nil, &pyValueError{message: "providers 必须是对象"}
	}
	return providers, nil
}
