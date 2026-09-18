//go:build windows

package service

import "syscall"

// isUserAnAdminWindows 对应 service.py:461 的 is_windows_admin：
// `ctypes.windll.shell32.IsUserAnAdmin()`。
//
// Go 侧用 syscall 直接调 shell32，避免为此引入 golang.org/x/sys 的 windows 子包
// （它在 go.mod 里只是间接依赖）。参照实现把任何异常都折成 false，这里同样：
// 加载 DLL 或调用失败都返回 false。
//
// 这一条路径**只有 fake 覆盖**（测试注入 Env.IsAdmin）。
func isUserAnAdminWindows() bool {
	shell32 := syscall.NewLazyDLL("shell32.dll")
	procedure := shell32.NewProc("IsUserAnAdmin")
	result, _, _ := procedure.Call()
	return result != 0
}

// platformProcessHooks 给出 Windows 下 Env 的三个平台默认实现：
// tasklist 存活判定、taskkill 终止、shell32 管理员判定。
func platformProcessHooks(env *Env) (func(int) bool, func(int) error, func() bool) {
	return env.windowsRunning, env.windowsTerminate, isUserAnAdminWindows
}
