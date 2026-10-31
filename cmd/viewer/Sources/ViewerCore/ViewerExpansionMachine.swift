import Foundation

/// 「点小窗 → 真窗口到最前」这条链路的状态机。
///
/// 现场症状（2026-09-22 批次 #2c）：点开小窗后只是一张**点不动、滚不动**的图。根因是
/// 壳侧的 1.2s 超时比引擎一次搬运（5~6 次 CDP 往返，引擎侧 `viewerIntentTimeout=5s`）
/// 短得多：忙页面上壳先回落到自绘放大面板，**随后**引擎的成功回执才到，而旧代码在
/// 「已经回落」的分支里又主动补发一条 collapse，把真窗口搬了回去 ⇒ 用户永远看不到真页面。
///
/// 修法就是区分「超时回落」与「用户取消」：超时只回落（`.fellBack`），**不**撤销、**不**
/// 补发 collapse；迟到的 ok=true 仍能把自绘放大面板升级成真页面；只有用户真的不要了
/// （等回执期间点收回 → `.cancelled`）才替他把引擎动过的真窗口放回去。
///
/// 2026-09-22 口径更新：展开 = 引擎把那扇真窗口**抬到最前**（不搬位置、不缩放），所以
/// 「真页面在屏幕上」不再是一个需要壳追踪的状态——引擎动过窗口（抬前时为救援屏外位置
/// 搬过一次）也只有 `.restoreRealWindow` 那一种收尾。小窗自己播完「飞向它」的动画就
/// 消失，屏幕上只剩真窗口（互斥不变量）。
///
/// 纯逻辑、无 AppKit：这些分支正是缺陷所在，必须在没有窗口服务器的环境里可测。
public struct ViewerExpansionMachine: Equatable {
    public enum Phase: Equatable {
        /// 不在展开链路上。
        case idle
        /// 意图已发，等引擎回执。
        case expanding
        /// 用户在半路不要了：不再等回执，但引擎可能已经把窗口抬到最前 / 救援搬过。
        case cancelled
        /// 回执迟到：先画自绘放大面板，同时留着迟到回执的升级机会。
        case fellBack
    }

    public enum ExpansionOutcome: Equatable {
        case nothing
        /// 回落到自绘放大面板（「点开一定有东西」）。
        case showDrawnPanel
        /// 引擎已经把真窗口抬到最前：小窗播「飞向它」的动画后消失，把屏幕让给真窗口。
        case handOverToRealWindow
        /// 引擎动过那扇真窗口（抬前时救援搬过一次），但用户已经不要了 / 面板已经换人：
        /// 替他放回去。
        case restoreRealWindow
    }

    public enum CollapseRequest: Equatable {
        case ignored
        /// 等回执期间用户不要了：不再等，但迟到的成功回执仍要把真窗口放回原位。
        case cancelled
        /// 自绘放大面板：壳自己收，不劳引擎。
        case collapseDrawnPanel
    }

    public private(set) var phase: Phase = .idle
    /// 这条展开链路属于哪个会话（回执要能对上号）。
    public private(set) var session: String?

    public init() {}

    /// 用户点了小窗：可以发 expand 意图（宿主据此不重复发）。
    public mutating func beginExpansion(session: String) -> Bool {
        switch phase {
        case .idle, .cancelled:
            phase = .expanding
            self.session = session
            return true
        case .expanding, .fellBack:
            return false
        }
    }

    /// 等回执超时：先画一张放大的图，但**不**撤销——迟到的成功回执还能升级成真窗口。
    public mutating func expansionTimedOut() -> ExpansionOutcome {
        guard phase == .expanding else { return .nothing }
        phase = .fellBack
        return .showDrawnPanel
    }

    /// 引擎对 expand 的回执。
    ///
    /// - `sessionStillShown`：这块面板这会儿还在放这个会话（没被 ✕ 关掉、也没换绑给别人）。
    /// - `drawnPanelOnScreen`：自绘放大面板正开着（用户还没把它收回）。
    public mutating func resolveExpansion(
        ok: Bool,
        sessionStillShown: Bool,
        drawnPanelOnScreen: Bool
    ) -> ExpansionOutcome {
        switch phase {
        case .expanding:
            guard ok else {
                phase = .idle
                return .showDrawnPanel
            }
            guard sessionStillShown else {
                // 面板已经换人/被关掉：真窗口确实被抬到了最前，替他放回去。
                phase = .idle
                return .restoreRealWindow
            }
            phase = .idle
            session = nil
            return .handOverToRealWindow

        case .cancelled:
            phase = .idle
            return ok ? .restoreRealWindow : .nothing

        case .fellBack:
            guard ok else {
                phase = .idle
                return .nothing
            }
            guard sessionStillShown, drawnPanelOnScreen else {
                phase = .idle
                return .restoreRealWindow
            }
            // 迟到的成功回执：自绘面板让位给真窗口（旧代码在这里反而把窗口搬回去）。
            phase = .idle
            session = nil
            return .handOverToRealWindow

        case .idle:
            return .nothing
        }
    }

    /// 用户点了收回：自绘放大面板上的收回，或等回执期间不要了。
    public mutating func requestCollapse() -> CollapseRequest {
        switch phase {
        case .expanding:
            phase = .cancelled
            return .cancelled
        case .cancelled, .fellBack:
            phase = .idle
            return .collapseDrawnPanel
        case .idle:
            return .ignored
        }
    }

    public mutating func reset() {
        phase = .idle
        session = nil
    }
}
