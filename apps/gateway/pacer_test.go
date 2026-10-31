package gateway

import (
	"context"
	"testing"
	"time"
)

func TestHumanPacer_JitterDelay(t *testing.T) {
	cfg := HumanPacerConfig{
		MinInterTurnDelay: 2.0,
		JitterFraction:    0.2, // ±20%
	}
	pacer := NewHumanPacer(cfg)

	base := 1000 * time.Millisecond
	for i := 0; i < 50; i++ {
		d := pacer.JitterDelay(base)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("JitterDelay 超出范围 [800ms, 1200ms]: %v", d)
		}
	}
}

func TestHumanPacer_InterTurnCooldown(t *testing.T) {
	cfg := HumanPacerConfig{
		MinInterTurnDelay: 1.0,
		JitterFraction:    0.0, // 无抖动便于测试
		MaxTurnsPerMinute: 10,
	}
	pacer := NewHumanPacer(cfg)

	var slept time.Duration
	pacer.SetSleepFunc(func(d time.Duration) {
		slept += d
	})

	sid := "sess_test_pacer"
	// 第 1 轮
	if err := pacer.BeforeTurn(context.Background(), sid); err != nil {
		t.Fatalf("BeforeTurn failed: %v", err)
	}
	if slept != 0 {
		t.Fatalf("首轮不应发生等待: slept=%v", slept)
	}
	pacer.AfterTurn(sid)

	// 立即发第 2 轮 -> 应当被要求等待约 1s
	if err := pacer.BeforeTurn(context.Background(), sid); err != nil {
		t.Fatalf("BeforeTurn failed: %v", err)
	}
	if slept < 900*time.Millisecond {
		t.Fatalf("次轮未能强制冷静期: slept=%v", slept)
	}
}

func TestHumanPacer_MaxTurnsPerMinute(t *testing.T) {
	cfg := HumanPacerConfig{
		MinInterTurnDelay: 0.01,
		JitterFraction:    0.0,
		MaxTurnsPerMinute: 3,
	}
	pacer := NewHumanPacer(cfg)

	var slept time.Duration
	pacer.SetSleepFunc(func(d time.Duration) {
		slept += d
	})

	sid := "sess_throttle"
	for i := 0; i < 3; i++ {
		_ = pacer.BeforeTurn(context.Background(), sid)
		pacer.AfterTurn(sid)
	}

	// 第 4 轮应当触发 1 分钟频控限流
	_ = pacer.BeforeTurn(context.Background(), sid)
	if slept < 50*time.Second {
		t.Fatalf("超出每分钟上限时应等待至少 50s: slept=%v", slept)
	}
}
