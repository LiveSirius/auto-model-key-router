package keypool

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// corpusCase 是 scripts/gen_keypool_corpus.py 产出的一条对拍用例。
type corpusCase struct {
	Name    string `json:"name"`
	Input   string `json:"input"`
	Ops     string `json:"ops"`
	Results string `json:"results"`
	Note    string `json:"note"`
}

// op 是一次操作。
type op struct {
	Op          string   `json:"op"`
	Model       string   `json:"model"`
	Key         string   `json:"key"`
	Excluded    []string `json:"excluded"`
	VisitorOnly bool     `json:"visitor_only"`
	AffinityKey *string  `json:"affinity_key"`
	StatusCode  *int     `json:"status_code"`
	RetryAfter  *float64 `json:"retry_after"`
	Path        string   `json:"path"`
	// HasKey 标记 op 是否显式带了 "key" 字段。
	//
	// 这是必需的：`key: null`（未指定）与 `key: ""`（显式空 key）在参照实现里
	// 行为不同，而 Go 的 json 反序列化无法区分「字段缺失」与「字段为 null」，
	// 因此额外记录字段是否存在。
	HasKey bool `json:"has_key"`
}

// result 是参照实现在一步操作后的可观察结果。
type result struct {
	Op        string          `json:"op"`
	OK        bool            `json:"ok"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Error     string          `json:"error"`
	ErrorType string          `json:"error_type"`
}

// loadCorpus 读取对拍语料。
func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()
	path := filepath.Join("testdata", "selection.jsonl")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开语料失败: %v", err)
	}
	defer func() { _ = file.Close() }()

	var cases []corpusCase
	scanner := bufio.NewScanner(file)
	// 单条用例（含随机序列）可能很长，放宽缓冲上限。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item corpusCase
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatalf("解析语料失败: %v", err)
		}
		cases = append(cases, item)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("语料为空")
	}
	return cases
}

// parseOps 解析操作序列，并补出 HasKey。
func parseOps(t *testing.T, raw string) []op {
	t.Helper()
	var probes []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probes); err != nil {
		t.Fatalf("解析操作序列失败: %v", err)
	}
	ops := make([]op, 0, len(probes))
	for _, probe := range probes {
		encoded, err := json.Marshal(probe)
		if err != nil {
			t.Fatalf("重编码操作失败: %v", err)
		}
		var item op
		if err := json.Unmarshal(encoded, &item); err != nil {
			t.Fatalf("解析操作失败: %v", err)
		}
		_, item.HasKey = probe["key"]
		ops = append(ops, item)
	}
	return ops
}

// TestKeyPoolMatchesPython 重放语料，逐条断言与参照实现一致。
func TestKeyPoolMatchesPython(t *testing.T) {
	// 时钟固定为一个常数：语料里的冷却状态是「刚标记就查询」，只要时间不前进，
	// 冷却判定就与 Python 侧在同一瞬间做出，结果可比。
	fixedClock := func() float64 { return 1_000_000.0 }

	corpus := loadCorpus(t)
	var assertions int

	for _, testCase := range corpus {
		t.Run(testCase.Name, func(t *testing.T) {
			configValue, err := canonical.ParseString(testCase.Input)
			if err != nil {
				t.Fatalf("解析配置失败: %v", err)
			}
			routerConfig, err := config.FromDict(configValue)
			if err != nil {
				t.Fatalf("构造 RouterConfig 失败: %v", err)
			}
			pool := New(routerConfig, nil, fixedClock)

			var want []result
			if err := json.Unmarshal([]byte(testCase.Results), &want); err != nil {
				t.Fatalf("解析期望结果失败: %v", err)
			}
			ops := parseOps(t, testCase.Ops)
			if len(ops) != len(want) {
				t.Fatalf("操作数与期望数不一致: %d vs %d", len(ops), len(want))
			}

			for index, operation := range ops {
				expected := want[index]
				step := index + 1
				got, err := replay(pool, operation, expected)
				assertions++
				if expected.OK {
					if err != nil {
						t.Fatalf("第 %d 步 %s: 参照成功但 Go 报错: %v",
							step, operation.Op, err)
					}
					if got != expectedKeyOf(expected) {
						t.Fatalf("第 %d 步 %s: 期望 %q，实际 %q",
							step, operation.Op, expectedKeyOf(expected), got)
					}
					continue
				}
				if err == nil {
					t.Fatalf("第 %d 步 %s: 参照报错 %s(%s) 但 Go 成功",
						step, operation.Op, expected.ErrorType, expected.Error)
				}
				if err.Error() != expected.Error {
					t.Fatalf("第 %d 步 %s: 期望错误 %q，实际 %q",
						step, operation.Op, expected.Error, err.Error())
				}
				assertErrorClass(t, step, operation.Op, expected.ErrorType, err)
			}
		})
	}
	t.Logf("共重放 %d 个断言", assertions)
}

// expectedKeyOf 取出期望结果的关键返回值。
//
// 对于 next/acquire 等返回 key 的操作，Python 侧把 key 放在顶层 "key" 字段；
// 其它操作用 "value"。这里统一成字符串，便于比较（unified_route 之类不是
// 字符串的操作返回空串，由 TestValueOpcodesMatchPython 单独断言）。
func expectedKeyOf(item result) string { return item.Key }

// assertErrorClass 断言 Go 的错误分类与 Python 异常类型对应。
func assertErrorClass(t *testing.T, step int, name, pythonType string, err error) {
	t.Helper()
	switch pythonType {
	case "KeyError":
		if !errors.Is(err, ErrUnknownModel) && !errors.Is(err, ErrNoUnifiedModel) {
			t.Fatalf("第 %d 步 %s: 期望 KeyError 语义，实际 %v", step, name, err)
		}
	case "RuntimeError":
		if !errors.Is(err, ErrNoUsableKey) {
			t.Fatalf("第 %d 步 %s: 期望 RuntimeError 语义，实际 %v", step, name, err)
		}
	}
}

// replay 执行一步操作并返回其关键结果。
func replay(pool *KeyPool, operation op, expected result) (string, error) {
	switch operation.Op {
	case "next":
		key, err := pool.NextKey(
			operation.Model, operation.Excluded,
			operation.VisitorOnly, derefOr(operation.AffinityKey, ""),
		)
		if err != nil {
			return "", err
		}
		return key.Name, nil
	case "acquire":
		pool.AcquireKey(operation.Model, operation.Key)
		return "", nil
	case "release":
		pool.ReleaseKey(operation.Model, operation.Key)
		return "", nil
	case "success":
		pool.MarkSuccess(operation.Model, operation.Key)
		return "", nil
	case "failure":
		pool.MarkFailure(operation.Model, operation.Key,
			operation.StatusCode, operation.RetryAfter)
		return "", nil
	case "cooling":
		if pool.IsCoolingDown(operation.Model, operation.Key) != expectedBool(expected) {
			return "", errMismatch
		}
		return "", nil
	case "active_count":
		if pool.ActiveCount(operation.Model, operation.Key) != expectedInt(expected) {
			return "", errMismatch
		}
		return "", nil
	case "model_ids":
		if !equalStrings(pool.ModelIDs(), expectedStrings(expected)) {
			return "", errMismatch
		}
		return "", nil
	case "public_ids":
		if !equalStrings(pool.PublicModelIDs(), expectedStrings(expected)) {
			return "", errMismatch
		}
		return "", nil
	case "hidden_ids":
		if !equalStrings(pool.HiddenModelIDs(), expectedStrings(expected)) {
			return "", errMismatch
		}
		return "", nil
	case "available_ids":
		got := pool.AvailableModelIDs(operation.VisitorOnly)
		if !equalStrings(got, expectedStrings(expected)) {
			return "", errMismatch
		}
		return "", nil
	case "resolve_model":
		if pool.ResolveModelID(operation.Model) != expectedScalar(expected) {
			return "", errMismatch
		}
		return "", nil
	case "resolve_visitor":
		got, _ := pool.ResolveVisitorModelID(operation.Model)
		if got != expectedScalar(expected) {
			return "", errMismatch
		}
		return "", nil
	case "routing_mode":
		if pool.RoutingMode(operation.Model) != expectedScalar(expected) {
			return "", errMismatch
		}
		return "", nil
	case "key_count":
		if pool.KeyCount(operation.Model) != expectedInt(expected) {
			return "", errMismatch
		}
		return "", nil
	case "visitor_key_count":
		if pool.VisitorKeyCount(operation.Model) != expectedInt(expected) {
			return "", errMismatch
		}
		return "", nil
	case "resolve_route":
		var keyName *string
		if operation.HasKey {
			key := operation.Key
			keyName = &key
		}
		modelID, key, err := pool.ResolveRoute(operation.Model, keyName, operation.Path)
		if err != nil {
			return "", err
		}
		if !equalRouteValue(modelID, key, expected) {
			return "", errMismatch
		}
		return "", nil
	case "task_plan":
		plan, found := pool.TaskPlan(operation.Model)
		if !equalTaskPlan(plan, found, expected) {
			return "", errMismatch
		}
		return "", nil
	case "task_params":
		params := pool.TaskParams(operation.Model)
		if !equalCanonical(params, expected) {
			return "", errMismatch
		}
		return "", nil
	case "unified_route":
		got := pool.UnifiedRoute()
		if !equalCanonical(got, expected) {
			return "", errMismatch
		}
		return "", nil
	}
	return "", errMismatch
}

// errMismatch 表示返回值与语料不符。
//
// 用固定错误而非 t.Fatalf，是为了让 replay 保持纯函数便于阅读；上层会把具体
// 差异打印出来（见 TestValueOpcodesMatchPython 的详细路径）。
var errMismatch = errors.New("返回值与语料不一致")

// derefOr 解引用可选字符串，nil 时返回默认值。
func derefOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

// expectedBool 取出期望的布尔值。
func expectedBool(item result) bool {
	var value bool
	_ = json.Unmarshal(item.Value, &value)
	return value
}

// expectedInt 取出期望的整数。
func expectedInt(item result) int {
	var value int
	_ = json.Unmarshal(item.Value, &value)
	return value
}

// expectedScalar 取出期望的标量；null 归一成空串。
func expectedScalar(item result) string {
	var value *string
	_ = json.Unmarshal(item.Value, &value)
	if value == nil {
		return ""
	}
	return *value
}

// expectedStrings 取出期望的字符串数组。
func expectedStrings(item result) []string {
	var value []string
	_ = json.Unmarshal(item.Value, &value)
	return value
}

// expectedValues 取出期望的原始值。
func expectedValues(item result) []*canonical.Value {
	var raw []json.RawMessage
	if err := json.Unmarshal(item.Value, &raw); err != nil {
		return nil
	}
	values := make([]*canonical.Value, 0, len(raw))
	for _, item := range raw {
		parsed, err := canonical.Parse(item)
		if err != nil {
			return nil
		}
		values = append(values, parsed)
	}
	return values
}

// equalStrings 比较两个字符串切片。
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// equalRouteValue 比较 resolve_route 的结果。
//
// 语料里 key 位置可能是 null 或 ""（Python 区分两者，Go 统一成 ""），因此这里
// 把 null 与 "" 视为等价——差异只在这一个维度，且下游行为一致（见
// ResolveRoute 的注释）。
func equalRouteValue(modelID, key string, item result) bool {
	values := expectedValues(item)
	if len(values) != 2 {
		return false
	}
	if values[0].PyStr() != modelID {
		return false
	}
	return canonicalKeyText(values[1]) == key
}

// canonicalKeyText 把语料里的 key 值归一成字符串：null 与 "" 都表示「无 key」。
//
// 不能直接用 PyStr：Python 的 str(None) 是 "None"，会把 null 误判成字面量
// "None"，从而与 Go 的 "" 不匹配。
func canonicalKeyText(value *canonical.Value) string {
	if value.Kind == canonical.KindNull {
		return ""
	}
	return value.PyStr()
}

// equalTaskPlan 比较任务计划；found=false 时期望必须是 null。
func equalTaskPlan(plan config.RoutePlan, found bool, item result) bool {
	trimmed := strings.TrimSpace(string(item.Value))
	if trimmed == "null" {
		return !found
	}
	if !found {
		return false
	}
	values := expectedValues(item)
	_ = values
	// 语料里 task_plan 是 {primary:{model,key}, fallback:...} 结构。
	var decoded struct {
		Primary struct {
			Model string  `json:"model"`
			Key   *string `json:"key"`
		} `json:"primary"`
		Fallback *struct {
			Model string  `json:"model"`
			Key   *string `json:"key"`
		} `json:"fallback"`
	}
	if err := json.Unmarshal(item.Value, &decoded); err != nil {
		return false
	}
	if decoded.Primary.Model != plan.Primary.Model {
		return false
	}
	if derefOr(decoded.Primary.Key, "") != plan.Primary.Key {
		return false
	}
	if (decoded.Fallback == nil) != (plan.Fallback == nil) {
		return false
	}
	if decoded.Fallback != nil && plan.Fallback != nil {
		if decoded.Fallback.Model != plan.Fallback.Model {
			return false
		}
		if derefOr(decoded.Fallback.Key, "") != plan.Fallback.Key {
			return false
		}
	}
	return true
}

// equalCanonical 比较 Go 侧 canonical 值与语料里的期望值。
//
// 用 canonical.Dumps 做比较而非直接比较结构：Go 的 Value 是带 map 的结构体，
// 直接比较需要手写递归，序列化成规范形式更短也更不容易漏字段。
func equalCanonical(got *canonical.Value, item result) bool {
	var want *canonical.Value
	parsed, err := canonical.Parse(item.Value)
	if err != nil {
		return false
	}
	want = parsed
	return canonical.Dumps(got) == canonical.Dumps(want)
}
