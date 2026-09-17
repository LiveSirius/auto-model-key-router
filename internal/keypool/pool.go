package keypool

import (
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// VisitorModelPrefix 是访客可见模型名的前缀（对齐 visitor.py:7）。
const VisitorModelPrefix = "amkr-"

// 错误分类与参照实现的异常一一对应，且**文本逐字一致**：
// Python 的 `KeyError(x)` 文本是 repr，即带引号的 `'x'`；RuntimeError 直接是消息。
// 用自定义类型而非 fmt.Errorf 包装，是为了让 Error() 只输出参照实现的文本，
// 同时保留 errors.Is 的可判定性（Go 惯例的 "sentinel: detail" 会污染文本）。
var (
	ErrNoUsableKey    = errors.New("没有可用 key")
	ErrUnknownModel   = errors.New("未配置的模型")
	ErrNoUnifiedModel = errors.New("未配置 unified_model")
)

// noUsableKeyError 对应 RuntimeError(f"模型 {model_id} 没有可用 key")。
type noUsableKeyError struct{ modelID string }

func (e *noUsableKeyError) Error() string { return "模型 " + e.modelID + " 没有可用 key" }
func (e *noUsableKeyError) Is(target error) bool {
	return target == ErrNoUsableKey
}

// unknownModelError 对应 KeyError(model_id)。
type unknownModelError struct{ modelID string }

func (e *unknownModelError) Error() string { return "'" + e.modelID + "'" }
func (e *unknownModelError) Is(target error) bool {
	return target == ErrUnknownModel
}

// missingKeyError 对应 RuntimeError(f"模型 {model_id} 未配置 key: {key_name}")。
type missingKeyError struct {
	modelID string
	keyName string
}

func (e *missingKeyError) Error() string {
	return "模型 " + e.modelID + " 未配置 key: " + e.keyName
}
func (e *missingKeyError) Is(target error) bool { return target == ErrNoUsableKey }

// noUnifiedModelError 对应 resolve_unified_plan 的 KeyError(UNIFIED_MODEL_ID)。
type noUnifiedModelError struct{}

func (e *noUnifiedModelError) Error() string { return "'" + config.UNIFIED_MODEL_ID + "'" }
func (e *noUnifiedModelError) Is(target error) bool {
	return target == ErrNoUnifiedModel
}

// KeyPool 负责按路由模式挑选 key，并管理并发计数、粘滞与冷却。
//
// 并发计数用 mutex 保护而非 Python 的 asyncio.Lock：那只锁保护的是字典更新的
// 原子性，Go 侧同样需要，但应该是普通互斥锁而非协程锁。
type KeyPool struct {
	mu sync.Mutex

	keys            map[string][]config.KeyConfig
	routingModes    map[string]string
	reasoningEffort map[string]string
	aliases         map[string]string
	hiddenNames     map[string]bool
	visitorRoutes   map[string]string

	failureThreshold int
	cooldownSeconds  float64

	unifiedDefault    *config.RoutePlan
	unifiedImage      *config.RoutePlan
	unifiedEmbeddings *config.RoutePlan
	taskPlans         map[string]config.RoutePlan
	taskParams        map[string]*canonical.Value

	cursors        map[string]int
	activeRequests map[[2]string]int
	stickyKeys     map[[3]string]string

	health       *KeyHealthStore
	capabilities *EndpointCapabilityCache

	// capabilityStore 为 nil 时不持久化（测试用）。
	capabilityStore *CapabilityStore
}

// stickyKey 是粘滞映射的键：(模型, 是否访客, 粘滞哈希)。
type stickyKey = [3]string

// applyConfig 装载配置。
//
// 字段划分与参照实现的 _apply_config 一一对应，含两处易错点：
//   - hidden_names 需排除已是真实 ID/别名的名字，再以 setdefault 语义补进别名表
//     （真实 ID/别名优先）；
//   - 访客路由前缀不与已有的 VISITOR_MODEL_PREFIX 名字重复添加。
func (p *KeyPool) applyConfig(cfg *config.RouterConfig) {
	p.keys = map[string][]config.KeyConfig{}
	p.routingModes = map[string]string{}
	p.reasoningEffort = map[string]string{}
	for _, model := range cfg.Models {
		p.keys[model.ID] = model.Keys
		p.routingModes[model.ID] = model.RoutingMode
		p.reasoningEffort[model.ID] = model.ReasoningEffort
	}
	p.failureThreshold = cfg.KeyFailureThreshold
	p.cooldownSeconds = cfg.KeyCooldownSeconds

	p.aliases = map[string]string{}
	for _, model := range cfg.Models {
		p.aliases[model.ID] = model.ID
		for _, alias := range model.Aliases {
			p.aliases[alias] = model.ID
		}
	}

	hiddenNames := cfg.HiddenModelNames()
	p.hiddenNames = map[string]bool{}
	for name := range hiddenNames {
		if _, isAlias := p.aliases[name]; !isAlias {
			p.hiddenNames[name] = true
		}
	}
	for name, modelID := range hiddenNames {
		if _, exists := p.aliases[name]; !exists {
			p.aliases[name] = modelID
		}
	}

	p.visitorRoutes = map[string]string{}
	for modelID := range p.keys {
		if !strings.HasPrefix(modelID, VisitorModelPrefix) {
			p.visitorRoutes[VisitorModelPrefix+modelID] = modelID
		}
	}
	for modelID := range p.keys {
		if strings.HasPrefix(modelID, VisitorModelPrefix) {
			p.visitorRoutes[modelID] = modelID
		}
	}

	p.unifiedDefault = p.canonicalPlan(cfg.UnifiedModel, func(u *config.UnifiedModelConfig) *config.RoutePlan {
		return &u.Default
	})
	p.unifiedImage = p.canonicalPlan(cfg.UnifiedModel, func(u *config.UnifiedModelConfig) *config.RoutePlan {
		return u.Image
	})
	p.unifiedEmbeddings = p.canonicalPlan(cfg.UnifiedModel, func(u *config.UnifiedModelConfig) *config.RoutePlan {
		return u.Embeddings
	})

	p.taskPlans = map[string]config.RoutePlan{}
	p.taskParams = map[string]*canonical.Value{}
	for _, task := range cfg.Tasks {
		p.taskPlans[task.Name] = p.canonicalizePlan(task.Plan())
		p.taskParams[task.Name] = task.Params
	}
}

// canonicalPlan 取 unified_model 的某个计划并做别名归一；unified_model 为空或
// 该计划缺失时返回 nil。
func (p *KeyPool) canonicalPlan(
	unified *config.UnifiedModelConfig,
	pick func(*config.UnifiedModelConfig) *config.RoutePlan,
) *config.RoutePlan {
	if unified == nil {
		return nil
	}
	plan := pick(unified)
	if plan == nil {
		return nil
	}
	canonicalized := p.canonicalizePlan(*plan)
	return &canonicalized
}

// canonicalizePlan 把计划里的模型名（可能是别名）归一成真实 ID。
func (p *KeyPool) canonicalizePlan(plan config.RoutePlan) config.RoutePlan {
	result := config.RoutePlan{Primary: p.canonicalTarget(plan.Primary)}
	if plan.Fallback != nil {
		fallback := p.canonicalTarget(*plan.Fallback)
		result.Fallback = &fallback
	}
	return result
}

// canonicalTarget 归一单个路由目标。
func (p *KeyPool) canonicalTarget(target config.RouteTarget) config.RouteTarget {
	return config.RouteTarget{Model: p.resolveModelID(target.Model), Key: target.Key}
}

// New 按配置构造 KeyPool。capabilityStore 可为 nil（不持久化能力缓存）。
func New(cfg *config.RouterConfig, capabilityStore *CapabilityStore, clock func() float64) *KeyPool {
	if clock == nil {
		clock = RealClock
	}
	pool := &KeyPool{
		cursors:         map[string]int{},
		activeRequests:  map[[2]string]int{},
		stickyKeys:      map[stickyKey]string{},
		health:          NewKeyHealthStore(clock),
		capabilityStore: capabilityStore,
	}
	var rawStates *canonical.Value
	if capabilityStore != nil {
		rawStates = capabilityStore.Load()
	}
	pool.capabilities = NewEndpointCapabilityCache(rawStates, clock)
	pool.applyConfig(cfg)
	return pool
}

// ApplyConfig 热重载配置，保留游标、并发计数、粘滞与健康状态。
//
// 对齐参照实现的行为：只替换配置派生的字段（_apply_config），其余状态沿用。
func (p *KeyPool) ApplyConfig(cfg *config.RouterConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applyConfig(cfg)
}

// ModelIDs 返回所有模型真实 ID（排序）。
func (p *KeyPool) ModelIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return sortedMapKeys(p.keys)
}

