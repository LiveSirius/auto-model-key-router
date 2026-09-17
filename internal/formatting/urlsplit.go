package formatting

import (
	"net"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 本文件是 CPython urllib.parse.urlsplit / urlparse 的最小移植，只保留
// compact_url 用得到的 netloc 与 path。
//
// 为什么不用 net/url：两者在太多地方不一样，而且差异都会改变显示结果——
//   - Python 先把 URL 开头的 C0 控制字符与空格 lstrip 掉、把 \t\r\n 整串删掉，
//     net/url 则直接报错；
//   - Python 不做百分号解码（path 保持原文），net/url 的 Path 是解码后的；
//   - Python 对 netloc 做 NFKC 安全检查、对 IPv6 括号做 ipaddress 校验，
//     net/url 的规则与报错条件都不同；
//   - urlparse 还会按 uses_params 协议切掉 path 里的 ";params"。
//
// 这些差异无法用 net/url 的参数修正，所以按 Python 的算法逐段切分。

// c0ControlOrSpace 是 urllib.parse._WHATWG_C0_CONTROL_OR_SPACE：
// U+0000..U+0020，只用于 lstrip（Python 刻意保留尾随空格）。
const c0ControlOrSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f" +
	"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

// unsafeURLBytes 是 urllib.parse._UNSAFE_URL_BYTES_TO_REMOVE，整串删除（不只是首尾）。
const unsafeURLBytes = "\t\r\n"

// schemeChars 是 urllib.parse.scheme_chars，首字符另需是 ASCII 字母。
const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// usesParams 是 urllib.parse.uses_params：这些协议（含空协议）的 path 里
// 最后一个 '/' 之后的 ';' 会被 urlparse 当成 params 切掉。
var usesParams = map[string]bool{
	"": true, "ftp": true, "hdl": true, "http": true, "https": true,
	"imap": true, "mms": true, "prospero": true, "rtsp": true, "rtsps": true,
	"rtspu": true, "sftp": true, "shttp": true, "sip": true, "sips": true,
	"tel": true,
}

// ipvFuturePattern 对应 ipaddress._check_bracketed_host 里
// `re.match(r"\Av[a-fA-F0-9]+\..+\Z", hostname)`。
var ipvFuturePattern = regexp.MustCompile(`\Av[0-9a-fA-F]+\..+\z`)

// parsedURL 是 urlsplit 结果里 compact_url 需要的部分。
type parsedURL struct {
	scheme string
	netloc string
	path   string
}

// parseURL 复刻 urlparse 的切分；ok=false 对应 Python 抛 ValueError。
//
// urlparse 与 urlsplit 的区别只在 params（由 splitParams 在调用点处理），
// 因此这里把 scheme 一并返回。
func parseURL(raw string) (parsedURL, bool) {
	url := strings.TrimLeft(raw, c0ControlOrSpace)
	for index := 0; index < len(unsafeURLBytes); index++ {
		url = strings.ReplaceAll(url, unsafeURLBytes[index:index+1], "")
	}

	var parsed parsedURL
	// scheme 判定：冒号前必须是「ASCII 字母开头 + scheme_chars」。
	if index := strings.IndexByte(url, ':'); index > 0 &&
		isASCIILetter(url[0]) && allSchemeChars(url[:index]) {
		parsed.scheme = strings.ToLower(url[:index])
		url = url[index+1:]
	}

	if strings.HasPrefix(url, "//") {
		netloc, rest := splitNetloc(url, 2)
		parsed.netloc = netloc
		url = rest
		// 括号必须成对，且成对时按 IP 字面量校验。
		if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
			return parsedURL{}, false
		}
		if strings.Contains(netloc, "[") && !validBracketedNetloc(netloc) {
			return parsedURL{}, false
		}
	}
	// 先切 fragment 再切 query：两者都在 path 之后，顺序影响 "a#b?c" 的归属。
	if index := strings.IndexByte(url, '#'); index >= 0 {
		url = url[:index]
	}
	if index := strings.IndexByte(url, '?'); index >= 0 {
		url = url[:index]
	}
	if !netlocSafe(parsed.netloc) {
		return parsedURL{}, false
	}
	parsed.path = url
	return parsed, true
}

// isASCIILetter 判断字节是否是 ASCII 字母（Python 的 `url[0].isascii() and isalpha()`）。
func isASCIILetter(char byte) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}

// allSchemeChars 判断 s 的每个字节是否都属于 scheme_chars。
func allSchemeChars(s string) bool {
	for index := 0; index < len(s); index++ {
		if strings.IndexByte(schemeChars, s[index]) < 0 {
			return false
		}
	}
	return true
}

// splitNetloc 复刻 _splitnetloc：从 start 起找第一个 '/'、'?'、'#' 作为 netloc 的终点。
func splitNetloc(url string, start int) (netloc, rest string) {
	delim := len(url)
	for _, candidate := range []byte{'/', '?', '#'} {
		if index := strings.IndexByte(url[start:], candidate); index >= 0 && start+index < delim {
			delim = start + index
		}
	}
	return url[start:delim], url[delim:]
}

