package challenge

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCircuitBreakerOpen 是熔断器处于开路阻断状态时返回的标准硬核错误。
var ErrCircuitBreakerOpen = errors.New("anti-thrashing circuit breaker is OPEN: automation frozen to prevent account ban")

// CircuitBreakerOptions 配置防乱点熔断中枢参数。
type CircuitBreakerOptions struct {
	MaxConsecutiveFailures int           // 触发防乱点熔断的连续失败次数阈值（默认 3 次）
	CooldownDuration       time.Duration // 熔断后进入半开试探态的冷却期（默认 30 秒）
}

// DefaultOptions 返回默认防乱点安全配置。
func DefaultOptions() CircuitBreakerOptions {
	return CircuitBreakerOptions{
		MaxConsecutiveFailures: 3,
		CooldownDuration:       30 * time.Second,
	}
}

// CircuitBreaker 是物理防乱点熔断器（Anti-thrashing Circuit Breaker）。
// 杜绝在 Paypal、Cloudflare 等严苛风控站点因未识别验证码而疯狂重试点击导致账号封禁。
type CircuitBreaker struct {
	mu sync.RWMutex

	state               CircuitState
	opts                CircuitBreakerOptions
	consecutiveFailures int
	lastSignal          *ChallengeSignal
	lastTripTime        time.Time
	trialInFlight       bool
}

// NewCircuitBreaker 创建新的防乱点熔断器。
func NewCircuitBreaker(opts ...CircuitBreakerOptions) *CircuitBreaker {
	opt := DefaultOptions()
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.MaxConsecutiveFailures <= 0 {
		opt.MaxConsecutiveFailures = 3
	}
	if opt.CooldownDuration <= 0 {
		opt.CooldownDuration = 30 * time.Second
	}
	return &CircuitBreaker{
		state: CircuitClosed,
		opts:  opt,
	}
}

// State 返回当前熔断器状态（自动处理冷却期超时转半开）。
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.checkStateTransitionLocked()
	return cb.state
}

// LastSignal 返回最近一次导致熔断的挑战证据。
func (cb *CircuitBreaker) LastSignal() *ChallengeSignal {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.lastSignal
}

// ConsecutiveFailures 返回当前同状态连续失败计数。
func (cb *CircuitBreaker) ConsecutiveFailures() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.consecutiveFailures
}

// AllowAction 护栏门禁：在发起任何物理动作或自动化重试前调用。
// 若熔断器处于 OPEN 态，立即拒绝执行，零物理损耗。
func (cb *CircuitBreaker) AllowAction() (bool, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.checkStateTransitionLocked()

	switch cb.state {
	case CircuitClosed:
		return true, nil
	case CircuitHalfOpen:
		if !cb.trialInFlight {
			cb.trialInFlight = true
			return true, nil
		}
		return false, fmt.Errorf("%w (trial action already in flight)", ErrCircuitBreakerOpen)
	case CircuitOpen:
		sigStr := "thrashing prevention"
		if cb.lastSignal != nil {
			sigStr = cb.lastSignal.String()
		}
		return false, fmt.Errorf("%w (tripped by %s at %s)", ErrCircuitBreakerOpen, sigStr, cb.lastTripTime.Format("15:04:05"))
	default:
		return true, nil
	}
}

// Trip 立即拉响防乱点熔断闸门，冻结一切物理动作。
func (cb *CircuitBreaker) Trip(signal *ChallengeSignal) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.state = CircuitOpen
	cb.lastTripTime = time.Now()
	cb.trialInFlight = false
	if signal != nil {
		cb.lastSignal = signal
	} else {
		cb.lastSignal = &ChallengeSignal{
			Type:       ChallengeTypeGenericWAF,
			Provider:   ProviderCircuitBreaker,
			Confidence: 1.0,
			Reason:     "人工或外部策略主动拉起物理熔断保护",
			DetectedAt: cb.lastTripTime,
		}
	}
}

// RecordFailure 记录一次操作失败；若连续失败达到阈值，自动触发防乱点熔断。
func (cb *CircuitBreaker) RecordFailure(reason string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.trialInFlight = false
	cb.consecutiveFailures++

	if cb.state == CircuitHalfOpen || cb.consecutiveFailures >= cb.opts.MaxConsecutiveFailures {
		cb.state = CircuitOpen
		cb.lastTripTime = time.Now()
		cb.lastSignal = &ChallengeSignal{
			Type:       ChallengeTypeGenericWAF,
			Provider:   ProviderAntiThrashing,
			Confidence: 0.95,
			Reason:     fmt.Sprintf("连续 %d 次操作失败无因果进展 (%s)，触发防频繁乱点熔断保护", cb.consecutiveFailures, reason),
			DetectedAt: cb.lastTripTime,
		}
		return true
	}
	return false
}

// RecordSuccess 记录一次操作成功并产生因果进展；复位失败计数，若处于半开则恢复闭合。
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures = 0
	cb.trialInFlight = false
	if cb.state == CircuitHalfOpen || cb.state == CircuitOpen {
		cb.state = CircuitClosed
		cb.lastSignal = nil
	}
}

// Reset 手动复位熔断器至闭合正常态。
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.state = CircuitClosed
	cb.consecutiveFailures = 0
	cb.lastSignal = nil
	cb.trialInFlight = false
}

func (cb *CircuitBreaker) checkStateTransitionLocked() {
	if cb.state == CircuitOpen && time.Since(cb.lastTripTime) >= cb.opts.CooldownDuration {
		cb.state = CircuitHalfOpen
		cb.trialInFlight = false
	}
}
