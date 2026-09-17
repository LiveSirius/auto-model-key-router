package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/api"
	"github.com/Sparrived/auto-model-key-router/internal/auth"
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/health"
	"github.com/Sparrived/auto-model-key-router/internal/keypool"
	"github.com/Sparrived/auto-model-key-router/internal/metrics"
	"github.com/Sparrived/auto-model-key-router/internal/proxy"
	"github.com/Sparrived/auto-model-key-router/internal/runtime"
	"github.com/Sparrived/auto-model-key-router/internal/upstream"
	"github.com/Sparrived/auto-model-key-router/internal/webui"
)

// Options 是装配一个服务所需的全部输入。
//
// 所有可选字段的 nil 语义都与参照实现对齐：nil 表示「跟随配置 / 用默认实现」。
type Options struct {
	// ConfigPath 是配置文件路径，对应 create_app 的 config_path 参数
	// （app.py:52）。空串表示不做热重载、/health 也不报 config_path
	// （app.py:85-87）。
	ConfigPath string
	// Config 是启动时载入的配置快照，对应 create_app 的 config 参数。
	Config *config.RouterConfig
	// Version 是进程版本号，对应 `from . import __version__`（app.py:15）。
	// 进入 /health 与版本检查。
	Version string
	// WebUIAssets 是**以 WebUI 资产目录为根**的文件系统。
	//
	// 做成注入参数而不是在这里 embed：`//go:embed` 的模式不能引用父目录，而资产在
	// auto_model_key_router/webui 下（见根包的 webui_assets.go）。nil 表示资产缺失
	// （webui.Available 为假，/health 的 webui_available 报 false）。
	WebUIAssets fs.FS
	// WebUIEnabled 覆盖配置里的 webui_enabled；nil 表示跟随配置
	// （app.py:82、app.py:147 的 `webui=None` 语义）。
	WebUIEnabled *bool
	// OpsEnabled 覆盖配置里的 ops_enabled；nil 表示跟随配置（app.py:142-143）。
	//
	// 它同时决定 /health 报出的 ops_enabled 与**运维面 7 条路由是否注册**：装配时
	// 原样传给 api.Server.OpsEnabled，由它在自己的 mux 上注册 /api/logs、/api/tool、
	// /api/tool/webui、/api/service/{action} 与三条 /api/integrations*。
	// 与 Python 相同：运维路由与管理路由注册在同一个应用上，共用同一套鉴权与错误形状。
	OpsEnabled *bool
	// Authorizer 覆盖鉴权实现，对应 create_app(authenticator=...)（app.py:84）。
	// nil 表示 internal/auth 的默认实现（本地 key = full，固定 visitor key = visitor）。
	//
	// 同一个鉴权器会同时作用于 app 面、proxy 与管理 API：参照实现只有
	// app.state.authenticator 一份。
	Authorizer auth.Authorizer
	// CheckUpdate 覆盖版本检查，对应 update.check_latest_version。
	//
	// nil 表示用 internal/updatecheck 的真实实现（会发网络请求）。注入它主要是为了
	// 测试与语料回放不联网。**UpdateAvailable 由适配器按 latest/current 推导**，
	// 见 updatecheck.go。
	CheckUpdate func(timeout float64) api.UpdateCheckResult
	// Logger 交给 internal/proxy 做结构化日志；nil 时按 AMKR_LOG_FORMAT 选择格式。
	Logger *slog.Logger
	// MountPrefix 是嵌入到宿主时的挂载前缀（独立运行时为空串）。它只影响 /health
	// 报出的 webui_path（app.py:418 的 mount_path 语义）。
	MountPrefix string
}

// App 是一个装配完成、可以开始处理请求的服务实例。
//
// 它可以并发使用：Handler 返回的 http.Handler 是并发安全的，热重载由 reloadMu
// 串行化，共享的 RuntimeManager 自带锁。
type App struct {
	options Options

	proxy   *proxy.Handler
	manager *runtime.RuntimeManager
	api     *api.Server
	handler http.Handler

	// configPath 是**绝对化之后**的配置路径，对应 app.state.config_path
	// （app.py:85-87 的 `str(Path(config_path).resolve())`）。
	configPath string
	// webuiAvailable / webuiMounted / webuiEnabled 对应 webui_status 的三个读数
	// （webui.py:48-70）。mounted 与 enabled 的区别是刻意的：改了开关需要重启才生效。
	webuiAvailable bool
	webuiMounted   bool
	webuiEnabled   bool
	// opsEnabled 对应 app.state.ops_enabled（app.py:146）。
	opsEnabled bool

	// reloadMu 对应 app.state.config_reload_lock（app.py:89）。
	reloadMu sync.Mutex
	// configMtime 对应 app.state.config_mtime；零值等价 Python 的 0.0（「拿不到
	// mtime」），此时不做热重载（app.py:446）。
	configMtime time.Time

	closeOnce sync.Once
}