// validBracketedNetloc 复刻 _check_bracketed_netloc：校验方括号里的主机部分。
func validBracketedNetloc(netloc string) bool {
	hostPort := netloc
	if index := strings.LastIndexByte(netloc, '@'); index >= 0 {
		hostPort = netloc[index+1:]
	}
	before, bracketed, hasOpen := strings.Cut(hostPort, "[")
	if hasOpen {
		// 方括号前不允许有任何内容（用户信息已在上面剥掉）。
		if before != "" {
			return false
		}
		hostname, port, _ := strings.Cut(bracketed, "]")
		// 右括号之后只能直接跟端口（以 ':' 开头）。
		if port != "" && !strings.HasPrefix(port, ":") {
			return false
		}
		return validBracketedHost(hostname)
	}
	hostname, _, _ := strings.Cut(hostPort, ":")
	return validBracketedHost(hostname)
}

// validBracketedHost 复刻 ipaddress._check_bracketed_host。
//
// Python 用 ipaddress.ip_address 判定：IPvFuture（v<hex>.<rest>）放行，
// 其余必须是合法 IPv6；IPv4 不允许写在方括号里。
func validBracketedHost(hostname string) bool {
	if strings.HasPrefix(hostname, "v") {
		return ipvFuturePattern.MatchString(hostname)
	}
	address := hostname
	// Python 3.9+ 的 IPv6Address 允许 scope id（fe80::1%eth0）：非空且不含第二个 '%'。
	if index := strings.IndexByte(hostname, '%'); index >= 0 {
		scope := hostname[index+1:]
		if scope == "" || strings.Contains(scope, "%") {
			return false
		}
		address = hostname[:index]
	}
	// 没有冒号意味着一律不合法：要么是 IPv4（被 ipaddress 明确拒绝），
	// 要么根本不是合法地址，两者在 Python 里都抛 ValueError。
	if !strings.Contains(address, ":") {
		return false
	}
	return net.ParseIP(address) != nil
}

// nfkcUnsafeRunes 是「NFKC 归一化后会引入 / ? # @ :」的码点集合。
//
// 它对应 CPython urllib.parse._checknetloc：netloc 含非 ASCII 时必须做
// NFKC 归一化检查，否则像 U+2100 "℀"（展开成 "a/c"）这种域名会被 IDNA
// 拆出路径分隔符。Go 标准库没有 NFKC，而 x/text 不在本模块依赖里
// （go.mod 由别的改动持有，不能引入新依赖）。这里只保留该检查**可观察的效果**：
// 归一化不会凭空造出分隔符，且 /?#@: 在检查前已被剔除，所以
// 「NFKC(netloc) 里出现分隔符」等价于「netloc 里出现这些码点」。
//
// 表由 Python 对全码点跑 unicodedata.normalize("NFKC") 扫出（共 19 个），
// 语料里的 nfkc_unsafe_runes 锁定了这份结果，改错会被测试挡住。
var nfkcUnsafeRunes = map[rune]struct{}{
	'\u2047': {}, '\u2048': {}, '\u2049': {}, '\u2100': {}, '\u2101': {},
	'\u2105': {}, '\u2106': {}, '\u2a74': {}, '\ufe13': {}, '\ufe16': {},
	'\ufe55': {}, '\ufe56': {}, '\ufe5f': {}, '\ufe6b': {}, '\uff03': {},
	'\uff0f': {}, '\uff1a': {}, '\uff1f': {}, '\uff20': {},
}

// netlocSafe 复刻 _checknetloc：ASCII netloc 一律放行，非 ASCII 的先剔除
// 已允许出现（且 Python 会特意忽略）的 @ : # ?，再按 NFKC 危险码点判定。
func netlocSafe(netloc string) bool {
	if netloc == "" || isASCII(netloc) {
		return true
	}
	cleaned := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	for _, char := range cleaned {
		if _, unsafe := nfkcUnsafeRunes[char]; unsafe {
			return false
		}
	}
	return true
}

// isASCII 等价于 Python 的 str.isascii()。
func isASCII(s string) bool {
	for index := 0; index < len(s); index++ {
		if s[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// splitParams 复刻 _splitparams：返回 path 里 ';' 之前的部分（params 被丢掉）。
//
// 调用前已确认 path 含 ';'（urlparse 的条件是 `scheme in uses_params and ';' in url`），
// 但这里仍按 Python 的写法处理「最后一个 '/' 之后没有 ';'」的情形。
func splitParams(path string) string {
	slash := strings.LastIndexByte(path, '/')
	if slash >= 0 {
		semicolon := strings.IndexByte(path[slash:], ';')
		if semicolon < 0 {
			return path
		}
		return path[:slash+semicolon]
	}
	semicolon := strings.IndexByte(path, ';')
	if semicolon < 0 {
		return path
	}
	return path[:semicolon]
}
