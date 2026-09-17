//go:build linux

package tui

// Linux 的 termios ioctl 常量与系统调用封装。

import (
	"syscall"
)

// tcgetattr 对应 Python 的 termios.tcgetattr。
func tcgetattr(fd int) (*syscall.Termios, error) {
	var mode syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), ioctlPointer(&mode), 0, 0, 0); errno != 0 {
		return nil, errno
	}
	return &mode, nil
}

// tcsetattr 对应 Python 的 termios.tcsetattr（TCSADRAIN 语义）。
func tcsetattr(fd int, mode *syscall.Termios) error {
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), ioctlPointer(mode), 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
