"""CLI E2E 测试（TDD-CLI-02：uws crawl 深度/signals/格式/输出目录）。"""

from __future__ import annotations

import json
from pathlib import Path
from unittest.mock import MagicMock

from coordinator.cli import create_parser, run_crawl
from coordinator.engine_client import ActionResult, InitResult


def test_tdd_cli_02_crawl_signals_and_output_dir(tmp_path: Path):
    """验证 uws crawl CLI 参数解析、信号提取、广告过滤与文件落地。"""
    mock_engine = MagicMock()
    mock_engine.initialize_context.return_value = InitResult(
        ok=True,
        target_id="tgt-cli-crawl-01",
        context_id="ctx-cli-crawl-01",
    )

    # 模拟提取信号响应
    def mock_execute_action(intent_id: str, target_semantic: str, expected_action: str, **kwargs):
        if "title" in target_semantic:
            return ActionResult(success=True, extracted_data="深入理解 Web 引擎底层架构", current_url="http://mock-blog.local/page1")
        if "content" in target_semantic:
            return ActionResult(success=True, extracted_data="第一章：浏览器事件循环与微任务队列机制...", current_url="http://mock-blog.local/page1")
        if "author" in target_semantic:
            return ActionResult(success=True, extracted_data="架构师老张", current_url="http://mock-blog.local/page1")
        if "下一页" in target_semantic:
            return ActionResult(success=False, error_reason="ACTION_ELEMENT_NOT_FOUND")
        return ActionResult(success=True)

    mock_engine.execute_action.side_effect = mock_execute_action

    out_dir = tmp_path / "crawl_output"
    parser = create_parser()
    args = parser.parse_args([
        "crawl",
        "http://mock-blog.local/page1",
        "--depth", "2",
        "--signals", "title", "content", "author",
        "--negative-keywords", "广告", "赞助商",
        "--format", "md",
        "--output-dir", str(out_dir),
    ])

    ret = run_crawl(args, engine=mock_engine)
    assert ret == 0

    # 验证输出文件生成
    md_files = list(out_dir.glob("page_*.md"))
    assert len(md_files) == 1
    content = md_files[0].read_text(encoding="utf-8")
    assert "---" in content
    assert "深入理解 Web 引擎底层架构" in content
    assert "架构师老张" in content
