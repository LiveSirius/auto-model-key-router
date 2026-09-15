from __future__ import annotations

import argparse
import fnmatch
import os
import re
import shlex
import shutil
import subprocess
import sys
import time
import tomllib
from dataclasses import dataclass
from datetime import date
from pathlib import Path
from typing import Sequence

from rich import box
from rich.console import Console
from rich.panel import Panel
from rich.table import Table
from rich.text import Text


def make_output_utf8_safe() -> None:
    """放开发布脚本输出流的编码错误处理。

    中文 Windows 控制台默认 GBK，Rich 打印 ✓ / ℹ / ⚠ 时会抛
    UnicodeEncodeError 并中断发布（改完版本号之后、提交之前）。
    这里只放开错误处理，不强制改写编码：UTF-8 终端不受影响，
    GBK 终端退化成 "?" 而不是崩溃。
    """
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if reconfigure is None:
            continue  # pytest 等替换流没有 reconfigure
        try:
            reconfigure(errors="replace")
        except (OSError, ValueError):
            pass


make_output_utf8_safe()


ROOT = Path(__file__).resolve().parents[1]
PYPROJECT_PATH = ROOT / "pyproject.toml"
CHANGELOG_PATH = ROOT / "CHANGELOG.md"
UV_LOCK_PATH = ROOT / "uv.lock"
console = Console()
error_console = Console(stderr=True)
VERSION_PATTERN = re.compile(r"^(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)(?:(?P<pre>a|b|rc)(?P<pre_num>\d+))?(?:\.post(?P<post>\d+))?(?:\.dev(?P<dev>\d+))?$")
RELEASE_TYPES = ("patch", "minor", "major", "post", "preview", "alpha", "beta", "dev", "stable", "custom")
RELEASE_LABELS = {
    "patch": "小版本 patch",
    "minor": "中版本 minor",
    "major": "大版本 major",
    "post": "post 版本",
    "preview": "preview 版本 rc",
    "alpha": "alpha 预览版本",
    "beta": "beta 预览版本",
    "dev": "dev 开发版本",
    "stable": "当前预览转正式版",
    "custom": "自定义版本号",
}
STEP_TITLES = {
    "install": "准备发布环境",
    "tests": "运行测试",
    "build": "构建分发产物",
    "twine": "校验分发产物",
    "git": "检查 Git 改动",
    "commit": "创建提交和标签",
    "push": "推送远端仓库",
}
SENSITIVE_PATTERNS = (
    ".env",
    ".env.*",
    ".last_config_path",
    "router-config.json",
    "data/*",
    "*.sqlite3",
    "*.sqlite3-wal",
    "*.sqlite3-shm",
    "*.log",
)
# setuptools 写 PKG-INFO 用 NamedTemporaryFile + os.replace（_core_metadata.py），
# Windows 上目标文件被杀软扫描或未释放句柄短暂占用时会抛 WinError 5；pip 装可编辑
# 包（`pip install -e`）与 `python -m build` 都会走到这里，属平台噪声而非真实错误。
# 用带方括号的写法匹配：错误正文是本地化的（中文 Windows 下没有 "Access is denied"），
# 只有 "[WinError 5]" 这段 ASCII 稳定出现，且不会误命中 "[WinError 50]"。
TRANSIENT_FILE_LOCK_MARKERS = (
    "[WinError 5]",
    "[WinError 32]",
)
# 推送/拉取远端时的连接类失败。代理异常与网络抖动都落在这里：前者先绕过代理，
# 后者靠退避重试。注意 schannel 的握手失败报文里既没有 "proxy" 也没有 "127.0.0.1"，
# 只按代理关键字判断会漏掉它 —— 这正是发布卡在最后一步的原因。
CONNECTIVITY_FAILURE_MARKERS = (
    "proxy",
    "127.0.0.1",
    "schannel",
    "ssl",
    "tls",
    "handshake",
    "unable to access",
    "failed to connect",
    "connection refused",
    "connection reset",
    "connection timed out",
    "could not resolve host",
    "operation timed out",
)
# 推送的整体尝试次数（每次都可能再补一次"绕过代理"）。
PUSH_ATTEMPTS = 3


