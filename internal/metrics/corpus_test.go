package metrics

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// testdataDir 是差分语料所在目录；语料由 gen_metrics_corpus.py（已随 Python 退役移除） 用**真实
// 的 Python 参照实现**生成。这些测试的价值全部来自这一点：期望值不是手写的，
// 而是参照实现的真实输出，因此任何偏差都会被捕获。
const testdataDir = "testdata"

// corpusCase 是语料行的通用结构。
//
// name 唯一；kind 决定用哪个断言函数；expect 是**紧凑有序**的 JSON 文本（响应体
// 键序是契约的一部分，所以不能重新序列化再比对）。
type corpusCase struct {
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	Note          string          `json:"note"`
	Now           string          `json:"now"`
	Parts         map[string]int  `json:"parts"`
	HoursLiteral  string          `json:"hours_literal"`
	BucketSeconds int64           `json:"bucket_seconds"`
	Input         json.RawMessage `json:"input"`
	Call          json.RawMessage `json:"call"`
	Params        json.RawMessage `json:"params"`
	DB            string          `json:"db"`
	Expect        string          `json:"expect"`
}

// loadCorpus 逐行读取 JSONL 语料。
//
// 用 Scanner 并放大缓冲：query.jsonl 里的单行可能有几十 KB（time_series 的点位
// 数组），默认 64 KB 上限会截断成 "token too long" 这种与真实问题无关的报错。
func loadCorpus(t *testing.T, name string) []corpusCase {
	t.Helper()
	path := filepath.Join(testdataDir, name)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开语料 %s: %v", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var cases []corpusCase
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var parsed corpusCase
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("%s: 解析语料行失败: %v", path, err)
		}
		cases = append(cases, parsed)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s: 读取语料失败: %v", path, err)
	}
	if len(cases) == 0 {
		t.Fatalf("%s: 语料为空", path)
	}
	return cases
}

// compact 把 canonical 值渲染成与语料 expect 同形的紧凑有序文本。
//
// 用 DumpsOrdered（插入顺序）而非 Dumps（键排序）：响应体的键序是契约的一部分，
// 而语料的 expect 就是 Python 按插入顺序序列化的结果。
func compact(t *testing.T, value *canonical.Value) string {
	t.Helper()
	return canonical.DumpsOrdered(value)
}

// expectInt 从语料的 expect 里取出整数。
func expectInt(t *testing.T, raw string) int64 {
	t.Helper()
	var value int64
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("expect 不是整数: %v (%s)", err, raw)
	}
	return value
}

// tempStore 在一个临时目录里打开 store。
func tempStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metrics.sqlite3"))
	if err != nil {
		t.Fatalf("打开 store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// copyFixture 把语料里的夹具库复制到临时目录，供需要真实数据的测试使用。
//
// 必须复制而不是直接用 testdata 下的文件：测试会写入（升级 legacy、写 WAL），
// 直接改会让语料自身漂移，也会让并行测试互相干扰。
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	source := filepath.Join(testdataDir, name)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("读取夹具 %s: %v", source, err)
	}
	target := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatalf("写入夹具 %s: %v", target, err)
	}
	return target
}

// parseFixtureTime 解析语料里的 now 字段。
func parseFixtureTime(t *testing.T, raw string) time.Time {
	t.Helper()
	if raw == "" {
		t.Fatalf("语料缺少 now 字段")
	}
	// 语料的 now 是 Python isoformat 的 JSON 字符串。
	var text string
	if err := json.Unmarshal([]byte(raw), &text); err != nil {
		t.Fatalf("now 不是字符串: %v", err)
	}
	return parsePythonISO(t, text)
}

// parsePythonISO 解析 Python isoformat 的文本。
//
// Go 的 time.RFC3339 恰好能吃下 Python 的 "2026-07-14T12:00:00+08:00" 与带微秒的
// 形式，也能吃下 "0001-01-01T00:00:00+08:05:43"（带秒的偏移是 RFC3339 允许的
// 扩展）。用 ONLY 解析（写入），绝不用它产出（写入必须走 formatISO）。
func parsePythonISO(t *testing.T, text string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("解析时间 %q 失败: %v", text, err)
	}
	return parsed
}

// installClock 固定 nowBeijing 并在测试结束时还原。
func installClock(t *testing.T, moment time.Time) {
	t.Helper()
	previous := nowBeijing
	nowBeijing = func() time.Time { return moment }
	t.Cleanup(func() { nowBeijing = previous })
}

// schemaSnapshot 是本包建库后的逻辑快照，字段名与语料一致。
//
// 比对的是**逻辑内容**而非文件字节：SQLite 的页布局会随写入顺序与版本变化，
// 比对字节只会得到假失败。真正要锁死的是 sqlite_master 的 SQL 原文、列序、
// 索引定义与 PRAGMA 状态。
type schemaSnapshot struct {
	Master         []masterRow `json:"master"`
	Columns        []columnRow `json:"columns"`
	IndexList      []indexRow  `json:"index_list"`
	Rows           [][]any     `json:"rows"`
	JournalMode    string      `json:"journal_mode"`
	UserVersion    int64       `json:"user_version"`
	IntegrityCheck string      `json:"integrity_check"`
}

type masterRow struct {
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	TblName string  `json:"tbl_name"`
	SQL     *string `json:"sql"`
}

type columnRow struct {
	CID       int64   `json:"cid"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	NotNull   int64   `json:"notnull"`
	DfltValue *string `json:"dflt_value"`
	PK        int64   `json:"pk"`
}

type indexRow struct {
	Seq     int64  `json:"seq"`
	Name    string `json:"name"`
	Unique  int64  `json:"unique"`
	Origin  string `json:"origin"`
	Partial int64  `json:"partial"`
}

// insertColumns 与语料的行导出顺序一致。
var insertColumns = []string{
	"created_at", "caller_type", "model_id", "requested_model_id",
	"provider_id", "pool_name", "upstream_model_id", "key_name",
	"status_code", "success", "retried", "prompt_tokens",
	"completion_tokens", "total_tokens", "cached_tokens",
	"cache_creation_input_tokens", "cache_read_input_tokens",
	"first_token_ms", "duration_ms",
}
