package keypool

import (
	"slices"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 负结果与错误结果的缓存时长（对齐 endpoint_capabilities.py:9）。
const (
	NativeNegativeTTLSeconds = 600
	NativeErrorTTLSeconds    = 60
)

// EndpointCapability 是某个 base_url + 路由路径的原生端点探测结果。
type EndpointCapability struct {
	Supported  bool
	CheckedAt  float64
	Reason     string
	TTLSeconds int
}

// EndpointCapabilityCache 按 URL 缓存原生端点支持情况。
type EndpointCapabilityCache struct {
	clock  func() float64
	states map[string]EndpointCapability
}

// NewEndpointCapabilityCache 从持久化状态构造缓存。
//
// rawStates 的键是 "<base_url>|<route>"，值可以是新版对象，也可以是旧版的布尔
// 值（遗留格式）；无法识别的条目被忽略——这与参照实现一致，坏数据不该让整个
// 缓存失效。
func NewEndpointCapabilityCache(rawStates *canonical.Value, clock func() float64) *EndpointCapabilityCache {
	if clock == nil {
		clock = RealClock
	}
	cache := &EndpointCapabilityCache{clock: clock, states: map[string]EndpointCapability{}}
	if !rawStates.IsObject() {
		return cache
	}
	for _, key := range rawStates.Obj.Keys() {
		state, ok := capabilityFromRaw(rawStates.Lookup(key), clock)
		if ok {
			cache.states[key] = state
		}
	}
	return cache
}

// Get 返回缓存的支持情况；nil 表示未测试或已过期。
//
// 兼容一条历史键格式：查询 v1/messages 时若精确键缺失，会退回到只按 base_url
// 记录的键（endpoint_capabilities.py:35）。
func (c *EndpointCapabilityCache) Get(baseURL, routePath string) *bool {
	key := capabilityKey(baseURL, routePath)
	state, found := c.states[key]
	if !found && strings.Trim(routePath, "/") == "v1/messages" {
		state, found = c.states[strings.TrimRight(baseURL, "/")]
	}
	if !found {
		return nil
	}
	if state.TTLSeconds > 0 && c.clock()-state.CheckedAt >= float64(state.TTLSeconds) {
		return nil
	}
	result := state.Supported
	return &result
}

// Update 记录一次探测结果。
//
// 正结果永久有效（ttl 0），负结果按原因给 600s 或 60s。这是刻意的：把失败结果
// 也永久缓存会让一次临时故障永久禁用某个上游端点。
func (c *EndpointCapabilityCache) Update(baseURL string, supported bool, routePath, reason string) {
	ttl := 0
	if !supported {
		ttl = negativeTTL(reason)
	}
	c.states[capabilityKey(baseURL, routePath)] = EndpointCapability{
		Supported:  supported,
		CheckedAt:  c.clock(),
		Reason:     reason,
		TTLSeconds: ttl,
	}
}

// Payloads 返回带过期信息的完整状态，供管理 API 展示。
func (c *EndpointCapabilityCache) Payloads() *canonical.Value {
	now := c.clock()
	result := canonical.NewObject()
	for _, key := range sortedStateKeys(c.states) {
		state := c.states[key]
		var expiresAt *canonical.Value = canonical.NewNull()
		var expiresIn *canonical.Value = canonical.NewNull()
		if state.TTLSeconds > 0 {
			expires := state.CheckedAt + float64(state.TTLSeconds)
			expiresAt = canonical.NewFloat(expires)
			// int() 向零截断；max(0, ...) 保证非负。
			remaining := int(expires - now)
			if remaining < 0 {
				remaining = 0
			}
			expiresIn = canonical.NewIntValue(int64(remaining))
		}
		result.SetKey(key, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "supported", Value: canonical.NewBool(state.Supported)},
			canonical.ObjectPair{Key: "checked_at", Value: canonical.NewFloat(state.CheckedAt)},
			canonical.ObjectPair{Key: "reason", Value: canonical.NewString(state.Reason)},
			canonical.ObjectPair{Key: "ttl_seconds", Value: canonical.NewIntValue(int64(state.TTLSeconds))},
			canonical.ObjectPair{Key: "expires_at", Value: expiresAt},
			canonical.ObjectPair{Key: "expires_in_seconds", Value: expiresIn},
		))
	}
	return result
}

// Persisted 返回用于持久化的精简状态（不含派生字段）。
func (c *EndpointCapabilityCache) Persisted() *canonical.Value {
	result := canonical.NewObject()
	for _, key := range sortedStateKeys(c.states) {
		state := c.states[key]
		result.SetKey(key, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "supported", Value: canonical.NewBool(state.Supported)},
			canonical.ObjectPair{Key: "checked_at", Value: canonical.NewFloat(state.CheckedAt)},
			canonical.ObjectPair{Key: "reason", Value: canonical.NewString(state.Reason)},
			canonical.ObjectPair{Key: "ttl_seconds", Value: canonical.NewIntValue(int64(state.TTLSeconds))},
		))
	}
	return result
}

// capabilityKey 拼出缓存键，路径统一去首尾斜杠，空路径退化为 v1/messages。
func capabilityKey(baseURL, routePath string) string {
	route := strings.Trim(routePath, "/")
	if route == "" {
		route = "v1/messages"
	}
	return strings.TrimRight(baseURL, "/") + "|" + route
}

// negativeTTL 按失败原因选择负结果缓存时长。
func negativeTTL(reason string) int {
	if reason == "error" {
		return NativeErrorTTLSeconds
	}
	return NativeNegativeTTLSeconds
}

// capabilityFromRaw 解析单条持久化状态。
func capabilityFromRaw(raw *canonical.Value, clock func() float64) (EndpointCapability, bool) {
	// 遗留格式：直接是布尔值。
	if raw.IsBool() {
		supported, _ := raw.AsBool()
		checkedAt := 0.0
		ttl := NativeNegativeTTLSeconds
		if supported {
			checkedAt = clock()
			ttl = 0
		}
		return EndpointCapability{
			Supported:  supported,
			CheckedAt:  checkedAt,
			Reason:     "legacy",
			TTLSeconds: ttl,
		}, true
	}
	if !raw.IsObject() {
		return EndpointCapability{}, false
	}
	rawSupported := raw.Lookup("supported")
	if !rawSupported.IsBool() {
		return EndpointCapability{}, false
	}
	supported, _ := rawSupported.AsBool()

	reason := "ok"
	if rawReason := raw.Lookup("reason"); rawReason.Truthy() {
		reason = rawReason.PyStr()
	}
	ttl := 0
	if rawTTL := raw.Lookup("ttl_seconds"); rawTTL.Truthy() {
		if parsed, ok := rawTTL.AsInt(); ok {
			ttl = int(parsed)
		}
	}
	if !supported && ttl <= 0 {
		ttl = negativeTTL(reason)
	}
	checkedAt := clock()
	if rawCheckedAt := raw.Lookup("checked_at"); rawCheckedAt.Truthy() {
		if parsed, ok := rawCheckedAt.AsFloat(); ok {
			checkedAt = parsed
		}
	}
	return EndpointCapability{
		Supported:  supported,
		CheckedAt:  checkedAt,
		Reason:     reason,
		TTLSeconds: ttl,
	}, true
}

// sortedStateKeys 返回排序后的状态键，保证输出稳定（Python 侧靠 json 排序或
// dict 插入序，Go 的 map 顺序随机，必须显式排序）。
func sortedStateKeys(states map[string]EndpointCapability) []string {
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	// 键是 "<url>|<route>"，按 Unicode 码点排序与 Python 的 sorted() 一致。
	slices.Sort(keys)
	return keys
}
