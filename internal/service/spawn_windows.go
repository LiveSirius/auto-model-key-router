//go:build windows

package service

import "syscall"

// Windows 进程创建标志，取值与 CPython 的 subprocess 常量一致
// （subprocess.DETACHED_PROCESS = 0x8、CREATE_NEW_PROCESS_GROUP = 0x200、
// CREATE_NO_WINDOW = 0x8000000）。service.py:72-76 三者取或。
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// detachedProcessFlags 是后台服务启动时传给 CreateProcess 的标志组合。
//
// 单独抽成常量是为了让「与 Python 同值」这件事可以被具名测试锁定：语料里记录了
// Python 侧 `subprocess.DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP |
// CREATE_NO_WINDOW` 的实际数值 0x08000208。
const detachedProcessFlags = detachedProcess | createNewProcessGroup | createNoWindow

// sysProcAttr 在 Windows 上把 Detached 翻译成 CreationFlags。
func sysProcAttr(detached bool) *syscall.SysProcAttr {
	if !detached {
		return nil
	}
	return &syscall.SysProcAttr{CreationFlags: detachedProcessFlags}
}
