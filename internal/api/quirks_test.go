package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件把主对拍之外**必须单独点名**的行为锁成具名测试。
//
// 主对拍（TestManagementAPIMatchesPython）已经逐字节覆盖了这些场景，这里再写一遍是
// 为了让「我们刻意保留了哪个怪癖」在测试名里可见：语料被重生成时，如果某个怪癖被
// 「修好」了，这些测试会以人类可读的方式失败，而不是只报一个字节差异。

// replayNamed 回放语料里已有的用例。
func (h *corpusHarness) replayNamed(name string) *corpusReplay {
	h.t.Helper()
	return h.replay(h.corpus.corpusIndex(h.t, name))
}

// replayRequest 现造一个请求回放；bodyJSON 为空表示不带请求体。
//
// "$REV" 会被替换成请求发出时的 config_revision（见 resolveRevisionPlaceholder）。
func (h *corpusHarness) replayRequest(method, path, bodyJSON, auth string) *corpusReplay {
	h.t.Helper()
	entry := corpusCase{Method: method, Path: path, Auth: auth, bodyKind: "none", resolveRevision: true}
	if bodyJSON != "" {
		entry.bodyKind = "json"
		entry.bodyValue = mustParseText(h.t, bodyJSON)
	}
	return h.replayEntry(entry)
}

// newHarness 载入语料并构造回放器。
func newHarness(t *testing.T) *corpusHarness {
	t.Helper()
	return &corpusHarness{corpus: loadCorpus(t), t: t}
}

// fixtureCanonical 返回「刚写盘时」的配置 canonical 形式，用于断言「没被改动」。
func fixtureCanonical(t *testing.T, corpus *corpusData) string {
	t.Helper()
	migrated, err := config.MigrateConfigData(corpus.fixture.Clone())
	if err != nil {
		t.Fatalf("迁移夹具失败: %v", err)
	}
	return canonical.Dumps(migrated)
}

// TestConfigRevisionMismatchLeavesConfigUntouched 锁定「先比版本、再改」这条防丢改动
// 的不变量：版本不符时必须是 409，且磁盘上的配置一个字节都不能变。
func TestConfigRevisionMismatchLeavesConfigUntouched(t *testing.T) {
	harness := newHarness(t)
	want := fixtureCanonical(t, harness.corpus)
	names := []string{
		"settings/put-stale-revision",
		"providers/delete-stale-revision",
		"routes/delete-stale-revision",
		"models/delete-stale-revision",
		"tasks/delete-stale-revision",
		"unified/delete-stale-revision",
		"config/import-stale-revision",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			replay := harness.replayNamed(name)
			if got := replay.status(); got != http.StatusConflict {
				t.Fatalf("状态码: got %d want 409", got)
			}
			if got := replay.body(); got != `{"detail":"配置版本已变更，请刷新后重试"}` {
				t.Fatalf("响应体: %s", got)
			}
			if got := replay.configOnDisk(t); got != want {
				t.Errorf("版本不符时配置被改动了:\n got=%s\nwant=%s", got, want)
			}
		})
	}
}

