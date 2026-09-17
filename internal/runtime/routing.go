package runtime

// RetryableStatusCodes 是需要重试的上游状态码。
//
// 注意 401 与 403 **在列**：参照实现把「key 鉴权失败」也当作可重试，因为轮换到
// 另一个 key 常常就能成功。这与直觉相反，但是既定契约（streaming.py:17）。
var RetryableStatusCodes = map[int]bool{
	401: true, 403: true, 429: true, 500: true,
	502: true, 503: true, 504: true, 521: true,
}

// IsRetryableStatus 报告状态码是否需要重试。
func IsRetryableStatus(statusCode int) bool { return RetryableStatusCodes[statusCode] }

// RetryPolicy 决定一次请求最多尝试几次。
//
// 移植 routing.py:7。三种情况下用 max_retries+1，否则用 key 数量（即只轮换一遍）：
//   - 调用方指定了 key：没有别的 key 可换，只能对同一个 key 重试；
//   - only_first 路由模式：同样固定单个 key；
//   - 只配置了一个 key：同上。
//
// 其余情况按 key 数走一轮，避免对同一 key 反复重试放大上游压力。
type RetryPolicy struct {
	MaxRetries int
}

// Attempts 返回最多尝试次数。
//
// requestedKeyName 用 *string：Python 判断的是真值（`if requested_key_name`），
// 因此空串与 nil 都走「按 key 数」分支，这里用 *string 表达「未指定」的意图更清楚。
func (p RetryPolicy) Attempts(keyCount int, requestedKeyName *string, onlyFirst bool) int {
	if (requestedKeyName != nil && *requestedKeyName != "") || onlyFirst || keyCount == 1 {
		return p.MaxRetries + 1
	}
	return keyCount
}