def _git_failure_text(result: subprocess.CompletedProcess[str] | None) -> str:
    if result is None:
        return ""
    return f"{result.stdout or ''}\n{result.stderr or ''}".lower()


def info(message: str) -> None:
    console.print(f"[cyan]ℹ[/cyan] {message}")


def success(message: str) -> None:
    console.print(f"[green]✓[/green] {message}")


def warning(message: str) -> None:
    console.print(f"[yellow]⚠[/yellow] {message}")


def step(title: str) -> None:
    console.rule(f"[bold cyan]{title}[/bold cyan]", style="cyan")


def print_error(message: str) -> None:
    console.print(Panel(f"[red]{message}[/red]", title="[bold red]发布已中止[/bold red]", border_style="red", box=box.ROUNDED))


def input_text(prompt: str) -> str:
    return input(f"{prompt} ")


@dataclass(frozen=True)
class ParsedVersion:
    major: int
    minor: int
    patch: int
    pre: str | None = None
    pre_num: int | None = None
    post: int | None = None
    dev: int | None = None

    @classmethod
    def parse(cls, value: str) -> "ParsedVersion":
        match = VERSION_PATTERN.fullmatch(value.strip())
        if not match:
            raise ValueError(f"不支持的版本号格式: {value}")
        groups = match.groupdict()
        return cls(
            major=int(groups["major"]),
            minor=int(groups["minor"]),
            patch=int(groups["patch"]),
            pre=groups["pre"],
            pre_num=int(groups["pre_num"]) if groups["pre_num"] is not None else None,
            post=int(groups["post"]) if groups["post"] is not None else None,
            dev=int(groups["dev"]) if groups["dev"] is not None else None,
        )

    @property
    def base(self) -> str:
        return f"{self.major}.{self.minor}.{self.patch}"

    @property
    def normalized(self) -> str:
        value = self.base
        if self.pre and self.pre_num is not None:
            value += f"{self.pre}{self.pre_num}"
        if self.post is not None:
            value += f".post{self.post}"
        if self.dev is not None:
            value += f".dev{self.dev}"
        return value


def format_command(args: Sequence[str]) -> str:
    if os.name == "nt":
        return subprocess.list2cmdline(list(args))
    return shlex.join(args)


def remove_stale_egg_info() -> None:
    """构建或可编辑安装前清掉旧的 egg-info。

    setuptools 写 PKG-INFO 用 NamedTemporaryFile + os.replace，目标文件已存在且被杀软
    或索引器扫到时会抛 WinError 5。目标不存在时 os.replace 只做创建，基本撞不上锁 ——
    与其失败后重试，不如先让这个竞态不具备发生的条件。egg-info 是构建产物，随时会重建。
    """
    for path in sorted(ROOT.glob("*.egg-info")):
        shutil.rmtree(path, ignore_errors=True)


def _run_with_tee(args: Sequence[str], *, cwd: Path) -> subprocess.CompletedProcess[str]:
    """执行命令，并把输出同时实时回显与留存。

    subprocess.run 只能二选一：capture=True 拿得到输出却没有实时进度，capture=False
    反之。而"命令是否被 Windows 文件占用拒绝"必须在输出里找 [WinError 5]，于是流式
    执行的命令永远判定不出该重试 —— `python -m build` 就是这样一步步失败却从不重试的。
    这里把 stderr 并入 stdout 逐行读取（只读一个管道，避免两个管道互相阻塞），
    兼顾进度可见与输出可查。子进程是 Python（pip / build），设置 PYTHONUNBUFFERED
    让它的 stdout 逐行落盘 —— 否则 pip 的 stdout 是块缓冲、stderr 是无缓冲，
    错误会跑到逻辑上在它之前的那几行前面，读起来像错位。
    """
    process = subprocess.Popen(
        args,
        cwd=cwd,
        text=True,
        encoding="utf-8",
        errors="replace",
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        bufsize=1,
        env={**os.environ, "PYTHONUNBUFFERED": "1"},
    )
    chunks: list[str] = []
    stream = process.stdout
    if stream is not None:
        for line in stream:
            chunks.append(line)
            # markup=False：构建输出里会出现 "[WinError 5]" 这类方括号文本，
            # 交给 Rich 当标记解析会抛 MarkupError，而这正是我们要检测的内容。
            console.print(line, end="", markup=False)
    return subprocess.CompletedProcess(args, process.wait(), "".join(chunks), "")