// TestDeleteBodyOptionalityIsAsymmetric 锁定 DELETE 请求体的不对称：
//
//	unified / tasks / models / model-keys  : 请求体可选（不带 body 直接成功）
//	providers / routes / provider-keys     : 请求体必填（不带 body 是 422）
//
// 而且「可选」不等于「可省版本号」：带了 {} 反而会因为缺 config_revision 报 422。
func TestDeleteBodyOptionalityIsAsymmetric(t *testing.T) {
	harness := newHarness(t)
	optional := []string{
		"unified/delete-no-body",
		"tasks/delete-no-body",
		"models/delete-no-body",
		"model-keys/delete-no-body",
	}
	for _, name := range optional {
		t.Run(name, func(t *testing.T) {
			if got := harness.replayNamed(name).status(); got != http.StatusNoContent {
				t.Fatalf("可选请求体的 DELETE 应当是 204, got %d", got)
			}
		})
	}
	required := []string{
		"providers/delete-no-body",
		"routes/delete-no-body",
		"provider-keys/delete-no-body",
	}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			replay := harness.replayNamed(name)
			if got := replay.status(); got != http.StatusUnprocessableEntity {
				t.Fatalf("必填请求体的 DELETE 应当是 422, got %d", got)
			}
			if got := replay.body(); got != `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}` {
				t.Fatalf("响应体: %s", got)
			}
		})
	}
	// 显式的 JSON null 与「没有请求体」等价（Pydantic 把 `X | None` 的 null 当缺席）。
	t.Run("unified/delete-null-body", func(t *testing.T) {
		if got := harness.replayNamed("unified/delete-null-body").status(); got != http.StatusNoContent {
			t.Fatalf("显式 null 的 DELETE 应当是 204, got %d", got)
		}
	})
	// 带了请求体就必须带版本号。
	t.Run("models/delete-empty-body", func(t *testing.T) {
		if got := harness.replayNamed("models/delete-empty-body").status(); got != http.StatusUnprocessableEntity {
			t.Fatalf("空对象请求体应当是 422, got %d", got)
		}
	})
}

// TestKeyResponseCarriesBaseURLAndRawKeyResponseDoesNot 锁定两个 key 响应形状的差异：
// 模型视角（KeyResponse）带 base_url 且不带 capabilities；供应商视角（RawKeyResponse）
// 反之。两者的指纹一致，且都**不含**明文 api_key。
func TestKeyResponseCarriesBaseURLAndRawKeyResponseDoesNot(t *testing.T) {
	harness := newHarness(t)

	modelKey := mustParseBody(t, harness.replayNamed("model-keys/get").body())
	for _, field := range []string{"name", "base_url", "enabled", "allow_visitor", "api_key_fingerprint"} {
		if modelKey.Lookup(field) == nil {
			t.Errorf("KeyResponse 缺少字段 %s", field)
		}
	}
	if modelKey.Lookup("capabilities") != nil {
		t.Error("KeyResponse 不应包含 capabilities")
	}

	rawKey := mustParseBody(t, harness.replayNamed("provider-keys/get").body())
	if rawKey.Lookup("base_url") != nil {
		t.Error("RawKeyResponse 不应包含 base_url")
	}
	if rawKey.Lookup("capabilities") == nil {
		t.Error("RawKeyResponse 应包含 capabilities（即使为 null）")
	}
	if got := modelKey.Lookup("api_key_fingerprint").Str; got != rawKey.Lookup("api_key_fingerprint").Str {
		t.Errorf("同一 key 的指纹不一致: %s vs %s", got, rawKey.Lookup("api_key_fingerprint").Str)
	}
	if got := modelKey.Lookup("api_key_fingerprint").Str; got != "65bbff9a6cb9" {
		t.Errorf("指纹应为 sha256[:12]: %s", got)
	}
}

// TestSecretsAreRedactedEverywhereExceptConfigExport 扫描**全部**语料响应：
// 上游密钥只允许出现在配置导出里（那是显式的备份接口），本地 API key 明文只允许出现
// 在重新生成接口里。
func TestSecretsAreRedactedEverywhereExceptConfigExport(t *testing.T) {
	corpus := loadCorpus(t)
	for _, entry := range corpus.cases {
		if strings.Contains(entry.BodyText, "sk-secret-") && entry.Name != "config/export" {
			t.Errorf("用例 %s 泄漏了上游密钥: %s", entry.Name, entry.BodyText)
		}
		if strings.Contains(entry.BodyText, corpusLocalAPIKey) && entry.Name != "settings/regenerate-key" {
			t.Errorf("用例 %s 泄漏了本地 API key 明文: %s", entry.Name, entry.BodyText)
		}
	}
}

