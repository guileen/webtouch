"""VLM 求解模块单元测试（TDD-COORD-04 支撑件）。"""

import json
import urllib.error

from coordinator.vlm import (
    HttpVlmSolver,
    MockVlmSolver,
    VlmError,
    VlmSolution,
    build_vlm_solver,
)


def test_vlm_solution_coords_text():
    """坐标序列化为引擎 ExecuteAction drag 消费的 x1,y1,x2,y2 文本。"""
    solution = VlmSolution(x1=100, y1=200, x2=300, y2=200)
    assert solution.coords() == "100,200,300,200"
    assert solution.action == "drag"


def test_mock_vlm_solver_deterministic_and_records_calls():
    """Mock 求解器返回固定坐标、记录输入（截图 + 挑战语义）。"""
    solver = MockVlmSolver()
    first = solver.solve(b"fake-png", "login_btn")
    second = solver.solve(b"fake-png", "login_btn")

    assert first == VlmSolution(x1=100, y1=200, x2=300, y2=200)
    assert first == second  # 确定性：同输入同输出
    assert solver.calls == [(b"fake-png", "login_btn"), (b"fake-png", "login_btn")]


def test_mock_vlm_solver_custom_solution():
    """可注入自定义坐标（如点击型挑战）。"""
    solver = MockVlmSolver(VlmSolution(x1=10, y1=20, x2=10, y2=20, action="click"))
    assert solver.solve(b"", "captcha_click").coords() == "10,20,10,20"


class _FakeResp:
    def __init__(self, body: bytes) -> None:
        self._body = body

    def __enter__(self) -> "_FakeResp":
        return self

    def __exit__(self, *exc: object) -> bool:
        return False

    def read(self) -> bytes:
        return self._body


def _capture_urlopen(captured: list, body: bytes):
    def _urlopen(req, timeout=10.0):
        captured.append((req, timeout))
        return _FakeResp(body)

    return _urlopen


def test_http_vlm_solver_posts_and_parses():
    """POST base64 截图 + 挑战语义，携带 Bearer，解析坐标响应。"""
    captured: list = []
    solver = HttpVlmSolver(
        endpoint="https://solver.example.com/v1/solve",
        api_key="vlm-sk",
        timeout_sec=7,
        urlopen=_capture_urlopen(
            captured, json.dumps({"x1": 100, "y1": 200, "x2": 300, "y2": 200}).encode()
        ),
    )
    solution = solver.solve(b"\x89PNG-fake", "login_btn")

    assert solution.coords() == "100,200,300,200"
    req, timeout = captured[0]
    assert timeout == 7
    assert req.get_header("Authorization") == "Bearer vlm-sk"
    body = json.loads(req.data)
    assert body["challenge_hint"] == "login_btn"
    assert body["image"]  # base64 非空
    assert solution.action == "drag"


def test_http_vlm_solver_http_error_raises():
    """服务端 4xx/5xx 抛 VlmError，协调器按 PAUSED 降级。"""

    def _urlopen(req, timeout=10.0):
        raise urllib.error.HTTPError(req.full_url, 502, "bad gateway", {}, None)

    solver = HttpVlmSolver("https://solver.example.com/v1/solve", urlopen=_urlopen)
    try:
        solver.solve(b"png", "login_btn")
    except VlmError as exc:
        assert "http_solve_failed" in str(exc)
    else:
        raise AssertionError("应抛 VlmError")


def test_http_vlm_solver_invalid_response_raises():
    """响应缺坐标字段 → VlmError，不产生脏坐标。"""
    solver = HttpVlmSolver(
        "https://solver.example.com/v1/solve",
        urlopen=_capture_urlopen([], b'{"action":"drag"}'),
    )
    try:
        solver.solve(b"png", "login_btn")
    except VlmError as exc:
        assert "invalid_solver_response" in str(exc)
    else:
        raise AssertionError("应抛 VlmError")


def test_build_vlm_solver_factory():
    """未配置 → None（PAUSED 降级）；mock → Mock；http → Http；缺 endpoint 报错。"""
    assert build_vlm_solver({}) is None
    assert build_vlm_solver({"vlm.provider": "mock"}) is not None
    solver = build_vlm_solver(
        {"vlm.provider": "http", "vlm.endpoint": "https://x/v1/solve", "vlm.api_key": "k"}
    )
    assert isinstance(solver, HttpVlmSolver)
    try:
        build_vlm_solver({"vlm.provider": "http"})
    except VlmError as exc:
        assert "endpoint" in str(exc)
    else:
        raise AssertionError("provider=http 缺 endpoint 应报错")
