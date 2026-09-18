// Command amkr 是 AMKR 的入口：完整 CLI + 前台服务。
//
// 参数解析与分支决策在 cli.go（可脱离副作用对拍），本文件负责装配与执行：
// 载入配置、装配 internal/server、监听并在中断时干净退出；以及把 CLI 动作分派给
// internal/service（后台启停、系统服务注册）与 internal/unifiedmodel（统一模型切换）。
//
// # 与参照实现（main.py，202 行）的关系
//
// 24 个 flag 的处置、被砍掉的三个 flag、以及默认动作的开放决策都写在 cli.go 的文件头。
// 本文件只补充执行层的差异：
//
//   - **--check-update 的可手动更新命令**。参照实现给的是 pip/uv 命令（update.py），
//     Go 版按决策 8 由 `go install` 分发，因此给的是 go install 命令。渲染函数把命令
//     作为参数，语料用参照实现的命令文本喂进去逐行对拍（见 versionCheckLines）。
//   - **--show-config 不含 quick_metrics_items**（要直查 metrics.db 的原始 SQL，
//     internal/metrics 没有等价接口），见 cli.go 的说明。
//
// # 退出码
//
//	0    正常退出（含所有只读子命令）
//	1    失败（配置读不出来、切换失败、启动失败等）
//	2    参数错误（与 argparse 一致）
//	130  Ctrl+C / SIGINT（128 + 2，与参照实现被键盘中断时的退出码一致）
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	amkr "github.com/Sparrived/auto-model-key-router"
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/configservice"
	"github.com/Sparrived/auto-model-key-router/internal/logfiles"
	"github.com/Sparrived/auto-model-key-router/internal/server"
	"github.com/Sparrived/auto-model-key-router/internal/service"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
	"github.com/Sparrived/auto-model-key-router/internal/unifiedmodel"
	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// version 与 pyproject.toml 的 project.version 保持一致；发布时可用
// `-ldflags "-X main.version=..."` 覆盖。
var version = "4.1.0"

// shutdownTimeout 是优雅关停的上限。
//
// 参照实现交给 uvicorn 的 timeout_graceful_shutdown（service.py 里给了 10 秒），
// 这里取同一个量级：流式响应不会被强杀，但也不会让关停无限期挂着。
const shutdownTimeout = 10 * time.Second

// checkUpdateTimeout 对应 main.py:88 的 `check_latest_version(timeout=10.0)`。
const checkUpdateTimeout = 10 * time.Second

// defaultUsage 是参数错误时打印的简短用法（argparse 的完整用法文本无法复刻，
// 语料只对拍退出码，见 cli.go 的差异 4）。
const defaultUsage = `用法: amkr [选项]

常用选项:
  --config PATH            配置文件路径（默认 $AMKR_CONFIG 或用户配置目录）
  --serve                  后台启动服务（默认前台启动）
  --stop / --status        停止 / 查看后台服务
  --show-config            展示配置摘要
  --show-address           展示监听地址
  --show-api-key           打印本地授权 Key
  --check-update           检查最新版本
  --version                打印版本号
  --install-service        注册为系统服务
  --service ACTION         管理系统服务（install/start/stop/restart/status/...）
`

// cliEnv 是执行层的外部依赖接缝：CLI 测试注入它以免真的起服务、连网络或改配置。
type cliEnv struct {
	// out / err 是标准输出与错误输出。
	out io.Writer
	err io.Writer
	// argv0 用于 --version 的程序名（对应 sys.argv[0]）。
	argv0 string
	// clearHistory 对应 main.py:86 的 clear_terminal_history。
	clearHistory func()
	// service 是服务管理接缝（internal/service 的 Env）。
	service *service.Env
	// checkUpdate 对应 update.check_latest_version(timeout=10.0)。
	checkUpdate func() updatecheck.Result
	// manualUpdateCommand 是 --check-update 展示的可手动更新命令。
	manualUpdateCommand string
	// switchUnified 执行统一模型切换（测试注入记录桩）。
	switchUnified func(configPath string, opts *options) (*config.RouterConfig, error)
	// serveForeground 前台启动服务，返回退出码（默认真的监听端口）。
	serveForeground func(configPath string, cfg *config.RouterConfig) int
}