// TestProbeFailureRedactsSecrets 锁定探测失败路径的脱敏：异常文本与探测结果里的
// Authorization 头、已知密钥都要被替换，且结果整体截断到 160 字符。
func TestProbeFailureRedactsSecrets(t *testing.T) {
	harness := newHarness(t)
	failed := harness.replayNamed("probes/get-failed")
	body := mustParseBody(t, failed.body())
	if got := body.Lookup("status").Str; got != "failed" {
		t.Fatalf("探测状态: %s", got)
	}
	message := body.Lookup("error").Str
	if strings.Contains(message, "sk-secret-a") {
		t.Errorf("探测错误未脱敏: %s", message)
	}
	if !strings.Contains(message, "[redacted]") {
		t.Errorf("探测错误缺少 [redacted] 标记: %s", message)
	}

	redacted := mustParseBody(t, harness.replayNamed("probes/get-redacted-availability-error").body())
	rows := redacted.Lookup("results")
	if rows.Len() == 0 {
		t.Fatal("探测结果为空")
	}
	rowError := rows.Index(0).Lookup("error").Str
	if strings.Contains(rowError, "sk-secret-a") || strings.Contains(rowError, "Bearer sk") {
		t.Errorf("探测结果未脱敏: %s", rowError)
	}
	if got := rows.Index(0).Lookup("status").Str; got != "failed" {
		t.Errorf("可用性失败的结果状态应为 failed: %s", got)
	}
}

// TestTaskParamWhitelistIsEnforcedTwice 锁定任务参数白名单的两道关卡：
// Pydantic（extra_forbidden，loc 在 params 之下）与 config 层（422 中文白名单提示）。
func TestTaskParamWhitelistIsEnforcedTwice(t *testing.T) {
	harness := newHarness(t)

	pydantic := harness.replayNamed("tasks/create-unknown-param")
	if got := pydantic.status(); got != http.StatusUnprocessableEntity {
		t.Fatalf("未知参数应当 422, got %d", got)
	}
	if !strings.Contains(pydantic.body(), `"extra_forbidden"`) ||
		!strings.Contains(pydantic.body(), `["body","params","nope"]`) {
		t.Fatalf("Pydantic 层错误形状不对: %s", pydantic.body())
	}

	// TaskParams 继承 APIModel，因此 config_revision 能通过 Pydantic，却被 config 层
	// 的白名单拒掉——这是「同一份白名单校验两次」最直观的例子。
	configLayer := harness.replayNamed("tasks/create-param-revision-quirk")
	if got := configLayer.status(); got != http.StatusUnprocessableEntity {
		t.Fatalf("params.config_revision 应当 422, got %d", got)
	}
	want := "任务 task-quirk 的 params 不支持的参数: config_revision（可用: temperature, top_p, top_k, " +
		"frequency_penalty, presence_penalty, seed, stop, reasoning_effort）"
	if !strings.Contains(configLayer.body(), want) {
		t.Fatalf("config 层白名单提示不对: %s", configLayer.body())
	}

	if got := harness.replayNamed("tasks/create").status(); got != http.StatusCreated {
		t.Fatalf("全部合法参数应当 201, got %d", got)
	}
}

// TestKeyCreateInheritsConfigRevisionField 锁定 KeyCreate 的两个不对称：
// 它因为继承 APIModel 而带 config_revision 字段（不会被当成额外字段拒绝），但
// POST /api/models/{id}/keys 会把它弹掉，而 POST /api/models 的 keys[] 不会。
func TestKeyCreateInheritsConfigRevisionField(t *testing.T) {
	harness := newHarness(t)
	if got := harness.replayNamed("models/create-key-revision-quirk").status(); got != http.StatusCreated {
		t.Fatalf("keys[] 里的 config_revision 应当被接受（201）, got %d", got)
	}
	if got := harness.replayNamed("model-keys/create").status(); got != http.StatusCreated {
		t.Fatalf("POST /api/models/{id}/keys 应当 201, got %d", got)
	}
	// 真正的额外字段仍然被拒绝——证明上面接受的是声明过的字段而不是「不校验」。
	if got := harness.replayNamed("models/create-extra-in-key").status(); got != http.StatusUnprocessableEntity {
		t.Fatalf("未声明字段应当 422, got %d", got)
	}
}

