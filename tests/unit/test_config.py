"""~/.uws/config.yaml 读取（纯 stdlib YAML 子集，与 cmd/uws/config.go 同构）。"""

from coordinator.config import config_int, load_uws_config


def test_load_uws_config_subset_yaml(tmp_path):
    cfg = tmp_path / "config.yaml"
    cfg.write_text(
        """# 注释行
core:
  max_targets: 10

llm:
  model: "deepseek-chat"
  api_key: sk-123

vlm:
  endpoint: https://solver.example.com/v1/solve
  api_key: 'vlm-sk-456'
""",
        encoding="utf-8",
    )
    values = load_uws_config(cfg)
    assert values["core.max_targets"] == "10"
    assert values["llm.model"] == "deepseek-chat"
    assert values["llm.api_key"] == "sk-123"
    assert values["vlm.endpoint"] == "https://solver.example.com/v1/solve"
    assert values["vlm.api_key"] == "vlm-sk-456"


def test_load_uws_config_missing_file_is_empty():
    assert load_uws_config("/nonexistent/uws/config.yaml") == {}


def test_load_uws_config_ignores_malformed_lines(tmp_path):
    cfg = tmp_path / "config.yaml"
    cfg.write_text("not-a-section\n  orphan: x\n\nvlm:\n  timeout_sec: 10\n", encoding="utf-8")
    values = load_uws_config(cfg)
    assert "orphan" not in values
    assert values["vlm.timeout_sec"] == "10"


def test_config_int_safe_default():
    values = {"vlm.timeout_sec": "12"}
    assert config_int(values, "vlm.timeout_sec", 10) == 12
    assert config_int(values, "vlm.max_solves", 3) == 3
    assert config_int({"vlm.timeout_sec": "abc"}, "vlm.timeout_sec", 10) == 10
