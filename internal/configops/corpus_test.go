package configops

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// corpusFile 是 gen_configops_corpus.py（已随 Python 退役移除） 产出的对拍语料。
type corpusFile struct {
	Version int          `json:"version"`
	Cases   []corpusCase `json:"cases"`
}

// corpusCase 是一条用例：初始配置 + 调用参数 + 参照结果（含调用后的配置）。
type corpusCase struct {
	Name string `json:"name"`
	// Call 是参照实现的函数名（Python 的 __name__）。
	Call string `json:"call"`
	// Data 是初始配置的 JSON 文本（保留键插入顺序）。
	Data string `json:"data"`
	// Args 是关键字参数的 JSON 对象（保留键插入顺序，update_settings 依赖它）。
	Args string `json:"args"`
	// Note 是人工备注；空表示没有。
	Note    string        `json:"note"`
	Outcome corpusOutcome `json:"outcome"`
}

// corpusOutcome 是一次调用的结果。
type corpusOutcome struct {
	OK bool `json:"ok"`
	// Result 是成功时的返回值（null 表示 Python 返回 None）。
	Result json.RawMessage `json:"result"`
	// Data 是**调用之后**的配置，成功与失败都记录。
	//
	// 失败路径同样要断言：参照实现的失败几乎都不是原子的（先 setdefault 再报错、
	// 先改 base_url 再抛 ValueError），只比对异常文本会漏掉这些差异。
	Data      string `json:"data"`
	ErrorType string `json:"error_type"`
	Error     string `json:"error"`
	// StatusCode 只对 ConfigOperationError 有值（其余为 null，即 Python 的 500）。
	StatusCode *int `json:"status_code"`
}

// loadConfigOpsCorpus 读取对拍语料。
func loadConfigOpsCorpus(t *testing.T) []corpusCase {
	t.Helper()
	path := filepath.Join("testdata", "configops_corpus.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取语料失败（语料已冻结并随仓库提交，生成器已随 Python 退役移除）: %v", err)
	}
	var corpus corpusFile
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	if corpus.Version != 1 {
		t.Fatalf("语料版本不支持: %d", corpus.Version)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("语料为空")
	}
	return corpus.Cases
}

// TestConfigOpsMatchesPython 重放语料，逐条断言与参照实现一致。
//
// 每条用例断言三件事：
//
//  1. 成功/失败与参照一致；
//  2. 错误文本逐字一致，且错误类型分类一致（是不是 ConfigOperationError），
//     ConfigOperationError 还要比对 status_code；
//  3. 调用后的整份配置逐字节一致（canonical.DumpsOrdered，即紧凑且保留键序）。
func TestConfigOpsMatchesPython(t *testing.T) {
	corpus := loadConfigOpsCorpus(t)

	for _, testCase := range corpus {
		t.Run(testCase.Name, func(t *testing.T) {
			data, err := canonical.ParseString(testCase.Data)
			if err != nil {
				t.Fatalf("解析初始配置失败: %v", err)
			}
			args, err := canonical.ParseString(testCase.Args)
			if err != nil {
				t.Fatalf("解析参数失败: %v", err)
			}

			got, err := dispatch(t, testCase.Call, data, args)
			if !testCase.Outcome.OK {
				if err == nil {
					t.Fatalf("参照报错 %s(%s) 但 Go 成功", testCase.Outcome.ErrorType, testCase.Outcome.Error)
				}
				if err.Error() != testCase.Outcome.Error {
					t.Fatalf("错误文本不一致\n参照: %q\n实际: %q", testCase.Outcome.Error, err.Error())
				}
				assertErrorClass(t, testCase.Outcome, err)
			} else {
				if err != nil {
					t.Fatalf("参照成功但 Go 报错: %v", err)
				}
				want, parseErr := canonical.Parse(testCase.Outcome.Result)
				if parseErr != nil {
					t.Fatalf("解析期望返回值失败: %v", parseErr)
				}
				if diff := diffCanonical(got, want); diff != "" {
					t.Errorf("返回值不一致: %s", diff)
				}
			}

			if diff := diffCanonical(data, mustParseText(t, testCase.Outcome.Data)); diff != "" {
				t.Errorf("调用后的配置不一致: %s", diff)
			}
		})
	}
	t.Logf("共重放 %d 条语料", len(corpus))
}

