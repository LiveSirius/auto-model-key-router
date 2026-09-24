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

// windowsCanTerminate 报告本进程是否有权终止 pid。
//
// 用 OpenProcess(PROCESS_TERMINATE) 探测，而不是看 taskkill 的输出：实测该调用能把
// 两种情况干净地分开（本机验证）——
//
//	同用户进程      → 成功
//	SYSTEM 会话0 进程 → ERROR_ACCESS_DENIED（在跑，但杀不掉）
//	不存在的 PID     → ERROR_INVALID_PARAMETER 87
//
// 这正是自更新需要的分辨力：SYSTEM 计划任务跑起来的实例对普通用户不可终止，此时必须
// 提前说清"自动重启不可用"，而不是让助手空等 30 秒再放弃。
//
// 与 isUserAnAdminWindows 同理，直接用 syscall 而不引入 x/sys/windows（它只是间接依赖）。
func windowsCanTerminate(pid int) bool {
	handle, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(handle)
	return true
}

// platformProcessHooks 给出 Windows 下 Env 的平台默认实现：
// tasklist 存活判定、taskkill 终止、shell32 管理员判定、OpenProcess 可终止性判定。
func platformProcessHooks(env *Env) (func(int) bool, func(int) error, func() bool, func(int) bool) {
	return env.windowsRunning, env.windowsTerminate, isUserAnAdminWindows, windowsCanTerminate
}
