//go:build !windows

package service

import "syscall"

// posixRunning 对应 service.py:343-347 的 `os.kill(pid, 0)`：信号 0 只做存在性与
// 权限检查，不真的投递信号。
//
// 与 Python 的差异：Python 只捕 OSError，权限不足（EPERM）会被当成「不可杀」而抛异常；
// Go 的 syscall.Kill 同样返回 EPERM，这里按「进程存在」处理（Python 的 os.kill 在
// EPERM 时也抛 OSError → 参照实现会异常；Go 侧选择不崩，见 doc.go 的记录）。
func posixRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// posixTerminate 对应 service.py:137 的 `os.kill(pid, signal.SIGTERM)`。
func posixTerminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// posixCanTerminate 报告本进程是否有权终止 pid。
//
// `kill(pid, 0)` 在无权时返回 EPERM——与 Running 相反，这里 EPERM 表示「**不**可终止」
// （进程在跑，但不是我杀得掉的）。参照实现没有这一问，因为 Python 版从不做更新前的
// 可重启性判断。
func posixCanTerminate(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// platformProcessHooks 给出非 Windows 下 Env 的平台默认实现：
// signal 存活判定、SIGTERM 终止、管理员判定恒假（service.py:462 的
// `platform.system().lower() != "windows" → False`）、signal 0 可终止性判定。
func platformProcessHooks(_ *Env) (func(int) bool, func(int) error, func() bool, func(int) bool) {
	return posixRunning, posixTerminate, func() bool { return false }, posixCanTerminate
}
