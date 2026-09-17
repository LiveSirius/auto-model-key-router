//go:build !windows

package tui

// POSIX 原生按键支持。
//
// 对应 tui.py:333-364（os.read + select）与 tui.py:521-582（termios/tty）。
//
// 差异说明（刻意，且只影响这一层原生接线，不影响按键解析）：Python 用
// `select.select([fd], [], [], timeout)` 轮询，而 syscall.Select 在 Linux 与
// Darwin/BSD 上的签名、FdSet 结构都不同；为免写三份平台代码，Go 侧改用
// O_NONBLOCK 轮询 fd 0（语义等价：有数据才算可读，超时返回 false）。
// PosixInputMode 的恢复函数会把 fd 0 还原成阻塞模式，避免影响后续的
// Bubble Tea 读取。

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// posixByteReader 是 POSIX 上的字节输入源。
type posixByteReader struct {
	pending []byte
	// nonblock 记录是否已经把 fd 0 设成非阻塞。
	nonblock bool
}

// ensureNonblock 把 fd 0 设为非阻塞，只做一次。
func (r *posixByteReader) ensureNonblock() {
	if r.nonblock {
		return
	}
	if err := syscall.SetNonblock(0, true); err == nil {
		r.nonblock = true
	}
}

// wouldBlock 判断读取错误是否只是「暂时没有数据」。
func wouldBlock(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR)
}

// Readable 轮询 fd 0，等价于 Python 的 select.select([fd], [], [], timeout)。
func (r *posixByteReader) Readable(timeout time.Duration) bool {
	if len(r.pending) > 0 {
		return true
	}
	r.ensureNonblock()
	deadline := time.Now().Add(timeout)
	for {
		var buffer [1]byte
		count, err := os.Stdin.Read(buffer[:])
		if count > 0 {
			r.pending = append(r.pending, buffer[0])
			return true
		}
		if err != nil && !wouldBlock(err) {
			return false
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// NextByte 读一个字节：已有缓冲先出缓冲，否则自旋等待。
//
// 对应 Python 的 `os.read(fd, 1)`；EOF 返回 ok=false（Python 返回 b""）。
func (r *posixByteReader) NextByte() (byte, bool) {
	if len(r.pending) > 0 {
		value := r.pending[0]
		r.pending = r.pending[1:]
		return value, true
	}
	r.ensureNonblock()
	for {
		var buffer [1]byte
		count, err := os.Stdin.Read(buffer[:])
		if count > 0 {
			return buffer[0], true
		}
		if err != nil && !wouldBlock(err) {
			return 0, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// readKeyFromNative 用 POSIX 终端读一次按键（tui.py:567-581）。
//
// 未处于 posix_input_mode 时临时切到原始模式再恢复，与 Python 的
// `tty.setraw` 行为一致。
func readKeyFromNative() string {
	reader := posixByteReader{}
	if posixInputModeActive {
		return ReadKeyFromByteReader(&reader)
	}
	restore := setRawMode()
	defer restore()
	return ReadKeyFromByteReader(&reader)
}

// keyPressedNative 非阻塞探测按键（tui.py:432-437）。
func keyPressedNative() (string, bool) {
	reader := posixByteReader{}
	if !reader.Readable(0) {
		return "", false
	}
	return ReadKeyFromByteReader(&reader), true
}

// setBlockingInput 把 fd 0 还原成阻塞模式（PosixInputMode 退出时调用）。
func setBlockingInput() {
	_ = syscall.SetNonblock(0, false)
}

// EnableWindowsVirtualTerminalInput 在非 Windows 上是 no-op，与 Python 的
// `return lambda: None`（tui.py:368-369）一致。
func EnableWindowsVirtualTerminalInput() func() { return func() {} }
