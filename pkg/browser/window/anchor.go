// 锚页（uws 自建窗口里的 about:blank 页）与中键派发的实现细节：
// 从「给锚页补装输入护栏」到「认领新 Tab / 关掉误落 Tab」为止的一整段。
//
// 2026-09-29 复杂度治理·阶段 2·单元 3a：与 `shared.go` 同批从宿主
// `pkg/browser/shared_window.go` 下沉；拆两个文件是为了不触代码体积软限。
package window

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"uws/pkg/browser/input"
)

const (
	// anchorElementID 是锚页里那个链接元素的 id（只存在于 uws 自建锚页）。
	anchorElementID = "__uws_shared_anchor__"
	// sharedTabTimeout 是「中键派发后等新 Tab 出现」的上限。
	sharedTabTimeout = 5 * time.Second
	// anchorReadyTimeout 是「锚页 about:blank 就绪、可注入链接」的上限。
	anchorReadyTimeout = 2 * time.Second
	// pollInterval 是轮询 Target.getTargets 的间隔。
	pollInterval = 100 * time.Millisecond
	// dispatchGrace 是「中键派发报了错，但点击可能已经生效」的宽限窗口：
	// 先去核对有没有新白板 Tab 出现，有就按成功继续走归属校验。
	dispatchGrace = 1 * time.Second
)

// errEvalEmpty 与宿主同文案：Runtime.evaluate 成功返回但 value 为空/缺失。
var errEvalEmpty = errors.New("cdp: evaluate returned empty value")

// TargetInfo 是 Target.getTargets 里我们关心的字段（最小同形解码；宿主 `pageTarget`
// 属宿主会话域、被多处引用，不搬）。
type TargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// point 是锚页里的点击坐标（宿主 `pt` 字段未导出，不出包）。
type point struct{ X, Y float64 }

// openBackgroundTabViaAnchor 在锚页上派发一次中键，等一个**恰好多出来**的白板 Tab
// 出现，并校验它确实落在这扇窗口里；否则只关掉这一个自建 Tab 并 fail-closed。
func openBackgroundTabViaAnchor(ctx context.Context, s Session, handle Handle) (string, error) {
	before, err := targetSnapshot(ctx, s)
	if err != nil {
		return "", fmt.Errorf("读点击前 target 集合失败: %w", err)
	}
	anchorSession, err := s.AttachTarget(ctx, handle.AnchorTargetID)
	if err != nil {
		return "", fmt.Errorf("attach 锚页失败: %w", err)
	}
	defer func() {
		detachCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.Send(detachCtx, "Target.detachFromTarget", map[string]any{"sessionId": anchorSession})
		s.OffSession(anchorSession)
	}()

	// 锚页是**临时 attach 出来的第二个 session**：会话自身的「输入必达」护栏只装在
	// 自己的 target 上（enableTargetDomains），所以这里必须给它补装同一层护栏——否则
	// 共享窗口不在前台时，中键会被 Chrome 的输入门控挂起到超时。2026-09-24 真机实测
	// 就是这一条：会话 2 复用共享窗口成功、Runtime.evaluate 也拿到了坐标，但
	// Input.dispatchMouseEvent 超时 → 退化成了专属小窗。
	if err := armAnchorInput(ctx, s, anchorSession); err != nil {
		log.Printf("browser: ⚠️ 锚页输入护栏安装失败（%v），仍尝试派发中键", err)
	}

	p, err := anchorClickPoint(ctx, s, anchorSession)
	if err != nil {
		return "", err
	}
	dispatchErr := dispatchAnchorMiddleClick(ctx, s, anchorSession, p)
	var targetID string
	if dispatchErr != nil {
		// 宽限：派发报错**不等于**点击没生效（可能只是回执没等到，Tab 已经开出来了）。
		// 先看有没有新白板 Tab 出现——有就按成功继续走归属校验，避免把一次已经发生的
		// 点击判成失败、留下一个我们不再认领的孤儿 Tab。
		graceCtx, cancel := context.WithTimeout(ctx, dispatchGrace)
		claimed, graceErr := awaitNewSharedTab(graceCtx, s, before, dispatchGrace)
		cancel()
		if graceErr != nil {
			return "", dispatchErr
		}
		log.Printf("browser: 锚页中键派发报错但新 Tab %s 已出现，按成功继续（%v）", claimed, dispatchErr)
		targetID = claimed
	} else {
		claimed, err := awaitNewSharedTab(ctx, s, before, sharedTabTimeout)
		if err != nil {
			return "", err
		}
		targetID = claimed
	}

	if err := claimSharedTab(ctx, s, targetID, handle); err != nil {
		return "", err
	}
	return targetID, nil
}

