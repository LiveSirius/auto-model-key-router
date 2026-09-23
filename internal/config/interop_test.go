package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestSaveNewlineMatchesPythonTextMode 固化 Python write_text 的换行行为。
//
// 实测（本机 Windows + Python 3.12）：
//
//	save_config_data 写出的字节为 b'{\r\n  "config_version": 4,\r\n...\r\n}\r\n'
//
// 即 Python 以文本模式打开文件时把 "\n" 翻译成 os.linesep。Go 的 os.WriteFile
// 不翻译，故必须显式补上，否则 Windows 下每个换行少一个 \r——文件功能上等价，
// 但「字节级兼容」的承诺就破了，且用户在两版之间切换时 git 会一直看到改动。
//
// 同时断言字符串**内部**的换行不受影响：JSON 里它是转义的两个字节（\ 与 n），
// 不是真实换行，因此不该被翻译。
func TestSaveNewlineMatchesPythonTextMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	// 值里带一个真实换行，用来验证它被转义而非被翻译。
	data := mustParse(t, `{"config_version":4,"multiline":"line1\nline2"}`)
	if err := SaveConfigData(path, data); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	text := string(raw)

	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(text, "{\r\n") {
			t.Errorf("Windows 下应以 CRLF 换行，实际开头: %q", text)
		}
		if strings.Contains(text, "\n") && !strings.Contains(text, "\r\n") {
			t.Error("Windows 下应全部使用 CRLF")
		}
	} else if strings.Contains(text, "\r\n") {
		t.Errorf("类 Unix 下不应出现 CRLF，实际: %q", text)
	}

	// 字符串内部的换行必须是转义形式（\ 后跟 n），且不被换行翻译影响。
	if !strings.Contains(text, `"line1\nline2"`) {
		t.Errorf("字符串内部的换行应保持 JSON 转义形式，实际:\n%s", text)
	}
	// 重新读取后值应完好。
	reloaded, err := LoadConfigData(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	got, _ := reloaded.Lookup("multiline").AsString()
	if got != "line1\nline2" {
		t.Errorf("往返后多行字符串损坏: %q", got)
	}
}

// TestUnknownFieldsSurviveRoundTrip 断言未知字段不会被丢弃。
//
// 后果很实际：若 Go 在保存时丢掉不认识的字段（例如未来版本新增的配置项），
// 用户用旧版 Go 打开一次配置就会永久丢失数据。解析时可忽略，写回时必须保留。
func TestUnknownFieldsSurviveRoundTrip(t *testing.T) {
	raw := mustParse(t, `{"config_version":4,"local_api_key":"k","future_feature":{"nested":[1,2]},"models":{},"providers":{}}`)
	text := canonical.DumpsIndent(raw, 2) + "\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	loaded, err := LoadConfigData(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !loaded.Obj.Has("future_feature") {
		t.Fatal("未知字段 future_feature 在加载后丢失")
	}
	if got := canonical.Dumps(loaded.Lookup("future_feature")); got != `{"nested":[1,2]}` {
		t.Errorf("未知字段内容改变: %s", got)
	}
	// 解析为配置对象同样不应因为存在未知字段而失败。
	if _, err := FromDict(loaded); err != nil {
		t.Errorf("未知字段导致解析失败: %v", err)
	}
}
