package service

import (
	"os"
	"os/exec"
)

// 本文件是分离进程启动的通用实现；平台差异（SysProcAttr）由
// spawn_windows.go / spawn_posix.go 提供。
//
// 这一整条路径**只有 fake 覆盖**：测试注入 Env.Spawn 记录「要启动什么」，
// 真实的进程创建不会被任何测试触发（见包注释）。

// defaultSpawn 返回真实实现：按 SpawnSpec 打开日志文件、设置平台分离标志并启动。
func defaultSpawn() SpawnFunc {
	return func(spec SpawnSpec) (SpawnResult, error) {
		command := exec.Command(spec.Command[0], spec.Command[1:]...)
		command.Dir = spec.Cwd
		command.Env = spec.Env
		// stdin=DEVNULL：Python 传 subprocess.DEVNULL，避免子进程继承终端。
		command.Stdin = nil

		// Python 用 `open(path, "a")` 打开日志并把同一个句柄交给 stdout/stderr，
		// 启动后父进程立刻 close（子进程持有自己的副本）。这里同样追加打开、
		// 启动后关闭。
		logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
		if err != nil {
			return SpawnResult{}, err
		}
		defer func() { _ = logFile.Close() }()
		command.Stdout = logFile
		command.Stderr = logFile

		command.SysProcAttr = sysProcAttr(spec.Detached)
		if err := command.Start(); err != nil {
			return SpawnResult{}, err
		}
		pid := command.Process.Pid
		// 不 Wait：后台服务由 PID 文件与健康检查管理，父进程退出后它继续运行。
		_ = command.Process.Release()
		return SpawnResult{Pid: pid}, nil
	}
}
