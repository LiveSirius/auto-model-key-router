from __future__ import annotations

import importlib.util
import io
import subprocess
import sys
from pathlib import Path

import pytest


def load_release_script():
    path = Path(__file__).resolve().parents[1] / "scripts" / "release.py"
    spec = importlib.util.spec_from_file_location("release_script", path)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    sys.modules["release_script"] = module
    spec.loader.exec_module(module)
    return module


release_script = load_release_script()


def test_calculate_next_version_for_stable_releases() -> None:
    assert release_script.calculate_next_version("1.2.2", "patch") == "1.2.3"
    assert release_script.calculate_next_version("1.2.2", "minor") == "1.3.0"
    assert release_script.calculate_next_version("1.2.2", "major") == "2.0.0"


def test_calculate_next_version_for_post_and_preview_releases() -> None:
    assert release_script.calculate_next_version("1.2.2", "post") == "1.2.2.post1"
    assert release_script.calculate_next_version("1.2.2.post1", "post") == "1.2.2.post2"
    assert release_script.calculate_next_version("1.2.2", "preview") == "1.2.3rc1"
    assert release_script.calculate_next_version("1.2.3rc1", "preview") == "1.2.3rc2"


def test_calculate_next_version_for_extra_release_types() -> None:
    assert release_script.calculate_next_version("1.2.2", "alpha") == "1.2.3a1"
    assert release_script.calculate_next_version("1.2.3a1", "alpha") == "1.2.3a2"
    assert release_script.calculate_next_version("1.2.2", "beta") == "1.2.3b1"
    assert release_script.calculate_next_version("1.2.2", "dev") == "1.2.3.dev1"
    assert release_script.calculate_next_version("1.2.3rc1", "stable") == "1.2.3"
    assert release_script.calculate_next_version("1.2.2", "custom", "3.0.0rc1") == "3.0.0rc1"


def test_git_args_disable_pager() -> None:
    assert release_script.git_args(["diff", "--check"]) == ["git", "--no-pager", "diff", "--check"]
    assert release_script.git_args(["push", "origin", "master"], no_proxy=True) == [
        "git",
        "--no-pager",
        "-c",
        "http.proxy=",
        "-c",
        "https.proxy=",
        "push",
        "origin",
        "master",
    ]


def test_run_command_prints_captured_stderr(capsys) -> None:
    result = release_script.run_command(
        [sys.executable, "-c", "import sys; sys.stderr.write('release error\\n')"],
        capture=True,
        check=False,
    )

    captured = capsys.readouterr()
    assert result.returncode == 0
    assert "release error" in captured.err


def _gbk_stream() -> tuple[io.TextIOWrapper, io.BytesIO]:
    buffer = io.BytesIO()
    return io.TextIOWrapper(buffer, encoding="gbk", errors="strict"), buffer


def test_make_output_utf8_safe_stops_gbk_console_from_crashing(monkeypatch) -> None:
    """回归：中文 Windows 控制台是 GBK，Rich 打印 ✓/ℹ/⚠ 会抛 UnicodeEncodeError，
    使发布在改完版本号之后、提交之前中断。"""
    stdout, stdout_buffer = _gbk_stream()
    stderr, _ = _gbk_stream()
    monkeypatch.setattr(sys, "stdout", stdout)
    monkeypatch.setattr(sys, "stderr", stderr)

    with pytest.raises(UnicodeEncodeError):
        stdout.write("✓")

    release_script.make_output_utf8_safe()
    stdout.write("✓")
    stdout.flush()

    assert stdout_buffer.getvalue().decode("gbk") == "?"


def test_render_updated_changelog_moves_unreleased_body() -> None:
    text = "# Changelog\n\n## [Unreleased]\n\n### Added\n- 新增功能\n\n## [1.0.0] - 2026-01-01\n\n### Added\n- 初始版本\n"

    updated = release_script.render_updated_changelog(text, "1.0.1", "2026-06-09")

    assert "## [Unreleased]\n\n## [1.0.1] - 2026-06-09" in updated
    assert "### Added\n- 新增功能" in updated
    assert updated.index("## [1.0.1]") < updated.index("## [1.0.0]")


