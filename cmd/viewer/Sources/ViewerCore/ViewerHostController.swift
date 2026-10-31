import AppKit
import CoreGraphics
import Foundation

/// 屏幕上那块**唯一**的小窗（2026-09-22 现场批次 #2）。
///
/// 历史：这个宿主曾经是「每会话一块面板 + 向下级联排布」（`ViewerSessionRegistry`），
/// 一次 AutoDL 实战就堆出 ctx-1…ctx-7 七块，用户「看不过来」。而需求
/// `docs/viewer-requirements-2026-09-11.md` §3.6 本来就写死「默认全局单小窗，跟随最近
/// 活跃会话自动切换视图；多会话并列多窗只是待定项」——所以这里回到单面板：
/// 一块**可重绑**的面板，由 `ViewerPanelRouter` 决定它在放谁。
///
/// 多会话在引擎侧仍然各有自己的 target、窗口与画面流（互不打架的那条规矩没变），
/// 壳只是**同时只显示一块**。位置就是用户拖到哪儿是哪儿：只有一块，不再需要级联槽位。
public final class ViewerHostController {
    /// 屏幕上唯一那块面板。创建一次，之后只换绑会话（位置也就自然保住了）。
    public private(set) var panel: ViewerPanelController?
    /// 这块面板在放谁，以及 ✕ 之后的静默规则。
    public private(set) var router = ViewerPanelRouter()
    /// 「点小窗 → 真页面」这条链路的状态机。
    public private(set) var expansion = ViewerExpansionMachine()

    /// Fired when the panel (re)binds to a session, mainly so a host app can log it.
    public var onRegister: ((String, ViewerPanelController) -> Void)?
    /// 壳 → 引擎的意图出口（app 接到 socket 客户端上）。
    public var onIntent: ((ViewerSocketIntent) -> Bool)?

    /// Host-level engine link; broadcast to the panel (it shows it in its pill).
    public private(set) var connectionState: ViewerLinkState = .unknown

    private let autoCollapse: Bool
    private let visibleFrameProvider: () -> CGRect
    private let primaryScreenHeightProvider: () -> CGFloat
    /// 展开链路上唯一那个在等的超时（单面板 = 同时只有一次展开）。
    private var pendingWork: DispatchWorkItem?
    private var hostPIDs: [String: Int] = [:]
    private var notifiedSession: String?

    /// 引擎必须在这个时间内回执；超时壳自己画一张放大的图，保证点击不落空。
    ///
    /// 这个值必须与引擎的 `viewerIntentTimeout`（5s）同量级：一次展开是 5~6 次 CDP
    /// 往返（`windowIDForTarget`/`getWindowBounds`/`setWindowState`/`setWindowBounds`/
    /// `evalValue(screen.*)`），忙页面上 1.2s 必超时——2026-09-22 现场「点开只是一张点
    /// 不动、滚不动的图」就是这么来的。超时只回落，**不**撤销（见 `ViewerExpansionMachine`）。
    public var expansionTimeout: TimeInterval = 5

    public init(
        autoCollapse: Bool = ViewerHostController.autoCollapseFromEnvironment,
        visibleFrame: @escaping () -> CGRect = { NSScreen.main?.visibleFrame ?? CGRect(x: 0, y: 0, width: 1440, height: 900) },
        primaryScreenHeight: @escaping () -> CGFloat = { NSScreen.screens.first?.frame.height ?? 0 }
    ) {
        self.autoCollapse = autoCollapse
        self.visibleFrameProvider = visibleFrame
        self.primaryScreenHeightProvider = primaryScreenHeight
    }

    public static var autoCollapseFromEnvironment: Bool {
        switch ProcessInfo.processInfo.environment["UWS_VIEWER_AUTOCOLLAPSE"]?.lowercased() {
        case "0", "false", "off", "no": return false
        default: return true
        }
    }

