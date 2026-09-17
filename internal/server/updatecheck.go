package server

import (
	"github.com/Sparrived/auto-model-key-router/internal/api"
	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// defaultCheckUpdate 是装配到 api.Server.CheckUpdate 的真实实现。
//
// 对应 update.check_latest_version：先 PyPI、失败再 GitHub，会发网络请求。
func defaultCheckUpdate(version string) func(timeout float64) api.UpdateCheckResult {
	return newUpdateCheck(version, updatecheck.HTTPFetcher(nil))
}

// newUpdateCheck 构造版本检查接缝。fetch 做成参数，测试与语料回放因此不必联网。
//
// **这里修掉了一处已知缺陷。** api 的 handleCheckUpdate 直接信任调用方给的
// UpdateAvailable 字段（internal/api/handlers_meta.go 里那段
// `if !updateAvailable && ...` 是死代码，它把变量重新赋成 false），而参照实现的
// update_available 是 VersionCheckResult 从 latest/current 推导出来的属性
// （update.py 的 is_newer_version(latest, current)）。如果在这里漏填，接口会在
// 「确实有新版」时静默回答 update_available: false —— 运维据此判断「已经最新」，
// 正是最不该出的错。因此在适配器里显式推导，而不是照抄字段。
//
// 映射规则（api.UpdateCheckResult 的字段类型是 string，nil 表示「没有」，
// api 侧把空串渲染回 JSON null，因此往返无损）：
//
//	latest_version nil -> ""   -> 响应里的 null
//	error          nil -> ""   -> 响应里的 null
func newUpdateCheck(version string, fetch updatecheck.Fetcher) func(timeout float64) api.UpdateCheckResult {
	return func(timeout float64) api.UpdateCheckResult {
		result := updatecheck.CheckLatestVersion(fetch, version, secondsDuration(timeout))
		updateAvailable := false
		if result.LatestVersion != nil {
			// 与参照实现的 VersionCheckResult.update_available 属性逐字等价。
			updateAvailable = updatecheck.IsNewerVersion(*result.LatestVersion, result.CurrentVersion)
		}
		return api.UpdateCheckResult{
			CurrentVersion:  result.CurrentVersion,
			LatestVersion:   textOrEmpty(result.LatestVersion),
			ReleaseURL:      textOrEmpty(result.ReleaseURL),
			Source:          textOrEmpty(result.Source),
			ArtifactURL:     textOrEmpty(result.ArtifactURL),
			ArtifactSHA256:  textOrEmpty(result.ArtifactSHA256),
			UpdateAvailable: updateAvailable,
			Error:           textOrEmpty(result.Error),
		}
	}
}

// textOrEmpty 把可选字符串折叠成空串。
func textOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
