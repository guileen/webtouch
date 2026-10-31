// 物理动作预算（时长上界）集中收口。
//
// 预算只约束「这一次物理动作多久必须收口」，不改变任何派发语义：预算到点仍按
// canceledObserved(ctx, dispatched) 如实区分「未派发可重放」与「已派发结果未知」，
// 由 pkg/engine/interrupt.go 统一翻译给调用方（详见 docs/gateway-contract-2026-08-26.md §6）。
package semantics

import "time"

// ActionBudget 是单次物理动作的默认预算上限：调用方没给（或给了更长的）deadline 时，
// 动作仍在 8 秒内收口，不会比历史行为更宽松。
const ActionBudget = 8 * time.Second

// SwitchActionBudget 是多阶段物理切换语义（模型 / 思考开关 / DeepSeek 模式）的预算上限。
//
// 这些语义一次调用里包含两段独立的有界等待（选择器 ≤5s、菜单项 ≤5s）、两次拟人轨迹
// 点击与一次落地回读，失败时还会整轮重开一次；8s 默认预算会被这条合法序列自身吃穿。
// 更糟的是超时点落在 disp.mark() 之后（菜单已定位、选项已选定），于是被如实归类成
// 不可重放的 physical_event_dispatched_outcome_unknown —— 把「切换很可能已生效」报成
// 「结果未知」（2026-09-26 WD-DEBATE-R1 现场第三次失败即此形态）。放宽的只是时长上界。
const SwitchActionBudget = 25 * time.Second

// multiPhaseSwitchSemantics 是需要多阶段物理序列的站点切换语义集合。
var multiPhaseSwitchSemantics = map[string]bool{
	"gemini_model_switch":    true,
	"gemini_thinking_switch": true,
	"deepseek_mode_switch":   true,
}

// BudgetFor 返回该语义的单次物理动作预算：多阶段切换语义用放宽后的上界，
// 其余（含未知语义）保持默认 8 秒。
func BudgetFor(semantic string) time.Duration {
	if multiPhaseSwitchSemantics[semantic] {
		return SwitchActionBudget
	}
	return ActionBudget
}

// siteControlLandingSemantics 是「落点自证不适用」的站点控件语义集合（§5.0.9 条款 8）。
//
// 这些语义的标识是**引擎/网关侧内部控件名**——页面上不存在这个字符串，落点元素的
// 可见文本是站点固定的 UI 文案（真机 2026-10-01：模型选择器 `Flash`、发送按钮
// `Send message`）。文本互含判据（条款 6/7）对它们没有判别力：站点专有文案按
// §5.0.2 纪律不得进通用别名表，判 miss 只会造出假失败并触发「不得换出口」硬失败。
//
// 准入两条（须同时满足，新增前先在 spec/algorithm-kernel.md §5.0.9 条款 8 登记）：
// 其一，语义标识是引擎/网关侧内部控件名（页面上不存在该字符串）；其二，该语义在
// 物理层有专用后验（切换＝选择器回读 ＋ 菜单自检；发送/新对话＝ pkg/browser/input/
// recovery.go::verifyGeminiClick 的输入区/回答区清空回读）。
//
// 集合在此单一维护，供引擎侧决策「落点自证是否适用」。
const (
	SemanticGeminiModelSwitch      = "gemini_model_switch"
	SemanticGeminiThinkingSwitch   = "gemini_thinking_switch"
	SemanticDeepSeekModeSwitch     = "deepseek_mode_switch"
	SemanticDeepSeekThinkingSwitch = "deepseek_thinking_switch"
	SemanticGeminiSend             = "gemini_send"
	SemanticGeminiNewChat          = "gemini_new_chat"
)

var siteControlLandingSemantics = map[string]bool{
	// 状态型切换语义：落点文本＝当前档位/开关状态；后验＝选择器回读 ＋ 菜单自检。
	SemanticGeminiModelSwitch:      true,
	SemanticGeminiThinkingSwitch:   true,
	SemanticDeepSeekModeSwitch:     true,
	SemanticDeepSeekThinkingSwitch: true,
	// 站点固定控件：落点文本＝站点 UI 标签；后验＝verifyGeminiClick。
	SemanticGeminiSend:    true,
	SemanticGeminiNewChat: true,
}

// IsSiteControlSemantic 报告该语义是否属站点控件语义（§5.0.9 条款 8：不适用文本互含
// 落点自证，一律归证据不足；正确性由其专用后验判定）。
func IsSiteControlSemantic(semantic string) bool {
	return siteControlLandingSemantics[semantic]
}
