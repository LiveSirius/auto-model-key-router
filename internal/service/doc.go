// Package service 移植 auto_model_key_router/service.py：进程/系统服务管理与状态呈现。
//
// # 覆盖范围
//
//   - 后台启动与停止（分离进程 + PID 文件 + 日志轮转）；
//   - Windows 计划任务（schtasks）与 Linux systemd user unit 的注册/注销/启停；
//   - 运行状态面板（本地 /health + 系统服务注册状态）；
//   - api.Server.RunServiceAction 接缝的实现（见 RunServiceAction）。
//
// 状态**解析**那一半在 internal/servicestatus 里已经移植并语料锁定（schtasks XML、
// systemd unit、systemctl show 属性），本包只调用它、绝不重写。
//
// # OS 接缝（为什么必须存在）
//
// 这个模块会真的安装计划任务、写 systemd unit、杀进程。测试**绝不能**在开发机上
// 产生这些副作用，因此所有 OS 交互都收敛到 Env 的注入点：
//
//	Env.Run       命令执行（servicestatus.CommandRunner，捕获 stdout/stderr）
//	Env.Spawn     分离进程启动（后台服务）
//	Env.Running   进程存活查询
//	Env.Terminate 终止进程
//	Env.IsAdmin   管理员权限判定（Windows UAC 分支）
//	Env.Get       /health HTTP 取回
//	Env.LookPath  PATH 查找（console script）
//	Env.Sleep/Now 轮询与缓存时钟
//
// 只有 fake 覆盖的路径（无法对拍、也无法在真机上安全执行）：
//
//   - defaultSpawn 真正拉起分离进程（spawn_windows.go / spawn_posix.go 的平台标志）；
//   - posixRunning / posixTerminate（真实 syscall.Kill）；
//   - isUserAnAdminWindows（真实 shell32!IsUserAnAdmin）；
//   - Env.Terminate 在 Windows 分支真正执行的 taskkill。
//
// 它们的**决策部分**（命令、参数、标志位取值、CSV 匹配规则）都有对拍语料或具名测试。
//
// # 与参照实现的结构化差异（逐条在对应位置说明）
//
//  1. **没有 Python 解释器**。service.py 的后台命令是 `[pythonw, -m, module, ...]`，
//     Go 侧是 `[自身可执行文件, --config, ..., --serve-foreground]`（divergence_test.go
//     的 TestBackgroundExecutableDivergesFromPython 具名锁定）。
//  2. **没有 uvicorn 日志配置**。service.py:682 的 uvicorn_log_config 是 uvicorn 专用
//     的 logging dictConfig，Go 侧没有 logging，因此改由 internal/logfiles 的
//     slog.Handler 承担同一件事：按 `%(asctime)s %(levelname)s %(name)s %(message)s`
//     的行格式把日志追加进同一个 log_file_path（另见 logfiles 对「不提升非成功响应
//     等级」等差异的说明）。**落点是同一个文件**，所以运维接口 /api/logs 读到的内容
//     与参照实现同源。
//     本包（后台启动）在此之外仍把子进程的 stdout/stderr 追加进同一个日志文件——这
//     是 Python `subprocess.Popen(stdout=log_file, stderr=log_file)` 的对应物，负责
//     兜住「进程在日志文件打开之前就崩溃」这类服务端日志覆盖不到的输出。
//     注意 Windows 计划任务与 systemd 路径**没有**这层重定向（TaskActionCommandLine
//     与 UnitText 都不设），因此这两条路径下的日志完全由服务进程自己写入——这正是
//     迁移时漏掉 file handler 会让 server.log 恒为空的原因。
//  3. **rich 渲染降级为等价纯文本**。面板统一由 internal/tui 渲染（宽度 100、
//     safe_box=False，与 gen_tui_corpus.py（已随 Python 退役移除） 同一套设置），因此语料可以逐字节
//     对拍；唯一例外是系统服务状态表——Go 用固定列宽（tui.Table 已声明的差异），
//     语料只对拍**行数据**，不对拍表格版式。
package service