def run_command(args: Sequence[str], *, capture: bool = False, check: bool = True, cwd: Path = ROOT, transient_retries: int = 0) -> subprocess.CompletedProcess[str]:
    console.print(Text(f"$ {format_command(args)}", style="bold blue"))
    delay = 0.5
    for attempt in range(transient_retries + 1):
        if capture:
            result = subprocess.run(args, cwd=cwd, text=True, capture_output=True, encoding="utf-8", errors="replace")
            # 输出是子进程的任意文本，不能让 Rich 当标记解析。
            if result.stdout:
                console.print(result.stdout, end="", markup=False)
            if result.stderr:
                error_console.print(result.stderr, end="", style="red", markup=False)
        elif transient_retries:
            # 既要实时进度又要能在输出里找错误码：走 tee。
            # 直接用 subprocess.run 的话 stdout/stderr 都是 None，下面的标记检测
            # 永远匹配不到，重试次数形同虚设。
            result = _run_with_tee(args, cwd=cwd)
        else:
            result = subprocess.run(args, cwd=cwd, text=True, encoding="utf-8", errors="replace")
        if result.returncode == 0 or attempt == transient_retries:
            break
        if not any(marker in f"{result.stdout or ''}\n{result.stderr or ''}" for marker in TRANSIENT_FILE_LOCK_MARKERS):
            break
        warning(f"命令被 Windows 文件占用拒绝，{delay:.1f}s 后重试（{attempt + 1}/{transient_retries}）。")
        time.sleep(delay)
        delay *= 2
    if check and result.returncode != 0:
        raise SystemExit(result.returncode)
    return result


def git_args(args: Sequence[str], *, no_proxy: bool = False) -> list[str]:
    command = ["git", "--no-pager"]
    if no_proxy:
        command.extend(["-c", "http.proxy=", "-c", "https.proxy="])
    command.extend(args)
    return command


def run_git(args: Sequence[str], *, capture: bool = False, check: bool = True, no_proxy: bool = False) -> subprocess.CompletedProcess[str]:
    return run_command(git_args(args, no_proxy=no_proxy), capture=capture, check=check)


def read_project_version(path: Path = PYPROJECT_PATH) -> str:
    data = tomllib.loads(path.read_text(encoding="utf-8"))
    version = data.get("project", {}).get("version")
    if not isinstance(version, str) or not version:
        raise RuntimeError("pyproject.toml 中缺少 project.version。")
    return version


def write_project_version(version: str, path: Path = PYPROJECT_PATH) -> None:
    text = path.read_text(encoding="utf-8")
    updated, count = re.subn(r'(?m)^version = "[^"]+"$', f'version = "{version}"', text, count=1)
    if count != 1:
        raise RuntimeError("无法更新 pyproject.toml 中的 version 字段。")
    path.write_text(updated, encoding="utf-8")


