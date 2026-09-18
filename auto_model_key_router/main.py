from __future__ import annotations

import argparse
import sys
from dataclasses import replace
from pathlib import Path

from . import __version__
from .config import DEFAULT_CONFIG_PATH, UNIFIED_MODEL_ID, RouterConfig
from .config_operations import UNIFIED_TARGETS
from .config_service import ConfigService
from .service import background_status_panel, manage_system_service, service_status_panel, start_service_background, start_service_foreground, stop_background_service
from .tui import clear_terminal_history, console, section_panel
from .unified_model import switch_unified_target
from .update import check_latest_version, render_version_check_result, restart_service_after_update, update_latest_version


def router_address_text(config: RouterConfig) -> str:
    host = config.host
    url_host = host
    if ":" in url_host and not url_host.startswith("["):
        url_host = f"[{url_host}]"
    return (
        f"监听 IP: [bold]{host}[/bold]\n"
        f"监听端口: [bold]{config.port}[/bold]\n"
        f"服务地址: [bold]http://{url_host}:{config.port}[/bold]"
    )


def plain_config_summary(config: RouterConfig, config_path: Path) -> str:
    """--show-config 的纯文本摘要。

    原由 ``dashboard.render_config`` 渲染成多块 Rich 面板；``dashboard.py`` 已按决策 7
    删除（终端仪表盘由 WebUI 取代），故退化成纯文本，只保留真正有用的字段。
    这段代码本身也计划随 ``main.py`` 一并删除，不要在此追加功能。
    """
    models = ", ".join(model.id for model in config.models) or "（无）"
    providers = ", ".join(provider.id for provider in config.providers) or "（无）"
    return "\n".join(
        (
            f"配置文件: {config_path}",
            f"监听地址: {config.host}:{config.port}",
            f"模型: {models}",
            f"提供方: {providers}",
            f"WebUI: {'启用' if config.webui_enabled else '关闭'}",
            f"运维接口: {'启用' if config.ops_enabled else '关闭'}",
        )
    )


def plain_unified_model_summary(config: RouterConfig, title: str) -> str:
    """统一模型状态的纯文本摘要（原 dashboard.unified_model_status_panel）。"""
    if config.unified_model is None:
        return f"{title}: 尚未配置 {UNIFIED_MODEL_ID}。请选择已有模型和 Key。"
    lines = [
        f"{title}",
        f"请求模型: {UNIFIED_MODEL_ID}",
        f"目标模型: {config.unified_model.model}",
        f"使用 Key: {config.unified_model.key or '自动路由'}",
    ]
    fallback = config.unified_model.default.fallback
    if fallback:
        lines.append(f"熔断模型: {fallback.model}")
        lines.append(f"熔断 Key: {fallback.key or '自动路由'}")
    for label, plan in (("图像", config.unified_model.image), ("嵌入", config.unified_model.embeddings)):
        if plan is not None:
            lines.append(f"{label}模型: {plan.primary.model}")
            lines.append(f"{label} Key: {plan.primary.key or '自动路由'}")
    return "\n".join(lines)


