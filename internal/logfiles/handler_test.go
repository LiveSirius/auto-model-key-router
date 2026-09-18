package logfiles

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOpenWritesPythonLogFormat 钉住落盘行格式：参照实现的 formatter 是
// `%(asctime)s %(levelname)s %(name)s %(message)s`，属性按 json.dumps 追加。
//
// 期望值取自历史日志的真实行（proxy_handler.py 的 "upstream stream error"），
// 逐字对齐（除时间戳）。
func TestOpenWritesPythonLogFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "server.log")
	sink, err := Open(path)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}

	// 时间戳由 handler 从 record.Time 取，这里无法注入时钟，因此按前缀断言。
	sink.App.Warn("upstream stream error",
		"model_id", "deepseek-v4.1-flash",
		"status_code", 200,
		"error_type", "ReadError",
		"chunks", 154,
	)
	sink.Access.Info(`127.0.0.1:50874 - "GET /metrics?hours=1 HTTP/1.1" 200`)
	if err := sink.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 行，得到 %d 行: %q", len(lines), lines)
	}

	// 第 1 行：应用日志。前缀是 `YYYY-MM-DD HH:MM:SS,mmm WARNING auto_model_key_router.app `。
	if !pythonTimestamp.MatchString(lines[0]) {
		t.Errorf("应用日志时间戳格式不符: %q", lines[0])
	}
	const appPrefix = " WARNING auto_model_key_router.app upstream stream error "
	if !strings.Contains(lines[0], appPrefix) {
		t.Errorf("应用日志前缀不符，期望包含 %q，得到 %q", appPrefix, lines[0])
	}
	// 属性是 json.dumps(ensure_ascii=False) 形态：数值不加引号，分隔符是 ", " 与 ": "。
	const attrs = `{"model_id": "deepseek-v4.1-flash", "status_code": 200, ` +
		`"error_type": "ReadError", "chunks": 154}`
	if !strings.HasSuffix(lines[0], attrs) {
		t.Errorf("应用日志属性不符\n期望后缀: %s\n实际整行: %s", attrs, lines[0])
	}

	// 第 2 行：访问日志，logger 名是 uvicorn.access，消息原样保留双引号。
	expected := ` INFO uvicorn.access 127.0.0.1:50874 - "GET /metrics?hours=1 HTTP/1.1" 200`
	if !strings.HasSuffix(lines[1], expected) {
		t.Errorf("访问日志不符\n期望后缀: %s\n实际整行: %s", expected, lines[1])
	}
}

// TestOpenAppendsAndLevelFilters 钉住两件容易改坏的事：以追加方式打开（前台启动前
// 已归档过旧日志，截断会抹掉归档后的新内容），以及 level="INFO" 的下界。
func TestOpenAppendsAndLevelFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte("既有内容\n"), 0o644); err != nil {
		t.Fatalf("预写日志失败: %v", err)
	}

	sink, err := Open(path)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	sink.App.Debug("这条不该出现")
	sink.App.Info("这条该出现")
	if err := sink.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "既有内容\n") {
		t.Errorf("旧内容被截断: %q", text)
	}
	if strings.Contains(text, "这条不该出现") {
		t.Errorf("DEBUG 不该落盘: %q", text)
	}
	if !strings.Contains(text, "INFO auto_model_key_router.app 这条该出现") {
		t.Errorf("INFO 未落盘: %q", text)
	}
}

// TestPythonLevelName 复刻 Python 的 levelname（WARN 在 Python 里叫 WARNING）。
func TestPythonLevelName(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARNING"},
		{slog.LevelError, "ERROR"},
	}
	for _, testCase := range cases {
		if got := pythonLevelName(testCase.level); got != testCase.want {
			t.Errorf("pythonLevelName(%v) = %q, 期望 %q", testCase.level, got, testCase.want)
		}
	}
}

// pythonTimestamp 匹配 Python logging 的 asctime：`%Y-%m-%d %H:%M:%S,mmm`。
var pythonTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3} `)

func TestOpenCreatesParentDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "server.log")
	sink, err := Open(path)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	defer func() { _ = sink.Close() }()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("日志文件未创建: %v", err)
	}
}