// HiddenModelIDs 返回可直接调用但不在 /v1/models 中列出的名字（排序）。
func (p *KeyPool) HiddenModelIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]string, 0, len(p.hiddenNames))
	for name := range p.hiddenNames {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

// PublicModelIDs 返回对外可见的模型名（排序）。
func (p *KeyPool) PublicModelIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for name := range p.aliases {
		if !p.hiddenNames[name] {
			seen[name] = true
		}
	}
	return sortedSet(seen)
}

// AvailableModelIDs 返回当前确实有可用 key 的对外模型名（排序）。
//
// visitorOnly 时只考虑允许访客的 key，且以访客前缀名为准。
func (p *KeyPool) AvailableModelIDs(visitorOnly bool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if visitorOnly {
		result := []string{}
		for publicID, modelID := range p.visitorRoutes {
			if len(p.keysForModelLocked(modelID, true)) > 0 {
				result = append(result, publicID)
			}
		}
		slices.Sort(result)
		return result
	}
	result := []string{}
	for name, modelID := range p.aliases {
		if !p.hiddenNames[name] && len(p.keysForModelLocked(modelID, false)) > 0 {
			result = append(result, name)
		}
	}
	slices.Sort(result)
	if p.unifiedDefault != nil {
		// 追加到末尾（参照实现用 append，不参与排序）。
		result = append(result, config.UNIFIED_MODEL_ID)
	}
	return result
}

