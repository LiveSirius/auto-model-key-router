package metrics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// pythonLogicalDump 用 Go 侧驱动导出与语料同形的逻辑快照。
//
// 逐字段对齐 gen_metrics_corpus.py（已随 Python 退役移除） 的 logical_dump：语料是 Python 的
// 真实输出，任何字段错位都会让比对变成噪声。
func pythonLogicalDump(t *testing.T, path string) schemaSnapshot {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开 %s: %v", path, err)
	}
	defer db.Close()

	snapshot := schemaSnapshot{Rows: [][]any{}}

	rows, err := db.Query(
		"SELECT type, name, tbl_name, sql FROM sqlite_master " +
			"WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")
	if err != nil {
		t.Fatalf("查询 sqlite_master: %v", err)
	}
	for rows.Next() {
		var entry masterRow
		if err := rows.Scan(&entry.Type, &entry.Name, &entry.TblName, &entry.SQL); err != nil {
			rows.Close()
			t.Fatalf("扫描 sqlite_master: %v", err)
		}
		snapshot.Master = append(snapshot.Master, entry)
	}
	rows.Close()

	columnRows, err := db.Query("PRAGMA table_info(request_metrics)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	for columnRows.Next() {
		var column columnRow
		if err := columnRows.Scan(&column.CID, &column.Name, &column.Type,
			&column.NotNull, &column.DfltValue, &column.PK); err != nil {
			columnRows.Close()
			t.Fatalf("扫描 table_info: %v", err)
		}
		snapshot.Columns = append(snapshot.Columns, column)
	}
	columnRows.Close()

	indexRows, err := db.Query("PRAGMA index_list(request_metrics)")
	if err != nil {
		t.Fatalf("PRAGMA index_list: %v", err)
	}
	for indexRows.Next() {
		var index indexRow
		if err := indexRows.Scan(&index.Seq, &index.Name, &index.Unique,
			&index.Origin, &index.Partial); err != nil {
			indexRows.Close()
			t.Fatalf("扫描 index_list: %v", err)
		}
		snapshot.IndexList = append(snapshot.IndexList, index)
	}
	indexRows.Close()

	query := "SELECT " + joinColumns(insertColumns) + " FROM request_metrics ORDER BY id"
	dataRows, err := db.Query(query)
	if err != nil {
		t.Fatalf("查询数据行: %v", err)
	}
	for dataRows.Next() {
		values := make([]any, len(insertColumns))
		targets := make([]any, len(insertColumns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := dataRows.Scan(targets...); err != nil {
			dataRows.Close()
			t.Fatalf("扫描数据行: %v", err)
		}
		// NULL 与整数需要转成 JSON 可比的形态：[]byte 会被序列化成 base64。
		normalized := make([]any, len(values))
		for i, value := range values {
			normalized[i] = normalizeCell(value)
		}
		snapshot.Rows = append(snapshot.Rows, normalized)
	}
	dataRows.Close()

	if err := db.QueryRow("PRAGMA journal_mode").Scan(&snapshot.JournalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&snapshot.UserVersion); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&snapshot.IntegrityCheck); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	return snapshot
}

// normalizeCell 把驱动返回的原始值转成与 Python sqlite3 一致的 JSON 形态。
//
// modernc 驱动对 TEXT 返回 string、对 INTEGER 返回 int64、对 NULL 返回 nil，
// 但某些路径会给 []byte。这里显式归一化，避免 base64 造成的假失败。
func normalizeCell(value any) any {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	default:
		return value
	}
}

func joinColumns(columns []string) string {
	out := ""
	for i, column := range columns {
		if i > 0 {
			out += ", "
		}
		out += column
	}
	return out
}

