package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件回放 gen_management_api_corpus.py（已随 Python 退役移除） 生成的语料。
//
// 语料由真实 Python FastAPI 应用产出，断言「状态码 + 响应体字节 + content-type /
// content-length + 变异请求后的配置文件」四件事。任何一处与 Go 实现不一致都算失败，
// 因此这里不做任何「宽容化」：不忽略 content-length，也不把 500 纯文本当噪音。

const corpusPath = "testdata/management_api_corpus.json"

// 语料里两个被 Python 侧 monkeypatch 的常量。
const (
	corpusVisitorKey    = "amkr-visitor"
	corpusLocalAPIKey   = "amkr_CORPUS0000000000000000000000000000000000000"
	corpusLocalAuthKey  = "local-key"
	corpusFixedUUIDHex  = "00112233445566778899aabbccddeeff"
	corpusProbeFailText = "upstream rejected Authorization: Bearer sk-secret-a"
)

// corpusCase 是语料里的一条用例。
//
// 元数据用 encoding/json 解，因为它们的类型是确定的；但**载荷与配置**必须走
// canonical 解析：Go 的 encoding/json 会把所有数字解成 float64（9000 变成 9000.0），
// 而语料里的 int/float 区别是可观察的（422 错误里的 input 与 ctx 都依赖它），
// 并且对象键顺序决定 Pydantic 的 extra_forbidden 顺序。
type corpusCase struct {
	Name          string   `json:"name"`
	Method        string   `json:"method"`
	Path          string   `json:"path"`
	Auth          string   `json:"auth"`
	Covers        []string `json:"covers"`
	Status        int      `json:"status"`
	ContentType   *string  `json:"content_type"`
	ContentLength *string  `json:"content_length"`
	BodyText      string   `json:"body_text"`
	DeleteConfig  bool     `json:"delete_config"`

	bodyKind               string
	bodyValue              *canonical.Value
	configReplace          *canonical.Value
	configAfter            *canonical.Value
	setup                  []corpusSetupStep
	patchFailCapabilities  bool
	patchAvailabilityError string
	// resolveRevision 让测试用现造的请求也能写 "$REV"。语料用例不需要它，因为生成
	// 脚本已经把版本号解析好写进语料了。
	resolveRevision bool
}

// revisionPlaceholder 是测试里表示「当前 config_revision」的占位符。
const revisionPlaceholder = "$REV"

// corpusSetupStep 是主请求之前要执行的准备请求（探测类用例用它预热状态）。
//
// 语料里准备请求的 $REV 也已经在生成时解析过，Go 侧直接原样回放。
type corpusSetupStep struct {
	Method string
	Path   string
	Body   *canonical.Value
}

type corpusData struct {
	version int
	fixture *canonical.Value
	cases   []corpusCase
}

// configPathPlaceholder 是语料响应体里「配置文件完整路径」的占位符。
//
// 语料录制的是生成机上的绝对路径（Windows 临时目录），而 409 的 detail 里那条路径
// 来自**回放时真正使用的配置文件位置**（replay.configPath）；同时 Windows 的 `\`
// 与 POSIX 的 `/` 也不能写死。因此语料只记录占位符，回放时用
// jsonEscapedPath(replay.configPath) 替换——与 internal/server 语料的
// <FIXTURE_DIR> 是同一套做法。
//
// 刻意不用 filepath.Join(fixtureDir, "router-config.json") 去还原：语料里的分隔符
// 是录制时的 `\`，Join 在 Linux 上会拼出混合分隔符，替换一个字节也匹配不到。
const configPathPlaceholder = "<CONFIG_PATH>"

