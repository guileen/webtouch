import Foundation

/// 屏幕上那块**唯一**小窗该放哪个会话（2026-09-22 现场批次 #2）。
///
/// 需求（`docs/viewer-requirements-2026-09-11.md` §3.6）本来就写死「默认全局单小窗，
/// 跟随最近活跃会话自动切换视图；支持列表切换或 pin；多会话并列多窗列为待定项」，
/// 而实现走成了「每会话一块面板」——一次 AutoDL 实战就堆出 ctx-1…ctx-7 七块，
/// 用户「看不过来」。这个类型把那条原始规则收回来：
///
///   1. 屏幕上只有一块面板，它**跟随最近有动作的会话**切换。只有帧、没有动作的会话
///      抢不走面板，否则几个 Agent 一起跑，画面会在几个人之间来回翻。
///   2. 用户点了 ✕：面板**真的**丢掉（不是隐藏），而且不会被下一帧弹回来——画面、
///      标签的变化正是他关掉它的理由。只有**新的动作**才值得把面板叫回来。
///   3. 会话真的结束了（引擎推 sessionClosed）：这条记录失去意义，下次重新来过。
///
/// 纯逻辑、无 AppKit：切换规则必须能在没有窗口服务器的环境里单测（本仓测试的纪律）。
public struct ViewerPanelRouter {
    /// 当前这块面板在放的会话；nil = 屏幕上不该有面板。
    public private(set) var bound: String?
    /// 用户关掉的会话：画面继续来也不弹回，直到这个会话又有新动作。
    private var dismissed: Set<String> = []
    /// 用户把唯一那块面板关掉了：屏幕上暂时什么都不放，直到**某个**会话有新动作。
    /// 与「还没见过任何会话」区分开——后者第一帧就该点亮面板。
    private var suppressed = false

    public init() {}

    public var boundSessionID: String? { bound }
    public var isSuppressed: Bool { suppressed }
    public var dismissedSessionIDs: Set<String> { dismissed }

    /// 用户按下 ✕：这块面板真的丢掉，而且不被画面变化弹回来。
    public mutating func dismiss(sessionID: String) {
        dismissed.insert(sessionID)
        guard bound == sessionID else { return }
        bound = nil
        suppressed = true
    }

    /// 会话真的结束了：这条记录没有意义了；面板正放着它就一起丢掉。
    public mutating func forget(sessionID: String) {
        dismissed.remove(sessionID)
        if bound == sessionID { bound = nil }
    }

    /// 引擎重连/重启：上一段链路的会话全部作废。
    public mutating func reset() {
        bound = nil
        dismissed.removeAll()
        suppressed = false
    }

    /// 一条引擎观测到达：屏幕上该放哪个会话（返回 nil = 这条更新不该显示）。
    public mutating func route(sessionID: String, hasAction: Bool) -> String? {
        if dismissed.contains(sessionID) {
            guard hasAction else { return nil }
            dismissed.remove(sessionID)
        }
        if hasAction {
            // 最近有动作的会话拿走这块面板——这就是「跟随最近活跃会话」。
            suppressed = false
            bound = sessionID
            return sessionID
        }
        if suppressed { return nil }
        if bound == nil || bound == sessionID {
            bound = sessionID
            return sessionID
        }
        return nil
    }
}