// TestSchemaMatchesPython 断言全新库的结构与参照实现逐字符一致。
//
// 这是整条兼容链的根：如果 CREATE TABLE 的文本不一致，SQLite 的 sqlite_master
// 就会出现不同内容，任何按库文本比对的工具（含本测试）都会报差异，而用户换
// 二进制后看到的却是"能用但结构不同"的库。
func TestSchemaMatchesPython(t *testing.T) {
	for _, testCase := range loadCorpus(t, "schema.jsonl") {
		t.Run(testCase.Name, func(t *testing.T) {
			store := tempStore(t)
			if err := store.Close(); err != nil {
				t.Fatalf("关闭 store: %v", err)
			}
			actual := pythonLogicalDump(t, store.Path())

			var expected schemaSnapshot
			if err := json.Unmarshal([]byte(testCase.Expect), &expected); err != nil {
				t.Fatalf("解析 expect: %v", err)
			}

			// 逐项比对，报出第一处差异的完整文本，方便直接定位字符。
			if len(actual.Master) != len(expected.Master) {
				t.Fatalf("sqlite_master 条目数不同: Go=%d Python=%d",
					len(actual.Master), len(expected.Master))
			}
			for i := range expected.Master {
				got, want := actual.Master[i], expected.Master[i]
				if got.Type != want.Type || got.Name != want.Name || got.TblName != want.TblName {
					t.Errorf("master[%d] 标识不同:\n Go=%+v\n Py=%+v", i, got, want)
				}
				if !equalSQL(got.SQL, want.SQL) {
					t.Errorf("master[%d] (%s) 的 SQL 原文不同:\n Go=%s\n Py=%s",
						i, want.Name, renderSQL(got.SQL), renderSQL(want.SQL))
				}
			}
			if len(actual.Columns) != len(expected.Columns) {
				t.Fatalf("列数不同: Go=%d Python=%d", len(actual.Columns), len(expected.Columns))
			}
			for i := range expected.Columns {
				if !equalColumn(actual.Columns[i], expected.Columns[i]) {
					t.Errorf("columns[%d] 不同:\n Go=%s\n Py=%s",
						i, renderColumn(actual.Columns[i]), renderColumn(expected.Columns[i]))
				}
			}
			if len(actual.IndexList) != len(expected.IndexList) {
				t.Fatalf("索引数不同: Go=%d Python=%d",
					len(actual.IndexList), len(expected.IndexList))
			}
			if actual.JournalMode != expected.JournalMode {
				t.Errorf("journal_mode 不同: Go=%q Python=%q",
					actual.JournalMode, expected.JournalMode)
			}
			if actual.UserVersion != expected.UserVersion {
				t.Errorf("user_version 不同: Go=%d Python=%d",
					actual.UserVersion, expected.UserVersion)
			}
			if actual.IntegrityCheck != expected.IntegrityCheck {
				t.Errorf("integrity_check 不同: Go=%q Python=%q",
					actual.IntegrityCheck, expected.IntegrityCheck)
			}
		})
	}
}

func equalSQL(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// equalColumn 按**值**比较列定义。
//
// 不能用 == 直接比结构体：DfltValue 是 *string，== 比的是指针地址，即使内容
// 相同也会判不相等（并把地址打进失败信息，看不出真实差异）。
func equalColumn(a, b columnRow) bool {
	return a.CID == b.CID && a.Name == b.Name && a.Type == b.Type &&
		a.NotNull == b.NotNull && a.PK == b.PK && equalSQL(a.DfltValue, b.DfltValue)
}

// renderColumn 渲染列定义，NULL 默认值显示为 <nil>，便于读失败信息。
func renderColumn(column columnRow) string {
	return fmt.Sprintf("{cid:%d name:%s type:%s notnull:%d default:%s pk:%d}",
		column.CID, column.Name, column.Type, column.NotNull,
		renderSQL(column.DfltValue), column.PK)
}

func renderSQL(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *value)
}

// TestLegacyDatabaseUpgrade 断言旧库能被**直接打开**并升级成与 Python 相同的结构。
//
// 这条测试对应真实的换二进制场景：用户手里已经有一个 metrics.sqlite3，它的表
// 缺少后加的列，caller_type 里有脏值。打开后必须补列、回填、建索引，且结果与
// 参照实现完全一致。
func TestLegacyDatabaseUpgrade(t *testing.T) {
	for _, testCase := range loadCorpus(t, "legacy.jsonl") {
		t.Run(testCase.Name, func(t *testing.T) {
			path := copyFixture(t, "legacy.sqlite3")
			// 打开即升级。
			store, err := Open(path)
			if err != nil {
				t.Fatalf("打开旧库: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("关闭 store: %v", err)
			}
			actual := pythonLogicalDump(t, path)

			var expected schemaSnapshot
			if err := json.Unmarshal([]byte(testCase.Expect), &expected); err != nil {
				t.Fatalf("解析 expect: %v", err)
			}
			if len(actual.Columns) != len(expected.Columns) {
				t.Fatalf("升级后列数不同: Go=%d Python=%d",
					len(actual.Columns), len(expected.Columns))
			}
			for i := range expected.Columns {
				if !equalColumn(actual.Columns[i], expected.Columns[i]) {
					t.Errorf("升级后 columns[%d] 不同:\n Go=%s\n Py=%s",
						i, renderColumn(actual.Columns[i]), renderColumn(expected.Columns[i]))
				}
			}
			if len(actual.Master) != len(expected.Master) {
				t.Fatalf("升级后 master 条目数不同: Go=%d Python=%d",
					len(actual.Master), len(expected.Master))
			}
			for i := range expected.Master {
				if !equalSQL(actual.Master[i].SQL, expected.Master[i].SQL) {
					t.Errorf("升级后 master[%d] (%s) SQL 不同:\n Go=%s\n Py=%s",
						i, expected.Master[i].Name,
						renderSQL(actual.Master[i].SQL), renderSQL(expected.Master[i].SQL))
				}
			}
			compareRows(t, actual.Rows, expected.Rows)
		})
	}
}

// compareRows 逐行逐列比对。
func compareRows(t *testing.T, actual, expected [][]any) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("行数不同: Go=%d Python=%d", len(actual), len(expected))
	}
	for i := range expected {
		if len(actual[i]) != len(expected[i]) {
			t.Fatalf("第 %d 行列数不同: Go=%d Python=%d",
				i, len(actual[i]), len(expected[i]))
		}
		for j := range expected[i] {
			got, want := jsonScalar(actual[i][j]), jsonScalar(expected[i][j])
			if got != want {
				t.Errorf("第 %d 行 %s 列不同: Go=%s Python=%s",
					i, insertColumns[j], got, want)
			}
		}
	}
}

