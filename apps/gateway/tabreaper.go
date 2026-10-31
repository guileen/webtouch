package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 标签页账本：记录「网关自己开过的站点标签页」（session_id -> target_id）。
// 进程重启后，内存里的会话没了，但浏览器里的页签还在 ⇒ 用账本把它们回收掉。
// 只关账本里的页，绝不碰用户手动打开的页签（2026-10-02 用户：测试窗口/会话要及时清理）。
type tabsLedger struct {
	Version int               `json:"version"`
	Targets map[string]string `json:"targets"`
}

const tabsLedgerVersion = 1

// loadTabLedger 读取账本（不存在 / 损坏都按空表处理，不影响启动）。
func (g *Gateway) loadTabLedger() {
	g.tabs = make(map[string]string)
	if g.tabsStatePath == "" {
		return
	}
	data, err := os.ReadFile(g.tabsStatePath)
	if err != nil {
		return
	}
	var led tabsLedger
	if err := json.Unmarshal(data, &led); err != nil {
		return
	}
	for sid, tid := range led.Targets {
		if sid != "" && tid != "" {
			g.tabs[sid] = tid
		}
	}
}

// saveTabLedgerLocked 原子写账本；调用方必须已持 g.mu。
func (g *Gateway) saveTabLedgerLocked() {
	if g.tabsStatePath == "" {
		return
	}
	led := tabsLedger{Version: tabsLedgerVersion, Targets: make(map[string]string, len(g.tabs))}
	for sid, tid := range g.tabs {
		led.Targets[sid] = tid
	}
	data, err := json.Marshal(led)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(g.tabsStatePath), ".gateway-tabs-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	tmp.Close()
	if err := os.Rename(name, g.tabsStatePath); err != nil {
		os.Remove(name)
	}
}

// recordTab 记下本会话由网关打开的标签页。
func (g *Gateway) recordTab(sid, targetID string) {
	if sid == "" || targetID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tabs[sid] = targetID
	g.saveTabLedgerLocked()
}

// forgetTab 在会话关闭时从账本移除。
func (g *Gateway) forgetTab(sid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.tabs[sid]; !ok {
		return
	}
	delete(g.tabs, sid)
	g.saveTabLedgerLocked()
}

// ledgerTab 返回账本里该会话的页签 ID（无则空串）。
func (g *Gateway) ledgerTab(sid string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tabs[sid]
}

// closeLedgerTab 只看账本：用调试端点关掉该会话的页签并出账，返回「页签确认已关」。
// 无页签 = true（无事可做）；attach-cdp 为空（引擎自建隔离 Chrome）或关不掉 = false，
// 条目一律保留，交给下次启动回收再试。
func (g *Gateway) closeLedgerTab(ctx context.Context, sid string) bool {
	tid := g.ledgerTab(sid)
	if tid == "" {
		return true
	}
	if g.attachCDP == "" {
		return false
	}
	if err := g.closeCDPTab(ctx, tid); err != nil {
		return false
	}
	g.forgetTab(sid)
	return true
}

// ReapOrphanTabs 回收「上一次进程遗留的站点标签页」：只关账本里网关自己开过的页，
// 不碰用户手动打开的页签。attach-cdp 为空（引擎自建隔离 Chrome）时不动作——那时
// 标签页归引擎管。返回成功关闭数。
func (g *Gateway) ReapOrphanTabs(ctx context.Context) int {
	g.mu.Lock()
	type entry struct{ sid, tid string }
	orphans := make([]entry, 0, len(g.tabs))
	seen := map[string]bool{}
	for sid, tid := range g.tabs {
		if tid != "" && !seen[tid] {
			seen[tid] = true
			orphans = append(orphans, entry{sid: sid, tid: tid})
		}
	}
	g.mu.Unlock()

	if g.attachCDP == "" || len(orphans) == 0 {
		return 0
	}
	closed := 0
	remaining := make(map[string]string, len(orphans))
	for _, e := range orphans {
		if err := g.closeCDPTab(ctx, e.tid); err == nil {
			closed++
			continue
		}
		// 关不掉（Chrome 未就绪 / 页签已不在）保留条目，下次启动再试；
		// 逐条 sleep 会拖慢启动，启动路径只做一遍。
		remaining[e.sid] = e.tid
	}
	g.mu.Lock()
	g.tabs = remaining
	g.saveTabLedgerLocked()
	g.mu.Unlock()
	if g.logger != nil && closed > 0 {
		g.logger.Printf("启动回收：关闭上次遗留的站点标签页 %d 个（只回收网关自己开的页）", closed)
	}
	return closed
}

// closeCDPTab 通过调试端点关闭指定标签页（仅用于回收账本里的页）。
func (g *Gateway) closeCDPTab(ctx context.Context, targetID string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	u := strings.TrimRight(g.attachCDP, "/") + "/json/close/" + targetID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("close target %s: HTTP %d", targetID, resp.StatusCode)
	}
	return nil
}
