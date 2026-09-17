package updatecheck

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// payloadDesc 是语料里的注入载荷描述符。
//
//   - none: 抛异常（模拟网络故障）
//   - text: 原样返回该文本（非 JSON）
//   - json: 返回该 JSON
type payloadDesc struct {
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value"`
}

type versionRow struct {
	Version    string          `json:"version"`
	Numbers    json.RawMessage `json:"numbers"`
	Comparable json.RawMessage `json:"comparable"`
}

type comparisonRow struct {
	Latest  string          `json:"latest"`
	Current string          `json:"current"`
	Newer   json.RawMessage `json:"newer"`
}

type resultRow struct {
	CurrentVersion string  `json:"current_version"`
	LatestVersion  *string `json:"latest_version"`
	LatestTag      *string `json:"latest_tag"`
	ReleaseURL     *string `json:"release_url"`
	Source         *string `json:"source"`
	ArtifactURL    *string `json:"artifact_url"`
	ArtifactSHA256 *string `json:"artifact_sha256"`
	FallbackError  *string `json:"fallback_error"`
	Error          *string `json:"error"`
	UpdateAvail    bool    `json:"update_available"`
}

type httpRow struct {
	Name   string      `json:"name"`
	Route  string      `json:"route"`
	Pypi   payloadDesc `json:"pypi"`
	Github payloadDesc `json:"github"`
	// LanguageSpecificError 为真时，error/fallback_error 的**文本**来自 JSON 解析器，
	// Python 的 json.JSONDecodeError 与 Go 的 encoding/json 文案必然不同，故只断言
	// 「有错」而不比文本。
	LanguageSpecificError bool      `json:"language_specific_error"`
	Result                resultRow `json:"result"`
}

type corpusFile struct {
	Version         int               `json:"version"`
	Note            string            `json:"note"`
	SourceConstants map[string]string `json:"source_constants"`
	Versions        []versionRow      `json:"versions"`
	Comparisons     []comparisonRow   `json:"comparisons"`
	HTTP            []httpRow         `json:"http"`
}

// loadCorpus 读取语料。
func loadCorpus(t *testing.T) corpusFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "updatecheck_corpus.json"))
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var corpus corpusFile
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	if len(corpus.Versions) < 30 || len(corpus.HTTP) < 20 {
		t.Fatalf("语料不完整：versions=%d http=%d", len(corpus.Versions), len(corpus.HTTP))
	}
	return corpus
}

// compactJSON 把语料里的（带缩进的）JSON 压成紧凑形式，便于逐字节比较。
func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("解析 %s 失败: %v", raw, err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(out)
}

// marshalInts 把数字切片渲染成紧凑 JSON。
func marshalInts(value []int64) string {
	out, _ := json.Marshal(value)
	return string(out)
}

// TestVersionNumbersMatchesPython 逐条对齐 version_numbers 与 comparable_version。
func TestVersionNumbersMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	for _, row := range corpus.Versions {
		t.Run(row.Version, func(t *testing.T) {
			if got, want := marshalInts(VersionNumbers(row.Version)), compactJSON(t, row.Numbers); got != want {
				t.Errorf("VersionNumbers(%q) = %s，期望 %s", row.Version, got, want)
			}
			if got, want := marshalInts(ComparableVersion(row.Version)), compactJSON(t, row.Comparable); got != want {
				t.Errorf("ComparableVersion(%q) = %s，期望 %s", row.Version, got, want)
			}
		})
	}
}

// TestIsNewerVersionMatchesPython 逐条对齐元组比较。
func TestIsNewerVersionMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	for _, row := range corpus.Comparisons {
		var want bool
		if err := json.Unmarshal(row.Newer, &want); err != nil {
			t.Fatalf("比较结果不是布尔值（latest=%q current=%q）: %s",
				row.Latest, row.Current, row.Newer)
		}
		if got := IsNewerVersion(row.Latest, row.Current); got != want {
			t.Errorf("IsNewerVersion(%q, %q) = %v，期望 %v", row.Latest, row.Current, got, want)
		}
	}
}