def write_locked_version(version: str, path: Path = UV_LOCK_PATH) -> None:
    """同步 uv.lock 里本项目自己的版本号。

    uv 把根项目也当作一个 package 写进 uv.lock。发布只改 pyproject 时，lock 里的版本
    会停在旧值：tag 上的 pyproject 是 4.1.0、uv.lock 却写着 4.0.3，而且之后任何
    `uv run` 都会把它改回来，于是工作区永远脏一块。

    这里直接改那一行，而不是跑 `uv lock`：只有根项目版本变了，依赖解析结果不变，
    跑 uv lock 反而要联网、可能因索引不可达而失败。找不到条目就静默跳过（不是 uv
    项目，或 lock 格式变了）—— 不猜、不误改依赖的版本号。
    """
    if not path.exists():
        return
    lines = path.read_text(encoding="utf-8").splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.strip() != 'name = "auto-model-key-router"':
            continue
        for offset in range(1, 4):  # version 紧跟 name，留一点余量
            target = index + offset
            if target < len(lines) and lines[target].startswith("version = "):
                lines[target] = f'version = "{version}"\n'
                path.write_text("".join(lines), encoding="utf-8")
                return


def next_prerelease(current: ParsedVersion, phase: str) -> str:
    if current.pre == phase and current.pre_num is not None:
        major, minor, patch, number = current.major, current.minor, current.patch, current.pre_num + 1
    else:
        major, minor, patch, number = current.major, current.minor, current.patch + 1, 1
    return f"{major}.{minor}.{patch}{phase}{number}"


def calculate_next_version(current_version: str, release_type: str, custom_version: str | None = None) -> str:
    current = ParsedVersion.parse(current_version)
    if release_type == "custom":
        if not custom_version:
            raise ValueError("自定义版本号不能为空。")
        return ParsedVersion.parse(custom_version).normalized
    if release_type == "patch":
        return f"{current.major}.{current.minor}.{current.patch + 1}"
    if release_type == "minor":
        return f"{current.major}.{current.minor + 1}.0"
    if release_type == "major":
        return f"{current.major + 1}.0.0"
    if release_type == "post":
        return f"{current.base}.post{(current.post or 0) + 1}"
    if release_type == "preview":
        return next_prerelease(current, "rc")
    if release_type == "alpha":
        return next_prerelease(current, "a")
    if release_type == "beta":
        return next_prerelease(current, "b")
    if release_type == "dev":
        next_dev = current.dev + 1 if current.dev is not None else 1
        return f"{current.major}.{current.minor}.{current.patch + (0 if current.dev is not None else 1)}.dev{next_dev}"
    if release_type == "stable":
        return current.base
    raise ValueError(f"不支持的发布类型: {release_type}")


def normalize_release_notes(notes: str) -> str:
    stripped = notes.strip()
    if not stripped:
        return ""
    if stripped.startswith("### "):
        return stripped
    lines = []
    for line in stripped.splitlines():
        line = line.strip()
        if not line:
            continue
        lines.append(line if line.startswith("- ") else f"- {line}")
    return "### Changed\n" + "\n".join(lines)


def find_next_version_heading(text: str, start: int) -> int:
    match = re.search(r"(?m)^##\s+", text[start:])
    return start + match.start() if match else len(text)


def render_updated_changelog(text: str, version: str, release_date: str, notes: str = "") -> str:
    if re.search(rf"(?m)^##\s+\[?v?{re.escape(version)}\]?(?:\s+-\s+.*)?[ \t]*$", text):
        raise RuntimeError(f"CHANGELOG.md 中已存在 {version} 条目。")
    match = re.search(r"(?m)^##\s+\[Unreleased\][ \t]*$", text)
    release_notes = normalize_release_notes(notes)
    if not release_notes:
        release_notes = "### Changed\n- 版本发布维护。"
    release_block = f"## [{version}] - {release_date}\n\n{release_notes.strip()}\n\n"
    if not match:
        return text.rstrip() + f"\n\n## [Unreleased]\n\n{release_block}"
    body_start = match.end()
    next_heading = find_next_version_heading(text, body_start)
    unreleased_body = text[body_start:next_heading].strip()
    if unreleased_body and not notes.strip():
        release_block = f"## [{version}] - {release_date}\n\n{unreleased_body}\n\n"
    return text[:body_start] + "\n\n" + release_block + text[next_heading:].lstrip("\n")


