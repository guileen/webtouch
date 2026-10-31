package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"uws/pkg/browser"
	"uws/pkg/matcher"
	"uws/pkg/vlm"

	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// vlmEnabled 报告 VLM 兜底是否可用（配置启用 + 视觉客户端就绪）。
func (s *Server) vlmEnabled() bool {
	return s.vlmFb != nil && s.vlmFb.Enabled && s.vlmFb.Client != nil
}

// executeWithSelector 按选择器执行物理动作（学习存储回写路径）；无选择器或会话
// 不支持时降级纯语义。executePhysical 与 VLM 兜底共用。
func (s *Server) executeWithSelector(ctx context.Context, bc *browser.BrowserContext, action, value, semantic, selector string) (bool, string) {
	if selector != "" {
		if as, ok := bc.Session().(browser.SelectorActionSession); ok {
			return as.ExecuteActionWithSelector(ctx, action, value, semantic, selector)
		}
	}
	if as, ok := bc.Session().(browser.ActionSession); ok {
		return as.ExecuteAction(ctx, action, value, semantic)
	}
	return true, "probe-only"
}

// vlmOutcome 标记一次 VLM 兜底的结局，供调用方区分失败信号（GLOBAL §5 #50 反哺 c）。
type vlmOutcome int

// vlmCanceledObserved 提取物理原语回报的中断观测串（canceled_before/after_dispatch）。
// VLM 主控路径用的物理原语（executeWithSelector 的 ExecuteAction、VLMProbeSession 的
// ClickAt/InputAt）与主路径共用同一套中断语义：一旦中断，必须原样上抛到引擎唯一的
// 中断分类点，而不能被当成「这个候选不行，再试下一个」——否则「事件已派发但结果未知」
// 会被吞成普通失败，上层重试就把一次点击变成两次（2026-09-23 审查结论）。
func vlmCanceledObserved(observed string) (string, bool) {
	if canceled, _ := browser.IsCanceledObserved(observed); canceled {
		return observed, true
	}
	return "", false
}

const (
	vlmOutcomeSkipped   vlmOutcome = iota // VLM 未启用/会话不支持截图：调用方保持原 NOT_FOUND
	vlmOutcomeSuccess                     // VLM 已确定性验证执行成功
	vlmOutcomeTransient                   // VLM 调用出错/解析失败（非「无解」结论）：调用方保持 NOT_FOUND
	vlmOutcomeExhausted                   // VLM 主控循环跑到结论（无解/轮数上限）：调用方返回区分信号
)

// errVLMExhausted 是「VLM 主控已运行但无解」的哨兵错误，调用方据此返回 Aborted 而非 NotFound。
var errVLMExhausted = errors.New("vlm: main-control exhausted")

// FaultVLMActionMismatch：VLM 自报动作与请求动作不相容（§5.0.8 条款 1 代号表 /
// §5.0.9 条款 4）。熔断必须发生在派发前，不得派发、不得写 learned、不得换出口重试。
const FaultVLMActionMismatch = "VLM_ACTION_MISMATCH"

// vlmExhaustedError 携带结构化失败代号（§5.0.8 条款 2）：让 Aborted 回执保留
// `TARGET_ROLE_MISMATCH` / `vlm_landing_unverified`，而不是被压成通用「主控耗尽」。
type vlmExhaustedError struct{ code string }

func (e *vlmExhaustedError) Error() string {
	if e.code != "" {
		return "vlm main-control exhausted: " + e.code
	}
	return "vlm main-control exhausted"
}

func (e *vlmExhaustedError) Is(target error) bool { return target == errVLMExhausted }

// vlmHardFailCode 提取硬失败代号（守卫熔断 / 落地未验证），无则返回空串。
func vlmHardFailCode(observed string) string {
	for _, code := range []string{browser.FaultTargetRoleMismatch, browser.FaultVLMlandingUnverified, browser.FaultTargetTopologyRupture, FaultVLMActionMismatch, FaultClickLandingMismatch} {
		if strings.Contains(observed, code) {
			return code
		}
	}
	return ""
}

// vlmObservationBacksAction 校验观测串是否携带「请求动作」对应的页面侧证据
// （§5.0.9 条款 5 成功出口证据门槛）：真相只认页面侧证据，生产者自报的
// vlmOutcomeSuccess 不足以单独决定成功出口。
func vlmObservationBacksAction(expected, observed string) bool {
	if observed == "" {
		return false
	}
	head, _, _ := strings.Cut(observed, ":")
	switch expected {
	case "input", "fill", "select":
		return head == "input" || head == "input_at" || head == "fill" || head == "select"
	case "click":
		// 落点身份由原语随成功一并返回（sel），出口门槛只校验动作族（§5.0.9 条款 5）；
		// hit_element 级落点相关性属 T-3 门禁。
		return head == "click" || head == "click_at"
	case "extract":
		return true // 提取结果本身就是证据（ActionExtract / Done 的文本）
	default:
		return true
	}
}