// ResolveModelID 把模型名（ID 或别名）解析成真实 ID；未知名原样返回。
func (p *KeyPool) ResolveModelID(modelID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolveModelID(modelID)
}

// resolveModelID 是无锁版本，调用方须持锁。
func (p *KeyPool) resolveModelID(modelID string) string {
	if resolved, found := p.aliases[modelID]; found {
		return resolved
	}
	return modelID
}

// ResolveVisitorModelID 把访客可见模型名解析成真实 ID。
func (p *KeyPool) ResolveVisitorModelID(publicModelID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	modelID, found := p.visitorRoutes[publicModelID]
	return modelID, found
}

// ErrNoUnifiedModel 由上面的 noUnifiedModelError 承载，见文件顶部定义。

// ResolveRoute 返回 (真实模型 ID, key 名)。
//
// 对齐 key_pool.py:115：unified-model 按路径类型挑计划，任务名用任务计划且
// **不接受调用方指定的 key**，其余走别名解析。
//
// 入参 keyName 用 *string：Python 区分 `None`（未指定）与 `""`（由
// `model[key]` 语法显式指定的空白 key，proxy_support.py:77 的 strip() 也是来源），
// 前者保留计划原值、后者替换成空串。
//
// 返回的 key 用 string，"" 表示「无 key」。Go 侧把 None 与 "" 统一成 ""：
// 下游四处使用（proxy_handler.py:111/232/391/424）全部是真值判断，两者行为
// 完全相同，因此这里不做区分；对拍测试对 key 位置做 null≡"" 归一后比较。
func (p *KeyPool) ResolveRoute(modelID string, keyName *string, path string) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if modelID == config.UNIFIED_MODEL_ID {
		plan, err := p.resolveUnifiedPlanLocked(RequestRouteKind(path), keyName)
		if err != nil {
			return "", "", err
		}
		return plan.Primary.Model, plan.Primary.Key, nil
	}
	if plan, found := p.taskPlans[modelID]; found {
		// 任务固定模型，不接受调用方指定的 Key。
		return plan.Primary.Model, plan.Primary.Key, nil
	}
	if keyName == nil {
		return p.resolveModelID(modelID), "", nil
	}
	return p.resolveModelID(modelID), *keyName, nil
}

// TaskPlan 返回任务名的路由计划。
func (p *KeyPool) TaskPlan(taskName string) (config.RoutePlan, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	plan, found := p.taskPlans[taskName]
	return plan, found
}

// TaskParams 返回任务的固定参数（副本）。
func (p *KeyPool) TaskParams(taskName string) *canonical.Value {
	p.mu.Lock()
	defer p.mu.Unlock()
	params, found := p.taskParams[taskName]
	if !found || params == nil {
		return canonical.NewObject()
	}
	return params.Clone()
}

// ResolveUnifiedPlan 按路径类型挑选 unified 计划。
func (p *KeyPool) ResolveUnifiedPlan(routeKind string, keyName *string) (config.RoutePlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolveUnifiedPlanLocked(routeKind, keyName)
}

