// 窗口域：同指纹共享窗口（锚页 + 中键开后台 Tab）的整段流程。
//
// 2026-09-29 复杂度治理·阶段 2·单元 3a：原宿主 `pkg/browser/shared_window.go` 的
// 15 个全私有 `cdpSession` 方法整体下沉到本包。子包不再直接持有 `*cdpSession`：
// 宿主经 `Session` 注入**实测用到的那几项能力**（7 项 CDP 通道 ＋ 4 项会话事实，
// 形状与 `input`/`modelbridge` 的窄接口对齐）。注册表（原包级 `sharedWindows`）随
// 流程一起下沉为**可注入的 `Registry“——宿主生产路径用 `DefaultRegistry()`，测试
// 各自 `NewRegistry()` 隔离，不再需要全局复位。
//
// 用户口径 ①：一个浏览器指纹至多一扇受管窗口，同一指纹的会话 Tab 集中在该窗口内。
// 活规范：docs/fingerprint-and-viewer-final-spec-2026-09-12.md §4.3；实测裁决与探针
// 原始读数：docs/evidence/pilots/window-tab-governance-2026-09-24.md。
//
// 为什么需要「锚页 + 中键」：CDP 没有「在指定窗口内开 Tab」也没有「把 Tab 移到某窗口」
// 的能力——Target.createTarget 只有 newWindow 布尔 + left/top/width/height（几何还仅
// 在 newWindow:true 时有效）。实测三条候选路径：
//
//	window.open（含 Runtime.evaluate userGesture:true） → 被弹窗拦截器拦掉（BLOCKED）
//	受信左键 target=_blank                              → 落同一扇窗口，但**前台**且激活该窗口
//	newWindow:false                                     → 落进用户自己的窗口（污染用户标签页）
//	**中键**（Blink kNewBackgroundTab）                 → 落同一扇窗口，且是**后台** Tab ✅
//
// 代价也被实测钉住：往某窗口里新建 Tab 会让该窗口被**前置一次**（前台 App 变化）。
// 这是 ①「一指纹一窗」优先于「零前置」的已定取舍，不是缺陷；`UWS_WINDOW_MODE=window`
// 可显式换回每会话一窗的零前置行为。
//
// 纪律：锚页只存在于 uws 自建窗口、只承载「开 Tab」一个动作，**绝不向用户页面注入
// 脚本**（§4.1）；归属校验失败时只关掉**我们自己刚开的那一个** Tab，绝不触碰用户
// 标签页。
package window

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Handle 是一扇已注册的共享窗口：窗口 id + 该窗口里的 uws 锚页 target。
type Handle struct {
	WindowID       int
	AnchorTargetID string
}

// TargetSpec 是 createTarget 的窗口语义（与宿主 `targetSpec` 同形，由宿主适配器转换）。
type TargetSpec struct {
	NewWindow  bool  // true = 新窗口（专属小窗），false = 现有窗口的新标签（旧行为）
	Background bool  // true = 后台创建，不把窗口拉到前台
	Focus      *bool // false = 不聚焦（nil = Chrome 默认）
	Width      int   // 仅 NewWindow 生效（窗口外框 DIP）
	Height     int
}

// Client 是本包实测用到的 CDP 通道面（7 项，＝shared_window.go 对宿主的全部出边）。
type Client interface {
	Call(ctx context.Context, method string, params, out any) error
	CallSession(ctx context.Context, sessionID, method string, params, out any) error
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
	OffSession(sessionID string) int
	AttachTarget(ctx context.Context, targetID string) (string, error)
	CreateTarget(ctx context.Context, url string, spec TargetSpec) (string, error)
	WindowIDForTarget(ctx context.Context, targetID string) (int, error)
}

// Session 是宿主会话向共享窗口簇暴露的最小能力面（7 项通道 ＋ 4 项会话事实）。
type Session interface {
	Client
	// Key 给出「同一个浏览器指纹」的窗口键（attach 端点 + profile id）；没有 attach
	// 端点时 ok=false —— 那种情况每个会话各自起 Chrome，没有窗口可共享。
	Key() (string, bool)
	// Headless 报告本会话是否无头（无头下不装焦点仿真护栏）。
	Headless() bool
	// MarkShared 回写：本会话的 Tab 开在 windowID 这扇共享窗口里；anchorOwner 表示
	// 锚页窗是**本会话**建的（只有它负责按默认几何摆位）。
	MarkShared(windowID int, anchorOwner bool)
	// SharedWindowID 返回本会话所属共享窗口 id（0 = 本会话不在共享窗口里）。
	SharedWindowID() int
}