def update_changelog(version: str, release_date: str, notes: str, path: Path = CHANGELOG_PATH) -> None:
    text = path.read_text(encoding="utf-8")
    path.write_text(render_updated_changelog(text, version, release_date, notes), encoding="utf-8")


COMMIT_TYPE_MAP: dict[str, str] = {
    "feat": "Added",
    "fix": "Fixed",
    "refactor": "Changed",
    "perf": "Changed",
    "docs": "Changed",
    "style": "Changed",
    "chore": "Changed",
    "build": "Changed",
    "ci": "Changed",
    "test": "Changed",
    "revert": "Changed",
}
COMMIT_PATTERN = re.compile(r"^(?P<type>[a-z]+)(?:\(.+?\))?!?:\s*(?P<desc>.+)$")


def get_last_release_tag() -> str | None:
    result = run_git(["describe", "--tags", "--abbrev=0"], capture=True, check=False)
    tag = result.stdout.strip()
    return tag if result.returncode == 0 and tag else None


def get_commits_since(tag: str | None) -> list[str]:
    args = ["log", "--no-merges", "--format=%s"]
    if tag:
        args.append(f"{tag}..HEAD")
    result = run_git(args, capture=True, check=False)
    if result.returncode != 0:
        return []
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def classify_commits(commits: Sequence[str]) -> dict[str, list[str]]:
    groups: dict[str, list[str]] = {"Added": [], "Changed": [], "Fixed": []}
    for commit in commits:
        match = COMMIT_PATTERN.match(commit)
        if match:
            commit_type = match.group("type")
            desc = match.group("desc")
            category = COMMIT_TYPE_MAP.get(commit_type, "Changed")
            groups[category].append(f"- {desc}")
        else:
            groups["Changed"].append(f"- {commit}")
    return {key: value for key, value in groups.items() if value}


def generate_release_notes() -> str:
    tag = get_last_release_tag()
    commits = get_commits_since(tag)
    if not commits:
        return ""
    groups = classify_commits(commits)
    if not groups:
        return ""
    sections = []
    for category in ("Added", "Changed", "Fixed"):
        if category in groups:
            sections.append(f"### {category}\n" + "\n".join(groups[category]))
    return "\n\n".join(sections)


def prompt_release_type(current_version: str) -> str:
    table = Table(title="发布类型", box=box.ROUNDED, header_style="bold cyan", border_style="cyan", show_lines=False)
    table.add_column("编号", justify="right", style="cyan", width=4)
    table.add_column("类型", style="bold")
    table.add_column("目标版本", style="green")
    for index, release_type in enumerate(RELEASE_TYPES, 1):
        try:
            preview = calculate_next_version(current_version, release_type, "0.0.0" if release_type == "custom" else None)
        except ValueError:
            preview = "手动输入"
        table.add_row(str(index), RELEASE_LABELS[release_type], preview)
    console.print(Panel(f"当前版本: [bold green]{current_version}[/bold green]", title="[bold]版本发布[/bold]", border_style="cyan", box=box.ROUNDED))
    console.print(table)
    while True:
        choice = input_text("请选择发布类型，默认 1:").strip() or "1"
        if choice.isdigit() and 1 <= int(choice) <= len(RELEASE_TYPES):
            return RELEASE_TYPES[int(choice) - 1]
        if choice in RELEASE_TYPES:
            return choice
        warning("请输入菜单编号或发布类型名称。")


def prompt_custom_version() -> str:
    while True:
        value = input_text("请输入自定义版本号:").strip()
        try:
            return ParsedVersion.parse(value).normalized
        except ValueError as exc:
            warning(str(exc))


