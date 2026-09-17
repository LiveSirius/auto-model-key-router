package config

import (
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// KeyConfig 是模型下的一个可用 key（由 provider key + target 组合而成）。
type KeyConfig struct {
	Name string
	// APIKey 是真实密钥。
	APIKey       string
	BaseURL      string
	Enabled      bool
	AllowVisitor bool
	// UpstreamRoutes 在 v4 里恒为空：见 RouterConfig 的说明。
	UpstreamRoutes map[string]string
	Provider       string
	UpstreamModel  string
}

// ModelConfig 是一个可路由的模型。
type ModelConfig struct {
	ID              string
	Keys            []KeyConfig
	Aliases         []string
	RoutingMode     string
	ReasoningEffort string
	NativeFirst     bool
	// HiddenAliases 是可直接调用但不出现在 /v1/models 的名字。
	HiddenAliases []string
}

// ProviderKeyConfig 是 provider 下配置的一个 key。
type ProviderKeyConfig struct {
	Name         string
	APIKey       string
	Enabled      bool
	AllowVisitor bool
	// Capabilities 是探测缓存（模型列表、各路由可用性等）。
	Capabilities *canonical.Value
}

// ProviderConfig 是一个上游供应商。
type ProviderConfig struct {
	ID           string
	BaseURL      string
	Keys         []ProviderKeyConfig
	Routes       map[string]string
	Capabilities *canonical.Value
}

// RouteTarget 是 unified_model / task 的一个目标（模型 + 可选 key）。
type RouteTarget struct {
	Model string
	Key   string
}

// RoutePlan 是 primary + 可选 fallback。
type RoutePlan struct {
	Primary  RouteTarget
	Fallback *RouteTarget
}

// TaskConfig 是任务名路由：model 传任务名时改用这里的模型与固定参数。
type TaskConfig struct {
	Name          string
	Model         string
	FallbackModel string
	Params        *canonical.Value
}

// Plan 把任务转成路由计划。
func (t TaskConfig) Plan() RoutePlan {
	plan := RoutePlan{Primary: RouteTarget{Model: t.Model}}
	if t.FallbackModel != "" {
		plan.Fallback = &RouteTarget{Model: t.FallbackModel}
	}
	return plan
}

// UnifiedModelConfig 是 unified-model 伪模型的三个路由计划。
type UnifiedModelConfig struct {
	Default RoutePlan
	Image   *RoutePlan
	// Embeddings 对应 /v1/embeddings 的默认模型。
	Embeddings *RoutePlan
}

// RouterConfig 是解析并校验后的完整运行配置。
type RouterConfig struct {
	Host                     string
	Port                     int
	RequestTimeout           float64
	MaxRetries               int
	KeyFailureThreshold      int
	KeyCooldownSeconds       float64
	EndpointCapabilitiesPath string
	MetricsDBPath            string
	LogFilePath              string
	LocalAPIKey              string
	Models                   []ModelConfig
	StreamFirstByteTimeout   float64
	StreamIdleTimeout        float64
	Providers                []ProviderConfig
	// UpstreamRoutes 是「上游 URL -> {模式: 路径}」。
	//
	// 重要：它**只**由 providers[].routes 汇总而来。配置文件顶层的
	// upstream_routes 与 provider key 级的 upstream_routes 都会被忽略——
	// 这是参照实现的实际行为（config.py:877 从空字典起步，而 KeyConfig 在
	// config.py:963 构造时未传 upstream_routes）。迁移对拍语料已固化该行为；
	// 详见 internal/config/doc.go 的说明。
	UpstreamRoutes map[string]map[string]string
	UnifiedModel   *UnifiedModelConfig
	Tasks          []TaskConfig
	WebUIEnabled   bool
	OpsEnabled     bool
	// ReasoningEffortByModel 是 model.id -> reasoning_effort（仅非空项）。
	ReasoningEffortByModel map[string]string
}

// FromDict 解析并校验原始配置。
//
// 对齐 config.py:869：先执行迁移，因此调用方可以直接传入任意版本的配置。
func FromDict(raw *canonical.Value) (*RouterConfig, error) {
	migrated, err := MigrateConfigData(raw)
	if err != nil {
		return nil, err
	}
	version, err := configVersionOf(migrated)
	if err != nil {
		return nil, err
	}
	if version != CONFIG_VERSION {
		return nil, errf("配置文件必须是 config_version %d", CONFIG_VERSION)
	}
	return fromMigrated(migrated)
}

// fromMigrated 解析已经迁移到 v4 的配置。
func fromMigrated(raw *canonical.Value) (*RouterConfig, error) {
	var err error
	config := &RouterConfig{
		UpstreamRoutes:         map[string]map[string]string{},
		ReasoningEffortByModel: map[string]string{},
	}
	// host 走 ``str(raw.get("host", "127.0.0.1"))``：键存在时即便内容是空串也
	// 照用（不回落默认值），与 Python 的 dict.get(key, default) 语义一致。
	config.Host = "127.0.0.1"
	if rawHost, present := raw.LookupOK("host"); present {
		config.Host = rawHost.PyStr()
	}

	if config.Port, err = intOr(raw.Lookup("port"), 8000); err != nil {
		return nil, err
	}
	if config.RequestTimeout, err = floatOr(raw.Lookup("request_timeout"), 60); err != nil {
		return nil, err
	}
	if config.StreamFirstByteTimeout, err = floatOr(raw.Lookup("stream_first_byte_timeout"), 60); err != nil {
		return nil, err
	}
	if config.StreamIdleTimeout, err = floatOr(raw.Lookup("stream_idle_timeout"), 60); err != nil {
		return nil, err
	}
	if config.MaxRetries, err = intOr(raw.Lookup("max_retries"), 2); err != nil {
		return nil, err
	}
	threshold, err := intOr(raw.Lookup("key_failure_threshold"), 2)
	if err != nil {
		return nil, err
	}
	if threshold < 1 {
		threshold = 1
	}
	config.KeyFailureThreshold = threshold

	cooldown, err := floatOr(raw.Lookup("key_cooldown_seconds"), 60)
	if err != nil {
		return nil, err
	}
	if cooldown < 0 {
		cooldown = 0
	}
	config.KeyCooldownSeconds = cooldown

	config.EndpointCapabilitiesPath = pathOr(raw, "endpoint_capabilities_path", "key_state_path", "")
	if config.EndpointCapabilitiesPath == "" {
		path, err := DefaultEndpointCapabilitiesPath()
		if err != nil {
			return nil, err
		}
		config.EndpointCapabilitiesPath = path
	}
	config.MetricsDBPath = pathOr(raw, "metrics_db_path", "", "")
	if config.MetricsDBPath == "" {
		path, err := DefaultMetricsDBPath()
		if err != nil {
			return nil, err
		}
		config.MetricsDBPath = path
	}
	config.LogFilePath = pathOr(raw, "log_file_path", "", "")
	if config.LogFilePath == "" {
		path, err := DefaultLogFilePath()
		if err != nil {
			return nil, err
		}
		config.LogFilePath = path
	}
	config.LocalAPIKey = stringOr(raw.Lookup("local_api_key"), "")
	config.WebUIEnabled = boolOr(raw.Lookup("webui_enabled"), false)
	config.OpsEnabled = boolOr(raw.Lookup("ops_enabled"), true)

	providers, providerKeys, err := parseProviders(raw)
	if err != nil {
		return nil, err
	}
	config.Providers = providers

	models, err := parseModels(raw, providerKeys)
	if err != nil {
		return nil, err
	}
	config.Models = models

	if config.UnifiedModel, err = parseUnifiedModel(raw, models); err != nil {
		return nil, err
	}
	if config.Tasks, err = parseTasks(raw, models); err != nil {
		return nil, err
	}

	// 顶层 upstream_routes 刻意不参与：参照实现从空字典起步。
	for _, provider := range config.Providers {
		if err := mergeUpstreamRoutesForURL(config.UpstreamRoutes, provider.BaseURL, provider.Routes); err != nil {
			return nil, err
		}
	}
	for _, model := range config.Models {
		for _, key := range model.Keys {
			if err := mergeUpstreamRoutesForURL(config.UpstreamRoutes, key.BaseURL, key.UpstreamRoutes); err != nil {
				return nil, err
			}
		}
	}

	for _, model := range config.Models {
		if model.ReasoningEffort != "" {
			config.ReasoningEffortByModel[model.ID] = model.ReasoningEffort
		}
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}

// providerKeyRef 是 (providerID, keyName) 索引的条目。
//
// 带上 BaseURL 是因为构造模型 KeyConfig 时需要 provider 的 base_url，而 key 自身
// 不存储它（参照实现是保留 provider 对象引用，Go 侧用值索引更清晰）。
type providerKeyRef struct {
	Key     ProviderKeyConfig
	BaseURL string
}

// parseProviders 解析 providers 段，并返回 (providerID, keyName) -> key 的索引。
func parseProviders(raw *canonical.Value) ([]ProviderConfig, map[[2]string]providerKeyRef, error) {
	var providers []ProviderConfig
	providerKeys := map[[2]string]providerKeyRef{}

	rawProviders := raw.Lookup("providers")
	if !rawProviders.IsObject() {
		return providers, providerKeys, nil
	}
	defaultBaseURL := stringOr(raw.Lookup("default_base_url"), "")
	if defaultBaseURL == "" {
		defaultBaseURL = "https://api.openai.com"
	}

	for _, providerID := range rawProviders.Obj.Keys() {
		provider := rawProviders.Lookup(providerID)
		if !provider.IsObject() {
			continue
		}
		var keys []ProviderKeyConfig
		rawKeys := provider.Lookup("keys")
		if !rawKeys.IsObject() {
			rawKeys = canonical.NewObject()
		}
		for _, keyName := range rawKeys.Obj.Keys() {
			key := rawKeys.Lookup(keyName)
			if !key.IsObject() {
				return nil, nil, errf("供应商 %s 的 key %s 必须是对象", providerID, keyName)
			}
			// Python 用 key["api_key"] 直接索引：缺失即 KeyError，不是空串。
			rawAPIKey, present := key.LookupOK("api_key")
			if !present {
				return nil, nil, errInternal("'api_key'")
			}
			var capabilities *canonical.Value
			if caps := key.Lookup("capabilities"); caps.IsObject() {
				capabilities = caps.Clone()
			}
			keys = append(keys, ProviderKeyConfig{
				Name:         keyName,
				APIKey:       rawAPIKey.PyStr(),
				Enabled:      boolOr(key.Lookup("enabled"), true),
				AllowVisitor: boolOr(key.Lookup("allow_visitor"), false),
				Capabilities: capabilities,
			})
		}

		baseURL := stringOr(provider.Lookup("base_url"), "")
		if baseURL == "" {
			baseURL = defaultBaseURL
		}
		normalizedBaseURL, err := NormalizeUpstreamBaseURL(canonical.NewString(baseURL))
		if err != nil {
			return nil, nil, err
		}
		routes, err := NormalizeUpstreamRoutes(provider.Lookup("routes"))
		if err != nil {
			return nil, nil, err
		}
		var providerCapabilities *canonical.Value
		if caps := provider.Lookup("capabilities"); caps.IsObject() {
			providerCapabilities = caps.Clone()
		}
		providerConfig := ProviderConfig{
			ID:           providerID,
			BaseURL:      normalizedBaseURL,
			Keys:         keys,
			Routes:       routes,
			Capabilities: providerCapabilities,
		}
		providers = append(providers, providerConfig)
		for _, keyConfig := range providerConfig.Keys {
			providerKeys[[2]string{providerConfig.ID, keyConfig.Name}] = providerKeyRef{
				Key:     keyConfig,
				BaseURL: providerConfig.BaseURL,
			}
		}
	}
	return providers, providerKeys, nil
}

// parseModels 解析 models 段。
func parseModels(raw *canonical.Value, providerKeys map[[2]string]providerKeyRef) ([]ModelConfig, error) {
	rawModels := raw.Lookup("models")
	if rawModels == nil {
		rawModels = canonical.NewObject()
	}
	if !rawModels.IsObject() {
		return nil, errf("config_version 4 的 models 必须是对象")
	}
	defaultRoutingMode := stringOr(raw.Lookup("routing_mode"), "")
	if defaultRoutingMode == "" {
		defaultRoutingMode = "round_robin"
	}

	var models []ModelConfig
	for _, rawModelID := range rawModels.Obj.Keys() {
		model := rawModels.Lookup(rawModelID)
		if !model.IsObject() {
			model = canonical.NewObject()
		}

		var keys []KeyConfig
		usedKeyNames := map[string]bool{}
		// modelKeyName 复刻 config.py:934 的去重闭包：同名时先加 provider 前缀，
		// 仍冲突则追加 -2、-3…… 该状态在整个模型的 targets 间共享。
		modelKeyName := func(baseName, qualifier string) string {
			name := baseName
			if !usedKeyNames[name] {
				usedKeyNames[name] = true
				return name
			}
			qualified := baseName
			if qualifier != "" {
				qualified = qualifier + "-" + baseName
			}
			name = qualified
			suffix := 2
			for usedKeyNames[name] {
				name = qualified + "-" + itoa(suffix)
				suffix++
			}
			usedKeyNames[name] = true
			return name
		}

		for _, target := range model.Lookup("targets").Items() {
			if !target.IsObject() {
				continue
			}
			providerID := strings.TrimSpace(target.Lookup("provider").StringValue())
			keyName := strings.TrimSpace(target.Lookup("key").StringValue())
			upstreamModel := strings.TrimSpace(target.Lookup("upstream_model").StringValue())
			if upstreamModel == "" {
				upstreamModel = rawModelID
			}
			targetEnabled := boolOr(target.Lookup("enabled"), true)

			providerKey, found := providerKeys[[2]string{providerID, keyName}]
			if !found {
				return nil, errf("模型 %s 引用了供应商 %s 不存在的 key: %s", rawModelID, providerID, keyName)
			}
			baseName := strings.TrimSpace(target.Lookup("name").StringValue())
			if baseName == "" {
				baseName = providerKey.Key.Name
			}
			keys = append(keys, KeyConfig{
				Name:    modelKeyName(baseName, providerID),
				APIKey:  providerKey.Key.APIKey,
				BaseURL: providerKey.BaseURL,
				// target 的 enabled 与 provider key 的 enabled 是「与」关系：
				// 任一方禁用该 key 都不可用。
				Enabled:       providerKey.Key.Enabled && targetEnabled,
				AllowVisitor:  providerKey.Key.AllowVisitor,
				Provider:      providerID,
				UpstreamModel: upstreamModel,
				// 参照实现在此未传 upstream_routes（config.py:963），故恒为空。
				UpstreamRoutes: map[string]string{},
			})
		}

		aliases := []string{}
		for _, alias := range model.Lookup("aliases").Items() {
			if rendered := alias.PyStr(); rendered != "" {
				aliases = append(aliases, rendered)
			}
		}
		hiddenAliases := []string{}
		for _, alias := range model.Lookup("hidden_aliases").Items() {
			if rendered := strings.TrimSpace(alias.PyStr()); rendered != "" {
				hiddenAliases = append(hiddenAliases, rendered)
			}
		}
		routingMode := strings.TrimSpace(model.Lookup("routing_mode").StringValue())
		if routingMode == "" {
			routingMode = defaultRoutingMode
		}
		reasoningEffort := strings.TrimSpace(model.Lookup("reasoning_effort").StringValue())
		if reasoningEffort == "default" || reasoningEffort == "downstream" {
			reasoningEffort = ""
		}
		modelID := strings.TrimSpace(model.Lookup("id").StringValue())
		if modelID == "" {
			modelID = rawModelID
		}

		models = append(models, ModelConfig{
			ID:              modelID,
			Keys:            keys,
			Aliases:         aliases,
			RoutingMode:     routingMode,
			ReasoningEffort: reasoningEffort,
			NativeFirst:     boolOr(model.Lookup("native_first"), true),
			HiddenAliases:   hiddenAliases,
		})
	}
	return models, nil
}

// parseUnifiedModel 解析 unified_model 段。
func parseUnifiedModel(raw *canonical.Value, models []ModelConfig) (*UnifiedModelConfig, error) {
	rawUnified := raw.Lookup("unified_model")
	if rawUnified == nil || rawUnified.IsNull() {
		return nil, nil
	}
	if !rawUnified.IsObject() {
		return nil, errf("unified_model 必须是对象")
	}
	idsByName := modelIDsByName(models)

	parseTarget := func(value *canonical.Value, fieldName string) (RouteTarget, error) {
		if !value.IsObject() {
			return RouteTarget{}, errf("%s 必须是对象", fieldName)
		}
		modelName := strings.TrimSpace(value.Lookup("model").StringValue())
		modelID, found := idsByName[modelName]
		if !found {
			return RouteTarget{}, errf("%s 引用了未配置的模型: %s", fieldName, modelName)
		}
		return RouteTarget{
			Model: modelID,
			Key:   strings.TrimSpace(value.Lookup("key").StringValue()),
		}, nil
	}
	parsePlan := func(value *canonical.Value, fieldName string) (*RoutePlan, error) {
		if !value.IsObject() {
			return nil, errf("%s 必须是对象", fieldName)
		}
		primary, err := parseTarget(value.Lookup("primary"), fieldName+".primary")
		if err != nil {
			return nil, err
		}
		plan := &RoutePlan{Primary: primary}
		if rawFallback, present := value.LookupOK("fallback"); present && !rawFallback.IsNull() {
			fallback, err := parseTarget(rawFallback, fieldName+".fallback")
			if err != nil {
				return nil, err
			}
			plan.Fallback = &fallback
		}
		return plan, nil
	}

	defaultPlan, err := parsePlan(rawUnified.Lookup("default"), "unified_model.default")
	if err != nil {
		return nil, err
	}
	unified := &UnifiedModelConfig{Default: *defaultPlan}
	if rawImage, present := rawUnified.LookupOK("image"); present && !rawImage.IsNull() {
		if unified.Image, err = parsePlan(rawImage, "unified_model.image"); err != nil {
			return nil, err
		}
	}
	if rawEmbeddings, present := rawUnified.LookupOK("embeddings"); present && !rawEmbeddings.IsNull() {
		if unified.Embeddings, err = parsePlan(rawEmbeddings, "unified_model.embeddings"); err != nil {
			return nil, err
		}
	}
	return unified, nil
}

// parseTasks 解析 tasks 段。
func parseTasks(raw *canonical.Value, models []ModelConfig) ([]TaskConfig, error) {
	rawTasks := raw.Lookup("tasks")
	if rawTasks == nil || rawTasks.IsNull() {
		return nil, nil
	}
	if !rawTasks.IsObject() {
		return nil, errf("tasks 必须是对象")
	}
	idsByName := modelIDsByName(models)

	var tasks []TaskConfig
	for _, rawTaskName := range rawTasks.Obj.Keys() {
		task := rawTasks.Lookup(rawTaskName)
		taskName := strings.TrimSpace(rawTaskName)
		if taskName == "" {
			return nil, errf("任务名不能为空")
		}
		if !task.IsObject() {
			return nil, errf("任务 %s 必须是对象", taskName)
		}
		resolve := func(value *canonical.Value, fieldName string) (string, error) {
			modelName := strings.TrimSpace(value.StringValue())
			modelID, found := idsByName[modelName]
			if !found {
				return "", errf("%s 引用了未配置的模型: %s", fieldName, modelName)
			}
			return modelID, nil
		}

		modelID, err := resolve(task.Lookup("model"), "tasks."+taskName+".model")
		if err != nil {
			return nil, err
		}
		fallbackModel := ""
		if rawFallback, present := task.LookupOK("fallback_model"); present && !rawFallback.IsNull() {
			if fallbackModel, err = resolve(rawFallback, "tasks."+taskName+".fallback_model"); err != nil {
				return nil, err
			}
		}
		params, err := NormalizeTaskParams(task.Lookup("params"), taskName)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, TaskConfig{
			Name:          taskName,
			Model:         modelID,
			FallbackModel: fallbackModel,
			Params:        params,
		})
	}
	return tasks, nil
}

// modelIDsByName 建立「模型 id 或别名 -> 真实 id」的映射。
func modelIDsByName(models []ModelConfig) map[string]string {
	out := map[string]string{}
	for _, model := range models {
		out[model.ID] = model.ID
		for _, alias := range model.Aliases {
			out[alias] = model.ID
		}
	}
	return out
}

// Validate 校验配置的自洽性。
//
// 对齐 config.py:1108。错误文本会经 management API 回给用户，属对外契约，
// 因此顺序与措辞都与参照实现保持一致。
func (c *RouterConfig) Validate() error {
	if c.StreamFirstByteTimeout <= 0 {
		return errf("stream_first_byte_timeout 必须大于 0")
	}
	if c.StreamIdleTimeout <= 0 {
		return errf("stream_idle_timeout 必须大于 0")
	}
	if c.LocalAPIKey == VISITOR_API_KEY {
		return errf("local_api_key 不能使用保留的访客 key: %s", VISITOR_API_KEY)
	}

	modelNames := map[string]bool{}
	idsByName := map[string]string{}
	modelsByID := map[string]ModelConfig{}
	for _, model := range c.Models {
		if model.ID == "" {
			return errf("模型 id 不能为空")
		}
		if !isValidRoutingMode(model.RoutingMode) {
			return errf("模型 %s 的 routing_mode 必须是 priority、round_robin 或 only_first", model.ID)
		}
		if model.ReasoningEffort != "" && !containsString(reasoningEfforts, model.ReasoningEffort) {
			return errf("模型 %s 的 reasoning_effort 必须是 none、minimal、low、medium、high、xhigh 或 max", model.ID)
		}
		for _, name := range append([]string{model.ID}, model.Aliases...) {
			if modelNames[name] {
				return errf("模型名称重复: %s", name)
			}
			modelNames[name] = true
			idsByName[name] = model.ID
		}
		modelsByID[model.ID] = model

		keyNames := map[string]bool{}
		for _, key := range model.Keys {
			if key.Name == "" {
				return errf("模型 %s 存在空 key name", model.ID)
			}
			if keyNames[key.Name] {
				return errf("模型 %s 的 key name 重复: %s", model.ID, key.Name)
			}
			keyNames[key.Name] = true
			if key.APIKey == "" {
				return errf("模型 %s 存在空 api_key", model.ID)
			}
			if !hasHTTPScheme(key.BaseURL) {
				return errf("模型 %s 的 base_url %s 必须以 http:// 或 https:// 开头", model.ID, key.BaseURL)
			}
		}
	}

	// 手写隐藏别名的冲突检查。自动推导的上游名允许重复（两个模型指向同一上游名时
	// 按 models 顺序取第一个），这里只管用户显式声明的那些。
	claimedHidden := map[string]string{}
	for _, model := range c.Models {
		for _, name := range model.HiddenAliases {
			if name == "" {
				return errf("模型 %s 存在空隐藏别名", model.ID)
			}
			if name == UNIFIED_MODEL_ID {
				return errf("隐藏别名不能使用保留名称: %s", UNIFIED_MODEL_ID)
			}
			if owner, found := idsByName[name]; found && owner != model.ID {
				return errf("模型名称重复: %s", name)
			}
			if previous, found := claimedHidden[name]; found && previous != model.ID {
				return errf("模型名称重复: %s", name)
			}
			claimedHidden[name] = model.ID
		}
	}

	for _, baseURL := range sortedKeys(c.UpstreamRoutes) {
		if !hasHTTPScheme(baseURL) {
			return errf("upstream_routes 的上游URL %s 必须以 http:// 或 https:// 开头", baseURL)
		}
		for _, routeMode := range sortedKeys(c.UpstreamRoutes[baseURL]) {
			if _, err := NormalizeUpstreamRoutePath(routeMode, canonical.NewString(c.UpstreamRoutes[baseURL][routeMode])); err != nil {
				return err
			}
		}
	}

	// 任务名必须能被唯一解析：与模型 ID/别名撞名会让 resolve_route 的语义变得
	// 取决于查表顺序，因此直接禁止。
	taskNames := map[string]bool{}
	hidden := c.HiddenModelNames()
	for _, task := range c.Tasks {
		if task.Name == "" {
			return errf("任务名不能为空")
		}
		if taskNames[task.Name] {
			return errf("任务名重复: %s", task.Name)
		}
		if modelNames[task.Name] || hidden[task.Name] != "" {
			return errf("任务名与模型名称冲突: %s", task.Name)
		}
		if task.Name == UNIFIED_MODEL_ID {
			return errf("任务名不能使用保留名称: %s", UNIFIED_MODEL_ID)
		}
		taskNames[task.Name] = true
		if _, found := modelsByID[task.Model]; !found {
			return errf("任务 %s 引用了未配置的模型: %s", task.Name, task.Model)
		}
		if task.FallbackModel != "" {
			if _, found := modelsByID[task.FallbackModel]; !found {
				return errf("任务 %s 的备选引用了未配置的模型: %s", task.Name, task.FallbackModel)
			}
			if task.FallbackModel == task.Model {
				return errf("任务 %s 的首选和备选不能引用同一模型", task.Name)
			}
		}
	}

	if c.UnifiedModel == nil {
		return nil
	}
	if modelNames[UNIFIED_MODEL_ID] {
		return errf("启用 unified_model 时，模型 ID 和别名不能使用保留名称: %s", UNIFIED_MODEL_ID)
	}
	plans := []struct {
		name string
		plan *RoutePlan
	}{
		{"default", &c.UnifiedModel.Default},
		{"image", c.UnifiedModel.Image},
		{"embeddings", c.UnifiedModel.Embeddings},
	}
	for _, entry := range plans {
		if entry.plan == nil {
			continue
		}
		if entry.plan.Fallback != nil && entry.plan.Primary.Model == entry.plan.Fallback.Model {
			return errf("unified_model.%s 的 primary 和 fallback 不能引用同一模型", entry.name)
		}
		targets := []struct {
			name   string
			target *RouteTarget
		}{
			{"primary", &entry.plan.Primary},
			{"fallback", entry.plan.Fallback},
		}
		for _, target := range targets {
			if target.target == nil {
				continue
			}
			targetModel, found := modelsByID[target.target.Model]
			if !found {
				return errf("unified_model.%s.%s 引用了未配置的模型: %s", entry.name, target.name, target.target.Model)
			}
			if target.target.Key != "" && !targetModel.hasEnabledKey(target.target.Key) {
				return errf("模型 %s 未配置可用 key: %s", target.target.Model, target.target.Key)
			}
		}
	}
	return nil
}

// hasEnabledKey 报告模型是否存在指定名称且启用的 key。
func (m ModelConfig) hasEnabledKey(name string) bool {
	for _, key := range m.Keys {
		if key.Name == name && key.Enabled {
			return true
		}
	}
	return false
}

// ConfiguredModelID 返回模型名（id 或别名）对应的真实 id。
func (c *RouterConfig) ConfiguredModelID(modelName string) (string, bool) {
	for _, model := range c.Models {
		if modelName == model.ID || containsString(model.Aliases, modelName) {
			return model.ID, true
		}
	}
	return "", false
}

// TaskFor 按名称查找任务。
func (c *RouterConfig) TaskFor(name string) (TaskConfig, bool) {
	for _, task := range c.Tasks {
		if task.Name == name {
			return task, true
		}
	}
	return TaskConfig{}, false
}

// HiddenModelNames 返回可直接调用、但不出现在 /v1/models 中的名字 -> 本地模型 ID。
//
// 手写 hidden_aliases 加上从每个 target 的 upstream_model 自动推导的名字（上游叫法
// 不变即可直接调用，无需重复维护）。同名的真实 ID/别名优先。
func (c *RouterConfig) HiddenModelNames() map[string]string {
	result := map[string]string{}
	for _, model := range c.Models {
		names := append([]string{}, model.HiddenAliases...)
		for _, key := range model.Keys {
			if key.UpstreamModel != "" {
				names = append(names, key.UpstreamModel)
			}
		}
		for _, name := range names {
			if _, exists := result[name]; !exists {
				result[name] = model.ID
			}
		}
	}
	return result
}

// NativeFirstForModel 返回模型的 native_first 设置（缺省 true）。
func (c *RouterConfig) NativeFirstForModel(modelID string) bool {
	for _, model := range c.Models {
		if model.ID == modelID {
			return model.NativeFirst
		}
	}
	return true
}

// UpstreamRoutesForBaseURL 返回某上游 URL 的路由副本。
func (c *RouterConfig) UpstreamRoutesForBaseURL(baseURL string) map[string]string {
	normalized, err := NormalizeUpstreamBaseURL(canonical.NewString(baseURL))
	if err != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	for mode, path := range c.UpstreamRoutes[normalized] {
		out[mode] = path
	}
	return out
}

// isValidRoutingMode 报告路由模式是否合法。
func isValidRoutingMode(mode string) bool {
	return mode == "priority" || mode == "round_robin" || mode == "only_first"
}

// hasHTTPScheme 报告 URL 是否以 http:// 或 https:// 开头。
func hasHTTPScheme(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}