// TestProviderRouteOrderFollowsConfig 锁定路由表键序：Python 的 dict(provider.routes)
// 保留配置里的插入顺序，Go 的 map 丢序，因此 providerResponse 必须从原始配置补回顺序。
func TestProviderRouteOrderFollowsConfig(t *testing.T) {
	harness := newHarness(t)
	body := mustParseBody(t, harness.replayNamed("providers/get").body())
	routes := body.Lookup("provider").Lookup("routes")
	got := routes.Obj.Keys()
	want := []string{"responses", "openai"}
	if len(got) != len(want) {
		t.Fatalf("路由数量: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("路由键序: got %v want %v", got, want)
		}
	}
}

// TestUnhandledErrorsBecomePlainText500 锁定「参照实现没有 exception handler」的后果：
// ManagementAPIError 逃出路由函数后是 500 纯文本，而不是它的 status 字段。
func TestUnhandledErrorsBecomePlainText500(t *testing.T) {
	harness := newHarness(t)
	names := []string{
		"providers/get-missing",
		"provider-keys/get-missing",
		"routes/get-missing",
		"providers/update-id-null",
		"provider-keys/update-name-null",
		"routes/update-id-null",
		"probes/start-missing-provider",
		"provider-keys/get-models-missing-key",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			replay := harness.replayNamed(name)
			if got := replay.status(); got != http.StatusInternalServerError {
				t.Fatalf("状态码: got %d want 500", got)
			}
			if got := replay.recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Fatalf("content-type: %s", got)
			}
			if got := replay.body(); got != "Internal Server Error" {
				t.Fatalf("响应体: %s", got)
			}
		})
	}
}

// TestBareValueErrorIs500AtRouteLevelAnd400InsideUpdateConfig 锁定同一类裸错误的两种
// 归宿：_routes() 在路由函数里抛 ValueError 时无人接管（500），而同样的错误经由
// _update_config 时会被 `except ValueError` 捕获成 400「配置校验失败」。
//
// 这条差异是移植中最容易被「顺手修好」的地方，故单独命名。
func TestBareValueErrorIs500AtRouteLevelAnd400InsideUpdateConfig(t *testing.T) {
	harness := newHarness(t)

	atRouteLevel := harness.replayNamed("broken/models-not-object-get-routes")
	if got := atRouteLevel.status(); got != http.StatusInternalServerError {
		t.Fatalf("路由层裸 ValueError 应当 500, got %d (%s)", got, atRouteLevel.body())
	}
	if got := atRouteLevel.body(); got != "Internal Server Error" {
		t.Fatalf("响应体: %s", got)
	}

	insideUpdate := harness.replayNamed("broken/models-not-object-put-routes")
	if got := insideUpdate.status(); got != http.StatusBadRequest {
		t.Fatalf("_update_config 内的 ValueError 应当 400, got %d (%s)", got, insideUpdate.body())
	}
	if !strings.HasPrefix(insideUpdate.body(), `{"detail":"配置校验失败: `) {
		t.Fatalf("响应体: %s", insideUpdate.body())
	}
}

// TestEmptyUpdateChecksAreAsymmetric 锁定「至少要提供一个字段」检查的分布：
// 模型 key 的 PUT、模型 PUT、任务 PUT 有检查；供应商与供应商 key 的 PUT 没有。
func TestEmptyUpdateChecksAreAsymmetric(t *testing.T) {
	harness := newHarness(t)
	checked := map[string]int{
		"models/update-empty":     http.StatusBadRequest,
		"model-keys/update-empty": http.StatusBadRequest,
		"tasks/update-nothing":    http.StatusUnprocessableEntity,
	}
	for name, want := range checked {
		t.Run(name, func(t *testing.T) {
			replay := harness.replayNamed(name)
			if got := replay.status(); got != want {
				t.Fatalf("状态码: got %d want %d (%s)", got, want, replay.body())
			}
			if !strings.Contains(replay.body(), "至少需要提供一个要更新的字段") {
				t.Fatalf("响应体: %s", replay.body())
			}
		})
	}
	for _, name := range []string{"providers/update-no-fields", "provider-keys/update-empty-updates"} {
		t.Run(name, func(t *testing.T) {
			if got := harness.replayNamed(name).status(); got != http.StatusOK {
				t.Fatalf("这条路由没有空更新检查，应当是 200, got %d", got)
			}
		})
	}
}

