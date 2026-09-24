package configeditor

// 面板内容的信息等价断言与「接缝/刻意差异」的具名测试。
//
// 版式（列宽、间距、边框、颜色）是本迁移明确不追求一致的部分，因此这里断言的是
// **信息**：面板渲染后抽出的「有内容的词」里应出现（或不应出现）哪些词。
// 抽词规则见 renderTokens。

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/Sparrived/auto-model-key-router/internal/api"
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// boxGlyphs 是渲染输出里的边框/分隔线字符：它们不是内容，抽词前先剔除。
var boxGlyphs = []string{
	"─", "━", "│", "┃", "╭", "╮", "╰", "╯", "├", "┤", "┬", "┴", "┼",
	"┏", "┓", "┗", "┛", "┣", "┫", "┳", "┻", "╋", "╸", "╺", "╹", "╻",
	"┄", "┅", "┆", "┇", "┈", "┉", "┊", "┋",
}

// renderTokens 把面板渲染成纯文本并抽出有内容的词。
func renderTokens(panel tui.Renderable) map[string]bool {
	text := strings.Join(tui.Console.RenderLines(panel, 200), "\n")
	for _, glyph := range boxGlyphs {
		text = strings.ReplaceAll(text, glyph, " ")
	}
	out := map[string]bool{}
	for _, piece := range strings.Fields(text) {
		if utf8.RuneCountInString(piece) < 2 {
			continue
		}
		hasContent := false
		for _, char := range piece {
			if unicode.IsLetter(char) || unicode.IsDigit(char) {
				hasContent = true
				break
			}
		}
		if hasContent {
			out[piece] = true
		}
	}
	return out
}

// TestProberSatisfiesAPISeams 用编译期赋值钉死三个探测方法与 api.Server 接缝的签名。
//
// 这不是形式主义：internal/api 的三条探测路由在生产里就靠这三个接缝工作，签名一旦漂移，
// 装配层要么编译不过（好事），要么在接缝为 nil 时静默返回 500。
func TestProberSatisfiesAPISeams(t *testing.T) {
	prober := Prober{}
	server := &api.Server{
		ProbeKeyCapability:           prober.ProbeKeyCapability,
		ProbeProviderKeyCapabilities: prober.ProbeProviderKeyCapabilities,
		ProbeKeyAvailability:         prober.ProbeKeyAvailability,
	}
	if server.ProbeKeyCapability == nil || server.ProbeProviderKeyCapabilities == nil ||
		server.ProbeKeyAvailability == nil {
		t.Fatal("探测接缝不应为 nil")
	}
}

// TestProbeKeyAvailabilitySeamCallShape 复刻 api.runKeyProbe 的调用形状：
// routes 传的是 **upstream_routes 的内容**（{base_url: {mode: path}}），而不是整份 data。
//
// internal/api/probes.go:180-182 就是这么构造的；这个测试保证包装层不会把语义搞反。
func TestProbeKeyAvailabilitySeamCallShape(t *testing.T) {
	routes := canonical.NewObjectOf(canonical.ObjectPair{
		Key: "https://vendor.example.test",
		Value: canonical.NewObjectOf(canonical.ObjectPair{
			Key: "responses", Value: canonical.NewString("custom/responses"),
		}),
	})
	key := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "name", Value: canonical.NewString("main")},
		canonical.ObjectPair{Key: "api_key", Value: canonical.NewString("sk-main")},
		canonical.ObjectPair{Key: "base_url", Value: canonical.NewString("https://vendor.example.test")},
	)
	step := canonicalFromRaw(`{"status": 200, "body": "{}"}`)
	transport := &scriptedTransport{t: t, steps: []*canonical.Value{step, step, step}}
	prober := Prober{Client: &http.Client{Transport: transport}, Clock: newScriptedClock()}
	results, err := prober.ProbeKeyAvailability(context.Background(), routes, "model-a", key, 15.0)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("结果条数 = %d, 期望 3", len(results))
	}
	want := []string{
		"https://vendor.example.test/v1/chat/completions",
		"https://vendor.example.test/v1/messages",
		"https://vendor.example.test/custom/responses",
	}
	for index, result := range results {
		if result.URL != want[index] {
			t.Errorf("第 %d 条 URL = %q, 期望 %q", index, result.URL, want[index])
		}
		if !result.Available {
			t.Errorf("第 %d 条应可用", index)
		}
	}
}

// canonicalFromRaw 解析一段 JSON 文本；测试数据写错时直接失败。
func canonicalFromRaw(raw string) *canonical.Value {
	value, err := canonical.ParseString(raw)
	if err != nil {
		panic(err)
	}
	return value
}

// TestRestartSeamUnsetFailsLoudly 钉住「未接线时响亮失败」：接缝为 nil 时改动会返回
// 错误，而不是伪造一个「配置已热重载」的成功面板。
func TestRestartSeamUnsetFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	writeConfig(t, path, `{"config_version": 4, "providers": {"p": {"base_url": "https://x", "keys": {}}}, "models": {}}`)
	editor := New(path)
	editor.UI = UI{ConfirmChoice: func(string, bool) bool { return true }}
	_, err := editor.DeleteProviderInteractively("p")
	if err == nil {
		t.Fatal("接缝未接线时应当返回错误")
	}
	if !strings.Contains(err.Error(), "未接入") {
		t.Fatalf("错误文本应说明未接入，得到 %q", err.Error())
	}
}

