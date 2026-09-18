from __future__ import annotations

import json
from pathlib import Path

from auto_model_key_router import config_editor
from auto_model_key_router.config import RouterConfig


def write_config(tmp_path: Path, **overrides: object) -> Path:
    path = tmp_path / "router-config.json"
    data: dict[str, object] = {
        "host": "127.0.0.1",
        "port": 8000,
        "local_api_key": "local-key",
        "models": [],
    }
    data.update(overrides)
    path.write_text(json.dumps(data), encoding="utf-8")
    return path


def test_set_webui_interactively_enables_and_reports_restart(
    tmp_path: Path, monkeypatch
) -> None:
    path = write_config(tmp_path)
    assert RouterConfig.load(path).webui_enabled is False

    monkeypatch.setattr(config_editor, "confirm_choice", lambda *a, **k: True)
    monkeypatch.setattr(
        config_editor, "restart_service_after_config_change", lambda *a, **k: None
    )

    config_editor.set_webui_interactively(path)

    assert RouterConfig.load(path).webui_enabled is True


def test_set_webui_interactively_can_disable_again(tmp_path: Path, monkeypatch) -> None:
    path = write_config(tmp_path, webui_enabled=True)

    monkeypatch.setattr(config_editor, "confirm_choice", lambda *a, **k: True)
    monkeypatch.setattr(
        config_editor, "restart_service_after_config_change", lambda *a, **k: None
    )

    config_editor.set_webui_interactively(path)

    assert RouterConfig.load(path).webui_enabled is False


def test_set_webui_interactively_leaves_config_untouched_on_decline(
    tmp_path: Path, monkeypatch
) -> None:
    path = write_config(tmp_path)
    before = path.read_text(encoding="utf-8")

    monkeypatch.setattr(config_editor, "confirm_choice", lambda *a, **k: False)

    config_editor.set_webui_interactively(path)

    assert path.read_text(encoding="utf-8") == before
