package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件回放 scripts/gen_server_corpus.py 生成的语料。
//
// 语料由**真实 Python FastAPI 应用**产出，断言「状态码 + 响应体字节 + content-type
// + content-length」。与 internal/api 的语料回放同一套做法：不做任何宽容化。
//
// # 两处回放侧的必要处理
//
//  1. **时间与路径归一化**。响应体里的窗口时间、started_at、系列点位时间戳来自墙钟，
//     而两侧都没有跨 HTTP 边界的可注入时钟，因此生成时已把它们替换成
//     <TIMESTAMP>、把夹具目录替换成 <FIXTURE_DIR>；这里用同样的规则归一化 Go 的
//     响应体再比对。归一化只覆盖这两类内容，其余字节（键序、`24.0` 与 `1e-05` 这种
//     浮点写法、Pydantic 错误文本）全部逐字节比对。
//     副作用：被归一化的用例不再比对 content-length（占位符长度与真实值不同），
//     改为断言「响应头与自身体长一致」；未归一化的用例照旧逐字节比对长度。
//  2. **HEAD 的响应体**。真实 http.Server 会丢弃 HEAD 的响应体（保留显式
//     Content-Length），而 httptest.ResponseRecorder 只是记录处理器写了什么。
//     因此回放 HEAD 用例时用「空体」与语料比较，长度仍按语料断言。
const serverCorpusPath = "testdata/server_corpus.json"

// 归一化占位符，与 scripts/gen_server_corpus.py 中的常量一一对应。
const (
	corpusDirPlaceholder       = "<FIXTURE_DIR>"
	corpusTimestampPlaceholder = "<TIMESTAMP>"
)

// corpusTimestampPattern 与生成脚本的正则逐字相同。
var corpusTimestampPattern = regexp.MustCompile(
	`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[+-]\d{2}:\d{2})?`)

// serverCorpusCase 是语料里的一条用例（元数据用 encoding/json 解）。
type serverCorpusCase struct {
	Name           string   `json:"name"`
	Method         string   `json:"method"`
	Path           string   `json:"path"`
	Auth           string   `json:"auth"`
	Covers         []string `json:"covers"`
	Status         int      `json:"status"`
	ContentType    *string  `json:"content_type"`
	ContentLength  *string  `json:"content_length"`
	BodyText       string   `json:"body_text"`
	BodyNormalized bool     `json:"body_normalized"`

	// configReplace 是请求前要写进配置文件的新配置（触发热重载）；从 canonical 树取。
	configReplace *canonical.Value
}

// serverCorpus 是整份语料。
type serverCorpus struct {
	version    int
	fixtureDir string
	fixture    *canonical.Value
	cases      []serverCorpusCase
	replaces   map[string]*canonical.Value
}

// loadServerCorpus 读取并解析语料。
func loadServerCorpus(t *testing.T) *serverCorpus {
	t.Helper()
	raw, err := os.ReadFile(serverCorpusPath)
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var meta struct {
		Version    int                `json:"version"`
		FixtureDir string             `json:"fixture_dir"`
		Cases      []serverCorpusCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("解析语料元数据失败: %v", err)
	}
	root, err := canonical.Parse(raw)
	if err != nil {
		t.Fatalf("canonical 解析语料失败: %v", err)
	}
	casesValue := root.Lookup("cases")
	if casesValue == nil || !casesValue.IsArray() || len(casesValue.Arr) != len(meta.Cases) {
		t.Fatalf("语料用例数不一致: canonical=%d json=%d", casesValue.Len(), len(meta.Cases))
	}
	replaces := map[string]*canonical.Value{}
	for i := range meta.Cases {
		entry := &meta.Cases[i]
		node := casesValue.Arr[i]
		if replaced, present := node.LookupOK("config_replace"); present && !replaced.IsNull() {
			entry.configReplace = replaced
			replaces[entry.Name] = replaced
		}
	}
	return &serverCorpus{
		version:    meta.Version,
		fixtureDir: meta.FixtureDir,
		fixture:    root.Lookup("fixture"),
		cases:      meta.Cases,
		replaces:   replaces,
	}
}

