package challenge

import "testing"

// TestIsHumanChallenge 守住「熔断 ≠ 验证码」的判定边界（2026-09-23 误报修复）：
// 只有页面真实存在的挑战信号才算人机验证；熔断器自产信号一律不算。
func TestIsHumanChallenge(t *testing.T) {
	cases := []struct {
		name string
		sig  *ChallengeSignal
		want bool
	}{
		{"nil 信号未知 → 不声称有挑战", nil, false},
		{"连续失败防乱点熔断", &ChallengeSignal{Provider: ProviderAntiThrashing}, false},
		{"人工/外部拉起熔断", &ChallengeSignal{Provider: ProviderCircuitBreaker}, false},
		{"探针命中 Cloudflare", &ChallengeSignal{Provider: "Cloudflare", Type: ChallengeTypeCloudflareTurnstile}, true},
		{"探针命中 hCaptcha", &ChallengeSignal{Provider: "hCaptcha", Type: ChallengeTypeHCaptcha}, true},
		{"物理层遭遇安全拦截", &ChallengeSignal{Provider: "PhysicalLayer"}, true},
	}
	for _, c := range cases {
		if got := c.sig.IsHumanChallenge(); got != c.want {
			t.Errorf("%s: IsHumanChallenge()=%v, want %v", c.name, got, c.want)
		}
	}
}