// vlmActionMismatch 判定 VLM 自报动作是否与请求动作相容（§5.0.9 条款 4 意图-动作
// 相容性守卫）；不相容返回非空原因串，调用方必须在**派发前**熔断。
//
// click / input / extract 三个动作各自携带执行语义（坐标点击 / 填值 / 取文本），
// 一旦被无条件采信就会绕过请求动作的证据判据；done / crop_zoom / back / impossible
// 是控制流动作，不决定执行语义（done 的执行仍按请求动作及其证据判据），不受约束。
func vlmActionMismatch(expected, got string) string {
	switch got {
	case vlm.ActionClick:
		if expected != "click" {
			return "VLM 自报 click"
		}
	case vlm.ActionInput:
		if expected != "input" && expected != "fill" && expected != "select" {
			return "VLM 自报 input"
		}
	case vlm.ActionExtract:
		if expected != "extract" {
			return "VLM 自报 extract"
		}
	}
	return ""
}

// vlmTry 尝试 VLM 兜底：① learned 命中 → 确定性执行（"下次就会听了"）。
// ② 否则进入 VLM 主控会话：VLM 自决 crop_zoom/click/back/done/impossible/extract；
// 历史候选（无 action）保留旧「粗看→细看→点击/提取」路径。返回 (观测串, 结局)。
func (s *Server) vlmTry(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, semantic, reason string, cands []matcher.Candidate) (string, vlmOutcome) {
	if !s.vlmEnabled() {
		return "", vlmOutcomeSkipped
	}
	sess := bc.Session()
	url := ""
	if vp, ok := sess.(browser.VLMProbeSession); ok {
		url = vp.PageURL()
	}
	action := req.GetExpectedAction()
	target := req.GetTargetSemantic()

	// 1) 学习命中：同页面同语义已有确定性定位。
	if s.vlmFb.Learned != nil && url != "" {
		if e, ok := s.vlmFb.Learned.Lookup(url, target); ok {
			if e.Selector != "" {
				if ok2, obs := s.executeWithSelector(ctx, bc, action, req.GetValue(), semantic, e.Selector); ok2 {
					// §5.0.9 条款 6：学到的定位在页面上漂移后可能点中无关元素，
					// 复用路径同样要判落点（miss ⇒ 清条目 + 硬失败，不重试）。
					if fault, rejected := s.learnedReuseFault(bc, action, url, target); rejected {
						return fault, vlmOutcomeExhausted
					}
					s.vlmLearn(url, target, e) // 命中 +1（自进化热度）
					log.Printf("vlm: learned hit url=%s semantic=%q selector=%q", url, target, e.Selector)
					return obs, vlmOutcomeSuccess
				} else if o, canceled := vlmCanceledObserved(obs); canceled {
					return o, vlmOutcomeTransient
				}
			}
			if e.Semantic != "" {
				if ok2, obs := s.executeWithSelector(ctx, bc, action, req.GetValue(), e.Semantic, ""); ok2 {
					if fault, rejected := s.learnedReuseFault(bc, action, url, target); rejected {
						return fault, vlmOutcomeExhausted
					}
					s.vlmLearn(url, target, e)
					log.Printf("vlm: learned hit url=%s semantic=%q via text", url, target)
					return obs, vlmOutcomeSuccess
				} else if o, canceled := vlmCanceledObserved(obs); canceled {
					return o, vlmOutcomeTransient
				}
			}
			log.Printf("vlm: learned entry stale url=%s semantic=%q, fall back to VLM", url, target)
		}
	}

	// 2) 候选块复选闭环（Phase 3 / §5.0.19）：
	// 尝试对 Top-4 候选进行带 Padding 局部裁块与 VLM 离散选择（A/B/C/D）；
	// 胜出且落点自证通过后直接执行并回写 learned，避免整页截图压缩与坐标回归出界。
	if obs, ok := s.tryDiscreteChoiceFallback(ctx, bc, req, semantic, url, cands); ok {
		return obs, vlmOutcomeSuccess
	} else if o, canceled := vlmCanceledObserved(obs); canceled {
		return o, vlmOutcomeTransient
	} else if vlmHardFailCode(obs) != "" {
		return obs, vlmOutcomeExhausted
	}

	// 2.5) 长页宏观切片路由（D11 Phase 3）：仅当文档超过 3 个视口且前序候选
	// 全低分/为空时采 3–5 个文档区域；切片被选中后才滚动，之后常规 VLM 在该视口精定位。
	_, macroSelected := s.tryMacroSliceRoute(ctx, bc, target, cands)

	candCount := len(cands)
	shot, ok := sess.(browser.Screenshotter)
	if !ok {
		return "", vlmOutcomeSkipped
	}
	// 导出一次 AX 树：继续记录影子统计，并把有界、净化后的目标摘录作为
	// VLM 辅助证据复用，避免丢弃已读取的数据或再增加一轮页面读取。
	axEvidence := s.axShadowLookup(ctx, bc, target, reason, candCount)
	png, w, h, err := shot.CaptureScreenshot(ctx)
	if err != nil {
		log.Printf("vlm: screenshot: %v", err)
		return "", vlmOutcomeTransient
	}
	fullW, fullH := w, h
	if macroSelected {
		log.Printf("vlm: macro slice viewport captured url=%s target=%q", url, target)
	}

	// 2) VLM 主控会话：VLM 自决动作；历史候选（无 action）走旧路径。
	caps := vlm.Capabilities(action)
	maxRounds := s.vlmMaxRounds()
	curPng, curW, curH := png, w, h
	origin := vlmRegion{X: 0, Y: 0, W: w, H: h}
	isCrop := false
	for round := 0; round < maxRounds; round++ {
		tctx, cancel := context.WithTimeout(ctx, s.vlmTimeout())
		res, rerr := s.vlmFb.Client.Resolve(tctx, curPng, curW, curH, vlm.ResolveRequest{
			Target: target, AXEvidence: axEvidence, Action: action, Capabilities: caps, Round: round, Crop: isCrop,
		})
		cancel()
		if rerr != nil {
			log.Printf("vlm: resolve step=%d semantic=%q: %v", round, target, rerr)
			return "", vlmOutcomeTransient
		}
		// §5.0.9 条款 4：意图-动作相容性守卫。VLM 自报动作只在请求语义下才可执行；
		// 错配时不得按自报动作派发（一次 input 意图被点成一次鼠标点击、一次 click 意图
		// 被写进一个输入框，都会被计成成功），也不得换出口重试。
		if why := vlmActionMismatch(action, res.Action); why != "" {
			log.Printf("vlm: action mismatch url=%s semantic=%q expected=%q got=%q (%s)",
				url, target, action, res.Action, FaultVLMActionMismatch)
			return fmt.Sprintf("%s:期望动作=%s，%s", FaultVLMActionMismatch, action, why), vlmOutcomeExhausted
		}
		switch res.Action {
		case vlm.ActionImpossible:
			log.Printf("vlm: impossible url=%s semantic=%q", url, target)
			return "", vlmOutcomeExhausted
		case vlm.ActionBack:
			if hn, ok := sess.(browser.HistoryNavigator); ok {
				if berr := hn.GoBack(ctx); berr != nil {
					log.Printf("vlm: back failed url=%s semantic=%q: %v", url, target, berr)
					return "", vlmOutcomeExhausted
				}
				png, w, h, err = shot.CaptureScreenshot(ctx)
				if err != nil {
					return "", vlmOutcomeTransient
				}
				curPng, curW, curH = png, w, h
				origin = vlmRegion{X: 0, Y: 0, W: w, H: h}
				isCrop = false
				// back 已切换页面；更新语义证据，避免把前一页 AX 内容带入下一轮。
				axEvidence = s.axShadowLookup(ctx, bc, target, reason, candCount)
				log.Printf("vlm: back → re-observe url=%s semantic=%q", url, target)
				continue
			}
			log.Printf("vlm: back unsupported url=%s semantic=%q", url, target)
			return "", vlmOutcomeExhausted
		case vlm.ActionCropZoom:
			if curW <= 0 || curH <= 0 {
				return "", vlmOutcomeTransient
			}
			rx, ry, rw, rh, ok2 := cropZoomRegion(origin, curW, curH, res)
			if !ok2 {
				log.Printf("vlm: crop_zoom invalid url=%s semantic=%q res=%+v", url, target, res)
				return "", vlmOutcomeExhausted
			}
			if reg, ok := sess.(browser.ScreenshotterRegion); ok {
				cpng, cw, ch, cerr := reg.CaptureScreenshotRegion(ctx, rx, ry, rw, rh)
				if cerr == nil && cw > 0 && ch > 0 {
					origin = vlmRegion{X: rx, Y: ry, W: rw, H: rh}
					curPng, curW, curH = cpng, cw, ch
					isCrop = true
					log.Printf("vlm: crop_zoom → %d,%d %dx%d url=%s semantic=%q", rx, ry, rw, rh, url, target)
					continue
				}
			}
			log.Printf("vlm: crop_zoom region failed url=%s semantic=%q", url, target)
			return "", vlmOutcomeExhausted
		case vlm.ActionDone:
			if action == "extract" && res.Text != "" {
				s.vlmLearn(url, target, s.vlmExtractEntry(ctx, bc, res, origin, curW, curH))
				return res.Text, vlmOutcomeSuccess
			}
			if obs, ok := s.vlmActOnCandidate(ctx, bc, req, semantic, url, target, res, origin, curW, curH); ok {
				return obs, vlmOutcomeSuccess
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, vlmOutcomeTransient
			}
			return "", vlmOutcomeExhausted
		case vlm.ActionExtract:
			if res.Text != "" {
				s.vlmLearn(url, target, s.vlmExtractEntry(ctx, bc, res, origin, curW, curH))
				return res.Text, vlmOutcomeSuccess
			}
			return "", vlmOutcomeExhausted
		case vlm.ActionInput:
			if obs, ok := s.vlmActOnInput(ctx, bc, req, semantic, url, target, res, origin, curW, curH); ok {
				return obs, vlmOutcomeSuccess
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, vlmOutcomeTransient
			} else if vlmHardFailCode(obs) != "" {
				// 硬失败代号必须原样上抛（§5.0.8 条款 2 / §5.0.9 条款 3）：不重试、不换出口。
				return obs, vlmOutcomeExhausted
			}
			return "", vlmOutcomeExhausted
		case vlm.ActionClick:
			if obs, ok := s.vlmActOnClick(ctx, bc, req, semantic, url, target, res, origin, curW, curH); ok {
				return obs, vlmOutcomeSuccess
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, vlmOutcomeTransient
			} else if vlmHardFailCode(obs) != "" {
				// 落点自证判 miss：硬失败代号原样上抛，不重试、不换出口（§5.0.9 条款 6）。
				return obs, vlmOutcomeExhausted
			}
			if obs, ok := s.vlmActOnCandidate(ctx, bc, req, semantic, url, target, res, origin, curW, curH); ok {
				return obs, vlmOutcomeSuccess
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, vlmOutcomeTransient
			}
			return "", vlmOutcomeExhausted
		default:
			// 历史候选（无 action）：保留旧「selector → 坐标点击(可 refine)→ text」路径。
			if obs, ok := s.vlmLegacy(ctx, bc, req, semantic, url, target, res, fullW, fullH); ok {
				return obs, vlmOutcomeSuccess
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, vlmOutcomeTransient
			}
			return "", vlmOutcomeExhausted
		}
	}
	log.Printf("vlm: main-control rounds exhausted url=%s semantic=%q rounds=%d", url, target, maxRounds)
	return "", vlmOutcomeExhausted
}

