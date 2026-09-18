package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// cachePathPlaceholder 是 model.jsonl 输出里「平台缓存目录 + 目录分隔符」的占位符。
//
// 语料由 Windows 上的参照实现产出，因此省略 endpoint_capabilities_path /
// metrics_db_path / log_file_path 时录制下来的是**生成机的缓存布局**
// （`C:\Users\<user>\AppData\Local\AutoModelKeyRouter\`）。那部分不是契约：契约是
// 「这三个字段缺失或为空时回落到 config.DefaultCacheDir()」。回放时把占位符换成
// 本平台解析出的缓存目录（连同平台目录分隔符），断言才在三个平台上都成立。
//
// 生成器已随 Python 退役移除，语料是**手工**把 Windows 前缀替换成这个占位符的。
const cachePathPlaceholder = "<cache>"

// TestFromDictMatchesPython 是 RouterConfig 解析与校验的核心对拍断言。
//
// 语料由 gen_config_model_corpus.py（已随 Python 退役移除） 生成，输入是原始配置 JSON 文本
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
			want := expectedConfigOutput(t, entry.Output)
			if got != want {
				t.Errorf("输出不一致\n期望: %s\n实际: %s", want, got)
			}
		})
	}
}

// expectedConfigOutput 把语料输出里的缓存目录占位符换成本平台解析出的目录。
//
// 只替换占位符，其余字节（键序、浮点写法、空值形态）照旧逐字节比对。
func expectedConfigOutput(t *testing.T, output string) string {
	t.Helper()
	if !strings.Contains(output, cachePathPlaceholder) {
		return output
	}
	cacheDir, err := DefaultCacheDir()
	if err != nil {
		t.Fatalf("解析平台缓存目录失败: %v", err)
	}
	// 占位符位于 JSON 字符串内部，替换值必须按 JSON 规则转义（Windows 路径里
	// 的反斜杠在语料文本中是 `\\`）。
	escaped := strings.ReplaceAll(cacheDir+string(filepath.Separator), `\`, `\\`)
	return strings.ReplaceAll(output, cachePathPlaceholder, escaped)
}

// serializeConfig 把 RouterConfig 摊平成与语料一致的结构。
//
// 这是**测试契约**：字段名与嵌套形状必须与 gen_config_model_corpus.py（已随 Python 退役移除） 的
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

// workspaceConfig 是一份同时带顶层 tasks 与 workspaces 的最小可用配置。
const workspaceConfig = `{"config_version":4,"local_api_key":"k",
	"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
	"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]},"model-b":{"targets":[{"provider":"p","key":"k"}]}},
	"tasks":{"shared":{"model":"model-a"}},
	"workspaces":{"teamA":{"tasks":{"shared":{"model":"model-b"},"only-a":{"model":"model-a"}}},"empty":{}}}`

// TestWorkspacesIsolateTaskNames 固化工作空间的核心语义：任务名只在空间内唯一。
//
// 三件事必须同时成立，缺任何一条「隔离」都是假的：
//  1. 同名任务在两个空间里都能解析，且各自指向自己的模型；
//  2. 顶层 tasks 归属 DefaultWorkspace（既有配置不带 workspaces 也照常工作）；
//  3. 空工作空间被保留——否则新建空工作空间落盘后立刻消失。
func TestWorkspacesIsolateTaskNames(t *testing.T) {
	cfg, err := FromDict(mustParse(t, workspaceConfig))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if len(cfg.Tasks) != 3 {
		t.Fatalf("任务数应为 3，实际 %d: %+v", len(cfg.Tasks), cfg.Tasks)
	}
	defaultTask, found := cfg.TaskForWorkspace(DefaultWorkspace, "shared")
	if !found || defaultTask.Model != "model-a" {
		t.Errorf("默认空间的 shared 应指向 model-a，实际 %+v（found=%v）", defaultTask, found)
	}
	teamTask, found := cfg.TaskForWorkspace("teamA", "shared")
	if !found || teamTask.Model != "model-b" {
		t.Errorf("teamA 的 shared 应指向 model-b，实际 %+v（found=%v）", teamTask, found)
	}
	if _, found := cfg.TaskForWorkspace("teamA", "shared-only-missing"); found {
		t.Error("不存在的工作空间任务不应被找到")
	}
	// 跨空间不可见：only-a 只在 teamA 里。
	if _, found := cfg.TaskForWorkspace(DefaultWorkspace, "only-a"); found {
		t.Error("teamA 的任务不应出现在默认空间")
	}

	// 工作空间清单由任务反推：默认空间在首位，其余按任务出现顺序；`empty` 是个
	// 空分组（里面没有任务），因此**不出现**——没有任务的分组既不可观测也没有意义。
	want := []string{"default", "teamA"}
	got := cfg.WorkspaceNames()
	if len(got) != len(want) {
		t.Fatalf("工作空间清单 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("工作空间清单 %v，期望 %v", got, want)
		}
	}
}

// TestWorkspacesOptional 固化兼容性：没有 workspaces 段的既有配置行为不变。
func TestWorkspacesOptional(t *testing.T) {
	cfg, err := FromDict(mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},
		"tasks":{"t":{"model":"model-a"}}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if names := cfg.WorkspaceNames(); len(names) != 1 || names[0] != DefaultWorkspace {
		t.Errorf("应只有默认工作空间，实际 %v", names)
	}
	task, found := cfg.TaskFor("t")
	if !found || task.Workspace != DefaultWorkspace {
		t.Errorf("顶层任务应归属默认空间，实际 %+v（found=%v）", task, found)
	}
}

// TestWorkspacesRejectsBadShapes 固化工作空间段的错误文本与触发条件。
func TestWorkspacesRejectsBadShapes(t *testing.T) {
	base := `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},"tasks":{},`
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"非对象", `"workspaces":[]}`, "workspaces 必须是对象"},
		{"空间非对象", `"workspaces":{"teamA":[]}}`, "工作空间 teamA 必须是对象"},
		{"空间名为空", `"workspaces":{"  ":{"tasks":{}}}}`, "工作空间名不能为空"},
		{"与默认空间重名", `"workspaces":{"default":{"tasks":{}}}}`, "工作空间名重复: default"},
		{"tasks 非对象", `"workspaces":{"teamA":{"tasks":[]}}}`, "workspaces.teamA.tasks 必须是对象"},
		{
			"模型未配置",
			`"workspaces":{"teamA":{"tasks":{"t":{"model":"nope"}}}}}`,
			"workspaces.teamA.tasks.t.model 引用了未配置的模型: nope",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := FromDict(mustParse(t, base+testCase.payload))
			if err == nil {
				t.Fatalf("应当报错 %q", testCase.want)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("期望 ConfigError，实际 %T: %v", err, err)
			}
			if configErr.Message != testCase.want {
				t.Errorf("错误文本不一致\n期望: %s\n实际: %s", testCase.want, configErr.Message)
			}
		})
	}
}

// TestTaskNameConflictsWithModelGlobally 固化：工作空间不能用来遮蔽模型名。
//
// 否则「任务名解析成哪个模型」会取决于调用方是否带了工作空间头，同一个名字在
// 不同入口指向不同东西。
func TestTaskNameConflictsWithModelGlobally(t *testing.T) {
	_, err := FromDict(mustParse(t, `{"config_version":4,"local_api_key":"k",
		"providers":{"p":{"base_url":"https://a.example.test","keys":{"k":{"api_key":"a"}}}},
		"models":{"model-a":{"targets":[{"provider":"p","key":"k"}]}},
		"workspaces":{"teamA":{"tasks":{"model-a":{"model":"model-a"}}}}}`))
	if err == nil {
		t.Fatal("工作空间内的任务名撞模型名时应当报错")
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) || configErr.Message != "任务名与模型名称冲突: model-a" {
		t.Fatalf("错误文本不一致: %v", err)
	}
}
