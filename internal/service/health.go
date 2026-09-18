package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// 本文件移植 service.py:640-669 的健康检查（is_service_healthy / service_health）。
//
// 两个超时是参照实现里刻意不同的量：探测只要 0.1 秒（避免界面卡顿），取回详情给
// 0.5 秒。缓存 TTL 是 2 秒（service.py:38），键为 (host, port)。

// serviceStatusCacheTTL 对应 service.py:38 的 _SERVICE_STATUS_CACHE_TTL。
const serviceStatusCacheTTL = 2 * time.Second

// healthyTimeout / healthDetailTimeout 对应 service.py:650 的 timeout=0.1 与
// service.py:665 的 timeout=0.5。
const (
	healthyTimeout      = 100 * time.Millisecond
	healthDetailTimeout = 500 * time.Millisecond
)

// HealthInfo 是 /health 响应的字段子集。
//
// Python 侧 service_health 返回整个 dict，但本包的两个消费者（后台状态面板与
// config_renderables 的「服务」指示灯）只用到 config_path 与
// local_api_key_fingerprint，因此这里只解析这两个字段并保留原始字段表供扩展。
type HealthInfo struct {
	// ConfigPath 是运行中服务实际使用的配置文件（health["config_path"]）。
	ConfigPath string
	// LocalAPIKeyFingerprint 是运行中服务的本地 key 指纹。
	LocalAPIKeyFingerprint string
	// Fields 是响应的原始字段（非对象响应为空表）。
	Fields map[string]any
}

// defaultHTTPGet 是 HTTPGet 的真实实现：GET 请求 + 超时 + 读全响应体。
//
// 与 Python urlopen 的差异：Go 的 http.Client 会对 5xx 等照常返回响应（不抛异常），
// 因此状态码由调用方判断（`response.status == 200` 的语义得以保留）。
func defaultHTTPGet(url string, timeout time.Duration) (int, []byte, error) {
	client := &http.Client{Timeout: timeout}
	response, err := client.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, body, nil
}

// healthURL 拼出 /health 地址，并把通配监听地址换成回环地址（service.py:648）。
//
// 注意与 Python 一样**不**给 IPv6 字面量加方括号——参照实现就是这么写的，
// `--host ::1` 会拼出无法解析的 URL 并因此判定「不健康」，这是刻意保留的行为。
func healthURL(host string, port int) string {
	connectHost := host
	if connectHost == "0.0.0.0" || connectHost == "::" {
		connectHost = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d/health", connectHost, port)
}

// healthCacheKey 对应 Python 的 `(host, port)` 元组键。
func healthCacheKey(host string, port int) string {
	return host + "\x00" + strconv.Itoa(port)
}

// IsServiceHealthy 对应 service.py:640 的 is_service_healthy。
//
// useCache 为假时强制重新探测（Python 的面板刷新都用 False）。
func (e *Env) IsServiceHealthy(host string, port int, useCache bool) bool {
	key := healthCacheKey(host, port)
	now := e.Now()
	if useCache {
		e.cacheMu.Lock()
		entry, present := e.healthCache[key]
		e.cacheMu.Unlock()
		if present && now.Sub(entry.at) < serviceStatusCacheTTL {
			return entry.healthy
		}
	}

	healthy := false
	status, _, err := e.Get(healthURL(host, port), healthyTimeout)
	if err == nil && status == http.StatusOK {
		healthy = true
	}

	e.cacheMu.Lock()
	if e.healthCache == nil {
		e.healthCache = map[string]healthCacheEntry{}
	}
	e.healthCache[key] = healthCacheEntry{at: now, healthy: healthy}
	e.cacheMu.Unlock()
	return healthy
}

// ServiceHealth 对应 service.py:658 的 service_health：不健康时返回 ok=false。
//
// 响应不是 JSON 对象（含解析失败）时按 `{"status": "ok"}` 处理——参照实现在
// 取回失败与「不是对象」两种情况下都退化成这个最小结构。
func (e *Env) ServiceHealth(host string, port int, useCache bool) (HealthInfo, bool) {
	if !e.IsServiceHealthy(host, port, useCache) {
		return HealthInfo{}, false
	}
	_, body, err := e.Get(healthURL(host, port), healthDetailTimeout)
	if err != nil {
		return HealthInfo{Fields: map[string]any{"status": "ok"}}, true
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return HealthInfo{Fields: map[string]any{"status": "ok"}}, true
	}
	return HealthInfo{
		ConfigPath:             healthString(fields["config_path"]),
		LocalAPIKeyFingerprint: healthString(fields["local_api_key_fingerprint"]),
		Fields:                 fields,
	}, true
}

// healthString 复刻 `str(value or "")`。
//
// 覆盖面板真正会读的两个字段（字符串或缺失）；数字按 JSON 的 float64 打印成
// Python 风格（整数值不带小数点）。非标量（数组/对象）刻意退回 Go 语法：Python 的
// str(dict) 是单引号且带空格，逐字复刻没有价值，且这两个字段在参照实现里恒为字符串。
func healthString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}
