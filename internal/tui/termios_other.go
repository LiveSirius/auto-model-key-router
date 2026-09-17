//go:build !windows && !linux && !darwin

package tui

// 其他类 Unix 平台的兜底：不实现 termios（保持终端默认模式），按键读取仍可用但
// 需要用户按回车。这是刻意的降级——迁移方案把交互界面的完整度排在共享助手之后。

import (
	"errors"
	"syscall"
)

// tcgetattr 在该平台未实现。
func tcgetattr(fd int) (*syscall.Termios, error) {
	return nil, errors.New("termios 未在该平台实现")
}

// tcsetattr 在该平台未实现。
func tcsetattr(fd int, mode *syscall.Termios) error {
	return errors.New("termios 未在该平台实现")
}