// TestCorpusCoversEveryDispatchedCall 确认语料覆盖了全部已知调用名，反之亦然。
//
// 这条测试的价值在于防漂移：新增一个被移植的函数却忘了加语料（或加了语料却忘了
// 在 dispatch 里接上）时，它会立刻失败，而不是让对拍静默地少测一块。
func TestCorpusCoversEveryDispatchedCall(t *testing.T) {
	corpus := loadConfigOpsCorpus(t)
	seen := map[string]int{}
	for _, testCase := range corpus {
		seen[testCase.Call]++
	}
	for call := range dispatchNames {
		if seen[call] == 0 {
			t.Errorf("语料没有覆盖 %s", call)
		}
	}
	for call := range seen {
		if !dispatchNames[call] {
			t.Errorf("语料里的 %s 没有对应的 dispatch 分支", call)
		}
	}
	if len(seen) < 30 {
		t.Errorf("覆盖的调用过少: %d", len(seen))
	}
}

// diffCanonical 用 canonical 的紧凑有序形式比较两个值，返回空串表示一致。
func diffCanonical(got, want *canonical.Value) string {
	gotText := canonical.DumpsOrdered(got)
	wantText := canonical.DumpsOrdered(want)
	if gotText == wantText {
		return ""
	}
	return "\n期望 " + wantText + "\n实际 " + gotText
}

// mustParseText 解析语料里的 JSON 文本。
func mustParseText(t *testing.T, text string) *canonical.Value {
	t.Helper()
	value, err := canonical.ParseString(text)
	if err != nil {
		t.Fatalf("解析 JSON 失败: %v", err)
	}
	return value
}

// assertErrorClass 断言 Go 的错误分类与 Python 异常类型对应。
func assertErrorClass(t *testing.T, outcome corpusOutcome, err error) {
	t.Helper()
	var opErr *ConfigOperationError
	isOpErr := errors.As(err, &opErr)
	if outcome.ErrorType == "ConfigOperationError" {
		if !isOpErr {
			t.Fatalf("参照抛 ConfigOperationError 但 Go 抛的是 %T: %v", err, err)
		}
		if outcome.StatusCode == nil {
			t.Fatal("语料缺少 status_code")
		}
		if opErr.StatusCode != *outcome.StatusCode {
			t.Fatalf("status_code 期望 %d，实际 %d", *outcome.StatusCode, opErr.StatusCode)
		}
		return
	}
	if isOpErr {
		t.Fatalf("参照抛 %s（非 ConfigOperationError）但 Go 折成了 ConfigOperationError(%d)",
			outcome.ErrorType, opErr.StatusCode)
	}
	if got := pythonErrorType(err); got != outcome.ErrorType {
		t.Fatalf("异常类型期望 %s，实际 %s（%v）", outcome.ErrorType, got, err)
	}
}

// pythonErrorType 把 Go 错误映射回它对应的 Python 异常类名。
//
// 这是一张「语义对照表」：config.ConfigError 对应 Python 的 ValueError，
// InternalError 对应那些不该出现的 TypeError/AttributeError，本包的 PyError
// 自带类名。
func pythonErrorType(err error) string {
	var (
		opErr       *ConfigOperationError
		pyErr       *PyError
		configErr   *config.ConfigError
		internalErr *config.InternalError
		valueErr    *canonical.ValueError
		typeErr     *canonical.TypeError
		notIterable *canonical.NotIterableError
		overflowErr *canonical.OverflowError
	)
	switch {
	case errors.As(err, &opErr):
		return "ConfigOperationError"
	case errors.As(err, &pyErr):
		return pyErr.TypeName
	case errors.As(err, &configErr):
		return "ValueError"
	case errors.As(err, &valueErr):
		return "ValueError"
	case errors.As(err, &typeErr):
		return "TypeError"
	case errors.As(err, &notIterable):
		return "TypeError"
	case errors.As(err, &overflowErr):
		return "OverflowError"
	case errors.As(err, &internalErr):
		return "InternalError"
	}
	return "?"
}

