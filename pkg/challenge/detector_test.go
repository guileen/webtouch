package challenge

import (
	"context"
	"encoding/json"
	"testing"
)

type mockEvaluator struct {
	result []byte
	err    error
}

func (m *mockEvaluator) EvaluateScript(ctx context.Context, expr string) ([]byte, error) {
	return m.result, m.err
}

func TestDetectorStaticPayPal(t *testing.T) {
	d := NewDetector()
	sig := d.DetectStatic("https://www.paypal.com/auth/challenge?ctx=123", "PayPal Checkout", 200, nil)
	if sig == nil {
		t.Fatal("expected paypal challenge signal, got nil")
	}
	if sig.Type != ChallengeTypePayPal || sig.Provider != "PayPal" {
		t.Fatalf("unexpected sig: %+v", sig)
	}
	if sig.Confidence < 0.9 {
		t.Fatalf("confidence too low: %f", sig.Confidence)
	}
}

func TestDetectorStaticCloudflare(t *testing.T) {
	d := NewDetector()
	// 5s 盾标题
	sig1 := d.DetectStatic("https://example.com/login", "Just a moment...", 200, nil)
	if sig1 == nil || sig1.Type != ChallengeTypeCloudflareInterstitial {
		t.Fatalf("expected cloudflare 5s interstitial, got %+v", sig1)
	}

	// 状态码 403 + cf-mitigated 头
	headers := map[string]string{
		"cf-mitigated": "challenge",
		"server":       "cloudflare",
	}
	sig2 := d.DetectStatic("https://example.com/api", "Access Denied", 403, headers)
	if sig2 == nil || sig2.Type != ChallengeTypeCloudflareTurnstile {
		t.Fatalf("expected cloudflare turnstile, got %+v", sig2)
	}
}

func TestDetectorStaticArkoseRecaptchaHCaptcha(t *testing.T) {
	d := NewDetector()

	sigArkose := d.DetectStatic("https://client-api.arkoselabs.com/fc/api/", "", 200, nil)
	if sigArkose == nil || sigArkose.Type != ChallengeTypeArkose {
		t.Fatalf("expected arkose, got %+v", sigArkose)
	}

	sigRecaptcha := d.DetectStatic("https://www.google.com/recaptcha/api2/anchor", "", 200, nil)
	if sigRecaptcha == nil || sigRecaptcha.Type != ChallengeTypeRecaptcha {
		t.Fatalf("expected recaptcha, got %+v", sigRecaptcha)
	}

	sigHCaptcha := d.DetectStatic("https://hcaptcha.com/1/api.js", "", 200, nil)
	if sigHCaptcha == nil || sigHCaptcha.Type != ChallengeTypeHCaptcha {
		t.Fatalf("expected hcaptcha, got %+v", sigHCaptcha)
	}
}

func TestDetectorDOMProbe(t *testing.T) {
	d := NewDetector()

	probeJSON := map[string]any{
		"type":       "cloudflare_turnstile",
		"provider":   "Cloudflare",
		"selector":   "div#cf-turnstile-wrapper",
		"reason":     "DOM 命中 Cloudflare Turnstile 质检盾",
		"confidence": 0.99,
	}
	b, _ := json.Marshal(probeJSON)
	quoted, _ := json.Marshal(string(b))

	mockEval := &mockEvaluator{result: quoted}
	sig := d.Detect(context.Background(), mockEval, "https://example.com/checkout", "My Order", 200, nil)
	if sig == nil {
		t.Fatal("expected DOM probe signal, got nil")
	}
	if sig.Type != ChallengeTypeCloudflareTurnstile || sig.Selector != "div#cf-turnstile-wrapper" {
		t.Fatalf("unexpected DOM probe sig: %+v", sig)
	}
}
