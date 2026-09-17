package config

import (
	"errors"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestFromDictMatchesPython 是 RouterConfig 解析与校验的核心对拍断言。
//
// 语料由 scripts/gen_config_model_corpus.py 生成，输入是原始配置 JSON 文本
// （保留键的插入顺序），输出是参照实现序列化后的稳定结构。
func TestFromDictMatchesPython(t *testing.T) {
	for _, entry := range loadCorpus(t, "model.jsonl") {
		t.Run(entry.Name, func(t *testing.T) {
			raw := mustParse(t, entry.Input)
			cfg, err := FromDict(raw)
			if !entry.OK {
				if err == nil {
					t.Fatalf("参照实现报错 %s，但 Go 成功了", entry.ErrorType)
				}
				if entry.ErrorType == "ValueError" {
					var configErr *ConfigError
					if !errors.As(err, &configErr) {
						t.Fatalf("期望契约错误（ConfigError），实际 %T: %v", err, err)
					}
					if configErr.Message != entry.Error {
						t.Errorf("错误文本不一致\n期望: %s\n实际: %s", entry.Error, configErr.Message)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("参照实现成功，但 Go 报错: %v", err)
			}
			got := canonical.Dumps(serializeConfig(cfg))
			if got != entry.Output {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", entry.Output, got)
			}
		})
	}
}

// serializeConfig 把 RouterConfig 摊平成与语料一致的结构。
//
// 这是**测试契约**：字段名与嵌套形状必须与 scripts/gen_config_model_corpus.py 的
// serialize_config 完全对应，两边任何一处单独改动都会让对拍失败（这是刻意设计，
// 用来防止一侧悄悄漂移）。
func serializeConfig(cfg *RouterConfig) *canonical.Value {
	models := make([]*canonical.Value, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		keys := make([]*canonical.Value, 0, len(model.Keys))
		for _, key := range model.Keys {
			keys = append(keys, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "name", Value: canonical.NewString(key.Name)},
				canonical.ObjectPair{Key: "api_key", Value: canonical.NewString(key.APIKey)},
				canonical.ObjectPair{Key: "base_url", Value: canonical.NewString(key.BaseURL)},
				canonical.ObjectPair{Key: "enabled", Value: canonical.NewBool(key.Enabled)},
				canonical.ObjectPair{Key: "allow_visitor", Value: canonical.NewBool(key.AllowVisitor)},
				canonical.ObjectPair{Key: "upstream_routes", Value: stringMapValue(key.UpstreamRoutes)},
				canonical.ObjectPair{Key: "provider", Value: nullableString(key.Provider)},
				canonical.ObjectPair{Key: "upstream_model", Value: nullableString(key.UpstreamModel)},
			))
		}
		models = append(models, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "id", Value: canonical.NewString(model.ID)},
			canonical.ObjectPair{Key: "keys", Value: canonical.NewArray(keys...)},
			canonical.ObjectPair{Key: "aliases", Value: canonical.NewStringArray(model.Aliases)},
			canonical.ObjectPair{Key: "routing_mode", Value: canonical.NewString(model.RoutingMode)},
			canonical.ObjectPair{Key: "reasoning_effort", Value: nullableString(model.ReasoningEffort)},
			canonical.ObjectPair{Key: "native_first", Value: canonical.NewBool(model.NativeFirst)},
			canonical.ObjectPair{Key: "hidden_aliases", Value: canonical.NewStringArray(model.HiddenAliases)},
		))
	}

	providers := make([]*canonical.Value, 0, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		keys := make([]*canonical.Value, 0, len(provider.Keys))
		for _, key := range provider.Keys {
			keys = append(keys, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "name", Value: canonical.NewString(key.Name)},
				canonical.ObjectPair{Key: "api_key", Value: canonical.NewString(key.APIKey)},
				canonical.ObjectPair{Key: "enabled", Value: canonical.NewBool(key.Enabled)},
				canonical.ObjectPair{Key: "allow_visitor", Value: canonical.NewBool(key.AllowVisitor)},
				canonical.ObjectPair{Key: "capabilities", Value: orNull(key.Capabilities)},
			))
		}
		providers = append(providers, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "id", Value: canonical.NewString(provider.ID)},
			canonical.ObjectPair{Key: "base_url", Value: canonical.NewString(provider.BaseURL)},
			canonical.ObjectPair{Key: "keys", Value: canonical.NewArray(keys...)},
			canonical.ObjectPair{Key: "routes", Value: stringMapValue(provider.Routes)},
			canonical.ObjectPair{Key: "capabilities", Value: orNull(provider.Capabilities)},
		))
	}

	tasks := make([]*canonical.Value, 0, len(cfg.Tasks))
	for _, task := range cfg.Tasks {
		tasks = append(tasks, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "name", Value: canonical.NewString(task.Name)},
			canonical.ObjectPair{Key: "model", Value: canonical.NewString(task.Model)},
			canonical.ObjectPair{Key: "fallback_model", Value: nullableString(task.FallbackModel)},
			canonical.ObjectPair{Key: "params", Value: orNull(task.Params)},
		))
	}

	var unified *canonical.Value = canonical.NewNull()
	if cfg.UnifiedModel != nil {
		unified = canonical.NewObjectOf(
			canonical.ObjectPair{Key: "default", Value: serializePlan(&cfg.UnifiedModel.Default)},
			canonical.ObjectPair{Key: "image", Value: serializePlan(cfg.UnifiedModel.Image)},
			canonical.ObjectPair{Key: "embeddings", Value: serializePlan(cfg.UnifiedModel.Embeddings)},
		)
	}

	routesByURL := canonical.NewObject()
	for _, baseURL := range sortedKeys(cfg.UpstreamRoutes) {
		routesByURL.SetKey(baseURL, stringMapValue(cfg.UpstreamRoutes[baseURL]))
	}

	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "host", Value: canonical.NewString(cfg.Host)},
		canonical.ObjectPair{Key: "port", Value: canonical.NewIntValue(int64(cfg.Port))},
		canonical.ObjectPair{Key: "request_timeout", Value: canonical.NewFloat(cfg.RequestTimeout)},
		canonical.ObjectPair{Key: "stream_first_byte_timeout", Value: canonical.NewFloat(cfg.StreamFirstByteTimeout)},
		canonical.ObjectPair{Key: "stream_idle_timeout", Value: canonical.NewFloat(cfg.StreamIdleTimeout)},
		canonical.ObjectPair{Key: "max_retries", Value: canonical.NewIntValue(int64(cfg.MaxRetries))},
		canonical.ObjectPair{Key: "key_failure_threshold", Value: canonical.NewIntValue(int64(cfg.KeyFailureThreshold))},
		canonical.ObjectPair{Key: "key_cooldown_seconds", Value: canonical.NewFloat(cfg.KeyCooldownSeconds)},
		canonical.ObjectPair{Key: "endpoint_capabilities_path", Value: canonical.NewString(cfg.EndpointCapabilitiesPath)},
		canonical.ObjectPair{Key: "metrics_db_path", Value: canonical.NewString(cfg.MetricsDBPath)},
		canonical.ObjectPair{Key: "log_file_path", Value: canonical.NewString(cfg.LogFilePath)},
		canonical.ObjectPair{Key: "local_api_key", Value: canonical.NewString(cfg.LocalAPIKey)},
		canonical.ObjectPair{Key: "webui_enabled", Value: canonical.NewBool(cfg.WebUIEnabled)},
		canonical.ObjectPair{Key: "ops_enabled", Value: canonical.NewBool(cfg.OpsEnabled)},
		canonical.ObjectPair{Key: "models", Value: canonical.NewArray(models...)},
		canonical.ObjectPair{Key: "providers", Value: canonical.NewArray(providers...)},
		canonical.ObjectPair{Key: "upstream_routes", Value: routesByURL},
		canonical.ObjectPair{Key: "unified_model", Value: unified},
		canonical.ObjectPair{Key: "tasks", Value: canonical.NewArray(tasks...)},
		canonical.ObjectPair{Key: "reasoning_effort_by_model", Value: stringMapValue(cfg.ReasoningEffortByModel)},
		canonical.ObjectPair{Key: "hidden_model_names", Value: stringMapValue(cfg.HiddenModelNames())},
	)
}