// defaultManualUpdateCommand 是 Go 版分发方式下的手动更新命令（决策 8）。
const defaultManualUpdateCommand = "go install github.com/Sparrived/auto-model-key-router/cmd/amkr@latest"

func main() {
	os.Exit(runCLI(os.Args, os.Stdout, os.Stderr, nil))
}

// runCLI 是 main.py 的 main() 等价物，返回进程退出码。
//
// overrides 非 nil 时用于测试注入（nil 表示真实依赖）。
func runCLI(argv []string, stdout, stderr io.Writer, overrides *cliEnv) int {
	ctx := overrides
	if ctx == nil {
		ctx = &cliEnv{}
	}
	if ctx.out == nil {
		ctx.out = stdout
	}
	if ctx.err == nil {
		ctx.err = stderr
	}
	if ctx.argv0 == "" {
		if len(argv) > 0 {
			ctx.argv0 = argv[0]
		} else {
			ctx.argv0 = "amkr"
		}
	}
	if ctx.clearHistory == nil {
		ctx.clearHistory = tui.ClearTerminalHistory
	}
	if ctx.service == nil {
		ctx.service = service.DefaultEnv()
	}
	if ctx.checkUpdate == nil {
		ctx.checkUpdate = func() updatecheck.Result {
			fetch := updatecheck.HTTPFetcher(&http.Client{Timeout: checkUpdateTimeout})
			return updatecheck.CheckLatestVersion(fetch, version, checkUpdateTimeout)
		}
	}
	if ctx.manualUpdateCommand == "" {
		ctx.manualUpdateCommand = defaultManualUpdateCommand
	}
	if ctx.switchUnified == nil {
		ctx.switchUnified = switchUnified
	}
	if ctx.serveForeground == nil {
		ctx.serveForeground = serveForeground
	}

	opts, err := parseOptions(argv[1:], ctx.err)
	if err != nil {
		fmt.Fprintf(ctx.err, "%s: %v\n%s", progName(ctx.argv0), err, defaultUsage)
		return 2
	}
	if opts.showVersion {
		fmt.Fprintf(ctx.out, "%s %s\n", progName(ctx.argv0), version)
		return 0
	}
	// main.py:85-86：机器可读的 key 输出不能混入终端控制序列。
	if !opts.showAPIKey {
		ctx.clearHistory()
	}
	terminal := newTerminal(ctx.out)
	command := selectCommand(opts)

	if command == commandCheckUpdate {
		terminal.Print(versionCheckPanel(ctx.checkUpdate(), ctx.manualUpdateCommand))
		return 0
	}

	resolved, err := config.ResolveConfigPath(opts.configPath)
	if err != nil {
		printErrorPanel(terminal, err, "配置加载失败")
		return 1
	}
	// WebUI/运维开关是持久化设置：三个启动路径（前台/后台/系统服务）都读同一份
	// 配置，写成配置项才能保证 --webui 对后台启动也生效（main.py:95-103）。
	if opts.webui != nil {
		if err := updateConfigFlag(resolved, "webui_enabled", *opts.webui); err != nil {
			printErrorPanel(terminal, err, "配置写入失败")
			return 1
		}
	}
	if opts.ops != nil {
		if err := updateConfigFlag(resolved, "ops_enabled", *opts.ops); err != nil {
			printErrorPanel(terminal, err, "配置写入失败")
			return 1
		}
	}

	loaded, err := config.Load(resolved)
	if err != nil {
		printErrorPanel(terminal, err, "配置加载失败")
		return 1
	}
	loaded = opts.configOverrides(loaded)

	switch command {
	case commandSwitchUnified:
		updated, err := ctx.switchUnified(resolved, opts)
		if err != nil {
			printErrorPanel(terminal, err, "统一模型切换失败")
			return 1
		}
		terminal.Print(unifiedModelPanel(updated, "统一模型已切换", "green"))
		return 0
	case commandShowAPIKey:
		fmt.Fprintln(ctx.out, loaded.LocalAPIKey)
		return 0
	case commandShowUnifiedModel:
		terminal.Print(unifiedModelPanel(loaded, "统一模型", "cyan"))
		return 0
	case commandShowAddress:
		terminal.Print(tui.SectionPanel(routerAddressText(loaded), "AMKR 地址", "cyan"))
		return 0
	case commandShowConfig:
		printConfigSummary(terminal, loaded)
		return 0
	case commandStop:
		terminal.Print(ctx.service.StopBackground(loaded))
		return 0
	case commandStatus:
		terminal.Print(ctx.service.BackgroundStatusPanel(loaded, resolved))
		return 0
	case commandInstallService, commandManageService:
		action := opts.serviceArgument()
		if action == "status" {
			terminal.Print(ctx.service.ServiceStatusPanel(loaded, resolved))
			return 0
		}
		panel, err := ctx.service.ManageSystemService(resolved, action)
		if err != nil {
			printErrorPanel(terminal, err, "系统服务操作失败")
			return 1
		}
		terminal.Print(panel)
		return 0
	case commandBackground:
		panel, err := ctx.service.StartBackground(resolved, loaded)
		if err != nil {
			printErrorPanel(terminal, err, "后台服务启动失败")
			return 1
		}
		terminal.Print(panel)
		return 0
	default: // commandForeground
		// main.py:108-109：后台启动会带 AMKR_LOG_ARCHIVED=1，避免二次归档。
		if _, _, err := ctx.service.ArchiveLogForForeground(loaded); err != nil {
			fmt.Fprintf(ctx.err, "amkr: 归档日志失败: %v\n", err)
		}
		return ctx.serveForeground(resolved, loaded)
	}
}