// resolveUnifiedPlanLocked 是无锁版本，调用方须持锁。
//
// 指定的 keyName 会**替换** primary 的 key，但 fallback 原样保留（包括它自己的
// key）——这是参照实现的行为（key_pool.py:145）。keyName 为 nil（未指定）时
// 保留计划原值；非 nil 时空串也会替换（`unified-model[ ]` 可产出空串，
// proxy_support.py:77 的 strip() 是来源）。
func (p *KeyPool) resolveUnifiedPlanLocked(routeKind string, keyName *string) (config.RoutePlan, error) {
	var plan *config.RoutePlan
	switch routeKind {
	case "image":
		plan = p.unifiedImage
	case "embeddings":
		plan = p.unifiedEmbeddings
	}
	if plan == nil {
		plan = p.unifiedDefault
	}
	// 计划缺失时参照实现抛 KeyError(UNIFIED_MODEL_ID)，其文本是 repr（带引号）。
	if plan == nil {
		return config.RoutePlan{}, &noUnifiedModelError{}
	}
	if keyName == nil {
		return *plan, nil
	}
	return config.RoutePlan{
		Primary:  config.RouteTarget{Model: plan.Primary.Model, Key: *keyName},
		Fallback: plan.Fallback,
	}, nil
}

// UnifiedRoute 返回 unified 路由的展示结构；未启用 unified_model 时返回 nil。
func (p *KeyPool) UnifiedRoute() *canonical.Value {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unifiedDefault == nil {
		return nil
	}
	serialize := func(plan *config.RoutePlan) *canonical.Value {
		pairs := []canonical.ObjectPair{
			{Key: "model", Value: canonical.NewString(plan.Primary.Model)},
			{Key: "key", Value: nullable(plan.Primary.Key)},
		}
		result := canonical.NewObjectOf(canonical.ObjectPair{
			Key: "primary", Value: canonical.NewObjectOf(pairs...),
		})
		if plan.Fallback != nil {
			result.SetKey("fallback", canonical.NewObjectOf(
				canonical.ObjectPair{Key: "model", Value: canonical.NewString(plan.Fallback.Model)},
				canonical.ObjectPair{Key: "key", Value: nullable(plan.Fallback.Key)},
			))
		}
		return result
	}
	result := canonical.NewObject()
	result.SetKey("default", serialize(p.unifiedDefault))
	if p.unifiedImage != nil {
		result.SetKey("image", serialize(p.unifiedImage))
	}
	if p.unifiedEmbeddings != nil {
		result.SetKey("embeddings", serialize(p.unifiedEmbeddings))
	}
	return result
}

// KeyCount 返回模型启用的 key 数。
func (p *KeyPool) KeyCount(modelID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keysForModelLocked(p.resolveModelID(modelID), false))
}

// VisitorKeyCount 返回模型允许访客的启用 key 数。
func (p *KeyPool) VisitorKeyCount(modelID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keysForModelLocked(p.resolveModelID(modelID), true))
}

// RoutingMode 返回模型的路由模式（缺省 round_robin）。
func (p *KeyPool) RoutingMode(modelID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	mode, found := p.routingModes[p.resolveModelID(modelID)]
	if !found {
		return "round_robin"
	}
	return mode
}

// ReasoningEffort 返回模型配置的 reasoning_effort。
func (p *KeyPool) ReasoningEffort(modelID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reasoningEffort[p.resolveModelID(modelID)]
}

// KeysForModel 返回模型启用的 key。
func (p *KeyPool) KeysForModel(modelID string, visitorOnly bool) []config.KeyConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keysForModelLocked(p.resolveModelID(modelID), visitorOnly)
}

// keysForModelLocked 是无锁版本，返回副本切片。
func (p *KeyPool) keysForModelLocked(modelID string, visitorOnly bool) []config.KeyConfig {
	all := p.keys[modelID]
	result := make([]config.KeyConfig, 0, len(all))
	for _, key := range all {
		if key.Enabled && (!visitorOnly || key.AllowVisitor) {
			result = append(result, key)
		}
	}
	return result
}