    /// 用户关掉小窗：**真的**丢掉这块面板（不是隐藏），并告诉引擎这个会话可以静默回收了。
    ///
    /// 它不结束任务、不关那扇真窗口、不碰上下文与续页语义——那些都原样保留，Agent 再动
    /// 一下面板就回来（`ViewerPanelRouter`）。「关掉之后画面照流但不再弹回来」的老规矩
    /// 也还在：画面、标签的变化正是用户关掉它的理由。
    public func dismiss(sessionID: String) {
        router.dismiss(sessionID: sessionID)
        if expansion.session == sessionID {
            expansion.reset()
        }
        cancelPendingWork()
        panel?.hide()
        _ = onIntent?(ViewerSocketIntent(intent: .close, session: sessionID))
    }

    /// 引擎重连 = 上一段链路里的会话状态全部作废。留着一张陈旧画面比什么都不显示更糟：
    /// 用户会以为那个任务还在跑（2026-09-16 现场：引擎重启后旧面板还挂在屏幕上，
    /// 只能重启观看壳才能清掉）。活跃会话会在下一个 tick 里自己重新出现。
    public func purgePanels() {
        panel?.hide()
        panel = nil
        router.reset()
        expansion.reset()
        cancelPendingWork()
        hostPIDs.removeAll()
        notifiedSession = nil
    }

    /// Applies exactly one engine observation to the single panel.
    public func apply(update: ViewerBridgeUpdate) {
        if update.action?.kind == "sessionClosed" {
            remove(sessionID: update.session.id)
            return
        }
        // 展开链路在飞、或自绘放大面板正开着时，这块面板属于那个会话：别让别的会话把它
        // 抢走，否则迟到的展开回执会把真窗口弹到别人头上。
        switch expansion.phase {
        case .expanding, .fellBack:
            guard expansion.session == update.session.id else { return }
        case .idle, .cancelled:
            break
        }
        guard let target = router.route(sessionID: update.session.id, hasAction: update.action != nil) else {
            return
        }
        render(update: update, sessionID: target)
    }

    private func render(update: ViewerBridgeUpdate, sessionID: String) {
        let controller = ensurePanel(for: update.session)
        if controller.state.session.id != sessionID {
            // 换绑：先把画面清掉，别让上一页的图挂在新会话的标题下（等它的第一帧）。
            controller.update(session: update.session)
            controller.display(frame: ViewerCanvasView.placeholderImage(size: controller.compactPanel.frame.size))
        } else {
            controller.update(session: update.session)
        }
        if router.boundSessionID != notifiedSession {
            notifiedSession = router.boundSessionID
            onRegister?(sessionID, controller)
        }
        if !controller.isVisible { controller.show() }
        if let pid = update.browserPID, pid > 0 {
            hostPIDs[sessionID] = pid
            controller.setHostPID(pid)
        }
        if let bounds = update.windowBounds {
            // 附着模式（帧里没有 browserPID）：引擎随帧给的矩形让面板在**用户点小窗之前**就能
            // 屏上认窗 → 认进程，第 5 条的遮挡判据从第一帧起生效（规范 §4.4 第 7 条）。
            // 坐标空间与 `CGWindowList` 一致，不需要换算（与展开回执那条通路同源）。
            controller.setHostWindowBounds(bounds.rect)
        }
        if let frame = update.frame, let image = NSImage(data: frame.jpeg) {
            controller.display(frame: image)
        }
        if let action = update.action {
            controller.setAction(action.summary ?? action.kind)
        }
        if let pointer = update.pointer,
           let point = ViewerPointerRouting.normalized(pointer, metadata: update.frame?.metadata) {
            controller.showVirtualCursor(
                x: point.x,
                y: point.y,
                clicked: ViewerPointerRouting.isPress(pointer.kind)
            )
        }
    }