func serializePlan(plan *RoutePlan) *canonical.Value {
	if plan == nil {
		return canonical.NewNull()
	}
	var fallback *canonical.Value = canonical.NewNull()
	if plan.Fallback != nil {
		fallback = canonical.NewObjectOf(
			canonical.ObjectPair{Key: "model", Value: canonical.NewString(plan.Fallback.Model)},
			canonical.ObjectPair{Key: "key", Value: nullableString(plan.Fallback.Key)},
		)
	}
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "primary", Value: canonical.NewObjectOf(
			canonical.ObjectPair{Key: "model", Value: canonical.NewString(plan.Primary.Model)},
			canonical.ObjectPair{Key: "key", Value: nullableString(plan.Primary.Key)},
		)},
		canonical.ObjectPair{Key: "fallback", Value: fallback},
	)
}

// nullableString 把空字符串表示为 JSON null。
func nullableString(value string) *canonical.Value {
	if value == "" {
		return canonical.NewNull()
	}
	return canonical.NewString(value)
}

// orNull 返回 nil 时给出 JSON null。
func orNull(value *canonical.Value) *canonical.Value {
	if value == nil {
		return canonical.NewNull()
	}
	return value
}

// stringMapValue 把 map[string]string 转成对象，键排序以匹配 canonical 输出。
func stringMapValue(values map[string]string) *canonical.Value {
	obj := canonical.NewObject()
	for _, key := range sortedKeys(values) {
		obj.SetKey(key, canonical.NewString(values[key]))
	}
	return obj
}

