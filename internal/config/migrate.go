package config

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// MigrateConfigData 把任意版本的配置迁移到 CONFIG_VERSION。
//
// 对齐 config.py:315。始终在私有副本上操作，调用方传入的 payload 不会被改写。
//
// 三条返回路径的差异是刻意的，语料逐条断言：
//   - 版本已是 4：原样返回（连 config_version 的类型都保留，比如 "4"、4.0、true）；
//   - 版本为 3：迁移并**强制**把 config_version 写成整数 4；
//   - 其它版本：v1/v2 列表布局展平；models 不是列表则原样返回。
func MigrateConfigData(raw *canonical.Value) (*canonical.Value, error) {
	if raw == nil {
		raw = canonical.NewObject()
	}
	if !raw.IsObject() {
		// Python 会在 normalized.get(...) 处抛 AttributeError；这里失败关闭即可。
		return nil, errInternal("'%s' object has no attribute 'get'", pyTypeName(raw))
	}
	normalized := raw.Clone()

	// 旧版 unified_model 折叠成 v4 的 primary/fallback 形状。
	unifiedModel := normalized.Lookup("unified_model")
	if unifiedModel != nil && unifiedModel.IsObject() && !unifiedModel.Obj.Has("default") {
		model := strings.TrimSpace(unifiedModel.Lookup("model").StringValue())
		if model != "" {
			defaultPlan := canonical.NewObjectOf(
				canonical.ObjectPair{Key: "primary", Value: canonical.NewObjectOf(
					canonical.ObjectPair{Key: "model", Value: canonical.NewString(model)},
					canonical.ObjectPair{Key: "key", Value: nullOrClone(unifiedModel.Lookup("key"))},
				)},
			)
			migratedUnified := canonical.NewObjectOf(
				canonical.ObjectPair{Key: "default", Value: defaultPlan},
			)
			imageModel := strings.TrimSpace(unifiedModel.Lookup("image_model").StringValue())
			if imageModel != "" {
				migratedUnified.SetKey("image", canonical.NewObjectOf(
					canonical.ObjectPair{Key: "primary", Value: canonical.NewObjectOf(
						canonical.ObjectPair{Key: "model", Value: canonical.NewString(imageModel)},
						canonical.ObjectPair{Key: "key", Value: nullOrClone(unifiedModel.Lookup("image_key"))},
					)},
				))
			}
			normalized.SetKey("unified_model", migratedUnified)
		}
	}

	// key_state_path 是 endpoint_capabilities_path 的旧名。
	if !normalized.Obj.Has("endpoint_capabilities_path") && normalized.Lookup("key_state_path").Truthy() {
		normalized.SetKey("endpoint_capabilities_path", nullOrClone(normalized.Lookup("key_state_path")))
	}
	normalized.DeleteKey("key_state_path")

	version, err := configVersionOf(normalized)
	if err != nil {
		return nil, err
	}

	switch {
	case version == CONFIG_VERSION:
		if normalized.Lookup("models").IsObject() {
			dropLegacyPoolRemnants(normalized)
		}
		return normalized, nil
	case version > CONFIG_VERSION:
		return nil, errf("配置文件版本 %d 高于当前支持的 %d，请升级软件", version, CONFIG_VERSION)
	case version == 3:
		if !normalized.Lookup("models").IsObject() {
			return normalized, nil
		}
		if err := migrateV3ToV4(normalized); err != nil {
			return nil, err
		}
		normalized.SetKey("config_version", canonical.NewIntValue(CONFIG_VERSION))
		return normalized, nil
	}

	models := normalized.Lookup("models")
	if !models.IsArray() {
		return normalized, nil
	}
	if version != 0 && version != 1 && version != 2 {
		return normalized, nil
	}
	if err := migrateLegacyListLayout(normalized, models, raw); err != nil {
		return nil, err
	}
	return normalized, nil
}

// nullOrClone 返回 nil 时给出显式 null，否则返回深拷贝。
//
// 用于把 unified_model 里的 model/key 搬进新结构：key 为 None 时必须仍然写出
// "key": null（Python 的 dict 会带上这个键），不能省略。
func nullOrClone(value *canonical.Value) *canonical.Value {
	if value == nil {
		return canonical.NewNull()
	}
	return value.Clone()
}

