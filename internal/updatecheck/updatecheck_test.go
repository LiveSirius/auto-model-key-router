package updatecheck

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// payloadDesc 是注入载荷的描述符。
//
//   - none: 抛异常（模拟网络故障）
//   - text: 原样返回该文本（非 JSON）
//   - json: 返回该 JSON
type payloadDesc struct {
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value"`
}

// stubFetcher 依据载荷描述符构造注入的取回函数。
//
// 它刻意复刻 fetch_json 的语义：解析失败即错误、解析成功但非对象也错误。
func stubFetcher(t *testing.T, github payloadDesc, seenURLs *[]string, seenHeaders *[]map[string]string) Fetcher {
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
		switch chosen.Kind {
		case "none":
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

// renderPtr 渲染可空字符串，便于失败信息阅读。
func renderPtr(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return "\"" + *value + "\""
}

// TestRequestContractMatchesPython 锁定请求的 URL 与头部。
//
// 这两点不会体现在 Result 里，但属对外行为：URL 错了会打到别的服务，头部错了
// GitHub API 会拒绝（缺 Accept/User-Agent 时返回 403）。
func TestRequestContractMatchesPython(t *testing.T) {
	payload := payloadDesc{Kind: "json", Value: json.RawMessage(`{"tag_name":"v1.0.0"}`)}
	var urls []string
	var headers []map[string]string
	fetch := stubFetcher(t, payload, &urls, &headers)

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

// TestCheckLatestVersionQueriesGitHubOnly 锁定「版本检查只查 GitHub」。
//
// 这是与参照实现**故意**不同的地方：Python 版先查 PyPI、失败才回退 GitHub；而 Python 退役后
// PyPI 上那个包冻结在最后一个 Python 版本，若沿用旧顺序，PyPI 会返回一个陈旧但**成功**的响应，
// 于是永远不回退 GitHub——用户跑着 Go 新版本却一直被告知"已是最新"。
//
// 断言两件事：只发出一次请求，且打的是 GitHub 的接口（绝不碰 pypi.org）。
func TestCheckLatestVersionQueriesGitHubOnly(t *testing.T) {
	github := payloadDesc{Kind: "json", Value: json.RawMessage(`{"tag_name":"v5.0.0"}`)}
	var urls []string
	fetch := stubFetcher(t, github, &urls, nil)
	got := CheckLatestVersion(fetch, "4.1.0", time.Second)
	if got.Source == nil || *got.Source != "GitHub" {
		t.Fatalf("应走 GitHub，实际 source=%v", renderPtr(got.Source))
	}
	if len(urls) != 1 || urls[0] != GitHubLatestReleaseAPI {
		t.Fatalf("应只请求 GitHub 接口一次，实际 %v", urls)
	}
	for _, url := range urls {
		if strings.Contains(url, "pypi.org") {
			t.Fatalf("不应再查询 PyPI，实际请求 %s", url)
		}
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