// jsonScalar 把值渲染成 JSON 文本用于比对（nil → null）。
func jsonScalar(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("<err %v>", err)
	}
	return string(encoded)
}

// TestRecordMatchesPython 断言 record() 的落库结果与参照实现一致。
func TestRecordMatchesPython(t *testing.T) {
	for _, testCase := range loadCorpus(t, "record.jsonl") {
		t.Run(testCase.Name, func(t *testing.T) {
			store := tempStore(t)
			installClock(t, parseFixtureTime(t, testCase.Now))

			params, err := recordParamsFromCorpus(testCase)
			if err != nil {
				t.Fatalf("构造参数: %v", err)
			}
			if err := store.Record(params); err != nil {
				t.Fatalf("写入: %v", err)
			}

			db, err := sql.Open("sqlite", store.Path())
			if err != nil {
				t.Fatalf("打开校验连接: %v", err)
			}
			defer db.Close()
			values := make([]any, len(insertColumns))
			targets := make([]any, len(insertColumns))
			for i := range values {
				targets[i] = &values[i]
			}
			query := "SELECT " + joinColumns(insertColumns) + " FROM request_metrics"
			if err := db.QueryRow(query).Scan(targets...); err != nil {
				t.Fatalf("读取落库行: %v", err)
			}
			row := make([]any, len(values))
			for i, value := range values {
				row[i] = normalizeCell(value)
			}

			// expect 是 {列名: 值} 的对象（不含自增 id），因此按列名逐项比对，
			// 而不是按位置——按位置会在列序调整时给出误导性的失败信息。
			var expectedRow map[string]any
			if err := json.Unmarshal([]byte(testCase.Expect), &expectedRow); err != nil {
				t.Fatalf("解析 expect: %v", err)
			}
			actualRow := map[string]any{}
			for i, column := range insertColumns {
				actualRow[column] = row[i]
			}
			if len(actualRow) != len(expectedRow) {
				t.Fatalf("列数不同: Go=%d Python=%d", len(actualRow), len(expectedRow))
			}
			for _, column := range insertColumns {
				got, want := jsonScalar(actualRow[column]), jsonScalar(expectedRow[column])
				if got != want {
					t.Errorf("列 %s 不同: Go=%s Python=%s", column, got, want)
				}
			}
		})
	}
}