// writeConfig 写一份初始配置（按插入顺序，避免 providers 顺序漂移）。
func writeConfig(t *testing.T, path, raw string) {
	t.Helper()
	value := canonicalFromRaw(raw)
	if err := os.WriteFile(path, []byte(canonical.DumpsOrdered(value)), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
}

// --------------------------------------------------------------------------- #
// 刻意差异（具名测试）
// --------------------------------------------------------------------------- #

// TestVisitorFeatureIsGone 记录 Go 侧已**整体删除**的访客模式。
//
// 参照实现用「能否 import itsdangerous」当访客功能的运行期标记（visitor.py:9-17），
// 并据此在 v2 汇总面板上挂一列「访客」、在供应商 Key 菜单里挂一项「访客访问」。这条
// 能力已被访问密钥取代（见 internal/config 的 AccessKeyConfig），因此：
//
//   - 汇总面板不再有「访客」列；
//   - visitorAvailable 与 FormatVisitorStatusText 都已删除，不存在「未安装」分支。
//
// 面板列数是这条删除最容易悄悄回流的地方（列数与分隔线必须对齐），因此显式钉住。
func TestVisitorFeatureIsGone(t *testing.T) {
	data := canonicalFromRaw(`{
		"config_version": 4,
		"providers": {"gateway": {"base_url": "https://gateway.example.test",
			"keys": {"main": {"api_key": "sk-main"}}}},
		"models": {}
	}`)
	editor := New("unused")
	panel, err := editor.V2SummaryPanel(data)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	tokens := renderTokens(panel)
	if tokens["访客"] || tokens["允许"] {
		t.Fatalf("访客列应已删除，实际 tokens=%v", tokens)
	}
	// 供应商表格的四列仍应齐全（删列时最容易顺手删错）。renderTokens 按空白切词，
	// 因此 "Base URL" 这种带空格的列名要按切出来的两个词分别断言。
	for _, column := range []string{"供应商", "Base", "URL", "Keys", "路由"} {
		if !tokens[column] {
			t.Errorf("供应商表格缺少 %q 列: %v", column, tokens)
		}
	}
}

// TestUpstreamRoutesToleratesShapesPythonCrashesOn 记录第二处有意差异：参照实现对
// 输入形状不对的 data 会抛 TypeError/AttributeError（`for model in None`、
// `"s".get(...)`），Go 侧按「没有遗留路由」处理。
func TestUpstreamRoutesToleratesShapesPythonCrashesOn(t *testing.T) {
	cases := []string{
		`{"models": null}`,
		`{"models": "abc"}`,
		`{"models": ["s"]}`,
		`{"models": [{"keys": "abc"}]}`,
		`{"models": [{"keys": ["s"]}]}`,
		`{"models": [{"keys": [null]}]}`,
	}
	for _, raw := range cases {
		routes, err := UpstreamRoutesForBaseURL(canonicalFromRaw(raw), "https://vendor.example.test")
		if err != nil {
			t.Errorf("%s 不应失败: %v", raw, err)
			continue
		}
		if routes.Obj.Len() != 0 {
			t.Errorf("%s 应得到空路由，得到 %s", raw, canonical.DumpsOrdered(routes))
		}
	}
}

// TestNonStringRouteValuesAreCoerced 记录第三处有意差异：`routes.get(mode)` 的值不是
// 字符串时，参照实现会在 `_join_url` 里对 int 调 .lstrip 而崩溃；Go 侧用 PyStr 折算。
func TestNonStringRouteValuesAreCoerced(t *testing.T) {
	routes := canonicalFromRaw(`{"openai": 42, "anthropic": false, "responses": ""}`)
	if got := upstreamRoutePath(routes, "openai"); got != "42" {
		t.Errorf("openai 路径 = %q, 期望 42", got)
	}
	// false 是假值：Python 的 `or` 会回落到默认路径。
	if got := upstreamRoutePath(routes, "anthropic"); got != config.UpstreamRouteDefaultPath("anthropic") {
		t.Errorf("anthropic 路径 = %q, 期望默认路径", got)
	}
	if got := upstreamRoutePath(routes, "responses"); got != config.UpstreamRouteDefaultPath("responses") {
		t.Errorf("responses 路径 = %q, 期望默认路径", got)
	}
}

// TestPythonFloatFormattingMatchesReference 锁定两处数字格式化（值与 Python 实测一致）：
//
//	str(float)            -> pythonFloatStr（超时默认值）
//	format(value, "g")    -> formatPythonG（超时结果页）
func TestPythonFloatFormattingMatchesReference(t *testing.T) {
	reprCases := map[float64]string{
		60:      "60.0",
		0:       "0.0",
		185.5:   "185.5",
		0.1:     "0.1",
		1e-5:    "1e-05",
		1234567: "1234567.0",
		1e15:    "1000000000000000.0",
		1e16:    "1e+16",
		-3.25:   "-3.25",
	}
	for value, want := range reprCases {
		if got := pythonFloatStr(value); got != want {
			t.Errorf("pythonFloatStr(%v) = %q, 期望 %q", value, got, want)
		}
	}
	gCases := map[float64]string{
		60:      "60",
		185.5:   "185.5",
		0.1:     "0.1",
		1e-5:    "1e-05",
		1234567: "1.23457e+06",
		0.0001:  "0.0001",
		1e15:    "1e+15",
	}
	for value, want := range gCases {
		if got := formatPythonG(value); got != want {
			t.Errorf("formatPythonG(%v) = %q, 期望 %q", value, got, want)
		}
	}
}