// claimSharedTab 校验新 Tab 确实落在注册的那扇共享窗口里；落错就只关掉**这一个**
// 自建 Tab 并 fail-closed——绝不触碰用户标签页，也不注销句柄（下次会话仍可复用）。
func claimSharedTab(ctx context.Context, s Session, targetID string, handle Handle) error {
	got, err := s.WindowIDForTarget(ctx, targetID)
	if err != nil {
		closeOwnedTarget(ctx, s, targetID, "读不到新 Tab 的窗口归属")
		return fmt.Errorf("读新 Tab 窗口归属失败: %w", err)
	}
	if got != handle.WindowID {
		closeOwnedTarget(ctx, s, targetID, fmt.Sprintf("中键开出的 Tab 落到了别的窗口（注册=%d 实际=%d）", handle.WindowID, got))
		return fmt.Errorf("共享窗口归属校验失败：新 Tab 在 window=%d，注册窗口=%d", got, handle.WindowID)
	}
	return nil
}

// armAnchorInput 给锚页 session 装上与会话自身同一层的「输入必达」护栏（GLOBAL #92）：
// 域使能 + 焦点仿真（只改渲染层判定，不抬窗口、不动用户前台应用）+ 页面生命周期 active。
// 命令全部幂等，失败不阻塞——中键仍会走重试与宽限路径，只是少一层护栏。
func armAnchorInput(ctx context.Context, s Session, anchorSession string) error {
	for _, m := range []string{"Page.enable", "Runtime.enable"} {
		if err := s.CallSession(ctx, anchorSession, m, nil, nil); err != nil {
			return fmt.Errorf("cdp: %s: %w", m, err)
		}
	}
	if !s.Headless() && input.FocusEmulationEnabled() {
		if err := s.CallSession(ctx, anchorSession, "Emulation.setFocusEmulationEnabled",
			map[string]any{"enabled": true}, nil); err != nil {
			return fmt.Errorf("cdp: Emulation.setFocusEmulationEnabled: %w", err)
		}
	}
	if err := s.CallSession(ctx, anchorSession, "Page.setWebLifecycleState",
		map[string]any{"state": "active"}, nil); err != nil {
		return fmt.Errorf("cdp: Page.setWebLifecycleState: %w", err)
	}
	return nil
}

// rearmAnchorInput 是重试路径上的「重放护栏」（与 ensureInputReady 同构）：独立小预算、
// 尽力而为、不返回错误——重试的成败由派发本身判定。
func rearmAnchorInput(s Session, anchorSession string) {
	ctx, cancel := context.WithTimeout(context.Background(), input.ReadinessTimeout())
	defer cancel()
	if !s.Headless() && input.FocusEmulationEnabled() {
		_ = s.CallSession(ctx, anchorSession, "Emulation.setFocusEmulationEnabled",
			map[string]any{"enabled": true}, nil)
	}
	_ = s.CallSession(ctx, anchorSession, "Page.setWebLifecycleState",
		map[string]any{"state": "active"}, nil)
}

// targetSnapshot 取当前 page target 集合（用于「新出现了哪个」的差集判定）。
func targetSnapshot(ctx context.Context, s Session) (map[string]bool, error) {
	targets, err := listPageTargets(ctx, s)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(targets))
	for _, info := range targets {
		out[info.TargetID] = true
	}
	return out, nil
}

// anchorJS 在锚页里保证「有一个铺在左上角的 _blank 链接」并回报它的中心点。
//
// 前置条件是**锚页自己仍是白板**：若它已经被导航成别的页面（用户点过它），这里拒绝
// 注入并回报 err —— 绝不向用户页面注入脚本（§4.1）。
const anchorJS = `(() => {
  if (!/^about:/.test(location.protocol)) {
    return {ok: false, err: 'anchor-not-blank:' + location.href};
  }
  const host = document.body || document.documentElement;
  if (!host) return {ok: false, err: 'anchor-no-host'};
  let a = document.getElementById('` + anchorElementID + `');
  if (!a || a.tagName !== 'A') {
    a = document.createElement('a');
    a.id = '` + anchorElementID + `';
    a.target = '_blank';
    a.rel = 'noopener';
    a.href = 'about:blank';
    a.textContent = 'uws';
    host.appendChild(a);
  }
  a.style.position = 'fixed';
  a.style.left = '0px';
  a.style.top = '0px';
  a.style.width = '60px';
  a.style.height = '60px';
  a.style.display = 'block';
  a.style.zIndex = '2147483647';
  const r = a.getBoundingClientRect();
  return {ok: true, x: r.left + r.width / 2, y: r.top + r.height / 2};
})()`

// anchorClickPoint 在锚页里部署链接并回报点击坐标（锚页刚建好时可能还没有 body，短轮询）。
func anchorClickPoint(ctx context.Context, s Session, anchorSession string) (point, error) {
	deadline := time.Now().Add(anchorReadyTimeout)
	var lastErr error
	for {
		p, err := evalAnchorClickPoint(ctx, s, anchorSession)
		if err == nil {
			return p, nil
		}
		lastErr = err
		if ctx.Err() != nil || time.Now().After(deadline) {
			return point{}, fmt.Errorf("锚页未就绪: %w", lastErr)
		}
		time.Sleep(pollInterval)
	}
}

