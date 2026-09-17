// Command amkr 是 AMKR 的**最小可运行入口**。
//
// 它只做四件事：解析少量参数、载入配置、装配 internal/server、监听并在中断时干净
// 退出。这是「Go 版可以顶替 Python 作为服务运行」的最后一块拼图。
//
// # 明确未实现（后续任务）
//
//   - **完整 CLI**：参照实现的 `amkr` 有 24 个 flag（--host/--port/--webui/
//     --no-webui/--ops/--log-level/--print-config/...），本入口只有一个 -config 与
//     -version。参数解析、优先级（CLI > 环境变量 > 配置文件）都留待后续。
//   - **service.py**：守护进程化、Windows 计划任务、systemd/launchd 注册、日志轮转、
//     单实例锁，全部未移植。连带后果：运维面的 `POST /api/service/{action}` 未接线
//     （返回 500 + 明确文案，不会假装成功）。
//   - **Agent 集成**：`/api/integrations*` 三条路由的接缝（api.Server.Integrations）
//     未接线。internal/agentconfig 已经就绪，装配它属于后续任务。
//   - **WebSocket**（/ws/events 与 /v1/{path} 的升级）：前端改为轮询，本任务不含。
//   - **自更新**：产品决策已砍掉（见 internal/updatecheck 的说明）。
//
// # 退出码
//
//	0    正常退出（服务被关停且没有错误）
//	1    启动失败（配置读不出来、指标库打不开、端口被占用等）
//	130  Ctrl+C / SIGINT（128 + 2，与参照实现被键盘中断时的退出码一致）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	amkr "github.com/Sparrived/auto-model-key-router"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/server"
)

// version 与 pyproject.toml 的 project.version 保持一致；发布时可用
// `-ldflags "-X main.version=..."` 覆盖。
var version = "4.1.0"

// shutdownTimeout 是优雅关停的上限。
//
// 参照实现交给 uvicorn 的 timeout_graceful_shutdown（service.py 里给了 10 秒），
// 这里取同一个量级：流式响应不会被强杀，但也不会让关停无限期挂着。
const shutdownTimeout = 10 * time.Second

func main() {
	var (
		configPath  string
		showVersion bool
	)
	flag.StringVar(&configPath, "config", "",
		"配置文件路径；留空则依次尝试 $AMKR_CONFIG 与默认用户配置目录")
	flag.BoolVar(&showVersion, "version", false, "打印版本号后退出")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}
	os.Exit(run(configPath))
}

// run 装配并运行服务，返回进程退出码。
func run(configPath string) int {
	// 路径解析与参照实现的 config.resolve_config_path 同源：显式路径 > AMKR_CONFIG
	// > 默认路径（并处理旧版 router-config.json 的迁移）。
	resolved, err := config.ResolveConfigPath(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amkr: 解析配置路径失败: %v\n", err)
		return 1
	}
	loaded, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amkr: 载入配置失败: %v\n", err)
		return 1
	}

	app, err := server.New(server.Options{
		ConfigPath:  resolved,
		Config:      loaded,
		Version:     version,
		WebUIAssets: amkr.WebUIAssets,
	})
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

	failures := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "amkr %s 监听 http://%s\n", version, address)
		failures <- httpServer.ListenAndServe()
	}()

	return waitForStop(httpServer, failures, signals.Done(), os.Stderr)
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