def test_render_updated_changelog_uses_notes_when_unreleased_empty() -> None:
    text = "# Changelog\n\n## [Unreleased]\n\n## [1.0.0] - 2026-01-01\n"

    updated = release_script.render_updated_changelog(text, "1.0.1", "2026-06-09", "修复发布流程")

    assert "## [1.0.1] - 2026-06-09\n\n### Changed\n- 修复发布流程" in updated


def test_preview_plan_renders_rich_release_summary(capsys) -> None:
    args = release_script.build_parser().parse_args(["--type", "patch", "--dry-run", "--skip-tests", "--no-push"])

    release_script.preview_plan("1.2.3", "1.2.4", "v1.2.4", args)

    output = capsys.readouterr().out
    assert "发布计划" in output
    assert "当前版本" in output
    assert "1.2.3" in output
    assert "目标版本" in output
    assert "1.2.4" in output
    assert "仅本地" in output


def test_classify_commits_by_conventional_type() -> None:
    commits = [
        "feat(api): 新增 unified-model 端点",
        "fix(ui): 修复统计页面显示",
        "refactor: 移除缓存命中次数统计",
        "docs: 更新 README",
        "普通提交没有前缀",
    ]

    groups = release_script.classify_commits(commits)

    assert groups["Added"] == ["- 新增 unified-model 端点"]
    assert groups["Fixed"] == ["- 修复统计页面显示"]
    assert "- 移除缓存命中次数统计" in groups["Changed"]
    assert "- 更新 README" in groups["Changed"]
    assert "- 普通提交没有前缀" in groups["Changed"]


def test_classify_commits_skips_empty_groups() -> None:
    commits = ["feat: 新功能"]

    groups = release_script.classify_commits(commits)

    assert "Added" in groups
    assert "Changed" not in groups
    assert "Fixed" not in groups


def test_classify_commits_empty_input() -> None:
    assert release_script.classify_commits([]) == {}


def test_run_command_retries_transient_windows_file_locks(monkeypatch) -> None:
    """回归：setuptools 用 NamedTemporaryFile + os.replace 写 egg-info/PKG-INFO，
    Windows 上目标文件被短暂占用会抛 WinError 5，使发布卡在准备发布环境一步。"""
    monkeypatch.setattr(release_script.time, "sleep", lambda _seconds: None)
    attempts: list[int] = []
    failures = 2

    def fake_run(args, **kwargs):
        attempts.append(1)
        if len(attempts) <= failures:
            # 错误正文是本地化的，只有 [WinError 5] 这段 ASCII 稳定出现。
            return subprocess.CompletedProcess(args, 1, "", "PermissionError: [WinError 5] 拒绝访问。: 'tmp8k2' -> 'PKG-INFO'")
        return subprocess.CompletedProcess(args, 0, "ok", "")

    monkeypatch.setattr(release_script.subprocess, "run", fake_run)

    result = release_script.run_command(["pip", "install", "-e", "."], capture=True, transient_retries=3)

    assert result.returncode == 0
    assert len(attempts) == failures + 1


def test_run_command_does_not_retry_real_failures(monkeypatch) -> None:
    monkeypatch.setattr(release_script.time, "sleep", lambda _seconds: None)
    # WinError 50 之类的其它错误码不应被当作文件占用而重试。
    messages = ["error: no matching distribution found", "OSError: [WinError 50] 不支持该请求"]

    for message in messages:
        attempts: list[int] = []

        def fake_run(args, _message=message, **kwargs):
            attempts.append(1)
            return subprocess.CompletedProcess(args, 1, "", _message)

        monkeypatch.setattr(release_script.subprocess, "run", fake_run)

        result = release_script.run_command(["pip", "install", "-e", "."], capture=True, check=False, transient_retries=3)

        assert result.returncode == 1
        assert len(attempts) == 1, message


def test_run_command_gives_up_after_transient_retries(monkeypatch) -> None:
    monkeypatch.setattr(release_script.time, "sleep", lambda _seconds: None)
    attempts: list[int] = []

    def fake_run(args, **kwargs):
        attempts.append(1)
        return subprocess.CompletedProcess(args, 1, "", "PermissionError: [WinError 5] 拒绝访问。")

    monkeypatch.setattr(release_script.subprocess, "run", fake_run)

    with pytest.raises(SystemExit):
        release_script.run_command(["pip", "install", "-e", "."], capture=True, transient_retries=3)

    assert len(attempts) == 4


