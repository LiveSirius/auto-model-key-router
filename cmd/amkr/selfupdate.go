package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/selfupdate"
	"github.com/Sparrived/auto-model-key-router/internal/service"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// 本文件是自更新的执行层：--update 子命令与收尾助手（--update-helper）。
//
// # 与 Python 版 update.py 的关系
//
// 产品决策 8 曾把自更新整体砍掉（约 573 行 update.py）。本次恢复，但**不恢复那套实现**：
// Python 版这么长，绝大部分在处理「运行中的可执行文件被锁住」，而 Windows 实际上允许
// *重命名* 正在运行的 exe（只是不允许覆盖或删除）。于是整个替换退化成两次 os.Rename，
// 剩下的问题只有「谁来收尾」——即把文件换好、旧进程退出之后，删掉旧文件并把服务拉起来。
// 这由一个分离的助手进程完成：编排在 internal/selfupdate，平台接线在这里。
//
// # 助手为什么是必须的
//
// 换完文件后，旧二进制 <exe>.old 还映射在旧进程里，删不掉；而发起更新的进程（CLI 前台
// 或正在服务的 WebUI 进程）又不能等自己退出。因此需要一个不属于旧进程树的进程来收尾。
// 实测确认：分离进程能在父进程退出后存活，且 `schtasks /End` 不会波及它；但
// `taskkill /T /F` **会**把整棵树杀掉——所以助手绝不能落在被 /T 波及的那棵树里，
// 这也是为什么服务端触发时由发起进程自己优雅退出，而不去杀自己。

// selfUpdateTimeout 是自更新的整体上游超时。
//
// 比版本检查（10 秒）宽松得多：要下载一个十几 MB 的二进制加一个校验和文件。
const selfUpdateTimeout = 5 * time.Minute

// updateHTTPClient 返回自更新用的 HTTP 客户端。
//
// 不复用 http.DefaultClient：自更新要下载大文件，需要独立的超时，也不该被别处改过的
// DefaultClient 影响。
func updateHTTPClient() *http.Client {
	return &http.Client{Timeout: selfUpdateTimeout}
}

// selfUpdateSpawner 把 service.Env 的分离进程启动适配成 selfupdate.Spawner。
func selfUpdateSpawner(env *service.Env) selfupdate.Spawner {
	return func(command []string, logPath string) (int, error) {
		result, err := env.Spawn(service.SpawnSpec{
			Command:    command,
			Cwd:        filepath.Dir(command[0]),
			Env:        os.Environ(),
			LogPath:    logPath,
			Detached:   true,
			CloseFiles: runtime.GOOS != "windows",
		})
		if err != nil {
			return 0, err
		}
		return result.Pid, nil
	}
}

// runSelfUpdate 执行一次 CLI 触发的自更新。
//
// StopOld 为真：发起更新的 `amkr --update` **不是**那个正在服务的进程，旧服务必须由
// 收尾助手停掉（服务端触发时相反，见 selfupdate.Options.StopOld）。
//
// 收尾助手继承的是**当前用户**的权限。本机实测：服务以 SYSTEM 计划任务运行时，非提权
// 进程连 `schtasks /Query` 都被拒，既查不到也停不掉它。那种情况下不启动助手，而是如实
// 告诉用户需要手动重启——见 service.CanRestartService。
func runSelfUpdate(check func() updatecheck.Result, executable, configPath string, loaded *config.RouterConfig) (selfupdate.Result, error) {
	env := service.DefaultEnv()
	canRestart := service.CanRestartServiceWith(env, configPath, loaded)
	var spawn selfupdate.Spawner
	if canRestart {
		spawn = selfUpdateSpawner(env)
	}
	result, err := selfupdate.Perform(selfupdate.Options{
		Check:      check,
		Executable: executable,
		ConfigPath: configPath,
		StopOld:    true,
		Client:     updateHTTPClient(),
		Spawn:      spawn,
	})
	if err == nil && result.Updated && !canRestart {
		// 文件已经换成新版，但没人能停下那个正在服务的实例（本机实测的 SYSTEM 计划任务
		// 限制）。说清"要做什么"，而不是留一句含糊的"请重启服务"。
		result.Message += " 当前实例以更高权限注册，自动重启不可用；" +
			"请以管理员身份运行 `amkr --service restart`（或重启计算机）使其生效。"
	}
	return result, err
}

// updatePanel 渲染自更新结果面板。
func updatePanel(result selfupdate.Result) tui.Renderable {
	return tui.SectionPanel(result.Message, "自更新", "green")
}

// runUpdateHelper 是 `--update-helper <stale>` 的实现：收尾助手进程的主体。
//
// stopOld 对应 `--update-helper-stop`：仅 CLI 触发时带上（见 selfupdate.HelperStopFlag）。
//
// 它以分离进程启动，因此日志文件是唯一的输出途径。
func runUpdateHelper(stalePath, configPath string, stopOld bool, out io.Writer) int {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(out, "amkr 更新助手: 取可执行文件路径失败（%v）\n", err)
		return 1
	}
	logPath := filepath.Join(filepath.Dir(executable), "update.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		// 日志都开不了就别硬撑：助手没有别的输出渠道。
		fmt.Fprintf(out, "amkr 更新助手: 打开日志失败（%v）\n", err)
		return 1
	}
	defer func() { _ = logFile.Close() }()

	resolved, err := config.ResolveConfigPath(configPath)
	if err != nil {
		fmt.Fprintf(logFile, "amkr 更新助手: 解析配置路径失败（%v）\n", err)
		return 1
	}
	loaded, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(logFile, "amkr 更新助手: 加载配置失败（%v）\n", err)
		return 1
	}

	selfupdate.RunHelper(
		selfupdate.HelperOptions{
			Stale:   stalePath,
			StopOld: stopOld,
			Log:     logFile,
		},
		service.SelfUpdateHooks(resolved, loaded),
	)
	return 0
}