def prompt_edit_notes(draft: str) -> str:
    if draft:
        console.print(Panel(draft, title="[bold cyan]自动生成的发布说明[/bold cyan]", border_style="cyan", box=box.ROUNDED))
        info("Enter 确认使用 · 输入内容替换 · 输入 e 编辑 · 输入 c 清空")
        choice = input("> ").strip()
        if choice == "e":
            return prompt_manual_notes()
        if choice == "c":
            return ""
        if choice:
            return choice
        return draft
    info("未从 commit 中提取到发布说明，请手动输入；空行结束。")
    return prompt_manual_notes()


def prompt_manual_notes() -> str:
    lines: list[str] = []
    while True:
        line = input()
        if not line:
            break
        lines.append(line)
    return "\n".join(lines)


def confirm(message: str, *, assume_yes: bool = False) -> bool:
    if assume_yes:
        success(f"{message} yes")
        return True
    return input_text(f"{message} [y/N]:").strip().lower() in {"y", "yes"}


def status_files() -> list[str]:
    result = run_git(["status", "--porcelain"], capture=True)
    files: list[str] = []
    for line in result.stdout.splitlines():
        path = line[3:].strip()
        if " -> " in path:
            path = path.split(" -> ", 1)[1]
        if path:
            files.append(path.replace("\\", "/"))
    return files


def staged_files() -> list[str]:
    result = run_git(["diff", "--cached", "--name-only"], capture=True)
    return [line.strip().replace("\\", "/") for line in result.stdout.splitlines() if line.strip()]


def find_sensitive_paths(paths: Sequence[str]) -> list[str]:
    risky: list[str] = []
    for path in paths:
        normalized = path.replace("\\", "/")
        if any(fnmatch.fnmatch(normalized, pattern) for pattern in SENSITIVE_PATTERNS):
            risky.append(normalized)
    return risky


def ensure_safe_paths(paths: Sequence[str]) -> None:
    risky = find_sensitive_paths(paths)
    if risky:
        joined = "\n".join(f"- {path}" for path in risky)
        raise SystemExit(f"检测到可能包含隐私或运行时数据的待提交文件，已中止发布:\n{joined}")


def ensure_branch(expected_branch: str, assume_yes: bool) -> None:
    result = run_git(["rev-parse", "--abbrev-ref", "HEAD"], capture=True)
    branch = result.stdout.strip()
    if branch != expected_branch and not confirm(f"当前分支是 {branch}，不是 {expected_branch}，是否继续?", assume_yes=assume_yes):
        raise SystemExit(1)


def tag_exists(tag: str, remote: str, no_proxy: bool) -> bool:
    local = run_git(["tag", "--list", tag], capture=True).stdout.strip()
    if local:
        return True
    remote_result = run_git(["ls-remote", "--tags", remote, tag], capture=True, check=False, no_proxy=no_proxy)
    if remote_result.returncode != 0:
        raise SystemExit(remote_result.returncode)
    return bool(remote_result.stdout.strip())