// newTerminal 构造一个写到 out 的终端（宽度沿用 COLUMNS，与 tui.Console 同规则）。
func newTerminal(out io.Writer) *tui.Terminal {
	terminal := tui.NewTerminal()
	terminal.SetOutput(out)
	return terminal
}

// updateConfigFlag 走 ConfigService 的读-改-写路径（与 Python 同一把按路径锁）。
func updateConfigFlag(configPath, key string, value bool) error {
	_, err := configservice.New(configPath).Update(func(data *canonical.Value) error {
		data.SetKey(key, canonical.NewBool(value))
		return nil
	})
	return err
}

// switchUnified 对应 main.py:134-142 的统一模型切换。
//
// key 传 "auto" 时用 nil 表示「恢复自动路由」（main.py:140）。
func switchUnified(configPath string, opts *options) (*config.RouterConfig, error) {
	var keyName *string
	if opts.switchKey != nil {
		if *opts.switchKey != "auto" {
			keyName = opts.switchKey
		}
	}
	return unifiedmodel.SwitchUnifiedTarget(
		configPath, opts.unifiedTarget, opts.switchModel, keyName, opts.switchKey != nil)
}

// printErrorPanel 复刻 main.py:126 的错误面板（红色，标题为场景名）。
func printErrorPanel(terminal *tui.Terminal, err error, title string) {
	terminal.Print(tui.SectionPanel(fmt.Sprintf("[red]%s[/red]", err.Error()), title, "red"))
}

// unifiedModelPanel 渲染统一模型摘要面板。
//
// 参照实现用 dashboard.unified_model_status_panel（已按决策 7 砍掉）；这里是等价纯文本。
func unifiedModelPanel(cfg *config.RouterConfig, title, color string) tui.Renderable {
	return tui.SectionPanel(strings.Join(unifiedModelSummaryLines(cfg), "\n"), title, color)
}

