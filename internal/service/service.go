package service

import (
	"fmt"

	"github.com/Sparrived/auto-model-key-router/internal/api"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// 本文件移植 service.py:350-360 的平台分派，并实现 internal/api 的
// Server.RunServiceAction 接缝。

// ManageSystemService 对应 service.py:350 的 manage_system_service。
//
// 与 Python 一致：先取绝对路径与可执行文件，再按平台分派；不支持的系统给「暂不支持」
// 面板（而不是错误）。
func (e *Env) ManageSystemService(configPath, action string) (tui.Renderable, error) {
	absoluteConfig := absPath(configPath)
	executable := e.BackgroundExecutable()
	switch e.GOOS {
	case "windows":
		return e.ManageWindowsTask(executable, absoluteConfig, action)
	case "linux":
		return e.ManageSystemdUserService(executable, absoluteConfig, action)
	default:
		return tui.SectionPanel(
			fmt.Sprintf("暂不支持当前系统自动注册: %s", displaySystemName(e.GOOS)),
			"系统服务", "yellow"), nil
	}
}

// RunServiceAction 是 api.Server.RunServiceAction 接缝的实现入口。
//
// 签名与 internal/api/server.go 的
//
//	RunServiceAction func(action string, configPath string, cfg *config.RouterConfig) (string, error)
//
// 完全一致，接线只需 `server.Options{...}` 之外的一行赋值：
//
//	srv.RunServiceAction = service.RunServiceAction
//
// 分派表直接用 api.OpsServiceTargets（它逐条对应 ops_api.py:50-62），因此**不会**
// 出现「Python 抄一遍、Go 再抄一遍」的双份真相。返回的是 rt 渲染后的纯文本，
// 对应 ops_api.py:103 的 _run_service_action。
//
// 未知动作返回的错误文案与 api handler 在调用本函数**之前**产出的 422 校验文案一致
// （`不支持的服务动作: <action>`）；HTTP 状态码由 handler 决定，本函数只负责让直接
// 调用方也拿到同一句话（见 TestRunServiceActionUnknownActionMatchesAPIMessage）。
func RunServiceAction(action string, configPath string, cfg *config.RouterConfig) (string, error) {
	return DefaultEnv().RunServiceAction(action, configPath, cfg)
}

// RunServiceAction 是 Env 上的实现体（可注入接缝，便于测试）。
func (e *Env) RunServiceAction(action string, configPath string, cfg *config.RouterConfig) (string, error) {
	target, supported := api.OpsServiceTargets[action]
	if !supported {
		return "", fmt.Errorf("不支持的服务动作: %s", action)
	}
	switch target.Kind {
	case "background-start":
		panel, err := e.StartBackground(configPath, cfg)
		if err != nil {
			return "", err
		}
		return RenderText(panel), nil
	case "background-stop":
		return RenderText(e.StopBackground(cfg)), nil
	case "background-restart":
		// ops_api.py:112-115：先停后启，两段文本用空行连接，空段被过滤掉。
		stopped := RenderText(e.StopBackground(cfg))
		started, err := func() (string, error) {
			panel, err := e.StartBackground(configPath, cfg)
			if err != nil {
				return "", err
			}
			return RenderText(panel), nil
		}()
		if err != nil {
			return "", err
		}
		return joinNonEmpty([]string{stopped, started}, "\n\n"), nil
	default:
		panel, err := e.ManageSystemService(configPath, target.Argument)
		if err != nil {
			return "", err
		}
		return RenderText(panel), nil
	}
}

// joinNonEmpty 复刻 `"\n\n".join(part for part in parts if part)`。
func joinNonEmpty(parts []string, separator string) string {
	out := ""
	first := true
	for _, part := range parts {
		if part == "" {
			continue
		}
		if !first {
			out += separator
		}
		out += part
		first = false
	}
	return out
}
