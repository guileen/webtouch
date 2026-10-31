package browser

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"uws/pkg/browser/find"
	"uws/pkg/browser/input"
)

// elementTarget 是语义定位命中的元素几何与身份。类型源在 `input.ElementTarget`
// （复杂度治理·阶段 2·单元 2b：输入域的落点采样与 find 域共用同一类型），此处以
// 别名引用 ⇒ 调用点零改动。
type elementTarget = input.ElementTarget

// 定位域适配（域逻辑在 `pkg/browser/find`）：本文件只留「宿主注入 ＋ 命中落账」，
// 域内不持有会话状态。形态同 `bridgeInputSession`/`tier2SessionBridge`（薄包装折叠进
// 既有文件，不新开转发文件 ⇒ 条目数不增）。

// findBridge 把宿主求值面暴露给定位域（`find.Evaluator` 的单项能力）。
type findBridge struct{ s *cdpSession }

func (b findBridge) EvalRaw(ctx context.Context, expr string) (json.RawMessage, error) {
	return b.s.evalRaw(ctx, expr)
}

// findElementOpt 定位可见交互元素：selector 非空时先按规则选择器精确过滤
// （querySelectorAll），再在命中集合内做语义评分择优；选择器命中但无语义分
// （如纯 SVG 图标按钮）时回退到第一个可见命中，解决「规则选择器 → 物理层」
// 接线缺口（DeepSeek chat 场景实测：输入框/发送按钮/回答区均需选择器）。
//
// 2026-08-25 多轮/新会话修复：
//   - preferLast=true（extract 动作）：无语义分回退与语义平分均取「最后一个」
//     命中（回答块流式追加语义，取第一个在多轮下必是旧回答）；
//   - selector 以 text= 前缀开头时改为按「精确文本」定位最小可见元素
//     （DeepSeek「开启新对话」是纯 div，无 role/button/onclick，常规扫描抓不到）。
//
// 复杂度治理·阶段 3 第一刀：扫描逻辑（含 340 行页内 JS）与严格/可编辑口径已下沉
// `find.Scan`，此处只做宿主注入与命中落账——`lastHit` 归宿主（跨域证据状态：
// find 写入、action 侧 enrichHitSelector 补全、pkg/engine 经 LastHitElement() 读取）。
func findElementOpt(ctx context.Context, s *cdpSession, semantic, selector string, preferLast bool) (elementTarget, error) {
	return scanHit(ctx, s, semantic, selector, preferLast, false, false)
}

// findElementMode 是 findElementOpt 的严格模式变体（2026-09-23 语义抽取门禁）：
// strictTarget=true 只认「元素自身文本 == 目标」（100）与「元素文本包含目标」（80），
// editableOnly=true 只接受可编辑目标（2026-09-25，spec/algorithm-kernel.md §5.0.3）——
// 两档口径的完整说明与根因见 `find.Scan` 文档，本处保持原调用面不变。
func findElementMode(ctx context.Context, s *cdpSession, semantic, selector string, preferLast, strictTarget, editableOnly bool) (elementTarget, error) {
	return scanHit(ctx, s, semantic, selector, preferLast, strictTarget, editableOnly)
}

// scanHit 调定位域并把命中写回会话证据链（域内不持状态）。
func scanHit(ctx context.Context, s *cdpSession, semantic, selector string, preferLast, strictTarget, editableOnly bool) (elementTarget, error) {
	t, err := find.Scan(ctx, findBridge{s}, semantic, selector, preferLast, strictTarget, editableOnly)
	if err != nil {
		return t, err
	}
	recordHit(s, t)
	return t, nil
}

// resolveNodeById 按探针 node_id（BFS 元素节点索引）回查目标元素，返回与 findElementOpt
// 同构的 elementTarget（marker + rect + tag/text）。node_id 越界 / 元素不可见返回
// Found=false（不抛错，调用方按 node_not_found）。表达式与遍历口径见 `find.NodeByID`。
func resolveNodeById(ctx context.Context, s *cdpSession, nodeID int32) (elementTarget, error) {
	t, err := find.NodeByID(ctx, findBridge{s}, nodeID)
	if err != nil {
		return t, err
	}
	recordHit(s, t)
	return t, nil
}

// recordHit 把定位命中写回证据链（仅 Found 才写，与搬移前逐行等价）。
func recordHit(s *cdpSession, t elementTarget) {
	if !t.Found {
		return
	}
	s.mu.Lock()
	s.lastHit = t
	s.mu.Unlock()
}

// LastHitElement 返回最近一次定位成功的命中元素证据（v1.5.4 HitElementSource）。
// 语义/选择器定位时 rect/text/tag 由 findElementOpt 填充，selector 由
// enrichHitSelector 在动作执行前补全（CSS 路径 = 点击点 elementFromPoint 真身）。
func (s *cdpSession) LastHitElement() (HitElement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.lastHit
	if !t.Found {
		return HitElement{}, false
	}
	return HitElement{
		Selector: t.Sel,
		Tag:      t.Tag,
		X:        t.X, Y: t.Y, W: t.W, H: t.H,
		Text: t.Text,
	}, true
}

// enrichHitSelector 补全最近命中元素的 CSS 路径：以命中点 elementFromPoint 解析
// 真身（与 ClickAt 同源），避免「定位到容器、点击点却是子节点」时证据失真。
func (s *cdpSession) enrichHitSelector(ctx context.Context) {
	s.mu.Lock()
	t := s.lastHit
	s.mu.Unlock()
	if !t.Found {
		return
	}
	sel, err := s.elementPathAt(ctx, int(t.X), int(t.Y))
	if err != nil || sel == "" {
		return
	}
	s.mu.Lock()
	s.lastHit.Sel = sel
	s.mu.Unlock()
}

// checkOcclusion 检测目标中心是否被其他元素遮挡（TDD-ENG-04）。
func (s *cdpSession) checkOcclusion(ctx context.Context, t elementTarget) (bool, error) {
	const js = `(() => {
	  const x = X, y = Y;
	  const el = document.elementFromPoint(x, y);
	  if (!el) return true;
	  const t = el.closest ? (el.closest('[data-uws-target="1"]') ||
	           (el.getAttribute && el.getAttribute('data-uws-target') === '1' ? el : null)) : null;
	  return !t;
	})()`
	expr := strings.ReplaceAll(strings.ReplaceAll(js, "X", strconv.FormatFloat(t.X, 'f', 1, 64)),
		"Y", strconv.FormatFloat(t.Y, 'f', 1, 64))
	occluded, err := s.evalBool(ctx, expr)
	if err != nil {
		return false, err
	}
	return occluded, nil
}