// TestSourceConstantsMatchPython 锁定 URL/常量。
func TestSourceConstantsMatchPython(t *testing.T) {
	corpus := loadCorpus(t)
	want := map[string]string{
		"package_name":              PackageName,
		"pypi_project_url":          PyPIProjectURL,
		"pypi_json_api":             PyPIJSONAPI,
		"github_repository":         GitHubRepository,
		"github_releases_url":       GitHubReleasesURL,
		"github_latest_release_api": GitHubLatestReleaseAPI,
	}
	for key, value := range want {
		if corpus.SourceConstants[key] != value {
			t.Errorf("%s = %q，期望 %q", key, value, corpus.SourceConstants[key])
		}
	}
}

// corpusFetcher 依据载荷描述符构造注入的取回函数。
//
// 它刻意复刻 fetch_json 的语义：解析失败即错误、解析成功但非对象也错误
// （后者文案与 Python 一致，故可逐字比对）。
func corpusFetcher(t *testing.T, pypi, github payloadDesc, seenURLs *[]string, seenHeaders *[]map[string]string) Fetcher {
	t.Helper()
	return func(url string, headers map[string]string, timeout time.Duration) (*canonical.Value, error) {
		_ = timeout
		if seenURLs != nil {
			*seenURLs = append(*seenURLs, url)
		}
		if seenHeaders != nil {
			*seenHeaders = append(*seenHeaders, headers)
		}
		chosen := github
		if strings.Contains(url, "pypi.org") {
			chosen = pypi
		}
		switch chosen.Kind {
		case "none":
			// 文案与生成器注入的 OSError 一致，便于逐字比对。
			return nil, errors.New("模拟网络故障")
		case "text":
			var text string
			if err := json.Unmarshal(chosen.Value, &text); err != nil {
				t.Fatalf("载荷不是字符串: %s", chosen.Value)
			}
			parsed, err := canonical.ParseString(text)
			if err != nil {
				return nil, err
			}
			if !parsed.IsObject() {
				return nil, errNotJSONObject
			}
			return parsed, nil
		default:
			parsed, err := canonical.Parse(chosen.Value)
			if err != nil {
				return nil, err
			}
			if !parsed.IsObject() {
				return nil, errNotJSONObject
			}
			return parsed, nil
		}
	}
}

// assertResult 比较 Result 与语料期望值。
func assertResult(t *testing.T, got Result, want resultRow, languageSpecific bool) {
	t.Helper()
	if got.CurrentVersion != want.CurrentVersion {
		t.Errorf("current_version = %q，期望 %q", got.CurrentVersion, want.CurrentVersion)
	}
	comparePtr := func(label string, got, want *string) {
		switch {
		case got == nil && want == nil:
		case got == nil || want == nil:
			t.Errorf("%s = %v，期望 %v", label, renderPtr(got), renderPtr(want))
		case *got != *want:
			t.Errorf("%s = %q，期望 %q", label, *got, *want)
		}
	}
	comparePtr("latest_version", got.LatestVersion, want.LatestVersion)
	comparePtr("latest_tag", got.LatestTag, want.LatestTag)
	comparePtr("release_url", got.ReleaseURL, want.ReleaseURL)
	comparePtr("source", got.Source, want.Source)
	comparePtr("artifact_url", got.ArtifactURL, want.ArtifactURL)
	comparePtr("artifact_sha256", got.ArtifactSHA256, want.ArtifactSHA256)
	if languageSpecific {
		// 文本来自 JSON 解析器，语言相关：只断言有无。
		if (got.Error == nil) != (want.Error == nil) {
			t.Errorf("error 有无不一致: got=%v want=%v", renderPtr(got.Error), renderPtr(want.Error))
		}
		if (got.FallbackError == nil) != (want.FallbackError == nil) {
			t.Errorf("fallback_error 有无不一致: got=%v want=%v",
				renderPtr(got.FallbackError), renderPtr(want.FallbackError))
		}
	} else {
		comparePtr("error", got.Error, want.Error)
		comparePtr("fallback_error", got.FallbackError, want.FallbackError)
	}
	if got.UpdateAvailable() != want.UpdateAvail {
		t.Errorf("update_available = %v，期望 %v", got.UpdateAvailable(), want.UpdateAvail)
	}
}