// New 装配服务。返回的 App 已经可以处理请求；调用方负责在退出时调用 Close。
//
// 构造顺序与 create_app 一致（app.py:82-147）：先解析开关与鉴权，再建 key pool、
// 指标库、上游客户端与运行时管理器，最后注册路由。
//
// 与参照实现的差别只有错误处理：Python 在构造期抛异常会直接让进程退出，而 Go 侧
// 只有指标库打不开（磁盘不可写）会返回 error，其余构造步骤不会失败。
func New(options Options) (*App, error) {
	if options.Config == nil {
		return nil, errors.New("server: Options.Config 不能为空")
	}
	app := &App{options: options}
	app.configPath = absoluteConfigPath(options.ConfigPath)
	app.configMtime = configMtime(app.configPath)
	app.opsEnabled = options.Config.OpsEnabled
	if options.OpsEnabled != nil {
		app.opsEnabled = *options.OpsEnabled
	}
	app.webuiEnabled = options.Config.WebUIEnabled
	if options.WebUIEnabled != nil {
		app.webuiEnabled = *options.WebUIEnabled
	}
	app.webuiAvailable = webui.Available(options.WebUIAssets)

	keyPool := keypool.New(options.Config,
		keypool.NewCapabilityStore(options.Config.EndpointCapabilitiesPath), nil)
	store, err := metrics.Open(options.Config.MetricsDBPath)
	if err != nil {
		return nil, err
	}
	adapter := newMetricsAdapter(store)
	client := newUpstreamClient(options.Config)
	app.manager = runtime.NewRuntimeManager(
		runtime.NewRuntimeResources(options.Config, keyPool, adapter, client),
	)

	proxyOptions := proxy.Options{Logger: options.Logger}
	if options.Authorizer != nil {
		// 参照实现只有一个 authenticator（app.state.authenticator），proxy 也走它
		// （proxy_handler.py 的 authorize 读 app.state）。proxy 侧的接缝只关心
		// visitor_only 一个布尔，因此这里做一次收窄转换。
		authorizer := options.Authorizer
		proxyOptions.Authorizer = func(r *http.Request, localAPIKey string) *proxy.AuthorizerResult {
			context := authorizer(r, localAPIKey)
			if context == nil {
				return nil
			}
			return &proxy.AuthorizerResult{VisitorOnly: context.VisitorOnly()}
		}
	}
	app.proxy = proxy.New(app.manager, &switchableSink{app: app}, proxyOptions)

	app.api = &api.Server{
		ConfigPath:    app.configPath,
		Reload:        app.reload,
		CurrentConfig: func() *config.RouterConfig { return app.manager.Current().Config },
		Authorizer:    options.Authorizer,
		CheckUpdate:   options.CheckUpdate,
		KeyStats:      app.keyStats,
		// 运维面（7 条路由）与 /api/tool 的读数。OpsEnabled 为零值时 api 一条 ops
		// 路由都不注册（落进兜底 404），因此**必须**显式传入，否则 /api/logs 等
		// URL 在装配后的服务上完全不可达。
		OpsEnabled:  app.opsEnabled,
		Version:     options.Version,
		WebUIStatus: app.webUIStatus,
	}
	if app.api.CheckUpdate == nil {
		app.api.CheckUpdate = defaultCheckUpdate(options.Version)
	}

	app.handler = app.buildHandler()
	return app, nil
}

// Handler 返回注册了全部路由的 http.Handler。
func (a *App) Handler() http.Handler { return a.handler }

// Manager 暴露运行时管理器，供嵌入方与测试观测（生产代码不需要它）。
func (a *App) Manager() *runtime.RuntimeManager { return a.manager }

// Close 关停服务：先挡住新请求并等待在途请求（含流式）结束，再关闭各代独占的
// 连接池与指标库。
//
// 对应 lifespan 的 finally（app.py:69-78）：先取消指标广播任务，再
// `await runtime_manager.close()`。广播任务未启动（见包文档），因此这里只剩
// manager.Close()。顺序不可交换的理由见 runtime.RuntimeManager.Close 的说明。
func (a *App) Close() error {
	var err error
	a.closeOnce.Do(func() {
		err = a.manager.Close()
	})
	return err
}