// Registry 是跨会话的共享窗口注册表。它既保护映射，也把「查不到就建」的整段时序串
// 起来：同一指纹的并发会话必须串行地建锚页窗，否则各自开一扇，等于回到多窗口。锚页
// 操作是低频动作（每个会话一次），持锁跨越 CDP 调用可以接受。
//
// 可注入（而非包级全局）是本次下沉的关键改进：测试各自 `NewRegistry()`，不再需要
// 「用例之间复位包级 map」的全局耦合。
type Registry struct {
	mu      sync.Mutex
	windows map[string]Handle
}

// NewRegistry 建一个空注册表（测试用；生产走 DefaultRegistry）。
func NewRegistry() *Registry {
	return &Registry{windows: make(map[string]Handle)}
}

var defaultRegistry = NewRegistry()

// DefaultRegistry 是宿主生产路径用的进程内注册表（一指纹一窗是进程级语义）。
func DefaultRegistry() *Registry { return defaultRegistry }

// Seed 直接写入一条句柄（测试用：模拟「上一次会话已经建好窗口」）。
func (r *Registry) Seed(key string, h Handle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.windows[key] = h
}

// Entry 读一条句柄（测试用）。
func (r *Registry) Entry(key string) (Handle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.windows[key]
	return h, ok
}

// Key 计算「同一个浏览器指纹」的窗口键。
//
// 指纹身份 = 同一个浏览器实例（attach 端点）+ 同一个持久身份（profile id）。没有
// attach 端点时每个会话各自起一个 Chrome 进程，**没有窗口可共享**，此时 ok=false，
// 调用方退化为每会话一窗（window 模式）。
func Key(endpoint, profileID string) (string, bool) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false
	}
	return endpoint + "\x00" + strings.TrimSpace(profileID), true
}

// errAnchorStale 表示注册表里的锚页**确实**失效了（target 消失 / 归属变了），可以
// 安全地丢句柄重建；与之相对的是「这次读不到」（CDP 报错）——那种情况不动注册表，
// 否则一次抖动就会多出一扇窗口。
var errAnchorStale = errors.New("共享窗口锚页已失效")