// vlmExtractEntry 为 extract 成功构造 learned 条目：坐标可用时回读真实定位器
// （selector），修复「extract 只写 NodeText、无定位器」的假闭环（GLOBAL §5 #50）——
// 否则下次 learned 命中判 stale 照烧 VLM。坐标缺失（主控 done/extract 只回文本）时
// 退化为 NodeText-only（该路径仍无法派生定位器，属后续统一契约的遗留）。
func (s *Server) vlmExtractEntry(ctx context.Context, bc *browser.BrowserContext, res vlm.Result, origin vlmRegion, curW, curH int) vlm.LearnedEntry {
	entry := vlm.LearnedEntry{NodeText: res.Text}
	if res.Found && res.XRatio > 0 && res.YRatio > 0 && curW > 0 && curH > 0 {
		px := origin.X + int(res.XRatio*float64(curW))
		py := origin.Y + int(res.YRatio*float64(curH))
		if vp, ok := bc.Session().(browser.VLMProbeSession); ok {
			if sel, txt, ok2 := vp.ExtractAt(ctx, px, py); ok2 {
				entry = vlm.LearnedEntry{Selector: sel, NodeText: txt}
			}
		}
	}
	return entry
}

// vlmRegion 是当前图像在视口 CSS 坐标中的偏移（整页 = 0,0,w,h；裁剪 = 源区域）。
type vlmRegion struct {
	X, Y, W, H int
}

