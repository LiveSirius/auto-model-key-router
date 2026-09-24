//go:build windows

package service

import (
	"os"
	"testing"
)

// 本文件跑**真实的** OpenProcess，而不是注入的桩。
//
// 为什么值得单开一个平台文件：windowsCanTerminate 是本次修复的地基——自更新靠它分辨
// 「在跑但杀不掉的 SYSTEM 实例」。这条判断的两个分支（有权 / 无权）都无法在 Linux CI
// 上覆盖，而它就是本机踩到的那个坑（PID 文件指向会话 0 的进程，taskkill 一律被拒）。
//
// 实测记录（未提权的普通用户）：
//
//	自身 pid        → 可终止
//	SYSTEM 会话0    → ERROR_ACCESS_DENIED
//	不存在的 pid    → ERROR_INVALID_PARAMETER 87
func TestWindowsCanTerminateDiscriminatesPrivilege(t *testing.T) {
	// 自己一定杀得掉（同用户、同权限）。
	if !windowsCanTerminate(os.Getpid()) {
		t.Error("应当有权终止自己")
	}

	// 不存在的 PID 必须判「不能」——**这条最关键**：若它返回 true，
	// CanRestartService 就无法把「进程已退出」与「进程杀不掉」分开，
	// 于是每次更新都会误报"无法自动重启"。
	//
	// 用一个几乎不可能存在的 PID（Windows PID 是 4 的倍数，且远小于此）。
	const impossible = 0x7FFFFFF0
	if windowsCanTerminate(impossible) {
		t.Errorf("不存在的 PID %d 不该判为可终止", impossible)
	}

	// SYSTEM 会话 0 的进程：在跑，但普通用户杀不掉。
	// 只在**未提权**时断言——提权后能否打开系统进程取决于具体进程的保护级别，
	// 那不是这个函数要保证的语义（它只回答"我现在杀不杀得掉"）。
	if isUserAnAdminWindows() {
		t.Skip("当前是管理员，跳过 SYSTEM 不可终止断言")
	}
	if windowsCanTerminate(4) {
		t.Error("未提权时不该有权终止 SYSTEM 进程（pid 4）")
	}
}
