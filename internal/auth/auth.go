// Package auth 实现 AMKR 的入站凭据判定：本地 API key 给完整权限。
//
// 原先这里还有第二档「访客」模式（固定 key `amkr-visitor` + 上游 key 上的
// allow_visitor 开关）。它已被**访问密钥**取代：那是一个独立的配置资源，每把 key
// 自带供应商与模型清单（见 config.AccessKeyConfig）。取代的理由是固定访客 key 的
// 两处硬伤——所有人共用一把 key，且权限只能靠逐个改上游 key 的开关来拼，既无法
// 一人一把，也无法按人收窄。
//
// 访问密钥的解析**刻意不在本包**：本包是被对拍语料锁定的纯函数，而访问密钥要读配置
// （密钥清单、供应商清单）并与 keypool 的选择逻辑协同。它落在 proxy 与 app 面的
// authorize 里，与工作空间推理 key 同一处。
//
// 本包不依赖任何 HTTP 框架，只吃 http.Header：判定逻辑是纯函数，便于逐条对拍，
// 也避免把鉴权绑死在某个路由库上。
package auth

import (
	"crypto/hmac"
	"net/http"
	"strings"
)

// Mode 是鉴权结果的权限范围。
//
// 只保留 full 一档：受限凭据（访问密钥、工作空间推理 key）的权限不是「更小的
// full」，而是由各自的清单在**代理层**逐项判定，无法用一个枚举表达。把它们塞进
// 这里只会让本包依赖配置。
type Mode string

const (
	// ModeFull 表示完整权限（本地 API key）。
	ModeFull Mode = "full"
)

// Context 是一次成功的鉴权结果。
//
// 只带 mode：调用方身份留给宿主自己（宿主手上就有请求对象，放自己的上下文即可），
// 框架不代管。
type Context struct {
	Mode Mode
}

// IsFull 报告是否为完整权限。
func (c Context) IsFull() bool { return c.Mode == ModeFull }

// Authorizer 把请求折算成鉴权结果，返回 nil 表示未通过。
//
// 宿主可整体替换（对应 Python 的 create_app(authenticator=...)）：宿主通常已有自己的
// 身份体系（session cookie、JWT、网关注入的身份头），此前的两个选择都不好——把
// local_api_key 留空等于**整体关闭鉴权**，或者让调用方再拿一套 AMKR 的 key。
type Authorizer func(r *http.Request, localAPIKey string) *Context

// RequestAPIKey 取出请求携带的凭据。
//
// 支持两种形态：`Authorization: Bearer <key>` 与 `x-api-key: <key>`。
// 若 Authorization 不是 Bearer 形态，则回落到 x-api-key（与参照实现一致）。
func RequestAPIKey(headers http.Header) string {
	authorization := headers.Get("authorization")
	if len(authorization) >= 7 && strings.EqualFold(authorization[:7], "bearer ") {
		return strings.TrimSpace(authorization[7:])
	}
	return headers.Get("x-api-key")
}

// ModeFromAPIKey 把凭据折算成权限模式，返回 nil 表示拒绝。所有实现共用这套判定。
//
// localAPIKey 为空表示鉴权整体关闭，一律按完整权限处理（既有语义，不可改成拒绝：
// 那会让所有本地开发用默认配置直接不可用）。
//
// 比较必须走 hmac.Equal 作用于 **UTF-8 字节**，不能用 == 比字符串。参照实现用
// hmac.compare_digest 并显式 encode("utf-8")，原因写在 auth.py:67：HTTP 头由
// Starlette 按 latin-1 解码，攻击者塞一个非 ASCII 凭据，Python 侧 compare_digest
// 会对 str 抛 TypeError 变成 500（而非 401）。Go 的 hmac.Equal 只接受 []byte，
// 天然规避；但**依然要保持字节比较**，因为那是恒定时间比较，字符串 == 会在第一个
// 不同字节处短路，泄漏前缀匹配长度。
func ModeFromAPIKey(apiKey, localAPIKey string) *Mode {
	if localAPIKey == "" {
		mode := ModeFull
		return &mode
	}
	if apiKey != "" && hmac.Equal([]byte(apiKey), []byte(localAPIKey)) {
		mode := ModeFull
		return &mode
	}
	return nil
}

// DefaultAuthorizer 是默认判定：只有本地 API key 通过，给完整权限。
//
// 受限凭据（访问密钥、工作空间推理 key）不在这里判定：它们要读配置清单，而本包
// 刻意不依赖 config。调用方在本函数返回 nil 之后再去查那两条通道。
func DefaultAuthorizer(r *http.Request, localAPIKey string) *Context {
	mode := ModeFromAPIKey(RequestAPIKey(r.Header), localAPIKey)
	if mode == nil {
		return nil
	}
	return &Context{Mode: *mode}
}

// Authenticate 走可替换的 authorizer，为 nil 时用 DefaultAuthorizer。
//
// 对应 Python 的 authorize()：它读 request.app.state.authenticator，未配置时用默认
// 实现。Go 侧由调用方持有 authorizer（通常是服务器结构体上的一个字段），传入 nil
// 即表示使用默认实现——比查全局状态更显式，也避免并发读写。
func Authenticate(authorizer Authorizer, r *http.Request, localAPIKey string) *Context {
	if authorizer == nil {
		authorizer = DefaultAuthorizer
	}
	return authorizer(r, localAPIKey)
}

// WebsocketAuthRequest 把 WebSocket 首帧 token 折算成可供鉴权使用的请求。
//
// 移植 auth.py:100。WebSocket 握手无法带自定义头，所以 AMKR 的凭据只能走首帧；但
// 宿主的 cookie 是**在**握手头里的，因此这里保留原始握手头（宿主用 session 鉴权时
// 仍然有效），只额外补一个 Authorization，让宿主写一个 HTTP 钩子就同时覆盖两条路径。
//
// 返回的请求没有 body：鉴权只读 header。原实现同样给了一个立即结束的 body。
func WebsocketAuthRequest(handshake http.Header, token string) *http.Request {
	headers := handshake.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Authorization", "Bearer "+token)
	request, err := http.NewRequest(http.MethodGet, "http://amkr.local/", nil)
	if err != nil {
		// 常量 URL 不会解析失败；真发生了说明程序处于不可能状态。
		panic("auth: 构造内部请求失败: " + err.Error())
	}
	request.Header = headers
	return request
}