    private func ensurePanel(for session: ViewerSession) -> ViewerPanelController {
        if let panel { return panel }
        let frame = ViewerGeometry.compactFrame(inVisibleFrame: visibleFrameProvider())
        let controller = ViewerPanelController(
            state: ViewerState(session: session),
            frame: frame,
            autoCollapse: autoCollapse
        )
        // 关的是**这块面板此刻**在放的会话：面板会被换绑，闭包不能在创建时就把 id 定死。
        controller.onDismiss = { [weak self, weak controller] in
            guard let self, let controller else { return }
            self.dismiss(sessionID: controller.state.session.id)
        }
        // 面板能不能重新亮起来：只有「还绑着会话」才行。用户 ✕ 掉之后画面照流，
        // 前台轮询不能把这块已经被丢掉的面板复活（2026-09-22 互斥不变量）。
        controller.canShow = { [weak self] in self?.router.boundSessionID != nil }
        // R4：小窗只发意图，由宿主把它变成真实的呈现切换。socket（生产）模式之前没接
        // 这条线，点击小窗毫无反应（2026-09-12 用户现场反馈「想把它点开放大，发现它
        // 放大不了」）。
        controller.state.onIntent = { [weak self, weak controller] intent in
            guard let self, let controller else { return }
            switch intent {
            case .requestExpansion(let sessionID):
                _ = self.beginExpansion(sessionID: sessionID, controller: controller)
            case .requestCollapse(let sessionID):
                _ = self.requestCollapse(sessionID: sessionID)
            }
        }
        controller.display(frame: ViewerCanvasView.placeholderImage(size: frame.size))
        controller.showCompact()
        controller.setConnectionState(connectionState)
        panel = controller
        return controller
    }

    // MARK: - 展开 = 真窗口到最前（R4）

    /// 用户点了小窗：先问引擎能不能把这扇真窗口**抬到最前**（2026-09-22 口径：不搬位置、
    /// 不缩放）。能（intentAck ok）小窗就播「飞向它」的动画让位；不能（无头会话、附着
    /// 会话、引擎不在场、超时）立刻回落到自绘放大面板——「点开一定有东西」比「一定是真
    /// 窗口」更重要。
    ///
    /// 超时**不**撤销：引擎那边的窗口可能已经被抬起来了，迟到的成功回执会把自绘面板升级成
    /// 真窗口（2026-09-22 现场批次 #2c）。
    ///
    /// 壳不再说要多大：位置与大小都是那扇真窗口自己的事（位置控制在真机上是缺陷源，
    /// 见 `pkg/browser/viewer_window.go`）。`ViewerWindowSize` 留在 wire 协议上只为不跟老壳
    /// 断连，引擎忽略它。
    @discardableResult
    private func beginExpansion(sessionID: String, controller: ViewerPanelController) -> Bool {
        guard expansion.beginExpansion(session: sessionID) else { return false }
        // 这一瞬间「浏览器变前台」是我们自己请引擎做的（抬到最前），不是用户切走了工作：
        // 先别让前台轮询把面板藏了，否则「飞向它」的动画没得播。
        controller.suppressPresenceHide = true
        // `deferRaise`：这一趟只让引擎把真窗口摆好、回矩形，**别**激活——到前台那一下放在
        // 飞走动画的末段（2026-09-24 第二轮口径，规范 §4.4 第 3 条：先抬窗口再播动画＝
        // 窗口先到、动画后到，两段脱节）。
        let sent = onIntent?(
            ViewerSocketIntent(intent: .expand, session: sessionID, deferRaise: true)
        ) ?? false
        guard sent else {
            NSLog("uws-viewer intent send failed: intent=expand session=%@", sessionID)
            controller.suppressPresenceHide = false
            expansion.reset()
            controller.expand(animated: true)
            return false
        }
        NSLog("uws-viewer expand requested: session=%@", sessionID)
        schedule(timeout: expansionTimeout) { [weak self, weak controller] in
            guard let self, self.expansion.expansionTimedOut() == .showDrawnPanel else { return }
            self.pendingWork = nil
            controller?.suppressPresenceHide = false
            controller?.expand(animated: true)
        }
        return true
    }

