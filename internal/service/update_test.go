package service

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件覆盖自更新收尾用到的「按注册状态停/启」判定。
//
// 这里的每条断言都对应一个写实现时踩到或推理出的坑，而不是为了覆盖率：
//   - 停止之后 PID 文件与进程都会消失，「原先是后台服务」必须靠更新前的快照记住，
//     否则更新完永远不会重启（服务静默消失）。
//   - 非提权进程查不到 SYSTEM 计划任务，此时既不能谎称会自动重启，也不该什么都不做。

// updateTestEnv 造一个不发真实命令的 Env。
type updateTestEnv struct {
	env *Env
	// pidLive 决定 PID 文件里的进程是否算存活。
	pidLive bool
	// healthy 决定 /health 探针的答案。
	healthy bool
}

func newUpdateTestEnv(t *testing.T) *updateTestEnv {
	t.Helper()
	fixture := &updateTestEnv{pidLive: true}
	env := DefaultEnv()
	env.Running = func(int) bool { return fixture.pidLive }
	env.Sleep = func(time.Duration) {}
	env.Get = func(string, time.Duration) (int, []byte, error) {
		if fixture.healthy {
			return 200, []byte(`{"status":"ok"}`), nil
		}
		return 0, nil, os.ErrDeadlineExceeded
	}
	// 清理可能被上一个用例污染的探针缓存。
	env.cacheMu.Lock()
	env.healthCache = nil
	env.cacheMu.Unlock()
	fixture.env = env
	return fixture
}

// updateTestConfig 造一份指向临时目录的配置（端口用不可达值，避免真实网络）。
func updateTestConfig(t *testing.T) (string, *config.RouterConfig) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.RouterConfig{
		Host:        "127.0.0.1",
		Port:        1,
		LogFilePath: filepath.Join(dir, "server.log"),
	}
	return filepath.Join(dir, "router-config.json"), cfg
}

// TestBackgroundServiceRegisteredIgnoresProcessLiveness 锁定这个区别：
// 「登记过后台服务」只看 PID 文件在不在，不看进程是否还活着。
//
// 这正是重启时需要的那一问：停止旧服务之后进程已经退出，但「这台机器原本是后台服务
// 形态」这个事实仍由 PID 文件记载。若用 Running 判断，服务停完后就认不出形态，
// 于是永远不会重启。
func TestBackgroundServiceRegisteredIgnoresProcessLiveness(t *testing.T) {
	fixture := newUpdateTestEnv(t)
	_, cfg := updateTestConfig(t)

	// 没有 PID 文件 → 未登记、也没在跑。
	if _, registered := fixture.env.BackgroundServiceRegistered(cfg); registered {
		t.Error("没有 PID 文件时不该报告已登记")
	}
	if _, running := fixture.env.BackgroundServiceRunning(cfg); running {
		t.Error("没有 PID 文件时不该报告在运行")
	}

	// 写下 PID 文件，但进程已退出（pidLive=false）。
	if err := os.WriteFile(PidFilePath(cfg), []byte("999999"), 0o666); err != nil {
		t.Fatal(err)
	}
	fixture.pidLive = false
	if _, registered := fixture.env.BackgroundServiceRegistered(cfg); !registered {
		t.Error("有 PID 文件时应当报告已登记（即使进程已退出）")
	}
	if _, running := fixture.env.BackgroundServiceRunning(cfg); running {
		t.Error("进程已退出时不该报告在运行")
	}

	// 进程也活着 → 两个都真。
	fixture.pidLive = true
	if _, running := fixture.env.BackgroundServiceRunning(cfg); !running {
		t.Error("PID 文件 + 进程存活时应当报告在运行")
	}
}

// TestServicePortReleased 锁定「端口能否独占绑定」这个判据。
func TestServicePortReleased(t *testing.T) {
	// 占住一个端口，让它必然不可绑定。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	busy := &config.RouterConfig{Host: "127.0.0.1", Port: port}
	if ServicePortReleased(busy) {
		t.Error("端口被占用时应报告未释放")
	}

	// 关掉之后应当报告已释放（同一端口可重新绑定）。
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if !ServicePortReleased(busy) {
		t.Error("端口空闲时应报告已释放")
	}
}

// TestStartRegisteredServiceUsesBackgroundWhenWasRunning 是回归断言，钉住修掉的那个坑：
//
// 服务停掉之后 PID 文件已被移走、进程也没了，此时若只看「当前是否有活着的后台服务」
// 就会什么都不做——更新完服务静默消失。wasRunning 快照必须让它仍然被拉起来。
func TestStartRegisteredServiceUsesBackgroundWhenWasRunning(t *testing.T) {
	fixture := newUpdateTestEnv(t)
	configPath, cfg := updateTestConfig(t)

	// 关键前提：此刻既没有 PID 文件、也没有活着的服务（模拟"刚被停掉"）。
	if _, registered := fixture.env.BackgroundServiceRegistered(cfg); registered {
		t.Fatal("前提不成立：不该有 PID 文件")
	}

	spawned := 0
	fixture.env.Spawn = func(SpawnSpec) (SpawnResult, error) {
		spawned++
		return SpawnResult{Pid: 1234}, nil
	}

	// wasRunning=true 时必须仍然尝试拉起。
	if err := fixture.env.StartRegisteredService(configPath, cfg, true); err != nil {
		t.Fatalf("StartRegisteredService 报错: %v", err)
	}
	if spawned == 0 {
		t.Error("wasRunning=true 时应当把服务重新拉起来（否则服务会静默消失）")
	}

	// 对照：wasRunning=false 且没有注册痕迹时什么都不做（纯前台手工启动的情形）。
	//
	// 必须换一份干净配置：上一次调用已经写下了 PID 文件，那台机器上"M 已经注册成
	// 后台服务"是事实，再调就不该什么都不做了。
	spawned = 0
	cleanConfigPath, cleanCfg := updateTestConfig(t)
	if _, registered := fixture.env.BackgroundServiceRegistered(cleanCfg); registered {
		t.Fatal("前提不成立：干净配置不该有 PID 文件")
	}
	if err := fixture.env.StartRegisteredService(cleanConfigPath, cleanCfg, false); err != nil {
		t.Fatalf("StartRegisteredService 报错: %v", err)
	}
	if spawned != 0 {
		t.Error("既没注册过也没在运行过时不该凭空拉起一个服务")
	}
}

