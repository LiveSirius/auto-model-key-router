// Package wsproxy 实现 WS /v1/{path} 的「HTTP-over-WebSocket」垫片，移植
// auto_model_key_router/websocket_proxy.py。
//
// # 它是什么
//
// **不是** WebSocket 到 WebSocket 的桥（参照实现的路径也是 /v1/{path}，与 HTTP
// 代理面共用同一个上游）。协议是「一条连接、一条请求」：
//
//  1. 校验升级、接受连接（websocket_proxy.py:28）；
//  2. 读**一帧**（文本或二进制）当作请求体（websocket_proxy.py:30-35）；
//  3. 把 WS 握手折算成一条 POST 请求（websocket_proxy.py:37、:51-67）；
//  4. 交给注入的 ProxyHandler 执行**真正的上游 HTTP 请求**（websocket_proxy.py:38）；
//  5. 把响应体按块成帧发回（websocket_proxy.py:70-94）；
//  6. 按响应状态码关闭连接，然后结束（websocket_proxy.py:40）。
//
// 因此没有循环：**一条连接只服务一条请求**，响应发完即关闭。
//
// # 响应头不下发
//
// 参照实现只发 body 帧 + 关闭码，**从不**把响应头写进任何帧。这不是遗漏而是既有
// 契约（客户端只能从关闭码推断成败），移植时照抄。
//
// # 成帧规则（决定客户端看到文本帧还是二进制帧）
//
//	content_type.startswith("text/") or "json" in content_type
//
// 两个判断都**大小写敏感**（websocket_proxy.py:91）——`Content-Type:
// Application/JSON` 会走**二进制**帧。另有两条容易看漏的规则：
//
//   - 非流式响应体为空时**一帧都不发**（websocket_proxy.py:82 的 `if response.body:`）；
//     流式响应里的空块却会发一个**空帧**（websocket_proxy.py:75-76 无条件 send）。
//   - 文本帧前会做 `decode("utf-8", errors="replace")`（websocket_proxy.py:92），
//     即非法 UTF-8 字节按最大子部分替换成 U+FFFD 之后再编码回 UTF-8。
//
// # 与参照实现的分歧（每一条都有具名测试）
//
//   - **握手头的大小写**：Python 在字节层比较 `name.lower()` 并把存活头的**原始
//     大小写**一路带到上游；Go 的 http.Header 会把键规范化（`x-api-key` →
//     `X-Api-Key`）。因为代理侧的头部过滤规则本来就大小写不敏感，上游看到的
//     语义相同，但「两个仅大小写不同的同名头」在 Go 里会被合并成一个键（后者胜），
//     而 Python 会把它们当成两个键都发出去。
//   - **Host**：Go 把 Host 放在 Request.Host 而不是 Request.Header 里（服务端解析
//     时就从头部摘掉了）。因为 proxysupport.UpstreamHeaders 本来就剔除 host，
//     对上游不可观测。
//   - **超时与关闭握手**：coder/websocket 的 Close 有握手（等对端关闭帧，最长 5 秒），
//     starlette 的 close 只是投递 ASGI 消息。关闭码与关闭帧的送达不受影响。
//   - **Origin 校验**：FastAPI 默认不校验 Origin，coder/websocket 默认会校验，因此
//     Accept 必须显式 InsecureSkipVerify（见 Handler.ServeHTTP）。
//   - **panic**：参照实现的 `except Exception` 覆盖不到 BaseException；Go 侧多了
//     recover 兜底并关 1011（比让连接裸断更接近参照实现的意图）。
package wsproxy
