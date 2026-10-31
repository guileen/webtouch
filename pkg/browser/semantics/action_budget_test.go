package semantics

import "testing"

// TestActionBudgetFor 锁定多阶段切换语义的预算放宽：这些语义一次调用含两段有界等待
// 与两次拟人轨迹点击，默认 8s 会被合法序列自身吃穿（超时点又在 disp.mark() 之后，
// 会误报成不可重放的 outcome_unknown）；其余语义必须保持 8s 不变。
func TestActionBudgetFor(t *testing.T) {
	for _, semantic := range []string{
		"gemini_model_switch", "gemini_thinking_switch", "deepseek_mode_switch",
	} {
		if got := BudgetFor(semantic); got != SwitchActionBudget {
			t.Errorf("BudgetFor(%q) = %v, want %v", semantic, got, SwitchActionBudget)
		}
	}
	for _, semantic := range []string{
		"", "click", "input", "extract", "gemini_search_switch", "deepseek_search_switch",
	} {
		if got := BudgetFor(semantic); got != ActionBudget {
			t.Errorf("BudgetFor(%q) = %v, want %v", semantic, got, ActionBudget)
		}
	}
	if SwitchActionBudget <= ActionBudget {
		t.Errorf("多阶段切换预算 %v 未放宽（默认 %v）", SwitchActionBudget, ActionBudget)
	}
}