def main() -> None:
    parser = argparse.ArgumentParser(prog=Path(sys.argv[0]).stem)
    parser.add_argument("--config", default=str(DEFAULT_CONFIG_PATH), help="配置文件路径")
    parser.add_argument("--host", help="覆盖配置中的监听地址")
    parser.add_argument("--port", type=int, help="覆盖配置中的监听端口")
    parser.add_argument("--show-config", action="store_true", help="只展示配置摘要，不启动服务")
    parser.add_argument("--show-address", action="store_true", help="查询 AMKR 的监听 IP、端口和服务地址")
    parser.add_argument(
        "--show-api-key",
        "--get-api-key",
        "--get-key",
        dest="show_api_key",
        action="store_true",
        help="获取当前 AMKR 的本地授权 Key",
    )
    parser.add_argument("--switch-model", metavar="MODEL", help=f"切换 {UNIFIED_MODEL_ID} 指向的已有模型或别名")
    parser.add_argument("--switch-key", metavar="KEY", help=f"切换 {UNIFIED_MODEL_ID} 使用的已有 key；传 auto 恢复自动路由")
    parser.add_argument("--unified-target", choices=list(UNIFIED_TARGETS), default="default.primary", help="选择要修改的 unified 路由目标")
    parser.add_argument("--show-unified-model", action="store_true", help=f"查看 {UNIFIED_MODEL_ID} 当前指向")
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    parser.add_argument("--check-update", action="store_true", help="通过 PyPI/GitHub 检查最新版本")
    parser.add_argument("--update", action="store_true", help="通过 PyPI/GitHub 手动更新到最新版本")
    parser.add_argument("--serve", action="store_true", help="跳过 Terminal UI，后台启动服务")
    parser.add_argument(
        "--webui",
        dest="webui",
        action="store_true",
        default=None,
        help="启用随包发布的 WebUI（写入 webui_enabled，重启服务后生效）",
    )
    parser.add_argument(
        "--no-webui",
        dest="webui",
        action="store_false",
        help="关闭 WebUI（写入 webui_enabled，重启服务后生效）",
    )
    parser.add_argument(
        "--no-ops",
        dest="ops",
        action="store_false",
        default=None,
        help="关闭运维接口 /api/logs、/api/service/*、/api/integrations/*、/api/tool（写入 ops_enabled，重启服务后生效）",
    )
    parser.add_argument("--serve-foreground", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--restart-service-after-update", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--stop", action="store_true", help="停止后台服务")
    parser.add_argument("--status", action="store_true", help="查看后台服务状态")
    parser.add_argument("--install-service", action="store_true", help="注册为 Windows/Linux 内置服务")
    parser.add_argument("--service", choices=["install", "install-user", "uninstall", "start", "stop", "restart", "status", "install-elevated", "uninstall-elevated", "start-elevated", "stop-elevated", "restart-elevated"], help="管理 Windows/Linux 内置服务")
    args = parser.parse_args()

    # Keep machine-readable key output free of terminal control sequences.
    if not args.show_api_key:
        clear_terminal_history()
    if args.check_update:
        console.print(render_version_check_result(check_latest_version(timeout=10.0)))
        return
    config_path = Path(args.config)

    try:
        # WebUI 开关是持久化设置：三个启动路径（前台/后台/系统服务）都读同一份
        # 配置，写成配置项才能保证 --webui 对后台启动也生效。
        if args.webui is not None:
            ConfigService(config_path).update(
                lambda data: data.update(webui_enabled=args.webui)
            )
        # 同理：后台/系统服务启动只带 --config，开关不进配置就会静默失效。
        if args.ops is not None:
            ConfigService(config_path).update(
                lambda data: data.update(ops_enabled=args.ops)
            )
        try:
            config = RouterConfig.load(config_path)
        except Exception as exc:
            console.print(section_panel(f"[red]{exc}[/red]", "配置加载失败", "red"))
            raise SystemExit(1) from exc

        if args.host:
            config = replace(config, host=args.host)
        if args.port:
            config = replace(config, port=args.port)

        if args.switch_model is not None or args.switch_key is not None:
            try:
                config = switch_unified_target(
                    config_path,
                    args.unified_target,
                    args.switch_model,
                    None if args.switch_key == "auto" else args.switch_key,
                    update_key=args.switch_key is not None,
                )
            except (OSError, ValueError) as exc:
                console.print(section_panel(f"[red]{exc}[/red]", "统一模型切换失败", "red"))
                raise SystemExit(1) from exc
            # 面板必须报整份配置：目标不止 default 一个，只打印 default 会在
            # `--unified-target embeddings.primary` 时显示一份根本没变过的计划。
            console.print(plain_unified_model_summary(config, "统一模型已切换"))
            return
        if args.show_api_key:
            print(config.local_api_key)
            return
        if args.show_unified_model:
            console.print(plain_unified_model_summary(config, "统一模型"))
            return

        if args.update:
            console.print(update_latest_version(timeout=10.0, config_path=config_path))
            return
        if args.restart_service_after_update:
            result = restart_service_after_update(config_path)
            if result is not None:
                console.print(result)
            return

        if args.show_address:
            console.print(section_panel(router_address_text(config), "AMKR 地址", "cyan"))
            return
        if args.show_config:
            print(plain_config_summary(config, config_path))
            return
        if args.stop:
            console.print(stop_background_service(config))
            return
        if args.status:
            console.print(background_status_panel(config, config_path))
            return
        if args.install_service:
            console.print(manage_system_service(config_path, "install"))
            return
        if args.service:
            result = service_status_panel(config, config_path) if args.service == "status" else manage_system_service(config_path, args.service)
            console.print(result)
            return
        if args.serve_foreground:
            start_service_foreground(config_path, config)
            return
        if not args.serve:
            # 无参数默认动作：此前是进 dashboard.run_terminal_ui 的终端界面，而 dashboard.py
            # 已按决策 7 删除（终端界面由 WebUI 取代）。默认改为**前台启动服务**，与 Go 版
            # cmd/amkr 的 defaultCommand 保持一致。
            start_service_foreground(config_path, config)
            return
        console.print(start_service_background(config_path, config))
    except KeyboardInterrupt:
        clear_terminal_history()
        raise SystemExit(130)


if __name__ == "__main__":
    main()