// recordParamsFromCorpus 从语料的 params 字段还原 RecordParams。
//
// 语料里的 params 是 Python record() 的关键字参数，键名与类型都需要显式映射：
// JSON 的数字统一是 float64，直接断言成 int64 会在 "duration_ms": 1200 这种
// 整数值上静默截断风险（这里用 json.Number 避免）。
func recordParamsFromCorpus(testCase corpusCase) (RecordParams, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(testCase.Params), &raw); err != nil {
		return RecordParams{}, err
	}
	params := RecordParams{CallerType: "local"}
	decodeString := func(key string, target *string) error {
		if value, ok := raw[key]; ok {
			return json.Unmarshal(value, target)
		}
		return nil
	}
	decodeOptionalString := func(key string) (*string, error) {
		value, ok := raw[key]
		if !ok {
			return nil, nil
		}
		var text *string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, err
		}
		return text, nil
	}
	decodeOptionalInt := func(key string) (*int64, error) {
		value, ok := raw[key]
		if !ok {
			return nil, nil
		}
		var number *int64
		if err := json.Unmarshal(value, &number); err != nil {
			return nil, err
		}
		return number, nil
	}
	if err := decodeString("model_id", &params.ModelID); err != nil {
		return params, err
	}
	if err := decodeString("key_name", &params.KeyName); err != nil {
		return params, err
	}
	if value, ok := raw["caller_type"]; ok {
		if err := json.Unmarshal(value, &params.CallerType); err != nil {
			return params, err
		}
	}
	var err error
	if params.StatusCode, err = decodeOptionalInt("status_code"); err != nil {
		return params, err
	}
	if params.RequestedModelID, err = decodeOptionalString("requested_model_id"); err != nil {
		return params, err
	}
	if params.ProviderID, err = decodeOptionalString("provider_id"); err != nil {
		return params, err
	}
	if params.PoolName, err = decodeOptionalString("pool_name"); err != nil {
		return params, err
	}
	if params.UpstreamModelID, err = decodeOptionalString("upstream_model_id"); err != nil {
		return params, err
	}
	for key, target := range map[string]*int64{
		"duration_ms": &params.DurationMS, "first_token_ms": &params.FirstTokenMS,
	} {
		if value, ok := raw[key]; ok {
			if err := json.Unmarshal(value, target); err != nil {
				return params, err
			}
		}
	}
	for key, target := range map[string]*bool{
		"retried": &params.Retried, "failed": &params.Failed,
	} {
		if value, ok := raw[key]; ok {
			if err := json.Unmarshal(value, target); err != nil {
				return params, err
			}
		}
	}
	if value, ok := raw["usage"]; ok && string(value) != "null" {
		parsed, err := canonical.Parse(value)
		if err != nil {
			return params, err
		}
		params.Usage = parsed
	}
	return params, nil
}