// printConfigSummary 是 --show-config 的输出：运行概览 + 公网风险 + 模型配置表。
func printConfigSummary(terminal *tui.Terminal, cfg *config.RouterConfig) {
	healthy := service.DefaultEnv().IsServiceHealthy(cfg.Host, cfg.Port, true)
	width, _ := terminal.Size()
	summary := configSummaryLine(cfg, healthy, true, width)
	terminal.Print(tui.SectionPanel(summary, "运行概览", "cyan"))
	if cfg.Host == "0.0.0.0" {
		terminal.Print(tui.SectionPanel(publicWarningText, "公网开放风险", "red"))
	}
	rows := configModelRows(cfg)
	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells = append(cells, row)
	}
	terminal.Print(tui.SectionPanel(tui.Table{
		Columns: []tui.TableColumn{{Width: 28}, {Width: 24}, {Width: 24}, {Width: 8}, {Width: 6}},
		Rows:    cells,
	}, "模型配置", "blue"))
}

// versionCheckPanel 渲染版本检查结果（update.py:557 的 render_version_check_result）。
func versionCheckPanel(result updatecheck.Result, manualCommand string) tui.Renderable {
	lines, style := versionCheckLines(result, manualCommand)
	return tui.SectionPanel(strings.Join(lines, "\n"), "版本检查", style)
}

// versionCheckLines 是版本检查面板的纯行文本。
//
// manualCommand 作为参数：参照实现给的是 pip/uv 命令，Go 版给的是 go install 命令
// （决策 8），对拍时用参照实现的命令文本喂入即可逐行比较其余内容。
func versionCheckLines(result updatecheck.Result, manualCommand string) ([]string, string) {
	if result.Error != nil {
		return []string{
			fmt.Sprintf("当前版本: [bold]%s[/bold]", escapeMarkup(result.CurrentVersion)),
			fmt.Sprintf("检查失败: [red]%s[/red]", escapeMarkup(*result.Error)),
			fmt.Sprintf("GitHub Releases: [bold]%s[/bold]", updatecheck.GitHubReleasesURL),
		}, "red"
	}
	latest := "-"
	if result.LatestVersion != nil && *result.LatestVersion != "" {
		latest = *result.LatestVersion
	}
	lines := []string{
		fmt.Sprintf("当前版本: [bold]%s[/bold]", escapeMarkup(result.CurrentVersion)),
		fmt.Sprintf("最新版本: [bold]%s[/bold]", escapeMarkup(latest)),
	}
	if result.Source != nil && *result.Source != "" {
		lines = append(lines, fmt.Sprintf("检查来源: [bold]%s[/bold]", escapeMarkup(*result.Source)))
	}
	if result.ReleaseURL != nil && *result.ReleaseURL != "" {
		lines = append(lines, fmt.Sprintf("发布页面: [bold]%s[/bold]", escapeMarkup(*result.ReleaseURL)))
	}
	if result.UpdateAvailable() {
		lines = append(lines,
			"状态: [yellow]发现新版本，可手动更新。[/yellow]",
			"手动更新命令:",
			fmt.Sprintf("[bold]%s[/bold]", escapeMarkup(manualCommand)),
		)
		return lines, "yellow"
	}
	lines = append(lines, "状态: [green]当前已是最新版本。[/green]")
	return lines, "green"
}

// escapeMarkup 对应 rich.markup.escape：把值里的方括号转义，避免被当标记吞掉。
func escapeMarkup(value string) string {
	return strings.ReplaceAll(value, "[", `\[`)
}

