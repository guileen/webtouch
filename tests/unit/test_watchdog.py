"""LLM 预算看门狗单元测试（TDD-WATCH-02，tdd-checklist.md §6.5）。"""

from pipeline.watchdog import TokenBucket


class _FakeClock:
    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


def test_tdd_watch_02_token_bucket():
    clock = _FakeClock()
    bucket = TokenBucket(capacity=20, refill_per_sec=2.0, clock=clock)

    # 突发容量 20 Token：一次性可连取 20
    for _ in range(20):
        assert bucket.try_acquire(1) is True
    assert bucket.try_acquire(1) is False

    # 每秒恢复 2：0.5s 后恢复 1 个
    clock.now += 0.5
    assert bucket.try_acquire(1) is True
    assert bucket.try_acquire(1) is False

    # 再等 1s 恢复 2 个，可一次性取 2
    clock.now += 1.0
    assert bucket.try_acquire(2) is True
    assert bucket.try_acquire(1) is False
