package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// corpusEntry 是一条 config 对拍语料。
//
//	input   被测数据的 canonical 文本（Python 侧 canonical 函数的输出）
//	kwargs  额外关键字参数的 canonical 文本
//	ok      参照实现是否成功
//	output  成功时的 canonical 输出
//	error   失败时的异常文本（error_type 为 Python 异常类名）
type corpusEntry struct {
	Name      string            `json:"name"`
	Input     string            `json:"input"`
	Kwargs    map[string]string `json:"kwargs"`
	OK        bool              `json:"ok"`
	Output    string            `json:"output"`
	Error     string            `json:"error"`
	ErrorType string            `json:"error_type"`
}

func loadCorpus(t *testing.T, name string) []corpusEntry {
	t.Helper()
	path := filepath.Join("testdata", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开语料失败（先运行 python scripts/gen_config_corpus.py）: %v", err)
	}
	defer f.Close()

	var entries []corpusEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry corpusEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("语料行解析失败: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("语料 %s 为空", name)
	}
	return entries
}

// mustParse 解析 canonical 文本，失败即终止测试。
func mustParse(t *testing.T, text string) *canonical.Value {
	t.Helper()
	value, err := canonical.ParseString(text)
	if err != nil {
		t.Fatalf("解析语料文本失败: %v\n文本: %s", err, text)
	}
	return value
}

// checkOutcome 断言 Go 结果与参照实现一致。
//
// 契约文本（ConfigError）逐字比对；参照实现自身的异常（TypeError 等）只要求
// Go 同样失败关闭——那些文本是解释器实现细节，不是对外契约。
func checkOutcome(t *testing.T, entry corpusEntry, got *canonical.Value, err error) {
	t.Helper()
	if !entry.OK {
		if err == nil {
			t.Errorf("参照实现报错 %s，但 Go 成功了，输出: %s", entry.ErrorType, canonical.Dumps(got))
			return
		}
		if entry.ErrorType == "ValueError" {
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Errorf("期望契约错误（ConfigError），实际 %T: %v", err, err)
				return
			}
			if configErr.Message != entry.Error {
				t.Errorf("错误文本不一致\n期望: %s\n实际: %s", entry.Error, configErr.Message)
			}
			return
		}
		// 非 ValueError：只要求失败关闭。
		return
	}
	if err != nil {
		t.Errorf("参照实现成功，但 Go 报错: %v", err)
		return
	}
	if gotText := canonical.Dumps(got); gotText != entry.Output {
		t.Errorf("输出不一致\n期望: %s\n实际: %s", entry.Output, gotText)
	}
}

// TestMigrateMatchesPython 是 config 迁移的核心对拍断言。
func TestMigrateMatchesPython(t *testing.T) {
	for _, entry := range loadCorpus(t, "migrate.jsonl") {
		t.Run(entry.Name, func(t *testing.T) {
			got, err := MigrateConfigData(mustParse(t, entry.Input))
			checkOutcome(t, entry, got, err)
		})
	}
}

// TestNormalizeUpstreamRoutesMatchesPython 对拍路由规范化。
func TestNormalizeUpstreamRoutesMatchesPython(t *testing.T) {
	for _, entry := range loadCorpus(t, "routes.jsonl") {
		t.Run(entry.Name, func(t *testing.T) {
			routes, err := NormalizeUpstreamRoutes(mustParse(t, entry.Input))
			var got *canonical.Value
			if err == nil {
				got = routesToValue(routes)
			}
			checkOutcome(t, entry, got, err)
		})
	}
}

// TestNormalizeTaskParamsMatchesPython 对拍任务参数规范化。
func TestNormalizeTaskParamsMatchesPython(t *testing.T) {
	for _, entry := range loadCorpus(t, "taskparams.jsonl") {
		t.Run(entry.Name, func(t *testing.T) {
			taskName := "TASK_1"
			if raw, ok := entry.Kwargs["task_name"]; ok {
				if parsed, err := canonical.ParseString(raw); err == nil {
					taskName, _ = parsed.AsString()
				}
			}
			got, err := NormalizeTaskParams(mustParse(t, entry.Input), taskName)
			checkOutcome(t, entry, got, err)
		})
	}
}

// routesToValue 把路由 map 转成 canonical Value，便于与语料比对。
//
// 语料的 expected 是 canonical 形式（键已排序），因此这里同样按键排序构造。
func routesToValue(routes map[string]string) *canonical.Value {
	obj := canonical.NewObject()
	for _, mode := range sortedKeys(routes) {
		obj.SetKey(mode, canonical.NewString(routes[mode]))
	}
	return obj
}