// TestModelLookupIgnoresAliases 锁定 _find_model 只比 id：GET /api/models/{alias} 是
// 404，即使该别名确实配置了。
func TestModelLookupIgnoresAliases(t *testing.T) {
	harness := newHarness(t)
	replay := harness.replayNamed("models/get-by-alias")
	if got := replay.status(); got != http.StatusNotFound {
		t.Fatalf("状态码: got %d want 404", got)
	}
	if got := replay.body(); got != `{"detail":"模型不存在: alias-a"}` {
		t.Fatalf("响应体: %s", got)
	}
}

// TestStatsHoursQueryIsParsedLeniently 锁定 hours 查询参数的解析：缺省与非法值都得到
// null（**不是** 422），合法数字转成浮点。语料里的 metrics 桩把 hours 原样回显，因此
// 这三条能直接断言解析结果。
func TestStatsHoursQueryIsParsedLeniently(t *testing.T) {
	harness := newHarness(t)
	cases := map[string]*canonical.Value{
		"":           canonical.NewNull(),
		"?hours=24":  canonical.NewFloat(24),
		"?hours=abc": canonical.NewNull(),
		"?hours=1e3": canonical.NewFloat(1000),
	}
	for query, want := range cases {
		t.Run("hours="+query, func(t *testing.T) {
			replay := harness.replayRequest("GET", "/api/models/model-a/keys/key-a/stats"+query, "", "full")
			if got := replay.status(); got != http.StatusOK {
				t.Fatalf("状态码: got %d (%s)", got, replay.body())
			}
			body := mustParseBody(t, replay.body())
			got := body.Lookup("hours")
			if canonical.Dumps(got) != canonical.Dumps(want) {
				t.Fatalf("hours: got %s want %s", canonical.Dumps(got), canonical.Dumps(want))
			}
			if body.Lookup("config_revision") == nil {
				t.Error("响应缺少 config_revision")
			}
		})
	}
}

// TestRequestWithoutContentTypeStillParses 锁定 FastAPI 的行为：请求体的解析只看
// body，不看 Content-Type。
func TestRequestWithoutContentTypeStillParses(t *testing.T) {
	harness := newHarness(t)
	entry := corpusCase{
		Method:          "PUT",
		Path:            "/api/settings",
		Auth:            "full",
		bodyKind:        "json",
		bodyValue:       mustParseText(t, `{"config_revision":"$REV","port":9000}`),
		resolveRevision: true,
	}
	replay := harness.replayEntry(entry)
	if got := replay.status(); got != http.StatusOK {
		t.Fatalf("状态码: got %d (%s)", got, replay.body())
	}
}

// mustParseBody 解析响应体为 canonical 值。
func mustParseBody(t *testing.T, text string) *canonical.Value {
	t.Helper()
	value, err := canonical.ParseString(text)
	if err != nil {
		t.Fatalf("解析响应体失败: %v (%s)", err, text)
	}
	return value
}

// TestCorpusContentLengthIsSelfConsistent 检查语料自身的一致性：除了 204 与
// 「路径被替换」的用例，content-length 必须等于响应体字节数。
//
// 这条断言把「语料被手工改坏」挡在门口：如果只改 body_text 不改 content-length，
// Go 侧会因为响应头对不上而失败，但失败原因会指向 Go 实现。这里先自查语料。
func TestCorpusContentLengthIsSelfConsistent(t *testing.T) {
	corpus := loadCorpus(t)
	for _, entry := range corpus.cases {
		if entry.ContentLength == nil {
			if entry.BodyText != "" {
				t.Errorf("用例 %s 记录了响应体却没有 content-length", entry.Name)
			}
			continue
		}
		want := strconv.Itoa(len(entry.BodyText))
		if *entry.ContentLength != want {
			t.Errorf("用例 %s 的 content-length=%s 与响应体长度 %s 不符", entry.Name, *entry.ContentLength, want)
		}
	}
}