// keyStats 对应 management_api 里的 `metrics.key_stats(...)` 调用。
//
// 用**当前代**的指标库：参照实现的管理路由拿的是运行时快照
// （`state.runtime_manager.current.metrics`），热重载换库后应当读新库。
func (a *App) keyStats(modelID, keyName string, hours *float64) (*canonical.Value, error) {
	adapter := a.currentMetricsAdapter()
	if adapter == nil {
		return nil, errors.New("server: 指标库未装配")
	}
	return adapter.store.KeyStats(modelID, keyName, hours)
}

// currentMetricsAdapter 返回**当前代**的指标适配器。
//
// proxy.New 只接受一次性的 MetricsSink（见 internal/proxy/lifecycle.go 的说明），
// 而热重载会换掉指标库；switchableSink 通过这里在每次写入时重新取当前代，行为与
// 参照实现的「从租约里取 metrics」一致。
func (a *App) currentMetricsAdapter() *metricsAdapter {
	if a.manager == nil {
		return nil
	}
	resources := a.manager.Current()
	if resources == nil {
		return nil
	}
	adapter, _ := resources.Metrics.(*metricsAdapter)
	return adapter
}

// webUIStatus 返回 webui_status(app) 的输入（webui.py:48-70）。
//
// `/health` 与 `/api/tool` 共用它，保证两处对同一份状态用同一套语义。
//
// enabled 的取值与参照实现一致：**优先读当前运行时配置**里的 webui_enabled
// （webui.py:73-78 的 _configured），只有拿不到 runtime 快照时才回退到启动时的开关。
// 因此「改了开关但没重启」会表现为 enabled 变了、mounted 没变——这正是运维要找的
// 信号（app.py:82 的注释）。
func (a *App) webUIStatus() health.WebUI {
	enabled := a.webuiEnabled
	if a.manager != nil {
		if resources := a.manager.Current(); resources != nil && resources.Config != nil {
			enabled = resources.Config.WebUIEnabled
		}
	}
	return health.WebUI{
		Available:   a.webuiAvailable,
		Enabled:     enabled,
		Mounted:     a.webuiMounted,
		MountPrefix: a.options.MountPrefix,
	}
}

// switchableSink 把 proxy 的指标写入转发到当前代。
//
// 为什么需要它，而不是直接传某一代的适配器：RuntimeManager 在热重载时会比较
// 「这一代的 Metrics 与其它代是否是同一个对象」来决定能否关闭它
// （runtime/manager.go 的 closeUnused）。如果所有代共用同一个 sink，指标库就永远
// 不会被关闭。因此每一代各自持有 metricsAdapter（可被正确关闭），proxy 侧则拿一个
// 稳定对象在写入时转发。
type switchableSink struct{ app *App }

// Record 实现 proxy.MetricsSink。
func (s *switchableSink) Record(ctx context.Context, record proxy.MetricRecord) error {
	adapter := s.app.currentMetricsAdapter()
	if adapter == nil {
		return nil
	}
	return adapter.Record(ctx, record)
}

// newUpstreamClient 构造访问上游的 HTTP 客户端。
//
// 两个超时的来源不同（见 proxy/retry.go:503-511）：
//   - RequestTimeout（config.request_timeout）是**非流式**整请求超时；
//   - FirstByteTimeout（config.stream_first_byte_timeout）是**流式**等响应头的窗口。
//
// 参照实现把后者的窗口放在 asyncio.timeout 里（proxy_handler.py:530-535），Go 侧
// 由 upstream.Client 承担，因此装配时必须把它一起传进去——否则流式的首字节窗口
// 会退化成「没有窗口」。
func newUpstreamClient(cfg *config.RouterConfig) *upstream.Client {
	return upstream.New(upstream.Config{
		RequestTimeout:   secondsDuration(cfg.RequestTimeout),
		FirstByteTimeout: secondsDuration(cfg.StreamFirstByteTimeout),
	})
}

// secondsDuration 把浮点秒折算成 time.Duration。
//
// 不能直接乘 time.Second：那会把 0.5 秒截断成 0，从而让超时立即触发（同一个坑在
// proxy.secondsToDuration 里有详细说明）。
func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// absoluteConfigPath 复刻 app.py:85-87 的路径归一。
//
// 空路径保持空串（表示不做热重载）；否则转成绝对路径。filepath.Abs 与
// `Path(...).resolve()` 一样会清掉 "." / ".." 与多余的斜杠。
func absoluteConfigPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

// configMtime 对应 app.py:434-440 的 _config_mtime：拿不到时返回零值（等价 0.0）。
func configMtime(path string) time.Time {
	if path == "" {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