    /// 收回：自绘放大面板上的「↩ 收回」，或「等回执的半路不要了」。
    ///
    /// 新口径下收回**不需要**再请引擎搬窗口：展开不改那扇真窗口的位置与大小，让位是小窗
    /// 自己演的（`flyAwayAndHide`），浏览器让开前台时它自己会亮回来。唯一还需要请引擎动
    /// 手的是「迟到的成功回执碰到已经取消的展开」——那时候真窗口已经被抬起来了，见
    /// `apply(intentResult:)` 的 `.restoreRealWindow`。
    @discardableResult
    private func requestCollapse(sessionID: String) -> Bool {
        guard expansion.session == sessionID else {
            // 不在展开链路上：自绘放大面板的收回是壳内的事。
            panel?.collapse(animated: true)
            return false
        }
        switch expansion.requestCollapse() {
        case .ignored:
            return false
        case .collapseDrawnPanel, .cancelled:
            // 自绘放大面板的收回是壳内的事（`collapse` 自己会挡住非展开态）；
            // 「半路不要了」时面板还停在 compact，这一下是空操作。
            cancelPendingWork()
            panel?.suppressPresenceHide = false
            panel?.collapse(animated: true)
            return true
        }
    }

    /// 引擎回执：它到底有没有把那扇真窗口抬到最前（`windowBounds` 是它生效后的真实矩形，
    /// 小窗拿它当「飞向它」的动画落点）。
    public func apply(intentResult result: ViewerIntentResult) {
        // 「点开没给真窗口」必须能在日志里问出原因：壳平时把回执静默吃掉，
        // 现场只剩一张放大的图片，没人知道引擎是「抬不起来」还是「抬失败了」。
        NSLog(
            "uws-viewer intent result: intent=%@ session=%@ ok=%@ reason=%@",
            result.intent, result.session, result.ok ? "true" : "false", result.reason ?? "-"
        )
        guard expansion.session == result.session else { return }
        let controller = panel
        switch (result.intent, result.ok) {
        case ("expand", true):
            if let pid = result.browserPID, pid > 0 {
                hostPIDs[result.session] = pid
                controller?.setHostPID(pid)
            }
            // 附着模式没有 `browserPID`：把这扇真窗口的矩形交给面板，让它在屏上认窗 → 认进程
            // （规范 §4.4 第 6 条）。坐标空间与 `CGWindowList` 一致，不需要换算。
            controller?.setHostWindowBounds(result.windowBounds?.rect)
            let frame = result.windowBounds.map {
                ViewerScreenGeometry.appKitFrame(from: $0, primaryScreenHeight: primaryScreenHeightProvider())
            } ?? controller?.compactPanel.frame ?? .zero
            let stillShown = router.boundSessionID == result.session
            let drawnPanelOnScreen = controller?.presentation == .expanded
            switch expansion.resolveExpansion(
                ok: true,
                sessionStillShown: stillShown,
                drawnPanelOnScreen: drawnPanelOnScreen
            ) {
            case .handOverToRealWindow:
                // 真窗口摆好了（`deferRaise` 那一趟只摆位、不激活）：小窗往它那儿飞、然后消失
                // ——屏幕上只管留一个（2026-09-22 互斥不变量）。
                //
                // `suppressPresenceHide` 要留到动画演完（2026-09-22 真机复验：前台轮询在
                // 飞走动画中途就 orderOut，动画被截断、只剩一次突兀的消失）。
                cancelPendingWork()
                // 到前台分两下，都放在**动画末段**（2026-09-24 第二轮现场口径，§4.4 第 3 条）：
                // ① 壳按 `browserPID` 抬这个进程（真实场景兜底：前台是同 bundle 的另一个
                //    Chrome 实例时，只有按 pid 指得到本会话这一只）；
                // ② 通知引擎补 CDP `Target.activateTarget`（把目标 Tab/窗口设为活动）。
                // 只在「确实要交棒给真窗口」这一支做；动画被打断 → `cancelPendingRaise` 取消。
                //
                // 这一下跑在面板的 `raiseQueue`（非主线程，见 `ViewerPanelController.raiseQueue`），
                // 所以先说**真正把窗口带到前台的那一步**：给引擎发 `raise`（socket 写自带锁，
                // 立即写出，不排队）。按 pid 激活是「同 bundle 多实例」的兜底、且必须主线程，
                // 异步丢回 main：不让它的等待拖住这一下的时刻（2026-09-24 真机读数：主线程排队
                // 把这一步推迟了 0.19–0.68s，设计值只有 0.15s）。
                controller?.flyAwayAndHide(toward: frame, raiseAtEnd: { [weak self] in
                    let sent = self?.onIntent?(
                        ViewerSocketIntent(intent: .raise, session: result.session)
                    ) ?? false
                    if !sent {
                        NSLog("uws-viewer intent send failed: intent=raise session=%@", result.session)
                    }
                    DispatchQueue.main.async {
                        ViewerAppActivation.activateBrowser(browserPID: result.browserPID)
                    }
                }) { [weak controller] in
                    controller?.suppressPresenceHide = false
                }
                expansion.reset()
            case .restoreRealWindow:
                // 引擎确实把窗口抬起来了，但用户已经不要了（或面板已经换人）：替他放回去。
                cancelPendingWork()
                controller?.suppressPresenceHide = false
                _ = onIntent?(ViewerSocketIntent(intent: .collapse, session: result.session))
            case .showDrawnPanel, .nothing:
                cancelPendingWork()
                controller?.suppressPresenceHide = false
            }
        case ("expand", false):
            // 引擎说抬不起来（无头会话 / 那扇真窗口已经被关掉）：别让用户干等超时，
            // 立刻回落到自绘放大面板。
            let outcome = expansion.resolveExpansion(
                ok: false,
                sessionStillShown: router.boundSessionID == result.session,
                drawnPanelOnScreen: false
            )
            cancelPendingWork()
            controller?.suppressPresenceHide = false
            if outcome == .showDrawnPanel { controller?.expand(animated: true) }
        default:
            // 包括我们自己发出去的那条 collapse（`.restoreRealWindow` 的收尾）的回执：
            // 那边没有壳状态要动，引擎自己搬完就是了。
            break
        }
    }

