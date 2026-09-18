package service

import (
	"net"
	"strconv"

	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/selfupdate"
)

// 本文件是自更新收尾所需的「按现有注册状态停/启服务」。
//
// 它放在 internal/service 而不是 internal/selfupdate：判断「注册成了什么、该怎么拉起」
// 需要平台知识（Windows 计划任务 / systemd user unit / 后台 PID 文件），而这正是本包
// 已有的能力（IsSystemServiceRegistered、ManageSystemService、StartBackground）。
// internal/selfupdate 因此保持零平台知识。

// BackgroundServiceRunning 报告是否存在活着的后台服务（PID 文件 + 进程存活）。
func (e *Env) BackgroundServiceRunning(cfg *config.RouterConfig) (int, bool) {
	pid, ok := ReadPid(PidFilePath(cfg))
	if !ok {
		return 0, false
	}
	if !e.IsProcessRunning(pid) {
		return pid, false
	}
	return pid, true
}

// BackgroundServiceRegistered 报告是否以「后台服务」形态管理过（只看 PID 文件是否登记）。
//
// 与 BackgroundServiceRunning 的区别是**不看进程是否还活着**，这正是重启时需要的那一问：
// 停止之后进程已经退出，但「这台机器原本是后台服务形态」这个事实仍由 PID 文件记载。
// 若用 Running 判断，服务停完后就认不出形态，于是永远不会重启（这是一个真实的坑）。
func (e *Env) BackgroundServiceRegistered(cfg *config.RouterConfig) (int, bool) {
	return ReadPid(PidFilePath(cfg))
}

// ServiceInstanceRunning 报告这台机器上当前是否有一个 amkr 实例在服务。
//
// 这是**更新前**的判断（此刻服务通常还活着），因此三种证据都用上，任一成立即算在跑：
//
//  1. 注册过系统服务。**注意这台机器上的实际限制**：非提权进程查询 SYSTEM 计划任务会被
//     拒绝（`schtasks /Query` 三种形态都回 "Access is denied"，已实测），因此普通用户跑
//     `amkr --update` 时这一条恒为假。
//  2. 后台服务（PID 文件 + 进程存活）。
//  3. **/health 探针**。这是非提权场景下唯一能发现 SYSTEM 任务实例的手段（它监听的是
//     本机端口，不需要任何特权）。没有它，`amkr --update` 会误判"没有实例在跑"，
//     于是既不停也不重启，还把"服务将自动重启"报给用户——那是不诚实的。
//
// 与「是否已停止」的判据**必须分开**（见 ServicePortReleased）：那个判断发生在更新后，
// 此刻 /health 必然失败，"探针失败=已停止"会把「刚开始关停、还在处理在途请求」误判成
// "已经停完"。
func (e *Env) ServiceInstanceRunning(configPath string, cfg *config.RouterConfig) bool {
	if e.IsSystemServiceRegistered(configPath) {
		return true
	}
	if _, running := e.BackgroundServiceRunning(cfg); running {
		return true
	}
	// useCache=false：这是更新前的关键判定，不能吃上一次探测的缓存。
	return e.IsServiceHealthy(cfg.Host, cfg.Port, false)
}

// CanRestartService 报告收尾助手是否有能力停掉并重新拉起当前实例。
//
// 为什么需要这一问，而不是「尽力而为」：本机实测的限制是——服务以 SYSTEM 计划任务运行
// 时，非提权进程连 `schtasks /Query` 都会被拒（三种形态都回 "Access is denied"），也就
// 既查不到注册、也停不掉它。此时若还向用户承诺"服务将自动重启"，就是在说假话：助手会
// 等端口释放直到超时，而那个端口永远不会释放（旧 SYSTEM 进程仍持有它）。
//
// 判据：
//   - 注册信息可读（systemd user unit / 可见的计划任务）→ 能停能起。
//   - 有 PID 文件（后台服务形态）→ 能停能起。
//   - 服务根本没在跑 → 无需重启，算「能」（助手只会清理旧文件）。
//   - 服务在跑但上面两条都不成立 → 只可能是查不到的 SYSTEM 任务，**不能**自动重启。
func (e *Env) CanRestartService(configPath string, cfg *config.RouterConfig) bool {
	if e.IsSystemServiceRegistered(configPath) {
		return true
	}
	if _, registered := e.BackgroundServiceRegistered(cfg); registered {
		return true
	}
	return !e.IsServiceHealthy(cfg.Host, cfg.Port, false)
}