// cropZoomRegion 把 VLM 的 crop_zoom 比例（相对当前图像宽/高）换算为视口绝对区域并钳制。
func cropZoomRegion(origin vlmRegion, curW, curH int, res vlm.Result) (x, y, w, h int, ok bool) {
	if curW <= 0 || curH <= 0 || res.XRatio <= 0 || res.YRatio <= 0 ||
		res.WRatio <= 0 || res.HRatio <= 0 {
		return 0, 0, 0, 0, false
	}
	rw := int(res.WRatio * float64(curW))
	rh := int(res.HRatio * float64(curH))
	if rw < 200 {
		rw = 200
	}
	if rh < 160 {
		rh = 160
	}
	if rw > curW {
		rw = curW
	}
	if rh > curH {
		rh = curH
	}
	cx := int(res.XRatio * float64(curW))
	cy := int(res.YRatio * float64(curH))
	x = origin.X + cx - rw/2
	y = origin.Y + cy - rh/2
	// 钳制到当前图像区域右/下边缘（整页=视口边缘，裁剪=源区域边缘）。
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x+rw > origin.W+origin.X {
		x = origin.W + origin.X - rw
	}
	if y+rh > origin.H+origin.Y {
		y = origin.H + origin.Y - rh
	}
	if x < 0 || y < 0 || rw < 1 || rh < 1 {
		return 0, 0, 0, 0, false
	}
	return x, y, rw, rh, true
}

