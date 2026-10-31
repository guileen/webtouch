package gateway

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// HumanPacerConfig 定义拟人化频率与速度控制参数。
type HumanPacerConfig struct {
	// MinInterTurnDelay 单会话连续两轮交互的基准最小间隔（秒，默认 2.5s）。
	MinInterTurnDelay float64
	// JitterFraction 随机抖动比例（0.0 ~ 1.0，默认 0.3，即 ±30% 浮动）。
	JitterFraction float64
	// MaxTurnsPerMinute 单会话每分钟最大交互轮次（默认 8 轮，超出将强制排队等待）。
	MaxTurnsPerMinute int
	// InputToSendDelay 输入完成到点击发送按钮之间的拟人视觉确认延迟基准（秒，默认 1.5s）。
	InputToSendDelay float64
}

// DefaultHumanPacerConfig 返回默认的拟人化频控参数（2026-10-03 收紧：网页端严格控频，防禁言封号）。
func DefaultHumanPacerConfig() HumanPacerConfig {
	return HumanPacerConfig{
		MinInterTurnDelay: 15.0,
		JitterFraction:    0.3,
		MaxTurnsPerMinute: 2,
		InputToSendDelay:  2.0,
	}
}

// sessionPaceRecord 记录单会话的时间戳历史。
type sessionPaceRecord struct {
	lastTurnEnd time.Time
	turnTimes   []time.Time
}

// HumanPacer 是面向浏览器会话的拟人化节律与速率控制器。
// 严禁以超出人类生理与正常认知习惯的机械化超人速在同一页面上连续操作。
type HumanPacer struct {
	mu      sync.Mutex
	cfg     HumanPacerConfig
	records map[string]*sessionPaceRecord
	rng     *rand.Rand
	sleepFn func(time.Duration)
}

// NewHumanPacer 创建拟人化频控器。
func NewHumanPacer(cfg HumanPacerConfig) *HumanPacer {
	if cfg.MinInterTurnDelay <= 0 {
		cfg.MinInterTurnDelay = 2.5
	}
	if cfg.JitterFraction < 0 || cfg.JitterFraction > 1.0 {
		cfg.JitterFraction = 0.3
	}
	if cfg.MaxTurnsPerMinute <= 0 {
		cfg.MaxTurnsPerMinute = 8
	}
	if cfg.InputToSendDelay <= 0 {
		cfg.InputToSendDelay = 1.5
	}

	return &HumanPacer{
		cfg:     cfg,
		records: make(map[string]*sessionPaceRecord),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		sleepFn: time.Sleep,
	}
}

// SetSleepFunc 允许在单测中注入 sleep 模拟函数。
func (p *HumanPacer) SetSleepFunc(fn func(time.Duration)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sleepFn = fn
}

// JitterDelay 在基准时间上追加正负随机扰动（杜绝机械式固化毫秒）。
func (p *HumanPacer) JitterDelay(base time.Duration) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jitterDelayLocked(base)
}

func (p *HumanPacer) jitterDelayLocked(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	frac := p.cfg.JitterFraction
	if frac == 0 {
		return base
	}
	delta := (p.rng.Float64()*2.0 - 1.0) * frac
	multiplier := 1.0 + delta
	if multiplier < 0.2 {
		multiplier = 0.2
	}
	return time.Duration(float64(base) * multiplier)
}

// BeforeTurn 在向指定会话发起新一轮请求前执行拟人化节奏约束。
// 若距离上轮完成过近或触发每分钟上限，将进行微等待，确保符合真人节奏。
func (p *HumanPacer) BeforeTurn(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		sessionID = DefaultSessionID
	}

	p.mu.Lock()
	rec, ok := p.records[sessionID]
	if !ok {
		rec = &sessionPaceRecord{}
		p.records[sessionID] = rec
	}

	now := time.Now()

	// 1. 清理 1 分钟以前的历史轮次记录
	cutoff := now.Add(-1 * time.Minute)
	validIdx := 0
	for i, t := range rec.turnTimes {
		if t.After(cutoff) {
			validIdx = i
			break
		}
		if i == len(rec.turnTimes)-1 {
			validIdx = len(rec.turnTimes)
		}
	}
	rec.turnTimes = rec.turnTimes[validIdx:]

	// 2. 检查每分钟频次上限
	var sleepDuration time.Duration
	if len(rec.turnTimes) >= p.cfg.MaxTurnsPerMinute {
		oldest := rec.turnTimes[0]
		waitToClear := oldest.Add(1*time.Minute).Sub(now) + 100*time.Millisecond
		if waitToClear > sleepDuration {
			sleepDuration = waitToClear
		}
	}

	// 3. 检查轮次间最小冷静期（带 Jitter）
	if !rec.lastTurnEnd.IsZero() {
		elapsed := now.Sub(rec.lastTurnEnd)
		baseDelay := time.Duration(p.cfg.MinInterTurnDelay * float64(time.Second))
		targetInterval := p.jitterDelayLocked(baseDelay)
		if elapsed < targetInterval {
			rem := targetInterval - elapsed
			if rem > sleepDuration {
				sleepDuration = rem
			}
		}
	}

	sleepFn := p.sleepFn
	p.mu.Unlock()

	// 4. 执行等待
	if sleepDuration > 0 {
		if sleepFn != nil {
			sleepFn(sleepDuration)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	return nil
}

// AfterInputSettle 在输入框填入内容后、点击发送按钮前，提供自然的拟人停留（阅读/确认延迟）。
func (p *HumanPacer) AfterInputSettle(ctx context.Context, customSettleSec float64) error {
	baseSec := p.cfg.InputToSendDelay
	if customSettleSec > 0 {
		baseSec = customSettleSec
	}
	base := time.Duration(baseSec * float64(time.Second))
	jittered := p.JitterDelay(base)

	p.mu.Lock()
	sleepFn := p.sleepFn
	p.mu.Unlock()

	if sleepFn != nil {
		sleepFn(jittered)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// AfterTurn 在一轮交互完成后标记结束时间，作为下一轮冷静期的基准。
func (p *HumanPacer) AfterTurn(sessionID string) {
	if sessionID == "" {
		sessionID = DefaultSessionID
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	rec, ok := p.records[sessionID]
	if !ok {
		rec = &sessionPaceRecord{}
		p.records[sessionID] = rec
	}

	now := time.Now()
	rec.lastTurnEnd = now
	rec.turnTimes = append(rec.turnTimes, now)
}