// KeyByName 按名字取启用中的 key。
func (p *KeyPool) KeyByName(modelID, keyName string, visitorOnly bool) (config.KeyConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	modelID = p.resolveModelID(modelID)
	keys, found := p.keys[modelID]
	if !found || len(keys) == 0 {
		return config.KeyConfig{}, &unknownModelError{modelID: modelID}
	}
	for _, key := range keys {
		if key.Name == keyName && key.Enabled && (!visitorOnly || key.AllowVisitor) {
			return key, nil
		}
	}
	return config.KeyConfig{}, &missingKeyError{modelID: modelID, keyName: keyName}
}

// NextKey 按路由模式挑选下一个可用 key。
//
// 四种模式的行为差异（对齐 key_pool.py:214）：
//   - only_first：只用第一个；被排除即失败，**不进入冷却判断**；
//   - priority：按配置顺序取第一个可用项，受冷却排除影响；
//   - round_robin：在并发数最低的候选里按游标轮转，支持粘滞；
//
// 冷却过滤是**软**的：可用集合为空时会回退到「仅排除 excluded」的集合，因此
// 冷却中的 key 仍可能被选中。这是刻意的降级策略，避免全部冷却时彻底不可用。
func (p *KeyPool) NextKey(modelID string, excluded []string, visitorOnly bool, affinityKey string) (config.KeyConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	modelID = p.resolveModelID(modelID)
	excludedSet := map[string]bool{}
	for _, name := range excluded {
		excludedSet[name] = true
	}
	keys := p.keysForModelLocked(modelID, visitorOnly)
	if len(keys) == 0 {
		if len(p.keys[modelID]) > 0 {
			return config.KeyConfig{}, &noUsableKeyError{modelID: modelID}
		}
		return config.KeyConfig{}, &unknownModelError{modelID: modelID}
	}

	if p.routingModeLocked(modelID) == "only_first" {
		first := keys[0]
		if !excludedSet[first.Name] {
			p.activeRequests[[2]string{modelID, first.Name}]++
			return first, nil
		}
		return config.KeyConfig{}, &noUsableKeyError{modelID: modelID}
	}

	available := make([]config.KeyConfig, 0, len(keys))
	for _, key := range keys {
		if !excludedSet[key.Name] && !p.health.IsCoolingDown(modelID, key.Name) {
			available = append(available, key)
		}
	}
	if len(available) == 0 {
		for _, key := range keys {
			if !excludedSet[key.Name] {
				available = append(available, key)
			}
		}
	}
	if len(available) == 0 {
		return config.KeyConfig{}, &noUsableKeyError{modelID: modelID}
	}

	if p.routingModeLocked(modelID) == "priority" {
		// 按 keys 的配置顺序（不是 available 的顺序）取第一个可用项。
		availableSet := map[string]bool{}
		for _, key := range available {
			availableSet[key.Name] = true
		}
		for _, candidate := range keys {
			if availableSet[candidate.Name] {
				p.activeRequests[[2]string{modelID, candidate.Name}]++
				return candidate, nil
			}
		}
		return config.KeyConfig{}, &noUsableKeyError{modelID: modelID}
	}

	var sticky stickyKey
	hasSticky := false
	if affinityKey != "" && p.routingModeLocked(modelID) == "round_robin" {
		sticky = stickyKey{modelID, boolString(visitorOnly), affinityKey}
		hasSticky = true

		if stickyName, found := p.stickyKeys[sticky]; found {
			for _, candidate := range available {
				if candidate.Name == stickyName {
					p.activeRequests[[2]string{modelID, candidate.Name}]++
					return candidate, nil
				}
			}
			delete(p.stickyKeys, sticky)
		}
	}

	lowestActive := -1
	for _, key := range available {
		active := p.activeRequests[[2]string{modelID, key.Name}]
		if lowestActive < 0 || active < lowestActive {
			lowestActive = active
		}
	}
	// 游标走 len(keys) 步，因此最多遍历一整轮；只有并发数恰好最低的候选被选中。
	for range len(keys) {
		cursor := p.cursors[modelID] % len(keys)
		p.cursors[modelID]++
		candidate := keys[cursor]
		if isInAvailable(available, candidate.Name) &&
			p.activeRequests[[2]string{modelID, candidate.Name}] == lowestActive {
			p.activeRequests[[2]string{modelID, candidate.Name}]++
			if hasSticky {
				p.stickyKeys[sticky] = candidate.Name
			}
			return candidate, nil
		}
	}
	return config.KeyConfig{}, &noUsableKeyError{modelID: modelID}
}

