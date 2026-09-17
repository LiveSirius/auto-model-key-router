//go:build !windows

package tui

// 本文件是 POSIX 终端模式的共享逻辑：对应 Python 的 tty.setcbreak / tty.setraw
// （tui.py:423、tui.py:576）。ioctl 常量各平台不同，见 termios_<goos>.go。

import (
	"os"
	"syscall"
	"unsafe"
)

// setCBreak 把终端切到 cbreak：保留信号（ISIG），关闭回显与行缓冲。
func setCBreak() func() {
	fd := int(os.Stdin.Fd())
	original, err := tcgetattr(fd)
	if err != nil {
		return func() {}
	}
	mode := *original
	mode.Lflag &^= syscall.ECHO | syscall.ICANON
	if tcsetattr(fd, &mode) != nil {
		return func() {}
	}
	return func() { _ = tcsetattr(fd, original) }
}

// setRawMode 把终端切到 raw，等价于 tty.setraw：关闭回显、行缓冲、信号与
// 输入输出后处理，VMIN=1/VTIME=0。
func setRawMode() func() {
	fd := int(os.Stdin.Fd())
	original, err := tcgetattr(fd)
	if err != nil {
		return func() {}
	}
	mode := *original
	mode.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	mode.Oflag &^= syscall.OPOST
	mode.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	mode.Cflag &^= syscall.CSIZE | syscall.PARENB
	mode.Cflag |= syscall.CS8
	mode.Cc[syscall.VMIN] = 1
	mode.Cc[syscall.VTIME] = 0
	if tcsetattr(fd, &mode) != nil {
		return func() {}
	}
	return func() { _ = tcsetattr(fd, original) }
}

// ioctlPointer 把 Termios 转成 ioctl 需要的不安全指针。
func ioctlPointer(mode *syscall.Termios) uintptr {
	return uintptr(unsafe.Pointer(mode))
}
