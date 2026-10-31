package challenge

import (
	"testing"
	"time"
)

func TestCircuitBreakerInitialState(t *testing.T) {
	cb := NewCircuitBreaker()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected initial state CLOSED, got %s", cb.State())
	}
	ok, err := cb.AllowAction()
	if !ok || err != nil {
		t.Fatalf("expected action allowed in CLOSED state, got ok=%v, err=%v", ok, err)
	}
}

func TestCircuitBreakerTripOnChallenge(t *testing.T) {
	cb := NewCircuitBreaker()
	sig := &ChallengeSignal{
		Type:       ChallengeTypePayPal,
		Provider:   "PayPal",
		Confidence: 0.99,
		Reason:     "Paypal challenge detected",
		DetectedAt: time.Now(),
	}

	cb.Trip(sig)
	if cb.State() != CircuitOpen {
		t.Fatalf("expected state OPEN after trip, got %s", cb.State())
	}

	ok, err := cb.AllowAction()
	if ok || err == nil {
		t.Fatalf("expected action blocked in OPEN state, got ok=%v, err=%v", ok, err)
	}
	if cb.LastSignal() != sig {
		t.Fatalf("expected last signal preserved, got %+v", cb.LastSignal())
	}
}

func TestCircuitBreakerAntiThrashingConsecutiveFailures(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerOptions{
		MaxConsecutiveFailures: 3,
		CooldownDuration:       1 * time.Second,
	})

	// 1st failure
	tripped := cb.RecordFailure("click no-op")
	if tripped || cb.State() != CircuitClosed {
		t.Fatalf("should not trip on 1st failure")
	}

	// 2nd failure
	tripped = cb.RecordFailure("click timeout")
	if tripped || cb.State() != CircuitClosed {
		t.Fatalf("should not trip on 2nd failure")
	}

	// 3rd failure -> Anti-thrashing trip!
	tripped = cb.RecordFailure("element detached")
	if !tripped || cb.State() != CircuitOpen {
		t.Fatalf("expected trip on 3rd failure, got tripped=%v, state=%s", tripped, cb.State())
	}

	// In OPEN state, action is blocked
	ok, err := cb.AllowAction()
	if ok || err == nil {
		t.Fatalf("expected AllowAction to fail when OPEN")
	}

	// Reset recovers to CLOSED
	cb.Reset()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected CLOSED after reset, got %s", cb.State())
	}
	ok, err = cb.AllowAction()
	if !ok || err != nil {
		t.Fatalf("expected AllowAction to succeed after reset")
	}
}

func TestCircuitBreakerCooldownTransition(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerOptions{
		MaxConsecutiveFailures: 2,
		CooldownDuration:       50 * time.Millisecond,
	})

	cb.Trip(nil)
	if cb.State() != CircuitOpen {
		t.Fatalf("expected OPEN")
	}

	time.Sleep(60 * time.Millisecond)

	// State should transition to HALF_OPEN after cooldown
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected HALF_OPEN after cooldown, got %s", cb.State())
	}

	// Allows 1 trial action in HALF_OPEN
	ok, err := cb.AllowAction()
	if !ok || err != nil {
		t.Fatalf("trial action should be allowed in HALF_OPEN")
	}

	// Concurrent second trial before result is rejected
	ok2, err2 := cb.AllowAction()
	if ok2 || err2 == nil {
		t.Fatalf("concurrent second trial should be rejected while trial in flight")
	}

	// If trial succeeds, resets to CLOSED
	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected CLOSED after success in HALF_OPEN, got %s", cb.State())
	}
}