    /// The link is one fact for the whole host: report it once and the panel stops
    /// pretending its last frame is live (2026-09-13 评审).
    public func setConnectionState(_ state: ViewerSocketClient.State) {
        setConnectionState(ViewerLinkState(state))
    }

    public func setConnectionState(_ link: ViewerLinkState) {
        let reconnected = link == .connected && connectionState != .connected
        connectionState = link
        if reconnected { purgePanels() }
        panel?.setConnectionState(link)
    }

    /// 这块面板此刻在放的控制器；问的不是当前会话就返回 nil。
    public func controller(for sessionID: String) -> ViewerPanelController? {
        guard router.boundSessionID == sessionID else { return nil }
        return panel
    }

    /// 会话结束了（引擎推 sessionClosed）：这块面板如果正放着它就跟着丢掉——留一张
    /// 没人维护的陈旧画面比什么都不显示更糟。
    public func remove(sessionID: String) {
        router.forget(sessionID: sessionID)
        if expansion.session == sessionID {
            expansion.reset()
            cancelPendingWork()
        }
        if router.boundSessionID == nil { panel?.hide() }
    }

    public func setAction(_ text: String?, sessionID: String) {
        guard router.boundSessionID == sessionID else { return }
        panel?.setAction(text)
    }

    private func schedule(timeout: TimeInterval, _ body: @escaping () -> Void) {
        cancelPendingWork()
        let item = DispatchWorkItem(block: body)
        pendingWork = item
        DispatchQueue.main.asyncAfter(deadline: .now() + timeout, execute: item)
    }

    private func cancelPendingWork() {
        pendingWork?.cancel()
        pendingWork = nil
        // 动画末段那一次「抬真窗口」也算待办：展开被取消/收回时它不该再响。
        panel?.cancelPendingRaise()
    }
}