// OpenTab 在「本会话所属指纹的共享窗口」里开一个后台 Tab，返回它的 targetID。
//
// 命中注册表即复用同一扇窗口；未命中（或锚页确实失效）才建锚页窗并登记。
// 任何失败都返回错误——调用方据此退化为专属小窗，共享路径故障不该让会话起不来。
func (r *Registry) OpenTab(ctx context.Context, s Session, width, height int) (string, error) {
	key, ok := s.Key()
	if !ok {
		return "", errors.New("没有可共享的浏览器端点（attach 为空），共享模式不适用")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	handle, hit := r.windows[key]
	if hit {
		if err := verifySharedAnchor(ctx, s, handle); err != nil {
			if !errors.Is(err, errAnchorStale) {
				return "", fmt.Errorf("共享窗口锚页无法证实（%w），本次不共享", err)
			}
			log.Printf("browser: 共享窗口 window=%d 的锚页已失效（%v），重新建窗", handle.WindowID, err)
			delete(r.windows, key)
			hit = false
		}
	}
	created := false
	if !hit {
		fresh, err := createSharedAnchorWindow(ctx, s, width, height)
		if err != nil {
			return "", err
		}
		handle = fresh
		r.windows[key] = handle
		created = true
		log.Printf("browser: 新建共享窗口 window=%d 锚页 target=%s（键=%s）", handle.WindowID, handle.AnchorTargetID, key)
	} else {
		log.Printf("browser: 复用共享窗口 window=%d 锚页 target=%s（键=%s）", handle.WindowID, handle.AnchorTargetID, key)
	}

	targetID, err := openBackgroundTabViaAnchor(ctx, s, handle)
	if err != nil {
		return "", err
	}
	log.Printf("browser: 共享窗口 window=%d 已开后台 Tab target=%s", handle.WindowID, targetID)
	s.MarkShared(handle.WindowID, created)
	return targetID, nil
}

// Release 在会话关闭时回收共享窗口：**只在窗内只剩锚页**（没有任何别的 page target，
// 含用户自己的页面）时关锚页——窗口随之消失。窗内还有别的页面则一律不动（那可能是
// 别的会话的 Tab，也可能是用户自己开的）。
func (r *Registry) Release(s Session) {
	windowID := s.SharedWindowID()
	if windowID == 0 {
		return
	}
	key, ok := s.Key()
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	handle, ok := r.windows[key]
	if !ok || handle.WindowID != windowID {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	targets, err := listPageTargets(ctx, s)
	if err != nil {
		return // 读不到就不动：宁可留一扇空窗，也不乱关
	}
	for _, info := range targets {
		if info.TargetID == handle.AnchorTargetID {
			continue
		}
		got, err := s.WindowIDForTarget(ctx, info.TargetID)
		if err != nil || got != handle.WindowID {
			continue
		}
		return // 窗内还有别的页面：不动
	}
	if _, err := s.Send(ctx, "Target.closeTarget", map[string]any{"targetId": handle.AnchorTargetID}); err != nil {
		log.Printf("browser: 关共享窗口锚页失败（%v），句柄保留", err)
		return
	}
	delete(r.windows, key)
	log.Printf("browser: 共享窗口 window=%d 已无会话 Tab，关掉锚页并回收句柄", handle.WindowID)
}

// listPageTargets 读取浏览器当前的页面 target（与宿主 `pageTargets` 的区别：报错原样
// 返回，供「读不到就别乱动」的判定使用）。
func listPageTargets(ctx context.Context, s Session) ([]TargetInfo, error) {
	var res struct {
		TargetInfos []TargetInfo `json:"TargetInfos"`
	}
	if err := s.Call(ctx, "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	out := make([]TargetInfo, 0, len(res.TargetInfos))
	for _, info := range res.TargetInfos {
		if info.Type == "page" {
			out = append(out, info)
		}
	}
	return out, nil
}

// NewPageTargets 返回相对点击前「新出现」的 page target（纯函数，便于单测）。
func NewPageTargets(before map[string]bool, infos []TargetInfo) []TargetInfo {
	out := make([]TargetInfo, 0, 1)
	for _, info := range infos {
		if before[info.TargetID] {
			continue
		}
		out = append(out, info)
	}
	return out
}

// BlankURL 判定新 Tab 的 URL 是否还是白板（about:blank / 空）。
// 只认领白板：字面数字地址或用户页面一律不认（宁可不共享，也不误认别人的 Tab）。
func BlankURL(raw string) bool {
	url := strings.TrimSpace(raw)
	return url == "" || strings.HasPrefix(url, "about:")
}

// verifySharedAnchor 证实注册表里的锚页仍存在、且仍在注册的那扇窗口里。
func verifySharedAnchor(ctx context.Context, s Session, handle Handle) error {
	if strings.TrimSpace(handle.AnchorTargetID) == "" {
		return fmt.Errorf("%w: 锚页 target 为空", errAnchorStale)
	}
	targets, err := listPageTargets(ctx, s)
	if err != nil {
		return fmt.Errorf("读 target 列表失败: %w", err)
	}
	found := false
	for _, info := range targets {
		if info.TargetID == handle.AnchorTargetID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: 锚页 target %s 已不存在", errAnchorStale, handle.AnchorTargetID)
	}
	got, err := s.WindowIDForTarget(ctx, handle.AnchorTargetID)
	if err != nil {
		return fmt.Errorf("读锚页窗口归属失败: %w", err)
	}
	if got != handle.WindowID {
		return fmt.Errorf("%w: 锚页已不在注册窗口内（注册=%d 实际=%d）", errAnchorStale, handle.WindowID, got)
	}
	return nil
}

// createSharedAnchorWindow 建一扇 uws 自建窗口并在其中开锚页；窗口几何语义与专属小窗
// 一致（newWindow + background + focus=false，尺寸取当前策略），一次创建、长期复用。
func createSharedAnchorWindow(ctx context.Context, s Session, width, height int) (Handle, error) {
	noFocus := false
	anchorTargetID, err := s.CreateTarget(ctx, "about:blank", TargetSpec{
		NewWindow:  true,
		Background: true,
		Focus:      &noFocus,
		Width:      width,
		Height:     height,
	})
	if err != nil {
		return Handle{}, fmt.Errorf("建共享窗口失败: %w", err)
	}
	windowID, err := s.WindowIDForTarget(ctx, anchorTargetID)
	if err != nil {
		// 归属读不到就没法校验后续 Tab 落点（也就不能用这扇窗），关掉再报错。
		closeOwnedTarget(ctx, s, anchorTargetID, "读不到新建锚页窗的窗口归属")
		return Handle{}, fmt.Errorf("读新建共享窗口归属失败: %w", err)
	}
	return Handle{WindowID: windowID, AnchorTargetID: anchorTargetID}, nil
}
