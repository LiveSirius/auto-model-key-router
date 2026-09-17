package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件覆盖 _reload_config_if_changed（app.py:443-475）的四条可观测语义。
//
// 语料里已有一条用例（models_after_external_config_edit）从字节层钉住「外部改配置后
// 清单立刻变空」；这里补的是无法用响应字节表达的接线：
//
//	1. 重载真的**换了一代资源**（RuntimeManager 的当前代指针变了）；
//	2. metrics_db_path 不变时**复用同一个指标库**（否则每次热重载都会丢连接池与句柄）；
//	3. metrics_db_path 变化时换库；
//	4. 配置解析失败时**静默保留旧配置**（用户改坏了配置，服务不能跟着坏）。
//
// 四条都必须在「应用构造之后」改配置文件，且 mtime 必须真的变化——Windows 的文件
// 时间戳粒度是 100ns 级，30ms 的间隔足够（生成脚本用同一个值）。

// rewriteConfig 覆盖配置文件并确保 mtime 变化。
func rewriteConfig(t *testing.T, path string, text string) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("覆盖配置失败: %v", err)
	}
}

// emptyModelsConfig 返回一份「没有任何 provider/model」的配置文本。
//
// 保留 local_api_key 与三个路径，这样重载后 key pool 为空但服务仍然可用。
func emptyModelsConfig(dir string) string {
	return `{
  "config_version": 4,
  "host": "127.0.0.1",
  "port": 8000,
  "request_timeout": 10,
  "stream_first_byte_timeout": 30,
  "stream_idle_timeout": 60,
  "max_retries": 1,
  "key_failure_threshold": 1,
  "key_cooldown_seconds": 60,
  "endpoint_capabilities_path": "` + jsonPath(filepath.Join(dir, "endpoint-capabilities.json")) + `",
  "metrics_db_path": "` + jsonPath(filepath.Join(dir, "metrics.sqlite3")) + `",
  "log_file_path": "` + jsonPath(filepath.Join(dir, "server.log")) + `",
  "local_api_key": "local-key",
  "ops_enabled": true,
  "webui_enabled": false,
  "providers": {},
  "models": {}
}`
}

// TestReloadReplacesRuntimeGeneration 断言外部改配置后当前代被替换。
func TestReloadReplacesRuntimeGeneration(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, true, false)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	app, err := New(Options{ConfigPath: path, Config: loaded, Version: testVersion})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = app.Close() }()

	before := app.Manager().Current()
	if ids := modelIDs(t, serve(app, http.MethodGet, "/v1/models", fullAuthorization)); len(ids) == 0 {
		t.Fatal("初始清单不应为空")
	}

	rewriteConfig(t, path, emptyModelsConfig(dir))

	// 触发重载的请求本身也该看到新配置（热重载发生在处理请求之前）。
	if ids := modelIDs(t, serve(app, http.MethodGet, "/v1/models", fullAuthorization)); len(ids) != 0 {
		t.Fatalf("重载后的模型清单应为空，实际 %v", ids)
	}
	after := app.Manager().Current()
	if after == before {
		t.Fatal("配置变化后应当换一代运行时资源")
	}
	if !before.Retired() {
		t.Error("旧一代应被标记为 retired")
	}
	// metrics_db_path 未变 ⇒ 指标库与上游客户端必须复用（连接池/句柄不能每次重载都丢）。
	if after.Metrics != before.Metrics {
		t.Error("metrics_db_path 未变时不应更换指标库")
	}
	if after.HTTPClient != before.HTTPClient {
		t.Error("两个超时都未变时不应更换上游客户端")
	}
}

// TestReloadSwapsMetricsStore 断言 metrics_db_path 变化时换库。
func TestReloadSwapsMetricsStore(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, true, false)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	app, err := New(Options{ConfigPath: path, Config: loaded, Version: testVersion})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = app.Close() }()

	before := app.Manager().Current()
	moved := strings.Replace(
		emptyModelsConfig(dir),
		jsonPath(filepath.Join(dir, "metrics.sqlite3")),
		jsonPath(filepath.Join(dir, "moved.sqlite3")),
		1,
	)
	rewriteConfig(t, path, moved)

	// 任意一次 app 面请求都会触发重载。
	serve(app, http.MethodGet, "/health", "")
	after := app.Manager().Current()
	if after.Metrics == before.Metrics {
		t.Error("metrics_db_path 变化后应当更换指标库")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "moved.sqlite3")); statErr != nil {
		t.Errorf("新指标库文件未创建: %v", statErr)
	}
}

// TestReloadKeepsOldConfigWhenFileBroken 断言解析失败时静默保留旧配置。
//
// 这是可观测的：用户把配置改坏之后，服务仍然按旧配置工作（管理 API 也能读到旧配置
// 并用新内容修正它）。参照实现 app.py:452-455 的 `except (OSError, ValueError): return`。
func TestReloadKeepsOldConfigWhenFileBroken(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, true, false)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	app, err := New(Options{ConfigPath: path, Config: loaded, Version: testVersion})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = app.Close() }()

	before := app.Manager().Current()
	rewriteConfig(t, path, "{ 这不是 JSON")

	if ids := modelIDs(t, serve(app, http.MethodGet, "/v1/models", fullAuthorization)); len(ids) == 0 {
		t.Fatal("配置解析失败时应保留旧配置（清单不应变空）")
	}
	if app.Manager().Current() != before {
		t.Error("配置解析失败时不应换代")
	}
}

// TestReloadIgnoresUnchangedMtime 断言「mtime 没变就不重载」。
//
// 这条语义的价值在于性能与稳定：每次请求都重新解析 JSON 会让 /v1/models 这类热路径
// 多一次磁盘 IO 与 json 解析（参照实现刻意只在 mtime 变化时读盘）。
func TestReloadIgnoresUnchangedMtime(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, true, false)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	app, err := New(Options{ConfigPath: path, Config: loaded, Version: testVersion})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = app.Close() }()

	before := app.Manager().Current()
	for index := 0; index < 3; index++ {
		serve(app, http.MethodGet, "/health", "")
	}
	if app.Manager().Current() != before {
		t.Error("mtime 未变化时不应换代")
	}
}

// TestReloadWithoutConfigPathIsNoop 断言没有配置路径时热重载是空操作（app.py:446）。
func TestReloadWithoutConfigPathIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, true, false)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	// 刻意不传 ConfigPath：模拟「嵌入到宿主、配置由宿主自己管」的场景。
	app, err := New(Options{Config: loaded, Version: testVersion})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = app.Close() }()

	before := app.Manager().Current()
	rewriteConfig(t, path, emptyModelsConfig(dir))
	serve(app, http.MethodGet, "/health", "")
	if app.Manager().Current() != before {
		t.Error("没有配置路径时不应换代")
	}
	// /health 的 config_path 为空串（app.py:85-87 的语义）。
	parsed := serve(app, http.MethodGet, "/health", "").Body.String()
	if !strings.Contains(parsed, `"config_path":""`) {
		t.Errorf("/health 的 config_path 应为空串: %s", parsed)
	}
}