// jsonEscapedPath 把路径转成它出现在 JSON 响应体里的样子。
//
// 409 的 detail 本身是 JSON 字符串，路径里的反斜杠在**响应体字节**里是 `\\`，
// 因此替换时必须用转义后的形式，否则 ReplaceAll 匹配不到。
func jsonEscapedPath(path string) string {
	return strings.ReplaceAll(path, `\`, `\\`)
}

// loadCorpus 读取并解析语料。
func loadCorpus(t *testing.T) *corpusData {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var meta struct {
		Version int          `json:"version"`
		Cases   []corpusCase `json:"cases"`
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
	for i := range meta.Cases {
		node := casesValue.Arr[i]
		entry := &meta.Cases[i]
		if body := node.Lookup("body"); body != nil {
			entry.bodyKind = body.Lookup("kind").StringValue()
			if value, present := body.LookupOK("value"); present {
				entry.bodyValue = value
			}
		}
		if replaced, present := node.LookupOK("config_replace"); present && !replaced.IsNull() {
			entry.configReplace = replaced
		}
		if after, present := node.LookupOK("config_after"); present && !after.IsNull() {
			if text, ok := after.AsString(); ok {
				entry.configAfter = mustParseText(t, text)
			}
		}
		if patch := node.Lookup("patch_state"); patch != nil {
			entry.patchFailCapabilities = patch.Lookup("fail_capabilities").Truthy()
			entry.patchAvailabilityError = patch.Lookup("availability_error").StringValue()
		}
		if setup := node.Lookup("setup"); setup != nil && setup.IsArray() {
			for _, step := range setup.Arr {
				entry.setup = append(entry.setup, corpusSetupStep{
					Method: step.Lookup("method").StringValue(),
					Path:   step.Lookup("path").StringValue(),
					Body:   step.Lookup("body").Lookup("value"),
				})
			}
		}
	}
	return &corpusData{
		version: meta.Version,
		fixture: root.Lookup("fixture"),
		cases:   meta.Cases,
	}
}

// mustParseText 解析一段 canonical JSON 文本（语料里的 config_after）。
func mustParseText(t *testing.T, text string) *canonical.Value {
	t.Helper()
	value, err := canonical.ParseString(text)
	if err != nil {
		t.Fatalf("解析 canonical 文本失败: %v", err)
	}
	return value
}

// corpusIndex 按名字找用例下标。
func (c *corpusData) corpusIndex(t *testing.T, name string) int {
	t.Helper()
	for i := range c.cases {
		if c.cases[i].Name == name {
			return i
		}
	}
	t.Fatalf("语料里没有用例 %q", name)
	return -1
}

// substitutePaths 把语料里的固定临时目录换成当前用例的目录。
//
// 只用于**响应体**里的路径（例如 409「配置文件不存在: ...」）。夹具内容不能被替换，
// 否则 config_revision 会随回放目录变化（夹具里的三个路径字段是整份配置哈希的一部分，
// 回放时改写它们会让所有带 config_revision 的响应都对不上）。
func substitutePaths(value *canonical.Value, from, to string) *canonical.Value {
	if from == "" || value == nil {
		return value.Clone()
	}
	switch value.Kind {
	case canonical.KindString:
		return canonical.NewString(strings.ReplaceAll(value.Str, from, to))
	case canonical.KindArray:
		items := make([]*canonical.Value, 0, len(value.Arr))
		for _, item := range value.Arr {
			items = append(items, substitutePaths(item, from, to))
		}
		return canonical.NewArray(items...)
	case canonical.KindObject:
		out := canonical.NewObject()
		for _, key := range value.Obj.Keys() {
			child, _ := value.Obj.Get(key)
			out.SetKey(key, substitutePaths(child, from, to))
		}
		return out
	default:
		return value.Clone()
	}
}

// corpusReplay 是一次回放的结果。
type corpusReplay struct {
	recorder   *httptest.ResponseRecorder
	server     *Server
	configPath string
}

// body 返回响应体文本。
func (r *corpusReplay) body() string { return r.recorder.Body.String() }

// status 返回状态码。
func (r *corpusReplay) status() int { return r.recorder.Result().StatusCode }

// deleteConfigFile 删除配置文件，用于「配置文件不存在」的用例。
func (r *corpusReplay) deleteConfigFile(t *testing.T) {
	t.Helper()
	if err := os.Remove(r.configPath); err != nil {
		t.Fatalf("删除配置失败: %v", err)
	}
}

// configOnDisk 返回磁盘上配置文件的 canonical 形式。
func (r *corpusReplay) configOnDisk(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(r.configPath)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	value, err := canonical.Parse(raw)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	return canonical.Dumps(value)
}

// corpusHarness 为语料用例准备可回放的服务端。
type corpusHarness struct {
	corpus *corpusData
	t      *testing.T
}

// replay 回放某条用例。
func (h *corpusHarness) replay(index int) *corpusReplay {
	h.t.Helper()
	entry := h.corpus.cases[index]
	return h.replayEntry(entry)
}

// replayEntry 用给定用例（可以是临时改造过的副本）回放。
func (h *corpusHarness) replayEntry(entry corpusCase) *corpusReplay {
	h.t.Helper()
	dir := h.t.TempDir()
	configPath := filepath.Join(dir, "router-config.json")

	// 夹具**原样**使用：里面的路径字段参与 config_revision，改写它们会让所有带
	// 版本号的响应都对不上（只有配置文件的落盘位置允许不同）。
	fixture := h.corpus.fixture.Clone()
	migrated, err := config.MigrateConfigData(fixture)
	if err != nil {
		h.t.Fatalf("迁移夹具失败: %v", err)
	}
	if err := config.SaveConfigData(configPath, migrated); err != nil {
		h.t.Fatalf("写入夹具失败: %v", err)
	}
	// 对应 create_app 载入的运行时配置：此后即使磁盘被改坏，这份快照仍然有效
	// （参照实现的 _reload_config_if_changed 在解析失败时静默保留旧配置，
	// broken/* 用例正是靠这一点才有 500/400 两种表现）。
	initial, err := config.Load(configPath)
	if err != nil {
		h.t.Fatalf("载入夹具失败: %v", err)
	}

	server := newCorpusServer(configPath, initial, entry)
	handler := server.Handler()
	replay := &corpusReplay{server: server, configPath: configPath}

	// 请求执行顺序与生成脚本完全一致：写夹具 -> （可选）替换配置 -> 准备请求 ->
	// （可选）删除配置 -> 主请求。顺序影响 config_revision，不能调整。
	send := func(method, path string, value *canonical.Value, hasBody bool) *httptest.ResponseRecorder {
		var body io.Reader
		if hasBody {
			body = strings.NewReader(canonical.DumpsOrdered(value))
		}
		request := httptest.NewRequest(method, path, body)
		switch entry.Auth {
		case "none":
		case "visitor":
			request.Header.Set("Authorization", "Bearer "+corpusVisitorKey)
		default:
			request.Header.Set("Authorization", "Bearer "+corpusLocalAuthKey)
		}
		if hasBody {
			request.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		server.waitProbes()
		return recorder
	}

	if entry.configReplace != nil {
		replaced, err := config.MigrateConfigData(entry.configReplace.Clone())
		if err != nil {
			h.t.Fatalf("迁移替换配置失败: %v", err)
		}
		if err := config.SaveConfigData(configPath, replaced); err != nil {
			h.t.Fatalf("写入替换配置失败: %v", err)
		}
	}
	for _, step := range entry.setup {
		send(step.Method, step.Path, step.Body, true)
	}
	if entry.resolveRevision && entry.bodyValue != nil {
		entry.bodyValue = resolveRevisionPlaceholder(h.t, configPath, entry.bodyValue)
	}
	if entry.DeleteConfig {
		replay.deleteConfigFile(h.t)
	}
	replay.recorder = send(entry.Method, entry.Path, entry.bodyValue, entry.bodyKind == "json")
	return replay
}

// resolveRevisionPlaceholder 把请求体里的 "$REV" 换成当前磁盘配置的版本号。
//
// 只在测试现造请求时使用（corpusCase.resolveRevision）：语料本身记录的是生成时已经
// 解析好的版本号，不能二次替换。
func resolveRevisionPlaceholder(t *testing.T, configPath string, value *canonical.Value) *canonical.Value {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	data, err := canonical.Parse(raw)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	migrated, err := config.MigrateConfigData(data)
	if err != nil {
		t.Fatalf("迁移配置失败: %v", err)
	}
	return replaceRevisionPlaceholder(value, canonical.RevisionHash(migrated))
}

// replaceRevisionPlaceholder 递归替换字符串里的 "$REV"。
func replaceRevisionPlaceholder(value *canonical.Value, revision string) *canonical.Value {
	switch value.Kind {
	case canonical.KindString:
		return canonical.NewString(strings.ReplaceAll(value.Str, revisionPlaceholder, revision))
	case canonical.KindArray:
		items := make([]*canonical.Value, 0, len(value.Arr))
		for _, item := range value.Arr {
			items = append(items, replaceRevisionPlaceholder(item, revision))
		}
		return canonical.NewArray(items...)
	case canonical.KindObject:
		out := canonical.NewObject()
		for _, key := range value.Obj.Keys() {
			child, _ := value.Obj.Get(key)
			out.SetKey(key, replaceRevisionPlaceholder(child, revision))
		}
		return out
	default:
		return value.Clone()
	}
}

// newCorpusServer 构造注入了语料常量接缝的服务端。
//
// 与真实装配的差异只在「外部依赖的实现」：真实服务用 config_editor / update /
// metrics，而这些在语料生成时被 Python 侧 monkeypatch 成了确定性桩；这里必须给出
// 逐字相同的桩，否则比对的是桩与桩的差异，而不是 API 逻辑。
func newCorpusServer(configPath string, initial *config.RouterConfig, entry corpusCase) *Server {
	return &Server{
		ConfigPath: configPath,
		CurrentConfig: func() *config.RouterConfig {
			return initial
		},
		UUIDHex:             func() string { return corpusFixedUUIDHex },
		GenerateLocalAPIKey: func() (string, error) { return corpusLocalAPIKey, nil },
		ProbeKeyCapability: func(provider *canonical.Value, keyName string, modes []string, timeout float64) (*canonical.Value, error) {
			result := canonical.NewObject()
			result.SetKey("models", canonical.NewStringArray([]string{"model-a", "model-b"}))
			routes := canonical.NewObject()
			routes.SetKey("openai", canonical.NewBool(true))
			routes.SetKey("anthropic", canonical.NewBool(false))
			result.SetKey("routes", routes)
			modesValue := canonical.NewNull()
			if modes != nil {
				modesValue = canonical.NewStringArray(modes)
			}
			result.SetKey("modes", modesValue)
			return result, nil
		},
		ProbeProviderKeyCapabilities: func(provider *canonical.Value, keyNames []string, timeout float64) (*canonical.Value, error) {
			if entry.patchFailCapabilities {
				return nil, &stubError{message: corpusProbeFailText}
			}
			models := canonical.NewObject()
			for _, name := range keyNames {
				models.SetKey(name, canonical.NewStringArray([]string{"model-a"}))
			}
			result := canonical.NewObject()
			result.SetKey("key_models", models)
			return result, nil
		},
		ProbeKeyAvailability: func(ctx context.Context, routes *canonical.Value, model string, key *canonical.Value, timeout float64) ([]ProbeAvailability, error) {
			if entry.patchAvailabilityError != "" {
				return []ProbeAvailability{{
					Available:  false,
					URL:        "https://a.example.test/v1/models",
					DurationMS: 250,
					Error:      entry.patchAvailabilityError,
				}}, nil
			}
			return []ProbeAvailability{{
				Available:  true,
				URL:        "https://a.example.test/v1/models",
				DurationMS: 250,
			}}, nil
		},
		CheckUpdate: func(timeout float64) UpdateCheckResult {
			return UpdateCheckResult{
				CurrentVersion:  "1.2.3",
				LatestVersion:   "1.3.0",
				ReleaseURL:      "https://example.test/release",
				Source:          "pypi",
				ArtifactURL:     "https://example.test/a.whl",
				ArtifactSHA256:  "deadbeef",
				UpdateAvailable: true,
			}
		},
		KeyStats: func(modelID, keyName string, hours *float64) (*canonical.Value, error) {
			// Python 侧的桩把 hours 原样回显，好让语料能断言查询参数的解析行为。
			hoursValue := canonical.NewNull()
			if hours != nil {
				hoursValue = canonical.NewFloat(*hours)
			}
			stats := canonical.NewObject()
			stats.SetKey("requests", canonical.NewInt("0"))
			return objectOf(
				canonical.ObjectPair{Key: "model_id", Value: canonical.NewString(modelID)},
				canonical.ObjectPair{Key: "key_name", Value: canonical.NewString(keyName)},
				canonical.ObjectPair{Key: "hours", Value: hoursValue},
				canonical.ObjectPair{Key: "stats", Value: stats},
			), nil
		},
	}
}

// stubError 是测试用的固定错误。
type stubError struct{ message string }

func (e *stubError) Error() string { return e.message }

// waitProbes 等待所有后台探测任务结束，让后续 GET /api/probes/{id} 看到终态。
func (s *Server) waitProbes() {
	s.probesMu.Lock()
	records := make([]*probeRecord, 0, len(s.probes))
	for _, record := range s.probes {
		records = append(records, record)
	}
	s.probesMu.Unlock()
	for _, record := range records {
		<-record.done
	}
}

// TestManagementAPIMatchesPython 是主对拍：逐条回放并逐字节比对。
func TestManagementAPIMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	if corpus.version != 1 {
		t.Fatalf("语料版本不支持: %d", corpus.version)
	}
	harness := &corpusHarness{corpus: corpus, t: t}
	for index := range corpus.cases {
		entry := corpus.cases[index]
		t.Run(entry.Name, func(t *testing.T) {
			replay := harness.replay(index)
			if got := replay.status(); got != entry.Status {
				t.Fatalf("状态码不一致: got %d want %d, body=%s", got, entry.Status, replay.body())
			}
			if got := replay.recorder.Header().Get("Content-Type"); !equalOptionalHeader(got, entry.ContentType) {
				t.Errorf("content-type 不一致: got %q want %v", got, derefString(entry.ContentType))
			}
			// 只有「配置文件不存在」的 409 会把**配置文件路径**写进响应体：语料里记的是
			// <CONFIG_PATH> 占位符，这里换成回放时真正用的路径（按 JSON 规则转义，Windows
			// 上是 `\\`、POSIX 上是 `/`）；配置内容里的路径不做替换。
			wantBody := strings.ReplaceAll(
				entry.BodyText,
				configPathPlaceholder,
				jsonEscapedPath(replay.configPath),
			)
			// 路径换过之后长度自然变了，此时 content-length 的期望值要跟着体一起重算，
			// 否则这条断言只在两条路径等长时才能通过。
			wantLength := entry.ContentLength
			if wantBody != entry.BodyText {
				length := strconv.Itoa(len(wantBody))
				wantLength = &length
			}
			if got := replay.recorder.Header().Get("Content-Length"); !equalOptionalHeader(got, wantLength) {
				t.Errorf("content-length 不一致: got %q want %v", got, derefString(wantLength))
			}
			if got := replay.body(); got != wantBody {
				t.Errorf("响应体不一致:\n got=%s\nwant=%s", got, wantBody)
			}
			if entry.configAfter != nil {
				want := canonical.Dumps(entry.configAfter.Clone())
				if got := replay.configOnDisk(t); got != want {
					t.Errorf("配置文件不一致:\n got=%s\nwant=%s", got, want)
				}
			}
		})
	}
}

// equalOptionalHeader 比较可选响应头（nil 表示 Python 没有发这个头）。
func equalOptionalHeader(got string, want *string) bool {
	if want == nil {
		return got == ""
	}
	return got == *want
}

// derefString 把可选字符串渲染成日志友好的形式。
func derefString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// TestCorpusCoversEveryRoute 断言 47 条路由每条都被至少一条语料用例覆盖。
func TestCorpusCoversEveryRoute(t *testing.T) {
	corpus := loadCorpus(t)
	covered := map[string]bool{}
	for _, entry := range corpus.cases {
		for _, pattern := range entry.Covers {
			covered[pattern] = true
		}
	}
	patterns := routePatterns()
	if len(patterns) != 47 {
		t.Fatalf("路由数量不对: %d", len(patterns))
	}
	for _, pattern := range patterns {
		if !covered[pattern] {
			t.Errorf("路由未被语料覆盖: %s", pattern)
		}
	}
	for pattern := range covered {
		if !containsPattern(patterns, pattern) {
			t.Errorf("语料声明了不存在的路由: %s", pattern)
		}
	}
}

// containsPattern 报告模式是否在清单里。
func containsPattern(patterns []string, target string) bool {
	for _, pattern := range patterns {
		if pattern == target {
			return true
		}
	}
	return false
}

// TestEveryRoutePatternIsRegistered 逐条把模式实例化并发请求，确认命中的不是兜底
// 404。
//
// ServeMux 不暴露已注册的模式，因此只能靠「发了请求但没落到 catch-all」来证明注册
// 成功。路径参数统一填 "x"，此刻返回 404/400/500 都无所谓，唯独不能是兜底的那句
// {"detail":"Not Found"}。
func TestEveryRoutePatternIsRegistered(t *testing.T) {
	corpus := loadCorpus(t)
	harness := &corpusHarness{corpus: corpus, t: t}
	replacer := strings.NewReplacer(
		"{task_name}", "x", "{provider_id}", "x", "{key_name}", "x",
		"{route_id}", "x", "{model_id}", "x", "{probe_id}", "x",
	)
	for _, pattern := range routePatterns() {
		parts := strings.SplitN(pattern, " ", 2)
		entry := corpusCase{
			Method:   parts[0],
			Path:     replacer.Replace(parts[1]),
			Auth:     "full",
			bodyKind: "none",
		}
		replay := harness.replayEntry(entry)
		if replay.status() == 404 && replay.body() == `{"detail":"Not Found"}` {
			t.Errorf("路由未注册: %s", pattern)
		}
	}
}
