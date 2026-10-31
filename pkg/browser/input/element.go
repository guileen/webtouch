// 语义定位命中元素的几何与身份（类型源）
//
// 复杂度治理·阶段 2·单元 2b（2026-09-29）：本类型自宿主 `pkg/browser/cdp_session_find.go`
// 迁入，宿主以别名 `type elementTarget = input.ElementTarget` 引用（字段全导出、无方法，
// 故调用点零改动）。落点采样（输入域策略）与语义定位（find 域）共用同一类型，避免
// 跨包转换与两份字段定义；依赖方向 find → input，不成环。
package input

// ElementTarget 是一次语义定位命中的元素：视口 CSS 几何 ＋ 身份（tag/selector/文本）。
type ElementTarget struct {
	Found bool    `json:"found"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Text  string  `json:"text"`
	Tag   string  `json:"tag"`
	Sel   string  `json:"selector"`
	// Matched 是本次命中真正匹配上的候选文案（spec/algorithm-kernel.md §5.0.2
	// 条款 6：别名展开后必须能区分「命中的候选」与元素文本，供日志/复测归因）。
	Matched string `json:"matched"`
	// Editable 是命中元素是否可编辑（spec/algorithm-kernel.md §5.0.3 条款 1：
	// input/fill/select 文本汇支路只接受可编辑目标，否则必须明确失败）。
	Editable bool `json:"editable"`
}