// CanRestartServiceWith 与 CanRestartService 相同，但由调用方提供 Env（测试注入用）。
func CanRestartServiceWith(env *Env, configPath string, cfg *config.RouterConfig) bool {
	if cfg == nil {
		return false
	}
	return env.CanRestartService(configPath, cfg)
}

// StopRegisteredService 停掉服务（停止方式与注册形态对应）。
func (e *Env) StopRegisteredService(configPath string, cfg *config.RouterConfig) error {
	if e.IsSystemServiceRegistered(configPath) {
		_, err := e.ManageSystemService(configPath, "stop")
		return err
	}
	e.StopBackground(cfg)
	return nil
}

// StartRegisteredService 按**当前的注册状态**把服务重新拉起来。
//
// 三种形态：
//
//  1. 注册过系统服务 → ManageSystemService(configPath, "start")。这不仅是最正确的拉起
//     方式，也是**唯一**能保证与注册时的命令行一致的方式（计划任务的 /TR 里含绝对路径
//     与 --serve-foreground）。
//  2. 登记过后台服务（**有 PID 文件即可，不要求进程还活着**——调用本函数时旧进程通常
//     刚被停掉，用 Running 判断会导致永远不重启）→ StartBackground，与 `amkr --serve`
//     一致。
//  3. 两者都不是，但**更新前**有实例在服务（例如 SYSTEM 计划任务在非提权视角下查不到
//     注册信息）→ 仍用 StartBackground 拉起。这一条是为上面 ServiceInstanceRunning 里
//     说的限制兜底：宁可多起一个后台服务，也不要让用户在更新后面对一个静默消失的服务。
//  4. 都不是（纯前台手工启动）→ 什么都不做。用户自己的终端会话已经结束，替他后台
//     拉起一个服务反而是意外行为。
func (e *Env) StartRegisteredService(configPath string, cfg *config.RouterConfig, wasRunning bool) error {
	if e.IsSystemServiceRegistered(configPath) {
		_, err := e.ManageSystemService(configPath, "start")
		return err
	}
	if _, registered := e.BackgroundServiceRegistered(cfg); registered || wasRunning {
		_, err := e.StartBackground(configPath, cfg)
		return err
	}
	return nil
}

// ServicePortReleased 报告服务端口是否已经可以重新绑定（即旧实例真的让出了位置）。
//
// 判据是「能否独占绑定」，而不是「/health 是否失败」：后者在旧进程刚开始关停、仍在处理
// 在途请求时就是失败的，会把「还没停完」误判成「已经停了」。绑定成功才是启动新版本
// 真正需要的前提。绑定成功后立刻释放，不会影响紧随其后的启动。
func ServicePortReleased(cfg *config.RouterConfig) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// SelfUpdateHooks 把本包的能力适配成 internal/selfupdate 的助手依赖。
//
// 签名逐字对齐 selfupdate.HelperHooks 的字段类型，因此接线是直接赋值（编译期由
// selfupdate 的字段类型兜住，签名漂移会在赋值处报错）。
func SelfUpdateHooks(configPath string, cfg *config.RouterConfig) selfupdate.HelperHooks {
	return SelfUpdateHooksWith(DefaultEnv(), configPath, cfg)
}

// SelfUpdateHooksWith 与 SelfUpdateHooks 相同，但由调用方提供 Env（测试注入用）。
//
// running 与 start 都**捕获更新前**的形态：StartRegisteredService 需要在停止之后仍能
// 认出「原本是后台服务」，而停止会移走 PID 文件并杀掉进程——那时再判断就什么都查不到了。
func SelfUpdateHooksWith(env *Env, configPath string, cfg *config.RouterConfig) selfupdate.HelperHooks {
	// 更新前的形态快照，供停止之后的重启复用。
	registered := env.IsSystemServiceRegistered(configPath)
	_, hadPidFile := env.BackgroundServiceRegistered(cfg)
	healthy := env.IsServiceHealthy(cfg.Host, cfg.Port, false)
	running := registered || hadPidFile || healthy

	return selfupdate.HelperHooks{
		Running: running,
		Stop: func() error {
			return env.StopRegisteredService(configPath, cfg)
		},
		Stopped: func() bool {
			return ServicePortReleased(cfg)
		},
		Sleep: env.Sleep,
		Start: func() error {
			// 重新判断一次注册状态：更新前查不到 SYSTEM 任务（非提权时被拒），
			// 但这不影响正确性——那种情况走下面的 wasRunning 兜底。
			return env.StartRegisteredService(configPath, cfg, running)
		},
	}
}
