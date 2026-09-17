package proxy

import (
	"net/url"
	"strings"
)

// reencodeQuery 复刻参照实现转发 query string 的方式。
//
// **这是一处与文档不符的实测发现。** 迁移方案 §4.5 写的是「query string 原样
// 转发」，`internal/upstream` 的 UpstreamURL 也按「绝不重编码」实现；但参照实现
// 把 Starlette 的 `request.query_params` 交给 httpx 的 `params=`
// （proxy_support.py:355），而 httpx 对 Mapping 走 `urlencode`，因此**确实会
// 重编码**。用真实 Python 实测（`httpx.AsyncClient` + `MockTransport`）：
//
//	'x=1&y=%20z'     -> 'x=1&y=+z'      （空格写成 +）
//	'a=1&b=2&a=3'    -> 'a=3&b=2'       （重复键：取**最后**的值、保留**首次**位置）
//	'k' / 'k='       -> 'k='            （裸键补 =）
//	''  / 'a=1&'     -> '' / 'a=1'      （空段丢弃）
//	'=x'             -> '=x'            （空键保留）
//	'a=%7E' / 'a=~'  -> 'a=~'           （~ 不被转义）
//	'a=%2A'          -> 'a=%2A'         （* 被转义）
//	'q=%E4%B8%AD'    -> 'q=%E4%B8%AD'   （非 ASCII 按 UTF-8 百分号编码）
//
// 换成 Go 的等价实现：`url.QueryUnescape` 对应 urllib 的 unquote_plus，
// `url.QueryEscape` 与 `quote_plus(safe="")` 在可打印 ASCII 上的取值集合**完全
// 相同**（已逐字符实测，含 `!()*'@,;$` 都会转义、`~` 不转义）。
//
// 为什么必须对齐：query 会原样出现在上游日志、签名校验与缓存键里。用户在
// `?a=1&b=2&a=3` 上看到的「最后一个值生效」是既有行为，Go 侧若改成原样透传，
// 同一份请求在两种实现下会命中不同的上游缓存键。
func reencodeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	// 保留**首次出现**的键顺序，值取最后一次出现——Python dict 的语义。
	var order []string
	values := map[string]string{}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		key := unquotePlus(rawKey)
		value := unquotePlus(rawValue)
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = value
	}
	var builder strings.Builder
	for index, key := range order {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(url.QueryEscape(key))
		builder.WriteByte('=')
		builder.WriteString(url.QueryEscape(values[key]))
	}
	return builder.String()
}

// unquotePlus 复刻 urllib 的 unquote_plus；非法转义序列按原样保留。
//
// 参照实现一侧的行为来自 `urllib.parse.parse_qsl(..., errors="replace")`（httpx
// 的默认值）：无法解析的 `%` 序列不报错，而是留在文本里。Go 的 QueryUnescape 会
// 对 `%zz` 返回错误，因此这里显式回退到原文。
func unquotePlus(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}
