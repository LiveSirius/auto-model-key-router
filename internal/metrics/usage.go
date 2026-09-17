package metrics

import (
	"sort"
	"unicode"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// Usage 是归一化后的 6 个 token 计数，对应 metrics.py 的 _normalize_usage 返回值。
type Usage struct {
	PromptTokens             int64
	CompletionTokens         int64
	TotalTokens              int64
	CachedTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// UsageStats 对应 metrics.py:42 的 UsageStats。
//
// MinDurationMS / MinFirstTokenMS 用指针区分「空集」与「真的出现过 0 毫秒」：
// SQL 的 MIN() 在空集上是 NULL，Python 侧是 None，而 to_dict() 再把它渲染成 0。
// 直接存 0 看起来等价，但 snapshot 的 caller_types 预置项与全空库会因此产生
// 无法分辨的状态，且与参照实现的数据模型不一致。
type UsageStats struct {
	Requests                 int64
	Successes                int64
	Failures                 int64
	Retries                  int64
	PromptTokens             int64
	CompletionTokens         int64
	TotalTokens              int64
	CachedTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	TotalDurationMS          int64
	MinDurationMS            *int64
	MaxDurationMS            int64
	TotalFirstTokenMS        int64
	MinFirstTokenMS          *int64
	MaxFirstTokenMS          int64
	StatusCodes              map[string]int64
}

// dict 对应 UsageStats.to_dict()（metrics.py:62）。
//
// 键的顺序是响应体的字节序，必须与参照实现一致（canonical 的 DumpsOrdered 按
// 插入顺序输出）。cached_token_rate 与 avg_* 是派生值，位置也在其中。
func (s UsageStats) dict() *canonical.Value {
	minDuration := int64(0)
	if s.MinDurationMS != nil {
		minDuration = *s.MinDurationMS
	}
	minFirstToken := int64(0)
	if s.MinFirstTokenMS != nil {
		minFirstToken = *s.MinFirstTokenMS
	}
	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "requests", Value: canonical.NewIntValue(s.Requests)},
		canonical.ObjectPair{Key: "successes", Value: canonical.NewIntValue(s.Successes)},
		canonical.ObjectPair{Key: "failures", Value: canonical.NewIntValue(s.Failures)},
		canonical.ObjectPair{Key: "retries", Value: canonical.NewIntValue(s.Retries)},
		canonical.ObjectPair{Key: "prompt_tokens", Value: canonical.NewIntValue(s.PromptTokens)},
		canonical.ObjectPair{Key: "completion_tokens", Value: canonical.NewIntValue(s.CompletionTokens)},
		canonical.ObjectPair{Key: "total_tokens", Value: canonical.NewIntValue(s.TotalTokens)},
		canonical.ObjectPair{Key: "cached_tokens", Value: canonical.NewIntValue(s.CachedTokens)},
		canonical.ObjectPair{Key: "cache_creation_input_tokens", Value: canonical.NewIntValue(s.CacheCreationInputTokens)},
		canonical.ObjectPair{Key: "cache_read_input_tokens", Value: canonical.NewIntValue(s.CacheReadInputTokens)},
		canonical.ObjectPair{Key: "cached_token_rate", Value: canonical.NewFloat(rate(s.CachedTokens, s.PromptTokens))},
		canonical.ObjectPair{Key: "total_duration_ms", Value: canonical.NewIntValue(s.TotalDurationMS)},
		canonical.ObjectPair{Key: "avg_duration_ms", Value: canonical.NewIntValue(s.average(s.TotalDurationMS))},
		canonical.ObjectPair{Key: "min_duration_ms", Value: canonical.NewIntValue(minDuration)},
		canonical.ObjectPair{Key: "max_duration_ms", Value: canonical.NewIntValue(s.MaxDurationMS)},
		canonical.ObjectPair{Key: "total_first_token_ms", Value: canonical.NewIntValue(s.TotalFirstTokenMS)},
		canonical.ObjectPair{Key: "avg_first_token_ms", Value: canonical.NewIntValue(s.average(s.TotalFirstTokenMS))},
		canonical.ObjectPair{Key: "min_first_token_ms", Value: canonical.NewIntValue(minFirstToken)},
		canonical.ObjectPair{Key: "max_first_token_ms", Value: canonical.NewIntValue(s.MaxFirstTokenMS)},
		canonical.ObjectPair{Key: "status_codes", Value: statusCodeDict(s.StatusCodes)},
	)
}

// average 对应 `round(total / requests) if requests else 0`。
//
// 先做浮点除法再银行家舍入：Python 的 round 无 ndigits 时返回整数，且 0.5 取偶。
func (s UsageStats) average(total int64) int64 {
	if s.Requests == 0 {
		return 0
	}
	return roundHalfEven(float64(total) / float64(s.Requests))
}

// statusCodeDict 对应 `dict(sorted(counter.items()))`。
//
// Counter 的键是 str(status_code)，按字符串（码点）升序排列，所以 "100" 排在
// "99" 前面。canonical 的 Dumps 会自行排序，但这里保持插入有序以免依赖它。
func statusCodeDict(codes map[string]int64) *canonical.Value {
	keys := make([]string, 0, len(codes))
	for key := range codes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]canonical.ObjectPair, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, canonical.ObjectPair{Key: key, Value: canonical.NewIntValue(codes[key])})
	}
	return canonical.NewObjectOf(pairs...)
}