// migrateLegacyListLayout 把 v1/v2 的 models 列表布局展平成 v4。
//
// 对齐 config.py:357：把每个模型的 keys 数组变成 providers + targets 两张表，
// 并把每个 key 的 upstream_routes 按上游 URL 汇总。
func migrateLegacyListLayout(migrated, models, raw *canonical.Value) error {
	migrated.SetKey("config_version", canonical.NewIntValue(CONFIG_VERSION))
	providers := canonical.NewObject()
	migratedModels := canonical.NewObject()

	upstreamRoutes, err := NormalizeUpstreamURLRoutes(raw.Lookup("upstream_routes"))
	if err != nil {
		return err
	}

	for _, rawModel := range models.Items() {
		if !rawModel.IsObject() {
			continue
		}
		modelID := strings.TrimSpace(rawModel.Lookup("id").StringValue())
		if modelID == "" {
			continue
		}
		rawKeys := rawModel.Lookup("keys")
		if !rawKeys.IsArray() {
			rawKeys = canonical.NewArray()
		}

		targets := canonical.NewArray()
		usedKeyNames := map[string]bool{}
		for index, rawKey := range rawKeys.Items() {
			if !rawKey.IsObject() {
				continue
			}
			keyName := strings.TrimSpace(rawKey.Lookup("name").StringValue())
			if keyName == "" {
				keyName = modelID + "-" + strconv.Itoa(index+1)
			}
			apiKey := strings.TrimSpace(rawKey.Lookup("api_key").StringValue())
			if keyName == "" || apiKey == "" {
				continue
			}
			if usedKeyNames[keyName] {
				return errf("模型 %s 的 key name 重复: %s", modelID, keyName)
			}
			usedKeyNames[keyName] = true

			rawBaseURL := rawKey.Lookup("base_url")
			if !rawBaseURL.Truthy() {
				rawBaseURL = raw.Lookup("default_base_url")
			}
			if !rawBaseURL.Truthy() {
				rawBaseURL = canonical.NewString("https://api.openai.com")
			}
			baseURL, err := NormalizeUpstreamBaseURL(rawBaseURL)
			if err != nil {
				return err
			}
			providerID := legacyProviderID(baseURL)

			provider := providers.Lookup(providerID)
			if !provider.IsObject() {
				provider = canonical.NewObjectOf(
					canonical.ObjectPair{Key: "base_url", Value: canonical.NewString(baseURL)},
					canonical.ObjectPair{Key: "keys", Value: canonical.NewObject()},
				)
				providers.SetKey(providerID, provider)
			}
			if !provider.Obj.Has("base_url") {
				provider.SetKey("base_url", canonical.NewString(baseURL))
			}
			providerKeys := provider.Lookup("keys")
			if !providerKeys.IsObject() {
				providerKeys = canonical.NewObject()
				provider.SetKey("keys", providerKeys)
			}

			enabled := true
			if value, ok := rawKey.LookupOK("enabled"); ok {
				enabled = value.Truthy()
			}
			providerKeys.SetKey(keyName, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "api_key", Value: canonical.NewString(apiKey)},
				canonical.ObjectPair{Key: "enabled", Value: canonical.NewBool(enabled)},
			))

			upstreamModel := strings.TrimSpace(rawKey.Lookup("upstream_model").StringValue())
			if upstreamModel == "" {
				upstreamModel = modelID
			}
			keyRoutes, err := NormalizeUpstreamRoutes(rawKey.Lookup("upstream_routes"))
			if err != nil {
				return err
			}
			if err := mergeUpstreamRoutesForURL(upstreamRoutes, baseURL, keyRoutes); err != nil {
				return err
			}

			targets.Arr = append(targets.Arr, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "provider", Value: canonical.NewString(providerID)},
				canonical.ObjectPair{Key: "key", Value: canonical.NewString(keyName)},
				canonical.ObjectPair{Key: "upstream_model", Value: canonical.NewString(upstreamModel)},
			))
		}

		migratedModel := canonical.NewObjectOf(
			canonical.ObjectPair{Key: "targets", Value: targets},
		)
		for _, fieldName := range []string{"aliases", "routing_mode", "reasoning_effort", "native_first"} {
			if value, ok := rawModel.LookupOK(fieldName); ok {
				migratedModel.SetKey(fieldName, value.Clone())
			}
		}
		migratedModels.SetKey(modelID, migratedModel)
	}

	migrated.SetKey("providers", providers)
	migrated.SetKey("models", migratedModels)
	if len(upstreamRoutes) > 0 {
		routesValue := canonical.NewObject()
		// ponytail: 这里按键排序输出，而 Python 保留插入顺序。两者内容相同、都是
		// 合法 JSON，且 config_revision 用 sort_keys 计算摘要，因此不影响哈希与
		// 语义；差异只体现在一次性 v1/v2 升级后落盘文件的字段顺序上（可读性差异）。
		// Go 的 map 迭代顺序是随机的，若不排序会让输出不确定，反而更糟。
		// 若日后要求与 Python 的落盘字节完全一致，需把路由累积改成有序结构。
		for _, baseURL := range sortedKeys(upstreamRoutes) {
			perMode := canonical.NewObject()
			for _, mode := range sortedKeys(upstreamRoutes[baseURL]) {
				perMode.SetKey(mode, canonical.NewString(upstreamRoutes[baseURL][mode]))
			}
			routesValue.SetKey(baseURL, perMode)
		}
		migrated.SetKey("upstream_routes", routesValue)
	} else {
		migrated.DeleteKey("upstream_routes")
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// legacyProviderID 从上游 URL 推导出旧版供应商 ID。
//
// 对齐 config.py:645。这是用户可见的 ID（会出现在 provider 引用里），
// 因此 - 与 : 的替换规则必须一致。
func legacyProviderID(baseURL string) string {
	providerID := strings.ReplaceAll(baseURL, "https://", "")
	providerID = strings.ReplaceAll(providerID, "http://", "")
	providerID = strings.Trim(providerID, "/")
	providerID = strings.ReplaceAll(providerID, "/", "-")
	providerID = strings.ReplaceAll(providerID, ":", "-")
	if providerID == "" {
		return "default"
	}
	return providerID
}

// migrateV3ToV4 把 v3 的 provider pool 展平成 key 级 targets。
//
// 对齐 config.py:434。迁移是幂等的，且不会因「仅供参考」的探测元数据失败。
func migrateV3ToV4(migrated *canonical.Value) error {
	providersRaw := migrated.Lookup("providers")
	modelsRaw := migrated.Lookup("models")
	if !providersRaw.IsObject() || !modelsRaw.IsObject() {
		// 把标量/列表残留交给 from_dict 明确报错。
		migrated.DeleteKey("pools")
		return nil
	}

	for _, providerID := range providersRaw.Obj.Keys() {
		provider := providersRaw.Lookup(providerID)
		if !provider.IsObject() {
			continue
		}
		rawKeys := provider.Lookup("keys")

		// v3 的探测元数据是按池记录的；v4 按 key 缓存。这里保守地把旧池的模型
		// 折叠进池内每个 key，让 TUI 立刻能提供绑定；后续按 key 刷新会覆盖。
		legacy, err := legacyProbeCapabilities(provider)
		if err != nil {
			return err
		}
		if legacy != nil {
			// Python 在此处直接对 raw_keys 调 .values()：keys 不是对象时抛
			// AttributeError。必须同样失败关闭，否则会静默丢探测数据。
			if !rawKeys.IsObject() {
				return errInternal("'%s' object has no attribute 'values'", pyTypeName(rawKeys))
			}
			for _, keyName := range rawKeys.Obj.Keys() {
				key := rawKeys.Lookup(keyName)
				if key.IsObject() && !key.Lookup("capabilities").IsObject() {
					key.SetKey("capabilities", legacy.Clone())
				}
			}
		}
		for _, field := range []string{"capabilities", "available_models", "key_models", "routes_by_key", "_probe_cache"} {
			provider.DeleteKey(field)
		}
	}

	for _, modelID := range modelsRaw.Obj.Keys() {
		model := modelsRaw.Lookup(modelID)
		if !model.IsObject() {
			continue
		}
		targets := model.Lookup("targets")
		if !targets.IsArray() {
			model.SetKey("targets", canonical.NewArray())
			continue
		}
		rewritten := canonical.NewArray()
		for _, target := range targets.Items() {
			if !target.IsObject() {
				continue
			}
			providerID := strings.TrimSpace(target.Lookup("provider").StringValue())
			poolName := strings.TrimSpace(target.Lookup("pool").StringValue())
			upstreamModel := strings.TrimSpace(target.Lookup("upstream_model").StringValue())
			if upstreamModel == "" {
				upstreamModel = modelID
			}
			provider := providersRaw.Lookup(providerID)
			if !provider.IsObject() {
				// v3 解析器会拒绝引用未知供应商；迁移保持同样的严格性，
				// 以免有引用被静默丢弃。
				return errf("模型 %s 引用了未配置的供应商: %s", modelID, providerID)
			}
			poolKeys := v3PoolKeys(provider, poolName)
			// 已存在的 v3 池可以合法地没有 key（例如 key 被删光）。它是有效的
			// 空目标，由下面的白名单语义过滤掉；只有真正缺失的池才是非法引用。
			if poolName != "" && !v3PoolExists(provider, poolName) {
				return errf("模型 %s 引用了供应商 %s 不存在的 pool: %s", modelID, providerID, poolName)
			}
			// v3 按池白名单过滤 target（upstream_model 必须在 pool.models 里，
			// 空数组 = 未启用任何模型、同样过滤）。迁移必须复刻该语义：白名单
			// 键存在即过滤（含空），键缺失才不设限。不这样做会让 v3 中被静默
			// 丢弃的死引用在升级后意外复活。
			if hasWhitelist, err := v3PoolHasWhitelist(provider, poolName); err != nil {
				return err
			} else if hasWhitelist {
				poolModels, err := v3PoolEnabledModels(provider, poolName)
				if err != nil {
					return err
				}
				if !poolModels[upstreamModel] {
					continue
				}
			}
			for _, keyName := range poolKeys {
				rewritten.Arr = append(rewritten.Arr, canonical.NewObjectOf(
					canonical.ObjectPair{Key: "provider", Value: canonical.NewString(providerID)},
					canonical.ObjectPair{Key: "key", Value: canonical.NewString(keyName)},
					canonical.ObjectPair{Key: "upstream_model", Value: canonical.NewString(upstreamModel)},
				))
			}
		}
		model.SetKey("targets", rewritten)
	}

	for _, providerID := range providersRaw.Obj.Keys() {
		providersRaw.Lookup(providerID).DeleteKey("pools")
	}
	return nil
}

// v3PoolExists 报告指定名称的池是否存在。
func v3PoolExists(provider *canonical.Value, poolName string) bool {
	pools := provider.Lookup("pools")
	return pools.IsObject() && poolName != "" && pools.Obj.Has(poolName)
}

// v3PoolKeys 返回池内的 key 名称列表。
func v3PoolKeys(provider *canonical.Value, poolName string) []string {
	pools := provider.Lookup("pools")
	if !pools.IsObject() || poolName == "" {
		return nil
	}
	keys := pools.Lookup(poolName).Lookup("keys")
	if !keys.IsArray() {
		return nil
	}
	out := make([]string, 0, keys.Len())
	for _, key := range keys.Items() {
		out = append(out, key.PyStr())
	}
	return out
}

// v3PoolHasWhitelist 报告池是否带有显式的 models 白名单。
//
// 显式白名单（哪怕是空数组）意味着该池只服务列出的上游模型；v3 会丢弃其它
// 所有 target 引用。
func v3PoolHasWhitelist(provider *canonical.Value, poolName string) (bool, error) {
	pools := provider.Lookup("pools")
	if !pools.IsObject() || poolName == "" {
		return false, nil
	}
	pool := pools.Lookup(poolName)
	if !pool.IsObject() {
		return false, nil
	}
	models := pool.Lookup("models")
	if models == nil {
		return false, nil
	}
	if !models.IsArray() {
		// Python 的 isinstance(pool.get("models"), list) 对非列表返回 False，
		// 即「没有白名单」，不报错。
		return false, nil
	}
	return true, nil
}

// v3PoolEnabledModels 返回池白名单里的模型集合。
func v3PoolEnabledModels(provider *canonical.Value, poolName string) (map[string]bool, error) {
	pools := provider.Lookup("pools")
	if !pools.IsObject() || poolName == "" {
		return map[string]bool{}, nil
	}
	models := pools.Lookup(poolName).Lookup("models")
	if !models.IsArray() {
		return map[string]bool{}, nil
	}
	out := map[string]bool{}
	for _, modelID := range models.Items() {
		if rendered := modelID.PyStr(); rendered != "" {
			out[rendered] = true
		}
	}
	return out, nil
}

// iterateOrEmpty 复刻 Python 的 “for x in (value or [])“。
//
// 关键的 Python 语义有两层：先做真值替换（None、空串、0、空容器都变成空列表），
// 再按类型迭代（list 出元素、str 出字符、dict 出键，int/float 抛 TypeError）。
// 池白名单写成字符串 "ab" 时会被展开成 'a'、'b'，这是语料实测的行为。
func iterateOrEmpty(value *canonical.Value) ([]string, error) {
	if !truthyValue(value) {
		return nil, nil
	}
	items, err := canonical.PyIterate(value)
	if err != nil {
		return nil, errInternal("%s", err.Error())
	}
	return items, nil
}

// legacyProbeCapabilities 提取旧版（v3 池 / 早期 v4 供应商级）探测元数据。
//
// 对齐 config.py:575，折叠成保守的 {models, checked_at}，迁移时复制到该供应商的
// 每个 key 上。无内容时返回 nil。
func legacyProbeCapabilities(provider *canonical.Value) (*canonical.Value, error) {
	mergedModels := map[string]bool{}
	var mergedChecked *canonical.Value

	if capabilities := provider.Lookup("capabilities"); capabilities.IsObject() {
		models, err := iterateOrEmpty(capabilities.Lookup("models"))
		if err != nil {
			return nil, err
		}
		for _, modelID := range models {
			mergedModels[modelID] = true
		}
		if checked := capabilities.Lookup("checked_at"); checked != nil {
			mergedChecked = checked.Clone()
		}
	}

	if pools := provider.Lookup("pools"); pools.IsObject() {
		for _, poolName := range pools.Obj.Keys() {
			pool := pools.Lookup(poolName)
			if !pool.IsObject() {
				continue
			}
			for _, field := range []string{"available_models", "all_available_models", "models"} {
				models, err := iterateOrEmpty(pool.Lookup(field))
				if err != nil {
					return nil, err
				}
				for _, modelID := range models {
					mergedModels[modelID] = true
				}
			}
			if checked := pool.Lookup("checked_at"); checked.Truthy() && !truthyValue(mergedChecked) {
				mergedChecked = checked.Clone()
			}
		}
	}

	if len(mergedModels) == 0 {
		return nil, nil
	}
	models := make([]string, 0, len(mergedModels))
	for modelID := range mergedModels {
		models = append(models, modelID)
	}
	sort.Strings(models)

	result := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "models", Value: canonical.NewStringArray(models)},
	)
	if truthyValue(mergedChecked) {
		result.SetKey("checked_at", mergedChecked)
	}
	return result, nil
}