def push_with_proxy_fallback(args: Sequence[str], no_proxy: bool) -> None:
    """推送，并处理两类可自愈的失败。

    1) 代理/经代理的 TLS 握手异常 → 临时绕过代理再试一次；
    2) 纯网络抖动 → 退避后重试。

    背景：原来只在错误文本里出现 "proxy" 或 "127.0.0.1" 时才绕过代理，而
    `schannel: failed to receive handshake, SSL/TLS connection failed` 两者都不含，
    于是既不绕代理、也不重试 —— 一次瞬时抖动就把整个发布卡在最后一步，而提交和
    标签已经建好，留下「已提交已打标签、但没推上去」的半成品状态。
    """
    delay = 1.0
    last: subprocess.CompletedProcess[str] | None = None
    for attempt in range(PUSH_ATTEMPTS):
        result = run_git(args, capture=True, check=False, no_proxy=no_proxy)
        if result.returncode == 0:
            return
        last = result
        text = _git_failure_text(result)
        if not any(marker in text for marker in CONNECTIVITY_FAILURE_MARKERS):
            # 不是连接问题（例如鉴权被拒），重试和绕代理都没意义。
            break
        if not no_proxy:
            warning("检测到 Git 代理/TLS 连接异常，临时绕过代理重试。")
            direct = run_git(args, capture=True, check=False, no_proxy=True)
            if direct.returncode == 0:
                return
            last = direct
            no_proxy = True  # 直连也失败，后面就别再绕回代理了
            if not any(marker in _git_failure_text(direct) for marker in CONNECTIVITY_FAILURE_MARKERS):
                break
        if attempt == PUSH_ATTEMPTS - 1:
            break
        warning(f"推送遇到瞬时网络错误，{delay:.0f}s 后重试（{attempt + 1}/{PUSH_ATTEMPTS}）。")
        time.sleep(delay)
        delay *= 2
    raise SystemExit(
        f"推送失败（已尝试 {PUSH_ATTEMPTS} 次，含代理与直连）。\n"
        f"{(_git_failure_text(last) if last else '')}".strip()
    )


def verify_dist(version: str) -> None:
    dist_files = sorted(str(path) for path in (ROOT / "dist").glob(f"*{version}*"))
    if not dist_files:
        raise SystemExit(f"dist 中没有找到版本 {version} 的构建产物。")
    run_command([sys.executable, "-m", "twine", "check", *dist_files])


def install_dev_environment() -> None:
    pip_args = [sys.executable, "-m", "pip", "install", "--upgrade", "-e", ".[dev]"]
    remove_stale_egg_info()
    result = run_command(pip_args, capture=True, check=False, transient_retries=3)
    if result.returncode == 0:
        return
    if "No module named pip" not in result.stderr:
        raise SystemExit(result.returncode)
    warning("当前解释器缺少 pip，正在通过 ensurepip 初始化。")
    run_command([sys.executable, "-m", "ensurepip", "--upgrade"])
    run_command(pip_args, transient_retries=3)