// renderPtr 渲染可空字符串，便于失败信息阅读。
func renderPtr(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return "\"" + *value + "\""
}

// TestHTTPChecksMatchPython 重放全部 HTTP 分支。
func TestHTTPChecksMatchPython(t *testing.T) {
	corpus := loadCorpus(t)
	for _, row := range corpus.HTTP {
		t.Run(row.Name, func(t *testing.T) {
			fetch := corpusFetcher(t, row.Pypi, row.Github, nil, nil)
			var got Result
			switch row.Route {
			case "pypi":
				got = CheckLatestPyPI(fetch, "4.1.0", 3*time.Second)
			case "github":
				got = CheckLatestRelease(fetch, "4.1.0", 3*time.Second)
			default:
				got = CheckLatestVersion(fetch, "4.1.0", 3*time.Second)
			}
			assertResult(t, got, row.Result, row.LanguageSpecificError)
		})
	}
}

// TestRequestContractMatchesPython 锁定请求的 URL 与头部。
//
// 这两点不会体现在 Result 里，但属对外行为：URL 错了会打到别的服务，头部错了
// GitHub API 会拒绝（缺 Accept/User-Agent 时返回 403）。
func TestRequestContractMatchesPython(t *testing.T) {
	row := httpRow{
		Pypi:   payloadDesc{Kind: "json", Value: json.RawMessage(`{"info":{"version":"1.0.0"}}`)},
		Github: payloadDesc{Kind: "json", Value: json.RawMessage(`{"tag_name":"v1.0.0"}`)},
	}
	var urls []string
	var headers []map[string]string
	fetch := corpusFetcher(t, row.Pypi, row.Github, &urls, &headers)

	CheckLatestPyPI(fetch, "4.1.0", time.Second)
	if len(urls) != 1 || urls[0] != PyPIJSONAPI {
		t.Fatalf("PyPI URL = %v，期望 [%s]", urls, PyPIJSONAPI)
	}
	if headers[0]["Accept"] != "application/json" {
		t.Errorf("PyPI Accept = %q", headers[0]["Accept"])
	}
	if headers[0]["User-Agent"] != "auto-model-key-router/4.1.0" {
		t.Errorf("PyPI User-Agent = %q", headers[0]["User-Agent"])
	}

	urls, headers = nil, nil
	fetch = corpusFetcher(t, row.Pypi, row.Github, &urls, &headers)
	CheckLatestRelease(fetch, "4.1.0", time.Second)
	if len(urls) != 1 || urls[0] != GitHubLatestReleaseAPI {
		t.Fatalf("GitHub URL = %v，期望 [%s]", urls, GitHubLatestReleaseAPI)
	}
	if headers[0]["Accept"] != "application/vnd.github+json" {
		t.Errorf("GitHub Accept = %q", headers[0]["Accept"])
	}
	if headers[0]["User-Agent"] != "auto-model-key-router/4.1.0" {
		t.Errorf("GitHub User-Agent = %q", headers[0]["User-Agent"])
	}
}