// --------------------------------------------------------------------------- #
// 参数解码
// --------------------------------------------------------------------------- #

// argRaw 取出参数对象里的原始值；键缺失或为 null 时返回 nil。
func argRaw(args *canonical.Value, key string) *canonical.Value {
	if args == nil || !args.IsObject() {
		return nil
	}
	value, ok := args.LookupOK(key)
	if !ok || value.IsNull() {
		return nil
	}
	return value
}

// argString 取字符串参数；缺失时为 ""（对应 Python 的默认值或必填校验）。
//
// 用 StringValue 而不是 AsString：参照实现里字符串参数一律走
// `str(value or "").strip()`，数字 5 会被渲染成 "5"。
func argString(args *canonical.Value, key string) string {
	value := argRaw(args, key)
	if value == nil {
		return ""
	}
	return value.StringValue()
}

// argStringPtr 取可选字符串参数；null 与缺失都返回 nil（Python None）。
func argStringPtr(args *canonical.Value, key string) *string {
	value := argRaw(args, key)
	if value == nil {
		return nil
	}
	text := value.StringValue()
	return &text
}

// argBool 取布尔参数（Python 的 bool(value)，缺失为 false）。
func argBool(args *canonical.Value, key string) bool {
	value := argRaw(args, key)
	if value == nil {
		return false
	}
	return value.Truthy()
}

// argBoolPtr 取可选布尔参数；null 与缺失都返回 nil。
func argBoolPtr(args *canonical.Value, key string) *bool {
	value := argRaw(args, key)
	if value == nil {
		return nil
	}
	flag := value.Truthy()
	return &flag
}

// argInt 取整数参数，缺失为 0。
func argInt(t *testing.T, args *canonical.Value, key string) int {
	t.Helper()
	value := argRaw(args, key)
	if value == nil {
		return 0
	}
	number, ok := value.AsInt()
	if !ok {
		t.Fatalf("参数 %s 不是整数: %s", key, canonical.DumpsOrdered(value))
	}
	return int(number)
}

// argValue 取单个 JSON 参数；null 与缺失都返回 nil。
func argValue(args *canonical.Value, key string) *canonical.Value {
	return argRaw(args, key)
}

// argValues 取 JSON 数组参数；null 与缺失返回 nil，空数组返回非 nil 空切片
// （参照实现区分 targets=None 与 targets=[]）。
func argValues(t *testing.T, args *canonical.Value, key string) []*canonical.Value {
	t.Helper()
	value := argRaw(args, key)
	if value == nil {
		return nil
	}
	if !value.IsArray() {
		t.Fatalf("参数 %s 期望数组: %s", key, canonical.DumpsOrdered(value))
	}
	return value.Items()
}

// --------------------------------------------------------------------------- #
// 调用分发
// --------------------------------------------------------------------------- #

// dispatchNames 是 dispatch 支持的全部调用名，供覆盖率测试比对。
var dispatchNames = map[string]bool{
	"providers": true, "models": true, "provider_keys": true, "model_targets": true,
	"require_provider": true, "require_key": true, "require_model": true,
	"fallback_model_id": true, "normalize_base_url": true,
	"create_provider": true, "update_provider": true, "create_provider_key": true,
	"update_provider_key": true, "delete_provider_key": true, "delete_provider": true,
	"provider_id_for_base_url": true, "set_upstream_routes_for_base_url": true,
	"validate_targets": true, "add_model_target": true, "update_model_target": true,
	"delete_model_target": true, "create_model": true, "update_model": true,
	"delete_model": true, "create_model_key": true, "create_model_with_keys": true,
	"update_model_key_local": true, "delete_model_key_local": true, "delete_model_key": true,
	"key_service_models": true, "set_key_service_models": true,
	"replace_unified_model_name": true, "set_unified_model": true,
	"switch_unified_target": true, "repair_unified_model": true,
	"repair_model_references": true, "regenerate_local_api_key": true,
	"update_settings": true, "existing_tasks": true, "require_task": true,
	"create_task": true, "update_task": true, "delete_task": true, "repair_tasks": true,
	"transferable_config": true, "merge_transferable_config": true,
}