func evalAnchorClickPoint(ctx context.Context, s Session, anchorSession string) (point, error) {
	var raw struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := s.CallSession(ctx, anchorSession, "Runtime.evaluate",
		map[string]any{"expression": anchorJS, "returnByValue": true, "awaitPromise": true}, &raw); err != nil {
		return point{}, err
	}
	if len(raw.ExceptionDetails) > 0 {
		return point{}, fmt.Errorf("锚页脚本异常: %s", string(raw.ExceptionDetails))
	}
	if len(raw.Result.Value) == 0 || string(raw.Result.Value) == "null" {
		return point{}, errEvalEmpty
	}
	var out struct {
		OK  bool    `json:"ok"`
		Err string  `json:"err"`
		X   float64 `json:"x"`
		Y   float64 `json:"y"`
	}
	if err := json.Unmarshal(raw.Result.Value, &out); err != nil {
		return point{}, err
	}
	if !out.OK {
		return point{}, fmt.Errorf("锚页拒绝部署链接（%s）", out.Err)
	}
	if out.X < 1 || out.Y < 1 {
		return point{}, fmt.Errorf("锚页链接几何非法（%v,%v）", out.X, out.Y)
	}
	return point{X: out.X, Y: out.Y}, nil
}

// dispatchAnchorMiddleClick 在锚页里派发中键（Blink kNewBackgroundTab）。
//
// 重试策略（2026-09-24 真机缺陷修复）：
//   - **只有 mouseMoved 可以重试**：它不产生点击，重试无副作用；超时类错误（输入被
//     后台/遮挡门控挂起）时先重放锚页护栏、再按 input.BackoffFor 退避重试；
//   - mousePressed / mouseReleased **绝不重试**：多派发一次就可能一次开出两个 Tab，
//     语义就乱了（这种情况由调用方的宽限判定兜底）。
func dispatchAnchorMiddleClick(ctx context.Context, s Session, anchorSession string, p point) error {
	moved := map[string]any{"type": "mouseMoved", "x": p.X, "y": p.Y, "button": "none", "buttons": 0}
	attempts := input.AttemptsFromEnv()
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			rearmAnchorInput(s, anchorSession)
			if d := input.BackoffFor(attempt - 1); d > 0 {
				timer := time.NewTimer(d)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return fmt.Errorf("锚页中键派发失败: %w", lastErr)
				}
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, input.AttemptTimeout())
		err := s.CallSession(callCtx, anchorSession, "Input.dispatchMouseEvent", moved, nil)
		cancel()
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if !input.IsTimeoutLike(err) {
			break
		}
	}
	if lastErr != nil {
		return fmt.Errorf("锚页中键派发失败: %w", lastErr)
	}
	events := []map[string]any{
		{"type": "mousePressed", "x": p.X, "y": p.Y, "button": "middle", "buttons": 4, "clickCount": 1},
		{"type": "mouseReleased", "x": p.X, "y": p.Y, "button": "middle", "buttons": 0, "clickCount": 1},
	}
	for _, ev := range events {
		callCtx, cancel := context.WithTimeout(ctx, sharedTabTimeout)
		err := s.CallSession(callCtx, anchorSession, "Input.dispatchMouseEvent", ev, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("锚页中键派发失败: %w", err)
		}
	}
	return nil
}

// awaitNewSharedTab 轮询等「恰好多出一个白板 page target」；0 个则超时失败，
// 多于 1 个或 URL 不符一律拒绝认领（宁可本次不共享，也不误认别人的 Tab）。
func awaitNewSharedTab(ctx context.Context, s Session, before map[string]bool, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		targets, err := listPageTargets(ctx, s)
		if err != nil {
			return "", fmt.Errorf("读 target 列表失败: %w", err)
		}
		fresh := NewPageTargets(before, targets)
		switch {
		case len(fresh) == 1:
			if !BlankURL(fresh[0].URL) {
				return "", fmt.Errorf("锚页中键开出的新 Tab 不是白板（url=%q），拒绝认领", fresh[0].URL)
			}
			return fresh[0].TargetID, nil
		case len(fresh) > 1:
			return "", fmt.Errorf("锚页中键一次开出 %d 个新 page target，语义不明，拒绝认领", len(fresh))
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("锚页中键未在 %s 内开出新 Tab（锚页可能已不是该窗口的活动 Tab）", timeout)
		}
		time.Sleep(pollInterval)
	}
}

// closeOwnedTarget 关掉一个「uws 自己刚开的」Tab，并把原因写进日志。
// 只在这个明确的自建 targetId 上调用——绝不用来关用户的标签页。
func closeOwnedTarget(ctx context.Context, s Session, targetID, reason string) {
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Send(closeCtx, "Target.closeTarget", map[string]any{"targetId": targetID}); err != nil {
		log.Printf("browser: ⚠️ 关掉误落的自建 Tab %s 失败（%v）：%s", targetID, err, reason)
		return
	}
	log.Printf("browser: 已关掉误落的自建 Tab %s（%s）", targetID, reason)
}