// vlmActOnClick 对显式 click 动作做坐标点击（VLM 主控），坐标相对当前图像 + origin 偏移。
func (s *Server) vlmActOnClick(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, semantic, url, target string, res vlm.Result, origin vlmRegion, curW, curH int) (string, bool) {
	if !res.Found || res.XRatio <= 0 || res.YRatio <= 0 || curW <= 0 || curH <= 0 {
		return "", false
	}
	px := origin.X + int(res.XRatio*float64(curW))
	py := origin.Y + int(res.YRatio*float64(curH))
	if vp, ok := bc.Session().(browser.VLMProbeSession); ok {
		log.Printf("vlm: main-control click plan-point url=%s semantic=%q x_ratio=%.17g y_ratio=%.17g curW=%d curH=%d origin_x=%d origin_y=%d origin_w=%d origin_h=%d point_x=%d point_y=%d",
			url, target, res.XRatio, res.YRatio, curW, curH,
			origin.X, origin.Y, origin.W, origin.H, px, py)
		sel, ok2, obs := vp.ClickAt(ctx, px, py)
		if ok2 {
			// §5.0.9 条款 6 落点自证：miss ⇒ 硬判失败（不换出口，避免假成功换假重试）；
			// 只有「相关」才写 learned，证据不足同样不写（缺证据不改判出口）。
			v, h := clickLandingOf(bc, target)
			if isNodeAddressed(target) {
				v = landingRelevant // node_id 寻址无独立目标语义，落点自证不适用
			} else if !landingSelfCheckApplies(target) {
				// §5.0.9 条款 8：站点控件语义的标识是引擎侧内部控件名、落点文本是站点固定
				// UI 文案，文本互含判据无判别力 ⇒ 归证据不足（不判 miss、不写 learned）。
				v = landingUnverified
			}
			if v == landingMiss {
				o := FaultClickLandingMismatch + ":落点与目标语义无关 hit=" + h.Text
				log.Printf("vlm: click landing mismatch url=%s semantic=%q hit=%q", url, target, h.Text)
				return o, false
			}
			if v == landingRelevant {
				s.vlmLearn(url, target, vlm.LearnedEntry{Selector: sel, NodeText: res.Text})
			} else {
				log.Printf("vlm: click landing unverified, learned skipped url=%s semantic=%q", url, target)
			}
			log.Printf("vlm: main-control click at %d,%d url=%s semantic=%q selector=%q", px, py, url, target, sel)
			return obs, true
		}
		if o, canceled := vlmCanceledObserved(obs); canceled {
			// 坐标点击被调用方中断：不要再往下试候选（ctx 已经没了，只会再打一次）。
			return o, false
		}
		log.Printf("vlm: main-control click_at failed url=%s semantic=%q obs=%s", url, target, obs)
	}
	return "", false
}

