package selfupdate

import (
	"fmt"
	"io"
	"os"
	"time"
)

// HelperFlag 是触发「更新收尾助手」的隐藏参数，取值是被挪开的旧二进制路径。
//
// 它由更新流程自己以分离进程方式启动，指向**刚装好的新二进制**。
const HelperFlag = "--update-helper"

// HelperStopFlag 告诉助手「旧服务需要你主动停掉」。
//
// 仅由 CLI 触发时带上：`amkr --update` 不是那个正在服务的进程，旧服务得由助手停。
// 服务端触发时**不带**——发起更新的就是服务进程本身，它自己优雅退出，助手去停只会
// 让 `taskkill /T /F` 把自己也杀掉（见 HelperHooks.Stop 的说明）。
const HelperStopFlag = "--update-helper-stop"

// 助手等待旧服务停止的默认上限。
//
// 旧进程收到关停信号后要等在途请求（含流式）结束，上限是它自己的 shutdownTimeout
// （10 秒）。这里留三倍余量：等不到就照常收尾，绝不无限期挂着。
const defaultHelperTimeout = 30 * time.Second

// HelperOptions 是助手的输入。
type HelperOptions struct {
	// Stale 是替换时挪开的旧二进制路径（<exe>.old）；空串表示不清理。
	Stale string
	// StopOld 表示需要助手主动停掉旧服务（CLI 触发的情形）。为假时假设旧进程会自己
	// 退出（服务端触发：那个进程正是发起更新的进程）。
	StopOld bool
	// Timeout 是等待旧服务停止的上限；0 表示用 defaultHelperTimeout。
	Timeout time.Duration
	// Log 记录助手做了什么；nil 表示不记录。
	Log io.Writer
}

// HelperHooks 是助手依赖的外部动作，全部注入以便测试。
type HelperHooks struct {
	// Running 表示更新前确实有实例在跑。为假时助手**不做重启**——
	// 用户只是跑了一次 `amkr --update`，不该因此凭空多出一个服务。
	Running bool
	// Stop 主动停掉服务。仅在 opts.StopOld 为真时被调用；为 nil 表示不主动停。
	//
	// 为什么不是无条件停：助手是发起方的**子进程**，而停止后台服务走
	// `taskkill /T /F`——实测 `/T` 会把整棵树连助手一起杀掉，助手就再也没机会收尾。
	// 由发起方自己优雅退出没有这个问题（实测分离子进程能在父进程退出后存活）。
	Stop func() error
	// Stopped 报告服务是否已彻底停止（端口已释放）。
	//
	// 用「端口是否还被占用」而不是「某个 PID 是否还在」：SYSTEM 计划任务形态下 PID
	// 拿不到（查询注册信息需要提权），而端口是**两种形态共有**的、且是启动新版本真正
	// 需要的那个资源。实测 `schtasks /End` 与优雅退出都会释放它。
	Stopped func() bool
	// Sleep 等待一小段时间。
	Sleep func(time.Duration)
	// Start 重新拉起服务；nil 表示不重启。
	Start func() error
}

// RunHelper 是更新收尾助手：停旧 → 等端口释放 → 删旧二进制 → 启动新版本。
//
// 顺序不可交换：删 .old 必须在旧进程退出之后（Windows 不允许删除正在运行的映像），
// 启动新版本也必须在旧进程让出端口之后。
//
// 任何一步失败都只记录、不返回错误：此刻新版本已经就位，清理或重启失败不该让用户以为
// 更新没成功（新二进制下次启动时会自己清掉残留的 .old，见 CleanStale）。
func RunHelper(opts HelperOptions, hooks HelperHooks) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHelperTimeout
	}
	logf := func(format string, args ...any) {
		if opts.Log == nil {
			return
		}
		fmt.Fprintf(opts.Log, "amkr 更新助手: "+format+"\n", args...)
	}
	logf("开始收尾（上限 %s，主动停旧=%v）", timeout, opts.StopOld)

	if !hooks.Running {
		// 更新前没有实例在跑：不必停、也不必启，只清理旧文件。
		// 用户只是跑了一次 `amkr --update`，不该因此凭空多出一个服务。
		logf("更新前没有运行中的实例，跳过停止与重启")
		removeStaleWithLog(opts.Stale, logf)
		return
	}

	if opts.StopOld && hooks.Stop != nil {
		logf("停止旧服务")
		if err := hooks.Stop(); err != nil {
			logf("停止旧服务失败（%v），仍继续等待端口释放", err)
		}
	}

	if hooks.Stopped != nil && hooks.Sleep != nil {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) && !hooks.Stopped() {
			hooks.Sleep(250 * time.Millisecond)
		}
		if !hooks.Stopped() {
			// 端口还占着：此刻启动新版本只会绑定失败，删 .old 也会因映像仍被映射而
			// 失败。留着 .old，交给新版本下次启动时清理。
			logf("旧服务未在 %s 内停止，跳过收尾", timeout)
			return
		}
		logf("旧服务已停止")
	}

	removeStaleWithLog(opts.Stale, logf)

	if hooks.Start == nil {
		logf("未注册服务，不重启")
		return
	}
	logf("启动新版本")
	if err := hooks.Start(); err != nil {
		logf("启动失败（%v）", err)
		return
	}
	logf("完成")
}

// removeStaleWithLog 删除旧二进制并记录结果；失败只记录，不影响后续步骤。
func removeStaleWithLog(stale string, logf func(string, ...any)) {
	if stale == "" {
		return
	}
	if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
		logf("删除旧版本文件失败（%v）", err)
		return
	}
	logf("已清理旧版本文件")
}

// CleanStale 删除上次更新遗留的 <executable>.old，返回是否删掉了东西。
//
// 助手的清理可能失败（旧进程此刻仍映射着那个映像，例如更新自身的就是它），因此启动时
// 再试一次：那时的持有者已经退出，删除必然成功。错误一律忽略——这是尽力而为的清理，
// 不该影响启动。
func CleanStale(executable string) bool {
	err := os.Remove(StalePath(executable))
	return err == nil
}
