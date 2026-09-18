//go:build !windows

package service

import "syscall"

// sysProcAttr 在 POSIX 上把 Detached 翻译成 Setsid，对应 Python 的
// `start_new_session=True`（service.py:78）。
//
// 与 Python 的差异：Python 还会传 close_fds=True；Go 的 os/exec 在 POSIX 上本来就
// 只把 0/1/2 交给子进程，无需额外设置（SpawnSpec.CloseFiles 因此只作记录）。
func sysProcAttr(detached bool) *syscall.SysProcAttr {
	if !detached {
		return nil
	}
	return &syscall.SysProcAttr{Setsid: true}
}