// normalizeUsage 对应 metrics.py:1108 的 _normalize_usage。
//
// 两个供应商的字段名不同，且 Anthropic 的 input_tokens **不含**缓存 token，
// 需要加回缓存读写量；OpenAI 则把缓存量放在 prompt_tokens_details.cached_tokens
// 或顶层 cached_tokens。所有回退链都用 `or`（Python 真值判断），所以显式的 0
// 会继续向后回退——这不是笔误，必须照抄。
//
// usage 为 nil 或非对象时按空字典处理（对应 `usage or {}`）。
func normalizeUsage(usage *canonical.Value) Usage {
	if !usage.IsObject() {
		usage = nil
	}
	promptTokens := intValue(lookup(usage, "prompt_tokens"))
	inputTokens := intValue(lookup(usage, "input_tokens"))
	completionTokens := intValue(lookup(usage, "completion_tokens"))
	if completionTokens == 0 {
		completionTokens = intValue(lookup(usage, "output_tokens"))
	}

	promptDetails := detailsObject(lookup(usage, "prompt_tokens_details"))
	inputDetails := detailsObject(lookup(usage, "input_tokens_details"))

	cacheRead := intValue(lookup(usage, "cache_read_input_tokens"))
	if cacheRead == 0 {
		cacheRead = intValue(lookup(inputDetails, "cache_read_input_tokens"))
	}
	cacheCreation := intValue(lookup(usage, "cache_creation_input_tokens"))
	if cacheCreation == 0 {
		cacheCreation = intValue(lookup(inputDetails, "cache_creation_input_tokens"))
	}

	cachedTokens := intValue(lookup(usage, "cached_tokens"))
	if cachedTokens == 0 {
		cachedTokens = intValue(lookup(promptDetails, "cached_tokens"))
	}
	if cachedTokens == 0 {
		cachedTokens = intValue(lookup(inputDetails, "cached_tokens"))
	}
	if cachedTokens == 0 {
		cachedTokens = cacheRead
	}

	// Anthropic: input_tokens 不含缓存 token，加回来。
	if inputTokens != 0 && promptTokens == 0 {
		promptTokens = inputTokens + cacheRead + cacheCreation
	}

	totalTokens := intValue(lookup(usage, "total_tokens"))
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}
	return Usage{
		PromptTokens:             promptTokens,
		CompletionTokens:         completionTokens,
		TotalTokens:              totalTokens,
		CachedTokens:             cachedTokens,
		CacheCreationInputTokens: cacheCreation,
		CacheReadInputTokens:     cacheRead,
	}
}

// lookup 在对象上取键；usage 为 nil（被判为非对象）时返回 nil。
func lookup(usage *canonical.Value, key string) *canonical.Value {
	if usage == nil {
		return nil
	}
	return usage.Lookup(key)
}

// detailsObject 对应 `x = usage.get(...); if not isinstance(x, dict): x = {}`。
func detailsObject(value *canonical.Value) *canonical.Value {
	if !value.IsObject() {
		return nil
	}
	return value
}

// intValue 对应 metrics.py:1148 的 _int_value。
//
// Python 的判定顺序是 int → float → str.isdigit()，其中 **bool 是 int 的子类**，
// 所以 True 会得到 1 而不是 0。字符串分支只接受「全部是数字字符」的串：
// "-5"、" 5"、"5.0" 都是 0，而 "007" 是 7。
//
// ponytail: 两处刻意的宽容处理，都只在参照实现**抛异常**的输入上生效，因此不会
// 造成静默错值：
//   - NaN / ±Inf 在 Python 里 int() 会抛 ValueError/OverflowError，这里返回 0；
//   - str.isdigit() 为真但 int() 解析不了的字符（"²"、"①"：Unicode Digit 属性但
//     不是十进制数字）在 Python 里抛 ValueError，这里返回 0。
//
// 这两条是 metrics.py 的真实缺陷（上游 usage 字段一旦被污染，record() 会直接
// 抛异常打断落库），已在交付报告里作为 bug 上报。升级路径：如果上游确认要用
// 「照抄异常」的方式对齐，就把 intValue 改成返回 error 并让 record 冒泡。
func intValue(value *canonical.Value) int64 {
	if value == nil {
		return 0
	}
	switch value.Kind {
	case canonical.KindBool:
		if value.Bool {
			return 1
		}
		return 0
	case canonical.KindNumber:
		parsed, err := canonical.ToInt(value)
		if err != nil {
			return 0
		}
		return parsed
	case canonical.KindString:
		if !pythonIsDigit(value.Str) {
			return 0
		}
		parsed, err := canonical.ToInt(value)
		if err != nil {
			return 0
		}
		return parsed
	}
	return 0
}

// pythonIsDigit 对齐 Python 的 str.isdigit()。
//
// 该谓词为真当且仅当串非空且每个字符都属于 Unicode 的十进制数字（Nd）或带
// Numeric_Type=Digit 的字符（上标 "²"、带圈数字 "①"）。这里用 Nd 判定，比
// Python 略窄：差额只落在「Python 会抛 ValueError」的字符上（见 intValue 的
// ponytail 说明），因此不会漏掉任何能成功解析的输入。空串为 False，与 Python 的
// `"".isdigit()` 一致。
func pythonIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