// dispatch 按语料里的调用名执行对应的 Go 实现，并把返回值编码成 canonical 值。
//
// 返回值统一成 *canonical.Value 是为了和语料里的 JSON 直接比对；Python 返回
// None 时返回 null 值。
func dispatch(t *testing.T, call string, data, args *canonical.Value) (*canonical.Value, error) {
	t.Helper()
	null := canonical.NewNull()
	switch call {
	case "providers":
		return Providers(data)
	case "models":
		return Models(data)
	case "provider_keys":
		return ProviderKeys(data)
	case "model_targets":
		return ModelTargets(data)
	case "require_provider":
		return RequireProvider(data, argString(args, "provider_id"))
	case "require_key":
		return RequireKey(data, argString(args, "key_name"))
	case "require_model":
		return RequireModel(data, argString(args, "model_id"))
	case "fallback_model_id":
		id, found, err := FallbackModelID(data)
		if err != nil || !found {
			return null, err
		}
		return canonical.NewString(id), nil
	case "normalize_base_url":
		normalized, err := NormalizeBaseURL(data)
		if err != nil {
			return null, err
		}
		return canonical.NewString(normalized), nil
	case "create_provider":
		return CreateProvider(data, argString(args, "provider_id"), argString(args, "base_url"))
	case "update_provider":
		id, err := UpdateProvider(data, argString(args, "provider_id"), UpdateProviderOptions{
			NewID:        argStringPtr(args, "new_id"),
			BaseURL:      argStringPtr(args, "base_url"),
			Routes:       argValue(args, "routes"),
			UpdateRoutes: argBool(args, "update_routes"),
		})
		if err != nil {
			return null, err
		}
		return canonical.NewString(id), nil
	case "create_provider_key":
		return CreateProviderKey(data, argString(args, "provider_id"), argString(args, "key_name"),
			argString(args, "api_key"), CreateProviderKeyOptions{
				Enabled:      argBoolPtr(args, "enabled"),
				AllowVisitor: argBool(args, "allow_visitor"),
			})
	case "update_provider_key":
		id, err := UpdateProviderKey(data, argString(args, "provider_id"), argString(args, "key_name"),
			UpdateProviderKeyOptions{
				NewName:      argStringPtr(args, "new_name"),
				APIKey:       argStringPtr(args, "api_key"),
				Enabled:      argBoolPtr(args, "enabled"),
				AllowVisitor: argBoolPtr(args, "allow_visitor"),
			})
		if err != nil {
			return null, err
		}
		return canonical.NewString(id), nil
	case "delete_provider_key":
		removed, err := DeleteProviderKey(data, argString(args, "provider_id"), argString(args, "key_name"))
		if err != nil {
			return null, err
		}
		return canonical.NewStringArray(removed), nil
	case "delete_provider":
		removed, err := DeleteProvider(data, argString(args, "provider_id"))
		if err != nil {
			return null, err
		}
		return canonical.NewStringArray(removed), nil
	case "provider_id_for_base_url":
		id, err := ProviderIDForBaseURL(data, argString(args, "base_url"))
		if err != nil {
			return null, err
		}
		return canonical.NewString(id), nil
	case "set_upstream_routes_for_base_url":
		err := SetUpstreamRoutesForBaseURL(data, argString(args, "base_url"), argValue(args, "routes"))
		return null, err
	case "validate_targets":
		return null, ValidateTargets(data, argValues(t, args, "targets"))
	case "add_model_target":
		err := AddModelTarget(data, argString(args, "model_id"), argValue(args, "target"))
		return null, err
	case "update_model_target":
		err := UpdateModelTarget(data, argString(args, "model_id"), argInt(t, args, "target_index"),
			argString(args, "upstream_model"))
		return null, err
	case "delete_model_target":
		return DeleteModelTarget(data, argString(args, "model_id"), argInt(t, args, "target_index"))
	case "create_model":
		return CreateModel(data, argString(args, "model_id"), CreateModelOptions{
			Aliases:         argStrings(args, "aliases"),
			RoutingMode:     argString(args, "routing_mode"),
			ReasoningEffort: argStringPtr(args, "reasoning_effort"),
			Targets:         argValues(t, args, "targets"),
			HiddenAliases:   argStrings(args, "hidden_aliases"),
		})
	case "update_model":
		id, err := UpdateModel(data, argString(args, "model_id"), UpdateModelOptions{
			NewID:                 argStringPtr(args, "new_id"),
			Aliases:               argStrings(args, "aliases"),
			RoutingMode:           argStringPtr(args, "routing_mode"),
			ReasoningEffort:       argStringPtr(args, "reasoning_effort"),
			UpdateReasoningEffort: argBool(args, "update_reasoning_effort"),
			Targets:               argValues(t, args, "targets"),
			HiddenAliases:         argStrings(args, "hidden_aliases"),
		})
		if err != nil {
			return null, err
		}
		return canonical.NewString(id), nil
	case "delete_model":
		return null, DeleteModel(data, argString(args, "model_id"))
	case "create_model_key":
		err := CreateModelKey(data, argString(args, "model_id"), argString(args, "key_name"),
			argString(args, "api_key"), CreateModelKeyOptions{
				BaseURL:              argStringPtr(args, "base_url"),
				Enabled:              argBoolPtr(args, "enabled"),
				AllowVisitor:         argBool(args, "allow_visitor"),
				UpstreamModel:        argStringPtr(args, "upstream_model"),
				UpstreamRoutes:       argValue(args, "upstream_routes"),
				UpdateUpstreamRoutes: argBool(args, "update_upstream_routes"),
			})
		return null, err
	case "create_model_with_keys":
		err := CreateModelWithKeys(data, argString(args, "model_id"), CreateModelOptions{
			Aliases:         argStrings(args, "aliases"),
			RoutingMode:     argString(args, "routing_mode"),
			ReasoningEffort: argStringPtr(args, "reasoning_effort"),
			HiddenAliases:   argStrings(args, "hidden_aliases"),
		}, argValues(t, args, "keys"))
		return null, err
	case "update_model_key_local":
		id, err := UpdateModelKey(data, argString(args, "model_id"), argString(args, "key_name"),
			UpdateModelKeyOptions{
				NewName:              argStringPtr(args, "new_name"),
				APIKey:               argStringPtr(args, "api_key"),
				BaseURL:              argStringPtr(args, "base_url"),
				UpdateBaseURL:        argBool(args, "update_base_url"),
				Enabled:              argBoolPtr(args, "enabled"),
				AllowVisitor:         argBoolPtr(args, "allow_visitor"),
				UpstreamRoutes:       argValue(args, "upstream_routes"),
				UpdateUpstreamRoutes: argBool(args, "update_upstream_routes"),
			})
		if err != nil {
			return null, err
		}
		return canonical.NewString(id), nil
	case "delete_model_key_local", "delete_model_key":
		return null, DeleteModelKey(data, argString(args, "model_id"), argString(args, "key_name"))
	case "key_service_models":
		models, err := KeyServiceModels(data, argString(args, "provider_id"), argString(args, "key_name"))
		if err != nil {
			return null, err
		}
		return canonical.NewStringArray(models), nil
	case "set_key_service_models":
		result, err := SetKeyServiceModels(data, argString(args, "provider_id"), argString(args, "key_name"),
			argStrings(args, "model_ids"))
		if err != nil {
			return null, err
		}
		return result.Value(), nil
	case "replace_unified_model_name":
		return null, ReplaceUnifiedModelName(data, argString(args, "old_name"), argString(args, "new_name"))
	case "set_unified_model":
		return null, SetUnifiedModel(data, argValue(args, "unified"))
	case "switch_unified_target":
		err := SwitchUnifiedTarget(data, argString(args, "target"), argStringPtr(args, "model_name"),
			argStringPtr(args, "key_name"), argBool(args, "update_key"))
		return null, err
	case "repair_unified_model":
		return null, RepairUnifiedModel(data)
	case "repair_model_references":
		return null, RepairModelReferences(data)
	case "regenerate_local_api_key":
		return null, RegenerateLocalAPIKey(data, argString(args, "api_key"))
	case "update_settings":
		return null, UpdateSettings(data, args)
	case "existing_tasks":
		return ExistingTasks(data), nil
	case "require_task":
		return RequireTask(data, argString(args, "task_name"))
	case "create_task":
		return CreateTask(data, argString(args, "task_name"), CreateTaskOptions{
			Model:         argString(args, "model"),
			FallbackModel: argStringPtr(args, "fallback_model"),
			Params:        argValue(args, "params"),
		})
	case "update_task":
		id, err := UpdateTask(data, argString(args, "task_name"), UpdateTaskOptions{
			Model:          argStringPtr(args, "model"),
			FallbackModel:  argStringPtr(args, "fallback_model"),
			UpdateFallback: argBool(args, "update_fallback"),
			Params:         argValue(args, "params"),
			UpdateParams:   argBool(args, "update_params"),
		})
		if err != nil {
			return null, err
		}
		return canonical.NewString(id), nil
	case "delete_task":
		return null, DeleteTask(data, argString(args, "task_name"))
	case "repair_tasks":
		removed, err := RepairTasks(data)
		if err != nil {
			return null, err
		}
		return canonical.NewStringArray(removed), nil
	case "transferable_config":
		return TransferableConfig(data, argBool(args, "include_visitor"))
	case "merge_transferable_config":
		result, err := MergeTransferableConfig(data, argValue(args, "transfer_data"))
		if err != nil {
			return null, err
		}
		return canonical.NewArray(
			result.Config,
			canonical.NewIntValue(int64(result.AddedModels)),
			canonical.NewIntValue(int64(result.AddedKeys)),
			canonical.NewIntValue(int64(result.SkippedKeys)),
		), nil
	}
	t.Fatalf("语料里的调用名没有 dispatch 分支: %s", call)
	return null, nil
}

