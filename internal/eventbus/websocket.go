package eventbus

import (
	"context"
	"errors"

	"github.com/coder/websocket"
)

// ErrBinaryFrame 表示首帧是二进制帧。
//
// 参照实现的 receive_text() 只取 ASGI 消息里的 "text" 键：二进制帧要么触发
// KeyError，要么给出 None（取决于 Starlette 版本），前者落进 `except Exception`
// 变 4001，后者被 json.loads(None) 抛 TypeError 也变 4001。两条路殊途同归，因此
// 这里直接返回错误，由 Authenticate 关 4001。
var ErrBinaryFrame = errors.New("eventbus: 需要文本帧")

// CoderConn 把 github.com/coder/websocket 的连接适配成 Conn。
//
// 适配层是刻意薄的一层：总线只依赖 Conn 的三个方法，因此换 WebSocket 库、或在测试
// 里用假连接驱动，都不需要改总线逻辑。
type CoderConn struct {
	conn *websocket.Conn
}

// NewCoderConn 包装一条 coder/websocket 连接。
func NewCoderConn(conn *websocket.Conn) *CoderConn { return &CoderConn{conn: conn} }

// ReadText 读取下一帧文本；二进制帧按「非法消息」处理。
//
// ctx 可以是普通 context，但**不要**传带 deadline 的 context：coder/websocket 在
// context 到期时会直接关闭底层连接（read.go 的 setupReadTimeout 注册的 AfterFunc），
// 于是 4001 的关闭码永远发不出去，客户端只会看到 1006。10 秒上限由 Authenticate
// 的 receiveFirstFrame 负责。
func (c *CoderConn) ReadText(ctx context.Context) (string, error) {
	messageType, data, err := c.conn.Read(ctx)
	if err != nil {
		return "", err
	}
	if messageType != websocket.MessageText {
		return "", ErrBinaryFrame
	}
	return string(data), nil
}

// SendText 发送一帧文本。
func (c *CoderConn) SendText(ctx context.Context, text string) error {
	return c.conn.Write(ctx, websocket.MessageText, []byte(text))
}

// Close 执行关闭握手。
//
// 与参照实现的分歧：coder/websocket 的 Close 是有握手的（写关闭帧，再等对端的
// 关闭帧，各 5 秒上限），而 starlette 的 close 只投递一条 ASGI 消息。关闭码与
// 关闭帧的送达不受影响；只是当对端既不应答也不断开时，本侧最多要等 5 秒。错误被
// 丢弃——参照实现也不检查 close 的返回值（websocket_proxy.py:45-48 只对
// RuntimeError 做了容错）。
func (c *CoderConn) Close(code int, reason string) {
	_ = c.conn.Close(websocket.StatusCode(code), reason)
}