// routingModeLocked 是无锁版本。
func (p *KeyPool) routingModeLocked(modelID string) string {
	mode, found := p.routingModes[modelID]
	if !found {
		return "round_robin"
	}
	return mode
}

// AcquireKey 增加某 key 的在途计数。
func (p *KeyPool) AcquireKey(modelID, keyName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activeRequests[[2]string{p.resolveModelID(modelID), keyName}]++
}

// ReleaseKey 减少某 key 的在途计数；归零时删除条目。
//
// 删除而非留 0 是为了让 activeRequests 不随访问过的 key 无限增长（与 Python 的
// pop 语义一致）。
func (p *KeyPool) ReleaseKey(modelID, keyName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	activeKey := [2]string{p.resolveModelID(modelID), keyName}
	count := p.activeRequests[activeKey]
	if count <= 1 {
		delete(p.activeRequests, activeKey)
		return
	}
	p.activeRequests[activeKey] = count - 1
}

// ActiveCount 返回某 key 的当前在途请求数（供测试与观测）。
func (p *KeyPool) ActiveCount(modelID, keyName string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeRequests[[2]string{p.resolveModelID(modelID), keyName}]
}

// MarkSuccess 记录成功。
func (p *KeyPool) MarkSuccess(modelID, keyName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health.MarkSuccess(p.resolveModelID(modelID), keyName)
}

// MarkFailure 记录失败。
func (p *KeyPool) MarkFailure(modelID, keyName string, statusCode *int, retryAfter *float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health.MarkFailure(
		p.resolveModelID(modelID), keyName,
		statusCode, retryAfter, p.failureThreshold, p.cooldownSeconds,
	)
}

// IsCoolingDown 报告某 key 是否在冷却中。
func (p *KeyPool) IsCoolingDown(modelID, keyName string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.health.IsCoolingDown(p.resolveModelID(modelID), keyName)
}

// EndpointCapabilityStates 返回所有 URL 的探测状态（供管理 API）。
func (p *KeyPool) EndpointCapabilityStates() *canonical.Value {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capabilities.Payloads()
}

// SupportsNativeEndpoint 返回 nil=未测试、true/false=已测试。
func (p *KeyPool) SupportsNativeEndpoint(baseURL, routePath string) *bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capabilities.Get(baseURL, routePath)
}

// UpdateNativeEndpoint 记录探测结果并尝试持久化。
//
// 持久化失败不影响内存状态：能力缓存是缓存而非数据源，写不进去不该让请求失败。
// 参照实现同样吞掉写入错误（endpoint_capability_store.py:42）。
func (p *KeyPool) UpdateNativeEndpoint(baseURL string, supported bool, routePath, reason string) error {
	p.mu.Lock()
	p.capabilities.Update(baseURL, supported, routePath, reason)
	persisted := p.capabilities.Persisted()
	store := p.capabilityStore
	p.mu.Unlock()

	if store == nil {
		return nil
	}
	return store.Save(persisted)
}

// RequestRouteKind 把代理路径归类为 unified 路由类型。
//
// 对齐 proxy_support.py:252——AMKR 只此一份路径分类，KeyPool 与 proxy_handler
// 共用它。新增一类独立目标只需改这里。
func RequestRouteKind(path string) string {
	switch path {
	case "images/generations", "images/edits":
		return "image"
	case "embeddings":
		return "embeddings"
	}
	return "default"
}

// isInAvailable 报告 available 中是否存在该名字的 key。
//
// available 元素来自 keysForModelLocked，是 KeyConfig 值副本，因此按名字比较
// （不能比较结构体：KeyConfig 含 map 字段，不可比较）。
func isInAvailable(available []config.KeyConfig, name string) bool {
	for _, key := range available {
		if key.Name == name {
			return true
		}
	}
	return false
}

// nullable 把空字符串表示为 JSON null。
func nullable(value string) *canonical.Value {
	if value == "" {
		return canonical.NewNull()
	}
	return canonical.NewString(value)
}

// boolString 把布尔渲染成 Python 风格的字符串键片段（True/False）。
//
// 粘滞键包含 visitor_only，参照实现直接用它作为元组元素；Go 需要可比较的键，
// 故转成字符串。取值本身不影响行为，只要求同一布尔稳定映射到同一字符串。
func boolString(value bool) string {
	if value {
		return "True"
	}
	return "False"
}

// sortedMapKeys 返回 map 的排序键。
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// sortedSet 返回集合的排序切片。
func sortedSet(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