// TestCanRestartService 锁定「能不能自动重启」的判据。
//
// 最关键的一条：服务在跑、但注册信息查不到（非提权看 SYSTEM 计划任务就是如此）时必须
// 报**不能**——否则就是向用户承诺一件做不到的事，助手会等端口释放直到超时。
func TestCanRestartService(t *testing.T) {
	configPath, cfg := updateTestConfig(t)

	// 没有任何注册信息、服务也没在跑 → 无需重启，算「能」（助手只会清理旧文件）。
	idle := newUpdateTestEnv(t)
	idle.healthy = false
	if !idle.env.CanRestartService(configPath, cfg) {
		t.Error("服务没在跑时应当报告可以（无需重启）")
	}

	// 有 PID 文件（后台服务形态）→ 能。
	if err := os.WriteFile(PidFilePath(cfg), []byte("999999"), 0o666); err != nil {
		t.Fatal(err)
	}
	if !idle.env.CanRestartService(configPath, cfg) {
		t.Error("有 PID 文件时应当报告可以自动重启")
	}
	if err := os.Remove(PidFilePath(cfg)); err != nil {
		t.Fatal(err)
	}

	// 没注册信息、没 PID 文件，但 /health 说服务在跑 → 只可能是查不到的 SYSTEM 任务，
	// 不能自动重启。
	busy := newUpdateTestEnv(t)
	busy.healthy = true
	if !busy.env.IsServiceHealthy(cfg.Host, cfg.Port, false) {
		t.Fatal("夹具前提不成立：/health 应当报告健康")
	}
	if busy.env.CanRestartService(configPath, cfg) {
		t.Error("服务在跑但注册信息不可读时，必须报告不能自动重启（否则是空头承诺）")
	}
}

// TestServiceInstanceRunningUsesHealthProbe 锁定更新前的「有没有实例在跑」判定：
// 非提权场景下 /health 是唯一能发现 SYSTEM 任务实例的手段。
//
// 没有它，`amkr --update` 会误判「没有实例在跑」，于是既不停也不重启，还把「服务将
// 自动重启」报给用户——那是不诚实的。
func TestServiceInstanceRunningUsesHealthProbe(t *testing.T) {
	configPath, cfg := updateTestConfig(t)

	fixture := newUpdateTestEnv(t)
	fixture.healthy = true
	if !fixture.env.ServiceInstanceRunning(configPath, cfg) {
		t.Error("/health 健康时应当报告有实例在服务（PID 文件查不到 SYSTEM 任务）")
	}

	// 对照：探针不健康、也没有 PID 文件 → 没有实例。
	idle := newUpdateTestEnv(t)
	idle.healthy = false
	if idle.env.ServiceInstanceRunning(configPath, cfg) {
		t.Error("探针不健康且无 PID 文件时不该报告有实例")
	}

	// 有 PID 文件（即使进程死了，登记本身也是证据）→ 有实例。
	if err := os.WriteFile(PidFilePath(cfg), []byte("999999"), 0o666); err != nil {
		t.Fatal(err)
	}
	if !idle.env.ServiceInstanceRunning(configPath, cfg) {
		t.Error("有 PID 文件登记时应当报告有实例")
	}
}

// TestSelfUpdateHooksCapturePreStopState 锁定钩子捕获的是**更新前**的形态。
//
// 停止之后 PID 文件与进程都没了，Start 里若现查必然查不到，于是服务不会被重启。
func TestSelfUpdateHooksCapturePreStopState(t *testing.T) {
	fixture := newUpdateTestEnv(t)
	configPath, cfg := updateTestConfig(t)
	if err := os.WriteFile(PidFilePath(cfg), []byte("999999"), 0o666); err != nil {
		t.Fatal(err)
	}

	hooks := SelfUpdateHooksWith(fixture.env, configPath, cfg)
	if !hooks.Running {
		t.Error("有 PID 文件时应当报告更新前有实例在跑")
	}

	spawned := 0
	fixture.env.Spawn = func(SpawnSpec) (SpawnResult, error) {
		spawned++
		return SpawnResult{Pid: 4321}, nil
	}

	// 模拟"旧服务已经停掉"：PID 文件被移走、进程也没了。
	if err := os.Remove(PidFilePath(cfg)); err != nil {
		t.Fatal(err)
	}
	fixture.pidLive = false

	if err := hooks.Start(); err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	if spawned == 0 {
		t.Error("Start 必须凭更新前的快照认出后台服务形态并把它拉起来")
	}
}