// truthyValue 是 nil 安全的真值判断。
func truthyValue(v *canonical.Value) bool { return v != nil && v.Truthy() }

// dropLegacyPoolRemnants 幂等地清除 v4 payload 里的池残留。
//
// 对齐 config.py:612：同时把早期 v4 写在供应商级的 capabilities promote 到每个
// 尚无按 key 探测缓存的 key 上，然后删掉供应商级字段，保持磁盘布局以 key 为准。
func dropLegacyPoolRemnants(data *canonical.Value) {
	providers := data.Lookup("providers")
	if !providers.IsObject() {
		return
	}
	for _, providerID := range providers.Obj.Keys() {
		provider := providers.Lookup(providerID)
		if !provider.IsObject() {
			continue
		}
		provider.DeleteKey("pools")
		provider.DeleteKey("available_models")
		capabilities := provider.Lookup("capabilities")
		provider.DeleteKey("capabilities")
		if !capabilities.IsObject() || !capabilities.Lookup("models").Truthy() {
			continue
		}
		rawKeys := provider.Lookup("keys")
		if !rawKeys.IsObject() {
			continue
		}
		folded := canonical.NewObjectOf(
			canonical.ObjectPair{Key: "models", Value: nullOrClone(capabilities.Lookup("models"))},
		)
		for _, field := range []string{"checked_at", "errors", "route_status"} {
			if value := capabilities.Lookup(field); value.Truthy() {
				folded.SetKey(field, value.Clone())
			}
		}
		for _, keyName := range rawKeys.Obj.Keys() {
			key := rawKeys.Lookup(keyName)
			if key.IsObject() && !key.Lookup("capabilities").IsObject() {
				key.SetKey("capabilities", folded.Clone())
			}
		}
	}
}