# —— 流式命令（capture=False）的瞬时占用重试 ——
# 上面三个测试都传了 capture=True，而真正会撞上 WinError 5 的两个调用点用的是默认的
# capture=False。那条路径走 subprocess.run 时 stdout/stderr 都是 None，标记检测永远
# 匹配不到，于是 python -m build 一步失败一次就直接放弃 —— 重试写了却从未生效。
# 现在 capture=False 且有重试次数时改走 _run_with_tee，下面用 Popen 替身锁住它。


class _FakePipe:
    def __init__(self, text: str) -> None:
        self._lines = text.splitlines(keepends=True)

    def __iter__(self):
        return iter(self._lines)


class _FakeProcess:
    def __init__(self, returncode: int, output: str) -> None:
        self.stdout = _FakePipe(output)
        self._returncode = returncode

    def wait(self) -> int:
        return self._returncode


class _FakePopen:
    """按预设序列逐次返回 (返回码, 输出)，并记录被调用的命令行。"""

    def __init__(self, script: list[tuple[int, str]]) -> None:
        self._script = script
        self.commands: list[list[str]] = []

    def __call__(self, args, **_kwargs):
        index = min(len(self.commands), len(self._script) - 1)
        self.commands.append(list(args))
        return _FakeProcess(*self._script[index])

    @property
    def attempts(self) -> int:
        return len(self.commands)


def test_run_command_retries_transient_lock_without_capture(monkeypatch) -> None:
    """回归：python -m build 用 capture=False 调用，也必须能重试瞬时文件占用。"""
    monkeypatch.setattr(release_script.time, "sleep", lambda _seconds: None)
    fake = _FakePopen([(1, "error: [WinError 5] 拒绝访问。: 'tmppbk1qbm' -> 'PKG-INFO'")])
    monkeypatch.setattr(release_script.subprocess, "Popen", fake)

    result = release_script.run_command(
        [sys.executable, "-m", "build"], check=False, transient_retries=3
    )

    assert result.returncode == 1
    assert fake.attempts == 4, "瞬时占用必须在 capture=False 时也重试到上限"
    # 输出要留存下来供标记检测，否则判定依据又丢了。
    assert "[WinError 5]" in result.stdout


def test_run_command_recovers_when_transient_lock_clears(monkeypatch) -> None:
    """占用是瞬时的：第 3 次成功就应当收工，而不是继续重试。"""
    monkeypatch.setattr(release_script.time, "sleep", lambda _seconds: None)
    fake = _FakePopen([
        (1, "error: [WinError 5] 拒绝访问。"),
        (1, "error: [WinError 5] 拒绝访问。"),
        (0, "Successfully built"),
    ])
    monkeypatch.setattr(release_script.subprocess, "Popen", fake)

    result = release_script.run_command(
        [sys.executable, "-m", "build"], check=False, transient_retries=3
    )

    assert result.returncode == 0
    assert fake.attempts == 3


def test_run_command_does_not_retry_real_failures_without_capture(monkeypatch) -> None:
    """capture=False 时同样不能把真实错误当文件占用来重试。"""
    monkeypatch.setattr(release_script.time, "sleep", lambda _seconds: None)
    fake = _FakePopen([(1, "error: no matching distribution found")])
    monkeypatch.setattr(release_script.subprocess, "Popen", fake)

    result = release_script.run_command(
        [sys.executable, "-m", "build"], check=False, transient_retries=3
    )

    assert result.returncode == 1
    assert fake.attempts == 1


def test_remove_stale_egg_info_deletes_build_artifacts(tmp_path, monkeypatch) -> None:
    """构建前清掉 egg-info：目标不存在时 os.replace 就不会撞锁。"""
    stale = tmp_path / "auto_model_key_router.egg-info"
    stale.mkdir()
    (stale / "PKG-INFO").write_text("old", encoding="utf-8")
    (tmp_path / "unrelated").mkdir()

    monkeypatch.setattr(release_script, "ROOT", tmp_path)
    release_script.remove_stale_egg_info()

    assert not stale.exists()
    assert (tmp_path / "unrelated").exists(), "只清 egg-info，不能误删其它目录"