def preview_plan(current_version: str, next_version: str, tag: str, args: argparse.Namespace) -> None:
    table = Table(box=box.SIMPLE_HEAVY, show_header=False, padding=(0, 1))
    table.add_column("项目", style="cyan", no_wrap=True)
    table.add_column("值", style="bold")
    table.add_row("当前版本", current_version)
    table.add_row("目标版本", f"[green]{next_version}[/green]")
    table.add_row("标签名称", f"[magenta]{tag}[/magenta]")
    table.add_row("目标分支", args.branch)
    table.add_row("远端仓库", args.remote)
    table.add_row("测试", "[yellow]跳过[/yellow]" if args.skip_tests else "[green]执行[/green]")
    table.add_row("构建", "[yellow]跳过[/yellow]" if args.skip_build else "[green]执行[/green]")
    table.add_row("Twine", "[yellow]跳过[/yellow]" if args.skip_twine else "[green]执行[/green]")
    table.add_row("推送", "[yellow]仅本地[/yellow]" if args.no_push else "[green]推送远端[/green]")
    console.print(Panel(table, title="[bold cyan]发布计划[/bold cyan]", border_style="cyan", box=box.ROUNDED))


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="交互式版本发布脚本")
    parser.add_argument("--type", choices=RELEASE_TYPES, dest="release_type", help="发布类型")
    parser.add_argument("--version", dest="custom_version", help="自定义版本号，仅 --type custom 时使用")
    parser.add_argument("--notes", help="发布说明；留空时优先使用 CHANGELOG Unreleased 内容，其次自动从 commit 生成")
    parser.add_argument("--branch", default="master", help="允许发布的目标分支")
    parser.add_argument("--remote", default="origin", help="推送目标远端")
    parser.add_argument("--yes", "-y", action="store_true", help="跳过确认提示")
    parser.add_argument("--dry-run", action="store_true", help="只计算版本和展示计划，不修改文件、不提交、不推送")
    parser.add_argument("--no-push", action="store_true", help="完成本地提交和标签后不推送")
    parser.add_argument("--no-proxy", action="store_true", help="推送和查询远端标签时临时绕过 Git 代理")
    parser.add_argument("--skip-tests", action="store_true", help="跳过 pytest")
    parser.add_argument("--skip-build", action="store_true", help="跳过 python -m build")
    parser.add_argument("--skip-twine", action="store_true", help="跳过 twine check")
    parser.add_argument("--commit-message", help="自定义提交信息")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    os.chdir(ROOT)
    current_version = read_project_version()
    release_type = args.release_type or prompt_release_type(current_version)
    custom_version = args.custom_version
    if release_type == "custom" and not custom_version and not args.dry_run:
        custom_version = prompt_custom_version()
    next_version = calculate_next_version(current_version, release_type, custom_version)
    tag = f"v{next_version}"
    preview_plan(current_version, next_version, tag, args)
    if args.dry_run:
        return 0
    if not confirm("确认开始发布流程?", assume_yes=args.yes):
        return 1
    step("发布前检查")
    ensure_branch(args.branch, args.yes)
    ensure_safe_paths(status_files())
    if tag_exists(tag, args.remote, args.no_proxy):
        raise SystemExit(f"标签 {tag} 已存在。")
    step(STEP_TITLES["install"])
    install_dev_environment()
    if not args.skip_tests:
        step(STEP_TITLES["tests"])
        run_command([sys.executable, "-m", "pytest"])
    if args.notes is not None:
        notes = args.notes
    else:
        changelog_text = CHANGELOG_PATH.read_text(encoding="utf-8") if CHANGELOG_PATH.exists() else ""
        has_unreleased = bool(re.search(r"(?m)^##\s+\[Unreleased\][ \t]*$", changelog_text))
        if has_unreleased:
            match = re.search(r"(?m)^##\s+\[Unreleased\][ \t]*$", changelog_text)
            assert match is not None
            body_start = match.end()
            next_heading = find_next_version_heading(changelog_text, body_start)
            unreleased_body = changelog_text[body_start:next_heading].strip()
            if unreleased_body:
                notes = ""
                info("CHANGELOG Unreleased 已有内容，将直接使用。")
            else:
                notes = generate_release_notes()
                if not args.yes:
                    notes = prompt_edit_notes(notes)
        else:
            notes = generate_release_notes()
            if not args.yes:
                notes = prompt_edit_notes(notes)
    write_project_version(next_version)
    write_locked_version(next_version)
    update_changelog(next_version, date.today().isoformat(), notes)
    success("已更新 pyproject.toml、uv.lock 和 CHANGELOG.md")
    if not args.skip_build:
        step(STEP_TITLES["build"])
        remove_stale_egg_info()
        run_command([sys.executable, "-m", "build"], transient_retries=3)
    if not args.skip_twine:
        step(STEP_TITLES["twine"])
        verify_dist(next_version)
    step(STEP_TITLES["git"])
    run_git(["diff", "--check"])
    run_git(["add", "-A"])
    ensure_safe_paths(staged_files())
    run_git(["diff", "--cached", "--check"])
    message = args.commit_message or f"chore(release): 发布 {next_version} 版本"
    step(STEP_TITLES["commit"])
    run_git(["commit", "-m", message])
    run_git(["tag", "-a", tag, "-m", tag])
    if not args.no_push:
        step(STEP_TITLES["push"])
        push_with_proxy_fallback(["push", args.remote, args.branch], args.no_proxy)
        push_with_proxy_fallback(["push", args.remote, tag], args.no_proxy)
    run_git(["status", "--short", "--branch"])
    console.print(Panel(f"[green]发布流程完成[/green]\n[bold magenta]{tag}[/bold magenta]", title="[bold green]成功[/bold green]", border_style="green", box=box.ROUNDED))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