// TestCheckLatestVersionCallsPyPIFirst 锁定「PyPI 成功就不再查 GitHub」。
//
// 用 github 载荷为 none（会报错）作为绊线：若先查了 GitHub，结果不可能仍是 PyPI。
func TestCheckLatestVersionCallsPyPIFirst(t *testing.T) {
	pypi := payloadDesc{Kind: "json", Value: json.RawMessage(`{"info":{"version":"5.0.0"}}`)}
	github := payloadDesc{Kind: "none"}
	var urls []string
	fetch := corpusFetcher(t, pypi, github, &urls, nil)
	got := CheckLatestVersion(fetch, "4.1.0", time.Second)
	if got.Source == nil || *got.Source != "PyPI" {
		t.Fatalf("应走 PyPI，实际 source=%v", renderPtr(got.Source))
	}
	if len(urls) != 1 {
		t.Fatalf("PyPI 成功时不应再查 GitHub，实际请求 %v", urls)
	}
}

// TestIsNewerVersionPrefixRule 锁定「一方是另一方前缀时短者更小」。
//
// Python 元组比较的关键性质：comparable_version 补齐到**至少**三段而非截断到三段，
// 所以 1.2.3 < 1.2.3.0 而 1.2.3 > 1.2.2。
func TestIsNewerVersionPrefixRule(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"1.2.3", "1.2.3", false},
		{"1.2.3.0", "1.2.3", true},
		{"1.2.3", "1.2.3.0", false},
		{"1.2.2", "1.2.3", false},
		{"1.2.4", "1.2.3", true},
		{"1.2", "1.2.0", false},
		{"1.2.0", "1.2", false},
		{"2", "1.99.99", true},
		{"1.0.0", "1", false},
		// build 元数据被切掉，不参与比较。
		{"1.2.3+build.9", "1.2.3", false},
		{"1.2.4+build.1", "1.2.3", true},
	}
	for _, item := range cases {
		if got := IsNewerVersion(item.latest, item.current); got != item.want {
			t.Errorf("IsNewerVersion(%q, %q) = %v，期望 %v", item.latest, item.current, got, item.want)
		}
	}
}

// TestUpdateAvailableNeedsLatest 锁定「没有 latest_version 时 update_available 为假」。
func TestUpdateAvailableNeedsLatest(t *testing.T) {
	none := Result{CurrentVersion: "4.1.0"}
	if none.UpdateAvailable() {
		t.Fatal("latest_version 为 nil 时不应报告有更新")
	}
	same := Result{CurrentVersion: "4.1.0", LatestVersion: stringPtr("4.1.0")}
	if same.UpdateAvailable() {
		t.Fatal("同版本不应报告有更新")
	}
	newer := Result{CurrentVersion: "4.1.0", LatestVersion: stringPtr("4.2.0")}
	if !newer.UpdateAvailable() {
		t.Fatal("更高版本应报告有更新")
	}
}

// TestVersionNumbersUnicodeDigitsDiverge 记录一处**已知且有意保留**的差异。
//
// Python 3 的 `\d` 匹配 Unicode 十进制数字（`int("４５") == 45`），而 Go 正则的 `\d`
// 是 ASCII 限定的。版本号里出现全角或其它文种数字不是现实场景，故不收窄到复刻
// Python 的 Unicode 语义；本测试把这个差异钉住，避免日后被误当作 bug 修掉却不知
// 道会偏离哪一侧。
func TestVersionNumbersUnicodeDigitsDiverge(t *testing.T) {
	if got := VersionNumbers("４５"); len(got) != 0 {
		t.Fatalf("Go 侧对全角数字应不匹配（与 Python 有意不同），实际 %v", got)
	}
	// ASCII 数字仍须正常。
	if got := VersionNumbers("4.5"); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("ASCII 数字应正常解析，实际 %v", got)
	}
}

// TestGitHubSourceArchiveURL 锁定归档 URL 拼接。
func TestGitHubSourceArchiveURL(t *testing.T) {
	got := GitHubSourceArchiveURL("v5.0.0")
	want := "https://github.com/Sparrived/auto-model-key-router/archive/refs/tags/v5.0.0.zip"
	if got != want {
		t.Fatalf("GitHubSourceArchiveURL = %q，期望 %q", got, want)
	}
}