// vlmActOnCandidate 对 VLM 已定位的候选做确定性执行：selector → 坐标点击 → 文本语义重试。
func (s *Server) vlmActOnCandidate(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, semantic, url, target string, res vlm.Result, origin vlmRegion, curW, curH int) (string, bool) {
	action := req.GetExpectedAction()
	if res.Selector != "" {
		if ok2, obs := s.executeWithSelector(ctx, bc, action, req.GetValue(), semantic, res.Selector); ok2 {
			s.vlmLearnOnClick(bc, action, url, target, vlm.LearnedEntry{Selector: res.Selector, NodeText: res.Text})
			return obs, true
		} else if o, canceled := vlmCanceledObserved(obs); canceled {
			return o, false
		}
	}
	if res.Found && res.XRatio > 0 && res.YRatio > 0 && curW > 0 && curH > 0 {
		px := origin.X + int(res.XRatio*float64(curW))
		py := origin.Y + int(res.YRatio*float64(curH))
		if vp, ok := bc.Session().(browser.VLMProbeSession); ok {
			if sel, ok2, obs := vp.ClickAt(ctx, px, py); ok2 {
				s.vlmLearnOnClick(bc, action, url, target, vlm.LearnedEntry{Selector: sel, NodeText: res.Text})
				return obs, true
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, false
			}
		}
	}
	if res.Text != "" {
		if ok2, obs := s.executeWithSelector(ctx, bc, action, req.GetValue(), res.Text, ""); ok2 {
			s.vlmLearnOnClick(bc, action, url, target, vlm.LearnedEntry{Semantic: res.Text, NodeText: res.Text})
			return obs, true
		} else if o, canceled := vlmCanceledObserved(obs); canceled {
			return o, false
		}
	}
	return "", false
}

// vlmActOnInput 对显式 input 动作做「定位输入框 → 点击聚焦 → 填入请求值」（VLM 主控，
// 2026-08-31：规则库未覆盖站点（如 Airwallex）的 input 语义兜底）。坐标相对当前图像 +
// origin 偏移；填值用请求携带的 value（DSL `input:目标=值`），成功后回写 learned。
func (s *Server) vlmActOnInput(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, semantic, url, target string, res vlm.Result, origin vlmRegion, curW, curH int) (string, bool) {
	action := "input"
	value := req.GetValue()
	// 候选 1：VLM 给 CSS 选择器 → 直接填值。
	if res.Selector != "" {
		if ok2, obs := s.executeWithSelector(ctx, bc, action, value, semantic, res.Selector); ok2 {
			s.vlmLearn(url, target, vlm.LearnedEntry{Selector: res.Selector, NodeText: res.Text})
			return obs, true
		} else if o, canceled := vlmCanceledObserved(obs); canceled {
			return o, false
		}
	}
	// 候选 2：VLM 给坐标 → 坐标就近输入框原语（InputAt）直接聚焦填值，返回输入框
	// 自身 selector（2026-08-31：Airwallex 描述 textarea 无 placeholder/aria-label，
	// 点击命中容器后按 selector 填值会落空，必须按坐标就近定位输入框）。
	if res.Found && res.XRatio > 0 && res.YRatio > 0 && curW > 0 && curH > 0 {
		px := origin.X + int(res.XRatio*float64(curW))
		py := origin.Y + int(res.YRatio*float64(curH))
		if vp, ok := bc.Session().(browser.VLMProbeSession); ok {
			if sel, ok2, obs := vp.InputAt(ctx, px, py, value, semantic); ok2 {
				s.vlmLearn(url, target, vlm.LearnedEntry{Selector: sel, NodeText: res.Text})
				log.Printf("vlm: main-control input at %d,%d url=%s semantic=%q selector=%q", px, py, url, target, sel)
				return obs, true
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, false
			} else if vlmHardFailCode(obs) != "" {
				// §5.0.9：守卫熔断与落地未验证都不得继续降级到「点击聚焦 + 按 selector 填值」
				// 或文本语义重试——那会把一次越权/无证据写入换出口重演（github 实测教训）。
				log.Printf("vlm: main-control input blocked url=%s semantic=%q obs=%s", url, target, obs)
				return obs, false
			}
			// 回退：点击聚焦 → 按返回 selector 填值（旧会话/旧实现兼容）。
			if sel, ok2, obs := vp.ClickAt(ctx, px, py); ok2 {
				if ok3, obs3 := s.executeWithSelector(ctx, bc, action, value, semantic, sel); ok3 {
					s.vlmLearn(url, target, vlm.LearnedEntry{Selector: sel, NodeText: res.Text})
					log.Printf("vlm: main-control input(click+selector) at %d,%d url=%s semantic=%q selector=%q", px, py, url, target, sel)
					return obs3, true
				} else if o, canceled := vlmCanceledObserved(obs3); canceled {
					return o, false
				}
				log.Printf("vlm: main-control input fill failed url=%s semantic=%q obs=%s", url, target, obs)
			} else if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, false
			}
		}
	}
	// 候选 3：VLM 给精确文本 → 内部语义重试（绕过用户输入 sanitize 白名单）。
	if res.Text != "" {
		if ok2, obs := s.executeWithSelector(ctx, bc, action, value, res.Text, ""); ok2 {
			s.vlmLearn(url, target, vlm.LearnedEntry{Semantic: res.Text, NodeText: res.Text})
			return obs, true
		} else if o, canceled := vlmCanceledObserved(obs); canceled {
			return o, false
		}
	}
	log.Printf("vlm: input candidates exhausted url=%s semantic=%q", url, target)
	return "", false
}