// TestNormalizeUsageMatchesPython 断言 usage 归一化与参照实现一致。
func TestNormalizeUsageMatchesPython(t *testing.T) {
	checked := 0
	for _, testCase := range loadCorpus(t, "usage.jsonl") {
		switch testCase.Kind {
		case "normalize_usage":
			t.Run(testCase.Name, func(t *testing.T) {
				var input map[string]any
				if err := json.Unmarshal(testCase.Input, &input); err != nil {
					t.Fatalf("解析 input: %v", err)
				}
				usage, err := canonical.Parse(testCase.Input)
				if err != nil {
					t.Fatalf("解析 usage: %v", err)
				}
				actual := normalizeUsage(usage)
				// 语料的 expect 是六个可观测整数。
				result := canonical.NewObjectOf(
					canonical.ObjectPair{Key: "prompt_tokens", Value: canonical.NewIntValue(actual.PromptTokens)},
					canonical.ObjectPair{Key: "completion_tokens", Value: canonical.NewIntValue(actual.CompletionTokens)},
					canonical.ObjectPair{Key: "total_tokens", Value: canonical.NewIntValue(actual.TotalTokens)},
					canonical.ObjectPair{Key: "cached_tokens", Value: canonical.NewIntValue(actual.CachedTokens)},
					canonical.ObjectPair{Key: "cache_creation_input_tokens", Value: canonical.NewIntValue(actual.CacheCreationInputTokens)},
					canonical.ObjectPair{Key: "cache_read_input_tokens", Value: canonical.NewIntValue(actual.CacheReadInputTokens)},
				)
				if got := compact(t, result); got != testCase.Expect {
					t.Errorf("归一化结果不同:\n Go=%s\n Py=%s", got, testCase.Expect)
				}
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("没有跑到任何 normalize_usage 用例")
	}
}

// TestIntValueMatchesPython 断言 _int_value 的判定顺序与真值语义。
func TestIntValueMatchesPython(t *testing.T) {
	for _, testCase := range loadCorpus(t, "usage.jsonl") {
		if testCase.Kind != "int_value" {
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			value, err := canonical.Parse(testCase.Input)
			if err != nil {
				t.Fatalf("解析 input: %v", err)
			}
			// input 是 JSON 标量，canonical 解析出的就是对应类型。
			actual := intValue(value)
			if actual != expectInt(t, testCase.Expect) {
				t.Errorf("int_value(%s) = %d, 期望 %s",
					testCase.Input, actual, testCase.Expect)
			}
		})
	}
}

// TestRateMatchesPython 断言 _rate 的浮点舍入与参照实现逐位一致。
//
// 这里覆盖的是最容易错的点：round(x, 6) 是在 double 的精确十进制展开上做最近
// 偶数舍入，不是"四舍五入"，也不能用 math.Round 近似。含 1/128、3/128 这类
// 二进制精确、十进制恰好落在中点的输入。
func TestRateMatchesPython(t *testing.T) {
	checked := 0
	for _, testCase := range loadCorpus(t, "usage.jsonl") {
		if testCase.Kind != "rate" {
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			var pair []json.Number
			if err := json.Unmarshal(testCase.Input, &pair); err != nil {
				t.Fatalf("解析 input: %v", err)
			}
			if len(pair) != 2 {
				t.Fatalf("input 需要两个数: %s", testCase.Input)
			}
			numerator, err := pair[0].Int64()
			if err != nil {
				t.Fatalf("分子不是整数: %v", err)
			}
			denominator, err := pair[1].Int64()
			if err != nil {
				t.Fatalf("分母不是整数: %v", err)
			}
			actual := canonical.NewFloat(rate(numerator, denominator))
			// 直接解析语料里的 Python 字面量，**不要**经过 Go 的
			// encoding/json：Go 会把 0.0 渲染成 "0"、1.0 渲染成 "1"，丢掉
			// 浮点身份，于是 "0.0" vs "0" 会产生假失败，而 -0.0 也会失真。
			expectedValue, err := canonical.Parse([]byte(testCase.Expect))
			if err != nil {
				t.Fatalf("解析 expect %s: %v", testCase.Expect, err)
			}
			expectedText := canonical.DumpsOrdered(expectedValue)
			if got := compact(t, actual); got != expectedText {
				t.Errorf("rate(%d, %d) = %s, 期望 %s",
					numerator, denominator, got, expectedText)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("没有跑到任何 rate 用例")
	}
}

// mustCanonical 用 Python 的 JSON 往返把普通值转成 canonical。
func mustCanonical(t *testing.T, value any) *canonical.Value {
	t.Helper()
	text, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	parsed, err := canonical.Parse(text)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return parsed
}

// TestRouterStatusMatchesPython 断言健康状态阈值。
func TestRouterStatusMatchesPython(t *testing.T) {
	for _, testCase := range loadCorpus(t, "usage.jsonl") {
		if testCase.Kind != "router_status" {
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			var input struct {
				Requests  int64 `json:"requests"`
				Successes int64 `json:"successes"`
				Retries   int64 `json:"retries"`
			}
			if err := json.Unmarshal(testCase.Input, &input); err != nil {
				t.Fatalf("解析 input: %v", err)
			}
			stats := &UsageStats{
				Requests: input.Requests, Successes: input.Successes,
				Retries: input.Retries,
			}
			actual := canonical.NewString(routerStatus(stats))
			var expected string
			if err := json.Unmarshal([]byte(testCase.Expect), &expected); err != nil {
				t.Fatalf("解析 expect: %v", err)
			}
			if got := compact(t, actual); got != compact(t, canonical.NewString(expected)) {
				t.Errorf("router_status = %s, 期望 %q", got, expected)
			}
		})
	}
}

// TestToDictMatchesPython 断言 to_dict 的键序、派生值与空集渲染。
//
// 键序是响应体的字节序，必须整体逐字比对，不能逐字段断言。
func TestToDictMatchesPython(t *testing.T) {
	checked := 0
	for _, testCase := range loadCorpus(t, "usage.jsonl") {
		if testCase.Kind != "to_dict" {
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			stats := buildStatsFromCorpus(t, testCase.Input)
			if got := compact(t, stats.dict()); got != testCase.Expect {
				t.Errorf("to_dict 不同:\n Go=%s\n Py=%s", got, testCase.Expect)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("没有跑到任何 to_dict 用例")
	}
}

// buildStatsFromCorpus 从语料 input 构造 UsageStats。
//
// status_codes 的值是 dict（键为状态码文本），其余是整数；min_* 缺席即 None。
func buildStatsFromCorpus(t *testing.T, raw json.RawMessage) UsageStats {
	t.Helper()
	var input struct {
		Requests                 int64            `json:"requests"`
		Successes                int64            `json:"successes"`
		Failures                 int64            `json:"failures"`
		Retries                  int64            `json:"retries"`
		PromptTokens             int64            `json:"prompt_tokens"`
		CompletionTokens         int64            `json:"completion_tokens"`
		TotalTokens              int64            `json:"total_tokens"`
		CachedTokens             int64            `json:"cached_tokens"`
		CacheCreationInputTokens int64            `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64            `json:"cache_read_input_tokens"`
		TotalDurationMS          int64            `json:"total_duration_ms"`
		MinDurationMS            *int64           `json:"min_duration_ms"`
		MaxDurationMS            int64            `json:"max_duration_ms"`
		TotalFirstTokenMS        int64            `json:"total_first_token_ms"`
		MinFirstTokenMS          *int64           `json:"min_first_token_ms"`
		MaxFirstTokenMS          int64            `json:"max_first_token_ms"`
		StatusCodes              map[string]int64 `json:"status_codes"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatalf("解析 input: %v", err)
	}
	stats := UsageStats{
		Requests: input.Requests, Successes: input.Successes,
		Failures: input.Failures, Retries: input.Retries,
		PromptTokens: input.PromptTokens, CompletionTokens: input.CompletionTokens,
		TotalTokens: input.TotalTokens, CachedTokens: input.CachedTokens,
		CacheCreationInputTokens: input.CacheCreationInputTokens,
		CacheReadInputTokens:     input.CacheReadInputTokens,
		TotalDurationMS:          input.TotalDurationMS,
		MinDurationMS:            input.MinDurationMS,
		MaxDurationMS:            input.MaxDurationMS,
		TotalFirstTokenMS:        input.TotalFirstTokenMS,
		MinFirstTokenMS:          input.MinFirstTokenMS,
		MaxFirstTokenMS:          input.MaxFirstTokenMS,
		StatusCodes:              input.StatusCodes,
	}
	if stats.StatusCodes == nil {
		stats.StatusCodes = map[string]int64{}
	}
	return stats
}

// TestClockMatchesPython 断言 created_at 文本、小时运算与分桶。
//
// 这是静默错位的最高危面：
//   - 整秒多出 ".000000" 会让字典序比较整体偏移；
//   - timedelta(hours=) 的微秒舍入错 1 微秒，就会让边界记录错进错出；
//   - 分桶的 int() 截断方向（向零 vs 向负无穷）在负 unix 秒上会差一个桶。
func TestClockMatchesPython(t *testing.T) {
	checked := map[string]int{}
	for _, testCase := range loadCorpus(t, "clock.jsonl") {
		t.Run(testCase.Kind+"/"+testCase.Name, func(t *testing.T) {
			moment := buildMoment(t, testCase.Parts)
			switch testCase.Kind {
			case "isoformat":
				actual := formatISO(moment)
				var expected string
				if err := json.Unmarshal([]byte(testCase.Expect), &expected); err != nil {
					t.Fatalf("解析 expect: %v", err)
				}
				if actual != expected {
					t.Errorf("isoformat 不同:\n Go=%q\n Py=%q", actual, expected)
				}
			case "add_hours":
				// 语料用 Go/Python 都能解析的十进制字面量，避免 1/3 这种
				// Python 表达式无法入 JSON 的问题。
				hours, err := parseHoursLiteral(testCase.HoursLiteral)
				if err != nil {
					t.Fatalf("解析 hours_literal: %v", err)
				}
				actual := formatISO(addHours(moment, hours))
				var expected string
				if err := json.Unmarshal([]byte(testCase.Expect), &expected); err != nil {
					t.Fatalf("解析 expect: %v", err)
				}
				if actual != expected {
					t.Errorf("add_hours(%s) 不同:\n Go=%q\n Py=%q",
						testCase.HoursLiteral, actual, expected)
				}
			case "bucket_epoch":
				actual := bucketEpoch(moment, testCase.BucketSeconds)
				if actual != expectInt(t, testCase.Expect) {
					t.Errorf("bucket_epoch(%s, %d) = %d, 期望 %s",
						moment.Format(time.RFC3339), testCase.BucketSeconds,
						actual, testCase.Expect)
				}
			default:
				t.Fatalf("未知 kind %q", testCase.Kind)
			}
			checked[testCase.Kind]++
		})
	}
	// 保证每类都真的跑过：语料被改坏时不能静默通过。
	for _, kind := range []string{"isoformat", "add_hours", "bucket_epoch"} {
		if checked[kind] == 0 {
			t.Errorf("没有跑到任何 %s 用例", kind)
		}
	}
}

// buildMoment 从语料的分量构造北京时间。
//
// 语料的 parts 只记录**显式设置**的分量（生成侧用默认值补齐），因此这里必须
// 套用同一组默认值，否则缺省字段会变成 0 年和 0 月。
func buildMoment(t *testing.T, parts map[string]int) time.Time {
	t.Helper()
	lookup := func(key string, fallback int) int {
		if value, ok := parts[key]; ok {
			return value
		}
		return fallback
	}
	return time.Date(
		lookup("year", 2026), time.Month(lookup("month", 1)), lookup("day", 1),
		lookup("hour", 0), lookup("minute", 0), lookup("second", 0),
		lookup("microsecond", 0)*1000, beijingTZ,
	)
}

// parseHoursLiteral 解析语料里的 hours 字面量。
//
// 语料存的是 Python repr（如 "0.3333333333333333"、"1e-09"），Go 的 ParseFloat
// 能吃下同样的形式。
func parseHoursLiteral(literal string) (float64, error) {
	var value float64
	if err := json.Unmarshal([]byte(literal), &value); err != nil {
		return 0, err
	}
	return value, nil
}

// TestMetricFilterMatchesPython 断言过滤器 SQL 文本与参数序。
//
// SQL 文本是查询计划与参数顺序的双重契约：顺序错了不会报错，只会静默换一种
// 语义（例如把 attributed 的 OR 组拆开）。
func TestMetricFilterMatchesPython(t *testing.T) {
	checked := map[string]int{}
	for _, testCase := range loadCorpus(t, "filter.jsonl") {
		t.Run(testCase.Kind+"/"+testCase.Name, func(t *testing.T) {
			switch testCase.Kind {
			case "metric_filter":
				filter, err := filterFromCorpus(testCase.Input)
				if err != nil {
					t.Fatalf("解析 input: %v", err)
				}
				whereSQL, parameters := filter.sql()
				actual := canonical.NewArray(
					canonical.NewString(whereSQL),
					canonicalValueOf(t, parameters),
				)
				if got := compact(t, actual); got != testCase.Expect {
					t.Errorf("过滤器不同:\n Go=%s\n Py=%s", got, testCase.Expect)
				}
			case "append_filter":
				var pair []string
				if err := json.Unmarshal(testCase.Input, &pair); err != nil {
					t.Fatalf("解析 input: %v", err)
				}
				actual := canonical.NewString(appendFilter(pair[0], pair[1]))
				var expected string
				if err := json.Unmarshal([]byte(testCase.Expect), &expected); err != nil {
					t.Fatalf("解析 expect: %v", err)
				}
				if got := compact(t, actual); got != compact(t, canonical.NewString(expected)) {
					t.Errorf("append_filter 不同:\n Go=%s\n Py=%s", got, testCase.Expect)
				}
			default:
				t.Fatalf("未知 kind %q", testCase.Kind)
			}
			checked[testCase.Kind]++
		})
	}
	for _, kind := range []string{"metric_filter", "append_filter"} {
		if checked[kind] == 0 {
			t.Errorf("没有跑到任何 %s 用例", kind)
		}
	}
}

// canonicalValueOf 把 Go 的 any 切片转成 canonical 数组。
func canonicalValueOf(t *testing.T, values []any) *canonical.Value {
	t.Helper()
	items := make([]*canonical.Value, 0, len(values))
	for _, value := range values {
		text, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("序列化参数: %v", err)
		}
		parsed, err := canonical.Parse(text)
		if err != nil {
			t.Fatalf("解析参数: %v", err)
		}
		items = append(items, parsed)
	}
	return canonical.NewArray(items...)
}

// filterFromCorpus 从语料的 input 还原 metricFilter。
func filterFromCorpus(raw json.RawMessage) (metricFilter, error) {
	var input struct {
		SinceCreatedAt   *string `json:"since_created_at"`
		UntilCreatedAt   *string `json:"until_created_at"`
		CallerType       *string `json:"caller_type"`
		ModelID          *string `json:"model_id"`
		RequestedModelID *string `json:"requested_model_id"`
		ProviderID       *string `json:"provider_id"`
		PoolName         *string `json:"pool_name"`
		UpstreamModelID  *string `json:"upstream_model_id"`
		KeyName          *string `json:"key_name"`
		StatusCode       *int64  `json:"status_code"`
		Success          *bool   `json:"success"`
		Attributed       *bool   `json:"attributed"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return metricFilter{}, err
	}
	return metricFilter{
		SinceCreatedAt: input.SinceCreatedAt, UntilCreatedAt: input.UntilCreatedAt,
		CallerType: input.CallerType, ModelID: input.ModelID,
		RequestedModelID: input.RequestedModelID, ProviderID: input.ProviderID,
		PoolName: input.PoolName, UpstreamModelID: input.UpstreamModelID,
		KeyName: input.KeyName, StatusCode: input.StatusCode,
		Success: input.Success, Attributed: input.Attributed,
	}, nil
}

// TestQueriesMatchPython 在主夹具库上重放全部查询并与参照实现的响应比对。
//
// 期望值是 Python 输出的**有序** JSON，因此这里逐字比对整个响应体——任何字段
// 值、键序或聚合口径的偏差都会被捕获。夹具库里的数据刻意包含不自洽的
// success/status_code 组合，正是为了让 17 个聚合的写法差异暴露出来。
func TestQueriesMatchPython(t *testing.T) {
	checked := 0
	for _, testCase := range loadCorpus(t, "query.jsonl") {
		t.Run(testCase.Kind+"/"+testCase.Name, func(t *testing.T) {
			path := copyFixture(t, testCase.DB+".sqlite3")
			installClock(t, parseFixtureTime(t, testCase.Now))

			store, err := Open(path)
			if err != nil {
				t.Fatalf("打开夹具库: %v", err)
			}
			defer store.Close()
			// 语料生成时固定了 2 个活跃请求，才能复现 active_requests。
			store.AcquireActive()
			store.AcquireActive()

			actual, err := runQueryCall(t, store, testCase.Call)
			if testCase.Kind == "error" {
				if err == nil {
					t.Fatalf("期望校验失败，但没有报错")
				}
				var expected string
				if err := json.Unmarshal([]byte(testCase.Expect), &expected); err != nil {
					t.Fatalf("解析 expect: %v", err)
				}
				if err.Error() != expected {
					t.Errorf("错误文本不同:\n Go=%q\n Py=%q", err.Error(), expected)
				}
				checked++
				return
			}
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			// database_path 是机器相关的绝对路径，两边都替换成占位符再比对
			// （语料生成时也做了同样处理）。
			if actual.Lookup("database_path").IsString() {
				actual.SetKey("database_path", canonical.NewString("<db>"))
			}
			got := compact(t, actual)
			if got != testCase.Expect {
				t.Errorf("响应不同:\n Go=%s\n\n Py=%s", got, testCase.Expect)
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("没有跑到任何查询用例")
	}
}

// runQueryCall 按语料里的 call 分发到对应方法。
func runQueryCall(t *testing.T, store *Store, raw json.RawMessage) (*canonical.Value, error) {
	t.Helper()
	var call struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		t.Fatalf("解析 call: %v", err)
	}
	switch call.Method {
	case "snapshot":
		var params struct {
			Hours    *float64 `json:"hours"`
			SinceISO *string  `json:"since_iso"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatalf("解析 snapshot 参数: %v", err)
		}
		var since *time.Time
		if params.SinceISO != nil {
			parsed := parsePythonISO(t, *params.SinceISO)
			since = &parsed
		}
		return store.Snapshot(params.Hours, since)
	case "key_stats":
		var params struct {
			ModelID string   `json:"model_id"`
			KeyName string   `json:"key_name"`
			Hours   *float64 `json:"hours"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatalf("解析 key_stats 参数: %v", err)
		}
		return store.KeyStats(params.ModelID, params.KeyName, params.Hours)
	case "request_history":
		var params struct {
			Hours            *float64 `json:"hours"`
			CallerType       *string  `json:"caller_type"`
			ModelID          *string  `json:"model_id"`
			RequestedModelID *string  `json:"requested_model_id"`
			ProviderID       *string  `json:"provider_id"`
			PoolName         *string  `json:"pool_name"`
			UpstreamModelID  *string  `json:"upstream_model_id"`
			KeyName          *string  `json:"key_name"`
			StatusCode       *int64   `json:"status_code"`
			Success          *bool    `json:"success"`
			Attributed       *bool    `json:"attributed"`
			Limit            *int64   `json:"limit"`
			BeforeID         *int64   `json:"before_id"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatalf("解析 request_history 参数: %v", err)
		}
		limit := int64(50)
		if params.Limit != nil {
			limit = *params.Limit
		}
		return store.RequestHistory(RequestHistoryParams{
			Hours: params.Hours, CallerType: params.CallerType,
			ModelID: params.ModelID, RequestedModelID: params.RequestedModelID,
			ProviderID: params.ProviderID, PoolName: params.PoolName,
			UpstreamModelID: params.UpstreamModelID, KeyName: params.KeyName,
			StatusCode: params.StatusCode, Success: params.Success,
			Attributed: params.Attributed, Limit: limit, BeforeID: params.BeforeID,
		})
	case "time_series":
		var params struct {
			Hours            float64 `json:"hours"`
			BucketSeconds    int64   `json:"bucket_seconds"`
			CallerType       *string `json:"caller_type"`
			ModelID          *string `json:"model_id"`
			RequestedModelID *string `json:"requested_model_id"`
			ProviderID       *string `json:"provider_id"`
			PoolName         *string `json:"pool_name"`
			UpstreamModelID  *string `json:"upstream_model_id"`
			KeyName          *string `json:"key_name"`
			StatusCode       *int64  `json:"status_code"`
			Success          *bool   `json:"success"`
			Attributed       *bool   `json:"attributed"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatalf("解析 time_series 参数: %v", err)
		}
		return store.TimeSeries(TimeSeriesParams{
			Hours: params.Hours, BucketSeconds: params.BucketSeconds,
			CallerType: params.CallerType, ModelID: params.ModelID,
			RequestedModelID: params.RequestedModelID, ProviderID: params.ProviderID,
			PoolName: params.PoolName, UpstreamModelID: params.UpstreamModelID,
			KeyName: params.KeyName, StatusCode: params.StatusCode,
			Success: params.Success, Attributed: params.Attributed,
		})
	}
	t.Fatalf("未知方法 %q", call.Method)
	return nil, nil
}

var _ = sort.Strings
var _ = filepath.Join