// serveForeground 写 PID 文件后监听端口（原 main.go 的 run，加上了 PID/日志处理）。
func serveForeground(configPath string, loaded *config.RouterConfig) int {
	env := service.DefaultEnv()
	cleanup, err := env.ForegroundPIDHook(loaded)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amkr: 写 PID 文件失败: %v\n", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// 日志落点：参照实现的 uvicorn_log_config 把**所有**日志（应用侧、访问、服务器
	// 自身）写进 config.log_file_path，运维接口 /api/logs 读的就是这个文件。迁移时
	// 这一环漏了，Go 侧只往 stderr 写，于是 log_file_path 恒为空、WebUI 的「服务日志」
	// 面板永远是空的——这正是本次修复的根因。
	//
	// 前台启动**不归档**旧日志：对应的归档动作由调用方在更早处完成
	// （ArchiveLogForForeground，且 AMKR_LOG_ARCHIVED=1 时不重复归档），这里以追加
	// 方式打开即可。
	sink, err := logfiles.Open(loaded.LogFilePath)
	if err != nil {
		// 日志文件打不开不该拦住服务：参照实现里 logging.FileHandler 的失败同样只是
		// 让那一路日志丢失，进程照常启动。
		fmt.Fprintf(os.Stderr, "amkr: 打开日志文件 %s 失败: %v\n", loaded.LogFilePath, err)
		sink = nil
	}
	defer func() {
		if sink != nil {
			_ = sink.Close()
		}
	}()

	options := server.Options{
		ConfigPath:  configPath,
		Config:      loaded,
		Version:     version,
		WebUIAssets: amkr.WebUIAssets,
	}
	if sink != nil {
		options.Logger = sink.App
		options.AccessLogger = sink.Access
	}
	app, err := server.New(options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amkr: 装配服务失败: %v\n", err)
		return 1
	}
	defer func() {
		if closeErr := app.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "amkr: 关停时释放资源失败: %v\n", closeErr)
		}
	}()

	address := net.JoinHostPort(loaded.Host, strconv.Itoa(loaded.Port))
	httpServer := &http.Server{
		Addr:    address,
		Handler: app.Handler(),
		// 不设 ReadTimeout/WriteTimeout：流式响应可能长时间只推少量字节
		// （参照实现的流式语义是「没有总时长上限」，见 internal/runtime/streaming.go）。
		// 各阶段的超时由 proxy/upstream 自己的窗口负责。
	}

	// 信号处理：Ctrl+C（os.Interrupt）与 SIGTERM 都走同一条优雅关停路径。
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 启动行写日志文件，对应 uvicorn.error 的 `Uvicorn running on http://…`。
	//
	// 只在日志不可用**或** stderr 是终端时才同时打印到 stderr，避免同一行落进日志
	// 文件两遍：后台启动路径（internal/service/spawn.go）把子进程的 stdout/stderr
	// 重定向到**同一个**日志文件，若这里无条件打印，日志里就会先是 Go 原样的
	// `amkr 4.1.0 监听 …`、再来一行 Python 格式的同义行。Windows 计划任务路径没有
	// 重定向，无条件打印则纯粹是把这行丢掉。
	startup := fmt.Sprintf("amkr %s 监听 http://%s", version, address)
	if sink != nil {
		sink.Server.Info(startup)
	}
	if sink == nil || isTerminal(os.Stderr) {
		fmt.Fprintln(os.Stderr, startup)
	}

	failures := make(chan error, 1)
	go func() {
		failures <- httpServer.ListenAndServe()
	}()

	return waitForStop(httpServer, failures, signals.Done(), os.Stderr)
}

// isTerminal 判断 w 是不是字符设备（交互式终端）。
//
// 只用于决定「这行要不要同时给人看」：日志文件已由 sink 保证，控制台输出是锦上
// 添花，判错了也不影响日志内容的正确性。
func isTerminal(w *os.File) bool {
	info, err := w.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// waitForStop 等待「监听失败」与「收到中断」两件事之一，返回进程退出码。
//
// 单独抽出来是为了让 130 这条路径可测：测试进程里无法真的给自己发 Ctrl+C
// （Go 在 Windows 上不支持 os.Process.Signal(os.Interrupt)），但把「中断通道已关闭」
// 作为输入就能逐条断言三个退出码。
func waitForStop(httpServer *http.Server, failures <-chan error, interrupted <-chan struct{}, out io.Writer) int {
	select {
	case err := <-failures:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(out, "amkr: 监听 %s 失败: %v\n", httpServer.Addr, err)
			return 1
		}
		return 0
	case <-interrupted:
		fmt.Fprintln(out, "amkr: 收到中断信号，正在关停……")
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			// 超时不代表启动失败：强制关闭是最后手段，但仍按中断语义返回 130。
			fmt.Fprintf(out, "amkr: 优雅关停超时: %v\n", err)
			_ = httpServer.Close()
		}
		return 130
	}
}