// vlmLegacy 保留旧「粗看→细看→点击/提取」路径：用于 VLM 未返回 action 的历史候选。
// vw/vh 为整页视口尺寸（坐标点击基准；refine 内部另调一次 VLM）。
func (s *Server) vlmLegacy(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, semantic, url, target string, res vlm.Result, vw, vh int) (string, bool) {
	sess := bc.Session()
	action := req.GetExpectedAction()

	// extract：VLM 直接给出元素可见文本作为提取结果（比「听不到」的 DOM 更可靠）。
	if action == "extract" && res.Found && res.Text != "" {
		s.vlmLearn(url, target, s.vlmExtractEntry(ctx, bc, res, vlmRegion{X: 0, Y: 0, W: vw, H: vh}, vw, vh))
		return res.Text, true
	}
	// 候选 1：VLM 给 CSS 选择器。
	if res.Selector != "" {
		if ok2, obs := s.executeWithSelector(ctx, bc, action, req.GetValue(), semantic, res.Selector); ok2 {
			s.vlmLearn(url, target, vlm.LearnedEntry{Selector: res.Selector, NodeText: res.Text})
			return obs, true
		} else if o, canceled := vlmCanceledObserved(obs); canceled {
			return o, false
		}
	}
	// 候选 2：VLM 给相对坐标 → 视口 CSS 像素 → 坐标点击（返回可学习 CSS 路径）。
	if res.Found && res.XRatio > 0 && res.YRatio > 0 && vw > 0 && vh > 0 {
		px, py := int(res.XRatio*float64(vw)), int(res.YRatio*float64(vh))
		if s.vlmFb.Refine {
			if reg, ok := sess.(browser.ScreenshotterRegion); ok {
				if rx, ry, rw, rh, ok2 := refineRegion(px, py, vw, vh); ok2 {
					rpng, rw2, rh2, rerr := reg.CaptureScreenshotRegion(ctx, rx, ry, rw, rh)
					if rerr == nil && rw2 > 0 && rh2 > 0 {
						rctx, rcancel := context.WithTimeout(ctx, s.vlmTimeout())
						rres, rerr2 := s.vlmFb.Client.Resolve(rctx, rpng, rw2, rh2, vlm.ResolveRequest{
							Target: target, Action: action, Capabilities: vlm.Capabilities(action),
							Round: 1, Crop: true,
						})
						rcancel()
						if rerr2 == nil && rres.Found && rres.XRatio > 0 && rres.YRatio > 0 {
							px = rx + int(rres.XRatio*float64(rw2))
							py = ry + int(rres.YRatio*float64(rh2))
							log.Printf("vlm: refined to %d,%d (crop %d,%d %dx%d)", px, py, rx, ry, rw2, rh2)
						}
					}
				}
			}
		}
		if vp, ok := sess.(browser.VLMProbeSession); ok {
			sel, ok2, obs := vp.ClickAt(ctx, px, py)
			if ok2 {
				s.vlmLearn(url, target, vlm.LearnedEntry{Selector: sel, NodeText: res.Text})
				log.Printf("vlm: clicked at %d,%d url=%s semantic=%q selector=%q", px, py, url, target, sel)
				return obs, true
			}
			if o, canceled := vlmCanceledObserved(obs); canceled {
				return o, false
			}
			log.Printf("vlm: click_at failed url=%s semantic=%q obs=%s", url, target, obs)
		}
	}
	// 候选 3：VLM 给精确文本 → 内部语义重试（绕过用户输入 sanitize 白名单）。
	if res.Text != "" {
		if ok2, obs := s.executeWithSelector(ctx, bc, action, req.GetValue(), res.Text, ""); ok2 {
			s.vlmLearn(url, target, vlm.LearnedEntry{Semantic: res.Text, NodeText: res.Text})
			return obs, true
		} else if o, canceled := vlmCanceledObserved(obs); canceled {
			return o, false
		}
	}
	log.Printf("vlm: candidates exhausted url=%s semantic=%q", url, target)
	return "", false
}