// TestModelCorpusIsDiscriminating 是一道测试有效性防线。
//
// 它断言语料确实覆盖了会失败的用例类别：若有人说「解析逻辑全对」，那么语料里
// 必须同时存在成功与失败的样本，且失败样本的错误文本各不相同。语料被削弱到
// 只剩几个 happy path 时本测试会失败。
func TestModelCorpusIsDiscriminating(t *testing.T) {
	entries := loadCorpus(t, "model.jsonl")
	successes, configErrors, internalErrors := 0, 0, 0
	distinctMessages := map[string]bool{}
	for _, entry := range entries {
		if entry.OK {
			successes++
			continue
		}
		switch entry.ErrorType {
		case "ValueError":
			configErrors++
			distinctMessages[entry.Error] = true
		default:
			internalErrors++
		}
	}
	t.Logf("语料构成：%d 成功、%d 契约错误（%d 种文本）、%d 内部错误",
		successes, configErrors, len(distinctMessages), internalErrors)

	if successes < 40 {
		t.Errorf("成功用例过少（%d），覆盖不足", successes)
	}
	if configErrors < 15 {
		t.Errorf("契约错误用例过少（%d），校验分支覆盖不足", configErrors)
	}
	if len(distinctMessages) < 10 {
		t.Errorf("契约错误文本种类过少（%d），不同校验分支未被区分", len(distinctMessages))
	}
}

// TestParseDroppedRoutesDocumented 固化两个已知的参照实现行为，防止 Go 侧
// 「顺手修好」而与 Python 产生差异。
//
// 背景（均已实测确认）：
//
//  1. 配置文件顶层的 upstream_routes 被 from_dict 完全忽略（config.py:877 从空
//     字典起步，其后只从 providers 与 model key 汇总）。v1/v2 迁移会写出这个
//     字段，因此那些版本升级上来的路由配置实际不生效。
//  2. KeyConfig 在 config.py:963 构造时未传 upstream_routes，导致
//     key.upstream_routes 恒为 {}，令 config.py:840 的合并循环成为死代码。
//
// Go 侧刻意保持同样行为：迁移期间不得让同一个配置文件在两种实现下产生不同的
// 上游请求路径。修复应当作为独立变更，并在两种实现上同步进行。
func TestParseDroppedRoutesDocumented(t *testing.T) {
	// 情况 1：顶层 upstream_routes 被忽略。
	raw := mustParse(t, `{"config_version":4,"local_api_key":"k",
		"upstream_routes":{"https://api.openai.com":{"anthropic":"top-prefix"}},
		"providers":{"openai":{"base_url":"https://api.openai.com","keys":{"main":{"api_key":"a"}}}},
		"models":{"m":{"targets":[{"provider":"openai","key":"main","upstream_model":"u"}]}}}`)
	cfg, err := FromDict(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if routes := cfg.UpstreamRoutesForBaseURL("https://api.openai.com"); len(routes) != 0 {
		t.Errorf("顶层 upstream_routes 应被忽略，实际得到 %v", routes)
	}

	// 情况 2：provider key 级 upstream_routes 被忽略。
	raw = mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"openai":{"base_url":"https://api.openai.com","keys":{"main":{"api_key":"a","upstream_routes":{"anthropic":"key-prefix"}}}}},
		"models":{"m":{"targets":[{"provider":"openai","key":"main","upstream_model":"u"}]}}}`)
	cfg, err = FromDict(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfg.Models) != 1 || len(cfg.Models[0].Keys) != 1 {
		t.Fatalf("解析结果结构异常: %+v", cfg.Models)
	}
	if routes := cfg.Models[0].Keys[0].UpstreamRoutes; len(routes) != 0 {
		t.Errorf("key 级 upstream_routes 应恒为空，实际得到 %v", routes)
	}

	// 对照组：provider 级 routes 确实生效（证明上面的空值不是「都没生效」）。
	raw = mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"openai":{"base_url":"https://api.openai.com","keys":{"main":{"api_key":"a"}},"routes":{"anthropic":"provider-prefix"}}},
		"models":{"m":{"targets":[{"provider":"openai","key":"main","upstream_model":"u"}]}}}`)
	cfg, err = FromDict(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	routes := cfg.UpstreamRoutesForBaseURL("https://api.openai.com")
	if routes["anthropic"] != "provider-prefix/v1/messages" {
		t.Errorf("provider 级 routes 应生效，实际得到 %v", routes)
	}
}

// TestModelCorpusCoversEveryCase 防止语料读取被静默截断。
//
// 生成脚本产出 109 条；条数偏少说明读取中途失败（例如 bufio.Scanner 的 64KB
// 行长上限），而那种失败只会表现为「少测了几条」，不会报错。
func TestModelCorpusCoversEveryCase(t *testing.T) {
	const expected = 109
	if entries := loadCorpus(t, "model.jsonl"); len(entries) != expected {
		t.Errorf("语料条数 %d，期望 %d", len(entries), expected)
	}
}