// jsonEscaped 返回路径在 JSON 字符串里的样子（与生成脚本的同名函数一致）。
//
// Windows 路径在响应体里是 `C:\\Users\\...`，直接替换原始路径一个字节也匹配不到。
func jsonEscaped(path string) string {
	return strings.ReplaceAll(path, `\`, `\\`)
}

// normalizeCorpusBody 把夹具目录与墙钟时间替换成占位符。
func normalizeCorpusBody(body, fixtureDir string) string {
	if fixtureDir != "" {
		body = strings.ReplaceAll(body, jsonEscaped(fixtureDir), corpusDirPlaceholder)
	}
	return corpusTimestampPattern.ReplaceAllString(body, corpusTimestampPlaceholder)
}

// substituteCorpusPaths 把语料里的固定夹具目录换成当前用例的目录。
//
// 与 internal/api 的语料回放不同，这里**可以**改写夹具内容：app 面路由（/v1/models
// 与三条 metrics）都不返回 config_revision，路径字段因此不是可观察内容。不改写的
// 后果是每个用例都会去写语料生成时那个固定的临时目录（并行测试互相踩）。
func substituteCorpusPaths(value *canonical.Value, from, to string) *canonical.Value {
	if value == nil {
		return nil
	}
	switch value.Kind {
	case canonical.KindString:
		return canonical.NewString(strings.ReplaceAll(value.Str, from, to))
	case canonical.KindArray:
		items := make([]*canonical.Value, 0, len(value.Arr))
		for _, item := range value.Arr {
			items = append(items, substituteCorpusPaths(item, from, to))
		}
		return canonical.NewArray(items...)
	case canonical.KindObject:
		out := canonical.NewObject()
		for _, key := range value.Obj.Keys() {
			child, _ := value.Obj.Get(key)
			out.SetKey(key, substituteCorpusPaths(child, from, to))
		}
		return out
	default:
		return value.Clone()
	}
}

// corpusAuthorization 把语料的 auth 标记折算成请求头。
func corpusAuthorization(t *testing.T, auth string) string {
	t.Helper()
	switch auth {
	case "none":
		return ""
	case "visitor":
		return "Bearer amkr-visitor"
	case "wrong":
		return "Bearer not-the-key"
	case "full":
		return "Bearer local-key"
	}
	t.Fatalf("语料里的 auth 取值未知: %q", auth)
	return ""
}

// replayServerCorpusCase 回放一条用例，返回记录器与「归一化后的 Go 响应体」。
func replayServerCorpusCase(t *testing.T, corpus *serverCorpus, entry serverCorpusCase) (*httptest.ResponseRecorder, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "router-config.json")

	initial := substituteCorpusPaths(corpus.fixture, corpus.fixtureDir, dir)
	migrated, err := config.MigrateConfigData(initial)
	if err != nil {
		t.Fatalf("迁移夹具失败: %v", err)
	}
	if err := config.SaveConfigData(configPath, migrated); err != nil {
		t.Fatalf("写入夹具失败: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("载入夹具失败: %v", err)
	}

	// 与生成脚本完全同序：写夹具 -> create_app（这里 New）-> （可选）替换配置 ->
	// 主请求。mtime 必须在应用构造**之后**变化，热重载才会触发。
	app, err := New(Options{ConfigPath: configPath, Config: loaded, Version: "0.0.0-corpus"})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() {
		if closeErr := app.Close(); closeErr != nil {
			t.Errorf("关停失败: %v", closeErr)
		}
	}()

	if entry.configReplace != nil {
		// 睡一小会儿再写：与生成脚本一样，保证 mtime 真的变化（文件系统时间戳
		// 粒度在 Windows 上是 100ns 级，这个间隔足够）。
		time.Sleep(30 * time.Millisecond)
		replaced := substituteCorpusPaths(entry.configReplace, corpus.fixtureDir, dir)
		replacedMigrated, err := config.MigrateConfigData(replaced)
		if err != nil {
			t.Fatalf("迁移替换配置失败: %v", err)
		}
		if err := config.SaveConfigData(configPath, replacedMigrated); err != nil {
			t.Fatalf("写入替换配置失败: %v", err)
		}
	}

	request := httptest.NewRequest(entry.Method, entry.Path, nil)
	if authorization := corpusAuthorization(t, entry.Auth); authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder, normalizeCorpusBody(recorder.Body.String(), dir)
}

// TestServerMatchesPython 是主对拍：逐条回放并逐字节比对。
func TestServerMatchesPython(t *testing.T) {
	corpus := loadServerCorpus(t)
	if corpus.version != 1 {
		t.Fatalf("语料版本不支持: %d", corpus.version)
	}
	if len(corpus.cases) == 0 {
		t.Fatal("语料为空")
	}
	for _, entry := range corpus.cases {
		t.Run(entry.Name, func(t *testing.T) {
			recorder, body := replayServerCorpusCase(t, corpus, entry)

			if got := recorder.Code; got != entry.Status {
				t.Fatalf("状态码不一致: got %d want %d（body=%s）",
					got, entry.Status, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); !equalOptionalHeader(got, entry.ContentType) {
				t.Errorf("content-type 不一致: got %q want %v",
					got, derefOrNull(entry.ContentType))
			}
			if entry.Method == "HEAD" {
				// httptest.ResponseRecorder 不会像真实 http.Server 那样丢弃 HEAD 的
				// 响应体（处理器写了什么就记什么），因此 HEAD 用例只比对状态码与
				// 响应头——参照实现的空体是 HTTP 服务器行为，不是处理器行为。
			} else if body != entry.BodyText {
				t.Errorf("响应体不一致:\n got=%s\nwant=%s", body, entry.BodyText)
			}
			if entry.BodyNormalized {
				// 归一化过的响应体长度不可比（占位符与真实时间/路径的长度不同），
				// 退一步只断言响应头与自身体长自洽。
				if header := recorder.Header().Get("Content-Length"); header != "" {
					if header != strconv.Itoa(recorder.Body.Len()) {
						t.Errorf("content-length = %s 与体长 %d 不一致",
							header, recorder.Body.Len())
					}
				}
				return
			}
			if got := recorder.Header().Get("Content-Length"); !equalOptionalHeader(got, entry.ContentLength) {
				t.Errorf("content-length 不一致: got %q want %v",
					got, derefOrNull(entry.ContentLength))
			}
		})
	}
}

// TestServerCorpusCoversEveryRoute 断言语料覆盖了 app 面的每一条路由。
//
// 清单与 routes.go 的注册手工对齐；新增路由时必须同步（否则这条测试不会失败——
// 因此它同时检查「语料声明的路由都在清单里」这一反向，防止笔误）。
func TestServerCorpusCoversEveryRoute(t *testing.T) {
	corpus := loadServerCorpus(t)
	covered := map[string]bool{}
	for _, entry := range corpus.cases {
		for _, pattern := range entry.Covers {
			covered[pattern] = true
		}
	}
	// 与 internal/server/routes.go 的注册一一对应。
	expected := []string{
		"HEAD /",
		"GET /",
		"HEAD /health",
		"POST /health",
		"GET /v1/models",
		"GET /metrics",
		"GET /metrics/requests",
		"GET /metrics/series",
		"GET /v1/{path}",
		"GET /api/providers",
	}
	if len(expected) != 10 {
		t.Fatalf("app 面路由数量不对: %d", len(expected))
	}
	for _, pattern := range expected {
		if !covered[pattern] {
			t.Errorf("路由未被语料覆盖: %s", pattern)
		}
	}
	for pattern := range covered {
		found := false
		for _, candidate := range expected {
			if candidate == pattern {
				found = true
			}
		}
		if !found {
			t.Errorf("语料声明了清单以外的路由: %s", pattern)
		}
	}
}

// derefOrNull 把可选字符串渲染成日志友好的形式。
func derefOrNull(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// equalOptionalHeader 比较可选响应头（nil 表示参照实现没有发这个头）。
//
// 与 internal/api 的语料回放同名同义：Go 侧读出来永远是字符串，Python 侧可能是
// None，因此 nil 必须映射成「头不存在」而不是「头等于空串」。
func equalOptionalHeader(got string, want *string) bool {
	if want == nil {
		return got == ""
	}
	return got == *want
}
