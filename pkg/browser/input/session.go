// 输入域会话能力面与派发标记面（复杂度治理·阶段 2·单元 2c，2026-09-29）
//
// 穿透寻底（原宿主 `pkg/browser/pierce_hit.go`）与点击/输入自愈（原宿主
// `pkg/browser/click_recovery.go`）迁入本包后不再直接持有 `*cdpSession`：宿主经
// `Session` 注入**实测用到的那几项能力**（形状与 2a 的 `Eval`、2b 的 `Caller` 对齐），
// 派发标记面用 `DispatchTracker`。本接口不含任何为搬运新造的能力——方法集＝该簇对
// 宿主的实测出边（13 ≤ 16）。
package input

import "context"

// Session 是宿主会话向「穿透寻底 + 物理输入自愈」簇暴露的最小能力面。
type Session interface {
	// Eval 是页面内求值（2a 已定义的最小面：`EvalValue`）。
	Eval
	// Call 把一条原始 CDP 命令发到本会话 target（穿透寻底/节点句柄用；形状同 2b Caller）。
	Call(ctx context.Context, method string, params map[string]any, out any) error
	// InsertText 走宿主「必达」护栏下发 `Input.insertText`（穿透出口的物理写值）。
	InsertText(ctx context.Context, text string) error
	// EvalBool 是页面内求值并读布尔结果。
	EvalBool(ctx context.Context, expr string) (bool, error)
	// FindElementMode 按语义/选择器定位（自愈闭环的定位层）。
	FindElementMode(ctx context.Context, semantic, selector string, preferLast, strictTarget, editableOnly bool) (ElementTarget, error)
	// CheckOcclusion 判定命中目标是否被遮挡。
	CheckOcclusion(ctx context.Context, t ElementTarget) (bool, error)
	// ClickAt 在已定位目标上派发物理点击。
	ClickAt(ctx context.Context, t ElementTarget) error
	// InputAt 是宿主唯一的输入派发点（受体守卫/可编辑门禁/重锚/落地回读都在宿主内）。
	InputAt(ctx context.Context, disp *DispatchTracker, t ElementTarget, action, value, semantic string) error
	// NotifyPreDispatchReceptor 在「受体已解析、物理事件未发出」时回传受体（§5.0.14 决议 0③）。
	NotifyPreDispatchReceptor(t ElementTarget)
	// CloseGeminiMenu 关闭 Gemini 菜单/弹层（点歪副作用恢复）。
	CloseGeminiMenu(ctx context.Context) error
	// GeminiInputEmpty 报告 Gemini 输入区是否已清空（发送类语义自检）。
	GeminiInputEmpty(ctx context.Context) bool
	// TargetDestroyed 报告标签页是否已被关闭（用户手动关 Tab）。
	TargetDestroyed() bool
	// RecordHit 回写「最近一次定位成功的命中元素」（宿主证据链回显）。
	RecordHit(t ElementTarget)
}

// DispatchTracker 记录「物理事件是否已经派发出去」（spec/algorithm-kernel.md §5.0.11
// 条款 1/2）。ctx 在派发后中断时，事件可能已经落地、结果未知——取消不能撤销已经发给
// Chrome 的鼠标/键盘事件，故上层据此区分「可安全重放」与「不得重放」。
//
// 零值可用；nil 表示调用方不关心中断分类（与宿主旧写法的 nil 语义一致）。
type DispatchTracker struct{ dispatched bool }

// Mark 在第一个物理事件即将发出之前调用（此前的中断都算「未派发」）。
func (d *DispatchTracker) Mark() { d.dispatched = true }

// Dispatched 报告本标记是否已置位。
func (d *DispatchTracker) Dispatched() bool { return d.dispatched }
