package selfupdate

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// Spawner 启动分离的收尾助手进程，返回其 PID。
//
// 抽成接口让本包不依赖 internal/service：调用方（cmd/amkr 与 internal/server）各自把
// service.Env 的 Spawn 适配过来即可。
type Spawner func(command []string, logPath string) (int, error)

// Result 是一次自更新的结果，供 CLI 渲染与 API 序列化共用。
type Result struct {
	// Updated 表示确实换掉了二进制。
	Updated bool
	// CurrentVersion 是更新前的版本。
	CurrentVersion string
	// LatestVersion 是装上去的（或已是最新的）版本。
	LatestVersion string
	// RestartPending 表示已启动收尾助手，服务将由它重启。
	RestartPending bool
	// Message 是给人看的一句话结论。
	Message string
}

// Options 是 Perform 的输入。
type Options struct {
	// Check 查最新版本（注入以免测试联网）。
	Check func() updatecheck.Result
	// Executable 是当前运行的可执行文件路径（要被替换的那个）。
	Executable string
	// ConfigPath 交给收尾助手，让它按同一份配置判断注册形态并重启；可为空。
	ConfigPath string
	// StopOld 表示收尾助手需要**主动停掉**旧服务。
	//
	// 由 CLI 触发时为真：发起更新的 `amkr --update` 不是那个正在服务的进程，旧服务
	// （系统服务或后台服务）必须由助手停掉。
	//
	// 由服务端（WebUI/API）触发时为**假**：发起更新的就是正在服务的那个进程，它会自己
	// 优雅退出（见 cmd/amkr 的说明）。此时若让助手去停，后台服务形态下走的是
	// `taskkill /T /F`——实测 `/T` 会把助手自己也杀掉，助手就再没机会收尾。
	StopOld bool
	// Client 下载用的 HTTP 客户端；nil 表示 http.DefaultClient。
	Client *http.Client
	// Spawn 启动收尾助手；nil 表示不启动（只换文件，不自动重启）。
	Spawn Spawner
}

// Perform 执行自更新：查版本 → 下载 → 校验 → 替换 → 启动收尾助手。
//
// 返回的错误只表示「没能换上文件」。收尾助手启动失败**不算**错误：文件已经就位，只是
// 需要用户手动重启服务，这一点写在 Message 里。
func Perform(opts Options) (Result, error) {
	result := opts.Check()
	if result.Error != nil {
		return Result{CurrentVersion: result.CurrentVersion},
			fmt.Errorf("检查更新失败: %s", *result.Error)
	}
	out := Result{CurrentVersion: result.CurrentVersion}
	if !result.UpdateAvailable() {
		out.LatestVersion = out.CurrentVersion
		if result.LatestVersion != nil {
			out.LatestVersion = *result.LatestVersion
		}
		out.Message = "当前已是最新版本（" + out.LatestVersion + "）。"
		return out, nil
	}

	latest := *result.LatestVersion
	out.LatestVersion = latest
	asset := AssetName(latest, runtime.GOOS, runtime.GOARCH)

	// 校验先于替换：Apply 里任何失败都还没动过已安装的二进制（替换失败会回滚），
	// 因此这里可以放心地把错误原样报出去。
	stale, err := Apply(opts.Client, latest, asset, "", opts.Executable)
	if err != nil {
		return out, fmt.Errorf("更新到 %s 失败: %w", latest, err)
	}
	out.Updated = true
	out.Message = "已更新 " + out.CurrentVersion + " → " + latest + "。"

	if opts.Spawn == nil {
		out.Message += " 请重启服务以生效。"
		return out, nil
	}

	command, logPath := HelperCommand(opts.Executable, stale, opts.ConfigPath, opts.StopOld)
	if _, err := opts.Spawn(command, logPath); err != nil {
		// 文件已换好，只是没人收尾。新版本下次启动时 CleanStale 会清掉 .old，
		// 所以不判为失败，但要说清服务需要手动重启。
		out.Message += " 未能启动收尾助手（" + err.Error() + "），请手动重启服务以生效。"
		return out, nil
	}
	out.RestartPending = true
	out.Message += " 服务将由收尾助手自动重启。"
	return out, nil
}

// HelperCommand 返回收尾助手的命令行与日志路径。
//
// 助手用**已替换到位的目标路径**（executable）而不是当前进程的 os.Executable()：两者
// 此刻内容相同，但用目标路径能让「助手就是新版本」在参数上显而易见。
//
// 日志放在可执行文件旁边：助手以分离进程运行，没有任何终端可看，写文件是唯一的排查途径。
func HelperCommand(executable, stale, configPath string, stopOld bool) (command []string, logPath string) {
	// 取值作为**独立 argv 元素**传递：命令直接喂给 exec.Command，中间没有 shell，
	// 因此路径里的空格不会被拆开，不需要引号或 `=` 形态。
	command = []string{executable, HelperFlag, stale}
	if configPath != "" {
		command = []string{executable, "--config", configPath, HelperFlag, stale}
	}
	if stopOld {
		command = append(command, HelperStopFlag)
	}
	return command, filepath.Join(filepath.Dir(executable), "update.log")
}

// DefaultTempDir 返回下载暂存目录。
func DefaultTempDir() string { return os.TempDir() }