// refineRegion 以粗定位点为中心裁剪放大区域（视口 CSS 坐标，钳制到视口内）。
func refineRegion(cx, cy, vw, vh int) (x, y, w, h int, ok bool) {
	w = vw * 60 / 100
	h = vh * 45 / 100
	if w < 200 {
		w = 200
	}
	if h < 160 {
		h = 160
	}
	if w > vw {
		w = vw
	}
	if h > vh {
		h = vh
	}
	x = cx - w/2
	y = cy - h/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x+w > vw {
		x = vw - w
	}
	if y+h > vh {
		y = vh - h
	}
	if x < 0 || y < 0 || w < 1 || h < 1 {
		return 0, 0, 0, 0, false
	}
	return x, y, w, h, true
}

// vlmFallbackFinish 在决策带 NOT_FOUND 路径尝试 VLM 兜底。
// 成功则完成动作并返回响应；「VLM 主控已运行但无解」返回 errVLMExhausted（调用方给区分信号）；
// 未启用/瞬态错误返回 nil,nil（调用方保持原 NOT_FOUND 语义，失真率可统计）。
func (s *Server) vlmFallbackFinish(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, page *pageVerifier,
	urlBefore string, cands []matcher.Candidate) (*uwspb.ActionResultResponse, bool, error) {
	if !s.vlmEnabled() {
		return nil, false, nil
	}
	observed, outcome := s.vlmTry(ctx, bc, req, req.GetTargetSemantic(), "no_candidate", cands)
	// VLM 路径里的物理原语被调用方 ctx 中断时，中断语义必须原样回到引擎的中断分类点，
	// 不能被展开成 Aborted/NOT_FOUND（结果未知的点击最怕被当普通失败重试）。
	if obs, canceled := vlmCanceledObserved(observed); canceled {
		_, dispatched := browser.IsCanceledObserved(obs)
		return nil, false, actionInterruptedError(ctx, dispatched)
	}
	switch outcome {
	case vlmOutcomeSuccess:
		resp, err := s.finishAction(ctx, time.Now(), 0, 0, false, page, bc, req, observed, urlBefore)
		if err != nil {
			log.Printf("vlm: finishAction: %v", err)
			return nil, false, nil
		}
		return resp, true, nil
	case vlmOutcomeExhausted:
		return nil, false, &vlmExhaustedError{code: vlmHardFailCode(observed)}
	default:
		// Skipped / Transient：调用方保持原 NOT_FOUND（失真率可统计，但语义不变）。
		return nil, false, nil
	}
}

// vlmAtNotFound 是「决策带 NOT_FOUND」四个分支共用的 VLM 兜底收口：
// 成功 → (响应, nil)；中断 → (nil, 中断错误，交 execute 的幂等账本保留去重状态)；
// 耗尽 → (nil, Aborted)。handled=false 表示调用方保持原 NOT_FOUND 语义。
func (s *Server) vlmAtNotFound(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, page *pageVerifier, urlBefore string, cands []matcher.Candidate) (*uwspb.ActionResultResponse, error, bool) {
	resp, done, ferr := s.vlmFallbackFinish(ctx, bc, req, page, urlBefore, cands)
	if done {
		return resp, nil, true
	}
	if ferr != nil {
		if errors.Is(ferr, errVLMExhausted) {
			return nil, status.Error(codes.Aborted, ferr.Error()), true
		}
		return nil, ferr, true
	}
	return nil, nil, false
}

func (s *Server) vlmTimeout() time.Duration {
	if s.vlmFb != nil && s.vlmFb.Timeout > 0 {
		return s.vlmFb.Timeout
	}
	return vlm.DefaultTimeout
}

func (s *Server) vlmMaxRounds() int {
	if s.vlmFb != nil && s.vlmFb.MaxRounds > 0 {
		return s.vlmFb.MaxRounds
	}
	return vlm.DefaultMaxRounds
}

// vlmLearn 把成功定位写入自进化存储（页面 + 语义 → 确定性定位）。
func (s *Server) vlmLearn(url, semantic string, e vlm.LearnedEntry) {
	if s.vlmFb == nil || s.vlmFb.Learned == nil || url == "" {
		return
	}
	e.VerifiedAt = time.Now().Format(time.RFC3339)
	if err := s.vlmFb.Learned.Learn(url, semantic, e); err != nil {
		log.Printf("vlm: learn url=%s semantic=%q: %v", url, semantic, err)
	}
}