// argStrings 取字符串数组参数；null 与缺失返回 nil，空数组返回非 nil 空切片。
func argStrings(args *canonical.Value, key string) []string {
	value := argRaw(args, key)
	if value == nil || !value.IsArray() {
		return nil
	}
	out := make([]string, 0, len(value.Arr))
	for _, item := range value.Arr {
		out = append(out, item.StringValue())
	}
	return out
}

// TestCorpusIsDiscriminating 确认语料本身不是「全是成功」的软柿子。
func TestCorpusIsDiscriminating(t *testing.T) {
	corpus := loadConfigOpsCorpus(t)
	failures, successes, withMutation := 0, 0, 0
	for _, testCase := range corpus {
		if testCase.Outcome.OK {
			successes++
		} else {
			failures++
		}
		if testCase.Outcome.Data != testCase.Data {
			withMutation++
		}
	}
	if successes < 100 || failures < 80 {
		t.Errorf("语料区分度不足: 成功 %d、失败 %d", successes, failures)
	}
	if withMutation < 100 {
		t.Errorf("记录到配置改动的用例过少: %d", withMutation)
	}
	if !strings.Contains(strings.Join(caseNames(corpus), "\n"), "set_unified_model_simple") {
		t.Error("语料缺少已知用例名")
	}
}

// caseNames 返回全部用例名，供上一条测试做抽样校验。
func caseNames(corpus []corpusCase) []string {
	names := make([]string, 0, len(corpus))
	for _, testCase := range corpus {
		names = append(names, testCase.Name)
	}
	return names
}
